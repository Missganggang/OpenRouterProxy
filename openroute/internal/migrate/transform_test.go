package migrate

import (
	"archive/zip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/openroute/openroute/internal/database"
	"github.com/openroute/openroute/internal/model"
	"github.com/openroute/openroute/internal/util"
)

// ---------------------------------------------------------------------------
// 名称冲突与重命名策略（规格书 4.1 / 7.4）
// ---------------------------------------------------------------------------

// TestResolveNameFailIsDefault 校验默认策略下重名直接报错。
func TestResolveNameFailIsDefault(t *testing.T) {
	used := map[string]string{}
	if _, keep, err := resolveName(RenameFail, "用户", "admin", used); err != nil || !keep {
		t.Fatalf("首次占用不应失败：keep=%v err=%v", keep, err)
	}
	// 「Admin」与「admin」大小写不同，但归一化后重名。
	_, _, err := resolveName(RenameFail, "用户", "Admin", used)
	if err == nil {
		t.Fatal("默认策略 fail 下重名必须报错")
	}
	if !strings.Contains(err.Error(), "名称冲突") {
		t.Fatalf("错误信息应说明是名称冲突，实际：%v", err)
	}
	if !strings.Contains(err.Error(), "rename-policy") {
		t.Fatalf("错误信息应提示 --rename-policy 选项，实际：%v", err)
	}
}

// TestResolveNameSuffix 校验 suffix 策略自动加 _2 后缀。
func TestResolveNameSuffix(t *testing.T) {
	used := map[string]string{}
	if _, _, err := resolveName(RenameSuffix, "节点", "HK-01", used); err != nil {
		t.Fatalf("首次占用不应失败：%v", err)
	}
	got, keep, err := resolveName(RenameSuffix, "节点", "hk-01", used)
	if err != nil || !keep {
		t.Fatalf("suffix 策略不应失败：keep=%v err=%v", keep, err)
	}
	if got != "hk-01_2" {
		t.Fatalf("期望加后缀得到 hk-01_2，实际 %q", got)
	}
	// 再来一个重名的应当拿到 _3。
	got, _, err = resolveName(RenameSuffix, "节点", "HK-01", used)
	if err != nil {
		t.Fatalf("第三次同样不应失败：%v", err)
	}
	if got != "HK-01_3" {
		t.Fatalf("期望 HK-01_3，实际 %q", got)
	}
}

// TestResolveNameSkip 校验 skip 策略跳过冲突项。
func TestResolveNameSkip(t *testing.T) {
	used := map[string]string{}
	if _, _, err := resolveName(RenameSkip, "用户", "admin", used); err != nil {
		t.Fatalf("首次占用不应失败：%v", err)
	}
	got, keep, err := resolveName(RenameSkip, "用户", "Admin", used)
	if err != nil {
		t.Fatalf("skip 策略不应返回错误：%v", err)
	}
	if keep || got != "" {
		t.Fatalf("skip 策略应当跳过该条：keep=%v got=%q", keep, got)
	}
}

// TestParseRenamePolicy 校验策略字符串解析与默认值。
func TestParseRenamePolicy(t *testing.T) {
	cases := []struct {
		in      string
		want    RenamePolicy
		wantErr bool
	}{
		{"", RenameFail, false},
		{"fail", RenameFail, false},
		{"FAIL", RenameFail, false},
		{" suffix ", RenameSuffix, false},
		{"skip", RenameSkip, false},
		{"whatever", RenameFail, true},
	}
	for _, c := range cases {
		got, err := ParseRenamePolicy(c.in)
		if c.wantErr {
			if err == nil {
				t.Fatalf("%q 应当报错", c.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q 不应报错：%v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("%q 期望 %v，实际 %v", c.in, c.want, got)
		}
	}
}

// TestDetectNameConflicts 校验跨库名称冲突检测（规格书 4.1）。
func TestDetectNameConflicts(t *testing.T) {
	users := []nyUser{{ID: 1, Username: "Admin"}, {ID: 2, Username: "admin"}, {ID: 3, Username: "bob"}}
	nodes := []nyNode{{ID: 1, Name: "HK-01"}, {ID: 2, Name: "hk-01"}}
	groups := []nyDeviceGroup{{ID: 1, Name: "In"}}
	ruleGroups := []nyRuleGroup{{ID: 1, Name: "默认"}}
	rules := []nyRule{{ID: 1, Name: "test-a"}, {ID: 2, Name: "TEST-A"}}

	got := detectNameConflicts(users, nodes, nil, groups, ruleGroups, rules)

	byResource := map[string]int{}
	for _, c := range got {
		byResource[c.Resource]++
	}
	if byResource["user"] != 1 {
		t.Fatalf("期望 1 处用户名称冲突，实际 %d", byResource["user"])
	}
	if byResource["node"] != 1 {
		t.Fatalf("期望 1 处节点名称冲突，实际 %d", byResource["node"])
	}
	if byResource["rule"] != 1 {
		t.Fatalf("期望 1 处规则名称冲突，实际 %d", byResource["rule"])
	}
	if byResource["device_group"] != 0 {
		t.Fatalf("设备组没有冲突，实际 %d", byResource["device_group"])
	}
	for _, c := range got {
		if c.Suggestion == "" {
			t.Fatalf("冲突项必须带建议处理方式：%+v", c)
		}
	}
}

// ---------------------------------------------------------------------------
// 端口冲突 / 引用缺失 / 零倍率（规格书 7.2 阶段 3）
// ---------------------------------------------------------------------------

// TestDetectPortConflicts 校验同一入口组内的端口冲突检测。
func TestDetectPortConflicts(t *testing.T) {
	rules := []nyRule{
		{ID: 1, Name: "test-a", InboundGroupID: 10, Port: 8443},
		{ID: 2, Name: "test-b", InboundGroupID: 10, Port: 8443},
		{ID: 3, Name: "test-c", InboundGroupID: 11, Port: 8443},
		{ID: 4, Name: "sub", InboundGroupID: 10, Port: 8443, IsSubRule: true},
	}
	names := map[uint64]string{10: "HK-In", 11: "US-In"}

	got := detectPortConflicts(rules, names)
	if len(got) != 1 {
		t.Fatalf("期望 1 处端口冲突，实际 %d：%+v", len(got), got)
	}
	if got[0].Port != 8443 || got[0].InboundGroupName != "HK-In" {
		t.Fatalf("冲突内容不对：%+v", got[0])
	}
	if len(got[0].Rules) != 2 {
		t.Fatalf("期望冲突规则 2 条，实际 %+v", got[0].Rules)
	}
	// 子规则不单独监听端口，不应计入冲突。
	for _, name := range got[0].Rules {
		if name == "sub" {
			t.Fatal("子规则不应计入端口冲突")
		}
	}
}

// TestDetectMissingRefs 校验引用缺失检测。
func TestDetectMissingRefs(t *testing.T) {
	users := []nyUser{{ID: 1, Username: "admin"}}
	groups := []nyDeviceGroup{
		{ID: 10, Name: "In", Type: "inbound"},
		{ID: 20, Name: "Out", Type: "outbound"},
	}
	ruleGroups := []nyRuleGroup{{ID: 5, Name: "默认"}}
	rules := []nyRule{
		{ID: 1, Name: "ok", InboundGroupID: 10, OutboundGroup: 20, UserID: 1, RuleGroupID: 5},
		{ID: 2, Name: "bad-in", InboundGroupID: 999},
		{ID: 3, Name: "bad-out", InboundGroupID: 10, OutboundGroup: 888},
		{ID: 4, Name: "bad-user", InboundGroupID: 10, UserID: 777},
		{ID: 5, Name: "sub", InboundGroupID: 10, IsSubRule: true, ParentID: 12345},
	}

	got := detectMissingRefs(rules, users, groups, ruleGroups, nil)

	fields := map[string]int{}
	for _, m := range got {
		fields[m.Field]++
	}
	if fields["inbound_group_id"] != 1 {
		t.Fatalf("期望 1 处入口组缺失，实际 %d", fields["inbound_group_id"])
	}
	if fields["outbound_group_id"] != 1 {
		t.Fatalf("期望 1 处出口组缺失，实际 %d", fields["outbound_group_id"])
	}
	if fields["user_id"] != 1 {
		t.Fatalf("期望 1 处用户缺失，实际 %d", fields["user_id"])
	}
	if fields["parent_id"] != 1 {
		t.Fatalf("期望 1 处主规则缺失，实际 %d", fields["parent_id"])
	}
	// 每条缺失都必须自带可读说明，报告直接展示它。
	for _, m := range got {
		if m.Message == "" {
			t.Fatalf("引用缺失必须带说明：%+v", m)
		}
	}
}

// TestDetectZeroMultipliers 校验倍率为 0 的设备组检测。
func TestDetectZeroMultipliers(t *testing.T) {
	groups := []nyDeviceGroup{
		{ID: 1, Name: "zero", Type: "inbound", Multiplier: 0},
		{ID: 2, Name: "one", Type: "inbound", Multiplier: 1},
		{ID: 3, Name: "half", Type: "outbound", Multiplier: 0.5},
	}
	got := detectZeroMultipliers(groups)
	if len(got) != 1 {
		t.Fatalf("期望 1 个零倍率组，实际 %d", len(got))
	}
	if got[0].Name != "zero" {
		t.Fatalf("命中的组不对：%+v", got[0])
	}
}

// ---------------------------------------------------------------------------
// 字段映射辅助
// ---------------------------------------------------------------------------

// TestExpandTargets 校验单目标展开与多目标保留（规格书 7.2）。
func TestExpandTargets(t *testing.T) {
	// 单目标：由 target_host / target_port 构造一条。
	got := expandTargets(nyRule{TargetHost: "example.com", TargetPort: 443, TargetWeight: 3})
	if len(got) != 1 {
		t.Fatalf("期望 1 个目标，实际 %d", len(got))
	}
	if got[0].Host != "example.com" || got[0].Port != 443 || got[0].Weight != 3 {
		t.Fatalf("目标内容不对：%+v", got[0])
	}
	if got[0].Status != model.TargetUp {
		t.Fatalf("默认状态应为 up，实际 %q", got[0].Status)
	}

	// 多目标：已有 targets 数组时原样展开，非法条目被过滤。
	raw := `[{"host":"a.com","port":80},{"host":"b.com","port":8080},{"host":"","port":0}]`
	got = expandTargets(nyRule{Targets: raw})
	if len(got) != 2 {
		t.Fatalf("期望过滤掉非法条目后剩 2 个，实际 %d：%+v", len(got), got)
	}
	if got[1].Host != "b.com" {
		t.Fatalf("第二个目标不对：%+v", got[1])
	}

	// 无目标：入口直出的规则本来就没有目标，不应报错。
	if got = expandTargets(nyRule{}); len(got) != 0 {
		t.Fatalf("无目标时应返回空列表，实际 %+v", got)
	}
}

// TestSpeedBytes 校验限速单位折算。
func TestSpeedBytes(t *testing.T) {
	// 小数值按 KB/s 折算。
	if got := speedBytes(100); got != 100*1024 {
		t.Fatalf("100 应折算为 102400，实际 %d", got)
	}
	// 大数值视为已是 byte/s。
	if got := speedBytes(1048576); got != 1048576 {
		t.Fatalf("大数值应原样保留，实际 %d", got)
	}
	if got := speedBytes(0); got != 0 {
		t.Fatalf("0 应保持 0，实际 %d", got)
	}
}

// TestNormalizeTrafficDimensions 校验流量维度归一化。
func TestNormalizeTrafficDimensions(t *testing.T) {
	// 日期：已经是 YYYY-MM-DD 的原样保留，不做时区重算（规格书 7.4）。
	if got := normalizeDate("2026-01-01"); got != "2026-01-01" {
		t.Fatalf("标准日期应原样保留，实际 %q", got)
	}
	// 紧凑写法补分隔符。
	if got := normalizeDate("20260101"); got != "2026-01-01" {
		t.Fatalf("紧凑日期应补分隔符，实际 %q", got)
	}
	if got := normalizeDate(""); got != "" {
		t.Fatalf("空日期应为空，实际 %q", got)
	}

	// 小时：越界归一到 -1（按天）。
	cases := map[int]int{-1: model.HourDaily, 2525: model.HourDaily, 24: model.HourDaily, 0: 0, 23: 23}
	for in, want := range cases {
		if got := normalizeHour(in); got != want {
			t.Fatalf("hour %d 期望 %d，实际 %d", in, want, got)
		}
	}

	// 方向：各种写法归一为 in / out。
	dirs := map[string]string{
		"in": model.DirectionIn, "upload": model.DirectionIn, "up": model.DirectionIn,
		"out": model.DirectionOut, "download": model.DirectionOut, "": model.DirectionIn,
	}
	for in, want := range dirs {
		if got := normalizeDirection(in); got != want {
			t.Fatalf("direction %q 期望 %q，实际 %q", in, want, got)
		}
	}
}

// TestJSONHelpers 校验 JSON 字段规范化。
func TestJSONHelpers(t *testing.T) {
	// 合法 JSON 原样保留（规格书要求 Config 原样迁移）。
	in := `{"protocol":"tls","tls":{}}`
	if got := jsonOrNull(in); got != in {
		t.Fatalf("合法 JSON 应原样保留，实际 %q", got)
	}
	// 非法 JSON 会被包装成 JSON 字符串，保证写入目标库时不报错。
	if got := jsonOrNull("{not json"); !util.IsValidJSON(got) {
		t.Fatalf("非法 JSON 应被包装为合法 JSON，实际 %q", got)
	}
	if got := jsonOrNull(nil); got != "null" {
		t.Fatalf("nil 应得到 null，实际 %q", got)
	}
	// 数组字段的空值语义是 []，不是 null。
	if got := string(jsonArrayOrNull("")); got != "[]" {
		t.Fatalf("空数组字段应为 []，实际 %q", got)
	}
	if got := string(jsonObjectOrNull("")); got != "{}" {
		t.Fatalf("空对象字段应为 {}，实际 %q", got)
	}
}

// TestParseListValue 校验「JSON 数组 / 逗号分隔」两种存法的解析。
func TestParseListValue(t *testing.T) {
	if got := parseListValue(`[1,2,3]`); len(got) != 3 || got[2] != 3 {
		t.Fatalf("JSON 数组解析失败：%+v", got)
	}
	if got := parseListValue("1,2,3"); len(got) != 3 || got[1] != 2 {
		t.Fatalf("逗号分隔解析失败：%+v", got)
	}
	if got := parseListValue(""); len(got) != 0 {
		t.Fatalf("空值应解析为空列表，实际 %+v", got)
	}
	// 非法条目被跳过而不是整体失败。
	if got := parseListValue("1,abc,3"); len(got) != 2 {
		t.Fatalf("非法条目应被跳过，实际 %+v", got)
	}
}

// TestAsTimeUTC 校验时间归一化到 UTC（规格书 7.3）。
func TestAsTimeUTC(t *testing.T) {
	// Unix 秒。
	got := asTime(int64(1767225600))
	if got.Location() != time.UTC {
		t.Fatalf("时间应当是 UTC，实际 %v", got.Location())
	}
	if got.Format("2006-01-01") != "2026-01-01" {
		t.Fatalf("时间戳解析结果不对：%v", got)
	}
	// 带时区的字符串会被转换到 UTC。
	shanghai := time.FixedZone("CST", 8*3600)
	local := time.Date(2026, 1, 1, 8, 0, 0, 0, shanghai)
	if got := asTime(local); got.UTC().Hour() != 0 {
		t.Fatalf("带时区时间应转换到 UTC，实际 %v", got.UTC())
	}
	// 零值时间转成 nil 指针，避免写入 0001-01-01。
	if timePtr(time.Time{}) != nil {
		t.Fatal("零值时间应转为 nil")
	}
}

// ---------------------------------------------------------------------------
// ID 分配器
// ---------------------------------------------------------------------------

// TestIDAllocator 校验「优先保留源 ID、冲突时后移」的策略。
//
// 分配器的游标始终单调递增，不会回退填补更小的空洞：
// 这是有意为之——回填空洞会让 migrate_id_map 里的 target_id 显得跳来跳去，
// 迁移失败时人工排查更困难；单调递增的代价只是留下少量空洞，完全可以接受。
func TestIDAllocator(t *testing.T) {
	a := newIDAllocator()
	// 目标库已有 1、2。
	a.reserve(1)
	a.reserve(2)

	// 空闲的 ID 直接使用。
	if got := a.take(5); got != 5 {
		t.Fatalf("空闲 ID 应直接使用，实际 %d", got)
	}
	// take(5) 把游标推进到 6，因此接下来是 6（而不是回填 3）。
	if got := a.take(1); got != 6 {
		t.Fatalf("已占用的 ID 应后移，期望 6 实际 %d", got)
	}
	if got := a.take(0); got != 7 {
		t.Fatalf("未指定 ID 时应取下一个可用值，期望 7 实际 %d", got)
	}
	// 分配出去的 ID 必须被标记为已占用，否则后续行会撞主键。
	for _, id := range []uint64{1, 2, 5, 6, 7} {
		if !a.used[id] {
			t.Fatalf("ID %d 已分配但未标记为占用：%v", id, a.used)
		}
	}
	// 再次取用已占用的 ID 时必须继续后移。
	if got := a.take(5); got != 8 {
		t.Fatalf("重复请求已占用的 ID 应继续后移，期望 8 实际 %d", got)
	}
}

// ---------------------------------------------------------------------------
// 报告渲染（规格书 7.2 的样例格式）
// ---------------------------------------------------------------------------

// TestReportRenderMatchesSpec 校验报告渲染包含规格书要求的全部版式元素。
func TestReportRenderMatchesSpec(t *testing.T) {
	r := NewReport("nyanpass", "Nyanpass 20260301", "sqlite3 (./data.db)", true)
	r.SourceTableCount = 16
	r.Tables = []ReportTableItem{
		{Label: "用户", Count: 12, Target: "users"},
		{Label: "用户分组", Count: 3, Target: "user_groups"},
		{Label: "节点", Count: 28, Target: "nodes"},
		{Label: "节点分组", Count: 4, Target: "node_groups"},
		{Label: "设备组", Count: 16, Target: "device_groups", Extra: "入口 9 / 出口 7"},
		{Label: "规则分组", Count: 5, Target: "rule_groups"},
		{Label: "转发规则", Count: 214, Target: "forward_rules"},
		{Label: "流量记录", Count: 18204, Target: "traffic_logs", Extra: "按天聚合后约 900 条"},
	}
	r.NameConflicts = []NameConflict{
		{Resource: "user", Left: "Admin", Right: "admin", Suggestion: `在目标库将合并 → 建议重命名为 "admin2"`},
		{Resource: "node", Left: "HK-01", Right: "hk-01", Suggestion: `在目标库将合并 → 建议重命名为 "hk-012"`},
	}
	r.PortConflicts = []PortConflict{
		{InboundGroupID: 1, InboundGroupName: "HK-In", Port: 8443, Rules: []string{"test-a", "test-b"}},
	}
	r.ZeroMultipliers = []ZeroMultiplierGroup{{ID: 9, Name: "零倍率组", Type: "inbound", Rate: 0}}
	r.Discards = []DiscardItem{
		{Source: "users.license_key"},
		{Source: "users.recharge_total"},
		{Source: "orders.*"},
		{Source: "products.*"},
		{Source: "payment_configs.*"},
		{Source: "license_domain"},
		{Source: "expire_license_at"},
	}
	r.Suggestions = r.defaultSuggestions()

	out := r.Render()

	// 版式元素：粗框线标题、字段行、四个章节。
	//
	// 框线宽度取规格书样例的实际宽度：OpenRouter.md 里所有框线行都是 55 个字符，
	// 因此这里按 55 断言，而不是硬编码一串容易数错的字面量。
	const boxWidth = 55
	mustContain := []string{
		strings.Repeat("═", boxWidth),
		"Nyanpass → OpenRoute 迁移预检报告",
		"源库", "源版本", "目标库", "迁移模式",
		"DRY-RUN（不会写入任何数据）",
		strings.Repeat("─", boxWidth),
		"【将迁移的数据】",
		"【冲突与风险】",
		"【不迁移的字段（已识别并丢弃）】",
		"【建议操作】",
		"用户", "12 条", "users",
		"流量记录", "18204 条", "traffic_logs", "按天聚合后约 900 条",
		"⚠ 名称冲突 2 处（MySQL/PG 大小写敏感）",
		"⚠ 端口冲突 1 处",
		"⚠ 引用缺失 0 处",
		"⚠ 倍率为 0 的设备组 1 个 → 统计将显示为 0（源库即为如此）",
		"users.license_key",
		"orders.*",
		"-backup",
	}
	for _, want := range mustContain {
		if !strings.Contains(out, want) {
			t.Fatalf("报告缺少片段 %q\n---- 完整报告 ----\n%s", want, out)
		}
	}

	// 尾部必须是闭合的粗框线。
	if !strings.HasSuffix(out, strings.Repeat("═", boxWidth)+"\n") {
		t.Fatalf("报告末尾应当是闭合框线，实际结尾：%q", out[len(out)-60:])
	}
}

// TestReportJSONRoundTrip 校验 JSON 形式包含结构化内容且可序列化。
func TestReportJSONRoundTrip(t *testing.T) {
	r := NewReport("nyanpass", "Nyanpass 20260301", "sqlite3 (./data.db)", true)
	r.Tables = []ReportTableItem{{Label: "用户", Count: 3, Target: "users"}}
	r.RowsToWrite = map[string]int64{"users": 3}
	r.TotalRows = 3
	r.Suggestions = r.defaultSuggestions()

	jr := r.ToJSONReport()
	if jr.Status != "precheck" {
		t.Fatalf("dry-run 报告状态应为 precheck，实际 %q", jr.Status)
	}
	if jr.Text == "" {
		t.Fatal("JSON 报告应内嵌纯文本内容")
	}
	if jr.Summary.TotalRows != 3 {
		t.Fatalf("概览行数不对：%+v", jr.Summary)
	}
	// 空集合必须序列化成 [] 而不是 null，前端才能无脑遍历。
	if jr.NameConflicts == nil || jr.Discards == nil || jr.Suggestions == nil {
		t.Fatal("空的冲突 / 丢弃 / 建议列表不应为 nil")
	}

	b, err := r.MarshalJSONReport()
	if err != nil {
		t.Fatalf("JSON 序列化失败：%v", err)
	}
	var back map[string]interface{}
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("序列化结果不是合法 JSON：%v", err)
	}
	for _, key := range []string{"summary", "tables", "name_conflicts", "discards", "text"} {
		if _, ok := back[key]; !ok {
			t.Fatalf("JSON 报告缺少字段 %q", key)
		}
	}
}

// TestBuildMigrationNotes 校验迁移后必做事项（规格书 7.2 硬性要求）。
func TestBuildMigrationNotes(t *testing.T) {
	notes := BuildMigrationNotes(214, 28)
	joined := strings.Join(notes, "\n")

	mustContain := []string{
		"节点密钥全部重新生成",
		"nsk_", // token 前缀说明
		"节点客户端不兼容",
		"openroute.uninstall.sh",
		"unsynced",
		"大小写敏感",
	}
	for _, want := range mustContain {
		if !strings.Contains(joined, want) {
			t.Fatalf("迁移后提示缺少 %q\n---- 实际 ----\n%s", want, joined)
		}
	}
}

// ---------------------------------------------------------------------------
// 阶段编号与顺序（规格书 7.2 / 7.3）
// ---------------------------------------------------------------------------

// TestStagesAndCopyOrder 校验 7 个阶段与跨库复制的表顺序。
func TestStagesAndCopyOrder(t *testing.T) {
	stages := Stages()
	if len(stages) != StageTotal {
		t.Fatalf("阶段数量应为 %d，实际 %d", StageTotal, len(stages))
	}
	if StageConnect != 1 || StageScan != 2 || StageConflict != 3 ||
		StageDryRun != 4 || StageMigrate != 5 || StageVerify != 6 || StageRollback != 7 {
		t.Fatalf("阶段编号与规格书不一致：%d %d %d %d %d %d %d",
			StageConnect, StageScan, StageConflict, StageDryRun,
			StageMigrate, StageVerify, StageRollback)
	}

	order := CopyOrder()
	if len(order) != 16 {
		t.Fatalf("跨库复制应覆盖 16 张表，实际 %d：%v", len(order), order)
	}
	// 规格书给定顺序：先父表后子表。
	want := []string{
		"users", "user_groups", "node_groups", "nodes",
		"rule_groups", "device_groups", "forward_rules",
		"traffic_logs", "sessions", "probe_metrics",
		"system_settings", "audit_logs", "api_tokens",
		"config_snapshots", "alert_rules", "alert_histories",
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("复制顺序第 %d 项应为 %q，实际 %q", i+1, want[i], order[i])
		}
	}
}

// TestDiscardTableDetection 校验支付 / 授权相关表被整体跳过（规格书 1.3、7.2）。
func TestDiscardTableDetection(t *testing.T) {
	// 用内存库伪造一张源库，只建「有数据的表」。
	db := newMemDB(t)
	mustExec(t, db, `create table users (id integer, username text)`)
	mustExec(t, db, `create table orders (id integer)`)
	mustExec(t, db, `create table products (id integer)`)
	mustExec(t, db, `create table payment_configs (id integer)`)
	mustExec(t, db, `create table order_items (id integer)`)
	mustExec(t, db, `create table device_groups (id integer, name text, config text)`)

	src := NewNyanpassSource(db)
	got := detectDiscardTables(context.Background(), src)

	set := map[string]bool{}
	for _, g := range got {
		set[g] = true
	}
	for _, want := range []string{"orders", "products", "payment_configs", "order_items"} {
		if !set[want] {
			t.Fatalf("表 %q 应当被识别为丢弃表，实际 %v", want, got)
		}
	}
	if set["device_groups"] {
		t.Fatalf("device_groups 不应出现在丢弃表中：%v", got)
	}
	if set["users"] {
		t.Fatalf("users 不应出现在丢弃表中：%v", got)
	}
}

// TestBuildDiscardItems 校验字段级丢弃清单只列出「源库真的存在」的列。
func TestBuildDiscardItems(t *testing.T) {
	db := newMemDB(t)
	mustExec(t, db, `create table users (id integer, username text, license_key text, recharge_total integer)`)

	src := NewNyanpassSource(db)
	items := buildDiscardItems(context.Background(), src, nil)

	set := map[string]bool{}
	for _, it := range items {
		set[it.Source] = true
		if it.Reason == "" {
			t.Fatalf("丢弃项必须说明原因：%+v", it)
		}
	}
	if !set["users.license_key"] {
		t.Fatalf("应识别 users.license_key，实际 %v", items)
	}
	if !set["users.recharge_total"] {
		t.Fatalf("应识别 users.recharge_total，实际 %v", items)
	}
	// 源库不存在的字段不应出现在清单里。
	if set["users.money"] {
		t.Fatalf("源库没有 users.money，不应列出：%v", items)
	}
}

// ---------------------------------------------------------------------------
// 断点续传与幂等（规格书 7.2 阶段 5）
// ---------------------------------------------------------------------------

// TestMigratedSetRoundTrip 校验 migrate_id_map 的去重键读写。
func TestMigratedSetRoundTrip(t *testing.T) {
	if got := idMapKey("users", 12); got != "users#12" {
		t.Fatalf("去重键格式不对：%q", got)
	}
}

// TestToRowsReverseMapping 校验「目标 ID → 源 ID」的反向还原。
//
// 迁移时模型上带的是目标 ID，但 migrate_id_map 要记源 ID，
// 这个反向还原是断点续传与回滚的基础，必须准确。
func TestToRowsReverseMapping(t *testing.T) {
	users := []model.User{
		{ID: 10, Username: "admin"},
		{ID: 11, Username: "bob"},
	}
	idMap := map[uint64]uint64{1: 10, 2: 11} // 源 1 → 目标 10，源 2 → 目标 11

	rows := toRows(users, func(v model.User) uint64 { return v.ID }, idMap)
	if len(rows) != 2 {
		t.Fatalf("期望 2 行，实际 %d", len(rows))
	}
	if rows[0].sourceID != 1 || rows[1].sourceID != 2 {
		t.Fatalf("源 ID 还原失败：%d, %d", rows[0].sourceID, rows[1].sourceID)
	}
	u, ok := rows[0].value.(*model.User)
	if !ok || u.Username != "admin" {
		t.Fatalf("模型值还原失败：%+v", rows[0].value)
	}
}

// ---------------------------------------------------------------------------
// 备份与恢复（规格书 6.15）
// ---------------------------------------------------------------------------

// TestBackupRestoreRoundTrip 校验备份包结构与恢复流程。
func TestBackupRestoreRoundTrip(t *testing.T) {
	dir := t.TempDir()

	// 造一个 SQLite「数据文件」与一份配置。
	dbPath := filepath.Join(dir, "data.db")
	db := openAt(t, dbPath)
	mustExec(t, db, `create table users (id integer primary key, username text)`)
	mustExec(t, db, `insert into users (username) values ('admin')`)
	if err := db.Close(); err != nil {
		t.Fatalf("关闭数据库失败：%v", err)
	}

	cfgPath := filepath.Join(dir, "config.yml")
	cfgContent := "database-path: \"./data.db\"\nsecret-key: \"super-secret-value\"\nlisten: 0.0.0.0:18888\n"
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0o600); err != nil {
		t.Fatalf("写配置失败：%v", err)
	}

	outZip := filepath.Join(dir, "backup.zip")

	// —— 默认脱敏备份 ——
	res, err := CreateBackup(context.Background(), BackupOptions{
		DataDBPath: dbPath,
		ConfigPath: cfgPath,
		OutZipPath: outZip,
		WithSecret: false,
		AppVersion: "v1.0.0-test",
	})
	if err != nil {
		t.Fatalf("生成备份失败：%v", err)
	}
	if res.Size <= 0 || res.Checksum == "" {
		t.Fatalf("备份结果缺少大小或校验和：%+v", res)
	}
	if res.Manifest.DataEntry != BackupEntryData {
		t.Fatalf("SQLite 备份的数据条目应为 %s，实际 %s", BackupEntryData, res.Manifest.DataEntry)
	}
	if res.Manifest.WithSecret {
		t.Fatal("默认不应保留 secret-key")
	}
	if res.Manifest.Tables["users"] != 1 {
		t.Fatalf("清单中的行数不对：%+v", res.Manifest.Tables)
	}
	if res.Manifest.TotalRows != 1 {
		t.Fatalf("清单总行数不对：%d", res.Manifest.TotalRows)
	}

	// 包内条目齐全。
	names := zipEntryNames(t, outZip)
	for _, want := range []string{BackupEntryData, BackupEntryConfig, BackupEntryManifest} {
		if !names[want] {
			t.Fatalf("备份包缺少条目 %q，实际 %v", want, names)
		}
	}

	// secret-key 已脱敏。
	cfgInZip := readZipEntry(t, outZip, BackupEntryConfig)
	if strings.Contains(cfgInZip, "super-secret-value") {
		t.Fatalf("secret-key 未被脱敏：\n%s", cfgInZip)
	}
	if !strings.Contains(cfgInZip, "REDACTED") {
		t.Fatalf("脱敏占位串缺失：\n%s", cfgInZip)
	}
	// 其它配置项必须保留，否则恢复后用户还得手工补。
	if !strings.Contains(cfgInZip, "0.0.0.0:18888") {
		t.Fatalf("脱敏时不应丢其它配置：\n%s", cfgInZip)
	}

	// —— 恢复：先破坏数据，再恢复 ——
	mustExec(t, openAt(t, dbPath), `delete from users`)
	if err := os.Remove(cfgPath); err != nil && !os.IsNotExist(err) {
		t.Fatalf("删除配置失败：%v", err)
	}

	if _, err := RestoreBackup(outZip, dbPath, cfgPath); err != nil {
		t.Fatalf("恢复失败：%v", err)
	}

	// 恢复前快照必须存在（规格书 6.15 硬性要求）。
	if _, err := os.Stat(dbPath + ".before-restore"); err != nil {
		t.Fatalf("恢复前应生成 data.db.before-restore：%v", err)
	}
	// 数据已回来。
	restored := openAt(t, dbPath)
	var n int64
	if err := restored.Raw(`select count(*) from users`).Scan(&n).Error; err != nil {
		t.Fatalf("读取恢复后的数据失败：%v", err)
	}
	if n != 1 {
		t.Fatalf("恢复后应有 1 行，实际 %d", n)
	}
	// 配置也回来了。
	if _, err := os.Stat(cfgPath); err != nil {
		t.Fatalf("恢复后配置应存在：%v", err)
	}
}

// TestBackupWithSecret 校验 --with-secret 时保留密钥。
func TestBackupWithSecret(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "data.db")
	_ = openAt(t, dbPath)

	cfgPath := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(cfgPath, []byte("secret-key: \"keep-me-please\"\n"), 0o600); err != nil {
		t.Fatalf("写配置失败：%v", err)
	}

	outZip := filepath.Join(dir, "backup-secret.zip")
	res, err := CreateBackup(context.Background(), BackupOptions{
		DataDBPath: dbPath,
		ConfigPath: cfgPath,
		OutZipPath: outZip,
		WithSecret: true,
	})
	if err != nil {
		t.Fatalf("生成备份失败：%v", err)
	}
	if !res.Manifest.WithSecret {
		t.Fatal("--with-secret 时清单应标记 WithSecret")
	}
	if got := readZipEntry(t, outZip, BackupEntryConfig); !strings.Contains(got, "keep-me-please") {
		t.Fatalf("--with-secret 时应保留密钥，实际：\n%s", got)
	}
}

// TestRestoreRejectsNewerFormat 校验更高版本的备份格式被拒绝。
func TestRestoreRejectsNewerFormat(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "future.zip")

	if err := writeTestZip(zipPath, map[string]string{
		BackupEntryManifest: `{"version":999,"data_entry":"data.db","tables":{}}`,
		BackupEntryData:     "not a real db",
	}); err != nil {
		t.Fatalf("构造测试备份失败：%v", err)
	}

	_, err := RestoreBackup(zipPath, filepath.Join(dir, "data.db"), "")
	if err == nil {
		t.Fatal("更高版本的备份格式应当被拒绝")
	}
	if !strings.Contains(err.Error(), "升级面板") {
		t.Fatalf("错误信息应提示升级面板，实际：%v", err)
	}
}

// TestRegisterBackupIsIdempotent 校验同一路径重复登记会覆盖而不是新增。
func TestRegisterBackupIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "data.db")
	db := openAt(t, dbPath)
	if _, err := db.AutoMigrate(); err != nil {
		t.Fatalf("建表失败：%v", err)
	}

	zipPath := filepath.Join(dir, "b.zip")
	if err := writeTestZip(zipPath, map[string]string{
		BackupEntryManifest: `{"version":1,"tables":{"users":3}}`,
	}); err != nil {
		t.Fatalf("构造测试备份失败：%v", err)
	}

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := RegisterBackup(ctx, db, zipPath, nil, false); err != nil {
			t.Fatalf("第 %d 次登记失败：%v", i+1, err)
		}
	}

	list, err := ListBackups(ctx, db)
	if err != nil {
		t.Fatalf("读取备份列表失败：%v", err)
	}
	if len(list) != 1 {
		t.Fatalf("同一路径重复登记应只有 1 条记录，实际 %d", len(list))
	}
	if list[0].Manifest == nil || list[0].Manifest.Tables["users"] != 3 {
		t.Fatalf("清单未正确解析：%+v", list[0].Manifest)
	}
}

// ---------------------------------------------------------------------------
// 跨库类型转换（规格书 7.3）
// ---------------------------------------------------------------------------

// TestConvertRowTypeConversion 校验 JSON 校验、布尔显式转换与 UTC 归一。
func TestConvertRowTypeConversion(t *testing.T) {
	schema := targetTableSchema{
		All:  map[string]bool{"id": true, "config": true, "enable": true, "created_at": true},
		JSON: map[string]bool{"config": true},
		Bool: map[string]bool{"enable": true},
	}

	// 合法 JSON + TINYINT(1) + 时间：正常转换。
	row := map[string]interface{}{
		"id":         1,
		"config":     `{"a":1}`,
		"enable":     1,
		"created_at": "2026-01-01 08:00:00",
		"gone":       "源库多出来的列",
	}
	got, ok := convertRow(row, schema)
	if !ok {
		t.Fatal("合法行不应被跳过")
	}
	if _, exists := got["gone"]; exists {
		t.Fatal("目标表不存在的列应当被丢弃")
	}
	if got["config"] != `{"a":1}` {
		t.Fatalf("合法 JSON 应原样保留，实际 %v", got["config"])
	}
	if b, _ := got["enable"].(bool); !b {
		t.Fatalf("TINYINT(1) 应显式转为 bool，实际 %#v", got["enable"])
	}
	ts, _ := got["created_at"].(time.Time)
	if ts.Location() != time.UTC {
		t.Fatalf("时间应转为 UTC，实际 %v", ts.Location())
	}

	// 非法 JSON：整行跳过。
	if _, ok := convertRow(map[string]interface{}{"id": 2, "config": "{not json"}, schema); ok {
		t.Fatal("非法 JSON 的行应当被跳过（规格书 7.3）")
	}

	// 空 JSON：写入 NULL 而不是跳过。
	got, ok = convertRow(map[string]interface{}{"id": 3, "config": ""}, schema)
	if !ok || got["config"] != nil {
		t.Fatalf("空 JSON 应写入 NULL，实际 ok=%v value=%#v", ok, got["config"])
	}
}

// TestCanonicalRowIsColumnOrderIndependent 校验校验和不受列顺序影响。
func TestCanonicalRowIsColumnOrderIndependent(t *testing.T) {
	cols := map[string]bool{"id": true, "name": true}
	a := canonicalRow(map[string]interface{}{"id": 1, "name": "x"}, cols, cols)
	b := canonicalRow(map[string]interface{}{"name": "x", "id": 1}, cols, cols)
	if a != b {
		t.Fatalf("列顺序不同的同一行应得到相同校验和：\n%q\n%q", a, b)
	}

	// 目标表没有的列不参与校验和，避免「源库多一列」导致永远不一致。
	c := canonicalRow(map[string]interface{}{"id": 1, "name": "x", "extra": "y"}, cols, cols)
	if a != c {
		t.Fatalf("目标表不存在的列不应影响校验和：\n%q\n%q", a, c)
	}
}

// TestPercentDiff 校验流量总量差异百分比计算。
func TestPercentDiff(t *testing.T) {
	if got := percentDiff(1000, 1000); got != 0 {
		t.Fatalf("完全一致应为 0，实际 %v", got)
	}
	// 允许 ±0.1% 的边界。
	if got := percentDiff(1000000, 1001000); got > TrafficTolerancePercent {
		t.Fatalf("0.1%% 的差异应在容差内，实际 %v", got)
	}
	if got := percentDiff(1000000, 1010000); got <= TrafficTolerancePercent {
		t.Fatalf("1%% 的差异应超出容差，实际 %v", got)
	}
	// 目标为 0 而源不为 0：完全丢失，必须超出容差。
	if got := percentDiff(100, 0); got <= TrafficTolerancePercent {
		t.Fatalf("目标为 0 时应判为超差，实际 %v", got)
	}
}

// TestCompactJSON 校验「原样迁移」的 JSON 内容比对忽略空白。
func TestCompactJSON(t *testing.T) {
	a := `{"protocol":"tls",  "tls": { }}`
	b := "{\"protocol\":\"tls\",\n\"tls\":{}}"
	if compactJSON(a) != compactJSON(b) {
		t.Fatalf("仅空白不同的 JSON 应视为内容一致：\n%s\n%s", compactJSON(a), compactJSON(b))
	}
	// 字符串内部的空白不能被动。
	c := `{"host":"a b"}`
	if compactJSON(c) != c {
		t.Fatalf("字符串内的空白不应被删除：%s", compactJSON(c))
	}
}

// ---------------------------------------------------------------------------
// 测试辅助
// ---------------------------------------------------------------------------

// newMemDB 打开一个内存 SQLite 库。
func newMemDB(t *testing.T) *database.DB {
	t.Helper()
	db, err := database.Open(database.Options{Path: ":memory:", MaxOpen: 1, MaxIdle: 1})
	if err != nil {
		t.Fatalf("打开内存数据库失败：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// openAt 打开指定文件的 SQLite 库。
func openAt(t *testing.T, path string) *database.DB {
	t.Helper()
	db, err := database.Open(database.Options{Path: path, MaxOpen: 1, MaxIdle: 1})
	if err != nil {
		t.Fatalf("打开数据库 %s 失败：%v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// mustExec 执行一条 SQL，失败即终止测试。
func mustExec(t *testing.T, db *database.DB, sql string) {
	t.Helper()
	if err := db.Exec(sql).Error; err != nil {
		t.Fatalf("执行 SQL 失败（%s）：%v", sql, err)
	}
}

// zipEntryNames 返回 zip 包内的条目名集合。
func zipEntryNames(t *testing.T, zipPath string) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("打开备份包失败：%v", err)
	}
	defer func() { _ = zr.Close() }()
	for _, f := range zr.File {
		names[f.Name] = true
	}
	return names
}

// readZipEntry 读取 zip 中指定条目的文本内容。
func readZipEntry(t *testing.T, zipPath, name string) string {
	t.Helper()
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("打开备份包失败：%v", err)
	}
	defer func() { _ = zr.Close() }()
	f, err := findEntry(&zr.Reader, name)
	if err != nil {
		t.Fatalf("备份包中没有 %s：%v", name, err)
	}
	b, err := readEntry(f)
	if err != nil {
		t.Fatalf("读取条目 %s 失败：%v", name, err)
	}
	return string(b)
}

// writeTestZip 构造一个只含指定文本条目的 zip，供恢复流程的测试使用。
//
// 参数 zipPath 为输出路径；entries 为「条目名 → 文本内容」。
// 返回写入错误。
func writeTestZip(zipPath string, entries map[string]string) error {
	f, err := os.Create(zipPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	zw := zip.NewWriter(f)
	// 固定条目顺序，避免 map 迭代顺序导致测试结果不稳定。
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		w, err := zw.Create(name)
		if err != nil {
			_ = zw.Close()
			return err
		}
		if _, err := w.Write([]byte(entries[name])); err != nil {
			_ = zw.Close()
			return err
		}
	}
	return zw.Close()
}
