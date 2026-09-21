// Package api 是 HTTP 层，负责路由注册、请求解析与响应输出。
//
// 分层纪律（规格书 2.3）：handler 只做参数校验与调用 service，
// 禁止直接操作 Gorm（极简查询除外），业务逻辑一律写在 app 包的 service 中。
package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/openroute/openroute/internal/api/middleware"
	"github.com/openroute/openroute/internal/api/response"
	"github.com/openroute/openroute/internal/app"
	"github.com/openroute/openroute/internal/job"
)

// Deps 是路由层需要的依赖，全部由 main.go 注入。
type Deps struct {
	App *app.App
	Log *zap.Logger
	// Jobs 是后台任务调度器，供「任务列表 / 手动触发」接口使用。
	// 允许为 nil（CLI 一次性命令模式下不启动调度）。
	Jobs *job.Scheduler
}

// Server 持有 HTTP 路由引擎。
type Server struct {
	engine *gin.Engine
	deps   Deps
}

// Engine 返回底层的 gin 引擎，供测试与优雅关闭使用。
func (s *Server) Engine() *gin.Engine { return s.engine }

// NewServer 构造 HTTP 服务并完成全部路由注册。
//
// 路由组织：
//
//	/api/v1/*    版本化的管理接口（Session / JWT / API Token 认证）
//	/api/node/*  节点通信接口（不带版本号，用节点 Token 认证）
//	/sub/:token  用户订阅（无需认证）
//	/install.sh  节点一键安装脚本
//	/api/docs    交互式 API 文档（可开关）
//	/*           前端静态资源与 SPA 回退
//
// 参数 deps 为依赖；返回构造好的 Server。
func NewServer(deps Deps) *Server {
	// 设置运行模式：非 debug 配置下关闭 gin 的调试输出，
	// 避免每个请求都往终端刷一行。
	gin.SetMode(gin.ReleaseMode)
	if deps.App.Config.LogLevel == "debug" {
		gin.SetMode(gin.DebugMode)
	}

	engine := gin.New()
	engine.RedirectTrailingSlash = false
	// 单端口同时提供 API 与 WebUI，默认信任所有代理以便取到真实 IP。
	_ = engine.SetTrustedProxies(nil)

	s := &Server{engine: engine, deps: deps}
	s.registerGlobalMiddleware()
	s.registerRoutes()
	return s
}

// registerGlobalMiddleware 注册全局中间件。
func (s *Server) registerGlobalMiddleware() {
	cfg := s.deps.App.Config

	s.engine.Use(middleware.RequestID())
	s.engine.Use(middleware.Recovery(s.deps.Log))
	s.engine.Use(middleware.CORS())
	s.engine.Use(middleware.SecurityHeaders())
	s.engine.Use(middleware.AccessLog(s.deps.Log))
	s.engine.Use(middleware.Gzip(cfg.DisableGzip))
	// 请求体上限 20MB：备份恢复走独立入口，这里的 JSON 接口不需要更大。
	s.engine.Use(middleware.MaxBodySize(20 << 20))
}

// registerRoutes 注册全部路由。
func (s *Server) registerRoutes() {
	a := s.deps.App

	// authDeps 供认证中间件使用。
	authDeps := middleware.AuthDeps{
		ParseToken: func(tokenStr, wantType string) (*middleware.JWTClaims, error) {
			claims, err := a.Auth.ParseToken(tokenStr, wantType)
			if err != nil {
				return nil, err
			}
			return &middleware.JWTClaims{
				UserID:    claims.UserID,
				Username:  claims.Username,
				Role:      claims.Role,
				TokenType: claims.TokenType,
			}, nil
		},
		DB:        a.DB.DB,
		SecretKey: a.Config.SecretKey,
	}

	authenticated := middleware.Authenticate(authDeps)
	requireAdmin := middleware.RequireAdmin()

	// 限流器按接口类别创建（规格书 8.17）。
	readLimit := middleware.NewRateLimiter(a.Config.UserRateLimit.Rate, a.Config.UserRateLimit.Limit).
		Middleware(nil)

	h := NewHandlers(s.deps)

	// ── 公开接口（无需认证）─────────────────────────────────
	s.engine.GET("/api/v1/health", h.Health)
	s.engine.GET("/api/v1/system/openapi.json", h.OpenAPIJSON)

	authGroup := s.engine.Group("/api/v1/auth")
	{
		authGroup.POST("/login", middleware.LoginRateLimit(), h.Login)
		authGroup.GET("/captcha", h.Captcha)
		authGroup.POST("/refresh", h.Refresh)
		authGroup.POST("/logout", authenticated, h.Logout)
		authGroup.GET("/me", authenticated, h.Me)
		authGroup.POST("/password", authenticated, h.ChangePassword)
	}

	// ── 需要认证的管理接口 ──────────────────────────────────
	v1 := s.engine.Group("/api/v1")
	v1.Use(authenticated)

	// 系统与设置
	sys := v1.Group("/system")
	{
		sys.GET("/info", readLimit, middleware.RequireScope(middleware.ScopeSystemRead), h.SystemInfo)
		sys.GET("/status", readLimit, middleware.RequireScope(middleware.ScopeSystemRead), h.SystemStatus)
		sys.GET("/version", readLimit, middleware.RequireScope(middleware.ScopeSystemRead), h.SystemVersion)
		sys.GET("/errors", readLimit, middleware.RequireScope(middleware.ScopeSystemRead), h.ErrorDict)
	}

	v1.GET("/settings", readLimit, middleware.RequireScope(middleware.ScopeSystemRead), h.GetSettings)
	v1.PUT("/settings", requireAdmin, middleware.RequireScope(middleware.ScopeSystemWrite), h.UpdateSettings)
	v1.GET("/audit-logs", readLimit, requireAdmin, middleware.RequireScope(middleware.ScopeSystemRead), h.ListAuditLogs)

	// 节点
	nodes := v1.Group("/nodes")
	{
		nodes.GET("", readLimit, middleware.RequireScope(middleware.ScopeNodeRead), h.ListNodes)
		nodes.POST("", middleware.WriteRateLimit(), middleware.RequireScope(middleware.ScopeNodeWrite), h.CreateNode)
		nodes.GET("/stream", h.NodesStream)

		nodes.GET("/:id", readLimit, middleware.RequireScope(middleware.ScopeNodeRead), h.GetNode)
		nodes.PUT("/:id", middleware.WriteRateLimit(), middleware.RequireScope(middleware.ScopeNodeWrite), h.UpdateNode)
		nodes.DELETE("/:id", middleware.WriteRateLimit(), requireAdmin, middleware.RequireScope(middleware.ScopeNodeWrite), h.DeleteNode)
		nodes.POST("/:id/install-command", middleware.RequireScope(middleware.ScopeNodeWrite), h.NodeInstallCommand)
		nodes.POST("/:id/upgrade", middleware.BatchRateLimit(), middleware.RequireScope(middleware.ScopeNodeExec), h.NodeUpgrade)
		nodes.POST("/:id/restart", middleware.BatchRateLimit(), middleware.RequireScope(middleware.ScopeNodeExec), h.NodeRestart)
		nodes.POST("/:id/exec", middleware.BatchRateLimit(), middleware.RequireScope(middleware.ScopeNodeExec), h.NodeExec)
		nodes.POST("/:id/reset-token", middleware.WriteRateLimit(), middleware.RequireScope(middleware.ScopeNodeWrite), h.NodeResetToken)
		nodes.GET("/:id/metrics", readLimit, middleware.RequireScope(middleware.ScopeNodeRead), h.NodeMetrics)
		nodes.GET("/:id/metrics/realtime", readLimit, middleware.RequireScope(middleware.ScopeNodeRead), h.NodeMetricsRealtime)
		nodes.GET("/:id/rules", readLimit, middleware.RequireScope(middleware.ScopeRuleRead), h.NodeRules)
		nodes.GET("/:id/logs", readLimit, middleware.RequireScope(middleware.ScopeNodeRead), h.NodeLogs)
		nodes.GET("/:id/drift", readLimit, middleware.RequireScope(middleware.ScopeNodeRead), h.NodeDrift)
		nodes.POST("/:id/drift/fix", middleware.RequireScope(middleware.ScopeNodeWrite), h.NodeFixDrift)
		nodes.GET("/:id/terminal", middleware.RequireScope(middleware.ScopeNodeExec), h.NodeTerminal)

		nodes.POST("/batch/upgrade", middleware.BatchRateLimit(), middleware.RequireScope(middleware.ScopeNodeExec), h.NodesBatchUpgrade)
		nodes.POST("/batch/restart", middleware.BatchRateLimit(), middleware.RequireScope(middleware.ScopeNodeExec), h.NodesBatchRestart)
		nodes.POST("/batch/exec", middleware.BatchRateLimit(), middleware.RequireScope(middleware.ScopeNodeExec), h.NodesBatchExec)
		nodes.POST("/batch/group", middleware.BatchRateLimit(), middleware.RequireScope(middleware.ScopeNodeWrite), h.NodesBatchGroup)
		nodes.POST("/batch/weight", middleware.BatchRateLimit(), middleware.RequireScope(middleware.ScopeNodeWrite), h.NodesBatchWeight)
	}

	// 节点分组
	s.crudRoutes(v1.Group("/node-groups"), readLimit,
		middleware.RequireScope(middleware.ScopeGroupRead),
		middleware.RequireScope(middleware.ScopeGroupWrite),
		h.ListNodeGroups, h.CreateNodeGroup, h.GetNodeGroup, h.UpdateNodeGroup, h.DeleteNodeGroup)
	v1.GET("/node-groups/:id/nodes", readLimit, middleware.RequireScope(middleware.ScopeGroupRead), h.NodeGroupNodes)
	v1.GET("/node-groups/:id/export", readLimit, middleware.RequireScope(middleware.ScopeGroupRead), h.NodeGroupExportCSV)

	// 设备组
	dg := v1.Group("/device-groups")
	{
		dg.GET("", readLimit, middleware.RequireScope(middleware.ScopeGroupRead), h.ListDeviceGroups)
		dg.POST("", middleware.WriteRateLimit(), middleware.RequireScope(middleware.ScopeGroupWrite), h.CreateDeviceGroup)
		dg.GET("/:id", readLimit, middleware.RequireScope(middleware.ScopeGroupRead), h.GetDeviceGroup)
		dg.PUT("/:id", middleware.WriteRateLimit(), middleware.RequireScope(middleware.ScopeGroupWrite), h.UpdateDeviceGroup)
		dg.DELETE("/:id", middleware.WriteRateLimit(), requireAdmin, middleware.RequireScope(middleware.ScopeGroupWrite), h.DeleteDeviceGroup)
		dg.GET("/:id/schema", readLimit, middleware.RequireScope(middleware.ScopeGroupRead), h.DeviceGroupSchema)
		dg.POST("/:id/validate", middleware.RequireScope(middleware.ScopeGroupWrite), h.ValidateDeviceGroup)
		dg.GET("/:id/health", readLimit, middleware.RequireScope(middleware.ScopeGroupRead), h.DeviceGroupHealth)
		dg.POST("/:id/reorder", middleware.WriteRateLimit(), middleware.RequireScope(middleware.ScopeGroupWrite), h.DeviceGroupReorder)
	}
	// 新建组时尚无 ID，schema 用 type 查询。
	v1.GET("/device-groups-schema", readLimit, middleware.RequireScope(middleware.ScopeGroupRead), h.DeviceGroupSchemaByType)

	// 转发规则
	fr := v1.Group("/forward-rules")
	{
		fr.GET("", readLimit, middleware.RequireScope(middleware.ScopeRuleRead), h.ListForwardRules)
		fr.POST("", middleware.WriteRateLimit(), middleware.RequireScope(middleware.ScopeRuleWrite), h.CreateForwardRule)
		fr.GET("/export", readLimit, middleware.RequireScope(middleware.ScopeRuleRead), h.ExportForwardRules)
		fr.POST("/import", middleware.BatchRateLimit(), middleware.RequireScope(middleware.ScopeRuleWrite), h.ImportForwardRules)
		fr.POST("/batch", middleware.BatchRateLimit(), middleware.RequireScope(middleware.ScopeRuleWrite), h.BatchForwardRules)
		fr.POST("/batch-multiplier", middleware.BatchRateLimit(), middleware.RequireScope(middleware.ScopeRuleWrite), h.BatchMultiplier)
		fr.GET("/:id", readLimit, middleware.RequireScope(middleware.ScopeRuleRead), h.GetForwardRule)
		fr.PUT("/:id", middleware.WriteRateLimit(), middleware.RequireScope(middleware.ScopeRuleWrite), h.UpdateForwardRule)
		fr.DELETE("/:id", middleware.WriteRateLimit(), requireAdmin, middleware.RequireScope(middleware.ScopeRuleWrite), h.DeleteForwardRule)
		fr.POST("/:id/enable", middleware.WriteRateLimit(), middleware.RequireScope(middleware.ScopeRuleWrite), h.EnableForwardRule)
		fr.POST("/:id/disable", middleware.WriteRateLimit(), middleware.RequireScope(middleware.ScopeRuleWrite), h.DisableForwardRule)
		fr.POST("/:id/resync", middleware.WriteRateLimit(), middleware.RequireScope(middleware.ScopeRuleWrite), h.ResyncForwardRule)
		fr.GET("/:id/traffic", readLimit, middleware.RequireScope(middleware.ScopeTrafficRead), h.RuleTraffic)
		fr.GET("/:id/sessions", readLimit, middleware.RequireScope(middleware.ScopeRuleRead), h.RuleSessions)
	}

	// 规则分组
	s.crudRoutes(v1.Group("/rule-groups"), readLimit,
		middleware.RequireScope(middleware.ScopeRuleRead),
		middleware.RequireScope(middleware.ScopeRuleWrite),
		h.ListRuleGroups, h.CreateRuleGroup, h.GetRuleGroup, h.UpdateRuleGroup, h.DeleteRuleGroup)
	v1.POST("/rule-groups/reorder", middleware.WriteRateLimit(), middleware.RequireScope(middleware.ScopeRuleWrite), h.ReorderRuleGroups)

	// 用户与用户分组
	users := v1.Group("/users")
	{
		users.GET("", readLimit, middleware.RequireScope(middleware.ScopeUserRead), h.ListUsers)
		users.POST("", middleware.WriteRateLimit(), requireAdmin, middleware.RequireScope(middleware.ScopeUserWrite), h.CreateUser)
		users.GET("/:id", readLimit, middleware.RequireScope(middleware.ScopeUserRead), h.GetUser)
		users.PUT("/:id", middleware.WriteRateLimit(), requireAdmin, middleware.RequireScope(middleware.ScopeUserWrite), h.UpdateUser)
		users.DELETE("/:id", middleware.WriteRateLimit(), requireAdmin, middleware.RequireScope(middleware.ScopeUserWrite), h.DeleteUser)
		users.POST("/:id/reset-password", middleware.WriteRateLimit(), requireAdmin, middleware.RequireScope(middleware.ScopeUserWrite), h.ResetUserPassword)
		users.POST("/:id/reset-token", middleware.WriteRateLimit(), middleware.RequireScope(middleware.ScopeUserWrite), h.ResetUserToken)
		users.POST("/:id/disable", middleware.WriteRateLimit(), requireAdmin, middleware.RequireScope(middleware.ScopeUserWrite), h.DisableUser)
		users.GET("/:id/traffic", readLimit, middleware.RequireScope(middleware.ScopeTrafficRead), h.UserTraffic)
		users.GET("/:id/rules", readLimit, middleware.RequireScope(middleware.ScopeUserRead), h.UserRules)
	}

	userGroups := v1.Group("/user-groups")
	{
		userGroups.GET("", readLimit, middleware.RequireScope(middleware.ScopeGroupRead), h.ListUserGroups)
		userGroups.POST("", middleware.WriteRateLimit(), requireAdmin, middleware.RequireScope(middleware.ScopeGroupWrite), h.CreateUserGroup)
		userGroups.GET("/:id", readLimit, middleware.RequireScope(middleware.ScopeGroupRead), h.GetUserGroup)
		userGroups.PUT("/:id", middleware.WriteRateLimit(), requireAdmin, middleware.RequireScope(middleware.ScopeGroupWrite), h.UpdateUserGroup)
		userGroups.DELETE("/:id", middleware.WriteRateLimit(), requireAdmin, middleware.RequireScope(middleware.ScopeGroupWrite), h.DeleteUserGroup)
	}

	// 订阅（无需认证，按 token 限流）
	s.engine.GET("/api/v1/sub/:token", middleware.SubscribeRateLimit(), h.Subscribe)
	s.engine.GET("/sub/:token", middleware.SubscribeRateLimit(), h.Subscribe)

	// 流量统计
	traffic := v1.Group("/traffic")
	{
		traffic.GET("/overview", readLimit, middleware.RequireScope(middleware.ScopeTrafficRead), h.TrafficOverview)
		traffic.GET("/timeseries", readLimit, middleware.RequireScope(middleware.ScopeTrafficRead), h.TrafficTimeseries)
		traffic.GET("/top", readLimit, middleware.RequireScope(middleware.ScopeTrafficRead), h.TrafficTop)
		traffic.GET("/export", readLimit, middleware.RequireScope(middleware.ScopeTrafficRead), h.TrafficExport)
		traffic.GET("/dashboard", readLimit, middleware.RequireScope(middleware.ScopeTrafficRead), h.Dashboard)
	}

	// 探针与监控
	probe := v1.Group("/probe")
	{
		probe.GET("/overview", readLimit, middleware.RequireScope(middleware.ScopeNodeRead), h.ProbeOverview)
		probe.GET("/nodes/:id", readLimit, middleware.RequireScope(middleware.ScopeNodeRead), h.ProbeNode)
		probe.GET("/cleanup", middleware.BatchRateLimit(), middleware.RequireScope(middleware.ScopeSystemWrite), h.ProbeCleanup)
	}

	// 告警
	alerts := v1.Group("/alerts")
	{
		alerts.GET("", readLimit, middleware.RequireScope(middleware.ScopeSystemRead), h.ListAlerts)
		alerts.POST("", middleware.WriteRateLimit(), middleware.RequireScope(middleware.ScopeSystemWrite), h.CreateAlert)
		alerts.GET("/history", readLimit, middleware.RequireScope(middleware.ScopeSystemRead), h.AlertHistory)
		alerts.PUT("/:id", middleware.WriteRateLimit(), middleware.RequireScope(middleware.ScopeSystemWrite), h.UpdateAlert)
		alerts.DELETE("/:id", middleware.WriteRateLimit(), middleware.RequireScope(middleware.ScopeSystemWrite), h.DeleteAlert)
		alerts.POST("/:id/test", middleware.BatchRateLimit(), middleware.RequireScope(middleware.ScopeSystemWrite), h.TestAlert)
		alerts.POST("/:id/resolve", middleware.WriteRateLimit(), middleware.RequireScope(middleware.ScopeSystemWrite), h.ResolveAlert)
	}
	v1.POST("/webhooks/test", middleware.BatchRateLimit(), middleware.RequireScope(middleware.ScopeSystemWrite), h.TestWebhook)

	// 快照
	snap := v1.Group("/snapshots")
	{
		snap.GET("", readLimit, middleware.RequireScope(middleware.ScopeSystemRead), h.ListSnapshots)
		snap.POST("", middleware.WriteRateLimit(), middleware.RequireScope(middleware.ScopeSystemWrite), h.CreateSnapshot)
		snap.GET("/:id", readLimit, middleware.RequireScope(middleware.ScopeSystemRead), h.GetSnapshot)
		snap.DELETE("/:id", middleware.WriteRateLimit(), requireAdmin, middleware.RequireScope(middleware.ScopeSystemWrite), h.DeleteSnapshot)
		snap.GET("/:id/diff", readLimit, middleware.RequireScope(middleware.ScopeSystemRead), h.SnapshotDiff)
		snap.POST("/:id/rollback", middleware.BatchRateLimit(), requireAdmin, middleware.RequireScope(middleware.ScopeSystemWrite), h.SnapshotRollback)
	}

	// API Token 管理
	tokens := v1.Group("/api-tokens")
	{
		tokens.GET("", readLimit, requireAdmin, middleware.RequireScope(middleware.ScopeSystemWrite), h.ListAPITokens)
		tokens.POST("", middleware.WriteRateLimit(), requireAdmin, middleware.RequireScope(middleware.ScopeSystemWrite), h.CreateAPIToken)
		tokens.DELETE("/:id", middleware.WriteRateLimit(), requireAdmin, middleware.RequireScope(middleware.ScopeSystemWrite), h.DeleteAPIToken)
	}

	// 任务
	tasks := v1.Group("/tasks")
	{
		tasks.GET("", readLimit, middleware.RequireScope(middleware.ScopeSystemRead), h.ListTasks)
		tasks.POST("/:name/run", middleware.BatchRateLimit(), middleware.RequireScope(middleware.ScopeSystemWrite), h.RunTask)
	}

	// 迁移与备份
	mig := v1.Group("/migrations")
	{
		mig.POST("/precheck", middleware.BatchRateLimit(), middleware.RequireScope(middleware.ScopeMigrateRun), h.MigrationPrecheck)
		mig.POST("/run", middleware.BatchRateLimit(), middleware.RequireScope(middleware.ScopeMigrateRun), h.MigrationRun)
		mig.GET("", readLimit, middleware.RequireScope(middleware.ScopeMigrateRun), h.ListMigrations)
		mig.GET("/:id", readLimit, middleware.RequireScope(middleware.ScopeMigrateRun), h.GetMigration)
		mig.GET("/:id/progress", h.MigrationProgress)
		mig.POST("/:id/rollback", middleware.BatchRateLimit(), requireAdmin, middleware.RequireScope(middleware.ScopeMigrateRun), h.MigrationRollback)
	}

	backups := v1.Group("/backups")
	{
		backups.POST("", middleware.BatchRateLimit(), middleware.RequireScope(middleware.ScopeBackupRun), h.CreateBackup)
		backups.GET("", readLimit, middleware.RequireScope(middleware.ScopeBackupRun), h.ListBackups)
		backups.GET("/:id/download", middleware.RequireScope(middleware.ScopeBackupRun), h.DownloadBackup)
		backups.POST("/:id/restore", middleware.BatchRateLimit(), requireAdmin, middleware.RequireScope(middleware.ScopeBackupRun), h.RestoreBackup)
	}

	// ── 节点通信接口（不带 /v1，用节点 Token 认证）──────────
	nodeGroup := s.engine.Group("/api/node")
	{
		// 注册接口用 body 中的 token 认证，不能走 NodeAuthenticate。
		nodeGroup.POST("/register", h.NodeRegister)

		nodeAuth := nodeGroup.Group("")
		nodeAuth.Use(middleware.NodeAuthenticate(authDeps))
		{
			nodeAuth.POST("/heartbeat", h.NodeHeartbeat)
			nodeAuth.GET("/config", h.NodeConfig)
			nodeAuth.POST("/report", h.NodeReport)
			nodeAuth.GET("/tasks", h.NodeTasks)
			nodeAuth.POST("/task-result", h.NodeTaskResult)
			nodeAuth.GET("/stream", h.NodeStream)
		}
	}

	// 安装脚本与二进制：无需认证（按 token 参数模板化）。
	s.engine.GET("/install.sh", h.InstallScript)
	s.engine.GET("/api/node/install.sh", h.InstallScript)
	s.engine.GET("/uninstall.sh", h.UninstallScript)
	s.engine.GET("/api/node/binary/:arch", h.NodeBinary)

	// ── API 文档 ────────────────────────────────────────────
	if a.Setting.GetBool("openapi_docs", true) {
		s.engine.GET("/api/docs", h.DocsPage)
	}

	// ── 前端静态资源与 SPA 回退 ─────────────────────────────
	s.registerStatic(a.Config.HTMLPath)
}

// crudRoutes 注册一组标准的 REST 路由，减少重复代码。
//
// 参数 group 为路由组；readLimit 为读接口限流；
// readScope / writeScope 为读写权限范围；五个 handler 依次对应
// 列表 / 创建 / 详情 / 更新 / 删除。
func (s *Server) crudRoutes(
	group *gin.RouterGroup,
	readLimit gin.HandlerFunc,
	readScope, writeScope gin.HandlerFunc,
	list, create, get, update, del gin.HandlerFunc,
) {
	group.GET("", readLimit, readScope, list)
	group.POST("", middleware.WriteRateLimit(), writeScope, create)
	group.GET("/:id", readLimit, readScope, get)
	group.PUT("/:id", middleware.WriteRateLimit(), writeScope, update)
	group.DELETE("/:id", middleware.WriteRateLimit(), writeScope, del)
}

// registerStatic 注册前端静态资源服务。
//
// 行为：
//   - html-path 存在时提供静态文件；
//   - 未命中静态文件的路径回退到 index.html，支持前端路由（SPA）；
//   - html-path 不存在时返回一段引导文案，而不是 404 空白页。
func (s *Server) registerStatic(htmlPath string) {
	indexPath := filepath.Join(htmlPath, "index.html")

	s.engine.NoRoute(func(c *gin.Context) {
		// API 路径未命中时返回统一的 404 响应体，而不是前端页面。
		if strings.HasPrefix(c.Request.URL.Path, "/api/") ||
			strings.HasPrefix(c.Request.URL.Path, "/sub/") {
			response.Fail(c, response.New(response.CodeNotFound, "接口不存在"))
			return
		}

		// 静态资源存在则直接返回。
		reqPath := filepath.Clean(strings.TrimPrefix(c.Request.URL.Path, "/"))
		if reqPath != "." && reqPath != "" {
			full := filepath.Join(htmlPath, reqPath)
			// 防止路径穿越：拼接后的路径必须仍位于 html-path 之内。
			if isSubPath(htmlPath, full) {
				if st, err := os.Stat(full); err == nil && !st.IsDir() {
					c.File(full)
					return
				}
			}
		}

		// SPA 回退：交给前端路由处理。
		if _, err := os.Stat(indexPath); err == nil {
			c.File(indexPath)
			return
		}

		// 前端产物缺失时的引导页。
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.String(http.StatusOK, missingFrontendPage(htmlPath))
	})

	s.engine.GET("/", func(c *gin.Context) {
		if _, err := os.Stat(indexPath); err == nil {
			c.File(indexPath)
			return
		}
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.String(http.StatusOK, missingFrontendPage(htmlPath))
	})
}

// isSubPath 判断 target 是否位于 base 目录之内，用于阻止路径穿越攻击。
func isSubPath(base, target string) bool {
	absBase, err1 := filepath.Abs(base)
	absTarget, err2 := filepath.Abs(target)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(absBase, absTarget)
	if err != nil {
		return false
	}
	return !strings.HasPrefix(rel, "..")
}

// missingFrontendPage 返回前端产物缺失时的引导页面。
func missingFrontendPage(htmlPath string) string {
	return `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<title>OpenRoute 面板</title>
<style>
  body { font-family: -apple-system, "Segoe UI", "Microsoft YaHei", sans-serif;
         max-width: 720px; margin: 80px auto; padding: 0 24px; color: #1f2329; line-height: 1.7; }
  code { background: #f2f3f5; padding: 2px 6px; border-radius: 4px; }
  .box { background: #f7f8fa; border-left: 4px solid #165dff; padding: 16px 20px; margin: 20px 0; }
  h1 { font-size: 22px; }
</style>
</head>
<body>
  <h1>后端已启动，但未找到前端资源</h1>
  <div class="box">
    <p>API 已经可以正常使用，例如：</p>
    <p><code>curl http://127.0.0.1:18888/api/v1/health</code></p>
  </div>
  <p>请先构建前端产物，再刷新本页面：</p>
  <p><code>cd frontend &amp;&amp; npm install &amp;&amp; npm run build</code></p>
  <p>构建产物需要位于 <code>` + htmlPath + `</code>（由 <code>config.yml</code> 的
     <code>html-path</code> 指定）。</p>
</body>
</html>`
}
