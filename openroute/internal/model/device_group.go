package model

import "time"

// 设备组类型常量。
const (
	GroupTypeInbound  = "inbound"  // 入口组
	GroupTypeOutbound = "outbound" // 出口组
)

// 出口组负载均衡策略常量。
const (
	BalanceLeastConn  = "least_conn"  // 最少连接数（默认，对齐 Nyanpass）
	BalanceRoundRobin = "round_robin" // 轮询（加权平滑轮询 SWRR）
	BalanceHashIP     = "hash_ip"     // 源 IP 哈希
	BalanceWeighted   = "weighted"    // 按权重随机
)

// DeviceGroup 是设备组表，整个系统最重要的表。
//
// 设备组定义「一组机器的共同行为」：网络策略、协议、负载均衡、故障转移、SNI 策略。
// Config 字段存放入口组/出口组的原始 JSON（结构见规格书 4.3 / 4.4），
// 前端用表单编辑，后端存原文并做语义校验。
type DeviceGroup struct {
	ID   uint64 `gorm:"primaryKey" json:"id"`
	Name string `gorm:"size:64;uniqueIndex;not null" json:"name"`
	Type string `gorm:"size:16;not null;index" json:"type"` // inbound | outbound
	// 组成员（节点 ID 数组，有序；出口组的顺序参与负载均衡初始化）
	NodeIDs JSON `gorm:"type:text" json:"node_ids"`
	// 组的 JSON 配置，结构见规格书 4.3 / 4.4
	Config JSON `gorm:"type:text" json:"config"`

	// 出口组专用：负载均衡策略
	Balance string `gorm:"size:16;default:least_conn" json:"balance"`

	// 出口组专用：健康检查
	HealthCheckEnable    bool `gorm:"default:true" json:"health_check_enable"`
	HealthCheckInterval  int  `gorm:"default:10" json:"health_check_interval"`  // 秒
	HealthCheckTimeout   int  `gorm:"default:3" json:"health_check_timeout"`    // 秒
	HealthCheckFailCount int  `gorm:"default:3" json:"health_check_fail_count"` // 连续失败次数后摘除
	HealthCheckSuccCount int  `gorm:"default:2" json:"health_check_succ_count"` // 连续成功次数后恢复

	// 故障转移组 ID（出口组专用；本组全挂时转投该组）
	FailoverGroupID uint64 `gorm:"index" json:"failover_group_id"`

	Remark    string    `gorm:"size:255" json:"remark"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName 指定表名。
func (DeviceGroup) TableName() string { return "device_groups" }

// IsInbound 判断是否为入口组。
func (g *DeviceGroup) IsInbound() bool { return g.Type == GroupTypeInbound }

// IsOutbound 判断是否为出口组。
func (g *DeviceGroup) IsOutbound() bool { return g.Type == GroupTypeOutbound }

// RuleGroup 是规则分组表。
type RuleGroup struct {
	ID        uint64    `gorm:"primaryKey" json:"id"`
	Name      string    `gorm:"size:64;uniqueIndex;not null" json:"name"`
	Sort      int       `gorm:"default:0" json:"sort"`
	Remark    string    `gorm:"size:255" json:"remark"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName 指定表名。
func (RuleGroup) TableName() string { return "rule_groups" }
