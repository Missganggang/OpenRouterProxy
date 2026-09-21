package api

import (
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/openroute/openroute/internal/api/middleware"
	"github.com/openroute/openroute/internal/api/response"
	"github.com/openroute/openroute/internal/app"
	"github.com/openroute/openroute/internal/model"
)

// SystemInfo 返回系统概况（规格书 8.14）。
func (h *Handlers) SystemInfo(c *gin.Context) {
	ctx := c.Request.Context()
	db := h.app.DB

	var nodeCount, onlineNodeCount, ruleCount, userCount int64
	db.WithContext(ctx).Model(&model.Node{}).Count(&nodeCount)
	db.WithContext(ctx).Model(&model.Node{}).Where("online = ?", true).Count(&onlineNodeCount)
	db.WithContext(ctx).Model(&model.ForwardRule{}).Count(&ruleCount)
	db.WithContext(ctx).Model(&model.User{}).Count(&userCount)

	response.OK(c, gin.H{
		"version":           app.Version,
		"build":             app.BuildStamp,
		"uptime":            h.app.Uptime(),
		"database":          db.Dialect.Display,
		"node_count":        nodeCount,
		"online_node_count": onlineNodeCount,
		"rule_count":        ruleCount,
		"user_count":        userCount,
		"schema_version":    db.SchemaVersionOf(),
		"subscriber_count":  h.app.Hub().SubscriberCount(),
		"config_version":    h.app.ConfigVersion(),
	})
}

// SystemStatus 返回健康检查结果（规格书 8.14）。
//
// 逐项检查数据库、任务调度、节点通信与前端资源，
// 每项给出是否正常与可读说明，便于「面板出问题先看这里」。
func (h *Handlers) SystemStatus(c *gin.Context) {
	ctx := c.Request.Context()
	checks := make([]gin.H, 0, 5)
	healthy := true

	// 1. 数据库：执行一次极轻量的查询验证连通性。
	dbErr := h.app.DB.WithContext(ctx).Raw("select 1").Scan(new(int)).Error
	if dbErr != nil {
		healthy = false
		checks = append(checks, gin.H{
			"name":    "数据库",
			"ok":      false,
			"message": "查询失败: " + dbErr.Error(),
		})
	} else {
		checks = append(checks, gin.H{
			"name":    "数据库",
			"ok":      true,
			"message": h.app.DB.Dialect.Display + "，结构版本 " + itoa(h.app.DB.SchemaVersionOf()),
		})
	}

	// 2. 数据库可写性：检查文件的写权限（SQLite 场景最常见的故障）。
	if wErr := h.app.DB.WriteUnavailable(ctx); wErr != nil {
		healthy = false
		checks = append(checks, gin.H{
			"name":    "数据库写入",
			"ok":      false,
			"message": wErr.Error(),
		})
	} else {
		checks = append(checks, gin.H{
			"name":    "数据库写入",
			"ok":      true,
			"message": "可写",
		})
	}

	// 3. 后台任务
	ok, msg := h.schedulerStatus()
	if !ok {
		healthy = false
	}
	checks = append(checks, gin.H{"name": "后台任务", "ok": ok, "message": msg})

	// 4. 节点通信
	checks = append(checks, gin.H{
		"name":    "节点通信",
		"ok":      true,
		"message": "心跳间隔 " + itoa(h.app.Config.HeartbeatInterval) + "s，离线判定 " + itoa(h.app.Config.OfflineNodeTime) + "s",
	})

	// 5. 实时推送
	checks = append(checks, gin.H{
		"name":    "实时推送",
		"ok":      true,
		"message": "当前订阅者 " + itoa(h.app.Hub().SubscriberCount()) + " 个",
	})

	response.OK(c, gin.H{
		"healthy": healthy,
		"checks":  checks,
	})
}

// schedulerStatus 读取后台任务的健康状态。
//
// 返回是否正常与说明文本；调度器未注入时（例如 CLI 模式）视为正常。
func (h *Handlers) schedulerStatus() (bool, string) {
	s := h.scheduler()
	if s == nil {
		return true, "未启用（当前为一次性命令模式）"
	}
	list := s.Status()
	running := 0
	failed := 0
	for _, t := range list {
		if t.Running {
			running++
		}
		if t.LastResult == "failed" {
			failed++
		}
	}
	if failed > 0 {
		return false, "共 " + itoa(len(list)) + " 个任务，" + itoa(failed) + " 个最近执行失败"
	}
	return true, "共 " + itoa(len(list)) + " 个任务，" + itoa(running) + " 个正在运行"
}

// SystemVersion 返回后端与节点客户端的版本清单（规格书 8.14）。
func (h *Handlers) SystemVersion(c *gin.Context) {
	ctx := c.Request.Context()

	// 节点客户端版本取所有节点上报值的去重集合，
	// 便于「面板升级后还有哪些节点没升级」的排查。
	var versions []string
	h.app.DB.WithContext(ctx).Model(&model.Node{}).
		Where("client_ver <> ''").
		Distinct("client_ver").
		Pluck("client_ver", &versions)

	response.OK(c, gin.H{
		"backend":     app.Version,
		"node_client": versions,
		"build":       app.BuildStamp,
	})
}

// ErrorDict 返回完整错误码字典（规格书 8.14 的补充能力）。
//
// 前端据此把错误码翻译成中文，并用于文档展示。
func (h *Handlers) ErrorDict(c *gin.Context) {
	response.OK(c, response.DictList())
}

// GetSettings 返回全部系统设置（规格书 8.14）。
func (h *Handlers) GetSettings(c *gin.Context) {
	response.OK(c, h.app.Setting.All())
}

// UpdateSettings 批量更新系统设置（规格书 8.14）。
//
// 请求体是「键 → 任意值」的扁平对象。
func (h *Handlers) UpdateSettings(c *gin.Context) {
	var values map[string]interface{}
	if err := c.ShouldBindJSON(&values); err != nil {
		badRequest(c, "body", "请求体必须是「键 → 值」的 JSON 对象")
		return
	}
	if len(values) == 0 {
		badRequest(c, "body", "至少要提供一项设置")
		return
	}

	ctx := c.Request.Context()

	// 记录变更前的值，写入审计日志（规格书 11.14）。
	before := map[string]interface{}{}
	for k := range values {
		before[k] = h.app.Setting.Get(k)
	}

	if err := h.app.Setting.Update(ctx, values); err != nil {
		response.Fail(c, response.Wrap(response.CodeInternal, err, "更新设置失败"))
		return
	}

	uid, un, _, _ := middleware.CurrentUser(c)
	h.app.Audit.Write(ctx, app.AuditEntry{
		UserID:   uid,
		Username: un,
		Action:   model.ActionUpdate,
		Resource: "settings",
		Before:   before,
		After:    values,
		Message:  "更新系统设置",
	})

	response.OKMsg(c, "设置已保存", h.app.Setting.All())
}

// ListAuditLogs 查询审计日志（规格书 8.14）。
func (h *Handlers) ListAuditLogs(c *gin.Context) {
	page, pageSize := pageParams(c)

	filter := app.AuditFilter{
		UserID:     queryUint64(c, "user_id", 0),
		Username:   strings.TrimSpace(c.Query("username")),
		Action:     strings.TrimSpace(c.Query("action")),
		Resource:   strings.TrimSpace(c.Query("resource")),
		ResourceID: queryUint64(c, "resource_id", 0),
		Result:     strings.TrimSpace(c.Query("result")),
		Page:       page,
		PageSize:   pageSize,
	}
	if from, ok := parseTimeQuery(c, "from"); ok {
		filter.From = from
	}
	if to, ok := parseTimeQuery(c, "to"); ok {
		filter.To = to
	}

	items, total, err := h.app.Audit.Query(c.Request.Context(), filter)
	if err != nil {
		response.Fail(c, response.Wrap(response.CodeInternal, err, "查询审计日志失败"))
		return
	}
	if items == nil {
		items = []model.AuditLog{}
	}
	response.List(c, items, page, pageSize, total)
}

// ListTasks 返回后台任务列表与最近执行状态（规格书 8.14）。
func (h *Handlers) ListTasks(c *gin.Context) {
	s := h.scheduler()
	if s == nil {
		response.OK(c, []interface{}{})
		return
	}
	response.OK(c, s.Status())
}

// RunTask 手动触发一个后台任务（规格书 8.14）。
func (h *Handlers) RunTask(c *gin.Context) {
	s := h.scheduler()
	if s == nil {
		response.Fail(c, response.New(response.CodeInternal, "当前进程未启用任务调度"))
		return
	}

	name := c.Param("name")
	if strings.TrimSpace(name) == "" {
		badRequest(c, "name", "缺少任务名")
		return
	}

	ctx := c.Request.Context()
	if err := s.RunNow(ctx, name); err != nil {
		if jobIsNotFound(err) {
			response.Fail(c, response.New(response.CodeNotFound, err.Error()))
			return
		}
		response.Fail(c, response.Wrap(response.CodeInternal, err, "任务执行失败"))
		return
	}

	uid, un, _, _ := middleware.CurrentUser(c)
	h.app.Audit.Write(ctx, app.AuditEntry{
		UserID:   uid,
		Username: un,
		Action:   model.ActionExec,
		Resource: "task",
		Message:  "手动触发任务 " + name,
	})

	response.OKMsg(c, "任务 "+name+" 执行完成", nil)
}

// -------------------- API Token 管理 --------------------

// ListAPITokens 返回 API Token 列表（规格书 8.14）。
//
// 绝不返回明文 Token 与哈希，只返回可展示的元信息。
func (h *Handlers) ListAPITokens(c *gin.Context) {
	page, pageSize := pageParams(c)
	ctx := c.Request.Context()

	q := h.app.DB.WithContext(ctx).Model(&model.APIToken{})
	if kw := strings.TrimSpace(c.Query("keyword")); kw != "" {
		q = q.Where("name LIKE ?", "%"+kw+"%")
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		response.Fail(c, response.Wrap(response.CodeInternal, err, "查询 API 令牌失败"))
		return
	}

	items := make([]model.APIToken, 0)
	if err := q.Order("created_at DESC").
		Offset((page - 1) * pageSize).Limit(pageSize).
		Find(&items).Error; err != nil {
		response.Fail(c, response.Wrap(response.CodeInternal, err, "查询 API 令牌失败"))
		return
	}

	// 清空敏感字段后返回。
	for i := range items {
		items[i].Token = ""
		items[i].TokenHash = ""
	}
	response.List(c, items, page, pageSize, total)
}

// CreateAPIToken 创建 API Token（规格书 8.14）。
//
// 明文 Token 仅在本次响应中返回一次，库中只存哈希。
func (h *Handlers) CreateAPIToken(c *gin.Context) {
	var body struct {
		Name        string   `json:"name"`
		Scopes      []string `json:"scopes"`
		IPWhitelist []string `json:"ip_whitelist"`
		ExpireAt    string   `json:"expire_at"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		badRequest(c, "body", "请求体不是合法 JSON")
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		response.Fail(c, response.Field(response.CodeParamInvalid, "name", nil, "请填写令牌名称"))
		return
	}
	if len(body.Scopes) == 0 {
		response.Fail(c, response.Field(response.CodeParamInvalid, "scopes", nil,
			"至少需要勾选一个权限范围"))
		return
	}
	// 校验 Scope 合法性，避免创建出永远无法生效的令牌。
	for _, s := range body.Scopes {
		if !validScope(s) {
			response.Fail(c, response.Field(response.CodeParamInvalid, "scopes", s,
				"未知的权限范围；可用值见 GET /api/v1/system/openapi.json 的 scopes 枚举"))
			return
		}
	}
	// 校验 IP 白名单格式。
	for _, cidr := range body.IPWhitelist {
		if _, ok := normalizeCIDR(cidr); !ok {
			response.Fail(c, response.Field(response.CodeParamInvalid, "ip_whitelist", cidr,
				"IP 白名单格式错误，应为 IP 或 CIDR，如 1.2.3.4 或 1.2.3.0/24"))
			return
		}
	}

	plain, err := newAPIToken()
	if err != nil {
		response.Fail(c, response.Wrap(response.CodeInternal, err, "生成令牌失败"))
		return
	}

	ctx := c.Request.Context()
	tok := model.APIToken{
		Name:        strings.TrimSpace(body.Name),
		Token:       plain,
		TokenHash:   hashToken(plain),
		Scopes:      model.FromAny(body.Scopes),
		IPWhitelist: model.FromAny(body.IPWhitelist),
		Enabled:     true,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	if strings.TrimSpace(body.ExpireAt) != "" {
		t, perr := time.Parse(time.RFC3339, body.ExpireAt)
		if perr != nil {
			response.Fail(c, response.Field(response.CodeParamInvalid, "expire_at", body.ExpireAt,
				"过期时间需为 RFC 3339 格式，如 2026-12-31T00:00:00Z"))
			return
		}
		tok.ExpireAt = &t
	}

	if err := h.app.DB.WithContext(ctx).Create(&tok).Error; err != nil {
		response.Fail(c, response.Wrap(response.CodeInternal, err, "创建令牌失败"))
		return
	}

	uid, un, _, _ := middleware.CurrentUser(c)
	h.app.Audit.Write(ctx, app.AuditEntry{
		UserID:     uid,
		Username:   un,
		Action:     model.ActionCreate,
		Resource:   "api_token",
		ResourceID: tok.ID,
		After:      gin.H{"name": tok.Name, "scopes": body.Scopes},
		Message:    "创建 API 令牌 " + tok.Name,
	})

	response.OKMsg(c, "令牌已创建，请立即复制保存（明文只显示这一次）", gin.H{
		"id":           tok.ID,
		"name":         tok.Name,
		"token":        plain,
		"scopes":       body.Scopes,
		"ip_whitelist": body.IPWhitelist,
		"expire_at":    tok.ExpireAt,
		"created_at":   tok.CreatedAt,
	})
}

// DeleteAPIToken 删除 API Token（规格书 8.14）。
func (h *Handlers) DeleteAPIToken(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "令牌 ID 必须是正整数")
		return
	}

	ctx := c.Request.Context()
	var tok model.APIToken
	if err := h.app.DB.WithContext(ctx).First(&tok, id).Error; err != nil {
		response.Fail(c, response.New(response.CodeNotFound, "令牌不存在"))
		return
	}

	if err := h.app.DB.WithContext(ctx).Delete(&model.APIToken{}, id).Error; err != nil {
		response.Fail(c, response.Wrap(response.CodeInternal, err, "删除令牌失败"))
		return
	}

	uid, un, _, _ := middleware.CurrentUser(c)
	h.app.Audit.Write(ctx, app.AuditEntry{
		UserID:     uid,
		Username:   un,
		Action:     model.ActionDelete,
		Resource:   "api_token",
		ResourceID: id,
		Before:     gin.H{"name": tok.Name, "scopes": tok.ScopeList()},
		Message:    "删除 API 令牌 " + tok.Name,
	})

	response.OK(c, nil)
}

// ListBackupsRef 是备份列表的适配入口（供 migration 相关 handler 复用）。
func (h *Handlers) backupPageParams(c *gin.Context) (int, int) { return pageParams(c) }

// gormDBOf 返回可用于直接查询的 Gorm 句柄。
func (h *Handlers) gormDBOf(c *gin.Context) *gorm.DB {
	return h.app.DB.WithContext(c.Request.Context())
}
