package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/openroute/openroute/internal/api/response"
	"github.com/openroute/openroute/internal/model"
	"github.com/openroute/openroute/internal/util"
)

// 本文件实现规格书 5.4 的配置下发与同步状态机，以及规格书 11.3 的
// 「规则同步可视化与失败诊断」（按节点分组的同步总览）。
//
// 状态机（MUST 严格按此实现）：
//
//	unsynced（未同步，黄色）
//	   │ 收到配置
//	   ▼
//	syncing（同步中，蓝色）
//	   ├─ 应用成功 ─► normal（正常，绿色）
//	   └─ 应用失败 ─► failed（同步失败，红色）+ 错误原因

// SyncService 维护规则的同步状态机。
type SyncService struct {
	app *App
}

// NewSyncService 构造同步服务。
func NewSyncService(a *App) *SyncService { return &SyncService{app: a} }

// ───────────────────────── 状态迁移 ─────────────────────────

// MarkUnsynced 把指定规则标记为「未同步」（规格书 5.4 的第一步：写库时置 unsynced）。
//
// 这是配置变更后的统一入口：任何规则变更都 MUST 经过它，
// 否则节点永远不会知道有新配置（推送是尽力而为的，轮询看的是状态与版本号）。
//
// 参数 ctx 为上下文；ruleIDs 为规则 ID 列表；reason 是变更原因，
// 会被写入 sync_error 字段，供列表页展示「为什么这条规则还是黄色」。
// 返回错误（仅数据库错误）。
func (s *SyncService) MarkUnsynced(ruleIDs []uint64, reason string) error {
	ids := dedupeUint64(ruleIDs)
	if len(ids) == 0 {
		return nil
	}
	return s.updateRules(context.Background(), ids, map[string]interface{}{
		"sync_status": model.SyncUnsynced,
		"sync_error":  util.Truncate(strings.TrimSpace(reason), 512),
		"synced_at":   nil,
		"updated_at":  time.Now().UTC(),
	})
}

// MarkSyncing 把指定规则标记为「同步中」（规格书 5.4 的中间态）。
//
// 参数 ctx 为上下文；ruleIDs 为规则 ID 列表。返回错误。
func (s *SyncService) MarkSyncing(ctx context.Context, ruleIDs []uint64) error {
	ids := dedupeUint64(ruleIDs)
	if len(ids) == 0 {
		return nil
	}
	return s.updateRules(ctx, ids, map[string]interface{}{
		"sync_status": model.SyncSyncing,
		"sync_error":  "",
		"updated_at":  time.Now().UTC(),
	})
}

// MarkResult 记录某个节点对某条规则的应用结果（规格书 5.4 的状态机终点）。
//
// 语义：
//
//	ok = true  → normal，写入 synced_at，清空错误；
//	ok = false → failed，写入节点返回的**原始错误**（规格书 6.4：
//	             「规则行 MUST 显示红色状态与失败原因（可展开查看节点返回的原始错误）」）。
//
// 说明：一条规则可能被下发到多个节点（入口组有多台机器）。本方法按
// 「最后一次上报的结果」决定状态——这符合个人自用场景（用户关心的是
// 「有节点应用失败」，具体哪台看 SyncOverview）。为了避免「先成功的节点
// 把后失败的结果覆盖成正常」，失败结果具有粘性：已 failed 的规则不会被
// 后来的成功重置为 normal，必须通过 Resync 显式清空。
//
// 参数 ruleID 为规则 ID；nodeID 为上报的节点 ID；ok 为是否应用成功；
// errMsg 为节点返回的原始错误。返回错误。
func (s *SyncService) MarkResult(ctx context.Context, ruleID, nodeID uint64, ok bool, errMsg string) error {
	if ruleID == 0 {
		return response.New(response.CodeParamInvalid, "规则 ID 不能为空")
	}

	var r model.ForwardRule
	if err := s.app.DB.WithContext(ctx).First(&r, ruleID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// 规则可能已被删除（节点还在上报旧配置的结果）：静默忽略，
			// 这不是错误，否则节点会被反复要求重试。
			s.app.Log.Debug("忽略已删除规则的同步结果",
				zap.Uint64("rule_id", ruleID), zap.Uint64("node_id", nodeID))
			return nil
		}
		return response.Wrap(response.CodeInternal, err, "查询规则失败")
	}

	now := time.Now().UTC()
	updates := map[string]interface{}{"updated_at": now}

	if ok {
		// 失败粘性：已 failed 的规则不因单台节点成功就转绿。
		if r.SyncStatus == model.SyncFailed {
			s.app.Log.Debug("规则处于同步失败状态，跳过成功上报",
				zap.Uint64("rule_id", ruleID), zap.Uint64("node_id", nodeID))
			return nil
		}
		updates["sync_status"] = model.SyncNormal
		updates["sync_error"] = ""
		updates["synced_at"] = now
	} else {
		updates["sync_status"] = model.SyncFailed
		updates["sync_error"] = util.Truncate(fmt.Sprintf("节点 %d：%s", nodeID, errMsg), 512)
	}

	if err := s.app.DB.WithContext(ctx).Model(&model.ForwardRule{}).
		Where("id = ?", ruleID).Updates(updates).Error; err != nil {
		return response.Wrap(response.CodeInternal, err, "写入同步结果失败")
	}

	// 节点自身的最近错误也同步一份，便于节点列表展示（规格书 4.2.3 的 last_error）。
	if !ok {
		_ = s.app.DB.WithContext(ctx).Model(&model.Node{}).Where("id = ?", nodeID).
			Update("last_error", util.Truncate(errMsg, 512)).Error
	}

	// 把结果广播给前端，规则行可以立刻由黄色/蓝色变成绿色或红色。
	s.app.Hub().Broadcast(Event{
		Type: "rule_sync",
		Data: map[string]interface{}{
			"rule_id":     ruleID,
			"node_id":     nodeID,
			"ok":          ok,
			"sync_status": updates["sync_status"],
			"error":       updates["sync_error"],
		},
	})
	if !ok {
		s.app.Log.Warn("规则同步失败",
			zap.Uint64("rule_id", ruleID), zap.Uint64("node_id", nodeID),
			zap.String("error", errMsg))
	}
	return nil
}

// MarkBatchResults 批量记录同一个节点对多条规则的应用结果。
//
// 参数 ctx 为上下文；nodeID 为节点 ID；results 为「规则 ID → 结果」；
// 全部成功时返回 nil 错误。返回逐条失败的原因（不会中断后续处理）。
func (s *SyncService) MarkBatchResults(ctx context.Context, nodeID uint64, results map[uint64]SyncResult) []error {
	errs := make([]error, 0, len(results))
	for ruleID, res := range results {
		if err := s.MarkResult(ctx, ruleID, nodeID, res.OK, res.ErrMsg); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// SyncResult 是节点上报的一条规则应用结果。
type SyncResult struct {
	OK     bool   `json:"ok"`
	ErrMsg string `json:"error,omitempty"`
}

// Resync 强制重新下发一条规则（规格书 8.9 POST /forward-rules/:id/resync）。
//
// 行为：清空失败状态、置为 unsynced、自增配置版本号触发推送。
// 这是「失败粘性」的显式解除口：用户点了「重新下发」就应当得到干净的状态。
//
// 参数 ctx 为上下文；ruleID 为规则 ID。返回错误。
func (s *SyncService) Resync(ctx context.Context, ruleID uint64) error {
	var r model.ForwardRule
	if err := s.app.DB.WithContext(ctx).First(&r, ruleID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return response.New(response.CodeNotFound, fmt.Sprintf("规则 %d 不存在", ruleID))
		}
		return response.Wrap(response.CodeInternal, err, "查询规则失败")
	}

	// 子规则重新下发时，主规则也必须重发（节点侧是按主+子整体生效的）。
	ids := []uint64{ruleID}
	if r.IsSubRule && r.ParentID > 0 {
		ids = append(ids, r.ParentID)
	}

	if err := s.updateRules(ctx, ids, map[string]interface{}{
		"sync_status": model.SyncUnsynced,
		"sync_error":  "",
		"synced_at":   nil,
		"updated_at":  time.Now().UTC(),
	}); err != nil {
		return err
	}

	s.app.BumpConfigVersion(fmt.Sprintf("重新下发规则 %s", r.Name))
	s.app.Log.Info("规则已标记为重新下发", zap.Uint64("rule_id", ruleID))
	return nil
}

// ResyncAll 强制重新下发全部规则。
//
// 用途：节点版本升级、协议参数调整、配置漂移纠正后的一键重推。
//
// 参数 ctx 为上下文。返回受影响的规则数。
func (s *SyncService) ResyncAll(ctx context.Context) (int64, error) {
	res := s.app.DB.WithContext(ctx).Model(&model.ForwardRule{}).
		Where("enable = ?", true).
		Updates(map[string]interface{}{
			"sync_status": model.SyncUnsynced,
			"sync_error":  "",
			"synced_at":   nil,
			"updated_at":  time.Now().UTC(),
		})
	if res.Error != nil {
		return 0, response.Wrap(response.CodeInternal, res.Error, "重置规则同步状态失败")
	}
	if res.RowsAffected > 0 {
		s.app.BumpConfigVersion("全量重新下发规则")
	}
	return res.RowsAffected, nil
}

// MarkAllPendingReinstall 标记「全部待重装」（规格书 7.2 的迁移路径）。
//
// 迁移完成后 MUST 做的事（规格书 7.2 第 2、3 条）：
//
//   - 节点客户端与旧面板协议不兼容，MUST 在每台机器上重新安装；
//     因此在节点与用户上打「待重装」标记，供节点列表页标注状态；
//   - 全部规则置为 unsynced，节点重装上线后会自动同步。
//
// 本方法把用户、节点、规则三处一次性置位，并由调用方（迁移工具）
// 在报告里提示用户重新安装节点。
//
// 参数 ctx 为上下文。返回（受影响规则数，错误）。
func (s *SyncService) MarkAllPendingReinstall(ctx context.Context) (int64, error) {
	defer s.app.BumpConfigVersion("迁移后标记全部待重装")

	err := s.app.DB.Tx(ctx, func(tx *gorm.DB) error {
		now := time.Now().UTC()
		// 1. 用户：标记待重装。
		if err := tx.Model(&model.User{}).Where("1 = 1").
			Update("pending_reinstall", true).Error; err != nil {
			return err
		}
		// 2. 节点：清空配置版本，让节点上线后必然重新拉取全量配置。
		if err := tx.Model(&model.Node{}).Where("1 = 1").
			Updates(map[string]interface{}{
				"config_ver": 0,
				"last_error": "迁移自其它面板：节点客户端需要重新安装",
				"updated_at": now,
			}).Error; err != nil {
			return err
		}
		// 3. 规则：全部回到未同步（节点重装上线后自动同步）。
		if err := tx.Model(&model.ForwardRule{}).Where("1 = 1").
			Updates(map[string]interface{}{
				"sync_status": model.SyncUnsynced,
				"sync_error":  "迁移后待重新下发",
				"synced_at":   nil,
				"updated_at":  now,
			}).Error; err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return 0, response.Wrap(response.CodeInternal, err, "标记待重装失败")
	}

	var n int64
	_ = s.app.DB.WithContext(ctx).Model(&model.ForwardRule{}).Count(&n).Error
	s.app.Log.Info("已标记全部用户/节点/规则待重装", zap.Int64("rules", n))
	return n, nil
}

// updateRules 批量更新规则的同步字段。
//
// 参数 ctx 为上下文；ids 为规则 ID；values 为待更新字段。
// 返回 AppError（数据库不可写时给 50301）。
func (s *SyncService) updateRules(ctx context.Context, ids []uint64, values map[string]interface{}) error {
	if len(ids) == 0 {
		return nil
	}
	// 分批更新：SQLite 对单条 SQL 的变量数有上限（默认 999），
	// 批量导入几百条规则时很容易撞上。
	const batch = 200
	for start := 0; start < len(ids); start += batch {
		end := start + batch
		if end > len(ids) {
			end = len(ids)
		}
		if err := s.app.DB.WithContext(ctx).Model(&model.ForwardRule{}).
			Where("id IN ?", ids[start:end]).Updates(values).Error; err != nil {
			return s.app.Group.wrapWriteErr(err, "更新规则同步状态失败")
		}
	}
	return nil
}

// ───────────────────────── 同步总览（规格书 11.3） ─────────────────────────

// SyncOverview 是按节点分组的同步总览（规格书 11.3 的
// 「每条规则的同步状态、失败原因原文、按节点分组的同步总览」）。
type SyncOverview struct {
	// ConfigVersion 是当前全局配置版本号（规格书 5.4 的版本号机制）。
	ConfigVersion int64 `json:"config_version"`
	// GeneratedAt 是统计时间（RFC 3339 UTC）。
	GeneratedAt string `json:"generated_at"`
	// Totals 是全局计数。
	Totals SyncTotals `json:"totals"`
	// Nodes 是按节点分组的明细。
	Nodes []NodeSyncSummary `json:"nodes"`
	// Rules 是同步状态异常的规则（失败 + 未同步），供面板直接展示待处理清单。
	Rules []RuleSyncIssue `json:"rules"`
}

// SyncTotals 是全局同步计数。
type SyncTotals struct {
	Rules       int `json:"rules"`
	Unsynced    int `json:"unsynced"`
	Syncing     int `json:"syncing"`
	Normal      int `json:"normal"`
	Failed      int `json:"failed"`
	NodesTotal  int `json:"nodes_total"`
	NodesOnline int `json:"nodes_online"`
	NodesBehind int `json:"nodes_behind"` // 配置版本落后于面板的节点数
}

// NodeSyncSummary 是单个节点的同步概况。
type NodeSyncSummary struct {
	NodeID   uint64 `json:"node_id"`
	NodeName string `json:"node_name"`
	Online   bool   `json:"online"`
	Role     string `json:"role"`
	// ConfigVersion 是节点当前生效的配置版本。
	ConfigVersion int64 `json:"config_version"`
	// Behind 标记节点配置版本落后于面板（需要重新下发）。
	Behind bool `json:"behind"`
	// RuleCount 是该节点应承载的规则数（按角色与设备组归属统计）。
	RuleCount int `json:"rule_count"`
	// LastError 是该节点最近一次同步错误原文。
	LastError string `json:"last_error,omitempty"`
	// LastSeen 是最近心跳时间。
	LastSeen string `json:"last_seen,omitempty"`
	// HealthScore 是节点健康度评分（规格书 6.1）。
	HealthScore int `json:"health_score"`
}

// RuleSyncIssue 是一条同步异常的规则。
type RuleSyncIssue struct {
	RuleID   uint64 `json:"rule_id"`
	RuleName string `json:"rule_name"`
	// Status 为 failed 或 unsynced。
	Status string `json:"status"`
	// Error 是节点返回的原始错误原文（可能为空）。
	Error string `json:"error,omitempty"`
	// UpdatedAt 是最近变更时间。
	UpdatedAt string `json:"updated_at"`
	// InboundGroupID / OutboundGroupID 便于前端直接跳转到设备组。
	InboundGroupID  uint64 `json:"inbound_group_id"`
	OutboundGroupID uint64 `json:"outbound_group_id"`
}

// SyncOverviewData 返回同步总览（规格书 11.3）。
//
// 参数 ctx 为上下文。返回总览数据与错误。
func (s *SyncService) SyncOverview(ctx context.Context) (*SyncOverview, error) {
	out := &SyncOverview{
		ConfigVersion: s.app.ConfigVersion(),
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		Nodes:         []NodeSyncSummary{},
		Rules:         []RuleSyncIssue{},
	}

	// 1. 全局规则状态计数。
	type statusCount struct {
		SyncStatus string
		N          int
	}
	var counts []statusCount
	if err := s.app.DB.WithContext(ctx).Model(&model.ForwardRule{}).
		Select("sync_status, count(*) AS n").
		Group("sync_status").Scan(&counts).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "统计规则同步状态失败")
	}
	for _, c := range counts {
		out.Totals.Rules += c.N
		switch c.SyncStatus {
		case model.SyncUnsynced:
			out.Totals.Unsynced = c.N
		case model.SyncSyncing:
			out.Totals.Syncing = c.N
		case model.SyncNormal:
			out.Totals.Normal = c.N
		case model.SyncFailed:
			out.Totals.Failed = c.N
		}
	}

	// 2. 节点概况。
	var nodes []model.Node
	if err := s.app.DB.WithContext(ctx).Order("id asc").Find(&nodes).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "查询节点失败")
	}
	// 每个节点承载的规则数：入口节点 = 以它所在入口组为入口的规则；
	// 出口节点 = 以它所在出口组为出口的规则。这里用一次分组统计替代逐节点查询。
	ruleCountByNode, err := s.ruleCountByNode(ctx, nodes)
	if err != nil {
		return nil, err
	}

	out.Totals.NodesTotal = len(nodes)
	for _, n := range nodes {
		if n.Online {
			out.Totals.NodesOnline++
		}
		behind := n.ConfigVersion < out.ConfigVersion
		if behind && n.Online {
			out.Totals.NodesBehind++
		}
		summary := NodeSyncSummary{
			NodeID:        n.ID,
			NodeName:      n.Name,
			Online:        n.Online,
			Role:          n.Role,
			ConfigVersion: n.ConfigVersion,
			Behind:        behind,
			RuleCount:     ruleCountByNode[n.ID],
			LastError:     n.LastError,
			HealthScore:   n.HealthScore,
		}
		if n.LastSeen != nil {
			summary.LastSeen = n.LastSeen.UTC().Format(time.RFC3339)
		}
		out.Nodes = append(out.Nodes, summary)
	}

	// 3. 异常规则清单（failed 优先，其次 unsynced），最多 200 条。
	var issues []model.ForwardRule
	if err := s.app.DB.WithContext(ctx).
		Where("sync_status IN ?", []string{model.SyncFailed, model.SyncUnsynced}).
		Order("updated_at desc").Limit(200).Find(&issues).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "查询异常规则失败")
	}
	// failed 排在前面，便于用户先处理真正出问题的规则。
	sortRulesBySyncSeverity(issues)
	for i := range issues {
		out.Rules = append(out.Rules, RuleSyncIssue{
			RuleID:          issues[i].ID,
			RuleName:        issues[i].Name,
			Status:          issues[i].SyncStatus,
			Error:           issues[i].SyncError,
			UpdatedAt:       issues[i].UpdatedAt.UTC().Format(time.RFC3339),
			InboundGroupID:  issues[i].InboundGroupID,
			OutboundGroupID: issues[i].OutboundGroupID,
		})
	}
	return out, nil
}

// ruleCountByNode 统计每个节点承载的规则数。
//
// 口径：
//   - 入口角色节点：其所在入口组被多少条规则引用；
//   - 出口角色节点：其所在出口组被多少条规则引用（含链式与反向）。
//
// 参数 ctx 为上下文；nodes 为节点列表。返回「节点 ID → 规则数」。
func (s *SyncService) ruleCountByNode(ctx context.Context, nodes []model.Node) (map[uint64]int, error) {
	out := make(map[uint64]int, len(nodes))
	if len(nodes) == 0 {
		return out, nil
	}

	var groupRows []model.DeviceGroup
	var rules []model.ForwardRule
	if err := s.app.DB.WithContext(ctx).Find(&groupRows).Error; err != nil {
		return nil, err
	}
	if err := s.app.DB.WithContext(ctx).Where("enable = ?", true).Find(&rules).Error; err != nil {
		return nil, err
	}
	groups := map[uint64]*model.DeviceGroup{}
	for i := range groupRows {
		groups[groupRows[i].ID] = &groupRows[i]
	}
	for _, node := range nodes {
		if node.Disabled {
			continue
		}
		for i := range rules {
			in, exit := model.RuleNodeRoles(&rules[i], node.ID, groups)
			if in || exit {
				out[node.ID]++
			}
		}
	}

	return out, nil
}

// sortRulesBySyncSeverity 把 failed 的规则排到 unsynced 之前。
//
// 用稳定排序，同一严重级别内保持数据库返回的顺序（更新时间倒序）。
func sortRulesBySyncSeverity(rules []model.ForwardRule) {
	rank := func(st string) int {
		if st == model.SyncFailed {
			return 0
		}
		return 1
	}
	for i := 1; i < len(rules); i++ {
		for j := i; j > 0 && rank(rules[j].SyncStatus) < rank(rules[j-1].SyncStatus); j-- {
			rules[j], rules[j-1] = rules[j-1], rules[j]
		}
	}
}

// RuleSyncStatus 返回单条规则的同步状态视图（规格书 8.9 的列表页展示用）。
type RuleSyncStatus struct {
	RuleID     uint64 `json:"rule_id"`
	RuleName   string `json:"rule_name"`
	SyncStatus string `json:"sync_status"`
	SyncError  string `json:"sync_error,omitempty"`
	SyncedAt   string `json:"synced_at,omitempty"`
	UpdatedAt  string `json:"updated_at"`
	// Color 是前端直接可用的状态色，避免前端各处重复写映射表。
	Color string `json:"color"`
}

// SyncStatusColor 返回同步状态对应的颜色标识（规格书 5.4 的状态机注释）。
func SyncStatusColor(status string) string {
	switch status {
	case model.SyncUnsynced:
		return "yellow"
	case model.SyncSyncing:
		return "blue"
	case model.SyncNormal:
		return "green"
	case model.SyncFailed:
		return "red"
	}
	return "gray"
}

// RuleSyncStatusOf 返回一条规则的同步状态视图。
//
// 参数 ctx 为上下文；ruleID 为规则 ID。返回状态视图或 40401。
func (s *SyncService) RuleSyncStatusOf(ctx context.Context, ruleID uint64) (*RuleSyncStatus, error) {
	var r model.ForwardRule
	if err := s.app.DB.WithContext(ctx).First(&r, ruleID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, response.New(response.CodeNotFound, fmt.Sprintf("规则 %d 不存在", ruleID))
		}
		return nil, response.Wrap(response.CodeInternal, err, "查询规则失败")
	}
	out := &RuleSyncStatus{
		RuleID:     r.ID,
		RuleName:   r.Name,
		SyncStatus: r.SyncStatus,
		SyncError:  r.SyncError,
		UpdatedAt:  r.UpdatedAt.UTC().Format(time.RFC3339),
		Color:      SyncStatusColor(r.SyncStatus),
	}
	if r.SyncedAt != nil {
		out.SyncedAt = r.SyncedAt.UTC().Format(time.RFC3339)
	}
	return out, nil
}

// PendingRulesForNode 返回需要下发给指定节点的规则 ID 列表。
//
// 这是规格书 5.4 兜底轮询（节点每 20 秒 GET /api/node/config?since=版本号）
// 的核心查询：面板据此决定下发全量还是「有变化的那些规则」。
//
// 判断口径：
//   - 节点配置版本落后于面板 → 返回全部启用规则（全量下发）；
//   - 版本一致 → 只返回处于 unsynced / failed 的启用规则（增量下发）。
//
// 参数 ctx 为上下文；nodeID 为节点 ID；since 为节点上报的版本号。
// 返回规则 ID 列表与错误。
func (s *SyncService) PendingRulesForNode(ctx context.Context, nodeID uint64, since int64) ([]uint64, error) {
	var node model.Node
	if err := s.app.DB.WithContext(ctx).First(&node, nodeID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, response.New(response.CodeNotFound, fmt.Sprintf("节点 %d 不存在", nodeID))
		}
		return nil, response.Wrap(response.CodeInternal, err, "查询节点失败")
	}

	q := s.app.DB.WithContext(ctx).Model(&model.ForwardRule{}).Where("enable = ?", true)
	if since >= s.app.ConfigVersion() {
		// 版本一致：只推有问题的。
		q = q.Where("sync_status IN ?", []string{model.SyncUnsynced, model.SyncFailed})
	}
	var ids []uint64
	if err := q.Pluck("id", &ids).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "查询待下发规则失败")
	}
	if ids == nil {
		ids = []uint64{}
	}
	return ids, nil
}
