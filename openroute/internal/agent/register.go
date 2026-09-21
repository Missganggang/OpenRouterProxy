package agent

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/openroute/openroute/internal/api/response"
	"github.com/openroute/openroute/internal/app"
	"github.com/openroute/openroute/internal/model"
	"github.com/openroute/openroute/internal/util"
)

// Registry 是节点通信接口的处理器集合。
//
// 所有节点通信接口（/api/node/*）不带 /v1 版本前缀（规格书 8.1），
// 因为节点客户端与面板版本是绑定发布的，不需要独立演进。
type Registry struct {
	app *app.App
	// cfg 缓存节点侧配置生成器，避免每次请求重复构造。
	cfg *ConfigBuilder
}

// NewRegistry 构造节点通信处理器集合。
//
// 参数 a 为运行时依赖容器；返回可直接注册路由的 *Registry。
func NewRegistry(a *app.App) *Registry {
	return &Registry{app: a, cfg: NewConfigBuilder(a)}
}

// App 返回处理器持有的 App 容器，供路由层与测试使用。
func (r *Registry) App() *app.App { return r.app }

// ConfigBuilder 返回配置生成器，供路由层单独调用（如 /api/node/config）。
func (r *Registry) ConfigBuilder() *ConfigBuilder { return r.cfg }

// ---------------------------------------------------------------------------
// 注册
// ---------------------------------------------------------------------------

// RegisterHandler 处理节点首次注册（POST /api/node/register，规格书 5.1）。
//
// 流程：
//  1. 用请求体中的 token 找到节点（注册阶段节点还没有其它身份）；
//  2. 标记节点在线、写入上报的系统信息；
//  3. 返回节点 ID 与初始全量配置。
//
// 返回 HTTP 状态码与响应体由 Gin 写回；token 无效时返回 401 + 60004。
func (r *Registry) RegisterHandler(c *gin.Context) {
	var req RegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, response.Field(response.CodeParamInvalid, "body", nil, "请求体不是合法 JSON"))
		return
	}
	req.Token = strings.TrimSpace(req.Token)
	if req.Token == "" {
		fail(c, response.Field(response.CodeParamInvalid, "token", "", "节点密钥不能为空"))
		return
	}

	ctx := c.Request.Context()
	node, err := r.nodeByToken(ctx, req.Token)
	if err != nil {
		fail(c, err)
		return
	}

	// 注册时维护节点在线状态：注册本身就是一次「我还活着」的声明。
	now := time.Now().UTC()
	wasOnline := node.Online
	updates := map[string]interface{}{
		"disable_execute": req.DisableExecute,
		"online":          true,
		"last_seen":       now,
		"client_ver":      util.Truncate(req.Version, 32),
		"config_version":  req.ConfigVersion,
		"last_error":      "",
		"updated_at":      now,
	}
	applySystemInfo(updates, req.System)
	networkChanged, err := applyNetworkInfo(node, req.Network, updates)
	if err != nil {
		fail(c, err)
		return
	}

	if err := r.app.DB.WithContext(ctx).Model(&model.Node{}).Where("id = ?", node.ID).
		Updates(updates).Error; err != nil {
		fail(c, response.Wrap(response.CodeInternal, err, "更新节点注册信息失败"))
		return
	}

	if networkChanged {
		r.app.BumpConfigVersion("node_network_changed")
	}

	// 刷新内存中的节点副本，保证后续生成配置时拿到最新字段。
	node.Online = true
	node.LastSeen = &now
	applySystemInfoToNode(node, req.System)
	node.ClientVer = util.Truncate(req.Version, 32)
	node.DisableExecute = req.DisableExecute
	if !wasOnline && r.app.Alert != nil {
		r.app.Alert.NotifyEvent(ctx, app.EventNodeOnline, map[string]interface{}{"node_id": node.ID, "name": node.Name})
	}

	// 注册等价于一次首连：广播上线事件，WebUI 立即把节点刷成在线。
	r.app.Hub().Broadcast(app.Event{
		Type: "node_online",
		Data: map[string]interface{}{
			"node_id":   node.ID,
			"name":      node.Name,
			"public_ip": node.PublicIPv4,
		},
	})

	// 注册总是返回全量配置：节点刚从安装脚本启动，本地没有可用配置。
	cfgResp, err := r.cfg.BuildFull(ctx, node)
	if err != nil {
		fail(c, err)
		return
	}

	r.app.Log.Info("节点注册成功",
		zap.Uint64("node_id", node.ID),
		zap.String("name", node.Name),
		zap.String("os", req.System.OS),
		zap.String("arch", req.System.Arch),
		zap.String("client_ver", req.Version))

	c.JSON(http.StatusOK, RegisterResponse{
		NodeID:            node.ID,
		NodeName:          node.Name,
		Role:              node.Role,
		IsOutbound:        node.IsOutbound(),
		Config:            *cfgResp,
		HeartbeatInterval: r.heartbeatInterval(),
		ServerTime:        now.Unix(),
	})
}

// ---------------------------------------------------------------------------
// 心跳
// ---------------------------------------------------------------------------

// HeartbeatHandler 处理节点心跳（POST /api/node/heartbeat，规格书 8.16）。
//
// 认证用 X-Node-Token 请求头。一次心跳完成四件事：
//  1. 更新节点的 LastSeen / 在线状态 / 运行时指标；
//  2. 写入一条探针指标（probe_metrics）；
//  3. 若节点由离线转为在线，广播 node_online 事件；
//  4. 对比 config_version，告诉节点是否需要重新拉取配置。
//
// 返回心跳响应体（含 need_config 与附带的任务列表）。
func (r *Registry) HeartbeatHandler(c *gin.Context) {
	node, err := r.authenticate(c)
	if err != nil {
		fail(c, err)
		return
	}

	var req HeartbeatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, response.Field(response.CodeParamInvalid, "body", nil, "请求体不是合法 JSON"))
		return
	}
	// 请求体里的 node_id 与 token 解析出的节点不一致时，以 token 为准，
	// 但记录一条告警：这是「一台机器被复制部署」的典型症状。
	if req.NodeID != 0 && req.NodeID != node.ID {
		r.app.Log.Warn("心跳中的 node_id 与 token 不匹配，已按 token 处理",
			zap.Uint64("token_node_id", node.ID),
			zap.Uint64("body_node_id", req.NodeID))
	}

	ctx := c.Request.Context()
	now := time.Now().UTC()
	wasOnline := node.Online

	updates := map[string]interface{}{
		"disable_execute": req.DisableExecute,
		"config_hash":     util.Truncate(req.ConfigHash, 64),
		"online":          true,
		"last_seen":       now,
		"config_version":  req.ConfigVersion,
		"cpu_usage":       clampMetrics(req.Metrics.CPU),
		"mem_used":        req.Metrics.MemUsed,
		"disk_used":       req.Metrics.DiskUsed,
		"net_in_speed":    req.Metrics.NetInSpeed,
		"net_out_speed":   req.Metrics.NetOutSpeed,
		"load1":           req.Metrics.Load1,
		"uptime":          req.Metrics.Uptime,
		"current_conn":    req.Metrics.TcpConn + req.Metrics.UdpConn,
		"updated_at":      now,
	}
	if req.Version != "" {
		updates["client_ver"] = util.Truncate(req.Version, 32)
	}
	if req.Metrics.MemTotal > 0 {
		updates["mem_total"] = req.Metrics.MemTotal
	}
	if req.Metrics.DiskTotal > 0 {
		updates["disk_total"] = req.Metrics.DiskTotal
	}
	networkChanged, err := applyNetworkInfo(node, req.Network, updates)
	if err != nil {
		fail(c, err)
		return
	}

	if err := r.app.DB.WithContext(ctx).Model(&model.Node{}).Where("id = ?", node.ID).
		Updates(updates).Error; err != nil {
		fail(c, response.Wrap(response.CodeInternal, err, "更新节点心跳失败"))
		return
	}

	if networkChanged {
		r.app.BumpConfigVersion("node_network_changed")
	}

	// 写入探针指标。失败不影响心跳（指标是旁路数据），仅记日志。
	r.recordProbe(ctx, node.ID, req.Metrics)
	// 漂移检测：把节点上报的运行规则与面板期望配置比对（规格书 6.14）。
	drifted := false
	if req.ConfigHash != "" {
		drifted = r.checkConfigDrift(ctx, node, req)
	} else {
		r.checkDrift(ctx, node.ID, req.RunningRules)
	}

	if !wasOnline {
		if r.app.Alert != nil {
			r.app.Alert.NotifyEvent(ctx, app.EventNodeOnline, map[string]interface{}{"node_id": node.ID, "name": node.Name})
		}
		r.app.Hub().Broadcast(app.Event{
			Type: "node_online",
			Data: map[string]interface{}{
				"node_id": node.ID,
				"name":    node.Name,
			},
		})
		r.app.Log.Info("节点已上线", zap.Uint64("node_id", node.ID), zap.String("name", node.Name))
	}

	// 节点上报的版本落后于面板当前版本 → 需要拉配置。
	current := r.app.ConfigVersion()
	needConfig := req.ConfigVersion != current || drifted

	resp := HeartbeatResponse{
		NeedConfig:        needConfig,
		ConfigVersion:     current,
		HeartbeatInterval: r.heartbeatInterval(),
		ServerTime:        now.Unix(),
	}
	// 需要新配置时顺手把待执行任务带回去，省一次轮询。
	if tasks, err := r.pendingTasks(ctx, node.ID); err == nil {
		resp.Tasks = tasks
	}
	c.JSON(http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// 认证
// ---------------------------------------------------------------------------

// authenticate 从请求头取出节点 token 并解析出节点。
//
// 供除注册外的全部节点通信接口使用。失败时返回 401 + 60004。
func (r *Registry) authenticate(c *gin.Context) (*model.Node, error) {
	token := strings.TrimSpace(c.GetHeader(NodeTokenHeader))
	if token == "" {
		// 兼容：部分节点客户端会把 token 放在 Authorization: Bearer 里。
		auth := strings.TrimSpace(c.GetHeader("Authorization"))
		if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			token = strings.TrimSpace(auth[len("bearer "):])
		}
	}
	if token == "" {
		return nil, response.Field(response.CodeNodeTokenInvalid, NodeTokenHeader, "",
			"缺少节点密钥请求头")
	}
	return r.nodeByToken(c.Request.Context(), token)
}

// nodeByToken 按 token 精确查找节点。
//
// token 是随机串，不存在大小写变体，因此这里不做 EqualFold 归一化
// （规格书 4.1 的归一化要求只针对「名称」）。
func (r *Registry) nodeByToken(ctx context.Context, token string) (*model.Node, error) {
	var n model.Node
	if err := r.app.DB.WithContext(ctx).Where("token = ?", token).First(&n).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, response.Field(response.CodeNodeTokenInvalid, "token", util.MaskSecret(token),
				"节点密钥无效，请重新执行安装命令或在面板重置密钥")
		}
		return nil, response.Wrap(response.CodeInternal, err, "查询节点失败")
	}
	return &n, nil
}

// ---------------------------------------------------------------------------
// 内部工具
// ---------------------------------------------------------------------------

// applySystemInfo 把注册上报的系统信息写入待更新的字段映射。
func applySystemInfo(updates map[string]interface{}, s RegisterSystem) {
	if v := util.Truncate(strings.TrimSpace(s.PublicIPv4), 64); v != "" {
		updates["public_ipv4"] = v
	}
	if v := util.Truncate(strings.TrimSpace(s.PublicIPv6), 64); v != "" {
		updates["public_ipv6"] = v
	}
	if v := util.Truncate(strings.TrimSpace(s.PrivateIP), 64); v != "" {
		updates["private_ip"] = v
	}
	if v := util.Truncate(strings.TrimSpace(s.OS), 64); v != "" {
		updates["os"] = v
	}
	if v := util.Truncate(strings.TrimSpace(s.Arch), 32); v != "" {
		updates["arch"] = v
	}
	if v := util.Truncate(strings.TrimSpace(s.KernelVer), 64); v != "" {
		updates["kernel_ver"] = v
	}
	if v := util.Truncate(strings.TrimSpace(s.CPUModel), 128); v != "" {
		updates["cpu_model"] = v
	}
	if s.CPUCores > 0 {
		updates["cpu_cores"] = s.CPUCores
	}
	if s.MemTotal > 0 {
		updates["mem_total"] = s.MemTotal
	}
	if s.DiskTotal > 0 {
		updates["disk_total"] = s.DiskTotal
	}
	if s.BootTime > 0 {
		bt := time.Unix(s.BootTime, 0).UTC()
		updates["boot_time"] = bt
	}
}

// applySystemInfoToNode 把注册信息同步写回内存中的节点对象。
//
// 用于紧接着生成配置——避免再查一次库。
func applySystemInfoToNode(n *model.Node, s RegisterSystem) {
	if s.PublicIPv4 != "" {
		n.PublicIPv4 = s.PublicIPv4
	}
	if s.PublicIPv6 != "" {
		n.PublicIPv6 = s.PublicIPv6
	}
	if s.PrivateIP != "" {
		n.PrivateIP = s.PrivateIP
	}
	if s.CPUModel != "" {
		n.CPUModel = s.CPUModel
	}
	if s.CPUCores > 0 {
		n.CPUCores = s.CPUCores
	}
	if s.MemTotal > 0 {
		n.MemTotal = s.MemTotal
	}
	if s.DiskTotal > 0 {
		n.DiskTotal = s.DiskTotal
	}
}

// recordProbe 把心跳指标写入 probe_metrics。
//
// 探针关闭或写入失败都不影响心跳流程，只写日志。
func (r *Registry) recordProbe(ctx context.Context, nodeID uint64, m HeartbeatMetrics) {
	if !r.app.Config.EnableProbe {
		return
	}
	metric := model.ProbeMetric{
		NodeID:      nodeID,
		Timestamp:   time.Now().UTC().Unix(),
		CPU:         clampMetrics(m.CPU),
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
		Load1:       m.Load1,
		Load5:       m.Load5,
		Load15:      m.Load15,
		TcpConn:     m.TcpConn,
		UdpConn:     m.UdpConn,
		Uptime:      m.Uptime,
		CreatedAt:   time.Now().UTC(),
	}
	if err := r.app.Probe.RecordMetrics(ctx, nodeID, metric); err != nil {
		r.app.Log.Debug("写入探针指标失败",
			zap.Uint64("node_id", nodeID),
			zap.Error(err))
	}
}

// checkDrift 执行一次配置漂移比对并落库（规格书 6.14）。
//
// 只在节点上报了运行规则时比对；比对失败只记日志，不影响心跳。
func (r *Registry) checkDrift(ctx context.Context, nodeID uint64, rules []RunningRule) {
	if rules == nil {
		return
	}
	actual := make([]app.RunningRuleSummary, 0, len(rules))
	for _, ru := range rules {
		actual = append(actual, app.RunningRuleSummary{
			RuleID: ru.RuleID,
			Port:   ru.Port,
			Status: ru.Status,
			Conn:   ru.Conn,
		})
	}
	report, err := r.app.Node.DetectDrift(ctx, nodeID, actual)
	if err != nil {
		r.app.Log.Debug("配置漂移检测失败", zap.Uint64("node_id", nodeID), zap.Error(err))
		return
	}
	if err := r.app.Node.SaveDrift(ctx, report); err != nil {
		r.app.Log.Debug("保存配置漂移结果失败", zap.Uint64("node_id", nodeID), zap.Error(err))
	}
}

// pendingTasks 查询节点待执行任务并转换为协议结构。
func (r *Registry) pendingTasks(ctx context.Context, nodeID uint64) ([]TaskItem, error) {
	rows, err := r.app.Node.PendingTasks(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	items := make([]TaskItem, 0, len(rows))
	for i := range rows {
		items = append(items, TaskItem{
			TaskID:    rows[i].ID,
			Type:      rows[i].Type,
			Payload:   decodePayload(rows[i].Payload),
			CreatedAt: rows[i].CreatedAt.UTC().Unix(),
		})
	}
	return items, nil
}

// heartbeatInterval 返回心跳间隔，配置缺失时回退到默认值。
func (r *Registry) heartbeatInterval() int {
	if v := r.app.Config.HeartbeatInterval; v > 0 {
		return v
	}
	return HeartbeatIntervalDefault
}

// clampMetrics 把 CPU 使用率限制在 0~100，防止节点上报异常值污染曲线。
func clampMetrics(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// decodePayload 把任务载荷 JSON 解析为 map；解析失败返回空 map。
func decodePayload(raw model.JSON) map[string]interface{} {
	out := map[string]interface{}{}
	if len(raw) == 0 || string(raw) == "null" {
		return out
	}
	if err := jsonUnmarshal(string(raw), &out); err != nil {
		return map[string]interface{}{}
	}
	if out == nil {
		return map[string]interface{}{}
	}
	return out
}

// fail 把业务错误按统一响应体写回。
//
// 与面板管理接口共用 response.AppError 的错误码字典（规格书 8.4），
// 节点侧据此做分支处理（例如 60004 时重新注册）。
func fail(c *gin.Context, err error) {
	ae := response.AsAppError(err)
	c.AbortWithStatusJSON(ae.HTTPStatus, ErrorResponse{
		Code:    ae.Code,
		Message: ae.Msg,
	})
}
