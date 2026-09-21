package api

import (
	"context"
	"net/http"
	"net/url"
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
	"github.com/openroute/openroute/internal/nodestream"
	"github.com/openroute/openroute/internal/util"
)

// wsUpgrader 是 WebSocket 升级器。
//
// Browsers must connect from the panel origin because cookies authenticate the
// socket. Node clients omit Origin and authenticate with their node token.
var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		u, err := url.Parse(origin)
		return err == nil && (u.Scheme == "http" || u.Scheme == "https") && strings.EqualFold(u.Host, r.Host)
	},
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
		return
	}
	defer conn.Close()
	lease := h.terminalBroker.attach(node.ID, node.DisableExecute)
	defer h.terminalBroker.detach(node.ID, lease)
	events, cancel := h.app.Hub().Subscribe("node:"+utoa(node.ID), node.ID)
	defer cancel()
	if writeJSON(conn, app.Event{Type: "hello", Data: gin.H{"config_version": h.app.ConfigVersion(), "heartbeat_interval": h.app.Config.HeartbeatInterval}}) != nil {
		return
	}
	conn.SetReadLimit(128 << 10)
	_ = conn.SetReadDeadline(time.Now().Add(wsPongWait))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(wsPongWait)) })
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			var message nodestream.Message
			if conn.ReadJSON(&message) != nil {
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(wsPongWait))
			if message.Type == "node_hello" {
				h.terminalBroker.setDisabled(node.ID, lease, message.Data.DisableExecute)
				continue
			}
			// Authentication fixes the node identity; session IDs cannot route output to another node.
			h.terminalBroker.receive(node.ID, lease, message)
		}
	}()
	ticker := time.NewTicker(wsPingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-lease.done:
			return
		case <-h.app.Done():
			return
		case ev, ok := <-events:
			if !ok || writeJSON(conn, ev) != nil {
				return
			}
		case <-ticker.C:
			// Token rotation/deletion and disabling execution take effect on existing streams too.
			var current model.Node
			if h.app.DB.WithContext(c.Request.Context()).First(&current, node.ID).Error != nil || current.Token != node.Token {
				return
			}
			h.terminalBroker.setDisabled(node.ID, lease, current.DisableExecute)
			if conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteWait)) != nil {
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
// The node opens a PTY and sends output on its authenticated control connection.
func (h *Handlers) NodeTerminal(c *gin.Context) {
	if !middleware.IsAdmin(c) {
		response.Abort(c, response.New(response.CodeForbidden, "Only administrators may open node terminals"))
		return
	}
	if !h.app.Config.EnableWebSSH {
		response.Abort(c, response.New(response.CodeWebSSHDisabled, "WebSSH is disabled"))
		return
	}
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "invalid node ID")
		return
	}
	node, err := h.app.Node.Get(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if node.DisableExecute {
		response.Abort(c, response.New(response.CodeWebSSHDisabled, "DISABLE_EXECUTE=1"))
		return
	}
	sid, output, ok := h.terminalBroker.open(id)
	if !ok {
		response.Abort(c, response.New(response.CodeNodeOffline, "Node has no active control connection or execution is disabled"))
		return
	}
	defer h.terminalBroker.close(sid)
	uid, un, _, _ := middleware.CurrentUser(c)
	key := "term:" + utoa(uid)
	if h.terminalCounter.increment(key) > terminalMaxSessionsPerUser {
		h.terminalCounter.decrement(key)
		response.Abort(c, response.New(response.CodeRateLimited, "At most three terminals per user"))
		return
	}
	defer h.terminalCounter.decrement(key)
	conn, err := wsUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	sendNode := func(kind string, payload nodestream.Payload) bool {
		payload.SessionID = sid
		return h.app.Hub().BroadcastToNode(id, app.Event{Type: kind, Data: payload})
	}
	if !sendNode("terminal_open", nodestream.Payload{Cols: 80, Rows: 24}) {
		_ = writeJSON(conn, gin.H{"type": "error", "message": "Node disconnected"})
		return
	}
	defer sendNode("terminal_close", nodestream.Payload{})
	h.app.Audit.Write(c.Request.Context(), app.AuditEntry{UserID: uid, Username: un, Action: model.ActionExec, Resource: "node_terminal", ResourceID: id, IP: util.ClientIP(c.Request), UserAgent: c.GetHeader("User-Agent"), Message: "Open node terminal " + node.Name})
	conn.SetReadLimit(64 << 10)
	_ = conn.SetReadDeadline(time.Now().Add(wsPongWait))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(wsPongWait)) })
	input := make(chan terminalMessage, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			var msg terminalMessage
			if conn.ReadJSON(&msg) != nil {
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(wsPongWait))
			select {
			case input <- msg:
			case <-c.Request.Context().Done():
				return
			case <-h.app.Done():
				return
			default:
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
		case <-c.Request.Context().Done():
			return
		case msg, ok := <-output:
			if !ok {
				_ = writeJSON(conn, gin.H{"type": "closed", "message": "Node control connection closed"})
				return
			}
			kind := strings.TrimPrefix(msg.Type, "terminal_")
			if writeJSON(conn, gin.H{"type": kind, "data": msg.Data.Data, "message": msg.Data.Message, "node_id": id}) != nil {
				return
			}
			if kind == "error" || kind == "closed" {
				return
			}
		case msg := <-input:
			switch msg.Type {
			case "input":
				if !sendNode("terminal_input", nodestream.Payload{Data: msg.Data}) {
					return
				}
			case "resize":
				if msg.Cols > 0 && msg.Cols <= 500 && msg.Rows > 0 && msg.Rows <= 300 {
					if !sendNode("terminal_resize", nodestream.Payload{Cols: msg.Cols, Rows: msg.Rows}) {
						return
					}
				}
			case "ping":
				if writeJSON(conn, gin.H{"type": "pong"}) != nil {
					return
				}
			}
		case <-ticker.C:
			if conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteWait)) != nil {
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
