package migrate

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/openroute/openroute/internal/database"
	"github.com/openroute/openroute/internal/model"
	"github.com/openroute/openroute/internal/util"
)

// ColumnInfo 描述源库中一张表的一个列。
//
// 迁移预检只依赖列名与类型名做映射判断，因此这里刻意不引入方言相关的细节。
type ColumnInfo struct {
	Name string
	Type string
}

// ProbeResult 是阶段 1「连接与探测」的结果。
//
// 它同时服务于两件事：判断源库版本是否过旧，以及给预检报告提供源库概况。
type ProbeResult struct {
	// DialectName 是方言名：sqlite / mysql / postgres
	DialectName string
	// Display 是脱敏后的可读地址，可直接打印进报告
	Display string
	// SchemaVersion 是源库记录的结构版本号，读不到时为 0
	SchemaVersion int
	// SchemaVersionRaw 是版本原值（可能是 nc20260101 这类字符串），读不到时为空
	SchemaVersionRaw string
	// TableCount 是源库中的表数量
	TableCount int
	// RowCounts 是「表名 → 行数」，只包含 Nyanpass 相关的表
	RowCounts map[string]int64
	// MissingTables 是源库缺失的必要表
	MissingTables []string
	// MissingColumns 是「表名 → 缺失的列」
	MissingColumns map[string][]string
	// Empty 表示源库没有任何业务数据（用户/节点/规则全为空）
	Empty bool
	// TotalRows 是全部业务表的行数之和
	TotalRows int64
}

// Version 返回用于报告展示的版本字符串。
func (p *ProbeResult) Version() string {
	if p.SchemaVersionRaw != "" {
		return p.SchemaVersionRaw
	}
	if p.SchemaVersion > 0 {
		return fmt.Sprintf("Nyanpass %d", p.SchemaVersion)
	}
	// 读不到版本时按「未知」处理，报告里照实写，不猜测版本号。
	return "Nyanpass（源库未记录版本）"
}

// SourceReader 是「数据源」的统一抽象。
//
// Nyanpass 与 OpenRoute 各自实现一份，迁移编排器只依赖本接口，
// 因此 `-migrate from=nyanpass` 与 `-copy-database` 可以复用同一套编排逻辑。
type SourceReader interface {
	// DialectName 返回源库方言名。
	DialectName() string
	// Display 返回脱敏后的可读地址。
	Display() string
	// Tables 返回源库中实际存在的表名集合（键为小写表名）。
	Tables(ctx context.Context) (map[string]bool, error)
	// Columns 返回指定表的列信息，表不存在时返回空切片。
	Columns(ctx context.Context, table string) ([]ColumnInfo, error)
	// CountRows 返回指定表的行数，表不存在时返回 0。
	CountRows(ctx context.Context, table string) (int64, error)
	// SchemaVersion 读取源库的结构版本号，读不到返回 0 与空串。
	SchemaVersion(ctx context.Context) (int, string)
	// ScanRows 流式扫描指定表，逐行回调；callback 返回错误时中止扫描。
	ScanRows(ctx context.Context, table string, callback func(row map[string]interface{}) error) error
}

// 兼容旧版 Nyanpass 的字段别名表。
//
// Nyanpass 在不同大版本里改过列名（例如 ssh_port → listen_port），
// 迁移工具必须同时认这些名字，否则会把「能迁的数据」误判为字段缺失。
var (
	aliasPort      = []string{"listen_port", "ssh_port", "port"}
	aliasNodeIDs   = []string{"node_ids", "node_list", "server_ids", "nodes"}
	aliasGroupIDs  = []string{"group_ids", "server_group_ids", "node_group_ids"}
	aliasNodeID    = []string{"node_id", "server_id"}
	aliasPassword  = []string{"password_hash", "password", "passwd"}
	aliasTargetPos = []string{"target_port", "remote_port", "dest_port", "to_port"}
	aliasTargetHst = []string{"target_host", "remote_host", "dest_host", "to_host"}
)

// pickColumn 按别名优先级挑选第一个存在的列名。
//
// 参数 cols 为小写列名集合；names 为候选列名（按优先级排序）。
// 返回值：命中的原始列名与是否存在。
func pickColumn(cols map[string]bool, names ...string) (string, bool) {
	for _, n := range names {
		if cols[strings.ToLower(n)] {
			return n, true
		}
	}
	return "", false
}

// rowGet 按别名取列值，取不到时返回 nil。
//
// 先做一次大小写不敏感的精确匹配，再退化为遍历查找，
// 因为部分驱动的列名大小写与建表语句并不一致。
func rowGet(row map[string]interface{}, names ...string) interface{} {
	for _, n := range names {
		if v, ok := row[n]; ok && v != nil {
			return v
		}
	}
	for _, n := range names {
		for k, v := range row {
			if strings.EqualFold(k, n) && v != nil {
				return v
			}
		}
	}
	return nil
}

// asString 把列值转成字符串。
func asString(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	case time.Time:
		return x.UTC().Format(time.RFC3339)
	case bool:
		if x {
			return "1"
		}
		return "0"
	default:
		return fmt.Sprintf("%v", x)
	}
}

// asInt 把列值转成 int，失败时返回 0。
func asInt(v interface{}) int { return int(asInt64(v)) }

// asInt64 把列值转成 int64，失败时返回 0。
func asInt64(v interface{}) int64 {
	switch x := v.(type) {
	case nil:
		return 0
	case int:
		return int64(x)
	case int8:
		return int64(x)
	case int16:
		return int64(x)
	case int32:
		return int64(x)
	case int64:
		return x
	case uint:
		return int64(x)
	case uint8:
		return int64(x)
	case uint16:
		return int64(x)
	case uint32:
		return int64(x)
	case uint64:
		return int64(x)
	case float32:
		return int64(x)
	case float64:
		return int64(x)
	case bool:
		if x {
			return 1
		}
		return 0
	case []byte:
		return parseInt64String(string(x))
	case string:
		return parseInt64String(x)
	default:
		return 0
	}
}

// parseInt64String 从字符串里解析整数，遇到非数字字符即停止。
func parseInt64String(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	var v int64
	started := false
	for _, c := range s {
		if c < '0' || c > '9' {
			if started {
				break
			}
			continue
		}
		started = true
		v = v*10 + int64(c-'0')
	}
	if !started {
		return 0
	}
	if neg {
		return -v
	}
	return v
}

// asFloat 把列值转成 float64，失败时返回 0。
func asFloat(v interface{}) float64 {
	switch x := v.(type) {
	case nil:
		return 0
	case float32:
		return float64(x)
	case float64:
		return x
	case string:
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(x), "%g", &f); err != nil {
			return 0
		}
		return f
	case []byte:
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(string(x)), "%g", &f); err != nil {
			return 0
		}
		return f
	default:
		return float64(asInt64(x))
	}
}

// asBool 把列值转成布尔。
//
// 兼容 MySQL 的 TINYINT(1)、SQLite 的 0/1 整数与 "true"/"yes" 之类的字符串。
func asBool(v interface{}) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		s := strings.ToLower(strings.TrimSpace(x))
		return s == "1" || s == "true" || s == "yes" || s == "on"
	case []byte:
		return asBool(string(x))
	default:
		return asInt64(x) != 0
	}
}

// jsonOrNull 把任意值规范化为合法 JSON 文本。
//
// 已经合法的 JSON 原样返回（保持与源库逐字节一致，满足「原样迁移」要求）；
// 非法时尝试包装为 JSON 字符串；再失败则返回 "null"。
func jsonOrNull(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		if util.IsValidJSON(x) {
			return x
		}
		if strings.TrimSpace(x) == "" {
			return "null"
		}
		b, err := json.Marshal(x)
		if err != nil {
			return "null"
		}
		return string(b)
	case []byte:
		return jsonOrNull(string(x))
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return "null"
		}
		return string(b)
	}
}

// parseListValue 把「JSON 数组 / 逗号分隔」两种存法统一解析为 ID 列表。
//
// 源库里有两种存法：新版存 JSON 数组，旧版存逗号分隔的整数串，两种都要认。
func parseListValue(v interface{}) []uint64 {
	raw := strings.TrimSpace(asString(v))
	if raw == "" || raw == "null" {
		return []uint64{}
	}
	if util.IsValidJSON(raw) {
		var ids []uint64
		if err := json.Unmarshal([]byte(raw), &ids); err == nil {
			if ids == nil {
				return []uint64{}
			}
			return ids
		}
	}
	ids, _ := util.ParseIntList(raw)
	return ids
}

// asTime 把列值转成 UTC 时间，失败时返回零值。
//
// 规格书 7.3 要求时间字段统一转 UTC 后写入；这里同时兼容
// time.Time（驱动已解析）、Unix 秒/毫秒整数与常见字符串格式。
func asTime(v interface{}) time.Time {
	switch x := v.(type) {
	case nil:
		return time.Time{}
	case time.Time:
		return x.UTC()
	case int64:
		return unixToTime(x)
	case int:
		return unixToTime(int64(x))
	case float64:
		return unixToTime(int64(x))
	case []byte:
		return parseTimeString(string(x))
	case string:
		return parseTimeString(x)
	default:
		return time.Time{}
	}
}

// unixToTime 按数量级推断 Unix 秒 / 毫秒 / 微秒 / 纳秒。
func unixToTime(n int64) time.Time {
	if n <= 0 {
		return time.Time{}
	}
	switch {
	case n > 1e17: // 纳秒
		return time.Unix(0, n).UTC()
	case n > 1e14: // 微秒
		return time.Unix(0, n*1e3).UTC()
	case n > 1e11: // 毫秒
		return time.UnixMilli(n).UTC()
	default:
		return time.Unix(n, 0).UTC()
	}
}

// parseTimeString 解析常见的时间字符串格式，统一返回 UTC 时间。
func parseTimeString(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" || s == "0000-00-00 00:00:00" {
		return time.Time{}
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"2006-01-02 15:04:05 -0700 MST",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		"2006-01-02",
	}
	for _, l := range layouts {
		if t, err := time.ParseInLocation(l, s, time.UTC); err == nil {
			return t.UTC()
		}
	}
	// 纯数字字符串按 Unix 时间戳处理。
	if v := parseInt64String(s); v > 0 {
		return unixToTime(v)
	}
	return time.Time{}
}

// timePtr 把时间转成指针，零值返回 nil。
//
// 「无值」与「零值时间」在语义上不同，因此入库前统一用 nil 表示缺失。
func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

// tableColumns 把列信息列表转成「小写列名 → 是否存在」的集合。
func tableColumns(cols []ColumnInfo) map[string]bool {
	out := make(map[string]bool, len(cols))
	for _, c := range cols {
		out[strings.ToLower(c.Name)] = true
	}
	return out
}

// isSafeIdent 校验标识符仅含字母数字与下划线。
//
// 拼 SQL 前必须先过这一步：所有表名都会直接进入 SQL 文本，
// 不做校验就等于把注入口留在迁移工具里。
func isSafeIdent(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
		default:
			return false
		}
	}
	return true
}

// listTableNames 按方言读取库中的全部表名。
//
// 规格书 7.2 阶段 1 需要打印「表数量」，因此这里统计的是全库而不只是
// Nyanpass 相关的表。返回结果已排序，保证报告输出稳定。
func listTableNames(ctx context.Context, db *gorm.DB, dialect string) ([]string, error) {
	var names []string
	switch dialect {
	case "mysql":
		if err := db.WithContext(ctx).
			Raw("select table_name from information_schema.tables where table_schema = database()").
			Scan(&names).Error; err != nil {
			return nil, err
		}
	case "postgres":
		if err := db.WithContext(ctx).
			Raw("select table_name from information_schema.tables where table_schema = current_schema()").
			Scan(&names).Error; err != nil {
			return nil, err
		}
	default:
		if err := db.WithContext(ctx).
			Raw("select name from sqlite_master where type = 'table' and name not like 'sqlite_%'").
			Scan(&names).Error; err != nil {
			return nil, err
		}
	}
	sort.Strings(names)
	return names, nil
}

// tableColumnsOf 按方言读取一张表的列信息。
func tableColumnsOf(ctx context.Context, db *gorm.DB, dialect, table string) ([]ColumnInfo, error) {
	if !isSafeIdent(table) {
		return nil, fmt.Errorf("非法的表名 %q", table)
	}
	switch dialect {
	case "mysql":
		type row struct {
			ColumnName string `gorm:"column:COLUMN_NAME"`
			DataType   string `gorm:"column:DATA_TYPE"`
		}
		var rows []row
		if err := db.WithContext(ctx).Raw(
			"select COLUMN_NAME, DATA_TYPE from information_schema.columns "+
				"where table_schema = database() and table_name = ? order by ordinal_position", table).
			Scan(&rows).Error; err != nil {
			return nil, err
		}
		out := make([]ColumnInfo, 0, len(rows))
		for _, r := range rows {
			out = append(out, ColumnInfo{Name: r.ColumnName, Type: r.DataType})
		}
		return out, nil

	case "postgres":
		type row struct {
			ColumnName string `gorm:"column:column_name"`
			DataType   string `gorm:"column:data_type"`
		}
		var rows []row
		if err := db.WithContext(ctx).Raw(
			"select column_name, data_type from information_schema.columns "+
				"where table_schema = current_schema() and table_name = ? order by ordinal_position", table).
			Scan(&rows).Error; err != nil {
			return nil, err
		}
		out := make([]ColumnInfo, 0, len(rows))
		for _, r := range rows {
			out = append(out, ColumnInfo{Name: r.ColumnName, Type: r.DataType})
		}
		return out, nil

	default:
		type row struct {
			Name string `gorm:"column:name"`
			Type string `gorm:"column:type"`
		}
		// PRAGMA 不支持参数占位符，表名已通过 isSafeIdent 校验。
		var rows []row
		if err := db.WithContext(ctx).Raw(fmt.Sprintf("pragma table_info(%s)", table)).Scan(&rows).Error; err != nil {
			return nil, err
		}
		out := make([]ColumnInfo, 0, len(rows))
		for _, r := range rows {
			out = append(out, ColumnInfo{Name: r.Name, Type: r.Type})
		}
		return out, nil
	}
}

// countTableRows 统计表行数，表不存在或出错时返回 0。
func countTableRows(ctx context.Context, db *gorm.DB, table string) int64 {
	if !isSafeIdent(table) {
		return 0
	}
	var n int64
	if err := db.WithContext(ctx).Raw("select count(*) from " + table).Scan(&n).Error; err != nil {
		return 0
	}
	return n
}

// scanTableRows 是跨方言的流式行扫描实现。
//
// 用 database/sql 的 Rows 而不是 Gorm 的 Find，是为了让流量表这种大表
// 不必一次性全部装载进内存（规格书 7.2 阶段 2「逐表扫描」）。
func scanTableRows(ctx context.Context, db *gorm.DB, table string, callback func(row map[string]interface{}) error) error {
	if !isSafeIdent(table) {
		return fmt.Errorf("非法的表名 %q", table)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	rows, err := sqlDB.QueryContext(ctx, "select * from "+table)
	if err != nil {
		return err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	for rows.Next() {
		values := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		row := make(map[string]interface{}, len(cols))
		for i, c := range cols {
			row[c] = normalizeScanValue(values[i])
		}
		if err := callback(row); err != nil {
			return err
		}
	}
	return rows.Err()
}

// normalizeScanValue 把驱动返回的底层类型规范化。
//
// 主要是把 []byte 转成 string：SQLite 与 MySQL 驱动对 TEXT 列几乎都返回
// []byte，不转的话后面所有字符串处理都要散落类型断言。
func normalizeScanValue(v interface{}) interface{} {
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return v
}

// ---------------------------------------------------------------------------
// OpenRoute 数据源（供 -copy-database 使用）
// ---------------------------------------------------------------------------

// OpenRouteSource 把另一个 OpenRoute 库当作数据源读取。
//
// 结构升级交给 AutoMigrate，本类型只负责「发现表 / 列 / 行」，
// 真正的逐表复制由 CopyDatabase 完成。
type OpenRouteSource struct {
	db      *gorm.DB
	dialect *database.Dialect
}

// NewOpenRouteSource 构造 OpenRoute 数据源。
func NewOpenRouteSource(db *database.DB) *OpenRouteSource {
	return &OpenRouteSource{db: db.DB, dialect: db.Dialect}
}

// DialectName 返回源库方言。
func (s *OpenRouteSource) DialectName() string {
	if s.dialect == nil {
		return "sqlite"
	}
	return s.dialect.Name
}

// Display 返回脱敏后的可读地址。
func (s *OpenRouteSource) Display() string {
	if s.dialect == nil {
		return ""
	}
	return s.dialect.Display
}

// Tables 返回源库中的全部表。
func (s *OpenRouteSource) Tables(ctx context.Context) (map[string]bool, error) {
	names, err := listTableNames(ctx, s.db, s.DialectName())
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[strings.ToLower(n)] = true
	}
	return out, nil
}

// Columns 返回指定表的列信息。
func (s *OpenRouteSource) Columns(ctx context.Context, table string) ([]ColumnInfo, error) {
	return tableColumnsOf(ctx, s.db, s.DialectName(), table)
}

// CountRows 返回指定表的行数。
func (s *OpenRouteSource) CountRows(ctx context.Context, table string) (int64, error) {
	return countTableRows(ctx, s.db, table), nil
}

// SchemaVersion 读取源库的 schema_version 最高版本。
func (s *OpenRouteSource) SchemaVersion(ctx context.Context) (int, string) {
	var v int
	if err := s.db.WithContext(ctx).Raw("select coalesce(max(version), 0) from schema_version").Scan(&v).Error; err != nil {
		return 0, ""
	}
	return v, ""
}

// ScanRows 流式扫描指定表。
func (s *OpenRouteSource) ScanRows(ctx context.Context, table string, callback func(row map[string]interface{}) error) error {
	return scanTableRows(ctx, s.db, table, callback)
}

// ---------------------------------------------------------------------------
// Nyanpass 数据源
// ---------------------------------------------------------------------------

// NyanpassSource 读取一个 Nyanpass 面板的数据库。
type NyanpassSource struct {
	db      *gorm.DB
	dialect *database.Dialect
	// tables 缓存源库的表名集合，避免重复查询元数据
	tables map[string]bool
	// cols 缓存「表 → 列集合」
	cols map[string]map[string]bool
}

// NewNyanpassSource 构造 Nyanpass 数据源。
func NewNyanpassSource(db *database.DB) *NyanpassSource {
	return &NyanpassSource{
		db:      db.DB,
		dialect: db.Dialect,
		tables:  map[string]bool{},
		cols:    map[string]map[string]bool{},
	}
}

// DialectName 返回源库方言。
func (s *NyanpassSource) DialectName() string {
	if s.dialect == nil {
		return "sqlite"
	}
	return s.dialect.Name
}

// Display 返回脱敏后的可读地址。
func (s *NyanpassSource) Display() string {
	if s.dialect == nil {
		return ""
	}
	return s.dialect.Display
}

// Tables 返回源库中的全部表。
func (s *NyanpassSource) Tables(ctx context.Context) (map[string]bool, error) {
	names, err := listTableNames(ctx, s.db, s.DialectName())
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[strings.ToLower(n)] = true
	}
	s.tables = out
	return out, nil
}

// ensureTables 保证表名集合已加载。
func (s *NyanpassSource) ensureTables(ctx context.Context) {
	if s.tables == nil {
		s.tables = map[string]bool{}
	}
	if len(s.tables) == 0 {
		_, _ = s.Tables(ctx)
	}
}

// Columns 返回指定表的列信息。
func (s *NyanpassSource) Columns(ctx context.Context, table string) ([]ColumnInfo, error) {
	cols, err := tableColumnsOf(ctx, s.db, s.DialectName(), table)
	if err != nil {
		return nil, err
	}
	s.cols[strings.ToLower(table)] = tableColumns(cols)
	return cols, nil
}

// columnSet 返回「表 → 小写列名集合」，结果会缓存。
//
// 元数据查询很廉价但调用频繁（每个字段映射都要问一遍），缓存是必要的；
// 读取失败时缓存一个空集合，避免同一张缺失的表被反复查询。
func (s *NyanpassSource) columnSet(ctx context.Context, table string) map[string]bool {
	if cached, ok := s.cols[strings.ToLower(table)]; ok {
		return cached
	}
	cols, err := tableColumnsOf(ctx, s.db, s.DialectName(), table)
	set := map[string]bool{}
	if err == nil {
		set = tableColumns(cols)
	}
	s.cols[strings.ToLower(table)] = set
	return set
}

// CountRows 返回指定表的行数。
func (s *NyanpassSource) CountRows(ctx context.Context, table string) (int64, error) {
	return countTableRows(ctx, s.db, table), nil
}

// ScanRows 流式扫描指定表。
func (s *NyanpassSource) ScanRows(ctx context.Context, table string, callback func(row map[string]interface{}) error) error {
	return scanTableRows(ctx, s.db, table, callback)
}

// SchemaVersion 读取 Nyanpass 的结构版本。
//
// Nyanpass 的版本记录方式在不同大版本间有差异，因此做了三级兜底：
//  1. 优先读 schema_version 表（新版）；
//  2. 退回 system_settings / settings 表里的 version 键（旧版）；
//  3. 都读不到返回 0，由调用方按「未记录版本」处理。
func (s *NyanpassSource) SchemaVersion(ctx context.Context) (int, string) {
	s.ensureTables(ctx)

	if s.tables["schema_version"] {
		cols := s.columnSet(ctx, "schema_version")
		if cols["version"] {
			type vrow struct {
				Version      int    `gorm:"column:version"`
				SchemaVer    string `gorm:"column:schema_version"`
				VersionLabel string `gorm:"column:version_name"`
			}
			var out vrow
			if err := s.db.WithContext(ctx).
				Raw("select * from schema_version order by version desc limit 1").
				Scan(&out).Error; err == nil {
				if out.Version > 0 {
					label := out.VersionLabel
					if label == "" {
						label = out.SchemaVer
					}
					if label == "" {
						label = fmt.Sprintf("Nyanpass %d", out.Version)
					}
					return out.Version, label
				}
				if out.VersionLabel != "" {
					return 0, out.VersionLabel
				}
			}
		}
	}

	// 旧版把版本号塞在设置表里。
	for _, t := range []string{"system_settings", "settings", "configs"} {
		if !s.tables[t] {
			continue
		}
		cols := s.columnSet(ctx, t)
		keyCol := ""
		switch {
		case cols["key"]:
			keyCol = "key"
		case cols["name"]:
			keyCol = "name"
		}
		if keyCol == "" || !cols["value"] {
			continue
		}
		var raw string
		q := fmt.Sprintf("select value from %s where %s in ('schema_version','version','panel_version') limit 1", t, keyCol)
		if err := s.db.WithContext(ctx).Raw(q).Scan(&raw).Error; err != nil || raw == "" {
			continue
		}
		raw = strings.Trim(raw, `"' `)
		if v := parseInt64String(raw); v > 0 {
			return int(v), raw
		}
		if raw != "" {
			return 0, raw
		}
	}
	return 0, ""
}

// 以下是 Nyanpass 侧的行结构，字段只为「把数据读出来」而存在。
//
// 之所以不复用 model/*：源库的列名与目标模型差异很大（倍率是设备组单列而非
// 规则双列、节点列表叫 node_list 等），硬套模型会掩盖真实差异、反而容易迁错数据。

// nyUser 对应 Nyanpass 的 users 表。
type nyUser struct {
	ID       uint64
	Username string
	Password string
	Nickname string
	Role     string
	Status   int
	GroupID  uint64
	Token    string

	TrafficUsed  int64
	TrafficLimit int64
	SpeedLimit   int64
	IPLimit      int
	DeviceLimit  int
	ConnLimit    int
	ExpireAt     *time.Time
	LastLoginAt  *time.Time
	LastLoginIP  string
	Remark       string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// nyUserGroup 对应 Nyanpass 的 user_groups 表。
type nyUserGroup struct {
	ID           uint64
	Name         string
	TrafficLimit int64
	SpeedLimit   int64
	IPLimit      int
	ConnLimit    int
	RuleGroupIDs []uint64
	Remark       string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// nyNode 对应 Nyanpass 的 nodes / servers 表。
type nyNode struct {
	ID          uint64
	Name        string
	Token       string
	Role        string
	PublicIPv4  string
	PublicIPv6  string
	PrivateIP   string
	ConnectHost string
	IsStatic    bool
	DirectPort  int
	WsPort      int
	TlsPort     int
	UdpPort     int
	RevPort     int
	Weight      int
	MaxConn     int
	GroupIDs    []uint64
	Remark      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// nyNodeGroup 对应 Nyanpass 的 node_groups / server_groups 表。
type nyNodeGroup struct {
	ID        uint64
	Name      string
	Remark    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// nyDeviceGroup 对应 Nyanpass 的 device_groups / groups 表。
type nyDeviceGroup struct {
	ID         uint64
	Name       string
	Type       string
	NodeIDs    []uint64
	Config     string
	Multiplier float64
	Remark     string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// nyRuleGroup 对应 Nyanpass 的 rule_groups 表。
type nyRuleGroup struct {
	ID        uint64
	Name      string
	Sort      int
	Remark    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// nyRule 对应 Nyanpass 的 rules / forward_rules 表。
type nyRule struct {
	ID             uint64
	Name           string
	UserID         uint64
	RuleGroupID    uint64
	InboundGroupID uint64
	Port           int
	PortEnd        int
	TargetHost     string
	TargetPort     int
	TargetWeight   int
	Targets        string
	Balance        string
	OutboundGroup  uint64
	ChainGroups    []uint64
	ReverseEnable  bool
	ReversePort    int
	ReverseGroup   uint64
	IsSubRule      bool
	ParentID       uint64
	SNI            string
	Shaping        string
	Options        string
	Enable         bool
	Remark         string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// nyTraffic 是读取流量表时的中间结构（尚未聚合）。
type nyTraffic struct {
	Date      string
	Hour      int
	UserID    uint64
	RuleID    uint64
	NodeID    uint64
	Direction string
	RawBytes  int64
	Bytes     int64
}

// loadUsers 读取用户。
func (s *NyanpassSource) loadUsers(ctx context.Context) ([]nyUser, error) {
	cols := s.columnSet(ctx, tableUsers)
	pwdCol, _ := pickColumn(cols, aliasPassword...)

	out := []nyUser{}
	err := s.ScanRows(ctx, tableUsers, func(row map[string]interface{}) error {
		u := nyUser{
			ID:           uint64(asInt64(rowGet(row, "id"))),
			Username:     asString(rowGet(row, "username", "name")),
			Password:     asString(rowGet(row, pwdCol)),
			Nickname:     asString(rowGet(row, "nickname", "nick", "display_name")),
			Role:         strings.ToLower(asString(rowGet(row, "role", "is_admin"))),
			Status:       asInt(rowGet(row, "status", "enable", "enabled")),
			GroupID:      uint64(asInt64(rowGet(row, "group_id", "user_group_id"))),
			Token:        asString(rowGet(row, "token", "sub_token", "subscribe_token")),
			TrafficUsed:  asInt64(rowGet(row, "traffic_used", "used_traffic", "upload_traffic")),
			TrafficLimit: asInt64(rowGet(row, "traffic_limit", "transfer_enable")),
			SpeedLimit:   speedBytes(rowGet(row, "speed_limit", "speed", "rate")),
			IPLimit:      asInt(rowGet(row, "ip_limit", "ip_count", "max_ip")),
			DeviceLimit:  asInt(rowGet(row, "device_limit", "device_count")),
			ConnLimit:    asInt(rowGet(row, "conn_limit", "connection_limit", "max_conn")),
			LastLoginAt:  timePtr(asTime(rowGet(row, "last_login_at", "last_login"))),
			LastLoginIP:  asString(rowGet(row, "last_login_ip", "login_ip", "last_ip")),
			Remark:       asString(rowGet(row, "remark", "note")),
			CreatedAt:    asTime(rowGet(row, "created_at")),
			UpdatedAt:    asTime(rowGet(row, "updated_at")),
		}
		u.ExpireAt = timePtr(asTime(rowGet(row, "expire_at", "expired_at", "expire_time")))

		// 部分旧版把「管理员」写成 1/0 的 is_admin 列而不是 role 字符串。
		if u.Role == "1" || u.Role == "true" {
			u.Role = model.RoleAdmin
		}
		if u.Role != model.RoleAdmin {
			u.Role = model.RoleUser
		}
		out = append(out, u)
		return nil
	})
	return out, err
}

// speedBytes 把限速列规范化为 byte/s。
//
// Nyanpass 的限速单位在不同版本里有 KB/s 与 byte/s 两种写法，
// 这里按「数值很小则视为 KB/s」的惯例折算，与源面板的展示口径保持一致。
func speedBytes(v interface{}) int64 {
	n := asInt64(v)
	if n <= 0 {
		return 0
	}
	// 小于 100000 的量级几乎不可能是 byte/s 限速（低于 100KB/s），按 KB/s 处理。
	if n < 100000 {
		return n * 1024
	}
	return n
}

// loadUserGroups 读取用户分组。
func (s *NyanpassSource) loadUserGroups(ctx context.Context) ([]nyUserGroup, error) {
	out := []nyUserGroup{}
	err := s.ScanRows(ctx, tableUserGroups, func(row map[string]interface{}) error {
		out = append(out, nyUserGroup{
			ID:           uint64(asInt64(rowGet(row, "id"))),
			Name:         asString(rowGet(row, "name", "group_name", "title")),
			TrafficLimit: asInt64(rowGet(row, "traffic_limit", "transfer_enable")),
			SpeedLimit:   speedBytes(rowGet(row, "speed_limit", "speed", "rate")),
			IPLimit:      asInt(rowGet(row, "ip_limit", "ip_count")),
			ConnLimit:    asInt(rowGet(row, "conn_limit", "connection_limit")),
			RuleGroupIDs: parseListValue(rowGet(row, "rule_group_ids", "rule_groups", "group_list")),
			Remark:       asString(rowGet(row, "remark", "note")),
			CreatedAt:    asTime(rowGet(row, "created_at")),
			UpdatedAt:    asTime(rowGet(row, "updated_at")),
		})
		return nil
	})
	return out, err
}

// loadNodes 读取节点。表名兼容 nodes 与 servers 两种历史命名。
func (s *NyanpassSource) loadNodes(ctx context.Context) ([]nyNode, error) {
	s.ensureTables(ctx)

	table := tableNodes
	cols := s.columnSet(ctx, table)
	if len(cols) == 0 && s.tables[tableServers] {
		table = tableServers
		cols = s.columnSet(ctx, table)
	}
	portCol, _ := pickColumn(cols, aliasPort...)
	groupCol, _ := pickColumn(cols, aliasGroupIDs...)

	out := []nyNode{}
	err := s.ScanRows(ctx, table, func(row map[string]interface{}) error {
		n := nyNode{
			ID:          uint64(asInt64(rowGet(row, "id"))),
			Name:        asString(rowGet(row, "name", "server_name", "title")),
			Token:       asString(rowGet(row, "token", "node_token", "key", "secret")),
			Role:        strings.ToLower(asString(rowGet(row, "role", "node_type", "type"))),
			PublicIPv4:  asString(rowGet(row, "public_ipv4", "ip", "ipv4", "public_ip")),
			PublicIPv6:  asString(rowGet(row, "public_ipv6", "ipv6")),
			PrivateIP:   asString(rowGet(row, "private_ip", "inner_ip", "intranet_ip")),
			ConnectHost: asString(rowGet(row, "connect_host", "connect_address", "host")),
			IsStatic:    asBool(rowGet(row, "is_static", "static")),
			DirectPort:  asInt(rowGet(row, "direct_port")),
			WsPort:      asInt(rowGet(row, "ws_port")),
			TlsPort:     asInt(rowGet(row, "tls_port")),
			UdpPort:     asInt(rowGet(row, "udp_port")),
			RevPort:     asInt(rowGet(row, "rev_port", "reverse_port")),
			Weight:      asInt(rowGet(row, "weight", "default_weight")),
			MaxConn:     asInt(rowGet(row, "max_conn", "max_connection")),
			Remark:      asString(rowGet(row, "remark", "note")),
			CreatedAt:   asTime(rowGet(row, "created_at")),
			UpdatedAt:   asTime(rowGet(row, "updated_at")),
		}
		// 没有独立端口列时，退回通用的 port 列作为监听端口。
		if n.DirectPort == 0 && portCol != "" {
			n.DirectPort = asInt(rowGet(row, portCol))
		}
		if groupCol != "" {
			n.GroupIDs = parseListValue(rowGet(row, groupCol))
		}
		// 单分组列（group_id）也要认。
		if len(n.GroupIDs) == 0 {
			if gid := uint64(asInt64(rowGet(row, "group_id", "node_group_id"))); gid > 0 {
				n.GroupIDs = []uint64{gid}
			}
		}
		// 角色缺失时按双端处理，避免迁移后节点无法承担任何角色。
		switch n.Role {
		case model.RoleInbound, model.RoleOutbound, model.RoleBoth:
		default:
			n.Role = model.RoleBoth
		}
		if n.Weight <= 0 {
			n.Weight = 1
		}
		out = append(out, n)
		return nil
	})
	return out, err
}

// loadNodeGroups 读取节点分组，兼容 node_groups 与 server_groups。
func (s *NyanpassSource) loadNodeGroups(ctx context.Context) ([]nyNodeGroup, error) {
	s.ensureTables(ctx)

	table := tableNodeGroups
	if len(s.columnSet(ctx, table)) == 0 && s.tables[tableServerGroups] {
		table = tableServerGroups
	}
	out := []nyNodeGroup{}
	err := s.ScanRows(ctx, table, func(row map[string]interface{}) error {
		out = append(out, nyNodeGroup{
			ID:        uint64(asInt64(rowGet(row, "id"))),
			Name:      asString(rowGet(row, "name", "group_name", "title")),
			Remark:    asString(rowGet(row, "remark", "note")),
			CreatedAt: asTime(rowGet(row, "created_at")),
			UpdatedAt: asTime(rowGet(row, "updated_at")),
		})
		return nil
	})
	return out, err
}

// loadDeviceGroups 读取设备组。表名兼容 device_groups 与 groups。
func (s *NyanpassSource) loadDeviceGroups(ctx context.Context) ([]nyDeviceGroup, error) {
	s.ensureTables(ctx)

	table := tableDeviceGroups
	cols := s.columnSet(ctx, table)
	if len(cols) == 0 && s.tables[tableGroups] {
		table = tableGroups
		cols = s.columnSet(ctx, table)
	}
	nodeCol, _ := pickColumn(cols, aliasNodeIDs...)

	out := []nyDeviceGroup{}
	err := s.ScanRows(ctx, table, func(row map[string]interface{}) error {
		g := nyDeviceGroup{
			ID:         uint64(asInt64(rowGet(row, "id"))),
			Name:       asString(rowGet(row, "name", "group_name", "title")),
			Type:       strings.ToLower(asString(rowGet(row, "type", "group_type", "kind"))),
			Config:     jsonOrNull(rowGet(row, "config", "config_json", "settings")),
			Multiplier: asFloat(rowGet(row, "multiplier", "rate", "ratio")),
			Remark:     asString(rowGet(row, "remark", "note")),
			CreatedAt:  asTime(rowGet(row, "created_at")),
			UpdatedAt:  asTime(rowGet(row, "updated_at")),
		}
		if nodeCol != "" {
			g.NodeIDs = parseListValue(rowGet(row, nodeCol))
		}
		if len(g.NodeIDs) == 0 {
			g.NodeIDs = parseListValue(rowGet(row, "node_list", "node_ids", "nodes"))
		}
		// type 列为空时用 inbound 布尔列推断，避免整组设备组被判成出口组。
		if g.Type != "inbound" && g.Type != "outbound" {
			if asBool(rowGet(row, "inbound", "is_inbound")) {
				g.Type = "inbound"
			} else {
				g.Type = "outbound"
			}
		}
		out = append(out, g)
		return nil
	})
	return out, err
}

// loadRuleGroups 读取规则分组。
func (s *NyanpassSource) loadRuleGroups(ctx context.Context) ([]nyRuleGroup, error) {
	out := []nyRuleGroup{}
	err := s.ScanRows(ctx, tableRuleGroups, func(row map[string]interface{}) error {
		out = append(out, nyRuleGroup{
			ID:        uint64(asInt64(rowGet(row, "id"))),
			Name:      asString(rowGet(row, "name", "group_name", "title")),
			Sort:      asInt(rowGet(row, "sort", "order", "weight", "sort_order")),
			Remark:    asString(rowGet(row, "remark", "note")),
			CreatedAt: asTime(rowGet(row, "created_at")),
			UpdatedAt: asTime(rowGet(row, "updated_at")),
		})
		return nil
	})
	return out, err
}

// loadRules 读取转发规则，兼容 rules 与 forward_rules 两种表名。
func (s *NyanpassSource) loadRules(ctx context.Context) ([]nyRule, error) {
	s.ensureTables(ctx)

	table := tableRules
	cols := s.columnSet(ctx, table)
	if len(cols) == 0 && s.tables[tableForwardRules] {
		table = tableForwardRules
		cols = s.columnSet(ctx, table)
	}
	portCol, _ := pickColumn(cols, aliasPort...)
	hostCol, _ := pickColumn(cols, aliasTargetHst...)
	tportCol, _ := pickColumn(cols, aliasTargetPos...)

	out := []nyRule{}
	err := s.ScanRows(ctx, table, func(row map[string]interface{}) error {
		r := nyRule{
			ID:             uint64(asInt64(rowGet(row, "id"))),
			Name:           asString(rowGet(row, "name", "rule_name", "title")),
			UserID:         uint64(asInt64(rowGet(row, "user_id", "uid"))),
			RuleGroupID:    uint64(asInt64(rowGet(row, "rule_group_id", "rule_group"))),
			InboundGroupID: uint64(asInt64(rowGet(row, "inbound_group_id", "listen_group_id"))),
			Port:           asInt(rowGet(row, portCol)),
			PortEnd:        asInt(rowGet(row, "listen_port_end", "port_end", "end_port")),
			TargetHost:     asString(rowGet(row, hostCol)),
			TargetPort:     asInt(rowGet(row, tportCol)),
			TargetWeight:   asInt(rowGet(row, "target_weight", "weight")),
			Targets:        jsonOrNull(rowGet(row, "targets", "target_list", "remote_targets")),
			Balance:        strings.ToLower(asString(rowGet(row, "target_balance", "balance", "lb"))),
			OutboundGroup:  uint64(asInt64(rowGet(row, "outbound_group_id", "out_group_id"))),
			ReverseEnable:  asBool(rowGet(row, "reverse_enable", "reverse")),
			ReversePort:    asInt(rowGet(row, "reverse_port", "rev_port")),
			ReverseGroup:   uint64(asInt64(rowGet(row, "reverse_group_id"))),
			IsSubRule:      asBool(rowGet(row, "is_sub_rule", "sub_rule")),
			ParentID:       uint64(asInt64(rowGet(row, "parent_id", "main_rule_id"))),
			SNI:            asString(rowGet(row, "sni", "server_name")),
			Shaping:        jsonOrNull(rowGet(row, "shaping", "shaping_list")),
			Options:        jsonOrNull(rowGet(row, "options", "tls", "protocol_config")),
			Remark:         asString(rowGet(row, "remark", "note")),
			CreatedAt:      asTime(rowGet(row, "created_at")),
			UpdatedAt:      asTime(rowGet(row, "updated_at")),
		}
		// 规则分组列缺失时退回通用 group_id，但要注意不要与入口组列冲突：
		// 只有 inbound_group_id 不存在时才允许用 group_id 兜底。
		if r.RuleGroupID == 0 {
			if _, hasInbound := cols["inbound_group_id"]; !hasInbound {
				r.RuleGroupID = uint64(asInt64(rowGet(row, "group_id")))
			}
		}
		if r.InboundGroupID == 0 {
			r.InboundGroupID = uint64(asInt64(rowGet(row, "group_id", "listen_group_id")))
		}
		if v := rowGet(row, "enable", "enabled", "status"); v != nil {
			r.Enable = asBool(v)
		} else {
			// 列缺失时默认启用，避免迁移后规则全部处于停用状态。
			r.Enable = true
		}
		r.ChainGroups = parseListValue(rowGet(row, "chain_groups", "chain_group_ids"))
		if len(r.ChainGroups) == 0 {
			r.ChainGroups = parseListValue(rowGet(row, "chain_group_id"))
		}
		out = append(out, r)
		return nil
	})
	return out, err
}

// loadTrafficRows 读取流量记录并按唯一键聚合。
//
// 规格书 7.2 明确要求按 (date, hour, user_id, rule_id, node_id, direction)
// 聚合后再写入；这里同时返回聚合前的总字节数，便于阶段 6 做
// 「流量总量 ±0.1%」的对比。
func (s *NyanpassSource) loadTrafficRows(ctx context.Context) (map[TrafficKey]*TrafficRow, int64, error) {
	s.ensureTables(ctx)

	table := tableTrafficLogs
	cols := s.columnSet(ctx, table)
	if len(cols) == 0 && s.tables[tableTraffics] {
		table = tableTraffics
		cols = s.columnSet(ctx, table)
	}
	nodeCol, _ := pickColumn(cols, aliasNodeID...)

	agg := map[TrafficKey]*TrafficRow{}
	var total int64

	err := s.ScanRows(ctx, table, func(row map[string]interface{}) error {
		t := nyTraffic{
			Date:      normalizeDate(asString(rowGet(row, "date", "day", "dt"))),
			Hour:      normalizeHour(rowGet(row, "hour", "h")),
			UserID:    uint64(asInt64(rowGet(row, "user_id", "uid"))),
			RuleID:    uint64(asInt64(rowGet(row, "rule_id", "rule"))),
			NodeID:    uint64(asInt64(rowGet(row, nodeCol))),
			Direction: normalizeDirection(asString(rowGet(row, "direction", "flow_type", "type"))),
			RawBytes:  asInt64(rowGet(row, "raw_bytes", "raw")),
			Bytes:     asInt64(rowGet(row, "bytes", "traffic", "total_bytes", "used")),
		}
		// 条目级字段缺失时互相兜底：只填了其中一列也能迁。
		if t.Bytes == 0 {
			t.Bytes = t.RawBytes
		}
		if t.RawBytes == 0 {
			t.RawBytes = t.Bytes
		}
		if t.Date == "" || t.Bytes == 0 {
			// 无日期或无流量的行无法归入任何时间桶，直接丢弃（不计入总量）。
			return nil
		}
		total += t.Bytes

		key := TrafficKey{
			Date: t.Date, Hour: t.Hour, UserID: t.UserID,
			RuleID: t.RuleID, NodeID: t.NodeID, Direction: t.Direction,
		}
		if cur, ok := agg[key]; ok {
			cur.RawBytes += t.RawBytes
			cur.Bytes += t.Bytes
			return nil
		}
		agg[key] = &TrafficRow{
			Date: t.Date, Hour: t.Hour, UserID: t.UserID,
			RuleID: t.RuleID, NodeID: t.NodeID, Direction: t.Direction,
			RawBytes: t.RawBytes, Bytes: t.Bytes,
		}
		return nil
	})
	return agg, total, err
}

// normalizeDate 把日期规范化为 YYYY-MM-DD。
//
// 规格书 7.4：跨年跨月的流量记录按 UTC 日期归属，不做时区重算。
// 这里只做格式规整，不改变日期本身。
func normalizeDate(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) >= 10 && s[4] == '-' && s[7] == '-' {
		return s[:10]
	}
	// 20260101 这类紧凑写法补上分隔符。
	if len(s) == 8 {
		digits := true
		for _, c := range s {
			if c < '0' || c > '9' {
				digits = false
				break
			}
		}
		if digits {
			return s[:4] + "-" + s[4:6] + "-" + s[6:8]
		}
	}
	if t := parseTimeString(s); !t.IsZero() {
		return t.UTC().Format("2006-01-02")
	}
	return s
}

// normalizeHour 规范小时字段。
//
// 规格书 4.2.8：hour 为 0~23 表示按小时聚合，-1 表示按天聚合。
// 源库里的 -1、2525（Nyanpass 用 2525 表示「按天」）、其它越界伪值
// 统一归一到 -1，避免写出非法的小时桶。
func normalizeHour(v interface{}) int {
	h := asInt(v)
	if h < 0 || h > 23 {
		return model.HourDaily
	}
	return h
}

// normalizeDirection 规范流量方向。
func normalizeDirection(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "in", "upload", "up", "ul", "1", "rx":
		return model.DirectionIn
	case "out", "download", "down", "dl", "2", "tx":
		return model.DirectionOut
	case "":
		return model.DirectionIn
	default:
		if strings.Contains(s, "in") || strings.Contains(s, "上") {
			return model.DirectionIn
		}
		return model.DirectionOut
	}
}

// Nyanpass 侧的表名。同时列出新旧两套命名，读取时按存在性择优。
const (
	tableUsers        = "users"
	tableUserGroups   = "user_groups"
	tableNodes        = "nodes"
	tableServers      = "servers"
	tableNodeGroups   = "node_groups"
	tableServerGroups = "server_groups"
	tableDeviceGroups = "device_groups"
	tableGroups       = "groups"
	tableRuleGroups   = "rule_groups"
	tableRules        = "rules"
	tableForwardRules = "forward_rules"
	tableTrafficLogs  = "traffic_logs"
	tableTraffics     = "traffics"
)

// requiredNyanpassTables 是迁移运行 MUST 存在的表。
//
// 缺少任何一张都说明源库版本过旧（规格书 7.4：列出缺失表、退出码 1）。
// 这里刻意只列「缺了就没法迁」的表：节点分组、规则分组、流量表缺失时
// 迁移仍可继续，只是相应数据为空。
var requiredNyanpassTables = []string{
	tableUsers,
	tableUserGroups,
	tableDeviceGroups,
}

// requiredNyanpassColumns 是「表 → 必要列」的清单。
//
// 键为表名，值为该表必须存在的列（按别名组合判断，任一命中即算存在）。
// 用途：识别「表在但结构过旧」的源库，例如 users 表没有 username。
var requiredNyanpassColumns = map[string][][]string{
	tableUsers:        {{"id"}, {"username", "name"}, {"role", "is_admin"}},
	tableDeviceGroups: {{"id"}, {"name", "group_name"}, {"config", "config_json"}},
}
