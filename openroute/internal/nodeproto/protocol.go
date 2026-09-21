// Package nodeproto defines the shared panel and node client wire protocol.
//
// 通信形态（规格书 5.1 / 8.16）：
//   - 节点 → 面板：HTTP 上报（注册、心跳、同步结果、任务结果）；
//   - 面板 → 节点：尽力而为的即时推送（长连），失败时由节点侧轮询兜底。
//
// This package has no panel, database or HTTP framework dependencies.
package nodeproto

// 本文件是面板与节点之间的「接口契约」：所有结构体的 JSON 标签与
// 规格书 8.16 的报文示例逐字段对应，字段名一律 snake_case。
// 面板与节点客户端共享同一份定义，因此任何改动都属于破坏性变更。

// ---------------------------------------------------------------------------
// 通用
// ---------------------------------------------------------------------------

// Env 是节点身份上下文，由认证步骤产出并注入到各 handler。
type Env struct {
	// NodeID 节点 ID。
	NodeID uint64 `json:"node_id"`
	// NodeName 节点名称。
	NodeName string `json:"node_name"`
	// Role 节点角色：inbound / outbound / both。
	Role string `json:"role"`
}

// ---------------------------------------------------------------------------
// 注册（POST /api/node/register）
// ---------------------------------------------------------------------------

// RegisterRequest 是节点首次注册的请求体（规格书 5.1 第 5 步）。
//
// token 放在 body 里而不是请求头，因为注册阶段节点还没有其它身份信息，
// 且安装脚本已经把 token 写进了 config.yml。
type RegisterRequest struct {
	// Token 节点密钥，必填。
	Token string `json:"token"`
	// Version 节点客户端版本，如 nc20260101。
	Version string `json:"version"`
	// System 节点上报的系统信息。
	System RegisterSystem `json:"system"`
	// ConfigVersion 节点本地已持久化的配置版本，重启后用于判断是否需要全量下发。
	ConfigVersion int64 `json:"config_version"`
	// UUID 多实例部署时的实例标识（规格书 5.3 的 UUID 环境变量）。
	UUID string `json:"uuid"`
}

// RegisterSystem 是注册时上报的系统信息。
type RegisterSystem struct {
	PublicIPv4 string `json:"public_ipv4"`
	PublicIPv6 string `json:"public_ipv6"`
	PrivateIP  string `json:"private_ip"`
	OS         string `json:"os"`
	Arch       string `json:"arch"`
	KernelVer  string `json:"kernel_ver"`
	CPUModel   string `json:"cpu_model"`
	CPUCores   int    `json:"cpu_cores"`
	MemTotal   int64  `json:"mem_total"`
	DiskTotal  int64  `json:"disk_total"`
	// BootTime 系统启动时间（Unix 秒）。
	BootTime int64 `json:"boot_time"`
}

// RegisterResponse 是注册成功的响应体。
//
// 节点拿到 node_id 后才会开始心跳，因此这里的字段是后续所有请求的前置条件。
type RegisterResponse struct {
	NodeID   uint64 `json:"node_id"`
	NodeName string `json:"node_name"`
	Role     string `json:"role"`
	// IsOutbound 由面板的角色判定直接给出，节点据此决定是否开启出口监听。
	IsOutbound bool `json:"is_outbound"`
	// Config 是初始配置，等价于一次 full=true 的配置下发。
	Config ConfigResponse `json:"config"`
	// HeartbeatInterval 心跳间隔（秒），由面板统一规定。
	HeartbeatInterval int `json:"heartbeat_interval"`
	// ServerTime 面板当前时间（Unix 秒），供节点校正时钟偏差。
	ServerTime int64 `json:"server_time"`
}

// ---------------------------------------------------------------------------
// 心跳（POST /api/node/heartbeat）
// ---------------------------------------------------------------------------

// HeartbeatRequest 是节点心跳请求体（规格书 8.16 心跳请求）。
//
// metrics 子对象的字段名与示例逐字对应，不做任何重命名。
type HeartbeatRequest struct {
	NodeID uint64 `json:"node_id"`
	// Version 节点客户端版本。
	Version string `json:"version"`
	// ConfigVersion 节点当前生效的配置版本；面板据此判断是否需要下发新配置。
	ConfigVersion int64 `json:"config_version"`
	// Metrics 系统指标快照。
	Metrics HeartbeatMetrics `json:"metrics"`
	// RunningRules 当前运行中的规则摘要，用于配置漂移检测（规格书 6.14）。
	RunningRules []RunningRule `json:"running_rules"`
	// Timestamp 节点侧时间（Unix 秒）。
	Timestamp int64 `json:"timestamp"`
}

// HeartbeatMetrics 是心跳中的指标子对象。
//
// 字段名严格按规格书 8.16 的示例：cpu、mem_used、mem_total、
// disk_used、disk_total、net_in、net_out、net_in_speed、net_out_speed、
// load1、load5、load15、tcp_conn、udp_conn、uptime。
type HeartbeatMetrics struct {
	// CPU 使用率 0~100。
	CPU float64 `json:"cpu"`
	// MemUsed / MemTotal 内存使用量与总量（字节）。
	MemUsed  int64 `json:"mem_used"`
	MemTotal int64 `json:"mem_total"`
	// SwapUsed / SwapTotal 交换分区。
	SwapUsed  int64 `json:"swap_used"`
	SwapTotal int64 `json:"swap_total"`
	// DiskUsed / DiskTotal 磁盘。
	DiskUsed  int64 `json:"disk_used"`
	DiskTotal int64 `json:"disk_total"`
	// NetIn / NetOut 累计流量（字节，来自 COUNT_INTERFACE 指定网卡）。
	NetIn  int64 `json:"net_in"`
	NetOut int64 `json:"net_out"`
	// NetInSpeed / NetOutSpeed 瞬时速率（byte/s）。
	NetInSpeed  int64 `json:"net_in_speed"`
	NetOutSpeed int64 `json:"net_out_speed"`
	// Load1 / Load5 / Load15 系统负载。
	Load1  float64 `json:"load1"`
	Load5  float64 `json:"load5"`
	Load15 float64 `json:"load15"`
	// TcpConn / UdpConn 连接数。
	TcpConn int `json:"tcp_conn"`
	UdpConn int `json:"udp_conn"`
	// Uptime 运行时长（秒）。
	Uptime int64 `json:"uptime"`
}

// RunningRule 是节点上报的运行中规则摘要。
type RunningRule struct {
	RuleID uint64 `json:"rule_id"`
	Port   int    `json:"port"`
	// Status 运行状态：running / stopped / error。
	Status string `json:"status"`
	// Conn 当前连接数。
	Conn int `json:"conn"`
}

// HeartbeatResponse 是心跳响应体。
type HeartbeatResponse struct {
	// NeedConfig 面板是否认为节点需要拉取新配置。
	NeedConfig bool `json:"need_config"`
	// ConfigVersion 面板当前的最新配置版本。
	ConfigVersion int64 `json:"config_version"`
	// HeartbeatInterval 下一次心跳间隔（秒）。
	HeartbeatInterval int `json:"heartbeat_interval"`
	// ServerTime 面板当前时间（Unix 秒）。
	ServerTime int64 `json:"server_time"`
	// Tasks 附带返回待执行任务，减少一次轮询请求。
	Tasks []TaskItem `json:"tasks,omitempty"`
}

// ---------------------------------------------------------------------------
// 配置下发（GET /api/node/config、POST /api/node/register）
// ---------------------------------------------------------------------------

// ConfigResponse 是面板下发给节点的配置报文（规格书 8.16 配置响应）。
type ConfigResponse struct {
	// ConfigVersion 本次配置的版本号。
	ConfigVersion int64 `json:"config_version"`
	// Full 为 true 表示全量配置，节点应清空旧规则后重建；false 表示增量。
	Full bool `json:"full"`
	// Rules 本次下发的规则列表。
	Rules []ConfigRule `json:"rules"`
	// RemovedRuleIDs 增量下发时，需要节点删除的规则 ID。
	RemovedRuleIDs []uint64 `json:"removed_rule_ids"`
	// DeviceGroupConfig 设备组配置（入口组 + 出口组，按 ID 索引）。
	DeviceGroupConfig map[string]DeviceGroupConfig `json:"device_group_config"`
	// HeartbeatInterval 心跳间隔（秒）。
	HeartbeatInterval int `json:"heartbeat_interval"`
	// GeneratedAt 本次配置生成时间（Unix 秒），供节点判断新鲜度。
	GeneratedAt int64 `json:"generated_at,omitempty"`
}

// ConfigRule 是下发给节点的一条转发规则。
type ConfigRule struct {
	RuleID uint64 `json:"rule_id"`
	Name   string `json:"name"`
	// IsOutbound 标记该规则在当前节点上是入口侧还是出口侧视角。
	IsOutbound bool `json:"is_outbound"`
	// InboundGroupID / OutboundGroupID 参与两端协作的节点需要知道对端组。
	InboundGroupID  uint64 `json:"inbound_group_id"`
	OutboundGroupID uint64 `json:"outbound_group_id"`
	// ListenPort / ListenPortEnd 入口监听端口（段）。
	ListenPort    int `json:"listen_port"`
	ListenPortEnd int `json:"listen_port_end"`
	// Protocol 隧道协议：direct / ws / http / tls。
	Protocol string `json:"protocol"`
	// Targets 目标地址列表（多目标负载均衡）。
	Targets []ConfigTarget `json:"targets"`
	// Options 规则级高级选项（协议细节、TLS 证书、SNI 分流等）。
	Options map[string]interface{} `json:"options"`
	// TargetBalance 目标级负载均衡策略。
	TargetBalance string `json:"target_balance"`
	// 限制项：0 表示不限。
	SpeedLimit int64 `json:"speed_limit"`
	ConnLimit  int   `json:"conn_limit"`
	IPLimit    int   `json:"ip_limit"`
	// 倍率，仅用于节点侧流量标记。
	InboundMultiplier  float64 `json:"inbound_multiplier"`
	OutboundMultiplier float64 `json:"outbound_multiplier"`
	// 反向隧道。
	ReverseEnable  bool   `json:"reverse_enable"`
	ReversePort    int    `json:"reverse_port"`
	ReverseGroupID uint64 `json:"reverse_group_id"`
	// 链式出口（2~3 跳）设备组 ID 列表。
	ChainGroups []uint64 `json:"chain_groups"`
	// SNI 分流。
	IsSubRule bool   `json:"is_sub_rule"`
	ParentID  uint64 `json:"parent_id"`
	SNI       string `json:"sni"`
	// Shaping 整流选项。
	Shaping []int `json:"shaping"`
	// Enable 是否启用；关闭的规则以 enable=false 下发，节点应停止监听。
	Enable bool `json:"enable"`
}

// ConfigTarget 是一个转发目标。
type ConfigTarget struct {
	Host   string `json:"host"`
	Port   int    `json:"port"`
	Weight int    `json:"weight,omitempty"`
	Status string `json:"status,omitempty"`
}

// DeviceGroupConfig 是下发到节点的设备组配置。
//
// 保留原组的原始 Config JSON（结构见规格书 4.3 / 4.4），
// 另外把「解析后的关键字段」平铺出来，方便节点侧直接使用。
type DeviceGroupConfig struct {
	GroupID uint64 `json:"group_id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	// NodeIDs 组内节点顺序，出口组的顺序参与负载均衡初始化。
	NodeIDs []uint64 `json:"node_ids"`
	// Balance 出口组负载均衡策略。
	Balance string `json:"balance"`
	// HealthCheck 出口组健康检查参数。
	HealthCheckEnable    bool `json:"health_check_enable"`
	HealthCheckInterval  int  `json:"health_check_interval"`
	HealthCheckTimeout   int  `json:"health_check_timeout"`
	HealthCheckFailCount int  `json:"health_check_fail_count"`
	HealthCheckSuccCount int  `json:"health_check_succ_count"`
	// FailoverGroupID 故障转移组。
	FailoverGroupID uint64 `json:"failover_group_id"`
	// Config 组原始配置（入口组见 4.3，出口组见 4.4）。
	Config map[string]interface{} `json:"config"`
	// Peers 组内其它节点的连接地址，供出口侧建连使用。
	Peers []GroupPeer `json:"peers"`
}

// GroupPeer 是设备组中一个对端节点的连接信息。
//
// 连接地址的优先级（规格书 4.3）：静态地址 > connect_host > 动态 IPv4
// > IPv6 组策略 > 动态 IPv6 > 回退公网 IPv4。面板在生成时已经解析完毕，
// 节点直接使用 Host / Port 即可。
type GroupPeer struct {
	NodeID uint64 `json:"node_id"`
	Name   string `json:"name"`
	Host   string `json:"host"`
	// 各协议端口。
	DirectPort int  `json:"direct_port"`
	WsPort     int  `json:"ws_port"`
	TlsPort    int  `json:"tls_port"`
	UdpPort    int  `json:"udp_port"`
	RevPort    int  `json:"rev_port"`
	Weight     int  `json:"weight"`
	Online     bool `json:"online"`
}

// ---------------------------------------------------------------------------
// 上报（POST /api/node/report）
// ---------------------------------------------------------------------------

// ReportRequest 是节点上报同步结果、连接数与错误日志的请求体（规格书 5.1）。
type ReportRequest struct {
	NodeID uint64 `json:"node_id"`
	// ConfigVersion 本次应用（成功或失败）的配置版本。
	ConfigVersion int64 `json:"config_version"`
	// Results 各规则的同步结果。
	Results []RuleSyncResult `json:"results"`
	// Stats 被本次上报覆盖的节点级统计。
	Stats *ReportStats `json:"stats,omitempty"`
	// Error 节点侧的错误日志摘要（同步失败原因会写回规则的 sync_error）。
	Error string `json:"error,omitempty"`
	// Timestamp 节点侧时间（Unix 秒）。
	Timestamp int64 `json:"timestamp"`
}

// RuleSyncResult 是单条规则的同步结果。
type RuleSyncResult struct {
	RuleID uint64 `json:"rule_id"`
	// Status 取值：normal（应用成功）/ failed（应用失败）。
	Status string `json:"status"`
	// Error 失败原因，成功时为空。
	Error string `json:"error,omitempty"`
}

// ReportStats 是节点级运行时统计。
type ReportStats struct {
	CurrentConn int   `json:"current_conn"`
	NetIn       int64 `json:"net_in"`
	NetOut      int64 `json:"net_out"`
	// RuleTraffic 按规则维度的累计流量增量（字节）。
	RuleTraffic []RuleTrafficItem `json:"rule_traffic,omitempty"`
}

// RuleTrafficItem 是单条规则的流量增量。
type RuleTrafficItem struct {
	RuleID     uint64 `json:"rule_id"`
	TrafficIn  int64  `json:"traffic_in"`
	TrafficOut int64  `json:"traffic_out"`
}

// ReportResponse 是上报响应体。
type ReportResponse struct {
	// Accepted 面板接受的结果条数。
	Accepted int `json:"accepted"`
	// ServerTime 面板当前时间（Unix 秒）。
	ServerTime int64 `json:"server_time"`
}

// ---------------------------------------------------------------------------
// 任务（GET /api/node/tasks、POST /api/node/task-result）
// ---------------------------------------------------------------------------

// TaskItem 是下发给节点的单个任务。
type TaskItem struct {
	TaskID uint64 `json:"task_id"`
	// Type 取值：upgrade / restart / exec。
	Type string `json:"type"`
	// Payload 任务参数：exec 类任务含 command 与 timeout。
	Payload map[string]interface{} `json:"payload"`
	// CreatedAt 任务创建时间（Unix 秒）。
	CreatedAt int64 `json:"created_at"`
}

// TasksResponse 是任务拉取响应体。
type TasksResponse struct {
	Tasks []TaskItem `json:"tasks"`
	// ServerTime 面板当前时间（Unix 秒）。
	ServerTime int64 `json:"server_time"`
}

// TaskResultRequest 是节点上报任务结果（POST /api/node/task-result）。
type TaskResultRequest struct {
	TaskID uint64 `json:"task_id"`
	// Status 取值：done / failed。
	Status string `json:"status"`
	// Result 命令原始输出，面板侧会截断到 8192 字节。
	Result string `json:"result"`
	// Timestamp 节点侧时间（Unix 秒）。
	Timestamp int64 `json:"timestamp"`
}

// TaskResultResponse 是任务结果上报的响应体。
type TaskResultResponse struct {
	OK         bool  `json:"ok"`
	ServerTime int64 `json:"server_time"`
}

// ---------------------------------------------------------------------------
// 认证与错误
// ---------------------------------------------------------------------------

// 节点 token 的传输位置。注册接口用 body，其余接口用请求头（规格书 8.16）。
const (
	// NodeTokenHeader 是节点通信使用的认证请求头名。
	NodeTokenHeader = "X-Node-Token"
	// NodeIDHeader 允许节点用请求头补充 node_id，便于排障时定位。
	NodeIDHeader = "X-Node-Id"
)

// ErrorResponse 是节点通信接口的失败响应体。
//
// 与面板管理接口的 response.Body 形状一致（规格书 8.3），
// 但节点侧只关心 code / message，因此这里收敛成最小结构。
type ErrorResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ---------------------------------------------------------------------------
// 常量
// ---------------------------------------------------------------------------

// 节点上报的规则运行状态。
const (
	// RuleStatusRunning 规则正在监听。
	RuleStatusRunning = "running"
	// RuleStatusStopped 规则已停止。
	RuleStatusStopped = "stopped"
	// RuleStatusError 规则启动失败。
	RuleStatusError = "error"
)

// 任务类型（与 model 包保持一致，这里单独定义避免节点侧反向依赖）。
const (
	TaskTypeUpgrade = "upgrade"
	TaskTypeRestart = "restart"
	TaskTypeExec    = "exec"
)

// HeartbeatIntervalDefault 是兜底的心跳间隔（秒）。
const HeartbeatIntervalDefault = 10
