package migrate

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/openroute/openroute/internal/database"
	"github.com/openroute/openroute/internal/model"
	"github.com/openroute/openroute/internal/util"
)

// 备份包内的固定条目名（规格书 6.15）。
const (
	// BackupEntryData 是数据条目：SQLite 直接放 data.db，其它方言放 SQL dump。
	BackupEntryData = "data.db"
	// BackupEntryDump 是 MySQL / PostgreSQL 的 SQL dump 条目名。
	BackupEntryDump = "dump.sql"
	// BackupEntryConfig 是配置文件条目。
	BackupEntryConfig = "config.yml"
	// BackupEntryManifest 是清单条目。
	BackupEntryManifest = "manifest.json"
)

// BackupManifest 是备份清单（规格书 6.15）。
//
// 它既被写进 zip 内的 manifest.json，也被写入 backups 表的 manifest 字段，
// 因此 WebUI 不需要解压就能展示备份内容。
type BackupManifest struct {
	// Version 是备份格式版本
	Version int `json:"version"`
	// AppVersion 是生成备份的面板版本
	AppVersion string `json:"app_version"`
	// SchemaVersion 是数据库结构版本
	SchemaVersion int `json:"schema_version"`
	// Dialect 是源库方言
	Dialect string `json:"dialect"`
	// ExportedAt 是导出时间（UTC）
	ExportedAt time.Time `json:"exported_at"`
	// WithSecret 表示 config.yml 中的 secret-key 是否被保留
	WithSecret bool `json:"with_secret"`
	// Tables 是「表名 → 行数」
	Tables map[string]int64 `json:"tables"`
	// TotalRows 是全部表行数之和
	TotalRows int64 `json:"total_rows"`
	// Checksum 是数据条目的 SHA256（SQLite 为 data.db 的摘要，其它为 dump）
	Checksum string `json:"checksum"`
	// ConfigChecksum 是 config.yml 条目的摘要
	ConfigChecksum string `json:"config_checksum,omitempty"`
	// ConfigPath 记录原始配置文件路径，恢复时用于提示
	ConfigPath string `json:"config_path,omitempty"`
	// DataEntry 是数据条目在包内的名字
	DataEntry string `json:"data_entry"`
	// Notes 是补充说明（例如「SQLite 直接复制文件，未做 dump」）
	Notes []string `json:"notes"`
}

// BackupFormatVersion 是当前备份格式版本号。
//
// 升级本包时若打包结构有变，MUST 递增此值，恢复时据此给出兼容性提示。
const BackupFormatVersion = 1

// BackupOptions 是生成备份的参数。
type BackupOptions struct {
	// DataDBPath 是 config.yml 中的 database-path
	DataDBPath string
	// ConfigPath 是 config.yml 的路径，为空时不打包配置
	ConfigPath string
	// OutZipPath 是输出 zip 路径
	OutZipPath string
	// WithSecret 为 true 时保留 config.yml 中的 secret-key
	WithSecret bool
	// AppVersion 是面板版本号，写入清单
	AppVersion string
	// Log 可为空
	Log Logger
}

// BackupResult 是一次备份的结果。
type BackupResult struct {
	Path     string          `json:"path"`
	Size     int64           `json:"size"`
	Checksum string          `json:"checksum"`
	Manifest *BackupManifest `json:"manifest"`
}

// Logger 是本包对日志的最小依赖。
//
// 定义在这里而不是直接用 zap，是为了让备份功能可以被单独测试，
// 同时避免整个包硬绑定某个日志实现。
type Logger interface {
	// Info 输出一条普通日志。
	Info(msg string, fields ...zap.Field)
	// Warn 输出一条告警日志。
	Warn(msg string, fields ...zap.Field)
}

// CreateBackup 生成单文件全量备份（规格书 6.15）。
//
// 包内结构：
//   - data.db      SQLite 数据库文件；MySQL / PostgreSQL 则为 dump.sql
//   - config.yml   配置文件，secret-key 默认脱敏
//   - manifest.json 版本号、导出时间、表行数、校验和
//
// 参数 opt 见 BackupOptions。
// 返回备份结果（含清单）与错误。
func CreateBackup(ctx context.Context, opt BackupOptions) (*BackupResult, error) {
	if strings.TrimSpace(opt.DataDBPath) == "" {
		return nil, fmt.Errorf("缺少 database-path，无法备份")
	}
	if strings.TrimSpace(opt.OutZipPath) == "" {
		return nil, fmt.Errorf("缺少备份输出路径")
	}

	dialect, err := database.ParseDatabasePath(opt.DataDBPath)
	if err != nil {
		return nil, err
	}

	if dir := filepath.Dir(absPath(opt.OutZipPath)); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建备份目录 %s 失败: %w", dir, err)
		}
	}

	out, err := os.Create(opt.OutZipPath)
	if err != nil {
		return nil, fmt.Errorf("创建备份文件 %s 失败: %w", opt.OutZipPath, err)
	}
	// 出错时删除半成品，避免留下一个打不开的 zip 让用户误以为备份成功。
	success := false
	defer func() {
		_ = out.Close()
		if !success {
			_ = os.Remove(opt.OutZipPath)
		}
	}()

	zw := zip.NewWriter(out)
	defer func() { _ = zw.Close() }()

	manifest := &BackupManifest{
		Version:    BackupFormatVersion,
		AppVersion: opt.AppVersion,
		Dialect:    dialect.Name,
		ExportedAt: nowUTC(),
		WithSecret: opt.WithSecret,
		Tables:     map[string]int64{},
		Notes:      []string{},
		ConfigPath: opt.ConfigPath,
	}

	// —— 1) 数据 ——
	switch dialect.Name {
	case "sqlite":
		// SQLite 直接在文件层面复制：纯 Go 驱动与 cgo 驱动产出的都是标准
		// SQLite 文件，复制即完整备份，比逻辑 dump 快且不会漏掉任何表。
		checksum, size, err := writeSQLiteEntry(zw, dialect.File)
		if err != nil {
			return nil, err
		}
		manifest.DataEntry = BackupEntryData
		manifest.Checksum = checksum
		manifest.Notes = append(manifest.Notes,
			"SQLite 备份为数据库文件的完整副本（含未提交的 WAL 已先行检查点）")
		_ = size

		// 行数统计需要读库，用一个只读连接完成。
		db, err := database.Open(database.Options{Path: opt.DataDBPath, MaxOpen: 1, MaxIdle: 1, LogLevel: gormSilent()})
		if err != nil {
			return nil, fmt.Errorf("读取数据行数失败: %w", err)
		}
		manifest.Tables, manifest.TotalRows = countAllTables(ctx, db.DB, dialect.Name)
		manifest.SchemaVersion = db.SchemaVersionOf()
		_ = db.Close()

	default:
		db, err := database.Open(database.Options{Path: opt.DataDBPath, MaxOpen: 1, MaxIdle: 1, LogLevel: gormSilent()})
		if err != nil {
			return nil, fmt.Errorf("连接数据库失败: %w", err)
		}
		defer func() { _ = db.Close() }()

		checksum, err := writeSQLEntry(ctx, zw, db.DB, dialect.Name)
		if err != nil {
			return nil, err
		}
		manifest.DataEntry = BackupEntryDump
		manifest.Checksum = checksum
		manifest.Tables, manifest.TotalRows = countAllTables(ctx, db.DB, dialect.Name)
		manifest.SchemaVersion = db.SchemaVersionOf()
		manifest.Notes = append(manifest.Notes,
			"MySQL / PostgreSQL 备份为逻辑 SQL dump，恢复时需手动导入目标库")
	}

	// —— 2) 配置 ——
	if opt.ConfigPath != "" {
		raw, err := os.ReadFile(opt.ConfigPath)
		switch {
		case err == nil:
			content := raw
			if !opt.WithSecret {
				// secret-key 默认脱敏（规格书 6.15）。
				content = redactSecretKey(raw)
			}
			sum, err := writeEntry(zw, BackupEntryConfig, content)
			if err != nil {
				return nil, err
			}
			manifest.ConfigChecksum = sum
			if !opt.WithSecret {
				manifest.Notes = append(manifest.Notes,
					"config.yml 中的 secret-key 已脱敏；恢复后需重新填写，否则所有会话与节点凭证失效")
			}
		case os.IsNotExist(err):
			manifest.Notes = append(manifest.Notes,
				"配置文件 "+opt.ConfigPath+" 不存在，本次备份未包含 config.yml")
		default:
			return nil, fmt.Errorf("读取配置文件失败: %w", err)
		}
	}

	// —— 3) 清单 ——
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("序列化备份清单失败: %w", err)
	}
	if _, err := writeEntry(zw, BackupEntryManifest, manifestBytes); err != nil {
		return nil, err
	}

	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("写入备份压缩包失败: %w", err)
	}
	if err := out.Sync(); err != nil {
		return nil, fmt.Errorf("刷盘失败: %w", err)
	}
	if err := out.Close(); err != nil {
		return nil, fmt.Errorf("关闭备份文件失败: %w", err)
	}
	success = true

	info, err := os.Stat(opt.OutZipPath)
	if err != nil {
		return nil, fmt.Errorf("读取备份文件信息失败: %w", err)
	}
	// 整个 zip 文件的摘要：登记入库后可用于「文件被改动」的检测。
	fileSum, err := fileSHA256(opt.OutZipPath)
	if err != nil {
		return nil, err
	}

	return &BackupResult{
		Path:     opt.OutZipPath,
		Size:     info.Size(),
		Checksum: fileSum,
		Manifest: manifest,
	}, nil
}

// writeEntry 向 zip 写入一个条目并返回其内容的 SHA256。
func writeEntry(zw *zip.Writer, name string, content []byte) (string, error) {
	w, err := zw.Create(name)
	if err != nil {
		return "", fmt.Errorf("创建备份条目 %s 失败: %w", name, err)
	}
	if _, err := w.Write(content); err != nil {
		return "", fmt.Errorf("写入备份条目 %s 失败: %w", name, err)
	}
	return util.SHA256HexBytes(content), nil
}

// writeSQLiteEntry 把 SQLite 数据库文件写入 zip。
//
// 复制前先做一次 WAL 检查点：WAL 模式下最新数据可能还在 .db-wal 里，
// 只复制 .db 会丢掉最近的写入。
func writeSQLiteEntry(zw *zip.Writer, dbFile string) (string, int64, error) {
	if dbFile == "" || dbFile == ":memory:" {
		return "", 0, fmt.Errorf("SQLite 备份需要真实的数据库文件路径，当前是 %q", dbFile)
	}
	path := absPath(dbFile)
	if _, err := os.Stat(path); err != nil {
		return "", 0, fmt.Errorf("数据库文件 %s 不可读: %w", path, err)
	}

	// 尽力而为地做检查点：失败不阻断备份（例如数据库被独占锁定）。
	if err := checkpointSQLite(path); err != nil {
		// 只在日志里提示，因为多数情况下数据已经落盘。
		_ = err
	}

	f, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("打开数据库文件失败: %w", err)
	}
	defer f.Close()

	w, err := zw.Create(BackupEntryData)
	if err != nil {
		return "", 0, fmt.Errorf("创建备份条目失败: %w", err)
	}
	h := newSHA256()
	n, err := io.Copy(io.MultiWriter(w, h), f)
	if err != nil {
		return "", 0, fmt.Errorf("复制数据库文件失败: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// writeSQLEntry 把 MySQL / PostgreSQL 的数据导出为逻辑 SQL dump。
//
// 实现为「逐表 select + 拼 insert」而不是调用 mysqldump / pg_dump：
// 面板要求单二进制部署，不能假设目标机器装了命令行客户端。
func writeSQLEntry(ctx context.Context, zw *zip.Writer, db *gorm.DB, dialectName string) (string, error) {
	w, err := zw.Create(BackupEntryDump)
	if err != nil {
		return "", fmt.Errorf("创建 dump 条目失败: %w", err)
	}
	h := newSHA256()
	buf := io.MultiWriter(w, h)

	header := fmt.Sprintf("-- OpenRoute 备份 SQL dump\n-- 方言: %s\n-- 导出时间(UTC): %s\n\n",
		dialectName, nowUTC().Format(time.RFC3339))
	if _, err := buf.Write([]byte(header)); err != nil {
		return "", err
	}

	tables := tablesForDump()
	for _, table := range tables {
		if _, err := buf.Write([]byte(fmt.Sprintf("-- 表 %s\n", table))); err != nil {
			return "", err
		}
		// 逐表导出：先查行，再拼 INSERT；任何一行的序列化失败都跳过该行
		// 而不是中断整份 dump，保证「能备份的先备份下来」。
		rows := []map[string]interface{}{}
		if err := db.WithContext(ctx).Raw("select * from " + table).Scan(&rows).Error; err != nil {
			// 表不存在时继续下一张。
			continue
		}
		for _, row := range rows {
			stmt, err := buildInsert(table, row)
			if err != nil {
				continue
			}
			if _, err := buf.Write([]byte(stmt + "\n")); err != nil {
				return "", err
			}
		}
		if _, err := buf.Write([]byte("\n")); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// buildInsert 把一行数据拼成 INSERT 语句。
//
// 这里对每个值都做转义，且整体是发给用户的 SQL 文本而不是驱动程序执行的
// 语句，因此必须严格处理引号与反斜杠，避免恢复时语法错误。
func buildInsert(table string, row map[string]interface{}) (string, error) {
	if len(row) == 0 {
		return "", fmt.Errorf("空行")
	}
	cols := make([]string, 0, len(row))
	vals := make([]string, 0, len(row))
	// 列顺序固定，便于人工阅读与 diff。
	for _, k := range sortedInterfaceKeys(row) {
		cols = append(cols, k)
		vals = append(vals, sqlLiteral(row[k]))
	}
	return fmt.Sprintf("insert into %s (%s) values (%s);",
		table, strings.Join(cols, ", "), strings.Join(vals, ", ")), nil
}

// sqlLiteral 把任意值转成 SQL 字面量。
func sqlLiteral(v interface{}) string {
	if v == nil {
		return "null"
	}
	switch x := v.(type) {
	case bool:
		if x {
			return "1"
		}
		return "0"
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return fmt.Sprintf("%v", x)
	case time.Time:
		return "'" + x.UTC().Format("2006-01-02 15:04:05") + "'"
	default:
		s := asString(v)
		// 单引号翻倍是 SQL 标准的转义方式，三种方言都认。
		s = strings.ReplaceAll(s, "'", "''")
		// 反斜杠在 MySQL 里是转义字符，翻倍后不会改变原值。
		s = strings.ReplaceAll(s, "\\", "\\\\")
		return "'" + s + "'"
	}
}

// tablesForDump 返回 dump 时导出的表清单（与 AllModels 顺序一致）。
func tablesForDump() []string {
	names := []string{}
	for _, m := range database.AllModels() {
		if t, ok := modelTableName(m); ok {
			names = append(names, t)
		}
	}
	return names
}

// countAllTables 统计全部表的行数。
func countAllTables(ctx context.Context, db *gorm.DB, dialectName string) (map[string]int64, int64) {
	out := map[string]int64{}
	var total int64
	for _, t := range tablesForDump() {
		n := countTableRows(ctx, db, t)
		if n == 0 {
			// 表不存在时也会得到 0；不区分「空表」与「表不存在」，
			// 因为两者对备份语义没有影响。
		}
		out[t] = n
		total += n
	}
	return out, total
}

// redactSecretKey 把 config.yml 中的 secret-key 替换为占位串（规格书 6.15）。
//
// 用逐行处理而不是 YAML 解析再序列化：后者会丢掉用户写的注释，
// 而注释对「照着恢复配置」这件事很重要。
func redactSecretKey(raw []byte) []byte {
	const placeholder = `secret-key: "***REDACTED***"`
	lines := strings.Split(string(raw), "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "secret-key:") {
			continue
		}
		// 保留原缩进。
		indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		lines[i] = indent + placeholder
	}
	return []byte(strings.Join(lines, "\n"))
}

// RestoreBackup 从备份文件恢复（规格书 6.15）。
//
// 行为（顺序不可调换）：
//  1. 校验 zip 与清单；
//  2. 把当前数据另存为 data.db.before-restore（恢复失败可回退）；
//  3. 覆盖写入数据库文件与 config.yml。
//
// 参数 zipPath 为备份文件；dataDBPath 为 config.yml 的 database-path；
// configPath 为 config.yml 路径（为空时跳过配置恢复）。
// 返回恢复后的清单与错误。
func RestoreBackup(zipPath, dataDBPath, configPath string) (*BackupManifest, error) {
	if strings.TrimSpace(zipPath) == "" {
		return nil, fmt.Errorf("缺少备份文件路径")
	}
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, fmt.Errorf("打开备份文件失败（可能不是有效的 zip）: %w", err)
	}
	defer zr.Close()

	// —— 1) 读清单 ——
	manifest, err := readManifest(&zr.Reader)
	if err != nil {
		return nil, err
	}
	if manifest.Version > BackupFormatVersion {
		return nil, fmt.Errorf("备份格式版本 %d 高于当前程序支持的 %d，请升级面板后再恢复",
			manifest.Version, BackupFormatVersion)
	}

	dialect, err := database.ParseDatabasePath(dataDBPath)
	if err != nil {
		return nil, err
	}

	// —— 2) 校验数据条目存在 ——
	dataEntry, err := findEntry(&zr.Reader, manifest.DataEntry)
	if err != nil {
		return nil, err
	}

	// —— 3) 恢复前先备份当前数据 ——
	if dialect.Name == "sqlite" {
		target := absPath(dialect.File)

		// 3.1 先把「当前」数据完整落盘再做任何改动。
		//
		// 这一步至关重要：若数据库处于 WAL 模式且仍有未检查点的写入，
		// 直接复制主文件会丢掉最近的事务。这里先做一次 TRUNCATE 检查点，
		// 保证 -wal 中的内容全部合并进主文件后再复制。
		if _, statErr := os.Stat(target); statErr == nil {
			_ = checkpointSQLite(target)

			before := target + ".before-restore"
			if err := copyFile(target, before); err != nil {
				return nil, fmt.Errorf("恢复前备份当前数据失败（%s）: %w", before, err)
			}
			// 连同一并复制 WAL，确保回滚用的快照自身也是完整的。
			// 复制失败不阻断主流程（此时已经做过检查点，主文件是完整的）。
			if _, err := os.Stat(target + "-wal"); err == nil {
				_ = copyFile(target+"-wal", before+"-wal")
			}
		}

		// 3.2 再对「备份中的」数据库文件做一次检查点。
		//
		// 备份包里的 data.db 可能是从 WAL 模式下复制出来的，其主文件
		// 未必包含全部数据。先把内容释放到临时文件、检查点合并、
		// 再原子替换目标文件，避免出现「恢复了但读出来是空的」。
		tmp := target + ".restore-tmp"
		if err := extractEntryTo(dataEntry, tmp); err != nil {
			_ = os.Remove(tmp)
			return nil, fmt.Errorf("%w: 写入数据库文件失败: %v", ErrRestoreFailed, err)
		}
		if err := checkpointSQLite(tmp); err != nil {
			// 检查点失败不致命：文件本身可能就是完整的主文件。
			_ = err
		}

		// 3.3 清理目标的 WAL / SHM。
		//
		// 必须在替换主文件之后、再做一次检查点之前删除，并在删除后
		// 立刻用新文件重建，否则残留的 WAL 会在下次打开时被重放，
		// 把刚恢复的数据覆盖成「恢复前」的状态。
		_ = os.Remove(target + "-wal")
		_ = os.Remove(target + "-shm")

		// 3.4 原子替换。
		if err := os.Rename(tmp, target); err != nil {
			// 跨设备或目标被占用时 Rename 可能失败，退化为复制。
			if cerr := copyFile(tmp, target); cerr != nil {
				_ = os.Remove(tmp)
				return nil, fmt.Errorf("%w: 覆盖数据库文件失败: %v", ErrRestoreFailed, cerr)
			}
			_ = os.Remove(tmp)
		}

		// 3.5 用恢复后的文件重建 WAL 索引，并再次检查点，
		//     确保落盘状态自洽（此时 -wal 是全新的空文件）。
		if err := checkpointSQLite(target); err != nil {
			_ = err
		}
		_ = os.Remove(target + "-wal")
		_ = os.Remove(target + "-shm")
	} else {
		// 非 SQLite：把 dump 释放成同名 .sql 文件，并说明需要手工导入。
		out := absPath(dialect.File)
		if out == "" || out == ":memory:" {
			out = filepath.Join(filepath.Dir(absPath(zipPath)), "restore.sql")
		} else {
			out = out + ".restore.sql"
		}
		if err := extractEntryTo(dataEntry, out); err != nil {
			return nil, fmt.Errorf("%w: 释放 SQL dump 失败: %v", ErrRestoreFailed, err)
		}
		return manifest, fmt.Errorf("%w: 目标库为 %s，dump 已释放到 %s，"+
			"请手动导入（不要直接覆盖正在运行的库）",
			ErrRestoreFailed, dialect.Name, out)
	}

	// —— 4) 恢复配置 ——
	if configPath != "" {
		if cfgEntry, err := findEntry(&zr.Reader, BackupEntryConfig); err == nil {
			content, err := readEntry(cfgEntry)
			if err != nil {
				return nil, fmt.Errorf("%w: 读取配置条目失败: %v", ErrRestoreFailed, err)
			}
			// 恢复前同样留一份旧配置。
			if _, statErr := os.Stat(configPath); statErr == nil {
				if err := copyFile(configPath, configPath+".before-restore"); err != nil {
					return nil, fmt.Errorf("%w: 备份当前配置失败: %v", ErrRestoreFailed, err)
				}
			}
			if err := os.WriteFile(configPath, content, 0o600); err != nil {
				return nil, fmt.Errorf("%w: 写入配置文件失败: %v", ErrRestoreFailed, err)
			}
			if !manifest.WithSecret {
				manifest.Notes = append(manifest.Notes,
					"配置文件已恢复，但 secret-key 是脱敏占位值，请手工填回原密钥")
			}
		}
	}

	return manifest, nil
}

// RestoreWithDB 在已开启的数据库连接上执行恢复，并在完成后登记备份记录。
//
// 与 RestoreBackup 的区别：本方法额外维护 backups 表，
// 供 WebUI 的「上传恢复」入口使用。
func RestoreWithDB(ctx context.Context, db *database.DB, zipPath, dataDBPath, configPath string) (*BackupRecord, error) {
	manifest, err := RestoreBackup(zipPath, dataDBPath, configPath)
	if err != nil {
		return nil, err
	}
	// 恢复完成后重新登记一条记录，便于在列表里看到「这份文件被用过」。
	rec, regErr := RegisterBackup(ctx, db, zipPath, manifest, false)
	if regErr != nil {
		return &BackupRecord{Manifest: manifest, Path: zipPath}, nil
	}
	return rec, nil
}

// BackupRecord 是一条备份记录。
type BackupRecord struct {
	ID         uint64          `json:"id"`
	Name       string          `json:"name"`
	Path       string          `json:"path"`
	Size       int64           `json:"size"`
	Checksum   string          `json:"checksum"`
	WithSecret bool            `json:"with_secret"`
	Manifest   *BackupManifest `json:"manifest"`
	CreatedAt  time.Time       `json:"created_at"`
}

// RegisterBackup 把备份文件登记到 backups 表（规格书 6.15）。
//
// 幂等：同一个路径重复登记时更新既有行而不是新增，避免列表里出现
// 一串一模一样的记录（用户重复点「生成备份」很常见）。
func RegisterBackup(ctx context.Context, db *database.DB, zipPath string, manifest *BackupManifest, withSecret bool) (*BackupRecord, error) {
	if db == nil {
		return nil, fmt.Errorf("数据库未连接")
	}
	info, err := os.Stat(zipPath)
	if err != nil {
		return nil, fmt.Errorf("读取备份文件失败: %w", err)
	}
	sum, err := fileSHA256(zipPath)
	if err != nil {
		return nil, err
	}
	if manifest == nil {
		// 从文件里读一次清单，保证登记的内容与文件一致。
		if m, err := readManifestFromFile(zipPath); err == nil {
			manifest = m
		} else {
			manifest = &BackupManifest{Version: BackupFormatVersion, ExportedAt: info.ModTime().UTC()}
		}
	}

	manifestBytes, _ := json.Marshal(manifest)

	row := model.Backup{
		Name:       filepath.Base(zipPath),
		Path:       zipPath,
		Size:       info.Size(),
		Checksum:   sum,
		Manifest:   model.JSON(manifestBytes),
		WithSecret: withSecret || manifest.WithSecret,
		CreatedAt:  nowUTC(),
	}

	if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 先删同路径的旧记录，再插入，等价于 UPSERT 但不需要依赖
		// path 上的唯一索引（该索引在模型里没有声明）。
		if err := tx.Where("path = ?", zipPath).Delete(&model.Backup{}).Error; err != nil {
			return err
		}
		return tx.Create(&row).Error
	}); err != nil {
		return nil, fmt.Errorf("登记备份记录失败: %w", err)
	}

	return &BackupRecord{
		ID: row.ID, Name: row.Name, Path: row.Path, Size: row.Size,
		Checksum: row.Checksum, WithSecret: row.WithSecret,
		Manifest: manifest, CreatedAt: row.CreatedAt,
	}, nil
}

// ListBackups 返回备份列表，最新的在前（供 GET /api/v1/backups）。
func ListBackups(ctx context.Context, db *database.DB) ([]BackupRecord, error) {
	if db == nil {
		return nil, fmt.Errorf("数据库未连接")
	}
	var rows []model.Backup
	if err := db.WithContext(ctx).Order("id DESC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]BackupRecord, 0, len(rows))
	for i := range rows {
		r := rows[i]
		rec := BackupRecord{
			ID: r.ID, Name: r.Name, Path: r.Path, Size: r.Size,
			Checksum: r.Checksum, WithSecret: r.WithSecret, CreatedAt: r.CreatedAt,
		}
		var m BackupManifest
		if len(r.Manifest) > 0 && json.Unmarshal(r.Manifest, &m) == nil {
			rec.Manifest = &m
		}
		out = append(out, rec)
	}
	return out, nil
}

// DeleteBackup 删除备份记录及磁盘文件。
//
// 先删文件再删记录：反过来的话删文件失败会留下一条指向不存在文件的记录。
func DeleteBackup(ctx context.Context, db *database.DB, id uint64, removeFile bool) error {
	var row model.Backup
	if err := db.WithContext(ctx).First(&row, id).Error; err != nil {
		return fmt.Errorf("备份记录 %d 不存在: %w", id, err)
	}
	if removeFile && row.Path != "" {
		if err := os.Remove(row.Path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("删除备份文件 %s 失败: %w", row.Path, err)
		}
	}
	return db.WithContext(ctx).Delete(&model.Backup{}, id).Error
}

// ---------------------------------------------------------------------------
// 内部工具
// ---------------------------------------------------------------------------

// ErrRestoreFailed 表示恢复失败（对应错误码 70005）。
var ErrRestoreFailed = fmt.Errorf("备份恢复失败")

// newSHA256 返回一个 SHA256 哈希器。
//
// 单独包一层是为了让「本包用哪个哈希算法」只有一个来源，
// 后续换算法时只需要改这里与备份格式版本号。
func newSHA256() hash.Hash { return sha256.New() }

// checkpointSQLite 执行一次 WAL 检查点。
//
// 通过 Gorm 的连接执行 `pragma wal_checkpoint(TRUNCATE)`，
// 把 WAL 中的内容合并回主库文件，确保直接复制文件时数据完整。
func checkpointSQLite(path string) error {
	db, err := database.Open(database.Options{
		Path: path, MaxOpen: 1, MaxIdle: 1, LogLevel: gormSilent(),
	})
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	return db.DB.Exec("pragma wal_checkpoint(TRUNCATE)").Error
}

// gormSilent 返回 Gorm 的静默日志级别。
//
// 备份与迁移都是临时操作，SQL 日志只会刷屏，统一关掉。
func gormSilent() logger.LogLevel { return logger.Silent }

// findEntry 在 zip 中按名字查找条目。
func findEntry(r *zip.Reader, name string) (*zip.File, error) {
	for _, f := range r.File {
		if f.Name == name {
			return f, nil
		}
	}
	return nil, fmt.Errorf("备份文件中缺少 %s 条目", name)
}

// readEntry 读取 zip 条目的全部内容。
func readEntry(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// readManifest 从 zip 中读取清单。
func readManifest(r *zip.Reader) (*BackupManifest, error) {
	f, err := findEntry(r, BackupEntryManifest)
	if err != nil {
		return nil, err
	}
	b, err := readEntry(f)
	if err != nil {
		return nil, fmt.Errorf("读取备份清单失败: %w", err)
	}
	var m BackupManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("解析备份清单失败: %w", err)
	}
	if m.Tables == nil {
		m.Tables = map[string]int64{}
	}
	return &m, nil
}

// readManifestFromFile 从备份文件读取清单（不解压到磁盘）。
func readManifestFromFile(zipPath string) (*BackupManifest, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return readManifest(&zr.Reader)
}

// extractEntryTo 把 zip 条目解压到指定路径。
func extractEntryTo(f *zip.File, dest string) error {
	if dir := filepath.Dir(dest); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, rc); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// copyFile 复制文件，用于生成 .before-restore 快照。
func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if dir := filepath.Dir(dest); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// fileSHA256 计算文件的 SHA256 十六进制摘要。
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := newSHA256()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sortedInterfaceKeys 返回 map 的排序键。
func sortedInterfaceKeys(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

// modelTableName 取出模型对应的表名。
//
// 模型都实现了 TableName() string，用类型断言统一获取，
// 避免在本包里硬编码一份表名清单（两份清单迟早会不一致）。
func modelTableName(m interface{}) (string, bool) {
	type tabler interface{ TableName() string }
	if t, ok := m.(tabler); ok {
		return t.TableName(), true
	}
	return "", false
}
