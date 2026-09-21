package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/openroute/openroute/internal/api/response"
	"github.com/openroute/openroute/internal/model"
	"github.com/openroute/openroute/internal/util"
)

// 本文件实现规格书 4.2.14 / 6.13「配置快照与回滚」。
//
// 快照内容为「全量配置」：device_groups + forward_rules + nodes 的关系（含 node_groups）。
// 每次「批量规则变更」「设备组变更」「迁移前」以及每日定时都会生成一份；
// 快照列表页支持查看差异、一键回滚、导出 JSON、删除。
// **回滚前 MUST 再生成一份「回滚前快照」**，保证操作可逆（规格书 6.13 明文要求）。

// 快照原因常量（与 model 中的定义保持一致，单独列一份便于本文件内直接引用）。
const (
	SnapshotReasonManual         = model.SnapshotReasonManual
	SnapshotReasonAutoBeforeSync = model.SnapshotReasonAutoBeforeSync
	SnapshotReasonAutoDaily      = model.SnapshotReasonAutoDaily
	SnapshotReasonBeforeMigrate  = model.SnapshotReasonBeforeMigrate
)

// SnapshotPayload 是快照的 Payload 结构。
//
// 字段都带 json 标签，因此同一个结构既用于落库、也直接用于导出 JSON。
type SnapshotPayload struct {
	// Version 是快照格式版本，升级格式时便于识别旧快照。
	Version int `json:"version"`
	// CreatedAt 为快照生成时间（与 ConfigSnapshot.CreatedAt 一致，冗余一份便于导出文件自解释）。
	CreatedAt string `json:"created_at"`

	DeviceGroups []SnapshotDeviceGroup `json:"device_groups"`
	NodeGroups   []SnapshotNodeGroup   `json:"node_groups"`
	Nodes        []SnapshotNode        `json:"nodes"`
	Rules        []SnapshotRule        `json:"forward_rules"`
	RuleGroups   []SnapshotRuleGroup   `json:"rule_groups"`
	UserGroups   []SnapshotUserGroup   `json:"user_groups"`
}

// SnapshotDeviceGroup 是设备组的快照行。
type SnapshotDeviceGroup struct {
	ID                   uint64     `json:"id"`
	Name                 string     `json:"name"`
	Type                 string     `json:"type"`
	NodeIDs              model.JSON `json:"node_ids"`
	Config               model.JSON `json:"config"`
	Balance              string     `json:"balance"`
	HealthCheckEnable    bool       `json:"health_check_enable"`
	HealthCheckInterval  int        `json:"health_check_interval"`
	HealthCheckTimeout   int        `json:"health_check_timeout"`
	HealthCheckFailCount int        `json:"health_check_fail_count"`
	HealthCheckSuccCount int        `json:"health_check_succ_count"`
	FailoverGroupID      uint64     `json:"failover_group_id"`
	Remark               string     `json:"remark"`
}

// SnapshotNodeGroup 是节点分组的快照行。
type SnapshotNodeGroup struct {
	ID     uint64 `json:"id"`
	Name   string `json:"name"`
	Remark string `json:"remark"`
}

// SnapshotNode 是节点的快照行。
//
// 只保留「配置相关」字段：网络信息、探针指标、健康分等运行时字段不参与回滚，
// 否则回滚会把节点当前的真实状态覆盖成快照时的旧状态。
type SnapshotNode struct {
	ID          uint64     `json:"id"`
	Name        string     `json:"name"`
	Role        string     `json:"role"`
	Token       string     `json:"token"`
	ConnectHost string     `json:"connect_host"`
	IsStatic    bool       `json:"is_static"`
	DirectPort  int        `json:"direct_port"`
	WsPort      int        `json:"ws_port"`
	TlsPort     int        `json:"tls_port"`
	UdpPort     int        `json:"udp_port"`
	RevPort     int        `json:"rev_port"`
	GroupIDs    model.JSON `json:"group_ids"`
	Weight      int        `json:"weight"`
	MaxConn     int        `json:"max_conn"`
	Remark      string     `json:"remark"`
}

// SnapshotRule 是转发规则的快照行。
type SnapshotRule struct {
	ID                 uint64     `json:"id"`
	Name               string     `json:"name"`
	UserID             uint64     `json:"user_id"`
	RuleGroupID        uint64     `json:"rule_group_id"`
	InboundGroupID     uint64     `json:"inbound_group_id"`
	ListenPort         int        `json:"listen_port"`
	ListenPortEnd      int        `json:"listen_port_end"`
	OutboundGroupID    uint64     `json:"outbound_group_id"`
	Targets            model.JSON `json:"targets"`
	TargetBalance      string     `json:"target_balance"`
	InboundMultiplier  float64    `json:"inbound_multiplier"`
	OutboundMultiplier float64    `json:"outbound_multiplier"`
	SpeedLimit         int64      `json:"speed_limit"`
	ConnLimit          int        `json:"conn_limit"`
	IPLimit            int        `json:"ip_limit"`
	Options            model.JSON `json:"options"`
	ChainGroups        model.JSON `json:"chain_groups"`
	ReverseEnable      bool       `json:"reverse_enable"`
	ReversePort        int        `json:"reverse_port"`
	ReverseGroupID     uint64     `json:"reverse_group_id"`
	IsSubRule          bool       `json:"is_sub_rule"`
	ParentID           uint64     `json:"parent_id"`
	SNI                string     `json:"sni"`
	Shaping            model.JSON `json:"shaping"`
	Enable             bool       `json:"enable"`
	Remark             string     `json:"remark"`
}

// SnapshotRuleGroup 是规则分组的快照行。
type SnapshotRuleGroup struct {
	ID     uint64 `json:"id"`
	Name   string `json:"name"`
	Sort   int    `json:"sort"`
	Remark string `json:"remark"`
}

// SnapshotUserGroup 是用户分组的快照行。
//
// 用户分组影响限速与流量策略，属于「配置」的一部分，因此纳入快照。
// 用户本身（账号、密码、已用流量）不属于配置，不纳入。
type SnapshotUserGroup struct {
	ID           uint64     `json:"id"`
	Name         string     `json:"name"`
	TrafficLimit int64      `json:"traffic_limit"`
	SpeedLimit   int64      `json:"speed_limit"`
	IPLimit      int        `json:"ip_limit"`
	ConnLimit    int        `json:"conn_limit"`
	RuleGroupIDs model.JSON `json:"rule_group_ids"`
	Remark       string     `json:"remark"`
}

// SnapshotService 负责配置快照的生成、比对与回滚。
type SnapshotService struct {
	app *App
	// now 可注入，便于单测固定时间。
	now func() time.Time
}

// NewSnapshotService 构造快照服务。
//
// 参数 a 为运行时依赖容器；返回可直接使用的实例。
func NewSnapshotService(a *App) *SnapshotService {
	return &SnapshotService{
		app: a,
		now: func() time.Time { return time.Now().UTC() },
	}
}

// snapshotFormatVersion 是当前快照 Payload 的格式版本。
const snapshotFormatVersion = 1

// SnapshotMeta 是快照列表项（不含庞大的 Payload）。
type SnapshotMeta struct {
	ID         uint64    `json:"id"`
	Name       string    `json:"name"`
	Reason     string    `json:"reason"`
	Checksum   string    `json:"checksum"`
	RuleCount  int       `json:"rule_count"`
	GroupCount int       `json:"group_count"`
	NodeCount  int       `json:"node_count"`
	CreatedAt  time.Time `json:"created_at"`
	// Size 为 Payload 的字节数，便于前端提示「这份快照有多大」。
	Size int `json:"size"`
}

// collect 采集当前全量配置。
//
// 参数 ctx 为上下文。返回 Payload 或错误。
func (s *SnapshotService) collect(ctx context.Context) (*SnapshotPayload, error) {
	payload := &SnapshotPayload{
		Version:   snapshotFormatVersion,
		CreatedAt: s.now().Format(time.RFC3339),
	}

	var groups []model.DeviceGroup
	if err := s.app.DB.WithContext(ctx).Model(&model.DeviceGroup{}).Order("id ASC").
		Find(&groups).Error; err != nil {
		return nil, err
	}
	for _, g := range groups {
		payload.DeviceGroups = append(payload.DeviceGroups, SnapshotDeviceGroup{
			ID:                   g.ID,
			Name:                 g.Name,
			Type:                 g.Type,
			NodeIDs:              g.NodeIDs,
			Config:               g.Config,
			Balance:              g.Balance,
			HealthCheckEnable:    g.HealthCheckEnable,
			HealthCheckInterval:  g.HealthCheckInterval,
			HealthCheckTimeout:   g.HealthCheckTimeout,
			HealthCheckFailCount: g.HealthCheckFailCount,
			HealthCheckSuccCount: g.HealthCheckSuccCount,
			FailoverGroupID:      g.FailoverGroupID,
			Remark:               g.Remark,
		})
	}

	var nodeGroups []model.NodeGroup
	if err := s.app.DB.WithContext(ctx).Model(&model.NodeGroup{}).Order("id ASC").
		Find(&nodeGroups).Error; err != nil {
		return nil, err
	}
	for _, g := range nodeGroups {
		payload.NodeGroups = append(payload.NodeGroups, SnapshotNodeGroup{
			ID: g.ID, Name: g.Name, Remark: g.Remark,
		})
	}

	var nodes []model.Node
	if err := s.app.DB.WithContext(ctx).Model(&model.Node{}).Order("id ASC").
		Find(&nodes).Error; err != nil {
		return nil, err
	}
	for _, n := range nodes {
		payload.Nodes = append(payload.Nodes, SnapshotNode{
			ID: n.ID, Name: n.Name, Role: n.Role, Token: n.Token,
			ConnectHost: n.ConnectHost, IsStatic: n.IsStatic,
			DirectPort: n.DirectPort, WsPort: n.WsPort, TlsPort: n.TlsPort,
			UdpPort: n.UdpPort, RevPort: n.RevPort,
			GroupIDs: n.GroupIDs, Weight: n.Weight, MaxConn: n.MaxConn, Remark: n.Remark,
		})
	}

	var rules []model.ForwardRule
	if err := s.app.DB.WithContext(ctx).Model(&model.ForwardRule{}).Order("id ASC").
		Find(&rules).Error; err != nil {
		return nil, err
	}
	for _, r := range rules {
		payload.Rules = append(payload.Rules, SnapshotRule{
			ID: r.ID, Name: r.Name, UserID: r.UserID, RuleGroupID: r.RuleGroupID,
			InboundGroupID: r.InboundGroupID, ListenPort: r.ListenPort,
			ListenPortEnd: r.ListenPortEnd, OutboundGroupID: r.OutboundGroupID,
			Targets: r.Targets, TargetBalance: r.TargetBalance,
			InboundMultiplier: r.InboundMultiplier, OutboundMultiplier: r.OutboundMultiplier,
			SpeedLimit: r.SpeedLimit, ConnLimit: r.ConnLimit, IPLimit: r.IPLimit,
			Options: r.Options, ChainGroups: r.ChainGroups,
			ReverseEnable: r.ReverseEnable, ReversePort: r.ReversePort,
			ReverseGroupID: r.ReverseGroupID,
			IsSubRule:      r.IsSubRule, ParentID: r.ParentID, SNI: r.SNI,
			Shaping: r.Shaping, Enable: r.Enable, Remark: r.Remark,
		})
	}

	var ruleGroups []model.RuleGroup
	if err := s.app.DB.WithContext(ctx).Model(&model.RuleGroup{}).Order("id ASC").
		Find(&ruleGroups).Error; err != nil {
		return nil, err
	}
	for _, g := range ruleGroups {
		payload.RuleGroups = append(payload.RuleGroups, SnapshotRuleGroup{
			ID: g.ID, Name: g.Name, Sort: g.Sort, Remark: g.Remark,
		})
	}

	var userGroups []model.UserGroup
	if err := s.app.DB.WithContext(ctx).Model(&model.UserGroup{}).Order("id ASC").
		Find(&userGroups).Error; err != nil {
		return nil, err
	}
	for _, g := range userGroups {
		payload.UserGroups = append(payload.UserGroups, SnapshotUserGroup{
			ID: g.ID, Name: g.Name, TrafficLimit: g.TrafficLimit, SpeedLimit: g.SpeedLimit,
			IPLimit: g.IPLimit, ConnLimit: g.ConnLimit,
			RuleGroupIDs: g.RuleGroupIDs, Remark: g.Remark,
		})
	}

	// 保证空集合序列化为 []，便于前端稳定处理。
	payload.DeviceGroups = nonNil(payload.DeviceGroups)
	payload.NodeGroups = nonNil(payload.NodeGroups)
	payload.Nodes = nonNil(payload.Nodes)
	payload.Rules = nonNil(payload.Rules)
	payload.RuleGroups = nonNil(payload.RuleGroups)
	payload.UserGroups = nonNil(payload.UserGroups)
	return payload, nil
}

// nonNil 把 nil 切片换成空切片。
//
// 泛型版本，避免为每种快照行各写一个函数。
func nonNil[T any](in []T) []T {
	if in == nil {
		return []T{}
	}
	return in
}

// Create 生成一份当前配置的快照（规格书 4.2.14）。
//
// 参数 ctx；name 为快照名（为空时按原因 + 时间自动命名）；reason 为快照原因。
// 返回创建的快照（含 Payload）或错误。
func (s *SnapshotService) Create(ctx context.Context, name, reason string) (*model.ConfigSnapshot, error) {
	if reason == "" {
		reason = SnapshotReasonManual
	}
	if !ValidSnapshotReason(reason) {
		return nil, response.Field(response.CodeParamInvalid, "reason", reason,
			"快照原因必须是 manual / auto_before_sync / auto_daily / before_migrate 之一")
	}

	payload, err := s.collect(ctx)
	if err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "采集当前配置失败")
	}

	raw := util.ToJSON(payload)
	if name = trimSpace(name); name == "" {
		name = defaultSnapshotName(reason, s.now())
	}

	rec := &model.ConfigSnapshot{
		Name:      util.Truncate(name, 128),
		Reason:    util.Truncate(reason, 255),
		Payload:   model.JSON(raw),
		Checksum:  util.SHA256Hex(raw),
		RuleCount: len(payload.Rules),
		// GroupCount 统计设备组 + 节点组 + 规则组 + 用户组的合计，
		// 与前端「分组数量」的直觉一致（用户关心的是「涉及多少个组」）。
		GroupCount: len(payload.DeviceGroups) + len(payload.NodeGroups) +
			len(payload.RuleGroups) + len(payload.UserGroups),
		NodeCount: len(payload.Nodes),
		CreatedAt: s.now(),
	}
	if err := s.app.DB.WithContext(ctx).Create(rec).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "写入配置快照失败")
	}

	s.app.Log.Info("已生成配置快照",
		zap.Uint64("id", rec.ID), zap.String("reason", reason),
		zap.Int("rules", rec.RuleCount), zap.Int("nodes", rec.NodeCount))
	return rec, nil
}

// ValidSnapshotReason 校验快照原因是否合法。
func ValidSnapshotReason(reason string) bool {
	switch reason {
	case SnapshotReasonManual, SnapshotReasonAutoBeforeSync,
		SnapshotReasonAutoDaily, SnapshotReasonBeforeMigrate:
		return true
	}
	return false
}

// defaultSnapshotName 生成默认快照名，格式形如「手动-20260101-120000」。
func defaultSnapshotName(reason string, at time.Time) string {
	label := map[string]string{
		SnapshotReasonManual:         "手动",
		SnapshotReasonAutoBeforeSync: "同步前自动",
		SnapshotReasonAutoDaily:      "每日自动",
		SnapshotReasonBeforeMigrate:  "迁移前",
	}[reason]
	if label == "" {
		label = reason
	}
	return fmt.Sprintf("%s-%s", label, at.Format("20060102-150405"))
}

// List 返回快照列表（不含 Payload，避免列表接口传输几百 KB 的 JSON）。
//
// 参数 ctx；limit 为条数上限，<= 0 时默认 50。返回列表或错误。
func (s *SnapshotService) List(ctx context.Context, limit int) ([]SnapshotMeta, error) {
	if limit <= 0 {
		limit = 50
	}
	items := make([]SnapshotMeta, 0)
	err := s.app.DB.WithContext(ctx).Model(&model.ConfigSnapshot{}).
		Select("id, name, reason, checksum, rule_count, group_count, node_count, created_at, " +
			"LENGTH(payload) AS size").
		Order("created_at DESC").Limit(limit).Scan(&items).Error
	if err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "查询快照列表失败")
	}
	return items, nil
}

// Get 返回单份快照（含 Payload）。
//
// 参数 ctx；id 为快照 ID。返回快照或错误。
func (s *SnapshotService) Get(ctx context.Context, id uint64) (*model.ConfigSnapshot, error) {
	var rec model.ConfigSnapshot
	if err := s.app.DB.WithContext(ctx).First(&rec, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, response.New(response.CodeNotFound, "配置快照不存在")
		}
		return nil, response.Wrap(response.CodeInternal, err, "查询配置快照失败")
	}
	return &rec, nil
}

// Delete 删除一份快照。
//
// 参数 ctx；id 为快照 ID。返回错误。
func (s *SnapshotService) Delete(ctx context.Context, id uint64) error {
	res := s.app.DB.WithContext(ctx).Delete(&model.ConfigSnapshot{}, id)
	if res.Error != nil {
		return response.Wrap(response.CodeInternal, res.Error, "删除配置快照失败")
	}
	if res.RowsAffected == 0 {
		return response.New(response.CodeNotFound, "配置快照不存在")
	}
	return nil
}

// PruneAuto 清理超出保留数量的自动快照，让「每日自动快照」不会无限堆积。
//
// 只删原因以 auto_ 开头的快照，手动快照永远保留——用户手动存的快照一定有用途。
//
// 参数 ctx；keep 为保留条数（<=0 时默认 30）。返回删除行数与错误。
func (s *SnapshotService) PruneAuto(ctx context.Context, keep int) (int64, error) {
	if keep <= 0 {
		keep = 30
	}

	var ids []uint64
	err := s.app.DB.WithContext(ctx).Model(&model.ConfigSnapshot{}).
		Where("reason = ? OR reason = ?", SnapshotReasonAutoDaily, SnapshotReasonAutoBeforeSync).
		Order("created_at DESC").Offset(keep).Pluck("id", &ids).Error
	if err != nil {
		return 0, response.Wrap(response.CodeInternal, err, "查询待清理快照失败")
	}
	if len(ids) == 0 {
		return 0, nil
	}

	res := s.app.DB.WithContext(ctx).Where("id IN ?", ids).Delete(&model.ConfigSnapshot{})
	if res.Error != nil {
		return 0, response.Wrap(response.CodeInternal, res.Error, "清理自动快照失败")
	}
	return res.RowsAffected, nil
}

// -------------------- 差异对比 --------------------

// DiffEntry 是配置差异的一项。
type DiffEntry struct {
	// Resource 为资源类型：device_group | node | rule | rule_group | node_group | user_group。
	Resource string `json:"resource"`
	// ID 为资源 ID；新增项在快照侧有 ID，删除项在当前侧有 ID。
	ID uint64 `json:"id"`
	// Name 为资源名，便于前端直接展示而不必再查一次。
	Name string `json:"name"`
	// Action 为 add（快照有、当前无，回滚将新增）、
	// remove（当前有、快照无，回滚将删除）、
	// update（两侧都有但内容不同，回滚将覆盖）、same（内容一致）。
	Action string `json:"action"`
	// Fields 列出发生变化的字段：字段名 → {current, snapshot}。
	// 结构对齐「左右对照」展示（规格书 6.13 的「查看差异」）。
	Fields []FieldDiff `json:"fields,omitempty"`
}

// FieldDiff 是单个字段的左右差异。
type FieldDiff struct {
	Field string `json:"field"`
	// Current 为当前值，Snapshot 为快照值。
	// 统一用字符串承载，前端按文本对照展示；导出 JSON 时也便于阅读。
	Current  string `json:"current"`
	Snapshot string `json:"snapshot"`
}

// DiffResult 是一次差异对比的结果。
type DiffResult struct {
	SnapshotID uint64      `json:"snapshot_id"`
	Checksum   string      `json:"checksum"`
	CreatedAt  time.Time   `json:"created_at"`
	Entries    []DiffEntry `json:"entries"`
	// 统计信息，便于前端在标题上显示「3 新增 / 2 删除 / 5 修改」。
	Added   int `json:"added"`
	Removed int `json:"removed"`
	Updated int `json:"updated"`
	Same    int `json:"same"`
	// HasDiff 为 true 时表示当前配置与快照不一致。
	HasDiff bool `json:"has_diff"`
}

// diffAction 常量。
const (
	DiffAdd    = "add"
	DiffRemove = "remove"
	DiffUpdate = "update"
	DiffSame   = "same"
)

// Diff 对比当前配置与指定快照（规格书 8.14 GET /snapshots/:id/diff）。
//
// 对比范围覆盖设备组、节点、规则、规则分组、节点分组、用户分组六类资源。
// 返回结构化结果，前端可直接做左右对照展示。
//
// 参数 ctx；id 为快照 ID。返回差异结果或错误。
func (s *SnapshotService) Diff(ctx context.Context, id uint64) (*DiffResult, error) {
	rec, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	var payload SnapshotPayload
	if err := jsonUnmarshal(string(rec.Payload), &payload); err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "解析快照内容失败")
	}
	current, err := s.collect(ctx)
	if err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "采集当前配置失败")
	}

	res := &DiffResult{
		SnapshotID: rec.ID,
		Checksum:   rec.Checksum,
		CreatedAt:  rec.CreatedAt,
		Entries:    make([]DiffEntry, 0),
	}

	// 六类资源逐一对比。泛型辅助函数把「按 ID 建索引 + 比字段」的重复逻辑收敛到一处。
	res.Entries = append(res.Entries, diffResource("device_group",
		payload.DeviceGroups, current.DeviceGroups, func(g SnapshotDeviceGroup) uint64 { return g.ID },
		func(g SnapshotDeviceGroup) string { return g.Name }, deviceGroupFields)...)

	res.Entries = append(res.Entries, diffResource("node_group",
		payload.NodeGroups, current.NodeGroups, func(g SnapshotNodeGroup) uint64 { return g.ID },
		func(g SnapshotNodeGroup) string { return g.Name }, nodeGroupFields)...)

	res.Entries = append(res.Entries, diffResource("node",
		payload.Nodes, current.Nodes, func(n SnapshotNode) uint64 { return n.ID },
		func(n SnapshotNode) string { return n.Name }, nodeFields)...)

	res.Entries = append(res.Entries, diffResource("rule",
		payload.Rules, current.Rules, func(r SnapshotRule) uint64 { return r.ID },
		func(r SnapshotRule) string { return r.Name }, ruleFields)...)

	res.Entries = append(res.Entries, diffResource("rule_group",
		payload.RuleGroups, current.RuleGroups, func(g SnapshotRuleGroup) uint64 { return g.ID },
		func(g SnapshotRuleGroup) string { return g.Name }, ruleGroupFields)...)

	res.Entries = append(res.Entries, diffResource("user_group",
		payload.UserGroups, current.UserGroups, func(g SnapshotUserGroup) uint64 { return g.ID },
		func(g SnapshotUserGroup) string { return g.Name }, userGroupFields)...)

	for _, e := range res.Entries {
		switch e.Action {
		case DiffAdd:
			res.Added++
		case DiffRemove:
			res.Removed++
		case DiffUpdate:
			res.Updated++
		default:
			res.Same++
		}
	}
	res.HasDiff = res.Added > 0 || res.Removed > 0 || res.Updated > 0
	return res, nil
}

// fieldPair 描述一个字段的左右取值。
type fieldPair struct {
	Name  string
	Left  string // 快照侧
	Right string // 当前侧
}

// fieldExtractor 把一条快照行抽取为字段对列表。
type fieldExtractor[T any] func(T) []fieldPair

// deviceGroupFields 抽取设备组的可比较字段。
func deviceGroupFields(g SnapshotDeviceGroup) []fieldPair {
	return []fieldPair{
		{"name", g.Name, ""},
		{"type", g.Type, ""},
		{"node_ids", string(g.NodeIDs), ""},
		{"config", string(g.Config), ""},
		{"balance", g.Balance, ""},
		{"health_check_enable", strconv.FormatBool(g.HealthCheckEnable), ""},
		{"health_check_interval", strconv.Itoa(g.HealthCheckInterval), ""},
		{"health_check_timeout", strconv.Itoa(g.HealthCheckTimeout), ""},
		{"health_check_fail_count", strconv.Itoa(g.HealthCheckFailCount), ""},
		{"health_check_succ_count", strconv.Itoa(g.HealthCheckSuccCount), ""},
		{"failover_group_id", strconv.FormatUint(g.FailoverGroupID, 10), ""},
		{"remark", g.Remark, ""},
	}
}

// nodeGroupFields 抽取节点分组的可比较字段。
func nodeGroupFields(g SnapshotNodeGroup) []fieldPair {
	return []fieldPair{
		{"name", g.Name, ""},
		{"remark", g.Remark, ""},
	}
}

// nodeFields 抽取节点的可比较字段。
func nodeFields(n SnapshotNode) []fieldPair {
	return []fieldPair{
		{"name", n.Name, ""},
		{"role", n.Role, ""},
		{"token", n.Token, ""},
		{"connect_host", n.ConnectHost, ""},
		{"is_static", strconv.FormatBool(n.IsStatic), ""},
		{"direct_port", strconv.Itoa(n.DirectPort), ""},
		{"ws_port", strconv.Itoa(n.WsPort), ""},
		{"tls_port", strconv.Itoa(n.TlsPort), ""},
		{"udp_port", strconv.Itoa(n.UdpPort), ""},
		{"rev_port", strconv.Itoa(n.RevPort), ""},
		{"group_ids", string(n.GroupIDs), ""},
		{"weight", strconv.Itoa(n.Weight), ""},
		{"max_conn", strconv.Itoa(n.MaxConn), ""},
		{"remark", n.Remark, ""},
	}
}

// ruleFields 抽取转发规则的可比较字段。
func ruleFields(r SnapshotRule) []fieldPair {
	return []fieldPair{
		{"name", r.Name, ""},
		{"user_id", strconv.FormatUint(r.UserID, 10), ""},
		{"rule_group_id", strconv.FormatUint(r.RuleGroupID, 10), ""},
		{"inbound_group_id", strconv.FormatUint(r.InboundGroupID, 10), ""},
		{"listen_port", strconv.Itoa(r.ListenPort), ""},
		{"listen_port_end", strconv.Itoa(r.ListenPortEnd), ""},
		{"outbound_group_id", strconv.FormatUint(r.OutboundGroupID, 10), ""},
		{"targets", string(r.Targets), ""},
		{"target_balance", r.TargetBalance, ""},
		{"inbound_multiplier", strconv.FormatFloat(r.InboundMultiplier, 'f', -1, 64), ""},
		{"outbound_multiplier", strconv.FormatFloat(r.OutboundMultiplier, 'f', -1, 64), ""},
		{"speed_limit", strconv.FormatInt(r.SpeedLimit, 10), ""},
		{"conn_limit", strconv.Itoa(r.ConnLimit), ""},
		{"ip_limit", strconv.Itoa(r.IPLimit), ""},
		{"options", string(r.Options), ""},
		{"chain_groups", string(r.ChainGroups), ""},
		{"reverse_enable", strconv.FormatBool(r.ReverseEnable), ""},
		{"reverse_port", strconv.Itoa(r.ReversePort), ""},
		{"reverse_group_id", strconv.FormatUint(r.ReverseGroupID, 10), ""},
		{"is_sub_rule", strconv.FormatBool(r.IsSubRule), ""},
		{"parent_id", strconv.FormatUint(r.ParentID, 10), ""},
		{"sni", r.SNI, ""},
		{"shaping", string(r.Shaping), ""},
		{"enable", strconv.FormatBool(r.Enable), ""},
		{"remark", r.Remark, ""},
	}
}

// ruleGroupFields 抽取规则分组的可比较字段。
func ruleGroupFields(g SnapshotRuleGroup) []fieldPair {
	return []fieldPair{
		{"name", g.Name, ""},
		{"sort", strconv.Itoa(g.Sort), ""},
		{"remark", g.Remark, ""},
	}
}

// userGroupFields 抽取用户分组的可比较字段。
func userGroupFields(g SnapshotUserGroup) []fieldPair {
	return []fieldPair{
		{"name", g.Name, ""},
		{"traffic_limit", strconv.FormatInt(g.TrafficLimit, 10), ""},
		{"speed_limit", strconv.FormatInt(g.SpeedLimit, 10), ""},
		{"ip_limit", strconv.Itoa(g.IPLimit), ""},
		{"conn_limit", strconv.Itoa(g.ConnLimit), ""},
		{"rule_group_ids", string(g.RuleGroupIDs), ""},
		{"remark", g.Remark, ""},
	}
}

// diffResource 对比一类资源。
//
// 参数：
//   - resource 为资源类型名；
//   - snapshot / current 为两侧集合；
//   - idOf / nameOf 取 ID 与名称；
//   - fields 抽取字段对。
//
// 返回排序后的差异条目（新增、删除、修改在前，内容一致在后）。
func diffResource[T any](resource string, snapshot, current []T,
	idOf func(T) uint64, nameOf func(T) string, fields fieldExtractor[T]) []DiffEntry {

	snapIdx := make(map[uint64]T, len(snapshot))
	for _, v := range snapshot {
		snapIdx[idOf(v)] = v
	}
	curIdx := make(map[uint64]T, len(current))
	for _, v := range current {
		curIdx[idOf(v)] = v
	}

	entries := make([]DiffEntry, 0, len(snapshot)+len(current))

	// 1. 快照里有、当前没有 → 回滚会新增。
	for _, v := range snapshot {
		id := idOf(v)
		if _, ok := curIdx[id]; !ok {
			entries = append(entries, DiffEntry{
				Resource: resource, ID: id, Name: nameOf(v), Action: DiffAdd,
			})
		}
	}

	// 2. 两侧都有 → 逐字段比较。
	for _, v := range snapshot {
		id := idOf(v)
		cur, ok := curIdx[id]
		if !ok {
			continue
		}
		snapFields := fields(v)
		curFields := fields(cur)
		// 两侧都由同一份抽取函数生成，长度与顺序一致，因此可以按下标对齐。
		diffs := make([]FieldDiff, 0, len(snapFields))
		for i := range snapFields {
			left := snapFields[i].Left
			right := left
			if i < len(curFields) {
				right = curFields[i].Left
			}
			if left == right {
				continue
			}
			diffs = append(diffs, FieldDiff{
				Field:    snapFields[i].Name,
				Current:  right,
				Snapshot: left,
			})
		}
		action := DiffSame
		if len(diffs) > 0 {
			action = DiffUpdate
		}
		entries = append(entries, DiffEntry{
			Resource: resource, ID: id, Name: nameOf(v),
			Action: action, Fields: diffs,
		})
	}

	// 3. 当前有、快照没有 → 回滚会删除。
	for _, v := range current {
		id := idOf(v)
		if _, ok := snapIdx[id]; !ok {
			entries = append(entries, DiffEntry{
				Resource: resource, ID: id, Name: nameOf(v), Action: DiffRemove,
			})
		}
	}

	sortDiffEntries(entries)
	return entries
}

// sortDiffEntries 让差异条目按「资源 → 动作 → ID」稳定排序。
//
// 稳定输出顺序是前端做左右对照的前提，也让两次 Diff 的结果可以直接 diff 文本。
func sortDiffEntries(entries []DiffEntry) {
	actionOrder := func(a string) int {
		switch a {
		case DiffAdd:
			return 0
		case DiffUpdate:
			return 1
		case DiffRemove:
			return 2
		default:
			return 3
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Resource != entries[j].Resource {
			return entries[i].Resource < entries[j].Resource
		}
		if actionOrder(entries[i].Action) != actionOrder(entries[j].Action) {
			return actionOrder(entries[i].Action) < actionOrder(entries[j].Action)
		}
		return entries[i].ID < entries[j].ID
	})
}

// -------------------- 回滚 --------------------

// RollbackResult 是一次回滚的结果。
type RollbackResult struct {
	// PreRollbackSnapshotID 是回滚前自动生成的快照 ID，
	// 用户若对回滚结果不满意，可以再用它回滚回去（规格书 6.13 的「可逆」保证）。
	PreRollbackSnapshotID uint64 `json:"pre_rollback_snapshot_id"`
	// Restored 各类资源的恢复条数。
	Restored map[string]int `json:"restored"`
	// Removed 各类资源的删除条数（快照里没有、当前多余的）。
	Removed map[string]int `json:"removed"`
	// ConfigVersion 为回滚后自增到的配置版本号。
	ConfigVersion int64 `json:"config_version"`
}

// Rollback 把当前配置回滚到指定快照（规格书 6.13）。
//
// MUST 先创建一份「回滚前快照」，保证整个操作可逆——这是规格书明文要求，
// 也是本方法第一步就做的事：如果这一步失败，直接中止，不做任何写入。
//
// 回滚语义：
//   - 快照中存在的资源：按 ID 覆盖（Upsert），ID 保持不变，因此规则与设备组的引用关系不会错位；
//   - 快照中不存在的资源：从当前配置中删除（否则「回滚」会留下被删掉的东西）；
//   - 节点只覆盖配置字段，不覆盖在线状态、探针指标等运行时字段。
//
// 参数 ctx；id 为快照 ID；operator 为操作者名字（写入日志）。返回结果或错误。
func (s *SnapshotService) Rollback(ctx context.Context, id uint64, operator string) (*RollbackResult, error) {
	rec, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	var payload SnapshotPayload
	if err := jsonUnmarshal(string(rec.Payload), &payload); err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "解析快照内容失败")
	}

	// 第一步：生成回滚前快照。失败即中止，绝不在「不可逆」的状态下动数据库。
	pre, err := s.Create(ctx, fmt.Sprintf("回滚前自动快照（将回滚到 #%d）", rec.ID),
		SnapshotReasonBeforeMigrate)
	if err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "生成回滚前快照失败，已中止回滚")
	}

	result := &RollbackResult{
		PreRollbackSnapshotID: pre.ID,
		Restored:              make(map[string]int),
		Removed:               make(map[string]int),
	}

	// 第二步：在单个事务内完成全部恢复与清理。
	err = s.app.DB.Tx(ctx, func(tx *gorm.DB) error {
		if err := s.restoreDeviceGroups(ctx, tx, payload.DeviceGroups, result); err != nil {
			return err
		}
		if err := s.restoreNodeGroups(ctx, tx, payload.NodeGroups, result); err != nil {
			return err
		}
		if err := s.restoreNodes(ctx, tx, payload.Nodes, result); err != nil {
			return err
		}
		if err := s.restoreRuleGroups(ctx, tx, payload.RuleGroups, result); err != nil {
			return err
		}
		if err := s.restoreUserGroups(ctx, tx, payload.UserGroups, result); err != nil {
			return err
		}
		// 规则最后恢复：它引用设备组与规则分组，放最后可以避免中间态触发外键式校验。
		if err := s.restoreRules(ctx, tx, payload.Rules, result); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "回滚配置失败")
	}

	// 第三步：版本自增并通知节点拉取，否则节点仍跑着旧配置。
	result.ConfigVersion = s.app.BumpConfigVersion("snapshot_rollback")

	s.app.Log.Info("配置已回滚",
		zap.Uint64("snapshot_id", rec.ID), zap.Uint64("pre_snapshot_id", pre.ID),
		zap.String("operator", operator), zap.Int64("config_version", result.ConfigVersion))
	return result, nil
}

// restoreDeviceGroups 恢复设备组，并删除快照中不存在的组。
func (s *SnapshotService) restoreDeviceGroups(ctx context.Context, tx *gorm.DB, rows []SnapshotDeviceGroup, res *RollbackResult) error {
	ids := make([]uint64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
		rec := model.DeviceGroup{
			ID: r.ID, Name: r.Name, Type: r.Type,
			NodeIDs: r.NodeIDs, Config: r.Config, Balance: r.Balance,
			HealthCheckEnable: r.HealthCheckEnable, HealthCheckInterval: r.HealthCheckInterval,
			HealthCheckTimeout: r.HealthCheckTimeout, HealthCheckFailCount: r.HealthCheckFailCount,
			HealthCheckSuccCount: r.HealthCheckSuccCount, FailoverGroupID: r.FailoverGroupID,
			Remark: r.Remark, UpdatedAt: timeNow(),
		}
		// 显式 Save 会带上主键，Gorm 转为「有则更新、无则插入」。
		if err := tx.WithContext(ctx).Save(&rec).Error; err != nil {
			return err
		}
		res.Restored["device_group"]++
	}
	return deleteMissing(ctx, tx, &model.DeviceGroup{}, ids, "device_group", res)
}

// restoreNodeGroups 恢复节点分组并删除多余项。
func (s *SnapshotService) restoreNodeGroups(ctx context.Context, tx *gorm.DB, rows []SnapshotNodeGroup, res *RollbackResult) error {
	ids := make([]uint64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
		rec := model.NodeGroup{ID: r.ID, Name: r.Name, Remark: r.Remark, UpdatedAt: timeNow()}
		if err := tx.WithContext(ctx).Save(&rec).Error; err != nil {
			return err
		}
		res.Restored["node_group"]++
	}
	return deleteMissing(ctx, tx, &model.NodeGroup{}, ids, "node_group", res)
}

// restoreNodes 恢复节点的配置字段。
//
// 刻意不用 Save：节点在线状态、探针指标、配置版本等运行时字段
// 属于「当前事实」，回滚配置不应该把它们改成快照时的旧值。
func (s *SnapshotService) restoreNodes(ctx context.Context, tx *gorm.DB, rows []SnapshotNode, res *RollbackResult) error {
	ids := make([]uint64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
		updates := map[string]interface{}{
			"name": r.Name, "role": r.Role, "token": r.Token,
			"connect_host": r.ConnectHost, "is_static": r.IsStatic,
			"direct_port": r.DirectPort, "ws_port": r.WsPort, "tls_port": r.TlsPort,
			"udp_port": r.UdpPort, "rev_port": r.RevPort,
			"group_ids": r.GroupIDs, "weight": r.Weight, "max_conn": r.MaxConn,
			"remark": r.Remark, "updated_at": timeNow(),
		}
		res2 := tx.WithContext(ctx).Model(&model.Node{}).Where("id = ?", r.ID).Updates(updates)
		if res2.Error != nil {
			return res2.Error
		}
		if res2.RowsAffected == 0 {
			// 节点在快照之后被删除：直接重建，保证回滚结果与快照一致。
			rec := model.Node{
				ID: r.ID, Name: r.Name, Role: r.Role, Token: r.Token,
				ConnectHost: r.ConnectHost, IsStatic: r.IsStatic,
				DirectPort: r.DirectPort, WsPort: r.WsPort, TlsPort: r.TlsPort,
				UdpPort: r.UdpPort, RevPort: r.RevPort,
				GroupIDs: r.GroupIDs, Weight: r.Weight, MaxConn: r.MaxConn,
				Remark: r.Remark,
			}
			if err := tx.WithContext(ctx).Create(&rec).Error; err != nil {
				return err
			}
		}
		res.Restored["node"]++
	}
	return deleteMissing(ctx, tx, &model.Node{}, ids, "node", res)
}

// restoreRuleGroups 恢复规则分组并删除多余项。
func (s *SnapshotService) restoreRuleGroups(ctx context.Context, tx *gorm.DB, rows []SnapshotRuleGroup, res *RollbackResult) error {
	ids := make([]uint64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
		rec := model.RuleGroup{ID: r.ID, Name: r.Name, Sort: r.Sort, Remark: r.Remark, UpdatedAt: timeNow()}
		if err := tx.WithContext(ctx).Save(&rec).Error; err != nil {
			return err
		}
		res.Restored["rule_group"]++
	}
	return deleteMissing(ctx, tx, &model.RuleGroup{}, ids, "rule_group", res)
}

// restoreUserGroups 恢复用户分组并删除多余项。
//
// 注意：删除用户分组前不检查是否仍被用户引用——回滚的目标是「回到快照那一刻」，
// 若因引用而拒绝删除，回滚就会半途而废。用户的 group_id 会在下次编辑时重新指定。
func (s *SnapshotService) restoreUserGroups(ctx context.Context, tx *gorm.DB, rows []SnapshotUserGroup, res *RollbackResult) error {
	ids := make([]uint64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
		rec := model.UserGroup{
			ID: r.ID, Name: r.Name, TrafficLimit: r.TrafficLimit, SpeedLimit: r.SpeedLimit,
			IPLimit: r.IPLimit, ConnLimit: r.ConnLimit,
			RuleGroupIDs: r.RuleGroupIDs, Remark: r.Remark, UpdatedAt: timeNow(),
		}
		if err := tx.WithContext(ctx).Save(&rec).Error; err != nil {
			return err
		}
		res.Restored["user_group"]++
	}
	return deleteMissing(ctx, tx, &model.UserGroup{}, ids, "user_group", res)
}

// restoreRules 恢复转发规则并删除多余项。
//
// 规则用 Save（带主键）而不是 Updates-from-map，因为规则字段多且含大量 JSON 列，
// Save 的「全字段覆盖」语义正是回滚想要的：快照里是什么，回滚后就是什么。
// 但 sync_status 需要特殊处理：回滚只改了配置，同步状态应当重置为 unsynced，
// 让节点重新拉取并上报真实的同步结果。
func (s *SnapshotService) restoreRules(ctx context.Context, tx *gorm.DB, rows []SnapshotRule, res *RollbackResult) error {
	ids := make([]uint64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
		rec := model.ForwardRule{
			ID: r.ID, Name: r.Name, UserID: r.UserID, RuleGroupID: r.RuleGroupID,
			InboundGroupID: r.InboundGroupID, ListenPort: r.ListenPort,
			ListenPortEnd: r.ListenPortEnd, OutboundGroupID: r.OutboundGroupID,
			Targets: r.Targets, TargetBalance: r.TargetBalance,
			InboundMultiplier: r.InboundMultiplier, OutboundMultiplier: r.OutboundMultiplier,
			SpeedLimit: r.SpeedLimit, ConnLimit: r.ConnLimit, IPLimit: r.IPLimit,
			Options: r.Options, ChainGroups: r.ChainGroups,
			ReverseEnable: r.ReverseEnable, ReversePort: r.ReversePort,
			ReverseGroupID: r.ReverseGroupID,
			IsSubRule:      r.IsSubRule, ParentID: r.ParentID, SNI: r.SNI,
			Shaping: r.Shaping, Enable: r.Enable, Remark: r.Remark,
			// 回滚后配置与节点上的实际配置必然不一致，状态置为未同步并清掉错误。
			SyncStatus: model.SyncUnsynced,
			SyncError:  "",
			UpdatedAt:  timeNow(),
		}
		if err := tx.WithContext(ctx).Save(&rec).Error; err != nil {
			return err
		}
		res.Restored["rule"]++
	}
	return deleteMissing(ctx, tx, &model.ForwardRule{}, ids, "rule", res)
}

// deleteMissing 删除当前存在但快照中不存在的记录。
//
// 参数 ctx / tx；target 为模型；keepIDs 为快照中的 ID 集合；
// resource 为资源名（用于统计）；res 为累加结果的目标。
func deleteMissing(ctx context.Context, tx *gorm.DB, target interface{}, keepIDs []uint64, resource string, res *RollbackResult) error {
	q := tx.WithContext(ctx).Where("1 = 1")
	if len(keepIDs) > 0 {
		q = q.Where("id NOT IN ?", keepIDs)
	}
	del := q.Delete(target)
	if del.Error != nil {
		return del.Error
	}
	if del.RowsAffected > 0 {
		res.Removed[resource] = int(del.RowsAffected)
	}
	return nil
}

// ExportJSON 把一份快照的 Payload 原样导出，供「导出 JSON」按钮使用。
//
// 参数 ctx；id 为快照 ID。返回 JSON 字节或错误。
func (s *SnapshotService) ExportJSON(ctx context.Context, id uint64) ([]byte, error) {
	rec, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(rec.Payload) == 0 {
		return nil, response.New(response.CodeNotFound, "该快照没有内容")
	}
	return []byte(rec.Payload), nil
}
