package agent

import (
	"context"
	"github.com/openroute/openroute/internal/app"
	"github.com/openroute/openroute/internal/model"
	"github.com/openroute/openroute/internal/nodeproto"
	"time"
)

func (r *Registry) checkConfigDrift(ctx context.Context, node *model.Node, req HeartbeatRequest) bool {
	cfg, err := r.cfg.BuildFull(ctx, node)
	if err != nil {
		return false
	}
	expected := nodeproto.ConfigHash(*cfg)
	report := &app.DriftReport{NodeID: node.ID, Name: node.Name, CheckedAt: time.Now().UTC(), Items: []app.DriftItem{}}
	if expected != req.ConfigHash {
		report.Items = append(report.Items, app.DriftItem{Kind: "config_mismatch", Expected: expected, Actual: req.ConfigHash, Message: "节点实际配置与面板配置不同（含目标、协议、限制与关联设备组）"})
	}
	for _, running := range req.RunningRules {
		if running.Status == nodeproto.RuleStatusError {
			report.Items = append(report.Items, app.DriftItem{Kind: "status_mismatch", RuleID: running.RuleID, Expected: "running", Actual: running.Status, Message: "节点报告规则运行错误"})
		}
	}
	report.Drifted = len(report.Items) != 0
	_ = r.app.Node.SaveDrift(ctx, report)
	return report.Drifted
}
