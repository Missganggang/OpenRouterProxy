package app

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/openroute/openroute/internal/model"
	"github.com/openroute/openroute/internal/nodeproto"
)

func TestDelayedTrafficUsesDeliveredPolicyAndOriginalMonth(t *testing.T) {
	a := reportFixture(t)
	for _, value := range []interface{}{
		&model.User{ID: 2, Username: "new-owner", PasswordHash: "unused", Token: "new-owner-token"},
		&model.NodeTrafficPolicy{NodeID: 1, ConfigVersion: 7, RuleID: 1, Direction: "inbound", UserID: 1, Multiplier: 2},
	} {
		if err := a.DB.Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := a.DB.Model(&model.ForwardRule{}).Where("id = 1").Updates(map[string]interface{}{"user_id": 2, "inbound_multiplier": 9}).Error; err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 8, 31, 23, 0, 0, 0, time.UTC)
	report := nodeproto.ReportRequest{BatchID: "delayed-old-config", ConfigVersion: 7, Timestamp: when.Unix(), Stats: &nodeproto.ReportStats{RuleTraffic: []nodeproto.RuleTrafficItem{{RuleID: 1, Direction: "inbound", TrafficIn: 100}}}}
	if err := a.Traffic.RecordNodeReport(context.Background(), 1, report); err != nil {
		t.Fatal(err)
	}
	var original, current model.User
	a.DB.First(&original, 1)
	a.DB.First(&current, 2)
	if original.TrafficUsed != 200 || current.TrafficUsed != 0 {
		t.Fatalf("charged incorrect owner: old=%d new=%d", original.TrafficUsed, current.TrafficUsed)
	}
	var log model.TrafficLog
	if err := a.DB.Where("hour = 23").First(&log).Error; err != nil {
		t.Fatal(err)
	}
	if log.Date != "2026-08-31" || log.UserID != 1 || log.Bytes != 200 {
		t.Fatalf("lost historical bucket: %+v", log)
	}
	report.Timestamp++
	if err := a.Traffic.RecordNodeReport(context.Background(), 1, report); err == nil {
		t.Fatal("same receipt accepted altered timestamp")
	}
	report.BatchID, report.ConfigVersion = "unknown-config", 8
	if err := a.Traffic.RecordNodeReport(context.Background(), 1, report); err == nil {
		t.Fatal("unknown configuration charged using current owner")
	}
	// Deleting the rule does not erase the attribution of bytes already forwarded.
	a.DB.Delete(&model.ForwardRule{}, 1)
	report.BatchID, report.ConfigVersion, report.Timestamp = "deleted-rule-tail", 7, when.Unix()
	if err := a.Traffic.RecordNodeReport(context.Background(), 1, report); err != nil {
		t.Fatal(err)
	}
	a.DB.First(&original, 1)
	if original.TrafficUsed != 400 {
		t.Fatalf("lost deleted rule tail: %d", original.TrafficUsed)
	}
}

func TestTrafficOverflowRollsBackReceiptAndCounters(t *testing.T) {
	for _, target := range []string{"batch", "user", "rule", "log"} {
		t.Run(target, func(t *testing.T) {
			a := reportFixture(t)
			ctx := context.Background()
			report := nodeproto.ReportRequest{BatchID: "overflow", Timestamp: time.Now().Unix(), Stats: &nodeproto.ReportStats{RuleTraffic: []nodeproto.RuleTrafficItem{{RuleID: 1, Direction: "inbound", TrafficIn: 10}}}}
			switch target {
			case "batch":
				a.DB.Model(&model.ForwardRule{}).Where("id = 1").UpdateColumn("inbound_multiplier", 1000)
				report.Stats.RuleTraffic = make([]nodeproto.RuleTrafficItem, 10)
				for i := range report.Stats.RuleTraffic {
					report.Stats.RuleTraffic[i] = nodeproto.RuleTrafficItem{RuleID: 1, Direction: "inbound", TrafficIn: 1 << 50}
				}
			case "user":
				a.DB.Model(&model.User{}).Where("id = 1").UpdateColumn("traffic_used", int64(math.MaxInt64-1))
			case "rule":
				a.DB.Model(&model.ForwardRule{}).Where("id = 1").UpdateColumn("traffic_out", int64(math.MaxInt64-1))
			case "log":
				if err := a.DB.Create(&model.TrafficLog{Date: time.Now().UTC().Format("2006-01-02"), Hour: -1, RuleID: 1, UserID: 1, NodeID: 1, Direction: model.DirectionIn, Bytes: math.MaxInt64 - 1}).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := a.Traffic.RecordNodeReport(ctx, 1, report); err == nil {
				t.Fatal("overflow was accepted")
			}
			var count int64
			a.DB.Model(&model.NodeReportBatch{}).Count(&count)
			if count != 0 {
				t.Fatal("failed batch receipt was committed")
			}
			var user model.User
			a.DB.First(&user, 1)
			if user.TrafficUsed < 0 {
				t.Fatal("quota wrapped negative")
			}
		})
	}
}
