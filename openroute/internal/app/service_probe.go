package app

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/openroute/openroute/internal/api/response"
	"github.com/openroute/openroute/internal/model"
)

// ProbeService 负责探针指标的采集落库、降采样查询与保留策略（规格书 6.11）。
//
// 数据来源：节点心跳上报的 metrics 子对象（见 internal/agent/heartbeat.go）。
// 查询策略：面板不缓存指标，全部交给 SQL 聚合，避免内存占用随节点数增长。
type ProbeService struct {
	app *App
}

// NewProbeService 构造探针服务。
//
// 参数 a 为运行时依赖容器；返回可直接使用的 *ProbeService。
func NewProbeService(a *App) *ProbeService {
	return &ProbeService{app: a}
}

// 降采样粒度（规格书 6.11：1 小时 / 1 天 / 7 天自动降采样）。
const (
	// IntervalAuto 由时间范围自动选择粒度。
	IntervalAuto = "auto"
	// IntervalRaw 原始粒度（不降采样）。
	IntervalRaw = "raw"
	// Interval5m 5 分钟粒度，用于节点详情页实时曲线。
	Interval5m = "5m"
	// Interval1h 1 小时粒度。
	Interval1h = "1h"
	// Interval1d 1 天粒度。
	Interval1d = "1d"
	// Interval7d 7 天粒度。
	Interval7d = "7d"
)

// BucketSeconds 把粒度名称换算成秒数；未知粒度返回 0。
func BucketSeconds(interval string) int64 {
	switch interval {
	case Interval5m:
		return 300
	case Interval1h:
		return 3600
	case Interval1d:
		return 86400
	case Interval7d:
		return 7 * 86400
	}
	return 0
}

// autoBucketSeconds 按查询跨度自动选择桶大小（规格书 6.11）。
//
//   - 跨度 ≤ 6 小时：5 分钟
//   - 跨度 ≤ 3 天：1 小时
//   - 跨度 ≤ 30 天：1 天
//   - 更长：7 天
func autoBucketSeconds(from, to time.Time) int64 {
	span := to.Sub(from)
	switch {
	case span <= 6*time.Hour:
		return 300
	case span <= 72*time.Hour:
		return 3600
	case span <= 30*24*time.Hour:
		return 86400
	default:
		return 7 * 86400
	}
}

// ProbePoint 是一个降采样后的指标点。
type ProbePoint struct {
	// Timestamp 桶起始时间（Unix 秒）。
	Timestamp int64 `json:"timestamp"`
	// Time 桶起始时间的 RFC3339 文本，便于前端直接展示。
	Time string `json:"time"`

	CPU       float64 `json:"cpu"`
	MemUsed   int64   `json:"mem_used"`
	MemTotal  int64   `json:"mem_total"`
	SwapUsed  int64   `json:"swap_used"`
	SwapTotal int64   `json:"swap_total"`
	DiskUsed  int64   `json:"disk_used"`
	DiskTotal int64   `json:"disk_total"`

	NetIn       int64 `json:"net_in"`
	NetOut      int64 `json:"net_out"`
	NetInSpeed  int64 `json:"net_in_speed"`
	NetOutSpeed int64 `json:"net_out_speed"`

	Load1  float64 `json:"load1"`
	Load5  float64 `json:"load5"`
	Load15 float64 `json:"load15"`

	TcpConn int   `json:"tcp_conn"`
	UdpConn int   `json:"udp_conn"`
	Uptime  int64 `json:"uptime"`

	// Samples 该桶内参与聚合的样本数。
	Samples int64 `json:"samples"`
}

// probeAggRow 是 SQL 聚合结果的扫描目标。
//
// 计数类字段用 MAX（瞬时值取最新/峰值更有意义），
// 水位类字段用 AVG（曲线平滑），速率类用 AVG + MAX 组合。
type probeAggRow struct {
	Bucket         int64   `gorm:"column:bucket"`
	CPUMax         float64 `gorm:"column:cpu_max"`
	CPUAvg         float64 `gorm:"column:cpu_avg"`
	MemUsedMax     int64   `gorm:"column:mem_used_max"`
	MemTotalMax    int64   `gorm:"column:mem_total_max"`
	SwapUsedMax    int64   `gorm:"column:swap_used_max"`
	SwapTotalMax   int64   `gorm:"column:swap_total_max"`
	DiskUsedMax    int64   `gorm:"column:disk_used_max"`
	DiskTotalMax   int64   `gorm:"column:disk_total_max"`
	NetInMax       int64   `gorm:"column:net_in_max"`
	NetOutMax      int64   `gorm:"column:net_out_max"`
	NetInSpeedAvg  int64   `gorm:"column:net_in_speed_avg"`
	NetOutSpeedAvg int64   `gorm:"column:net_out_speed_avg"`
	Load1Avg       float64 `gorm:"column:load1_avg"`
	Load5Avg       float64 `gorm:"column:load5_avg"`
	Load15Avg      float64 `gorm:"column:load15_avg"`
	TcpMax         int     `gorm:"column:tcp_max"`
	UdpMax         int     `gorm:"column:udp_max"`
	UptimeMax      int64   `gorm:"column:uptime_max"`
	Samples        int64   `gorm:"column:samples"`
}

// RecordMetrics 写入一条探针指标（规格书 6.11）。
//
// 参数 ctx 为上下文；nodeID 为节点 ID；m 为指标。
// Timestamp 为 0 时自动填充当前时间；节点 ID 为 0 时返回参数错误。
// 返回错误。
func (s *ProbeService) RecordMetrics(ctx context.Context, nodeID uint64, m model.ProbeMetric) error {
	if nodeID == 0 {
		return response.Field(response.CodeParamInvalid, "node_id", nodeID, "节点 ID 不能为空")
	}
	if !s.app.Config.EnableProbe {
		// 探针关闭时静默丢弃，不写库也不报错。
		return nil
	}
	m.ID = 0
	m.NodeID = nodeID
	if m.Timestamp <= 0 {
		m.Timestamp = time.Now().UTC().Unix()
	}
	m.CreatedAt = time.Now().UTC()

	if err := s.app.DB.WithContext(ctx).Create(&m).Error; err != nil {
		if isDuplicateError(err) {
			return nil
		}
		return response.Wrap(response.CodeInternal, err, "写入探针指标失败")
	}
	return nil
}

// Metrics 查询指定节点的降采样指标序列（规格书 6.11）。
//
// 参数 ctx 为上下文；nodeID 为节点 ID；from / to 为时间范围（零值表示
// 最近 1 小时 / 当前）；interval 为粒度，auto 表示按跨度自动选择。
//
// 聚合在 SQL 中完成：桶表达式用「整数除法」把 Unix 时间戳对齐到桶边界，
//
//	bucket = timestamp / bucketSeconds * bucketSeconds
//
// 整数除法在 SQLite / MySQL / PostgreSQL 三家的语义完全一致，
// 因此不需要任何方言分支。
//
// 返回按时间升序的点列表。
func (s *ProbeService) Metrics(ctx context.Context, nodeID uint64, from, to time.Time, interval string) ([]ProbePoint, error) {
	if nodeID == 0 {
		return nil, response.Field(response.CodeParamInvalid, "node_id", nodeID, "节点 ID 不能为空")
	}
	now := time.Now().UTC()
	if to.IsZero() {
		to = now
	}
	if from.IsZero() {
		from = to.Add(-time.Hour)
	}
	if !from.Before(to) {
		return nil, response.Field(response.CodeParamInvalid, "from", from.Format(time.RFC3339),
			"起始时间必须早于结束时间")
	}

	var bucket int64
	raw := false
	if interval == IntervalAuto || interval == "" {
		bucket = autoBucketSeconds(from, to)
	} else if interval == IntervalRaw {
		raw = true
	} else {
		bucket = BucketSeconds(interval)
		if bucket == 0 {
			return nil, response.Field(response.CodeParamInvalid, "interval", interval,
				"粒度只能是 auto / raw / 5m / 1h / 1d / 7d")
		}
	}

	fromTS := from.Unix()
	toTS := to.Unix()

	if raw {
		return s.rawMetrics(ctx, nodeID, fromTS, toTS)
	}

	rows := make([]probeAggRow, 0)
	sql := fmt.Sprintf(`
SELECT
  (timestamp / %d) * %d                                   AS bucket,
  MAX(cpu)                                                AS cpu_max,
  AVG(cpu)                                                AS cpu_avg,
  MAX(mem_used)                                           AS mem_used_max,
  MAX(mem_total)                                          AS mem_total_max,
  MAX(swap_used)                                          AS swap_used_max,
  MAX(swap_total)                                         AS swap_total_max,
  MAX(disk_used)                                          AS disk_used_max,
  MAX(disk_total)                                         AS disk_total_max,
  MAX(net_in)                                             AS net_in_max,
  MAX(net_out)                                            AS net_out_max,
  CAST(AVG(net_in_speed) AS INTEGER)                      AS net_in_speed_avg,
  CAST(AVG(net_out_speed) AS INTEGER)                     AS net_out_speed_avg,
  AVG(load1)                                              AS load1_avg,
  AVG(load5)                                              AS load5_avg,
  AVG(load15)                                             AS load15_avg,
  MAX(tcp_conn)                                           AS tcp_max,
  MAX(udp_conn)                                           AS udp_max,
  MAX(uptime)                                             AS uptime_max,
  COUNT(*)                                                AS samples
FROM probe_metrics
WHERE node_id = ? AND timestamp >= ? AND timestamp <= ?
GROUP BY bucket
ORDER BY bucket ASC`, bucket, bucket)

	if err := s.app.DB.WithContext(ctx).Raw(sql, nodeID, fromTS, toTS).Scan(&rows).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "查询探针指标失败")
	}

	points := make([]ProbePoint, 0, len(rows))
	for _, r := range rows {
		points = append(points, ProbePoint{
			Timestamp:   r.Bucket,
			Time:        time.Unix(r.Bucket, 0).UTC().Format(time.RFC3339),
			CPU:         round2(r.CPUMax),
			MemUsed:     r.MemUsedMax,
			MemTotal:    r.MemTotalMax,
			SwapUsed:    r.SwapUsedMax,
			SwapTotal:   r.SwapTotalMax,
			DiskUsed:    r.DiskUsedMax,
			DiskTotal:   r.DiskTotalMax,
			NetIn:       r.NetInMax,
			NetOut:      r.NetOutMax,
			NetInSpeed:  r.NetInSpeedAvg,
			NetOutSpeed: r.NetOutSpeedAvg,
			Load1:       round2(r.Load1Avg),
			Load5:       round2(r.Load5Avg),
			Load15:      round2(r.Load15Avg),
			TcpConn:     r.TcpMax,
			UdpConn:     r.UdpMax,
			Uptime:      r.UptimeMax,
			Samples:     r.Samples,
		})
	}
	return points, nil
}

// rawMetrics 返回未降采样的原始点，最多 2000 条。
func (s *ProbeService) rawMetrics(ctx context.Context, nodeID uint64, fromTS, toTS int64) ([]ProbePoint, error) {
	rows := make([]model.ProbeMetric, 0)
	err := s.app.DB.WithContext(ctx).
		Where("node_id = ? AND timestamp >= ? AND timestamp <= ?", nodeID, fromTS, toTS).
		Order("timestamp ASC").Limit(2000).Find(&rows).Error
	if err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "查询原始探针指标失败")
	}
	points := make([]ProbePoint, 0, len(rows))
	for i := range rows {
		r := rows[i]
		points = append(points, ProbePoint{
			Timestamp:   r.Timestamp,
			Time:        time.Unix(r.Timestamp, 0).UTC().Format(time.RFC3339),
			CPU:         round2(r.CPU),
			MemUsed:     r.MemUsed,
			MemTotal:    r.MemTotal,
			SwapUsed:    r.SwapUsed,
			SwapTotal:   r.SwapTotal,
			DiskUsed:    r.DiskUsed,
			DiskTotal:   r.DiskTotal,
			NetIn:       r.NetIn,
			NetOut:      r.NetOut,
			NetInSpeed:  r.NetInSpeed,
			NetOutSpeed: r.NetOutSpeed,
			Load1:       round2(r.Load1),
			Load5:       round2(r.Load5),
			Load15:      round2(r.Load15),
			TcpConn:     r.TcpConn,
			UdpConn:     r.UdpConn,
			Uptime:      r.Uptime,
			Samples:     1,
		})
	}
	return points, nil
}

// Realtime 返回节点最近一次采集的指标（规格书 8.6 实时指标）。
//
// 无数据时返回 nil 与 nil（前端展示「暂无数据」而不是报错）。
func (s *ProbeService) Realtime(nodeID uint64) (*model.ProbeMetric, error) {
	if nodeID == 0 {
		return nil, response.Field(response.CodeParamInvalid, "node_id", nodeID, "节点 ID 不能为空")
	}
	var m model.ProbeMetric
	err := s.app.DB.Where("node_id = ?", nodeID).Order("timestamp DESC").First(&m).Error
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, response.Wrap(response.CodeInternal, err, "查询实时指标失败")
	}
	return &m, nil
}

// NodeOverview 是全局监控页中单个节点的卡片数据（规格书 6.11）。
type NodeOverview struct {
	NodeID     uint64   `json:"node_id"`
	Name       string   `json:"name"`
	Role       string   `json:"role"`
	Online     bool     `json:"online"`
	PublicIPv4 string   `json:"public_ipv4"`
	GroupIDs   []uint64 `json:"group_ids"`

	CPUUsage    float64 `json:"cpu_usage"`
	MemUsed     int64   `json:"mem_used"`
	MemTotal    int64   `json:"mem_total"`
	DiskUsed    int64   `json:"disk_used"`
	DiskTotal   int64   `json:"disk_total"`
	NetInSpeed  int64   `json:"net_in_speed"`
	NetOutSpeed int64   `json:"net_out_speed"`
	Load1       float64 `json:"load1"`
	TcpConn     int     `json:"tcp_conn"`
	UdpConn     int     `json:"udp_conn"`
	Uptime      int64   `json:"uptime"`

	CurrentConn int    `json:"current_conn"`
	HealthScore int    `json:"health_score"`
	ClientVer   string `json:"client_ver"`
	LastSeen    string `json:"last_seen"`
	UpdatedAt   string `json:"updated_at"`

	// Sparkline 是最近 5 分钟的 CPU / 网络迷你曲线，供卡片直接绘制。
	Sparkline []ProbePoint `json:"sparkline"`
}

// Overview 返回所有节点的最新指标与迷你曲线（规格书 8.13）。
//
// 参数 ctx 为上下文；sparklinePoints 为每张卡片的曲线点数，
// 传 0 表示不返回曲线（默认取 60 个点，约 5 分钟 @5 秒粒度）。
// 返回节点卡片列表或错误。
func (s *ProbeService) Overview(ctx context.Context, sparklinePoints int) ([]NodeOverview, error) {
	if sparklinePoints <= 0 {
		sparklinePoints = 60
	}

	nodes := make([]model.Node, 0)
	if err := s.app.DB.WithContext(ctx).Order("id ASC").Find(&nodes).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "查询节点列表失败")
	}

	// 一次性取出全部节点的最近 N 条指标，避免 N+1 查询。
	nodeIDs := make([]uint64, 0, len(nodes))
	for i := range nodes {
		nodeIDs = append(nodeIDs, nodes[i].ID)
	}

	sparklines := map[uint64][]ProbePoint{}
	if len(nodeIDs) > 0 {
		rows := make([]model.ProbeMetric, 0)
		err := s.app.DB.WithContext(ctx).
			Where("node_id IN ? AND timestamp >= ?", nodeIDs,
				time.Now().UTC().Add(-10*time.Minute).Unix()).
			Order("node_id ASC, timestamp ASC").
			Limit(sparklinePoints * len(nodeIDs)).
			Find(&rows).Error
		if err != nil {
			return nil, response.Wrap(response.CodeInternal, err, "查询节点指标失败")
		}
		for i := range rows {
			r := rows[i]
			sparklines[r.NodeID] = append(sparklines[r.NodeID], metricToPoint(&r))
		}
	}

	out := make([]NodeOverview, 0, len(nodes))
	for i := range nodes {
		n := nodes[i]
		series := sparklines[n.ID]
		if len(series) > sparklinePoints {
			series = series[len(series)-sparklinePoints:]
		}
		card := NodeOverview{
			NodeID:      n.ID,
			Name:        n.Name,
			Role:        n.Role,
			Online:      n.Online,
			PublicIPv4:  n.PublicIPv4,
			GroupIDs:    n.GroupIDs.AsUint64Slice(),
			CPUUsage:    round2(n.CPUUsage),
			MemUsed:     n.MemUsed,
			MemTotal:    n.MemTotal,
			DiskUsed:    n.DiskUsed,
			DiskTotal:   n.DiskTotal,
			NetInSpeed:  n.NetInSpeed,
			NetOutSpeed: n.NetOutSpeed,
			Load1:       round2(n.Load1),
			TcpConn:     int(n.CurrentConn),
			Uptime:      n.Uptime,
			CurrentConn: n.CurrentConn,
			HealthScore: n.HealthScore,
			ClientVer:   n.ClientVer,
			UpdatedAt:   n.UpdatedAt.UTC().Format(time.RFC3339),
			Sparkline:   append([]ProbePoint{}, series...),
		}
		if n.LastSeen != nil {
			card.LastSeen = n.LastSeen.UTC().Format(time.RFC3339)
		}
		if card.Sparkline == nil {
			card.Sparkline = []ProbePoint{}
		}
		out = append(out, card)
	}
	return out, nil
}

// Cleanup 删除超过保留期的探针数据（规格书 6.11 保留策略）。
//
// 参数 ctx 为上下文；keepDays 为保留天数，<=0 时回退到配置的 probe-keep-days。
// 返回删除的行数与错误。
func (s *ProbeService) Cleanup(ctx context.Context, keepDays int) (int64, error) {
	if keepDays <= 0 {
		keepDays = s.app.Config.ProbeKeepDays
	}
	if keepDays <= 0 {
		keepDays = 7
	}
	deadline := time.Now().UTC().Add(-time.Duration(keepDays) * 24 * time.Hour).Unix()

	res := s.app.DB.WithContext(ctx).
		Where("timestamp < ?", deadline).Delete(&model.ProbeMetric{})
	if res.Error != nil {
		return 0, response.Wrap(response.CodeInternal, res.Error, "清理探针数据失败")
	}
	return res.RowsAffected, nil
}

// StatsOf 返回某节点在给定时段内的聚合统计（最大 / 平均），
// 用于节点详情页顶部的数字卡片。
//
// 参数 ctx 为上下文；nodeID 为节点；since 为统计起点。
// 返回 CPU 平均、内存峰值与样本数。
func (s *ProbeService) StatsOf(ctx context.Context, nodeID uint64, since time.Time) (cpuAvg float64, memMax int64, samples int64, err error) {
	var row struct {
		CPUAvg  float64 `gorm:"column:cpu_avg"`
		MemMax  int64   `gorm:"column:mem_max"`
		Samples int64   `gorm:"column:samples"`
	}
	sql := `SELECT AVG(cpu) AS cpu_avg, MAX(mem_used) AS mem_max, COUNT(*) AS samples
FROM probe_metrics WHERE node_id = ? AND timestamp >= ?`
	if e := s.app.DB.WithContext(ctx).Raw(sql, nodeID, since.Unix()).Scan(&row).Error; e != nil {
		return 0, 0, 0, response.Wrap(response.CodeInternal, e, "统计探针数据失败")
	}
	return round2(row.CPUAvg), row.MemMax, row.Samples, nil
}

// metricToPoint 把库中的原始指标转换为曲线点。
func metricToPoint(m *model.ProbeMetric) ProbePoint {
	return ProbePoint{
		Timestamp:   m.Timestamp,
		Time:        time.Unix(m.Timestamp, 0).UTC().Format(time.RFC3339),
		CPU:         round2(m.CPU),
		MemUsed:     m.MemUsed,
		MemTotal:    m.MemTotal,
		SwapUsed:    m.SwapUsed,
		SwapTotal:   m.SwapTotal,
		DiskUsed:    m.DiskUsed,
		DiskTotal:   m.DiskTotal,
		NetIn:       m.NetIn,
		NetOut:      m.NetOut,
		NetInSpeed:  m.NetInSpeed,
		NetOutSpeed: m.NetOutSpeed,
		Load1:       round2(m.Load1),
		Load5:       round2(m.Load5),
		Load15:      round2(m.Load15),
		TcpConn:     m.TcpConn,
		UdpConn:     m.UdpConn,
		Uptime:      m.Uptime,
		Samples:     1,
	}
}

// round2 把浮点数保留两位小数，避免曲线接口返回过长的小数串。
func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

// isNotFound 判断错误是否为 Gorm 的「记录不存在」。
func isNotFound(err error) bool {
	return err == gorm.ErrRecordNotFound
}
