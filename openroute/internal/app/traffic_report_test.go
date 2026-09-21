package app

import (
	"context"
	"github.com/openroute/openroute/internal/model"
	"github.com/openroute/openroute/internal/nodeproto"
	"testing"
	"time"
)

func reportFixture(t *testing.T) *App {
	t.Helper()
	a := newLimitTestApp(t)
	a.hub = newHub(a.Log)
	t.Cleanup(func() { a.hub.Close() })
	a.Traffic = NewTrafficService(a)
	a.Sync = NewSyncService(a)
	for _, value := range []interface{}{
		&model.User{ID: 1, Username: "report-user", PasswordHash: "unused", Token: "user-token", TrafficLimit: 1000},
		&model.Node{ID: 1, Name: "ingress", Token: "in-token"},
		&model.Node{ID: 2, Name: "egress", Token: "out-token"},
		&model.Node{ID: 3, Name: "unrelated", Token: "other-token"},
		&model.DeviceGroup{ID: 1, Name: "in", Type: "inbound", NodeIDs: model.FromAny([]uint64{1})},
		&model.DeviceGroup{ID: 2, Name: "out", Type: "outbound", NodeIDs: model.FromAny([]uint64{2})},
		&model.ForwardRule{ID: 1, Name: "forward", UserID: 1, InboundGroupID: 1, OutboundGroupID: 2, InboundMultiplier: 2, OutboundMultiplier: 3},
	} {
		if err := a.DB.Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
	return a
}

func TestNodeTrafficTransactionRetryAndMultiplier(t *testing.T) {
	a := reportFixture(t)
	ctx := context.Background()
	report := nodeproto.ReportRequest{NodeID: 1, BatchID: "batch-1", Timestamp: time.Now().Unix(), Stats: &nodeproto.ReportStats{RuleTraffic: []nodeproto.RuleTrafficItem{{RuleID: 1, Direction: "inbound", TrafficIn: 100, TrafficOut: 200}}}}
	// Force a failure after detail writes; receipt and all aggregates must roll back.
	if err := a.DB.Exec("CREATE TRIGGER reject_traffic BEFORE UPDATE OF traffic_used ON users BEGIN SELECT RAISE(ABORT, 'test failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	if err := a.Traffic.RecordNodeReport(ctx, 1, report); err == nil {
		t.Fatal("wanted transaction failure")
	}
	var receipts, logs int64
	a.DB.Model(&model.NodeReportBatch{}).Count(&receipts)
	a.DB.Model(&model.TrafficLog{}).Count(&logs)
	if receipts != 0 || logs != 0 {
		t.Fatalf("partial transaction: receipts=%d logs=%d", receipts, logs)
	}
	a.DB.Exec("DROP TRIGGER reject_traffic")
	if err := a.Traffic.RecordNodeReport(ctx, 1, report); err != nil {
		t.Fatal(err)
	}
	// New service instance models retry after a panel restart.
	if err := NewTrafficService(a).RecordNodeReport(ctx, 1, report); err != nil {
		t.Fatal(err)
	}
	var user model.User
	a.DB.First(&user, 1)
	if user.TrafficUsed != 600 {
		t.Fatalf("retry double counted: %d", user.TrafficUsed)
	}
	report.BatchID, report.NodeID = "batch-2", 2
	report.Stats.RuleTraffic[0].Direction = "outbound"
	if err := a.Traffic.RecordNodeReport(ctx, 2, report); err != nil {
		t.Fatal(err)
	}
	a.DB.First(&user, 1)
	if user.TrafficUsed != 1500 {
		t.Fatalf("both endpoint multipliers: %d", user.TrafficUsed)
	}
	if a.ConfigVersion() != 1 {
		t.Fatalf("quota crossing did not push config: %d", a.ConfigVersion())
	}
	var bytes int64
	a.DB.Model(&model.TrafficLog{}).Where("hour = -1").Select("sum(bytes)").Scan(&bytes)
	if bytes != 1500 {
		t.Fatalf("daily aggregation: %d", bytes)
	}
	report.Stats.RuleTraffic[0].TrafficIn++
	if err := a.Traffic.RecordNodeReport(ctx, 2, report); err == nil {
		t.Fatal("changed contents reused batch id")
	}
}

func TestNodeTrafficOwnershipZeroMultiplierAndInvalidInput(t *testing.T) {
	a := reportFixture(t)
	a.DB.Model(&model.ForwardRule{}).Where("id = 1").UpdateColumn("inbound_multiplier", 0)
	report := nodeproto.ReportRequest{BatchID: "zero", Stats: &nodeproto.ReportStats{RuleTraffic: []nodeproto.RuleTrafficItem{{RuleID: 1, Direction: "inbound", TrafficIn: 123}}}}
	if err := a.Traffic.RecordNodeReport(context.Background(), 1, report); err != nil {
		t.Fatal(err)
	}
	var row model.TrafficLog
	a.DB.Where("hour = -1").First(&row)
	if row.RawBytes != 123 || row.Bytes != 0 {
		t.Fatalf("zero multiplier changed: %+v", row)
	}
	report.BatchID = "forged"
	if err := a.Traffic.RecordNodeReport(context.Background(), 3, report); err != nil {
		t.Fatal(err)
	}
	a.DB.Where("hour = -1").First(&row)
	if row.RawBytes != 123 {
		t.Fatal("foreign node wrote traffic")
	}
	report.BatchID = "negative"
	report.Stats.RuleTraffic[0].TrafficIn = -1
	if err := a.Traffic.RecordNodeReport(context.Background(), 1, report); err == nil {
		t.Fatal("accepted negative traffic")
	}
}

func TestNodeSyncResultsAggregateAndRecover(t *testing.T) {
	a := reportFixture(t)
	seedSyncPolicies(t, a, 4)
	ctx := context.Background()
	check := func(want string) {
		t.Helper()
		var rule model.ForwardRule
		a.DB.First(&rule, 1)
		if rule.SyncStatus != want {
			t.Fatalf("status=%s want=%s", rule.SyncStatus, want)
		}
	}
	if err := a.Sync.RecordNodeResult(ctx, 1, 1, 4, true, ""); err != nil {
		t.Fatal(err)
	}
	check(model.SyncSyncing)
	if err := a.Sync.RecordNodeResult(ctx, 1, 2, 4, false, "port occupied"); err != nil {
		t.Fatal(err)
	}
	check(model.SyncFailed)
	if err := a.Sync.RecordNodeResult(ctx, 1, 1, 4, true, ""); err != nil {
		t.Fatal(err)
	}
	check(model.SyncFailed)
	if err := a.Sync.RecordNodeResult(ctx, 1, 2, 4, true, ""); err != nil {
		t.Fatal(err)
	}
	check(model.SyncNormal)
	if err := a.Sync.RecordNodeResult(ctx, 1, 3, 4, false, "forged"); err != nil {
		t.Fatal(err)
	}
	if err := a.Sync.RecordNodeResult(ctx, 1, 2, 3, false, "stale"); err != nil {
		t.Fatal(err)
	}
	check(model.SyncNormal)
}

func TestConfigVersionSurvivesRestart(t *testing.T) {
	a := reportFixture(t)
	a.BumpConfigVersion("first")
	a.BumpConfigVersion("second")
	b := New(a.Config, a.DB, a.Log).Init()
	t.Cleanup(b.Stop)
	if b.ConfigVersion() != 2 {
		t.Fatalf("lost version: %d", b.ConfigVersion())
	}
	if b.BumpConfigVersion("third") != 3 {
		t.Fatal("version did not remain monotonic")
	}
}
