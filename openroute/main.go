// OpenRoute 面板入口。
//
// 启动流程严格按规格书 3.3 的顺序执行：
//
//  1. 解析命令行参数；-h/-v 等立即执行并退出
//  2. 加载 config.yml（不存在则写默认并提示）
//  3. 初始化日志
//  4. 打印横幅
//  5. 连接数据库
//  6. 执行 AutoMigrate（MIGRATE=1 或表结构缺失时）
//  7. 用户表为空时创建管理员（ADMIN 环境变量或交互式引导）
//  8. 启动后台任务调度器
//  9. 启动节点通信服务
//  10. 启动 HTTP 服务
//  11. 等待 SIGINT/SIGTERM 并优雅关闭
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm/logger"

	"github.com/openroute/openroute/internal/api"
	"github.com/openroute/openroute/internal/app"
	"github.com/openroute/openroute/internal/config"
	"github.com/openroute/openroute/internal/database"
	"github.com/openroute/openroute/internal/job"
)

func main() {
	// 1. 解析命令行参数
	opts := parseFlags()

	// -h / -v 等无需初始化即可完成的操作，直接执行并退出。
	if opts.ShowHelp {
		printHelp()
		os.Exit(0)
	}
	if opts.ShowVersion {
		fmt.Printf("%s (build %s)\n", app.Version, config.BuildStampNow())
		os.Exit(0)
	}

	// 2. 加载配置
	cfgPath := opts.ConfigPath
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fatal("加载配置失败", err)
	}
	if err := cfg.Validate(); err != nil {
		fatal("配置校验失败（请修改 config.yml 后重试）", err)
	}

	// 3. 初始化日志
	log := cfg.NewLogger()
	defer func() { _ = log.Sync() }()

	// -check 自检：只做检查，不启动服务。
	if opts.Check {
		os.Exit(runSelfCheck(cfg, log))
	}

	// -reset-password 交互式重置管理员密码。
	if opts.ResetPassword {
		os.Exit(runResetPassword(cfg, log))
	}

	// -backup / -restore
	if opts.BackupPath != "" {
		os.Exit(runBackup(cfg, log, opts.BackupPath, opts.WithSecret))
	}
	if opts.RestorePath != "" {
		os.Exit(runRestore(cfg, log, opts.RestorePath))
	}

	// -copy-database 跨库转换
	if opts.CopyDatabase != "" {
		os.Exit(runCopyDatabase(cfg, log, opts.CopyDatabase, opts.Force))
	}

	// -migrate 从其它面板迁移
	if opts.MigrateSpec != "" {
		os.Exit(runMigrate(cfg, log, opts.MigrateSpec))
	}

	// -clean 清理失效数据
	if opts.Clean != 0 {
		os.Exit(runClean(cfg, log, opts.Clean))
	}

	// 5. 连接数据库
	db, err := cfg.OpenDB()
	if err != nil {
		fatal("数据库连接失败", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			log.Warn("关闭数据库连接失败", zap.Error(err))
		}
	}()

	// 6. 自动迁移：MIGRATE=1 或无表结构时执行。
	migrateEnv := os.Getenv("MIGRATE") == "1"
	needMigrate := migrateEnv || !db.Migrator().HasTable("users")
	if needMigrate {
		diffs, err := db.AutoMigrate()
		if err != nil {
			fatal("数据库结构迁移失败", err)
		}
		printDiffs(log, diffs)
	}

	// 构造应用容器并装配业务服务。
	a := app.New(cfg, db, log).Init()

	// 初始化设置缓存与默认数据。
	ctx := context.Background()
	if err := a.Setting.EnsureInitialized(ctx); err != nil {
		fatal("初始化默认数据失败", err)
	}
	if err := a.Setting.Load(ctx); err != nil {
		fatal("加载系统设置失败", err)
	}

	// 初始化配置版本号：取数据库中的最大规则 updated_at 作为起点，
	// 保证重启后节点上报的 config_version 仍能正确比较。

	// 7. 用户表为空时创建管理员。
	if err := ensureAdmin(ctx, a); err != nil {
		fatal("创建管理员失败", err)
	}

	// 打印横幅
	printBanner(a, cfg)

	// 8. 启动后台任务调度器
	scheduler := job.NewScheduler(log)
	jobCount := job.RegisterAll(scheduler, a)
	if cfg.DisableCron {
		log.Info("disable-cron 已开启，后台任务不会被调度")
		// 仍然注册，只是不启动，这样任务列表页依然可见。
	} else {
		scheduler.Start(ctx)
	}
	_ = jobCount

	// 10. 启动 HTTP 服务（同时提供 API 与 WebUI）
	server := api.NewServer(api.Deps{App: a, Log: log, Jobs: scheduler})

	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.Engine(),
		ReadHeaderTimeout: 20 * time.Second,
		// 不设置 ReadTimeout/WriteTimeout：WebSSH 与实时推送需要长连接。
		IdleTimeout: 120 * time.Second,
	}

	// 端口占用检查：在启动前明确提示冲突，而不是抛一个裸错误。
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		fatal(fmt.Sprintf("监听地址 %s 失败（端口可能已被占用）", cfg.Listen), err)
	}

	serveErr := make(chan error, 1)
	go func() {
		if cfg.TLSCert != "" && cfg.TLSKey != "" {
			log.Info("已启用 TLS", zap.String("cert", cfg.TLSCert))
			serveErr <- httpServer.ServeTLS(ln, cfg.TLSCert, cfg.TLSKey)
			return
		}
		serveErr <- httpServer.Serve(ln)
	}()

	log.Info("HTTP 服务已启动",
		zap.String("listen", cfg.Listen),
		zap.String("webui", config.WebUIURL(cfg.Listen, cfg.TLSCert != "" && cfg.TLSKey != "")))

	// 11. 等待退出信号
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-quit:
		log.Info("收到退出信号，开始优雅关闭", zap.String("signal", sig.String()))
	case err := <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			log.Error("HTTP 服务异常退出", zap.Error(err))
		}
	}

	// 优雅关闭：停止接受新连接 → 等待在途请求（最多 10 秒）→ 停止后台任务。
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Warn("HTTP 服务关闭超时，可能有在途请求未完成", zap.Error(err))
	}
	scheduler.Stop()
	a.Stop()

	log.Info("OpenRoute 已停止")
}

// ensureAdmin 在用户表为空时创建管理员（规格书 3.3 第 7 步）。
//
// 优先读取 ADMIN / ADMIN_PASSWORD 环境变量；
// 未提供密码时进入交互式引导（终端可用时）或直接报错退出。
func ensureAdmin(ctx context.Context, a *app.App) error {
	envUser := os.Getenv("ADMIN")
	envPass := os.Getenv("ADMIN_PASSWORD")

	created, err := a.Setting.EnsureAdmin(ctx, envUser, envPass)
	if err != nil {
		if err == app.ErrNoAdminCredentials {
			// 进入交互式引导。
			pwd, ierr := promptAdminPassword(envUser)
			if ierr != nil {
				return ierr
			}
			created, err = a.Setting.EnsureAdmin(ctx, envUser, pwd)
			if err != nil {
				return err
			}
		} else {
			return err
		}
	}

	if created != nil {
		fmt.Printf("\n管理员账号已创建：%s\n", created.Username)
		if envPass == "" {
			fmt.Println("请使用你刚才输入的密码登录，并尽快在「设置 → 安全」中修改。")
		}
		fmt.Println()
	}
	return nil
}

// printBanner 打印启动横幅。
func printBanner(a *app.App, cfg *config.Config) {
	info := config.BannerInfo{
		Version:         app.Version,
		BuildStamp:      app.BuildStamp,
		DatabaseDisplay: a.DB.Dialect.Display,
		Listen:          cfg.Listen,
		WebUI:           config.WebUIURL(cfg.Listen, cfg.TLSCert != "" && cfg.TLSKey != ""),
		LogPath:         cfg.LogPath,
		HeartbeatSec:    cfg.HeartbeatInterval,
		OfflineSec:      cfg.OfflineNodeTime,
		JobCount:        8,
		HTMLPath:        cfg.HTMLPath,
	}
	if err := config.PrintBanner(info); err != nil {
		fmt.Fprintf(os.Stderr, "打印横幅失败: %v\n", err)
	}
}

// printDiffs 打印数据库结构变更 diff（规格书 4.6）。
//
// 无变更时也要明确说明，否则用户无法确认迁移是否真的执行过。
func printDiffs(log *zap.Logger, diffs []database.ColumnDiff) {
	if len(diffs) == 0 {
		fmt.Println("数据库结构：无变更")
		return
	}
	fmt.Println("数据库结构变更：")
	for _, d := range diffs {
		fmt.Print(d.String())
	}
}

// fatal 打印致命错误并以退出码 1 结束。
//
// 规格书 3.1 要求数据库连接失败、端口占用等情况
// MUST 打印可读错误并返回退出码 1。
func fatal(msg string, err error) {
	fmt.Fprintf(os.Stderr, "\n启动失败：%s\n", msg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  错误详情: %v\n\n", err)
	} else {
		fmt.Fprintln(os.Stderr)
	}
	os.Exit(1)
}

// openLoggerLevel 把配置的日志级别映射为 Gorm 的日志级别。
func openLoggerLevel(cfg *config.Config) logger.LogLevel {
	if cfg.LogLevel == "debug" {
		return logger.Info
	}
	// 默认只记录慢查询与错误，避免 SQL 日志淹没业务日志。
	return logger.Warn
}
