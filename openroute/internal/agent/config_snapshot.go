package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/openroute/openroute/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Save immutable billing ownership before returning a configuration. A delayed
// report must retain the owner and multiplier that were active on its node.
func (b *ConfigBuilder) saveTrafficPolicies(ctx context.Context, config *ConfigResponse, hashes map[uint64]string) error {
	return b.app.DB.Tx(ctx, func(tx *gorm.DB) error {
		var revision model.ConfigRevision
		if err := tx.Where("id = ?", 1).Find(&revision).Error; err != nil {
			return err
		}
		if revision.Revision != config.ConfigVersion {
			return fmt.Errorf("配置版本尚未持久化，请重试获取节点配置")
		}
		for _, rule := range config.Rules {
			direction, multiplier := "inbound", rule.InboundMultiplier
			if rule.IsOutbound {
				direction, multiplier = "outbound", rule.OutboundMultiplier
			}
			policy := model.NodeTrafficPolicy{NodeID: config.NodeID, ConfigVersion: config.ConfigVersion, RuleID: rule.RuleID,
				Direction: direction, UserID: rule.UserID, Multiplier: multiplier, RuleHash: hashes[rule.RuleID], CreatedAt: time.Now().UTC()}
			if policy.RuleHash == "" {
				return fmt.Errorf("规则 %d 配置无效", rule.RuleID)
			}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&policy).Error; err != nil {
				return err
			}
			var saved model.NodeTrafficPolicy
			if err := tx.Where("node_id = ? AND config_version = ? AND rule_id = ? AND direction = ?", policy.NodeID, policy.ConfigVersion, policy.RuleID, policy.Direction).First(&saved).Error; err != nil {
				return err
			}
			if saved.UserID != policy.UserID || saved.Multiplier != policy.Multiplier || saved.RuleHash != policy.RuleHash {
				return fmt.Errorf("规则 %d 配置已变化，等待新版本后重试", rule.RuleID)
			}
		}
		return nil
	})
}
