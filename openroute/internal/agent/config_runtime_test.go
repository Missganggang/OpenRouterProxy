package agent

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"path/filepath"
	"testing"

	"github.com/openroute/openroute/internal/app"
	"github.com/openroute/openroute/internal/config"
	"github.com/openroute/openroute/internal/database"
	"github.com/openroute/openroute/internal/model"
	"github.com/openroute/openroute/internal/nodeproto"
	"go.uber.org/zap"
)

func TestConfigCompilerCompletePathAndPolicies(t *testing.T) {
	db, err := database.Open(database.Options{Path: "sqlite3://" + filepath.Join(t.TempDir(), "config.db"), MaxOpen: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.AutoMigrate(); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.SecretKey = "compiler-test-secret"
	a := app.New(cfg, db, zap.NewNop()).Init()
	t.Cleanup(a.Stop)
	nodes := []model.Node{{ID: 1, Name: "in", Token: "one", PublicIPv4: "127.0.0.1"}, {ID: 2, Name: "hop", Token: "two", PublicIPv4: "127.0.0.2"}, {ID: 3, Name: "exit", Token: "three", PublicIPv4: "127.0.0.3"}, {ID: 4, Name: "failover", Token: "four", PublicIPv4: "127.0.0.4"}}
	for i := range nodes {
		if err := db.Create(&nodes[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range []interface{}{
		&model.UserGroup{ID: 1, Name: "quota", SpeedLimit: 512, IPLimit: 2, TrafficLimit: 10000},
		&model.User{ID: 1, Username: "user", Token: "user", PasswordHash: "unused", Status: 1, GroupID: 1, ConnLimit: 7},
		&model.DeviceGroup{ID: 1, Name: "in", Type: "inbound", NodeIDs: model.FromAny([]uint64{1}), Config: model.FromAny(map[string]interface{}{"protocol": "tls"})},
		&model.DeviceGroup{ID: 2, Name: "hop", Type: "outbound", NodeIDs: model.FromAny([]uint64{2})},
		&model.DeviceGroup{ID: 3, Name: "exit", Type: "outbound", NodeIDs: model.FromAny([]uint64{3})},
		&model.DeviceGroup{ID: 4, Name: "fallback", Type: "outbound", NodeIDs: model.FromAny([]uint64{4})},
		&model.ForwardRule{ID: 1, Name: "chain", UserID: 1, Enable: true, InboundGroupID: 1, OutboundGroupID: 4, ChainGroups: model.FromAny([]uint64{2, 3}), ListenPort: 10001, Targets: model.FromAny([]model.Target{{Host: "example.com", Port: 443}})},
	} {
		if err := db.Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
	b := NewConfigBuilder(a)
	configs := make([]*ConfigResponse, 4)
	for i := range nodes {
		configs[i], err = b.BuildFull(context.Background(), &nodes[i])
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(configs[0].Rules) != 1 || len(configs[1].Rules) != 1 || len(configs[2].Rules) != 1 || len(configs[3].Rules) != 0 {
		t.Fatalf("wrong chain recipients")
	}
	for _, c := range configs[:3] {
		if len(c.DeviceGroupConfig) != 3 {
			t.Fatalf("missing chain peers: %v", c.DeviceGroupConfig)
		}
		if c.Rules[0].UserLimits.SpeedLimit != 512 || c.Rules[0].UserLimits.IPLimit != 2 || c.Rules[0].UserLimits.ConnLimit != 7 {
			t.Fatalf("user/group policy not compiled: %+v", c.Rules[0].UserLimits)
		}
		if c.Rules[0].Protocol != "tls" {
			t.Fatal("chain protocol dropped")
		}
	}
	if configs[0].Rules[0].TunnelToken != configs[2].Rules[0].TunnelToken || configs[0].Rules[0].TunnelToken == nodes[0].Token {
		t.Fatal("incorrect rule-scoped key")
	}
	cert, err := tls.X509KeyPair([]byte(configs[2].Listeners.TLSCertPEM), []byte(configs[2].Listeners.TLSKeyPEM))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(cert.Certificate[0])
	if configs[0].DeviceGroupConfig["3"].Peers[0].TLSPin != hex.EncodeToString(sum[:]) {
		t.Fatal("egress certificate pin mismatch")
	}
	if configs[2].Listeners.TLSKeyPEM == configs[0].Listeners.TLSKeyPEM {
		t.Fatal("nodes share private keys")
	}
	cAgain, err := NewConfigBuilder(a).BuildFull(context.Background(), &nodes[2])
	if err != nil {
		t.Fatal(err)
	}
	if nodeproto.ConfigHash(*cAgain) != nodeproto.ConfigHash(*configs[2]) {
		t.Fatal("unstable config/certificate across builds")
	}
	db.Model(&model.User{}).Where("id=1").UpdateColumn("status", 0)
	cAgain, err = b.BuildFull(context.Background(), &nodes[0])
	if err != nil {
		t.Fatal(err)
	}
	if !cAgain.Rules[0].UserLimits.Disabled {
		t.Fatal("disabled user still allowed")
	}
	if err := db.Model(&nodes[0]).Update("disabled", true).Error; err != nil {
		t.Fatal(err)
	}
	cAgain, err = b.BuildFull(context.Background(), &nodes[0])
	if err != nil {
		t.Fatal(err)
	}
	if !cAgain.NodeDisabled || cAgain.Rules[0].Enable {
		t.Fatal("disabled node still enabled")
	}
}
