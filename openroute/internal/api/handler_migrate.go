package api

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/openroute/openroute/internal/api/middleware"
	"github.com/openroute/openroute/internal/api/response"
	"github.com/openroute/openroute/internal/app"
	"github.com/openroute/openroute/internal/model"
)

// migrationInput 是迁移预检与执行共用的请求体（规格书 8.15）。
type migrationInput struct {
	// From 迁移来源：nyanpass | openroute
	From string `json:"source"`
	// DSN 源库地址，如 sqlite3:///old/data.db
	DSN string `json:"dsn"`
	// RenamePolicy 名称冲突策略：fail | suffix | skip
	RenamePolicy string `json:"rename_policy"`
	// BackupBefore 是否在迁移前自动备份目标库
	BackupBefore *bool `json:"backup_before"`
}

// toRequest 把请求体转换为服务层参数。
//
// 参数 dryRun 由外部决定（预检恒为 true，执行时取自查询参数）。
func (in migrationInput) toRequest(dryRun bool) app.MigrateRequest {
	backup := true
	if in.BackupBefore != nil {
		backup = *in.BackupBefore
	}
	return app.MigrateRequest{
		From:         strings.TrimSpace(in.From),
		DSN:          strings.TrimSpace(in.DSN),
		DryRun:       dryRun,
		RenamePolicy: strings.TrimSpace(in.RenamePolicy),
		BackupBefore: backup,
	}
}

// validate 校验迁移请求的必填项与取值。
func (in migrationInput) validate() error {
	from := strings.TrimSpace(in.From)
	switch from {
	case "nyanpass", "openroute":
	case "":
		return response.Field(response.CodeParamInvalid, "source", nil,
			"请选择迁移来源（nyanpass 或 openroute）")
	default:
		return response.Field(response.CodeParamInvalid, "source", from,
			"不支持的迁移来源；当前支持 nyanpass 与 openroute")
	}
	if strings.TrimSpace(in.DSN) == "" {
		return response.Field(response.CodeParamInvalid, "dsn", nil,
			"请填写源库地址，例如 sqlite3:///old/nyanpass/data.db")
	}
	// 名称冲突策略必须是三选一。
	switch strings.TrimSpace(in.RenamePolicy) {
	case "", "fail", "suffix", "skip":
	default:
		return response.Field(response.CodeParamInvalid, "rename_policy", in.RenamePolicy,
			"名称冲突策略只能是 fail、suffix 或 skip")
	}
	return nil
}

// MigrationPrecheck 执行迁移预检（规格书 8.15）。
//
// 预检**绝不写入任何数据**，只返回报告供用户确认。
func (h *Handlers) MigrationPrecheck(c *gin.Context) {
	var in migrationInput
	if err := c.ShouldBindJSON(&in); err != nil {
		badRequest(c, "body", "请求体不是合法 JSON："+err.Error())
		return
	}
	if err := in.validate(); err != nil {
		response.Fail(c, err)
		return
	}

	// 预检固定为 dry-run。
	report, err := h.app.Migrate.Precheck(c.Request.Context(), in.toRequest(true))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, report)
}

// MigrationRun 执行迁移（规格书 8.15）。
//
// 查询参数 `dry_run=true` 时只做预览；
// 正式迁移属于危险操作，前端必须做二次确认。
func (h *Handlers) MigrationRun(c *gin.Context) {
	var in migrationInput
	if err := c.ShouldBindJSON(&in); err != nil {
		badRequest(c, "body", "请求体不是合法 JSON："+err.Error())
		return
	}
	if err := in.validate(); err != nil {
		response.Fail(c, err)
		return
	}

	dryRun := strings.EqualFold(strings.TrimSpace(c.Query("dry_run")), "true")

	ctx := c.Request.Context()
	result, err := h.app.Migrate.Run(ctx, in.toRequest(dryRun))
	if err != nil {
		response.Fail(c, err)
		return
	}

	// 只有正式迁移才写审计，避免预览把日志刷满。
	if !dryRun {
		uid, un, _, _ := middleware.CurrentUser(c)
		h.app.Audit.Write(ctx, app.AuditEntry{
			UserID:   uid,
			Username: un,
			Action:   model.ActionMigrate,
			Resource: "migration",
			After: gin.H{
				"batch_id": result.BatchID,
				"source":   in.From,
				"passed":   result.Passed,
			},
			Message: "执行数据迁移，来源 " + in.From,
		})
	}

	response.OK(c, result)
}

// ListMigrations 返回迁移批次列表（规格书 8.15）。
func (h *Handlers) ListMigrations(c *gin.Context) {
	limit := queryInt(c, "limit", 50)
	if limit <= 0 || limit > 500 {
		limit = 50
	}

	items, err := h.app.Migrate.List(c.Request.Context(), limit)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if items == nil {
		items = []model.MigrateBatch{}
	}
	// 批次数量有限（不会很多），按当前长度返回分页信息。
	response.List(c, items, 1, limit, int64(len(items)))
}

// GetMigration 返回迁移批次详情与报告（规格书 8.15）。
func (h *Handlers) GetMigration(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "迁移批次 ID 必须是正整数")
		return
	}

	batch, err := h.app.Migrate.Get(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, batch)
}

// MigrationProgress 返回迁移实时进度（规格书 8.15）。
//
// 前端以 1~2 秒的间隔轮询本接口绘制阶段进度条。
func (h *Handlers) MigrationProgress(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "迁移批次 ID 必须是正整数")
		return
	}

	progress, err := h.app.Migrate.Progress(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, progress)
}

// MigrationRollback 回滚一个迁移批次（规格书 8.15、7.2 阶段 7）。
//
// 通过 migrate_id_map 逆向删除本批次写入的数据。
func (h *Handlers) MigrationRollback(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "迁移批次 ID 必须是正整数")
		return
	}

	ctx := c.Request.Context()
	report, err := h.app.Migrate.Rollback(ctx, id)
	if err != nil {
		response.Fail(c, err)
		return
	}

	uid, un, _, _ := middleware.CurrentUser(c)
	h.app.Audit.Write(ctx, app.AuditEntry{
		UserID:     uid,
		Username:   un,
		Action:     model.ActionRollback,
		Resource:   "migration",
		ResourceID: id,
		Message:    "回滚迁移批次",
	})

	response.OK(c, report)
}

// -------------------- 备份与恢复 --------------------

// CreateBackup 生成一份全量备份（规格书 8.15、6.15）。
func (h *Handlers) CreateBackup(c *gin.Context) {
	var body struct {
		WithSecret bool `json:"with_secret"`
	}
	_ = c.ShouldBindJSON(&body)

	ctx := c.Request.Context()
	rec, err := h.app.Backup.Create(ctx, app.BackupRequest{WithSecret: body.WithSecret})
	if err != nil {
		response.Fail(c, response.Wrap(response.CodeInternal, err, "生成备份失败"))
		return
	}

	// 备份完成后通过 Webhook 通知（规格书 8.18）。
	h.app.Alert.NotifyEvent(ctx, app.EventBackupFinished, map[string]interface{}{
		"name": rec.Name,
		"path": rec.Path,
		"size": rec.Size,
	})

	uid, un, _, _ := middleware.CurrentUser(c)
	h.app.Audit.Write(ctx, app.AuditEntry{
		UserID:   uid,
		Username: un,
		Action:   model.ActionBackup,
		Resource: "backup",
		After:    gin.H{"name": rec.Name, "with_secret": body.WithSecret},
		Message:  "生成备份 " + rec.Name,
	})

	response.OK(c, rec)
}

// ListBackups 返回备份列表（规格书 8.15）。
func (h *Handlers) ListBackups(c *gin.Context) {
	items, err := h.app.Backup.List(c.Request.Context())
	if err != nil {
		response.Fail(c, err)
		return
	}
	// 备份数量天然有限（每次几十 MB），不做服务端分页，
	// 但保持响应结构一致，前端无需区分两种列表形态。
	page, pageSize := pageParams(c)
	response.List(c, items, page, pageSize, int64(len(items)))
}

// DownloadBackup 下载备份文件（规格书 8.15）。
func (h *Handlers) DownloadBackup(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "备份 ID 必须是正整数")
		return
	}

	path, err := h.app.Backup.Path(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, err)
		return
	}

	// 二次确认文件确实存在且是常规文件，避免路径穿越。
	st, statErr := os.Stat(path)
	if statErr != nil || st.IsDir() {
		response.Fail(c, response.New(response.CodeNotFound, "备份文件不存在或已被移动"))
		return
	}

	attachmentHeader(c, filepath.Base(path), "application/zip")
	c.File(path)
}

// RestoreBackup 从备份恢复（规格书 8.15、6.15）。
//
// 恢复前会自动把当前数据备份为 data.db.before-restore，因此可逆。
func (h *Handlers) RestoreBackup(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "备份 ID 必须是正整数")
		return
	}

	ctx := c.Request.Context()

	zipPath, err := h.app.Backup.Path(ctx, id)
	if err != nil {
		response.Fail(c, err)
		return
	}

	manifest, err := h.app.Backup.Restore(ctx, id, zipPath)
	if err != nil {
		response.Fail(c, err)
		return
	}

	uid, un, _, _ := middleware.CurrentUser(c)
	h.app.Audit.Write(ctx, app.AuditEntry{
		UserID:     uid,
		Username:   un,
		Action:     model.ActionRestore,
		Resource:   "backup",
		ResourceID: id,
		Message:    "从备份恢复数据",
	})

	response.OKMessage(c,
		"恢复完成。当前进程仍在使用旧数据库，请重启面板使恢复生效。",
		gin.H{
			"manifest": manifest,
		})
}
