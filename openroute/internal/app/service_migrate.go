package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/openroute/openroute/internal/api/response"
	"github.com/openroute/openroute/internal/database"
	"github.com/openroute/openroute/internal/migrate"
	"github.com/openroute/openroute/internal/model"
	"github.com/openroute/openroute/internal/util"
)

// 本文件是 internal/migrate 与 App 之间唯一的桥。
//
// migrate 包刻意不依赖 app（可独立编译与测试），因此这里只做三件事：
//  1. 把 App 的依赖（DB / Log / Config）翻译成 migrate 的参数；
//  2. 把 migrate 的结构化错误翻译成统一的业务错误（错误码 70001~70005）；
//  3. 补充 App 侧的副作用（配置版本自增、审计留痕）。

// MigrateService 是迁移功能的门面（规格书第 7 章）。
//
// 命令行 `-migrate` 与 API `/api/v1/migrations/*` 共用本实现，
// 差异只在于「进度回调」与「交互确认」由调用方注入。
type MigrateService struct {
	app *App
}

// NewMigrateService 构造迁移服务。
func NewMigrateService(a *App) *MigrateService {
	return &MigrateService{app: a}
}

// MigrateRequest 是一次迁移或预检的请求参数。
//
// 字段命名与命令行参数一一对应，便于 CLI 层直接透传。
type MigrateRequest struct {
	// From 是来源标识，目前仅支持 nyanpass
	From string
	// DSN 是源库地址（Nyanpass 的 database-path 或 data.db 路径）
	DSN string
	// DryRun 为 true 时只做预检，不写数据
	DryRun bool
	// RenamePolicy 是名称冲突策略：fail | suffix | skip，空值按 fail
	RenamePolicy string
	// BackupBefore 为 true 时迁移前自动备份目标库
	BackupBefore bool

	// Progress 是进度回调，可为 nil
	Progress migrate.ProgressFunc
	// Recover 是未完成批次的处理决策回调，可为 nil（默认继续）
	Recover migrate.RecoverFunc
	// Confirm 是交互确认回调，可为 nil
	Confirm func(prompt string) (bool, error)
}

// MigrateResult 是一次迁移的结果。
type MigrateResult struct {
	BatchID uint64              `json:"batch_id"`
	Report  *migrate.JSONReport `json:"report"`
	// Text 是纯文本报告，供 CLI 直接打印
	Text string `json:"text"`
	// Passed 表示校验是否通过（预检时为 true 表示没有致命错误）
	Passed bool `json:"passed"`
	// Backup 是迁移前生成的备份路径
	Backup string `json:"backup_path,omitempty"`
}

// Run 执行迁移（或预检）。
//
// 参数 ctx 为上下文；req 为请求参数。
// 返回迁移结果与错误；错误已按业务语义包装（70001~70005）。
func (s *MigrateService) Run(ctx context.Context, req MigrateRequest) (*MigrateResult, error) {
	policy, err := migrate.ParseRenamePolicy(req.RenamePolicy)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.DSN) == "" {
		return nil, fmt.Errorf("请提供源库地址（dsn）")
	}

	// 迁移前自动备份（规格书 7.2 阶段 7：迁移前自动备份，回滚失败时可用它恢复）。
	backupPath := ""
	if req.BackupBefore && !req.DryRun {
		backup := s.app.Backup
		if backup == nil {
			// 装配顺序不确定时自建一个：备份服务本身无状态，可以安全地临时构造。
			backup = NewBackupService(s.app)
		}
		res, err := backup.Create(ctx, BackupRequest{OutPath: s.defaultBackupPath("before-migrate")})
		if err != nil {
			return nil, fmt.Errorf("迁移前备份失败，已中止迁移：%w", err)
		}
		backupPath = res.Path
	}

	runner, err := migrate.NewRunner(migrate.Config{
		Source:       req.From,
		SourceDSN:    req.DSN,
		Target:       s.app.DB,
		TargetDSN:    s.app.Config.DatabasePath,
		DryRun:       req.DryRun,
		RenamePolicy: policy,
		BackupPath:   backupPath,
		Log:          s.app.Log,
		Progress:     req.Progress,
		Recover:      req.Recover,
	})
	if err != nil {
		return nil, err
	}

	report, err := runner.Run(ctx)
	if err != nil {
		// 报告里已经写好了可读的错误说明，把两者一起交给调用方。
		//
		// 注意：runner.Run 在早期阶段（如连接源库）失败时可能返回 nil report，
		// 因此下面所有对 report 的访问都必须判空，否则会把一个本该返回给用户的
		// 业务错误变成 panic。
		wrapped := wrapMigrateError(err)
		result := &MigrateResult{BatchID: runner.BatchID()}
		if report != nil {
			if report.Error != "" {
				wrapped = appendErrDetail(wrapped, report.Error)
			}
			result.Report = report.ToJSONReport()
			result.Text = report.Render()
		}
		// 把根因写进日志：API 只返回友好信息，排查必须靠日志。
		s.app.Log.Error("迁移执行失败",
			zap.String("from", req.From),
			zap.String("dsn", util.MaskSecret(req.DSN)),
			zap.Int64("batch_id", int64(runner.BatchID())),
			zap.Error(err))
		return result, wrapped
	}

	// 正常情况下 report 不应为 nil；退一步保护，避免 panic。
	if report == nil {
		return &MigrateResult{BatchID: runner.BatchID()},
			response.Wrap(response.CodeInternal, nil, "迁移未返回报告，请检查面板日志")
	}

	// 正式迁移成功后配置发生变化（新增规则 / 设备组 / 节点），
	// 自增版本号通知在线节点拉取新配置（规格书 5.4）。
	if !req.DryRun && report.MigratedRows > 0 {
		s.app.BumpConfigVersion("migrate")
	}

	result := &MigrateResult{
		BatchID: runner.BatchID(),
		Report:  report.ToJSONReport(),
		Text:    report.Render(),
		Backup:  backupPath,
	}
	if report.Verify != nil {
		result.Passed = report.Verify.Passed
	} else {
		result.Passed = report.Error == ""
	}
	if !req.DryRun && result.Passed && s.app.Alert != nil {
		s.app.Alert.NotifyEvent(ctx, EventMigrationFinished, map[string]interface{}{
			"batch_id": result.BatchID, "source": req.From, "migrated_rows": report.MigratedRows,
		})
	}
	return result, nil
}

// Precheck 只做预检并返回报告（对应 API `POST /api/v1/migrations/precheck`）。
//
// 无论请求里的 DryRun 取值如何都不写数据，保证预检接口天然只读。
//
// 参数 ctx 为上下文；req 为请求参数。
// 返回 JSON 报告与错误。
func (s *MigrateService) Precheck(ctx context.Context, req MigrateRequest) (*migrate.JSONReport, error) {
	policy, err := migrate.ParseRenamePolicy(req.RenamePolicy)
	if err != nil {
		return nil, err
	}
	runner, err := migrate.NewRunner(migrate.Config{
		Source:       req.From,
		SourceDSN:    req.DSN,
		Target:       s.app.DB,
		TargetDSN:    s.app.Config.DatabasePath,
		DryRun:       true,
		RenamePolicy: policy,
		Log:          s.app.Log,
		Progress:     req.Progress,
	})
	if err != nil {
		return nil, err
	}
	report, err := runner.Precheck(ctx)
	if err != nil {
		wrapped := wrapMigrateError(err)
		if report != nil {
			if report.Error != "" {
				// 把详细的报告正文附到错误信息后面。
				//
				// 关键点：必须保留 wrapped 的类型（*response.AppError）而不是
				// 用 fmt.Errorf("%v...") 把它拼成一个普通 error——那样会把
				// 错误码 700xx 丢失，客户端只会看到 50001「内部错误」。
				wrapped = appendErrDetail(wrapped, report.Error)
			}
			return report.ToJSONReport(), wrapped
		}
		return nil, wrapped
	}
	return report.ToJSONReport(), nil
}

// appendErrDetail 在保持原错误类型的前提下，把补充说明追加到错误信息之后。
//
// 参数 err 为原始错误；detail 为要追加的说明文本。
// 返回类型不变的新错误（若 err 是 *response.AppError，则返回的仍是它）。
func appendErrDetail(err error, detail string) error {
	if err == nil || strings.TrimSpace(detail) == "" {
		return err
	}
	var ae *response.AppError
	if errors.As(err, &ae) {
		// 复制一份再改 Msg，避免影响可能被别处持有的原对象。
		clone := *ae
		clone.Msg = ae.Msg + "\n" + detail
		return &clone
	}
	return fmt.Errorf("%w\n%s", err, detail)
}

// Rollback 回滚一个迁移批次（对应 API `POST /api/v1/migrations/:id/rollback`）。
//
// 参数 ctx 为上下文；batchID 为批次 ID。
// 返回回滚后的报告与错误。
func (s *MigrateService) Rollback(ctx context.Context, batchID uint64) (*migrate.JSONReport, error) {
	// 回滚只需要目标库：migrate_id_map 里记录了要删除的目标行 ID，
	// 因此这里把「源库」也指向目标库，仅为满足 Runner 的构造要求，
	// 不会真的去读它。
	runner, err := migrate.NewRunner(migrate.Config{
		Source:       "openroute",
		SourceDSN:    s.app.Config.DatabasePath,
		Target:       s.app.DB,
		TargetDSN:    s.app.Config.DatabasePath,
		RenamePolicy: migrate.RenameFail,
		Log:          s.app.Log,
	})
	if err != nil {
		return nil, err
	}
	report, err := runner.Rollback(ctx, batchID)
	if err != nil {
		return nil, wrapMigrateError(err)
	}
	s.app.BumpConfigVersion("migrate_rollback")

	// 回滚会删除大量规则，写一条审计留痕（规格书 4.2.12）。
	s.app.Audit.Write(ctx, AuditEntry{
		Action:   model.ActionRollback,
		Resource: "migration",
		Result:   model.ResultSuccess,
		Message:  fmt.Sprintf("回滚迁移批次 %d", batchID),
	})
	return report.ToJSONReport(), nil
}

// List 返回迁移批次列表（对应 API `GET /api/v1/migrations`）。
//
// 参数 ctx 为上下文；limit 为返回条数上限（<=0 时取 50）。
// 返回批次列表与错误。
func (s *MigrateService) List(ctx context.Context, limit int) ([]model.MigrateBatch, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	var out []model.MigrateBatch
	if err := s.app.DB.WithContext(ctx).Order("id DESC").Limit(limit).Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// Get 返回单个批次的详情与报告（对应 API `GET /api/v1/migrations/:id`）。
//
// 参数 ctx 为上下文；batchID 为批次 ID。
// 返回批次与错误。
func (s *MigrateService) Get(ctx context.Context, batchID uint64) (*model.MigrateBatch, error) {
	var b model.MigrateBatch
	if err := s.app.DB.WithContext(ctx).First(&b, batchID).Error; err != nil {
		return nil, err
	}
	return &b, nil
}

// MigrateProgress 是迁移进度的快照。
type MigrateProgress struct {
	BatchID      uint64           `json:"batch_id"`
	Status       string           `json:"status"`
	Stage        int              `json:"stage"`
	StageTotal   int              `json:"stage_total"`
	TotalRows    int64            `json:"total_rows"`
	MigratedRows int64            `json:"migrated_rows"`
	Percent      float64          `json:"percent"`
	ByTable      map[string]int64 `json:"by_table"`
	Error        string           `json:"error,omitempty"`
	FinishedAt   *time.Time       `json:"finished_at,omitempty"`
}

// Progress 返回批次的实时进度（对应 API `GET /api/v1/migrations/:id/progress`）。
//
// 实现为「按 migrate_id_map 统计已迁移行数 / 总行数」的快照查询，
// 调用方轮询即可；SSE 由 handler 层基于本方法包装。
//
// 参数 ctx 为上下文；batchID 为批次 ID。
// 返回进度快照与错误。
func (s *MigrateService) Progress(ctx context.Context, batchID uint64) (*MigrateProgress, error) {
	var b model.MigrateBatch
	if err := s.app.DB.WithContext(ctx).First(&b, batchID).Error; err != nil {
		return nil, err
	}

	type countRow struct {
		Table string `gorm:"column:table_name"`
		N     int64  `gorm:"column:n"`
	}
	var rows []countRow
	_ = s.app.DB.WithContext(ctx).Table("migrate_id_map").
		Select("table_name, count(*) as n").
		Where("batch_id = ?", batchID).
		Group("table_name").Scan(&rows).Error

	byTable := map[string]int64{}
	for _, r := range rows {
		byTable[r.Table] = r.N
	}

	out := &MigrateProgress{
		BatchID:      b.ID,
		Status:       b.Status,
		Stage:        b.Stage,
		StageTotal:   migrate.StageTotal,
		TotalRows:    b.TotalRows,
		MigratedRows: b.MigratedRows,
		ByTable:      byTable,
		Error:        b.Error,
		FinishedAt:   b.FinishedAt,
	}
	if b.TotalRows > 0 {
		out.Percent = float64(b.MigratedRows) / float64(b.TotalRows) * 100
		if out.Percent > 100 {
			out.Percent = 100
		}
	}
	return out, nil
}

// Unfinished 返回未完成的迁移批次（规格书 7.4：重启后提示「继续 / 回滚 / 放弃」）。
//
// 参数 ctx 为上下文。返回未完成批次列表与错误。
func (s *MigrateService) Unfinished(ctx context.Context) ([]model.MigrateBatch, error) {
	var out []model.MigrateBatch
	if err := s.app.DB.WithContext(ctx).
		Where("status in ?", []string{model.MigratePending, model.MigrateRunning}).
		Order("id DESC").Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// Fields 返回 Nyanpass → OpenRoute 的字段映射表，供 WebUI 展示。
func (s *MigrateService) Fields() map[string][]migrate.FieldMap {
	return migrate.NyanpassFieldMapping()
}

// Stages 返回迁移的 7 个阶段名称，供 WebUI 展示进度条。
func (s *MigrateService) Stages() []string { return migrate.Stages() }

// defaultBackupPath 生成默认的备份文件路径。
//
// 放在数据库文件同目录下的 backups/ 里，便于用户与运维脚本一起打包。
func (s *MigrateService) defaultBackupPath(prefix string) string {
	dir := s.dataDir()
	name := fmt.Sprintf("%s-%s.zip", prefix, time.Now().UTC().Format("20060102-150405"))
	return filepath.Join(dir, "backups", name)
}

// dataDir 返回数据文件所在目录（SQLite 场景）。
func (s *MigrateService) dataDir() string {
	if d, err := database.ParseDatabasePath(s.app.Config.DatabasePath); err == nil && d.File != "" {
		return dirOrDot(d.File)
	}
	// 非 SQLite：退回进程工作目录。
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

// wrapMigrateError 把 migrate 包的结构化错误翻译成带错误码的业务错误。
//
// migrate 包不依赖 api/response（保持可独立编译），因此错误码的绑定放在这里。
// 返回的错误文本是面向终端用户的完整说明。
func wrapMigrateError(err error) error {
	if err == nil {
		return nil
	}

	// 已经是 AppError 的直接透传，避免被二次包装。
	var ae *response.AppError
	if errors.As(err, &ae) {
		return ae
	}

	// 把 migrate 包的哨兵错误映射成规格书 8.4 定义的错误码。
	//
	// 必须返回 *response.AppError 而不是 fmt.Errorf("[70001] ...")：
	// 后者只是一个普通 error，统一错误处理会把它当成 50001 内部错误，
	// 客户端拿到的就是「内部错误」而不是「源库无法连接」。
	msg := err.Error()
	switch {
	case errors.Is(err, migrate.ErrSameDatabase) || strings.Contains(msg, migrate.ErrSameDatabase.Error()):
		return response.Wrap(response.CodeSameDatabase, err, "")
	case errors.Is(err, migrate.ErrSourceUnsupported) || strings.Contains(msg, migrate.ErrSourceUnsupported.Error()):
		return response.Wrap(response.CodeSourceUnsupported, err, "")
	case errors.Is(err, migrate.ErrSourceUnreachable) || strings.Contains(msg, migrate.ErrSourceUnreachable.Error()):
		return response.Wrap(response.CodeSourceUnreachable, err, "")
	case errors.Is(err, migrate.ErrUnresolvedConflict) || strings.Contains(msg, migrate.ErrUnresolvedConflict.Error()):
		return response.Wrap(response.CodeUnresolvedConflict, err, "")
	case errors.Is(err, migrate.ErrInsufficientSpace) || strings.Contains(msg, migrate.ErrInsufficientSpace.Error()):
		// 空间不足归入「无法自动处理的冲突」，让用户先腾出空间再迁移。
		return response.Wrap(response.CodeUnresolvedConflict, err,
			"磁盘空间不足，无法完成迁移："+msg)
	case errors.Is(err, migrate.ErrRestoreFailed) || strings.Contains(msg, migrate.ErrRestoreFailed.Error()):
		return response.Wrap(response.CodeRestoreFailed, err, "")
	default:
		// 未识别的错误统一按内部错误处理，原始信息只进日志。
		return response.Wrap(response.CodeInternal, err, "")
	}
}

// dirOrDot 返回路径的目录部分，空路径回退为当前目录。
func dirOrDot(p string) string {
	dir := filepath.Dir(p)
	if dir == "" {
		return "."
	}
	return dir
}

// ---------------------------------------------------------------------------
// 备份服务（规格书 6.15）
// ---------------------------------------------------------------------------

// BackupService 是备份与恢复功能的门面。
//
// 命令行 `-backup` / `-restore` 与 API `/api/v1/backups/*` 共用本实现。
type BackupService struct {
	app *App
}

// NewBackupService 构造备份服务。
func NewBackupService(a *App) *BackupService {
	return &BackupService{app: a}
}

// BackupRequest 是一次备份请求。
type BackupRequest struct {
	// OutPath 是输出路径；为空时自动生成到 <数据目录>/backups/ 下
	OutPath string
	// WithSecret 为 true 时保留 config.yml 的 secret-key
	WithSecret bool
}

// Create 生成一次备份并登记入库。
//
// 参数 ctx 为上下文；req 为请求参数。
// 返回备份记录与错误。
func (s *BackupService) Create(ctx context.Context, req BackupRequest) (*migrate.BackupRecord, error) {
	out := strings.TrimSpace(req.OutPath)
	if out == "" {
		dir := s.backupDir()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建备份目录 %s 失败: %w", dir, err)
		}
		out = filepath.Join(dir, "backup-"+time.Now().UTC().Format("20060102-150405")+".zip")
	}

	res, err := migrate.CreateBackup(ctx, migrate.BackupOptions{
		DataDBPath: s.app.Config.DatabasePath,
		ConfigPath: s.app.Config.ConfigPath,
		OutZipPath: out,
		WithSecret: req.WithSecret,
		AppVersion: Version,
		Log:        s.app.Log,
	})
	if err != nil {
		return nil, err
	}

	rec, err := migrate.RegisterBackup(ctx, s.app.DB, res.Path, res.Manifest, req.WithSecret)
	if err != nil {
		// 登记失败不影响备份文件本身，但要明确告知用户（记录列表里不会出现它）。
		s.app.Log.Warn("备份已生成但登记入库失败",
			zap.String("path", res.Path), zap.Error(err))
		return &migrate.BackupRecord{
			Path: res.Path, Size: res.Size, Checksum: res.Checksum,
			WithSecret: req.WithSecret, Manifest: res.Manifest,
		}, nil
	}

	s.app.Audit.Write(ctx, AuditEntry{
		Action:   model.ActionBackup,
		Resource: "backup",
		Result:   model.ResultSuccess,
		Message:  fmt.Sprintf("生成备份 %s（%d 字节）", rec.Path, rec.Size),
	})
	return rec, nil
}

// AutoBackup 按默认路径生成一次备份，供「迁移前自动备份」等内部场景调用。
//
// 参数 ctx 为上下文；prefix 为文件名前缀（如 before-migrate）。
// 返回备份记录与错误。
func (s *BackupService) AutoBackup(ctx context.Context, prefix string) (*migrate.BackupRecord, error) {
	if strings.TrimSpace(prefix) == "" {
		prefix = "auto"
	}
	req := BackupRequest{
		OutPath: filepath.Join(s.backupDir(),
			fmt.Sprintf("%s-%s.zip", prefix, time.Now().UTC().Format("20060102-150405"))),
	}
	return s.Create(ctx, req)
}

// List 返回备份列表（对应 API `GET /api/v1/backups`）。
//
// 参数 ctx 为上下文。返回备份记录列表与错误。
func (s *BackupService) List(ctx context.Context) ([]migrate.BackupRecord, error) {
	return migrate.ListBackups(ctx, s.app.DB)
}

// Restore 从备份文件恢复（对应 API `POST /api/v1/backups/:id/restore`）。
//
// 恢复前会自动把当前数据另存为 data.db.before-restore（规格书 6.15）。
//
// 参数 ctx 为上下文；id 为备份记录 ID；zipPath 为直接指定的备份文件路径。
// 两者都为空时返回错误。
// 返回恢复后的清单与错误。
func (s *BackupService) Restore(ctx context.Context, id uint64, zipPath string) (*migrate.BackupManifest, error) {
	path := strings.TrimSpace(zipPath)
	if path == "" {
		if id == 0 {
			return nil, fmt.Errorf("请指定备份 ID 或备份文件路径")
		}
		var row model.Backup
		if err := s.app.DB.WithContext(ctx).First(&row, id).Error; err != nil {
			return nil, fmt.Errorf("备份记录 %d 不存在: %w", id, err)
		}
		path = row.Path
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("备份文件 %s 不可读: %w", path, err)
	}

	manifest, err := migrate.RestoreBackup(path, s.app.Config.DatabasePath, s.app.Config.ConfigPath)
	if err != nil {
		s.app.Audit.Write(ctx, AuditEntry{
			Action:   model.ActionRestore,
			Resource: "backup",
			Result:   model.ResultFailed,
			Message:  err.Error(),
		})
		return nil, wrapMigrateError(err)
	}

	s.app.Audit.Write(ctx, AuditEntry{
		Action:   model.ActionRestore,
		Resource: "backup",
		Result:   model.ResultSuccess,
		Message:  fmt.Sprintf("从 %s 恢复完成", path),
	})
	// 恢复后配置与数据都可能变了，通知节点重新拉取。
	s.app.BumpConfigVersion("restore")
	return manifest, nil
}

// Delete 删除备份记录（可选同时删除磁盘文件）。
//
// 参数 ctx 为上下文；id 为备份记录 ID；removeFile 表示是否删除文件。
// 返回错误。
func (s *BackupService) Delete(ctx context.Context, id uint64, removeFile bool) error {
	return migrate.DeleteBackup(ctx, s.app.DB, id, removeFile)
}

// Path 返回备份文件的磁盘路径（对应 API 的下载入口）。
//
// 参数 ctx 为上下文；id 为备份记录 ID。
// 返回文件路径与错误。
func (s *BackupService) Path(ctx context.Context, id uint64) (string, error) {
	var row model.Backup
	if err := s.app.DB.WithContext(ctx).First(&row, id).Error; err != nil {
		return "", fmt.Errorf("备份记录 %d 不存在: %w", id, err)
	}
	if _, err := os.Stat(row.Path); err != nil {
		return "", fmt.Errorf("备份文件已不在磁盘上: %w", err)
	}
	return row.Path, nil
}

// backupDir 返回默认的备份目录。
//
// 与数据库文件同级的 backups/ 子目录：备份与数据一起搬走即可完成迁移。
func (s *BackupService) backupDir() string {
	dir := "."
	if d, err := database.ParseDatabasePath(s.app.Config.DatabasePath); err == nil && d.File != "" {
		dir = dirOrDot(d.File)
	}
	return filepath.Join(dir, "backups")
}

// ---------------------------------------------------------------------------
// 跨库转换（规格书 7.3）
// ---------------------------------------------------------------------------

// CopyDatabase 执行跨数据库转换（对应命令行 `-copy-database`）。
//
// 参数 ctx 为上下文；sourcePath 为源 database-path；
// confirm 为交互确认回调（需自行实现「输入 YES」的校验）；
// force 为 true 时跳过确认。
// 返回转换报告与错误。
func (s *MigrateService) CopyDatabase(
	ctx context.Context, sourcePath string, force bool,
	confirm func(prompt string) (bool, error),
) (*migrate.CopyReport, error) {
	return migrate.CopyDatabase(ctx, migrate.CopyOptions{
		SourcePath: sourcePath,
		Target:     s.app.DB,
		Force:      force,
		Confirm:    confirm,
		Log:        s.app.Log,
	})
}
