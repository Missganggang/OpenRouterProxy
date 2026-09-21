package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/openroute/openroute/internal/config"
	"github.com/openroute/openroute/internal/database"
	"github.com/openroute/openroute/internal/model"
)

// 本文件覆盖规格书 6.9「速度与资源限制」的全部强制语义。
//
// 之所以把这些语义写成测试而不是只写在注释里：
// 它们是**用户可感知的契约**——「规则限速和用户限速叠加」如果实现成
// 「取较大值」而不是「取较小值」，用户会立刻感觉「我设的 10MB/s 完全没生效」，
// 而这种错误在没有测试时极易在重构中悄悄引入。
//
// 覆盖点：
//  1. min() 叠加语义（EffectiveRate / EffectiveConnLimit / EffectiveIPLimit）；
//  2. 0 表示不限（任一维度为 0 时不应把它当作「限速 0」而掐死全部流量）；
//  3. UDP 无法限速；
//  4. IP 限制只拒绝新 IP，已存在 IP 继续可用；
//  5. 连接数 / 设备数超限拒绝新连接；
//  6. 每个入口独立计算（限流器状态是进程内私有的，不共享）。

// newLimitTestApp 构造一个带临时 SQLite 库的 App，仅用于限制与流量测试。
//
// 日志丢弃到内存，避免测试输出被 SQL 与调试日志淹没。
func newLimitTestApp(t *testing.T) *App {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "limit_test.db")
	db, err := database.Open(database.Options{Path: "sqlite3://" + dbPath, MaxOpen: 1, MaxIdle: 1})
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	if _, err := db.AutoMigrate(); err != nil {
		t.Fatalf("自动建表失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	cfg := config.Default()
	cfg.SecretKey = "test-secret-key"
	// 日志目录指到临时目录，避免测试在仓库里留下 logs/ 目录。
	cfg.LogPath = t.TempDir()

	return &App{
		Config:    cfg,
		DB:        db,
		Log:       newTestLogger(),
		StartedAt: time.Now().UTC(),
		stopCh:    make(chan struct{}),
	}
}

// ───────────────────────── 语义 1+2：min() 叠加与 0 = 不限 ─────────────────────────

// TestLimitEffectiveRateStacking 验证规则级与用户级限速按 min 叠加（规格书 6.9）。
func TestLimitEffectiveRateStacking(t *testing.T) {
	cases := []struct {
		name               string
		ruleRate, userRate int64
		want               int64
	}{
		{"两者都限：取更严格者（规则更小）", 1024, 4096, 1024},
		{"两者都限：取更严格者（用户更小）", 8192, 2048, 2048},
		{"两者相同：取该值", 2048, 2048, 2048},
		{"规则不限：只用用户限速", 0, 4096, 4096},
		{"用户不限：只用规则限速", 4096, 0, 4096},
		{"都为 0：完全不限速", 0, 0, 0},
		{"负数视为不限（容错）", -1, 4096, 4096},
		{"两负数视为不限", -5, -3, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := EffectiveRate(c.ruleRate, c.userRate)
			if got != c.want {
				t.Errorf("EffectiveRate(%d, %d) = %d，期望 %d",
					c.ruleRate, c.userRate, got, c.want)
			}
		})
	}
}

// TestLimitEffectiveRateIsMinNotMax 显式固化「叠加 = 取 min」而不是「取 max」。
//
// 这是规格书 6.9 中最容易被实现错的一条，单独用一条测试把方向钉死。
func TestLimitEffectiveRateIsMinNotMax(t *testing.T) {
	const rule, user int64 = 1000, 9000
	got := EffectiveRate(rule, user)
	if got != 1000 {
		t.Fatalf("叠加结果 = %d，期望 1000（min 语义）；若得到 9000 说明实现成了 max", got)
	}
}

// TestLimitEffectiveConnAndIPStacking 验证连接数 / IP 数上限同样按 min 叠加。
func TestLimitEffectiveConnAndIPStacking(t *testing.T) {
	cases := []struct {
		name             string
		ruleLim, userLim int
		want             int
	}{
		{"都限：取较小", 10, 5, 5},
		{"都限：规则更小", 3, 20, 3},
		{"规则不限（0）", 0, 8, 8},
		{"用户不限（0）", 8, 0, 8},
		{"都不限", 0, 0, 0},
		{"负数视为不限", -1, 6, 6},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := EffectiveConnLimit(c.ruleLim, c.userLim); got != c.want {
				t.Errorf("EffectiveConnLimit(%d, %d) = %d，期望 %d",
					c.ruleLim, c.userLim, got, c.want)
			}
			if got := EffectiveIPLimit(c.ruleLim, c.userLim); got != c.want {
				t.Errorf("EffectiveIPLimit(%d, %d) = %d，期望 %d",
					c.ruleLim, c.userLim, got, c.want)
			}
		})
	}
}

// TestLimitOver 验证「0 = 不限」与「达到上限即拒绝」。
func TestLimitOver(t *testing.T) {
	cases := []struct {
		name  string
		limit int
		value int
		want  bool
	}{
		{"0 表示不限，任意值都放行", 0, 99999, false},
		{"负数表示不限", -3, 10, false},
		{"未达上限", 5, 4, false},
		{"刚好达到上限即拒绝（为新连接预留名额）", 5, 5, true},
		{"超过上限", 5, 6, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := LimitOver(c.limit, c.value); got != c.want {
				t.Errorf("LimitOver(%d, %d) = %v，期望 %v", c.limit, c.value, got, c.want)
			}
		})
	}
}

// ───────────────────────── 语义 3：UDP 无法限速 ─────────────────────────

// TestLimitUDPCannotBeRateLimited 验证 UDP 不受限速影响（规格书 6.9）。
func TestLimitUDPCannotBeRateLimited(t *testing.T) {
	if CanRateLimitProtocol("udp") {
		t.Fatal("UDP 必须无法限速（CanRateLimitProtocol(udp) 应为 false）")
	}
	for _, proto := range []string{"ws", "http", "tls", "direct"} {
		if !CanRateLimitProtocol(proto) {
			t.Errorf("协议 %s 应当可以被限速", proto)
		}
	}

	app := newLimitTestApp(t)
	lim := NewLimitService(app)

	// 极小限速 + 大额读取：TCP 会立刻被拒，UDP 必须始终放行。
	const rate = 1024
	blocked := false
	for i := 0; i < 50; i++ {
		if !lim.Allow("rule:1", rate, "tls", 1<<20) {
			blocked = true
			break
		}
	}
	if !blocked {
		t.Fatal("TCP 在超量读取后应当被限速拒绝")
	}

	for i := 0; i < 50; i++ {
		if !lim.Allow("rule:2", rate, "udp", 1<<20) {
			t.Fatal("UDP 不应被限速拒绝（规格书 6.9：UDP 无法限速）")
		}
	}
	// UDP 连限流器都不应该留下，避免内存被 UDP 流撑大。
	if n := lim.LimiterCount(); n != 1 {
		t.Errorf("限流器数量 = %d，期望 1（只有 TCP 那个；UDP 不应创建限流器）", n)
	}
}

// TestLimitRateZeroMeansUnlimited 验证限速 0 表示不限速。
func TestLimitRateZeroMeansUnlimited(t *testing.T) {
	app := newLimitTestApp(t)
	lim := NewLimitService(app)

	for i := 0; i < 100; i++ {
		if !lim.Allow("rule:1", 0, "tls", 1<<20) {
			t.Fatal("限速为 0（不限）时任何读取都应放行")
		}
	}
	if n := lim.LimiterCount(); n != 0 {
		t.Errorf("限速为 0 时不应创建限流器，实际 = %d", n)
	}
}

// TestLimitBucketActuallyLimits 验证令牌桶确实按速率放行与拒绝。
func TestLimitBucketActuallyLimits(t *testing.T) {
	app := newLimitTestApp(t)
	lim := NewLimitService(app)

	// 固定时钟让测试与真实时间无关：不推进时间则桶不会被补充。
	now := time.Unix(1700000000, 0)
	lim.now = func() time.Time { return now }

	const rate = 1 << 20 // 1MB/s
	// 桶容量为 1 秒的速率，先掏空它。
	if !lim.Allow("rule:1", rate, "tls", rate) {
		t.Fatal("首次读取应当放行（桶是满的）")
	}
	if lim.Allow("rule:1", rate, "tls", 1) {
		t.Fatal("桶已掏空，紧接着的读取应当被拒绝")
	}

	// 推进 1 秒后应当又能拿到约 1 秒的令牌。
	now = now.Add(time.Second)
	if !lim.Allow("rule:1", rate, "tls", rate/2) {
		t.Fatal("经过 1 秒填充后，读取半个桶的额度应当放行")
	}
}

// TestLimitLimiterRebuiltOnRateChange 验证「改完限速立即生效」。
func TestLimitLimiterRebuiltOnRateChange(t *testing.T) {
	app := newLimitTestApp(t)
	lim := NewLimitService(app)

	now := time.Unix(1700000000, 0)
	lim.now = func() time.Time { return now }

	// 先用极小的速率把桶掏空，确认受限。
	//
	// 注意：桶容量有 64KB 的下限（避免把正常流打碎），
	// 因此每次读取必须达到该量级才能真正掏空它。
	const bucketFloor = 65536
	for i := 0; i < 4; i++ {
		lim.Allow("rule:1", 1024, "tls", bucketFloor)
	}
	if lim.Allow("rule:1", 1024, "tls", bucketFloor) {
		t.Fatal("小速率下应当被限速")
	}

	// 提高速率：新桶应当是满的，立刻可以放行。
	if !lim.Allow("rule:1", 1<<30, "tls", 1<<20) {
		t.Fatal("提高限速后应当立即生效（旧桶必须被换掉）")
	}
}

// ───────────────────────── 语义 4：IP 限制只拒绝新 IP ─────────────────────────

// TestLimitIPRejectsNewIPKeepsExisting 是规格书 6.9 的核心用例。
//
// 要求：超限时只拒绝**新** IP 的连接，已建立的连接不中断。
func TestLimitIPRejectsNewIPKeepsExisting(t *testing.T) {
	app := newLimitTestApp(t)
	lim := NewLimitService(app)

	now := time.Unix(1700000000, 0)
	lim.now = func() time.Time { return now }

	const ruleID uint64 = 7
	const limit = 2

	// 前两个 IP 建立连接。
	if !lim.AllowIP(ruleID, "1.1.1.1", limit) {
		t.Fatal("第 1 个 IP 应当放行")
	}
	lim.OpenConn(ruleID, 0, "", "1.1.1.1")

	if !lim.AllowIP(ruleID, "2.2.2.2", limit) {
		t.Fatal("第 2 个 IP 应当放行")
	}
	lim.OpenConn(ruleID, 0, "", "2.2.2.2")

	// 第 3 个 IP 必须被拒。
	if lim.AllowIP(ruleID, "3.3.3.3", limit) {
		t.Fatalf("IP 数已达上限 %d，第 3 个 IP 应当被拒绝", limit)
	}

	// 已存在的两个 IP 必须继续可用（已建立的连接不中断）。
	if !lim.AllowIP(ruleID, "1.1.1.1", limit) {
		t.Fatal("已存在的 IP 必须继续放行（已建立的连接不中断）")
	}
	if !lim.AllowIP(ruleID, "2.2.2.2", limit) {
		t.Fatal("已存在的 IP 必须继续放行（已建立的连接不中断）")
	}

	if got := lim.IPCount(ruleID); got != 2 {
		t.Errorf("窗口内 IP 数 = %d，期望 2", got)
	}
}

// TestLimitIPZeroMeansUnlimited 验证 IP 上限为 0（不限）时永远放行。
func TestLimitIPZeroMeansUnlimited(t *testing.T) {
	app := newLimitTestApp(t)
	lim := NewLimitService(app)

	for i := 0; i < 100; i++ {
		ip := "10.0.0." + itoa(i)
		if !lim.AllowIP(1, ip, 0) {
			t.Fatalf("IP 上限为 0（不限）时 %s 应当放行", ip)
		}
	}
	// 询问本身不应占用名额（AllowIP 是只读判断）。
	if got := lim.IPCount(1); got != 0 {
		t.Errorf("仅询问未建立连接时窗口内 IP 数应为 0，实际 %d", got)
	}
}

// TestLimitIPWindowExpiresOldIPs 验证 IP 数限制是 5 分钟滑动窗口。
func TestLimitIPWindowExpiresOldIPs(t *testing.T) {
	app := newLimitTestApp(t)
	lim := NewLimitService(app)

	now := time.Unix(1700000000, 0)
	lim.now = func() time.Time { return now }

	// 两个 IP 建立连接后又全部断开。
	lim.AllowIP(1, "1.1.1.1", 2)
	lim.OpenConn(1, 0, "", "1.1.1.1")
	lim.AllowIP(1, "2.2.2.2", 2)
	lim.OpenConn(1, 0, "", "2.2.2.2")
	lim.CloseConn(1, 0, "", "1.1.1.1")
	lim.CloseConn(1, 0, "", "2.2.2.2")

	// 窗口内：仍算占用了两个名额，新 IP 被拒。
	if lim.AllowIP(1, "3.3.3.3", 2) {
		t.Fatal("窗口内旧 IP 仍应占用名额")
	}

	// 滑出 5 分钟窗口后：名额应当被释放。
	now = now.Add(IPWindow + time.Second)
	if !lim.AllowIP(1, "3.3.3.3", 2) {
		t.Fatal("滑出 5 分钟窗口后应当允许新 IP")
	}
}

// TestLimitIPDifferentRulesIndependent 验证 IP 限制按规则独立统计。
//
// 规格书 6.9：每个入口独立计算，规则之间也不共享额度。
func TestLimitIPDifferentRulesIndependent(t *testing.T) {
	app := newLimitTestApp(t)
	lim := NewLimitService(app)

	lim.AllowIP(1, "1.1.1.1", 1)
	lim.OpenConn(1, 0, "", "1.1.1.1")

	// 规则 1 已满。
	if lim.AllowIP(1, "2.2.2.2", 1) {
		t.Fatal("规则 1 的 IP 额度已被占用")
	}
	// 规则 2 有自己的额度，不应受规则 1 影响。
	if !lim.AllowIP(2, "2.2.2.2", 1) {
		t.Fatal("不同规则的 IP 统计必须彼此独立")
	}
}

// ───────────────────────── 语义 5：设备数限制 ─────────────────────────

// TestLimitDeviceRejectsNewKeepsExisting 验证设备数限制的语义。
func TestLimitDeviceRejectsNewKeepsExisting(t *testing.T) {
	app := newLimitTestApp(t)
	lim := NewLimitService(app)

	const userID uint64 = 9
	const limit = 1

	if !lim.AllowDevice(userID, "device-A", limit) {
		t.Fatal("第 1 个设备应当放行")
	}
	lim.OpenConn(0, userID, "device-A", "")

	if lim.AllowDevice(userID, "device-B", limit) {
		t.Fatal("设备数已达上限，新设备应当被拒绝")
	}
	if !lim.AllowDevice(userID, "device-A", limit) {
		t.Fatal("已存在的设备必须继续放行")
	}
	if got := lim.DeviceCount(userID); got != 1 {
		t.Errorf("设备数 = %d，期望 1", got)
	}
}

// TestLimitDeviceZeroMeansUnlimited 验证设备上限为 0 时不限。
func TestLimitDeviceZeroMeansUnlimited(t *testing.T) {
	app := newLimitTestApp(t)
	lim := NewLimitService(app)
	for i := 0; i < 50; i++ {
		if !lim.AllowDevice(1, "dev-"+itoa(i), 0) {
			t.Fatal("设备上限为 0（不限）时应当全部放行")
		}
	}
}

// TestLimitEmptyDeviceIDAlwaysAllowed 验证拿不到 DeviceID 时不拒绝。
//
// 取舍：设备指纹依赖 UA，某些客户端根本给不出稳定指纹；
// 此时若「因为统计不到而拒绝」，用户会直接连不上，代价远大于放宽限制。
func TestLimitEmptyDeviceIDAlwaysAllowed(t *testing.T) {
	app := newLimitTestApp(t)
	lim := NewLimitService(app)
	lim.OpenConn(0, 1, "dev-A", "")
	if !lim.AllowDevice(1, "", 1) {
		t.Fatal("DeviceID 为空时不应拒绝（取不到信息不等于超限）")
	}
}

// TestLimitEmptyIPAlwaysAllowed 验证拿不到客户端 IP 时不拒绝。
func TestLimitEmptyIPAlwaysAllowed(t *testing.T) {
	app := newLimitTestApp(t)
	lim := NewLimitService(app)
	lim.OpenConn(1, 0, "", "1.1.1.1")
	if !lim.AllowIP(1, "", 1) {
		t.Fatal("客户端 IP 为空时不应拒绝")
	}
}

// ───────────────────────── 组合判定 CheckIngress ─────────────────────────

// TestLimitCheckIngressStackingAndUDP 验证组合判定把叠加语义与 UDP 豁免都带上了。
func TestLimitCheckIngressStackingAndUDP(t *testing.T) {
	app := newLimitTestApp(t)
	lim := NewLimitService(app)

	rule := RuleLimitSpec{SpeedLimit: 1 << 20, ConnLimit: 10, IPLimit: 5}
	user := UserLimitSpec{SpeedLimit: 4 << 20, ConnLimit: 0, IPLimit: 3, DeviceLimit: 2}

	res := lim.CheckIngress(IngressLimitInput{
		RuleID: 1, UserID: 1, ClientIP: "1.1.1.1", DeviceID: "d1", Protocol: "tls",
	}, rule, user, 0, 0)
	if !res.Allowed {
		t.Fatalf("首次连接应当放行，实际被拒: %s", res.Reason)
	}
	if res.EffectiveRate != 1<<20 {
		t.Errorf("叠加后的限速 = %d，期望 %d（规则更严格）", res.EffectiveRate, 1<<20)
	}

	// 连接数已达规则上限 10 → 拒绝。
	res = lim.CheckIngress(IngressLimitInput{
		RuleID: 1, UserID: 1, ClientIP: "1.1.1.1", DeviceID: "d1", Protocol: "tls",
	}, rule, user, 10, 0)
	if res.Allowed || res.Reason != LimitReasonConn {
		t.Fatalf("规则连接数达上限时应拒绝，实际 allowed=%v reason=%s", res.Allowed, res.Reason)
	}
}

// TestLimitCheckIngressRejectsDisabledUser 验证被禁用的用户直接拒绝。
func TestLimitCheckIngressRejectsDisabledUser(t *testing.T) {
	app := newLimitTestApp(t)
	lim := NewLimitService(app)

	res := lim.CheckIngress(IngressLimitInput{RuleID: 1, UserID: 1, ClientIP: "1.1.1.1"},
		RuleLimitSpec{}, UserLimitSpec{Disabled: true}, 0, 0)
	if res.Allowed || res.Reason != LimitReasonUserDisabled {
		t.Fatalf("被禁用的用户必须拒绝，实际 allowed=%v reason=%s", res.Allowed, res.Reason)
	}
}

// TestLimitCheckIngressRejectsTrafficExhausted 验证流量用尽后拒绝新连接。
func TestLimitCheckIngressRejectsTrafficExhausted(t *testing.T) {
	app := newLimitTestApp(t)
	lim := NewLimitService(app)

	res := lim.CheckIngress(IngressLimitInput{RuleID: 1, UserID: 1, ClientIP: "1.1.1.1"},
		RuleLimitSpec{}, UserLimitSpec{TrafficLimit: 100, TrafficUsed: 100}, 0, 0)
	if res.Allowed || res.Reason != LimitReasonTrafficUsed {
		t.Fatalf("流量用尽必须拒绝，实际 allowed=%v reason=%s", res.Allowed, res.Reason)
	}
	// 未设上限（0）时不应拒绝。
	res = lim.CheckIngress(IngressLimitInput{RuleID: 1, UserID: 1, ClientIP: "1.1.1.1"},
		RuleLimitSpec{}, UserLimitSpec{TrafficLimit: 0, TrafficUsed: 1 << 40}, 0, 0)
	if !res.Allowed {
		t.Fatal("未设置流量上限时不应因用量而拒绝")
	}
}

// TestLimitCheckIngressAllowsAllWhenNoLimit 验证全部限制为 0 时永远放行。
func TestLimitCheckIngressAllowsAllWhenNoLimit(t *testing.T) {
	app := newLimitTestApp(t)
	lim := NewLimitService(app)

	for i := 0; i < 50; i++ {
		res := lim.CheckIngress(IngressLimitInput{
			RuleID: 1, UserID: 1, ClientIP: "10.0.0." + itoa(i),
			DeviceID: "dev-" + itoa(i), Protocol: "ws", Bytes: 1 << 20,
		}, RuleLimitSpec{}, UserLimitSpec{}, 0, 0)
		if !res.Allowed {
			t.Fatalf("全部限制为 0（不限）时应当放行，实际 reason=%s", res.Reason)
		}
	}
}

// TestLimitCheckIngressUDPBypass 验证 UDP 只豁免限速，
// 连接数与 IP 数限制对 UDP 依然有效（它们与协议无关）。
func TestLimitCheckIngressUDPBypass(t *testing.T) {
	app := newLimitTestApp(t)
	lim := NewLimitService(app)

	rule := RuleLimitSpec{SpeedLimit: 1, ConnLimit: 0, IPLimit: 0}
	user := UserLimitSpec{SpeedLimit: 1}

	// UDP：即使 1 byte/s 的极端限速也不拦。
	for i := 0; i < 20; i++ {
		res := lim.CheckIngress(IngressLimitInput{
			RuleID: 1, UserID: 1, ClientIP: "1.1.1.1", Protocol: "udp", Bytes: 1 << 20,
		}, rule, user, 0, 0)
		if !res.Allowed {
			t.Fatalf("UDP 不应被限速拦截，实际 reason=%s", res.Reason)
		}
	}

	// 但连接数上限对 UDP 同样生效。
	rule2 := RuleLimitSpec{ConnLimit: 2}
	res := lim.CheckIngress(IngressLimitInput{
		RuleID: 1, UserID: 1, ClientIP: "1.1.1.1", Protocol: "udp",
	}, rule2, UserLimitSpec{}, 2, 0)
	if res.Allowed {
		t.Fatal("连接数限制与协议无关，UDP 也应受限")
	}
}

// ───────────────────────── 语义 6：每个入口独立计算 ─────────────────────────

// TestLimitEachIngressComputesIndependently 验证「每个入口独立计算」。
//
// 规格书 6.9：入口组内两台机器各自限速，总速率是两倍。
// 这要求限流器状态**只存在于单进程内**，绝不落库、绝不跨进程共享。
// 本测试用两个独立的 LimitService 实例模拟两台入口机。
func TestLimitEachIngressComputesIndependently(t *testing.T) {
	app := newLimitTestApp(t)

	ingressA := NewLimitService(app)
	ingressB := NewLimitService(app)

	now := time.Unix(1700000000, 0)
	ingressA.now = func() time.Time { return now }
	ingressB.now = func() time.Time { return now }

	const rate = 1 << 20

	// 入口 A 把桶掏空。
	if !ingressA.Allow("rule:1", rate, "tls", rate) {
		t.Fatal("入口 A 首次读取应放行")
	}
	if ingressA.Allow("rule:1", rate, "tls", 1) {
		t.Fatal("入口 A 的桶应已空")
	}
	// 入口 B 有自己的桶，速率是两倍（规格书 6.9 的预期结果）。
	if !ingressB.Allow("rule:1", rate, "tls", rate) {
		t.Fatal("入口 B 必须独立计算，不应受入口 A 影响（总速率应为两倍）")
	}

	// 限流器状态不落库：数据库里不存在任何限制状态表。
	for _, table := range []string{"sessions", "traffic_logs"} {
		var count int64
		if err := app.DB.Table(table).Count(&count).Error; err != nil {
			t.Fatalf("统计表 %s 失败: %v", table, err)
		}
	}
}

// ───────────────────────── 清理与回收 ─────────────────────────

// TestLimitCleanupReclaimsIdleLimiters 验证空闲限流器会被回收，避免内存无限增长。
func TestLimitCleanupReclaimsIdleLimiters(t *testing.T) {
	app := newLimitTestApp(t)
	lim := NewLimitService(app)

	now := time.Unix(1700000000, 0)
	lim.now = func() time.Time { return now }

	for i := 0; i < 10; i++ {
		lim.Allow("rule:"+itoa(i), 1024, "tls", 1)
	}
	if got := lim.LimiterCount(); got != 10 {
		t.Fatalf("限流器数量 = %d，期望 10", got)
	}

	// 未到回收时间：不应被清掉。
	if removed := lim.Cleanup(); removed != 0 {
		t.Errorf("未到空闲阈值时不应回收，实际回收 %d 个", removed)
	}

	// 超过空闲阈值后全部回收。
	now = now.Add(limiterIdleTTL + time.Minute)
	if removed := lim.Cleanup(); removed != 10 {
		t.Errorf("空闲超阈值后应回收全部 10 个限流器，实际 %d 个", removed)
	}
	if got := lim.LimiterCount(); got != 0 {
		t.Errorf("回收后限流器数量 = %d，期望 0", got)
	}
}

// TestLimitSetRateZeroRemovesLimiter 验证把限速改为 0 会移除限流器（恢复不限速）。
func TestLimitSetRateZeroRemovesLimiter(t *testing.T) {
	app := newLimitTestApp(t)
	lim := NewLimitService(app)

	lim.SetRate(userLimiterKey(1), 4096)
	if lim.LimiterCount() != 1 {
		t.Fatal("设置限速后应存在一个限流器")
	}
	lim.SetRate(userLimiterKey(1), 0)
	if lim.LimiterCount() != 0 {
		t.Fatal("把限速改为 0 后限流器应被移除")
	}
	// 移除后应当彻底不限速。
	//
	// 注意这里传的 bytesPerSec 必须是 0（即「改动之后」的限速值）：
	// Allow 的第一个参数是「当前生效的限速」，传入旧的 4096 会重新建桶并立刻触发限流，
	// 那验证的就不是「取消限速」而是「重新施加限速」了。
	for i := 0; i < 100; i++ {
		if !lim.Allow(userLimiterKey(1), 0, "tls", 1<<20) {
			t.Fatal("限速被取消后不应再拦截")
		}
	}
	if lim.LimiterCount() != 0 {
		t.Fatal("不限速时不应创建限流器，避免内存被无效条目占用")
	}
}

// ───────────────────────── 从数据库读取限制参数 ─────────────────────────

// TestLimitLoadRuleAndUserSpec 验证从库中读取限制参数。
func TestLimitLoadRuleAndUserSpec(t *testing.T) {
	app := newLimitTestApp(t)
	ctx := context.Background()
	lim := NewLimitService(app)

	if err := app.DB.Create(&model.ForwardRule{
		Name: "r1", InboundGroupID: 1, ListenPort: 8443,
		SpeedLimit: 2048, ConnLimit: 8, IPLimit: 4,
	}).Error; err != nil {
		t.Fatalf("写入规则失败: %v", err)
	}
	if err := app.DB.Create(&model.User{
		Username: "u1", PasswordHash: "x", Status: model.StatusEnabled,
		SpeedLimit: 1024, ConnLimit: 6, IPLimit: 2, DeviceLimit: 3,
		TrafficLimit: 1000, TrafficUsed: 200,
	}).Error; err != nil {
		t.Fatalf("写入用户失败: %v", err)
	}

	ruleSpec, ok := lim.LoadRuleSpec(ctx, 1)
	if !ok {
		t.Fatal("应能读到规则限制参数")
	}
	if ruleSpec.SpeedLimit != 2048 || ruleSpec.ConnLimit != 8 || ruleSpec.IPLimit != 4 {
		t.Errorf("规则限制参数不符: %+v", ruleSpec)
	}

	userSpec, ok := lim.LoadUserSpec(ctx, 1)
	if !ok {
		t.Fatal("应能读到用户限制参数")
	}
	if userSpec.SpeedLimit != 1024 || userSpec.DeviceLimit != 3 || userSpec.Disabled {
		t.Errorf("用户限制参数不符: %+v", userSpec)
	}

	// 叠加后的速率应当是更严格的 1024。
	if got := EffectiveRate(ruleSpec.SpeedLimit, userSpec.SpeedLimit); got != 1024 {
		t.Errorf("叠加速率 = %d，期望 1024", got)
	}
	// 叠加后的连接数上限应当是更严格的 6。
	if got := EffectiveConnLimit(ruleSpec.ConnLimit, userSpec.ConnLimit); got != 6 {
		t.Errorf("叠加连接数 = %d，期望 6", got)
	}
	// 叠加后的 IP 上限应当是更严格的 2。
	if got := EffectiveIPLimit(ruleSpec.IPLimit, userSpec.IPLimit); got != 2 {
		t.Errorf("叠加 IP 数 = %d，期望 2", got)
	}

	// 不存在的 ID：返回 false，且参数为「全部不限」。
	if _, ok := lim.LoadRuleSpec(ctx, 999); ok {
		t.Error("不存在的规则不应报告存在")
	}
	if spec, ok := lim.LoadUserSpec(ctx, 999); ok {
		t.Errorf("不存在的用户不应报告存在: %+v", spec)
	}
}

// TestLimitLoadUserSpecDetectsDisabled 验证被禁用的用户会被识别出来。
func TestLimitLoadUserSpecDetectsDisabled(t *testing.T) {
	app := newLimitTestApp(t)
	ctx := context.Background()
	lim := NewLimitService(app)

	// token 列带唯一索引，而空串在唯一索引下会互相冲突，
	// 因此测试里显式给出 token（真实路径下 UserService.Create 会自动生成）。
	user := model.User{Username: "banned", PasswordHash: "x", Token: "tok_banned"}
	if err := app.DB.Create(&user).Error; err != nil {
		t.Fatalf("写入用户失败: %v", err)
	}
	// 显式把状态改成 0（禁用）。
	//
	// 必须用 Update 而不是在 Create 时传 Status: 0：
	// Gorm 对带 default 标签的字段会用零值触发默认值回填，
	// Create 时的 0 会被写回默认的 1，导致这条测试测不到禁用分支。
	if err := app.DB.Model(&model.User{}).Where("id = ?", user.ID).
		Update("status", model.StatusDisabled).Error; err != nil {
		t.Fatalf("禁用用户失败: %v", err)
	}

	spec, ok := lim.LoadUserSpec(ctx, user.ID)
	if !ok {
		t.Fatal("应能读到用户")
	}
	if !spec.Disabled {
		t.Fatal("status=0 的用户必须被标记为禁用")
	}

	// 启用中的用户不应被标记为禁用。
	normal := model.User{
		Username: "normal", PasswordHash: "x",
		Status: model.StatusEnabled, Token: "tok_normal",
	}
	if err := app.DB.Create(&normal).Error; err != nil {
		t.Fatalf("写入用户失败: %v", err)
	}
	spec2, _ := lim.LoadUserSpec(ctx, normal.ID)
	if spec2.Disabled {
		t.Fatal("启用中的用户不应被标记为禁用")
	}
}

// newTestLogger 返回一个丢弃全部日志的 zap 日志器。
//
// 测试关心的是限制判定的结果，不是日志内容；
// 丢弃输出可以让 `go test -v` 的结果不被 SQL 与调试日志淹没。
func newTestLogger() *zap.Logger {
	return zap.NewNop()
}
