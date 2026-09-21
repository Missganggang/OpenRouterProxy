package api

import (
	"time"

	"github.com/gin-gonic/gin"

	"github.com/openroute/openroute/internal/api/middleware"
	"github.com/openroute/openroute/internal/api/response"
	"github.com/openroute/openroute/internal/app"
	"github.com/openroute/openroute/internal/model"
	"github.com/openroute/openroute/internal/util"
)

// 本文件实现规格书 8.13 的探针与监控接口（规格书 6.11 的数据来源）。
//
// 约定：探针关闭（enable-probe: false）时链路仍然可用，只是没有新样本，
// 因此这些接口照样返回 200 与空列表，让前端展示「探针未开启」
// 而不是把整页变成错误页。

// probeSparklinePoints 是全局监控页每张卡片的曲线点数。
//
// 取 30：按心跳间隔 10 秒算约 5 分钟，与规格书 6.11
// 「节点详情页 5 分钟粒度的实时曲线」的观察窗口一致，
// 同时保证几十个节点同屏时响应体不会过大。
const probeSparklinePoints = 30

// ProbeOverview 返回全部节点的监控快照（规格书 8.13 GET /probe/overview）。
//
// 每张卡片包含：节点基本信息、最新指标、健康度评分与迷你曲线。
// 前端全局监控页直接按返回顺序渲染网格。
func (h *Handlers) ProbeOverview(c *gin.Context) {
	ctx := c.Request.Context()

	items, err := h.app.Probe.Overview(ctx, probeSparklinePoints)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if items == nil {
		items = []app.NodeOverview{}
	}

	// 汇总计数：即使前端只看概览条也知道整体健康情况。
	online, unhealthy := 0, 0
	for i := range items {
		if items[i].Online {
			online++
		}
		// 阈值与 app 包的健康度标尺保持一致（< 60 标红）。
		if items[i].HealthScore < app.UnhealthyThreshold {
			unhealthy++
		}
	}

	response.OK(c, gin.H{
		"items":          items,
		"total":          len(items),
		"online":         online,
		"offline":        len(items) - online,
		"unhealthy":      unhealthy,
		"probe_enabled":  h.app.Config.EnableProbe,
		"keep_days":      h.app.Config.ProbeKeepDays,
		"sparkline_size": probeSparklinePoints,
		"generated_at":   time.Now().UTC().Format(time.RFC3339),
	})
}

// ProbeNode 返回单个节点的探针曲线与实时指标（规格书 8.13 GET /probe/nodes/:id）。
//
// 参数：?from=&to=&interval=，interval ∈ 1h|1d|7d（也接受 auto / raw / 5m）。
// 缺省范围为最近 1 小时。
func (h *Handlers) ProbeNode(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "节点 ID 必须是正整数")
		return
	}
	interval, ok := probeIntervalParam(c)
	if !ok {
		return
	}
	from, to := timeRange(c, time.Hour, 0)

	ctx := c.Request.Context()
	n, err := h.app.Node.Get(ctx, id)
	if err != nil {
		response.Fail(c, err)
		return
	}

	points, err := h.app.Probe.Metrics(ctx, id, from, to, interval)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if points == nil {
		points = []app.ProbePoint{}
	}

	// 实时指标可能为空（新节点尚未上报样本），此时保持 null，
	// 前端展示「暂无数据」而不是画一条零值直线。
	realtime, err := h.app.Probe.Realtime(id)
	if err != nil {
		response.Fail(c, err)
		return
	}

	response.OK(c, gin.H{
		"node_id":       id,
		"node_name":     n.Name,
		"role":          n.Role,
		"online":        n.Online,
		"from":          from.Format(time.RFC3339),
		"to":            to.Format(time.RFC3339),
		"interval":      interval,
		"points":        points,
		"samples":       len(points),
		"realtime":      realtime,
		"probe_enabled": h.app.Config.EnableProbe,
	})
}

// ProbeCleanup 手动触发探针数据清理（规格书 8.13 / 6.11 保留策略）。
//
// 保留天数固定取配置的 probe-keep-days：这是「保留策略」的唯一事实来源，
// 若允许请求方随意指定，一次误调用就可能把历史曲线全部删掉。
// 因此不接受任何查询参数覆盖，需要调整策略请改 config.yml。
func (h *Handlers) ProbeCleanup(c *gin.Context) {
	ctx := c.Request.Context()
	keepDays := h.app.Config.ProbeKeepDays

	deleted, err := h.app.Probe.Cleanup(ctx, keepDays)
	if err != nil {
		response.Fail(c, err)
		return
	}

	uid, un, _, _ := middleware.CurrentUser(c)
	h.app.Audit.Write(ctx, app.AuditEntry{
		UserID:    uid,
		Username:  un,
		Action:    model.ActionDelete,
		Resource:  "probe_metrics",
		IP:        util.ClientIP(c.Request),
		UserAgent: c.GetHeader("User-Agent"),
		Result:    model.ResultSuccess,
		After: gin.H{
			"keep_days": keepDays,
			"deleted":   deleted,
		},
		Message: "清理超过保留期的探针数据，保留 " + itoa(keepDays) + " 天，删除 " + itoa(int(deleted)) + " 行",
	})

	response.OKMsg(c, "探针数据已清理", gin.H{
		"keep_days":    keepDays,
		"deleted_rows": deleted,
	})
}
