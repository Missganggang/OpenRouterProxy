package nodeclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openroute/openroute/internal/nodeproto"
)

type fakeEngine struct {
	configs []nodeproto.ConfigResponse
	stats   nodeproto.ReportStats
	closed  bool
}

func (e *fakeEngine) Apply(c nodeproto.ConfigResponse) []nodeproto.RuleSyncResult {
	e.configs = append(e.configs, c)
	var results []nodeproto.RuleSyncResult
	for _, rule := range c.Rules {
		results = append(results, nodeproto.RuleSyncResult{RuleID: rule.RuleID, Status: "normal"})
	}
	return results
}
func (e *fakeEngine) RunningRules() []nodeproto.RunningRule { return []nodeproto.RunningRule{} }
func (e *fakeEngine) Stats() nodeproto.ReportStats          { s := e.stats; e.stats.RuleTraffic = nil; return s }
func (e *fakeEngine) Close()                                { e.closed = true }

func TestClientRegistrationRetryHeartbeatConfigTasksAndRestore(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var registrations, heartbeats, reports, tasks atomic.Int32
	const token = "test-node-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/node/register" && r.Header.Get(nodeproto.NodeTokenHeader) != token {
			t.Error("authenticated request lacks X-Node-Token")
		}
		if r.URL.Path != "/api/node/register" && r.Header.Get(nodeproto.NodeIDHeader) != "7" {
			t.Error("node ID header missing")
		}
		switch r.URL.Path {
		case "/api/node/register":
			var req nodeproto.RegisterRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			if req.Token != token || req.UUID == "" || req.Version != "test-version" {
				t.Error("invalid registration")
			}
			if registrations.Add(1) < 3 {
				w.WriteHeader(503)
				return
			}
			json.NewEncoder(w).Encode(nodeproto.RegisterResponse{NodeID: 7, HeartbeatInterval: 1, Config: nodeproto.ConfigResponse{Full: true, ConfigVersion: 1}})
		case "/api/node/heartbeat":
			var req nodeproto.HeartbeatRequest
			json.NewDecoder(r.Body).Decode(&req)
			if req.NodeID != 7 || req.ConfigVersion != 1 || req.Version != "test-version" || req.Timestamp <= 0 {
				t.Error("invalid heartbeat")
			}
			heartbeats.Add(1)
			json.NewEncoder(w).Encode(nodeproto.HeartbeatResponse{ConfigVersion: 2, NeedConfig: true, HeartbeatInterval: 1})
		case "/api/node/config":
			if r.URL.Query().Get("version") != "0" {
				t.Error("full config must be requested")
			}
			json.NewEncoder(w).Encode(nodeproto.ConfigResponse{Full: true, ConfigVersion: 2, Rules: []nodeproto.ConfigRule{{RuleID: 9, Enable: true}}})
		case "/api/node/report":
			var req nodeproto.ReportRequest
			json.NewDecoder(r.Body).Decode(&req)
			if req.ConfigVersion != 2 || len(req.Results) != 1 || req.Results[0].RuleID != 9 {
				t.Error("config results were not reported")
			}
			reports.Add(1)
			json.NewEncoder(w).Encode(nodeproto.ReportResponse{Accepted: 1})
		case "/api/node/tasks":
			json.NewEncoder(w).Encode(nodeproto.TasksResponse{Tasks: []nodeproto.TaskItem{{TaskID: 99, Type: "exec"}}})
		case "/api/node/task-result":
			var req nodeproto.TaskResultRequest
			json.NewDecoder(r.Body).Decode(&req)
			if req.TaskID != 99 || req.Status != "failed" {
				t.Error("unsupported task must fail explicitly")
			}
			tasks.Add(1)
			json.NewEncoder(w).Encode(nodeproto.TaskResultResponse{OK: true})
			cancel()
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	client, err := NewClient(Config{BaseURL: server.URL, Token: token, DataDir: dir}, "test-version", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	engine := &fakeEngine{}
	client.engine = engine
	client.retryMin, client.retryMax = 5*time.Millisecond, 20*time.Millisecond
	started := time.Now()
	if err := client.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}
	if time.Since(started) < 15*time.Millisecond || registrations.Load() != 3 || heartbeats.Load() != 1 || reports.Load() != 1 || tasks.Load() != 1 {
		t.Fatal("retry or protocol sequence incomplete")
	}
	if !engine.closed {
		t.Error("engine not closed on cancellation")
	}
	if len(engine.configs) != 2 || engine.configs[1].ConfigVersion != 2 {
		t.Fatal("full configuration not applied")
	}
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), token) {
		t.Error("saved config contains token")
	}
	restored, err := NewClient(Config{BaseURL: server.URL, Token: token, DataDir: dir}, "test-version", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	restored.engine = &fakeEngine{}
	if err := restored.restore(); err != nil || restored.configVersion != 2 {
		t.Fatalf("restore failed: %v", err)
	}
	if restored.config.MachineID != client.config.MachineID {
		t.Error("machine ID changed after restart")
	}
	restored.config.Token = "different-node"
	if err := restored.restore(); err == nil {
		t.Error("restored configuration for another node")
	}
}

func TestClientCancelsInflightRegistration(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-release }))
	defer server.Close()
	defer close(release)
	client, err := NewClient(Config{BaseURL: server.URL, Token: "secret", DataDir: t.TempDir()}, "test", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not interrupt HTTP request")
	}
}

func TestClientRetainsTrafficWhenReportFails(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req nodeproto.ReportRequest
		json.NewDecoder(r.Body).Decode(&req)
		if len(req.Results) != 1 {
			t.Error("lost pending sync result")
		}
		if calls == 1 {
			w.WriteHeader(503)
			return
		}
		if len(req.Stats.RuleTraffic) != 1 || req.Stats.RuleTraffic[0].TrafficIn != 10 {
			t.Error("lost pending traffic")
		}
		json.NewEncoder(w).Encode(nodeproto.ReportResponse{Accepted: 1})
	}))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: server.URL, Token: "secret", DataDir: t.TempDir()}, "test", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	engine := &fakeEngine{stats: nodeproto.ReportStats{RuleTraffic: []nodeproto.RuleTrafficItem{{RuleID: 9, TrafficIn: 7}}}}
	client.engine = engine
	client.results = []nodeproto.RuleSyncResult{{RuleID: 9, Status: "normal"}}
	if err := client.report(context.Background()); err == nil {
		t.Fatal("expected failed report")
	}
	engine.stats.RuleTraffic = []nodeproto.RuleTrafficItem{{RuleID: 9, TrafficIn: 3}}
	if err := client.report(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(client.pendingTraffic) != 0 || len(client.results) != 0 {
		t.Error("acknowledged results not cleared")
	}
}

func TestLoadConfigInstallerAndOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte("base-url: https://panel.example/\ntoken: old-token\nis-outbound: false\ndirect-port: 0\nmachine-id: fixed-id\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path, "https://override.example/", "new-token")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL != "https://override.example" || cfg.Token != "new-token" || cfg.MachineID != "fixed-id" {
		t.Fatal("installer config or CLI override not honored")
	}
	for _, bad := range []string{"file:///etc/passwd", "https://secret@host", "https://host?token=secret"} {
		if _, err := LoadConfig(path, bad, "token"); err == nil {
			t.Errorf("accepted invalid URL %q", bad)
		}
	}
}

func TestClientContinuesPeriodicHeartbeats(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var beats atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/node/register":
			json.NewEncoder(w).Encode(nodeproto.RegisterResponse{NodeID: 1, Config: nodeproto.ConfigResponse{Full: true, ConfigVersion: 1}})
		case "/api/node/heartbeat":
			json.NewEncoder(w).Encode(nodeproto.HeartbeatResponse{ConfigVersion: 1})
			if beats.Add(1) == 2 {
				cancel()
			}
		case "/api/node/report":
			json.NewEncoder(w).Encode(nodeproto.ReportResponse{})
		case "/api/node/tasks":
			json.NewEncoder(w).Encode(nodeproto.TasksResponse{})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: server.URL, Token: "secret", DataDir: t.TempDir()}, "test", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	client.interval = 10 * time.Millisecond
	if err := client.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}
	if beats.Load() != 2 {
		t.Fatal("periodic heartbeat not sent")
	}
}

func TestClientRetainsPartiallyAcceptedResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(nodeproto.ReportResponse{Accepted: 1})
	}))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: server.URL, Token: "secret", DataDir: t.TempDir()}, "test", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	client.results = []nodeproto.RuleSyncResult{{RuleID: 1, Status: "normal"}, {RuleID: 2, Status: "failed"}}
	if err := client.report(context.Background()); err == nil {
		t.Error("expected incomplete acknowledgement error")
	}
	if len(client.results) != 2 {
		t.Error("partially acknowledged sync results were discarded")
	}
}
