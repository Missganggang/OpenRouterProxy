package model

import "time"

// 系统设置的已知键名。
//
// 这些键在首次初始化时写入默认值，前端「设置」页直接读取。
const (
	SettingSiteName       = "site_name"       // 站点名称
	SettingTheme          = "theme"           // 主题：classic | transparent
	SettingLogo           = "logo"            // Logo 地址
	SettingFavicon        = "favicon"         // favicon 地址
	SettingAnnouncement   = "announcement"    // 公告
	SettingDefaultSpeed   = "default_speed"   // 默认限速
	SettingAlertThreshold = "alert_threshold" // 告警阈值
	SettingWebhookURL     = "webhook_url"     // Webhook 地址
	SettingAPIEnabled     = "api_enabled"     // API 开关
	SettingOpenAPIDocs    = "openapi_docs"    // 是否开启 /api/docs
)

// SystemSetting 是键值配置表。
type SystemSetting struct {
	Key       string    `gorm:"primaryKey;size:64" json:"key"`
	Value     JSON      `gorm:"type:text" json:"value"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName 指定表名。
func (SystemSetting) TableName() string { return "system_settings" }

// AuditLog 是操作审计表，记录每次变更的 before/after 快照。
type AuditLog struct {
	ID         uint64    `gorm:"primaryKey" json:"id"`
	UserID     uint64    `gorm:"index" json:"user_id"`
	Username   string    `gorm:"size:64" json:"username"`
	Action     string    `gorm:"size:64;index" json:"action"`   // create | update | delete | login | sync | migrate ...
	Resource   string    `gorm:"size:64;index" json:"resource"` // node | rule | device_group | user ...
	ResourceID uint64    `gorm:"index" json:"resource_id"`
	Before     JSON      `gorm:"type:text" json:"before"` // 变更前快照
	After      JSON      `gorm:"type:text" json:"after"`  // 变更后快照
	IP         string    `gorm:"size:64" json:"ip"`
	UserAgent  string    `gorm:"size:255" json:"user_agent"`
	Result     string    `gorm:"size:16" json:"result"` // success | failed
	Message    string    `gorm:"size:512" json:"message"`
	CreatedAt  time.Time `gorm:"index" json:"created_at"`
}

// TableName 指定表名。
func (AuditLog) TableName() string { return "audit_logs" }

// 审计动作常量。
const (
	ActionCreate   = "create"
	ActionUpdate   = "update"
	ActionDelete   = "delete"
	ActionLogin    = "login"
	ActionLogout   = "logout"
	ActionSync     = "sync"
	ActionMigrate  = "migrate"
	ActionBackup   = "backup"
	ActionRestore  = "restore"
	ActionRollback = "rollback"
	ActionExec     = "exec"
	ActionImport   = "import"
	ActionExport   = "export"
)

// 审计结果常量。
const (
	ResultSuccess = "success"
	ResultFailed  = "failed"
)

// APIToken 是对外 API 令牌表。
//
// 明文 Token 仅在创建时返回一次，库中只存哈希。
type APIToken struct {
	ID          uint64     `gorm:"primaryKey" json:"id"`
	Name        string     `gorm:"size:64;not null" json:"name"`
	Token       string     `gorm:"size:128;uniqueIndex;not null" json:"token"` // 明文仅创建时返回一次
	TokenHash   string     `gorm:"size:128;index" json:"-"`                    // 库中只存哈希
	Scopes      JSON       `gorm:"type:text" json:"scopes"`                    // ["node:read","rule:write",...]
	IPWhitelist JSON       `gorm:"type:text" json:"ip_whitelist"`              // ["1.2.3.0/24"]
	ExpireAt    *time.Time `json:"expire_at"`
	LastUsedAt  *time.Time `json:"last_used_at"`
	Enabled     bool       `gorm:"default:true" json:"enabled"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// TableName 指定表名。
func (APIToken) TableName() string { return "api_tokens" }

// ScopeList 解析权限范围列表。
func (t *APIToken) ScopeList() []string { return t.Scopes.AsSlice() }

// HasScope 判断令牌是否具备指定权限。
//
// 支持通配 `*` 与「资源级通配」（如 `rule:*` 覆盖 `rule:read` / `rule:write`）。
func (t *APIToken) HasScope(need string) bool {
	for _, s := range t.ScopeList() {
		if s == "*" || s == need {
			return true
		}
		// rule:* 覆盖 rule:read
		if len(s) > 2 && s[len(s)-1] == '*' && s[len(s)-2] == ':' {
			if len(need) > len(s)-1 && need[:len(s)-1] == s[:len(s)-1] {
				return true
			}
		}
	}
	return false
}

// Expired 判断令牌是否已过期。
func (t *APIToken) Expired(now time.Time) bool {
	return t.ExpireAt != nil && now.After(*t.ExpireAt)
}

// ConfigSnapshot 是配置快照表，用于一键回滚。
type ConfigSnapshot struct {
	ID       uint64 `gorm:"primaryKey" json:"id"`
	Name     string `gorm:"size:128" json:"name"`
	Reason   string `gorm:"size:255" json:"reason"`   // manual | auto_before_sync | auto_daily | before_migrate
	Payload  JSON   `gorm:"type:text" json:"payload"` // 全量配置：device_groups + forward_rules + nodes 关联
	Checksum string `gorm:"size:64" json:"checksum"`
	// 快照条目统计，用于列表页快速展示，避免解析 Payload
	RuleCount  int       `gorm:"default:0" json:"rule_count"`
	GroupCount int       `gorm:"default:0" json:"group_count"`
	NodeCount  int       `gorm:"default:0" json:"node_count"`
	CreatedAt  time.Time `gorm:"index" json:"created_at"`
}

// TableName 指定表名。
func (ConfigSnapshot) TableName() string { return "config_snapshots" }

// 快照生成原因常量。
const (
	SnapshotReasonManual         = "manual"
	SnapshotReasonAutoBeforeSync = "auto_before_sync"
	SnapshotReasonAutoDaily      = "auto_daily"
	SnapshotReasonBeforeMigrate  = "before_migrate"
)

// AlertRule 是告警规则表（规格书 6.12 新增能力）。
type AlertRule struct {
	ID   uint64 `gorm:"primaryKey" json:"id"`
	Name string `gorm:"size:128" json:"name"`
	// 类型：node_offline | node_cpu | node_mem | node_disk | rule_sync_failed
	//      | user_traffic_pct | rule_traffic_pct | node_traffic_pct | cert_expire
	Type         string  `gorm:"size:32;index" json:"type"`
	TargetID     uint64  `gorm:"index" json:"target_id"`         // 关联节点/用户/规则 ID，0 = 全局
	Threshold    float64 `gorm:"default:0" json:"threshold"`     // 阈值（百分比 / 秒数）
	TrafficLimit int64   `gorm:"default:0" json:"traffic_limit"` // 规则/节点累计计费流量的告警基准（字节），不限制转发
	Duration     int     `gorm:"default:0" json:"duration"`      // 持续时长（秒）后才触发，防抖
	// 通知渠道
	Channels    JSON       `gorm:"type:text" json:"channels"`      // [{"type":"webhook","url":"..."},...]
	SilenceFor  int        `gorm:"default:300" json:"silence_for"` // 静默期（秒），避免重复轰炸
	Enabled     bool       `gorm:"default:true" json:"enabled"`
	LastFiredAt *time.Time `json:"last_fired_at"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// TableName 指定表名。
func (AlertRule) TableName() string { return "alert_rules" }

// 告警类型常量。
const (
	AlertNodeOffline    = "node_offline"
	AlertNodeCPU        = "node_cpu"
	AlertNodeMem        = "node_mem"
	AlertNodeDisk       = "node_disk"
	AlertRuleSyncFailed = "rule_sync_failed"
	AlertUserTrafficPct = "user_traffic_pct"
	AlertRuleTrafficPct = "rule_traffic_pct"
	AlertNodeTrafficPct = "node_traffic_pct"
	AlertCertExpire     = "cert_expire"
)

// 告警级别常量。
const (
	AlertLevelInfo     = "info"
	AlertLevelWarning  = "warning"
	AlertLevelCritical = "critical"
)

// Channel 是告警通知渠道配置。
type Channel struct {
	Type  string `json:"type"`            // webhook | telegram | email
	URL   string `json:"url,omitempty"`   // webhook 地址
	Token string `json:"token,omitempty"` // telegram bot token
	Chat  string `json:"chat,omitempty"`  // telegram chat id
	Host  string `json:"host,omitempty"`  // SMTP 主机
	Port  int    `json:"port,omitempty"`  // SMTP 端口
	User  string `json:"user,omitempty"`  // SMTP 用户名
	Pass  string `json:"pass,omitempty"`  // SMTP 密码
	To    string `json:"to,omitempty"`    // 收件人
}

// ChannelList 解析通知渠道列表。
func (a *AlertRule) ChannelList() []Channel {
	out := []Channel{}
	if len(a.Channels) == 0 {
		return out
	}
	_ = unmarshalJSON(a.Channels, &out)
	return out
}

// AlertHistory 是告警历史表。
type AlertHistory struct {
	ID       uint64 `gorm:"primaryKey" json:"id"`
	RuleID   uint64 `gorm:"index" json:"rule_id"`
	Level    string `gorm:"size:16" json:"level"` // info | warning | critical
	Title    string `gorm:"size:255" json:"title"`
	Content  string `gorm:"type:text" json:"content"`
	Resolved bool   `gorm:"default:false;index" json:"resolved"`
	// 关联对象，便于前端跳转
	Resource       string     `gorm:"size:64" json:"resource"`
	ResourceID     uint64     `gorm:"index" json:"resource_id"`
	FiredAt        time.Time  `gorm:"index" json:"fired_at"`
	LastNotifiedAt *time.Time `json:"last_notified_at"`
	ResolvedAt     *time.Time `json:"resolved_at"`
}

// TableName 指定表名。
func (AlertHistory) TableName() string { return "alert_histories" }

// SchemaVersion 记录数据库结构版本，升级时按版本号顺序执行变更脚本。
type SchemaVersion struct {
	ID        uint64    `gorm:"primaryKey" json:"id"`
	Version   int       `gorm:"uniqueIndex;not null" json:"version"`
	AppliedAt time.Time `json:"applied_at"`
	Note      string    `gorm:"size:255" json:"note"`
}

// TableName 指定表名。
func (SchemaVersion) TableName() string { return "schema_version" }

// NodeTask 是下发给节点的一次性任务（升级、重启、执行命令）。
type NodeTask struct {
	ID        uint64    `gorm:"primaryKey" json:"id"`
	NodeID    uint64    `gorm:"index;not null" json:"node_id"`
	Type      string    `gorm:"size:32;index" json:"type"`                   // upgrade | restart | exec
	Payload   JSON      `gorm:"type:text" json:"payload"`                    // 命令内容等参数
	Status    string    `gorm:"size:16;default:pending;index" json:"status"` // pending | running | done | failed
	Result    string    `gorm:"type:text" json:"result"`                     // 节点返回的原始输出
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName 指定表名。
func (NodeTask) TableName() string { return "node_tasks" }

// 节点任务类型与状态常量。
const (
	TaskUpgrade = "upgrade"
	TaskRestart = "restart"
	TaskExec    = "exec"

	TaskPending = "pending"
	TaskRunning = "running"
	TaskDone    = "done"
	TaskFailed  = "failed"
)
