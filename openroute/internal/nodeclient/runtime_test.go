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

	"github.com/gorilla/websocket"
	"github.com/openroute/openroute/internal/nodeproto"
	"github.com/openroute/openroute/internal/nodestream"
)

func runtimeClient(t *testing.T, cfg Config) *Client {
	t.Helper()
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://panel.example"
	}
	if cfg.Token == "" {
		cfg.Token = "test-secret"
	}
	if cfg.DataDir == "" {
		cfg.DataDir = t.TempDir()
	}
	c, err := NewClient(cfg, "test-version", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	c.engine = &fakeEngine{}
	return c
}

func TestTaskExecutionIsDeduplicatedAcrossRestart(t *testing.T) {
	c := runtimeClient(t, Config{})
	var executions atomic.Int32
	c.tasks.exec = func(_ context.Context, command string, timeout time.Duration) (string, error) {
		executions.Add(1)
		if command != "echo expected" || timeout != 7*time.Second {
			t.Error("command or timeout lost")
		}
		return "expected", nil
	}
	task := nodeproto.TaskItem{TaskID: 13, Type: nodeproto.TaskTypeExec, Payload: map[string]interface{}{"command": "echo expected", "timeout": 7}}
	c.tasks.submit([]nodeproto.TaskItem{task, task})
	if len(c.tasks.queue) != 1 {
		t.Fatal("duplicate queued")
	}
	c.tasks.execute(context.Background(), <-c.tasks.queue)
	c.tasks.submit([]nodeproto.TaskItem{task})
	if len(c.tasks.queue) != 0 || executions.Load() != 1 {
		t.Fatal("completed task replayed")
	}
	restored := runtimeClient(t, c.config)
	restored.tasks.submit([]nodeproto.TaskItem{task})
	if len(restored.tasks.queue) != 0 || restored.tasks.records[13].Result != "expected" {
		t.Fatal("persisted execution was forgotten")
	}
}

func TestTaskInterruptedAtExecutionMarkerIsNotReplayed(t *testing.T) {
	c := runtimeClient(t, Config{})
	task := nodeproto.TaskItem{TaskID: 14, Type: nodeproto.TaskTypeExec}
	c.tasks.records[14] = &taskRecord{Task: task, State: "running"}
	if err := c.tasks.saveLocked(); err != nil {
		t.Fatal(err)
	}
	restored := runtimeClient(t, c.config)
	restored.tasks.submit([]nodeproto.TaskItem{task})
	if len(restored.tasks.queue) != 0 || restored.tasks.records[14].State != "failed" || !strings.Contains(restored.tasks.records[14].Result, "not replayed") {
		t.Fatal("uncertain command replayed")
	}
}

func TestRestartCompletesOnlyAfterNewProcessRegisters(t *testing.T) {
	c := runtimeClient(t, Config{})
	task := nodeproto.TaskItem{TaskID: 15, Type: nodeproto.TaskTypeRestart}
	if !c.tasks.execute(context.Background(), task) {
		t.Fatal("restart not requested")
	}
	if err := c.tasks.registered(); err != nil {
		t.Fatal(err)
	}
	if c.tasks.records[15].State != "restart_pending" {
		t.Fatal("old process acknowledged restart")
	}
	if err := c.wait(context.Background(), time.Second); !errors.Is(err, ErrRestart) {
		t.Fatalf("restart signal: %v", err)
	}
	restored := runtimeClient(t, c.config)
	if restored.tasks.records[15].State != "restart_pending" {
		t.Fatal("restart acknowledged before registration")
	}
	if err := restored.tasks.registered(); err != nil {
		t.Fatal(err)
	}
	if restored.tasks.records[15].State != "done" {
		t.Fatal("successful restarted registration not acknowledged")
	}
}

func TestDisableExecuteRejectsEverySideEffect(t *testing.T) {
	c := runtimeClient(t, Config{DisableExecute: true})
	c.tasks.exec = func(context.Context, string, time.Duration) (string, error) { t.Error("exec ran"); return "", nil }
	c.tasks.upgrade = func(context.Context, nodeproto.TaskItem) (string, error) { t.Error("upgrade ran"); return "", nil }
	for i, kind := range []string{nodeproto.TaskTypeExec, nodeproto.TaskTypeRestart, nodeproto.TaskTypeUpgrade} {
		id := uint64(i + 1)
		if c.tasks.execute(context.Background(), nodeproto.TaskItem{TaskID: id, Type: kind, Payload: map[string]interface{}{"command": "echo bad"}}) {
			t.Error("disabled task requested restart")
		}
		if rec := c.tasks.records[id]; rec.State != "failed" || !strings.Contains(rec.Result, "DISABLE_EXECUTE") {
			t.Errorf("task not rejected: %+v", rec)
		}
	}
	var reply nodestream.Message
	m := newTerminalManager(context.Background(), true, func(message nodestream.Message) error { reply = message; return nil })
	defer m.close()
	m.handle(nodestream.Message{Type: "terminal_open", Data: nodestream.Payload{SessionID: strings.Repeat("a", 32)}})
	if reply.Type != "terminal_error" || !strings.Contains(reply.Data.Message, "DISABLE_EXECUTE") {
		t.Fatal("disabled terminal not rejected")
	}
}

func TestTaskResultRetriesWithoutReexecution(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var result nodeproto.TaskResultRequest
		json.NewDecoder(r.Body).Decode(&result)
		if result.TaskID != 1 || result.Status != "done" || result.Result != "once" {
			t.Error("incorrect task result")
		}
		if calls.Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		json.NewEncoder(w).Encode(nodeproto.TaskResultResponse{OK: true})
	}))
	defer server.Close()
	c := runtimeClient(t, Config{BaseURL: server.URL})
	c.tasks.exec = func(context.Context, string, time.Duration) (string, error) { return "once", nil }
	c.tasks.execute(context.Background(), nodeproto.TaskItem{TaskID: 1, Type: nodeproto.TaskTypeExec, Payload: map[string]interface{}{"command": "echo once"}})
	if c.tasks.report(context.Background()) == nil {
		t.Fatal("expected failed upload")
	}
	restored := runtimeClient(t, c.config)
	if err := restored.tasks.report(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := restored.tasks.report(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || !restored.tasks.records[1].Acknowledged {
		t.Fatal("result was lost or uploaded after acknowledgement")
	}
}

func TestReportsPreserveDirectionsAndRequireMatchingBatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(nodeproto.ReportResponse{BatchID: "wrong-batch"})
	}))
	defer server.Close()
	c := runtimeClient(t, Config{BaseURL: server.URL})
	c.engine = &fakeEngine{stats: nodeproto.ReportStats{RuleTraffic: []nodeproto.RuleTrafficItem{{RuleID: 1, Direction: "inbound", TrafficIn: 3}, {RuleID: 1, Direction: "outbound", TrafficIn: 5}, {RuleID: 1, Direction: "inbound", TrafficIn: 7}}}}
	if c.report(context.Background()) == nil {
		t.Fatal("accepted mismatched acknowledgement")
	}
	restored := runtimeClient(t, c.config)
	if restored.outbox == nil || restored.outbox.BatchID != c.outbox.BatchID || len(restored.outbox.Stats.RuleTraffic) != 2 {
		t.Fatal("batch or direction lost")
	}
	for _, row := range restored.outbox.Stats.RuleTraffic {
		if row.Direction == "inbound" && row.TrafficIn != 10 || row.Direction == "outbound" && row.TrafficIn != 5 {
			t.Errorf("wrong aggregate: %+v", row)
		}
	}
}

type effectiveTestEngine struct {
	fakeEngine
	effective nodeproto.ConfigResponse
}

func (e *effectiveTestEngine) Apply(cfg nodeproto.ConfigResponse) []nodeproto.RuleSyncResult {
	return []nodeproto.RuleSyncResult{{RuleID: 1, Status: "failed", Error: "port in use"}}
}
func (e *effectiveTestEngine) EffectiveConfig() nodeproto.ConfigResponse { return e.effective }

func TestFailedConfigPersistsOnlyEffectiveRules(t *testing.T) {
	c := runtimeClient(t, Config{})
	c.engine = &effectiveTestEngine{effective: nodeproto.ConfigResponse{Full: true, ConfigVersion: 2, Rules: []nodeproto.ConfigRule{{RuleID: 1, ListenPort: 8080, Enable: true}}}}
	c.apply(nodeproto.ConfigResponse{Full: true, ConfigVersion: 3, Rules: []nodeproto.ConfigRule{{RuleID: 1, ListenPort: 9090, Enable: true}}})
	data, err := os.ReadFile(filepath.Join(c.config.DataDir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved savedConfig
	if err = json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Config.ConfigVersion != 2 || saved.Config.Rules[0].ListenPort != 8080 || c.configError == "" {
		t.Fatal("failed desired config replaced effective config")
	}
}

func TestNodeStreamAuthenticatesAndWakesConfigPolling(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/node/stream" || r.Header.Get(nodeproto.NodeTokenHeader) != "test-secret" {
			t.Error("stream authentication missing")
			w.WriteHeader(401)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var hello nodestream.Message
		if conn.ReadJSON(&hello) != nil || hello.Type != "node_hello" || !hello.Data.DisableExecute {
			t.Error("node capability handshake missing")
			return
		}
		conn.WriteJSON(nodestream.Message{Type: "config_changed"})
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	c := runtimeClient(t, Config{BaseURL: server.URL, DisableExecute: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.streamOnce(ctx) }()
	waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
	defer waitCancel()
	if err := c.wait(waitCtx, 30*time.Second); err != nil || !c.forceConfig {
		t.Fatalf("stream failed to wake polling: %v", err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream did not cancel")
	}
}

func TestConfigHonorsDisableExecuteEnvironment(t *testing.T) {
	t.Setenv("DISABLE_EXECUTE", "1")
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "config.yml"), "https://panel.example", "secret")
	if err != nil || !cfg.DisableExecute {
		t.Fatalf("environment restriction missing: %v", err)
	}
}

func TestLongTaskDoesNotBlockHeartbeats(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var beats atomic.Int32
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/node/register":
			json.NewEncoder(w).Encode(nodeproto.RegisterResponse{NodeID: 1, Config: nodeproto.ConfigResponse{Full: true, ConfigVersion: 1}})
		case "/api/node/heartbeat":
			json.NewEncoder(w).Encode(nodeproto.HeartbeatResponse{ConfigVersion: 1, Tasks: []nodeproto.TaskItem{{TaskID: 1, Type: nodeproto.TaskTypeExec, Payload: map[string]interface{}{"command": "slow command"}}}})
			if beats.Add(1) >= 4 {
				select {
				case <-started:
					cancel()
				default:
				}
			}
		case "/api/node/report":
			var req nodeproto.ReportRequest
			json.NewDecoder(r.Body).Decode(&req)
			json.NewEncoder(w).Encode(nodeproto.ReportResponse{BatchID: req.BatchID})
		case "/api/node/stream":
			w.WriteHeader(404)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	c := runtimeClient(t, Config{BaseURL: server.URL})
	c.interval = 10 * time.Millisecond
	c.tasks.exec = func(ctx context.Context, _ string, _ time.Duration) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}
	if err := c.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("long task starved heartbeats: %v", err)
	}
	if beats.Load() < 4 {
		t.Fatal("heartbeats stopped during execution")
	}
}
