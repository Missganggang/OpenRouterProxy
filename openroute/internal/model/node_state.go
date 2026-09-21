package model

import "time"

type ConfigRevision struct {
	ID       uint `gorm:"primaryKey;autoIncrement:false"`
	Revision int64
}

// NodeReportBatch is committed in the same transaction as its traffic counters.
// Keeping the receipt makes a lost HTTP response safe to retry after a restart.
type NodeReportBatch struct {
	NodeID      uint64 `gorm:"primaryKey;autoIncrement:false"`
	BatchID     string `gorm:"primaryKey;size:128"`
	PayloadHash string `gorm:"size:64;not null"`
	CreatedAt   time.Time
}

// NodeTrafficPolicy preserves the owner and multiplier of a configuration that
// was actually sent, so delayed reports remain correct after rule edits.
type NodeTrafficPolicy struct {
	NodeID        uint64 `gorm:"primaryKey;autoIncrement:false"`
	ConfigVersion int64  `gorm:"primaryKey;autoIncrement:false"`
	RuleID        uint64 `gorm:"primaryKey;autoIncrement:false"`
	Direction     string `gorm:"primaryKey;size:16"`
	UserID        uint64
	Multiplier    float64
	RuleHash      string `gorm:"size:64"`
	CreatedAt     time.Time
}

type NodeRuleSync struct {
	NodeID        uint64    `gorm:"primaryKey;autoIncrement:false" json:"node_id"`
	RuleID        uint64    `gorm:"primaryKey;autoIncrement:false" json:"rule_id"`
	ConfigVersion int64     `json:"config_version"`
	Status        string    `gorm:"size:16" json:"status"`
	Error         string    `gorm:"size:512" json:"error"`
	UpdatedAt     time.Time `json:"updated_at"`
}
