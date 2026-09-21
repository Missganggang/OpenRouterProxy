package model

import "time"

// 用户角色常量。个人自用版收敛为两种角色。
const (
	RoleAdmin = "admin" // 管理员：全部权限
	RoleUser  = "user"  // 普通用户：仅可见自己的规则与流量
)

// 用户状态常量。
const (
	StatusDisabled = 0 // 禁用
	StatusEnabled  = 1 // 启用
)

// User 是用户表。
//
// 移除支付体系后，本表只服务于「自己 + 极少数共同使用的账号」：
// 没有注册入口、没有余额字段、没有订单关联，管理员在面板中手工建号。
// 保留多用户的目的是实现用户级限速、用户级流量统计与规则归属。
//
// Token 的唯一索引带 `where:token <> ”` 条件：
// 空 Token 不参与唯一性约束。原因是三种方言对「唯一索引中的空值」语义不同
// （PostgreSQL/SQLite 允许多个 NULL，MySQL 则把空串视为可重复），
// 加上条件后行为统一，且允许「暂时没有 Token」的行存在。
type User struct {
	ID           uint64 `gorm:"primaryKey" json:"id"`
	Username     string `gorm:"size:64;uniqueIndex;not null" json:"username"` // 登录名
	PasswordHash string `gorm:"size:128;not null" json:"-"`                   // bcrypt，绝不外泄
	Nickname     string `gorm:"size:64" json:"nickname"`
	Role         string `gorm:"size:16;default:user" json:"role"`                                   // admin | user
	Status       int    `gorm:"default:1" json:"status"`                                            // 1 启用 0 禁用
	GroupID      uint64 `gorm:"index" json:"group_id"`                                              // 用户分组
	Token        string `gorm:"size:64;uniqueIndex:idx_users_token,where:token <> ''" json:"token"` // 用户级订阅/API Token

	// 流量与限制
	TrafficUsed  int64 `gorm:"default:0" json:"traffic_used"`  // 已用流量（字节），按倍率折算后
	TrafficLimit int64 `gorm:"default:0" json:"traffic_limit"` // 流量上限，0 = 不限
	SpeedLimit   int64 `gorm:"default:0" json:"speed_limit"`   // 限速 byte/s，0 = 不限
	IPLimit      int   `gorm:"default:0" json:"ip_limit"`      // 同时在线 IP 数上限，0 = 不限
	DeviceLimit  int   `gorm:"default:0" json:"device_limit"`  // 同时在线设备数上限，0 = 不限
	ConnLimit    int   `gorm:"default:0" json:"conn_limit"`    // 并发连接数上限，0 = 不限

	// PasswordResetRequired 标记密码哈希算法不兼容（迁移自其它面板时可能出现），
	// 为 true 时用户首次登录必须改密。
	PasswordResetRequired bool `gorm:"default:false" json:"password_reset_required"`

	// PendingReinstall 标记该用户是由迁移工具从旧面板带入的，节点需重装。
	PendingReinstall bool `gorm:"default:false" json:"pending_reinstall"`

	ExpireAt    *time.Time `json:"expire_at"` // 有效期，nil = 永久（个人自用默认 nil）
	LastLoginAt *time.Time `json:"last_login_at"`
	LastLoginIP string     `gorm:"size:64" json:"last_login_ip"`
	Remark      string     `gorm:"size:255" json:"remark"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName 指定表名，符合「小写蛇形复数」的跨方言约定。
func (User) TableName() string { return "users" }

// IsAdmin 判断用户是否为管理员。
func (u *User) IsAdmin() bool { return u.Role == RoleAdmin }

// UserGroup 是用户分组表。
//
// 分组持有默认的流量/限速策略，用户未单独设置时继承。
type UserGroup struct {
	ID   uint64 `gorm:"primaryKey" json:"id"`
	Name string `gorm:"size:64;uniqueIndex;not null" json:"name"`
	// 该分组默认的流量/限速策略（用户未单独设置时继承）
	TrafficLimit int64 `gorm:"default:0" json:"traffic_limit"`
	SpeedLimit   int64 `gorm:"default:0" json:"speed_limit"`
	IPLimit      int   `gorm:"default:0" json:"ip_limit"`
	ConnLimit    int   `gorm:"default:0" json:"conn_limit"`
	// 可访问的规则分组白名单，空数组 = 全部可访问
	RuleGroupIDs JSON      `gorm:"type:text" json:"rule_group_ids"`
	Remark       string    `gorm:"size:255" json:"remark"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// TableName 指定表名。
func (UserGroup) TableName() string { return "user_groups" }
