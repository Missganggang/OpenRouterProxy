package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/openroute/openroute/internal/model"
	"github.com/openroute/openroute/internal/util"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// RecordNodeResult tracks each node independently. A success from one node may
// never hide another node's failure or an unacknowledged configuration.
func (s *SyncService) RecordNodeResult(ctx context.Context, ruleID, nodeID uint64, version int64, ok bool, message string) error {
	_, err := s.RecordNodeResultAccepted(ctx, ruleID, nodeID, version, ok, message)
	return err
}

// RecordNodeResultAccepted also reports whether this ACK was committed. A nil
// error alone can mean a stale or unrelated acknowledgement was ignored.
func (s *SyncService) RecordNodeResultAccepted(ctx context.Context, ruleID, nodeID uint64, version int64, ok bool, message string) (bool, error) {
	// Keep the version stable through commit. Lock order is always version then
	// database, matching BumpConfigVersion; taking this lock inside a transaction
	// could deadlock with a bump waiting for that transaction's database locks.
	s.app.versionMu.RLock()
	if version != s.app.configVersion {
		s.app.versionMu.RUnlock()
		return false, nil
	}
	var oldStatus, status, aggregateError string
	err := s.app.DB.Tx(ctx, func(tx *gorm.DB) error {
		var revision model.ConfigRevision
		if err := tx.Where("id = ?", 1).Find(&revision).Error; err != nil {
			return err
		}
		if revision.Revision != version {
			return nil
		}
		var rule model.ForwardRule
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&rule, ruleID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		// A rule edit commits before the global version is bumped. Validate the
		// delivered configuration as well, so an old ACK in that interval cannot
		// mark the just-edited rule successful (or replace its pending status).
		var policies []model.NodeTrafficPolicy
		if err := tx.Where("node_id = ? AND config_version = ? AND rule_id = ?", nodeID, version, ruleID).Find(&policies).Error; err != nil {
			return err
		}
		if len(policies) == 0 && version > 0 {
			return nil
		}
		hash := model.RuleConfigHash(rule)
		for _, policy := range policies {
			if hash == "" || policy.RuleHash != hash {
				return nil
			}
		}
		var list []model.DeviceGroup
		if err := tx.Find(&list).Error; err != nil {
			return err
		}
		groups := map[uint64]*model.DeviceGroup{}
		for i := range list {
			groups[list[i].ID] = &list[i]
		}
		in, out := model.RuleNodeRoles(&rule, nodeID, groups)
		if !in && !out {
			return nil
		}
		oldStatus = rule.SyncStatus
		row := model.NodeRuleSync{RuleID: ruleID, NodeID: nodeID, ConfigVersion: version, Status: model.SyncNormal, UpdatedAt: time.Now().UTC()}
		if !ok {
			row.Status = model.SyncFailed
			row.Error = util.Truncate(message, 512)
		}
		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "node_id"}, {Name: "rule_id"}}, DoUpdates: clause.AssignmentColumns([]string{"config_version", "status", "error", "updated_at"})}).Create(&row).Error; err != nil {
			return err
		}
		expected := map[uint64]bool{}
		for id := range model.RuleGroupIDs(&rule, groups) {
			if g := groups[id]; g != nil {
				for _, nid := range g.NodeIDs.AsUint64Slice() {
					expected[nid] = true
				}
			}
		}
		var disabled []uint64
		if err := tx.Model(&model.Node{}).Where("disabled = ?", true).Pluck("id", &disabled).Error; err != nil {
			return err
		}
		for _, id := range disabled {
			delete(expected, id)
		}
		var results []model.NodeRuleSync
		if err := tx.Where("rule_id = ? AND config_version = ?", ruleID, version).Find(&results).Error; err != nil {
			return err
		}
		status = model.SyncNormal
		failures := []string{}
		for _, result := range results {
			if !expected[result.NodeID] {
				continue
			}
			delete(expected, result.NodeID)
			if result.Status == model.SyncFailed {
				failures = append(failures, fmt.Sprintf("节点 %d：%s", result.NodeID, result.Error))
			}
		}
		if len(expected) != 0 {
			status = model.SyncSyncing
		}
		if len(failures) != 0 {
			status = model.SyncFailed
			sort.Strings(failures)
			aggregateError = util.Truncate(strings.Join(failures, "；"), 512)
		}
		updates := map[string]interface{}{"sync_status": status, "sync_error": aggregateError, "updated_at": time.Now().UTC()}
		if status == model.SyncNormal {
			updates["synced_at"] = time.Now().UTC()
		}
		return tx.Model(&model.ForwardRule{}).Where("id = ?", ruleID).Updates(updates).Error
	})
	s.app.versionMu.RUnlock()
	if err != nil || status == "" {
		return false, err
	}
	s.app.Hub().Broadcast(Event{Type: "rule_sync", Data: map[string]interface{}{"rule_id": ruleID, "node_id": nodeID, "config_version": version, "sync_status": status, "error": aggregateError, "ok": status == model.SyncNormal}})
	if status != oldStatus && s.app.Alert != nil {
		event := ""
		if status == model.SyncNormal {
			event = EventRuleSyncOK
		} else if status == model.SyncFailed {
			event = EventRuleSyncFailed
		}
		if event != "" {
			s.app.Alert.NotifyEvent(ctx, event, map[string]interface{}{"rule_id": ruleID, "node_id": nodeID, "error": aggregateError})
		}
	}
	return true, nil
}
