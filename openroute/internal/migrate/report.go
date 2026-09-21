package migrate

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/openroute/openroute/internal/util"
)

// 报告使用的框线字符（规格书 7.2 的样例格式）。
//
// 用 rune 常量而不是字面量，是为了让宽度计算与渲染共用同一个来源，
// 后续调整框线样式时只需要改这里。
const (
	reportHeavyLine = "═"
	reportThinLine  = "─"
	// reportWidth 是框线的字符数，与规格书样例一致。
	reportWidth = 55
)

// heavyRule 返回一条粗框线。
func heavyRule() string { return strings.Repeat(reportHeavyLine, reportWidth) }

// thinRule 返回一条细框线。
func thinRule() string { return strings.Repeat(reportThinLine, reportWidth) }

// Render 把报告渲染为规格书 7.2 要求的纯文本格式。
//
// 输出严格遵循样例的版式：粗框线标题、字段冒号对齐、
// 四个用细框线分隔的章节（将迁移的数据 / 冲突与风险 /
// 不迁移的字段 / 建议操作）。
func (r *Report) Render() string {
	var b strings.Builder

	// ── 标题 ──────────────────────────────────────────────
	b.WriteString(heavyRule() + "\n")
	b.WriteString("  " + r.title() + "\n")
	b.WriteString(heavyRule() + "\n")
	writeField(&b, "源库", r.Source)
	writeField(&b, "源版本", r.SourceVersion)
	writeField(&b, "目标库", r.Target)

	if r.DryRun {
		writeField(&b, "迁移模式", "DRY-RUN（不会写入任何数据）")
	} else {
		writeField(&b, "迁移模式", "正式迁移（会写入目标库）")
	}
	if r.Stage > 0 {
		writeField(&b, "当前阶段", fmt.Sprintf("第 %d 阶段 / 共 7 阶段", r.Stage))
	}
	if r.SourceTableCount > 0 {
		writeField(&b, "源库概况", fmt.Sprintf("%d 张表", r.SourceTableCount))
	}

	// ── 将迁移的数据 ──────────────────────────────────────
	b.WriteString(thinRule() + "\n")
	b.WriteString("【将迁移的数据】\n")
	if len(r.Tables) == 0 {
		b.WriteString("  （无）\n")
	}
	for _, t := range r.Tables {
		line := fmt.Sprintf("  %s%d 条   →  %s", padLabel(t.Label), t.Count, t.Target)
		if t.Extra != "" {
			line += "（" + t.Extra + "）"
		}
		b.WriteString(line + "\n")
	}

	// ── 冲突与风险 ────────────────────────────────────────
	b.WriteString(thinRule() + "\n")
	b.WriteString("【冲突与风险】\n")
	r.writeConflicts(&b)

	// ── 不迁移的字段 ──────────────────────────────────────
	b.WriteString(thinRule() + "\n")
	b.WriteString("【不迁移的字段（已识别并丢弃）】\n")
	r.writeDiscards(&b)

	// ── 跳过清单（引用缺失等原因未写入） ──────────────────
	if len(r.Skipped) > 0 {
		b.WriteString(thinRule() + "\n")
		b.WriteString("【跳过清单（标记待修复，未写入）】\n")
		for _, s := range r.Skipped {
			b.WriteString(fmt.Sprintf("  · 规则 %q（源 ID %d）：%s\n", s.Name, s.SourceID, s.Reason))
		}
	}

	// ── 示例转换 ──────────────────────────────────────────
	if len(r.Examples) > 0 {
		b.WriteString(thinRule() + "\n")
		b.WriteString("【示例转换（before → after）】\n")
		for i, ex := range r.Examples {
			b.WriteString(fmt.Sprintf("  %d) %s\n", i+1, ex.Title))
			b.WriteString("     before : " + ex.Before + "\n")
			b.WriteString("     after  : " + ex.After + "\n")
			if ex.Note != "" {
				b.WriteString("     说明   : " + ex.Note + "\n")
			}
		}
	}

	// ── 建议操作 ──────────────────────────────────────────
	b.WriteString(thinRule() + "\n")
	b.WriteString("【建议操作】\n")
	for i, s := range r.Suggestions {
		b.WriteString(fmt.Sprintf("  %d. %s\n", i+1, s))
	}

	// ── 迁移后必做事项 ────────────────────────────────────
	if len(r.Notes) > 0 {
		b.WriteString(thinRule() + "\n")
		b.WriteString("【迁移后必做】\n")
		for i, n := range r.Notes {
			b.WriteString(fmt.Sprintf("  %d. %s\n", i+1, n))
		}
	}

	// ── 校验结果 ──────────────────────────────────────────
	if r.Verify != nil {
		b.WriteString(thinRule() + "\n")
		b.WriteString("【校验结果】\n")
		r.writeVerify(&b)
	}

	// ── 致命错误 ──────────────────────────────────────────
	if r.Error != "" {
		b.WriteString(thinRule() + "\n")
		b.WriteString("【错误】\n")
		for _, line := range strings.Split(r.Error, "\n") {
			b.WriteString("  " + line + "\n")
		}
	}

	b.WriteString(heavyRule() + "\n")
	return b.String()
}

// title 返回报告标题。
//
// 预检与正式迁移的标题不同，便于用户在终端日志里一眼分辨两份输出。
func (r *Report) title() string {
	switch {
	case r.Error != "":
		return fmt.Sprintf("%s → OpenRoute 迁移报告（未完成）", panelName(r.Source))
	case r.Verify != nil:
		return fmt.Sprintf("%s → OpenRoute 迁移报告（已校验）", panelName(r.Source))
	default:
		return fmt.Sprintf("%s → OpenRoute 迁移预检报告", panelName(r.Source))
	}
}

// panelName 把源标识转成展示用名称。
func panelName(source string) string {
	if strings.EqualFold(source, "nyanpass") {
		return "Nyanpass"
	}
	if source == "" {
		return "未知源"
	}
	return source
}

// writeField 输出「键 : 值」，键宽固定 10 字节以对齐冒号。
func writeField(b *strings.Builder, key, value string) {
	const keyWidth = 10
	pad := keyWidth - len([]rune(key))
	if pad < 0 {
		pad = 0
	}
	b.WriteString(key + strings.Repeat(" ", pad) + ": " + value + "\n")
}

// padLabel 把标签补齐到固定显示宽度，让「条数」列对齐。
func padLabel(label string) string {
	const width = 14
	n := 0
	for _, ch := range label {
		if ch > 0x7F {
			// 中日韩全宽字符按两个显示列计算。
			n += 2
		} else {
			n++
		}
	}
	if n >= width {
		return label + " "
	}
	return label + strings.Repeat(" ", width-n)
}

// writeConflicts 输出「冲突与风险」章节。
func (r *Report) writeConflicts(b *strings.Builder) {
	// 名称冲突：按资源类型分组展示，每条给出建议改法。
	if len(r.NameConflicts) > 0 {
		fmt.Fprintf(b, "  ⚠ 名称冲突 %d 处（MySQL/PG 大小写敏感）\n", len(r.NameConflicts))
		for _, c := range r.NameConflicts {
			fmt.Fprintf(b, "      · %s \"%s\" 与 \"%s\"%s\n",
				resourceLabel(c.Resource), c.Left, c.Right, c.Suggestion)
		}
	} else {
		b.WriteString("  ⚠ 名称冲突 0 处\n")
	}

	// 端口冲突
	if len(r.PortConflicts) > 0 {
		fmt.Fprintf(b, "  ⚠ 端口冲突 %d 处\n", len(r.PortConflicts))
		for _, c := range r.PortConflicts {
			fmt.Fprintf(b, "      · 规则 %s 在入口组 \"%s\" 上均监听 %d\n",
				quoteJoin(c.Rules), c.InboundGroupName, c.Port)
		}
	} else {
		b.WriteString("  ⚠ 端口冲突 0 处\n")
	}

	// 引用缺失
	if len(r.MissingRefs) > 0 {
		fmt.Fprintf(b, "  ⚠ 引用缺失 %d 处\n", len(r.MissingRefs))
		for _, m := range r.MissingRefs {
			fmt.Fprintf(b, "      · 规则 %q 引用的 %s（ID %d）在源库中不存在\n",
				m.RuleName, refFieldLabel(m.Field), m.RefID)
		}
	} else {
		b.WriteString("  ⚠ 引用缺失 0 处\n")
	}

	// 倍率为 0 的设备组
	if len(r.ZeroMultipliers) > 0 {
		fmt.Fprintf(b, "  ⚠ 倍率为 0 的设备组 %d 个 → 统计将显示为 0（源库即为如此）\n",
			len(r.ZeroMultipliers))
		for _, g := range r.ZeroMultipliers {
			fmt.Fprintf(b, "      · %s \"%s\" 倍率 %.2f\n", groupTypeLabel(g.Type), g.Name, g.Rate)
		}
	}
}

// writeDiscards 输出「不迁移的字段」章节。
//
// 规格书样例把这些字段横向罗列并用中文顿号/逗号分隔，这里保持一致；
// 数量较多时自动换行，避免单行过长。
func (r *Report) writeDiscards(b *strings.Builder) {
	if len(r.Discards) == 0 {
		b.WriteString("  （无）\n")
	} else {
		parts := make([]string, 0, len(r.Discards))
		for _, d := range r.Discards {
			parts = append(parts, d.Source)
		}
		for _, line := range wrapList(parts, 66) {
			b.WriteString("  " + line + "\n")
		}
	}

	// 填充清单紧随其后，说明「目标有、源端没有」的字段是怎么处理的。
	if len(r.Fills) > 0 {
		fmt.Fprintf(b, "  ⓘ 使用默认值填充的字段 %d 项：\n", len(r.Fills))
		for _, f := range r.Fills {
			fmt.Fprintf(b, "      · %s = %s（%s）\n", f.Target, f.Default, f.Reason)
		}
	}
}

// writeVerify 输出校验结果。
func (r *Report) writeVerify(b *strings.Builder) {
	v := r.Verify

	// 行数对比
	b.WriteString("  · 行数对比\n")
	for _, c := range v.RowCounts {
		mark := "✔"
		if !c.Passed {
			mark = "✘"
		}
		line := fmt.Sprintf("      %s %-16s 源 %d / 目标 %d", mark, c.Table, c.Source, c.Target)
		if c.Note != "" {
			line += "（" + c.Note + "）"
		}
		b.WriteString(line + "\n")
	}

	// 抽样字段比对
	if v.Sampled > 0 {
		mark := "✔"
		if v.SampleMismatches > 0 {
			mark = "✘"
		}
		fmt.Fprintf(b, "  · 抽样字段比对 %s 抽样 %d 条，不一致 %d 条\n",
			mark, v.Sampled, v.SampleMismatches)
		for _, d := range v.SampleDetails {
			b.WriteString("      · " + d + "\n")
		}
	}

	// 引用完整性
	if len(v.RefIntegrityFailures) == 0 {
		b.WriteString("  · 引用完整性 ✔ 每条规则的入口组 / 出口组 / 用户均在目标库存在\n")
	} else {
		fmt.Fprintf(b, "  · 引用完整性 ✘ 失败 %d 处\n", len(v.RefIntegrityFailures))
		for _, d := range v.RefIntegrityFailures {
			b.WriteString("      · " + d + "\n")
		}
	}

	// 流量总量
	mark := "✔"
	if v.TrafficDiffPercent > v.TrafficTolerance {
		mark = "✘"
	}
	fmt.Fprintf(b, "  · 流量总量 %s 源 %s / 目标 %s，差异 %.4f%%（允许 ±%.1f%%）\n",
		mark, util.FormatBytes(v.TrafficSourceBytes), util.FormatBytes(v.TrafficTargetBytes),
		v.TrafficDiffPercent, v.TrafficTolerance)

	for _, m := range v.Messages {
		b.WriteString("  · " + m + "\n")
	}

	if v.Passed {
		b.WriteString("  校验结论：✔ 全部通过\n")
	} else {
		b.WriteString("  校验结论：✘ 存在未通过项，请查看上方明细\n")
	}
}

// wrapList 把字符串列表按最大显示宽度折行。
//
// 返回值是若干行，行内以「、」分隔 —— 与规格书样例的字段罗列写法一致。
func wrapList(items []string, maxWidth int) []string {
	if len(items) == 0 {
		return []string{""}
	}
	lines := []string{}
	cur := ""
	for i, it := range items {
		piece := it
		if i < len(items)-1 {
			piece += "、"
		}
		next := cur + piece
		if displayWidth(next) > maxWidth && cur != "" {
			// 上一行已满：去掉行尾的分隔符再收尾。
			lines = append(lines, strings.TrimSuffix(cur, "、"))
			cur = piece
			continue
		}
		cur = next
	}
	if cur != "" {
		lines = append(lines, strings.TrimSuffix(cur, "、"))
	}
	return lines
}

// displayWidth 估算字符串的终端显示宽度（全宽字符按 2 列计）。
func displayWidth(s string) int {
	n := 0
	for _, ch := range s {
		if ch > 0x7F {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// quoteJoin 把规则名列表渲染成 "a" 与 "b" 的形式。
func quoteJoin(names []string) string {
	quoted := make([]string, 0, len(names))
	for _, n := range names {
		quoted = append(quoted, fmt.Sprintf("%q", n))
	}
	return strings.Join(quoted, " 与 ")
}

// resourceLabel 把资源类型转成中文标签。
func resourceLabel(kind string) string {
	switch kind {
	case "user":
		return "用户"
	case "node":
		return "节点"
	case "node_group":
		return "节点分组"
	case "device_group":
		return "设备组"
	case "rule_group":
		return "规则分组"
	case "rule":
		return "规则"
	default:
		return kind
	}
}

// refFieldLabel 把引用字段名转成中文。
func refFieldLabel(field string) string {
	switch field {
	case "inbound_group_id":
		return "入口设备组"
	case "outbound_group_id":
		return "出口设备组"
	case "user_id":
		return "用户"
	case "rule_group_id":
		return "规则分组"
	case "reverse_group_id":
		return "反向组"
	case "parent_id":
		return "主规则"
	case "node_id":
		return "节点"
	default:
		return field
	}
}

// groupTypeLabel 把设备组类型转成中文。
func groupTypeLabel(t string) string {
	switch t {
	case "inbound":
		return "入口组"
	case "outbound":
		return "出口组"
	default:
		return t
	}
}

// ---------------------------------------------------------------------------
// JSON 形式（API / WebUI）
// ---------------------------------------------------------------------------

// JSONReport 是报告的 JSON 形式，供 API 与 WebUI 直接消费。
//
// 与纯文本报告同源：字段一一对应，不存在「终端看到的」与
// 「接口返回的」不一致的问题。
type JSONReport struct {
	// 报告头
	Source        string `json:"source"`
	SourceVersion string `json:"source_version"`
	Target        string `json:"target"`
	DryRun        bool   `json:"dry_run"`
	Stage         int    `json:"stage"`
	Status        string `json:"status"`

	// 概览
	Summary     ReportSummary     `json:"summary"`
	Tables      []ReportTableItem `json:"tables"`
	Outline     []OutlineRow      `json:"outline"`
	RowCounts   map[string]int64  `json:"source_row_counts"`
	RowsToWrite map[string]int64  `json:"rows_to_write"`

	// 冲突与风险
	NameConflicts   []NameConflict        `json:"name_conflicts"`
	PortConflicts   []PortConflict        `json:"port_conflicts"`
	MissingRefs     []MissingRef          `json:"missing_refs"`
	ZeroMultipliers []ZeroMultiplierGroup `json:"zero_multipliers"`

	// 清单
	Discards      []DiscardItem `json:"discards"`
	Fills         []FillItem    `json:"fills"`
	DiscardTables []string      `json:"discard_tables"`
	Skipped       []SkippedRule `json:"skipped"`

	// 示例转换
	Examples []ExampleConversion `json:"examples"`

	// 建议与注意事项
	Suggestions []string `json:"suggestions"`
	Notes       []string `json:"notes"`

	// 校验
	Verify *VerifyReport `json:"verify,omitempty"`

	// 预估
	EstimatedBytes int64 `json:"estimated_bytes"`

	// 错误
	Error string `json:"error,omitempty"`

	// Text 是纯文本报告的完整内容，便于前端「复制/下载」时不用自己拼版
	Text string `json:"text"`
}

// ReportSummary 是 JSON 报告的概览块。
type ReportSummary struct {
	TotalRows      int64 `json:"total_rows"`
	MigratedRows   int64 `json:"migrated_rows"`
	ConflictCount  int   `json:"conflict_count"`
	DiscardCount   int   `json:"discard_count"`
	SkippedCount   int   `json:"skipped_count"`
	SourceRowTotal int64 `json:"source_row_total"`
}

// ToJSONReport 把报告转换成 JSON 形式。
//
// 返回值同时包含完整的纯文本报告，前端可以「一处展示、一处复制」。
func (r *Report) ToJSONReport() *JSONReport {
	d := &JSONReport{
		Source:          r.Source,
		SourceVersion:   r.SourceVersion,
		Target:          r.Target,
		DryRun:          r.DryRun,
		Stage:           r.Stage,
		Tables:          nonNilTableItems(r.Tables),
		Outline:         nonNilOutline(r.Outline),
		RowCounts:       nonNilIntMap(r.SourceRowCounts),
		RowsToWrite:     nonNilIntMap(r.RowsToWrite),
		NameConflicts:   nonNilNameConflicts(r.NameConflicts),
		PortConflicts:   nonNilPortConflicts(r.PortConflicts),
		MissingRefs:     nonNilMissingRefs(r.MissingRefs),
		ZeroMultipliers: nonNilZeroMultipliers(r.ZeroMultipliers),
		Discards:        nonNilDiscards(r.Discards),
		Fills:           nonNilFills(r.Fills),
		DiscardTables:   nonNilStrings(r.DiscardTables),
		Skipped:         nonNilSkipped(r.Skipped),
		Examples:        nonNilExamples(r.Examples),
		Suggestions:     nonNilStrings(r.Suggestions),
		Notes:           nonNilStrings(r.Notes),
		Verify:          r.Verify,
		EstimatedBytes:  r.EstimatedBytes,
		Error:           r.Error,
		Text:            r.Render(),
	}

	var sourceTotal int64
	for _, v := range r.SourceRowCounts {
		sourceTotal += v
	}
	d.Summary = ReportSummary{
		TotalRows:    r.TotalRows,
		MigratedRows: r.MigratedRows,
		ConflictCount: len(r.NameConflicts) + len(r.PortConflicts) + len(r.MissingRefs) +
			len(r.ZeroMultipliers),
		DiscardCount:   len(r.Discards),
		SkippedCount:   len(r.Skipped),
		SourceRowTotal: sourceTotal,
	}
	switch {
	case r.Error != "":
		d.Status = "failed"
	case r.Verify != nil && r.Verify.Passed:
		d.Status = "verified"
	case !r.DryRun:
		d.Status = "finished"
	default:
		d.Status = "precheck"
	}
	return d
}

// MarshalJSONReport 把报告序列化为缩进后的 JSON 字节。
func (r *Report) MarshalJSONReport() ([]byte, error) {
	return json.MarshalIndent(r.ToJSONReport(), "", "  ")
}

// 以下 nonNil* 辅助函数保证 JSON 输出里空集合是 [] 而不是 null，
// 前端就不必在每个渲染点判空。

// nonNilStrings 保证字符串切片非 nil。
func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// nonNilIntMap 保证 map 非 nil。
func nonNilIntMap(in map[string]int64) map[string]int64 {
	if in == nil {
		return map[string]int64{}
	}
	return in
}

// nonNilTableItems 保证表格条目切片非 nil。
func nonNilTableItems(in []ReportTableItem) []ReportTableItem {
	if in == nil {
		return []ReportTableItem{}
	}
	return in
}

// nonNilOutline 保证工作表切片非 nil。
func nonNilOutline(in []OutlineRow) []OutlineRow {
	if in == nil {
		return []OutlineRow{}
	}
	return in
}

// nonNilNameConflicts 保证名称冲突切片非 nil。
func nonNilNameConflicts(in []NameConflict) []NameConflict {
	if in == nil {
		return []NameConflict{}
	}
	return in
}

// nonNilPortConflicts 保证端口冲突切片非 nil。
func nonNilPortConflicts(in []PortConflict) []PortConflict {
	if in == nil {
		return []PortConflict{}
	}
	return in
}

// nonNilMissingRefs 保证引用缺失切片非 nil。
func nonNilMissingRefs(in []MissingRef) []MissingRef {
	if in == nil {
		return []MissingRef{}
	}
	return in
}

// nonNilZeroMultipliers 保证零倍率切片非 nil。
func nonNilZeroMultipliers(in []ZeroMultiplierGroup) []ZeroMultiplierGroup {
	if in == nil {
		return []ZeroMultiplierGroup{}
	}
	return in
}

// nonNilDiscards 保证丢弃清单非 nil。
func nonNilDiscards(in []DiscardItem) []DiscardItem {
	if in == nil {
		return []DiscardItem{}
	}
	return in
}

// nonNilFills 保证填充清单非 nil。
func nonNilFills(in []FillItem) []FillItem {
	if in == nil {
		return []FillItem{}
	}
	return in
}

// nonNilSkipped 保证跳过清单非 nil。
func nonNilSkipped(in []SkippedRule) []SkippedRule {
	if in == nil {
		return []SkippedRule{}
	}
	return in
}

// nonNilExamples 保证示例转换切片非 nil。
func nonNilExamples(in []ExampleConversion) []ExampleConversion {
	if in == nil {
		return []ExampleConversion{}
	}
	return in
}

// ---------------------------------------------------------------------------
// 建议与注意事项
// ---------------------------------------------------------------------------

// defaultSuggestions 生成「建议操作」章节的内容。
//
// 规格书 7.2 的样例给了三条固定建议，这里按实际情况补充：
// 存在冲突时追加改名建议，已通过校验时追加后续动作。
func (r *Report) defaultSuggestions() []string {
	out := []string{
		"迁移前先执行 -backup 备份目标库",
		fmt.Sprintf("确认无误后执行：%s", r.migrateCommand()),
	}
	if len(r.Discards) > 0 {
		out = append(out, fmt.Sprintf(
			"以下字段已识别并丢弃，不会写入目标库：%s",
			strings.Join(discardSources(r.Discards), "、")))
	}
	return out
}

// migrateCommand 返回建议用户执行的迁移命令。
func (r *Report) migrateCommand() string {
	dsn := r.sourceDSNForCommand()
	return fmt.Sprintf(`-migrate from=nyanpass,dsn="%s",dry-run=false`, dsn)
}

// sourceDSNForCommand 返回用于展示的命令行 DSN。
//
// 优先使用用户原始传入的 DSN；没有时退回报告中的源库描述。
func (r *Report) sourceDSNForCommand() string {
	if r.sourceDSN != "" {
		return r.sourceDSN
	}
	return r.Source
}

// discardSources 提取丢弃清单中的源端定位。
func discardSources(items []DiscardItem) []string {
	out := make([]string, 0, len(items))
	for _, d := range items {
		out = append(out, d.Source)
	}
	return out
}

// BuildMigrationNotes 生成「迁移后必须做的事」（规格书 7.2）。
//
// 这段文字是硬性要求：用户必须知道节点密钥已变、节点要重装、
// 规则要等节点上线后才会同步。任何一项缺失都可能让用户以为迁移失败了。
func BuildMigrationNotes(ruleCount int, nodeTokensRegenerated int) []string {
	notes := []string{
		fmt.Sprintf("节点密钥全部重新生成：共 %d 个节点的 token 已更换为新的 nsk_ 前缀密钥；"+
			"节点上的 /opt/openroute/config.yml 里的 token 与旧面板不同，需要重装节点才会生效。",
			nodeTokensRegenerated),
		"节点客户端不兼容：Nyanpass 节点与 OpenRoute 面板的通信协议不同，MUST 在每台机器上重新安装 OpenRoute 节点：\n" +
			"       bash /opt/openroute/openroute.uninstall.sh      # 卸载旧节点\n" +
			"       # 然后在面板复制新的安装命令执行\n" +
			"       迁移工具已在节点列表中把这些节点标记为「待重装」。",
		fmt.Sprintf("规则需要重新下发：迁移后 %d 条规则的状态均为 unsynced，"+
			"节点重装并上线后会自动同步。", ruleCount),
		"大小写敏感差异：若源库使用 MySQL 且目标使用 SQLite/PostgreSQL，" +
			"注意名称唯一性由应用层 EqualFold 归一化维护（见上方冲突清单）。",
	}
	return notes
}
