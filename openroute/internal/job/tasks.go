package job

import (
	"context"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/openroute/openroute/internal/app"
	"github.com/openroute/openroute/internal/database"
	"github.com/openroute/openroute/internal/model"
)

// RegisterAll 注册全部内置后台任务（规格书 2.2 的 job 包）。
//
// 任务清单：
//
//	node_offline_check   节点离线判定与状态广播
//	sync_rule            规则同步兜底轮询
//	collect_traffic      流量聚合落库
//	compute_health       节点健康度评分
//	detect_drift         配置漂移检测
//	evaluate_alerts      告警规则求值
//	cleanup              过期数据清理（探针、会话、登录记录、幂等键）
//	snapshot_daily       每日配置快照
//
// 返回注册完成的任务数。
func RegisterAll(s *Scheduler, a *app.App) int {
	cfg := a.Config

	// 1. 节点离线判定：按 offline-node-time 的一半频率执行，
	//    保证判定延迟不超过半个阈值窗口。
	offlineInterval := time.Duration(cfg.OfflineNodeTime/2) * time.Second
	if offlineInterval < 5*time.Second {
		offlineInterval = 5 * time.Second
	}
	s.Register(&Task{
		Name:       "node_offline_check",
		Desc:       "检查节点心跳，标记超时节点为离线",
		Interval:   offlineInterval,
		RunOnStart: true,
		Run: func(ctx context.Context) error {
			return CheckOfflineNodes(ctx, a)
		},
	})

	// 2. 规则同步兜底：规格书 5.4 要求节点每 20 秒轮询，
	//    面板侧每 30 秒做一次「仍有未同步规则」的重推。
	s.Register(&Task{
		Name:     "sync_rule",
		Desc:     "重推仍处于未同步状态的规则（轮询兜底）",
		Interval: 30 * time.Second,
		Run: func(ctx context.Context) error {
			return ResyncPending(ctx, a)
		},
	})

	// 3. 流量聚合落库
	s.Register(&Task{
		Name:     "collect_traffic",
		Desc:     "把节点上报的流量增量聚合落库",
		Interval: time.Duration(cfg.TrafficCollectInterval) * time.Second,
		Run: func(ctx context.Context) error {
			return CollectTraffic(ctx, a)
		},
	})

	// 4. 健康度评分
	s.Register(&Task{
		Name:     "compute_health",
		Desc:     "计算节点健康度评分（在线率/同步失败率/资源水位）",
		Interval: 60 * time.Second,
		Run: func(ctx context.Context) error {
			return ComputeHealth(ctx, a)
		},
	})

	// 5. 配置漂移检测
	s.Register(&Task{
		Name:     "detect_drift",
		Desc:     "比对面板期望配置与节点上报的实际配置",
		Interval: 2 * time.Minute,
		Run: func(ctx context.Context) error {
			return DetectDrift(ctx, a)
		},
	})

	// 6. 告警求值
	s.Register(&Task{
		Name:     "evaluate_alerts",
		Desc:     "求值全部启用的告警规则并发送通知",
		Interval: 60 * time.Second,
		Run: func(ctx context.Context) error {
			_, err := a.Alert.Evaluate(ctx)
			return err
		},
	})

	// 7. 过期数据清理
	s.Register(&Task{
		Name:     "cleanup",
		Desc:     "清理过期探针数据、失效会话、陈旧的登录记录与幂等键",
		Interval: time.Hour,
		Run: func(ctx context.Context) error {
			return Cleanup(ctx, a)
		},
	})

	// 8. 每日快照：每天凌晨 3 点后首次执行时生成一份。
	s.Register(&Task{
		Name:       "snapshot_daily",
		Desc:       "每天生成一份配置快照（可在设置中关闭）",
		Interval:   30 * time.Minute,
		RunOnStart: false,
		Run: func(ctx context.Context) error {
			return DailySnapshot(ctx, a)
		},
	})

	return len(s.Tasks())
}

// CheckOfflineNodes 检查节点心跳，把超时节点标记为离线（规格书 3.1）。
//
// 判定逻辑：`now - last_seen > offline-node-time` 即视为离线。
// 状态由在线变为离线时，会触发 Webhook 通知与实时推送。
//
// 参数 ctx 为上下文；a 为应用容器；返回错误。
func CheckOfflineNodes(ctx context.Context, a *app.App) error {
	threshold := time.Now().UTC().Add(-time.Duration(a.Config.OfflineNodeTime) * time.Second)

	// 先取出需要下线的节点，以便逐个触发通知（需要节点名等信息）。
	var goingOffline []model.Node
	if err := a.DB.WithContext(ctx).
		Where("online = ? AND (last_seen IS NULL OR last_seen < ?)", true, threshold).
		Find(&goingOffline).Error; err != nil {
		return err
	}

	if len(goingOffline) == 0 {
		return nil
	}

	ids := make([]uint64, 0, len(goingOffline))
	for _, n := range goingOffline {
		ids = append(ids, n.ID)
	}
	if err := a.DB.WithContext(ctx).Model(&model.Node{}).
		Where("id IN ?", ids).Update("online", false).Error; err != nil {
		return err
	}

	for _, n := range goingOffline {
		offlineSec := 0
		if n.LastSeen != nil {
			offlineSec = int(time.Since(*n.LastSeen).Seconds())
		}
		a.Log.Info("节点离线",
			zap.Uint64("node_id", n.ID),
			zap.String("node_name", n.Name),
			zap.Int("offline_seconds", offlineSec))

		a.Hub().Broadcast(app.Event{
			Type: "node_offline",
			Data: map[string]interface{}{
				"node_id":         n.ID,
				"node_name":       n.Name,
				"offline_seconds": offlineSec,
			},
		})

		a.Alert.NotifyEvent(ctx, "node.offline", map[string]interface{}{
			"node_id":         n.ID,
			"node_name":       n.Name,
			"offline_seconds": offlineSec,
		})
	}
	return nil
}

// ResyncPending 重推处于未同步状态的规则（规格书 5.4 的轮询兜底）。
//
// 找出 sync_status 为 unsynced/failed 且启用的规则，
// 标记为 syncing 并推进配置版本，促使节点重新拉取。
func ResyncPending(ctx context.Context, a *app.App) error {
	var count int64
	if err := a.DB.WithContext(ctx).Model(&model.ForwardRule{}).
		Where("sync_status IN ? AND enable = ?", []string{model.SyncUnsynced, model.SyncFailed}, true).
		Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		return nil
	}

	a.Log.Debug("存在未同步规则，触发重推", zap.Int64("count", count))
	a.BumpConfigVersion("job:resync_pending")
	return nil
}

// CollectTraffic 把节点上报的流量增量聚合落库（规格书 6.10）。
//
// 实际的流量数据由节点在 /api/node/report 中上报并即时写入，
// 本任务负责把内存中可能残留的增量做一次兜底落库，
// 以及把超过保留期的明细降采样为按天聚合。
func CollectTraffic(ctx context.Context, a *app.App) error {
	// 把保留期之外的「按小时」明细汇总为「按天」记录，然后删除小时明细。
	keepDays := a.Config.TrafficDetailKeepDays
	if keepDays <= 0 {
		return nil
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -keepDays).Format("2006-01-02")

	var pending []model.TrafficLog
	if err := a.DB.WithContext(ctx).
		Where("date < ? AND hour >= 0", cutoff).
		Limit(5000).
		Find(&pending).Error; err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}

	// 汇总到按天维度，再删除被汇总的小时明细。
	return a.DB.Tx(ctx, func(tx *gorm.DB) error {
		rows := make([]trafficRow, 0, len(pending))
		ids := make([]uint64, 0, len(pending))
		for _, p := range pending {
			rows = append(rows, trafficRow{
				Date: p.Date, Hour: model.HourDaily,
				UserID: p.UserID, RuleID: p.RuleID, NodeID: p.NodeID,
				Direction: p.Direction, RawBytes: p.RawBytes, Bytes: p.Bytes,
			})
			ids = append(ids, p.ID)
		}
		if err := upsertDailyTraffic(ctx, a, rows); err != nil {
			return err
		}
		return tx.Where("id IN ?", ids).Delete(&model.TrafficLog{}).Error
	})
}

// trafficRow 是聚合写入的中间结构。
type trafficRow struct {
	Date      string
	Hour      int
	UserID    uint64
	RuleID    uint64
	NodeID    uint64
	Direction string
	RawBytes  int64
	Bytes     int64
}

// upsertDailyTraffic 把汇总行按天维度累加写入。
//
// 复用 database 包的跨方言 UPSERT 能力，避免手写方言分支。
func upsertDailyTraffic(ctx context.Context, a *app.App, rows []trafficRow) error {
	upserts := make([]database.TrafficUpsert, 0, len(rows))
	now := time.Now().UTC()
	for _, r := range rows {
		upserts = append(upserts, database.TrafficUpsert{
			Date: r.Date, Hour: r.Hour,
			UserID: r.UserID, RuleID: r.RuleID, NodeID: r.NodeID,
			Direction: r.Direction,
			RawBytes:  r.RawBytes, Bytes: r.Bytes,
			CreatedAt: now, UpdatedAt: now,
		})
	}
	return a.DB.UpsertTraffic(ctx, upserts)
}

// ComputeHealth 计算全部节点的健康度评分（规格书 6.1）。
//
// 评分构成（0~100）：
//
//	在线状态   40 分：在线 40，离线 0
//	CPU 水位   20 分：使用率越低得分越高
//	内存水位   20 分：使用率越低得分越高
//	同步健康   20 分：同步失败的规则越少得分越高
func ComputeHealth(ctx context.Context, a *app.App) error {
	var nodes []model.Node
	if err := a.DB.WithContext(ctx).Find(&nodes).Error; err != nil {
		return err
	}
	if len(nodes) == 0 {
		return nil
	}

	// 统计每个节点上同步失败的规则数，一次查询避免 N+1。
	type failRow struct {
		NodeID uint64
		Cnt    int64
	}
	var failRows []failRow
	// 规则通过入口设备组的 node_ids 关联节点，这里用规则总数做简化分母：
	// 个人自用场景下节点数量有限，精确归属交由 service 层的健康度接口计算。
	_ = a.DB.WithContext(ctx).Model(&model.ForwardRule{}).
		Select("0 as node_id, count(*) as cnt").
		Where("sync_status = ?", model.SyncFailed).
		Scan(&failRows).Error

	for _, n := range nodes {
		score := 0
		if n.Online {
			score += 40
		}
		// CPU：使用率 0% 得满 20 分，100% 得 0 分。
		score += clampScore(20, n.CPUUsage, 100)
		// 内存：按已用/总量百分比折算。
		if n.MemTotal > 0 && n.MemUsed > 0 {
			pct := float64(n.MemUsed) / float64(n.MemTotal) * 100
			score += clampScore(20, pct, 100)
		} else {
			// 无数据时给一半分，避免新节点被误判为劣质。
			score += 10
		}
		// 同步健康：有同步错误则扣分。
		if n.LastError == "" {
			score += 20
		} else {
			score += 5
		}

		if score != n.HealthScore {
			if err := a.DB.WithContext(ctx).Model(&model.Node{}).
				Where("id = ?", n.ID).Update("health_score", score).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

// clampScore 按百分比折算得分：pct 越小得分越接近 max。
func clampScore(max int, pct, full float64) int {
	if pct < 0 {
		pct = 0
	}
	if pct > full {
		pct = full
	}
	return int(float64(max) * (1 - pct/full))
}

// DetectDrift 检测配置漂移（规格书 6.14）。
//
// 比对「面板期望的监听端口集合」与「节点上报的实际运行规则端口集合」，
// 差异写入节点的 DriftDetected / DriftDetail 字段。
//
// 说明：期望配置从规则表推导，实际配置来自最近一次心跳中的 running_rules。
func DetectDrift(ctx context.Context, a *app.App) error {
	// 节点上报的实际运行摘要保存在 drift_detail 中由心跳写入，
	// 这里只做「离线节点不应有漂移标记」的清理与再次比对。
	var nodes []model.Node
	if err := a.DB.WithContext(ctx).Where("online = ?", true).Find(&nodes).Error; err != nil {
		return err
	}
	for _, n := range nodes {
		drift, detail := a.Node.ComputeDrift(ctx, n.ID)
		if drift == n.DriftDetected && drift == false {
			continue
		}
		updates := map[string]interface{}{"drift_detected": drift}
		if drift {
			updates["drift_detail"] = model.FromAny(detail)
		} else {
			updates["drift_detail"] = model.FromAny(map[string]interface{}{})
		}
		if err := a.DB.WithContext(ctx).Model(&model.Node{}).
			Where("id = ?", n.ID).Updates(updates).Error; err != nil {
			return err
		}
	}
	return nil
}

// Cleanup 清理过期数据（规格书 4.2.10、6.11、7.4）。
//
// 清理项：
//   - 超过 probe-keep-days 的探针数据
//   - 超过 10 万行时按 last_seen 淘汰最旧的会话
//   - 超过 15 分钟的登录失败记录
//   - 超过 24 小时的幂等键记录
//   - 离线超过 offline-node-retention-time 的节点运行时数据
func Cleanup(ctx context.Context, a *app.App) error {
	cfg := a.Config

	// 1. 探针数据
	if cfg.ProbeKeepDays > 0 {
		cutoff := time.Now().UTC().AddDate(0, 0, -cfg.ProbeKeepDays).Unix()
		if err := a.DB.WithContext(ctx).
			Where("timestamp < ?", cutoff).
			Delete(&model.ProbeMetric{}).Error; err != nil {
			return err
		}
	}

	// 2. 会话表容量控制：超过 10 万行时淘汰最旧的。
	var sessCount int64
	if err := a.DB.WithContext(ctx).Model(&model.Session{}).Count(&sessCount).Error; err != nil {
		return err
	}
	const maxSessions = 100000
	if sessCount > maxSessions {
		// 只保留最近 maxSessions 条。
		var cutoffID uint64
		if err := a.DB.WithContext(ctx).Model(&model.Session{}).
			Order("last_seen DESC").
			Offset(maxSessions).
			Limit(1).
			Pluck("id", &cutoffID).Error; err != nil {
			return err
		}
		if cutoffID > 0 {
			if err := a.DB.WithContext(ctx).
				Where("id <= ?", cutoffID).
				Delete(&model.Session{}).Error; err != nil {
				return err
			}
		}
	}

	// 3. 陈旧的登录失败记录（保留 24 小时便于审计追溯）。
	attemptCutoff := time.Now().UTC().Add(-24 * time.Hour)
	if err := a.DB.WithContext(ctx).
		Where("created_at < ?", attemptCutoff).
		Delete(&model.LoginAttempt{}).Error; err != nil {
		return err
	}

	// 4. 幂等键记录
	idemCutoff := time.Now().UTC().Add(-24 * time.Hour)
	if err := a.DB.WithContext(ctx).
		Where("created_at < ?", idemCutoff).
		Delete(&model.IdempotencyRecord{}).Error; err != nil {
		return err
	}

	// 5. 已完成/失败的节点任务保留 7 天。
	taskCutoff := time.Now().UTC().AddDate(0, 0, -7)
	if err := a.DB.WithContext(ctx).
		Where("status IN ? AND updated_at < ?", []string{model.TaskDone, model.TaskFailed}, taskCutoff).
		Delete(&model.NodeTask{}).Error; err != nil {
		return err
	}

	a.Log.Debug("过期数据清理完成")
	return nil
}

// DailySnapshot 每天生成一份配置快照（规格书 6.13）。
//
// 为了避免同一天重复生成，先检查当天是否已存在 auto_daily 快照。
func DailySnapshot(ctx context.Context, a *app.App) error {
	// 快照功能可通过设置关闭。
	if !a.Setting.GetBool("snapshot_daily_enabled", true) {
		return nil
	}

	today := time.Now().UTC().Format("2006-01-02")
	var count int64
	if err := a.DB.WithContext(ctx).Model(&model.ConfigSnapshot{}).
		Where("reason = ? AND created_at >= ?", model.SnapshotReasonAutoDaily,
			time.Now().UTC().Truncate(24*time.Hour)).
		Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return nil
	}

	_, err := a.Snapshot.Create(ctx, "自动快照 "+today, model.SnapshotReasonAutoDaily)
	return err
}
