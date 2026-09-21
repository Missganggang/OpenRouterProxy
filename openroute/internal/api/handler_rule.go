package api

import (
	"encoding/csv"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/openroute/openroute/internal/api/middleware"
	"github.com/openroute/openroute/internal/api/response"
	"github.com/openroute/openroute/internal/app"
	"github.com/openroute/openroute/internal/model"
)

// 本文件实现规格书 8.9「转发规则接口」的全部 handler，
// 以及规则子资源（流量、会话）的读取入口。
//
// 分层纪律（规格书 2.3）：handler 只做「解析参数 → 调用 service → 输出响应」，
// 业务校验一律留在 app.RuleService 中。
//
// 明确的非目标：本文件不涉及任何支付、订单、套餐、许可证或域名绑定逻辑；
// 规则上的 traffic_in / traffic_out 只用于统计展示。

// ruleSortWhitelist 是规则列表允许排序的字段白名单。
//
// 只有白名单内的字段才会进入 ORDER BY，这是防止 SQL 注入的第一道闸门
// （service 侧还会对取值再做一次收敛）。
var ruleSortWhitelist = map[string]bool{
	"created_at":  true,
	"updated_at":  true,
	"name":        true,
	"listen_port": true,
	"traffic_in":  true,
	"traffic_out": true,
	"sync_status": true,
}

// queryBoolValue 是 queryBool 的「只要真值」便捷版本。
//
// 参数缺失、非法或显式传 false 时都返回 false。
func queryBoolValue(c *gin.Context, name string) bool {
	v := queryBool(c, name)
	return v != nil && *v
}

// ruleListFilter 解析规则列表的全部过滤条件（规格书 8.9 的查询参数）。
//
// 支持参数：
//
//	user_id / rule_group_id / inbound_group_id / outbound_group_id
//	sync_status / enable / keyword
//	only_sub_rules / exclude_sub_rules / parent_id（SNI 分流页使用）
//	page / page_size / sort / order
//
// 非管理员只能查看自己名下的规则：即使显式传了别人的 user_id，
// 也会被无声地收敛为「查自己」，避免普通用户改一个查询参数就读到全站规则。
//
// 参数 c 为 Gin 上下文。返回组装好的过滤条件。
func ruleListFilter(c *gin.Context) app.RuleListFilter {
	page, pageSize := pageParams(c)
	sortBy, order := sortParams(c, ruleSortWhitelist, "created_at", "desc")

	filter := app.RuleListFilter{
		UserID:          queryUint64(c, "user_id", 0),
		RuleGroupID:     queryUint64(c, "rule_group_id", 0),
		InboundGroupID:  queryUint64(c, "inbound_group_id", 0),
		OutboundGroupID: queryUint64(c, "outbound_group_id", 0),
		SyncStatus:      strings.TrimSpace(c.Query("sync_status")),
		Enable:          queryBool(c, "enable"),
		Keyword:         strings.TrimSpace(c.Query("keyword")),
		Page:            page,
		PageSize:        pageSize,
		SortBy:          sortBy,
		Order:           order,
		OnlySubRules:    queryBoolValue(c, "only_sub_rules"),
		ExcludeSubRules: queryBoolValue(c, "exclude_sub_rules"),
		ParentID:        queryUint64(c, "parent_id", 0),
	}
	if !middleware.IsAdmin(c) {
		filter.UserID = middleware.CurrentUserID(c)
	}
	return filter
}

// ruleCanAccess 判断当前用户是否有权访问指定归属的规则。
//
// 管理员可访问全部规则；普通用户只能访问自己名下的规则。
// 无权限时返回 false，调用方应输出 40301。
func ruleCanAccess(c *gin.Context, ownerID uint64) bool {
	if middleware.IsAdmin(c) {
		return true
	}
	return middleware.CurrentUserID(c) == ownerID
}

// ruleAudit 写入一条规则相关的审计记录。
//
// 参数 c 为上下文；action 为动作常量；id 为规则 ID；msg 为人话描述；
// before / after 为变更快照（无则传 nil）。
func (h *Handlers) ruleAudit(c *gin.Context, action string, id uint64, msg string, before, after interface{}) {
	uid, un, _, _ := middleware.CurrentUser(c)
	h.app.Audit.Write(c.Request.Context(), app.AuditEntry{
		UserID:     uid,
		Username:   un,
		Action:     action,
		Resource:   "forward_rule",
		ResourceID: id,
		Before:     before,
		After:      after,
		Message:    msg,
	})
}

// -------------------- 列表与增删改 --------------------

// ListForwardRules 返回转发规则列表（规格书 8.9 GET /forward-rules）。
//
// 支持规格书列出的全部筛选参数与分页、排序参数；非管理员只返回自己名下的规则。
func (h *Handlers) ListForwardRules(c *gin.Context) {
	filter := ruleListFilter(c)
	items, total, err := h.app.Rule.List(c.Request.Context(), filter)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if items == nil {
		items = []app.RuleListItem{}
	}
	response.List(c, items, filter.Page, filter.PageSize, total)
}

// CreateForwardRule 创建转发规则（规格书 8.9 POST /forward-rules）。
//
// 请求体与规格书 8.9 的「创建规则请求」一致；倍率等字段用指针区分
// 「未传」与「显式传 0」。创建成功后写一条审计记录。
func (h *Handlers) CreateForwardRule(c *gin.Context) {
	var in app.RuleInput
	if err := c.ShouldBindJSON(&in); err != nil {
		badRequest(c, "body", err.Error())
		return
	}
	// 普通用户只能把规则建在自己名下，传别人的 user_id 会被覆盖掉。
	if !middleware.IsAdmin(c) {
		in.UserID = middleware.CurrentUserID(c)
	}

	rule, err := h.app.Rule.Create(c.Request.Context(), in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	h.ruleAudit(c, model.ActionCreate, rule.ID, "创建转发规则 "+rule.Name, nil, nil)
	response.OK(c, rule)
}

// GetForwardRule 返回单条规则详情（规格书 8.9 GET /forward-rules/:id）。
//
// 普通用户只能读取自己名下的规则，否则返回 40301。
func (h *Handlers) GetForwardRule(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "规则 ID 必须是正整数")
		return
	}
	item, err := h.app.Rule.Get(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if !ruleCanAccess(c, item.UserID) {
		response.Fail(c, response.New(response.CodeForbidden, "只能查看自己名下的转发规则"))
		return
	}
	response.OK(c, item)
}

// UpdateForwardRule 更新转发规则（规格书 8.9 PUT /forward-rules/:id）。
//
// 更新后规则会被 service 标记为「未同步」，等待同步服务重新下发。
func (h *Handlers) UpdateForwardRule(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "规则 ID 必须是正整数")
		return
	}
	// 先取一次做归属判断：越权更新必须在写库之前拦下。
	current, err := h.app.Rule.Get(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if !ruleCanAccess(c, current.UserID) {
		response.Fail(c, response.New(response.CodeForbidden, "只能修改自己名下的转发规则"))
		return
	}

	var in app.RuleInput
	if err := c.ShouldBindJSON(&in); err != nil {
		badRequest(c, "body", err.Error())
		return
	}
	if !middleware.IsAdmin(c) {
		// 传 0 表示「归属不变」，普通用户不能把规则转到别人名下。
		in.UserID = 0
	}

	rule, err := h.app.Rule.Update(c.Request.Context(), id, in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	h.ruleAudit(c, model.ActionUpdate, id, "更新转发规则 "+rule.Name,
		gin.H{
			"name":           current.Name,
			"listen_port":    current.ListenPort,
			"inbound_group":  current.InboundGroupID,
			"outbound_group": current.OutboundGroupID,
			"in_multiplier":  current.InboundMultiplier,
			"out_multiplier": current.OutboundMultiplier,
		}, rule)
	response.OK(c, rule)
}

// DeleteForwardRule 删除转发规则（规格书 8.9 DELETE /forward-rules/:id）。
//
// 删除主规则时其名下的子规则会被 service 连带删除。
func (h *Handlers) DeleteForwardRule(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "规则 ID 必须是正整数")
		return
	}
	current, err := h.app.Rule.Get(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if !ruleCanAccess(c, current.UserID) {
		response.Fail(c, response.New(response.CodeForbidden, "只能删除自己名下的转发规则"))
		return
	}

	if err := h.app.Rule.Delete(c.Request.Context(), id); err != nil {
		response.Fail(c, err)
		return
	}
	h.ruleAudit(c, model.ActionDelete, id, "删除转发规则 "+current.Name,
		gin.H{"name": current.Name, "listen_port": current.ListenPort}, nil)
	response.OK(c, nil)
}

// -------------------- 启用 / 禁用 / 重新下发 --------------------

// setRuleEnable 是启用与禁用的公共实现（规格书 8.9 的 /enable 与 /disable）。
//
// 执行顺序（顺序很重要）：
//
//  1. Rule.SetEnable 写库，service 内部已把 sync_status 置为 unsynced 并抬高配置版本；
//  2. TouchSyncUnsynced 把「为什么要重发」写进 sync_error，便于用户在列表上看原因；
//  3. BumpConfigVersion 再次抬高配置版本号，节点下一次心跳时才会来拉新配置。
//
// 参数 c 为上下文；enable 为目标状态。
func (h *Handlers) setRuleEnable(c *gin.Context, enable bool) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "规则 ID 必须是正整数")
		return
	}
	ctx := c.Request.Context()
	info, err := h.app.Rule.Get(ctx, id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if !ruleCanAccess(c, info.UserID) {
		response.Fail(c, response.New(response.CodeForbidden, "只能操作自己名下的转发规则"))
		return
	}

	if err := h.app.Rule.SetEnable(ctx, id, enable); err != nil {
		response.Fail(c, err)
		return
	}

	reason := "规则已禁用，等待下发"
	if enable {
		reason = "规则已启用，等待下发"
	}
	if err := h.app.Rule.TouchSyncUnsynced(ctx, []uint64{id}, reason); err != nil {
		response.Fail(c, err)
		return
	}
	h.app.BumpConfigVersion(reason)

	h.ruleAudit(c, model.ActionUpdate, id, reason+"："+info.Name, nil,
		gin.H{"enable": enable})
	msg := "规则已禁用"
	if enable {
		msg = "规则已启用"
	}
	response.OKMessage(c, msg, gin.H{"id": id, "enable": enable})
}

// EnableForwardRule 启用一条转发规则（规格书 8.9 POST /forward-rules/:id/enable）。
func (h *Handlers) EnableForwardRule(c *gin.Context) { h.setRuleEnable(c, true) }

// DisableForwardRule 禁用一条转发规则（规格书 8.9 POST /forward-rules/:id/disable）。
//
// 在规格书 6.9 的语义下，禁用后入口不再监听，该规则上的连接与限速也随之失效。
func (h *Handlers) DisableForwardRule(c *gin.Context) { h.setRuleEnable(c, false) }

// ResyncForwardRule 强制重新下发一条规则（规格书 8.9 POST /forward-rules/:id/resync）。
//
// 与启用 / 禁用不同，本接口不改动规则的启用状态，只把它打回未同步状态，
// 由同步服务在下一轮把期望配置重新推给相关节点。
func (h *Handlers) ResyncForwardRule(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "规则 ID 必须是正整数")
		return
	}
	ctx := c.Request.Context()
	info, err := h.app.Rule.Get(ctx, id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if !ruleCanAccess(c, info.UserID) {
		response.Fail(c, response.New(response.CodeForbidden, "只能操作自己名下的转发规则"))
		return
	}

	// Resync 内部会一并重发该子规则对应的主规则（规格书 6.8），
	// 并按需抬高配置版本号，因此这里不再重复 Bump。
	if err := h.app.Sync.Resync(ctx, id); err != nil {
		response.Fail(c, err)
		return
	}
	h.ruleAudit(c, model.ActionSync, id, "强制重新下发规则 "+info.Name, nil, nil)
	response.OKMessage(c, "已重新加入下发队列", gin.H{"id": id})
}

// -------------------- 规则级流量与会话 --------------------

// ruleTrafficResponse 是规则级流量响应（规格书 8.9 GET /forward-rules/:id/traffic）。
//
// 结构选择：把 service 的「拆解」（今日 / 昨日 / 本月 / 累计 + 今日 24 小时曲线）
// 与「时间序列」（任意区间曲线）组合在一次响应里返回。这两个数字出现在
// 同一张规则详情页上，拆成两次请求只会让首屏多抖一次。
type ruleTrafficResponse struct {
	*app.RuleTraffic
	// From / To / Interval 回显曲线的时间口径，便于前端渲染横轴。
	From     string `json:"from"`
	To       string `json:"to"`
	Interval string `json:"interval"`
	// BytesMode 回显字节口径：raw = 实际字节，scaled = 乘倍率后（规格书 6.10）。
	BytesMode app.BytesMode `json:"bytes_mode"`
	// Series 是 from~to 区间内按 direction 维度分桶的曲线。
	Series []app.TrafficSeriesPoint `json:"series"`
}

// RuleTraffic 返回单条规则的流量统计（规格书 8.9 GET /forward-rules/:id/traffic）。
//
// 查询参数：
//
//	from / to   时间区间，缺省为「最近 7 天」
//	interval    hour | day，默认 hour
//	raw         true 时展示实际字节，缺省展示乘倍率后的字节（规格书 6.10）
func (h *Handlers) RuleTraffic(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "规则 ID 必须是正整数")
		return
	}
	ctx := c.Request.Context()

	// 归属校验：流量曲线同样属于用户隐私，不能靠猜 ID 读到别人的数据。
	info, err := h.app.Rule.Get(ctx, id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if !ruleCanAccess(c, info.UserID) {
		response.Fail(c, response.New(response.CodeForbidden, "只能查看自己名下规则的流量"))
		return
	}

	// 缺省区间取最近 7 天：规则页最常见的诉求是「这一周跑了多少」。
	from, to := timeRange(c, 7*24*time.Hour, 0)
	if !to.After(from) {
		badRequest(c, "to", "结束时间必须晚于开始时间")
		return
	}
	interval := trafficIntervalParam(c)
	mode := bytesMode(c)

	breakdown, err := h.app.Traffic.RuleBreakdown(ctx, id, mode)
	if err != nil {
		response.Fail(c, err)
		return
	}
	series, err := h.app.Traffic.Timeseries(ctx, from, to, interval, app.GroupByDirection, mode)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if series == nil {
		series = []app.TrafficSeriesPoint{}
	}

	response.OK(c, ruleTrafficResponse{
		RuleTraffic: breakdown,
		From:        from.Format(time.RFC3339),
		To:          to.Format(time.RFC3339),
		Interval:    interval,
		BytesMode:   mode,
		Series:      series,
	})
}

// RuleSessions 返回某条规则当前的在线会话（规格书 8.9 GET /forward-rules/:id/sessions）。
//
// 会话只用于展示与连接数限制判断，不参与任何计费口径。
func (h *Handlers) RuleSessions(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "规则 ID 必须是正整数")
		return
	}
	ctx := c.Request.Context()

	info, err := h.app.Rule.Get(ctx, id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if !ruleCanAccess(c, info.UserID) {
		response.Fail(c, response.New(response.CodeForbidden, "只能查看自己名下规则的会话"))
		return
	}

	items, err := h.app.Rule.Sessions(ctx, id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if items == nil {
		items = []model.Session{}
	}
	response.OK(c, items)
}

// -------------------- 批量操作 --------------------

// ruleOwnedIDs 返回当前用户名下全部规则的 ID。
//
// 实现说明：走一次列表查询循环翻页（单页上限 200）。个人自用场景下
// 规则数不会超过几千条，代价可以接受；若将来要支持大规模部署，
// 应改成在 service 层加一个按 user_id 直接 Pluck 的方法。
func (h *Handlers) ruleOwnedIDs(c *gin.Context) []uint64 {
	me := middleware.CurrentUserID(c)
	if me == 0 {
		return []uint64{}
	}
	out := make([]uint64, 0, 16)
	const pageSize = 200
	for page := 1; page <= 50; page++ {
		items, total, err := h.app.Rule.List(c.Request.Context(), app.RuleListFilter{
			UserID:   me,
			Page:     page,
			PageSize: pageSize,
		})
		if err != nil {
			return out
		}
		for i := range items {
			out = append(out, items[i].ID)
		}
		if int64(len(out)) >= total || len(items) < pageSize {
			break
		}
	}
	return out
}

// filterOwnedRuleIDs 从 ID 列表中筛出属于当前用户的规则 ID（保持原顺序）。
//
// 参数 c 为上下文；ids 为待筛选的规则 ID。返回筛选后的列表。
func (h *Handlers) filterOwnedRuleIDs(c *gin.Context, ids []uint64) []uint64 {
	me := middleware.CurrentUserID(c)
	if me == 0 || len(ids) == 0 {
		return []uint64{}
	}
	out := make([]uint64, 0, len(ids))
	for _, id := range ids {
		item, err := h.app.Rule.Get(c.Request.Context(), id)
		if err != nil || item.UserID != me {
			continue
		}
		out = append(out, id)
	}
	return out
}

// BatchForwardRules 执行批量操作（规格书 8.9 POST /forward-rules/batch）。
//
// 绑定策略说明：前端发送的 JSON 形状是 `{action, ids, params}`，
// 与 app.BatchInput 的 json tag（action / ids / params）逐字段一致，
// 因此这里**直接绑定 service 的类型**，不做中间映射层——少一层映射就少一处
// 「字段改名后行为悄悄变了」的可能。
func (h *Handlers) BatchForwardRules(c *gin.Context) {
	var in app.BatchInput
	if err := c.ShouldBindJSON(&in); err != nil {
		badRequest(c, "body", err.Error())
		return
	}
	// 普通用户只能批量操作自己名下的规则：先过滤掉不属于自己的 ID。
	if !middleware.IsAdmin(c) {
		in.IDs = h.filterOwnedRuleIDs(c, in.IDs)
		if len(in.IDs) == 0 {
			response.Fail(c, response.New(response.CodeForbidden,
				"没有可操作的规则（只能操作自己名下的规则）"))
			return
		}
	}

	res, err := h.app.Rule.Batch(c.Request.Context(), in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	h.ruleAudit(c, model.ActionUpdate, 0, "批量操作转发规则："+in.Action,
		gin.H{"ids": in.IDs, "action": in.Action},
		gin.H{"affected": res.Affected, "skipped": res.Skipped})
	response.OK(c, res)
}

// batchMultiplierBody 是批量调倍率的请求体
// （规格书 8.9 POST /forward-rules/batch-multiplier）。
//
//	{ "ids": [1, 2, 3], "scale": 1.5, "both": false }
type batchMultiplierBody struct {
	// IDs 为空表示作用于全部规则（前端选了「全部规则」）。
	IDs []uint64 `json:"ids"`
	// Scale 是缩放比例，如 1.5 表示「全部 ×1.5」，必须大于 0。
	Scale float64 `json:"scale"`
	// Both 为 true 时出口倍率跟随入口倍率。
	Both bool `json:"both"`
}

// BatchMultiplier 按比例批量调整倍率
// （规格书 8.9 POST /forward-rules/batch-multiplier）。
//
// 与 /batch 的 scale_multiplier 动作的区别：本接口面向「全部 ×1.5」这种
// 不带 ids 的整体调整，因此 ids 为空时表示作用于全部规则。
// 非管理员在 ids 为空时会收敛为「自己名下的全部规则」，避免越权缩放全站倍率。
func (h *Handlers) BatchMultiplier(c *gin.Context) {
	var body batchMultiplierBody
	if err := c.ShouldBindJSON(&body); err != nil {
		badRequest(c, "body", err.Error())
		return
	}
	// NaN / Inf 会让后续的倍率校验失去意义（NaN 与任何数比较都为 false），
	// 因此在入口就拒掉。
	if math.IsNaN(body.Scale) || math.IsInf(body.Scale, 0) || body.Scale <= 0 {
		badRequest(c, "scale", "缩放比例必须是大于 0 的有限数值（如 1.5 表示 ×1.5）")
		return
	}

	ids := parseIDList(body.IDs)
	if !middleware.IsAdmin(c) {
		if len(ids) == 0 {
			ids = h.ruleOwnedIDs(c)
		} else {
			ids = h.filterOwnedRuleIDs(c, ids)
		}
		if len(ids) == 0 {
			response.Fail(c, response.New(response.CodeForbidden,
				"没有可调整的规则（只能调整自己名下的规则）"))
			return
		}
	}

	res, err := h.app.Rule.BatchMultiplier(c.Request.Context(), ids, body.Scale, body.Both)
	if err != nil {
		response.Fail(c, err)
		return
	}
	h.ruleAudit(c, model.ActionUpdate, 0, "批量调整规则倍率",
		gin.H{"ids": ids, "scale": body.Scale, "both": body.Both},
		gin.H{"affected": res.Affected})
	response.OK(c, res)
}

// -------------------- 导入 / 导出 --------------------

// ImportForwardRules 导入转发规则（规格书 8.9 POST /forward-rules/import）。
//
// 预览参数优先级（重要）：查询串带 `?preview=true`（或 1）时，**无条件**只预览
// 不落库，即使请求体里写了 `"preview": false` 也一样——预览是这个写接口的
// 安全阀，参数冲突时以「更安全的那一侧」为准。查询串没有 preview 时，
// 才采用请求体中的 preview 字段。
func (h *Handlers) ImportForwardRules(c *gin.Context) {
	var in app.ImportInput
	if err := c.ShouldBindJSON(&in); err != nil {
		badRequest(c, "body", err.Error())
		return
	}
	if queryBoolValue(c, "preview") {
		in.Preview = true
	}

	result, err := h.app.Rule.Import(c.Request.Context(), in)
	if err != nil {
		response.Fail(c, err)
		return
	}

	// 预览不落库，也不写审计：否则审计表会被「点一下预览」刷屏。
	if in.Preview {
		response.OK(c, result)
		return
	}
	h.ruleAudit(c, model.ActionImport, 0,
		"导入转发规则，新建 "+strconv.Itoa(len(result.CreatedIDs))+" 条", nil,
		gin.H{"created_ids": result.CreatedIDs,
			"conflicts": len(result.Conflicts), "invalid": len(result.Invalid)})
	response.OK(c, result)
}

// ExportForwardRules 导出转发规则（规格书 8.9 GET /forward-rules/export）。
//
// 筛选条件与列表接口完全一致；`?with_groups=true` 时附加规则依赖的设备组原文，
// 便于在另一台面板上完整重建（规格书 6.4 的「批量导出 JSON」）。
//
// 输出格式：
//
//	默认返回 JSON（app.ExportBundle）；
//	请求头 `Accept: text/csv` 时返回 CSV 下载（字段为创建请求的常用子集）。
func (h *Handlers) ExportForwardRules(c *gin.Context) {
	filter := ruleListFilter(c)
	withGroups := queryBoolValue(c, "with_groups")

	bundle, err := h.app.Rule.Export(c.Request.Context(), filter, withGroups)
	if err != nil {
		response.Fail(c, err)
		return
	}
	h.ruleAudit(c, model.ActionExport, 0,
		"导出转发规则 "+strconv.Itoa(bundle.Count)+" 条", nil, nil)

	if wantsCSV(c) {
		h.writeRuleCSV(c, bundle)
		return
	}
	response.OK(c, bundle)
}

// wantsCSV 判断客户端是否要求 CSV 输出（Accept 中含 text/csv）。
func wantsCSV(c *gin.Context) bool {
	return strings.Contains(strings.ToLower(c.GetHeader("Accept")), "text/csv")
}

// writeRuleCSV 把导出包渲染为 CSV 并直接写入响应体。
//
// 参数 c 为上下文；bundle 为导出包。
// 与流量导出保持一致，先写 UTF-8 BOM，避免 Excel 打开中文列名乱码。
// 写入过程中的错误无法再通过响应体告知（响应头与内容已经开始发送），
// 因此只记警告日志。
func (h *Handlers) writeRuleCSV(c *gin.Context, bundle *app.ExportBundle) {
	csvHeader(c, "forward-rules.csv")
	if _, err := c.Writer.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
		h.log.Warn("写入规则导出 BOM 失败")
		return
	}
	cw := csv.NewWriter(c.Writer)
	header := []string{
		"名称", "监听端口", "端口段结束", "入口组", "出口组", "归属用户", "规则分组",
		"入口倍率", "出口倍率", "目标", "启用", "备注",
	}
	if err := cw.Write(header); err != nil {
		h.log.Warn("写规则导出表头失败")
		return
	}
	for i := range bundle.Rules {
		r := bundle.Rules[i]
		targets := make([]string, 0, len(r.Targets))
		for _, t := range r.Targets {
			targets = append(targets, t.Host+":"+strconv.Itoa(t.Port))
		}
		rec := []string{
			r.Name,
			strconv.Itoa(r.ListenPort),
			strconv.Itoa(r.ListenPortEnd),
			strconv.FormatUint(r.InboundGroupID, 10),
			strconv.FormatUint(r.OutboundGroupID, 10),
			strconv.FormatUint(r.UserID, 10),
			strconv.FormatUint(r.RuleGroupID, 10),
			strconv.FormatFloat(r.InboundMultiplier, 'f', -1, 64),
			strconv.FormatFloat(r.OutboundMultiplier, 'f', -1, 64),
			strings.Join(targets, "; "),
			boolText(r.Enable),
			r.Remark,
		}
		if err := cw.Write(rec); err != nil {
			h.log.Warn("写规则导出行失败")
			return
		}
	}
	cw.Flush()
}

// boolText 把布尔值渲染为中文「是 / 否」。
func boolText(v bool) string {
	if v {
		return "是"
	}
	return "否"
}
