package model

import (
	"fmt"
	"time"
)

// 规则同步状态常量（规格书 5.4 的状态机）。
const (
	SyncUnsynced = "unsynced" // 未同步（黄色）
	SyncSyncing  = "syncing"  // 同步中（蓝色）
	SyncNormal   = "normal"   // 正常（绿色）
	SyncFailed   = "failed"   // 同步失败（红色）+ 错误原因
)

// 目标状态常量。
const (
	TargetUp   = "up"   // 参与分发
	TargetDown = "down" // 已被目标故障转移摘除
)

// 目标级负载均衡策略（规格书 6.3）。
const (
	TargetBalanceFailover   = "failover"    // 主备
	TargetBalanceLeastConn  = "least_conn"  // 最少连接
	TargetBalanceRoundRobin = "round_robin" // 轮询
	TargetBalanceWeighted   = "weighted"    // 按权重
	TargetBalanceHashIP     = "hash_ip"     // 源 IP 哈希
)

// Target 是转发规则的一个目标地址。
//
// 一条规则可配置多个目标，实现目标级负载均衡与故障转移。
type Target struct {
	Host   string `json:"host"`             // 域名或 IP
	Port   int    `json:"port"`             // 目标端口
	Weight int    `json:"weight,omitempty"` // 目标级负载均衡权重，0 按 1 处理
	Status string `json:"status,omitempty"` // up | down
}

// NormalizedWeight 返回用于计算的有效权重，缺失或非法时按 1 处理。
func (t Target) NormalizedWeight() int {
	if t.Weight <= 0 {
		return 1
	}
	return t.Weight
}

// Up 判断目标是否参与分发（空状态视为 up，兼容手工导入的配置）。
func (t Target) Up() bool { return t.Status != TargetDown }

// String 返回 host:port 形式，便于日志与错误信息。
func (t Target) String() string { return fmt.Sprintf("%s:%d", t.Host, t.Port) }

// ForwardRule 是转发规则表，一条「入口监听端口 → 目标地址」的转发定义。
type ForwardRule struct {
	ID   uint64 `gorm:"primaryKey" json:"id"`
	Name string `gorm:"size:128;index;not null" json:"name"`
	// 归属
	UserID      uint64 `gorm:"index" json:"user_id"`       // 归属用户；0 = 系统/管理员
	RuleGroupID uint64 `gorm:"index" json:"rule_group_id"` // 规则分组

	// 入口侧
	InboundGroupID uint64 `gorm:"index;not null" json:"inbound_group_id"` // 入口设备组
	ListenPort     int    `gorm:"index" json:"listen_port"`               // 0 = 随机端口（新格式）
	ListenPortEnd  int    `gorm:"default:0" json:"listen_port_end"`       // 端口段结束（0 = 单端口）

	// 出口侧
	OutboundGroupID uint64 `gorm:"index" json:"outbound_group_id"` // 出口设备组；0 = 单端（入口直出）

	// 目标地址（支持多目标负载均衡）
	Targets JSON `gorm:"type:text" json:"targets"`

	// 目标级负载均衡策略
	TargetBalance string `gorm:"size:16;default:failover" json:"target_balance"`

	// 倍率
	InboundMultiplier  float64 `gorm:"default:1" json:"inbound_multiplier"`  // 入口倍率
	OutboundMultiplier float64 `gorm:"default:1" json:"outbound_multiplier"` // 出口倍率

	// 限制（叠加在用户限制之上，入口侧生效）
	SpeedLimit int64 `gorm:"default:0" json:"speed_limit"` // byte/s，0 = 不限
	ConnLimit  int   `gorm:"default:0" json:"conn_limit"`
	IPLimit    int   `gorm:"default:0" json:"ip_limit"`

	// 协议与高级选项（JSON，结构见规格书 4.5）
	Options JSON `gorm:"type:text" json:"options"`

	// 链式出口（2~3 跳），存设备组 ID 数组；空 = 不启用
	ChainGroups JSON `gorm:"type:text" json:"chain_groups"`

	// 反向隧道
	ReverseEnable  bool   `gorm:"default:false" json:"reverse_enable"`
	ReversePort    int    `gorm:"default:0" json:"reverse_port"`
	ReverseGroupID uint64 `gorm:"default:0" json:"reverse_group_id"`

	// 分流（SNI 分裂）
	IsSubRule bool   `gorm:"default:false;index" json:"is_sub_rule"` // 是否为子规则
	ParentID  uint64 `gorm:"index" json:"parent_id"`                 // 主规则 ID
	SNI       string `gorm:"size:255;index" json:"sni"`              // 子规则匹配的 SNI

	// 整流选项（规格书 6.8），存整形数组，空 = 不整流
	Shaping JSON `gorm:"type:text" json:"shaping"`

	// 状态机
	SyncStatus string     `gorm:"size:16;default:unsynced;index" json:"sync_status"`
	SyncError  string     `gorm:"size:512" json:"sync_error"`
	SyncedAt   *time.Time `json:"synced_at"`

	// 流量统计（累计，明细在 traffic_logs；已乘倍率）
	TrafficIn  int64 `gorm:"default:0" json:"traffic_in"`  // 入口方向累计（字节，已乘倍率）
	TrafficOut int64 `gorm:"default:0" json:"traffic_out"` // 出口方向累计（字节，已乘倍率）

	Enable    bool      `gorm:"default:true;index" json:"enable"`
	Remark    string    `gorm:"size:255" json:"remark"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName 指定表名。
func (ForwardRule) TableName() string { return "forward_rules" }

// TargetList 解析 Targets JSON 为切片，解析失败返回空切片。
func (r *ForwardRule) TargetList() []Target {
	out := []Target{}
	if len(r.Targets) == 0 {
		return out
	}
	_ = unmarshalJSON(r.Targets, &out)
	if out == nil {
		return []Target{}
	}
	return out
}

// SetTargets 把目标列表序列化回 JSON 字段。
func (r *ForwardRule) SetTargets(list []Target) {
	r.Targets = FromAny(list)
}

// ChainGroupList 解析链式出口设备组 ID。
func (r *ForwardRule) ChainGroupList() []uint64 { return r.ChainGroups.AsUint64Slice() }

// ShapingList 解析整流选项列表。
func (r *ForwardRule) ShapingList() []int {
	out := []int{}
	if len(r.Shaping) == 0 {
		return out
	}
	_ = unmarshalJSON(r.Shaping, &out)
	return out
}

// IsMultiPort 判断规则是否监听端口段。
func (r *ForwardRule) IsMultiPort() bool {
	return r.ListenPortEnd > 0 && r.ListenPortEnd > r.ListenPort
}

// ValidSyncStatus 校验同步状态取值是否合法。
func ValidSyncStatus(s string) bool {
	switch s {
	case SyncUnsynced, SyncSyncing, SyncNormal, SyncFailed:
		return true
	}
	return false
}

// ValidTargetBalance 校验目标级负载均衡策略是否合法。
func ValidTargetBalance(s string) bool {
	switch s {
	case TargetBalanceFailover, TargetBalanceLeastConn, TargetBalanceRoundRobin,
		TargetBalanceWeighted, TargetBalanceHashIP:
		return true
	}
	return false
}

// ValidBalance 校验出口组负载均衡策略是否合法。
func ValidBalance(s string) bool {
	switch s {
	case BalanceLeastConn, BalanceRoundRobin, BalanceHashIP, BalanceWeighted:
		return true
	}
	return false
}
