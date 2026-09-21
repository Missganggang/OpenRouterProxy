package app

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/time/rate"

	"github.com/openroute/openroute/internal/model"
)

// 本文件实现规格书 6.9「速度与资源限制」。
//
// MUST 遵守的语义（与 Nyanpass 一致，逐条编码为可单测的函数）：
//
//  1. 所有限制在**入口端**强制执行 —— 出口端不做限速。
//     因此调用点只有一个：入口接受新连接之前。
//  2. 每个入口**独立计算** —— 入口组内两台机器各自限速，总速率是两倍。
//     因此限流器状态是「进程内」的，绝不跨节点共享，也绝不落库。
//  3. 规则级限制与用户级限制**叠加** —— 实际生效的是两者中更严格的那个（取 min）。
//     由 EffectiveRate 实现：任一为 0 表示「该维度不限」，两者都限时取较小值。
//  4. **UDP 无法限速** —— UDP 无连接、无流控窗口，UI 上必须标注。
//     由 CanRateLimitProtocol 实现，UDP 直接放行。
//  5. **无法限制整个入口或某个程序的总体速度** —— 只能按规则 / 用户维度。
//     因此这里不存在「全局 limiter」这种东西，只有按规则键与用户键的限流器。
//
// 连接数 / IP 数 / 设备数限制的共性：
// 超限只拒绝**新连接**，绝不断开已建立的连接（规格书 6.9 明文要求）。

// 限制原因常量。返回值用于日志与前端提示，取值稳定。
const (
	// LimitOK 表示放行。
	LimitOK = "ok"
	// LimitReasonRate 表示被令牌桶限速。
	LimitReasonRate = "rate_limited"
	// LimitReasonConn 表示连接数超限。
	LimitReasonConn = "conn_limit"
	// LimitReasonIP 表示客户端 IP 数超限。
	LimitReasonIP = "ip_limit"
	// LimitReasonDevice 表示设备数超限。
	LimitReasonDevice = "device_limit"
	// LimitReasonUserDisabled 表示用户已被禁用或已过期。
	LimitReasonUserDisabled = "user_disabled"
	// LimitReasonTrafficUsed 表示流量已超出上限。
	LimitReasonTrafficUsed = "traffic_limit"
)

// IPWindow 是 IP 数限制的滑动窗口长度（规格书 6.9：默认 5 分钟）。
const IPWindow = 5 * time.Minute

// limiterIdleTTL 是限流器在空闲多久后可以被回收。
//
// 取 10 分钟：远长于一次连接的常见寿命，短于「用户改完限速想立刻生效」的等待耐心。
const limiterIdleTTL = 10 * time.Minute

// ipEntry 记录一个客户端 IP 在本进程内的活动情况。
type ipEntry struct {
	// First 为首次见到该 IP 的时间，用于滑动窗口判定。
	First time.Time
	// Last 为最近一次活动时间，用于窗口过期与内存回收。
	Last time.Time
	// Conns 为该 IP 当前活跃连接数。
	Conns int
}

// deviceEntry 记录一个 DeviceID 的活动情况。
//
// 与 ipEntry 分开是因为规格书 6.9 把「IP 数」与「设备数」定义为两个独立限制：
// 同一个 IP 上可能有多个设备（NAT 后的手机 + 电脑），反之一个设备换网也会换 IP。
type deviceEntry struct {
	First time.Time
	Last  time.Time
	Conns int
}

// LimitService 提供入口侧的限速与连接 / IP / 设备数限制。
//
// 装配方式：由 TrafficService 在构造时一并创建，通过 TrafficService.Limit() 取用。
// 之所以不挂到 App 上，是因为它只有入口链路一个使用者，
// 且其全部状态都是「进程内热态」，与 App 的长生命周期依赖性质不同。
type LimitService struct {
	app *App

	// now 是可注入的时钟，便于用极短窗口做确定性单测。
	now func() time.Time

	mu sync.Mutex
	// limiters 是按资源键（rule:<id> / user:<id>）组织的令牌桶。
	limiters map[string]*resourceLimiter
	// ips 是 rule:<id> → (客户端 IP → 活动记录)。
	// 维度是「规则」而不是「全站」：规格书 6.9 的 IP 限制属于规则 / 用户维度。
	ips map[string]map[string]*ipEntry
	// devices 是 user:<id> → (DeviceID → 活动记录)。
	devices map[string]map[string]*deviceEntry
}

// resourceLimiter 是一个带空闲时间的令牌桶。
type resourceLimiter struct {
	lim *rate.Limiter
	// rate 为当前生效的速率（byte/s）；用户改了限速后需要重建。
	rate int64
	// touched 为最近一次使用时间，供空闲回收。
	touched time.Time
}

// NewLimitService 构造入口限制服务。
//
// 参数 a 为运行时依赖容器；返回可直接使用的实例。
func NewLimitService(a *App) *LimitService {
	return &LimitService{
		app:      a,
		now:      func() time.Time { return time.Now().UTC() },
		limiters: make(map[string]*resourceLimiter),
		ips:      make(map[string]map[string]*ipEntry),
		devices:  make(map[string]map[string]*deviceEntry),
	}
}

// -------------------- 纯函数：可单测的语义编码 --------------------

// CanRateLimitProtocol 判断某协议是否可被限速（规格书 6.9）。
//
// UDP 无连接、无流控窗口，**无法限速**，因此返回 false。
// 这是规格书明文要求写进 UI 的语义，这里编码成唯一判据，避免各处自行判断。
func CanRateLimitProtocol(protocol string) bool {
	return protocol != "udp"
}

// EffectiveRate 计算规则级与用户级限速叠加后的实际速率（规格书 6.9）。
//
// 语义：
//   - 0 表示「该维度不限速」；
//   - 两个都大于 0 时取**更严格的那个**（min），即「叠加」的真实含义；
//   - 只有一个大于 0 时用那个值；
//   - 两个都是 0 时返回 0，表示不限速。
//
// 参数 ruleRate / userRate 为 byte/s。返回叠加后的 byte/s。
func EffectiveRate(ruleRate, userRate int64) int64 {
	switch {
	case ruleRate <= 0 && userRate <= 0:
		return 0
	case ruleRate <= 0:
		return userRate
	case userRate <= 0:
		return ruleRate
	default:
		if ruleRate < userRate {
			return ruleRate
		}
		return userRate
	}
}

// EffectiveConnLimit 计算连接数上限（规则级与用户级取 min，0 = 不限）。
func EffectiveConnLimit(ruleLimit, userLimit int) int {
	return minInt(ruleLimit, userLimit)
}

// EffectiveIPLimit 计算 IP 数上限（规则级与用户级取 min，0 = 不限）。
func EffectiveIPLimit(ruleLimit, userLimit int) int {
	return minInt(ruleLimit, userLimit)
}

// minInt 返回两个上限中更严格的那个；任一为 0（不限）时取另一个。
func minInt(a, b int) int {
	switch {
	case a <= 0 && b <= 0:
		return 0
	case a <= 0:
		return b
	case b <= 0:
		return a
	default:
		if a < b {
			return a
		}
		return b
	}
}

// LimitOver 判断当前值是否已越过上限。
//
// value >= limit 表示「已经用满」：因为调用点在**接受新连接之前**，
// 用满即拒绝，保证活跃数不会超过上限本身。
// limit <= 0 表示不限，永远返回 false。
func LimitOver(limit, value int) bool {
	if limit <= 0 {
		return false
	}
	return value >= limit
}

// -------------------- 限速（令牌桶） --------------------

// limiterKey 生成限流器的键。规则维度与用户维度用不同前缀，避免 ID 相同互相串扰。
func limiterKey(kind string, id uint64) string {
	return fmt.Sprintf("%s:%d", kind, id)
}

// Allow 判断一次入口读取（bytes 字节）是否被允许。
//
// 参数：
//   - key 为限流器键（由 limiterKey 生成）；
//   - bytesPerSec 为限速值，<= 0 表示不限速，直接放行；
//   - protocol 为入站协议，udp 直接放行（规格书 6.9）；
//   - n 为本次读取的字节数，<= 0 时按 1 处理（只做一次「探测」）。
//
// 返回是否放行。放行后会从令牌桶扣除 n 个令牌。
func (l *LimitService) Allow(key string, bytesPerSec int64, protocol string, n int) bool {
	if !CanRateLimitProtocol(protocol) {
		// UDP 无法限速：直接放行，并且不创建限流器，避免内存被 UDP 流撑大。
		return true
	}
	if bytesPerSec <= 0 {
		return true
	}
	if n <= 0 {
		n = 1
	}

	lim := l.acquire(key, bytesPerSec)
	return lim.AllowN(l.now(), n)
}

// AllowBytes 是 Allow 的便捷版本，按「一次突发」的字节数判断。
//
// 用于节点上报「本周期读了 N 字节」后询问是否继续。
// 超过平台 int 表示范围的字节数会被夹到 int 上限，避免转换溢出成负数而误判为「不限」。
func (l *LimitService) AllowBytes(key string, bytesPerSec int64, protocol string, n int64) bool {
	if limit := int64(platformMaxInt()); n > limit {
		n = limit
	}
	if n < 0 {
		n = 0
	}
	return l.Allow(key, bytesPerSec, protocol, int(n))
}

// acquire 取得（必要时创建）给定键的令牌桶。
//
// 桶容量取 max(1s 的速率, 64KB)：既允许约 1 秒的数据突发以避免限速把正常流打碎，
// 又不会大到让限速形同虚设。速率变化时重建桶，保证「改完限速立即生效」。
func (l *LimitService) acquire(key string, bytesPerSec int64) *rate.Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	if rl, ok := l.limiters[key]; ok {
		if rl.rate == bytesPerSec {
			rl.touched = now
			return rl.lim
		}
		// 速率已变：直接换一个新桶（旧的立即失效，符合「立即生效」的直觉）。
		rl.lim = newBucket(bytesPerSec)
		rl.rate = bytesPerSec
		rl.touched = now
		return rl.lim
	}

	lim := newBucket(bytesPerSec)
	l.limiters[key] = &resourceLimiter{lim: lim, rate: bytesPerSec, touched: now}
	return lim
}

// newBucket 按 byte/s 创建一个令牌桶。
func newBucket(bytesPerSec int64) *rate.Limiter {
	burst := int(bytesPerSec)
	if burst < 65536 {
		burst = 65536
	}
	if burst > 8*1024*1024 {
		burst = 8 * 1024 * 1024
	}
	// 初始令牌数等于桶容量，避免「刚建桶就限速」导致的起步抖动。
	return rate.NewLimiter(rate.Limit(bytesPerSec), burst)
}

// platformMaxInt 返回平台 int 的最大值。
//
// 命名带平台前缀是为了与本包内已有的 maxInt(v, min) 助手区分开：
// 后者是「取两者较大值」，语义完全不同。
func platformMaxInt() int { return int(^uint(0) >> 1) }

// SetRate 立即改写某个键的限速值，供设置变更后主动下发。
//
// 传 0 表示取消限速（限流器被移除，后续读取直接放行）。
func (l *LimitService) SetRate(key string, bytesPerSec int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if bytesPerSec <= 0 {
		delete(l.limiters, key)
		return
	}
	lim := newBucket(bytesPerSec)
	l.limiters[key] = &resourceLimiter{lim: lim, rate: bytesPerSec, touched: l.now()}
}

// LimiterCount 返回当前驻留的限流器数量，供 /system/status 观测内存占用。
func (l *LimitService) LimiterCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.limiters)
}

// Cleanup 回收长时间未使用的限流器与过期的 IP / 设备记录。
//
// 规格书要求「不引入 Redis」，因此所有限制状态都在进程内，
// 必须靠定期回收保证内存不会随「改过限速的规则数」无限增长。
//
// 返回被回收的限流器数量。
func (l *LimitService) Cleanup() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	removed := 0
	for k, rl := range l.limiters {
		if now.Sub(rl.touched) > limiterIdleTTL {
			delete(l.limiters, k)
			removed++
		}
	}

	// IP 记录：滑出窗口且没有活跃连接即可回收。
	for scope, m := range l.ips {
		for ip, e := range m {
			if e.Conns <= 0 && now.Sub(e.Last) > IPWindow {
				delete(m, ip)
			}
		}
		if len(m) == 0 {
			delete(l.ips, scope)
		}
	}
	for scope, m := range l.devices {
		for id, e := range m {
			if e.Conns <= 0 && now.Sub(e.Last) > IPWindow {
				delete(m, id)
			}
		}
		if len(m) == 0 {
			delete(l.devices, scope)
		}
	}
	return removed
}

// -------------------- IP 数限制（5 分钟滑动窗口） --------------------

// IPCount 返回指定规则在滑动窗口内出现过的不同客户端 IP 数。
//
// 参数 ruleID 为规则 ID。返回 IP 数量。
func (l *LimitService) IPCount(ruleID uint64) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.pruneIPs(scopeKey("rule", ruleID), l.now()))
}

// pruneIPs 剔除滑出窗口且没有活跃连接的 IP，返回剩余的记录表。
//
// 调用方必须持有 l.mu。
func (l *LimitService) pruneIPs(scope string, now time.Time) map[string]*ipEntry {
	m := l.ips[scope]
	if m == nil {
		return nil
	}
	for ip, e := range m {
		// 判定窗口用 First：一个 IP 从第一次出现起算，满窗口后重获「新 IP」资格。
		// 这一点很关键——否则「用了 5 分钟的老 IP」会被永久占用名额。
		if e.Conns <= 0 && now.Sub(e.First) >= IPWindow {
			delete(m, ip)
		}
	}
	return m
}

// AllowIP 判断某个客户端 IP 是否被允许建立新连接。
//
// 语义（规格书 6.9）：
//   - 已在窗口内的 IP 永远放行 —— **已建立的连接不中断**，也不因新 IP 到来被挤掉；
//   - 窗口内的不同 IP 数达到上限时，拒绝**新的** IP；
//   - limit <= 0 表示不限，始终放行；
//   - 本函数不改变任何计数（计数由 OpenConn / CloseConn 维护），
//     因此「先问后建」的调用方式不会因询问本身占掉名额。
//
// 参数 ruleID 为规则 ID（scope 维度）；ip 为客户端 IP；limit 为上限；已存在返回 true。
func (l *LimitService) AllowIP(ruleID uint64, ip string, limit int) bool {
	if limit <= 0 {
		return true
	}
	if ip == "" {
		// 拿不到 IP 时不拒绝——这是取不到信息的场景，拒绝会造成误伤。
		return true
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	scope := scopeKey("rule", ruleID)
	m := l.pruneIPs(scope, now)
	if e, ok := m[ip]; ok {
		// 老 IP：放行并刷新活跃时间，保证它不会在连接尚未结束时被判为过期。
		e.Last = now
		return true
	}
	if len(m) >= limit {
		return false
	}
	return true
}

// OpenConn 登记一条新建立的连接。
//
// 会同时更新「规则 → IP」与「用户 → 设备」两个维度的记录，
// 并各自累加连接数。参数均为 0 的维度会被跳过。
//
// 参数 ruleID / userID / deviceID / ip 为连接属性。
func (l *LimitService) OpenConn(ruleID, userID uint64, deviceID, ip string) {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	if ip != "" {
		scope := scopeKey("rule", ruleID)
		m := l.ips[scope]
		if m == nil {
			m = make(map[string]*ipEntry)
			l.ips[scope] = m
		}
		e := m[ip]
		if e == nil {
			e = &ipEntry{First: now}
			m[ip] = e
		}
		e.Last = now
		e.Conns++
	}

	if deviceID != "" {
		scope := scopeKey("user", userID)
		m := l.devices[scope]
		if m == nil {
			m = make(map[string]*deviceEntry)
			l.devices[scope] = m
		}
		e := m[deviceID]
		if e == nil {
			e = &deviceEntry{First: now}
			m[deviceID] = e
		}
		e.Last = now
		e.Conns++
	}
}

// CloseConn 注销一条连接。
//
// 参数同上。连接数降到 0 后记录不会立刻删除，
// 而是等滑出窗口（IP）或空闲超时（设备）后由 Cleanup 回收——
// 这样「刚断开的 IP」在窗口内仍算「已见过的 IP」，符合滑动窗口语义。
func (l *LimitService) CloseConn(ruleID, userID uint64, deviceID, ip string) {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	if ip != "" {
		if m := l.ips[scopeKey("rule", ruleID)]; m != nil {
			if e, ok := m[ip]; ok {
				if e.Conns > 0 {
					e.Conns--
				}
				e.Last = now
			}
		}
	}
	if deviceID != "" {
		if m := l.devices[scopeKey("user", userID)]; m != nil {
			if e, ok := m[deviceID]; ok {
				if e.Conns > 0 {
					e.Conns--
				}
				e.Last = now
			}
		}
	}
}

// DeviceCount 返回指定用户在滑动窗口内出现过的不同设备数。
//
// 参数 userID 为用户 ID。返回设备数量。
func (l *LimitService) DeviceCount(userID uint64) int {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	scope := scopeKey("user", userID)
	m := l.devices[scope]
	for id, e := range m {
		if e.Conns <= 0 && now.Sub(e.First) >= IPWindow {
			delete(m, id)
		}
	}
	return len(m)
}

// AllowDevice 判断某个设备是否被允许建立新连接。
//
// 语义与 AllowIP 完全一致（规格书 6.9：设备数基于 DeviceID 统计，超限拒绝新连接）。
// 参数 userID 为用户 ID；deviceID 为设备指纹；limit 为上限。
func (l *LimitService) AllowDevice(userID uint64, deviceID string, limit int) bool {
	if limit <= 0 {
		return true
	}
	if deviceID == "" {
		return true
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	scope := scopeKey("user", userID)
	m := l.devices[scope]
	for id, e := range m {
		if e.Conns <= 0 && now.Sub(e.First) >= IPWindow {
			delete(m, id)
		}
	}

	if e, ok := m[deviceID]; ok {
		e.Last = now
		return true
	}
	return len(m) < limit
}

// scopeKey 生成 IP / 设备记录的维度键。
func scopeKey(kind string, id uint64) string {
	return fmt.Sprintf("%s:%d", kind, id)
}

// -------------------- 组合判定 --------------------

// IngressLimitInput 是一次入口新连接的限制判定输入。
//
// 全部限制都取「规则级与用户级叠加后的严格值」，与规格书 6.9 的叠加语义一致。
type IngressLimitInput struct {
	RuleID   uint64
	UserID   uint64
	ClientIP string
	DeviceID string
	// Protocol 取 ws | http | tls | direct | udp。
	Protocol string
	// Bytes 为本次想读取的字节数，仅用于限速判定；0 表示只做「探测」。
	Bytes int64
}

// IngressLimitResult 是限制判定的结果。
type IngressLimitResult struct {
	// Allowed 为 false 时表示应当拒绝这条**新**连接。
	Allowed bool `json:"allowed"`
	// Reason 为拒绝原因，取值见 LimitReason* 常量。
	Reason string `json:"reason,omitempty"`
	// Message 为面向用户的中文提示。
	Message string `json:"message,omitempty"`
	// RuleRate / UserRate / EffectiveRate 回显三档限速值（byte/s），便于前端解释。
	RuleRate      int64 `json:"rule_rate"`
	UserRate      int64 `json:"user_rate"`
	EffectiveRate int64 `json:"effective_rate"`
}

// RuleLimitSpec 是规则级的限制参数快照。
type RuleLimitSpec struct {
	SpeedLimit int64
	ConnLimit  int
	IPLimit    int
}

// UserLimitSpec 是用户级的限制参数快照。
type UserLimitSpec struct {
	SpeedLimit  int64
	ConnLimit   int
	IPLimit     int
	DeviceLimit int
	// TrafficLimit / TrafficUsed 用于「流量已用尽」判定，0 表示不限。
	TrafficLimit int64
	TrafficUsed  int64
	// Disabled 表示用户被禁用。
	Disabled bool
}

// CheckIngress 执行一次完整的入口准入判定。
//
// 判定顺序（先便宜后昂贵，且先判定「身份」再判定「资源」）：
//  1. 用户是否被禁用 / 流量是否用尽；
//  2. 连接数上限（规则与用户叠加，取 min）；
//  3. IP 数上限（规则与用户叠加，取 min，5 分钟滑动窗口）；
//  4. 设备数上限（用户级）；
//  5. 限速（令牌桶，UDP 跳过）。
//
// 重要：只有第 5 步会消耗令牌；被前面步骤拒绝时不会扣减，避免误伤已有流速。
// 判定通过**不代表**已经建立连接——调用方建立成功后必须再调用 OpenConn 登记。
//
// 参数 in 为判定输入；rule / user 为限制参数快照；ruleConns / userConns 为当前活跃连接数。
// 返回判定结果。
func (l *LimitService) CheckIngress(in IngressLimitInput, rule RuleLimitSpec, user UserLimitSpec,
	ruleConns, userConns int) IngressLimitResult {

	effRate := EffectiveRate(rule.SpeedLimit, user.SpeedLimit)
	res := IngressLimitResult{
		Allowed:       true,
		Reason:        LimitOK,
		RuleRate:      rule.SpeedLimit,
		UserRate:      user.SpeedLimit,
		EffectiveRate: effRate,
	}

	if user.Disabled {
		res.Allowed, res.Reason = false, LimitReasonUserDisabled
		res.Message = "账号已被禁用"
		return res
	}
	if user.TrafficLimit > 0 && user.TrafficUsed >= user.TrafficLimit {
		res.Allowed, res.Reason = false, LimitReasonTrafficUsed
		res.Message = "已用流量达到上限"
		return res
	}

	// 连接数：规则与用户叠加取 min。
	if connLimit := EffectiveConnLimit(rule.ConnLimit, user.ConnLimit); connLimit > 0 {
		if LimitOver(connLimit, ruleConns) || LimitOver(connLimit, userConns) {
			res.Allowed, res.Reason = false, LimitReasonConn
			res.Message = fmt.Sprintf("并发连接数已达上限 %d", connLimit)
			return res
		}
	}

	// IP 数：规则与用户叠加取 min。已见过的 IP 永远放行（已建立的连接不中断）。
	if ipLimit := EffectiveIPLimit(rule.IPLimit, user.IPLimit); ipLimit > 0 {
		if !l.AllowIP(in.RuleID, in.ClientIP, ipLimit) {
			res.Allowed, res.Reason = false, LimitReasonIP
			res.Message = fmt.Sprintf("同时在线 IP 数已达上限 %d，已建立连接不受影响", ipLimit)
			return res
		}
	}

	// 设备数：用户级。
	if !l.AllowDevice(in.UserID, in.DeviceID, user.DeviceLimit) {
		res.Allowed, res.Reason = false, LimitReasonDevice
		res.Message = fmt.Sprintf("同时在线设备数已达上限 %d", user.DeviceLimit)
		return res
	}

	// 限速：UDP 直接跳过（规格书 6.9）；规则与用户各建一个桶，两个都要通过。
	if effRate > 0 && CanRateLimitProtocol(in.Protocol) {
		n := in.Bytes
		if n <= 0 {
			n = 1
		}
		if in.RuleID > 0 && rule.SpeedLimit > 0 {
			if !l.AllowBytes(limiterKey("rule", in.RuleID), rule.SpeedLimit, in.Protocol, n) {
				res.Allowed, res.Reason = false, LimitReasonRate
				res.Message = fmt.Sprintf("触发规则限速 %d byte/s", rule.SpeedLimit)
				return res
			}
		}
		if in.UserID > 0 && user.SpeedLimit > 0 {
			if !l.AllowBytes(limiterKey("user", in.UserID), user.SpeedLimit, in.Protocol, n) {
				res.Allowed, res.Reason = false, LimitReasonRate
				res.Message = fmt.Sprintf("触发用户限速 %d byte/s", user.SpeedLimit)
				return res
			}
		}
	}

	return res
}

// LoadRuleSpec 从数据库读取规则级限制参数。
//
// 规则不存在或查询失败时返回零值与 false；零值表示「全部不限」，
// 调用方据此可以安全地继续——一次查询抖动不应拒绝用户流量。
//
// 参数 ctx；ruleID。返回参数快照与是否存在。
func (l *LimitService) LoadRuleSpec(ctx context.Context, ruleID uint64) (RuleLimitSpec, bool) {
	var spec RuleLimitSpec
	if ruleID == 0 {
		return spec, false
	}
	var rule model.ForwardRule
	if err := l.app.DB.WithContext(ctx).
		Select("id", "speed_limit", "conn_limit", "ip_limit").
		First(&rule, ruleID).Error; err != nil {
		l.app.Log.Debug("读取规则限制参数失败", zap.Uint64("rule_id", ruleID), zap.Error(err))
		return spec, false
	}
	spec.SpeedLimit = rule.SpeedLimit
	spec.ConnLimit = rule.ConnLimit
	spec.IPLimit = rule.IPLimit
	return spec, true
}

// LoadUserSpec 从数据库读取用户级限制参数。
//
// 用户不存在或查询失败时返回零值（全部不限）与 false。
//
// 参数 ctx；userID。返回参数快照与是否存在。
func (l *LimitService) LoadUserSpec(ctx context.Context, userID uint64) (UserLimitSpec, bool) {
	var spec UserLimitSpec
	if userID == 0 {
		return spec, false
	}
	var user model.User
	if err := l.app.DB.WithContext(ctx).
		Select("id", "status", "traffic_limit", "traffic_used", "speed_limit",
			"ip_limit", "device_limit", "conn_limit").
		First(&user, userID).Error; err != nil {
		l.app.Log.Debug("读取用户限制参数失败", zap.Uint64("user_id", userID), zap.Error(err))
		return spec, false
	}
	// 只要不是「启用」就视为禁用。
	//
	// 这里刻意用 != StatusEnabled 而不是 == StatusDisabled：
	// status 列也可能因为导入脏数据而取到其它值，此时按「禁用」处理更安全
	// （宁可少放行一条连接，也不要让一个状态异常的用户继续跑流量）。
	spec.Disabled = user.Status != model.StatusEnabled
	spec.TrafficLimit = user.TrafficLimit
	spec.TrafficUsed = user.TrafficUsed
	spec.SpeedLimit = user.SpeedLimit
	spec.IPLimit = user.IPLimit
	spec.DeviceLimit = user.DeviceLimit
	spec.ConnLimit = user.ConnLimit
	return spec, true
}
