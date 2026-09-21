package app

import (
	"context"

	"go.uber.org/zap"
)

// NotifyEvent 向用户配置的 Webhook 地址推送一个事件（规格书 8.18）。
//
// 这是 job 包与 handler 层统一的事件出口：调用方只需给出事件名与数据，
// 由本方法负责按事件名选择对应的发送器。
//
// 事件名取值（与规格书 8.18 的表格一致）：
//
//	node.online          node.offline
//	rule.sync_failed     rule.sync_ok
//	alert.fired          alert.resolved
//	migration.finished   backup.finished
//
// 发送是异步的（内部走带重试的队列），因此本方法不会阻塞调用方，
// 也不会因为 Webhook 地址不可达而影响主流程。
//
// 若用户未配置 Webhook 地址，则静默跳过。
func (s *AlertService) NotifyEvent(ctx context.Context, event string, data map[string]interface{}) {
	if s.webhook == nil {
		return
	}
	// 未配置地址时不发送，避免产生无意义的空请求。
	if s.webhook.SettingWebhookURL() == "" {
		return
	}
	if data == nil {
		data = map[string]interface{}{}
	}

	switch event {
	case EventNodeOnline:
		s.webhook.SendNodeOnline(data)
	case EventNodeOffline:
		s.webhook.SendNodeOffline(data)
	case EventRuleSyncFailed:
		s.webhook.SendRuleSyncFailed(data)
	case EventRuleSyncOK:
		s.webhook.SendRuleSyncOK(data)
	case EventAlertFired:
		s.webhook.SendAlertFired(data)
	case EventAlertResolved:
		s.webhook.SendAlertResolved(data)
	case EventMigrationFinished:
		s.webhook.SendMigrationFinished(data)
	case EventBackupFinished:
		s.webhook.SendBackupFinished(data)
	default:
		s.app.Log.Warn("未知的 Webhook 事件类型，已跳过", zap.String("event", event))
	}
	_ = ctx
}

// ComputeDrift 计算某个节点的配置漂移，返回是否漂移与差异详情。
//
// 供 job 包的定时漂移检测使用（规格书 6.14）。
//
// 实现说明：节点上报的实际运行摘要（running_rules）在心跳时被写入
// 节点的 DriftDetail 字段作为「上一次实际状态」的缓存，
// 因此本方法从该缓存还原实际状态，再与面板的期望配置比对。
//
// 当节点尚未上报过任何运行摘要时，视为「无漂移」——
// 否则刚添加、还没上线过的节点会立刻被标记为漂移，产生噪音。
//
// 返回 drift 表示是否存在差异；detail 为差异详情（无差异时为空 map）。
func (s *NodeService) ComputeDrift(ctx context.Context, nodeID uint64) (bool, map[string]interface{}) {
	report, err := s.GetDrift(ctx, nodeID)
	if err != nil {
		// 没有缓存的漂移记录：说明还没拿到过节点的实际状态，不做判定。
		return false, map[string]interface{}{}
	}
	if report == nil {
		return false, map[string]interface{}{}
	}

	if !report.Drifted {
		return false, map[string]interface{}{}
	}

	// 把结构化差异转成前端易展示的 map。
	// DriftItem.Kind 取值：rule_missing / rule_extra / port_mismatch / status_mismatch。
	items := make([]map[string]interface{}, 0, len(report.Items))
	for _, it := range report.Items {
		items = append(items, map[string]interface{}{
			"kind":     it.Kind,
			"rule_id":  it.RuleID,
			"expected": it.Expected,
			"actual":   it.Actual,
			"message":  it.Message,
		})
	}

	return true, map[string]interface{}{
		"node_id":    report.NodeID,
		"node_name":  report.Name,
		"checked_at": report.CheckedAt,
		"items":      items,
	}
}

// RecordDriftFromHeartbeat 把节点心跳上报的运行规则摘要转成漂移报告并落库。
//
// 由 agent 包的心跳处理调用：拿到 running_rules 后立刻比对，
// 这样节点卡片上的「配置漂移」标记能在一次心跳内更新，而不是等定时任务。
//
// 参数 ctx 为上下文；nodeID 为节点；running 为节点上报的运行中规则摘要。
// 返回比对结果与错误。
func (s *NodeService) RecordDriftFromHeartbeat(ctx context.Context, nodeID uint64, running []RunningRuleSummary) (*DriftReport, error) {
	report, err := s.DetectDrift(ctx, nodeID, running)
	if err != nil {
		return nil, err
	}
	if err := s.SaveDrift(ctx, report); err != nil {
		return nil, err
	}
	return report, nil
}
