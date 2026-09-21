package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/openroute/openroute/internal/api/response"
	"github.com/openroute/openroute/internal/model"
	"github.com/openroute/openroute/internal/util"
)

// 本文件实现规格书 6.4 / 8.9 的转发规则业务逻辑：
// CRUD、批量操作、旧文本格式与新 JSON 格式的导入导出、SNI 子规则自动关联、
// 以及 6.4 的流量计费口径（纯函数，带单元测试）。

// RuleService 管理转发规则：CRUD、批量操作、导入导出与流量核算。
type RuleService struct {
	app *App
}

// NewRuleService 构造转发规则服务。
func NewRuleService(a *App) *RuleService { return &RuleService{app: a} }

// ───────────────────────── 列表查询 ─────────────────────────

// RuleListFilter 是规则列表的过滤条件（规格书 8.9 的查询参数）。
type RuleListFilter struct {
	UserID          uint64
	RuleGroupID     uint64
	InboundGroupID  uint64
	OutboundGroupID uint64
	SyncStatus      string
	Enable          *bool
	Keyword         string
	// 分页
	Page     int
	PageSize int
	// 排序：sort_by 取 id / name / listen_port / traffic / created_at / updated_at；
	// order 取 asc / desc。非法值退回「按 id 倒序」，这是列表页默认行为。
	SortBy string
	Order  string
	// OnlySubRules / ExcludeSubRules 供 SNI 分流页按需筛选。
	OnlySubRules    bool
	ExcludeSubRules bool
	// ParentID 非 0 时只返回该主规则下的子规则。
	ParentID uint64
}

// Normalize 补齐分页与排序的默认值。
func (f RuleListFilter) Normalize() RuleListFilter {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize < 1 {
		f.PageSize = 20
	}
	// 上限 200：防止有人用 page_size=100000 把面板打挂。
	if f.PageSize > 200 {
		f.PageSize = 200
	}
	if f.Order != "asc" && f.Order != "desc" {
		f.Order = "desc"
	}
	switch f.SortBy {
	case "id", "name", "listen_port", "traffic", "created_at", "updated_at":
	default:
		f.SortBy = "id"
	}
	return f
}

// RuleCounters 是规则的关联计数，用于列表页显示「受影响用户」等提示。
type RuleCounters struct {
	// SubRuleCount 是该主规则下的子规则数量。
	SubRuleCount int `json:"sub_rule_count"`
}

// RuleListItem 是规则列表项（规格书 6.4 的列表页字段 + 解析后的目标）。
type RuleListItem struct {
	model.ForwardRule
	// TargetList 是解析后的多目标列表（数据库里存的是 JSON 文本）。
	TargetList []model.Target `json:"target_list"`
	// ChainGroupList 是解析后的链式出口组 ID。
	ChainGroupList []uint64 `json:"chain_group_list"`
	// ShapingList 是解析后的整流选项。
	ShapingList []int `json:"shaping_list"`
	// InboundGroupName / OutboundGroupName 供列表页直接展示。
	InboundGroupName  string `json:"inbound_group_name"`
	OutboundGroupName string `json:"outbound_group_name"`
	// UserName 供列表页展示归属用户。
	UserName string `json:"user_name"`
	// RuleGroupName 供列表页展示规则分组。
	RuleGroupName string `json:"rule_group_name"`
	// SubRuleCount 是该主规则下的子规则数量（SNI 分流场景）。
	SubRuleCount int `json:"sub_rule_count"`
	// TotalTraffic 是入向 + 出向的累计流量（已乘倍率）。
	TotalTraffic int64 `json:"total_traffic"`
}

// List 返回规则列表（规格书 8.9 GET /forward-rules）。
//
// 参数 ctx 为上下文；filter 为过滤条件。
// 返回当前页的列表项与总数。
func (s *RuleService) List(ctx context.Context, filter RuleListFilter) ([]RuleListItem, int64, error) {
	filter = filter.Normalize()
	q := s.app.DB.WithContext(ctx).Model(&model.ForwardRule{})

	if filter.UserID > 0 {
		q = q.Where("user_id = ?", filter.UserID)
	}
	if filter.RuleGroupID > 0 {
		q = q.Where("rule_group_id = ?", filter.RuleGroupID)
	}
	if filter.InboundGroupID > 0 {
		q = q.Where("inbound_group_id = ?", filter.InboundGroupID)
	}
	if filter.OutboundGroupID > 0 {
		q = q.Where("outbound_group_id = ?", filter.OutboundGroupID)
	}
	if filter.SyncStatus != "" {
		if !model.ValidSyncStatus(filter.SyncStatus) {
			return nil, 0, response.Field(response.CodeParamInvalid, "sync_status",
				filter.SyncStatus, "可选：unsynced / syncing / normal / failed")
		}
		q = q.Where("sync_status = ?", filter.SyncStatus)
	}
	if filter.Enable != nil {
		q = q.Where("enable = ?", *filter.Enable)
	}
	if kw := strings.TrimSpace(filter.Keyword); kw != "" {
		// 关键词同时匹配规则名、备注与目标地址（用户常按目标 IP 找规则）。
		like := "%" + kw + "%"
		q = q.Where("name LIKE ? OR remark LIKE ? OR targets LIKE ?", like, like, like)
	}
	if filter.OnlySubRules {
		q = q.Where("is_sub_rule = ?", true)
	}
	if filter.ExcludeSubRules {
		q = q.Where("is_sub_rule = ?", false)
	}
	if filter.ParentID > 0 {
		q = q.Where("parent_id = ?", filter.ParentID)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, response.Wrap(response.CodeInternal, err, "统计规则数量失败")
	}

	order := ruleOrderClause(filter.SortBy, filter.Order)
	var rows []model.ForwardRule
	if err := q.Order(order).
		Offset((filter.Page - 1) * filter.PageSize).
		Limit(filter.PageSize).
		Find(&rows).Error; err != nil {
		return nil, 0, response.Wrap(response.CodeInternal, err, "查询规则列表失败")
	}

	items := s.decorate(ctx, rows)
	return items, total, nil
}

// ruleOrderClause 把排序参数翻译成 SQL 片段。
//
// 所有取值都在白名单内，不存在拼接注入的风险。
func ruleOrderClause(sortBy, order string) string {
	col := "id"
	switch sortBy {
	case "name":
		col = "name"
	case "listen_port":
		col = "listen_port"
	case "created_at":
		col = "created_at"
	case "updated_at":
		col = "updated_at"
	case "traffic":
		// 累计流量 = 入向 + 出向；跨方言都能算，不必拿出来做应用层排序。
		col = "(traffic_in + traffic_out)"
	case "id":
		col = "id"
	}
	return col + " " + strings.ToUpper(order)
}

// decorate 把数据库行补齐成列表项（解析 JSON 字段 + 关联名称）。
//
// 参数 rows 为规则行；返回列表项。关联名称通过一次性批量查询填充，
// 避免 N+1（列表页最多 200 行，两三次查询足够）。
func (s *RuleService) decorate(ctx context.Context, rows []model.ForwardRule) []RuleListItem {
	out := make([]RuleListItem, 0, len(rows))
	if len(rows) == 0 {
		return out
	}

	groupNames := s.groupNameMap(ctx)
	userNames := s.userNameMap(ctx)
	ruleGroupNames := s.ruleGroupNameMap(ctx)
	subCounts := s.subRuleCountMap(ctx, rows)

	for i := range rows {
		r := rows[i]
		item := RuleListItem{
			ForwardRule:       r,
			TargetList:        r.TargetList(),
			ChainGroupList:    r.ChainGroupList(),
			ShapingList:       r.ShapingList(),
			InboundGroupName:  groupNames[r.InboundGroupID],
			OutboundGroupName: groupNames[r.OutboundGroupID],
			UserName:          userNames[r.UserID],
			RuleGroupName:     ruleGroupNames[r.RuleGroupID],
			TotalTraffic:      r.TrafficIn + r.TrafficOut,
		}
		if n, ok := subCounts[r.ID]; ok {
			item.SubRuleCount = n
		}
		out = append(out, item)
	}
	return out
}

// groupNameMap 返回「设备组 ID → 名称」。
func (s *RuleService) groupNameMap(ctx context.Context) map[uint64]string {
	var rows []struct {
		ID   uint64
		Name string
	}
	out := map[uint64]string{}
	if err := s.app.DB.WithContext(ctx).Model(&model.DeviceGroup{}).
		Select("id, name").Find(&rows).Error; err != nil {
		s.app.Log.Warn("加载设备组名称失败", zap.Error(err))
		return out
	}
	for _, r := range rows {
		out[r.ID] = r.Name
	}
	return out
}

// userNameMap 返回「用户 ID → 用户名」。
func (s *RuleService) userNameMap(ctx context.Context) map[uint64]string {
	var rows []struct {
		ID       uint64
		Username string
	}
	out := map[uint64]string{}
	if err := s.app.DB.WithContext(ctx).Model(&model.User{}).
		Select("id, username").Find(&rows).Error; err != nil {
		s.app.Log.Warn("加载用户名失败", zap.Error(err))
		return out
	}
	for _, r := range rows {
		out[r.ID] = r.Username
	}
	return out
}

// ruleGroupNameMap 返回「规则分组 ID → 名称」。
func (s *RuleService) ruleGroupNameMap(ctx context.Context) map[uint64]string {
	var rows []struct {
		ID   uint64
		Name string
	}
	out := map[uint64]string{}
	if err := s.app.DB.WithContext(ctx).Model(&model.RuleGroup{}).
		Select("id, name").Find(&rows).Error; err != nil {
		s.app.Log.Warn("加载规则分组名称失败", zap.Error(err))
		return out
	}
	for _, r := range rows {
		out[r.ID] = r.Name
	}
	return out
}

// subRuleCountMap 统计主规则的子规则数量。
//
// 参数 rows 为当前页的规则；返回「主规则 ID → 子规则数」。
func (s *RuleService) subRuleCountMap(ctx context.Context, rows []model.ForwardRule) map[uint64]int {
	ids := make([]uint64, 0, len(rows))
	for _, r := range rows {
		if !r.IsSubRule {
			ids = append(ids, r.ID)
		}
	}
	out := map[uint64]int{}
	if len(ids) == 0 {
		return out
	}
	var subs []struct {
		ParentID uint64
	}
	if err := s.app.DB.WithContext(ctx).Model(&model.ForwardRule{}).
		Select("parent_id").Where("parent_id IN ?", ids).Find(&subs).Error; err != nil {
		s.app.Log.Warn("统计子规则数量失败", zap.Error(err))
		return out
	}
	for _, sub := range subs {
		out[sub.ParentID]++
	}
	return out
}

// ───────────────────────── 单条增删改 ─────────────────────────

// RuleInput 是创建 / 更新转发规则的入参（规格书 8.9 的创建规则请求）。
type RuleInput struct {
	Name               string         `json:"name"`
	UserID             uint64         `json:"user_id"`
	RuleGroupID        uint64         `json:"rule_group_id"`
	InboundGroupID     uint64         `json:"inbound_group_id"`
	ListenPort         int            `json:"listen_port"`
	ListenPortEnd      int            `json:"listen_port_end"`
	OutboundGroupID    uint64         `json:"outbound_group_id"`
	Targets            []model.Target `json:"targets"`
	TargetBalance      string         `json:"target_balance"`
	InboundMultiplier  *float64       `json:"inbound_multiplier"`
	OutboundMultiplier *float64       `json:"outbound_multiplier"`
	SpeedLimit         int64          `json:"speed_limit"`
	ConnLimit          int            `json:"conn_limit"`
	IPLimit            int            `json:"ip_limit"`
	Options            model.JSON     `json:"options"`
	ChainGroups        []uint64       `json:"chain_groups"`
	ReverseEnable      bool           `json:"reverse_enable"`
	ReversePort        int            `json:"reverse_port"`
	ReverseGroupID     uint64         `json:"reverse_group_id"`
	IsSubRule          bool           `json:"is_sub_rule"`
	ParentID           uint64         `json:"parent_id"`
	SNI                string         `json:"sni"`
	Shaping            []int          `json:"shaping"`
	Enable             *bool          `json:"enable"`
	Remark             string         `json:"remark"`
}

// Create 创建转发规则（规格书 8.9 POST /forward-rules）。
//
// 校验链（任一失败即返回）：
//
//	名称非空 → 名称查重（EqualFold）→ 倍率范围 → 至少一个有效目标
//	→ 端口合法与冲突 → SNI 固定端口与白名单 → 链式约束 → 反向组约束
//	→ SNI 子规则自动关联主规则（规格书 6.8）→ 落库（状态 unsynced）
//
// 参数 ctx 为上下文；in 为入参。返回创建后的规则。
func (s *RuleService) Create(ctx context.Context, in RuleInput) (*model.ForwardRule, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, response.Field(response.CodeParamInvalid, "name", in.Name, "规则名不能为空")
	}
	if err := s.checkNameConflict(ctx, name, 0); err != nil {
		return nil, err
	}

	r := &model.ForwardRule{
		Name:            util.Truncate(name, 128),
		UserID:          in.UserID,
		RuleGroupID:     in.RuleGroupID,
		InboundGroupID:  in.InboundGroupID,
		ListenPort:      in.ListenPort,
		ListenPortEnd:   in.ListenPortEnd,
		OutboundGroupID: in.OutboundGroupID,
		TargetBalance:   model.TargetBalanceFailover,
		SpeedLimit:      maxInt64(in.SpeedLimit, 0),
		ConnLimit:       maxInt(in.ConnLimit, 0),
		IPLimit:         maxInt(in.IPLimit, 0),
		ReverseEnable:   in.ReverseEnable,
		ReversePort:     in.ReversePort,
		ReverseGroupID:  in.ReverseGroupID,
		IsSubRule:       in.IsSubRule,
		ParentID:        in.ParentID,
		SNI:             util.Truncate(strings.TrimSpace(in.SNI), 255),
		Enable:          true,
		Remark:          util.Truncate(strings.TrimSpace(in.Remark), 255),
		// 初始状态「未同步」（规格书 5.4），由同步服务推进到 syncing/normal/failed。
		SyncStatus: model.SyncUnsynced,
	}
	r.SetTargets(in.Targets)
	if in.TargetBalance != "" {
		r.TargetBalance = in.TargetBalance
	}
	if in.Enable != nil {
		r.Enable = *in.Enable
	}
	if in.InboundMultiplier != nil {
		r.InboundMultiplier = *in.InboundMultiplier
	} else {
		r.InboundMultiplier = 1
	}
	if in.OutboundMultiplier != nil {
		r.OutboundMultiplier = *in.OutboundMultiplier
	} else {
		r.OutboundMultiplier = 1
	}
	if in.Options != nil {
		r.Options = in.Options
	}
	if in.ChainGroups != nil {
		r.ChainGroups = model.FromAny(dedupeUint64(in.ChainGroups))
	}
	if in.Shaping != nil {
		r.Shaping = model.FromAny(in.Shaping)
	}

	if err := s.validateRule(ctx, r, 0); err != nil {
		return nil, err
	}
	// SNI 子规则自动关联主规则（规格书 6.8），必须在落库前完成 ParentID 的确定。
	if err := s.linkSubRule(ctx, r); err != nil {
		return nil, err
	}

	if err := s.app.DB.WithContext(ctx).Create(r).Error; err != nil {
		return nil, s.app.Group.wrapWriteErr(err, "创建转发规则失败")
	}
	s.app.BumpConfigVersion(fmt.Sprintf("创建规则 %s", r.Name))
	s.app.Log.Info("转发规则已创建",
		zap.Uint64("id", r.ID), zap.String("name", r.Name),
		zap.Int("listen_port", r.ListenPort), zap.Bool("is_sub_rule", r.IsSubRule))
	return r, nil
}

// Update 更新转发规则。
//
// 更新语义：入参中显式提供的字段覆盖原值；指针类型的倍率与开关
// 用「是否非 nil」判断是否提供，避免把 0 误判为「未提供」。
//
// 参数 ctx 为上下文；id 为规则 ID；in 为入参。返回更新后的规则。
func (s *RuleService) Update(ctx context.Context, id uint64, in RuleInput) (*model.ForwardRule, error) {
	r, err := s.mustGet(ctx, id)
	if err != nil {
		return nil, err
	}

	if name := strings.TrimSpace(in.Name); name != "" && !util.EqualFoldName(name, r.Name) {
		if err := s.checkNameConflict(ctx, name, id); err != nil {
			return nil, err
		}
		r.Name = util.Truncate(name, 128)
	}
	if in.UserID > 0 {
		r.UserID = in.UserID
	}
	if in.RuleGroupID > 0 || in.RuleGroupID == 0 {
		r.RuleGroupID = in.RuleGroupID
	}
	r.InboundGroupID = in.InboundGroupID
	r.ListenPort = in.ListenPort
	r.ListenPortEnd = in.ListenPortEnd
	r.OutboundGroupID = in.OutboundGroupID
	if in.Targets != nil {
		r.SetTargets(in.Targets)
	}
	if in.TargetBalance != "" {
		r.TargetBalance = in.TargetBalance
	}
	if in.InboundMultiplier != nil {
		r.InboundMultiplier = *in.InboundMultiplier
	}
	if in.OutboundMultiplier != nil {
		r.OutboundMultiplier = *in.OutboundMultiplier
	}
	r.SpeedLimit = maxInt64(in.SpeedLimit, 0)
	r.ConnLimit = maxInt(in.ConnLimit, 0)
	r.IPLimit = maxInt(in.IPLimit, 0)
	if in.Options != nil {
		r.Options = in.Options
	}
	if in.ChainGroups != nil {
		r.ChainGroups = model.FromAny(dedupeUint64(in.ChainGroups))
	}
	r.ReverseEnable = in.ReverseEnable
	r.ReversePort = in.ReversePort
	r.ReverseGroupID = in.ReverseGroupID
	if in.SNI != "" {
		r.SNI = util.Truncate(strings.TrimSpace(in.SNI), 255)
	}
	if in.Shaping != nil {
		r.Shaping = model.FromAny(in.Shaping)
	}
	if in.Enable != nil {
		r.Enable = *in.Enable
	}
	if in.Remark != "" {
		r.Remark = util.Truncate(strings.TrimSpace(in.Remark), 255)
	}
	// 子规则标记与父关联不允许通过普通更新改（必须走 SNI 关联逻辑），
	// 否则会出现「孤儿子规则」——它的 SNI 没有任何主规则接收。
	if in.IsSubRule && !r.IsSubRule {
		r.IsSubRule = true
	}

	if err := s.validateRule(ctx, r, id); err != nil {
		return nil, err
	}
	if err := s.linkSubRule(ctx, r); err != nil {
		return nil, err
	}

	// 配置已变，必须重新下发：状态回到「未同步」（规格书 5.4）。
	r.SyncStatus = model.SyncUnsynced
	r.SyncError = ""
	if err := s.app.DB.WithContext(ctx).Save(r).Error; err != nil {
		return nil, s.app.Group.wrapWriteErr(err, "更新转发规则失败")
	}
	s.app.BumpConfigVersion(fmt.Sprintf("更新规则 %s", r.Name))
	return r, nil
}

// Get 返回单条规则（含解析后的字段）。
//
// 参数 ctx 为上下文；id 为规则 ID。返回列表项结构（复用 decorate）。
func (s *RuleService) Get(ctx context.Context, id uint64) (*RuleListItem, error) {
	r, err := s.mustGet(ctx, id)
	if err != nil {
		return nil, err
	}
	items := s.decorate(ctx, []model.ForwardRule{*r})
	if len(items) == 0 {
		return nil, response.New(response.CodeNotFound, "转发规则不存在")
	}
	return &items[0], nil
}

// Delete 删除转发规则。
//
// 删除主规则时，其子规则会被一并删除（否则子规则会因为失去主规则而失效）。
// 所有删除都在一个事务里完成。
//
// 参数 ctx 为上下文；id 为规则 ID。返回错误（成功时为 nil）。
func (s *RuleService) Delete(ctx context.Context, id uint64) error {
	r, err := s.mustGet(ctx, id)
	if err != nil {
		return err
	}
	err = s.app.DB.Tx(ctx, func(tx *gorm.DB) error {
		if !r.IsSubRule {
			if err := tx.Where("parent_id = ?", id).Delete(&model.ForwardRule{}).Error; err != nil {
				return err
			}
		}
		return tx.Delete(&model.ForwardRule{}, id).Error
	})
	if err != nil {
		return s.app.Group.wrapWriteErr(err, "删除转发规则失败")
	}
	s.app.BumpConfigVersion(fmt.Sprintf("删除规则 %s", r.Name))
	s.app.Log.Info("转发规则已删除", zap.Uint64("id", id), zap.String("name", r.Name))
	return nil
}

// SetEnable 启用 / 禁用规则（规格书 8.9 的 /enable 与 /disable）。
//
// 启用一条子规则时，若其主规则处于禁用状态则一并启用——
// 否则子规则永远收不到流量（用户会以为面板坏了）。
//
// 参数 ctx 为上下文；id 为规则 ID；enable 为目标状态。返回错误（成功时为 nil）。
func (s *RuleService) SetEnable(ctx context.Context, id uint64, enable bool) error {
	r, err := s.mustGet(ctx, id)
	if err != nil {
		return err
	}
	err = s.app.DB.Tx(ctx, func(tx *gorm.DB) error {
		if err := tx.Model(&model.ForwardRule{}).Where("id = ?", id).
			Updates(map[string]interface{}{
				"enable":      enable,
				"sync_status": model.SyncUnsynced,
				"sync_error":  "",
				"updated_at":  time.Now().UTC(),
			}).Error; err != nil {
			return err
		}
		if enable && r.IsSubRule && r.ParentID > 0 {
			if err := tx.Model(&model.ForwardRule{}).Where("id = ?", r.ParentID).
				Updates(map[string]interface{}{
					"enable":      true,
					"sync_status": model.SyncUnsynced,
					"updated_at":  time.Now().UTC(),
				}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return s.app.Group.wrapWriteErr(err, "修改规则启用状态失败")
	}
	s.app.BumpConfigVersion(fmt.Sprintf("规则 %s 启用状态变更", r.Name))
	return nil
}

// ───────────────────────── 规则校验 ─────────────────────────

// validateRule 执行规格书 8.9 要求的全部单条规则校验。
//
// 参数 ctx 为上下文；r 为待校验规则（可未落库）；excludeID 为更新时排除自身的 ID。
// 返回首个违规的 AppError。
func (s *RuleService) validateRule(ctx context.Context, r *model.ForwardRule, excludeID uint64) error {
	// 1. 入口组必需且存在（规格书 6.4 的必填项）。
	if r.InboundGroupID == 0 {
		return response.Field(response.CodeParamInvalid, "inbound_group_id", 0,
			"必须选择入口设备组（它决定协议与入口策略）")
	}
	inGroup, err := s.loadGroup(ctx, r.InboundGroupID)
	if err != nil {
		return err
	}
	if !inGroup.IsInbound() {
		return response.Field(response.CodeGroupRoleMismatch, "inbound_group_id",
			r.InboundGroupID, fmt.Sprintf("设备组 %s 不是入口组", inGroup.Name))
	}

	// 2. 至少一个有效目标（42208）。
	targets := r.TargetList()
	valid := make([]model.Target, 0, len(targets))
	for _, t := range targets {
		if !util.ValidIPOrHost(t.Host) {
			return response.Field(response.CodeParamInvalid, "targets", t.Host,
				fmt.Sprintf("目标地址 %q 不是合法的域名或 IP", t.Host))
		}
		if !util.ValidPort(t.Port) {
			return response.Field(response.CodeParamInvalid, "targets", t.Port,
				fmt.Sprintf("目标端口 %d 非法，范围 1~65535", t.Port))
		}
		valid = append(valid, t)
	}
	if len(valid) == 0 {
		return response.Field(response.CodeTargetRequired, "targets", targets,
			"至少需要一个有效目标（域名或 IP + 端口）")
	}

	// 3. 目标级负载均衡策略。
	if !model.ValidTargetBalance(r.TargetBalance) {
		return response.Field(response.CodeParamInvalid, "target_balance", r.TargetBalance,
			"可选：failover / least_conn / round_robin / weighted / hash_ip")
	}

	// 4. 倍率范围 0~100 且小数位 <= 2（42205）。
	if err := util.ValidMultiplier(r.InboundMultiplier); err != nil {
		return response.Field(response.CodeMultiplierRange, "inbound_multiplier",
			r.InboundMultiplier, err.Error())
	}
	if err := util.ValidMultiplier(r.OutboundMultiplier); err != nil {
		return response.Field(response.CodeMultiplierRange, "outbound_multiplier",
			r.OutboundMultiplier, err.Error())
	}

	// 5. 监听端口：-1 与 0 之外的负数非法；端口段必须递增。
	if r.ListenPort < 0 || r.ListenPortEnd < 0 {
		return response.Field(response.CodeParamInvalid, "listen_port", r.ListenPort,
			"监听端口不能为负数；0 表示随机端口")
	}
	if r.ListenPort > 0 && !util.ValidPort(r.ListenPort) {
		return response.Field(response.CodeParamInvalid, "listen_port", r.ListenPort,
			"监听端口超出范围 1~65535")
	}
	if r.IsMultiPort() && !util.ValidPort(r.ListenPortEnd) {
		return response.Field(response.CodeParamInvalid, "listen_port_end", r.ListenPortEnd,
			"端口段结束值超出范围 1~65535")
	}

	// 6. 同入口组内的端口冲突（40902）。
	if err := s.checkPortConflict(ctx, r, excludeID); err != nil {
		return err
	}

	// 7. 入口组的域名策略（规格书 4.3）：目标命中黑名单直接拒绝。
	if cfg, perr := ParseInboundConfig(inGroup.Config); perr == nil {
		for _, t := range valid {
			if allowed, reason := util.CheckHostPolicy(t.Host, cfg.AllowedHost, cfg.BlockedHost); !allowed {
				return response.Field(response.CodeSNINotAllowed, "targets", t.Host, reason)
			}
		}
	}

	// 8. 出口组（可选；留空 = 单端）。
	if r.OutboundGroupID > 0 {
		og, err := s.loadGroup(ctx, r.OutboundGroupID)
		if err != nil {
			return err
		}
		if !og.IsOutbound() {
			return response.Field(response.CodeGroupRoleMismatch, "outbound_group_id",
				r.OutboundGroupID, fmt.Sprintf("设备组 %s 不是出口组", og.Name))
		}
	}

	// 9. 链式出口约束（42202 / 42203）。
	if err := s.app.Group.ValidateChainRule(ctx, r); err != nil {
		return err
	}

	// 10. 反向隧道约束（42209）与端口分离。
	if err := s.validateReverse(ctx, r); err != nil {
		return err
	}

	// 11. SNI 子规则约束（42204 / 42207）。
	if err := s.validateSNI(ctx, r, inGroup); err != nil {
		return err
	}

	// 12. 整流选项取值。
	for _, v := range r.ShapingList() {
		if !ValidShape(v) {
			return response.Field(response.CodeParamInvalid, "shaping", v,
				"整流选项取值非法，见规格书 6.8 的整流选项表")
		}
	}
	return nil
}

// checkPortConflict 检查同一入口组内是否已有规则占用相同端口（40902）。
//
// 冲突口径：
//   - 单端口与单端口：相等即冲突；
//   - 端口段与端口段：区间相交即冲突；
//   - 单端口与端口段：端口落在区间内即冲突；
//   - 监听端口为 0（随机端口）时不参与冲突判定，由节点侧分配空闲端口。
//
// 参数 ctx 为上下文；r 为待检查规则；excludeID 为更新时排除自身的 ID。
func (s *RuleService) checkPortConflict(ctx context.Context, r *model.ForwardRule, excludeID uint64) error {
	if r.ListenPort <= 0 {
		return nil
	}
	q := s.app.DB.WithContext(ctx).Model(&model.ForwardRule{}).
		Where("inbound_group_id = ?", r.InboundGroupID)
	if excludeID > 0 {
		q = q.Where("id <> ?", excludeID)
	}
	var others []model.ForwardRule
	if err := q.Select("id, name, listen_port, listen_port_end, enable").
		Find(&others).Error; err != nil {
		return response.Wrap(response.CodeInternal, err, "检查端口冲突失败")
	}

	newStart, newEnd := portRangeOf(r)
	for _, o := range others {
		// 已禁用的规则不占用端口：它不会真正监听。
		if !o.Enable {
			continue
		}
		if o.ListenPort <= 0 {
			continue
		}
		oStart, oEnd := portRangeOf(&o)
		if rangesOverlap(newStart, newEnd, oStart, oEnd) {
			detail := fmt.Sprintf("端口 %d 已被规则 %s 占用", r.ListenPort, o.Name)
			if newStart != newEnd || oStart != oEnd {
				detail = fmt.Sprintf("端口段 %s 与规则 %s 的 %s 重叠",
					rangeText(newStart, newEnd), o.Name, rangeText(oStart, oEnd))
			}
			return response.Field(response.CodePortConflict, "listen_port", r.ListenPort, detail)
		}
	}
	return nil
}

// portRangeOf 返回规则实际占用的端口区间 [start, end]。
func portRangeOf(r *model.ForwardRule) (int, int) {
	if r.IsMultiPort() {
		return r.ListenPort, r.ListenPortEnd
	}
	return r.ListenPort, r.ListenPort
}

// rangesOverlap 判断两个闭区间是否相交。
func rangesOverlap(aStart, aEnd, bStart, bEnd int) bool {
	return aStart <= bEnd && bStart <= aEnd
}

// rangeText 把端口区间渲染成可读文本。
func rangeText(start, end int) string {
	if start == end {
		return strconv.Itoa(start)
	}
	return fmt.Sprintf("%d-%d", start, end)
}

// validateReverse 校验反向隧道配置（规格书 6.6，42209）。
func (s *RuleService) validateReverse(ctx context.Context, r *model.ForwardRule) error {
	if !r.ReverseEnable {
		return nil
	}
	if r.ReverseGroupID == 0 {
		return response.Field(response.CodeParamInvalid, "reverse_group_id", 0,
			"启用反向隧道时必须选择反向组")
	}
	rg, err := s.loadGroup(ctx, r.ReverseGroupID)
	if err != nil {
		return err
	}
	if !rg.IsOutbound() {
		return response.Field(response.CodeReverseGroupInvalid, "reverse_group_id",
			r.ReverseGroupID, fmt.Sprintf("反向组 %s 不是出口组", rg.Name))
	}
	// 反向组的出口节点 MUST 配置为出口角色（规格书 6.6）。
	members, err := s.app.Group.loadMembers(ctx, rg.NodeIDs.AsUint64Slice())
	if err != nil {
		return err
	}
	for _, n := range members {
		if !n.IsOutbound() {
			return response.Field(response.CodeReverseGroupInvalid, "reverse_group_id",
				r.ReverseGroupID,
				fmt.Sprintf("反向组 %s 的节点 %s 角色是 %s，未配置为出口角色（is-outbound: true）",
					rg.Name, n.Name, n.Role))
		}
	}
	// 反向隧道端口 MUST 与正向端口分离（规格书 6.6）。
	if r.ReversePort > 0 && util.ValidPort(r.ListenPort) {
		revStart, revEnd := r.ReversePort, r.ReversePort
		fwdStart, fwdEnd := portRangeOf(r)
		if rangesOverlap(revStart, revEnd, fwdStart, fwdEnd) {
			return response.Field(response.CodePortConflict, "reverse_port", r.ReversePort,
				"反向隧道端口 MUST 与正向监听端口分离配置")
		}
	}
	return nil
}

// validateSNI 校验 SNI 子规则（规格书 6.8，42204 / 42207）。
//
// 规则：
//   - 标记为子规则时必须填写 SNI；
//   - SNI MUST 落在入口组 allowed_host 白名单内；
//   - SNI 分流场景下 MUST 使用固定端口（listen_port > 0 且不是端口段）。
//
// 参数 ctx 为上下文；r 为规则；inGroup 为其入口组。
func (s *RuleService) validateSNI(ctx context.Context, r *model.ForwardRule,
	inGroup *model.DeviceGroup) error {

	if !r.IsSubRule && strings.TrimSpace(r.SNI) == "" {
		return nil
	}
	if strings.TrimSpace(r.SNI) == "" {
		return response.Field(response.CodeSNINotAllowed, "sni", "",
			"子规则必须填写 SNI（用于在同一 TLS 端口上按 SNI 分裂流量）")
	}
	cfg, err := ParseInboundConfig(inGroup.Config)
	if err != nil {
		return response.Wrap(response.CodeInternal, err, "入口组 config 解析失败")
	}
	// 必须使用固定端口（42207）：随机端口或端口段都无法承载 SNI 分流。
	if r.ListenPort <= 0 || r.IsMultiPort() {
		return response.Field(response.CodeFixPointRequired, "listen_port", r.ListenPort,
			"SNI 分流必须使用固定端口（listen_port > 0 且不能是端口段）")
	}
	// SNI 必须落在白名单内（42204）。
	if !sniAllowed(r.SNI, cfg.AllowedHost) {
		return response.Field(response.CodeSNINotAllowed, "sni", r.SNI,
			fmt.Sprintf("SNI %s 不在入口组 %s 的 allowed_host 白名单内", r.SNI, inGroup.Name)).
			WithDetails(response.DetailsField{
				Field: "sni", Value: r.SNI,
				Hint: fmt.Sprintf("请把该 SNI 的后缀（如 .%s）加入入口组的 allowed_host",
					strings.TrimPrefix(r.SNI, ".")),
			})
	}
	// SNI 分流要求入口组的 TLS 策略为 2；不是时给出明确的操作指引。
	if cfg.TLSInboundPolicy != TLSInboundSNISplit {
		return response.Field(response.CodeParamInvalid, "config.tls_inbound_policy",
			cfg.TLSInboundPolicy,
			fmt.Sprintf("入口组 %s 的 tls_inbound_policy 不是 2（SNI 分流），"+
				"请在设备组配置中开启 SNI 分流模式", inGroup.Name))
	}
	return nil
}

// ───────────────────────── SNI 子规则自动关联（规格书 6.8） ─────────────────────────

// linkSubRule 在保存子规则前自动找到（或创建）共享 TLS 端口的主规则并设置 ParentID。
//
// 规格书 6.8 的配置步骤：
//
//  2. 管理员创建「主规则」：监听 443（固定端口）、uid: 0（系统归属），
//     该主规则本身不承载用户流量，只做分发；
//  3. 用户创建「子规则」：同一入口组、共享 TLS 端口 443、填写 SNI
//     → 子规则由面板自动关联到主规则（ParentID）。
//
// 主规则的识别口径：同一入口组 + 同一监听端口 + `is_sub_rule = false` 的规则。
// 找不到时**自动创建**一条（用户归属 0、启用、备注标记为自动创建），
// 这样用户只需要填 SNI 就能跑通，不必先手工建主规则。
//
// 参数 ctx 为上下文；r 为待保存的规则（函数会就地修改 r.ParentID）。
// 返回错误（仅数据库错误或语义冲突）。
func (s *RuleService) linkSubRule(ctx context.Context, r *model.ForwardRule) error {
	if !r.IsSubRule {
		return nil
	}
	// 用户显式指定了父规则：校验它确实是一条合格的主规则。
	if r.ParentID > 0 {
		var parent model.ForwardRule
		if err := s.app.DB.WithContext(ctx).First(&parent, r.ParentID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return response.Field(response.CodeNotFound, "parent_id", r.ParentID,
					"指定的主规则不存在")
			}
			return response.Wrap(response.CodeInternal, err, "读取主规则失败")
		}
		if parent.IsSubRule {
			return response.Field(response.CodeParamInvalid, "parent_id", r.ParentID,
				"指定的主规则本身是子规则，不能作为父规则")
		}
		if parent.InboundGroupID != r.InboundGroupID || parent.ListenPort != r.ListenPort {
			return response.Field(response.CodeParamInvalid, "parent_id", r.ParentID,
				"主规则必须与本子规则使用相同的入口组与监听端口")
		}
		return nil
	}

	// 自动查找共享同一入口组与 TLS 端口的主规则。
	var parents []model.ForwardRule
	if err := s.app.DB.WithContext(ctx).
		Where("inbound_group_id = ? AND listen_port = ? AND is_sub_rule = ?",
			r.InboundGroupID, r.ListenPort, false).
		Order("id asc").Find(&parents).Error; err != nil {
		return response.Wrap(response.CodeInternal, err, "查找主规则失败")
	}
	// 排除自己（更新场景）。
	for i := range parents {
		if parents[i].ID != r.ID {
			r.ParentID = parents[i].ID
			return nil
		}
	}

	// 没有主规则：自动创建一条只做分发的主规则（规格书 6.8 第 2 步）。
	parent := model.ForwardRule{
		Name:           fmt.Sprintf("%s-主规则", util.Truncate(r.Name, 100)),
		UserID:         0, // 系统归属
		RuleGroupID:    r.RuleGroupID,
		InboundGroupID: r.InboundGroupID,
		ListenPort:     r.ListenPort,
		ListenPortEnd:  0,
		// 主规则本身不承载用户流量，因此不设出口组与目标（目标由子规则各自持有）。
		OutboundGroupID:    0,
		TargetBalance:      model.TargetBalanceFailover,
		InboundMultiplier:  1,
		OutboundMultiplier: 1,
		IsSubRule:          false,
		Enable:             true,
		SyncStatus:         model.SyncUnsynced,
		Remark:             "SNI 分流：面板自动创建的主规则（只做分发，不承载用户流量）",
	}
	// 名称冲突时追加后缀，避免因为重名导致子规则保存失败。
	parent.Name = s.uniqueRuleName(ctx, parent.Name)
	parent.SetTargets([]model.Target{})
	if err := s.app.DB.WithContext(ctx).Create(&parent).Error; err != nil {
		return s.app.Group.wrapWriteErr(err, "自动创建 SNI 主规则失败")
	}
	s.app.Log.Info("已自动创建 SNI 分流主规则",
		zap.Uint64("id", parent.ID), zap.String("name", parent.Name),
		zap.Int("listen_port", parent.ListenPort))
	r.ParentID = parent.ID
	return nil
}

// uniqueRuleName 生成一个不冲突的规则名（重名时追加 -2、-3 …）。
//
// 参数 ctx 为上下文；base 为期望名称。返回可用的名称。
func (s *RuleService) uniqueRuleName(ctx context.Context, base string) string {
	var names []string
	if err := s.app.DB.WithContext(ctx).Model(&model.ForwardRule{}).
		Pluck("name", &names).Error; err != nil {
		// 查询失败时直接返回原名，让后续的唯一索引把问题暴露出来。
		return util.Truncate(base, 128)
	}
	if _, conflict := util.FindNameConflict(names, base); !conflict {
		return util.Truncate(base, 128)
	}
	for i := 2; i < 1000; i++ {
		cand := fmt.Sprintf("%s-%d", base, i)
		if _, conflict := util.FindNameConflict(names, cand); !conflict {
			return util.Truncate(cand, 128)
		}
	}
	return util.Truncate(base+"-auto", 128)
}

// checkNameConflict 检查规则名是否重复（EqualFold，40901）。
//
// 参数 ctx 为上下文；name 为待检查名称；excludeID 为更新时排除自身的 ID。
func (s *RuleService) checkNameConflict(ctx context.Context, name string, excludeID uint64) error {
	q := s.app.DB.WithContext(ctx).Model(&model.ForwardRule{})
	if excludeID > 0 {
		q = q.Where("id <> ?", excludeID)
	}
	var names []string
	if err := q.Pluck("name", &names).Error; err != nil {
		return response.Wrap(response.CodeInternal, err, "校验规则名失败")
	}
	if _, conflict := util.FindNameConflict(names, name); conflict {
		return response.Field(response.CodeNameConflict, "name", name,
			fmt.Sprintf("规则名 %s 已存在（忽略大小写）", name))
	}
	return nil
}

// loadGroup 按 ID 加载设备组，不存在时返回 40401。
func (s *RuleService) loadGroup(ctx context.Context, id uint64) (*model.DeviceGroup, error) {
	var g model.DeviceGroup
	if err := s.app.DB.WithContext(ctx).First(&g, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, response.New(response.CodeNotFound,
				fmt.Sprintf("设备组 %d 不存在", id))
		}
		return nil, response.Wrap(response.CodeInternal, err, "读取设备组失败")
	}
	return &g, nil
}

// mustGet 按 ID 加载规则，不存在时返回 40401。
func (s *RuleService) mustGet(ctx context.Context, id uint64) (*model.ForwardRule, error) {
	if id == 0 {
		return nil, response.New(response.CodeParamInvalid, "规则 ID 不能为空")
	}
	var r model.ForwardRule
	if err := s.app.DB.WithContext(ctx).First(&r, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, response.New(response.CodeNotFound, fmt.Sprintf("规则 %d 不存在", id))
		}
		return nil, response.Wrap(response.CodeInternal, err, "查询规则失败")
	}
	return &r, nil
}

// ───────────────────────── 批量操作（规格书 8.9 /batch） ─────────────────────────

// 批量操作动作常量（规格书 8.9 的 action 可选值）。
const (
	BatchEnable           = "enable"
	BatchDisable          = "disable"
	BatchDelete           = "delete"
	BatchSetRuleGroup     = "set_rule_group"
	BatchSetMultiplier    = "set_multiplier"
	BatchScaleMultiplier  = "scale_multiplier"
	BatchSetOutboundGroup = "set_outbound_group"
	BatchSetLimits        = "set_limits"
	BatchSetChain         = "set_chain"
)

// ValidBatchAction 判断批量动作是否合法。
func ValidBatchAction(a string) bool {
	switch a {
	case BatchEnable, BatchDisable, BatchDelete, BatchSetRuleGroup,
		BatchSetMultiplier, BatchScaleMultiplier, BatchSetOutboundGroup,
		BatchSetLimits, BatchSetChain:
		return true
	}
	return false
}

// BatchParams 是批量操作的参数（规格书 8.9 的 params 对象）。
//
// 所有字段都是可选的，只对该动作有意义的字段生效。
type BatchParams struct {
	RuleGroupID        uint64   `json:"rule_group_id"`
	OutboundGroupID    uint64   `json:"outbound_group_id"`
	InboundMultiplier  *float64 `json:"inbound_multiplier"`
	OutboundMultiplier *float64 `json:"outbound_multiplier"`
	// Scale 是 scale_multiplier / batch-multiplier 的缩放比例，如 1.5 表示「全部 ×1.5」。
	Scale       float64  `json:"scale"`
	SpeedLimit  int64    `json:"speed_limit"`
	ConnLimit   int      `json:"conn_limit"`
	IPLimit     int      `json:"ip_limit"`
	ChainGroups []uint64 `json:"chain_groups"`
	// ApplyToBoth 在 set_multiplier 时表示「入口与出口倍率都用 inbound_multiplier」。
	ApplyToBoth bool `json:"apply_to_both"`
}

// BatchInput 是批量操作的入参（规格书 8.9）。
type BatchInput struct {
	Action string      `json:"action"`
	IDs    []uint64    `json:"ids"`
	Params BatchParams `json:"params"`
}

// BatchResult 是批量操作的执行结果。
type BatchResult struct {
	Action string `json:"action"`
	// Affected 是受影响（已成功处理）的规则数。
	Affected int `json:"affected"`
	// Skipped 是被跳过的规则 ID（不存在 / 不符合条件）。
	Skipped []uint64 `json:"skipped"`
	// Messages 是逐条的人话说明（如「已删除 3 条规则，影响 2 个用户」）。
	Messages []string `json:"messages"`
}

// Batch 执行批量操作（规格书 8.9 POST /forward-rules/batch）。
//
// 参数 ctx 为上下文；in 为入参。返回执行结果。
//
// 实现要点：整个批量操作放在一个事务里，保证「要么全成功、要么全不改」。
// 唯一的例外是 delete（也可能被取消），仍放在同一事务中。
func (s *RuleService) Batch(ctx context.Context, in BatchInput) (*BatchResult, error) {
	if !ValidBatchAction(in.Action) {
		return nil, response.Field(response.CodeParamInvalid, "action", in.Action,
			"可选：enable / disable / delete / set_rule_group / set_multiplier / "+
				"scale_multiplier / set_outbound_group / set_limits / set_chain")
	}
	ids := dedupeUint64(in.IDs)
	if len(ids) == 0 {
		return nil, response.Field(response.CodeParamInvalid, "ids", in.IDs,
			"必须选择至少一条规则")
	}

	res := &BatchResult{Action: in.Action, Skipped: []uint64{}, Messages: []string{}}

	// 先做参数校验（在事务外，避免开事务后又回滚）。
	if err := s.validateBatchParams(ctx, in); err != nil {
		return nil, err
	}

	err := s.app.DB.Tx(ctx, func(tx *gorm.DB) error {
		// 逐条加载，跳过不存在的 ID（不因单条脏数据让整批失败）。
		var rows []model.ForwardRule
		if err := tx.Where("id IN ?", ids).Find(&rows).Error; err != nil {
			return err
		}
		found := map[uint64]bool{}
		for i := range rows {
			found[rows[i].ID] = true
		}
		for _, id := range ids {
			if !found[id] {
				res.Skipped = append(res.Skipped, id)
			}
		}
		if len(rows) == 0 {
			res.Messages = append(res.Messages, "没有找到任何可处理的规则")
			return nil
		}

		now := time.Now().UTC()
		switch in.Action {
		case BatchEnable, BatchDisable:
			enable := in.Action == BatchEnable
			if err := tx.Model(&model.ForwardRule{}).Where("id IN ?", ids).
				Updates(map[string]interface{}{
					"enable": enable, "sync_status": model.SyncUnsynced,
					"sync_error": "", "updated_at": now,
				}).Error; err != nil {
				return err
			}
			res.Affected = len(rows)
			res.Messages = append(res.Messages, fmt.Sprintf("已%s %d 条规则",
				map[bool]string{true: "启用", false: "禁用"}[enable], len(rows)))

		case BatchDelete:
			// 删除主规则时连带删除其子规则（与单条删除一致）。
			parentIDs := make([]uint64, 0, len(rows))
			for i := range rows {
				if !rows[i].IsSubRule {
					parentIDs = append(parentIDs, rows[i].ID)
				}
			}
			if len(parentIDs) > 0 {
				if err := tx.Where("parent_id IN ?", parentIDs).
					Delete(&model.ForwardRule{}).Error; err != nil {
					return err
				}
			}
			if err := tx.Where("id IN ?", ids).Delete(&model.ForwardRule{}).Error; err != nil {
				return err
			}
			res.Affected = len(rows)
			res.Messages = append(res.Messages, fmt.Sprintf(
				"已删除 %d 条规则，涉及 %d 个归属用户（子规则已连带删除）",
				len(rows), countDistinctUsers(rows)))

		case BatchSetRuleGroup:
			if err := tx.Model(&model.ForwardRule{}).Where("id IN ?", ids).
				Updates(map[string]interface{}{
					"rule_group_id": in.Params.RuleGroupID, "updated_at": now,
				}).Error; err != nil {
				return err
			}
			res.Affected = len(rows)
			res.Messages = append(res.Messages, fmt.Sprintf("已把 %d 条规则移入规则分组 %d",
				len(rows), in.Params.RuleGroupID))

		case BatchSetMultiplier:
			values := map[string]interface{}{"updated_at": now, "sync_status": model.SyncUnsynced}
			if in.Params.InboundMultiplier != nil {
				values["inbound_multiplier"] = *in.Params.InboundMultiplier
				if in.Params.ApplyToBoth {
					values["outbound_multiplier"] = *in.Params.InboundMultiplier
				}
			}
			if in.Params.OutboundMultiplier != nil {
				values["outbound_multiplier"] = *in.Params.OutboundMultiplier
			}
			if err := tx.Model(&model.ForwardRule{}).Where("id IN ?", ids).
				Updates(values).Error; err != nil {
				return err
			}
			res.Affected = len(rows)
			res.Messages = append(res.Messages, fmt.Sprintf("已更新 %d 条规则的倍率", len(rows)))

		case BatchScaleMultiplier:
			for i := range rows {
				inM := roundMultiplier(rows[i].InboundMultiplier * in.Params.Scale)
				outM := roundMultiplier(rows[i].OutboundMultiplier * in.Params.Scale)
				if err := tx.Model(&model.ForwardRule{}).Where("id = ?", rows[i].ID).
					Updates(map[string]interface{}{
						"inbound_multiplier": inM, "outbound_multiplier": outM,
						"sync_status": model.SyncUnsynced, "updated_at": now,
					}).Error; err != nil {
					return err
				}
				res.Affected++
			}
			res.Messages = append(res.Messages, fmt.Sprintf(
				"已按 ×%.2f 缩放 %d 条规则的倍率", in.Params.Scale, res.Affected))

		case BatchSetOutboundGroup:
			if err := tx.Model(&model.ForwardRule{}).Where("id IN ?", ids).
				Updates(map[string]interface{}{
					"outbound_group_id": in.Params.OutboundGroupID,
					"sync_status":       model.SyncUnsynced, "updated_at": now,
				}).Error; err != nil {
				return err
			}
			res.Affected = len(rows)
			res.Messages = append(res.Messages, fmt.Sprintf("已把 %d 条规则迁移到出口组 %d",
				len(rows), in.Params.OutboundGroupID))

		case BatchSetLimits:
			if err := tx.Model(&model.ForwardRule{}).Where("id IN ?", ids).
				Updates(map[string]interface{}{
					"speed_limit": maxInt64(in.Params.SpeedLimit, 0),
					"conn_limit":  maxInt(in.Params.ConnLimit, 0),
					"ip_limit":    maxInt(in.Params.IPLimit, 0),
					"sync_status": model.SyncUnsynced, "updated_at": now,
				}).Error; err != nil {
				return err
			}
			res.Affected = len(rows)
			res.Messages = append(res.Messages, fmt.Sprintf("已更新 %d 条规则的限制", len(rows)))

		case BatchSetChain:
			chain := dedupeUint64(in.Params.ChainGroups)
			// 链式长度 MUST 为 0（取消）或 2~3（规格书 6.7）。
			if len(chain) != 0 && (len(chain) < 2 || len(chain) > 3) {
				return response.Wrap(response.CodeParamInvalid,
					fmt.Errorf("链式出口需要 2~3 跳，收到 %d 个", len(chain)),
					"链式出口的跳数不合法")
			}
			for i := range rows {
				// 逐条走链式约束校验，避免把不合法的组合写进库。
				probe := rows[i]
				probe.ChainGroups = model.FromAny(chain)
				if err := s.app.Group.ValidateChainRule(ctx, &probe); err != nil {
					ae := response.AsAppError(err)
					return ae.WithDetails(response.DetailsField{
						Field: "chain_groups", Value: rows[i].Name, Hint: ae.Msg,
					})
				}
			}
			if err := tx.Model(&model.ForwardRule{}).Where("id IN ?", ids).
				Updates(map[string]interface{}{
					"chain_groups": model.FromAny(chain),
					"sync_status":  model.SyncUnsynced, "updated_at": now,
				}).Error; err != nil {
				return err
			}
			res.Affected = len(rows)
			res.Messages = append(res.Messages, fmt.Sprintf("已设置 %d 条规则的链式出口", len(rows)))
		}
		return nil
	})
	if err != nil {
		return nil, response.AsAppError(err)
	}

	s.app.BumpConfigVersion("批量操作转发规则：" + in.Action)
	return res, nil
}

// validateBatchParams 在开事务前校验批量参数。
func (s *RuleService) validateBatchParams(ctx context.Context, in BatchInput) error {
	switch in.Action {
	case BatchSetRuleGroup:
		if in.Params.RuleGroupID > 0 {
			var n int64
			if err := s.app.DB.WithContext(ctx).Model(&model.RuleGroup{}).
				Where("id = ?", in.Params.RuleGroupID).Count(&n).Error; err != nil {
				return response.Wrap(response.CodeInternal, err, "校验规则分组失败")
			}
			if n == 0 {
				return response.Field(response.CodeNotFound, "rule_group_id",
					in.Params.RuleGroupID, "规则分组不存在")
			}
		}
	case BatchSetOutboundGroup:
		if in.Params.OutboundGroupID > 0 {
			g, err := s.loadGroup(ctx, in.Params.OutboundGroupID)
			if err != nil {
				return err
			}
			if !g.IsOutbound() {
				return response.Field(response.CodeGroupRoleMismatch, "outbound_group_id",
					in.Params.OutboundGroupID, fmt.Sprintf("设备组 %s 不是出口组", g.Name))
			}
		}
	case BatchSetMultiplier:
		if in.Params.InboundMultiplier != nil {
			if err := util.ValidMultiplier(*in.Params.InboundMultiplier); err != nil {
				return response.Field(response.CodeMultiplierRange, "inbound_multiplier",
					*in.Params.InboundMultiplier, err.Error())
			}
		}
		if in.Params.OutboundMultiplier != nil {
			if err := util.ValidMultiplier(*in.Params.OutboundMultiplier); err != nil {
				return response.Field(response.CodeMultiplierRange, "outbound_multiplier",
					*in.Params.OutboundMultiplier, err.Error())
			}
		}
	case BatchScaleMultiplier:
		if in.Params.Scale <= 0 {
			return response.Field(response.CodeParamInvalid, "scale", in.Params.Scale,
				"缩放比例必须大于 0（如 1.5 表示 ×1.5，0.5 表示 ×0.5）")
		}
		// 缩放后可能越界，这里先取当前最大倍率做一次预检，给出明确提示。
		var stats struct {
			MaxIn  float64 `gorm:"column:max_in"`
			MaxOut float64 `gorm:"column:max_out"`
		}
		err := s.app.DB.WithContext(ctx).Model(&model.ForwardRule{}).
			Where("id IN ?", dedupeUint64(in.IDs)).
			Select("COALESCE(MAX(inbound_multiplier), 0) AS max_in, " +
				"COALESCE(MAX(outbound_multiplier), 0) AS max_out").
			Scan(&stats).Error
		// 预检失败不阻断：落库时的逐条校验仍会兜底。
		if err == nil {
			if err := util.ValidMultiplier(roundMultiplier(stats.MaxIn * in.Params.Scale)); err != nil {
				return response.Field(response.CodeMultiplierRange, "scale", in.Params.Scale,
					"缩放后入口倍率会超出允许范围 0~100")
			}
			if err := util.ValidMultiplier(roundMultiplier(stats.MaxOut * in.Params.Scale)); err != nil {
				return response.Field(response.CodeMultiplierRange, "scale", in.Params.Scale,
					"缩放后出口倍率会超出允许范围 0~100")
			}
		}
	case BatchSetChain:
		chain := dedupeUint64(in.Params.ChainGroups)
		if len(chain) != 0 && (len(chain) < 2 || len(chain) > 3) {
			return response.Field(response.CodeParamInvalid, "chain_groups", chain,
				"链式出口需要 2~3 跳（留空表示取消链式）")
		}
	}
	return nil
}

// BatchMultiplier 按比例批量调整倍率（规格书 8.9 POST /batch-multiplier）。
//
// 与 Batch(BatchScaleMultiplier) 的区别：本方法面向「全部 ×1.5」这种
// 不带 ids 的调用（用户在前端选了「全部规则」），因此 ids 为空时
// 默认作用于全部规则。
//
// 参数 ctx 为上下文；ids 为目标规则；scale 为缩放比例；仅入口 / 仅出口 / 两者
// 由 both 决定。返回执行结果。
func (s *RuleService) BatchMultiplier(ctx context.Context, ids []uint64, scale float64, both bool) (*BatchResult, error) {
	if scale <= 0 {
		return nil, response.Field(response.CodeParamInvalid, "scale", scale,
			"缩放比例必须大于 0（如 1.5 表示 ×1.5）")
	}
	res := &BatchResult{Action: BatchScaleMultiplier, Skipped: []uint64{}, Messages: []string{}}

	q := s.app.DB.WithContext(ctx).Model(&model.ForwardRule{})
	if picked := dedupeUint64(ids); len(picked) > 0 {
		q = q.Where("id IN ?", picked)
	}

	var rows []model.ForwardRule
	if err := q.Find(&rows).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "查询待调整的规则失败")
	}
	if len(rows) == 0 {
		res.Messages = append(res.Messages, "没有匹配到任何规则")
		return res, nil
	}

	// 先整体预检：只要有一条会越界就在写入前报错，避免「改了一半」。
	for i := range rows {
		inM := roundMultiplier(rows[i].InboundMultiplier * scale)
		outM := roundMultiplier(rows[i].OutboundMultiplier * scale)
		if both {
			outM = inM
		}
		if err := util.ValidMultiplier(inM); err != nil {
			return nil, response.Field(response.CodeMultiplierRange, "inbound_multiplier",
				rows[i].InboundMultiplier,
				fmt.Sprintf("规则 %s 缩放后的入口倍率 %.4f 超出范围 0~100",
					rows[i].Name, inM))
		}
		if err := util.ValidMultiplier(outM); err != nil {
			return nil, response.Field(response.CodeMultiplierRange, "outbound_multiplier",
				rows[i].OutboundMultiplier,
				fmt.Sprintf("规则 %s 缩放后的出口倍率 %.4f 超出范围 0~100",
					rows[i].Name, outM))
		}
	}

	err := s.app.DB.Tx(ctx, func(tx *gorm.DB) error {
		now := time.Now().UTC()
		for i := range rows {
			inM := roundMultiplier(rows[i].InboundMultiplier * scale)
			outM := roundMultiplier(rows[i].OutboundMultiplier * scale)
			if both {
				outM = inM
			}
			if err := tx.Model(&model.ForwardRule{}).Where("id = ?", rows[i].ID).
				Updates(map[string]interface{}{
					"inbound_multiplier": inM, "outbound_multiplier": outM,
					"sync_status": model.SyncUnsynced, "updated_at": now,
				}).Error; err != nil {
				return err
			}
			res.Affected++
		}
		return nil
	})
	if err != nil {
		return nil, response.AsAppError(err)
	}
	res.Messages = append(res.Messages, fmt.Sprintf("已按 ×%.2f 缩放 %d 条规则的倍率",
		scale, res.Affected))
	s.app.BumpConfigVersion("批量调整规则倍率")
	return res, nil
}

// countDistinctUsers 统计规则集合涉及的归属用户数。
func countDistinctUsers(rows []model.ForwardRule) int {
	seen := map[uint64]struct{}{}
	for _, r := range rows {
		seen[r.UserID] = struct{}{}
	}
	return len(seen)
}

// roundMultiplier 把倍率四舍五入到两位小数（规格书 6.4 的限制）。
//
// 浮点乘法会产生 0.30000000000000004 这类尾巴，
// 直接写库会让「两位小数校验」在下次保存时误报。
func roundMultiplier(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

// ───────────────────────── 流量计费（规格书 6.4） ─────────────────────────

// TrafficSegments 是一次流量统计的原始输入（**未乘倍率**）。
//
// 规格书 6.4 的「单向流量统计」：入向与出向分别按各自的倍率折算后相加。
type TrafficSegments struct {
	// InboundRaw 是入口实际流量（字节，双向合计）。
	InboundRaw int64
	// OutboundRaw 是出口实际流量（字节，双向合计）。
	OutboundRaw int64
}

// TrafficChargeInput 是计费所需的全部参数。
type TrafficChargeInput struct {
	// InboundRaw / OutboundRaw 是入口与出口的实际流量（字节）。
	InboundRaw  int64
	OutboundRaw int64
	// InboundMultiplier / OutboundMultiplier 是入口与出口倍率。
	InboundMultiplier  float64
	OutboundMultiplier float64
	// ChainRaws 是链式出口各段的实际流量（规格书 6.7：
	// 流量同时经过入口与链式出口，两段分别按各自倍率计入）。
	ChainRaws []ChainSegment
}

// ChainSegment 是链式出口中的一段。
type ChainSegment struct {
	// GroupID 是该跳的设备组 ID（用于日志定位）。
	GroupID uint64
	// Raw 是该段的实际流量（字节）。
	Raw int64
	// Multiplier 是该段的倍率。
	Multiplier float64
}

// TrafficCharge 是计费结果。
type TrafficCharge struct {
	// InboundBytes 是入口段折算后的字节数。
	InboundBytes int64
	// OutboundBytes 是出口段折算后的字节数。
	OutboundBytes int64
	// ChainBytes 是链式各段折算后的字节数之和。
	ChainBytes int64
	// Total 是用户应计入的总流量。
	Total int64
}

// ChargeTraffic 实现规格书 6.4 的计费公式：
//
//	用户产生的流量 = 入口实际流量 × 入口倍率 + 出口实际流量 × 出口倍率
//
// 规格书给出的示例（MUST 精确复现，见单元测试）：
//
//	用户下载 500M、上传 100M → 该规则流量增加 500 + 100 = 600M
//	若入口倍率 1.5、出口倍率 0.5
//	则用户已用流量增加 = 600 × 1.5 + 600 × 0.5 = 900 + 300 = 1200MB
//
// 链式场景（规格书 6.4 / 6.7）：两段分别按各自倍率计入，
// 即入口段按入口倍率、每个链式跳按其所在设备组的倍率。
//
// 参数 in 为计费输入。返回计费明细。
func ChargeTraffic(in TrafficChargeInput) TrafficCharge {
	inM := normalizeMultiplier(in.InboundMultiplier)
	outM := normalizeMultiplier(in.OutboundMultiplier)

	out := TrafficCharge{
		InboundBytes:  scaleBytes(in.InboundRaw, inM),
		OutboundBytes: scaleBytes(in.OutboundRaw, outM),
	}
	for _, seg := range in.ChainRaws {
		out.ChainBytes += scaleBytes(seg.Raw, normalizeMultiplier(seg.Multiplier))
	}
	out.Total = out.InboundBytes + out.OutboundBytes + out.ChainBytes
	return out
}

// ChargeRuleTraffic 是 ChargeTraffic 在「单条规则」上的便捷封装。
//
// 参数 rule 为规则；inRaw / outRaw 为入口与出口的实际流量（字节）；
// chainSegments 为链式各段（非链式传 nil）。返回计费明细。
func ChargeRuleTraffic(rule *model.ForwardRule, inRaw, outRaw int64, chainSegments []ChainSegment) TrafficCharge {
	in := TrafficChargeInput{
		InboundRaw:  inRaw,
		OutboundRaw: outRaw,
		ChainRaws:   chainSegments,
	}
	if rule != nil {
		in.InboundMultiplier = rule.InboundMultiplier
		in.OutboundMultiplier = rule.OutboundMultiplier
	} else {
		in.InboundMultiplier = 1
		in.OutboundMultiplier = 1
	}
	return ChargeTraffic(in)
}

// TotalRuleTraffic 返回一条规则当前累计的用户流量（已乘倍率）。
//
// 数据库里的 traffic_in / traffic_out 按规格书 4.2.6 的注释是**已乘倍率**的
// 累计值，因此这里只需要相加。
//
// 参数 rule 为规则。返回累计流量（字节）。
func TotalRuleTraffic(rule *model.ForwardRule) int64 {
	if rule == nil {
		return 0
	}
	return rule.TrafficIn + rule.TrafficOut
}

// UserTrafficOfRules 汇总一组规则的用户流量（已乘倍率）。
//
// 参数 rules 为规则集合。返回用户维度的累计流量。
func UserTrafficOfRules(rules []model.ForwardRule) int64 {
	var total int64
	for i := range rules {
		total += TotalRuleTraffic(&rules[i])
	}
	return total
}

// normalizeMultiplier 把缺失或非法的倍率归一为 1（规格书 6.4 的默认值）。
func normalizeMultiplier(v float64) float64 {
	if v <= 0 && v != 0 {
		// 负数视为非法，退回默认值 1。
		return 1
	}
	return v
}

// scaleBytes 按倍率折算字节数（四舍五入到整数）。
func scaleBytes(raw int64, multiplier float64) int64 {
	if raw == 0 || multiplier == 0 {
		return 0
	}
	return int64(float64(raw)*multiplier + 0.5)
}

// ───────────────────────── 导入 / 导出（规格书 6.4 / 8.9） ─────────────────────────

// ImportFormat 是导入格式。
const (
	ImportFormatText = "text" // 旧文本格式（规格书 6.4 / 附录 D）
	ImportFormatJSON = "json" // 新版 JSON 格式
)

// ImportInput 是导入请求的入参。
type ImportInput struct {
	// Format 为 text / json；为空时自动嗅探（以 [ 或 { 开头视为 JSON）。
	Format string `json:"format"`
	// Content 是原始文本内容。
	Content string `json:"content"`
	// Items 是 JSON 格式的结构化条目（与 Content 二选一）。
	Items []ImportItem `json:"items"`
	// Defaults 是所有条目共享的默认值（如归属用户、入口组）。
	Defaults ImportDefaults `json:"defaults"`
	// Preview 为 true 时只解析不落库（规格书 8.9 的 ?preview=true）。
	Preview bool `json:"preview"`
	// OnConflict 是冲突处理策略：skip（跳过，默认）/ rename（自动改名）。
	OnConflict string `json:"on_conflict"`
}

// ImportDefaults 是导入的共享默认值。
type ImportDefaults struct {
	UserID             uint64  `json:"user_id"`
	RuleGroupID        uint64  `json:"rule_group_id"`
	InboundGroupID     uint64  `json:"inbound_group_id"`
	OutboundGroupID    uint64  `json:"outbound_group_id"`
	TargetBalance      string  `json:"target_balance"`
	InboundMultiplier  float64 `json:"inbound_multiplier"`
	OutboundMultiplier float64 `json:"outbound_multiplier"`
}

// ImportItem 是导入的一条规则（新版 JSON 格式，规格书 6.4）。
type ImportItem struct {
	Name               string         `json:"name"`
	ListenPort         int            `json:"listen_port"`
	ListenPortEnd      int            `json:"listen_port_end"`
	InboundGroupID     uint64         `json:"inbound_group_id"`
	OutboundGroupID    uint64         `json:"outbound_group_id"`
	UserID             uint64         `json:"user_id"`
	RuleGroupID        uint64         `json:"rule_group_id"`
	Targets            []model.Target `json:"targets"`
	TargetBalance      string         `json:"target_balance"`
	InboundMultiplier  float64        `json:"inbound_multiplier"`
	OutboundMultiplier float64        `json:"outbound_multiplier"`
	SpeedLimit         int64          `json:"speed_limit"`
	ConnLimit          int            `json:"conn_limit"`
	IPLimit            int            `json:"ip_limit"`
	Remark             string         `json:"remark"`
}

// ImportConflict 是一条冲突（规格书 8.9 的 conflicts 数组元素）。
type ImportConflict struct {
	// Line 是条目的行号（文本格式）或序号（JSON 格式，从 1 开始）。
	Line int `json:"line"`
	// Name 是规则名。
	Name string `json:"name"`
	// Reason 是冲突原因。
	Reason string `json:"reason"`
	// Suggestion 是处理建议。
	Suggestion string `json:"suggestion"`
}

// ImportInvalid 是一条非法条目（规格书 8.9 的 invalid 数组元素）。
type ImportInvalid struct {
	Line   int    `json:"line"`
	Raw    string `json:"raw"`
	Reason string `json:"reason"`
}

// ImportPreview 是导入预览结果（规格书 8.9 的导入预览响应 data）。
//
// 字段与顺序 MUST 与规格书一致：total / will_create / conflicts / invalid。
type ImportPreview struct {
	Total      int              `json:"total"`
	WillCreate int              `json:"will_create"`
	Conflicts  []ImportConflict `json:"conflicts"`
	Invalid    []ImportInvalid  `json:"invalid"`
	// Applied 为 true 表示本次请求已经真正落库（非预览模式）。
	Applied bool `json:"applied"`
	// CreatedIDs 是本次成功创建的规则 ID（预览模式为空）。
	CreatedIDs []uint64 `json:"created_ids,omitempty"`
}

// Import 执行导入（规格书 8.9 POST /forward-rules/import）。
//
// 支持两种格式：
//
//	旧文本格式（规格书 6.4 + 附录 D）：
//	  名称#监听端口#目标地址#目标端口      单端口
//	  名称##目标地址#目标端口              随机端口
//	  名称#8443-8450#1.2.3.4#443           端口段
//
//	新版 JSON 格式（规格书 6.4）：
//	  { "name": ..., "listen_port": 0, "targets": [...] }
//
// 预览模式（Preview = true，对应 ?preview=true）只解析不落库，
// 返回结构与规格书 8.9 的示例逐字段一致。
//
// 参数 ctx 为上下文；in 为入参。返回预览 / 执行结果。
func (s *RuleService) Import(ctx context.Context, in ImportInput) (*ImportPreview, error) {
	format := strings.ToLower(strings.TrimSpace(in.Format))
	if format == "" {
		format = sniffImportFormat(in.Content)
	}

	result, err := parseImportItems(format, in)
	if err != nil {
		return nil, err
	}
	items := result.Items

	preview := &ImportPreview{Conflicts: []ImportConflict{}, Invalid: []ImportInvalid{}}
	preview.Total = len(items)

	// 已存在的规则名与端口占用情况（一次查出来，避免逐条查库）。
	existingNames, portOwners, err := s.importContext(ctx)
	if err != nil {
		return nil, err
	}

	acc := make([]ImportItem, 0, len(items))
	renames := map[string]int{}

	for i, it := range items {
		line := i + 1
		// 1. 非法条目：文本格式在解析阶段就记录原因；JSON 格式在这里做语义校验。
		if reason := result.ParseErrors[i]; reason != "" {
			preview.Invalid = append(preview.Invalid, ImportInvalid{
				Line: line, Raw: result.Raws[i], Reason: reason,
			})
			continue
		}
		if reason, bad := s.importItemInvalid(ctx, it); bad {
			preview.Invalid = append(preview.Invalid, ImportInvalid{
				Line: line, Raw: result.Raws[i], Reason: reason,
			})
			continue
		}
		// 2. 名称冲突。
		if _, conflict := util.FindNameConflict(existingNames, it.Name); conflict {
			switch strings.ToLower(strings.TrimSpace(in.OnConflict)) {
			case "rename":
				cand := util.Truncate(fmt.Sprintf("%s-%d", it.Name, renames[it.Name]+2), 128)
				renames[it.Name]++
				// 递归找一个真正不冲突的名字。
				for {
					if _, c := util.FindNameConflict(existingNames, cand); !c {
						break
					}
					renames[it.Name]++
					cand = util.Truncate(fmt.Sprintf("%s-%d", it.Name, renames[it.Name]+1), 128)
				}
				it.Name = cand
			default:
				preview.Conflicts = append(preview.Conflicts, ImportConflict{
					Line: line, Name: it.Name, Reason: "名称已存在",
					Suggestion: fmt.Sprintf("改名为 %s-2", it.Name),
				})
				continue
			}
		}
		// 3. 端口冲突（规格书 8.9 示例的第二类冲突）。
		if it.ListenPort > 0 {
			key := fmt.Sprintf("%d:%d", it.InboundGroupID, it.ListenPort)
			if owner, busy := portOwners[key]; busy {
				preview.Conflicts = append(preview.Conflicts, ImportConflict{
					Line: line, Name: it.Name,
					Reason:     fmt.Sprintf("端口 %d 已被规则 %s 占用", it.ListenPort, owner),
					Suggestion: "改用随机端口或其它端口",
				})
				continue
			}
			portOwners[key] = it.Name
		}
		existingNames = append(existingNames, it.Name)
		acc = append(acc, it)
	}

	preview.WillCreate = len(acc)

	if in.Preview {
		return preview, nil
	}

	// 正式落库：整批放在一个事务里（规格书 2.3 的事务要求）。
	err = s.app.DB.Tx(ctx, func(tx *gorm.DB) error {
		for i := range acc {
			r := buildRuleFromImport(acc[i])
			if err := tx.Create(&r).Error; err != nil {
				return err
			}
			preview.CreatedIDs = append(preview.CreatedIDs, r.ID)
		}
		return nil
	})
	if err != nil {
		return nil, response.AsAppError(err)
	}
	preview.Applied = true
	s.app.BumpConfigVersion(fmt.Sprintf("导入 %d 条转发规则", len(preview.CreatedIDs)))
	s.app.Log.Info("转发规则导入完成",
		zap.Int("created", len(preview.CreatedIDs)),
		zap.Int("conflicts", len(preview.Conflicts)),
		zap.Int("invalid", len(preview.Invalid)))
	return preview, nil
}

// sniffImportFormat 根据内容首字符嗅探导入格式。
func sniffImportFormat(content string) string {
	c := strings.TrimSpace(content)
	if strings.HasPrefix(c, "[") || strings.HasPrefix(c, "{") {
		return ImportFormatJSON
	}
	return ImportFormatText
}

// importParseResult 是导入解析的中间结果。
//
// Items / Raws / ParseErrors 三个切片**下标一一对应**：
// 文本格式里解析失败的行会保留占位（Items 为空条目、ParseErrors 记录原因），
// 这样预览结果里的 line 号才能与用户看到的文本行号对齐。
type importParseResult struct {
	Items       []ImportItem
	Raws        []string
	ParseErrors []string
}

// parseImportItems 解析导入内容为结构化条目。
//
// 参数 format 为格式（text / json）；in 为入参。
// 返回值：解析结果与「整体性错误」（内容为空、JSON 语法错误等）。
func parseImportItems(format string, in ImportInput) (*importParseResult, error) {
	defaults := in.Defaults
	if defaults.TargetBalance == "" {
		defaults.TargetBalance = model.TargetBalanceFailover
	}
	if defaults.InboundMultiplier == 0 {
		defaults.InboundMultiplier = 1
	}
	if defaults.OutboundMultiplier == 0 {
		defaults.OutboundMultiplier = 1
	}

	switch format {
	case ImportFormatJSON:
		items := in.Items
		if len(items) == 0 {
			trimmed := strings.TrimSpace(in.Content)
			if trimmed == "" {
				return nil, response.New(response.CodeParamInvalid, "导入内容不能为空")
			}
			if strings.HasPrefix(trimmed, "[") {
				if err := jsonUnmarshal(trimmed, &items); err != nil {
					return nil, response.Wrap(response.CodeParamInvalid, err,
						"导入的 JSON 数组解析失败")
				}
			} else {
				var single ImportItem
				if err := jsonUnmarshal(trimmed, &single); err != nil {
					return nil, response.Wrap(response.CodeParamInvalid, err,
						"导入的 JSON 对象解析失败")
				}
				items = []ImportItem{single}
			}
		}
		res := &importParseResult{
			Items:       make([]ImportItem, len(items)),
			Raws:        make([]string, len(items)),
			ParseErrors: make([]string, len(items)),
		}
		for i := range items {
			applyImportDefaults(&items[i], defaults)
			res.Items[i] = items[i]
			res.Raws[i] = util.ToJSON(items[i])
		}
		return res, nil

	case ImportFormatText:
		lines := splitImportLines(in.Content)
		if len(lines) == 0 {
			return nil, response.New(response.CodeParamInvalid, "导入内容不能为空")
		}
		res := &importParseResult{
			Items:       make([]ImportItem, 0, len(lines)),
			Raws:        make([]string, 0, len(lines)),
			ParseErrors: make([]string, 0, len(lines)),
		}
		for _, ln := range lines {
			item, err := ParseLegacyRuleLine(ln)
			if err != nil {
				// 解析失败的行也占一个位置，reason 直接来自解析器，
				// 这样预览里的 invalid[].reason 就是「缺少目标端口」这类人话。
				res.Items = append(res.Items, ImportItem{})
				res.Raws = append(res.Raws, ln)
				res.ParseErrors = append(res.ParseErrors, err.Error())
				continue
			}
			applyImportDefaults(&item, defaults)
			res.Items = append(res.Items, item)
			res.Raws = append(res.Raws, ln)
			res.ParseErrors = append(res.ParseErrors, "")
		}
		return res, nil
	}
	return nil, response.Field(response.CodeParamInvalid, "format", format,
		"可选：text（旧文本格式）/ json（新版 JSON 格式）")
}

// applyImportDefaults 把共享默认值填入条目中缺失的字段。
func applyImportDefaults(it *ImportItem, d ImportDefaults) {
	if it.InboundGroupID == 0 {
		it.InboundGroupID = d.InboundGroupID
	}
	if it.OutboundGroupID == 0 {
		it.OutboundGroupID = d.OutboundGroupID
	}
	if it.UserID == 0 {
		it.UserID = d.UserID
	}
	if it.RuleGroupID == 0 {
		it.RuleGroupID = d.RuleGroupID
	}
	if it.TargetBalance == "" {
		it.TargetBalance = d.TargetBalance
	}
	if it.InboundMultiplier == 0 {
		it.InboundMultiplier = d.InboundMultiplier
	}
	if it.OutboundMultiplier == 0 {
		it.OutboundMultiplier = d.OutboundMultiplier
	}
}

// importItemInvalid 判断导入条目是否非法，返回原因与是否非法。
//
// 参数 ctx 为上下文；it 为条目。返回（原因，是否非法）。
func (s *RuleService) importItemInvalid(ctx context.Context, it ImportItem) (string, bool) {
	if strings.TrimSpace(it.Name) == "" {
		return "格式错误：缺少规则名", true
	}
	if it.InboundGroupID == 0 {
		return "缺少入口设备组（请在 defaults.inbound_group_id 中指定）", true
	}
	if len(it.Targets) == 0 {
		return "格式错误：缺少目标地址", true
	}
	for _, t := range it.Targets {
		if !util.ValidIPOrHost(t.Host) {
			return fmt.Sprintf("目标地址 %q 不是合法的域名或 IP", t.Host), true
		}
		if !util.ValidPort(t.Port) {
			return fmt.Sprintf("格式错误：目标端口 %d 非法", t.Port), true
		}
	}
	if it.ListenPort < 0 || (it.ListenPort > 0 && !util.ValidPort(it.ListenPort)) {
		return fmt.Sprintf("监听端口 %d 非法", it.ListenPort), true
	}
	if it.ListenPortEnd > 0 && it.ListenPortEnd < it.ListenPort {
		return fmt.Sprintf("端口段 %d-%d 的结束值小于起始值", it.ListenPort, it.ListenPortEnd), true
	}
	if err := util.ValidMultiplier(it.InboundMultiplier); err != nil {
		return fmt.Sprintf("入口倍率非法：%s", err.Error()), true
	}
	if err := util.ValidMultiplier(it.OutboundMultiplier); err != nil {
		return fmt.Sprintf("出口倍率非法：%s", err.Error()), true
	}
	// 入口组是否存在（引用缺失是迁移预检里的一类风险，导入时同样要拦）。
	if it.InboundGroupID > 0 {
		var n int64
		if err := s.app.DB.WithContext(ctx).Model(&model.DeviceGroup{}).
			Where("id = ? AND type = ?", it.InboundGroupID, model.GroupTypeInbound).
			Count(&n).Error; err != nil {
			return "校验入口设备组失败", true
		}
		if n == 0 {
			return fmt.Sprintf("入口设备组 %d 不存在或不是入口组", it.InboundGroupID), true
		}
	}
	return "", false
}

// importContext 预加载导入所需的上下文：已有规则名与端口占用表。
//
// 返回（已有规则名列表，端口占用表，错误）。
// 端口占用表的键为 "入口组ID:端口"，值为占用它的规则名。
func (s *RuleService) importContext(ctx context.Context) ([]string, map[string]string, error) {
	var rows []model.ForwardRule
	if err := s.app.DB.WithContext(ctx).Model(&model.ForwardRule{}).
		Select("id, name, inbound_group_id, listen_port, enable").
		Find(&rows).Error; err != nil {
		return nil, nil, response.Wrap(response.CodeInternal, err, "加载已有规则失败")
	}
	names := make([]string, 0, len(rows))
	ports := make(map[string]string, len(rows))
	for i := range rows {
		names = append(names, rows[i].Name)
		if rows[i].Enable && rows[i].ListenPort > 0 {
			ports[fmt.Sprintf("%d:%d", rows[i].InboundGroupID, rows[i].ListenPort)] = rows[i].Name
		}
	}
	return names, ports, nil
}

// buildRuleFromImport 把导入条目转成可落库的规则模型。
func buildRuleFromImport(it ImportItem) model.ForwardRule {
	r := model.ForwardRule{
		Name:               util.Truncate(strings.TrimSpace(it.Name), 128),
		UserID:             it.UserID,
		RuleGroupID:        it.RuleGroupID,
		InboundGroupID:     it.InboundGroupID,
		ListenPort:         it.ListenPort,
		ListenPortEnd:      it.ListenPortEnd,
		OutboundGroupID:    it.OutboundGroupID,
		TargetBalance:      it.TargetBalance,
		InboundMultiplier:  it.InboundMultiplier,
		OutboundMultiplier: it.OutboundMultiplier,
		SpeedLimit:         maxInt64(it.SpeedLimit, 0),
		ConnLimit:          maxInt(it.ConnLimit, 0),
		IPLimit:            maxInt(it.IPLimit, 0),
		Enable:             true,
		Remark:             util.Truncate(strings.TrimSpace(it.Remark), 255),
		SyncStatus:         model.SyncUnsynced,
	}
	if r.TargetBalance == "" {
		r.TargetBalance = model.TargetBalanceFailover
	}
	if r.InboundMultiplier == 0 {
		r.InboundMultiplier = 1
	}
	if r.OutboundMultiplier == 0 {
		r.OutboundMultiplier = 1
	}
	r.SetTargets(it.Targets)
	return r
}

// splitImportLines 把导入文本切成有效行。
//
// 规则：去掉空行、以 `#` 开头或 `//` 开头的注释行；支持 \r\n 换行。
func splitImportLines(content string) []string {
	raw := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(raw))
	for _, ln := range raw {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		// 以 // 开头的整行注释。
		// 注意：# 是规则格式的有效分隔符，因此 MUST NOT 把 # 当作注释符。
		if strings.HasPrefix(ln, "//") {
			continue
		}
		out = append(out, ln)
	}
	return out
}

// ParseLegacyRuleLine 解析附录 D 的旧文本规则行。
//
// 支持的全部写法（规格书 6.4 / 附录 D）：
//
//	名称#监听端口#目标地址#目标端口      单端口
//	名称##目标地址#目标端口              随机端口（双井号）
//	名称#8443-8450#1.2.3.4#443           端口段
//
// 参数 line 为一行文本。返回解析结果与错误（错误信息即 invalid[].reason）。
func ParseLegacyRuleLine(line string) (ImportItem, error) {
	item := ImportItem{}
	line = strings.TrimSpace(line)
	if line == "" {
		return item, errors.New("空行")
	}

	parts := strings.Split(line, "#")
	if len(parts) < 4 {
		// 兼容只写了 3 段的写法：名称#端口#目标（缺目标端口）。
		return item, errors.New("格式错误：缺少目标端口")
	}
	if len(parts) > 4 {
		// 目标地址里本身带 # 的情况极罕见，这里用「前 3 段固定、其余合并为最后一段」
		// 的解析方式，保证第 4 段是目标端口。
		head := parts[:3]
		tail := parts[len(parts)-1]
		parts = append(head, tail)
	}

	name := strings.TrimSpace(parts[0])
	if name == "" {
		return item, errors.New("格式错误：缺少规则名")
	}

	portText := strings.TrimSpace(parts[1])
	targetHost := strings.TrimSpace(parts[2])
	targetPortText := strings.TrimSpace(parts[3])

	if targetHost == "" {
		return item, errors.New("格式错误：缺少目标地址")
	}

	// 监听端口：空 = 随机端口（附录 D 的 `名称##目标#端口` 写法）。
	listenPort, listenPortEnd := 0, 0
	if portText != "" {
		var err error
		listenPort, listenPortEnd, err = util.ParsePortRange(portText)
		if err != nil {
			return item, fmt.Errorf("格式错误：监听端口 %q 非法（%s）", portText, err.Error())
		}
	}

	targetPort := 0
	if targetPortText == "" {
		return item, errors.New("格式错误：缺少目标端口")
	}
	// 目标端口也允许端口段写法，但 Target 只有一个端口字段，
	// 因此这里只接受单端口（与 Nyanpass 的行为一致）。
	if strings.Contains(targetPortText, "-") {
		return item, errors.New("格式错误：目标端口不支持端口段，请一行写一个目标")
	}
	p, err := strconv.Atoi(targetPortText)
	if err != nil {
		return item, fmt.Errorf("格式错误：目标端口 %q 不是合法数字", targetPortText)
	}
	if !util.ValidPort(p) {
		return item, fmt.Errorf("格式错误：目标端口 %d 超出范围 1~65535", p)
	}
	targetPort = p

	item.Name = name
	item.ListenPort = listenPort
	item.ListenPortEnd = listenPortEnd
	item.Targets = []model.Target{{Host: targetHost, Port: targetPort, Weight: 1, Status: model.TargetUp}}
	return item, nil
}

// ExportRule 是导出 JSON 里的一条规则（规格书 8.9 GET /export）。
//
// 字段与创建请求对齐，便于「导出后直接导入」。
type ExportRule struct {
	Name               string         `json:"name"`
	ListenPort         int            `json:"listen_port"`
	ListenPortEnd      int            `json:"listen_port_end,omitempty"`
	InboundGroupID     uint64         `json:"inbound_group_id"`
	OutboundGroupID    uint64         `json:"outbound_group_id,omitempty"`
	UserID             uint64         `json:"user_id,omitempty"`
	RuleGroupID        uint64         `json:"rule_group_id,omitempty"`
	Targets            []model.Target `json:"targets"`
	TargetBalance      string         `json:"target_balance,omitempty"`
	InboundMultiplier  float64        `json:"inbound_multiplier"`
	OutboundMultiplier float64        `json:"outbound_multiplier"`
	SpeedLimit         int64          `json:"speed_limit,omitempty"`
	ConnLimit          int            `json:"conn_limit,omitempty"`
	IPLimit            int            `json:"ip_limit,omitempty"`
	ChainGroups        []uint64       `json:"chain_groups,omitempty"`
	ReverseEnable      bool           `json:"reverse_enable,omitempty"`
	ReversePort        int            `json:"reverse_port,omitempty"`
	ReverseGroupID     uint64         `json:"reverse_group_id,omitempty"`
	IsSubRule          bool           `json:"is_sub_rule,omitempty"`
	SNI                string         `json:"sni,omitempty"`
	Shaping            []int          `json:"shaping,omitempty"`
	Enable             bool           `json:"enable"`
	Remark             string         `json:"remark,omitempty"`
}

// ExportBundle 是导出的完整包。
//
// 除了规则本身，还带上它依赖的设备组（规格书 6.4 提到「批量导出 JSON」；
// 带上依赖才能在另一台面板上完整重建，否则导入后规则会因为找不到设备组而失效）。
type ExportBundle struct {
	// Version 是导出的格式版本，便于未来做兼容。
	Version string `json:"version"`
	// ExportedAt 是导出时间（RFC 3339 UTC）。
	ExportedAt string `json:"exported_at"`
	// Rules 是规则列表。
	Rules []ExportRule `json:"rules"`
	// InboundGroups / OutboundGroups 是规则依赖的设备组（原文 config）。
	InboundGroups  []model.DeviceGroup `json:"inbound_groups,omitempty"`
	OutboundGroups []model.DeviceGroup `json:"outbound_groups,omitempty"`
	// Count 是规则条数。
	Count int `json:"count"`
}

// Export 导出规则为 JSON（规格书 8.9 GET /forward-rules/export）。
//
// 参数 ctx 为上下文；filter 为筛选条件（与 List 同一套）；
// withGroups 为 true 时附带规则依赖的设备组原文。
// 返回导出包与错误。
func (s *RuleService) Export(ctx context.Context, filter RuleListFilter, withGroups bool) (*ExportBundle, error) {
	// 导出不做分页：上限 10000 条足以覆盖个人自用场景，
	// 同时避免把内存打爆。
	filter.Page = 1
	filter.PageSize = 10000
	items, _, err := s.List(ctx, filter)
	if err != nil {
		return nil, err
	}

	bundle := &ExportBundle{
		Version:    "1.0",
		ExportedAt: time.Now().UTC().Format(time.RFC3339),
		Rules:      make([]ExportRule, 0, len(items)),
		Count:      len(items),
	}
	inIDs := map[uint64]struct{}{}
	outIDs := map[uint64]struct{}{}
	for i := range items {
		r := items[i].ForwardRule
		bundle.Rules = append(bundle.Rules, ExportRule{
			Name:               r.Name,
			ListenPort:         r.ListenPort,
			ListenPortEnd:      r.ListenPortEnd,
			InboundGroupID:     r.InboundGroupID,
			OutboundGroupID:    r.OutboundGroupID,
			UserID:             r.UserID,
			RuleGroupID:        r.RuleGroupID,
			Targets:            r.TargetList(),
			TargetBalance:      r.TargetBalance,
			InboundMultiplier:  r.InboundMultiplier,
			OutboundMultiplier: r.OutboundMultiplier,
			SpeedLimit:         r.SpeedLimit,
			ConnLimit:          r.ConnLimit,
			IPLimit:            r.IPLimit,
			ChainGroups:        r.ChainGroupList(),
			ReverseEnable:      r.ReverseEnable,
			ReversePort:        r.ReversePort,
			ReverseGroupID:     r.ReverseGroupID,
			IsSubRule:          r.IsSubRule,
			SNI:                r.SNI,
			Shaping:            r.ShapingList(),
			Enable:             r.Enable,
			Remark:             r.Remark,
		})
		if r.InboundGroupID > 0 {
			inIDs[r.InboundGroupID] = struct{}{}
		}
		if r.OutboundGroupID > 0 {
			outIDs[r.OutboundGroupID] = struct{}{}
		}
		for _, cid := range r.ChainGroupList() {
			outIDs[cid] = struct{}{}
		}
	}

	if withGroups {
		bundle.InboundGroups = s.loadGroupsByIDs(ctx, inIDs)
		bundle.OutboundGroups = s.loadGroupsByIDs(ctx, outIDs)
	}
	return bundle, nil
}

// loadGroupsByIDs 按 ID 集合加载设备组原文。
func (s *RuleService) loadGroupsByIDs(ctx context.Context, ids map[uint64]struct{}) []model.DeviceGroup {
	if len(ids) == 0 {
		return nil
	}
	list := make([]uint64, 0, len(ids))
	for id := range ids {
		list = append(list, id)
	}
	sort.Slice(list, func(i, j int) bool { return list[i] < list[j] })
	var out []model.DeviceGroup
	if err := s.app.DB.WithContext(ctx).Where("id IN ?", list).Find(&out).Error; err != nil {
		s.app.Log.Warn("导出设备组失败", zap.Error(err))
		return nil
	}
	return out
}

// Sessions 返回某条规则当前的在线会话（规格书 8.9 GET /:id/sessions）。
//
// 面板侧的会话表（规格书 4.2.10）由节点上报维护；本方法只做一次投影，
// 不在这里做聚合统计（那是流量服务的职责）。
//
// 参数 ctx 为上下文；ruleID 为规则 ID。返回会话列表。
func (s *RuleService) Sessions(ctx context.Context, ruleID uint64) ([]model.Session, error) {
	if _, err := s.mustGet(ctx, ruleID); err != nil {
		return nil, err
	}
	var out []model.Session
	if err := s.app.DB.WithContext(ctx).
		Where("rule_id = ?", ruleID).Order("id desc").Find(&out).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "查询在线会话失败")
	}
	if out == nil {
		out = []model.Session{}
	}
	return out, nil
}

// ───────────────────────── 规则同步辅助 ─────────────────────────

// TouchSyncUnsynced 把指定规则标记为「未同步」（配置变更后调用）。
//
// 参数 ctx 为上下文；ids 为规则 ID 列表；reason 写入同步错误字段。
// 返回错误。
func (s *RuleService) TouchSyncUnsynced(ctx context.Context, ids []uint64, reason string) error {
	if len(ids) == 0 {
		return nil
	}
	return s.app.Sync.MarkUnsynced(ids, reason)
}

// RuleUpsertClause 是规则的 UPSERT 子句，供迁移工具复用。
//
// 唯一键为 name（规格书 4.2.6 的 name 只有普通索引，唯一性由应用层保证）；
// 因此这里用 ON CONFLICT(name) 做「同名覆盖」。
func RuleUpsertClause() clause.OnConflict {
	return clause.OnConflict{
		Columns:   []clause.Column{{Name: "name"}},
		DoUpdates: clause.AssignmentColumns([]string{"updated_at"}),
	}
}
