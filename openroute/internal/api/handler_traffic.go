package api

import (
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/openroute/openroute/internal/api/middleware"

	"github.com/openroute/openroute/internal/api/response"
	"github.com/openroute/openroute/internal/app"
	"github.com/openroute/openroute/internal/model"
)

// 本文件实现规格书 8.12「流量统计接口」与 6.10 的三个统计维度的读取入口。
//
// 口径约定（规格书 6.10）：
//
//   - 所有接口都支持 `?raw=true` 切换为「实际字节」，缺省展示「乘倍率后的字节」；
//   - 时间粒度 interval 取 hour（保留 30 天）或 day（永久保留）；
//   - 支持按时间范围、用户、规则、节点、方向任意组合筛选。
//
// 本文件只做统计，不涉及任何充值、消费、订单或套餐语义。

// trafficIntervalParam 解析统计粒度参数 interval。
//
// 参数 c 为上下文。返回 "hour" 或 "day"，非法或缺失时回退为 "hour"。
// 这里刻意不允许其它取值：service 的 Timeseries 会按 granularity 选择
// 「读小时行」还是「读天行」，含糊的取值只会让图表的桶宽与数据源不一致。
func trafficIntervalParam(c *gin.Context) string {
	if strings.ToLower(strings.TrimSpace(c.Query("interval"))) == app.DayBucket {
		return app.DayBucket
	}
	return app.HourBucket
}

// trafficFilter 解析流量明细的筛选条件（规格书 6.10：任意组合筛选）。
//
// 支持参数：from / to / user_id / rule_id / node_id / direction / interval
// 以及 page / page_size。
// 时间缺省区间为最近 7 天（天粒度）——明细表在小时粒度下一天就有上百行，
// 默认区间过大会让首屏的分页查询变慢。
//
// 参数 c 为上下文。返回组装好的过滤条件。
func trafficFilter(c *gin.Context) app.TrafficFilter {
	page, pageSize := pageParams(c)

	from, to := timeRange(c, 7*24*time.Hour, 0)
	interval := strings.ToLower(strings.TrimSpace(c.Query("interval")))
	if interval != app.HourBucket && interval != app.DayBucket {
		// 明细默认按天：同一次写入会同时产生小时行与天行，
		// 默认读天行可以避免前端一进来就看到重复计数的错觉。
		interval = app.DayBucket
	}

	direction := strings.ToLower(strings.TrimSpace(c.Query("direction")))
	if direction != "in" && direction != "out" {
		direction = ""
	}

	filter := app.TrafficFilter{
		From:      from,
		To:        to,
		UserID:    queryUint64(c, "user_id", 0),
		RuleID:    queryUint64(c, "rule_id", 0),
		NodeID:    queryUint64(c, "node_id", 0),
		Direction: direction,
		Interval:  interval,
		Page:      page,
		PageSize:  pageSize,
	}
	if !middleware.IsAdmin(c) {
		filter.UserID = middleware.CurrentUserID(c)
	}
	return filter
}

// TrafficOverview 返回全站流量概览（规格书 8.12 GET /traffic/overview）。
//
// 响应包含今日 / 昨日 / 本月 / 累计四个区间的流量、在线用户数、
// 在线与总节点数、规则数与当前活跃连接数，供首页卡片一次渲染完成。
func (h *Handlers) TrafficOverview(c *gin.Context) {
	if !middleware.IsAdmin(c) {
		ov, err := h.ownTrafficOverview(c)
		if err != nil {
			response.Fail(c, err)
			return
		}
		response.OK(c, ov)
		return
	}
	ov, err := h.app.Traffic.Overview(c.Request.Context(), bytesMode(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, ov)
}

// TrafficTimeseries 返回流量时间序列（规格书 8.12 GET /traffic/timeseries）。
//
// 查询参数：
//
//	from / to   时间区间，缺省为「最近 24 小时」
//	interval    hour | day，默认 hour
//	group_by    user | rule | node | direction，默认 direction
//	raw         是否展示实际字节
//
// 另支持按 user_id / rule_id / node_id 过滤具体的维度值，
// 这样「某个用户的曲线」不必拉到全站数据再在前端裁剪。
func (h *Handlers) TrafficTimeseries(c *gin.Context) {
	ctx := c.Request.Context()
	from, to := timeRange(c, 24*time.Hour, 0)
	if !to.After(from) {
		badRequest(c, "to", "结束时间必须晚于开始时间")
		return
	}
	interval := trafficIntervalParam(c)
	groupBy := normalizeTrafficGroupBy(c.Query("group_by"))
	mode := bytesMode(c)

	series, err := h.app.Traffic.TimeseriesFiltered(ctx, from, to, interval, groupBy, mode, trafficFilter(c))
	if err != nil {
		response.Fail(c, err)
		return
	}

	if series == nil {
		series = []app.TrafficSeriesPoint{}
	}

	response.OK(c, gin.H{
		"items":      series,
		"from":       from.Format(time.RFC3339),
		"to":         to.Format(time.RFC3339),
		"interval":   interval,
		"group_by":   groupBy,
		"bytes_mode": mode,
	})
}

// normalizeTrafficGroupBy 收敛分组维度取值。
//
// 参数 v 为原始参数。返回 user / rule / node / direction 之一，非法值回退 direction。
func normalizeTrafficGroupBy(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case app.GroupByUser:
		return app.GroupByUser
	case app.GroupByRule:
		return app.GroupByRule
	case app.GroupByNode:
		return app.GroupByNode
	default:
		return app.GroupByDirection
	}
}

// filterSeriesByGroup 只保留分组键等于 key 的序列点。
//
// 参数 series 为原始序列；key 为分组键（维度 ID 的字符串形式）。
// 返回裁剪后的序列；原序列为空时返回空切片而不是 nil。
func filterSeriesByGroup(series []app.TrafficSeriesPoint, key string) []app.TrafficSeriesPoint {
	out := make([]app.TrafficSeriesPoint, 0, len(series))
	for i := range series {
		if series[i].Group == key {
			out = append(out, series[i])
		}
	}
	return out
}

// TrafficTop 返回 Top-N 排行（规格书 8.12 GET /traffic/top）。
//
// 查询参数：
//
//	dimension  必填，取 user | rule | node
//	limit      默认 10，最大 100（再大对「排行」这个用途没有意义）
//	from / to  可选的统计区间，缺省为「累计」
//	raw        是否展示实际字节
func (h *Handlers) TrafficTop(c *gin.Context) {
	dimension := strings.ToLower(strings.TrimSpace(c.Query("dimension")))
	if dimension != app.DimensionUser && dimension != app.DimensionRule && dimension != app.DimensionNode {
		badRequest(c, "dimension", "必须是 user / rule / node 三者之一")
		return
	}

	limit := queryInt(c, "limit", 10)
	if limit <= 0 {
		limit = 10
	}
	// 上限 100：service 侧的上限是 200，但面板上的排行卡片放不下更多，
	// 在这里收得更紧可以省掉一次无意义的全表聚合。
	if limit > 100 {
		limit = 100
	}

	// from / to 都缺省时传零值，service 会按「累计」统计（规格书 6.10 全站级）。
	var from, to time.Time
	if v, ok := parseTimeQuery(c, "from"); ok {
		from = v
	}
	if v, ok := parseTimeQuery(c, "to"); ok {
		to = v
	}

	list, err := h.app.Traffic.TopFiltered(c.Request.Context(), dimension, limit, from, to, bytesMode(c), trafficFilter(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	if list == nil {
		list = []app.TrafficRankItem{}
	}
	response.OK(c, gin.H{
		"items":      list,
		"dimension":  dimension,
		"limit":      limit,
		"bytes_mode": bytesMode(c),
	})
}

// TrafficExport 导出流量明细 CSV（规格书 8.12 GET /traffic/export）。
//
// 筛选条件与明细查询一致；导出会忽略分页参数并按服务内部的行数上限截断
// （见 app.maxCSVExportRows），避免一次导出把内存打满。
//
// 注意：一旦开始写响应体就无法再返回 JSON 错误，因此 service 的错误一律
// 记日志并提前结束——响应头尚未发送时仍可正常返回错误体。
func (h *Handlers) TrafficExport(c *gin.Context) {
	ctx := c.Request.Context()
	filter := trafficFilter(c)

	csvHeader(c, "traffic.csv")
	if err := h.app.Traffic.ExportCSV(ctx, filter, c.Writer); err != nil {
		h.log.Warn("导出流量 CSV 失败：" + err.Error())
		response.Fail(c, err)
		return
	}
}

// Dashboard 返回首页仪表盘聚合数据（规格书 8.12 GET /traffic/dashboard）。
//
// 设计目的：首页一次请求拿全概览、排行、趋势与待处理事项，
// 避免前端并行发起五六个请求导致首屏抖动。
func (h *Handlers) Dashboard(c *gin.Context) {
	if !middleware.IsAdmin(c) {
		ctx := c.Request.Context()
		ov, err := h.ownTrafficOverview(c)
		if err != nil {
			response.Fail(c, err)
			return
		}
		filter := app.TrafficFilter{UserID: middleware.CurrentUserID(c)}
		now := time.Now().UTC()
		trend, err := h.app.Traffic.TimeseriesFiltered(ctx, now.Add(-24*time.Hour), now, app.HourBucket, app.GroupByDirection, bytesMode(c), filter)
		if err != nil {
			response.Fail(c, err)
			return
		}
		top, err := h.app.Traffic.TopFiltered(ctx, app.DimensionRule, 10, now.Truncate(24*time.Hour), now, bytesMode(c), filter)
		if err != nil {
			response.Fail(c, err)
			return
		}
		response.OK(c, &app.DashboardData{Overview: ov, Trend: trend, TopRules: top, TopUsers: []app.TrafficRankItem{}, TopNodes: []app.TrafficRankItem{}, SyncFailedRules: []app.DashboardRuleBrief{}, OfflineNodes: []app.DashboardNodeBrief{}, ActiveAlerts: []model.AlertHistory{}})
		return
	}
	data, err := h.app.Traffic.Dashboard(c.Request.Context(), bytesMode(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, data)
}

func (h *Handlers) ownTrafficOverview(c *gin.Context) (*app.TrafficOverview, error) {
	uid := middleware.CurrentUserID(c)
	user, err := h.app.Traffic.UserBreakdown(c.Request.Context(), uid, bytesMode(c))
	if err != nil {
		return nil, err
	}
	ov := &app.TrafficOverview{Today: user.Today, Yesterday: user.Yesterday, Month: user.Month, Total: user.Total, TotalRules: user.Rules, BytesMode: bytesMode(c), Timezone: "UTC"}
	var active int64
	if err := h.app.DB.WithContext(c.Request.Context()).Model(&model.ForwardRule{}).Where("user_id = ? AND enable = ?", uid, true).Count(&active).Error; err != nil {
		return nil, err
	}
	ov.ActiveRules = int(active)
	return ov, nil
}
