package app

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"math/rand"
	"sort"
	"strconv"
	"strings"

	"github.com/openroute/openroute/internal/model"
	"github.com/openroute/openroute/internal/util"
)

// 本文件实现规格书 6.3 的负载均衡与故障转移算法，以及规格书 4.3 的
// 「连接地址优先级」。所有函数都是**纯函数**（不触碰数据库），
// 目的是让这套最容易出错的选路逻辑可以被表驱动单元测试完整覆盖。

// ───────────────────────── 连接地址优先级（规格书 4.3） ─────────────────────────

// 连接地址优先级规则编号，取值即规格书 4.3 代码块里的行号。
//
// 调用方（前端提示、日志、审计）可以直接把这个编号展示给用户，
// 便于核对「为什么选了这个地址」。
const (
	// ConnectRuleStatic 静态连接地址：connect_type == "static" && connect_address != ""。
	ConnectRuleStatic = 1
	// ConnectRuleConnectHost 节点自报连接地址：connect_host != ""。
	ConnectRuleConnectHost = 2
	// ConnectRuleDynIPv4 动态 IPv4 可用。
	ConnectRuleDynIPv4 = 3
	// ConnectRuleIPv6Group ipv6_group 已配置且当前节点在组内。
	ConnectRuleIPv6Group = 4
	// ConnectRuleDynIPv6 动态 IPv6 可用。
	ConnectRuleDynIPv6 = 5
	// ConnectRulePublicIPv4 回退到上报的公网 IPv4。
	ConnectRulePublicIPv4 = 6
)

// ConnectRuleName 返回优先级编号对应的中文说明，供 UI 展示。
func ConnectRuleName(rule int) string {
	switch rule {
	case ConnectRuleStatic:
		return "静态连接地址（connect_address）"
	case ConnectRuleConnectHost:
		return "节点自报连接地址（connect_host）"
	case ConnectRuleDynIPv4:
		return "动态 IPv4（dyn_ip4）"
	case ConnectRuleIPv6Group:
		return "IPV6 优先组（ipv6_group）"
	case ConnectRuleDynIPv6:
		return "动态 IPv6（dyn_ip6）"
	case ConnectRulePublicIPv4:
		return "回退到公网 IPv4"
	}
	return "未知来源"
}

// ConnectAddressInput 是「连接地址优先级」判决所需的全部输入。
//
// 静态侧的 ConnectType / ConnectAddress / ConnectPort 来自**出口组的 config**；
// 其余字段来自**当前出口节点上报的信息**。
type ConnectAddressInput struct {
	// ── 出口组 config ──
	ConnectType    string // dyn_ip4 | dyn_ip6 | static
	ConnectAddress string // 静态地址，支持域名或 IP
	ConnectPort    int    // 静态地址时的连接端口

	// ── 出口节点上报 ──
	PublicIPv4  string   // 上报的公网 IPv4（最后的回退项）
	PublicIPv6  string   // 上报的公网 IPv6
	ConnectHost string   // 节点手工填写的连接地址
	DynIPv4     string   // 节点上报的动态 IPv4（可为空，空则退回 PublicIPv4）
	DynIPv6     string   // 节点上报的动态 IPv6（可为空，空则退回 PublicIPv6）
	NodePort    int      // 节点侧监听端口（ws/http/tls 端口），非静态地址时使用
	IPv6Group   []uint64 // 入口组 config.ipv6_group
	NodeID      uint64   // 当前出口节点 ID，用于判断是否在 ipv6_group 内
}

// ResolveConnectAddress 按规格书 4.3 的**严格顺序**推导连接地址。
//
// 顺序（MUST 不得调整、不得增加回退）：
//
//  1. connect_type == "static" && connect_address != ""  →  connect_address + connect_port
//  2. connect_host != ""                                 →  connect_host
//  3. dyn_ip4 可用                                        →  动态 IPv4
//  4. ipv6_group 配置且当前节点在组内                       →  动态 IPv6
//  5. dyn_ip6 可用                                        →  动态 IPv6
//  6. 回退                                                →  上报的公网 IPv4
//
// 特别说明（规格书 4.3 的警告）：connect 地址选择**没有回退机制**。
// 若第 4/5 步选中了 IPv6 而目标不可达，不会自动退回 IPv4——
// 这是刻意的语义，前端在选择 IPv6 优先时 MUST 弹出风险提示。
//
// 返回值：选中的地址、端口、命中的规则编号（ConnectRule*）。全部分支都不可用时
// 返回空地址与 0 端口，调用方据此判定该出口当前不可用。
func ResolveConnectAddress(in ConnectAddressInput) (string, int, int) {
	// 1. 静态地址优先。注意 dyn_ip4 / dyn_ip6 是「动态」策略，
	//    只有 connect_type == "static" 时才使用 connect_address。
	if strings.EqualFold(strings.TrimSpace(in.ConnectType), ConnectTypeStatic) {
		if addr := strings.TrimSpace(in.ConnectAddress); addr != "" {
			return addr, in.ConnectPort, ConnectRuleStatic
		}
	}

	// 2. 节点自报的连接地址（含静态域名/IPv4/IPv6）。
	if h := strings.TrimSpace(in.ConnectHost); h != "" {
		return h, in.NodePort, ConnectRuleConnectHost
	}

	// 3. 动态 IPv4：connect_type 指定 dyn_ip4，或未指定类型（默认按 IPv4 走）。
	if strings.EqualFold(strings.TrimSpace(in.ConnectType), ConnectTypeDynIPv4) ||
		strings.TrimSpace(in.ConnectType) == "" {
		if ip := dynamicIPv4(in); ip != "" {
			return ip, in.NodePort, ConnectRuleDynIPv4
		}
	}

	// 4. ipv6_group：入口组强制「所有出口优先 IPv6」，或「除列出的节点外优先 IPv6」。
	//    语义（规格书 4.3）：
	//      []        默认，不在此处命中
	//      [0]       所有出口优先 IPv6
	//      [0,1,2]   所有出口优先 IPv6，但节点 1、2 优先 IPv4
	if len(in.IPv6Group) > 0 && ipv6GroupPrefers(in.IPv6Group, in.NodeID) {
		if ip := dynamicIPv6(in); ip != "" {
			return ip, in.NodePort, ConnectRuleIPv6Group
		}
	}

	// 5. 动态 IPv6：connect_type 指定 dyn_ip6 时直接使用上报的 IPv6。
	if strings.EqualFold(strings.TrimSpace(in.ConnectType), ConnectTypeDynIPv6) {
		if ip := dynamicIPv6(in); ip != "" {
			return ip, in.NodePort, ConnectRuleDynIPv6
		}
	}

	// 6. 兜底：上报的公网 IPv4。
	if ip := dynamicIPv4(in); ip != "" {
		return ip, in.NodePort, ConnectRulePublicIPv4
	}
	return "", 0, 0
}

// dynamicIPv4 返回节点上报的动态 IPv4；为空时退回公网 IPv4。
func dynamicIPv4(in ConnectAddressInput) string {
	if ip := normalizeIPv4(in.DynIPv4); ip != "" {
		return ip
	}
	return normalizeIPv4(in.PublicIPv4)
}

// dynamicIPv6 返回节点上报的动态 IPv6；为空时退回公网 IPv6。
func dynamicIPv6(in ConnectAddressInput) string {
	if ip := strings.TrimSpace(in.DynIPv6); ip != "" {
		return ip
	}
	return strings.TrimSpace(in.PublicIPv6)
}

// ipv6GroupPrefers 判断指定节点是否应当优先使用 IPv6。
//
// 语法（规格书 4.3 / 附录 A）：
//
//	[]        不启用 IPv6 优先
//	[0]       所有出口优先 IPv6
//	[0,1,2]   所有出口优先 IPv6，但节点 1、2 保持 IPv4 优先
func ipv6GroupPrefers(group []uint64, nodeID uint64) bool {
	hasAll := false
	for _, id := range group {
		if id == 0 {
			hasAll = true
			break
		}
	}
	if !hasAll {
		return false
	}
	// 0 表示「全部」，其后列出的节点 ID 是「例外：仍优先 IPv4」。
	for _, id := range group {
		if id != 0 && id == nodeID {
			return false
		}
	}
	return true
}

// normalizeIPv4 校验并返回纯 IPv4 字面量；IPv6 或非法值返回空串。
func normalizeIPv4(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	i := strings.LastIndex(s, ":")
	head := s
	if i >= 0 {
		head = s[:i]
	}
	parts := strings.Split(head, ".")
	if len(parts) != 4 {
		return ""
	}
	for _, p := range parts {
		v, err := strconv.Atoi(p)
		if err != nil || v < 0 || v > 255 {
			return ""
		}
	}
	return head
}

// ───────────────────────── 「限制出口」语法（规格书 6.3） ─────────────────────────

// ExitRestriction 是「限制出口」字段的解析结果。
//
// 支持 Nyanpass 的全部写法（规格书 6.3 表格）：
//
//	"禁止单端"         → BanDirect = true，允许全部真实出口
//	"1,2,3"           → AllowIDs = [1,2,3]
//	"1,2,3,禁止单端"   → AllowIDs = [1,2,3] 且 BanDirect = true
//	"1145141919"      → AllowIDs = [1145141919]（不存在的 ID = 只允许单端）
//	""                → 不限制
type ExitRestriction struct {
	// AllowIDs 为白名单节点 ID；为空表示「允许全部真实出口」。
	AllowIDs []uint64
	// BanDirect 表示禁止使用单端出口（必须走隧道到独立出口机）。
	BanDirect bool
	// Raw 保留原始文本，便于回显与审计。
	Raw string
}

// ParseExitRestriction 解析「限制出口」文本。
func ParseExitRestriction(raw string) ExitRestriction {
	ids, banDirect := util.ParseIntList(raw)
	// ParseIntList 已兼容「禁止单端」关键字（含前后混写）。
	return ExitRestriction{AllowIDs: ids, BanDirect: banDirect, Raw: raw}
}

// Empty 判断是否等于「不限制」。
func (r ExitRestriction) Empty() bool { return len(r.AllowIDs) == 0 && !r.BanDirect }

// AllowNode 判断某个出口节点是否被放行。
//
// 判定顺序：
//  1. 白名单非空且不含该节点 → 拒绝（`1,2,3` 的语义是「仅允许这些」）；
//  2. 白名单为空但禁止单端 → 仅拒绝「单端」这个伪出口（nodeID == DirectExitNodeID）；
//  3. 其余放行。
func (r ExitRestriction) AllowNode(nodeID uint64) bool {
	if len(r.AllowIDs) > 0 {
		found := false
		for _, id := range r.AllowIDs {
			if id == nodeID {
				found = true
				break
			}
		}
		// 白名单命中即放行；未命中时，若同时禁止单端，语义仍是「不在名单内不放行」。
		return found
	}
	if r.BanDirect && nodeID == DirectExitNodeID {
		return false
	}
	return true
}

// AllowDirect 判断是否允许「单端」（入口直出、不经过任何出口机）。
//
// `禁止单端` 与 `1145141919` 两种写法都归一到 false。
func (r ExitRestriction) AllowDirect() bool {
	if r.BanDirect {
		return false
	}
	if len(r.AllowIDs) > 0 {
		// 白名单非空且不含伪出口 ID 时，单端同样被排除。
		return r.AllowNode(DirectExitNodeID)
	}
	return true
}

// DirectExitNodeID 是「单端出口」在「限制出口」语法中的伪节点 ID。
//
// 真实节点 ID 由数据库自增分配，不可能是这个量级的值，
// 因此用它代表「入口直出」这一虚拟出口是安全的（规格书 6.3 的 1145141919 技巧）。
const DirectExitNodeID uint64 = 1145141919

// ───────────────────────── 出口节点候选与过滤 ─────────────────────────

// BalanceCandidate 是参与负载均衡的一个出口候选。
//
// 它是 model.Node 在「选路」这一视角下的投影：只保留算法需要的字段，
// 因此可以在单元测试里直接构造，不需要真实的节点记录。
type BalanceCandidate struct {
	NodeID      uint64 // 节点 ID
	Name        string // 节点名（日志与错误提示用）
	Weight      int    // 负载均衡权重，<=0 视为 1
	MaxConn     int    // 本机最大连接数，0 = 不限
	CurrentConn int    // 当前连接数
	Online      bool   // 节点是否在线（心跳判定）
	Healthy     bool   // 是否通过健康检查（由故障转移状态机维护）
	Up          bool   // 是否未被摘除（连续失败达到 max_fail 后为 false）
	Latency     int    // 最近一次握手耗时（毫秒），并列时作为稳定排序的次级键
}

// EffectiveWeight 返回用于计算的有效权重（<=0 按 1 处理）。
func (c BalanceCandidate) EffectiveWeight() int {
	if c.Weight <= 0 {
		return 1
	}
	return c.Weight
}

// ExceedsMaxConn 判断候选是否已达到本机连接数上限。
func (c BalanceCandidate) ExceedsMaxConn() bool {
	return c.MaxConn > 0 && c.CurrentConn >= c.MaxConn
}

// LoadRatio 返回 current_conn / weight，即 least_conn 的比较键。
func (c BalanceCandidate) LoadRatio() float64 {
	return float64(c.CurrentConn) / float64(c.EffectiveWeight())
}

// BalanceFilter 是一次选路的过滤条件。
type BalanceFilter struct {
	// Restrict 是「限制出口」解析结果。
	Restrict ExitRestriction
	// HealthCheck 为 true 时才要求 Healthy；关闭健康检查的组只看 Online 与 Up。
	HealthCheck bool
}

// FilterCandidates 实现规格书 6.3 的候选过滤：
//
//	online == true 且已通过健康检查
//	满足「限制出口」规则、未超过 MaxConn、权重 > 0
//
// 注意：不要求候选一定「全部满足」，而是把不满足的直接剔除——
// 这是 Nyanpass 的行为，被剔除的节点不参与分发但不报错。
//
// 参数 list 为原始候选；filter 为过滤条件。
// 返回过滤后的候选切片（保持传入顺序，顺序本身是负载均衡的次级决策依据）。
func FilterCandidates(list []BalanceCandidate, filter BalanceFilter) []BalanceCandidate {
	out := make([]BalanceCandidate, 0, len(list))
	for _, c := range list {
		if !c.Online {
			continue
		}
		if !c.Up {
			continue
		}
		if filter.HealthCheck && !c.Healthy {
			continue
		}
		// 权重 <= 0 的节点在 Nyanpass 中意味着「不参与分发」，MUST 剔除。
		if c.Weight <= 0 {
			continue
		}
		if c.ExceedsMaxConn() {
			continue
		}
		if !filter.Restrict.AllowNode(c.NodeID) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// ───────────────────────── 负载均衡算法 ─────────────────────────

// BalanceOptions 是选路的可选参数。
type BalanceOptions struct {
	// ClientIP 用于 hash_ip 一致性哈希；其它策略忽略。
	ClientIP string
	// Seed 是加权随机 / 并列打破的种子；为 0 时使用进程内随机源。
	// 显式传入种子可以让单元测试结果可复现。
	Seed int64
	// RRState 是平滑加权轮询（SWRR）的跨调用状态，可为 nil（内部临时构造）。
	RRState *SWRRState
}

// BalanceResult 是一次选路的结果。
type BalanceResult struct {
	// NodeID 为选中的节点 ID；为 DirectExitNodeID 时表示走单端出口。
	NodeID uint64
	// Candidate 为选中的候选（NodeID 为 DirectExitNodeID 时为零值）。
	Candidate BalanceCandidate
	// Direct 标记本次选中了「单端」出口。
	Direct bool
	// Reason 说明选中原因，供日志与 UI 展示。
	Reason string
}

// PickExit 按指定策略从候选中选出一个出口。
//
// 参数 strategy 取值见 model.Balance*；list 为**未过滤**的候选；
// filter 为过滤条件；opts 为可选参数。
//
// 设计取舍：候选为空时**不返回错误**，而是返回 Direct=true 让调用方
// 去处理「本组全挂」的场景（转投 FailoverGroupID 或走入口直出）。
// 这样上层只写一条分支，不会漏掉空组的情形。
//
// 返回值：选路结果。空候选时 Direct 为 true 且 Reason 说明原因。
func PickExit(strategy string, list []BalanceCandidate, filter BalanceFilter, opts BalanceOptions) BalanceResult {
	cands := FilterCandidates(list, filter)
	if len(cands) == 0 {
		reason := "本组没有可用出口（离线 / 已摘除 / 超限 / 被限制出口规则排除）"
		if filter.Restrict.Empty() {
			reason = "本组没有可用出口（离线 / 已摘除 / 超限）"
		}
		return BalanceResult{Direct: true, Reason: reason}
	}

	switch strategy {
	case model.BalanceRoundRobin:
		idx := PickSWRR(cands, opts.RRState)
		return BalanceResult{
			NodeID:    cands[idx].NodeID,
			Candidate: cands[idx],
			Reason:    "平滑加权轮询（round_robin）",
		}
	case model.BalanceHashIP:
		idx := PickConsistentHash(cands, opts.ClientIP)
		return BalanceResult{
			NodeID:    cands[idx].NodeID,
			Candidate: cands[idx],
			Reason:    fmt.Sprintf("源 IP 一致性哈希（hash_ip），客户端 %s", opts.ClientIP),
		}
	case model.BalanceWeighted:
		idx := PickWeightedRandom(cands, newLocalRand(opts.Seed))
		return BalanceResult{
			NodeID:    cands[idx].NodeID,
			Candidate: cands[idx],
			Reason:    "按权重随机（weighted）",
		}
	default:
		// least_conn 是默认策略（规格书 6.3）。
		idx := PickLeastConn(cands, newLocalRand(opts.Seed))
		return BalanceResult{
			NodeID:    cands[idx].NodeID,
			Candidate: cands[idx],
			Reason: fmt.Sprintf("最少连接数（least_conn），current_conn/weight = %.4f",
				cands[idx].LoadRatio()),
		}
	}
}

// newLocalRand 按种子构造随机源；种子为 0 时用全局随机源的真随机种子。
func newLocalRand(seed int64) *rand.Rand {
	if seed == 0 {
		return rand.New(rand.NewSource(rand.Int63()))
	}
	return rand.New(rand.NewSource(seed))
}

// PickLeastConn 实现规格书 6.3 的 least_conn：
//
//	选择 current_conn / weight 最小的节点
//	并列时按权重加权随机，再按节点顺序取第一个
//
// 参数 cands 必须已经过滤完毕且非空。
// 返回选中候选在 cands 中的下标。
func PickLeastConn(cands []BalanceCandidate, r *rand.Rand) int {
	// 第一轮：找出最小负载比。
	best := cands[0].LoadRatio()
	for _, c := range cands[1:] {
		if r := c.LoadRatio(); r < best {
			best = r
		}
	}

	// 第二轮：收集并列者，保持原有顺序（顺序即「节点顺序」，规格书要求可拖动排序）。
	tied := make([]int, 0, len(cands))
	for i, c := range cands {
		if c.LoadRatio() == best {
			tied = append(tied, i)
		}
	}
	if len(tied) == 1 {
		return tied[0]
	}

	// 第三轮：并列者之间按权重加权随机；未被随机命中时取顺序最靠前者。
	return tied[weightedIndex(tied, cands, r)]
}

// weightedIndex 在给定下标集合中按权重随机选一个，返回其在 picks 中的位置。
func weightedIndex(picks []int, cands []BalanceCandidate, r *rand.Rand) int {
	total := 0
	for _, i := range picks {
		total += cands[i].EffectiveWeight()
	}
	if total <= 0 || r == nil {
		return 0
	}
	hit := r.Intn(total)
	acc := 0
	for pos, i := range picks {
		acc += cands[i].EffectiveWeight()
		if hit < acc {
			return pos
		}
	}
	return len(picks) - 1
}

// PickWeightedRandom 实现规格书 6.3 的 weighted：纯按权重随机。
//
// 参数 r 为随机源（测试可注入固定种子）；cands 必须非空。
// 返回选中候选在 cands 中的下标。
func PickWeightedRandom(cands []BalanceCandidate, r *rand.Rand) int {
	if len(cands) == 0 {
		return -1
	}
	total := 0
	for _, c := range cands {
		total += c.EffectiveWeight()
	}
	if total <= 0 || r == nil {
		return 0
	}
	hit := r.Intn(total)
	acc := 0
	for i, c := range cands {
		acc += c.EffectiveWeight()
		if hit < acc {
			return i
		}
	}
	return len(cands) - 1
}

// SWRRState 是平滑加权轮询（Smooth Weighted Round-Robin）的跨调用状态。
//
// 算法（Nginx 的 smooth weighted round-robin）：
//
//	每一轮：current[i] += weight[i]
//	        选出 current 最大的 i，返回 i
//	        current[selected] -= totalWeight
//
// 与朴素加权轮询相比，SWRR 把高权重节点的调用**均匀打散**，
// 不会出现「连续 3 次都打到同一个高权重节点」的毛刺。
//
// 本类型不是并发安全的：由调用方（出口组选择器）自行加锁或持有副本。
type SWRRState struct {
	// current 是每个候选的当前累计权重，长度与候选数一致。
	current []int
}

// NewSWRRState 构造 SWRR 状态。
func NewSWRRState() *SWRRState { return &SWRRState{} }

// Reset 清空累计权重，用于成员变更（增删节点）后重新开始。
func (s *SWRRState) Reset() { s.current = nil }

// PickSWRR 用平滑加权轮询选出一个候选，返回其在 cands 中的下标。
//
// 参数 cands 必须非空；state 可以为 nil（等价于一次性调用，退化为取权重最大者）。
// 当选数量发生变化时，state 会自动重建累计数组。
func PickSWRR(cands []BalanceCandidate, state *SWRRState) int {
	if len(cands) == 0 {
		return -1
	}
	if len(cands) == 1 {
		return 0
	}

	if state == nil {
		state = &SWRRState{}
	}
	if len(state.current) != len(cands) {
		state.current = make([]int, len(cands))
	}

	total := 0
	for i, c := range cands {
		w := c.EffectiveWeight()
		total += w
		state.current[i] += w
	}

	best := 0
	for i := 1; i < len(state.current); i++ {
		// 严格大于：并列时取顺序靠前者（node order 决定优先级）。
		if state.current[i] > state.current[best] {
			best = i
		}
	}
	state.current[best] -= total
	return best
}

// PickConsistentHash 实现规格书 6.3 的 hash_ip：对客户端 IP 做一致性哈希。
//
// 一致性哈希的选择很多，这里采用 **HRW（rendezvous hashing）**：
//
//	score(n) = H(clientIP + "#" + nodeID)
//	选 score 最大的节点
//
// 相比「哈希环 + 虚拟节点」，HRW 不需要维护环，也**天然满足一致性哈希性质**：
// 摘除一个节点时只有落在该节点上的客户端需要重新映射（其它客户端固定出口不变），
// 而节点顺序完全不影响结果——这对「同一客户端固定出口」的语义更稳。
//
// 参数 clientIP 为空时退化为按节点 ID 取哈希（仍然稳定，不随机漂移）。
// 返回选中候选在 cands 中的下标。
func PickConsistentHash(cands []BalanceCandidate, clientIP string) int {
	if len(cands) == 0 {
		return -1
	}
	if len(cands) == 1 {
		return 0
	}
	key := strings.TrimSpace(clientIP)
	best := 0
	var bestScore uint64
	for i, c := range cands {
		score := hrwScore(key, c)
		if i == 0 || score > bestScore {
			best, bestScore = i, score
		}
	}
	return best
}

// hrwScore 计算 rendezvous 哈希得分。
func hrwScore(clientIP string, c BalanceCandidate) uint64 {
	h := sha256.New()
	h.Write([]byte(clientIP))
	h.Write([]byte("#"))
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], c.NodeID)
	h.Write(buf[:])
	sum := h.Sum(nil)
	return binary.BigEndian.Uint64(sum[:8])
}

// HashKey 计算任意字符串的稳定哈希值（FNV-1a 64 位）。
//
// 用于「用户 ID 固定出口」这类不需要防碰撞攻击的场景。
func HashKey(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

// ───────────────────────── 故障转移状态机（规格书 6.3） ─────────────────────────

// FailoverConfig 是故障判定阈值，对应设备组表上的健康检查字段。
type FailoverConfig struct {
	// MaxFail 连续失败次数阈值（入口组 config.max_fail，默认 3）。
	MaxFail int
	// FailTimeoutSec 故障判定超时（秒），对应 config.fail_timout_sec。
	FailTimeoutSec int
	// SuccCount 连续成功次数阈值后恢复（health_check_succ_count，默认 2）。
	SuccCount int
	// Enable 健康检查开关（health_check_enable）。
	Enable bool
}

// DefaultFailoverConfig 返回与规格书一致的默认阈值。
func DefaultFailoverConfig() FailoverConfig {
	return FailoverConfig{MaxFail: 3, FailTimeoutSec: 30, SuccCount: 2, Enable: true}
}

// Normalize 把未设置（<=0）的阈值填成默认值。
func (c FailoverConfig) Normalize() FailoverConfig {
	if c.MaxFail <= 0 {
		c.MaxFail = 3
	}
	if c.FailTimeoutSec <= 0 {
		c.FailTimeoutSec = 30
	}
	if c.SuccCount <= 0 {
		c.SuccCount = 2
	}
	return c
}

// FailureReason 是「计为一次失败」的原因（规格书 6.3）。
type FailureReason string

// 失败原因常量。
const (
	// FailHandshakeTimeout TCP 握手超时（超过 fail_timout_sec）。
	FailHandshakeTimeout FailureReason = "tcp_handshake_timeout"
	// FailConnRefused TCP 连接被拒绝（RST）。
	FailConnRefused FailureReason = "tcp_conn_refused"
	// FailNoData 已建立连接但在健康检查周期内无任何数据往返。
	FailNoData FailureReason = "no_data_in_period"
	// FailProbeError 探测自身出错（节点侧网络不可用等）。
	FailProbeError FailureReason = "probe_error"
)

// FailureReasonText 返回失败原因的中文说明，供 UI 直接展示。
func FailureReasonText(r FailureReason) string {
	switch r {
	case FailHandshakeTimeout:
		return "TCP 握手超时"
	case FailConnRefused:
		return "TCP 连接被拒绝（RST）"
	case FailNoData:
		return "健康检查周期内无数据往返"
	case FailProbeError:
		return "探测失败"
	}
	return string(r)
}

// IsFailure 判断一次探测结果是否计为失败。
//
// 满足任一即计为一次失败（规格书 6.3）：
//   - TCP 握手超时（超过 fail_timout_sec，单位毫秒）
//   - TCP 连接被拒绝（RST）
//   - 已建立连接但在健康检查周期内无任何数据往返
//
// 参数 ok 表示连接是否成功；reason 为失败原因；elapsedMs 为握手耗时；
// failTimeoutSec 为配置的超时阈值（秒，<=0 时用 30）。
// 返回是否计为一次失败。
func IsFailure(ok bool, reason FailureReason, elapsedMs int64, failTimeoutSec int) bool {
	if failTimeoutSec <= 0 {
		failTimeoutSec = 30
	}
	if !ok {
		switch reason {
		case FailHandshakeTimeout, FailConnRefused, FailNoData, FailProbeError:
			return true
		}
		// 未标注原因的失败一律计为失败，避免「静默成功」。
		return true
	}
	// 连接成功但握手耗时超过阈值，同样计为一次失败。
	return elapsedMs > int64(failTimeoutSec)*1000
}

// NodeFailoverState 是单个出口节点的连续成功/失败计数。
type NodeFailoverState struct {
	// ConsecutiveFail 连续失败次数。
	ConsecutiveFail int
	// ConsecutiveSucc 连续成功次数。
	ConsecutiveSucc int
	// Up 当前是否参与分发（false = 已被摘除，状态标记 down）。
	Up bool
	// LastReason 最近一次失败原因，供 UI 展示。
	LastReason FailureReason
}

// NewNodeFailoverState 构造初始状态：默认 up（新加入的节点先参与分发）。
func NewNodeFailoverState() NodeFailoverState { return NodeFailoverState{Up: true} }

// RecordResult 记录一次探测结果并推进状态机。
//
// 规则（规格书 6.3）：
//
//	连续失败次数 >= max_fail            → 摘除（Up = false）
//	连续成功次数 >= health_check_succ_count → 恢复（Up = true）
//
// 摘除之后仍会继续探测（失败会继续累加，成功则开始累加成功计数），
// 这样被摘除的节点才能自动恢复。
//
// 参数 fail 标记本次探测是否计为失败；reason 为失败原因。
// 返回本次调用是否**改变了**节点的 up/down 状态（供调用方决定是否落库 + 推送）。
func (s *NodeFailoverState) RecordResult(fail bool, reason FailureReason, cfg FailoverConfig) bool {
	cfg = cfg.Normalize()
	changed := false

	if fail {
		s.ConsecutiveFail++
		s.ConsecutiveSucc = 0
		s.LastReason = reason
		if s.Up && s.ConsecutiveFail >= cfg.MaxFail {
			s.Up = false
			changed = true
		}
		return changed
	}

	s.ConsecutiveSucc++
	s.ConsecutiveFail = 0
	if !s.Up && s.ConsecutiveSucc >= cfg.SuccCount {
		s.Up = true
		changed = true
	}
	return changed
}

// FailoverTracker 维护一组出口节点的故障转移状态。
//
// 与 SWRRState 一样，本类型**不是并发安全的**，由持有方（出口组选择器）加锁。
type FailoverTracker struct {
	// states 的键为节点 ID。
	states map[uint64]*NodeFailoverState
	cfg    FailoverConfig
}

// NewFailoverTracker 构造故障转移跟踪器。
func NewFailoverTracker(cfg FailoverConfig) *FailoverTracker {
	return &FailoverTracker{states: make(map[uint64]*NodeFailoverState), cfg: cfg.Normalize()}
}

// SetConfig 更新阈值（设备组健康检查参数被修改时调用）。
func (t *FailoverTracker) SetConfig(cfg FailoverConfig) { t.cfg = cfg.Normalize() }

// Config 返回当前阈值。
func (t *FailoverTracker) Config() FailoverConfig { return t.cfg }

// State 返回指定节点的状态副本；不存在的节点返回「默认 up」的零值。
func (t *FailoverTracker) State(nodeID uint64) NodeFailoverState {
	if s, ok := t.states[nodeID]; ok {
		return *s
	}
	return NewNodeFailoverState()
}

// Record 记录一次探测结果，返回是否发生了状态变化。
func (t *FailoverTracker) Record(nodeID uint64, fail bool, reason FailureReason) bool {
	st, ok := t.states[nodeID]
	if !ok {
		s := NewNodeFailoverState()
		st = &s
		t.states[nodeID] = st
	}
	return st.RecordResult(fail, reason, t.cfg)
}

// Down 强制把节点置为 down（管理员手工摘除或节点离线）。
func (t *FailoverTracker) Down(nodeID uint64) {
	st, ok := t.states[nodeID]
	if !ok {
		s := NewNodeFailoverState()
		st = &s
		t.states[nodeID] = st
	}
	st.Up = false
}

// Up 强制把节点恢复为 up（管理员手工恢复）。
func (t *FailoverTracker) Up(nodeID uint64) {
	st, ok := t.states[nodeID]
	if !ok {
		s := NewNodeFailoverState()
		st = &s
		t.states[nodeID] = st
	}
	st.Up = true
	st.ConsecutiveFail = 0
	st.ConsecutiveSucc = 0
}

// AllDown 判断给定节点集合是否**全部**被摘除。
//
// 这是「本组所有出口均为 down → 转投 FailoverGroupID」的判定入口（规格书 6.3）。
// 空集合返回 true（没有任何出口 = 等同于全挂）。
//
// 参数 nodeIDs 为当前设备组成员；返回是否全部不可用。
func (t *FailoverTracker) AllDown(nodeIDs []uint64) bool {
	if len(nodeIDs) == 0 {
		return true
	}
	for _, id := range nodeIDs {
		if t.State(id).Up {
			return false
		}
	}
	return true
}

// SelectExit 是「整组出口」层面的选路：按策略选节点，全挂时给出故障转移建议。
//
// 参数 group 为出口设备组；members 为该组的组成员投影；
// filter 为过滤条件（含「限制出口」）；opts 为可选参数。
//
// 返回值：选路结果，以及**是否应当转投故障转移组**。
// 当 Direct 为 true 且 NeedFailover 为 true 时，调用方 MUST 查找
// group.FailoverGroupID 指向的组并重新选路；若该组为空则回落到入口直出。
func SelectExit(group *model.DeviceGroup, members []BalanceCandidate,
	filter BalanceFilter, opts BalanceOptions) (BalanceResult, bool) {

	strategy := model.BalanceLeastConn
	if group != nil && group.Balance != "" {
		strategy = group.Balance
	}
	res := PickExit(strategy, members, filter, opts)
	if !res.Direct {
		return res, false
	}

	// 全挂：有故障转移组就转投，没有就走单端。
	needFailover := group != nil && group.FailoverGroupID > 0
	return res, needFailover
}

// ───────────────────────── 规则级多目标负载均衡（规格书 6.3） ─────────────────────────

// PickTarget 按目标级策略从规则的目标列表里选出一个目标。
//
// 参数 strategy 取 model.TargetBalance* 之一；targets 为规则的全部目标；
// clientIP 供 hash_ip 使用；seed 供加权随机使用（0 = 真随机）。
//
// 语义：
//   - failover（主备）：按顺序取第一个 up 的目标；
//     MUST 排在其它策略之前判断，因为主备不允许「跳跃使用后面的目标」；
//   - 其余策略与出口组一致，但只在 up 的目标中选。
//
// 返回值：选中的目标、其在入参切片中的下标，以及是否成功。
// 当所有目标都 down 时返回 ok = false，调用方据此判定该规则当前不可用。
func PickTarget(strategy string, targets []model.Target, clientIP string, seed int64) (model.Target, int, bool) {
	// 先算出所有 up 的目标下标，后续策略都在这个集合里选。
	upIdx := make([]int, 0, len(targets))
	for i, t := range targets {
		if t.Up() {
			upIdx = append(upIdx, i)
		}
	}
	if len(upIdx) == 0 {
		return model.Target{}, -1, false
	}

	cands := make([]BalanceCandidate, 0, len(upIdx))
	for pos, i := range upIdx {
		cands = append(cands, BalanceCandidate{
			// 用「在 up 集合中的位置」当 NodeID，便于把下标映射回来。
			NodeID:  uint64(pos),
			Name:    targets[i].String(),
			Weight:  targets[i].NormalizedWeight(),
			Online:  true,
			Healthy: true,
			Up:      true,
		})
	}

	var pick int
	switch strategy {
	case model.TargetBalanceRoundRobin:
		pick = PickSWRR(cands, nil)
	case model.TargetBalanceHashIP:
		pick = PickConsistentHash(cands, clientIP)
	case model.TargetBalanceWeighted:
		pick = PickWeightedRandom(cands, newLocalRand(seed))
	case model.TargetBalanceLeastConn:
		// 目标级没有实时连接数，退化为按权重随机（与 Nyanpass 行为一致）。
		pick = PickWeightedRandom(cands, newLocalRand(seed))
	default:
		// failover（主备）：按顺序取第一个 up 的目标。
		pick = 0
	}
	if pick < 0 || pick >= len(cands) {
		pick = 0
	}
	idx := upIdx[pick]
	return targets[idx], idx, true
}

// PickTargetWithFailover 在主备（failover）策略下按「跳过连续失败目标」选路。
//
// 参数 failures 记录每个目标（按 host:port 归一化键）的连续失败次数；
// maxFail 为阈值（<=0 时取 3）。连续失败达到阈值的目标被视为 down，
// 但**不会**写回数据库——写回由调用方在探测任务里完成，这里只做本次选路裁决。
//
// 返回值：选中的目标与下标；全部被跳过时 ok = false。
func PickTargetWithFailover(targets []model.Target, failures map[string]int, maxFail int) (model.Target, int, bool) {
	if maxFail <= 0 {
		maxFail = 3
	}
	for i, t := range targets {
		if !t.Up() {
			continue
		}
		if failures[TargetKey(t)] >= maxFail {
			continue
		}
		return t, i, true
	}
	return model.Target{}, -1, false
}

// TargetKey 返回目标在失败计数表里的归一化键。
func TargetKey(t model.Target) string {
	return strings.ToLower(strings.TrimSpace(t.Host)) + ":" + strconv.Itoa(t.Port)
}

// MarkTargetDown 把指定下标的目标标记为 down（目标级故障转移）。
//
// 标记结果由调用方写回规则的 Targets 字段（本函数只改内存副本）。
// 返回是否发生了变更。
func MarkTargetDown(targets []model.Target, idx int) bool {
	if idx < 0 || idx >= len(targets) {
		return false
	}
	if targets[idx].Status == model.TargetDown {
		return false
	}
	targets[idx].Status = model.TargetDown
	return true
}

// MarkTargetUp 把指定下标的目标恢复为 up。
func MarkTargetUp(targets []model.Target, idx int) bool {
	if idx < 0 || idx >= len(targets) {
		return false
	}
	if targets[idx].Status == model.TargetUp || targets[idx].Status == "" {
		return false
	}
	targets[idx].Status = model.TargetUp
	return true
}

// SortTargetsByPriority 按「up 优先 + 原顺序」重排目标列表。
//
// 语义：up 的目标保持原有相对顺序排在前，down 的排在后面。
// 这是 UI 上「拖拽调整目标优先级」的落地实现：管理员拖动后，
// 面板把新的顺序写回 Targets 数组即可。
func SortTargetsByPriority(targets []model.Target) []model.Target {
	out := make([]model.Target, 0, len(targets))
	for _, t := range targets {
		if t.Up() {
			out = append(out, t)
		}
	}
	for _, t := range targets {
		if !t.Up() {
			out = append(out, t)
		}
	}
	return out
}

// ───────────────────────── 负载分布统计（规格书 8.8 /health） ─────────────────────────

// LoadDistribution 是单个出口节点在组内的负载占比。
type LoadDistribution struct {
	NodeID      uint64 `json:"node_id"`
	Name        string `json:"name"`
	Weight      int    `json:"weight"`
	CurrentConn int    `json:"current_conn"`
	MaxConn     int    `json:"max_conn"`
	// Ratio 是 current_conn / weight，即 least_conn 的实际比较键。
	Ratio float64 `json:"ratio"`
	// Share 是该节点在组内的相对承载占比（0~1，按权重归一化）。
	Share float64 `json:"share"`
}

// ComputeLoadDistribution 计算组内负载分布，供 GET /device-groups/:id/health 使用。
//
// 参数 members 为组成员投影；返回按节点顺序排列的分布列表。
// Share 是「理论承载占比」= weight / Σweight，而不是实时流量占比——
// 后者需要采集节点上报的速率，属于探针的职责。
func ComputeLoadDistribution(members []BalanceCandidate) []LoadDistribution {
	out := make([]LoadDistribution, 0, len(members))
	total := 0
	for _, c := range members {
		if c.Weight > 0 {
			total += c.Weight
		}
	}
	for _, c := range members {
		share := 0.0
		if total > 0 && c.Weight > 0 {
			share = float64(c.Weight) / float64(total)
		}
		out = append(out, LoadDistribution{
			NodeID:      c.NodeID,
			Name:        c.Name,
			Weight:      c.EffectiveWeight(),
			CurrentConn: c.CurrentConn,
			MaxConn:     c.MaxConn,
			Ratio:       c.LoadRatio(),
			Share:       share,
		})
	}
	return out
}

// SortCandidatesByNodeOrder 按设备组的 node_ids 顺序重排候选。
//
// 出口组的顺序参与负载均衡初始化与并列打破（规格书 6.3），
// 因此 UI 上的拖拽排序必须在这里被真正执行，而不只是展示层面的排序。
//
// 参数 list 为原始候选；order 为设备组的 node_ids（有序）。
// 未出现在 order 中的候选被丢弃（组成员已被移出组）。
func SortCandidatesByNodeOrder(list []BalanceCandidate, order []uint64) []BalanceCandidate {
	index := make(map[uint64]int, len(order))
	for i, id := range order {
		index[id] = i
	}
	out := make([]BalanceCandidate, 0, len(list))
	for _, c := range list {
		if _, ok := index[c.NodeID]; ok {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return index[out[i].NodeID] < index[out[j].NodeID]
	})
	return out
}
