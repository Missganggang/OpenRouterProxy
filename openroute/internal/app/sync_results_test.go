package app

import (
	"context"
	"testing"
	"time"

	"github.com/openroute/openroute/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func seedSyncPolicies(t *testing.T, a *App, version int64) {
	t.Helper()
	var rule model.ForwardRule
	if err := a.DB.First(&rule, 1).Error; err != nil {
		t.Fatal(err)
	}
	if err := a.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&model.ConfigRevision{ID: 1, Revision: version}).Error; err != nil {
		t.Fatal(err)
	}
	for _, policy := range []model.NodeTrafficPolicy{
		{NodeID: 1, ConfigVersion: version, RuleID: 1, Direction: "inbound", UserID: rule.UserID, Multiplier: rule.InboundMultiplier, RuleHash: model.RuleConfigHash(rule)},
		{NodeID: 2, ConfigVersion: version, RuleID: 1, Direction: "outbound", UserID: rule.UserID, Multiplier: rule.OutboundMultiplier, RuleHash: model.RuleConfigHash(rule)},
	} {
		if err := a.DB.Create(&policy).Error; err != nil {
			t.Fatal(err)
		}
	}
	a.SetConfigVersion(version)
}

func TestNodeSyncIgnoresACKAfterEditBeforeVersionBump(t *testing.T) {
	a := reportFixture(t)
	seedSyncPolicies(t, a, 1)
	// This is the real Save -> BumpConfigVersion interval. The global version
	// still matches the ACK, but the rule is no longer the configuration sent.
	if err := a.DB.Model(&model.ForwardRule{}).Where("id = ?", 1).Updates(map[string]interface{}{
		"listen_port": 23456, "sync_status": model.SyncUnsynced,
	}).Error; err != nil {
		t.Fatal(err)
	}
	for _, nodeID := range []uint64{1, 2} {
		if err := a.Sync.RecordNodeResult(context.Background(), 1, nodeID, 1, true, ""); err != nil {
			t.Fatal(err)
		}
	}
	var rule model.ForwardRule
	a.DB.First(&rule, 1)
	var count int64
	a.DB.Model(&model.NodeRuleSync{}).Count(&count)
	if rule.SyncStatus != model.SyncUnsynced || count != 0 {
		t.Fatalf("old ACK changed edited rule: status=%s receipts=%d", rule.SyncStatus, count)
	}
	seedSyncPolicies(t, a, 2)
	for _, nodeID := range []uint64{1, 2} {
		if err := a.Sync.RecordNodeResult(context.Background(), 1, nodeID, 2, true, ""); err != nil {
			t.Fatal(err)
		}
	}
	a.DB.First(&rule, 1)
	if rule.SyncStatus != model.SyncNormal {
		t.Fatalf("current ACK not accepted: %s", rule.SyncStatus)
	}
}

func TestNodeSyncVersionBumpWaitsForACKTransaction(t *testing.T) {
	a := reportFixture(t)
	seedSyncPolicies(t, a, 1)
	entered, release := make(chan struct{}), make(chan struct{})
	const callback = "test:pause_ack"
	if err := a.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "forward_rules" {
			close(entered)
			<-release
		}
	}); err != nil {
		t.Fatal(err)
	}
	ackDone := make(chan error, 1)
	go func() { ackDone <- a.Sync.RecordNodeResult(context.Background(), 1, 1, 1, true, "") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("ACK did not reach transaction")
	}
	// TryLock tests the lock directly without relying on scheduler timing.
	if a.versionMu.TryLock() {
		a.versionMu.Unlock()
		close(release)
		t.Fatal("configuration version can change during ACK transaction")
	}
	bumpDone := make(chan int64, 1)
	go func() { bumpDone <- a.BumpConfigVersion("concurrent change") }()
	close(release)
	select {
	case err := <-ackDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ACK deadlocked with version bump")
	}
	if err := a.DB.Callback().Query().Remove(callback); err != nil {
		t.Fatal(err)
	}
	select {
	case version := <-bumpDone:
		if version != 2 {
			t.Fatalf("version=%d", version)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("version bump deadlocked with ACK")
	}
}

func TestNodeSyncRequiresPersistedDeliveredVersion(t *testing.T) {
	for _, mode := range []string{"unpersisted", "undelivered"} {
		t.Run(mode, func(t *testing.T) {
			a := reportFixture(t)
			a.SetConfigVersion(1)
			if mode == "undelivered" {
				if err := a.DB.Create(&model.ConfigRevision{ID: 1, Revision: 1}).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := a.Sync.RecordNodeResult(context.Background(), 1, 1, 1, true, ""); err != nil {
				t.Fatal(err)
			}
			var rule model.ForwardRule
			a.DB.First(&rule, 1)
			if rule.SyncStatus != model.SyncUnsynced {
				t.Fatalf("accepted %s ACK: %s", mode, rule.SyncStatus)
			}
		})
	}
}
