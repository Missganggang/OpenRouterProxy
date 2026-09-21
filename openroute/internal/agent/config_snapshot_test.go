package agent

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/openroute/openroute/internal/app"
	"github.com/openroute/openroute/internal/config"
	"github.com/openroute/openroute/internal/database"
	"github.com/openroute/openroute/internal/model"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

func newConfigSnapshotFixture(t *testing.T) (*app.App, *model.Node) {
	t.Helper()
	db, err := database.Open(database.Options{Path: "sqlite3://" + filepath.Join(t.TempDir(), "snapshot.db"), MaxOpen: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.AutoMigrate(); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.SecretKey = "snapshot-test-secret"
	a := app.New(cfg, db, zap.NewNop()).Init()
	t.Cleanup(a.Stop)
	node := &model.Node{ID: 1, Name: "both-roles", Token: "node-token", PublicIPv4: "127.0.0.1"}
	for _, row := range []interface{}{
		node,
		&model.User{ID: 1, Username: "owner", PasswordHash: "unused", Token: "owner-token", Status: 1},
		&model.DeviceGroup{ID: 1, Name: "in", Type: "inbound", NodeIDs: model.FromAny([]uint64{1})},
		&model.DeviceGroup{ID: 2, Name: "out", Type: "outbound", NodeIDs: model.FromAny([]uint64{1})},
		&model.ForwardRule{ID: 1, Name: "rule", UserID: 1, InboundGroupID: 1, OutboundGroupID: 2, ListenPort: 12345,
			InboundMultiplier: 2, OutboundMultiplier: 3, Targets: model.FromAny([]model.Target{{Host: "example.com", Port: 80}})},
	} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	a.BumpConfigVersion("fixture")
	return a, node
}

func TestConfigCompilerRetriesConcurrentEditAndReloadsNode(t *testing.T) {
	a, node := newConfigSnapshotFixture(t)
	changed := false
	const callback = "test:edit_during_compile"
	if err := a.DB.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table != "forward_rules" || changed {
			return
		}
		changed = true
		if err := a.DB.Model(&model.ForwardRule{}).Where("id = 1").Update("listen_port", 23456).Error; err != nil {
			t.Fatal(err)
		}
		if err := a.DB.Model(&model.Node{}).Where("id = 1").Updates(map[string]interface{}{"disabled": true, "direct_port": 30000}).Error; err != nil {
			t.Fatal(err)
		}
		a.BumpConfigVersion("during compile")
	}); err != nil {
		t.Fatal(err)
	}
	defer a.DB.Callback().Query().Remove(callback)
	got, err := NewConfigBuilder(a).BuildFull(context.Background(), node)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || got.ConfigVersion != 2 || !got.NodeDisabled || got.Listeners.DirectPort != 30000 || len(got.Rules) != 2 {
		t.Fatalf("inconsistent response: %+v", got)
	}
	for _, rule := range got.Rules {
		if rule.ListenPort != 23456 || rule.Enable {
			t.Fatalf("old rule tagged with new version: %+v", rule)
		}
	}
	var policies []model.NodeTrafficPolicy
	a.DB.Order("direction").Find(&policies)
	if len(policies) != 2 || policies[0].ConfigVersion != 2 || policies[1].ConfigVersion != 2 || policies[0].Direction != "inbound" || policies[1].Direction != "outbound" {
		t.Fatalf("wrong delivered snapshots: %+v", policies)
	}
}

func TestConfigCompilerPreservesBillingSnapshots(t *testing.T) {
	a, node := newConfigSnapshotFixture(t)
	if err := a.DB.Model(&model.ForwardRule{}).Where("id = 1").UpdateColumn("inbound_multiplier", 0).Error; err != nil {
		t.Fatal(err)
	}
	b := NewConfigBuilder(a)
	for i := 0; i < 2; i++ {
		if _, err := b.BuildFull(context.Background(), node); err != nil {
			t.Fatal(err)
		}
	}
	// Editing without a bump cannot overwrite the already-delivered policy.
	if err := a.DB.Model(&model.ForwardRule{}).Where("id = 1").Updates(map[string]interface{}{"user_id": 9, "inbound_multiplier": 5}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := b.BuildFull(context.Background(), node); err == nil {
		t.Fatal("overwrote immutable policy in the same version")
	}
	var saved model.NodeTrafficPolicy
	a.DB.Where("node_id = 1 AND config_version = 1 AND rule_id = 1 AND direction = ?", "inbound").First(&saved)
	if saved.UserID != 1 || saved.Multiplier != 0 || saved.RuleHash == "" {
		t.Fatalf("historical policy changed: %+v", saved)
	}
	a.BumpConfigVersion("owner transfer")
	if _, err := b.BuildFull(context.Background(), node); err != nil {
		t.Fatal(err)
	}
	var policies []model.NodeTrafficPolicy
	a.DB.Order("config_version, direction").Find(&policies)
	if len(policies) != 4 || policies[2].UserID != 9 || policies[2].Multiplier != 5 || policies[0].RuleHash == policies[2].RuleHash {
		t.Fatalf("new and historical policies not retained: %+v", policies)
	}
}

func TestConfigCompilerBoundsRetriesDuringContinuousChanges(t *testing.T) {
	a, node := newConfigSnapshotFixture(t)
	attempts := 0
	const callback = "test:continuous_edits"
	if err := a.DB.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if _, ordered := tx.Statement.Clauses["ORDER BY"]; tx.Statement.Table == "forward_rules" && ordered {
			attempts++
			a.BumpConfigVersion("continuous edit")
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer a.DB.Callback().Query().Remove(callback)
	if _, err := NewConfigBuilder(a).BuildFull(context.Background(), node); err == nil || attempts != 3 {
		t.Fatalf("retries=%d err=%v", attempts, err)
	}
	var count int64
	a.DB.Model(&model.NodeTrafficPolicy{}).Count(&count)
	if count != 0 {
		t.Fatalf("saved %d policies for unsent configurations", count)
	}
}
