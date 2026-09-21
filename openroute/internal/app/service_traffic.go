package app

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/openroute/openroute/internal/api/response"
	"github.com/openroute/openroute/internal/database"
	"github.com/openroute/openroute/internal/model"
	"github.com/openroute/openroute/internal/util"
)

// 本文件实现规格书 6.10「流量统计」与 4.2.10「在线会话」的后端逻辑。
//
// 统计维度为规则级 / 用户级 / 全站级三层，字节口径为
// 「原始字节 + 乘倍率后字节」双记录，页面默认展示折算值、可切换原始值。
// 本服务不涉及任何计费、余额、订单或套餐语义。

// 流量聚合口径常量。
const (
	// HourBucket 表示按小时聚合（保留 30 天）。
	HourBucket = "hour"
	// DayBucket 表示按天聚合（永久保留）。
	DayBucket = "day"

	// GroupByUser / GroupByRule / GroupByNode / GroupByDirection 是时间序列的分组维度。
	GroupByUser      = "user"
	GroupByRule      = "rule"
	GroupByNode      = "node"
	GroupByDirection = "direction"

	// DimensionUser / DimensionRule / DimensionNode 是 Top-N 排行维度。
	DimensionUser = "user"
	DimensionRule = "rule"
	DimensionNode = "node"

	// defaultTopLimit 是 Top-N 的默认条数。
	defaultTopLimit = 10
	// maxTopLimit 是 Top-N 的硬上限，防止单次请求拉爆内存。
	maxTopLimit = 200
)

// TrafficSample 是一次流量上报的增量。
//
// 语义要点（规格书 6.10「统计口径」）：
//   - RawBytes 是**实际字节**，Bytes 是**乘倍率后字节**，两者都记录；
//   - 两个字段都是**增量**而不是累计值，落库走 database.UpsertTraffic 的累加路径；
//   - RuleID / UserID / NodeID 任一为 0 表示该维度未知，仍会落库以便站点级汇总；
//   - Direction 取 model.DirectionIn / DirectionOut，非法值归一化为 in。
type TrafficSample struct {
	// ExactBytes preserves an explicitly zero multiplier instead of applying the legacy fallback.
	ExactBytes bool
	// Hour 指定该增量所属的小时（0~23）。
	// 传 model.HourDaily(-1) 表示「只写按天聚合行」，用于调用方已自行汇总的场景。
	Hour int

	RuleID    uint64
	UserID    uint64
	NodeID    uint64
	Direction string

	// RawBytes 为实际字节增量；Bytes 为乘倍率后的字节增量。
	// Bytes 为 0 时回退为 RawBytes，避免调用方忘记填倍率导致统计出现空洞。
	RawBytes int64
	Bytes    int64
}

// TrafficService 负责流量统计、在线会话与限额口径的对外聚合。
//
// 依赖关系：
//   - 明细落在 traffic_logs（规格书 4.2.8），由 database.UpsertTraffic 做跨方言 UPSERT；
//   - 在线会话落在 sessions（规格书 4.2.10），内存保留一份热态以便即时判断；
//   - 入口限速与连接/IP/设备限制委托给 LimitService（规格书 6.9），构造时一并装配。
type TrafficService struct {
	app *App

	// limit 是入口侧限速与限制器（规格书 6.9）。
	limit *LimitService

	// sessions 是会话热态；sessMu 保护它与其派生统计。
	sessMu   sync.RWMutex
	sessions map[string]*sessionState
}

// sessionState 是单个会话在内存中的热态。
//
// 数据库是权威存储，内存副本只服务于「连接数 / IP 数 / 设备数」的即时判断；
// 进程重启后由 RebuildSessions 从 sessions 表重建。
type sessionState struct {
	ID        uint64
	RuleID    uint64
	UserID    uint64
	NodeID    uint64
	ClientIP  string
	DeviceID  string
	Protocol  string
	ConnCount int
	Upload    int64
	Download  int64
	StartedAt time.Time
	LastSeen  time.Time
}

// SessionCap 是 sessions 表的容量上限（规格书 4.2.10 的 10 万行）。
//
// 超过该行数时按 last_seen 淘汰最旧记录；会话不参与计费，仅用于限制与展示。
const SessionCap = 100000

// sessionIdleTimeout 是会话在内存中的空闲淘汰阈值。
//
// 取 10 分钟：比 IP 限制的 5 分钟窗口更长，
// 保证「刚滑出窗口的连接」仍被认作已建立连接，而不会被重复计入新连接。
const sessionIdleTimeout = 10 * time.Minute

// maxCSVExportRows 限制单次 CSV 导出的最大行数，避免个人自用机器上内存被打满。
const maxCSVExportRows = 200000

// sessionKey 生成内存会话的键。
//
// 键为「规则 + 客户端 IP + 设备 + 协议」，与 sessions 表的业务唯一性口径一致。
func sessionKey(ruleID uint64, ip, deviceID, protocol string) string {
	return fmt.Sprintf("%d|%s|%s|%s", ruleID, ip, deviceID, protocol)
}

// NewTrafficService 构造流量统计服务，并装配入口限制器。
//
// 参数 a 为运行时依赖容器；返回可直接使用的服务实例。
func NewTrafficService(a *App) *TrafficService {
	s := &TrafficService{
		app:      a,
		limit:    NewLimitService(a),
		sessions: make(map[string]*sessionState),
	}
	return s
}

// Limit 返回关联的入口限制服务（规格书 6.9）。
//
// 限速只在入口生效，出口端不做限速；调用方在「接受新连接」前调用它。
func (s *TrafficService) Limit() *LimitService { return s.limit }

// -------------------- 写入 --------------------

// Record 写入一批流量增量，并同步维护反规范化计数器。
//
// 行为：
//  1. 每条样本写「按小时聚合行」（Hour=0~23）与「按天聚合行」（Hour=model.HourDaily）两行。
//     小时行保留 30 天、天行永久保留（规格书 6.10 的时间粒度要求）；
//     样本显式传 model.HourDaily 时只写天行，避免调用方已自行汇总时重复累加。
//  2. 随后累加 forward_rules.traffic_in / traffic_out 与 users.traffic_used。
//     这两个字段是「已乘倍率」的累计值，仅供列表页直接展示；
//     权威明细始终在 traffic_logs，可据此随时重算。
//
// 参数 ctx 为上下文；samples 为流量增量列表。返回错误。
func (s *TrafficService) Record(ctx context.Context, samples []TrafficSample) error {
	return s.app.DB.Tx(ctx, func(tx *gorm.DB) error { return s.recordTraffic(ctx, tx, samples, timeNow()) })
}

func (s *TrafficService) recordTraffic(ctx context.Context, tx *gorm.DB, samples []TrafficSample, now time.Time) error {
	if len(samples) == 0 {
		return nil
	}

	date := now.Format("2006-01-02")

	ruleIn := make(map[uint64]int64)
	ruleOut := make(map[uint64]int64)
	userTotal := make(map[uint64]int64)

	hourRows := make([]database.TrafficUpsert, 0, len(samples))
	dayRows := make([]database.TrafficUpsert, 0, len(samples))

	mkRow := func(hour int, sp TrafficSample) database.TrafficUpsert {
		return database.TrafficUpsert{
			Date:      date,
			Hour:      hour,
			UserID:    sp.UserID,
			RuleID:    sp.RuleID,
			NodeID:    sp.NodeID,
			Direction: sp.Direction,
			RawBytes:  sp.RawBytes,
			Bytes:     sp.Bytes,
			CreatedAt: now,
			UpdatedAt: now,
		}
	}

	var totalRaw, totalBytes int64
	for _, raw := range samples {
		sp := normalizeSample(raw)
		if totalRaw > math.MaxInt64-sp.RawBytes || totalBytes > math.MaxInt64-sp.Bytes {
			return errors.New("流量批次增量溢出")
		}
		totalRaw += sp.RawBytes
		totalBytes += sp.Bytes
		if sp.RawBytes == 0 && sp.Bytes == 0 {
			// 零增量不落库，避免制造大量无意义的零行把表撑大。
			continue
		}
		if sp.Hour >= 0 && sp.Hour < 24 {
			hourRows = append(hourRows, mkRow(sp.Hour, sp))
		}
		dayRows = append(dayRows, mkRow(model.HourDaily, sp))

		if sp.RuleID > 0 {
			if sp.Direction == model.DirectionOut {
				ruleOut[sp.RuleID] += sp.Bytes
			} else {
				ruleIn[sp.RuleID] += sp.Bytes
			}
		}
		if sp.UserID > 0 {
			userTotal[sp.UserID] += sp.Bytes
		}
	}

	if len(dayRows) == 0 {
		return nil
	}

	// 明细写入：小时行与天行都走 database.UpsertTraffic 的跨方言累加路径。
	// 唯一键包含 hour 列，因此 -1 与 0~23 天然落在不同行，不会互相覆盖。
	txdb := &database.DB{DB: tx, Dialect: s.app.DB.Dialect}
	if err := txdb.UpsertTraffic(ctx, hourRows); err != nil {
		return response.Wrap(response.CodeDBUnwritable, err, "写入流量小时明细失败")
	}
	if err := txdb.UpsertTraffic(ctx, dayRows); err != nil {
		return response.Wrap(response.CodeDBUnwritable, err, "写入流量天明细失败")
	}

	// 反规范化计数器：整体放在一个事务里，避免「一半规则更新、一半没更新」。
	err := func() error {
		for id, delta := range ruleIn {
			if err := s.bumpRuleTraffic(ctx, tx, id, delta, 0); err != nil {
				return err
			}
		}
		for id, delta := range ruleOut {
			if err := s.bumpRuleTraffic(ctx, tx, id, 0, delta); err != nil {
				return err
			}
		}
		for id, delta := range userTotal {
			if delta == 0 {
				continue
			}
			result := tx.WithContext(ctx).Model(&model.User{}).Where("id = ? AND traffic_used <= ?", id, math.MaxInt64-delta).
				UpdateColumn("traffic_used", gorm.Expr("traffic_used + ?", delta))
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 0 {
				var count int64
				if err := tx.Model(&model.User{}).Where("id = ?", id).Count(&count).Error; err != nil {
					return err
				}
				if count != 0 {
					return errors.New("用户流量计数器溢出")
				}
			}
		}
		return nil
	}()
	if err != nil {
		return response.Wrap(response.CodeDBUnwritable, err, "更新流量计数器失败")
	}
	return nil
}

// bumpRuleTraffic 累加单条规则的入口 / 出口累计流量。
func (s *TrafficService) bumpRuleTraffic(ctx context.Context, tx *gorm.DB, ruleID uint64, in, out int64) error {
	if in == 0 && out == 0 {
		return nil
	}
	result := tx.WithContext(ctx).Model(&model.ForwardRule{}).Where("id = ? AND traffic_in <= ? AND traffic_out <= ?", ruleID, math.MaxInt64-in, math.MaxInt64-out).
		Where("traffic_in <= ? - traffic_out", math.MaxInt64-in-out).
		Updates(map[string]interface{}{
			"traffic_in":  gorm.Expr("traffic_in + ?", in),
			"traffic_out": gorm.Expr("traffic_out + ?", out),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		var count int64
		if err := tx.Model(&model.ForwardRule{}).Where("id = ?", ruleID).Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			return errors.New("规则流量计数器溢出")
		}
	}
	return nil
}

// normalizeSample 归一化一条样本的方向、小时与字节口径。
//
// 规则：
//   - 方向非法 → in；
//   - 负数 → 0（负数增量只可能来自计数回绕，直接丢弃增量避免统计倒扣）；
//   - 只填了其中一列字节时，用另一列补齐，保证两列语义一致；
//   - 小时 > 23 → model.HourDaily。
func normalizeSample(sp TrafficSample) TrafficSample {
	if sp.Direction != model.DirectionOut {
		sp.Direction = model.DirectionIn
	}
	if sp.RawBytes < 0 {
		sp.RawBytes = 0
	}
	if sp.Bytes < 0 {
		sp.Bytes = 0
	}
	if sp.Bytes == 0 && sp.RawBytes != 0 && !sp.ExactBytes {
		sp.Bytes = sp.RawBytes
	}
	if sp.RawBytes == 0 && sp.Bytes != 0 {
		sp.RawBytes = sp.Bytes
	}
	if sp.Hour > 23 || sp.Hour < 0 {
		sp.Hour = model.HourDaily
	}
	return sp
}

// -------------------- 展示口径 --------------------

// BytesMode 决定展示与导出使用哪一列字节（规格书 6.10「统计口径」）。
type BytesMode string

// 展示口径常量。
const (
	// BytesModeScaled 展示乘倍率后的字节（页面默认）。
	BytesModeScaled BytesMode = "scaled"
	// BytesModeRaw 展示实际字节。
	BytesModeRaw BytesMode = "raw"
)

// Column 返回该口径对应的数据库列名。
func (m BytesMode) Column() string {
	if m == BytesModeRaw {
		return "raw_bytes"
	}
	return "bytes"
}

// ParseBytesMode 解析展示口径参数，空值回退为默认的折算口径。
func ParseBytesMode(v string) BytesMode {
	if v == "raw" || v == "raw_bytes" {
		return BytesModeRaw
	}
	return BytesModeScaled
}

// TrafficPeriod 是一个时间区间的流量汇总。
type TrafficPeriod struct {
	From  string `json:"from,omitempty"`
	To    string `json:"to,omitempty"`
	In    int64  `json:"in"`
	Out   int64  `json:"out"`
	Total int64  `json:"total"`
	// Raw 是同期的实际字节，便于前端对照倍率差异。
	Raw int64 `json:"raw"`
}

// TrafficOverview 是全站概览（规格书 8.12 GET /traffic/overview）。
type TrafficOverview struct {
	Today     TrafficPeriod `json:"today"`
	Yesterday TrafficPeriod `json:"yesterday"`
	Month     TrafficPeriod `json:"month"`
	Total     TrafficPeriod `json:"total"`

	OnlineUsers int `json:"online_users"`
	OnlineNodes int `json:"online_nodes"`
	TotalNodes  int `json:"total_nodes"`
	TotalRules  int `json:"total_rules"`
	ActiveRules int `json:"active_rules"`
	ActiveConns int `json:"active_conns"`

	// BytesMode 回显本次响应的口径，前端据此决定表格标题与单位提示。
	BytesMode BytesMode `json:"bytes_mode"`
	// Timezone 固定为 UTC，说明按天聚合使用 UTC 日期。
	Timezone string `json:"timezone"`
}

// Overview 返回全站流量概览（今日 / 昨日 / 本月 / 累计 / 在线数）。
//
// 统计口径：只读 hour = model.HourDaily 的「按天聚合行」。
// 同一次写入会同时产生小时行与天行，两者混读会重复计算，因此必须区分。
//
// 参数 ctx 为上下文；mode 为空时按折算口径统计。返回概览数据或错误。
func (s *TrafficService) Overview(ctx context.Context, mode BytesMode) (*TrafficOverview, error) {
	if mode == "" {
		mode = BytesModeScaled
	}
	col := mode.Column()

	now := timeNow()
	today := now.Format("2006-01-02")
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")
	monthStart := now.Format("2006-01") + "-01"

	out := &TrafficOverview{BytesMode: mode, Timezone: "UTC"}

	// 折算字节：一条 SQL 扫出四个区间，用 CASE WHEN 分组避免四次全表扫描。
	var err error
	out.Today, out.Yesterday, out.Month, out.Total, err = s.periods(ctx, col, today, yesterday, monthStart)
	if err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "统计全站流量失败")
	}

	// 实际字节：单独扫一遍，便于前端展示「倍率带来了多少差异」。
	_, _, _, rawTotal, err := s.periods(ctx, "raw_bytes", today, yesterday, monthStart)
	if err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "统计全站实际流量失败")
	}
	if rawTotal.Total > 0 {
		// 只回填「累计」这一项即可满足页面对照需求，避免响应体膨胀。
		out.Total.Raw = rawTotal.Total
	}

	// 在线与规模统计。Gorm 的 Count 需要 *int64 目标，这里统一用中间变量承接。
	var n int64
	db := s.app.DB.WithContext(ctx)
	if err := db.Model(&model.Node{}).Where("online = ?", true).Count(&n).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "统计在线节点失败")
	}
	out.OnlineNodes = int(n)
	if err := db.Model(&model.Node{}).Count(&n).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "统计节点总数失败")
	}
	out.TotalNodes = int(n)
	if err := db.Model(&model.ForwardRule{}).Count(&n).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "统计规则总数失败")
	}
	out.TotalRules = int(n)
	if err := db.Model(&model.ForwardRule{}).Where("enable = ?", true).Count(&n).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "统计启用规则数失败")
	}
	out.ActiveRules = int(n)
	out.ActiveConns = s.ActiveConnections()
	out.OnlineUsers, err = s.OnlineUsers(ctx, 0)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// periods 统计今日 / 昨日 / 本月 / 累计四个区间的流量。
//
// 参数 col 为字节列名（bytes 或 raw_bytes）；today / yesterday / monthStart 为 UTC 日期。
// 返回四个区间的汇总。
func (s *TrafficService) periods(ctx context.Context, col, today, yesterday, monthStart string) (
	TrafficPeriod, TrafficPeriod, TrafficPeriod, TrafficPeriod, error) {

	var zero, zero2, zero3, zero4 TrafficPeriod

	rows, err := s.queryBuckets(ctx, col, "", []interface{}{today, yesterday, monthStart})
	if err != nil {
		return zero, zero2, zero3, zero4, err
	}

	var todayP, ydayP, monthP, totalP TrafficPeriod
	accumulatePeriods(&todayP, &ydayP, &monthP, &totalP, rows, today, yesterday, monthStart)
	return todayP, ydayP, monthP, totalP, nil
}

// bucketSQL 是「按时间区间 + 方向」聚合的公共 SQL 片段。
//
// 三个区间（今日 / 昨日 / 本月）用 CASE WHEN 在一次扫描里拆开，
// 只用到比较与字符串常量，因此三种方言都成立（规格书 4.1 跨方言约定）。
func bucketSQL(col string) string {
	return "CASE WHEN date = ? THEN 'today' WHEN date = ? THEN 'yesterday' " +
		"WHEN date >= ? AND date <= ? THEN 'month' ELSE 'old' END AS bucket, " +
		"direction AS direction, SUM(" + col + ") AS sum_bytes, SUM(raw_bytes) AS sum_raw"
}

// bucketRow 是「按时间区间 + 方向」聚合出的中间行。
type bucketRow struct {
	Bucket    string
	Direction string
	SumBytes  int64
	SumRaw    int64
}

// queryBuckets 按「区间 + 方向」聚合 traffic_logs。
//
// 参数 col 为字节列名；extraWhere 为附加的维度过滤条件（如 "rule_id = ? AND "，
// 末尾必须带 AND）；rangeArgs 为三个区间的日期参数，顺序固定为
// 今日 / 昨日 / 本月起始。只读按天聚合行（hour = -1），避免与小时行重复计数。
func (s *TrafficService) queryBuckets(ctx context.Context, col, extraWhere string, rangeArgs []interface{}) ([]bucketRow, error) {
	args := make([]interface{}, 0, len(rangeArgs)+2)
	// CASE uses today twice: once for its own bucket, once as the month-to-date end.
	args = append(args, rangeArgs[:3]...)
	args = append(args, rangeArgs[0])
	args = append(args, rangeArgs[3:]...)
	args = append(args, model.HourDaily)

	sqlStr := "SELECT " + bucketSQL(col) + " FROM traffic_logs WHERE " + extraWhere + " hour = ?" +
		" GROUP BY bucket, direction"

	var rows []bucketRow
	if err := s.app.DB.WithContext(ctx).Raw(sqlStr, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// -------------------- 时间序列 --------------------

// TrafficSeriesPoint 是时间序列上的一个分桶。
type TrafficSeriesPoint struct {
	// Time 为桶起始时间，RFC3339 UTC。
	Time string `json:"time"`
	// Unix 为桶起始的 Unix 秒，便于前端做图表横轴排序。
	Unix int64 `json:"unix"`
	// Group 为分组键：groupBy=direction 时为 in/out，其余为维度 ID 的字符串形式。
	Group string `json:"group"`
	// GroupName 为分组键的可读名称（用户/规则/节点名），direction 维度下与 Group 相同。
	GroupName string `json:"group_name"`

	In    int64 `json:"in"`
	Out   int64 `json:"out"`
	Total int64 `json:"total"`
	Raw   int64 `json:"raw"`
}

// Timeseries 返回时间序列（规格书 8.12 GET /traffic/timeseries）。
//
// 参数：
//   - from / to 为查询区间（含边界），零值时默认取最近 24 小时；
//   - interval 取 hour|day，默认 hour；
//   - groupBy 取 user|rule|node|direction，默认 direction。
//
// 分桶实现：时间桶在 Go 侧用「区间起点 + 偏移天数 + 小时」计算，
// 累计换算与除法都基于 Unix 整数，不依赖任何方言特有的日期函数。
//
// 返回按时间升序排列的序列点或错误。
func (s *TrafficService) Timeseries(ctx context.Context, from, to time.Time, interval, groupBy string, mode BytesMode) ([]TrafficSeriesPoint, error) {
	return s.TimeseriesFiltered(ctx, from, to, interval, groupBy, mode, TrafficFilter{})
}

// TimeseriesFiltered applies resource filters before aggregating by any dimension.
func (s *TrafficService) TimeseriesFiltered(ctx context.Context, from, to time.Time, interval, groupBy string, mode BytesMode, filter TrafficFilter) ([]TrafficSeriesPoint, error) {
	if mode == "" {
		mode = BytesModeScaled
	}
	interval = normalizeInterval(interval)
	groupBy = normalizeGroupBy(groupBy)

	now := timeNow()
	if from.IsZero() {
		from = now.Add(-24 * time.Hour)
	}
	if to.IsZero() {
		to = now
	}
	if !to.After(from) {
		return nil, response.New(response.CodeParamOutOfRange, "结束时间必须晚于开始时间")
	}

	col := mode.Column()
	from, to = from.UTC(), to.UTC()
	hourOnly := interval == HourBucket

	q := s.app.DB.WithContext(ctx).Model(&model.TrafficLog{}).
		Select("date, hour, direction, "+
			"SUM("+col+") AS sum_bytes, SUM(raw_bytes) AS sum_raw, "+groupColSQL(groupBy)+" AS grp").
		Where("date >= ? AND date <= ?", from.Format("2006-01-02"), to.Format("2006-01-02"))

	if hourOnly {
		q = q.Where("hour >= ?", 0)
	} else {
		q = q.Where("hour = ?", model.HourDaily)
	}
	filter.Interval, filter.From, filter.To = interval, from, to
	q = filter.apply(q)
	q = q.Group("date, hour, direction, grp")

	var rows []struct {
		Date      string
		Hour      int
		Direction string
		SumBytes  int64
		SumRaw    int64
		Grp       string
	}
	if err := q.Scan(&rows).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "统计流量时间序列失败")
	}

	idx := make(map[string]*TrafficSeriesPoint)
	out := make([]TrafficSeriesPoint, 0, len(rows))

	for _, r := range rows {
		ts := bucketUnix(r.Date, r.Hour, interval)
		if ts < from.Truncate(bucketDuration(interval)).Unix() || ts > to.Unix() {
			// 天聚合行只带日期，按天桶的起点做区间判断已足够精确。
			continue
		}
		key := r.Grp + "@" + strconv.FormatInt(ts, 10)
		p := idx[key]
		if p == nil {
			p = &TrafficSeriesPoint{
				Time:  time.Unix(ts, 0).UTC().Format(time.RFC3339),
				Unix:  ts,
				Group: r.Grp,
			}
			idx[key] = p
			out = append(out, *p)
		}
		if r.Direction == model.DirectionOut {
			p.Out += r.SumBytes
		} else {
			p.In += r.SumBytes
		}
		p.Total += r.SumBytes
		p.Raw += r.SumRaw
	}

	// 把内存里的累积值回写到切片（out 中存放的是结构体副本）。
	for i := range out {
		key := out[i].Group + "@" + strconv.FormatInt(out[i].Unix, 10)
		if p := idx[key]; p != nil {
			out[i] = *p
		}
	}

	names := s.resolveNames(ctx, groupBy, out)
	for i := range out {
		if n, ok := names[out[i].Group]; ok {
			out[i].GroupName = n
		} else {
			out[i].GroupName = out[i].Group
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Unix == out[j].Unix {
			return out[i].Group < out[j].Group
		}
		return out[i].Unix < out[j].Unix
	})
	return out, nil
}

func bucketDuration(interval string) time.Duration {
	if interval == DayBucket {
		return 24 * time.Hour
	}
	return time.Hour
}

// groupColSQL 返回分组维度对应的 SQL 表达式。
func groupColSQL(groupBy string) string {
	switch groupBy {
	case GroupByUser:
		return "user_id"
	case GroupByRule:
		return "rule_id"
	case GroupByNode:
		return "node_id"
	default:
		return "direction"
	}
}

// normalizeInterval 归一化时间粒度参数。
func normalizeInterval(v string) string {
	if v == DayBucket {
		return DayBucket
	}
	return HourBucket
}

// normalizeGroupBy 归一化分组维度参数。
func normalizeGroupBy(v string) string {
	switch v {
	case GroupByUser, GroupByRule, GroupByNode:
		return v
	default:
		return GroupByDirection
	}
}

// bucketUnix 计算一行聚合计录所属时间桶的 Unix 秒起点。
//
// 分桶规则：
//   - hour 粒度：桶起点 = 当天 00:00 + hour 小时；
//   - day 粒度：桶起点 = 当天 00:00（hour 行不参与）。
//
// 计算全部基于整数，避免引入任何方言的日期函数（规格书 4.1 跨方言约定）。
func bucketUnix(date string, hour int, interval string) int64 {
	t, err := time.ParseInLocation("2006-01-02", date, time.UTC)
	if err != nil {
		return 0
	}
	base := t.Unix()
	if interval == DayBucket || hour < 0 {
		return base
	}
	return base + int64(hour)*3600
}

// resolveNames 批量把分组键（ID 字符串）解析为可读名称。
//
// direction 维度不需要解析，直接返回空表；解析失败时调用方会退回显示原始 ID，
// 保证「节点被删但历史流量仍在」时图表不报错。
func (s *TrafficService) resolveNames(ctx context.Context, groupBy string, points []TrafficSeriesPoint) map[string]string {
	out := make(map[string]string)
	if groupBy == GroupByDirection || len(points) == 0 {
		return out
	}

	ids := make([]uint64, 0, len(points))
	seen := make(map[uint64]struct{}, len(points))
	for _, p := range points {
		id, err := strconv.ParseUint(p.Group, 10, 64)
		if err != nil || id == 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return out
	}

	var table, nameCol string
	switch groupBy {
	case GroupByUser:
		table, nameCol = "users", "username"
	case GroupByRule:
		table, nameCol = "forward_rules", "name"
	case GroupByNode:
		table, nameCol = "nodes", "name"
	default:
		return out
	}

	var rows []struct {
		ID   uint64
		Name string
	}
	if err := s.app.DB.WithContext(ctx).Table(table).
		Select("id, "+nameCol+" AS name").
		Where("id IN ?", ids).Scan(&rows).Error; err != nil {
		s.app.Log.Debug("解析流量分组名称失败", zap.String("group_by", groupBy), zap.Error(err))
		return out
	}
	for _, r := range rows {
		out[strconv.FormatUint(r.ID, 10)] = r.Name
	}
	return out
}

// -------------------- Top-N 排行 --------------------

// TrafficRankItem 是 Top-N 排行的一项。
type TrafficRankItem struct {
	ID    uint64 `json:"id"`
	Name  string `json:"name"`
	In    int64  `json:"in"`
	Out   int64  `json:"out"`
	Total int64  `json:"total"`
	Raw   int64  `json:"raw"`
	// Percent 为占全站同口径总量的百分比（0~100，保留两位）。
	Percent float64 `json:"percent"`
}

// Top 返回 Top-N 排行（规格书 8.12 GET /traffic/top）。
//
// 参数：
//   - dimension 取 user|rule|node，非法值回退为 rule；
//   - limit 默认 10、上限 200；
//   - from / to 为零值时统计「累计」，否则按日期区间过滤（按天聚合行）。
//
// 返回排行列表或错误。
func (s *TrafficService) Top(ctx context.Context, dimension string, limit int, from, to time.Time, mode BytesMode) ([]TrafficRankItem, error) {
	return s.TopFiltered(ctx, dimension, limit, from, to, mode, TrafficFilter{})
}

// TopFiltered uses the same resource filters for ranking and the percentage denominator.
func (s *TrafficService) TopFiltered(ctx context.Context, dimension string, limit int, from, to time.Time, mode BytesMode, filter TrafficFilter) ([]TrafficRankItem, error) {
	if mode == "" {
		mode = BytesModeScaled
	}
	if limit <= 0 {
		limit = defaultTopLimit
	}
	if limit > maxTopLimit {
		limit = maxTopLimit
	}

	var col string
	switch dimension {
	case DimensionUser:
		col = "user_id"
	case DimensionNode:
		col = "node_id"
	default:
		dimension = DimensionRule
		col = "rule_id"
	}

	bytesCol := mode.Column()
	from, to = from.UTC(), to.UTC()
	q := s.app.DB.WithContext(ctx).Model(&model.TrafficLog{}).
		Select(col + " AS id, direction AS direction, SUM(" + bytesCol + ") AS sum_bytes, SUM(raw_bytes) AS sum_raw").
		Where(col + " > 0").
		Group(col + ", direction")
	if !from.IsZero() {
		q = q.Where("date >= ?", from.Format("2006-01-02"))
	}
	if !to.IsZero() {
		q = q.Where("date <= ?", to.Format("2006-01-02"))
	}

	filter.From, filter.To = from, to
	q = filter.apply(q)

	var rows []struct {
		ID        uint64
		Direction string
		SumBytes  int64
		SumRaw    int64
	}
	if err := q.Scan(&rows).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "统计流量排行失败")
	}

	agg := make(map[uint64]*TrafficRankItem)
	var grandTotal int64
	for _, r := range rows {
		item := agg[r.ID]
		if item == nil {
			item = &TrafficRankItem{ID: r.ID}
			agg[r.ID] = item
		}
		if r.Direction == model.DirectionOut {
			item.Out += r.SumBytes
		} else {
			item.In += r.SumBytes
		}
		item.Total += r.SumBytes
		item.Raw += r.SumRaw
		grandTotal += r.SumBytes
	}

	list := make([]TrafficRankItem, 0, len(agg))
	for _, item := range agg {
		list = append(list, *item)
	}
	// 按总量降序；总量相同时按 ID 升序，保证分页稳定的确定性输出。
	sortRankItems(list)
	if len(list) > limit {
		list = list[:limit]
	}

	names := s.resolveRankNames(ctx, dimension, list)
	for i := range list {
		if list[i].Total > 0 && grandTotal > 0 {
			list[i].Percent = float64(list[i].Total) * 100 / float64(grandTotal)
		}
		if n, ok := names[list[i].ID]; ok {
			list[i].Name = n
		}
	}
	return list, nil
}

// sortRankItems 对排行做确定性排序（总量降序、ID 升序）。
func sortRankItems(list []TrafficRankItem) {
	// 规模很小（Top-N 上限 200），用插入排序即可，无需引入 sort 包比较器。
	for i := 1; i < len(list); i++ {
		cur := list[i]
		j := i - 1
		for j >= 0 && lessRank(cur, list[j]) {
			list[j+1] = list[j]
			j--
		}
		list[j+1] = cur
	}
}

// lessRank 判断 a 是否应排在 b 前面。
func lessRank(a, b TrafficRankItem) bool {
	if a.Total != b.Total {
		return a.Total > b.Total
	}
	return a.ID < b.ID
}

// resolveRankNames 批量解析排行项的名称。
func (s *TrafficService) resolveRankNames(ctx context.Context, dimension string, list []TrafficRankItem) map[uint64]string {
	out := make(map[uint64]string, len(list))
	if len(list) == 0 {
		return out
	}
	ids := make([]uint64, 0, len(list))
	for _, it := range list {
		ids = append(ids, it.ID)
	}

	var table, nameCol string
	switch dimension {
	case DimensionUser:
		table, nameCol = "users", "username"
	case DimensionNode:
		table, nameCol = "nodes", "name"
	default:
		table, nameCol = "forward_rules", "name"
	}

	var rows []struct {
		ID   uint64
		Name string
	}
	if err := s.app.DB.WithContext(ctx).Table(table).
		Select("id, "+nameCol+" AS name").
		Where("id IN ?", ids).Scan(&rows).Error; err != nil {
		s.app.Log.Debug("解析排行名称失败", zap.String("dimension", dimension), zap.Error(err))
		return out
	}
	for _, r := range rows {
		out[r.ID] = r.Name
	}
	return out
}

// -------------------- 明细查询与导出 --------------------

// TrafficFilter 是流量明细的筛选条件（规格书 6.10：时间范围 + 用户 + 规则 + 节点 + 方向任意组合）。
type TrafficFilter struct {
	From      time.Time
	To        time.Time
	UserID    uint64
	RuleID    uint64
	NodeID    uint64
	Direction string
	// Interval 取 hour|day，默认 day（只读按天聚合行，避免重复计数）。
	Interval string
	// Page / PageSize 用于分页；ExportCSV 会忽略它们并按上限截断。
	Page     int
	PageSize int
}

// apply 把筛选条件套用到查询上。
func (f TrafficFilter) apply(q *gorm.DB) *gorm.DB {
	f.From, f.To = f.From.UTC(), f.To.UTC()
	if f.Interval == HourBucket {
		q = q.Where("hour >= ?", 0)
	} else {
		q = q.Where("hour = ?", model.HourDaily)
	}
	if f.UserID > 0 {
		q = q.Where("user_id = ?", f.UserID)
	}
	if f.RuleID > 0 {
		q = q.Where("rule_id = ?", f.RuleID)
	}
	if f.NodeID > 0 {
		q = q.Where("node_id = ?", f.NodeID)
	}
	if f.Direction == model.DirectionIn || f.Direction == model.DirectionOut {
		q = q.Where("direction = ?", f.Direction)
	}
	if !f.From.IsZero() {
		q = q.Where("date >= ?", f.From.Format("2006-01-02"))
		if f.Interval == HourBucket {
			q = q.Where("(date > ? OR hour >= ?)", f.From.Format("2006-01-02"), f.From.Hour())
		}
	}
	if !f.To.IsZero() {
		q = q.Where("date <= ?", f.To.Format("2006-01-02"))
		if f.Interval == HourBucket {
			q = q.Where("(date < ? OR hour <= ?)", f.To.Format("2006-01-02"), f.To.Hour())
		}
	}
	return q
}

// TrafficDetail 是明细列表的一行（已关联出可读名称）。
type TrafficDetail struct {
	Date      string    `json:"date"`
	Hour      int       `json:"hour"`
	Direction string    `json:"direction"`
	UserID    uint64    `json:"user_id"`
	UserName  string    `json:"user_name"`
	RuleID    uint64    `json:"rule_id"`
	RuleName  string    `json:"rule_name"`
	NodeID    uint64    `json:"node_id"`
	NodeName  string    `json:"node_name"`
	RawBytes  int64     `json:"raw_bytes"`
	Bytes     int64     `json:"bytes"`
	BytesMode BytesMode `json:"bytes_mode"`
}

// Query 按筛选条件分页查询流量明细。
//
// 返回明细列表与总行数或错误。
func (s *TrafficService) Query(ctx context.Context, filter TrafficFilter, mode BytesMode) ([]TrafficDetail, int64, error) {
	if mode == "" {
		mode = BytesModeScaled
	}
	if filter.PageSize <= 0 {
		filter.PageSize = 20
	}
	if filter.Page <= 0 {
		filter.Page = 1
	}

	var total int64
	if err := filter.apply(s.app.DB.WithContext(ctx).Model(&model.TrafficLog{})).
		Count(&total).Error; err != nil {
		return nil, 0, response.Wrap(response.CodeInternal, err, "统计流量明细行数失败")
	}

	rows := make([]model.TrafficLog, 0)
	err := filter.apply(s.app.DB.WithContext(ctx).Model(&model.TrafficLog{})).
		Select("date, hour, direction, user_id, rule_id, node_id, raw_bytes, bytes").
		Order("date DESC, hour DESC, id DESC").
		Offset((filter.Page - 1) * filter.PageSize).
		Limit(filter.PageSize).
		Scan(&rows).Error
	if err != nil {
		return nil, 0, response.Wrap(response.CodeInternal, err, "查询流量明细失败")
	}

	items := make([]TrafficDetail, 0, len(rows))
	for _, r := range rows {
		items = append(items, TrafficDetail{
			Date:      r.Date,
			Hour:      r.Hour,
			Direction: r.Direction,
			UserID:    r.UserID,
			RuleID:    r.RuleID,
			NodeID:    r.NodeID,
			RawBytes:  r.RawBytes,
			Bytes:     r.Bytes,
			BytesMode: mode,
		})
	}
	s.fillDetailNames(ctx, items)
	return items, total, nil
}

// fillDetailNames 为明细补充用户 / 规则 / 节点名称。
func (s *TrafficService) fillDetailNames(ctx context.Context, items []TrafficDetail) {
	if len(items) == 0 {
		return
	}
	userIDs := make([]uint64, 0, len(items))
	ruleIDs := make([]uint64, 0, len(items))
	nodeIDs := make([]uint64, 0, len(items))
	for _, it := range items {
		if it.UserID > 0 {
			userIDs = append(userIDs, it.UserID)
		}
		if it.RuleID > 0 {
			ruleIDs = append(ruleIDs, it.RuleID)
		}
		if it.NodeID > 0 {
			nodeIDs = append(nodeIDs, it.NodeID)
		}
	}
	users := lookupNames(ctx, s.app, "users", "username", userIDs)
	rules := lookupNames(ctx, s.app, "forward_rules", "name", ruleIDs)
	nodes := lookupNames(ctx, s.app, "nodes", "name", nodeIDs)

	for i := range items {
		items[i].UserName = users[items[i].UserID]
		items[i].RuleName = rules[items[i].RuleID]
		items[i].NodeName = nodes[items[i].NodeID]
	}
}

// lookupNames 按 ID 集合批量查名称，缺失的 ID 不在返回表中。
func lookupNames(ctx context.Context, a *App, table, nameCol string, ids []uint64) map[uint64]string {
	out := make(map[uint64]string)
	if len(ids) == 0 {
		return out
	}
	uniq := make([]uint64, 0, len(ids))
	seen := make(map[uint64]struct{}, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		uniq = append(uniq, id)
	}

	var rows []struct {
		ID   uint64
		Name string
	}
	if err := a.DB.WithContext(ctx).Table(table).
		Select("id, "+nameCol+" AS name").
		Where("id IN ?", uniq).Scan(&rows).Error; err != nil {
		a.Log.Debug("批量查询名称失败", zap.String("table", table), zap.Error(err))
		return out
	}
	for _, r := range rows {
		out[r.ID] = r.Name
	}
	return out
}

// csvHeader 是导出 CSV 的表头（中文，便于直接交给表格软件打开）。
var csvHeader = []string{
	"日期", "小时", "方向", "用户ID", "用户", "规则ID", "规则",
	"节点ID", "节点", "实际字节", "折算字节",
}

// ExportCSV 按筛选条件导出 CSV（规格书 6.10：支持导出 CSV）。
//
// 输出带 UTF-8 BOM，避免 Excel 打开中文表头乱码。
// 导出行数上限为 maxCSVExportRows，超出部分截断并在日志中告警。
//
// 参数 ctx 为上下文；filter 为筛选条件；w 为输出目标。返回错误。
func (s *TrafficService) ExportCSV(ctx context.Context, filter TrafficFilter, w io.Writer) error {
	if w == nil {
		return response.New(response.CodeParamInvalid, "导出目标不能为空")
	}

	rows := make([]model.TrafficLog, 0)
	err := filter.apply(s.app.DB.WithContext(ctx).Model(&model.TrafficLog{})).
		Select("date, hour, direction, user_id, rule_id, node_id, raw_bytes, bytes").
		Order("date ASC, hour ASC, id ASC").
		Limit(maxCSVExportRows).
		Scan(&rows).Error
	if err != nil {
		return response.Wrap(response.CodeInternal, err, "查询导出数据失败")
	}
	if len(rows) == maxCSVExportRows {
		s.app.Log.Warn("流量导出达到行数上限，已截断",
			zap.Int("limit", maxCSVExportRows))
	}

	// BOM 必须在任何写入之前落盘，否则 Excel 会按本地编码解析首行。
	if _, err := w.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
		return response.Wrap(response.CodeInternal, err, "写入导出文件失败")
	}

	cw := csv.NewWriter(w)
	if err := cw.Write(csvHeader); err != nil {
		return response.Wrap(response.CodeInternal, err, "写入 CSV 表头失败")
	}

	items := make([]TrafficDetail, 0, len(rows))
	for _, r := range rows {
		items = append(items, TrafficDetail{
			Date:      r.Date,
			Hour:      r.Hour,
			Direction: r.Direction,
			UserID:    r.UserID,
			RuleID:    r.RuleID,
			NodeID:    r.NodeID,
			RawBytes:  r.RawBytes,
			Bytes:     r.Bytes,
		})
	}
	s.fillDetailNames(ctx, items)

	for _, it := range items {
		hour := strconv.Itoa(it.Hour)
		if it.Hour == model.HourDaily {
			hour = "全天"
		}
		rec := []string{
			it.Date, hour, directionLabel(it.Direction),
			strconv.FormatUint(it.UserID, 10), it.UserName,
			strconv.FormatUint(it.RuleID, 10), it.RuleName,
			strconv.FormatUint(it.NodeID, 10), it.NodeName,
			strconv.FormatInt(it.RawBytes, 10), strconv.FormatInt(it.Bytes, 10),
		}
		if err := cw.Write(rec); err != nil {
			return response.Wrap(response.CodeInternal, err, "写入 CSV 行失败")
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return response.Wrap(response.CodeInternal, err, "刷新 CSV 缓冲失败")
	}
	return nil
}

// directionLabel 返回方向的中文标签。
func directionLabel(d string) string {
	if d == model.DirectionOut {
		return "出口"
	}
	return "入口"
}

// -------------------- 首页仪表盘 --------------------

// DashboardData 是首页一次性聚合数据（规格书 8.12 GET /traffic/dashboard）。
type DashboardData struct {
	Overview *TrafficOverview `json:"overview"`

	// TopRules / TopUsers / TopNodes 是今日排行，供首页卡片直接渲染。
	TopRules []TrafficRankItem `json:"top_rules"`
	TopUsers []TrafficRankItem `json:"top_users"`
	TopNodes []TrafficRankItem `json:"top_nodes"`

	// Trend 是最近 24 小时按小时的流量曲线（direction 维度）。
	Trend []TrafficSeriesPoint `json:"trend"`

	// 待处理事项列表。
	SyncFailedRules []DashboardRuleBrief `json:"sync_failed_rules"`
	OfflineNodes    []DashboardNodeBrief `json:"offline_nodes"`
	ActiveAlerts    []model.AlertHistory `json:"active_alerts"`
	PendingIssues   int                  `json:"pending_issues"`
}

// DashboardRuleBrief 是仪表盘上的一条规则摘要。
type DashboardRuleBrief struct {
	ID         uint64 `json:"id"`
	Name       string `json:"name"`
	SyncStatus string `json:"sync_status"`
	SyncError  string `json:"sync_error"`
	UserID     uint64 `json:"user_id"`
}

// DashboardNodeBrief 是仪表盘上的一条节点摘要。
type DashboardNodeBrief struct {
	ID       uint64     `json:"id"`
	Name     string     `json:"name"`
	Role     string     `json:"role"`
	LastSeen *time.Time `json:"last_seen"`
	// OfflineFor 为距上次心跳的秒数，节点从未上线时等于进程启动以来的秒数。
	OfflineFor int64  `json:"offline_seconds"`
	LastError  string `json:"last_error"`
}

// Dashboard 组装首页所需的一次性聚合数据。
//
// 设计目的：首页一次请求拿全，避免前端并行发起五六个请求导致首屏抖动。
// 单次查询量都很小（Top 10 / 待处理列表各上限 20 条），不会拖慢首页。
//
// 参数 ctx 为上下文；mode 为展示口径。返回聚合数据或错误。
func (s *TrafficService) Dashboard(ctx context.Context, mode BytesMode) (*DashboardData, error) {
	if mode == "" {
		mode = BytesModeScaled
	}
	ov, err := s.Overview(ctx, mode)
	if err != nil {
		return nil, err
	}

	out := &DashboardData{Overview: ov}
	now := timeNow()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	// 首页排行统一按「今日」统计，符合「今天谁在跑」的使用直觉。
	for _, dim := range []struct {
		name string
		dst  *[]TrafficRankItem
	}{
		{DimensionRule, &out.TopRules},
		{DimensionUser, &out.TopUsers},
		{DimensionNode, &out.TopNodes},
	} {
		list, err := s.Top(ctx, dim.name, defaultTopLimit, dayStart, now, mode)
		if err != nil {
			return nil, err
		}
		*dim.dst = list
	}

	trend, err := s.Timeseries(ctx, now.Add(-24*time.Hour), now, HourBucket, GroupByDirection, mode)
	if err != nil {
		return nil, err
	}
	out.Trend = trend

	// 同步失败的规则。
	if err := s.app.DB.WithContext(ctx).Model(&model.ForwardRule{}).
		Select("id, name, sync_status, sync_error, user_id").
		Where("sync_status = ?", model.SyncFailed).
		Order("updated_at DESC").Limit(20).
		Scan(&out.SyncFailedRules).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "查询同步失败规则失败")
	}
	if out.SyncFailedRules == nil {
		out.SyncFailedRules = []DashboardRuleBrief{}
	}

	// 离线节点：按最后心跳时间升序，最久没上线的排在最前。
	if err := s.app.DB.WithContext(ctx).Model(&model.Node{}).
		Select("id, name, role, last_seen, last_error").
		Where("online = ?", false).
		Order("last_seen ASC").Limit(20).
		Scan(&out.OfflineNodes).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "查询离线节点失败")
	}
	if out.OfflineNodes == nil {
		out.OfflineNodes = []DashboardNodeBrief{}
	}
	for i := range out.OfflineNodes {
		out.OfflineNodes[i].OfflineFor = offlineSeconds(out.OfflineNodes[i].LastSeen, now)
	}

	// 当前活跃告警（未解决）。
	if err := s.app.DB.WithContext(ctx).Model(&model.AlertHistory{}).
		Where("resolved = ?", false).
		Order("fired_at DESC").Limit(20).
		Find(&out.ActiveAlerts).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "查询活跃告警失败")
	}
	if out.ActiveAlerts == nil {
		out.ActiveAlerts = []model.AlertHistory{}
	}

	out.PendingIssues = len(out.SyncFailedRules) + len(out.OfflineNodes) + len(out.ActiveAlerts)
	return out, nil
}

// offlineSeconds 计算节点已离线秒数。
func offlineSeconds(lastSeen *time.Time, now time.Time) int64 {
	if lastSeen == nil {
		return 0
	}
	d := now.Sub(*lastSeen)
	if d < 0 {
		return 0
	}
	return int64(d.Seconds())
}

// -------------------- 规则级 / 用户级 / 站点级拆解（规格书 6.10） --------------------

// RuleTraffic 是规则级流量拆解。
type RuleTraffic struct {
	RuleID       uint64  `json:"rule_id"`
	Name         string  `json:"name"`
	UserID       uint64  `json:"user_id"`
	Enable       bool    `json:"enable"`
	SyncStatus   string  `json:"sync_status"`
	InboundMult  float64 `json:"inbound_multiplier"`
	OutboundMult float64 `json:"outbound_multiplier"`

	Today     TrafficPeriod `json:"today"`
	Yesterday TrafficPeriod `json:"yesterday"`
	Month     TrafficPeriod `json:"month"`
	Total     TrafficPeriod `json:"total"`

	// Hourly 是今日 24 小时曲线，长度固定 24，缺失的小时补 0。
	Hourly []int64 `json:"hourly"`

	// Lifetime 是规则表上的累计计数器（forward_rules.traffic_in/out），
	// 与 Total 的区别是它由写入时增量累加，读取零成本。
	LifetimeIn  int64 `json:"lifetime_in"`
	LifetimeOut int64 `json:"lifetime_out"`
}

// RuleBreakdown 返回单条规则的流量拆解（今日 / 昨日 / 本月 / 累计 + 今日小时曲线）。
//
// 参数 ctx 为上下文；ruleID 为规则 ID；mode 为展示口径。返回拆解或错误。
func (s *TrafficService) RuleBreakdown(ctx context.Context, ruleID uint64, mode BytesMode) (*RuleTraffic, error) {
	if mode == "" {
		mode = BytesModeScaled
	}
	var rule model.ForwardRule
	if err := s.app.DB.WithContext(ctx).First(&rule, ruleID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, response.New(response.CodeNotFound, "规则不存在")
		}
		return nil, response.Wrap(response.CodeInternal, err, "查询规则失败")
	}

	out := &RuleTraffic{
		RuleID:       rule.ID,
		Name:         rule.Name,
		UserID:       rule.UserID,
		Enable:       rule.Enable,
		SyncStatus:   rule.SyncStatus,
		InboundMult:  rule.InboundMultiplier,
		OutboundMult: rule.OutboundMultiplier,
		LifetimeIn:   rule.TrafficIn,
		LifetimeOut:  rule.TrafficOut,
		Hourly:       make([]int64, 24),
	}

	now := timeNow()
	today := now.Format("2006-01-02")
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")
	monthStart := now.Format("2006-01") + "-01"
	col := mode.Column()

	rows, err := s.queryBuckets(ctx, col, "rule_id = ? AND",
		[]interface{}{today, yesterday, monthStart, ruleID})
	if err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "统计规则流量失败")
	}
	accumulatePeriods(&out.Today, &out.Yesterday, &out.Month, &out.Total,
		rows, today, yesterday, monthStart)

	// 今日小时曲线：只读 0~23 的小时行。
	var hourly []struct {
		Hour     int
		SumBytes int64
	}
	if err := s.app.DB.WithContext(ctx).Model(&model.TrafficLog{}).
		Select("hour AS hour, SUM("+col+") AS sum_bytes").
		Where("date = ? AND hour >= ? AND rule_id = ?", today, 0, ruleID).
		Group("hour").Scan(&hourly).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "统计规则小时曲线失败")
	}
	for _, h := range hourly {
		if h.Hour >= 0 && h.Hour < 24 {
			out.Hourly[h.Hour] = h.SumBytes
		}
	}
	return out, nil
}

// accumulatePeriods 把区间聚合行累加进四个 TrafficPeriod。
//
// 累计（total）包含全部行，与区间桶的选择无关。
func accumulatePeriods(today, yesterday, month, total *TrafficPeriod, rows []bucketRow, todayStr, ydayStr, monthStart string) {
	today.From, today.To = todayStr, todayStr
	yesterday.From, yesterday.To = ydayStr, ydayStr
	month.From, month.To = monthStart, todayStr

	acc := func(p *TrafficPeriod, direction string, sumBytes, sumRaw int64) {
		if direction == model.DirectionOut {
			p.Out += sumBytes
		} else {
			p.In += sumBytes
		}
		p.Total += sumBytes
		p.Raw += sumRaw
	}
	for _, r := range rows {
		switch r.Bucket {
		case "today":
			acc(today, r.Direction, r.SumBytes, r.SumRaw)
			acc(month, r.Direction, r.SumBytes, r.SumRaw)
		case "yesterday":
			acc(yesterday, r.Direction, r.SumBytes, r.SumRaw)
			if ydayStr >= monthStart {
				acc(month, r.Direction, r.SumBytes, r.SumRaw)
			}
		case "month":
			acc(month, r.Direction, r.SumBytes, r.SumRaw)
		}
		acc(total, r.Direction, r.SumBytes, r.SumRaw)
	}
}

// UserTraffic 是用户级流量拆解。
type UserTraffic struct {
	UserID   uint64 `json:"user_id"`
	Username string `json:"username"`
	Nickname string `json:"nickname"`
	Status   int    `json:"status"`

	// Used 是已用流量（用户表上的折算累计值）；Limit 为上限，0 = 不限。
	Used  int64 `json:"used"`
	Limit int64 `json:"limit"`
	// Remaining 为剩余流量；未设上限时为 -1，前端据此显示「不限」。
	Remaining int64 `json:"remaining"`
	// Percent 为使用百分比；未设上限时为 0。
	Percent float64 `json:"percent"`

	Today     TrafficPeriod `json:"today"`
	Yesterday TrafficPeriod `json:"yesterday"`
	Month     TrafficPeriod `json:"month"`
	Total     TrafficPeriod `json:"total"`

	// RuleDistribution 是按规则的流量分布，用于饼图（规格书 6.10 用户级维度）。
	RuleDistribution []TrafficRankItem `json:"rule_distribution"`

	// Rules 是该用户拥有的规则数（含禁用）。
	Rules int `json:"rules"`
}

// UserBreakdown 返回单个用户的流量拆解（已用 / 剩余 / 按规则分布）。
//
// 参数 ctx 为上下文；userID 为用户 ID；mode 为展示口径。返回拆解或错误。
func (s *TrafficService) UserBreakdown(ctx context.Context, userID uint64, mode BytesMode) (*UserTraffic, error) {
	if mode == "" {
		mode = BytesModeScaled
	}
	var user model.User
	if err := s.app.DB.WithContext(ctx).First(&user, userID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, response.New(response.CodeNotFound, "用户不存在")
		}
		return nil, response.Wrap(response.CodeInternal, err, "查询用户失败")
	}
	limit := user.TrafficLimit
	if limit == 0 && user.GroupID > 0 {
		var group model.UserGroup
		err := s.app.DB.WithContext(ctx).Select("traffic_limit").First(&group, user.GroupID).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, response.Wrap(response.CodeInternal, err, "查询用户组流量上限失败")
		}
		limit = group.TrafficLimit
	}

	out := &UserTraffic{
		UserID:    user.ID,
		Username:  user.Username,
		Nickname:  user.Nickname,
		Status:    user.Status,
		Used:      user.TrafficUsed,
		Limit:     limit,
		Remaining: -1,
	}
	if limit > 0 {
		out.Remaining = limit - user.TrafficUsed
		if out.Remaining < 0 {
			out.Remaining = 0
		}
		out.Percent = float64(user.TrafficUsed) * 100 / float64(limit)
	}

	now := timeNow()
	today := now.Format("2006-01-02")
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")
	monthStart := now.Format("2006-01") + "-01"
	col := mode.Column()

	rows, err := s.queryBuckets(ctx, col, "user_id = ? AND",
		[]interface{}{today, yesterday, monthStart, userID})
	if err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "统计用户流量失败")
	}
	accumulatePeriods(&out.Today, &out.Yesterday, &out.Month, &out.Total,
		rows, today, yesterday, monthStart)

	dist, err := s.userRuleDistribution(ctx, userID, mode)
	if err != nil {
		return nil, err
	}
	out.RuleDistribution = truncateRank(dist, 20)
	out.Rules = len(s.userRuleNames(ctx, userID))
	return out, nil
}

// userRuleDistribution 统计某用户各条规则的流量分布（用于饼图）。
//
// 参数 ctx；userID；mode 为展示口径。返回按总量降序的排行（含规则名）或错误。
func (s *TrafficService) userRuleDistribution(ctx context.Context, userID uint64, mode BytesMode) ([]TrafficRankItem, error) {
	col := mode.Column()

	var rows []struct {
		ID        uint64 `gorm:"column:id"`
		Direction string
		SumBytes  int64 `gorm:"column:sum_bytes"`
	}
	err := s.app.DB.WithContext(ctx).Model(&model.TrafficLog{}).
		Select("rule_id AS id, direction AS direction, SUM("+col+") AS sum_bytes").
		Where("hour = ? AND user_id = ? AND rule_id > 0", model.HourDaily, userID).
		Group("rule_id, direction").Scan(&rows).Error
	if err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "统计用户规则分布失败")
	}

	agg := make(map[uint64]*TrafficRankItem)
	for _, r := range rows {
		it := agg[r.ID]
		if it == nil {
			it = &TrafficRankItem{ID: r.ID}
			agg[r.ID] = it
		}
		if r.Direction == model.DirectionOut {
			it.Out += r.SumBytes
		} else {
			it.In += r.SumBytes
		}
		it.Total += r.SumBytes
	}

	list := make([]TrafficRankItem, 0, len(agg))
	for _, it := range agg {
		list = append(list, *it)
	}
	sortRankItems(list)

	names := s.userRuleNames(ctx, userID)
	for i := range list {
		list[i].Name = names[list[i].ID]
	}
	return list, nil
}

// userRuleNames 返回某用户拥有的规则「ID → 名称」映射。
func (s *TrafficService) userRuleNames(ctx context.Context, userID uint64) map[uint64]string {
	out := make(map[uint64]string)
	var rows []struct {
		ID   uint64
		Name string
	}
	if err := s.app.DB.WithContext(ctx).Model(&model.ForwardRule{}).
		Select("id, name").Where("user_id = ?", userID).Scan(&rows).Error; err != nil {
		s.app.Log.Debug("查询用户规则失败", zap.Uint64("user_id", userID), zap.Error(err))
		return out
	}
	for _, r := range rows {
		out[r.ID] = r.Name
	}
	return out
}

// truncateRank 截断排行列表到指定长度。
func truncateRank(list []TrafficRankItem, n int) []TrafficRankItem {
	if n > 0 && len(list) > n {
		return list[:n]
	}
	return list
}

// SiteBreakdown 是全站级拆解（规格书 6.10 全站级）。
type SiteBreakdown struct {
	Overview *TrafficOverview `json:"overview"`
	// TopNodes / TopUsers / TopRules 为累计排行。
	TopNodes []TrafficRankItem `json:"top_nodes"`
	TopUsers []TrafficRankItem `json:"top_users"`
	TopRules []TrafficRankItem `json:"top_rules"`
	// Hourly 为最近 24 小时按小时的站点级曲线。
	Hourly []TrafficSeriesPoint `json:"hourly"`
}

// SiteBreakdownData 返回全站级拆解数据。
//
// 参数 ctx 为上下文；mode 为展示口径。返回拆解或错误。
func (s *TrafficService) SiteBreakdown(ctx context.Context, mode BytesMode) (*SiteBreakdown, error) {
	if mode == "" {
		mode = BytesModeScaled
	}
	ov, err := s.Overview(ctx, mode)
	if err != nil {
		return nil, err
	}
	out := &SiteBreakdown{Overview: ov}

	for _, dim := range []struct {
		name string
		dst  *[]TrafficRankItem
	}{
		{DimensionNode, &out.TopNodes},
		{DimensionUser, &out.TopUsers},
		{DimensionRule, &out.TopRules},
	} {
		list, err := s.Top(ctx, dim.name, defaultTopLimit, time.Time{}, time.Time{}, mode)
		if err != nil {
			return nil, err
		}
		*dim.dst = list
	}

	now := timeNow()
	series, err := s.Timeseries(ctx, now.Add(-24*time.Hour), now, HourBucket, GroupByDirection, mode)
	if err != nil {
		return nil, err
	}
	out.Hourly = series
	return out, nil
}

// -------------------- 在线会话（规格书 4.2.10） --------------------

// SessionInput 是创建或更新会话的输入。
type SessionInput struct {
	UserID   uint64
	RuleID   uint64
	NodeID   uint64
	ClientIP string
	DeviceID string
	// Protocol 取 ws | http | tls | direct | udp。
	Protocol string
	// Delta 为本次连接数变化量（通常 +1 建立、-1 关闭）。
	Delta int
	// Upload / Download 为本次上报的上下行字节增量。
	Upload   int64
	Download int64
}

// OpenSession 记录一个新建连接的会话。
//
// 行为：同一「规则 + IP + 设备 + 协议」重复出现时累加连接数并刷新 LastSeen，
// 而不是新开一行——这样「同一客户端开 10 条连接」只占用一行，便于连接数限制判断。
//
// 参数 ctx 为上下文；in 为会话输入。返回会话 ID 或错误。
func (s *TrafficService) OpenSession(ctx context.Context, in SessionInput) (uint64, error) {
	if in.ClientIP == "" {
		return 0, response.New(response.CodeParamInvalid, "客户端 IP 不能为空")
	}
	if in.Delta == 0 {
		in.Delta = 1
	}
	now := timeNow()

	key := sessionKey(in.RuleID, in.ClientIP, in.DeviceID, in.Protocol)

	s.sessMu.Lock()
	st := s.sessions[key]
	if st == nil {
		st = &sessionState{
			RuleID:    in.RuleID,
			UserID:    in.UserID,
			NodeID:    in.NodeID,
			ClientIP:  in.ClientIP,
			DeviceID:  in.DeviceID,
			Protocol:  in.Protocol,
			StartedAt: now,
		}
		s.sessions[key] = st
	}
	st.ConnCount += in.Delta
	if st.ConnCount < 0 {
		st.ConnCount = 0
	}
	st.LastSeen = now
	st.Upload += in.Upload
	st.Download += in.Download
	if st.UserID == 0 {
		st.UserID = in.UserID
	}
	snapshot := *st
	s.sessMu.Unlock()

	// 取局部变量地址：循环中每次都会重新定义，不会发生「所有行指向同一个值」的问题。
	connCount := snapshot.ConnCount

	// 落库：sessions 表只服务于展示与统计，写入失败不影响转发链路。
	rec := model.Session{
		UserID:       snapshot.UserID,
		RuleID:       snapshot.RuleID,
		NodeID:       snapshot.NodeID,
		ClientIP:     util.Truncate(snapshot.ClientIP, 64),
		ClientIPHash: util.HashIP(snapshot.ClientIP, s.app.Config.SecretKey),
		DeviceID:     util.Truncate(snapshot.DeviceID, 128),
		// 取地址写入：模型层用指针才能把 0 真正存进库（见 model.Session.ConnCount 的说明）。
		ConnCount: &connCount,
		Protocol:  util.Truncate(snapshot.Protocol, 16),
		StartedAt: snapshot.StartedAt,
		LastSeen:  snapshot.LastSeen,
		Upload:    snapshot.Upload,
		Download:  snapshot.Download,
	}
	if snapshot.ID > 0 {
		rec.ID = snapshot.ID
	}

	if err := s.app.DB.WithContext(ctx).Save(&rec).Error; err != nil {
		s.app.Log.Warn("写入会话记录失败", zap.Error(err))
		return snapshot.ID, nil
	}

	s.sessMu.Lock()
	if cur := s.sessions[key]; cur != nil {
		cur.ID = rec.ID
	}
	s.sessMu.Unlock()
	return rec.ID, nil
}

// TouchSession 刷新会话的最后活跃时间与流量增量。
//
// 参数 ctx；ruleID / ip / deviceID / protocol 用于定位会话；
// upload / download 为本次增量。返回错误。
func (s *TrafficService) TouchSession(ctx context.Context, ruleID uint64, ip, deviceID, protocol string, upload, download int64) error {
	key := sessionKey(ruleID, ip, deviceID, protocol)
	now := timeNow()

	s.sessMu.Lock()
	st := s.sessions[key]
	if st == nil {
		s.sessMu.Unlock()
		return nil
	}
	st.LastSeen = now
	st.Upload += upload
	st.Download += download
	st.ConnCount++
	id := st.ID
	s.sessMu.Unlock()

	if id == 0 {
		return nil
	}
	return s.app.DB.WithContext(ctx).Model(&model.Session{}).Where("id = ?", id).
		Updates(map[string]interface{}{
			"last_seen":  now,
			"upload":     gorm.Expr("upload + ?", upload),
			"download":   gorm.Expr("download + ?", download),
			"conn_count": gorm.Expr("conn_count + 1"),
		}).Error
}

// CloseSession 关闭一条连接，连接数减到 0 时把会话从内存移除。
//
// 数据库行保留（供统计与展示），只更新连接数与最后活跃时间。
//
// 参数 ctx；定位参数同上。返回错误。
func (s *TrafficService) CloseSession(ctx context.Context, ruleID uint64, ip, deviceID, protocol string) error {
	key := sessionKey(ruleID, ip, deviceID, protocol)
	now := timeNow()

	s.sessMu.Lock()
	st := s.sessions[key]
	if st == nil {
		s.sessMu.Unlock()
		return nil
	}
	st.ConnCount--
	if st.ConnCount <= 0 {
		st.ConnCount = 0
		// 连接归零后立刻从热态移除，避免限制判断把「已断开」算作活跃。
		delete(s.sessions, key)
	}
	st.LastSeen = now
	id := st.ID
	s.sessMu.Unlock()

	if id == 0 {
		return nil
	}
	return s.app.DB.WithContext(ctx).Model(&model.Session{}).Where("id = ?", id).
		Updates(map[string]interface{}{
			"last_seen":  now,
			"conn_count": gorm.Expr("CASE WHEN conn_count > 0 THEN conn_count - 1 ELSE 0 END"),
		}).Error
}

// RuleSessions 返回某条规则当前活跃的会话列表。
//
// 参数 ctx；ruleID 为规则 ID。返回按最后活跃时间降序排列的会话或错误。
func (s *TrafficService) RuleSessions(ctx context.Context, ruleID uint64) ([]model.Session, error) {
	items := make([]model.Session, 0)
	if err := s.app.DB.WithContext(ctx).Model(&model.Session{}).
		Where("rule_id = ? AND conn_count > 0", ruleID).
		Order("last_seen DESC").Limit(500).
		Find(&items).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "查询规则会话失败")
	}
	return items, nil
}

// UserSessions 返回某个用户当前活跃的会话列表。
//
// 参数 ctx；userID 为用户 ID。返回会话列表或错误。
func (s *TrafficService) UserSessions(ctx context.Context, userID uint64) ([]model.Session, error) {
	items := make([]model.Session, 0)
	if err := s.app.DB.WithContext(ctx).Model(&model.Session{}).
		Where("user_id = ? AND conn_count > 0", userID).
		Order("last_seen DESC").Limit(500).
		Find(&items).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "查询用户会话失败")
	}
	return items, nil
}

// OnlineUsers 统计在线用户数。
//
// 判据：最近 onlineWindow 内仍有 last_seen 的会话所涉及的不同用户。
// userID 为 0 时统计全站，否则只判断该用户是否在线（返回 1 或 0）。
//
// 参数 ctx；userID。返回数量或错误。
func (s *TrafficService) OnlineUsers(ctx context.Context, userID uint64) (int, error) {
	since := timeNow().Add(-onlineWindow)
	q := s.app.DB.WithContext(ctx).Model(&model.Session{}).Where("last_seen >= ?", since)
	if userID > 0 {
		var count int64
		if err := q.Where("user_id = ?", userID).Limit(1).Count(&count).Error; err != nil {
			return 0, response.Wrap(response.CodeInternal, err, "统计在线用户失败")
		}
		if count > 0 {
			return 1, nil
		}
		return 0, nil
	}

	var count int64
	if err := q.Distinct("user_id").Count(&count).Error; err != nil {
		return 0, response.Wrap(response.CodeInternal, err, "统计在线用户失败")
	}
	return int(count), nil
}

// onlineWindow 判定「用户在线」的时间窗口。
//
// 取 5 分钟：与 IP 限制的滑动窗口一致，避免两处对「在线」的理解不一致。
const onlineWindow = 5 * time.Minute

// ActiveConnections 返回当前活跃连接数（内存热态之和）。
//
// 该值仅覆盖本进程见过的连接，进程重启后从 0 重新累计，
// 因此它是「展示用近似值」而不是计费依据。
func (s *TrafficService) ActiveConnections() int {
	s.sessMu.RLock()
	defer s.sessMu.RUnlock()
	total := 0
	for _, st := range s.sessions {
		total += st.ConnCount
	}
	return total
}

// AccountConnections 返回某个维度（规则或用户）的当前活跃连接数。
//
// 参数 ruleID 或 userID 二者取其一（都为 0 时返回全站连接数）。
func (s *TrafficService) AccountConnections(ruleID, userID uint64) int {
	s.sessMu.RLock()
	defer s.sessMu.RUnlock()
	total := 0
	for _, st := range s.sessions {
		if ruleID > 0 && st.RuleID != ruleID {
			continue
		}
		if userID > 0 && st.UserID != userID {
			continue
		}
		total += st.ConnCount
	}
	return total
}

// RebuildSessions 从 sessions 表重建内存热态，供进程启动时调用。
//
// 参数 ctx 为上下文。返回错误（失败仅记为告警，不阻断启动）。
func (s *TrafficService) RebuildSessions(ctx context.Context) error {
	since := timeNow().Add(-sessionIdleTimeout)
	var rows []model.Session
	if err := s.app.DB.WithContext(ctx).Model(&model.Session{}).
		Where("last_seen >= ? AND conn_count > 0", since).
		Limit(SessionCap).Find(&rows).Error; err != nil {
		return err
	}

	s.sessMu.Lock()
	s.sessions = make(map[string]*sessionState, len(rows))
	for i := range rows {
		r := rows[i]
		// ConnCount 在模型层是指针（以便能写入 0），这里统一解引用为 int。
		conn := 0
		if r.ConnCount != nil {
			conn = *r.ConnCount
		}
		key := sessionKey(r.RuleID, r.ClientIP, r.DeviceID, r.Protocol)
		if _, ok := s.sessions[key]; ok {
			s.sessions[key].ConnCount += conn
			continue
		}
		s.sessions[key] = &sessionState{
			ID:        r.ID,
			RuleID:    r.RuleID,
			UserID:    r.UserID,
			NodeID:    r.NodeID,
			ClientIP:  r.ClientIP,
			DeviceID:  r.DeviceID,
			Protocol:  r.Protocol,
			ConnCount: conn,
			Upload:    r.Upload,
			Download:  r.Download,
			StartedAt: r.StartedAt,
			LastSeen:  r.LastSeen,
		}
	}
	s.sessMu.Unlock()
	return nil
}

// CleanupSessions 清理垃圾会话，返回删除的行数。
//
// 「垃圾」包含两类（规格书 4.2.10 的容量控制要求）：
//  1. 空闲超过 sessionIdleTimeout 且连接数已归零的会话；
//  2. 行数超过 SessionCap 时按 last_seen 淘汰的最旧记录。
//
// 参数 ctx 为上下文。返回删除行数与错误。
func (s *TrafficService) CleanupSessions(ctx context.Context) (int64, error) {
	db := s.app.DB.WithContext(ctx)
	now := timeNow()

	// 1. 内存热态同步淘汰。
	s.sessMu.Lock()
	for k, st := range s.sessions {
		if st.ConnCount <= 0 && now.Sub(st.LastSeen) > sessionIdleTimeout {
			delete(s.sessions, k)
		}
	}
	s.sessMu.Unlock()

	// 2. 数据库层面的空闲行清理。
	idleBefore := now.Add(-sessionIdleTimeout)
	res := db.Where("last_seen < ? AND conn_count <= 0", idleBefore).Delete(&model.Session{})
	if res.Error != nil {
		return 0, response.Wrap(response.CodeInternal, res.Error, "清理空闲会话失败")
	}
	deleted := res.RowsAffected

	// 3. 超容量淘汰：找出应当保留的第 SessionCap 行的 last_seen 作为水位线。
	var count int64
	if err := db.Model(&model.Session{}).Count(&count).Error; err != nil {
		return deleted, response.Wrap(response.CodeInternal, err, "统计会话行数失败")
	}
	if count <= SessionCap {
		return deleted, nil
	}

	var cutoff struct{ LastSeen *time.Time }
	err := db.Model(&model.Session{}).
		Select("last_seen").Order("last_seen DESC").
		Offset(SessionCap - 1).Limit(1).Scan(&cutoff).Error
	if err != nil {
		return deleted, response.Wrap(response.CodeInternal, err, "计算会话淘汰水位失败")
	}
	if cutoff.LastSeen == nil {
		return deleted, nil
	}

	res = db.Where("last_seen < ?", *cutoff.LastSeen).Delete(&model.Session{})
	if res.Error != nil {
		return deleted, response.Wrap(response.CodeInternal, res.Error, "淘汰最旧会话失败")
	}
	deleted += res.RowsAffected
	s.app.Log.Info("已淘汰超容量的会话记录",
		zap.Int64("deleted", res.RowsAffected), zap.Int64("cap", int64(SessionCap)))
	return deleted, nil
}

// CleanupTraffic 清理超出保留期的小时明细行，返回删除行数。
//
// 规格书 6.10：小时粒度保留 30 天（TrafficDetailKeepDays 可配置），天粒度永久保留。
// 因此这里**只删 hour >= 0 的行**，绝不触碰 hour = -1 的按天聚合行。
//
// 参数 ctx 为上下文。返回删除行数与错误。
func (s *TrafficService) CleanupTraffic(ctx context.Context) (int64, error) {
	days := s.app.Config.TrafficDetailKeepDays
	if days <= 0 {
		days = 30
	}
	cutoff := timeNow().AddDate(0, 0, -days).Format("2006-01-02")

	res := s.app.DB.WithContext(ctx).Where("date < ? AND hour >= ?", cutoff, 0).
		Delete(&model.TrafficLog{})
	if res.Error != nil {
		return 0, response.Wrap(response.CodeInternal, res.Error, "清理流量小时明细失败")
	}
	return res.RowsAffected, nil
}
