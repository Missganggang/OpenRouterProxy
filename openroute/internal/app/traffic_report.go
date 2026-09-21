package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/openroute/openroute/internal/model"
	"github.com/openroute/openroute/internal/nodeproto"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// RecordNodeReport atomically records both the receipt and all traffic aggregates.
// A retried batch cannot double charge, even if the panel restarted after commit.
func (s *TrafficService) RecordNodeReport(ctx context.Context, nodeID uint64, report nodeproto.ReportRequest) error {
	if report.Stats == nil || len(report.Stats.RuleTraffic) == 0 {
		return nil
	}
	if strings.TrimSpace(report.BatchID) == "" || len(report.BatchID) > 128 {
		return errors.New("流量上报需要有效的 batch_id，请升级节点客户端")
	}
	if len(report.Stats.RuleTraffic) > 10000 {
		return errors.New("单次流量报告过大")
	}
	data, err := json.Marshal(struct {
		Version   int64
		Timestamp int64
		Traffic   []nodeproto.RuleTrafficItem
	}{report.ConfigVersion, report.Timestamp, report.Stats.RuleTraffic})
	if err != nil {
		return err
	}
	hash := sha256.Sum256(data)
	digest := hex.EncodeToString(hash[:])
	now := time.Now().UTC()
	when := time.Unix(report.Timestamp, 0).UTC()
	if report.Timestamp == 0 || when.After(now.Add(5*time.Minute)) {
		when = now
	}
	quotaChanged := false
	err = s.app.DB.Tx(ctx, func(tx *gorm.DB) error {
		receipt := model.NodeReportBatch{NodeID: nodeID, BatchID: report.BatchID, PayloadHash: digest, CreatedAt: now}
		created := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&receipt)
		if created.Error != nil {
			return created.Error
		}
		if created.RowsAffected == 0 {
			var existing model.NodeReportBatch
			if err := tx.Where("node_id = ? AND batch_id = ?", nodeID, report.BatchID).First(&existing).Error; err != nil {
				return err
			}
			if existing.PayloadHash != digest {
				return errors.New("同一流量批次不能修改内容")
			}
			return nil
		}
		groups := map[uint64]*model.DeviceGroup{}
		if report.ConfigVersion == 0 {
			var groupRows []model.DeviceGroup
			if err := tx.Find(&groupRows).Error; err != nil {
				return err
			}
			for i := range groupRows {
				groups[groupRows[i].ID] = &groupRows[i]
			}
		}
		samples := make([]TrafficSample, 0, len(report.Stats.RuleTraffic))
		users := map[uint64]bool{}
		for _, item := range report.Stats.RuleTraffic {
			if item.TrafficIn < 0 || item.TrafficOut < 0 || item.TrafficIn > 1<<50 || item.TrafficOut > 1<<50 {
				return errors.New("无效的流量增量")
			}
			role := item.Direction
			if role == "" {
				role = "inbound"
			}
			if role != "inbound" && role != "outbound" {
				return fmt.Errorf("无效的流量方向 %q", item.Direction)
			}
			var policy model.NodeTrafficPolicy
			err := tx.Where("node_id = ? AND config_version = ? AND rule_id = ? AND direction = ?", nodeID, report.ConfigVersion, item.RuleID, role).First(&policy).Error
			if err != nil {
				if !errors.Is(err, gorm.ErrRecordNotFound) {
					return err
				}
				if report.ConfigVersion != 0 {
					return errors.New("流量报告缺少对应的已下发配置记录")
				}
				// Compatibility for the initial, unversioned configuration only.
				var rule model.ForwardRule
				if err := tx.First(&rule, item.RuleID).Error; err != nil {
					if errors.Is(err, gorm.ErrRecordNotFound) {
						continue
					}
					return err
				}
				in, out := model.RuleNodeRoles(&rule, nodeID, groups)
				if (role == "inbound" && !in) || (role == "outbound" && !out) {
					continue
				}
				policy.UserID, policy.Multiplier = rule.UserID, rule.InboundMultiplier
				if role == "outbound" {
					policy.Multiplier = rule.OutboundMultiplier
				}
			}
			direction, multiplier := model.DirectionIn, policy.Multiplier
			if role == "outbound" {
				direction = model.DirectionOut
			}
			raw := item.TrafficIn + item.TrafficOut
			if math.IsNaN(multiplier) || math.IsInf(multiplier, 0) || multiplier < 0 || multiplier > 1000 {
				return errors.New("无效的流量倍率")
			}
			samples = append(samples, TrafficSample{Hour: when.Hour(), RuleID: item.RuleID, UserID: policy.UserID, NodeID: nodeID,
				Direction: direction, RawBytes: raw, Bytes: int64(math.Round(float64(raw) * multiplier)), ExactBytes: true})
			if policy.UserID != 0 {
				users[policy.UserID] = true
			}
		}
		// Detect a quota crossing so every ingress gets the aggregate promptly.
		before := map[uint64]bool{}
		for id := range users {
			exhausted, err := userQuotaExhausted(tx, id)
			if err != nil {
				return err
			}
			before[id] = exhausted
		}
		if err := s.recordTraffic(ctx, tx, samples, when); err != nil {
			return err
		}
		for id := range users {
			exhausted, err := userQuotaExhausted(tx, id)
			if err != nil {
				return err
			}
			if exhausted != before[id] {
				quotaChanged = true
			}
		}
		return nil
	})
	if err == nil && quotaChanged {
		s.app.BumpConfigVersion("用户流量额度耗尽")
	}
	return err
}

func userQuotaExhausted(tx *gorm.DB, id uint64) (bool, error) {
	var user model.User
	if err := tx.First(&user, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	limit := user.TrafficLimit
	if limit == 0 && user.GroupID != 0 {
		var group model.UserGroup
		if err := tx.First(&group, user.GroupID).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return false, err
		}
		limit = group.TrafficLimit
	}
	return limit > 0 && user.TrafficUsed >= limit, nil
}
