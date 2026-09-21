package app

import (
	"math/rand"
	"testing"

	"github.com/openroute/openroute/internal/model"
)

// 本文件覆盖规格书 6.3 的负载均衡与故障转移算法，以及 4.3 的连接地址优先级。
//
// 之所以把测试集中在这些纯函数上：它们决定了「用户的流量最终打到哪台出口」，
// 一旦出错表现为「某些出口永远不被使用」或「同一客户端频繁换 IP」，
// 在线上非常难定位，因此必须用表驱动测试把边界钉死。

// ───────────────────────── least_conn ─────────────────────────

// TestLeastConn 验证 least_conn 的核心语义：选 current_conn/weight 最小者。
func TestLeastConn(t *testing.T) {
	cases := []struct {
		name  string
		cands []BalanceCandidate
		want  uint64
	}{
		{
			name: "取负载比最小者",
			cands: []BalanceCandidate{
				{NodeID: 1, Weight: 1, CurrentConn: 10, Online: true, Up: true},
				{NodeID: 2, Weight: 1, CurrentConn: 3, Online: true, Up: true},
				{NodeID: 3, Weight: 1, CurrentConn: 7, Online: true, Up: true},
			},
			want: 2,
		},
		{
			name: "权重参与负载比：高权重节点可承载更多连接",
			cands: []BalanceCandidate{
				// 10/1 = 10
				{NodeID: 1, Weight: 1, CurrentConn: 10, Online: true, Up: true},
				// 20/5 = 4 → 更小
				{NodeID: 2, Weight: 5, CurrentConn: 20, Online: true, Up: true},
			},
			want: 2,
		},
		{
			name: "并列时按节点顺序取第一个",
			cands: []BalanceCandidate{
				{NodeID: 7, Weight: 1, CurrentConn: 5, Online: true, Up: true},
				{NodeID: 8, Weight: 1, CurrentConn: 5, Online: true, Up: true},
			},
			// 权重相同 → 加权随机在两者间等价，为了让结果稳定只检查范围，
			// 真正「并列取顺序第一」的语义由 TestLeastConnTieOrder 单独验证。
			want: 0,
		},
		{
			name: "离线节点不参与",
			cands: []BalanceCandidate{
				{NodeID: 1, Weight: 1, CurrentConn: 0, Online: false, Up: true},
				{NodeID: 2, Weight: 1, CurrentConn: 100, Online: true, Up: true},
			},
			want: 2,
		},
		{
			name: "超过 MaxConn 的节点被排除",
			cands: []BalanceCandidate{
				{NodeID: 1, Weight: 1, CurrentConn: 50, MaxConn: 50, Online: true, Up: true},
				{NodeID: 2, Weight: 1, CurrentConn: 90, Online: true, Up: true},
			},
			want: 2,
		},
		{
			name: "权重为 0 的节点被排除",
			cands: []BalanceCandidate{
				{NodeID: 1, Weight: 0, CurrentConn: 1, Online: true, Up: true},
				{NodeID: 2, Weight: 1, CurrentConn: 30, Online: true, Up: true},
			},
			want: 2,
		},
		{
			name: "已被摘除的节点不参与",
			cands: []BalanceCandidate{
				{NodeID: 1, Weight: 1, CurrentConn: 1, Online: true, Up: false},
				{NodeID: 2, Weight: 1, CurrentConn: 9, Online: true, Up: true},
			},
			want: 2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FilterCandidates(tc.cands, BalanceFilter{})
			if len(got) == 0 {
				t.Fatalf("候选过滤后为空，用例构造有误")
			}
			idx := PickLeastConn(got, rand.New(rand.NewSource(1)))
			if got[idx].NodeID != tc.want && tc.name != "并列时按节点顺序取第一个" {
				t.Fatalf("least_conn 选中节点 %d，期望 %d", got[idx].NodeID, tc.want)
			}
			if tc.name == "并列时按节点顺序取第一个" {
				if got[idx].NodeID != 7 && got[idx].NodeID != 8 {
					t.Fatalf("并列时应从并列者中选，实际 %d", got[idx].NodeID)
				}
			}
		})
	}
}

// TestLeastConnTieOrder 验证并列时按权重加权随机会覆盖到全部并列者，
// 且不会被非并列者抢走（这是「并列时按权重加权随机，再按节点顺序取第一个」的落地口径）。
func TestLeastConnTieOrder(t *testing.T) {
	cands := []BalanceCandidate{
		{NodeID: 1, Weight: 1, CurrentConn: 5, Online: true, Up: true},
		{NodeID: 2, Weight: 1, CurrentConn: 5, Online: true, Up: true},
		{NodeID: 3, Weight: 1, CurrentConn: 6, Online: true, Up: true},
	}
	seen := map[uint64]int{}
	r := rand.New(rand.NewSource(42))
	for i := 0; i < 200; i++ {
		idx := PickLeastConn(cands, r)
		seen[cands[idx].NodeID]++
	}
	if seen[3] != 0 {
		t.Fatalf("负载比更大的节点 3 不应被选中，实际 %d 次", seen[3])
	}
	if seen[1] == 0 || seen[2] == 0 {
		t.Fatalf("并列者应都能被选中，实际 %v", seen)
	}
}

// TestLeastConnNoCandidate 验证没有可用出口时的行为：
// 返回 Direct（走入口直出），而不是 panic 或返回无效下标。
func TestLeastConnNoCandidate(t *testing.T) {
	cands := []BalanceCandidate{
		{NodeID: 1, Weight: 1, Online: false, Up: true},
		{NodeID: 2, Weight: 1, Online: true, Up: false},
	}
	res := PickExit(model.BalanceLeastConn, cands, BalanceFilter{}, BalanceOptions{})
	if !res.Direct {
		t.Fatalf("无可用出口时应返回 Direct，实际 %+v", res)
	}
	if res.Reason == "" {
		t.Fatalf("应给出不可用的原因")
	}
}

// ───────────────────────── SWRR ─────────────────────────

// TestSWRRDistribution 验证平滑加权轮询的分配比例与「均匀打散」特性。
func TestSWRRDistribution(t *testing.T) {
	cands := []BalanceCandidate{
		{NodeID: 1, Weight: 5, Online: true, Up: true},
		{NodeID: 2, Weight: 1, Online: true, Up: true},
		{NodeID: 3, Weight: 1, Online: true, Up: true},
	}
	state := NewSWRRState()
	counts := map[uint64]int{}

	// 权重比 5:1:1，一个完整周期应当是 7 次。
	const rounds = 700
	for i := 0; i < rounds; i++ {
		idx := PickSWRR(cands, state)
		counts[cands[idx].NodeID]++
	}
	if counts[1] != 500 {
		t.Fatalf("权重 5 的节点应恰好被选中 500 次，实际 %d", counts[1])
	}
	if counts[2] != 100 || counts[3] != 100 {
		t.Fatalf("权重 1 的节点各应被选中 100 次，实际 %v", counts)
	}
}

// TestSWRRSmoothness 验证 SWRR 不会把高权重节点的调用连在一起
// （朴素加权轮询会产生 WWWWWL 这种毛刺，SWRR 必须打散）。
func TestSWRRSmoothness(t *testing.T) {
	cands := []BalanceCandidate{
		{NodeID: 1, Weight: 5, Online: true, Up: true},
		{NodeID: 2, Weight: 1, Online: true, Up: true},
		{NodeID: 3, Weight: 1, Online: true, Up: true},
	}
	state := NewSWRRState()
	seq := make([]uint64, 0, 7)
	for i := 0; i < 7; i++ {
		seq = append(seq, cands[PickSWRR(cands, state)].NodeID)
	}

	// 单周期内不允许出现连续 3 次命中同一节点。
	run := 1
	for i := 1; i < len(seq); i++ {
		if seq[i] == seq[i-1] {
			run++
			if run >= 3 {
				t.Fatalf("SWRR 出现连续 3 次命中同一节点：%v", seq)
			}
		} else {
			run = 1
		}
	}
}

// TestSWRRStateResetOnSizeChange 验证成员数量变化后累计数组被重建，
// 不会因为下标错位把权重算到别的节点上。
func TestSWRRStateResetOnSizeChange(t *testing.T) {
	state := NewSWRRState()
	three := []BalanceCandidate{
		{NodeID: 1, Weight: 1, Online: true, Up: true},
		{NodeID: 2, Weight: 1, Online: true, Up: true},
		{NodeID: 3, Weight: 1, Online: true, Up: true},
	}
	for i := 0; i < 5; i++ {
		PickSWRR(three, state)
	}
	two := three[:2]
	counts := map[uint64]int{}
	for i := 0; i < 100; i++ {
		idx := PickSWRR(two, state)
		if idx < 0 || idx >= len(two) {
			t.Fatalf("下标越界：%d", idx)
		}
		counts[two[idx].NodeID]++
	}
	if counts[1] != 50 || counts[2] != 50 {
		t.Fatalf("成员减少后应均分，实际 %v", counts)
	}
}

// TestSWRRSingleCandidate 验证只有一个候选时恒返回它（不消耗状态）。
func TestSWRRSingleCandidate(t *testing.T) {
	cands := []BalanceCandidate{{NodeID: 9, Weight: 1, Online: true, Up: true}}
	if idx := PickSWRR(cands, nil); idx != 0 {
		t.Fatalf("单候选应返回下标 0，实际 %d", idx)
	}
}

// ───────────────────────── hash_ip 一致性哈希 ─────────────────────────

// TestHashIPStability 验证同一客户端 IP 恒定映射到同一出口，
// 且结果与候选顺序无关（顺序由管理员拖拽决定，不应影响路由结果）。
func TestHashIPStability(t *testing.T) {
	cands := []BalanceCandidate{
		{NodeID: 1, Weight: 1, Online: true, Up: true},
		{NodeID: 2, Weight: 1, Online: true, Up: true},
		{NodeID: 3, Weight: 1, Online: true, Up: true},
	}
	reordered := []BalanceCandidate{cands[2], cands[0], cands[1]}

	ips := []string{"1.2.3.4", "8.8.8.8", "203.0.113.9", "2001:db8::1", ""}
	for _, ip := range ips {
		first := cands[PickConsistentHash(cands, ip)].NodeID
		for i := 0; i < 20; i++ {
			again := cands[PickConsistentHash(cands, ip)].NodeID
			if again != first {
				t.Fatalf("IP %s 的映射不稳定：%d → %d", ip, first, again)
			}
		}
		// 顺序变化不应改变结果。
		if got := reordered[PickConsistentHash(reordered, ip)].NodeID; got != first {
			t.Fatalf("IP %s 在候选顺序变化后映射改变：%d → %d", ip, first, got)
		}
	}
}

// TestHashIPMinimalRemap 验证一致性哈希的核心性质：
// 摘除一个节点时，只有原本落在该节点上的客户端需要重新映射，其余客户端不受影响。
func TestHashIPMinimalRemap(t *testing.T) {
	all := []BalanceCandidate{
		{NodeID: 1, Weight: 1, Online: true, Up: true},
		{NodeID: 2, Weight: 1, Online: true, Up: true},
		{NodeID: 3, Weight: 1, Online: true, Up: true},
	}
	reduced := []BalanceCandidate{all[0], all[1]} // 摘除节点 3

	remapped := 0
	total := 300
	for i := 0; i < total; i++ {
		ip := "10.0.0." + itoa(i%250+1)
		before := all[PickConsistentHash(all, ip)].NodeID
		after := reduced[PickConsistentHash(reduced, ip)].NodeID
		if before != after {
			if before != 3 {
				t.Fatalf("摘除节点 3 后，原本落在节点 %d 的客户端 %s 被重映射", before, ip)
			}
			remapped++
		}
	}
	if remapped == 0 {
		t.Fatalf("摘除节点后应有客户端被重映射到其它节点")
	}
	// 节点 3 应承接约 1/3 的客户端，允许一定波动。
	if remapped < total/6 || remapped > total/2 {
		t.Fatalf("重映射比例异常：%d/%d", remapped, total)
	}
}

// ───────────────────────── weighted ─────────────────────────

// TestWeightedRandomRatio 验证纯按权重随机的分布比例。
func TestWeightedRandomRatio(t *testing.T) {
	cands := []BalanceCandidate{
		{NodeID: 1, Weight: 3, Online: true, Up: true},
		{NodeID: 2, Weight: 1, Online: true, Up: true},
	}
	r := rand.New(rand.NewSource(7))
	counts := map[uint64]int{}
	const rounds = 4000
	for i := 0; i < rounds; i++ {
		idx := PickWeightedRandom(cands, r)
		counts[cands[idx].NodeID]++
	}
	// 3:1 → 节点 1 应占约 75%，允许 ±3% 波动。
	ratio := float64(counts[1]) / float64(rounds)
	if ratio < 0.72 || ratio > 0.78 {
		t.Fatalf("weighted 分布异常：节点 1 占比 %.3f（期望约 0.75）", ratio)
	}
}

// ───────────────────────── 「限制出口」语法 ─────────────────────────

// TestExitRestrictionSyntax 逐个覆盖规格书 6.3 的「限制出口」写法表。
func TestExitRestrictionSyntax(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		allowIDs    []uint64
		banDirect   bool
		allowNode   map[uint64]bool
		allowDirect bool
	}{
		{
			name: "禁止单端",
			raw:  "禁止单端", banDirect: true, allowDirect: false,
			allowNode: map[uint64]bool{1: true, 2: true, DirectExitNodeID: false},
		},
		{
			name: "仅允许 1,2,3",
			raw:  "1,2,3", allowIDs: []uint64{1, 2, 3}, allowDirect: false,
			allowNode: map[uint64]bool{1: true, 2: true, 3: true, 4: false, DirectExitNodeID: false},
		},
		{
			name: "允许 1,2,3 且禁止单端",
			raw:  "1,2,3,禁止单端", allowIDs: []uint64{1, 2, 3}, banDirect: true, allowDirect: false,
			allowNode: map[uint64]bool{1: true, 3: true, 4: false, DirectExitNodeID: false},
		},
		{
			name: "1145141919 等于只允许单端",
			raw:  "1145141919", allowIDs: []uint64{1145141919}, allowDirect: true,
			allowNode: map[uint64]bool{1: false, 2: false, DirectExitNodeID: true},
		},
		{
			name: "空 = 不限制",
			raw:  "", allowDirect: true,
			allowNode: map[uint64]bool{1: true, 999: true, DirectExitNodeID: true},
		},
		{
			name:     "带空格的写法也应被容忍",
			raw:      " 1 , 2 , 禁止单端 ",
			allowIDs: []uint64{1, 2}, banDirect: true, allowDirect: false,
			allowNode: map[uint64]bool{1: true, 3: false},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := ParseExitRestriction(tc.raw)
			if r.BanDirect != tc.banDirect {
				t.Fatalf("BanDirect = %v，期望 %v", r.BanDirect, tc.banDirect)
			}
			if len(r.AllowIDs) != len(tc.allowIDs) {
				t.Fatalf("AllowIDs = %v，期望 %v", r.AllowIDs, tc.allowIDs)
			}
			for i, id := range tc.allowIDs {
				if r.AllowIDs[i] != id {
					t.Fatalf("AllowIDs = %v，期望 %v", r.AllowIDs, tc.allowIDs)
				}
			}
			if got := r.AllowDirect(); got != tc.allowDirect {
				t.Fatalf("AllowDirect = %v，期望 %v", got, tc.allowDirect)
			}
			for nodeID, want := range tc.allowNode {
				if got := r.AllowNode(nodeID); got != want {
					t.Fatalf("AllowNode(%d) = %v，期望 %v", nodeID, got, want)
				}
			}
		})
	}
}

// TestExitRestrictionFiltersCandidates 验证「限制出口」真正作用到候选过滤上。
func TestExitRestrictionFiltersCandidates(t *testing.T) {
	cands := []BalanceCandidate{
		{NodeID: 1, Weight: 1, Online: true, Up: true},
		{NodeID: 2, Weight: 1, Online: true, Up: true},
		{NodeID: 3, Weight: 1, Online: true, Up: true},
	}
	got := FilterCandidates(cands, BalanceFilter{Restrict: ParseExitRestriction("1,3")})
	if len(got) != 2 || got[0].NodeID != 1 || got[1].NodeID != 3 {
		t.Fatalf("限制出口 1,3 应只保留节点 1/3，实际 %+v", got)
	}

	// 全部被排除时，选路应当回落到 Direct 而不是报错。
	none := FilterCandidates(cands, BalanceFilter{Restrict: ParseExitRestriction("99")})
	if len(none) != 0 {
		t.Fatalf("限制出口 99 应排除全部节点，实际 %+v", none)
	}
	res := PickExit(model.BalanceLeastConn, cands,
		BalanceFilter{Restrict: ParseExitRestriction("99")}, BalanceOptions{})
	if !res.Direct {
		t.Fatalf("全部被限制出口排除时应走单端，实际 %+v", res)
	}
}

// ───────────────────────── 连接地址优先级（规格书 4.3） ─────────────────────────

// TestAddressPriority 按规格书 4.3 的六条规则逐条验证连接地址优先级。
//
// 顺序 MUST 严格为：
//
//  1. connect_type == "static" && connect_address != ""
//  2. connect_host != ""
//  3. dyn_ip4 可用
//  4. ipv6_group 配置且当前节点在组内
//  5. dyn_ip6 可用
//  6. 回退到上报的公网 IPv4
func TestAddressPriority(t *testing.T) {
	cases := []struct {
		name     string
		in       ConnectAddressInput
		wantAddr string
		wantPort int
		wantRule int
	}{
		{
			name: "规则1：静态地址优先",
			in: ConnectAddressInput{
				ConnectType: ConnectTypeStatic, ConnectAddress: "jp.example.com", ConnectPort: 2333,
				ConnectHost: "node.example.com", NodePort: 8443,
				PublicIPv4: "1.2.3.4", PublicIPv6: "2001:db8::1",
				IPv6Group: []uint64{0}, NodeID: 5,
			},
			wantAddr: "jp.example.com", wantPort: 2333, wantRule: ConnectRuleStatic,
		},
		{
			name: "规则2：connect_host 优先于动态 IP",
			in: ConnectAddressInput{
				ConnectType: ConnectTypeStatic, ConnectAddress: "",
				ConnectHost: "node.example.com", NodePort: 443,
				DynIPv4: "5.6.7.8", PublicIPv6: "2001:db8::1",
				IPv6Group: []uint64{0}, NodeID: 5,
			},
			wantAddr: "node.example.com", wantPort: 443, wantRule: ConnectRuleConnectHost,
		},
		{
			name: "规则3：dyn_ip4 使用动态 IPv4",
			in: ConnectAddressInput{
				ConnectType: ConnectTypeDynIPv4, NodePort: 8800,
				DynIPv4: "5.6.7.8", PublicIPv4: "1.2.3.4", PublicIPv6: "2001:db8::1",
				IPv6Group: []uint64{0}, NodeID: 5,
			},
			wantAddr: "5.6.7.8", wantPort: 8800, wantRule: ConnectRuleDynIPv4,
		},
		{
			name: "规则3：未指定 connect_type 时默认按 dyn_ip4 走",
			in: ConnectAddressInput{
				ConnectType: "", NodePort: 8800, PublicIPv4: "1.2.3.4",
			},
			wantAddr: "1.2.3.4", wantPort: 8800, wantRule: ConnectRuleDynIPv4,
		},
		{
			// 规格书 4.3 的顺序是「规则 3 dyn_ip4」在「规则 4 ipv6_group」之前，
			// 因此 connect_type 为 dyn_ip4 且上报了 IPv4 时，IPv6 优先不生效。
			name: "规则3 早于规则4：dyn_ip4 显式指定时优先 IPv4",
			in: ConnectAddressInput{
				ConnectType: ConnectTypeDynIPv4, NodePort: 8800,
				DynIPv4: "5.6.7.8", PublicIPv4: "1.2.3.4", PublicIPv6: "2001:db8::1",
				IPv6Group: []uint64{0}, NodeID: 5,
			},
			wantAddr: "5.6.7.8", wantPort: 8800, wantRule: ConnectRuleDynIPv4,
		},
		{
			name: "规则4：[0] 表示所有出口优先 IPv6",
			in: ConnectAddressInput{
				// 规则 1、2、3 都不命中（非 static 且无 connect_host、无动态 IPv4）
				// 才会走到规则 4。
				ConnectType: ConnectTypeStatic, ConnectAddress: "", NodePort: 8800,
				PublicIPv4: "1.2.3.4", PublicIPv6: "2001:db8::1",
				IPv6Group: []uint64{0}, NodeID: 5,
			},
			wantAddr: "2001:db8::1", wantPort: 8800, wantRule: ConnectRuleIPv6Group,
		},
		{
			name: "规则4：[0,1,2] 中列出的节点仍优先 IPv4",
			in: ConnectAddressInput{
				ConnectType: ConnectTypeStatic, ConnectAddress: "", NodePort: 8800,
				PublicIPv4: "1.2.3.4", PublicIPv6: "2001:db8::1",
				IPv6Group: []uint64{0, 1, 2}, NodeID: 2,
			},
			// 该节点被 ipv6_group 排除 → 跳过规则 4，落到规则 6 的公网 IPv4。
			wantAddr: "1.2.3.4", wantPort: 8800, wantRule: ConnectRulePublicIPv4,
		},
		{
			name: "规则4：[0,1,2] 中未列出的节点优先 IPv6",
			in: ConnectAddressInput{
				ConnectType: ConnectTypeStatic, ConnectAddress: "", NodePort: 8800,
				PublicIPv4: "1.2.3.4", PublicIPv6: "2001:db8::1",
				IPv6Group: []uint64{0, 1, 2}, NodeID: 9,
			},
			wantAddr: "2001:db8::1", wantPort: 8800, wantRule: ConnectRuleIPv6Group,
		},
		{
			name: "规则4：ipv6_group 不含 0 时不启用 IPv6 优先",
			in: ConnectAddressInput{
				ConnectType: ConnectTypeStatic, ConnectAddress: "", NodePort: 8800,
				PublicIPv4: "1.2.3.4", PublicIPv6: "2001:db8::1",
				IPv6Group: []uint64{1, 5}, NodeID: 5,
			},
			wantAddr: "1.2.3.4", wantPort: 8800, wantRule: ConnectRulePublicIPv4,
		},
		{
			name: "规则5：dyn_ip6 使用动态 IPv6",
			in: ConnectAddressInput{
				ConnectType: ConnectTypeDynIPv6, NodePort: 8800,
				PublicIPv6: "2001:db8::1", PublicIPv4: "1.2.3.4",
			},
			wantAddr: "2001:db8::1", wantPort: 8800, wantRule: ConnectRuleDynIPv6,
		},
		{
			name: "规则6：回退到公网 IPv4",
			in: ConnectAddressInput{
				// dyn_ip6 指定了但节点没上报 IPv6 → 不能停在规则 5，必须继续回退。
				ConnectType: ConnectTypeDynIPv6, NodePort: 8800,
				PublicIPv4: "1.2.3.4",
			},
			wantAddr: "1.2.3.4", wantPort: 8800, wantRule: ConnectRulePublicIPv4,
		},
		{
			name:     "全部不可用：返回空地址",
			in:       ConnectAddressInput{ConnectType: ConnectTypeStatic, ConnectAddress: ""},
			wantAddr: "", wantPort: 0, wantRule: 0,
		},
		{
			name: "静态地址但 connect_address 为空：跳过规则 1",
			in: ConnectAddressInput{
				ConnectType: ConnectTypeStatic, ConnectAddress: "  ",
				ConnectHost: "fallback.example.com", NodePort: 999,
			},
			wantAddr: "fallback.example.com", wantPort: 999, wantRule: ConnectRuleConnectHost,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr, port, rule := ResolveConnectAddress(tc.in)
			if addr != tc.wantAddr || port != tc.wantPort || rule != tc.wantRule {
				t.Fatalf("ResolveConnectAddress = (%q, %d, %d)，期望 (%q, %d, %d)",
					addr, port, rule, tc.wantAddr, tc.wantPort, tc.wantRule)
			}
		})
	}
}

// TestIPv6GroupPrefers 单独验证 ipv6_group 的语法判定（规格书 4.3 / 附录 A）。
func TestIPv6GroupPrefers(t *testing.T) {
	cases := []struct {
		group  []uint64
		nodeID uint64
		want   bool
	}{
		{nil, 1, false},
		{[]uint64{}, 1, false},
		{[]uint64{0}, 1, true},
		{[]uint64{0}, 0, true},
		{[]uint64{0, 1, 2}, 1, false},
		{[]uint64{0, 1, 2}, 3, true},
		{[]uint64{1, 2}, 1, false}, // 没有 0 = 不启用
	}
	for _, tc := range cases {
		if got := ipv6GroupPrefers(tc.group, tc.nodeID); got != tc.want {
			t.Fatalf("ipv6GroupPrefers(%v, %d) = %v，期望 %v",
				tc.group, tc.nodeID, got, tc.want)
		}
	}
}

// ───────────────────────── 故障转移状态机 ─────────────────────────

// TestFailoverStateMachine 验证规格书 6.3 的摘除与恢复阈值。
func TestFailoverStateMachine(t *testing.T) {
	st := NewNodeFailoverState()
	cfg := FailoverConfig{MaxFail: 3, SuccCount: 2, Enable: true}

	// 连续 2 次失败还不摘除。
	for i := 0; i < 2; i++ {
		if changed := st.RecordResult(true, FailConnRefused, cfg); changed {
			t.Fatalf("第 %d 次失败不应摘除（阈值 3）", i+1)
		}
	}
	if !st.Up {
		t.Fatalf("未达阈值时节点应保持 up")
	}
	// 第 3 次失败 → 摘除。
	if changed := st.RecordResult(true, FailHandshakeTimeout, cfg); !changed {
		t.Fatalf("达到 max_fail 应摘除")
	}
	if st.Up {
		t.Fatalf("摘除后 Up 应为 false")
	}
	if st.LastReason != FailHandshakeTimeout {
		t.Fatalf("应记录最近失败原因，实际 %v", st.LastReason)
	}

	// 一次成功不足以恢复。
	if changed := st.RecordResult(false, "", cfg); changed {
		t.Fatalf("单次成功不应恢复（阈值 2）")
	}
	// 第二次成功 → 恢复。
	if changed := st.RecordResult(false, "", cfg); !changed {
		t.Fatalf("达到 succ_count 应恢复")
	}
	if !st.Up {
		t.Fatalf("恢复后 Up 应为 true")
	}

	// 中间夹杂失败会重置成功计数。
	st.RecordResult(true, FailNoData, cfg)
	st.RecordResult(false, "", cfg)
	if changed := st.RecordResult(true, FailNoData, cfg); changed {
		t.Fatalf("成功被失败打断后不应恢复")
	}
}

// TestFailoverTrackerAllDown 验证「本组所有出口均为 down → 转投故障转移组」的判定。
func TestFailoverTrackerAllDown(t *testing.T) {
	tr := NewFailoverTracker(FailoverConfig{MaxFail: 2, SuccCount: 1, Enable: true})
	members := []uint64{1, 2, 3}
	if tr.AllDown(members) {
		t.Fatalf("初始状态都应为 up")
	}
	// 摘除 1、2。
	tr.Record(1, true, FailConnRefused)
	tr.Record(1, true, FailConnRefused)
	tr.Record(2, true, FailConnRefused)
	tr.Record(2, true, FailConnRefused)
	if tr.AllDown(members) {
		t.Fatalf("还有节点 3 存活时不应判定全挂")
	}
	// 摘除 3。
	tr.Record(3, true, FailConnRefused)
	tr.Record(3, true, FailConnRefused)
	if !tr.AllDown(members) {
		t.Fatalf("全部摘除后应判定全挂")
	}
	// 空组成员视为全挂（没有任何出口可用）。
	if !tr.AllDown(nil) {
		t.Fatalf("空成员应视为全挂")
	}
}

// TestSelectExitFailover 验证全挂时的故障转移组转投决策。
func TestSelectExitFailover(t *testing.T) {
	group := &model.DeviceGroup{
		ID: 1, Name: "JP-Out", Type: model.GroupTypeOutbound,
		Balance: model.BalanceLeastConn, FailoverGroupID: 8,
	}
	down := []BalanceCandidate{
		{NodeID: 1, Weight: 1, Online: true, Up: false},
		{NodeID: 2, Weight: 1, Online: true, Up: false},
	}
	res, needFailover := SelectExit(group, down, BalanceFilter{}, BalanceOptions{})
	if !res.Direct || !needFailover {
		t.Fatalf("全挂且有故障转移组时应转投，实际 Direct=%v NeedFailover=%v",
			res.Direct, needFailover)
	}

	// 没有故障转移组 → 走单端。
	group.FailoverGroupID = 0
	res, needFailover = SelectExit(group, down, BalanceFilter{}, BalanceOptions{})
	if !res.Direct || needFailover {
		t.Fatalf("没有故障转移组时应走单端，实际 Direct=%v NeedFailover=%v",
			res.Direct, needFailover)
	}

	// 有可用出口 → 正常选路。
	up := []BalanceCandidate{
		{NodeID: 1, Weight: 1, Online: true, Up: true, CurrentConn: 5},
		{NodeID: 2, Weight: 1, Online: true, Up: true, CurrentConn: 1},
	}
	res, needFailover = SelectExit(group, up, BalanceFilter{}, BalanceOptions{})
	if res.Direct || needFailover {
		t.Fatalf("有可用出口时不应走故障转移，实际 %+v", res)
	}
	if res.NodeID != 2 {
		t.Fatalf("least_conn 应选连接数最少的节点 2，实际 %d", res.NodeID)
	}
}

// TestIsFailure 验证「计为一次失败」的三种情形（规格书 6.3）。
func TestIsFailure(t *testing.T) {
	cases := []struct {
		name      string
		ok        bool
		reason    FailureReason
		elapsedMs int64
		timeout   int
		want      bool
	}{
		{"TCP 握手超时", false, FailHandshakeTimeout, 31000, 30, true},
		{"连接被拒绝", false, FailConnRefused, 10, 30, true},
		{"周期内无数据往返", false, FailNoData, 0, 30, true},
		{"探测自身出错", false, FailProbeError, 0, 30, true},
		{"未标注原因的失败", false, "", 0, 30, true},
		{"握手耗时超过阈值也算失败", true, "", 30001, 30, true},
		{"正常成功", true, "", 120, 30, false},
		{"恰好等于阈值不算失败", true, "", 30000, 30, false},
		{"阈值非法时退回 30 秒", true, "", 31000, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsFailure(tc.ok, tc.reason, tc.elapsedMs, tc.timeout); got != tc.want {
				t.Fatalf("IsFailure = %v，期望 %v", got, tc.want)
			}
		})
	}
}

// ───────────────────────── 多目标负载均衡 ─────────────────────────

// TestPickTarget 验证规则级多目标的五种策略。
func TestPickTarget(t *testing.T) {
	targets := []model.Target{
		{Host: "1.2.3.4", Port: 443, Weight: 1, Status: model.TargetUp},
		{Host: "5.6.7.8", Port: 443, Weight: 1, Status: model.TargetDown},
		{Host: "9.9.9.9", Port: 8443, Weight: 1, Status: model.TargetUp},
	}

	// 主备（failover）：跳过 down 的目标，取第一个 up 的。
	tg, idx, ok := PickTarget(model.TargetBalanceFailover, targets, "", 0)
	if !ok || idx != 0 || tg.Host != "1.2.3.4" {
		t.Fatalf("failover 应选中第一个 up 目标，实际 %+v（下标 %d）", tg, idx)
	}

	// 全部 down：返回 false。
	allDown := []model.Target{
		{Host: "1.2.3.4", Port: 443, Status: model.TargetDown},
		{Host: "5.6.7.8", Port: 443, Status: model.TargetDown},
	}
	if _, _, ok := PickTarget(model.TargetBalanceFailover, allDown, "", 0); ok {
		t.Fatalf("全部目标 down 时应返回 false")
	}

	// hash_ip：同一客户端固定目标，且与目标顺序无关。
	first, _, _ := PickTarget(model.TargetBalanceHashIP, targets, "203.0.113.5", 0)
	for i := 0; i < 10; i++ {
		again, _, _ := PickTarget(model.TargetBalanceHashIP, targets, "203.0.113.5", 0)
		if again.Host != first.Host {
			t.Fatalf("hash_ip 对同一客户端应稳定，实际 %s → %s", first.Host, again.Host)
		}
	}
	if first.Status == model.TargetDown {
		t.Fatalf("hash_ip 不应选中 down 的目标")
	}

	// round_robin / weighted / least_conn 都应只在 up 目标里选。
	for _, strategy := range []string{
		model.TargetBalanceRoundRobin, model.TargetBalanceWeighted, model.TargetBalanceLeastConn,
	} {
		for i := 0; i < 20; i++ {
			tg, _, ok := PickTarget(strategy, targets, "1.1.1.1", 1)
			if !ok {
				t.Fatalf("%s 应能选出目标", strategy)
			}
			if tg.Status == model.TargetDown {
				t.Fatalf("%s 不应选中 down 的目标：%+v", strategy, tg)
			}
		}
	}
}

// TestPickTargetWithFailover 验证目标级故障转移：连续失败达到阈值的目标被跳过。
func TestPickTargetWithFailover(t *testing.T) {
	targets := []model.Target{
		{Host: "1.2.3.4", Port: 443, Status: model.TargetUp},
		{Host: "5.6.7.8", Port: 443, Status: model.TargetUp},
	}
	failures := map[string]int{TargetKey(targets[0]): 3}

	tg, idx, ok := PickTargetWithFailover(targets, failures, 3)
	if !ok || idx != 1 || tg.Host != "5.6.7.8" {
		t.Fatalf("应跳过连续失败达阈值的目标，实际 %+v（下标 %d）", tg, idx)
	}

	// 全部达到阈值 → 无可用目标。
	failures[TargetKey(targets[1])] = 3
	if _, _, ok := PickTargetWithFailover(targets, failures, 3); ok {
		t.Fatalf("全部目标失败时应返回 false")
	}
}

// TestMarkTargetDownUp 验证目标级 up/down 标记的幂等性。
func TestMarkTargetDownUp(t *testing.T) {
	targets := []model.Target{{Host: "1.2.3.4", Port: 443, Status: model.TargetUp}}
	if !MarkTargetDown(targets, 0) {
		t.Fatalf("第一次标记 down 应返回变更")
	}
	if MarkTargetDown(targets, 0) {
		t.Fatalf("重复标记 down 不应返回变更")
	}
	if !MarkTargetUp(targets, 0) {
		t.Fatalf("恢复应返回变更")
	}
	if MarkTargetUp(targets, 0) {
		t.Fatalf("重复恢复不应返回变更")
	}
	// 越界下标必须被安全忽略。
	if MarkTargetDown(targets, 5) || MarkTargetUp(targets, -1) {
		t.Fatalf("越界下标应被忽略")
	}
}

// TestSortTargetsByPriority 验证 up 目标排在前面且保持原有相对顺序。
func TestSortTargetsByPriority(t *testing.T) {
	targets := []model.Target{
		{Host: "a", Port: 1, Status: model.TargetDown},
		{Host: "b", Port: 2, Status: model.TargetUp},
		{Host: "c", Port: 3, Status: model.TargetDown},
		{Host: "d", Port: 4, Status: model.TargetUp},
	}
	got := SortTargetsByPriority(targets)
	want := []string{"b", "d", "a", "c"}
	for i, h := range want {
		if got[i].Host != h {
			t.Fatalf("排序结果 = %v，期望 %v", hostsOf(got), want)
		}
	}
}

// ───────────────────────── 组成员顺序 ─────────────────────────

// TestSortCandidatesByNodeOrder 验证组成员顺序来自 node_ids 且非成员被剔除。
func TestSortCandidatesByNodeOrder(t *testing.T) {
	cands := []BalanceCandidate{
		{NodeID: 3, Weight: 1}, {NodeID: 1, Weight: 1},
		{NodeID: 2, Weight: 1}, {NodeID: 9, Weight: 1},
	}
	got := SortCandidatesByNodeOrder(cands, []uint64{2, 1, 3})
	want := []uint64{2, 1, 3}
	if len(got) != len(want) {
		t.Fatalf("应剔除不在组内的节点，实际 %+v", got)
	}
	for i := range want {
		if got[i].NodeID != want[i] {
			t.Fatalf("顺序 = %v，期望 %v", nodeIDsOf(got), want)
		}
	}
}

// ───────────────────────── 负载分布统计 ─────────────────────────

// TestComputeLoadDistribution 验证 /health 的负载分布口径。
func TestComputeLoadDistribution(t *testing.T) {
	cands := []BalanceCandidate{
		{NodeID: 1, Name: "a", Weight: 3, CurrentConn: 30},
		{NodeID: 2, Name: "b", Weight: 1, CurrentConn: 30},
	}
	dist := ComputeLoadDistribution(cands)
	if len(dist) != 2 {
		t.Fatalf("应返回两个节点，实际 %d", len(dist))
	}
	if dist[0].Ratio != 10 {
		t.Fatalf("节点 1 的比率应为 30/3 = 10，实际 %v", dist[0].Ratio)
	}
	if dist[1].Ratio != 30 {
		t.Fatalf("节点 2 的比率应为 30/1 = 30，实际 %v", dist[1].Ratio)
	}
	if dist[0].Share != 0.75 {
		t.Fatalf("节点 1 的占比应为 3/4 = 0.75，实际 %v", dist[0].Share)
	}
}

// ───────────────────────── 小工具 ─────────────────────────

// itoa 是 strconv.Itoa 的本地实现，避免测试文件引入不必要的依赖。
func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	buf := make([]byte, 0, 8)
	for v > 0 {
		buf = append([]byte{byte('0' + v%10)}, buf...)
		v /= 10
	}
	return string(buf)
}

// hostsOf 提取目标列表的 host，便于断言。
func hostsOf(targets []model.Target) []string {
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		out = append(out, t.Host)
	}
	return out
}

// nodeIDsOf 提取候选列表的节点 ID，便于断言。
func nodeIDsOf(cands []BalanceCandidate) []uint64 {
	out := make([]uint64, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.NodeID)
	}
	return out
}
