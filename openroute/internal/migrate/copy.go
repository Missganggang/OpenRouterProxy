package migrate

import (
	"context"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/openroute/openroute/internal/database"
	"github.com/openroute/openroute/internal/util"
)

// 跨库复制的常量（规格书 7.3）。
const (
	// copyBatchSize 是每批写入的行数，规格书要求 1000 行/批。
	copyBatchSize = 1000
	// copyConfirmWord 是用户必须原样输入的确认词。
	copyConfirmWord = "YES"
)

// CopyOptions 是跨数据库转换的参数。
type CopyOptions struct {
	// SourcePath 是源库的 database-path 写法
	SourcePath string
	// Target 是已连接的目标库
	Target *database.DB
	// Force 为 true 时跳过交互确认（对应 --force）
	Force bool
	// Confirm 是交互确认回调，返回 false 表示用户放弃。
	// 非交互模式下应当由调用方提供（或依赖 Force）。
	Confirm func(prompt string) (bool, error)
	// Log 可为空
	Log *zap.Logger
	// Progress 可为空
	Progress ProgressFunc
}

// CopyTableResult 是单表复制的校验结果。
type CopyTableResult struct {
	Table          string `json:"table"`
	SourceRows     int64  `json:"source_rows"`
	TargetRows     int64  `json:"target_rows"`
	RowsCopied     int64  `json:"rows_copied"`
	SkippedRows    int64  `json:"skipped_rows"`
	SourceChecksum string `json:"source_checksum"`
	TargetChecksum string `json:"target_checksum"`
	Passed         bool   `json:"passed"`
	Note           string `json:"note,omitempty"`
}

// CopyReport 是跨库转换的完整报告。
type CopyReport struct {
	Source string `json:"source"`
	Target string `json:"target"`
	// SourceTables / TargetTables 是两端的表数量
	SourceTables int `json:"source_tables"`
	TargetTables int `json:"target_tables"`
	// Tables 是逐表结果
	Tables []CopyTableResult `json:"tables"`
	// TotalRows 是复制的总行数
	TotalRows int64 `json:"total_rows"`
	// SkippedRows 是因类型转换失败（如非法 JSON）被跳过的行数
	SkippedRows int64 `json:"skipped_rows"`
	// Notes 是类型转换说明
	Notes []string `json:"notes"`
	// Passed 表示整体校验是否通过
	Passed bool `json:"passed"`
}

// CopyDatabase 执行跨数据库转换（规格书 7.3，对应 `-copy-database`）。
//
// 行为：
//  1. 打印源库与目标库的信息；
//  2. 打印醒目警告：目标库将被完全覆盖且无法恢复；
//  3. 要求用户输入 YES 确认（Force 为 true 时跳过）；
//  4. 在目标库执行 AutoMigrate；
//  5. 按外键依赖顺序逐表复制，每表分批 1000 行；
//  6. 复制完成后校验每表行数与校验和。
//
// 返回值：复制报告与错误。校验未通过时报告仍然返回，便于调用方打印明细。
func CopyDatabase(ctx context.Context, opt CopyOptions) (*CopyReport, error) {
	if strings.TrimSpace(opt.SourcePath) == "" {
		return nil, fmt.Errorf("缺少源库地址")
	}
	if opt.Target == nil {
		return nil, fmt.Errorf("缺少已连接的目标库")
	}

	srcDialect, err := database.ParseDatabasePath(opt.SourcePath)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSourceUnreachable, err)
	}
	if sameDatabase(srcDialect, opt.Target.Dialect, "") {
		return nil, fmt.Errorf("%w: 源库与目标库都指向 %s", ErrSameDatabase, srcDialect.Display)
	}

	// 打开源库（只读使用）。
	sourceDB, err := database.Open(database.Options{
		Path: opt.SourcePath, MaxOpen: 2, MaxIdle: 1, LogLevel: gormSilent(),
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSourceUnreachable, err)
	}
	defer func() { _ = sourceDB.Close() }()

	src := NewOpenRouteSource(sourceDB)

	// —— 1) 打印两端信息 ——
	srcTables, err := src.Tables(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: 读取源库表清单失败: %v", ErrSourceUnreachable, err)
	}
	dstTables, err := NewOpenRouteSource(opt.Target).Tables(ctx)
	if err != nil {
		// 目标库尚未建表是正常情况（AutoMigrate 会补上），不阻断。
		dstTables = map[string]bool{}
	}

	report := &CopyReport{
		Source:       srcDialect.Display,
		Target:       opt.Target.Dialect.Display,
		SourceTables: len(srcTables),
		TargetTables: len(dstTables),
		Tables:       []CopyTableResult{},
		Notes:        []string{},
	}

	fmt.Println("跨数据库转换")
	fmt.Printf("  源库    : %s（%d 张表）\n", srcDialect.Display, len(srcTables))
	fmt.Printf("  目标库  : %s（%d 张表）\n", opt.Target.Dialect.Display, len(dstTables))
	fmt.Println("  源库行数:")
	for _, t := range copyOrder {
		if !srcTables[t] {
			continue
		}
		n, _ := src.CountRows(ctx, t)
		fmt.Printf("    %-18s %d\n", t, n)
	}
	fmt.Println()

	// —— 2) 醒目警告 ——
	printDestructiveWarning(opt.Target.Dialect.Display)

	// —— 3) 确认 ——
	if !opt.Force {
		if opt.Confirm == nil {
			return nil, fmt.Errorf("目标库将被完全覆盖且无法恢复，需要交互确认；" +
				"非交互模式下请显式加上 --force")
		}
		ok, err := opt.Confirm(fmt.Sprintf(
			"确认要把数据复制到 %s 吗？目标库现有数据将被完全覆盖且无法恢复。\n请输入 %s 继续：",
			opt.Target.Dialect.Display, copyConfirmWord))
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("用户取消了跨库转换")
		}
	}

	// —— 4) 目标库建表 ——
	// 规格书 7.3 第 4 步：自动在目标库执行 AutoMigrate。
	if err := opt.Target.WithContext(ctx).AutoMigrate(database.AllModels()...); err != nil {
		return nil, fmt.Errorf("目标库建表失败: %w", err)
	}

	log := opt.Log
	if log == nil {
		log = zap.NewNop()
	}

	// —— 5) 按外键依赖顺序逐表复制 ——
	for _, table := range copyOrder {
		if !srcTables[table] {
			report.Tables = append(report.Tables, CopyTableResult{
				Table: table, Passed: true, Note: "源库没有这张表，跳过",
			})
			continue
		}
		res, err := copyOneTable(ctx, src, opt, table, log)
		if err != nil {
			return report, err
		}
		report.Tables = append(report.Tables, *res)
		report.TotalRows += res.RowsCopied
		report.SkippedRows += res.SkippedRows
		if !res.Passed {
			report.Passed = false
			// 校验不通过时立即停止，避免在半截状态上继续覆盖更多表。
			return report, fmt.Errorf("表 %s 复制校验未通过: %s", table, res.Note)
		}
		if opt.Progress != nil {
			opt.Progress(Progress{
				Phase: PhaseWrite, Stage: StageMigrate, Table: table,
				Done: report.TotalRows, Total: report.TotalRows,
			})
		}
	}

	report.Passed = true
	report.Notes = append(report.Notes,
		"目标库的原数据已被覆盖，本次复制不做增量合并",
		"表结构由 AutoMigrate 生成；源库中已删除但模型仍保留的列不会出现在目标库")
	if report.SkippedRows > 0 {
		report.Notes = append(report.Notes, fmt.Sprintf(
			"因类型转换失败（例如非法 JSON）跳过了 %d 行，明细见各表结果的 skipped_rows",
			report.SkippedRows))
	}
	return report, nil
}

// printDestructiveWarning 打印醒目的覆盖警告。
//
// 用 ANSI 转义序列上红色：这是规格书 7.3 明确要求的「醒目红色警告」；
// 在不支持颜色的终端上会被当作普通文本忽略，不影响可读性。
func printDestructiveWarning(target string) {
	const (
		red     = "\033[31m"
		boldRed = "\033[1;31m"
		reset   = "\033[0m"
	)

	fmt.Println(red + "╔══════════════════════════════════════════════════════════════╗" + reset)
	fmt.Println(boldRed + "║  警告：目标库将被完全覆盖，且无法恢复                        ║" + reset)
	fmt.Println(red + "╚══════════════════════════════════════════════════════════════╝" + reset)
	fmt.Printf("%s  目标库: %s%s\n", boldRed, target, reset)
	fmt.Println("  该操作会用源库的数据覆盖目标库中的同名表，原有数据不会保留、不能撤销。")
	fmt.Println("  执行前请务必先用 -backup 生成目标库的备份。")
	fmt.Println()
}

// targetTableSchema 描述目标表在「类型转换」这一维度上的关键信息。
type targetTableSchema struct {
	// All 是全部列名（小写）
	All map[string]bool
	// JSON 是需要校验 JSON 合法性的列（小写）
	JSON map[string]bool
	// Bool 是需要显式布尔转换的列（小写）
	Bool map[string]bool
}

// loadTargetSchema 读取目标表的列类型。
//
// 直接问目标库（Migrator.ColumnTypes）而不是反射模型：
// 实际落库的类型才是决定「怎么写进去不报错」的依据 ——
// 例如同一个 model.JSON 字段在 MySQL 是 JSON、在 PG 是 JSONB、在 SQLite 是 TEXT，
// 只有前者需要校验合法性。
func loadTargetSchema(db *database.DB, table string) targetTableSchema {
	out := targetTableSchema{
		All:  map[string]bool{},
		JSON: map[string]bool{},
		Bool: map[string]bool{},
	}
	types, err := db.WithContext(context.Background()).Migrator().ColumnTypes(table)
	if err != nil {
		return out
	}
	for _, c := range types {
		name := strings.ToLower(c.Name())
		out.All[name] = true
		dbType := strings.ToLower(c.DatabaseTypeName())
		switch {
		case strings.Contains(dbType, "json"):
			out.JSON[name] = true
		case dbType == "boolean" || dbType == "bool":
			out.Bool[name] = true
		case strings.Contains(dbType, "tinyint"):
			// MySQL 的 TINYINT(1) 语义上是布尔；TINYINT(4) 是整数，不能当布尔。
			// 不同驱动把显示宽度放在 Length 或 DecimalSize 里，两者都试。
			if length, ok := c.Length(); ok && length <= 1 {
				out.Bool[name] = true
			}
			if precision, scale, ok := c.DecimalSize(); ok && precision <= 1 && scale <= 1 {
				out.Bool[name] = true
			}
		}
	}
	return out
}

// copyOneTable 复制一张表并做行数与校验和对比。
func copyOneTable(
	ctx context.Context, src *OpenRouteSource, opt CopyOptions,
	table string, log *zap.Logger,
) (*CopyTableResult, error) {
	res := &CopyTableResult{Table: table}

	// 源表列名：用于过滤「源库有但目标模型没有」的列（例如已删除字段）。
	srcCols, err := src.Columns(ctx, table)
	if err != nil {
		return nil, fmt.Errorf("读取源表 %s 的列失败: %w", table, err)
	}
	srcColSet := map[string]bool{}
	for _, c := range srcCols {
		srcColSet[strings.ToLower(c.Name)] = true
	}

	schema := loadTargetSchema(opt.Target, table)

	// 覆盖前先清空目标表：规格书要求「目标库被完全覆盖」。
	if err := opt.Target.WithContext(ctx).Exec("delete from " + table).Error; err != nil {
		if !isNoSuchTableErr(err) {
			return nil, fmt.Errorf("清空目标表 %s 失败: %w", table, err)
		}
	}

	srcSum := newSHA256()
	var srcCount, copied, skipped int64

	// 分批读取 + 分批写入：避免把大表整张读进内存。
	batch := make([]map[string]interface{}, 0, copyBatchSize)

	// flush 写入当前批次并返回实际写入的行数。
	//
	// 返回「实际写入数」而不是「传入行数」：批次内部可能因为非法 JSON
	// 跳过若干行，用传入行数统计会让 copied 虚高。
	flush := func() (int64, error) {
		if len(batch) == 0 {
			return 0, nil
		}
		rows := batch
		batch = make([]map[string]interface{}, 0, copyBatchSize)

		clean := make([]map[string]interface{}, 0, len(rows))
		for _, row := range rows {
			converted, ok := convertRow(row, schema)
			if !ok {
				// 非法 JSON 等无法转换的行：记录并跳过（规格书 7.3）。
				skipped++
				continue
			}
			clean = append(clean, converted)
		}
		if len(clean) == 0 {
			return 0, nil
		}
		if err := opt.Target.WithContext(ctx).Table(table).
			CreateInBatches(&clean, copyBatchSize).Error; err != nil {
			return 0, err
		}
		return int64(len(clean)), nil
	}

	// 逐行扫描源表并累计校验和。
	err = src.ScanRows(ctx, table, func(row map[string]interface{}) error {
		srcCount++
		// 校验和只覆盖源表与目标表共有的列，避免因「源库多了一列」
		// 导致校验和永远对不上。
		srcSum.Write([]byte(canonicalRow(row, srcColSet, schema.All)))
		batch = append(batch, row)
		if len(batch) >= copyBatchSize {
			n, err := flush()
			if err != nil {
				return err
			}
			copied += n
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("复制源表 %s 失败: %w", table, err)
	}
	// 收尾：最后不足一批的部分。
	if len(batch) > 0 {
		n, err := flush()
		if err != nil {
			return nil, fmt.Errorf("复制源表 %s 失败: %w", table, err)
		}
		copied += n
	}

	// 目标侧行数与校验和。
	var dstCount int64
	if err := opt.Target.WithContext(ctx).Raw("select count(*) from " + table).Scan(&dstCount).Error; err != nil {
		dstCount = 0
	}
	dstSum := newSHA256()
	var dstRows []map[string]interface{}
	if err := opt.Target.WithContext(ctx).Raw("select * from " + table).Scan(&dstRows).Error; err == nil {
		for _, row := range dstRows {
			dstSum.Write([]byte(canonicalRow(row, srcColSet, schema.All)))
		}
	}

	res.SourceRows = srcCount
	res.TargetRows = dstCount
	res.RowsCopied = dstCount
	res.SkippedRows = skipped
	res.SourceChecksum = hex.EncodeToString(srcSum.Sum(nil))
	res.TargetChecksum = hex.EncodeToString(dstSum.Sum(nil))

	// 校验：行数与校验和都要一致。
	// 有跳过行时，目标行数应当等于「源行数 - 跳过行数」；
	// 校验和不再比对，因为被跳过的行不会出现在目标库。
	switch {
	case skipped > 0:
		res.Passed = dstCount == srcCount-skipped
		res.Note = fmt.Sprintf("跳过 %d 行（非法 JSON 等），目标 %d 行 = 源 %d - 跳过 %d",
			skipped, dstCount, srcCount, skipped)
	case srcCount == dstCount && res.SourceChecksum == res.TargetChecksum:
		res.Passed = true
	default:
		res.Passed = false
		res.Note = fmt.Sprintf("行数或校验和不一致（源 %d 行 / 目标 %d 行）", srcCount, dstCount)
	}

	log.Info("单表复制完成",
		zap.String("table", table),
		zap.Int64("source_rows", srcCount),
		zap.Int64("target_rows", dstCount),
		zap.Int64("skipped", skipped),
		zap.Bool("passed", res.Passed))

	return res, nil
}

// convertRow 按目标列类型做类型转换（规格书 7.3 的类型转换注意事项）。
//
// 规则：
//   - 只保留目标表存在的列（源库的废弃列直接丢弃）；
//   - 目标为 JSON 列时 MUST 校验 JSON 合法性，非法则整行跳过；
//   - 目标为布尔列时显式转换（兼容 MySQL 的 TINYINT(1)）；
//   - 时间字段统一转 UTC 后写入；
//   - 大整数字节数三种库都用 BIGINT，不需要额外处理。
//
// 返回值：转换后的行与「是否保留该行」。
func convertRow(row map[string]interface{}, schema targetTableSchema) (map[string]interface{}, bool) {
	out := make(map[string]interface{}, len(row))
	for k, v := range row {
		lower := strings.ToLower(k)
		// 目标表不存在这一列时丢弃。
		if len(schema.All) > 0 && !schema.All[lower] {
			continue
		}
		switch {
		case schema.JSON[lower]:
			// SQLite 的 TEXT JSON → MySQL 的 JSON 列：MUST 校验合法性。
			s := strings.TrimSpace(asString(v))
			if s == "" || s == "null" {
				out[k] = nil
				continue
			}
			if !util.IsValidJSON(s) {
				return nil, false
			}
			out[k] = s

		case schema.Bool[lower]:
			// MySQL 的 TINYINT(1) → PG 的 boolean：显式转换。
			out[k] = asBool(v)

		case isTimeColumn(lower):
			// 时间字段统一转 UTC 后写入。
			t := asTime(v)
			if t.IsZero() {
				out[k] = nil
				continue
			}
			out[k] = t.UTC()

		default:
			out[k] = v
		}
	}
	return out, true
}

// isTimeColumn 判断列名是否为时间列。
func isTimeColumn(name string) bool {
	switch name {
	case "created_at", "updated_at", "started_at", "finished_at",
		"last_seen", "last_login_at", "expire_at", "applied_at",
		"synced_at", "fired_at", "resolved_at", "last_used_at",
		"last_fired_at", "boot_time":
		return true
	}
	return strings.HasSuffix(name, "_at") || strings.HasSuffix(name, "_time")
}

// canonicalRow 生成一行的规范化字符串，用于校验和比对。
//
// 只覆盖「源表有、目标表也有」的列，并按列名排序，
// 这样列顺序差异不会影响校验和。
func canonicalRow(row map[string]interface{}, srcCols, targetCols map[string]bool) string {
	keys := make([]string, 0, len(row))
	for k := range row {
		lower := strings.ToLower(k)
		if srcCols != nil && !srcCols[lower] {
			continue
		}
		if len(targetCols) > 0 && !targetCols[lower] {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		b.WriteString(strings.ToLower(k))
		b.WriteByte('=')
		b.WriteString(canonicalValue(row[k]))
		b.WriteByte(0x1f)
	}
	return b.String()
}

// canonicalValue 把值规范化为可比较的字符串。
//
// 时间统一按 UTC 秒级格式化：源库驱动可能返回带时区的 time.Time，
// 目标库取出来则可能是字符串或另一种浮点精度，直接用 %v 会误报不一致。
func canonicalValue(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return "<nil>"
	case time.Time:
		return x.UTC().Format("2006-01-02 15:04:05")
	case []byte:
		return string(x)
	case string:
		return x
	case bool:
		if x {
			return "1"
		}
		return "0"
	default:
		return fmt.Sprintf("%v", x)
	}
}

// isNoSuchTableErr 判断错误是否为「表不存在」。
func isNoSuchTableErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such table") ||
		strings.Contains(msg, "doesn't exist") ||
		strings.Contains(msg, "does not exist")
}

// FormatCopyReport 把跨库转换报告渲染为终端文本。
func FormatCopyReport(r *CopyReport) string {
	var b strings.Builder
	b.WriteString(heavyRule() + "\n")
	b.WriteString("  OpenRoute 跨数据库转换报告\n")
	b.WriteString(heavyRule() + "\n")
	writeField(&b, "源库", r.Source)
	writeField(&b, "目标库", r.Target)
	b.WriteString(thinRule() + "\n")
	b.WriteString("【逐表校验】\n")
	b.WriteString(fmt.Sprintf("  %-18s %10s %10s %8s %s\n", "表名", "源行数", "目标行数", "跳过", "结果"))
	for _, t := range r.Tables {
		if t.SourceRows == 0 && t.TargetRows == 0 && t.RowsCopied == 0 {
			b.WriteString(fmt.Sprintf("  %-18s %10s %10s %8s %s\n", t.Table, "-", "-", "-", t.Note))
			continue
		}
		mark := "✔ 一致"
		if !t.Passed {
			mark = "✘ " + t.Note
		}
		b.WriteString(fmt.Sprintf("  %-18s %10d %10d %8d %s\n",
			t.Table, t.SourceRows, t.TargetRows, t.SkippedRows, mark))
	}
	b.WriteString(thinRule() + "\n")
	b.WriteString(fmt.Sprintf("  合计复制 %d 行，跳过 %d 行\n", r.TotalRows, r.SkippedRows))
	if len(r.Notes) > 0 {
		b.WriteString(thinRule() + "\n")
		b.WriteString("【说明】\n")
		for _, n := range r.Notes {
			b.WriteString("  · " + n + "\n")
		}
	}
	if r.Passed {
		b.WriteString("  结论：✔ 全部表校验通过\n")
	} else {
		b.WriteString("  结论：✘ 存在校验不通过的表，请查看上方明细\n")
	}
	b.WriteString(heavyRule() + "\n")
	return b.String()
}

// CopyOrder 返回跨库复制的表顺序（规格书 7.3），供帮助文本与文档使用。
func CopyOrder() []string {
	out := make([]string, len(copyOrder))
	copy(out, copyOrder)
	return out
}
