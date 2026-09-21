package agent_test

import (
	"context"
	"io"
	"log"
	"net"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/openroute/openroute/internal/agent"
	"github.com/openroute/openroute/internal/app"
	"github.com/openroute/openroute/internal/config"
	"github.com/openroute/openroute/internal/database"
	"github.com/openroute/openroute/internal/model"
	"github.com/openroute/openroute/internal/nodeclient"
	"go.uber.org/zap"
)

// Exercise the real panel compiler, registry, two node clients, TLS tunnel,
// report outboxes, database accounting and live policy changes together.
func TestPanelTwoNodeTunnelTrafficAndDisable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.Open(database.Options{Path: "sqlite3://" + filepath.Join(t.TempDir(), "panel.db"), MaxOpen: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.AutoMigrate(); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.SecretKey = "integration-panel-key"
	cfg.HeartbeatInterval = 1
	a := app.New(cfg, db, zap.NewNop()).Init()
	t.Cleanup(a.Stop)
	freePort := func() int {
		t.Helper()
		ln, e := net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		_ = ln.Close()
		return port
	}
	nodes := []model.Node{{ID: 1, Name: "integration-in", Token: "integration-ingress-token"}, {ID: 2, Name: "integration-out", Token: "integration-egress-token"}}
	for i := range nodes {
		n := &nodes[i]
		n.ConnectHost = "127.0.0.1"
		n.IsStatic = true
		n.DirectPort = freePort()
		n.WsPort = freePort()
		n.TlsPort = freePort()
		n.UdpPort = freePort()
		n.RevPort = freePort()
		if err := db.Create(n).Error; err != nil {
			t.Fatal(err)
		}
	}
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = echo.Close() })
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	port := freePort()
	for _, value := range []interface{}{
		&model.User{ID: 1, Username: "owner", PasswordHash: "unused", Token: "owner-token", TrafficLimit: 1 << 30},
		&model.DeviceGroup{ID: 1, Name: "incoming", Type: "inbound", NodeIDs: model.FromAny([]uint64{1}), Config: model.FromAny(map[string]interface{}{"protocol": "tls"})},
		&model.DeviceGroup{ID: 2, Name: "outgoing", Type: "outbound", NodeIDs: model.FromAny([]uint64{2})},
		&model.ForwardRule{ID: 1, Name: "tls-route", UserID: 1, Enable: true, InboundGroupID: 1, OutboundGroupID: 2, ListenPort: port, Targets: model.FromAny([]model.Target{{Host: "127.0.0.1", Port: echo.Addr().(*net.TCPAddr).Port}}), InboundMultiplier: 2, OutboundMultiplier: 3},
	} {
		if err := db.Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
	a.BumpConfigVersion("integration fixture")
	registry := agent.NewRegistry(a)
	router := gin.New()
	router.POST("/api/node/register", registry.RegisterHandler)
	router.POST("/api/node/heartbeat", registry.HeartbeatHandler)
	router.GET("/api/node/config", registry.ConfigHandler)
	router.POST("/api/node/report", registry.ReportHandler)
	router.GET("/api/node/tasks", registry.TasksHandler)
	router.POST("/api/node/task-result", registry.TaskResultHandler)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 2)
	t.Cleanup(func() {
		cancel()
		for i := 0; i < 2; i++ {
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("node did not stop")
			}
		}
	})
	for _, n := range nodes {
		client, err := nodeclient.NewClient(nodeclient.Config{BaseURL: server.URL, Token: n.Token, DataDir: t.TempDir(), BindInbound: "127.0.0.1", DisableExecute: true}, "integration-test", log.New(io.Discard, "", 0))
		if err != nil {
			t.Fatal(err)
		}
		go func() { done <- client.Run(ctx) }()
	}
	wait := func(label string, condition func() bool) {
		t.Helper()
		deadline := time.Now().Add(12 * time.Second)
		for time.Now().Before(deadline) {
			if condition() {
				return
			}
			time.Sleep(40 * time.Millisecond)
		}
		t.Fatalf("timeout: %s", label)
	}
	bothApplied := func() bool {
		var count int64
		err := db.Model(&model.NodeRuleSync{}).Where("rule_id = ? AND config_version = ? AND status = ?", 1, a.ConfigVersion(), model.SyncNormal).Count(&count).Error
		return err == nil && count == 2
	}
	wait("both nodes applied the compiled TLS rule", bothApplied)
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	client, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	payload := []byte("cross-node-billing-payload")
	_ = client.SetDeadline(time.Now().Add(4 * time.Second))
	if _, err := client.Write(payload); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatal("tunnel changed payload")
	}
	want := int64(len(payload) * 2)
	wait("both directions billed exactly once", func() bool {
		var r model.ForwardRule
		db.First(&r, 1)
		return r.TrafficIn == want*2 && r.TrafficOut == want*3
	})
	var user model.User
	db.First(&user, 1)
	if user.TrafficUsed != want*5 {
		t.Fatalf("user total=%d want=%d", user.TrafficUsed, want*5)
	}
	if err := a.User.SetStatus(context.Background(), 1, false); err != nil {
		t.Fatal(err)
	}
	wait("disabled policy reached both nodes", bothApplied)
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("disabled user kept live forwarding connection")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("disabled user connection remained open until read timeout")
	}
	if err := a.User.SetStatus(context.Background(), 1, true); err != nil {
		t.Fatal(err)
	}
	// The entrance can apply before the exit. Wait for both acknowledgements;
	// an entrance heartbeat alone does not promise end-to-end readiness.
	wait("reenabled policy applied on both nodes", bothApplied)
	second, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	_ = second.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := second.Write(payload); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(second, got); err != nil {
		t.Fatal("reenabled forwarding:", err)
	}
}
