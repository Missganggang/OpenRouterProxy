package agent

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
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

// ConfigBuilder 负责把面板侧的规则与设备组「编译」成节点能直接使用的配置。
//
// 这是「面板期望配置」的唯一来源：漂移检测（规格书 6.14）与一键纠正都
// 以本构建器的输出为基准，因此任何下发语义的改动都要同时改这里。
type ConfigBuilder struct {
	app *app.App
}

// NewConfigBuilder 构造配置生成器。
//
// 参数 a 为运行时依赖容器；返回可直接使用的 *ConfigBuilder。
func NewConfigBuilder(a *app.App) *ConfigBuilder {
	return &ConfigBuilder{app: a}
}

// BuildFull 生成节点的全量配置（规格书 8.16 配置响应，full = true）。
//
// 参数 ctx 为上下文；n 为目标节点（需已带上节点 ID 与角色）。
// 返回全量配置报文或错误。
func (b *ConfigBuilder) BuildFull(ctx context.Context, n *model.Node) (*ConfigResponse, error) {
	return b.build(ctx, n, nil)
}

// BuildIncremental 生成节点的增量配置（规格书 8.16，full = false）。
//
// 参数 removedRuleIDs 为上一次下发后已从节点移除的规则 ID。
// 增量内容仍是该节点当前的完整规则集，但会附带 removed_rule_ids，
// 让节点在一次应用里完成「增 + 删」，避免出现半旧半新的中间态。
func (b *ConfigBuilder) BuildIncremental(ctx context.Context, n *model.Node, removedRuleIDs []uint64) (*ConfigResponse, error) {
	return b.build(ctx, n, removedRuleIDs)
}

// build 是 BuildFull / BuildIncremental 的公共实现。
func (b *ConfigBuilder) build(ctx context.Context, n *model.Node, removed []uint64) (*ConfigResponse, error) {
	if n == nil || n.ID == 0 {
		return nil, response.Field(response.CodeParamInvalid, "node_id", 0, "节点 ID 不能为空")
	}
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Take the version before any configuration reads. Tagging a response
		// with a version read at the end can permanently hide a concurrent edit.
		version := b.app.ConfigVersion()
		var current model.Node
		if err := b.app.DB.WithContext(ctx).First(&current, n.ID).Error; err != nil {
			return nil, err
		}
		resp, hashes, err := b.buildVersion(ctx, &current, removed, version)
		if version != b.app.ConfigVersion() {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err = b.saveTrafficPolicies(ctx, resp, hashes); err != nil {
			if version != b.app.ConfigVersion() {
				continue
			}
			return nil, err
		}
		if version == b.app.ConfigVersion() {
			return resp, nil
		}
	}
	return nil, response.New(response.CodeStateConflict, "配置正在更新，请重试获取节点配置")
}

func (b *ConfigBuilder) buildVersion(ctx context.Context, n *model.Node, removed []uint64, version int64) (*ConfigResponse, map[uint64]string, error) {
	groups, err := b.loadGroups(ctx)
	if err != nil {
		return nil, nil, err
	}

	rules, hashes, err := b.rulesForNodeWithHashes(ctx, n, groups)
	if err != nil {
		return nil, nil, err
	}
	if err := b.applyUserPolicies(ctx, rules); err != nil {
		return nil, nil, err
	}

	dgConfig, err := b.deviceGroupConfigs(ctx, n, groups)
	if err != nil {
		return nil, nil, err
	}

	// 全量下发的判定依据是「调用方是否传了要删除的规则」：
	// 传 nil 表示全量重建，传非 nil 表示增量（可能为空增量）。
	full := removed == nil
	if removed == nil {
		removed = []uint64{}
	}
	listeners := nodePorts(n)
	listeners.TLSCertPEM, listeners.TLSKeyPEM, _, err = b.nodeCertificate(n.ID)
	if err != nil {
		return nil, nil, err
	}
	return &ConfigResponse{
		NodeID:            n.ID,
		NodeDisabled:      n.Disabled,
		Listeners:         listeners,
		ConfigVersion:     version,
		Full:              full,
		Rules:             rules,
		RemovedRuleIDs:    removed,
		DeviceGroupConfig: dgConfig,
		HeartbeatInterval: b.HeartbeatInterval(),
		GeneratedAt:       time.Now().UTC().Unix(),
	}, hashes, nil
}

// HeartbeatInterval 返回面板规定的心跳间隔（秒），配置缺失时回退默认值。
func (b *ConfigBuilder) HeartbeatInterval() int {
	if v := b.app.Config.HeartbeatInterval; v > 0 {
		return v
	}
	return HeartbeatIntervalDefault
}

// FullMarkerConfig 生成一份显式的全量配置，用于「一键纠正漂移」。
//
// 与 BuildFull 的差别只在 full 字段恒为 true，语义上更明确。
func (b *ConfigBuilder) FullMarkerConfig(ctx context.Context, n *model.Node) (*ConfigResponse, error) {
	resp, err := b.build(ctx, n, nil)
	if err != nil {
		return nil, err
	}
	resp.Full = true
	resp.RemovedRuleIDs = []uint64{}
	return resp, nil
}

// LoadRulesForNode 是对外暴露的规则编译入口，供 handler 层复用。
//
// 参数 ctx 为上下文；nodeID 为节点 ID。返回该节点应运行的规则列表。
func (b *ConfigBuilder) LoadRulesForNode(ctx context.Context, nodeID uint64) ([]ConfigRule, error) {
	var n model.Node
	if err := b.app.DB.WithContext(ctx).First(&n, nodeID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, response.Field(response.CodeNotFound, "node_id", nodeID, "节点不存在")
		}
		return nil, response.Wrap(response.CodeInternal, err, "查询节点失败")
	}
	groups, err := b.loadGroups(ctx)
	if err != nil {
		return nil, err
	}
	return b.rulesForNode(ctx, &n, groups)
}

// loadGroups 一次性载入全部设备组，按 ID 索引。
func (b *ConfigBuilder) loadGroups(ctx context.Context) (map[uint64]*model.DeviceGroup, error) {
	var list []model.DeviceGroup
	if err := b.app.DB.WithContext(ctx).Find(&list).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "查询设备组失败")
	}
	out := make(map[uint64]*model.DeviceGroup, len(list))
	for i := range list {
		out[list[i].ID] = &list[i]
	}
	return out, nil
}

// rulesForNode 编译出某节点上应当运行的规则集合。
//
// 判定规则（规格书 6.4 / 6.7）：
//   - 节点必须是指向它的入口设备组的成员（入口侧规则）；
//   - 节点也是出口组成员时，同一条规则会以 is_outbound = true 再次下发；
//   - 节点既不是入口成员也不是出口成员时，该规则与它无关；
//   - 关闭（enable = false）的规则照常下发但带 enable = false，节点据此停监听。
func (b *ConfigBuilder) rulesForNode(ctx context.Context, n *model.Node, groups map[uint64]*model.DeviceGroup) ([]ConfigRule, error) {
	rules, _, err := b.rulesForNodeWithHashes(ctx, n, groups)
	return rules, err
}

func (b *ConfigBuilder) rulesForNodeWithHashes(ctx context.Context, n *model.Node, groups map[uint64]*model.DeviceGroup) ([]ConfigRule, map[uint64]string, error) {
	var rules []model.ForwardRule
	if err := b.app.DB.WithContext(ctx).Order("id ASC").Find(&rules).Error; err != nil {
		return nil, nil, response.Wrap(response.CodeInternal, err, "查询转发规则失败")
	}

	out := make([]ConfigRule, 0, len(rules))
	hashes := make(map[uint64]string)
	for i := range rules {
		r := &rules[i]

		inGroup := groups[r.InboundGroupID]
		outGroup := groups[r.OutboundGroupID]

		isInboundNode, isOutboundNode := model.RuleNodeRoles(r, n.ID, groups)

		if !isInboundNode && !isOutboundNode {
			continue
		}
		hashes[r.ID] = model.RuleConfigHash(*r)

		// 入口视角与出口视角分别下发。单端（无出口组）时只有入口视角。
		if isInboundNode {
			out = append(out, b.compileRule(r, n, inGroup, outGroup, false))
		}
		if isOutboundNode {
			out = append(out, b.compileRule(r, n, inGroup, outGroup, true))
		}
	}
	return out, hashes, nil
}

// compileRule 把一条规则编译成节点可用的下发结构。
//
// 参数 isOutbound 决定「监听端口」与「目标地址」的语义视角：
// 入口侧关心监听端口与隧道参数，出口侧关心真实目标与连接地址。
func (b *ConfigBuilder) compileRule(r *model.ForwardRule, n *model.Node,
	inGroup, outGroup *model.DeviceGroup, isOutbound bool) ConfigRule {

	cr := ConfigRule{
		TunnelToken:        hex.EncodeToString(b.deriveSecret("rule-tunnel", r.ID)),
		UserID:             r.UserID,
		RuleID:             r.ID,
		Name:               r.Name,
		IsOutbound:         isOutbound,
		InboundGroupID:     r.InboundGroupID,
		OutboundGroupID:    r.OutboundGroupID,
		ListenPort:         r.ListenPort,
		ListenPortEnd:      r.ListenPortEnd,
		Protocol:           ruleProtocol(r, inGroup),
		Targets:            compileTargets(r),
		Options:            rawOptions(r.Options),
		TargetBalance:      defaultString(r.TargetBalance, model.TargetBalanceFailover),
		SpeedLimit:         r.SpeedLimit,
		ConnLimit:          r.ConnLimit,
		IPLimit:            r.IPLimit,
		InboundMultiplier:  r.InboundMultiplier,
		OutboundMultiplier: r.OutboundMultiplier,
		ReverseEnable:      r.ReverseEnable,
		ReversePort:        r.ReversePort,
		ReverseGroupID:     r.ReverseGroupID,
		ChainGroups:        r.ChainGroupList(),
		IsSubRule:          r.IsSubRule,
		ParentID:           r.ParentID,
		SNI:                r.SNI,
		Shaping:            r.ShapingList(),
		Enable:             r.Enable && !n.Disabled,
	}
	if cr.Targets == nil {
		cr.Targets = []ConfigTarget{}
	}
	if cr.ChainGroups == nil {
		cr.ChainGroups = []uint64{}
	}
	if cr.Shaping == nil {
		cr.Shaping = []int{}
	}
	return cr
}

// compileTargets 解析规则的目标地址列表，过滤掉无效目标。
func compileTargets(r *model.ForwardRule) []ConfigTarget {
	list := r.TargetList()
	out := make([]ConfigTarget, 0, len(list))
	for _, t := range list {
		host := strings.TrimSpace(t.Host)
		if host == "" || t.Port <= 0 || t.Port > 65535 {
			continue
		}
		out = append(out, ConfigTarget{
			Host:   host,
			Port:   t.Port,
			Weight: t.NormalizedWeight(),
			Status: defaultString(t.Status, model.TargetUp),
		})
	}
	return out
}

// rawOptions 把规则 Options JSON 解析成 map。
//
// 解析失败时返回空 map 而不是报错：一条规则的坏 Options 不应阻断整次下发。
func rawOptions(raw model.JSON) map[string]interface{} {
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

// deviceGroupConfigs 生成该节点相关的设备组配置。
//
// 只下发与该节点有关的组：节点是成员的组，以及这些组引用的故障转移组 /
// 反向组 / 链式组——节点侧需要知道对端的连接地址才能建连。
func (b *ConfigBuilder) deviceGroupConfigs(ctx context.Context, n *model.Node,
	groups map[uint64]*model.DeviceGroup) (map[string]DeviceGroupConfig, error) {

	// 1. 收集相关组 ID。
	relevant := map[uint64]bool{}
	for id, g := range groups {
		if containsID(g.NodeIDs.AsUint64Slice(), n.ID) {
			relevant[id] = true
		}
	}
	// 2. 补上被引用的组（故障转移组）。
	var rules []model.ForwardRule
	if err := b.app.DB.WithContext(ctx).Find(&rules).Error; err != nil {
		return nil, err
	}
	for i := range rules {
		in, out := model.RuleNodeRoles(&rules[i], n.ID, groups)
		if in || out {
			for id := range model.RuleGroupIDs(&rules[i], groups) {
				relevant[id] = true
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for id := range relevant {
			if g := groups[id]; g != nil && g.FailoverGroupID != 0 && !relevant[g.FailoverGroupID] {
				relevant[g.FailoverGroupID] = true
				changed = true
			}
		}
	}

	// 3. 需要展示的连接地址信息：所有组内节点都要载入。
	peerIDs := map[uint64]bool{}
	for id := range relevant {
		if g := groups[id]; g != nil {
			for _, nid := range g.NodeIDs.AsUint64Slice() {
				peerIDs[nid] = true
			}
		}
	}
	peers, err := b.loadPeers(ctx, peerIDs)
	if err != nil {
		return nil, err
	}

	out := make(map[string]DeviceGroupConfig, len(relevant))
	for id := range relevant {
		g := groups[id]
		if g == nil {
			continue
		}
		resolved, err := b.resolvePeers(g, peers)
		if err != nil {
			return nil, err
		}
		out[strconv.FormatUint(id, 10)] = DeviceGroupConfig{
			GroupID:              g.ID,
			Name:                 g.Name,
			Type:                 g.Type,
			NodeIDs:              g.NodeIDs.AsUint64Slice(),
			Balance:              defaultString(g.Balance, model.BalanceLeastConn),
			HealthCheckEnable:    g.HealthCheckEnable,
			HealthCheckInterval:  g.HealthCheckInterval,
			HealthCheckTimeout:   g.HealthCheckTimeout,
			HealthCheckFailCount: g.HealthCheckFailCount,
			HealthCheckSuccCount: g.HealthCheckSuccCount,
			FailoverGroupID:      g.FailoverGroupID,
			Config:               rawOptions(g.Config),
			Peers:                resolved,
		}
	}
	return out, nil
}

// loadPeers 批量载入节点，按 ID 索引。
func (b *ConfigBuilder) loadPeers(ctx context.Context, ids map[uint64]bool) (map[uint64]*model.Node, error) {
	out := map[uint64]*model.Node{}
	if len(ids) == 0 {
		return out, nil
	}
	list := make([]uint64, 0, len(ids))
	for id := range ids {
		list = append(list, id)
	}
	sort.Slice(list, func(i, j int) bool { return list[i] < list[j] })

	var nodes []model.Node
	if err := b.app.DB.WithContext(ctx).Where("id IN ?", list).Find(&nodes).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "查询组内节点失败")
	}
	for i := range nodes {
		out[nodes[i].ID] = &nodes[i]
	}
	return out, nil
}

// resolvePeers 把设备组的成员解析成带连接地址的 peer 列表。
//
// 成员顺序即设备组的 NodeIDs 顺序：出口组的顺序参与负载均衡初始化
// （规格书 4.2.5），因此这里 MUST 保持原序，不能排序。
func (b *ConfigBuilder) resolvePeers(g *model.DeviceGroup, peers map[uint64]*model.Node) ([]GroupPeer, error) {
	ids := g.NodeIDs.AsUint64Slice()
	out := make([]GroupPeer, 0, len(ids))
	for _, id := range ids {
		n := peers[id]
		if n == nil {
			continue
		}
		host, connectPort := resolveConnectAddress(g, n)
		ports := nodePorts(n)
		wsPort := ports.WsPort
		// 静态连接地址显式给了端口时，用它覆盖节点上报的 ws 端口，
		// 这是「域名 + 非默认端口」部署模式下唯一可靠的连接端口来源。
		if connectPort > 0 && strings.EqualFold(fmt.Sprint(rawOptions(g.Config)["connect_type"]), "static") {
			wsPort = connectPort
			ports.DirectPort, ports.TlsPort, ports.UdpPort = connectPort, connectPort, connectPort
		}
		_, _, tlsPin, err := b.nodeCertificate(n.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, GroupPeer{
			TLSPin:      tlsPin,
			CurrentConn: n.CurrentConn,
			MaxConn:     n.MaxConn,
			NodeID:      n.ID,
			Name:        n.Name,
			Host:        host,
			DirectPort:  ports.DirectPort,
			WsPort:      wsPort,
			TlsPort:     ports.TlsPort,
			UdpPort:     ports.UdpPort,
			RevPort:     ports.RevPort,
			Weight:      normalizeWeight(n.Weight),
			Online:      n.Online && !n.Disabled,
		})
	}
	return out, nil
}

// resolveConnectAddress 按规格书 4.3 的连接地址优先级解析出对端地址。
//
// 优先级：
//
//  1. connect_type == "static" && connect_address != ""  → connect_address + connect_port
//  2. connect_host != ""                                  → connect_host
//  3. dyn_ip4 可用                                        → 动态 IPv4
//  4. ipv6_group 配置且当前节点在组内                       → 动态 IPv6
//  5. dyn_ip6 可用                                        → 动态 IPv6
//  6. 回退                                                → 上报的公网 IPv4
//
// 无回退机制：选中 IPv6 且不可达时不会自动退回 IPv4（规格书 4.3 的注意事项），
// 因此这里只在确实配置了 ipv6_group 或节点没有 IPv4 时才选择 IPv6。
func resolveConnectAddress(g *model.DeviceGroup, n *model.Node) (string, int) {
	cfg := struct {
		ConnectType    string   `json:"connect_type"`
		ConnectAddress string   `json:"connect_address"`
		ConnectPort    int      `json:"connect_port"`
		Protocol       string   `json:"protocol"`
		IPv6Group      []uint64 `json:"ipv6_group"`
	}{}
	if g != nil {
		_ = jsonUnmarshal(string(g.Config), &cfg)
	}
	ports := nodePorts(n)
	port := ports.WsPort
	switch cfg.Protocol {
	case "direct":
		port = ports.DirectPort
	case "tls":
		port = ports.TlsPort
	}
	if cfg.ConnectPort <= 0 {
		cfg.ConnectPort = port
	}
	host := n.ConnectHost
	if host == "" && n.IsStatic {
		host = n.PrivateIP
	}
	address, selectedPort, _ := app.ResolveConnectAddress(app.ConnectAddressInput{
		ConnectType: cfg.ConnectType, ConnectAddress: cfg.ConnectAddress, ConnectPort: cfg.ConnectPort,
		PublicIPv4: n.PublicIPv4, PublicIPv6: n.PublicIPv6, ConnectHost: host, NodePort: port, IPv6Group: cfg.IPv6Group, NodeID: n.ID,
	})
	if address == "" && cfg.ConnectType != "dyn_ip4" {
		address = n.PublicIPv6
	}
	return address, selectedPort
}

// portForProtocol 按协议返回节点对应的监听端口。
func portForProtocol(n *model.Node, protocol string) int {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "direct":
		return n.DirectPort
	case "tls":
		return n.TlsPort
	case "http", "ws":
		return n.WsPort
	default:
		if n.WsPort > 0 {
			return n.WsPort
		}
		return n.TlsPort
	}
}

// ruleProtocol 解析规则使用的隧道协议。
//
// 单端（无出口组）时固定为 direct（入口直出，规格书 6.5）；
// 否则取入口组配置里的 protocol 字段，缺省为 tls。
func ruleProtocol(r *model.ForwardRule, inGroup *model.DeviceGroup) string {
	if r.OutboundGroupID == 0 && len(r.ChainGroupList()) == 0 && !r.ReverseEnable {
		return "direct"
	}
	if inGroup == nil {
		return "tls"
	}
	var cfg struct {
		Protocol string `json:"protocol"`
	}
	if len(inGroup.Config) > 0 {
		_ = jsonUnmarshal(string(inGroup.Config), &cfg)
	}
	switch strings.ToLower(strings.TrimSpace(cfg.Protocol)) {
	case "ws", "http", "tls", "direct":
		return strings.ToLower(strings.TrimSpace(cfg.Protocol))
	}
	return "tls"
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

// ConfigHandler 处理节点拉取配置（GET /api/node/config?version=，规格书 8.16）。
//
// 认证用 X-Node-Token。行为：
//   - 节点上报的 version 等于面板当前版本时返回 304 语义（full 为 false 且 rules 为空），
//     避免每次轮询都传输全量配置；
//   - 否则返回全量配置。
func (r *Registry) ConfigHandler(c *gin.Context) {
	node, err := r.authenticate(c)
	if err != nil {
		fail(c, err)
		return
	}
	ctx := c.Request.Context()

	since, _ := strconv.ParseInt(c.Query("version"), 10, 64)
	current := r.app.ConfigVersion()

	if since == current && since > 0 {
		c.JSON(http.StatusOK, ConfigResponse{
			ConfigVersion:     current,
			Full:              false,
			Rules:             []ConfigRule{},
			RemovedRuleIDs:    []uint64{},
			DeviceGroupConfig: map[string]DeviceGroupConfig{},
			HeartbeatInterval: r.heartbeatInterval(),
			GeneratedAt:       time.Now().UTC().Unix(),
		})
		return
	}

	resp, err := r.cfg.BuildFull(ctx, node)
	if err != nil {
		fail(c, err)
		return
	}
	resp.Full = true
	c.JSON(http.StatusOK, resp)
}

// PushConfigHandler 处理面板主动推送配置（POST /api/node/config 的即时通道）。
//
// 规格书 5.4 的「立即推送（尽力而为）」：节点长连在线时走 WebSocket，
// 不在线时本接口不返回错误，只回报 delivered = false，由轮询兜底。
func (r *Registry) PushConfigHandler(c *gin.Context) {
	node, err := r.authenticate(c)
	if err != nil {
		fail(c, err)
		return
	}
	ctx := c.Request.Context()

	resp, err := r.cfg.BuildFull(ctx, node)
	if err != nil {
		fail(c, err)
		return
	}
	delivered := r.app.Hub().BroadcastToNode(node.ID, app.Event{
		Type: "config_changed",
		Data: map[string]interface{}{
			"config_version": resp.ConfigVersion,
			"full":           true,
		},
	})
	r.app.Log.Debug("配置推送结果",
		zap.Uint64("node_id", node.ID),
		zap.Int64("config_version", resp.ConfigVersion),
		zap.Bool("delivered", delivered))

	c.JSON(http.StatusOK, gin.H{
		"delivered":      delivered,
		"config_version": resp.ConfigVersion,
		"config":         resp,
	})
}

// ---------------------------------------------------------------------------
// 上报
// ---------------------------------------------------------------------------

// ReportHandler 处理节点上报同步结果与流量（POST /api/node/report，规格书 8.16）。
//
// 行为：
//   - 逐条更新规则的 sync_status / sync_error / synced_at；
//   - 更新节点的连接数与实时速率；
//   - 追加规则维度的累计流量。
//
// 返回接受的条数与面板时间。
func (r *Registry) ReportHandler(c *gin.Context) {
	node, err := r.authenticate(c)
	if err != nil {
		fail(c, err)
		return
	}

	var req ReportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, response.Field(response.CodeParamInvalid, "body", nil, "请求体不是合法 JSON"))
		return
	}
	ctx := c.Request.Context()
	now := time.Now().UTC()

	if req.NodeID != 0 && req.NodeID != node.ID {
		fail(c, response.New(response.CodeForbidden, "节点身份不匹配"))
		return
	}
	if err := r.app.Traffic.RecordNodeReport(ctx, node.ID, req); err != nil {
		fail(c, response.Wrap(response.CodeDBUnwritable, err, "流量报告未提交"))
		return
	}
	combined := map[uint64]RuleSyncResult{}
	for _, result := range req.Results {
		if result.RuleID == 0 {
			continue
		}
		if result.Status != "normal" && result.Status != "failed" {
			fail(c, response.New(response.CodeParamInvalid, "无效的规则同步状态"))
			return
		}
		previous, exists := combined[result.RuleID]
		if !exists || previous.Status != "failed" {
			combined[result.RuleID] = result
		}
	}
	acceptedSuccess := false
	for id, result := range combined {
		committed, err := r.app.Sync.RecordNodeResultAccepted(ctx, id, node.ID, req.ConfigVersion, result.Status == "normal", result.Error)
		if err != nil {
			fail(c, response.Wrap(response.CodeInternal, err, "保存节点同步结果失败"))
			return
		}
		acceptedSuccess = acceptedSuccess || (committed && result.Status == model.SyncNormal)
	}
	accepted := len(req.Results)

	// 节点级统计与错误摘要。
	nodeUpdates := map[string]interface{}{
		"last_seen":  now,
		"online":     true,
		"updated_at": now,
	}
	if req.Stats != nil {
		nodeUpdates["current_conn"] = req.Stats.CurrentConn

	}
	if req.Error != "" {
		nodeUpdates["last_error"] = util.Truncate(req.Error, 512)
	} else if req.ConfigVersion == r.app.ConfigVersion() && acceptedSuccess {
		// Historical traffic batches and ignored ACKs say nothing about whether
		// the node has recovered. Clear errors only after an accepted success for
		// the current configuration, with no current rule failure remaining.
		var states []model.NodeRuleSync
		if err := r.app.DB.WithContext(ctx).Select("rule_id, status").Where("node_id = ? AND config_version = ?", node.ID, req.ConfigVersion).Find(&states).Error; err != nil {
			fail(c, response.Wrap(response.CodeInternal, err, "查询节点同步结果失败"))
			return
		}
		currentFailure := false
		for _, state := range states {
			if state.Status == model.SyncFailed {
				currentFailure = true
			}
		}
		if !currentFailure {
			nodeUpdates["last_error"] = ""
		}
	}
	if err := r.app.DB.WithContext(ctx).Model(&model.Node{}).Where("id = ?", node.ID).
		Updates(nodeUpdates).Error; err != nil {
		fail(c, response.Wrap(response.CodeInternal, err, "更新节点上报信息失败"))
		return
	}

	c.JSON(http.StatusOK, ReportResponse{BatchID: req.BatchID, Accepted: accepted, ServerTime: now.Unix()})
}

// ---------------------------------------------------------------------------
// 任务
// ---------------------------------------------------------------------------

// TasksHandler 处理节点拉取待执行任务（GET /api/node/tasks，规格书 8.16）。
//
// 认证用 X-Node-Token；返回 pending / running 状态的任务，最多 50 条。
func (r *Registry) TasksHandler(c *gin.Context) {
	node, err := r.authenticate(c)
	if err != nil {
		fail(c, err)
		return
	}
	tasks, err := r.pendingTasks(c.Request.Context(), node.ID)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, TasksResponse{
		Tasks:      tasks,
		ServerTime: time.Now().UTC().Unix(),
	})
}

// TaskResultHandler 处理节点上报任务结果（POST /api/node/task-result，规格书 8.16）。
//
// 行为：更新 NodeTask 的 status / result；升级任务成功后顺带刷新节点版本号。
func (r *Registry) TaskResultHandler(c *gin.Context) {
	node, err := r.authenticate(c)
	if err != nil {
		fail(c, err)
		return
	}

	var req TaskResultRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, response.Field(response.CodeParamInvalid, "body", nil, "请求体不是合法 JSON"))
		return
	}
	if req.TaskID == 0 {
		fail(c, response.Field(response.CodeParamInvalid, "task_id", req.TaskID, "任务 ID 不能为空"))
		return
	}

	ctx := c.Request.Context()
	// 先读出任务类型，供后续的副作用处理（如升级成功后刷新版本号）。
	var task model.NodeTask
	if err := r.app.DB.WithContext(ctx).
		Where("id = ? AND node_id = ?", req.TaskID, node.ID).First(&task).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			fail(c, response.Field(response.CodeNotFound, "task_id", req.TaskID, "任务不存在或不属于该节点"))
			return
		}
		fail(c, response.Wrap(response.CodeInternal, err, "查询任务失败"))
		return
	}

	if err := r.app.Node.ApplyTaskResult(ctx, node.ID, app.TaskResultInput{
		TaskID: req.TaskID,
		Status: req.Status,
		Result: req.Result,
	}); err != nil {
		fail(c, err)
		return
	}

	// 升级任务成功后清空节点上次的错误信息；失败则写入错误原因。
	if task.Type == model.TaskUpgrade {
		updates := map[string]interface{}{"updated_at": time.Now().UTC()}
		if strings.EqualFold(req.Status, model.TaskFailed) {
			updates["last_error"] = util.Truncate("升级失败: "+req.Result, 512)
		} else {
			updates["last_error"] = ""
		}
		if err := r.app.DB.WithContext(ctx).Model(&model.Node{}).Where("id = ?", node.ID).
			Updates(updates).Error; err != nil {
			r.app.Log.Warn("更新节点升级结果失败",
				zap.Uint64("node_id", node.ID), zap.Error(err))
		}
	}

	r.app.Log.Info("节点任务已完成",
		zap.Uint64("node_id", node.ID),
		zap.Uint64("task_id", req.TaskID),
		zap.String("type", task.Type),
		zap.String("status", req.Status))

	c.JSON(http.StatusOK, TaskResultResponse{OK: true, ServerTime: time.Now().UTC().Unix()})
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

// containsID 判断 ID 是否在切片中。
func containsID(ids []uint64, target uint64) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}

// normalizeWeight 把权重归一化为至少 1，避免 0 权重导致节点被静默剔除。
func normalizeWeight(w int) int {
	if w <= 0 {
		return 1
	}
	return w
}

// defaultString 返回首个非空字符串。
func defaultString(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// DescribeRule 返回规则的简短描述，供日志与错误信息使用。
func DescribeRule(r *ConfigRule) string {
	if r == nil {
		return ""
	}
	return fmt.Sprintf("#%d %s(%s:%d)", r.RuleID, r.Name, r.Protocol, r.ListenPort)
}
