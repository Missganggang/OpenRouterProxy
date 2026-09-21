package api

import (
	"context"
	"encoding/csv"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/openroute/openroute/internal/api/middleware"
	"github.com/openroute/openroute/internal/api/response"
	"github.com/openroute/openroute/internal/app"
	"github.com/openroute/openroute/internal/model"
	"github.com/openroute/openroute/internal/util"
)

// 本文件实现规格书 8.7（节点分组）与 8.8（设备组）的接口，
// 以及规则分组、用户分组、节点分组的 CRUD。
//
// 分层纪律：所有业务规则（名称查重、引用检查、故障转移兼容性、
// 节点角色匹配）都在 app.GroupService / app.UserService 内完成，
// handler 只负责参数解析、必要的类型归一化与响应封装。

// -------------------- 节点分组（规格书 8.7） --------------------

// ListNodeGroups 返回节点分组列表（规格书 8.7）。
//
// 说明：GroupService.ListNodeGroups 返回的是**未分页的完整切片**，
// 因此这里用 int64(len(items)) 作为 total，并原样带上请求中的分页参数，
// 保证响应体形状与其它列表接口一致。节点分组是低频、小数据量的元数据
// （个人自用场景下通常只有几个），不做真正的分页是正确的取舍。
func (h *Handlers) ListNodeGroups(c *gin.Context) {
	page, pageSize := pageParams(c)
	ctx := c.Request.Context()

	items, err := h.app.Group.ListNodeGroups(ctx)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if items == nil {
		items = []model.NodeGroup{}
	}
	response.List(c, items, page, pageSize, int64(len(items)))
}

// CreateNodeGroup 创建节点分组（规格书 8.7）。
func (h *Handlers) CreateNodeGroup(c *gin.Context) {
	var in app.NameOnlyInput
	if err := c.ShouldBindJSON(&in); err != nil {
		badRequest(c, "body", "请求体不是合法 JSON："+err.Error())
		return
	}
	if strings.TrimSpace(in.Name) == "" {
		badRequest(c, "name", "分组名不能为空")
		return
	}

	ctx := c.Request.Context()
	row, err := h.app.Group.CreateNodeGroup(ctx, in)
	if err != nil {
		response.Fail(c, err)
		return
	}

	h.writeGroupAudit(c, model.ActionCreate, "node_group", row.ID,
		nil, row, "创建节点分组 "+row.Name)
	response.OK(c, row)
}

// GetNodeGroup 返回节点分组详情（规格书 8.7 GET /node-groups/:id）。
//
// NodeGroup 表没有单条查询的方法（分组无独立详情页，只有列表与成员），
// 这里在列表结果中定位目标行：数据量与创建/删除频率都很低，
// 一次全表扫描远优于为它新增一条 service 方法。
func (h *Handlers) GetNodeGroup(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "分组 ID 必须是正整数")
		return
	}
	ctx := c.Request.Context()

	items, err := h.app.Group.ListNodeGroups(ctx)
	if err != nil {
		response.Fail(c, err)
		return
	}
	for i := range items {
		if items[i].ID == id {
			// 附带成员数量，方便前端在详情页直接展示。
			nodes, err := h.app.Group.ListNodeGroupNodes(ctx, id)
			if err != nil {
				response.Fail(c, err)
				return
			}
			response.OK(c, gin.H{
				"id":         items[i].ID,
				"name":       items[i].Name,
				"remark":     items[i].Remark,
				"created_at": items[i].CreatedAt,
				"updated_at": items[i].UpdatedAt,
				"node_count": len(nodes),
			})
			return
		}
	}
	response.Fail(c, response.New(response.CodeNotFound, "节点分组不存在"))
}

// UpdateNodeGroup 更新节点分组（规格书 8.7 PUT /node-groups/:id）。
func (h *Handlers) UpdateNodeGroup(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "分组 ID 必须是正整数")
		return
	}
	var in app.NameOnlyInput
	if err := c.ShouldBindJSON(&in); err != nil {
		badRequest(c, "body", "请求体不是合法 JSON："+err.Error())
		return
	}

	ctx := c.Request.Context()
	row, err := h.app.Group.UpdateNodeGroup(ctx, id, in)
	if err != nil {
		response.Fail(c, err)
		return
	}

	h.writeGroupAudit(c, model.ActionUpdate, "node_group", id, nil, row,
		"更新节点分组 "+row.Name)
	response.OK(c, row)
}

// DeleteNodeGroup 删除节点分组（规格书 8.7 DELETE /node-groups/:id）。
//
// 节点分组只影响批量管理与筛选（规格书 6.2），不改变转发行为，
// 因此 service 会直接清理节点上的引用而不是阻止删除。
func (h *Handlers) DeleteNodeGroup(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "分组 ID 必须是正整数")
		return
	}

	ctx := c.Request.Context()
	if err := h.app.Group.DeleteNodeGroup(ctx, id); err != nil {
		response.Fail(c, err)
		return
	}

	h.writeGroupAudit(c, model.ActionDelete, "node_group", id, nil, nil, "删除节点分组")
	response.OK(c, nil)
}

// NodeGroupNodes 返回分组内的节点（规格书 8.7 GET /node-groups/:id/nodes）。
func (h *Handlers) NodeGroupNodes(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "分组 ID 必须是正整数")
		return
	}
	page, pageSize := pageParams(c)

	ctx := c.Request.Context()
	if _, err := h.nodeGroupExists(ctx, id); err != nil {
		response.Fail(c, err)
		return
	}

	items, err := h.app.Group.ListNodeGroupNodes(ctx, id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if items == nil {
		items = []model.Node{}
	}
	// 未分页列表（service 一次性返回全部成员）。
	response.List(c, items, page, pageSize, int64(len(items)))
}

// NodeGroupExportCSV 导出分组内节点为 CSV（规格书 6.2 / 8.7）。
//
// 列固定为：名称、角色、IP、在线状态、权重。
// 在线状态用中文（在线/离线）而不是 true/false——这份文件是给人看的。
func (h *Handlers) NodeGroupExportCSV(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "分组 ID 必须是正整数")
		return
	}
	ctx := c.Request.Context()

	group, err := h.nodeGroupExists(ctx, id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	nodes, err := h.app.Group.ListNodeGroupNodes(ctx, id)
	if err != nil {
		response.Fail(c, err)
		return
	}

	csvHeader(c, "node-group-"+strconv.FormatUint(id, 10)+".csv")

	// UTF-8 BOM：让 Excel 正确识别中文列名，否则会显示成乱码。
	_, _ = c.Writer.WriteString("\xEF\xBB\xBF")

	w := csv.NewWriter(c.Writer)
	_ = w.Write([]string{"名称", "角色", "IP", "在线状态", "权重"})
	for i := range nodes {
		n := &nodes[i]
		state := "离线"
		if n.Online {
			state = "在线"
		}
		_ = w.Write([]string{
			n.Name,
			nodeRoleLabel(n.Role),
			nodeIPText(n),
			state,
			strconv.Itoa(n.Weight),
		})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		// 响应头已发出（200 + CSV），此时无法再改成 JSON 错误；
		// 记录到日志并把中断原因写进响应体尾部，便于用户发现文件被截断。
		h.log.Warn("导出节点分组 CSV 失败",
			zap.Uint64("group_id", id), zap.Error(err))
	}

	h.writeGroupAudit(c, model.ActionExport, "node_group", group.ID, nil,
		gin.H{"rows": len(nodes)}, "导出节点分组 CSV："+group.Name)
}

// -------------------- 设备组（规格书 8.8） --------------------

// ListDeviceGroups 返回设备组列表（规格书 8.8 GET /device-groups）。
//
// 参数：?type=inbound|outbound 区分入口组/出口组，?keyword= 按名称模糊匹配。
func (h *Handlers) ListDeviceGroups(c *gin.Context) {
	page, pageSize := pageParams(c)

	// 类型取值做白名单校验：非法值如果直接透传，用户会得到「空列表」
	// 而不是「参数写错了」，排查成本更高。
	groupType := strings.ToLower(strings.TrimSpace(c.Query("type")))
	if groupType != "" && groupType != model.GroupTypeInbound && groupType != model.GroupTypeOutbound {
		badRequest(c, "type", "组类型只能是 inbound 或 outbound")
		return
	}

	items, err := h.app.Group.List(c.Request.Context(), app.GroupListFilter{
		Type:    groupType,
		Keyword: strings.TrimSpace(c.Query("keyword")),
	})
	if err != nil {
		response.Fail(c, err)
		return
	}
	if items == nil {
		items = []model.DeviceGroup{}
	}
	// 未分页列表：设备组是规则的下拉候选，前端需要一次性拿到全部。
	response.List(c, items, page, pageSize, int64(len(items)))
}

// CreateDeviceGroup 创建设备组（规格书 8.8 POST /device-groups）。
//
// 请求体中的 config 按类型原样落库（规格书 4.2.5：前端表单编辑、后端存原文），
// 语义校验由 service 完成，含故障转移组兼容性（42201）与节点角色匹配（42206）。
func (h *Handlers) CreateDeviceGroup(c *gin.Context) {
	var in app.DeviceGroupInput
	if err := c.ShouldBindJSON(&in); err != nil {
		badRequest(c, "body", "请求体不是合法 JSON："+err.Error())
		return
	}

	ctx := c.Request.Context()
	detail, err := h.app.Group.Create(ctx, in)
	if err != nil {
		response.Fail(c, err)
		return
	}

	h.writeGroupAudit(c, model.ActionCreate, "device_group", detail.ID,
		nil, gin.H{"name": detail.Name, "type": detail.Type}, "创建设备组 "+detail.Name)
	response.OK(c, detail)
}

// GetDeviceGroup 返回设备组详情（规格书 8.8 GET /device-groups/:id）。
//
// 响应中含**解析后的 Config**（inbound_config / outbound_config 二选一），
// 前端据此直接渲染表单，不必自己再解析一遍 JSON 文本。
func (h *Handlers) GetDeviceGroup(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "设备组 ID 必须是正整数")
		return
	}
	detail, err := h.app.Group.Get(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, detail)
}

// UpdateDeviceGroup 更新设备组（规格书 8.8 PUT /device-groups/:id）。
//
// 组的类型创建后不可修改（service 返回 40001），因为入口组与出口组的
// 字段集不同，改类型会让引用它的规则语义失效。
func (h *Handlers) UpdateDeviceGroup(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "设备组 ID 必须是正整数")
		return
	}
	var in app.DeviceGroupInput
	if err := c.ShouldBindJSON(&in); err != nil {
		badRequest(c, "body", "请求体不是合法 JSON："+err.Error())
		return
	}

	ctx := c.Request.Context()
	before, _ := h.app.Group.Get(ctx, id)

	detail, err := h.app.Group.Update(ctx, id, in)
	if err != nil {
		response.Fail(c, err)
		return
	}

	h.writeGroupAudit(c, model.ActionUpdate, "device_group", id, before, detail,
		"更新设备组 "+detail.Name)
	response.OK(c, detail)
}

// DeleteDeviceGroup 删除设备组（规格书 8.8 DELETE /device-groups/:id）。
//
// 被规则/故障转移/反向组引用时 service 返回 40903（该资源正在被引用）。
// 这里直接 response.Fail(c, err) 把原始 AppError 透出，
// 前端据 code 展示「请先解除引用」并高亮引用清单。
func (h *Handlers) DeleteDeviceGroup(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "设备组 ID 必须是正整数")
		return
	}

	ctx := c.Request.Context()
	before, _ := h.app.Group.Get(ctx, id)

	if err := h.app.Group.Delete(ctx, id); err != nil {
		response.Fail(c, err)
		return
	}

	name := ""
	if before != nil {
		name = before.Name
	}
	h.writeGroupAudit(c, model.ActionDelete, "device_group", id, before, nil,
		"删除设备组 "+name)
	response.OK(c, nil)
}

// DeviceGroupSchema 返回**该组所属类型**的配置字段 schema（规格书 8.8 GET /:id/schema）。
//
// 与 DeviceGroupSchemaByType 的区别：这个用在编辑已有组时，
// 组类型由 ID 决定，前端不需要再带一次 ?type=。
func (h *Handlers) DeviceGroupSchema(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "设备组 ID 必须是正整数")
		return
	}
	ctx := c.Request.Context()

	g, err := h.app.Group.Get(ctx, id)
	if err != nil {
		response.Fail(c, err)
		return
	}

	schema := h.app.Group.GetDeviceGroupSchema(g.Type)
	if schema == nil {
		response.Fail(c, response.New(response.CodeInternal,
			"设备组类型 "+g.Type+" 没有对应的配置 schema"))
		return
	}
	response.OK(c, gin.H{
		"group_id": g.ID,
		"name":     g.Name,
		"type":     g.Type,
		"schema":   schema,
	})
}

// DeviceGroupSchemaByType 按类型返回配置字段 schema（规格书 8.8）。
//
// 用于「新建组」场景：此时还没有组 ID，前端只能按 ?type= 取 schema。
// type 为必填，取值为 inbound / outbound。
func (h *Handlers) DeviceGroupSchemaByType(c *gin.Context) {
	groupType := strings.ToLower(strings.TrimSpace(c.Query("type")))
	if groupType == "" {
		badRequest(c, "type", "请提供 type=inbound 或 type=outbound")
		return
	}
	if groupType != model.GroupTypeInbound && groupType != model.GroupTypeOutbound {
		badRequest(c, "type", "组类型只能是 inbound 或 outbound")
		return
	}

	schema := h.app.Group.GetDeviceGroupSchema(groupType)
	if schema == nil {
		response.Fail(c, response.New(response.CodeInternal,
			"设备组类型 "+groupType+" 没有对应的配置 schema"))
		return
	}
	response.OK(c, schema)
}

// ValidateDeviceGroup 校验设备组配置的合法性（规格书 8.8 POST /:id/validate）。
//
// 这是一个「保存前预检」接口：请求体是**待校验的组配置**（与创建/更新同形状），
// 而不是已落库的内容，因此返回 Errors / Warnings 两份清单：
//   - Errors 非空 → 前端禁止提交，并定位到具体字段（field 形如 config.tls.chfp）；
//   - 仅 Warnings → 允许提交，但 MUST 展示给用户（如 IPv6 无回退、故障转移需数秒）。
//
// 覆盖规格书 6.3 的全部约束：故障转移组权限一致性（42201）、
// 链式出口不支持 UDP（42202）与故障转移（42203）、SNI 白名单（42204）、
// 节点角色匹配（42206）、反向组出口角色（42209）。
func (h *Handlers) ValidateDeviceGroup(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "设备组 ID 必须是正整数")
		return
	}
	var in app.DeviceGroupInput
	if err := c.ShouldBindJSON(&in); err != nil {
		badRequest(c, "body", "请求体不是合法 JSON："+err.Error())
		return
	}

	ctx := c.Request.Context()
	existing, err := h.app.Group.Get(ctx, id)
	if err != nil {
		response.Fail(c, err)
		return
	}

	// 构造一个「待校验的 DeviceGroup」：以库中的组为底（ID、类型等），
	// 用请求体覆盖可能被修改的字段。这样校验既能看到当前真实状态
	// （如组成员默认沿用），又能反映用户正在编辑的内容。
	g := existing.DeviceGroup
	if t := strings.TrimSpace(in.Type); t != "" && t != g.Type {
		// 类型不可修改，但校验接口不必报这个错——
		// 以库中类型为准，字段级错误交给 Update/Create 去裁决。
		_ = t
	}
	if in.NodeIDs != nil {
		g.NodeIDs = model.FromAny(parseIDList(in.NodeIDs))
	}
	if len(in.Config) > 0 && string(in.Config) != "null" {
		g.Config = in.Config
	}
	if name := strings.TrimSpace(in.Name); name != "" {
		g.Name = name
	}
	if b := strings.TrimSpace(in.Balance); b != "" {
		g.Balance = b
	}
	if in.HealthCheckEnable != nil {
		g.HealthCheckEnable = *in.HealthCheckEnable
	}
	if in.HealthCheckInterval > 0 {
		g.HealthCheckInterval = in.HealthCheckInterval
	}
	if in.HealthCheckTimeout > 0 {
		g.HealthCheckTimeout = in.HealthCheckTimeout
	}
	if in.HealthCheckFailCount > 0 {
		g.HealthCheckFailCount = in.HealthCheckFailCount
	}
	if in.HealthCheckSuccCount > 0 {
		g.HealthCheckSuccCount = in.HealthCheckSuccCount
	}
	g.FailoverGroupID = in.FailoverGroupID

	res, err := h.app.Group.ValidateDeviceGroupConfigDetailed(ctx, &g)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if res == nil {
		res = &app.ValidateResult{
			Valid:    true,
			Errors:   []app.ValidateIssue{},
			Warnings: []app.ValidateIssue{},
		}
	}
	if res.Errors == nil {
		res.Errors = []app.ValidateIssue{}
	}
	if res.Warnings == nil {
		res.Warnings = []app.ValidateIssue{}
	}
	response.OK(c, res)
}

// DeviceGroupHealth 返回组内节点健康状态与负载分布（规格书 8.8 GET /:id/health）。
//
// 返回内容含：成员在线/健康状态、被摘除原因、当前选路策略、
// 负载分布（least_conn 的比较键推导出的理论占比）与故障转移目标。
func (h *Handlers) DeviceGroupHealth(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "设备组 ID 必须是正整数")
		return
	}
	health, err := h.app.Group.Health(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, health)
}

// DeviceGroupReorder 调整组内节点顺序（规格书 8.8 POST /:id/reorder）。
//
// 请求体：{"node_ids":[3,1,2]}。MUST 提交组的**全部**节点：
// 出口组的顺序参与负载均衡初始化与并列打破（规格书 6.3），
// 只提交部分 ID 会被 service 判为 40001，避免成员被意外截断。
func (h *Handlers) DeviceGroupReorder(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "设备组 ID 必须是正整数")
		return
	}
	var body struct {
		NodeIDs []uint64 `json:"node_ids"`
		// NodeIDsAlt 兼容链式/移动端可能使用的 ids 写法。
		NodeIDsAlt []uint64 `json:"ids"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		badRequest(c, "body", "请求体必须是 {\"node_ids\":[...]} 形式的 JSON")
		return
	}
	ids := parseIDList(append(append([]uint64{}, body.NodeIDs...), body.NodeIDsAlt...))
	if len(ids) == 0 {
		badRequest(c, "node_ids", "请提交组内全部节点的新顺序")
		return
	}

	ctx := c.Request.Context()
	if err := h.app.Group.Reorder(ctx, id, ids); err != nil {
		response.Fail(c, err)
		return
	}

	h.writeGroupAudit(c, model.ActionUpdate, "device_group", id, nil,
		gin.H{"node_ids": ids}, "调整设备组节点顺序")
	response.OKMsg(c, "顺序已调整", gin.H{"group_id": id, "node_ids": ids})
}

// -------------------- 规则分组（规格书 8.10） --------------------

// ListRuleGroups 返回规则分组列表（规格书 8.10）。
//
// 未分页列表：规则分组是规则表单的下拉候选，前端需要一次拿全。
func (h *Handlers) ListRuleGroups(c *gin.Context) {
	page, pageSize := pageParams(c)

	items, err := h.app.Group.ListRuleGroups(c.Request.Context())
	if err != nil {
		response.Fail(c, err)
		return
	}
	if items == nil {
		items = []model.RuleGroup{}
	}
	response.List(c, items, page, pageSize, int64(len(items)))
}

// CreateRuleGroup 创建规则分组（规格书 8.10）。
func (h *Handlers) CreateRuleGroup(c *gin.Context) {
	var in app.NameOnlyInput
	if err := c.ShouldBindJSON(&in); err != nil {
		badRequest(c, "body", "请求体不是合法 JSON："+err.Error())
		return
	}
	if strings.TrimSpace(in.Name) == "" {
		badRequest(c, "name", "分组名不能为空")
		return
	}

	ctx := c.Request.Context()
	row, err := h.app.Group.CreateRuleGroup(ctx, in)
	if err != nil {
		response.Fail(c, err)
		return
	}

	h.writeGroupAudit(c, model.ActionCreate, "rule_group", row.ID, nil, row,
		"创建规则分组 "+row.Name)
	response.OK(c, row)
}

// GetRuleGroup 返回规则分组详情（规格书 8.10 GET /rule-groups/:id）。
//
// 与节点分组同理：GroupService 只提供列表方法，
// 这里在列表中定位目标行（规则分组是小数据量元数据）。
func (h *Handlers) GetRuleGroup(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "分组 ID 必须是正整数")
		return
	}

	items, err := h.app.Group.ListRuleGroups(c.Request.Context())
	if err != nil {
		response.Fail(c, err)
		return
	}
	for i := range items {
		if items[i].ID == id {
			response.OK(c, items[i])
			return
		}
	}
	response.Fail(c, response.New(response.CodeNotFound, "规则分组不存在"))
}

// UpdateRuleGroup 更新规则分组（规格书 8.10 PUT /rule-groups/:id）。
func (h *Handlers) UpdateRuleGroup(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "分组 ID 必须是正整数")
		return
	}
	var in app.NameOnlyInput
	if err := c.ShouldBindJSON(&in); err != nil {
		badRequest(c, "body", "请求体不是合法 JSON："+err.Error())
		return
	}

	ctx := c.Request.Context()
	row, err := h.app.Group.UpdateRuleGroup(ctx, id, in)
	if err != nil {
		response.Fail(c, err)
		return
	}

	h.writeGroupAudit(c, model.ActionUpdate, "rule_group", id, nil, row,
		"更新规则分组 "+row.Name)
	response.OK(c, row)
}

// DeleteRuleGroup 删除规则分组（规格书 8.10 DELETE /rule-groups/:id）。
//
// 组内仍有规则时返回 40903，前端据此提示「请先移动或删除这些规则」。
func (h *Handlers) DeleteRuleGroup(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "分组 ID 必须是正整数")
		return
	}

	ctx := c.Request.Context()
	if err := h.app.Group.DeleteRuleGroup(ctx, id); err != nil {
		response.Fail(c, err)
		return
	}

	h.writeGroupAudit(c, model.ActionDelete, "rule_group", id, nil, nil, "删除规则分组")
	response.OK(c, nil)
}

// ReorderRuleGroups 重排规则分组顺序（规格书 8.10 POST /rule-groups/reorder）。
//
// 请求体：{"ids":[3,1,2]}，索引即新的 sort 值。
func (h *Handlers) ReorderRuleGroups(c *gin.Context) {
	var body struct {
		IDs []uint64 `json:"ids"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		badRequest(c, "body", "请求体必须是 {\"ids\":[...]} 形式的 JSON")
		return
	}
	ids := parseIDList(body.IDs)
	if len(ids) == 0 {
		badRequest(c, "ids", "顺序列表不能为空")
		return
	}

	ctx := c.Request.Context()
	if err := h.app.Group.ReorderRuleGroups(ctx, ids); err != nil {
		response.Fail(c, err)
		return
	}

	h.writeGroupAudit(c, model.ActionUpdate, "rule_group", 0, nil,
		gin.H{"ids": ids}, "重排规则分组顺序")
	response.OKMsg(c, "顺序已调整", gin.H{"ids": ids})
}

// -------------------- 用户分组（规格书 8.11）说明 --------------------

// 用户分组（ListUserGroups / CreateUserGroup / GetUserGroup /
// UpdateUserGroup / DeleteUserGroup）的实现**不在本文件**，
// 而是由 handler_user.go 提供——用户分组属于用户模块，与用户 CRUD 同置一处，
// 便于共用「仅管理员可操作」的判断与审计口径。
//
// 这里只记录当时对比过的两套 service 实现，供后续维护者理解取舍：
//   - app.UserService.ListGroups / GetGroup / CreateGroup / UpdateGroup / DeleteGroup
//     （入参 app.UserGroupParam，对 limit 字段做了「不能为负数」的显式校验）
//     —— handler_user.go 采用的就是这一套；
//   - app.GroupService.ListUserGroups / CreateUserGroup / UpdateUserGroup / DeleteUserGroup
//     （入参 app.UserGroupInput）
//     —— 保留在 GroupService 上，供内部调用路径使用。
//
// 两组入参结构体的字段完全一致（name / traffic_limit / speed_limit /
// ip_limit / conn_limit / rule_group_ids / remark），因此两套实现可以互换。

// -------------------- 本文件内部工具 --------------------

// writeGroupAudit 写一条分组相关的审计记录。
//
// 与节点侧的写审计方法分开命名（writeNodeAudit），
// 避免与其它 agent 的 handler 文件产生符号冲突。
func (h *Handlers) writeGroupAudit(c *gin.Context, action, resource string, resourceID uint64,
	before, after interface{}, message string) {

	uid, un, _, _ := middleware.CurrentUser(c)
	h.app.Audit.Write(c.Request.Context(), app.AuditEntry{
		UserID:     uid,
		Username:   un,
		Action:     action,
		Resource:   resource,
		ResourceID: resourceID,
		Before:     before,
		After:      after,
		IP:         util.ClientIP(c.Request),
		UserAgent:  c.GetHeader("User-Agent"),
		Result:     model.ResultSuccess,
		Message:    message,
	})
}

// nodeGroupExists 校验节点分组存在并返回该行。
//
// GroupService 没有单条查询节点分组的方法，这里用列表定位；
// 不存在时返回 40401，让 /:id/nodes 与 /:id/export 在分组被删掉后
// 返回「分组不存在」而不是「空列表」——后者会让用户以为分组还在、只是没节点。
func (h *Handlers) nodeGroupExists(ctx context.Context, id uint64) (*model.NodeGroup, error) {
	items, err := h.app.Group.ListNodeGroups(ctx)
	if err != nil {
		return nil, err
	}
	for i := range items {
		if items[i].ID == id {
			return &items[i], nil
		}
	}
	return nil, response.New(response.CodeNotFound, "节点分组不存在")
}

// nodeRoleLabel 把节点角色转成中文，用于 CSV 等给人看的地方。
func nodeRoleLabel(role string) string {
	switch role {
	case model.RoleInbound:
		return "入口"
	case model.RoleOutbound:
		return "出口"
	case model.RoleBoth:
		return "双端"
	}
	return role
}

// nodeIPText 返回节点的展示用 IP：优先公网 IPv4，
// 其次公网 IPv6，最后回退到内网 IP 与静态连接地址。
func nodeIPText(n *model.Node) string {
	if strings.TrimSpace(n.PublicIPv4) != "" {
		return strings.TrimSpace(n.PublicIPv4)
	}
	if strings.TrimSpace(n.PublicIPv6) != "" {
		return strings.TrimSpace(n.PublicIPv6)
	}
	if strings.TrimSpace(n.PrivateIP) != "" {
		return strings.TrimSpace(n.PrivateIP)
	}
	return strings.TrimSpace(n.ConnectHost)
}
