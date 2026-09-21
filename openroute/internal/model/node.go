package model

import "time"

// 节点角色常量。
const (
	RoleInbound  = "inbound"  // 仅入口
	RoleOutbound = "outbound" // 仅出口
	RoleBoth     = "both"     // 双端
)

// Node 是节点服务器表。
//
// 节点是部署在转发服务器上的 Go 程序，接受面板下发配置并执行转发。
// 网络信息字段由节点上报覆盖，面板不手工维护。
type Node struct {
	ID    uint64 `gorm:"primaryKey" json:"id"`
	Name  string `gorm:"size:64;uniqueIndex;not null" json:"name"`
	Token string `gorm:"size:64;uniqueIndex;not null" json:"token"` // 节点密钥，安装时下发
	// 角色：inbound(入口) / outbound(出口) / both(双端)
	Role string `gorm:"size:16;default:both" json:"role"`

	// 网络信息（节点上报覆盖）
	PublicIPv4 string `gorm:"size:64;index" json:"public_ipv4"`
	PublicIPv6 string `gorm:"size:64" json:"public_ipv6"`
	PrivateIP  string `gorm:"size:64" json:"private_ip"`
	// 静态连接地址（出口用；为空则用上报的公网 IP）
	ConnectHost string `gorm:"size:255" json:"connect_host"`
	IsStatic    bool   `gorm:"default:false" json:"is_static"` // 对应配置 connect_type == "static"

	// 端口配置
	DirectPort int `gorm:"default:0" json:"direct_port"`
	WsPort     int `gorm:"default:0" json:"ws_port"`
	TlsPort    int `gorm:"default:0" json:"tls_port"`
	UdpPort    int `gorm:"default:0" json:"udp_port"`
	RevPort    int `gorm:"default:0" json:"rev_port"` // 反向隧道端口（入口侧监听）

	// 分组：所属节点分组（多选）
	GroupIDs JSON `gorm:"type:text" json:"group_ids"`

	// 状态
	Online   bool       `gorm:"default:false;index" json:"online"`
	LastSeen *time.Time `json:"last_seen"`
	Weight   int        `gorm:"default:1" json:"weight"`   // 负载均衡权重
	MaxConn  int        `gorm:"default:0" json:"max_conn"` // 本机最大连接数，0 = 不限

	// 系统信息（探针上报）
	OS        string     `gorm:"size:64" json:"os"`
	Arch      string     `gorm:"size:32" json:"arch"`
	KernelVer string     `gorm:"size:64" json:"kernel_ver"`
	ClientVer string     `gorm:"size:32" json:"client_ver"` // 节点客户端版本
	CPUModel  string     `gorm:"size:128" json:"cpu_model"`
	CPUCores  int        `json:"cpu_cores"`
	MemTotal  int64      `json:"mem_total"`
	DiskTotal int64      `json:"disk_total"`
	BootTime  *time.Time `json:"boot_time"`

	// 运行时状态（由心跳写入，不持久化到节点的配置下发中）
	CurrentConn int     `gorm:"default:0" json:"current_conn"`  // 当前连接数
	NetInSpeed  int64   `gorm:"default:0" json:"net_in_speed"`  // 实时入向速率 byte/s
	NetOutSpeed int64   `gorm:"default:0" json:"net_out_speed"` // 实时出向速率
	CPUUsage    float64 `gorm:"default:0" json:"cpu_usage"`     // 最近一次 CPU 使用率
	MemUsed     int64   `gorm:"default:0" json:"mem_used"`
	DiskUsed    int64   `gorm:"default:0" json:"disk_used"`
	Load1       float64 `gorm:"default:0" json:"load1"`
	Uptime      int64   `gorm:"default:0" json:"uptime"`
	// ConfigVersion 是节点当前生效的配置版本号（规格书 5.4）。
	//
	// 列名显式指定为 config_version：节点侧的通信协议（规格书 8.16）
	// 与心跳载荷都用这个名字，保持一致可以让 agent 包直接用
	// 裸 SQL 的 map 更新而不必做字段名翻译。
	ConfigVersion int64  `gorm:"column:config_version;default:0" json:"config_version"`
	LastError     string `gorm:"size:512" json:"last_error"` // 最近一次同步错误

	// HealthScore 是健康度评分 0~100（规格书 6.1 新增能力），
	// 由 job 包定时计算：综合在线率、同步失败率、CPU/内存水位。
	HealthScore int `gorm:"default:0" json:"health_score"`

	// DriftDetected 标记面板期望配置与节点上报的实际配置存在差异（规格书 6.14）。
	DriftDetected bool   `gorm:"default:false" json:"drift_detected"`
	DriftDetail   JSON   `gorm:"type:text" json:"drift_detail"`
	Remark        string `gorm:"size:255" json:"remark"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName 指定表名。
func (Node) TableName() string { return "nodes" }

// IsInbound 判断节点是否可承担入口角色。
func (n *Node) IsInbound() bool { return n.Role == RoleInbound || n.Role == RoleBoth }

// IsOutbound 判断节点是否可承担出口角色。
func (n *Node) IsOutbound() bool { return n.Role == RoleOutbound || n.Role == RoleBoth }

// NodeGroup 是节点分组表。
//
// 分组仅用于批量管理与筛选，不改变转发行为。
type NodeGroup struct {
	ID        uint64    `gorm:"primaryKey" json:"id"`
	Name      string    `gorm:"size:64;uniqueIndex;not null" json:"name"`
	Remark    string    `gorm:"size:255" json:"remark"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName 指定表名。
func (NodeGroup) TableName() string { return "node_groups" }
