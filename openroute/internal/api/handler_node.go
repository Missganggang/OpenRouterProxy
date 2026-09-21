package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/openroute/openroute/internal/api/middleware"
	"github.com/openroute/openroute/internal/api/response"
	"github.com/openroute/openroute/internal/app"
	"github.com/openroute/openroute/internal/model"
	"github.com/openroute/openroute/internal/util"
)

// 本文件实现规格书 8.6 的节点接口。
//
// 分层纪律：handler 只做参数解析、权限相关的轻量判断与 service 调用，
// 业务规则（名称查重、引用检查、任务派发）全部在 app.NodeService 内。
// 唯一例外是 NodeRules / NodeLogs：NodeService 没有「按节点查规则」「读节点日志」
// 的对外方法，这里用极简查询补齐，理由写在各方法注释里。

// -------------------- 列表与 CRUD --------------------

// ListNodes 返回节点列表（规格书 8.6 GET /nodes）。
//
// 支持 ?group_id=&role=&online=&keyword= 与分页、排序参数。
// 返回分页列表；空结果返回 [] 而不是 null。
func (h *Handlers) ListNodes(c *gin.Context) {
	page, pageSize := pageParams(c)
	sortField, order := sortParams(c, map[string]bool{
		"id": true, "name": true, "created_at": true, "updated_at": true,
		"last_seen": true, "online": true, "weight": true, "health_score": true,
	}, "id", "desc")

	q := app.NodeListQuery{
		GroupID:  queryUint64(c, "group_id", 0),
		Role:     strings.TrimSpace(c.Query("role")),
		Online:   queryBool(c, "online"),
		Keyword:  strings.TrimSpace(c.Query("keyword")),
		Page:     page,
		PageSize: pageSize,
		Sort:     sortField,
		Order:    order,
	}

	items, total, err := h.app.Node.List(c.Request.Context(), q)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if items == nil {
		items = []model.Node{}
	}
	response.List(c, items, page, pageSize, total)
}

// CreateNode 创建节点并返回一键对接所需的密钥与安装命令（规格书 8.6）。
//
// 安装命令里的面板地址用 baseURL(c) 从当前请求推导，
// 这样用户在内网/外网不同入口访问时拿到的命令都是可达的。
func (h *Handlers) CreateNode(c *gin.Context) {
	var in app.NodeCreateInput
	if err := c.ShouldBindJSON(&in); err != nil {
		badRequest(c, "body", "请求体不是合法 JSON："+err.Error())
		return
	}

	ctx := c.Request.Context()
	res, err := h.app.Node.Create(ctx, in, baseURL(c))
	if err != nil {
		response.Fail(c, err)
		return
	}

	h.writeNodeAudit(c, model.ActionCreate, "node", res.ID,
		nil, gin.H{"name": res.Name}, "创建节点 "+res.Name)
	response.OK(c, res)
}

// GetNode 返回节点详情（规格书 8.6 GET /nodes/:id）。
func (h *Handlers) GetNode(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "节点 ID 必须是正整数")
		return
	}
	n, err := h.app.Node.Get(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, n)
}

// UpdateNode 更新节点（规格书 8.6 PUT /nodes/:id）。
//
// 请求体是「部分更新」语义：只有出现的字段才会被写入。
// 版本自增由 NodeService.Update 完成（影响转发行为的变更才会触发），
// 这里不重复 BumpConfigVersion，避免节点被无意义地反复拉配置。
func (h *Handlers) UpdateNode(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "节点 ID 必须是正整数")
		return
	}
	var in app.NodeUpdateInput
	if err := c.ShouldBindJSON(&in); err != nil {
		badRequest(c, "body", "请求体不是合法 JSON："+err.Error())
		return
	}

	ctx := c.Request.Context()
	before, _ := h.app.Node.Get(ctx, id)

	n, err := h.app.Node.Update(ctx, id, in)
	if err != nil {
		response.Fail(c, err)
		return
	}

	h.writeNodeAudit(c, model.ActionUpdate, "node", id, before, n, "更新节点 "+n.Name)
	response.OK(c, n)
}

// DeleteNode 删除节点（规格书 8.6 DELETE /nodes/:id）。
//
// 被设备组引用时由 service 返回 40903；该节点上的规则会被标记为待处理。
func (h *Handlers) DeleteNode(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "节点 ID 必须是正整数")
		return
	}

	ctx := c.Request.Context()
	before, _ := h.app.Node.Get(ctx, id)

	if err := h.app.Node.Delete(ctx, id); err != nil {
		response.Fail(c, err)
		return
	}

	h.writeNodeAudit(c, model.ActionDelete, "node", id, before, nil, "删除节点")
	response.OK(c, nil)
}

// -------------------- 一键对接与运维 --------------------

// NodeInstallCommand 重新生成节点的安装命令（规格书 5.1 / 8.6）。
//
// 用于节点换机重装或用户丢失了原始命令的场景，不轮换密钥。
func (h *Handlers) NodeInstallCommand(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "节点 ID 必须是正整数")
		return
	}
	cmd, err := h.app.Node.GenerateInstallCommand(c.Request.Context(), id, baseURL(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"id": id, "install_command": cmd})
}

// NodeUpgrade 下发节点客户端升级任务（规格书 5.5 / 8.6）。
//
// 节点离线返回 60001，节点设置 DISABLE_EXECUTE=1 返回 40301。
// 返回创建的任务，前端据此提示「已排队，等待节点拉取」。
func (h *Handlers) NodeUpgrade(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "节点 ID 必须是正整数")
		return
	}
	ctx := c.Request.Context()
	t, err := h.app.Node.Upgrade(ctx, id)
	if err != nil {
		response.Fail(c, err)
		return
	}

	h.writeNodeAudit(c, model.ActionExec, "node", id, nil,
		gin.H{"task_id": t.ID, "type": model.TaskUpgrade}, "升级节点客户端")
	response.OKMsg(c, "升级任务已下发，节点将在下次心跳时拉取", t)
}

// NodeRestart 下发节点服务重启任务（规格书 5.5 / 8.6）。
//
// 重启会让节点上的转发短暂中断，前端 MUST 二次确认后再调用。
func (h *Handlers) NodeRestart(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "节点 ID 必须是正整数")
		return
	}
	ctx := c.Request.Context()
	t, err := h.app.Node.Restart(ctx, id)
	if err != nil {
		response.Fail(c, err)
		return
	}

	h.writeNodeAudit(c, model.ActionExec, "node", id, nil,
		gin.H{"task_id": t.ID, "type": model.TaskRestart}, "重启节点服务")
	response.OKMsg(c, "重启任务已下发", t)
}

// NodeExecBody 是单节点执行命令的请求体（规格书 8.6 POST /nodes/:id/exec）。
//
// Timeout 为 0 时由 service 回退到 30 秒。
type NodeExecBody struct {
	// Command 要执行的 shell 命令，最长 2048 字节。
	Command string `json:"command"`
	// Timeout 命令超时秒数，<= 0 表示使用默认值。
	Timeout int `json:"timeout"`
}

// NodeExec 在指定节点执行一条命令（规格书 8.6）。
//
// 这是权限最高的运维动作（等同于拿到该机器的 shell），
// 因此每次调用都写审计日志（谁、对哪台机器、执行了什么）。
func (h *Handlers) NodeExec(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "节点 ID 必须是正整数")
		return
	}
	var body NodeExecBody
	if err := c.ShouldBindJSON(&body); err != nil {
		badRequest(c, "body", "请求体必须是 {command, timeout} 形式的 JSON")
		return
	}
	if strings.TrimSpace(body.Command) == "" {
		badRequest(c, "command", "命令不能为空")
		return
	}

	ctx := c.Request.Context()
	t, err := h.app.Node.Exec(ctx, id, body.Command, body.Timeout)
	if err != nil {
		h.writeNodeAuditResult(c, model.ActionExec, "node", id, model.ResultFailed,
			"节点命令执行失败："+response.AsAppError(err).Msg,
			gin.H{"command": body.Command})
		response.Fail(c, err)
		return
	}

	h.writeNodeAudit(c, model.ActionExec, "node", id, nil,
		gin.H{"task_id": t.ID, "command": body.Command, "timeout": body.Timeout},
		"在节点执行命令")
	response.OKMsg(c, "命令已下发，执行结果可通过任务详情查看", t)
}

// NodeResetToken 重置节点密钥（规格书 8.6 POST /nodes/:id/reset-token）。
//
// 旧密钥立即失效，节点会掉线，必须用返回的安装命令或新密钥重新接入。
func (h *Handlers) NodeResetToken(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "节点 ID 必须是正整数")
		return
	}

	ctx := c.Request.Context()
	res, err := h.app.Node.ResetToken(ctx, id, baseURL(c))
	if err != nil {
		response.Fail(c, err)
		return
	}

	h.writeNodeAudit(c, model.ActionUpdate, "node", id, nil,
		gin.H{"action": "reset_token", "token": res.Token}, "重置节点密钥")
	response.OKMsg(c, "密钥已重置，请用新安装命令重新接入该节点（旧密钥立即失效）", res)
}

// -------------------- 监控与诊断 --------------------

// probeIntervals 是节点指标接口允许的粒度白名单（规格书 6.11）。
//
// 面板固定提供四档，避免前端拼出任意桶大小导致 SQL 聚合失去意义。
var probeIntervals = map[string]bool{
	"auto": true, "raw": true, "5m": true, "1h": true, "1d": true, "7d": true,
}

// probeIntervalParam 读取并校验 ?interval= 参数。
//
// 缺省返回 auto（按时间跨度自动降采样）；非法值返回 40002 让前端尽早发现拼写错误。
func probeIntervalParam(c *gin.Context) (string, bool) {
	raw := strings.ToLower(strings.TrimSpace(c.Query("interval")))
	if raw == "" {
		return app.IntervalAuto, true
	}
	switch raw {
	case "1h", "1d", "7d":
		// 规格书 8.6 明确列出的三档。
		return raw, true
	}
	if probeIntervals[raw] {
		return raw, true
	}
	response.Fail(c, response.Field(response.CodeParamOutOfRange, "interval", raw,
		"粒度只能是 1h / 1d / 7d（也接受 auto / raw / 5m）"))
	return "", false
}

// NodeMetrics 返回节点的历史探针曲线（规格书 8.6 / 6.11）。
//
// 参数：?from=&to=&interval=，interval ∈ 1h|1d|7d（默认 auto）。
// 缺省时间范围为最近 1 小时（timeRange 的第二参数为 0，即 to = 现在）。
func (h *Handlers) NodeMetrics(c *gin.Context) {
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
	if _, err := h.app.Node.Get(ctx, id); err != nil {
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
	response.OK(c, gin.H{
		"node_id":  id,
		"from":     from.Format(time.RFC3339),
		"to":       to.Format(time.RFC3339),
		"interval": interval,
		"points":   points,
	})
}

// NodeMetricsRealtime 返回节点最近一次采集的指标（规格书 8.6 实时指标）。
//
// 无采集数据时 points 为 null（前端显示「暂无数据」），而不是报 404。
func (h *Handlers) NodeMetricsRealtime(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "节点 ID 必须是正整数")
		return
	}

	ctx := c.Request.Context()
	n, err := h.app.Node.Get(ctx, id)
	if err != nil {
		response.Fail(c, err)
		return
	}

	m, err := h.app.Probe.Realtime(id)
	if err != nil {
		response.Fail(c, err)
		return
	}

	// 同时给出节点行上的即时状态：即使探针表还没有样本，
	// 前端也能拿到连接数、实时速率这些由心跳直接写入节点的字段。
	response.OK(c, gin.H{
		"node_id":       id,
		"online":        n.Online,
		"last_seen":     n.LastSeen,
		"current_conn":  n.CurrentConn,
		"net_in_speed":  n.NetInSpeed,
		"net_out_speed": n.NetOutSpeed,
		"cpu_usage":     n.CPUUsage,
		"mem_used":      n.MemUsed,
		"mem_total":     n.MemTotal,
		"disk_used":     n.DiskUsed,
		"disk_total":    n.DiskTotal,
		"load1":         n.Load1,
		"uptime":        n.Uptime,
		"health_score":  n.HealthScore,
		"metric":        m,
	})
}

// NodeRunningRule 是节点上运行中的规则条目（规格书 8.6 GET /nodes/:id/rules）。
type NodeRunningRule struct {
	ID            uint64 `json:"id"`
	Name          string `json:"name"`
	UserID        uint64 `json:"user_id"`
	RuleGroupID   uint64 `json:"rule_group_id"`
	InboundGroup  uint64 `json:"inbound_group_id"`
	ListenPort    int    `json:"listen_port"`
	ListenPortEnd int    `json:"listen_port_end"`
	OutboundGroup uint64 `json:"outbound_group_id"`
	Protocol      string `json:"protocol"`
	Targets       int    `json:"target_count"`
	SyncStatus    string `json:"sync_status"`
	SyncError     string `json:"sync_error"`
	CurrentConn   int    `json:"current_conn"`
	Enable        bool   `json:"enable"`
	Drifted       bool   `json:"drifted"`
}

// NodeRules 返回「该节点上运行的规则」（规格书 8.6 GET /nodes/:id/rules）。
//
// 实现说明（刻意绕开 service 的理由）：
// NodeService 只提供了 ExpectedRules（漂移比对用的精简结构），
// 没有面向 UI 的规则列表方法，因此这里用极简查询补齐：
//
//  1. 找出该节点所属的全部入口设备组（node_ids 是 JSON 文本，
//     三种方言没有统一的数组操作符，因此取候选后在应用层精确判断）；
//  2. 取这些入口组上的启用规则；
//  3. 规则协议由入口组 config 的 protocol 字段推导（direct 表示入口直出）。
//
// 查询只读、无副作用，符合规格书 2.3 对「极简查询」的例外约定。
func (h *Handlers) NodeRules(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "节点 ID 必须是正整数")
		return
	}
	ctx := c.Request.Context()

	n, err := h.app.Node.Get(ctx, id)
	if err != nil {
		response.Fail(c, err)
		return
	}

	// 1. 该节点所属的入口设备组。
	var groups []model.DeviceGroup
	if err := h.app.DB.WithContext(ctx).
		Where("type = ?", model.GroupTypeInbound).
		Find(&groups).Error; err != nil {
		response.Fail(c, response.Wrap(response.CodeInternal, err, "查询入口设备组失败"))
		return
	}
	groupsOfNode := make(map[uint64]*model.DeviceGroup)
	groupIDs := make([]uint64, 0, len(groups))
	for i := range groups {
		if nodeGroupHitLocal(groups[i].NodeIDs.AsUint64Slice(), id) {
			groupsOfNode[groups[i].ID] = &groups[i]
			groupIDs = append(groupIDs, groups[i].ID)
		}
	}

	items := make([]NodeRunningRule, 0)
	if len(groupIDs) > 0 {
		var rules []model.ForwardRule
		if err := h.app.DB.WithContext(ctx).
			Where("inbound_group_id IN ? AND enable = ?", groupIDs, true).
			Order("listen_port ASC, id ASC").Find(&rules).Error; err != nil {
			response.Fail(c, response.Wrap(response.CodeInternal, err, "查询节点规则失败"))
			return
		}
		for i := range rules {
			r := &rules[i]
			g := groupsOfNode[r.InboundGroupID]
			items = append(items, NodeRunningRule{
				ID:            r.ID,
				Name:          r.Name,
				UserID:        r.UserID,
				RuleGroupID:   r.RuleGroupID,
				InboundGroup:  r.InboundGroupID,
				ListenPort:    r.ListenPort,
				ListenPortEnd: r.ListenPortEnd,
				OutboundGroup: r.OutboundGroupID,
				Protocol:      nodeRuleProtocol(r, g),
				Targets:       len(r.TargetList()),
				SyncStatus:    r.SyncStatus,
				SyncError:     r.SyncError,
				Enable:        r.Enable,
			})
		}
	}

	response.OK(c, gin.H{
		"node_id":        id,
		"node_name":      n.Name,
		"online":         n.Online,
		"drift_detected": n.DriftDetected,
		"inbound_groups": groupIDs,
		"items":          items,
		"count":          len(items),
	})
}

// nodeRuleProtocol 推导规则在节点上实际使用的隧道协议。
//
// 规则没有独立的协议字段：入口直出（无出口组）恒为 direct，
// 否则取入口组 config 里的 protocol；入口组缺失时按默认 tls 处理
// （与 app.ruleProtocol 的口径保持一致）。
func nodeRuleProtocol(r *model.ForwardRule, g *model.DeviceGroup) string {
	if r.OutboundGroupID == 0 {
		return "direct"
	}
	if g == nil || len(g.Config) == 0 {
		return "tls"
	}
	var cfg struct {
		Protocol string `json:"protocol"`
	}
	if err := jsonUnmarshalLocal(string(g.Config), &cfg); err != nil {
		return "tls"
	}
	switch strings.ToLower(strings.TrimSpace(cfg.Protocol)) {
	case "ws", "http", "tls", "direct":
		return strings.ToLower(strings.TrimSpace(cfg.Protocol))
	}
	return "tls"
}

// NodeLogs 返回节点日志尾部（规格书 8.6 GET /nodes/:id/logs）。
//
// 现实约束：节点日志由节点侧的 systemd 收集（规格书 5.5：journalctl -fu openroute-node），
// 面板侧既没有把日志回传落库，NodeService 也没有提供读取日志的方法。
// 因此这里**不编造数据**：返回 empty=true 与一段明确的提示，
// 告诉运维去哪里看真实日志，并给出可直接复制到节点上执行的命令。
//
// 参数 ?lines=200 只做范围校验与规范化（1~2000），便于接口形状在前端先固定下来；
// 等节点侧日志回传能力落地后，本方法改为填 logs 字段即可，前端无需改动。
func (h *Handlers) NodeLogs(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "节点 ID 必须是正整数")
		return
	}
	lines := queryInt(c, "lines", 200)
	if lines < 1 || lines > 2000 {
		// 上限 2000：日志接口是给人看的，返回更多只会拖垮浏览器。
		badRequest(c, "lines", "行数必须在 1~2000 之间")
		return
	}

	ctx := c.Request.Context()
	n, err := h.app.Node.Get(ctx, id)
	if err != nil {
		response.Fail(c, err)
		return
	}

	response.OK(c, gin.H{
		"node_id":   id,
		"node_name": n.Name,
		"lines":     lines,
		"logs":      []string{},
		"empty":     true,
		"source":    "node",
		"message": "面板当前不保存节点日志。请在节点机器上执行 " +
			"journalctl -fu openroute-node 查看实时日志，或执行 " +
			"journalctl -u openroute-node -n " + itoa(lines) + " --no-pager 查看最近日志。",
	})
}

// NodeDrift 返回节点最近一次的配置漂移详情（规格书 6.14 / 8.6）。
func (h *Handlers) NodeDrift(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "节点 ID 必须是正整数")
		return
	}
	report, err := h.app.Node.GetDrift(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, report)
}

// NodeFixDrift 一键纠正配置漂移（规格书 6.14 / 8.6）。
//
// 动作是「自增配置版本 + 清空漂移标记」，由 service 完成；
// 因为 service 内部已经 BumpConfigVersion("drift_fix")，这里不再重复自增。
func (h *Handlers) NodeFixDrift(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "节点 ID 必须是正整数")
		return
	}

	ctx := c.Request.Context()
	if err := h.app.Node.FixDrift(ctx, id); err != nil {
		response.Fail(c, err)
		return
	}

	h.writeNodeAudit(c, model.ActionSync, "node", id, nil,
		gin.H{"action": "fix_drift"}, "一键纠正节点配置漂移")
	response.OKMsg(c, "已重新下发期望配置，漂移标记已清除", gin.H{"node_id": id, "drifted": false})
}

// -------------------- 批量操作 --------------------

// NodesBatchUpgrade 批量升级节点（规格书 8.6 POST /nodes/batch/upgrade）。
//
// 请求体：{"ids":[1,2,3]}，也可用 {"group_id":5} 按分组批量。
func (h *Handlers) NodesBatchUpgrade(c *gin.Context) {
	in, ok := h.bindNodeBatchInput(c, "升级")
	if !ok {
		return
	}
	ctx := c.Request.Context()
	res, err := h.app.Node.BatchUpgrade(ctx, app.NodeBatchInput{
		NodeIDs: in.IDs, GroupID: in.GroupID,
	})
	if err != nil {
		response.Fail(c, err)
		return
	}

	h.writeNodeAudit(c, model.ActionExec, "node", 0, nil,
		gin.H{"action": "batch_upgrade", "ids": in.IDs, "group_id": in.GroupID},
		fmt.Sprintf("批量升级节点：成功 %d，失败 %d", len(res.Succeeded), len(res.Failed)))
	response.OK(c, res)
}

// NodesBatchRestart 批量重启节点服务（规格书 8.6 的批量操作）。
//
// 与批量升级同构，只是下发的任务类型不同，因此复用同一套入参解析与审计口径。
func (h *Handlers) NodesBatchRestart(c *gin.Context) {
	in, ok := h.bindNodeBatchInput(c, "重启")
	if !ok {
		return
	}
	ctx := c.Request.Context()
	res, err := h.app.Node.BatchRestart(ctx, app.NodeBatchInput{
		NodeIDs: in.IDs, GroupID: in.GroupID,
	})
	if err != nil {
		response.Fail(c, err)
		return
	}

	h.writeNodeAudit(c, model.ActionExec, "node", 0, nil,
		gin.H{"action": "batch_restart", "ids": in.IDs, "group_id": in.GroupID},
		fmt.Sprintf("批量重启节点：成功 %d，失败 %d", len(res.Succeeded), len(res.Failed)))
	response.OK(c, res)
}

// NodesBatchExec 批量执行命令（规格书 8.6 POST /nodes/batch/exec）。
//
// 请求体：{"ids":[1,2,3],"command":"systemctl status openroute-node","timeout":30}。
func (h *Handlers) NodesBatchExec(c *gin.Context) {
	var body struct {
		IDs     []uint64 `json:"ids"`
		GroupID uint64   `json:"group_id"`
		Command string   `json:"command"`
		Timeout int      `json:"timeout"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		badRequest(c, "body", "请求体不是合法 JSON："+err.Error())
		return
	}
	if strings.TrimSpace(body.Command) == "" {
		badRequest(c, "command", "命令不能为空")
		return
	}
	if len(parseIDList(body.IDs)) == 0 && body.GroupID == 0 {
		badRequest(c, "ids", "请提供 ids（节点 ID 列表）或 group_id（节点分组）")
		return
	}

	ctx := c.Request.Context()
	res, err := h.app.Node.BatchExec(ctx, app.NodeBatchExecInput{
		NodeBatchInput: app.NodeBatchInput{
			NodeIDs: parseIDList(body.IDs), GroupID: body.GroupID,
		},
		Command: body.Command,
		Timeout: body.Timeout,
	})
	if err != nil {
		response.Fail(c, err)
		return
	}

	h.writeNodeAudit(c, model.ActionExec, "node", 0, nil,
		gin.H{"action": "batch_exec", "ids": body.IDs, "group_id": body.GroupID,
			"command": body.Command},
		fmt.Sprintf("批量执行命令：成功 %d，失败 %d", len(res.Succeeded), len(res.Failed)))
	response.OK(c, res)
}

// NodesBatchGroup 批量调整节点分组（规格书 8.6 POST /nodes/batch/group）。
//
// 请求体：{"ids":[1,2],"group_ids":[3,4],"mode":"add|remove|replace"}。
func (h *Handlers) NodesBatchGroup(c *gin.Context) {
	var body struct {
		IDs      []uint64 `json:"ids"`
		GroupID  uint64   `json:"group_id"`
		GroupIDs []uint64 `json:"group_ids"`
		Mode     string   `json:"mode"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		badRequest(c, "body", "请求体不是合法 JSON："+err.Error())
		return
	}
	if len(parseIDList(body.IDs)) == 0 && body.GroupID == 0 {
		badRequest(c, "ids", "请提供 ids（节点 ID 列表）或 group_id（节点分组）")
		return
	}
	// model.NodeService 不导出 GroupIDs 的语义说明，这里做一次显式校验，
	// 保证 40001 指向出错的字段而不是落到「没有匹配到任何节点」上。
	switch strings.ToLower(strings.TrimSpace(body.Mode)) {
	case "", "replace", "add", "remove":
	default:
		badRequest(c, "mode", "模式只能是 replace / add / remove")
		return
	}

	ctx := c.Request.Context()
	res, err := h.app.Node.BatchGroup(ctx, app.NodeBatchGroupInput{
		NodeBatchInput: app.NodeBatchInput{
			NodeIDs: parseIDList(body.IDs), GroupID: body.GroupID,
		},
		GroupIDs: parseIDList(body.GroupIDs),
		Mode:     body.Mode,
	})
	if err != nil {
		response.Fail(c, err)
		return
	}

	h.writeNodeAudit(c, model.ActionUpdate, "node", 0, nil,
		gin.H{"action": "batch_group", "ids": body.IDs, "group_ids": body.GroupIDs,
			"mode": body.Mode},
		fmt.Sprintf("批量改分组：成功 %d，失败 %d", len(res.Succeeded), len(res.Failed)))
	response.OK(c, res)
}

// NodesBatchWeight 批量修改节点权重（规格书 8.6 POST /nodes/batch/weight）。
//
// 请求体：{"ids":[1,2],"weight":5}，权重 0~100，0 表示不参与负载均衡。
func (h *Handlers) NodesBatchWeight(c *gin.Context) {
	var body struct {
		IDs     []uint64 `json:"ids"`
		GroupID uint64   `json:"group_id"`
		Weight  *int     `json:"weight"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		badRequest(c, "body", "请求体不是合法 JSON："+err.Error())
		return
	}
	// 用指针区分「未传 weight」与「weight=0」：后者是合法输入
	// （表示从负载均衡中剔除），前者是漏填字段的错误。
	if body.Weight == nil {
		badRequest(c, "weight", "请提供 weight（0~100，0 表示不参与负载均衡）")
		return
	}
	if len(parseIDList(body.IDs)) == 0 && body.GroupID == 0 {
		badRequest(c, "ids", "请提供 ids（节点 ID 列表）或 group_id（节点分组）")
		return
	}

	ctx := c.Request.Context()
	res, err := h.app.Node.BatchWeight(ctx, app.NodeBatchWeightInput{
		NodeBatchInput: app.NodeBatchInput{
			NodeIDs: parseIDList(body.IDs), GroupID: body.GroupID,
		},
		Weight: *body.Weight,
	})
	if err != nil {
		response.Fail(c, err)
		return
	}

	h.writeNodeAudit(c, model.ActionUpdate, "node", 0, nil,
		gin.H{"action": "batch_weight", "ids": body.IDs, "weight": *body.Weight},
		fmt.Sprintf("批量改权重：成功 %d，失败 %d", len(res.Succeeded), len(res.Failed)))
	response.OK(c, res)
}

// nodeBatchIDs 是批量操作请求体的公共部分。
//
// 规格书 8.6 的示例用 `ids`；同时接受 `node_ids` 与 `group_id`，
// 让前端与 API Token 调用方都能用自己习惯的写法。
type nodeBatchIDs struct {
	IDs     []uint64 `json:"ids"`
	NodeIDs []uint64 `json:"node_ids"`
	GroupID uint64   `json:"group_id"`
}

// bindNodeBatchInput 解析批量操作的公共入参并做「至少选了一个目标」的校验。
//
// 参数 action 仅用于错误提示文案。返回解析结果与是否通过校验。
func (h *Handlers) bindNodeBatchInput(c *gin.Context, action string) (nodeBatchIDs, bool) {
	var in nodeBatchIDs
	if err := c.ShouldBindJSON(&in); err != nil {
		badRequest(c, "body", "请求体不是合法 JSON："+err.Error())
		return in, false
	}
	// ids 与 node_ids 互为别名，合并后去重。
	in.IDs = parseIDList(append(append([]uint64{}, in.IDs...), in.NodeIDs...))
	if len(in.IDs) == 0 && in.GroupID == 0 {
		badRequest(c, "ids", "请提供 ids（节点 ID 列表）或 group_id（节点分组）后再"+action)
		return in, false
	}
	return in, true
}

// -------------------- WebSocket --------------------
//
// NodesStream（节点状态实时推送）与 NodeTerminal（WebSSH 终端）的
// 完整实现在 handler_ws.go 中，本文件不再重复声明。

// -------------------- 本文件内部工具 --------------------

// writeNodeAudit 写一条节点相关的审计记录（规格书 6.1 / 11.14）。
//
// 统一在这里取当前用户、来源 IP 与 User-Agent，避免每个 handler 重复四行样板。
func (h *Handlers) writeNodeAudit(c *gin.Context, action, resource string, resourceID uint64,
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

// writeNodeAuditResult 写一条指定结果的审计记录（用于失败也要留痕的动作）。
func (h *Handlers) writeNodeAuditResult(c *gin.Context, action, resource string, resourceID uint64,
	result, message string, extra interface{}) {

	uid, un, _, _ := middleware.CurrentUser(c)
	h.app.Audit.Write(c.Request.Context(), app.AuditEntry{
		UserID:     uid,
		Username:   un,
		Action:     action,
		Resource:   resource,
		ResourceID: resourceID,
		After:      extra,
		IP:         util.ClientIP(c.Request),
		UserAgent:  c.GetHeader("User-Agent"),
		Result:     result,
		Message:    message,
	})
}

// jsonUnmarshalLocal 是本包内的极简 JSON 反序列化封装。
//
// 存在的理由：api 包不能调用 app 包未导出的 jsonUnmarshal，
// 而 NodeRules 需要从设备组 config 里读一个字段。用 encoding/json 直接做，
// 失败一律由调用方回退到安全默认值，因此这里不引入新的错误类型。
func jsonUnmarshalLocal(raw string, v interface{}) error {
	if strings.TrimSpace(raw) == "" {
		return errors.New("空的 JSON 文本")
	}
	return json.Unmarshal([]byte(raw), v)
}

// nodeGroupHitLocal 判断分组 ID 是否出现在节点的分组列表中。
//
// app 包内有同名函数，但未导出；这里保留一份等价实现，
// 让 NodeRules 的成员判断不依赖 app 的内部工具。
func nodeGroupHitLocal(ids []uint64, target uint64) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}
