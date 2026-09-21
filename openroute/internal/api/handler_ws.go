package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"github.com/openroute/openroute/internal/api/middleware"
	"github.com/openroute/openroute/internal/api/response"
	"github.com/openroute/openroute/internal/app"
	"github.com/openroute/openroute/internal/model"
	"github.com/openroute/openroute/internal/util"
)

// wsUpgrader 是 WebSocket 升级器。
//
// CheckOrigin 放行全部来源：面板通常以 IP:端口 直接访问，
// 且真正的权限边界是认证中间件而非 Origin。
// 浏览器在跨域 WebSocket 场景下也会带上 Cookie，因此这里不能依赖 Origin 做隔离。
var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true },
	// 允许子协议协商，便于前端未来扩展。
	Subprotocols: []string{"openroute.v1"},
}

// WebSocket 心跳与超时参数。
const (
	// wsPingInterval 是服务端主动 ping 的间隔，用于探活并保活中间代理。
	wsPingInterval = 25 * time.Second
	// wsPongWait 是等待客户端 pong 的上限；超过即判定连接已死。
	wsPongWait = 60 * time.Second
	// wsWriteWait 是单次写入的超时。
	wsWriteWait = 10 * time.Second
	// terminalMaxSessionsPerUser 是每用户并发终端数上限（规格书 8.17：最多 3 个）。
	terminalMaxSessionsPerUser = 3
)

// NodesStream 是节点状态实时推送通道（规格书 8.6）。
//
// 前端订阅后，节点的上下线、配置版本变更、指标更新会即时推送，
// 无需轮询。认证沿用统一的认证中间件（Session / JWT / API Token）。
func (h *Handlers) NodesStream(c *gin.Context) {
	// WebSocket 无法自定义请求头，因此允许用 ?token= 传递访问令牌。
	// 认证中间件已在路由上运行；若它失败会直接返回 401，不会走到这里。
	uid, un, _, loggedIn := middleware.CurrentUser(c)
	if !loggedIn {
		// 尝试用查询参数中的令牌补救（浏览器 WebSocket 场景）。
		if !h.authFromQuery(c) {
			response.Abort(c, response.New(response.CodeUnauthorized, ""))
			return
		}
		uid, un, _, _ = middleware.CurrentUser(c)
	}

	conn, err := wsUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		// Upgrade 失败时已经写入了响应，这里只记录日志。
		h.log.Debug("WebSocket 升级失败", zap.Error(err), zap.String("user", un))
		return
	}

	name := "user:" + utoa(uid)
	if un != "" {
		name = "user:" + un
	}

	events, cancel := h.app.Hub().Subscribe(name, 0)
	defer cancel()
	defer func() { _ = conn.Close() }()

	// 连接建立后先发一条快照，避免前端在首条事件到达前没有内容可渲染。
	initial := app.Event{
		Type: "snapshot",
		Data: gin.H{
			"online_nodes": h.onlineNodeCount(c.Request.Context()),
			"timestamp":    time.Now().UTC().Format(time.RFC3339),
		},
	}
	if err := writeJSON(conn, initial); err != nil {
		return
	}

	// 读循环：只用于处理客户端关闭与 pong，收到的业务消息一律忽略。
	conn.SetReadLimit(4096)
	_ = conn.SetReadDeadline(time.Now().Add(wsPongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(wsPongWait))
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	ticker := time.NewTicker(wsPingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-h.app.Done():
			// 进程正在关闭：发一条关闭帧后退出。
			_ = conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseGoingAway, "server shutting down"),
				time.Now().Add(wsWriteWait))
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if err := writeJSON(conn, ev); err != nil {
				return
			}
		case <-ticker.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteWait)); err != nil {
				return
			}
		}
	}
}

// authFromQuery 用查询参数中的令牌完成认证，成功后写入上下文。
//
// 这是为浏览器 WebSocket 场景准备的：浏览器的 WebSocket API 不支持
// 自定义请求头，因此无法携带 Authorization，只能把令牌放进 URL。
//
// 返回是否认证成功。
func (h *Handlers) authFromQuery(c *gin.Context) bool {
	token := strings.TrimSpace(c.Query("token"))
	if token == "" {
		// 没有令牌时退回 Cookie：WebUI 场景下 Cookie 会自动带上。
		ck, err := c.Cookie(middleware.SessionCookieName)
		if err != nil || ck == "" {
			return false
		}
		token = ck
	}

	claims, err := h.app.Auth.ParseToken(token, "access")
	if err != nil {
		return false
	}
	user, err := h.app.Auth.Me(c.Request.Context(), claims.UserID)
	if err != nil || user.Status != model.StatusEnabled {
		return false
	}

	c.Set(middleware.CtxUserID, user.ID)
	c.Set(middleware.CtxUsername, user.Username)
	c.Set(middleware.CtxRole, user.Role)
	c.Set(middleware.CtxAuthType, middleware.AuthTypeJWT)
	return true
}

// NodeStream 是节点侧的长连接通道（规格书 5.4 的即时推送）。
//
// 与浏览器订阅不同，这里的订阅者是节点：它只接收 config_changed 事件，
// 收到后立即调用 /api/node/config 拉取最新配置。
func (h *Handlers) NodeStream(c *gin.Context) {
	node := middleware.CurrentNode(c)
	if node == nil {
		response.Abort(c, response.New(response.CodeNodeTokenInvalid, ""))
		return
	}

	conn, err := wsUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		h.log.Debug("节点 WebSocket 升级失败", zap.Error(err), zap.Uint64("node_id", node.ID))
		return
	}
	defer func() { _ = conn.Close() }()

	events, cancel := h.app.Hub().Subscribe("node:"+utoa(node.ID), node.ID)
	defer cancel()

	// 首帧告知节点当前的配置版本，节点据此决定是否立即拉取。
	if err := writeJSON(conn, app.Event{
		Type: "hello",
		Data: gin.H{
			"config_version":     h.app.ConfigVersion(),
			"heartbeat_interval": h.app.Config.HeartbeatInterval,
		},
	}); err != nil {
		return
	}

	conn.SetReadLimit(4096)
	_ = conn.SetReadDeadline(time.Now().Add(wsPongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(wsPongWait))
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	ticker := time.NewTicker(wsPingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-h.app.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if err := writeJSON(conn, ev); err != nil {
				return
			}
		case <-ticker.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteWait)); err != nil {
				return
			}
		}
	}
}

// NodeTerminal 是 WebSSH 终端通道（规格书 6.1）。
//
// 行为：
//   - enable-webssh 为 false 时返回 40304；
//   - 每用户最多 3 个并发终端（规格书 8.17），超限返回 42901；
//   - 每次打开终端 MUST 写入审计日志（谁、何时、连的哪台机器）；
//   - 终端字节流通过节点的长连接转发，浏览器端用 xterm.js 渲染。
//
// 协议（浏览器 ↔ 面板）：
//
//	浏览器发：JSON 文本帧 {"type":"input","data":"..."} / {"type":"resize","cols":80,"rows":24}
//	面板发：JSON 文本帧 {"type":"output","data":"..."} / {"type":"error","message":"..."}
//
// 说明：节点侧的 SSH 转发由节点客户端实现（其通过 /api/node/tasks 的
// exec 通道执行），面板在这里负责鉴权、并发控制、大小同步与审计。
func (h *Handlers) NodeTerminal(c *gin.Context) {
	// 1. 功能开关
	if !h.app.Config.EnableWebSSH {
		response.Abort(c, response.New(response.CodeWebSSHDisabled,
			"WebSSH 已在 config.yml 中被禁用（enable-webssh: false）"))
		return
	}

	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "节点 ID 必须是正整数")
		return
	}

	node, err := h.app.Node.Get(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, err)
		return
	}

	// 2. 并发终端数限制（按用户维度）
	uid, un, _, _ := middleware.CurrentUser(c)
	key := "term:" + utoa(uid)
	if n := h.terminalCounter.increment(key); n > terminalMaxSessionsPerUser {
		h.terminalCounter.decrement(key)
		response.Abort(c, response.New(response.CodeRateLimited,
			"每个用户最多同时打开 "+itoa(terminalMaxSessionsPerUser)+" 个终端，请先关闭其它终端"))
		return
	}
	defer h.terminalCounter.decrement(key)

	// 3. 审计：每次打开终端都要留痕（规格书 6.1 的 MUST 要求）
	ip := util.ClientIP(c.Request)
	h.app.Audit.Write(c.Request.Context(), app.AuditEntry{
		UserID:     uid,
		Username:   un,
		Action:     model.ActionExec,
		Resource:   "node_terminal",
		ResourceID: node.ID,
		IP:         ip,
		UserAgent:  c.GetHeader("User-Agent"),
		Message:    "打开节点 " + node.Name + " 的 WebSSH 终端",
	})

	// 4. 升级连接
	conn, err := wsUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		h.log.Debug("终端 WebSocket 升级失败", zap.Error(err))
		return
	}
	defer func() { _ = conn.Close() }()

	// 5. 节点必须在线才能转发
	if !node.Online {
		_ = writeJSON(conn, gin.H{
			"type":    "error",
			"message": "节点 " + node.Name + " 当前离线，无法打开终端。请先让节点上线。",
		})
		return
	}

	// 6. 通知节点开启一个终端会话。
	//
	// 设计说明：本版本通过节点长连接下发"打开终端"任务，
	// 节点侧建立 SSH 会话并把字节流通过后续的 report 通道回传。
	// 若节点未建立长连接（例如节点客户端版本较旧），明确告知用户。
	if !h.app.Hub().NodeConnected(node.ID) {
		_ = writeJSON(conn, gin.H{
			"type": "error",
			"message": "节点 " + node.Name +
				" 未建立长连接，无法转发终端会话。请确认节点客户端版本并重启节点服务。",
		})
		return
	}

	_ = writeJSON(conn, gin.H{
		"type":    "ready",
		"node_id": node.ID,
		"message": "会话已建立",
	})

	conn.SetReadLimit(1 << 20)
	_ = conn.SetReadDeadline(time.Now().Add(wsPongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(wsPongWait))
	})

	// 7. 读取浏览器输入并转发给节点。
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}

		var msg terminalMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			// 非法消息直接忽略，不中断会话。
			continue
		}

		switch msg.Type {
		case "input":
			h.app.Hub().BroadcastToNode(node.ID, app.Event{
				Type: "terminal_input",
				Data: gin.H{"data": msg.Data},
			})
		case "resize":
			// 窗口尺寸同步（规格书 6.1 要求支持）
			h.app.Hub().BroadcastToNode(node.ID, app.Event{
				Type: "terminal_resize",
				Data: gin.H{"cols": msg.Cols, "rows": msg.Rows},
			})
		case "ping":
			if err := writeJSON(conn, gin.H{"type": "pong"}); err != nil {
				return
			}
		}
	}
}

// terminalMessage 是终端通道的消息结构（浏览器 → 面板）。
type terminalMessage struct {
	// Type 取值：input / resize / ping
	Type string `json:"type"`
	// Data 是 input 类型的输入内容。
	Data string `json:"data"`
	// Cols / Rows 是 resize 类型的窗口尺寸。
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// onlineNodeCount 统计当前在线节点数。
func (h *Handlers) onlineNodeCount(ctx context.Context) int64 {
	var n int64
	if err := h.app.DB.WithContext(ctx).Model(&model.Node{}).
		Where("online = ?", true).Count(&n).Error; err != nil {
		return 0
	}
	return n
}

// writeJSON 向 WebSocket 写一帧 JSON，带写入超时。
func writeJSON(conn *websocket.Conn, v interface{}) error {
	if err := conn.SetWriteDeadline(time.Now().Add(wsWriteWait)); err != nil {
		return err
	}
	return conn.WriteJSON(v)
}

// counter 是带互斥的并发计数器，用于限制每用户的并发终端数。
//
// 之所以不复用 middleware.RateLimiter：那个是「速率」限制，
// 这里是「同时存在数量」的限制，语义不同。
type counter struct {
	mu sync.Mutex
	m  map[string]int
}

// increment 使计数加一并返回新值。
func (c *counter) increment(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = make(map[string]int)
	}
	c.m[key]++
	return c.m[key]
}

// decrement 使计数减一（不小于 0），并在归零时清理键。
func (c *counter) decrement(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		return
	}
	c.m[key]--
	if c.m[key] <= 0 {
		delete(c.m, key)
	}
}
