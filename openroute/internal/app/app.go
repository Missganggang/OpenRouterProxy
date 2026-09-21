// Package app 是运行时的依赖容器，把配置、数据库、日志与各业务服务装配在一起。
//
// 存在的意义是避免全局变量：所有需要依赖的地方通过 *App 获取，
// 测试时可以构造一份独立的 App 而不互相污染。
package app

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/openroute/openroute/internal/config"
	"github.com/openroute/openroute/internal/database"
)

// App 持有全部长生命周期依赖。
type App struct {
	Config *config.Config
	DB     *database.DB
	Log    *zap.Logger

	// StartedAt 记录进程启动时间，供 /system/info 展示运行时长。
	StartedAt time.Time

	// 各业务服务，在 wire() 中装配。
	Audit    *AuditService
	Setting  *SettingService
	Auth     *AuthService
	Snapshot *SnapshotService
	Sync     *SyncService
	Node     *NodeService
	Group    *GroupService
	Traffic  *TrafficService
	Probe    *ProbeService
	Rule     *RuleService
	Alert    *AlertService
	Migrate  *MigrateService
	Backup   *BackupService
	User     *UserService
	// Webhook 负责出站通知（规格书 8.18），供节点上下线、同步失败、告警等事件调用。
	Webhook *WebhookService

	// hub 是节点实时状态推送枢纽（WebSocket）。
	hub *Hub

	// versionMu 保护 configVersion。
	versionMu     sync.RWMutex
	configVersion int64

	// stopCh 用于通知后台任务退出。
	stopCh chan struct{}
	// wg 等待后台任务结束，实现优雅关闭。
	wg sync.WaitGroup
}

// New 构造 App 实例，只做字段填充与依赖装配，不触发网络 IO。
//
// 参数 cfg 为已加载校验的配置；db 为已连接的数据库；log 为已初始化的日志器。
// 返回可直接使用的 App。
func New(cfg *config.Config, db *database.DB, log *zap.Logger) *App {
	a := &App{
		Config:    cfg,
		DB:        db,
		Log:       log,
		StartedAt: time.Now().UTC(),
		stopCh:    make(chan struct{}),
	}
	a.hub = newHub(log)
	return a
}

// Init 装配全部业务服务。
//
// 与 New 分开的原因：New 只做纯粹的字段填充，Init 会因为构造服务
// 而启动后台 goroutine（如验证码清理），后者在测试中未必希望发生。
//
// 返回装配完成的 App 自身，便于链式调用。
func (a *App) Init() *App {
	a.wire()
	return a
}

// wire 装配各业务服务。
//
// 拆分出来是为了让依赖顺序显式可见：审计与设置最先（其它服务会用到它们），
// 规则与分组随后（同步服务依赖规则），最后是迁移与备份。
func (a *App) wire() {
	a.Audit = NewAuditService(a)
	a.Setting = NewSettingService(a)
	a.Auth = NewAuthService(a)
	a.Webhook = NewWebhookService(a)
	a.Snapshot = NewSnapshotService(a)
	a.Sync = NewSyncService(a)
	a.Node = NewNodeService(a)
	a.Group = NewGroupService(a)
	a.Traffic = NewTrafficService(a)
	a.Probe = NewProbeService(a)
	a.Rule = NewRuleService(a)
	a.Alert = NewAlertService(a)
	a.User = NewUserService(a)
	a.Migrate = NewMigrateService(a)
	a.Backup = NewBackupService(a)
}

// ConfigVersion 返回当前的全局配置版本号（规格书 5.4）。
func (a *App) ConfigVersion() int64 {
	a.versionMu.RLock()
	defer a.versionMu.RUnlock()
	return a.configVersion
}

// SetConfigVersion 在启动时把版本号初始化为数据库中的当前值。
func (a *App) SetConfigVersion(v int64) {
	a.versionMu.Lock()
	a.configVersion = v
	a.versionMu.Unlock()
}

// BumpConfigVersion 自增全局配置版本号并通知所有在线节点拉取新配置。
//
// 这是「规则变更在 20 秒内到达节点」的即时推送触发点：
// 调用方在写完库后调用本方法，长连接推送失败时会自动降级为节点侧轮询兜底。
//
// 参数 reason 用于日志定位是哪个操作触发的变更。
// 返回自增后的新版本号。
func (a *App) BumpConfigVersion(reason string) int64 {
	a.versionMu.Lock()
	a.configVersion++
	v := a.configVersion
	a.versionMu.Unlock()

	a.Log.Info("配置版本已更新",
		zap.Int64("config_version", v),
		zap.String("reason", reason))

	// 尽力而为地立即推送；失败不影响一致性，节点会轮询兜底。
	a.hub.Broadcast(Event{
		Type: "config_changed",
		Data: map[string]interface{}{
			"config_version": v,
			"reason":         reason,
		},
	})
	return v
}

// Hub 返回实时推送枢纽，供 WebSocket handler 注册连接。
func (a *App) Hub() *Hub { return a.hub }

// Context 返回后台任务使用的根上下文。
//
// 任务应当同时 select a.Done() 来感知进程关闭。
func (a *App) Context() context.Context {
	return context.Background()
}

// Stop 触发优雅关闭：通知后台任务退出并等待它们结束。
//
// 重复调用是安全的（内部对通道关闭做了保护）。
func (a *App) Stop() {
	select {
	case <-a.stopCh:
		// 已经关闭过，避免重复 close 造成 panic。
	default:
		close(a.stopCh)
	}
	a.wg.Wait()
	a.hub.Close()
}

// Done 返回关闭信号通道，后台任务可 select 它来感知退出。
func (a *App) Done() <-chan struct{} { return a.stopCh }

// Go 启动一个受 App 生命周期管理的后台 goroutine。
//
// 用法：a.Go(func(ctx context.Context) { ... })
// Stop 时会等待所有通过本方法启动的 goroutine 返回。
func (a *App) Go(fn func(ctx context.Context)) {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		fn(a.Context())
	}()
}

// Uptime 返回进程已运行的秒数。
func (a *App) Uptime() int64 {
	return int64(time.Since(a.StartedAt).Seconds())
}

// Version 是面板后端的版本号，供横幅与 /system/version 使用。
const Version = "v1.0.0"

// BuildStamp 是构建时间戳，发布时通过 -ldflags 注入，默认值为开发构建。
var BuildStamp = "dev"
