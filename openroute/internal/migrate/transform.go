package migrate

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/openroute/openroute/internal/model"
	"github.com/openroute/openroute/internal/util"
)

// ---------------------------------------------------------------------------
// 冲突与风险
// ---------------------------------------------------------------------------

// NameConflict 是一条「归一化后重名」的记录（规格书 4.1 / 7.2 阶段 3）。
//
// MySQL 默认不区分大小写，SQLite 与 PostgreSQL 区分，因此跨库迁移后
// 两个只在大小写上不同的名称会真的变成两条数据（或撞上唯一索引）。
type NameConflict struct {
	// Resource 是资源类型：user | node | node_group | device_group | rule_group | rule
	Resource string `json:"resource"`
	// Left / Right 是互相冲突的两个原始名称
	Left  string `json:"left"`
	Right string `json:"right"`
	// Suggestion 是建议的处理方式（报告里直接展示）
	Suggestion string `json:"suggestion"`
}

// PortConflict 是「同一入口组内多条规则监听同一端口」的记录。
type PortConflict struct {
	InboundGroupID   uint64 `json:"inbound_group_id"`
	InboundGroupName string `json:"inbound_group_name"`
	Port             int    `json:"port"`
	// Rules 是命中的规则名列表
	Rules []string `json:"rules"`
}

// MissingRef 是「规则引用的对象在源库中不存在」的记录。
type MissingRef struct {
	RuleID   uint64 `json:"rule_id"`
	RuleName string `json:"rule_name"`
	// Field 是缺失引用的字段名，如 inbound_group_id / user_id
	Field string `json:"field"`
	// RefID 是被引用但不存在的主体 ID
	RefID   uint64 `json:"ref_id"`
	Message string `json:"message"`
}

// ZeroMultiplierGroup 是倍率为 0 的设备组。
//
// 倍率为 0 会让该组的流量统计恒为 0，属于源库本身就存在的问题，
// 迁移工具不改数值，只在报告中提示。
type ZeroMultiplierGroup struct {
	ID   uint64  `json:"id"`
	Name string  `json:"name"`
	Type string  `json:"type"`
	Rate float64 `json:"rate"`
}

// SkippedRule 是「标记待修复、未写入目标库」的规则。
type SkippedRule struct {
	SourceID uint64 `json:"source_id"`
	Name     string `json:"name"`
	Reason   string `json:"reason"`
}

// ---------------------------------------------------------------------------
// 清单（丢弃 / 填充）
// ---------------------------------------------------------------------------

// DiscardItem 是「已识别并丢弃」的一个字段或整张表。
//
// 规格书 1.3 与 7.2 要求：支付、订单、商品、授权相关的数据必须整体不迁移，
// 且明确记录进丢弃清单 —— 不生成占位代码，也不生成 TODO 注释。
type DiscardItem struct {
	// Source 是源端定位，如 users.license_key、orders.*
	Source string `json:"source"`
	// Reason 是丢弃原因
	Reason string `json:"reason"`
	// Count 是命中的行数（整表丢弃时为表行数），未知时省略
	Count int64 `json:"count,omitempty"`
}

// FillItem 是「目标存在但源端缺失、使用默认值填充」的一个字段。
type FillItem struct {
	// Target 是目标端定位，如 users.token
	Target string `json:"target"`
	// Default 是写入的默认值（字符串描述）
	Default string `json:"default"`
	Reason  string `json:"reason"`
}

// ---------------------------------------------------------------------------
// 报告
// ---------------------------------------------------------------------------

// ReportTableItem 是报告「将迁移的数据」中的一行。
type ReportTableItem struct {
	Label        string `json:"label"`
	Count        int64  `json:"count"`
	Target       string `json:"target"`
	Extra        string `json:"extra,omitempty"`
	SourceTable  string `json:"source_table,omitempty"`
	TargetTable  string `json:"target_table,omitempty"`
	FieldMapping string `json:"field_mapping,omitempty"`
}

// ExampleConversion 是报告中的一条「before → after」示例转换。
type ExampleConversion struct {
	Title  string `json:"title"`
	Before string `json:"before"`
	After  string `json:"after"`
	Note   string `json:"note,omitempty"`
}

// OutlineRow 是「转移工作表」的一行。
//
// 这张表直接对应规格书 7.2 的字段映射表，既用于终端报告，
// 也用于 API 与 WebUI 展示「本次迁移在做什么」。
type OutlineRow struct {
	SourceTable  string `json:"source_table"`
	TargetTable  string `json:"target_table"`
	KeyMapping   string `json:"key_mapping"`
	SourceRows   int64  `json:"source_rows"`
	MigratedRows int64  `json:"migrated_rows,omitempty"`
	Note         string `json:"note,omitempty"`
}

// Report 是完整的迁移预检 / 迁移报告。
//
// 同一个结构承担两种渲染：纯文本（终端）与 JSON（API / WebUI），
// 避免两条渲染路径产生不一致的内容。
type Report struct {
	// 报告头
	Source           string `json:"source"`
	SourceVersion    string `json:"source_version"`
	Target           string `json:"target"`
	DryRun           bool   `json:"dry_run"`
	Stage            int    `json:"stage"`
	SourceTableCount int    `json:"source_table_count"`

	// 将迁移的数据
	Tables []ReportTableItem `json:"tables"`
	// 逐表行数（源库概况）
	SourceRowCounts map[string]int64 `json:"source_row_counts,omitempty"`
	// 数据转移工作表（规格书 7.2 字段映射表）
	Outline []OutlineRow `json:"outline,omitempty"`

	// 冲突与风险
	NameConflicts   []NameConflict        `json:"name_conflicts"`
	PortConflicts   []PortConflict        `json:"port_conflicts"`
	MissingRefs     []MissingRef          `json:"missing_refs"`
	ZeroMultipliers []ZeroMultiplierGroup `json:"zero_multipliers"`

	// 清单
	Discards []DiscardItem `json:"discards"`
	Fills    []FillItem    `json:"fills"`
	// Skipped 是「跳过清单」：引用缺失等原因未写入的规则
	Skipped []SkippedRule `json:"skipped"`
	// DiscardTables 是整体不迁移的表名（支付 / 订单 / 授权等）
	DiscardTables []string `json:"discard_tables"`

	// 示例转换（dry-run 必出三条）
	Examples []ExampleConversion `json:"examples,omitempty"`

	// 建议操作与迁移后必做事项
	Suggestions []string `json:"suggestions"`
	// Notes 是迁移后 MUST 提示用户的事项（节点重装、规则重下发等）
	Notes []string `json:"notes"`

	// 校验结果（阶段 6）
	Verify *VerifyReport `json:"verify,omitempty"`

	// 致命错误：阶段 1 的「源库过旧」「源库与目标库相同」等
	Error string `json:"error,omitempty"`

	// 统计
	TotalRows    int64 `json:"total_rows"`
	MigratedRows int64 `json:"migrated_rows"`

	// RowsToWrite 是「将写入的行数（按表）」，键为表名
	RowsToWrite map[string]int64 `json:"rows_to_write,omitempty"`

	// EstimatedBytes 是目标数据量的预估字节数
	EstimatedBytes int64 `json:"estimated_bytes,omitempty"`

	// sourceDSN 保存用户原始传入的 DSN，只用于拼装「建议执行」的命令行，
	// 不进入 JSON 输出，避免把带密码的连接串写进报告与数据库。
	sourceDSN string
}

// NewReport 构造一份带来源信息的空报告。
func NewReport(source, version, target string, dryRun bool) *Report {
	return &Report{
		Source:        source,
		SourceVersion: version,
		Target:        target,
		DryRun:        dryRun,
		RowsToWrite:   map[string]int64{},
		DiscardTables: []string{},
		Skipped:       []SkippedRule{},
		Suggestions:   []string{},
		Notes:         []string{},
	}
}

// VerifyReport 是阶段 6「校验」的结果。
type VerifyReport struct {
	// RowCounts 是逐表行数对比
	RowCounts []RowCountCompare `json:"row_counts"`
	// Sampled 是抽样字段比对的条数
	Sampled int `json:"sampled"`
	// SampleMismatches 是抽样比对中不一致的条数
	SampleMismatches int `json:"sample_mismatches"`
	// SampleDetails 是不一致样例（最多 10 条）
	SampleDetails []string `json:"sample_details,omitempty"`
	// RefIntegrityFailures 是引用完整性检查失败项
	RefIntegrityFailures []string `json:"ref_integrity_failures"`
	// TrafficSourceBytes / TrafficTargetBytes 是流量总量对比
	TrafficSourceBytes int64 `json:"traffic_source_bytes"`
	TrafficTargetBytes int64 `json:"traffic_target_bytes"`
	// TrafficDiffPercent 是差异百分比
	TrafficDiffPercent float64 `json:"traffic_diff_percent"`
	// TrafficTolerance 是允许的误差百分比（0.1）
	TrafficTolerance float64 `json:"traffic_tolerance"`
	// Passed 表示整体校验是否通过
	Passed bool `json:"passed"`
	// Messages 是补充说明
	Messages []string `json:"messages"`
}

// RowCountCompare 是单表行数对比。
type RowCountCompare struct {
	Table  string `json:"table"`
	Source int64  `json:"source"`
	Target int64  `json:"target"`
	Passed bool   `json:"passed"`
	// Note 说明差异原因（如「计划跳过 3 条」「流量按天聚合」）
	Note string `json:"note,omitempty"`
}

// TrafficTolerancePercent 是流量总量对比允许的误差（规格书 7.2 阶段 6）。
const TrafficTolerancePercent = 0.1

// ---------------------------------------------------------------------------
// 重命名策略
// ---------------------------------------------------------------------------

// RenamePolicy 是名称冲突的处理策略（规格书 7.4）。
type RenamePolicy string

const (
	// RenameFail 是默认策略：检测到冲突直接报错，不做任何写入。
	RenameFail RenamePolicy = "fail"
	// RenameSuffix 自动加 `_2` 后缀。
	RenameSuffix RenamePolicy = "suffix"
	// RenameSkip 跳过冲突的记录。
	RenameSkip RenamePolicy = "skip"
)

// ParseRenamePolicy 解析命令行传入的策略字符串。
func ParseRenamePolicy(s string) (RenamePolicy, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", string(RenameFail):
		return RenameFail, nil
	case string(RenameSuffix):
		return RenameSuffix, nil
	case string(RenameSkip):
		return RenameSkip, nil
	default:
		return RenameFail, fmt.Errorf("不支持的 --rename-policy 取值 %q，可用：fail、suffix、skip", s)
	}
}

// resolveName 按策略处理重名。
//
// 返回值：最终采用的名字、是否保留（false 表示按 skip 策略跳过）、错误。
// used 是「归一化名 → 原始名」的已占用集合，会被就地更新。
func resolveName(policy RenamePolicy, kind, name string, used map[string]string) (string, bool, error) {
	key := strings.ToLower(strings.TrimSpace(name))
	if _, ok := used[key]; !ok {
		used[key] = name
		return name, true, nil
	}

	switch policy {
	case RenameSuffix:
		for i := 2; i < 1000; i++ {
			candidate := fmt.Sprintf("%s_%d", name, i)
			ck := strings.ToLower(candidate)
			if _, ok := used[ck]; !ok {
				used[ck] = candidate
				return candidate, true, nil
			}
		}
		return "", false, fmt.Errorf("%s %q 重名，自动加后缀失败（已尝试到 _999）", kind, name)

	case RenameSkip:
		return "", false, nil

	default: // RenameFail
		return "", false, fmt.Errorf("检测到名称冲突：%s %q 与 %q 在目标库归一化后重名；"+
			"请在源库改名，或使用 --rename-policy=suffix 自动加后缀、--rename-policy=skip 跳过该条",
			kind, name, used[key])
	}
}

// ---------------------------------------------------------------------------
// 目标侧 ID 分配
// ---------------------------------------------------------------------------

// idAllocator 为目标表分配主键。
//
// 迁移尽量保持源 ID 不变（这样 migrate_id_map 直观、人工排查容易），
// 但目标库可能已有数据，因此冲突时向后寻找空位。
type idAllocator struct {
	used   map[uint64]bool
	maxID  uint64
	nextID uint64
}

// newIDAllocator 构造 ID 分配器。
func newIDAllocator() *idAllocator {
	return &idAllocator{used: map[uint64]bool{}, nextID: 1}
}

// reserve 显式占用一个 ID。
func (a *idAllocator) reserve(id uint64) {
	if id == 0 {
		return
	}
	a.used[id] = true
	if id > a.maxID {
		a.maxID = id
	}
	if a.nextID <= id {
		a.nextID = id + 1
	}
}

// take 返回可用的 ID：preferred 未被占用时直接用它。
func (a *idAllocator) take(preferred uint64) uint64 {
	if preferred > 0 && !a.used[preferred] {
		a.reserve(preferred)
		return preferred
	}
	for a.used[a.nextID] {
		a.nextID++
	}
	id := a.nextID
	a.reserve(id)
	return id
}

// ---------------------------------------------------------------------------
// 计划（内存中的全量转换结果）
// ---------------------------------------------------------------------------

// TrafficKey 是流量聚合的唯一键（规格书 4.2.8）。
type TrafficKey struct {
	Date      string
	Hour      int
	UserID    uint64
	RuleID    uint64
	NodeID    uint64
	Direction string
}

// TrafficRow 是聚合后的流量行。
type TrafficRow struct {
	Date      string
	Hour      int
	UserID    uint64
	RuleID    uint64
	NodeID    uint64
	Direction string
	RawBytes  int64
	Bytes     int64
}

// Key 返回该行的聚合键。
func (t *TrafficRow) Key() TrafficKey {
	return TrafficKey{
		Date: t.Date, Hour: t.Hour, UserID: t.UserID,
		RuleID: t.RuleID, NodeID: t.NodeID, Direction: t.Direction,
	}
}

// plan 保存一次迁移的全部转换结果与 ID 映射。
//
// 阶段 4（dry-run）与阶段 5（正式迁移）共用同一个 plan 结构：
// dry-run 只不落库，因此「预览的内容」与「真正写入的内容」天然一致。
type plan struct {
	users        []model.User
	userGroups   []model.UserGroup
	nodes        []model.Node
	nodeGroups   []model.NodeGroup
	deviceGroups []model.DeviceGroup
	ruleGroups   []model.RuleGroup
	rules        []model.ForwardRule
	traffic      []*TrafficRow

	// 源 ID → 目标 ID
	userIDMap        map[uint64]uint64
	userGroupIDMap   map[uint64]uint64
	nodeIDMap        map[uint64]uint64
	nodeGroupIDMap   map[uint64]uint64
	deviceGroupIDMap map[uint64]uint64
	ruleGroupIDMap   map[uint64]uint64
	ruleIDMap        map[uint64]uint64

	// 填充与丢弃清单（阶段 2 产出）
	discards []DiscardItem
	fills    []FillItem

	// 冲突（阶段 3 产出）
	nameConflicts   []NameConflict
	portConflicts   []PortConflict
	missingRefs     []MissingRef
	zeroMultipliers []ZeroMultiplierGroup
	skipped         []SkippedRule
	discardTables   []string

	// 三类资源的「归一化名占用表」，用于跨条目的重名检测
	usedUserNames  map[string]string
	usedNodeNames  map[string]string
	usedGroupNames map[string]string
	usedRuleNames  map[string]string

	// 源库流量总字节数（用于阶段 6 的总量对比）
	trafficSourceBytes int64

	// passwordResetUsers 记录因哈希算法不兼容而需重置密码的用户名
	passwordResetUsers []string
	// nodeTokensRegenerated 记录重新生成 token 的节点数
	nodeTokensRegenerated int
}

// newPlan 构造空计划。
func newPlan() *plan {
	return &plan{
		userIDMap:        map[uint64]uint64{},
		userGroupIDMap:   map[uint64]uint64{},
		nodeIDMap:        map[uint64]uint64{},
		nodeGroupIDMap:   map[uint64]uint64{},
		deviceGroupIDMap: map[uint64]uint64{},
		ruleGroupIDMap:   map[uint64]uint64{},
		ruleIDMap:        map[uint64]uint64{},
		usedUserNames:    map[string]string{},
		usedNodeNames:    map[string]string{},
		usedGroupNames:   map[string]string{},
		usedRuleNames:    map[string]string{},
	}
}

// totalRows 返回计划写入的总行数。
func (p *plan) totalRows() int64 {
	var n int64
	n += int64(len(p.users))
	n += int64(len(p.userGroups))
	n += int64(len(p.nodes))
	n += int64(len(p.nodeGroups))
	n += int64(len(p.deviceGroups))
	n += int64(len(p.ruleGroups))
	n += int64(len(p.rules))
	n += int64(len(p.traffic))
	return n
}

// rowsToWrite 返回「目标表 → 行数」。
func (p *plan) rowsToWrite() map[string]int64 {
	return map[string]int64{
		"users":         int64(len(p.users)),
		"user_groups":   int64(len(p.userGroups)),
		"nodes":         int64(len(p.nodes)),
		"node_groups":   int64(len(p.nodeGroups)),
		"device_groups": int64(len(p.deviceGroups)),
		"rule_groups":   int64(len(p.ruleGroups)),
		"forward_rules": int64(len(p.rules)),
		"traffic_logs":  int64(len(p.traffic)),
	}
}

// estimateBytes 预估目标数据量（用于磁盘空间检查，规格书 7.4）。
//
// 不追求精确：按各行结构的实际序列化长度估算，再乘一个安全系数，
// 目的是在「明显放不下」时提前拒绝，而不是精确到字节。
func (p *plan) estimateBytes() int64 {
	const safetyFactor = 1.25

	var total int64
	bump := func(v interface{}) {
		if b, err := json.Marshal(v); err == nil {
			total += int64(len(b))
		}
	}
	for i := range p.users {
		bump(p.users[i])
	}
	for i := range p.userGroups {
		bump(p.userGroups[i])
	}
	for i := range p.nodes {
		bump(p.nodes[i])
	}
	for i := range p.nodeGroups {
		bump(p.nodeGroups[i])
	}
	for i := range p.deviceGroups {
		bump(p.deviceGroups[i])
	}
	for i := range p.ruleGroups {
		bump(p.ruleGroups[i])
	}
	for i := range p.rules {
		bump(p.rules[i])
	}
	for _, t := range p.traffic {
		bump(t)
	}
	return int64(float64(total) * safetyFactor)
}

// ---------------------------------------------------------------------------
// 字段映射工作表（规格书 7.2 字段映射表）
// ---------------------------------------------------------------------------

// buildOutline 构造「数据转移工作表」。
//
// 这张表的每一行都对应规格书 7.2 字段映射表的一行，
// 并且附带该表在源库中的实际行数，便于用户在预检阶段核对。
func buildOutline(probe *ProbeResult, p *plan) []OutlineRow {
	rc := func(table string) int64 {
		if probe == nil || probe.RowCounts == nil {
			return 0
		}
		return probe.RowCounts[strings.ToLower(table)]
	}

	rows := []OutlineRow{
		{
			SourceTable: "users", TargetTable: "users",
			KeyMapping: "保留用户名、密码哈希（bcrypt 格式一致则直接复用，否则标记「需重置密码」）、分组、流量、限制；丢弃授权与充值字段",
			SourceRows: rc("users"), MigratedRows: int64(len(p.users)),
		},
		{
			SourceTable: "user_groups", TargetTable: "user_groups",
			KeyMapping: "名称、限速策略、规则分组白名单",
			SourceRows: rc("user_groups"), MigratedRows: int64(len(p.userGroups)),
		},
		{
			SourceTable: "nodes", TargetTable: "nodes",
			KeyMapping: "名称、token（MUST 重新生成，避免与旧面板冲突）、角色、IP、端口、权重、分组、备注",
			SourceRows: rc("nodes"), MigratedRows: int64(len(p.nodes)),
			Note: "token 已全部重新生成",
		},
		{
			SourceTable: "node_groups", TargetTable: "node_groups",
			KeyMapping: "名称",
			SourceRows: rc("node_groups"), MigratedRows: int64(len(p.nodeGroups)),
		},
		{
			SourceTable: "device_groups（入口）", TargetTable: "device_groups (type=inbound)",
			KeyMapping: "Config JSON 原样迁移，字段结构完全一致",
			SourceRows: inboundCount(p), MigratedRows: inboundCount(p),
		},
		{
			SourceTable: "device_groups（出口）", TargetTable: "device_groups (type=outbound)",
			KeyMapping: "Config JSON 原样迁移；Balance 默认 least_conn",
			SourceRows: outboundCount(p), MigratedRows: outboundCount(p),
		},
		{
			SourceTable: "rule_groups", TargetTable: "rule_groups",
			KeyMapping: "名称、排序",
			SourceRows: rc("rule_groups"), MigratedRows: int64(len(p.ruleGroups)),
		},
		{
			SourceTable: "forward_rules", TargetTable: "forward_rules",
			KeyMapping: "名称、端口、目标（多目标展开为 Targets 数组）、倍率、限制、归属",
			SourceRows: rc("rules"), MigratedRows: int64(len(p.rules)),
			Note: fmt.Sprintf("跳过 %d 条（引用缺失，已标记待修复）", len(p.skipped)),
		},
		{
			SourceTable: "流量记录", TargetTable: "traffic_logs",
			KeyMapping: "按 (date, hour, user_id, rule_id, node_id, direction) 聚合后写入；date 按 UTC 归属",
			SourceRows: rc("traffic_logs"), MigratedRows: int64(len(p.traffic)),
		},
	}

	// 整体不迁移的表也进工作表，并明确标注「不迁移」。
	for _, t := range p.discardTables {
		rows = append(rows, OutlineRow{
			SourceTable: t, TargetTable: "—",
			KeyMapping: "不迁移（支付 / 订单 / 授权相关）",
			SourceRows: rc(t),
			Note:       "记录到丢弃清单",
		})
	}
	return rows
}

// inboundCount 统计入口设备组数量。
func inboundCount(p *plan) int64 {
	var n int64
	for i := range p.deviceGroups {
		if p.deviceGroups[i].Type == model.GroupTypeInbound {
			n++
		}
	}
	return n
}

// outboundCount 统计出口设备组数量。
func outboundCount(p *plan) int64 {
	var n int64
	for i := range p.deviceGroups {
		if p.deviceGroups[i].Type == model.GroupTypeOutbound {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// 字段映射表（源字段 → 目标字段）
// ---------------------------------------------------------------------------

// FieldMap 是一条「源字段 → 目标字段」的映射说明。
type FieldMap struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Note   string `json:"note,omitempty"`
}

// NyanpassFieldMapping 返回规格书 7.2 字段映射表的机器可读形式。
//
// 供 API `POST /api/v1/migrations/precheck` 与 WebUI 的「字段映射」面板使用，
// 让用户在真正迁移前就能看清每一个字段的去向。
func NyanpassFieldMapping() map[string][]FieldMap {
	return map[string][]FieldMap{
		"users": {
			{Source: "id", Target: "id"},
			{Source: "username", Target: "username"},
			{Source: "password", Target: "password_hash", Note: "bcrypt 直接复用，否则标记需重置密码"},
			{Source: "nickname", Target: "nickname"},
			{Source: "role", Target: "role", Note: "收敛为 admin / user"},
			{Source: "status", Target: "status"},
			{Source: "group_id", Target: "group_id"},
			{Source: "traffic_used", Target: "traffic_used"},
			{Source: "traffic_limit", Target: "traffic_limit"},
			{Source: "speed_limit", Target: "speed_limit", Note: "KB/s 单位自动折算为 byte/s"},
			{Source: "ip_limit", Target: "ip_limit"},
			{Source: "device_limit", Target: "device_limit"},
			{Source: "conn_limit", Target: "conn_limit"},
			{Source: "expire_at", Target: "expire_at"},
			{Source: "license_key / recharge_total / balance / invite_code", Target: "—", Note: "丢弃"},
		},
		"user_groups": {
			{Source: "id", Target: "id"},
			{Source: "name", Target: "name"},
			{Source: "traffic_limit", Target: "traffic_limit"},
			{Source: "speed_limit", Target: "speed_limit"},
			{Source: "ip_limit", Target: "ip_limit"},
			{Source: "conn_limit", Target: "conn_limit"},
			{Source: "rule_group_ids", Target: "rule_group_ids", Note: "JSON 数组，同时兼容逗号分隔写法"},
		},
		"nodes": {
			{Source: "id", Target: "id"},
			{Source: "name", Target: "name"},
			{Source: "token", Target: "token", Note: "MUST 重新生成"},
			{Source: "role", Target: "role"},
			{Source: "public_ipv4", Target: "public_ipv4"},
			{Source: "private_ip", Target: "private_ip"},
			{Source: "connect_host", Target: "connect_host"},
			{Source: "direct_port / ws_port / tls_port / udp_port / rev_port", Target: "同名端口字段"},
			{Source: "weight", Target: "weight"},
			{Source: "node_list", Target: "group_ids", Note: "单值 group_id 也兼容"},
			{Source: "remark", Target: "remark"},
		},
		"device_groups": {
			{Source: "id", Target: "id"},
			{Source: "name", Target: "name"},
			{Source: "type / inbound", Target: "type", Note: "inbound | outbound"},
			{Source: "config", Target: "config", Note: "JSON 原样迁移，结构完全一致"},
			{Source: "node_list", Target: "node_ids"},
			{Source: "multiplier", Target: "forward_rules.inbound_multiplier", Note: "倍率下沉到规则级"},
			{Source: "—", Target: "balance", Note: "出口组默认 least_conn"},
		},
		"rule_groups": {
			{Source: "id", Target: "id"},
			{Source: "name", Target: "name"},
			{Source: "sort", Target: "sort"},
		},
		"forward_rules": {
			{Source: "id", Target: "id"},
			{Source: "name", Target: "name"},
			{Source: "user_id", Target: "user_id", Note: "经 migrate_id_map 重映射"},
			{Source: "rule_group_id", Target: "rule_group_id"},
			{Source: "inbound_group_id", Target: "inbound_group_id", Note: "经 migrate_id_map 重映射"},
			{Source: "ssh_port / listen_port", Target: "listen_port"},
			{Source: "target_host + target_port", Target: "targets", Note: "多目标展开为 Targets 数组"},
			{Source: "balance", Target: "target_balance"},
			{Source: "outbound_group_id", Target: "outbound_group_id"},
			{Source: "chain_groups", Target: "chain_groups"},
			{Source: "reverse_enable / reverse_port / reverse_group_id", Target: "同名反向隧道字段"},
			{Source: "is_sub_rule / parent_id / sni", Target: "同名分流字段"},
			{Source: "shaping", Target: "shaping"},
			{Source: "options / tls", Target: "options"},
			{Source: "enable", Target: "enable"},
			{Source: "—", Target: "sync_status", Note: "统一置为 unsynced，等节点上线后自动下发"},
		},
		"traffic_logs": {
			{Source: "date", Target: "date", Note: "UTC 日期，不做时区重算"},
			{Source: "hour", Target: "hour", Note: "越界值归一到 -1（按天）"},
			{Source: "user_id / rule_id / node_id", Target: "同名维度字段", Note: "经 migrate_id_map 重映射"},
			{Source: "direction", Target: "direction", Note: "in | out"},
			{Source: "bytes", Target: "raw_bytes / bytes"},
		},
	}
}

// ---------------------------------------------------------------------------
// 字段映射辅助
// ---------------------------------------------------------------------------

// jsonFieldOrNull 把 JSON 字段统一为空值形式。
//
// 目标模型用 TEXT/JSONB/JSON 存 JSON，空数组与空对象在语义上比 "null" 更友好
// （前端拿到的直接是 []，不需要额外判空），因此这里按字段类型给默认值。
func jsonArrayOrNull(s string) model.JSON {
	if strings.TrimSpace(s) == "" || s == "null" {
		return model.JSON("[]")
	}
	if !util.IsValidJSON(s) {
		return model.JSON("[]")
	}
	return model.JSON(s)
}

// jsonObjectOrNull 把 JSON 字段规范化为对象。
func jsonObjectOrNull(s string) model.JSON {
	if strings.TrimSpace(s) == "" || s == "null" {
		return model.JSON("{}")
	}
	if !util.IsValidJSON(s) {
		return model.JSON("{}")
	}
	return model.JSON(s)
}

// checksumOf 计算一行内容的校验和，用于幂等判断（规格书 7.5）。
func checksumOf(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return util.SHA256HexBytes(b)
}

// sortedKeys 返回 map 的排序键，保证报告输出稳定。
func sortedKeys(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sortSlice 按比较函数对切片排序。
//
// 单独包一层是为了让「用标准库排序」这件事在本包里只有一个入口，
// 避免各调用点自己实现排序算法。
func sortSlice[T any](s []T, less func(a, b T) bool) {
	sort.Slice(s, func(i, j int) bool { return less(s[i], s[j]) })
}

// nowUTC 返回当前 UTC 时间。
func nowUTC() time.Time { return time.Now().UTC() }
