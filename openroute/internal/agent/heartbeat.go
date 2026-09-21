package agent

import (
	"context"
	"encoding/json"
	"time"

	"go.uber.org/zap"

	"github.com/openroute/openroute/internal/app"
	"github.com/openroute/openroute/internal/model"
)

// 本文件承载心跳相关的辅助逻辑。
//
// 入口 handler 在 register.go（HeartbeatHandler），这里放与心跳副作用
// 相关的独立编排：离线扫描、在线率统计与指标持久化策略。

// OfflineScanner 是离线判定的定时器载体。
//
// 规格书 3.1 要求「超过 offline-node-time 秒未上报视为离线」。
// 面板不依赖节点主动告知离线，而是由本扫描器周期性把超时的节点置为离线，
// 因此节点进程被杀、机器断电这类场景也能被正确识别。
type OfflineScanner struct {
	app *app.App
}

// NewOfflineScanner 构造离线扫描器。
//
// 参数 a 为运行时依赖容器；返回可直接启动的 *OfflineScanner。
func NewOfflineScanner(a *app.App) *OfflineScanner {
	return &OfflineScanner{app: a}
}

// Run 启动离线扫描循环。
//
// 参数 ctx 为生命周期上下文；扫描间隔取 max(offline-node-time/2, 5) 秒，
// 保证最坏情况下节点状态抖动不超过一个离线阈值的一半。
// 本方法阻塞直到 ctx 结束，调用方应当放在独立 goroutine 中。
func (s *OfflineScanner) Run(ctx context.Context) {
	interval := s.scanInterval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	s.app.Log.Info("节点离线扫描已启动", zap.Duration("interval", interval))

	for {
		select {
		case <-ctx.Done():
			s.app.Log.Info("节点离线扫描已停止")
			return
		case <-s.app.Done():
			s.app.Log.Info("节点离线扫描已停止")
			return
		case <-ticker.C:
			s.scanOnce(ctx)
		}
	}
}

// scanInterval 计算扫描间隔。
func (s *OfflineScanner) scanInterval() time.Duration {
	offline := s.app.Config.OfflineNodeTime
	if offline <= 0 {
		offline = 20
	}
	sec := offline / 2
	if sec < 5 {
		sec = 5
	}
	return time.Duration(sec) * time.Second
}

// scanOnce 执行一次离线扫描。
func (s *OfflineScanner) scanOnce(ctx context.Context) {
	count, err := s.app.Node.MarkOfflineStale(ctx)
	if err != nil {
		s.app.Log.Warn("离线扫描失败", zap.Error(err))
		return
	}
	if count > 0 {
		s.app.Log.Info("已把超时节点标记为离线", zap.Int("count", count))
	}
	// 顺带刷新健康度：离线状态变化会直接影响评分。
	if _, err := s.app.Node.RefreshHealthScores(ctx); err != nil {
		s.app.Log.Debug("刷新节点健康度失败", zap.Error(err))
	}
}

// OnlineRateOf 统计节点在给定时间窗口内的在线率。
//
// 规格书 6.1 的健康度评分需要「在线率」，面板没有专门的心跳历史表，
// 因此用 probe_metrics 的采样密度近似：窗口内应到样本数 = 窗口秒数 / 心跳间隔，
// 实到样本数 = probe_metrics 行数，比值即在线率（0~1）。
//
// 参数 ctx 为上下文；nodeID 为节点；window 为统计窗口。
// 返回 0~1 的在线率与错误。
func (s *OfflineScanner) OnlineRateOf(ctx context.Context, nodeID uint64, window time.Duration) (float64, error) {
	interval := s.app.Config.HeartbeatInterval
	if interval <= 0 {
		interval = HeartbeatIntervalDefault
	}
	expected := window.Seconds() / float64(interval)
	if expected <= 0 {
		return 1, nil
	}

	since := time.Now().UTC().Add(-window).Unix()
	var actual int64
	err := s.app.DB.WithContext(ctx).Model(&model.ProbeMetric{}).
		Where("node_id = ? AND timestamp >= ?", nodeID, since).
		Count(&actual).Error
	if err != nil {
		return 0, err
	}

	rate := float64(actual) / expected
	if rate > 1 {
		rate = 1
	}
	if rate < 0 {
		rate = 0
	}
	return rate, nil
}

// jsonUnmarshal 是 json.Unmarshal 的薄封装，避免在各文件中重复导入。
func jsonUnmarshal(s string, v interface{}) error {
	return json.Unmarshal([]byte(s), v)
}
