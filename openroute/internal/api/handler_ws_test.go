package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/openroute/openroute/internal/api/middleware"
	"github.com/openroute/openroute/internal/app"
	"github.com/openroute/openroute/internal/config"
	"github.com/openroute/openroute/internal/database"
	"github.com/openroute/openroute/internal/model"
	"github.com/openroute/openroute/internal/nodestream"
	"go.uber.org/zap"
)

func TestWebTerminalBridgesAuthenticatedNodeSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.Open(database.Options{Path: "sqlite3://" + filepath.Join(t.TempDir(), "ws.db"), MaxOpen: 1, MaxIdle: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.DB.AutoMigrate(&model.Node{}, &model.AuditLog{}); err != nil {
		t.Fatal(err)
	}
	node := model.Node{ID: 1, Name: "test", Token: "node-secret", Online: true}
	if err := db.Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.EnableWebSSH = true
	a := app.New(cfg, db, zap.NewNop())
	defer a.Hub().Close()
	a.Node = app.NewNodeService(a)
	a.Audit = app.NewAuditService(a)
	h := &Handlers{app: a, log: zap.NewNop()}
	r := gin.New()
	r.GET("/api/node/stream", middleware.NodeAuthenticate(middleware.AuthDeps{DB: db.DB}), h.NodeStream)
	r.GET("/terminal/:id", func(c *gin.Context) {
		c.Set(middleware.CtxUserID, uint64(1))
		c.Set(middleware.CtxUsername, "test-admin")
		c.Set(middleware.CtxRole, c.Query("role"))
		c.Next()
	}, h.NodeTerminal)
	server := httptest.NewServer(r)
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	if conn, response, err := websocket.DefaultDialer.Dial(wsURL+"/api/node/stream", nil); err == nil {
		conn.Close()
		t.Fatal("unauthenticated node accepted")
	} else if response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("node authentication status: %v", response)
	}
	nodeConn, _, err := websocket.DefaultDialer.Dial(wsURL+"/api/node/stream", http.Header{"X-Node-Token": []string{"node-secret"}})
	if err != nil {
		t.Fatal(err)
	}
	defer nodeConn.Close()
	nodeConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var message nodestream.Message
	if err := nodeConn.ReadJSON(&message); err != nil || message.Type != "hello" {
		t.Fatalf("node handshake: %+v %v", message, err)
	}
	nodeConn.WriteJSON(nodestream.Message{Type: "node_hello"})
	if conn, response, err := websocket.DefaultDialer.Dial(wsURL+"/terminal/1?role=user", nil); err == nil {
		conn.Close()
		t.Fatal("ordinary user terminal accepted")
	} else if response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("terminal permission status: %v", response)
	}
	if conn, _, err := websocket.DefaultDialer.Dial(wsURL+"/terminal/1?role=admin", http.Header{"Origin": []string{"https://another.example"}}); err == nil {
		conn.Close()
		t.Fatal("cross-origin terminal accepted")
	}
	browser, _, err := websocket.DefaultDialer.Dial(wsURL+"/terminal/1?role=admin", http.Header{"Origin": []string{server.URL}})
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	browser.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := nodeConn.ReadJSON(&message); err != nil || message.Type != "terminal_open" || len(message.Data.SessionID) != 32 {
		t.Fatalf("missing terminal open: %+v %v", message, err)
	}
	sid := message.Data.SessionID
	nodeConn.WriteJSON(nodestream.Message{Type: "terminal_ready", Data: nodestream.Payload{SessionID: sid}})
	var frame map[string]interface{}
	if err := browser.ReadJSON(&frame); err != nil || frame["type"] != "ready" {
		t.Fatalf("browser ready: %+v %v", frame, err)
	}
	browser.WriteJSON(terminalMessage{Type: "input", Data: "echo connected\n"})
	if err := nodeConn.ReadJSON(&message); err != nil || message.Type != "terminal_input" || message.Data.SessionID != sid || message.Data.Data != "echo connected\n" {
		t.Fatalf("input bridge: %+v %v", message, err)
	}
	nodeConn.WriteJSON(nodestream.Message{Type: "terminal_output", Data: nodestream.Payload{SessionID: sid, Data: "connected\r\n"}})
	if err := browser.ReadJSON(&frame); err != nil || frame["type"] != "output" || frame["data"] != "connected\r\n" {
		t.Fatalf("output bridge: %+v %v", frame, err)
	}
	browser.WriteJSON(terminalMessage{Type: "resize", Cols: 123, Rows: 45})
	if err := nodeConn.ReadJSON(&message); err != nil || message.Type != "terminal_resize" || message.Data.Cols != 123 || message.Data.Rows != 45 {
		t.Fatalf("resize bridge: %+v %v", message, err)
	}
	nodeConn.Close()
	if err := browser.ReadJSON(&frame); err != nil || frame["type"] != "closed" {
		t.Fatalf("node disconnect did not close browser: %+v %v", frame, err)
	}
	var count int64
	if err := db.Model(&model.AuditLog{}).Where("resource = ?", "node_terminal").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("terminal audit count=%d err=%v", count, err)
	}
}
