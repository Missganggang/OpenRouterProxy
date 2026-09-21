package app

// DefaultAdminUsername 是首次初始化时使用的默认管理员用户名。
//
// 可通过环境变量 ADMIN 覆盖（规格书 3.3 第 7 步）。
const DefaultAdminUsername = "admin"

// 系统设置的补充键名。
//
// model 包中的 SettingXXX 常量覆盖「设置页直接可编辑」的项；
// 这里放的是运行期开关，设置页不一定暴露。
const (
	// SettingSnapshotDailyEnabled 控制每日自动快照是否开启（规格书 6.13）。
	SettingSnapshotDailyEnabled = "snapshot_daily_enabled"
)
