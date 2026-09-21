package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/openroute/openroute/internal/app"
	"github.com/openroute/openroute/internal/config"
	"github.com/openroute/openroute/internal/database"
)

// runBackup 执行 `-backup <文件>`（规格书 3.2、6.15）。
//
// 导出单文件备份（zip），内容包含 data.db、config.yml 与 manifest.json。
// 返回进程退出码。
func runBackup(cfg *config.Config, log *zap.Logger, outPath string, withSecret bool) int {
	db, err := cfg.OpenDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接数据库失败: %v\n", err)
		return 1
	}
	defer func() { _ = db.Close() }()

	a := app.New(cfg, db, log)

	rec, err := a.Backup.Create(context.Background(), app.BackupRequest{
		OutPath:    outPath,
		WithSecret: withSecret,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "生成备份失败: %v\n", err)
		return 1
	}

	fmt.Printf("\n备份已生成\n")
	fmt.Printf("  文件    : %s\n", rec.Path)
	fmt.Printf("  大小    : %s\n", humanSize(rec.Size))
	fmt.Printf("  校验和  : %s\n", rec.Checksum)
	if !withSecret {
		fmt.Printf("  提示    : secret-key 已脱敏；如需保留请加 -with-secret\n")
	}
	fmt.Printf("\n恢复方式：./openroute -restore %s\n\n", rec.Path)
	return 0
}

// runRestore 执行 `-restore <文件>`（规格书 3.2、6.15）。
//
// 恢复前会自动把当前数据备份为 data.db.before-restore，因此可逆。
// 返回进程退出码。
func runRestore(cfg *config.Config, log *zap.Logger, zipPath string) int {
	if _, err := os.Stat(zipPath); err != nil {
		fmt.Fprintf(os.Stderr, "备份文件不存在或不可读: %s\n", zipPath)
		return 1
	}

	db, err := cfg.OpenDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接数据库失败: %v\n", err)
		return 1
	}
	defer func() { _ = db.Close() }()

	a := app.New(cfg, db, log)

	// 恢复是破坏性操作，必须二次确认。
	fmt.Printf("\n即将从 %s 恢复数据。\n", zipPath)
	fmt.Println("当前数据会被覆盖（恢复前会自动保存为 data.db.before-restore）。")
	if !confirm("确认恢复？") {
		fmt.Println("已取消。")
		return 0
	}

	manifest, err := a.Backup.Restore(context.Background(), 0, zipPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "恢复失败: %v\n", err)
		fmt.Fprintln(os.Stderr, "提示：当前数据仍保存在 data.db.before-restore，可手工还原。")
		return 1
	}

	fmt.Printf("\n恢复完成\n")
	if manifest != nil {
		fmt.Printf("  备份格式版本: %d\n", manifest.Version)
		fmt.Printf("  面板版本    : %s\n", manifest.AppVersion)
		fmt.Printf("  导出时间    : %s\n", manifest.ExportedAt.Format("2006-01-02 15:04:05 UTC"))
	}
	fmt.Println("  提示      : 请重启面板使恢复生效")
	return 0
}

// runMigrate 执行 `-migrate from=nyanpass,dsn=...,dry-run=true`（规格书 3.2、7.2）。
//
// 也支持 `-migrate rollback=<批次ID>` 回滚一次迁移。
// 返回进程退出码。
func runMigrate(cfg *config.Config, log *zap.Logger, spec string) int {
	db, err := cfg.OpenDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接数据库失败: %v\n", err)
		return 1
	}
	defer func() { _ = db.Close() }()

	a := app.New(cfg, db, log).Init()
	ctx := context.Background()

	// 解析形如 from=nyanpass,dsn=...,dry-run=true 的参数串。
	params, err := parseMigrateSpec(spec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "参数解析失败: %v\n", err)
		return 1
	}

	// 回滚模式：-migrate rollback=<批次ID>
	if raw, ok := params["rollback"]; ok {
		batchID, perr := strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
		if perr != nil {
			fmt.Fprintf(os.Stderr, "rollback 参数必须是迁移批次 ID（数字），收到 %q\n", raw)
			return 1
		}
		return runMigrateRollback(ctx, a, batchID)
	}

	from := strings.TrimSpace(params["from"])
	dsn := strings.TrimSpace(params["dsn"])
	if from == "" || dsn == "" {
		fmt.Fprintln(os.Stderr, "参数不完整：至少需要 from=<来源> 与 dsn=<源库地址>")
		fmt.Fprintln(os.Stderr, `示例：./openroute -migrate from=nyanpass,dsn="sqlite3:///old/data.db",dry-run=true`)
		return 1
	}

	dryRun := strings.EqualFold(strings.TrimSpace(params["dry-run"]), "true")
	renamePolicy := strings.TrimSpace(params["rename-policy"])

	req := app.MigrateRequest{
		From:         from,
		DSN:          dsn,
		DryRun:       dryRun,
		RenamePolicy: renamePolicy,
		// 命令行迁移默认先备份目标库：迁移出错时可回退。
		BackupBefore: true,
	}

	// 正式迁移是破坏性操作，必须确认。
	if !dryRun {
		fmt.Printf("\n即将从 %s 迁移数据到当前库。\n", from)
		fmt.Println("迁移会写入数据；迁移前会自动备份目标库。")
		if !confirm("确认执行正式迁移？") {
			fmt.Println("已取消。可先加 dry-run=true 预览。")
			return 0
		}
	}

	result, err := a.Migrate.Run(ctx, req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "迁移失败: %v\n", err)
		return 1
	}

	// 打印报告：预检/迁移报告本身是排版好的纯文本，直接原样输出。
	fmt.Print("\n")
	if result.Text != "" {
		fmt.Println(result.Text)
	}
	if result.Backup != "" {
		fmt.Printf("\n迁移前备份: %s\n", result.Backup)
	}

	if dryRun {
		fmt.Println("以上为 DRY-RUN 结果，未写入任何数据。")
		fmt.Println("确认无误后执行：./openroute -migrate from=" + from + `,dsn="` + dsn + `",dry-run=false`)
		return 0
	}

	if !result.Passed {
		fmt.Fprintln(os.Stderr, "\n校验未通过，请检查报告中的冲突项。")
		fmt.Fprintf(os.Stderr, "如需撤销，执行：./openroute -migrate rollback=%d\n", result.BatchID)
		return 1
	}

	fmt.Printf("\n迁移完成（批次 ID %d）。\n", result.BatchID)
	fmt.Println("下一步（规格书 7.2 的必做事项）：")
	fmt.Println("  1. 节点密钥已全部重新生成，需在每台机器上卸载旧节点并重新安装")
	fmt.Println("     bash /opt/openroute/openroute.uninstall.sh")
	fmt.Println("  2. 迁移后所有规则状态为「未同步」，节点重装上线后会自动同步")
	fmt.Printf("  3. 如需回滚本次迁移：./openroute -migrate rollback=%d\n", result.BatchID)
	return 0
}

// runMigrateRollback 回滚一次迁移批次。
func runMigrateRollback(ctx context.Context, a *app.App, batchID uint64) int {
	batch, err := a.Migrate.Get(ctx, batchID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "找不到迁移批次 %d: %v\n", batchID, err)
		return 1
	}

	fmt.Printf("\n即将回滚迁移批次 #%d（来源 %s，状态 %s，已迁移 %d 行）。\n",
		batch.ID, batch.Source, batch.Status, batch.MigratedRows)
	fmt.Println("该批次写入的数据会被删除。")
	if !confirm("确认回滚？") {
		fmt.Println("已取消。")
		return 0
	}

	report, err := a.Migrate.Rollback(ctx, batchID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "回滚失败: %v\n", err)
		if batch.BackupPath != "" {
			fmt.Fprintf(os.Stderr, "可改用迁移前备份恢复：./openroute -restore %s\n", batch.BackupPath)
		}
		return 1
	}

	fmt.Println("\n回滚完成。")
	if report != nil && report.Text != "" {
		fmt.Println(report.Text)
	}
	return 0
}

// runCopyDatabase 执行 `-copy-database <目标 database-path>`（规格书 3.2、7.3）。
//
// 目标库会被完全覆盖且无法恢复，因此要求用户输入 YES（全大写）确认；
// 非交互模式需显式加 --force。
// 返回进程退出码。
func runCopyDatabase(cfg *config.Config, log *zap.Logger, target string, force bool) int {
	db, err := cfg.OpenDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接源数据库失败: %v\n", err)
		return 1
	}
	defer func() { _ = db.Close() }()

	a := app.New(cfg, db, log).Init()
	ctx := context.Background()

	// 打印源库与目标库信息（规格书 7.3 第 1 步）。
	srcDialect, _ := describeDatabasePath(cfg.DatabasePath)
	dstDialect, _ := describeDatabasePath(target)

	fmt.Println()
	fmt.Println(strings.Repeat("═", 55))
	fmt.Println("  跨数据库转换")
	fmt.Println(strings.Repeat("═", 55))
	fmt.Printf("  源库    : %s\n", srcDialect)
	fmt.Printf("  目标库  : %s\n", dstDialect)
	fmt.Println(strings.Repeat("─", 55))

	// 醒目警告（规格书 7.3 第 2 步）。
	fmt.Println("  ⚠ 警告：目标库将被完全覆盖，且无法恢复！")
	fmt.Println(strings.Repeat("═", 55))

	// 二次确认（规格书 7.3 第 3 步）。
	if !force {
		fmt.Print("\n请输入 YES（全大写）以确认继续: ")
		reader := bufio.NewReader(os.Stdin)
		line, _ := reader.ReadString('\n')
		if strings.TrimSpace(line) != "YES" {
			fmt.Println("确认失败，已取消。")
			return 0
		}
	}

	// 转换方向：把「当前库」的数据复制到 target 指定的新库。
	// force 为 true 时跳过服务层的交互确认（终端已在上面确认过一次）。
	report, err := a.Migrate.CopyDatabase(ctx, target, force, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\n转换失败: %v\n", err)
		return 1
	}

	fmt.Printf("\n转换完成：共复制 %d 行", report.TotalRows)
	if report.SkippedRows > 0 {
		fmt.Printf("，跳过 %d 行", report.SkippedRows)
	}
	fmt.Println()
	fmt.Printf("  源库表数 : %d\n", report.SourceTables)
	fmt.Printf("  目标表数 : %d\n", report.TargetTables)

	if len(report.Tables) > 0 {
		fmt.Println("\n各表行数（源 → 目标）：")
		for _, t := range report.Tables {
			mark := "✓"
			if !t.Passed {
				mark = "✗"
			}
			fmt.Printf("  %s %-24s %8d → %8d", mark, t.Table, t.SourceRows, t.TargetRows)
			if t.SkippedRows > 0 {
				fmt.Printf("  （跳过 %d 行：%s）", t.SkippedRows, t.Note)
			}
			fmt.Println()
		}
	}
	for _, n := range report.Notes {
		fmt.Printf("\n注意: %s\n", n)
	}

	if !report.Passed {
		fmt.Fprintln(os.Stderr, "\n行数校验未通过，请检查上面的逐表对比。")
		return 1
	}

	fmt.Printf("\n如需使用新库，请把 config.yml 的 database-path 改为：\n  %s\n\n", target)
	return 0
}

// parseMigrateSpec 解析 `-migrate` 的逗号分隔参数串。
//
// 支持两种写法：
//
//	from=nyanpass,dsn="sqlite3:///path/data.db",dry-run=true
//	rollback=3
//
// dsn 的值可能自带逗号（MySQL 的 DSN 里常见），因此以 `key=` 的出现位置切分，
// 而不是简单地按逗号 split。
//
// 返回解析出的键值对与错误。
func parseMigrateSpec(spec string) (map[string]string, error) {
	out := map[string]string{}
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return out, fmt.Errorf("参数为空")
	}

	// 找出所有 `key=` 的起始位置，据此切段。
	type pos struct {
		key   string
		start int
	}
	var marks []pos
	for i := 0; i < len(spec); i++ {
		// 只把「位于串首或紧跟逗号之后」的 `标识=` 视为键的开始。
		if i != 0 && spec[i-1] != ',' {
			continue
		}
		j := i
		for j < len(spec) && (isAlnum(spec[j]) || spec[j] == '-' || spec[j] == '_') {
			j++
		}
		if j > i && j < len(spec) && spec[j] == '=' {
			marks = append(marks, pos{key: spec[i:j], start: i})
		}
	}
	if len(marks) == 0 {
		return out, fmt.Errorf("未找到任何 key=value 形式的参数")
	}

	for idx, m := range marks {
		valStart := m.start + len(m.key) + 1
		valEnd := len(spec)
		if idx+1 < len(marks) {
			// 下一个键的位置，回退一位去掉分隔的逗号。
			valEnd = marks[idx+1].start - 1
		}
		if valEnd < valStart {
			valEnd = valStart
		}
		val := strings.TrimSpace(spec[valStart:valEnd])
		val = strings.Trim(val, `"`)
		out[m.key] = val
	}
	return out, nil
}

// isAlnum 判断字节是否为字母、数字或下划线。
func isAlnum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
}

// describeDatabasePath 返回 database-path 的可读描述（已脱敏）。
//
// 复用 database 包的方言解析，保证命令行展示与运行期日志口径一致。
func describeDatabasePath(path string) (string, error) {
	d, err := database.ParseDatabasePath(path)
	if err != nil {
		// 解析失败时原样返回，让用户在警告界面就能看到自己填的值。
		return path, err
	}
	return d.Display, nil
}

// humanSize 把字节数格式化为可读字符串。
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	v := float64(n)
	i := -1
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	return fmt.Sprintf("%.2f %s", v, units[i])
}
