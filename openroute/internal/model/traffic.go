package model

import "time"

// 流量方向常量。
const (
	DirectionIn  = "in"  // 入口方向
	DirectionOut = "out" // 出口方向
)

// HourDaily 表示「按天聚合」的 hour 字段取值。
//
// 规格书约定：hour 为 0~23 表示按小时聚合，-1 表示按天聚合。
// 用 -1 而不是 0 是为了让「按天」不占用凌晨 0 点这个真实的整点分桶。
const HourDaily = -1

// TrafficLog 是流量明细聚合记录表。
//
// 唯一约束 (date, hour, user_id, rule_id, node_id, direction)，
// 使用 Gorm 的 clause.OnConflict 实现 UPSERT，避免并发写重。
// 该唯一索引是 UPSERT 能生效的前提，通过 idx_traffic_unique 标签建立。
type TrafficLog struct {
	ID uint64 `gorm:"primaryKey" json:"id"`
	// 聚合维度
	// 六个维度字段共同构成唯一索引 idx_traffic_unique，供 UPSERT 使用。
	Date      string `gorm:"size:10;index;uniqueIndex:idx_traffic_unique,priority:1" json:"date"`    // 2026-01-01（UTC 日期）
	Hour      int    `gorm:"default:-1;index;uniqueIndex:idx_traffic_unique,priority:2" json:"hour"` // 0~23；-1 表示按天聚合
	UserID    uint64 `gorm:"index;default:0;uniqueIndex:idx_traffic_unique,priority:3" json:"user_id"`
	RuleID    uint64 `gorm:"index;default:0;uniqueIndex:idx_traffic_unique,priority:4" json:"rule_id"`
	NodeID    uint64 `gorm:"index;default:0;uniqueIndex:idx_traffic_unique,priority:5" json:"node_id"` // 产生流量的节点
	Direction string `gorm:"size:8;index;uniqueIndex:idx_traffic_unique,priority:6" json:"direction"`  // in | out
	// 计量
	RawBytes  int64     `gorm:"default:0" json:"raw_bytes"` // 实际字节
	Bytes     int64     `gorm:"default:0" json:"bytes"`     // 乘倍率后字节
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName 指定表名。
func (TrafficLog) TableName() string { return "traffic_logs" }

// Session 是在线会话表，用于 IP 限制 / 连接限制 / 在线用户展示。
//
// 会话数据不参与计费，仅用于限制与展示；
// 超过 10 万行时由 job 包按 last_seen 淘汰最旧记录。
type Session struct {
	ID     uint64 `gorm:"primaryKey" json:"id"`
	UserID uint64 `gorm:"index" json:"user_id"`
	RuleID uint64 `gorm:"index" json:"rule_id"`
	NodeID uint64 `gorm:"index" json:"node_id"`

	ClientIP     string `gorm:"size:64;index" json:"client_ip"`
	ClientIPHash string `gorm:"size:64;index" json:"client_ip_hash"` // 用于匿名化统计
	DeviceID     string `gorm:"size:128;index" json:"device_id"`     // 由 UA/指纹推导，用于设备数限制

	// ConnCount 是当前活跃连接数。
	//
	// 用指针类型承载：Gorm 对带 default 标签的非指针字段，会在值为 0 时回填默认值，
	// 导致「连接数归零」这一状态永远写不进去，而会话清理（规格书 4.2.10）
	// 恰恰要靠 conn_count <= 0 判定垃圾会话。指针可以区分「未设置」（nil → 用默认值）
	// 与「显式设为 0」（写入 0）。
	ConnCount *int   `gorm:"default:1" json:"conn_count"`
	Protocol  string `gorm:"size:16" json:"protocol"` // ws | http | tls | direct | udp

	StartedAt time.Time `json:"started_at"`
	LastSeen  time.Time `gorm:"index" json:"last_seen"`
	Upload    int64     `json:"upload"`
	Download  int64     `json:"download"`
}

// TableName 指定表名。
func (Session) TableName() string { return "sessions" }

// ProbeMetric 是探针指标表，保存节点上报的系统与网络指标。
type ProbeMetric struct {
	ID        uint64 `gorm:"primaryKey" json:"id"`
	NodeID    uint64 `gorm:"index;not null" json:"node_id"`
	Timestamp int64  `gorm:"index;not null" json:"timestamp"` // Unix 秒

	CPU       float64 `json:"cpu"` // 使用率 0~100
	MemUsed   int64   `json:"mem_used"`
	MemTotal  int64   `json:"mem_total"`
	SwapUsed  int64   `json:"swap_used"`
	SwapTotal int64   `json:"swap_total"`
	DiskUsed  int64   `json:"disk_used"`
	DiskTotal int64   `json:"disk_total"`

	NetIn       int64 `json:"net_in"` // 累计入口字节（来自 COUNT_INTERFACE 指定网卡）
	NetOut      int64 `json:"net_out"`
	NetInSpeed  int64 `json:"net_in_speed"` // 瞬时速率 byte/s
	NetOutSpeed int64 `json:"net_out_speed"`

	Load1  float64 `json:"load1"`
	Load5  float64 `json:"load5"`
	Load15 float64 `json:"load15"`

	TcpConn int   `json:"tcp_conn"`
	UdpConn int   `json:"udp_conn"`
	Uptime  int64 `json:"uptime"`

	CreatedAt time.Time `json:"created_at"`
}

// TableName 指定表名。
func (ProbeMetric) TableName() string { return "probe_metrics" }
