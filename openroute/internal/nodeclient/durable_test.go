package nodeclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/openroute/openroute/internal/nodeproto"
)

type sampledTestEngine struct {
	fakeEngine
	samples []TrafficSample
}

func (e *sampledTestEngine) CollectTraffic() (nodeproto.ReportStats, []TrafficSample) {
	samples := e.samples
	e.samples = nil
	return nodeproto.ReportStats{}, samples
}

func TestDurableTrafficKeepsMonthVersionAndFrozenRetries(t *testing.T) {
	oldTime := time.Date(2026, time.January, 31, 23, 59, 50, 0, time.UTC)
	newTime := oldTime.Add(20 * time.Second)
	var mu sync.Mutex
	var bodies [][]byte
	var reports []nodeproto.ReportRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var report nodeproto.ReportRequest
		if err := json.Unmarshal(body, &report); err != nil {
			t.Error(err)
		}
		mu.Lock()
		bodies = append(bodies, body)
		reports = append(reports, report)
		count := len(reports)
		mu.Unlock()
		if count <= 2 {
			w.WriteHeader(503)
			return
		}
		json.NewEncoder(w).Encode(nodeproto.ReportResponse{BatchID: report.BatchID, Accepted: len(report.Results)})
	}))
	defer server.Close()
	c := runtimeClient(t, Config{BaseURL: server.URL})
	c.nodeID = 1
	c.configVersion = 7
	c.now = func() time.Time { return oldTime }
	engine := &sampledTestEngine{samples: []TrafficSample{{ConfigVersion: 7, Timestamp: oldTime.Unix(), Item: nodeproto.RuleTrafficItem{RuleID: 1, Direction: "inbound", TrafficIn: 70}}}}
	c.engine = engine
	if err := c.report(context.Background()); err == nil {
		t.Fatal("expected first offline report")
	}
	c.now = func() time.Time { return newTime }
	c.configVersion = 8
	engine.samples = []TrafficSample{
		{ConfigVersion: 7, Timestamp: newTime.Unix(), Item: nodeproto.RuleTrafficItem{RuleID: 1, Direction: "inbound", TrafficIn: 20}},
		{ConfigVersion: 8, Timestamp: newTime.Unix(), Item: nodeproto.RuleTrafficItem{RuleID: 1, Direction: "inbound", TrafficIn: 30}},
	}
	if err := c.report(context.Background()); err == nil {
		t.Fatal("expected second offline report")
	}
	restored := runtimeClient(t, c.config)
	restored.nodeID = 1
	restored.configVersion = 8
	restored.now = func() time.Time { return newTime.AddDate(0, 1, 0) }
	restored.results = []nodeproto.RuleSyncResult{{RuleID: 1, Status: "normal"}}
	if err := restored.flushReports(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reports) != 5 {
		t.Fatalf("expected 2 failures plus 3 historical batches; got %d", len(reports))
	}
	if !bytes.Equal(bodies[0], bodies[1]) || !bytes.Equal(bodies[0], bodies[2]) {
		t.Fatal("retry/restart changed an in-flight batch")
	}
	totals := map[int64]map[int64]int64{}
	for _, report := range reports[2:] {
		if report.Timestamp >= newTime.AddDate(0, 1, 0).Unix() {
			t.Fatal("old traffic moved to reconnection month")
		}
		if len(report.Results) > 0 && report.ConfigVersion != 8 {
			t.Fatal("current synchronization result attached to historical version")
		}
		if totals[report.ConfigVersion] == nil {
			totals[report.ConfigVersion] = map[int64]int64{}
		}
		for _, row := range report.Stats.RuleTraffic {
			totals[report.ConfigVersion][report.Timestamp] += row.TrafficIn
		}
	}
	if totals[7][trafficHour(oldTime.Unix())] != 70 || totals[7][trafficHour(newTime.Unix())] != 20 || totals[8][trafficHour(newTime.Unix())] != 30 {
		t.Fatalf("version/month attribution lost: %+v", totals)
	}
	if len(restored.pendingTraffic) != 0 || restored.outbox != nil {
		t.Fatal("acknowledged historical traffic retained")
	}
}

func TestDurableTrafficCheckpointSurvivesRestartBeforeUpload(t *testing.T) {
	when := time.Date(2026, time.January, 31, 23, 0, 0, 0, time.UTC).Unix()
	c := runtimeClient(t, Config{})
	c.engine = &sampledTestEngine{samples: []TrafficSample{{ConfigVersion: 4, Timestamp: when, Item: nodeproto.RuleTrafficItem{RuleID: 1, Direction: "outbound", TrafficOut: 11}}}}
	c.checkpointTraffic()
	restored := runtimeClient(t, c.config)
	_, bucket := restored.nextTrafficBucket()
	if bucket == nil || bucket.Timestamp != when || bucket.ConfigVersion != 4 || bucket.Rows["1/outbound"].TrafficOut != 11 {
		t.Fatal("pre-upload offline sample lost")
	}
}

func TestOfflineRegistrationPersistsTrafficBeforeRequestCompletes(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/node/stream" {
			w.WriteHeader(404)
			return
		}
		close(started)
		<-release
		w.WriteHeader(503)
	}))
	defer server.Close()
	defer close(release)
	c := runtimeClient(t, Config{BaseURL: server.URL})
	when := time.Date(2026, time.January, 31, 23, 0, 0, 0, time.UTC).Unix()
	c.engine = &sampledTestEngine{samples: []TrafficSample{{ConfigVersion: 4, Timestamp: when, Item: nodeproto.RuleTrafficItem{RuleID: 1, Direction: "inbound", TrafficIn: 17}}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("registration did not start")
	}
	data, err := os.ReadFile(filepath.Join(c.config.DataDir, "report-outbox.json"))
	if err != nil {
		t.Fatal(err)
	}
	var journal reportJournal
	if err = json.Unmarshal(data, &journal); err != nil {
		t.Fatal(err)
	}
	if len(journal.Buckets) != 1 {
		t.Fatal("offline sample was not persisted before networking")
	}
	for _, bucket := range journal.Buckets {
		if bucket.Timestamp != when || bucket.ConfigVersion != 4 || bucket.Rows["1/inbound"].TrafficIn != 17 {
			t.Fatal("offline attribution changed")
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("client did not stop")
	}
}

func TestLegacyTrafficJournalMigrationRetainsFrozenBatch(t *testing.T) {
	c := runtimeClient(t, Config{})
	when := time.Date(2026, time.January, 31, 23, 30, 0, 0, time.UTC).Unix()
	legacy := reportJournal{Identity: c.identity(), Outbox: &nodeproto.ReportRequest{BatchID: newID(), ConfigVersion: 4, Timestamp: when, Stats: &nodeproto.ReportStats{RuleTraffic: []nodeproto.RuleTrafficItem{{RuleID: 1, TrafficIn: 9}}}}, Pending: map[string]nodeproto.RuleTrafficItem{"1/inbound": {RuleID: 1, Direction: "inbound", TrafficIn: 12}}}
	data, _ := json.Marshal(legacy)
	path := filepath.Join(c.config.DataDir, "report-outbox.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	restored := runtimeClient(t, c.config)
	if restored.outbox.BatchID != legacy.Outbox.BatchID || restored.outbox.Timestamp != when || restored.outbox.Stats.RuleTraffic[0].TrafficIn != 9 {
		t.Fatal("migration rewrote frozen batch")
	}
	_, bucket := restored.nextTrafficBucket()
	if bucket == nil || bucket.ConfigVersion != 4 || bucket.Timestamp != trafficHour(when) || bucket.Rows["1/inbound"].TrafficIn != 12 {
		t.Fatal("legacy pending traffic lost")
	}
	data, _ = os.ReadFile(path)
	var migrated reportJournal
	json.Unmarshal(data, &migrated)
	if migrated.Format != 2 || len(migrated.Pending) != 0 || len(migrated.Buckets) != 1 {
		t.Fatal("migration not persisted atomically")
	}
}

func TestDurableTrafficSplitsLargeCountersWithoutOverflow(t *testing.T) {
	c := runtimeClient(t, Config{})
	when := time.Now().Unix()
	c.addTrafficSample(TrafficSample{ConfigVersion: 2, Timestamp: when, Item: nodeproto.RuleTrafficItem{RuleID: 1, Direction: "inbound", TrafficIn: 2*maxTrafficIncrement + 7, TrafficOut: maxTrafficIncrement + 9}})
	var totalIn, totalOut int64
	for _, bucket := range c.pendingTraffic {
		for _, row := range bucket.Rows {
			if row.TrafficIn > maxTrafficIncrement || row.TrafficOut > maxTrafficIncrement || row.TrafficIn < 0 || row.TrafficOut < 0 {
				t.Fatal("oversized or wrapped increment")
			}
			totalIn += row.TrafficIn
			totalOut += row.TrafficOut
		}
	}
	if totalIn != 2*maxTrafficIncrement+7 || totalOut != maxTrafficIncrement+9 {
		t.Fatal("splitting dropped traffic")
	}
}
