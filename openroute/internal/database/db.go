// Package database 负责数据库连接、方言适配、自动迁移与事务封装。
//
// 支持的三种方言：SQLite（默认，纯 Go 驱动，无需 CGO）、MySQL、PostgreSQL。
// 设计目标之一是「交叉编译出单二进制」，因此 SQLite 使用纯 Go 实现，
// 不引入任何需要 C 编译器的依赖。
package database

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"

	"github.com/openroute/openroute/internal/model"
)

// DB 包装 *gorm.DB，并附带方言信息，供上层做跨方言分支。
type DB struct {
	*gorm.DB
	Dialect *Dialect
}

// Options 是打开数据库连接的参数。
type Options struct {
	// Path 即 config.yml 的 database-path
	Path string
	// MaxOpen / MaxIdle 连接池配置
	MaxOpen int
	MaxIdle int
	// LogLevel 控制 Gorm 自身的 SQL 日志级别
	LogLevel logger.LogLevel
}

// Open 按 database-path 建立数据库连接。
//
// 行为：
//   - 解析方言，SQLite 会自动创建所在目录；
//   - 设置连接池参数；
//   - 连接失败时返回包含「方言、地址（脱敏）、错误详情」的可读错误，
//     调用方据此打印并退出码 1。
//
// 返回值：包装后的 DB 与错误。
func Open(opt Options) (*DB, error) {
	d, err := ParseDatabasePath(opt.Path)
	if err != nil {
		return nil, err
	}

	gormCfg := &gorm.Config{
		Logger: logger.Default.LogMode(opt.LogLevel),
		// 表名由模型显式声明（TableName），关闭复数化推断避免意外改名。
		NamingStrategy: schema.NamingStrategy{SingularTable: false},
		// 关闭默认事务包装，由业务层显式控制事务边界。
		SkipDefaultTransaction: true,
	}

	var (
		gdb *gorm.DB
	)

	switch d.Name {
	case "sqlite":
		if d.DSN != ":memory:" && !strings.HasPrefix(d.DSN, "file:") {
			// 确保数据库文件所在目录存在，否则驱动会报 "unable to open database file"。
			dir := filepath.Dir(d.DSN)
			if dir != "" && dir != "." {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					return nil, fmt.Errorf("创建数据库目录 %s 失败: %w", dir, err)
				}
			}
		}
		// 纯 Go 驱动；开启 WAL 提升并发读性能，busy_timeout 避免瞬时锁冲突。
		dsn := d.DSN
		if dsn != ":memory:" && !strings.HasPrefix(dsn, "file:") {
			dsn = dsn + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(0)"
		}
		gdb, err = gorm.Open(sqlite.Open(dsn), gormCfg)
	case "mysql":
		gdb, err = gorm.Open(mysql.Open(d.DSN), gormCfg)
	case "postgres":
		gdb, err = gorm.Open(postgres.Open(d.DSN), gormCfg)
	default:
		return nil, fmt.Errorf("不支持的数据库方言 %q", d.Name)
	}

	if err != nil {
		return nil, fmt.Errorf("连接数据库失败\n"+
			"  方言    : %s\n"+
			"  地址    : %s\n"+
			"  错误    : %v\n"+
			"  提示    : 请检查 database-path 是否正确、数据库服务是否已启动、账号密码与网络是否可达",
			d.Name, d.Display, err)
	}

	sqlDB, err := gdb.DB()
	if err != nil {
		return nil, fmt.Errorf("获取数据库连接池失败: %w", err)
	}

	if sqlDB != nil {
		if opt.MaxOpen > 0 {
			sqlDB.SetMaxOpenConns(opt.MaxOpen)
		}
		if opt.MaxIdle > 0 {
			sqlDB.SetMaxIdleConns(opt.MaxIdle)
		}
		sqlDB.SetConnMaxLifetime(time.Hour)
		// 立刻 PING 一次，把「配置文件写错」这类问题在启动阶段就暴露出来。
		if err := sqlDB.Ping(); err != nil {
			return nil, fmt.Errorf("数据库连通性检查失败\n"+
				"  方言    : %s\n"+
				"  地址    : %s\n"+
				"  错误    : %v", d.Name, d.Display, err)
		}
	}

	return &DB{DB: gdb, Dialect: d}, nil
}

// Close 关闭底层连接池。
func (db *DB) Close() error {
	sqlDB, err := db.DB.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// WriteUnavailable 检查数据库当前是否处于「不可写」状态。
//
// 用于 /system/status 的健康检查：SQLite 最常见的故障是
// 磁盘写满或文件权限丢失，此时读仍可能成功而写会失败，
// 因此这里做一次真实的写入探测（更新一条 schema_version 记录的时间戳不需要，
// 改用一个临时表的建删操作更贴近真实写入且无副作用）。
//
// 返回 nil 表示可写；否则返回包含原因的错误。
func (db *DB) WriteUnavailable(ctx context.Context) error {
	// 用事务包住「建临时表 → 插入 → 回滚」，既能验证写入权限，
	// 又不会在数据库里留下任何痕迹。
	tx := db.DB.WithContext(ctx).Begin()
	if tx.Error != nil {
		return fmt.Errorf("无法开启事务: %w", tx.Error)
	}
	defer func() { _ = tx.Rollback() }()

	const probeTable = "openroute_write_probe"
	if err := tx.Exec("CREATE TABLE IF NOT EXISTS " + probeTable + " (id INTEGER)").Error; err != nil {
		return fmt.Errorf("数据库不可写: %w", err)
	}
	if err := tx.Exec("INSERT INTO " + probeTable + " (id) VALUES (1)").Error; err != nil {
		return fmt.Errorf("数据库不可写: %w", err)
	}
	return nil
}

// AllModels 返回需要 AutoMigrate 的全部模型，顺序即建表顺序（先父后子）。
//
// 规格书未使用数据库级外键，因此顺序只影响可读性，
// 但保持「被引用者在前」便于人工排查。
func AllModels() []interface{} {
	return []interface{}{
		&model.UserGroup{},
		&model.User{},
		&model.NodeGroup{},
		&model.Node{},
		&model.NodeTask{},
		&model.RuleGroup{},
		&model.DeviceGroup{},
		&model.ForwardRule{},
		&model.TrafficLog{},
		&model.Session{},
		&model.ProbeMetric{},
		&model.SystemSetting{},
		&model.AuditLog{},
		&model.APIToken{},
		&model.ConfigSnapshot{},
		&model.AlertRule{},
		&model.AlertHistory{},
		&model.MigrateBatch{},
		&model.MigrateIDMap{},
		&model.Backup{},
		&model.SchemaVersion{},
		&model.IdempotencyRecord{},
		&model.LoginAttempt{},
	}
}

// ColumnDiff 描述一张表的结构变更。
type ColumnDiff struct {
	Table   string
	Added   []string
	Changed []string
	// Removed 只提示不执行：删除列的破坏性太强，交给用户手工处理。
	Removed []string
}

// String 把结构变更渲染成可读的终端文本。
func (c ColumnDiff) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "  表 %s:\n", c.Table)
	if len(c.Added) > 0 {
		fmt.Fprintf(&b, "    + 新增列 %s\n", strings.Join(c.Added, ", "))
	}
	if len(c.Changed) > 0 {
		fmt.Fprintf(&b, "    ~ 类型变更 %s\n", strings.Join(c.Changed, ", "))
	}
	if len(c.Removed) > 0 {
		fmt.Fprintf(&b, "    ! 模型已删除但数据库仍保留的列（不会自动删除）: %s\n",
			strings.Join(c.Removed, ", "))
	}
	return b.String()
}

// AutoMigrate 执行表结构迁移，并返回结构变更 diff。
//
// 行为（规格书 4.6）：
//   - 新增表与新增列由 Gorm 自动完成；
//   - 删除列 MUST NOT 自动执行，只在 diff 中提示；
//   - 迁移完成后写入 schema_version 记录。
//
// 返回值：变更清单与错误。变更清单可能为空（无变化）。
func (db *DB) AutoMigrate() ([]ColumnDiff, error) {
	diffs, err := db.collectDiff()
	if err != nil {
		// 结构探测失败不阻断迁移，仅影响 diff 展示。
		diffs = nil
	}

	if err := db.DB.AutoMigrate(AllModels()...); err != nil {
		return diffs, fmt.Errorf("执行数据库结构迁移失败: %w", err)
	}

	if err := db.recordSchemaVersion(); err != nil {
		return diffs, err
	}
	return diffs, nil
}

// collectDiff 在迁移前对比模型与实际表结构，产出 diff。
//
// 只做「列集合」级别的比较（新增 / 缺失），类型变更通过 Gorm 的
// ColumnTypes 做字符串比较，足够个人自用场景的风险提示。
func (db *DB) collectDiff() ([]ColumnDiff, error) {
	migrator := db.DB.Migrator()
	var diffs []ColumnDiff

	for _, m := range AllModels() {
		stmt := &gorm.Statement{DB: db.DB}
		if err := stmt.Parse(m); err != nil {
			continue
		}
		table := stmt.Schema.Table

		if !migrator.HasTable(m) {
			diffs = append(diffs, ColumnDiff{Table: table, Added: []string{"(整表新增)"}})
			continue
		}

		existing, err := migrator.ColumnTypes(m)
		if err != nil {
			continue
		}
		existMap := make(map[string]string, len(existing))
		for _, c := range existing {
			existMap[c.Name()] = c.DatabaseTypeName()
		}

		diff := ColumnDiff{Table: table}
		for _, f := range stmt.Schema.Fields {
			if f.DBName == "" || f.IgnoreMigration {
				continue
			}
			dbType, ok := existMap[f.DBName]
			if !ok {
				diff.Added = append(diff.Added, f.DBName)
				continue
			}
			// 类型名在不同方言下大小写与别名不一致，做宽松比较。
			if !typeCompatible(dbType, string(f.DataType)) {
				diff.Changed = append(diff.Changed, fmt.Sprintf("%s (%s → %s)", f.DBName, dbType, f.DataType))
			}
		}
		for name := range existMap {
			if stmt.Schema.LookUpField(name) == nil {
				diff.Removed = append(diff.Removed, name)
			}
		}

		if len(diff.Added) > 0 || len(diff.Changed) > 0 || len(diff.Removed) > 0 {
			sort.Strings(diff.Added)
			sort.Strings(diff.Removed)
			diffs = append(diffs, diff)
		}
	}

	// 稳定输出顺序，便于比对两次运行的差异。
	sort.Slice(diffs, func(i, j int) bool { return diffs[i].Table < diffs[j].Table })
	return diffs, nil
}

// typeCompatible 宽松判断数据库列类型与 Gorm 字段类型是否兼容。
//
// 只关心「是否明显不兼容」，避免把方言别名（如 varchar 与 string）误报为变更。
func typeCompatible(dbType, gormType string) bool {
	d := strings.ToLower(dbType)
	g := strings.ToLower(gormType)
	if d == "" || g == "" {
		return true
	}
	// 数值族、字符串族、时间族内部视为兼容。
	family := func(s string) string {
		switch {
		case strings.Contains(s, "int"), strings.Contains(s, "serial"), strings.Contains(s, "numeric"), strings.Contains(s, "decimal"), strings.Contains(s, "float"), strings.Contains(s, "double"), strings.Contains(s, "real"):
			return "num"
		case strings.Contains(s, "char"), strings.Contains(s, "text"), strings.Contains(s, "clob"), strings.Contains(s, "json"), strings.Contains(s, "blob"):
			return "str"
		case strings.Contains(s, "time"), strings.Contains(s, "date"):
			return "time"
		case strings.Contains(s, "bool"):
			return "bool"
		default:
			return s
		}
	}
	return family(d) == family(g)
}

// recordSchemaVersion 记录当前结构版本，重复启动不会重复插入。
func (db *DB) recordSchemaVersion() error {
	var count int64
	if err := db.DB.Model(&model.SchemaVersion{}).
		Where("version = ?", CurrentSchemaVersion).Count(&count).Error; err != nil {
		return fmt.Errorf("读取 schema_version 失败: %w", err)
	}
	if count > 0 {
		return nil
	}
	rec := model.SchemaVersion{
		Version:   CurrentSchemaVersion,
		AppliedAt: time.Now().UTC(),
		Note:      "OpenRoute v1.0.0 初始结构",
	}
	if err := db.DB.Create(&rec).Error; err != nil {
		return fmt.Errorf("写入 schema_version 失败: %w", err)
	}
	return nil
}

// CurrentSchemaVersion 是当前代码期望的数据库结构版本号。
//
// 升级流程：每次结构变更把版本号 +1，并在 migrateSchema 中补一段迁移逻辑。
const CurrentSchemaVersion = 1

// SchemaVersionOf 读取数据库中的最高结构版本，用于 /system/status 展示。
func (db *DB) SchemaVersionOf() int {
	var v struct{ Version int }
	if err := db.DB.Raw("select coalesce(max(version), 0) as version from schema_version").Scan(&v).Error; err != nil {
		return 0
	}
	return v.Version
}

// WriteUnavailable 判断错误是否属于「数据库不可写」，
// 供上层转换成 50301 错误码。
func WriteUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "readonly") ||
		strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "unable to open database file") ||
		strings.Contains(msg, "disk i/o") ||
		strings.Contains(msg, "no space left")
}
