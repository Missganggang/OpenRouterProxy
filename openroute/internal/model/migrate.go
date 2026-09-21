package model

import "time"

// 迁移批次状态常量。
const (
	MigratePending    = "pending"
	MigrateRunning    = "running"
	MigrateFinished   = "finished"
	MigrateFailed     = "failed"
	MigrateRolledBack = "rolled_back"
)

// MigrateBatch 是迁移批次表，记录一次迁移的来源、进度与完整报告。
type MigrateBatch struct {
	ID           uint64 `gorm:"primaryKey" json:"id"`
	Source       string `gorm:"size:32" json:"source"`      // nyanpass | openroute
	SourceDSN    string `gorm:"size:512" json:"source_dsn"` // 脱敏后存储
	TargetDSN    string `gorm:"size:512" json:"target_dsn"`
	Status       string `gorm:"size:16" json:"status"` // pending | running | finished | failed | rolled_back
	Stage        int    `json:"stage"`                 // 当前阶段 1~7
	DryRun       bool   `gorm:"default:false" json:"dry_run"`
	TotalRows    int64  `json:"total_rows"`
	MigratedRows int64  `json:"migrated_rows"`
	// Report 保存完整预检/迁移报告，结构与 migrate 包的报告结构一致
	Report JSON `gorm:"type:text" json:"report"`
	// BackupPath 记录迁移前的自动备份文件路径，回滚失败时可用它恢复
	BackupPath string     `json:"backup_path"`
	Error      string     `gorm:"size:1024" json:"error"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

// TableName 指定表名。
func (MigrateBatch) TableName() string { return "migrate_batches" }

// MigrateIDMap 是 ID 映射表，用于回滚与增量续传。
//
// 字段名用 Table 而非 TableName：后者会与 Gorm 约定的 TableName() 方法冲突，
// 通过 column 标签显式映射到数据库列 table_name，对外 JSON 仍为 table_name。
type MigrateIDMap struct {
	ID        uint64    `gorm:"primaryKey" json:"id"`
	BatchID   uint64    `gorm:"index" json:"batch_id"`
	Table     string    `gorm:"column:table_name;size:64;index" json:"table_name"`
	SourceID  uint64    `gorm:"index" json:"source_id"`
	TargetID  uint64    `gorm:"index" json:"target_id"`
	Checksum  string    `gorm:"size:64" json:"checksum"` // 该行内容的校验和，用于幂等判断
	CreatedAt time.Time `json:"created_at"`
}

// TableName 指定表名。
func (MigrateIDMap) TableName() string { return "migrate_id_map" }

// Backup 是备份记录表，记录每次生成的备份文件。
//
// WebUI 的「下载备份 / 上传恢复」入口依赖本表，
// 命令行 `-backup` 生成的备份同样登记入库以便统一管理。
type Backup struct {
	ID         uint64    `gorm:"primaryKey" json:"id"`
	Name       string    `gorm:"size:128" json:"name"`
	Path       string    `gorm:"size:512" json:"path"`
	Size       int64     `json:"size"`
	Checksum   string    `gorm:"size:64" json:"checksum"`
	Manifest   JSON      `gorm:"type:text" json:"manifest"` // 版本号、导出时间、表行数
	WithSecret bool      `gorm:"default:false" json:"with_secret"`
	CreatedAt  time.Time `json:"created_at"`
}

// TableName 指定表名。
func (Backup) TableName() string { return "backups" }

// IdempotencyRecord 记录 POST 创建接口的幂等键，保证重复请求不重复建资源。
type IdempotencyRecord struct {
	Key        string    `gorm:"primaryKey;size:128" json:"key"`
	Endpoint   string    `gorm:"size:255;index" json:"endpoint"`
	StatusCode int       `json:"status_code"`
	Response   JSON      `gorm:"type:text" json:"response"`
	CreatedAt  time.Time `gorm:"index" json:"created_at"`
}

// TableName 指定表名。
func (IdempotencyRecord) TableName() string { return "idempotency_records" }

// LoginAttempt 记录登录失败次数，用于「连续失败 3 次要求验证码、10 次锁定 15 分钟」。
type LoginAttempt struct {
	ID        uint64    `gorm:"primaryKey" json:"id"`
	Username  string    `gorm:"size:64;index" json:"username"`
	IP        string    `gorm:"size:64;index" json:"ip"`
	Success   bool      `gorm:"index" json:"success"`
	CreatedAt time.Time `gorm:"index" json:"created_at"`
}

// TableName 指定表名。
func (LoginAttempt) TableName() string { return "login_attempts" }
