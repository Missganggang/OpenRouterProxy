package api

import (
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/openroute/openroute/internal/api/middleware"
	"github.com/openroute/openroute/internal/api/response"
	"github.com/openroute/openroute/internal/app"
	"github.com/openroute/openroute/internal/model"
)

// ListSnapshots 返回配置快照列表（规格书 8.14、6.13）。
func (h *Handlers) ListSnapshots(c *gin.Context) {
	limit := queryInt(c, "limit", 50)
	if limit <= 0 || limit > 500 {
		limit = 50
	}

	items, err := h.app.Snapshot.List(c.Request.Context(), limit)
	if err != nil {
		response.Fail(c, err)
		return
	}
	// 快照数量天然有限（每次变更一份，可按需手动清理），
	// 因此不做服务端分页，但保持响应结构一致。
	page, pageSize := pageParams(c)
	response.List(c, items, page, pageSize, int64(len(items)))
}

// CreateSnapshot 手动生成一份配置快照（规格书 8.14、6.13）。
//
// 请求体可选的 name 为空时，服务层会按时间自动命名。
func (h *Handlers) CreateSnapshot(c *gin.Context) {
	var body struct {
		Name string `json:"name"`
	}
	_ = c.ShouldBindJSON(&body)

	ctx := c.Request.Context()
	snap, err := h.app.Snapshot.Create(ctx, strings.TrimSpace(body.Name), model.SnapshotReasonManual)
	if err != nil {
		response.Fail(c, err)
		return
	}

	uid, un, _, _ := middleware.CurrentUser(c)
	h.app.Audit.Write(ctx, app.AuditEntry{
		UserID:     uid,
		Username:   un,
		Action:     model.ActionCreate,
		Resource:   "snapshot",
		ResourceID: snap.ID,
		After:      gin.H{"name": snap.Name, "checksum": snap.Checksum},
		Message:    "手动生成配置快照 " + snap.Name,
	})

	response.OK(c, snap)
}

// GetSnapshot 返回快照详情（规格书 8.14）。
func (h *Handlers) GetSnapshot(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "快照 ID 必须是正整数")
		return
	}

	snap, err := h.app.Snapshot.Get(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, snap)
}

// DeleteSnapshot 删除一份快照（规格书 8.14）。
func (h *Handlers) DeleteSnapshot(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "快照 ID 必须是正整数")
		return
	}

	ctx := c.Request.Context()
	if err := h.app.Snapshot.Delete(ctx, id); err != nil {
		response.Fail(c, err)
		return
	}

	uid, un, _, _ := middleware.CurrentUser(c)
	h.app.Audit.Write(ctx, app.AuditEntry{
		UserID:     uid,
		Username:   un,
		Action:     model.ActionDelete,
		Resource:   "snapshot",
		ResourceID: id,
		Message:    "删除配置快照",
	})

	response.OK(c, nil)
}

// SnapshotDiff 返回快照与当前配置的差异（规格书 8.14、6.13）。
//
// 响应结构为 {added, removed, changed}，前端用左右分栏展示。
func (h *Handlers) SnapshotDiff(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "快照 ID 必须是正整数")
		return
	}

	diff, err := h.app.Snapshot.Diff(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, diff)
}

// SnapshotRollback 一键回滚到指定快照（规格书 8.14、6.13）。
//
// MUST 在回滚前再生成一份「回滚前快照」，保证操作可逆——
// 这一步由 SnapshotService.Rollback 内部完成，handler 只负责转调与审计。
func (h *Handlers) SnapshotRollback(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "快照 ID 必须是正整数")
		return
	}

	ctx := c.Request.Context()
	uid, un, _, _ := middleware.CurrentUser(c)

	result, err := h.app.Snapshot.Rollback(ctx, id, un)
	if err != nil {
		response.Fail(c, err)
		return
	}

	h.app.Audit.Write(ctx, app.AuditEntry{
		UserID:     uid,
		Username:   un,
		Action:     model.ActionRollback,
		Resource:   "snapshot",
		ResourceID: id,
		After:      result,
		Message:    "回滚到配置快照",
	})

	response.OKMessage(c,
		"已回滚到指定快照；回滚前的配置已自动保存为新快照，可再次回滚撤销本次操作。",
		result)
}
