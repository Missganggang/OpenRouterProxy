package app

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/openroute/openroute/internal/model"
	"github.com/openroute/openroute/internal/util"
)

// 本文件覆盖规格书 6.4 的流量计费口径、6.4/附录 D 的旧文本导入格式、
// 4.3/4.4 的默认配置 JSON 逐字节一致性，以及 6.8 的整流选项表。

// ───────────────────────── 流量计费（规格书 6.4） ─────────────────────────

// TestMultiplierSpecExample 用规格书 6.4 给出的原始示例做逐字节验证。
//
// 规格书原文：
//
//	用户下载 500M、上传 100M
//	该规则流量增加 = 500 + 100 = 600M
//	若入口倍率 1.5、出口倍率 0.5
//	则用户已用流量增加 = 600 × 1.5 + 600 × 0.5 = 900 + 300 = 1200MB
func TestMultiplierSpecExample(t *testing.T) {
	const mb = 1024 * 1024

	got := ChargeTraffic(TrafficChargeInput{
		// 「单向流量统计」：下载 + 上传合计为一次上报的实际流量。
		InboundRaw:         600 * mb,
		OutboundRaw:        600 * mb,
		InboundMultiplier:  1.5,
		OutboundMultiplier: 0.5,
	})

	if got.InboundBytes != 900*mb {
		t.Fatalf("入口折算 = %d，期望 %d（600 × 1.5）", got.InboundBytes, 900*mb)
	}
	if got.OutboundBytes != 300*mb {
		t.Fatalf("出口折算 = %d，期望 %d（600 × 0.5）", got.OutboundBytes, 300*mb)
	}
	if got.Total != 1200*mb {
		t.Fatalf("用户已用流量 = %d，期望 %d（900 + 300）", got.Total, 1200*mb)
	}
}

// TestChargeTraffic 用表驱动覆盖计费公式的边界。
func TestChargeTraffic(t *testing.T) {
	cases := []struct {
		name string
		in   TrafficChargeInput
		want int64
	}{
		{
			name: "默认倍率 1:1 时不产生任何折算",
			in:   TrafficChargeInput{InboundRaw: 1000, OutboundRaw: 2000, InboundMultiplier: 1, OutboundMultiplier: 1},
			want: 3000,
		},
		{
			name: "倍率为 0 表示该段不计费",
			in:   TrafficChargeInput{InboundRaw: 1000, OutboundRaw: 2000, InboundMultiplier: 0, OutboundMultiplier: 2},
			want: 4000,
		},
		{
			name: "入口与出口都不产生流量",
			in:   TrafficChargeInput{InboundMultiplier: 1, OutboundMultiplier: 1},
			want: 0,
		},
		{
			name: "两位小数倍率（0.25）",
			in:   TrafficChargeInput{InboundRaw: 4000, InboundMultiplier: 0.25, OutboundMultiplier: 1},
			want: 1000,
		},
		{
			name: "四舍五入到整数（1 × 1.5 = 1.5 → 2）",
			in:   TrafficChargeInput{InboundRaw: 1, InboundMultiplier: 1.5, OutboundMultiplier: 1},
			want: 2,
		},
		{
			name: "只统计出口段",
			in:   TrafficChargeInput{OutboundRaw: 1000, InboundMultiplier: 1, OutboundMultiplier: 3},
			want: 3000,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ChargeTraffic(tc.in).Total; got != tc.want {
				t.Fatalf("总流量 = %d，期望 %d", got, tc.want)
			}
		})
	}
}

// TestChainTrafficCountsBothSegments 验证规格书 6.4 / 6.7 的链式计费：
// 「链式出口场景下，流量同时经过入口与链式出口，两段分别按各自倍率计入」。
func TestChainTrafficCountsBothSegments(t *testing.T) {
	got := ChargeTraffic(TrafficChargeInput{
		InboundRaw:         1000, // 入口段实际流量
		OutboundRaw:        0,    // 链式场景下最终出口不单独计费（只算链路上各跳）
		InboundMultiplier:  2,
		OutboundMultiplier: 1,
		ChainRaws: []ChainSegment{
			{GroupID: 10, Raw: 1000, Multiplier: 3},
			{GroupID: 11, Raw: 1000, Multiplier: 0.5},
		},
	})
	// 1000×2（入口） + 1000×3（第一跳） + 1000×0.5（第二跳） = 2000 + 3000 + 500
	if got.InboundBytes != 2000 {
		t.Fatalf("入口段 = %d，期望 2000", got.InboundBytes)
	}
	if got.ChainBytes != 3500 {
		t.Fatalf("链式段 = %d，期望 3500（3000 + 500）", got.ChainBytes)
	}
	if got.Total != 5500 {
		t.Fatalf("链式总流量 = %d，期望 5500", got.Total)
	}
}

// TestChargeRuleTraffic 验证规则封装：倍率取自规则本身，缺失时按 1 处理。
func TestChargeRuleTraffic(t *testing.T) {
	rule := &model.ForwardRule{InboundMultiplier: 1.5, OutboundMultiplier: 0.5}
	got := ChargeRuleTraffic(rule, 600, 600, nil)
	if got.Total != 1200 {
		t.Fatalf("规则计费 = %d，期望 1200", got.Total)
	}

	// nil 规则（防御性）：倍率按 1。
	got = ChargeRuleTraffic(nil, 100, 100, nil)
	if got.Total != 200 {
		t.Fatalf("空规则计费 = %d，期望 200", got.Total)
	}
}

// TestTotalRuleTraffic 验证累计流量的口径（数据库里的 traffic_in/out 已乘倍率）。
func TestTotalRuleTraffic(t *testing.T) {
	rule := &model.ForwardRule{TrafficIn: 900, TrafficOut: 300}
	if got := TotalRuleTraffic(rule); got != 1200 {
		t.Fatalf("累计流量 = %d，期望 1200", got)
	}
	if got := TotalRuleTraffic(nil); got != 0 {
		t.Fatalf("空规则应为 0，实际 %d", got)
	}
	rules := []model.ForwardRule{
		{TrafficIn: 10, TrafficOut: 5},
		{TrafficIn: 100, TrafficOut: 50},
	}
	if got := UserTrafficOfRules(rules); got != 165 {
		t.Fatalf("用户维度流量 = %d，期望 165", got)
	}
}

// ───────────────────────── 旧文本导入格式（规格书 6.4 / 附录 D） ─────────────────────────

// TestParseLegacyRuleLine 覆盖附录 D 的全部三种写法与各类非法输入。
func TestParseLegacyRuleLine(t *testing.T) {
	cases := []struct {
		name      string
		line      string
		wantName  string
		wantPort  int
		wantEnd   int
		wantHost  string
		wantDst   int
		wantErrAt string // 期望的错误关键词（空 = 应当解析成功）
	}{
		{
			name: "单端口写法", line: "HK-JP-01#8443#1.2.3.4#443",
			wantName: "HK-JP-01", wantPort: 8443, wantHost: "1.2.3.4", wantDst: 443,
		},
		{
			name: "双井号 = 随机端口", line: "random-port##example.com#80",
			wantName: "random-port", wantPort: 0, wantHost: "example.com", wantDst: 80,
		},
		{
			name: "端口段写法", line: "range#8443-8450#1.2.3.4#443",
			wantName: "range", wantPort: 8443, wantEnd: 8450, wantHost: "1.2.3.4", wantDst: 443,
		},
		{
			name: "域名目标", line: "域名规则#8443#jp.example.com#8443",
			wantName: "域名规则", wantPort: 8443, wantHost: "jp.example.com", wantDst: 8443,
		},
		{
			name: "缺少目标端口", line: "bad#line",
			wantErrAt: "缺少目标端口",
		},
		{
			name: "缺少规则名", line: "#8443#1.2.3.4#443",
			wantErrAt: "缺少规则名",
		},
		{
			name: "监听端口非法（字母）", line: "a#abc#1.2.3.4#443",
			wantErrAt: "监听端口",
		},
		{
			name: "监听端口越界", line: "a#99999#1.2.3.4#443",
			wantErrAt: "监听端口",
		},
		{
			name: "端口段结束值小于起始值", line: "a#8450-8443#1.2.3.4#443",
			wantErrAt: "监听端口",
		},
		{
			name: "目标地址为空", line: "a#8443##443",
			wantErrAt: "缺少目标地址",
		},
		{
			name: "目标端口非法（字母）", line: "a#8443#1.2.3.4#abc",
			wantErrAt: "目标端口",
		},
		{
			name: "目标端口越界", line: "a#8443#1.2.3.4#70000",
			wantErrAt: "目标端口",
		},
		{
			name: "目标端口不支持端口段", line: "a#8443#1.2.3.4#443-450",
			wantErrAt: "目标端口不支持端口段",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item, err := ParseLegacyRuleLine(tc.line)
			if tc.wantErrAt != "" {
				if err == nil {
					t.Fatalf("期望解析失败（%s），实际成功：%+v", tc.wantErrAt, item)
				}
				if !strings.Contains(err.Error(), tc.wantErrAt) {
					t.Fatalf("错误信息 %q 应包含 %q", err.Error(), tc.wantErrAt)
				}
				return
			}
			if err != nil {
				t.Fatalf("解析失败：%v", err)
			}
			if item.Name != tc.wantName {
				t.Fatalf("规则名 = %q，期望 %q", item.Name, tc.wantName)
			}
			if item.ListenPort != tc.wantPort {
				t.Fatalf("监听端口 = %d，期望 %d", item.ListenPort, tc.wantPort)
			}
			if item.ListenPortEnd != tc.wantEnd {
				t.Fatalf("端口段结束 = %d，期望 %d", item.ListenPortEnd, tc.wantEnd)
			}
			if len(item.Targets) != 1 {
				t.Fatalf("应解析出 1 个目标，实际 %d", len(item.Targets))
			}
			if item.Targets[0].Host != tc.wantHost {
				t.Fatalf("目标地址 = %q，期望 %q", item.Targets[0].Host, tc.wantHost)
			}
			if item.Targets[0].Port != tc.wantDst {
				t.Fatalf("目标端口 = %d，期望 %d", item.Targets[0].Port, tc.wantDst)
			}
			if item.Targets[0].NormalizedWeight() != 1 {
				t.Fatalf("导入的目标权重应默认为 1")
			}
			if !item.Targets[0].Up() {
				t.Fatalf("导入的目标应默认为 up")
			}
		})
	}
}

// TestParseLegacyRuleLinePortRange 验证端口段与随机端口的语义落地。
func TestParseLegacyRuleLinePortRange(t *testing.T) {
	item, err := ParseLegacyRuleLine("range#8443-8450#1.2.3.4#443")
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	r := buildRuleFromImport(item)
	if !r.IsMultiPort() {
		t.Fatalf("8443-8450 应被识别为端口段")
	}

	item, err = ParseLegacyRuleLine("rand##1.2.3.4#443")
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	r = buildRuleFromImport(item)
	if r.IsMultiPort() {
		t.Fatalf("随机端口不应被识别为端口段")
	}
	if r.ListenPort != 0 {
		t.Fatalf("随机端口的 listen_port 应为 0，实际 %d", r.ListenPort)
	}
	if r.SyncStatus != model.SyncUnsynced {
		t.Fatalf("导入的规则初始状态应为 unsynced，实际 %q", r.SyncStatus)
	}
}

// TestSplitImportLines 验证导入文本的行切分与注释过滤。
func TestSplitImportLines(t *testing.T) {
	content := "a#8443#1.2.3.4#443\r\n" +
		"\r\n" +
		"   \n" +
		"// 这是注释\n" +
		"b##2.3.4.5#80\n"
	lines := splitImportLines(content)
	if len(lines) != 2 {
		t.Fatalf("应切出 2 行有效规则，实际 %d：%v", len(lines), lines)
	}
	if lines[0] != "a#8443#1.2.3.4#443" || lines[1] != "b##2.3.4.5#80" {
		t.Fatalf("切分结果异常：%v", lines)
	}
}

// TestSniffImportFormat 验证导入格式的自动嗅探。
func TestSniffImportFormat(t *testing.T) {
	if got := sniffImportFormat(`[{"name":"a"}]`); got != ImportFormatJSON {
		t.Fatalf("JSON 数组应被识别为 json，实际 %q", got)
	}
	if got := sniffImportFormat(`  {"name":"a"}`); got != ImportFormatJSON {
		t.Fatalf("JSON 对象应被识别为 json，实际 %q", got)
	}
	if got := sniffImportFormat("a#8443#1.2.3.4#443"); got != ImportFormatText {
		t.Fatalf("文本格式应被识别为 text，实际 %q", got)
	}
}

// TestApplyImportDefaults 验证导入默认值的填充规则（缺失才填，不覆盖显式值）。
func TestApplyImportDefaults(t *testing.T) {
	def := ImportDefaults{
		UserID: 3, RuleGroupID: 4, InboundGroupID: 5, OutboundGroupID: 6,
		TargetBalance:     model.TargetBalanceRoundRobin,
		InboundMultiplier: 2, OutboundMultiplier: 3,
	}
	it := ImportItem{
		Name: "x", ListenPort: 8443,
		// 显式给了出口组与倍率 → 不应被默认值覆盖。
		OutboundGroupID: 99, InboundMultiplier: 0.5,
	}
	applyImportDefaults(&it, def)

	if it.InboundGroupID != 5 {
		t.Fatalf("入口组应填充默认值 5，实际 %d", it.InboundGroupID)
	}
	if it.OutboundGroupID != 99 {
		t.Fatalf("显式出口组不应被覆盖，实际 %d", it.OutboundGroupID)
	}
	if it.UserID != 3 || it.RuleGroupID != 4 {
		t.Fatalf("用户/规则分组应填充默认值，实际 %+v", it)
	}
	if it.InboundMultiplier != 0.5 {
		t.Fatalf("显式入口倍率不应被覆盖，实际 %v", it.InboundMultiplier)
	}
	if it.OutboundMultiplier != 3 {
		t.Fatalf("出口倍率应填充默认值 3，实际 %v", it.OutboundMultiplier)
	}
	if it.TargetBalance != model.TargetBalanceRoundRobin {
		t.Fatalf("目标策略应填充默认值，实际 %q", it.TargetBalance)
	}
}

// TestBuildRuleFromImportDefaults 验证导入落库时的默认值。
func TestBuildRuleFromImportDefaults(t *testing.T) {
	r := buildRuleFromImport(ImportItem{
		Name: "x", InboundGroupID: 1, ListenPort: 8443,
		Targets: []model.Target{{Host: "1.2.3.4", Port: 443, Weight: 3}},
	})
	if r.TargetBalance != model.TargetBalanceFailover {
		t.Fatalf("目标策略应默认 failover，实际 %q", r.TargetBalance)
	}
	if r.InboundMultiplier != 1 || r.OutboundMultiplier != 1 {
		t.Fatalf("倍率应默认 1，实际 %v/%v", r.InboundMultiplier, r.OutboundMultiplier)
	}
	if !r.Enable {
		t.Fatalf("导入的规则应默认启用")
	}
	if r.SyncStatus != model.SyncUnsynced {
		t.Fatalf("导入的规则应为未同步，实际 %q", r.SyncStatus)
	}
	if len(r.TargetList()) != 1 || r.TargetList()[0].Weight != 3 {
		t.Fatalf("目标应被正确序列化，实际 %+v", r.TargetList())
	}
}

// ───────────────────────── 默认配置 JSON（规格书 4.3 / 4.4） ─────────────────────────

// TestDefaultInboundConfigJSON 验证 DefaultInboundConfig 序列化后
// 与规格书 4.3 的示例 JSON **逐字节一致**（含字段顺序）。
func TestDefaultInboundConfigJSON(t *testing.T) {
	const spec = `{"allowed_host":[],"blocked_host":[],"blocked_path":[],"blocked_protocol":[],` +
		`"tls_inbound_policy":0,"tls_reject_empty_sni":false,"disable_udp":false,` +
		`"udp_over_tcp":false,"ipv6_group":[],"max_fail":3,"fail_timout_sec":30,` +
		`"reverse_group":[],"protocol":"tls","tls":{}}`

	got, err := json.Marshal(DefaultInboundConfig())
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	if string(got) != spec {
		t.Fatalf("入口组默认配置与规格书 4.3 不一致：\n实际: %s\n期望: %s", got, spec)
	}
}

// TestDefaultOutboundConfigJSON 验证 DefaultOutboundConfig 序列化后
// 与规格书 4.4 的示例 JSON 逐字节一致（含字段顺序）。
func TestDefaultOutboundConfigJSON(t *testing.T) {
	const spec = `{"connect_type":"static","connect_address":"node.example.com",` +
		`"connect_port":2333,"protocol":"ws","ws":{},"udp_over_tcp":false}`

	got, err := json.Marshal(DefaultOutboundConfig())
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	if string(got) != spec {
		t.Fatalf("出口组默认配置与规格书 4.4 不一致：\n实际: %s\n期望: %s", got, spec)
	}
}

// TestInboundConfigRoundTrip 验证入口组配置的解析口径：
// 缺省字段补默认值、显式 null 的数组归一为空切片、以及 4.5 的 tls 子配置。
func TestInboundConfigRoundTrip(t *testing.T) {
	raw := `{
	  "allowed_host": [".qq.com", "example.com"],
	  "blocked_protocol": ["socks", "fet"],
	  "tls_inbound_policy": 2,
	  "tls_reject_empty_sni": true,
	  "ipv6_group": [0, 1, 2],
	  "reverse_group": [7],
	  "protocol": "ws",
	  "ws": {"host": "cdn.example.com", "path": "/ws",
	         "request": "GET / HTTP/1.5\r\n\r\n", "response": "HTTP/1.5 200 OK\r\n\r\n"},
	  "tls": {"sni": "some.host.com", "alpn": ["http/1.1"], "chfp": "chrome"}
	}`
	cfg, err := ParseInboundConfig([]byte(raw))
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if len(cfg.AllowedHost) != 2 || cfg.AllowedHost[0] != ".qq.com" {
		t.Fatalf("白名单解析异常：%v", cfg.AllowedHost)
	}
	if cfg.TLSInboundPolicy != TLSInboundSNISplit || !cfg.TLSRejectEmptySNI {
		t.Fatalf("TLS 策略解析异常：%+v", cfg)
	}
	if len(cfg.IPv6Group) != 3 || cfg.IPv6Group[0] != 0 {
		t.Fatalf("ipv6_group 解析异常：%v", cfg.IPv6Group)
	}
	if cfg.Protocol != ProtocolWS {
		t.Fatalf("协议解析异常：%q", cfg.Protocol)
	}
	if cfg.WS == nil || cfg.WS.Host != "cdn.example.com" || cfg.WS.Path != "/ws" {
		t.Fatalf("ws 子配置解析异常：%+v", cfg.WS)
	}
	if cfg.WS.Request != "GET / HTTP/1.5\r\n\r\n" {
		t.Fatalf("请求模板 MUST 原样保留：%q", cfg.WS.Request)
	}
	if cfg.TLS.SNI != "some.host.com" || cfg.TLS.CHFP != "chrome" {
		t.Fatalf("tls 子配置解析异常：%+v", cfg.TLS)
	}
	// 未出现的字段应保留默认值。
	if cfg.MaxFail != 3 || cfg.FailTimoutSec != 30 {
		t.Fatalf("缺失字段应补默认值，实际 max_fail=%d fail_timout_sec=%d",
			cfg.MaxFail, cfg.FailTimoutSec)
	}
	if cfg.BlockedHost == nil || cfg.BlockedPath == nil {
		t.Fatalf("缺失的数组字段应归一为空切片")
	}
}

// TestOutboundConfigRoundTrip 验证出口组配置的解析。
func TestOutboundConfigRoundTrip(t *testing.T) {
	raw := `{
	  "connect_type": "dyn_ip6",
	  "connect_address": "",
	  "protocol": "tls",
	  "tls": {"sni": "edge.example.com", "alpn": ["h2", "http/1.1"], "chfp": "firefox"},
	  "udp_over_tcp": true
	}`
	cfg, err := ParseOutboundConfig([]byte(raw))
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if cfg.ConnectType != ConnectTypeDynIPv6 {
		t.Fatalf("连接方式解析异常：%q", cfg.ConnectType)
	}
	if !cfg.UDPOverTCP {
		t.Fatalf("udp_over_tcp 解析异常")
	}
	if cfg.TLS == nil || cfg.TLS.CHFP != "firefox" || len(cfg.TLS.ALPN) != 2 {
		t.Fatalf("tls 子配置解析异常：%+v", cfg.TLS)
	}

	// 空配置应返回默认值。
	empty, err := ParseOutboundConfig(nil)
	if err != nil {
		t.Fatalf("空配置解析失败：%v", err)
	}
	if empty.Protocol != ProtocolWS || empty.ConnectType != ConnectTypeStatic {
		t.Fatalf("空配置应返回默认值，实际 %+v", empty)
	}
}

// ───────────────────────── 校验辅助函数 ─────────────────────────

// TestValidChfp 验证规格书 4.5 的 chfp 允许值（8 个，MUST 严格保持）。
func TestValidChfp(t *testing.T) {
	allowed := []string{"chrome", "firefox", "safari", "ios", "android", "edge", "360", "qq"}
	for _, v := range allowed {
		if !ValidChfp(v) {
			t.Fatalf("%q 应为合法的指纹", v)
		}
		// 大小写不敏感（前端可能传 Chrome）。
		if !ValidChfp(strings.ToUpper(v)) {
			t.Fatalf("%q 的大写形式也应合法", v)
		}
	}
	// 空值合法 = 使用 Go 默认指纹。
	if !ValidChfp("") {
		t.Fatalf("空指纹应合法（使用 Go 默认 TLS 指纹）")
	}
	for _, v := range []string{"chromium", "opera", "curl", "golang", "Chrome120"} {
		if ValidChfp(v) {
			t.Fatalf("%q 不应是合法的指纹", v)
		}
	}
	if len(ChfpOptions()) != 8 {
		t.Fatalf("chfp 允许值应为 8 个，实际 %d", len(ChfpOptions()))
	}
}

// TestValidShape 验证规格书 6.8 的整流选项表。
func TestValidShape(t *testing.T) {
	for _, v := range []int{ShapeRejectTLS12, ShapeRejectTLS, ShapeRejectHTTP1, ShapeRejectWS, ShapeSkipIPv6} {
		if !ValidShape(v) {
			t.Fatalf("整流选项 %d 应合法", v)
		}
	}
	for _, v := range []int{0, 5, 100, 1999} {
		if ValidShape(v) {
			t.Fatalf("整流选项 %d 不应合法", v)
		}
	}
	if len(ShapeOptions()) != 5 {
		t.Fatalf("整流选项应为 5 个，实际 %d", len(ShapeOptions()))
	}
}

// TestSniAllowed 验证 SNI 白名单的后缀匹配语义（规格书 4.3 / 6.8）。
func TestSniAllowed(t *testing.T) {
	cases := []struct {
		sni     string
		allowed []string
		want    bool
	}{
		{"a.bilivideo.cn", []string{".bilivideo.cn"}, true},
		{"bilivideo.cn", []string{".bilivideo.cn"}, false}, // 带点号前缀不匹配自身
		{"bilivideo.cn", []string{"bilivideo.cn"}, true},   // 不带点号匹配自身
		{"i0.hdslb.com", []string{"i0.hdslb.com"}, true},
		{"evil.com", []string{".bilivideo.cn", ".example.com"}, false},
		{"anything.com", nil, true}, // 空白名单 = 不限制
		{"anything.com", []string{"*"}, true},
	}
	for _, tc := range cases {
		if got := sniAllowed(tc.sni, tc.allowed); got != tc.want {
			t.Fatalf("sniAllowed(%q, %v) = %v，期望 %v", tc.sni, tc.allowed, got, tc.want)
		}
	}
}

// TestRangesOverlap 验证端口区间相交判定（端口冲突检测的基础）。
func TestRangesOverlap(t *testing.T) {
	cases := []struct {
		a1, a2, b1, b2 int
		want           bool
	}{
		{8443, 8443, 8443, 8443, true},  // 单端口相同
		{8443, 8450, 8450, 8460, true},  // 端点相接
		{8443, 8450, 8451, 8460, false}, // 相邻但不重叠
		{8443, 8450, 8440, 8442, false},
		{8443, 8450, 8400, 8500, true}, // 完全包含
		{8400, 8500, 8443, 8450, true}, // 被包含
		{1000, 1000, 2000, 2000, false},
	}
	for _, tc := range cases {
		if got := rangesOverlap(tc.a1, tc.a2, tc.b1, tc.b2); got != tc.want {
			t.Fatalf("rangesOverlap(%d,%d,%d,%d) = %v，期望 %v",
				tc.a1, tc.a2, tc.b1, tc.b2, got, tc.want)
		}
	}
}

// TestRuleOrderClause 验证排序参数的白名单与拼接结果。
func TestRuleOrderClause(t *testing.T) {
	cases := []struct {
		sortBy, order, want string
	}{
		{"id", "desc", "id DESC"},
		{"name", "asc", "name ASC"},
		{"listen_port", "asc", "listen_port ASC"},
		{"traffic", "desc", "(traffic_in + traffic_out) DESC"},
		{"created_at", "asc", "created_at ASC"},
		{"updated_at", "desc", "updated_at DESC"},
		// 非法值退回默认排序，且不会把用户输入拼进 SQL。
		{"name; DROP TABLE users", "desc", "id DESC"},
		{"", "", "id DESC"},
	}
	for _, tc := range cases {
		// 排序参数先经过 Normalize（列表接口的真实调用顺序）。
		f := RuleListFilter{SortBy: tc.sortBy, Order: tc.order}.Normalize()
		if got := ruleOrderClause(f.SortBy, f.Order); got != tc.want {
			t.Fatalf("ruleOrderClause(%q, %q) = %q，期望 %q",
				tc.sortBy, tc.order, got, tc.want)
		}
	}
}

// TestRuleListFilterNormalize 验证分页与排序参数的归一化。
func TestRuleListFilterNormalize(t *testing.T) {
	got := RuleListFilter{}.Normalize()
	if got.Page != 1 || got.PageSize != 20 || got.SortBy != "id" || got.Order != "desc" {
		t.Fatalf("默认值异常：%+v", got)
	}
	// page_size 上限保护。
	got = RuleListFilter{Page: -5, PageSize: 100000}.Normalize()
	if got.Page != 1 || got.PageSize != 200 {
		t.Fatalf("上限保护异常：%+v", got)
	}
	got = RuleListFilter{SortBy: "name", Order: "asc"}.Normalize()
	if got.SortBy != "name" || got.Order != "asc" {
		t.Fatalf("合法值不应被修改：%+v", got)
	}
}

// TestRound2 验证倍率四舍五入到两位小数（规格书 6.4 的小数位限制）。
func TestRound2(t *testing.T) {
	cases := []struct {
		in   float64
		want float64
	}{
		{1.5, 1.5},
		{0.30000000000000004, 0.3},
		{2.005, 2.01},
		{0.125, 0.13},
		{100, 100},
		{0, 0},
	}
	for _, tc := range cases {
		if got := roundMultiplier(tc.in); got != tc.want {
			t.Fatalf("roundMultiplier(%v) = %v，期望 %v", tc.in, got, tc.want)
		}
	}
	// 四舍五入后的值必须能通过倍率校验（否则「批量 ×1.5」会写出非法值）。
	if err := util.ValidMultiplier(roundMultiplier(0.7 * 1.5)); err != nil {
		t.Fatalf("round2 后应通过倍率校验：%v", err)
	}
}

// TestValidBatchAction 验证规格书 8.9 的批量动作白名单。
func TestValidBatchAction(t *testing.T) {
	allowed := []string{
		BatchEnable, BatchDisable, BatchDelete, BatchSetRuleGroup, BatchSetMultiplier,
		BatchScaleMultiplier, BatchSetOutboundGroup, BatchSetLimits, BatchSetChain,
	}
	for _, a := range allowed {
		if !ValidBatchAction(a) {
			t.Fatalf("%q 应为合法动作", a)
		}
	}
	for _, a := range []string{"", "drop", "set_sni", "ENABLE"} {
		if ValidBatchAction(a) {
			t.Fatalf("%q 不应是合法动作", a)
		}
	}
}

// TestSyncStatusColor 验证状态机的颜色标识（规格书 5.4）。
func TestSyncStatusColor(t *testing.T) {
	cases := map[string]string{
		model.SyncUnsynced: "yellow",
		model.SyncSyncing:  "blue",
		model.SyncNormal:   "green",
		model.SyncFailed:   "red",
		"unknown":          "gray",
	}
	for status, want := range cases {
		if got := SyncStatusColor(status); got != want {
			t.Fatalf("SyncStatusColor(%q) = %q，期望 %q", status, got, want)
		}
	}
}

// TestSortRulesBySyncSeverity 验证 failed 的规则排在 unsynced 之前。
func TestSortRulesBySyncSeverity(t *testing.T) {
	rules := []model.ForwardRule{
		{Name: "a", SyncStatus: model.SyncUnsynced},
		{Name: "b", SyncStatus: model.SyncFailed},
		{Name: "c", SyncStatus: model.SyncUnsynced},
		{Name: "d", SyncStatus: model.SyncFailed},
	}
	sortRulesBySyncSeverity(rules)
	if rules[0].SyncStatus != model.SyncFailed || rules[1].SyncStatus != model.SyncFailed {
		t.Fatalf("failed 应排在最前：%+v", rules)
	}
	// 同级内部保持原有相对顺序（稳定排序）。
	if rules[0].Name != "b" || rules[1].Name != "d" {
		t.Fatalf("同级内顺序应稳定：%+v", rules)
	}
	if rules[2].Name != "a" || rules[3].Name != "c" {
		t.Fatalf("同级内顺序应稳定：%+v", rules)
	}
}

// TestDefaultFailoverConfig 验证故障转移阈值的默认值与归一化。
func TestDefaultFailoverConfig(t *testing.T) {
	cfg := DefaultFailoverConfig()
	if cfg.MaxFail != 3 || cfg.FailTimeoutSec != 30 || cfg.SuccCount != 2 || !cfg.Enable {
		t.Fatalf("默认阈值与规格书不符：%+v", cfg)
	}
	// 未设置的阈值（0 / 负数）应被补成默认值。
	got := FailoverConfig{MaxFail: 0, SuccCount: -1}.Normalize()
	if got.MaxFail != 3 || got.SuccCount != 2 || got.FailTimeoutSec != 30 {
		t.Fatalf("归一化异常：%+v", got)
	}
	// 显式设置的阈值不应被覆盖。
	got = FailoverConfig{MaxFail: 10, SuccCount: 5, FailTimeoutSec: 60}.Normalize()
	if got.MaxFail != 10 || got.SuccCount != 5 || got.FailTimeoutSec != 60 {
		t.Fatalf("显式阈值被覆盖：%+v", got)
	}
}

// TestDedupeUint64 验证去重保持顺序（出口组的顺序参与负载均衡，MUST 保留）。
func TestDedupeUint64(t *testing.T) {
	got := dedupeUint64([]uint64{3, 1, 3, 0, 2, 1})
	want := []uint64{3, 1, 2}
	if len(got) != len(want) {
		t.Fatalf("去重结果 = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("去重结果 = %v，期望 %v（顺序 MUST 保留）", got, want)
		}
	}
}

// TestSameUint64Set 验证集合比较（用于 Reorder 的成员一致性检查）。
func TestSameUint64Set(t *testing.T) {
	if !sameUint64Set([]uint64{1, 2, 3}, []uint64{3, 2, 1}) {
		t.Fatalf("元素相同应判定为同一集合")
	}
	if sameUint64Set([]uint64{1, 2}, []uint64{1, 2, 3}) {
		t.Fatalf("长度不同不应判定为同一集合")
	}
	if sameUint64Set([]uint64{1, 2, 3}, []uint64{1, 2, 4}) {
		t.Fatalf("元素不同不应判定为同一集合")
	}
}

// TestPortRangeOf 验证规则实际占用的端口区间。
func TestPortRangeOf(t *testing.T) {
	r := &model.ForwardRule{ListenPort: 8443}
	s, e := portRangeOf(r)
	if s != 8443 || e != 8443 {
		t.Fatalf("单端口区间 = [%d,%d]，期望 [8443,8443]", s, e)
	}
	r = &model.ForwardRule{ListenPort: 8443, ListenPortEnd: 8450}
	s, e = portRangeOf(r)
	if s != 8443 || e != 8450 {
		t.Fatalf("端口段区间 = [%d,%d]，期望 [8443,8450]", s, e)
	}
	if rangeText(8443, 8443) != "8443" {
		t.Fatalf("单端口文本应为 8443")
	}
	if rangeText(8443, 8450) != "8443-8450" {
		t.Fatalf("端口段文本应为 8443-8450，实际 %q", rangeText(8443, 8450))
	}
}

// TestNormalizeMultiplier 验证倍率缺失时的默认值处理。
func TestNormalizeMultiplier(t *testing.T) {
	if normalizeMultiplier(0) != 0 {
		t.Fatalf("显式 0 倍率应保留（表示该段不计费）")
	}
	if normalizeMultiplier(1.5) != 1.5 {
		t.Fatalf("合法倍率不应被修改")
	}
	if normalizeMultiplier(-1) != 1 {
		t.Fatalf("负数倍率应退回默认值 1")
	}
}

// TestGetDeviceGroupSchema 验证 8.8 /schema 覆盖规格书 4.3 / 4.4 的每一个字段。
func TestGetDeviceGroupSchema(t *testing.T) {
	// 用反射不必要，这里直接断言关键字段的键名出现。
	s := &GroupService{}

	in := s.GetDeviceGroupSchema(model.GroupTypeInbound)
	if in == nil {
		t.Fatalf("入口组 schema 不应为 nil")
	}
	wantInbound := []string{
		"allowed_host", "blocked_host", "blocked_path", "blocked_protocol",
		"tls_inbound_policy", "tls_reject_empty_sni", "disable_udp", "udp_over_tcp",
		"ipv6_group", "max_fail", "fail_timout_sec", "reverse_group", "protocol",
		"ws", "tls",
	}
	keys := fieldKeys(in.Fields)
	for _, k := range wantInbound {
		if !keys[k] {
			t.Fatalf("入口组 schema 缺少字段 %q（规格书 4.3）", k)
		}
	}
	// ws / tls 的子字段。
	ws := childKeys(in.Fields, "ws")
	for _, k := range []string{"host", "path", "request", "response"} {
		if !ws[k] {
			t.Fatalf("入口组 schema 的 ws 子配置缺少 %q（规格书 4.5）", k)
		}
	}
	tls := childKeys(in.Fields, "tls")
	for _, k := range []string{"sni", "alpn", "chfp"} {
		if !tls[k] {
			t.Fatalf("入口组 schema 的 tls 子配置缺少 %q（规格书 4.5）", k)
		}
	}

	out := s.GetDeviceGroupSchema(model.GroupTypeOutbound)
	if out == nil {
		t.Fatalf("出口组 schema 不应为 nil")
	}
	wantOutbound := []string{
		"connect_type", "connect_address", "connect_port", "protocol", "ws", "udp_over_tcp",
	}
	keys = fieldKeys(out.Fields)
	for _, k := range wantOutbound {
		if !keys[k] {
			t.Fatalf("出口组 schema 缺少字段 %q（规格书 4.4）", k)
		}
	}
	// 出口组的表字段（附录 A）。
	tableKeys := fieldKeys(out.TableFields)
	for _, k := range []string{
		"balance", "health_check_enable", "health_check_interval", "health_check_timeout",
		"health_check_fail_count", "health_check_succ_count", "failover_group_id", "node_ids",
	} {
		if !tableKeys[k] {
			t.Fatalf("出口组 schema 缺少表字段 %q（附录 A）", k)
		}
	}

	// 非法类型返回 nil。
	if s.GetDeviceGroupSchema("both") != nil {
		t.Fatalf("非法组类型应返回 nil")
	}
}

// fieldKeys 把字段列表转成「键 → 是否存在」的集合。
func fieldKeys(fields []SchemaField) map[string]bool {
	out := make(map[string]bool, len(fields))
	for _, f := range fields {
		out[f.Key] = true
	}
	return out
}

// childKeys 取某个 object 类型字段的子字段键集合。
func childKeys(fields []SchemaField, parent string) map[string]bool {
	for _, f := range fields {
		if f.Key == parent {
			return fieldKeys(f.Children)
		}
	}
	return map[string]bool{}
}

// ───────────────────────── 校验结果结构（规格书 8.8 /validate） ─────────────────────────

// TestValidateResultShape 验证校验结果序列化后的字段与规格书 8.8 /validate 的
// 响应体结构一致（valid / errors / warnings）。
func TestValidateResultShape(t *testing.T) {
	res := &ValidateResult{Valid: true, Errors: []ValidateIssue{}, Warnings: []ValidateIssue{}}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	if !strings.Contains(string(b), `"valid":true`) {
		t.Fatalf("应包含 valid 字段：%s", b)
	}
	if !strings.Contains(string(b), `"errors":[]`) {
		t.Fatalf("errors 应为空数组而非 null：%s", b)
	}
	if !strings.Contains(string(b), `"warnings":[]`) {
		t.Fatalf("warnings 应为空数组而非 null：%s", b)
	}
}

// TestImportPreviewShape 验证导入预览响应结构与规格书 8.9 的示例逐字段一致。
func TestImportPreviewShape(t *testing.T) {
	pv := &ImportPreview{
		Total:      50,
		WillCreate: 46,
		Conflicts: []ImportConflict{
			{Line: 7, Name: "test-a", Reason: "名称已存在", Suggestion: "改名为 test-a-2"},
			{Line: 12, Name: "test-b", Reason: "端口 8443 已被占用", Suggestion: "改用随机端口"},
		},
		Invalid: []ImportInvalid{
			{Line: 20, Raw: "bad#line", Reason: "格式错误：缺少目标端口"},
		},
	}
	b, err := json.Marshal(pv)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	got := string(b)
	for _, want := range []string{
		`"total":50`, `"will_create":46`,
		`"conflicts":[{"line":7,"name":"test-a","reason":"名称已存在","suggestion":"改名为 test-a-2"}`,
		`{"line":12,"name":"test-b","reason":"端口 8443 已被占用","suggestion":"改用随机端口"}]`,
		`"invalid":[{"line":20,"raw":"bad#line","reason":"格式错误：缺少目标端口"}]`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("预览响应缺少片段 %s\n实际: %s", want, got)
		}
	}
}
