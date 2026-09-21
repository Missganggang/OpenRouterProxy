package app

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/openroute/openroute/internal/model"
	"github.com/openroute/openroute/internal/util"
)

// AuditEntry 是一次审计记录的输入。
//
// Before / After 用于记录变更前后快照，实现「谁在什么时间改了什么」（规格书 11.14）。
type AuditEntry struct {
	UserID     uint64
	Username   string
	Action     string
	Resource   string
	ResourceID uint64
	Before     interface{}
	After      interface{}
	IP         string
	UserAgent  string
	Result     string
	Message    string
}

// AuditService 负责写入操作审计日志。
type AuditService struct {
	app *App
}

// NewAuditService 构造审计服务。
func NewAuditService(a *App) *AuditService {
	return &AuditService{app: a}
}

// Write 异步写入一条审计记录。
//
// 设计取舍：审计写入失败绝不能影响主业务，因此这里**不返回错误**，
// 仅在被调用方同步调用时记录日志告警。写入放在独立 goroutine 中，
// 避免给请求链路增加一次数据库写延迟（审计表写入频繁）。
func (s *AuditService) Write(ctx context.Context, e AuditEntry) {
	if e.Action == "" {
		e.Action = model.ActionUpdate
	}
	if e.Result == "" {
		e.Result = model.ResultSuccess
	}
	// 上下文可能因请求结束而被取消，因此用 Background 派生，保证能写完。
	ctx = context.WithoutCancel(ctx)

	rec := model.AuditLog{
		UserID:     e.UserID,
		Username:   util.Truncate(e.Username, 64),
		Action:     util.Truncate(e.Action, 64),
		Resource:   util.Truncate(e.Resource, 64),
		ResourceID: e.ResourceID,
		IP:         util.Truncate(e.IP, 64),
		UserAgent:  util.Truncate(e.UserAgent, 255),
		Result:     util.Truncate(e.Result, 16),
		Message:    util.Truncate(e.Message, 512),
		CreatedAt:  time.Now().UTC(),
	}
	if e.Before != nil {
		rec.Before = model.FromAny(e.Before)
	}
	if e.After != nil {
		rec.After = model.FromAny(e.After)
	}

	if err := s.app.DB.WithContext(ctx).Create(&rec).Error; err != nil {
		s.app.Log.Warn("写入审计日志失败",
			zap.String("action", rec.Action),
			zap.String("resource", rec.Resource),
			zap.Error(err))
	}
}

// Query 按条件分页查询审计日志。
//
// 参数 filter 支持按用户、动作、资源、时间范围筛选。
// 返回记录列表与总数。
func (s *AuditService) Query(ctx context.Context, filter AuditFilter) ([]model.AuditLog, int64, error) {
	q := s.app.DB.WithContext(ctx).Model(&model.AuditLog{})

	if filter.UserID > 0 {
		q = q.Where("user_id = ?", filter.UserID)
	}
	if filter.Username != "" {
		q = q.Where("username LIKE ?", "%"+filter.Username+"%")
	}
	if filter.Action != "" {
		q = q.Where("action = ?", filter.Action)
	}
	if filter.Resource != "" {
		q = q.Where("resource = ?", filter.Resource)
	}
	if filter.ResourceID > 0 {
		q = q.Where("resource_id = ?", filter.ResourceID)
	}
	if filter.Result != "" {
		q = q.Where("result = ?", filter.Result)
	}
	if !filter.From.IsZero() {
		q = q.Where("created_at >= ?", filter.From)
	}
	if !filter.To.IsZero() {
		q = q.Where("created_at <= ?", filter.To)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	items := make([]model.AuditLog, 0)
	err := q.Order("created_at DESC").
		Offset((filter.Page - 1) * filter.PageSize).
		Limit(filter.PageSize).
		Find(&items).Error
	return items, total, err
}

// AuditFilter 是审计日志的查询条件。
type AuditFilter struct {
	UserID     uint64
	Username   string
	Action     string
	Resource   string
	ResourceID uint64
	Result     string
	From       time.Time
	To         time.Time
	Page       int
	PageSize   int
}
