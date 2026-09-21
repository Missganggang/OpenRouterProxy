package app

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openroute/openroute/internal/model"
	"github.com/openroute/openroute/internal/util"
)

func alertCertificate(t *testing.T, expiry time.Time) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"}, NotBefore: expiry.Add(-365 * 24 * time.Hour), NotAfter: expiry}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestAlertCertificateExpiryAndRecovery(t *testing.T) {
	a := newLimitTestApp(t)
	a.hub = newHub(a.Log)
	a.Setting = NewSettingService(a)
	s := NewAlertService(a)
	now := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	a.Config.TLSCert = filepath.Join(t.TempDir(), "panel.pem")
	if err := os.WriteFile(a.Config.TLSCert, []byte(alertCertificate(t, now.Add(20*24*time.Hour))), 0600); err != nil {
		t.Fatal(err)
	}
	r := model.ForwardRule{Name: "custom", Options: model.FromAny(map[string]any{"tls": map[string]any{"cert": strings.Split(alertCertificate(t, now.Add(-time.Hour)), "\n")}})}
	if err := a.DB.Create(&r).Error; err != nil {
		t.Fatal(err)
	}
	rule := model.AlertRule{Name: "certificates", Type: model.AlertCertExpire, Threshold: 30, Enabled: true}
	if err := a.DB.Create(&rule).Error; err != nil {
		t.Fatal(err)
	}
	conds, err := s.evalCertExpire(context.Background(), &rule)
	if err != nil || len(conds) != 2 || !conds[0].Firing || !conds[1].Firing || conds[1].Level != model.AlertLevelCritical {
		t.Fatalf("expiry conditions: %+v, %v", conds, err)
	}
	if n, err := s.Evaluate(context.Background()); err != nil || n != 2 {
		t.Fatalf("initial immediate alerts: %d, %v", n, err)
	}
	if err := os.WriteFile(a.Config.TLSCert, []byte(alertCertificate(t, now.Add(90*24*time.Hour))), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.DB.Model(&r).Update("options", model.FromAny(map[string]any{"tls": map[string]any{"cert": alertCertificate(t, now.Add(90*24*time.Hour))}})).Error; err != nil {
		t.Fatal(err)
	}
	// A fresh service must recover persisted alerts after a panel restart.
	s = NewAlertService(a)
	s.now = func() time.Time { return now }
	if _, err := s.Evaluate(context.Background()); err != nil {
		t.Fatal(err)
	}
	var active int64
	a.DB.Model(&model.AlertHistory{}).Where("resolved = ?", false).Count(&active)
	if active != 0 {
		t.Fatalf("renewed certificates leave %d active alerts", active)
	}
	if err := os.WriteFile(a.Config.TLSCert, []byte("invalid certificate"), 0600); err != nil {
		t.Fatal(err)
	}
	conds, err = s.evalCertExpire(context.Background(), &rule)
	if err != nil || !conds[0].Firing || conds[0].Level != model.AlertLevelCritical {
		t.Fatalf("malformed certificate not reported: %+v, %v", conds, err)
	}
}

func TestAlertTrafficQuotaIndependentOfSiteShare(t *testing.T) {
	a := newLimitTestApp(t)
	s := NewAlertService(a)
	ctx := context.Background()
	rules := []model.ForwardRule{{Name: "limited", TrafficIn: 450, TrafficOut: 450}, {Name: "unrelated", TrafficIn: 1000000}}
	if err := a.DB.Create(&rules).Error; err != nil {
		t.Fatal(err)
	}
	rule := model.AlertRule{ID: 1, TargetID: rules[0].ID, Threshold: 80, TrafficLimit: 1000}
	conds, err := s.evalRuleTrafficPct(ctx, &rule)
	if err != nil || len(conds) != 1 || !conds[0].Firing {
		t.Fatalf("rule quota: %+v %v", conds, err)
	}
	nodes := []model.Node{{Name: "limited", Token: "one"}, {Name: "empty", Token: "two"}}
	if err := a.DB.Create(&nodes).Error; err != nil {
		t.Fatal(err)
	}
	logs := []model.TrafficLog{{NodeID: nodes[0].ID, RuleID: rules[0].ID, Date: "2026-09-20", Hour: model.HourDaily, Bytes: 450}, {NodeID: nodes[0].ID, RuleID: rules[0].ID, Date: "2026-09-21", Hour: model.HourDaily, Bytes: 450}, {NodeID: nodes[0].ID, RuleID: rules[0].ID, Date: "2026-09-21", Hour: 1, Bytes: 900}}
	if err := a.DB.Create(&logs).Error; err != nil {
		t.Fatal(err)
	}
	rule.TargetID = 0
	conds, err = s.evalNodeTrafficPct(ctx, &rule)
	if err != nil || len(conds) != 2 || !conds[0].Firing || conds[1].Firing || strings.Contains(conds[0].Title, "180") {
		t.Fatalf("node cumulative quota: %+v %v", conds, err)
	}
}

func TestAlertUserTrafficInheritsGroupQuota(t *testing.T) {
	a := newLimitTestApp(t)
	s := NewAlertService(a)
	group := model.UserGroup{Name: "quota-group", TrafficLimit: 1000}
	if err := a.DB.Create(&group).Error; err != nil {
		t.Fatal(err)
	}
	users := []model.User{
		{Username: "inherits", Token: "inherits", GroupID: group.ID, TrafficUsed: 900},
		{Username: "overrides", Token: "overrides", GroupID: group.ID, TrafficUsed: 900, TrafficLimit: 2000},
	}
	if err := a.DB.Create(&users).Error; err != nil {
		t.Fatal(err)
	}
	rule := model.AlertRule{ID: 1, Type: model.AlertUserTrafficPct, Threshold: 80}
	conds, err := s.evalUserTrafficPct(context.Background(), &rule)
	if err != nil || len(conds) != 2 || !conds[0].Firing || conds[1].Firing {
		t.Fatalf("inherited quota vs user override: %+v, %v", conds, err)
	}
	rule.TargetID = users[0].ID
	if err := a.DB.Model(&group).Update("traffic_limit", 0).Error; err != nil {
		t.Fatal(err)
	}
	conds, err = s.evalUserTrafficPct(context.Background(), &rule)
	if err != nil || len(conds) != 1 || conds[0].Firing || conds[0].ResourceID != users[0].ID {
		t.Fatalf("removing inherited quota must allow recovery: %+v, %v", conds, err)
	}
}

func TestAlertDebounceSilenceAndRestart(t *testing.T) {
	a := newLimitTestApp(t)
	a.hub = newHub(a.Log)
	a.Setting = NewSettingService(a)
	s := NewAlertService(a)
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	notices := 0
	s.smtpSend = func(string, int, string, string, []string, []byte) error { notices++; return nil }
	rule := model.AlertRule{Name: "cpu", Type: model.AlertNodeCPU, Duration: 10, SilenceFor: 60, Channels: model.FromAny([]model.Channel{{Type: "email", Host: "localhost", Port: 25, User: "sender@example.test", To: "recipient@example.test"}})}
	if err := a.DB.Create(&rule).Error; err != nil {
		t.Fatal(err)
	}
	cond := alertCondition{Key: condKey(rule.ID, "node", 1), Firing: true, Resource: "node", ResourceID: 1, Title: "High CPU", Level: model.AlertLevelWarning}
	ctx := context.Background()
	if n, err := s.apply(ctx, &rule, cond); n != 0 || err != nil {
		t.Fatalf("debounce: %d %v", n, err)
	}
	now = now.Add(10 * time.Second)
	if n, err := s.apply(ctx, &rule, cond); n != 1 || err != nil {
		t.Fatalf("fire: %d %v", n, err)
	}
	if notices != 1 {
		t.Fatalf("first notification count %d", notices)
	}
	now = now.Add(60 * time.Second)
	if _, err := s.apply(ctx, &rule, cond); err != nil {
		t.Fatal(err)
	}
	if notices != 2 {
		t.Fatalf("repeat notification count %d", notices)
	}
	s.inflight = map[string]uint64{}
	now = now.Add(time.Second)
	if n, err := s.apply(ctx, &rule, cond); n != 0 || err != nil {
		t.Fatalf("restart: %d %v", n, err)
	}
	if notices != 2 {
		t.Fatalf("silence not persisted: %d", notices)
	}
	cond.Firing = false
	cond.Title = ""
	if _, err := s.apply(ctx, &rule, cond); err != nil {
		t.Fatal(err)
	}
	if notices != 3 {
		t.Fatalf("recovery not sent: %d", notices)
	}
	var count int64
	a.DB.Model(&model.AlertHistory{}).Count(&count)
	if count != 1 {
		t.Fatalf("duplicate history: %d", count)
	}
}

func TestAlertUpdateNameAndQuota(t *testing.T) {
	a := newLimitTestApp(t)
	s := NewAlertService(a)
	ctx := context.Background()
	in := AlertRuleInput{Name: "quota", Type: model.AlertRuleTrafficPct, Threshold: 80, TrafficLimit: 1000}
	r, err := s.CreateRule(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	in.TrafficLimit = 2000
	updated, err := s.UpdateRule(ctx, r.ID, in)
	if err != nil || updated.TrafficLimit != 2000 {
		t.Fatalf("update unchanged name: %+v %v", updated, err)
	}
	in.TrafficLimit = 0
	if _, err := s.UpdateRule(ctx, r.ID, in); err == nil {
		t.Fatal("accepted zero quota")
	}
}

func TestWebhookManualTestAndCancellation(t *testing.T) {
	a := newLimitTestApp(t)
	s := NewWebhookService(a)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get(HeaderWebhookEvent) != EventWebhookTest {
			t.Error("incorrect event")
		}
		if r.Header.Get(HeaderWebhookSignature) != util.HMACSHA256Hex(a.Config.SecretKey, string(body)) {
			t.Error("incorrect signature")
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	if err := s.SendSync(context.Background(), server.URL, EventWebhookTest, map[string]any{"message": "test"}); err == nil {
		t.Fatal("expected 503 failure")
	}
	if requests != 1 {
		t.Fatalf("manual test retried %d times", requests)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.SendSync(ctx, server.URL, EventWebhookTest, nil); err == nil {
		t.Fatal("ignored cancellation")
	}
}
