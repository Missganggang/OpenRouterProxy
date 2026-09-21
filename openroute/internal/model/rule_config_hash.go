package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// RuleConfigHash binds an acknowledgement to the rule configuration that was
// actually sent. Runtime counters and status changes do not change that identity.
func RuleConfigHash(rule ForwardRule) string {
	rule.SyncStatus, rule.SyncError, rule.SyncedAt = "", "", nil
	rule.TrafficIn, rule.TrafficOut = 0, 0
	rule.CreatedAt, rule.UpdatedAt = time.Time{}, time.Time{}
	data, err := json.Marshal(rule)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
