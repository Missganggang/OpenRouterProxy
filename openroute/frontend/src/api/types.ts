/**
 * 与后端对齐的 TypeScript 类型定义。
 *
 * 命名与后端 JSON 字段保持一致（小驼峰），
 * 便于对照 `docs/openapi.yaml` 与错误码字典排查问题。
 */

/** 统一响应包体的分页信息（规格书 8.3）。 */
export interface Pagination {
  page: number
  page_size: number
  total: number
  total_pages: number
}

/** 列表接口的 data 结构。 */
export interface ListData<T> {
  items: T[]
  pagination: Pagination
}

/** 失败响应中的定位信息。 */
export interface ErrorDetails {
  field?: string
  value?: unknown
  hint?: string
}

/** 用户角色。 */
export type Role = 'admin' | 'user'

/** 用户状态。 */
export type UserStatus = 0 | 1

/** 用户（规格书 4.2.1）。 */
export interface User {
  id: number
  username: string
  nickname: string
  role: Role
  status: UserStatus
  group_id: number
  token: string
  traffic_used: number
  traffic_limit: number
  speed_limit: number
  ip_limit: number
  device_limit: number
  conn_limit: number
  password_reset_required: boolean
  pending_reinstall: boolean
  expire_at: string | null
  last_login_at: string | null
  last_login_ip: string
  remark: string
  created_at: string
  updated_at: string
}

/** 用户分组。 */
export interface UserGroup {
  id: number
  name: string
  traffic_limit: number
  speed_limit: number
  ip_limit: number
  conn_limit: number
  rule_group_ids: number[]
  remark: string
  created_at: string
  updated_at: string
}

/** 节点角色。 */
export type NodeRole = 'inbound' | 'outbound' | 'both'

/** 节点（规格书 4.2.3）。 */
export interface Node {
  id: number
  name: string
  token: string
  role: NodeRole
  public_ipv4: string
  public_ipv6: string
  private_ip: string
  connect_host: string
  is_static: boolean
  direct_port: number
  ws_port: number
  tls_port: number
  udp_port: number
  rev_port: number
  group_ids: number[]
  online: boolean
  last_seen: string | null
  weight: number
  max_conn: number
  os: string
  arch: string
  kernel_ver: string
  client_ver: string
  cpu_model: string
  cpu_cores: number
  mem_total: number
  disk_total: number
  boot_time: string | null
  current_conn: number
  net_in_speed: number
  net_out_speed: number
  cpu_usage: number
  mem_used: number
  disk_used: number
  load1: number
  uptime: number
  config_version: number
  last_error: string
  health_score: number
  drift_detected: boolean
  drift_detail: unknown
  remark: string
  created_at: string
  updated_at: string
}

/** 节点分组。 */
export interface NodeGroup {
  id: number
  name: string
  remark: string
  created_at: string
  updated_at: string
}

/** 设备组类型。 */
export type DeviceGroupType = 'inbound' | 'outbound'

/** 出口组负载均衡策略（规格书 6.3）。 */
export type Balance =
  | 'least_conn'
  | 'round_robin'
  | 'hash_ip'
  | 'weighted'

/** 隧道协议（规格书 4.5）。 */
export type TunnelProtocol = 'ws' | 'http' | 'tls' | 'direct'

/** ws / http 协议细节（http 复用同一份配置，仅少了 Upgrade 头）。 */
export interface WsConfig {
  host?: string
  path?: string
  /** 原始 HTTP 报文模板，用于绕过特征检测。 */
  request?: string
  response?: string
}

/** TLS 指纹可选值。 */
export type Chfp =
  | 'chrome'
  | 'firefox'
  | 'safari'
  | 'ios'
  | 'android'
  | 'edge'
  | '360'
  | 'qq'

/** 隧道协议细节。 */
export interface TlsConfig {
  sni?: string
  alpn?: string[]
  chfp?: Chfp
}

/** 入口组 config（规格书 4.3）。 */
export interface InboundConfig {
  /** 允许的目标域名白名单，后缀匹配；空数组 = 不限制。 */
  allowed_host: string[]
  /** 禁止的目标域名黑名单，后缀匹配；优先级高于白名单。 */
  blocked_host: string[]
  /** 禁止的 HTTP 路径前缀。 */
  blocked_path: string[]
  /** 禁止的入站协议类型：socks / fet / http / tls。 */
  blocked_protocol: string[]
  /** TLS 入站策略：0 不处理 / 1 强制剥离 / 2 SNI 分流。 */
  tls_inbound_policy: 0 | 1 | 2
  /** 拒绝无 SNI 的 TLS 握手。 */
  tls_reject_empty_sni: boolean
  /** 完全关闭 UDP 转发。 */
  disable_udp: boolean
  /** UDP 走 TCP 隧道（UoT）。 */
  udp_over_tcp: boolean
  /** IPv6 优先策略：[] / [0] / [0,1,2]。 */
  ipv6_group: number[]
  /** 出口连续失败次数阈值。 */
  max_fail: number
  /** 故障判定超时（秒）。 */
  fail_timout_sec: number
  /** 反向隧道设备组 ID 列表。 */
  reverse_group: number[]
  protocol: TunnelProtocol
  ws?: WsConfig
  tls?: TlsConfig
}

/** 出口组 config（规格书 4.4）。 */
export interface OutboundConfig {
  connect_type: 'dyn_ip4' | 'dyn_ip6' | 'static'
  connect_address?: string
  connect_port?: number
  protocol: TunnelProtocol
  ws?: WsConfig
  tls?: TlsConfig
  udp_over_tcp: boolean
}

/** 设备组（规格书 4.2.5）。 */
export interface DeviceGroup {
  id: number
  name: string
  type: DeviceGroupType
  node_ids: number[]
  config: InboundConfig | OutboundConfig
  balance: Balance
  health_check_enable: boolean
  health_check_interval: number
  health_check_timeout: number
  health_check_fail_count: number
  health_check_succ_count: number
  failover_group_id: number
  remark: string
  created_at: string
  updated_at: string
}

/** 转发目标。 */
export interface Target {
  host: string
  port: number
  weight?: number
  status?: 'up' | 'down'
}

/** 规则同步状态（规格书 5.4）。 */
export type SyncStatus = 'unsynced' | 'syncing' | 'normal' | 'failed'

/** 目标级负载均衡策略。 */
export type TargetBalance =
  | 'failover'
  | 'least_conn'
  | 'round_robin'
  | 'weighted'
  | 'hash_ip'

/** 转发规则（规格书 4.2.6）。 */
export interface ForwardRule {
  id: number
  name: string
  user_id: number
  rule_group_id: number
  inbound_group_id: number
  listen_port: number
  listen_port_end: number
  outbound_group_id: number
  targets: Target[]
  target_balance: TargetBalance
  inbound_multiplier: number
  outbound_multiplier: number
  speed_limit: number
  conn_limit: number
  ip_limit: number
  options: Record<string, unknown>
  chain_groups: number[]
  reverse_enable: boolean
  reverse_port: number
  reverse_group_id: number
  is_sub_rule: boolean
  parent_id: number
  sni: string
  shaping: number[]
  sync_status: SyncStatus
  sync_error: string
  synced_at: string | null
  traffic_in: number
  traffic_out: number
  enable: boolean
  remark: string
  created_at: string
  updated_at: string
}

/** 规则分组。 */
export interface RuleGroup {
  id: number
  name: string
  sort: number
  remark: string
  created_at: string
  updated_at: string
}

/** 流量方向。 */
export type Direction = 'in' | 'out'

/** 计量口径：scaled = 乘倍率后（默认展示），raw = 实际字节。 */
export type BytesMode = 'scaled' | 'raw'

/**
 * 一段区间的流量。
 *
 * 注意：后端把每个统计周期都建模成带区间边界的对象，而不是裸数值——
 * 这样前端能同时拿到 in / out / total / raw 四个口径，
 * 也能显示「本周期覆盖哪几天」。全站 / 用户 / 规则三处的周期结构完全一致。
 */
export interface TrafficPeriod {
  /** 区间起始日期（按天统计时为 YYYY-MM-DD，累计时为 0 值可忽略）。 */
  from?: string
  /** 区间结束日期。 */
  to?: string
  /** 入口方向字节数。 */
  in: number
  /** 出口方向字节数。 */
  out: number
  /** 合计（in + out），即当前展示口径下的值。 */
  total: number
  /** 同期的实际字节数，用于「显示原始值」切换。 */
  raw: number
}

/** 全站流量概览（规格书 8.12 GET /traffic/overview）。 */
export interface TrafficOverview {
  today: TrafficPeriod
  yesterday: TrafficPeriod
  month: TrafficPeriod
  total: TrafficPeriod
  online_users: number
  online_nodes: number
  total_nodes: number
  total_rules: number
  active_rules: number
  active_conns: number
  /** 本次响应的计量口径。 */
  bytes_mode: BytesMode
  /** 按天聚合使用的时区（固定 UTC）。 */
  timezone: string
}

/**
 * 流量时间序列的一个点（规格书 8.12 GET /traffic/timeseries）。
 *
 * group_by 为 direction 时 group 取 in / out，其余维度取 ID 的字符串形式，
 * group_name 是对应的可读名称。
 */
export interface TrafficSeriesPoint {
  /** 时间桶起始时间，RFC3339 UTC。 */
  time: string
  /** 时间桶起始的 Unix 秒，便于横轴排序。 */
  unix: number
  group: string
  group_name: string
  in: number
  out: number
  total: number
  raw: number
}

/** 时间序列响应。 */
export interface TrafficSeriesResult {
  items: TrafficSeriesPoint[]
  bytes_mode: BytesMode
  from: string
  to: string
  interval: 'hour' | 'day'
  group_by: string
}

/**
 * 前端绘图 / 表格用的「单一时间桶」视图模型。
 *
 * 后端的 TrafficSeriesPoint 是「一个桶 + 一个分组键」的明细行，
 * 同一个人时刻可能有多行（direction 维度就是 in/out 两行）。
 * 图表与表格需要的是「一个时刻一个值」，因此由 API 层统一折算成这个形状，
 * 页面就不必各自处理分组聚合。
 */
export interface TrafficPoint {
  /** 时间桶标识，形如 2026-01-01 12:00 或 2026-01-01。 */
  time: string
  /** 当前口径下的字节数（默认 = 乘倍率后）。 */
  bytes: number
  /** 实际字节数。 */
  raw_bytes: number
}

/** Top-N 排行条目。 */
export interface TrafficTopItem {
  id: number
  name: string
  in: number
  out: number
  total: number
  raw: number
  /** 占全站同口径总量的百分比（0~100）。 */
  percent: number
}

/** Top-N 排行响应。 */
export interface TrafficTopResult {
  items: TrafficTopItem[]
  bytes_mode: BytesMode
  dimension: string
  limit: number
}

/**
 * 用户级流量统计（规格书 6.10 的用户维度）。
 *
 * 与规则 / 全站不同，这里返回的是**各周期的合计**而不是时间序列：
 * 用户页需要的是「已用 / 上限 / 剩余」这类概览数字，以及按规则的分布饼图。
 */
export interface UserTraffic {
  user_id: number
  username: string
  nickname: string
  status: number
  /** 已用流量（按倍率折算后的累计值）。 */
  used: number
  /** 流量上限，0 = 不限。 */
  limit: number
  /** 剩余流量；未设上限时为 -1。 */
  remaining: number
  /** 使用百分比；未设上限时为 0。 */
  percent: number
  today: TrafficPeriod
  yesterday: TrafficPeriod
  month: TrafficPeriod
  total: TrafficPeriod
  /** 按规则的流量分布（饼图用）。 */
  rule_distribution: TrafficTopItem[]
  /** 该用户的规则数（含禁用）。 */
  rules: number
}

/** 仪表盘上的规则摘要（比完整规则精简，只用于列表展示）。 */
export interface DashboardRuleBrief {
  id: number
  name: string
  listen_port: number
  sync_status: SyncStatus
  sync_error: string
}

/** 仪表盘上的节点摘要。 */
export interface DashboardNodeBrief {
  id: number
  name: string
  role: NodeRole
  last_seen: string | null
  offline_seconds: number
  last_error: string
}

/**
 * 首页仪表盘聚合数据（规格书 8.12 GET /traffic/dashboard）。
 *
 * 与最初的设想不同，后端把待处理事项平铺在顶层（sync_failed_rules /
 * offline_nodes / active_alerts），而不是嵌在 pending 对象里；
 * 曲线字段名是 trend 而不是 timeseries。这里按后端实际结构定义。
 */
export interface DashboardData {
  overview: TrafficOverview
  /** 今日排行，供首页卡片直接渲染。 */
  top_rules: TrafficTopItem[]
  top_users: TrafficTopItem[]
  top_nodes: TrafficTopItem[]
  /** 最近 24 小时按小时的流量曲线。 */
  trend: TrafficSeriesPoint[]
  /** 待处理事项（三者互相独立，合计即 pending_issues）。 */
  sync_failed_rules: DashboardRuleBrief[]
  offline_nodes: DashboardNodeBrief[]
  active_alerts: AlertHistory[]
  /** 待处理事项总数，后端已算好，前端不必再求和。 */
  pending_issues: number
}

/** 在线会话（规格书 4.2.10）。 */
export interface Session {
  id: number
  user_id: number
  rule_id: number
  node_id: number
  client_ip: string
  client_ip_hash: string
  device_id: string
  conn_count: number
  protocol: string
  started_at: string
  last_seen: string
  upload: number
  download: number
}

/** 探针指标（规格书 4.2.9）。 */
export interface ProbeMetric {
  id: number
  node_id: number
  timestamp: number
  cpu: number
  mem_used: number
  mem_total: number
  swap_used: number
  swap_total: number
  disk_used: number
  disk_total: number
  net_in: number
  net_out: number
  net_in_speed: number
  net_out_speed: number
  load1: number
  load5: number
  load15: number
  tcp_conn: number
  udp_conn: number
  uptime: number
}

/** 告警类型。 */
export type AlertType =
  | 'node_offline'
  | 'node_cpu'
  | 'node_mem'
  | 'node_disk'
  | 'rule_sync_failed'
  | 'user_traffic_pct'
  | 'rule_traffic_pct'
  | 'node_traffic_pct'
  | 'cert_expire'

/** 告警通知渠道。 */
export interface AlertChannel {
  type: 'webhook' | 'telegram' | 'email'
  url?: string
  token?: string
  chat?: string
  host?: string
  port?: number
  user?: string
  pass?: string
  to?: string
}

/** 告警规则（规格书 4.2.15）。 */
export interface AlertRule {
  id: number
  name: string
  type: AlertType
  target_id: number
  threshold: number
  duration: number
  channels: AlertChannel[]
  silence_for: number
  enabled: boolean
  last_fired_at: string | null
  created_at: string
  updated_at: string
}

/** 告警历史（规格书 4.2.16）。 */
export interface AlertHistory {
  id: number
  rule_id: number
  level: 'info' | 'warning' | 'critical'
  title: string
  content: string
  resolved: boolean
  resource: string
  resource_id: number
  fired_at: string
  resolved_at: string | null
}

/** 配置快照（规格书 4.2.14）。 */
export interface ConfigSnapshot {
  id: number
  name: string
  reason: string
  checksum: string
  rule_count: number
  group_count: number
  node_count: number
  created_at: string
}

/** 审计日志（规格书 4.2.12）。 */
export interface AuditLog {
  id: number
  user_id: number
  username: string
  action: string
  resource: string
  resource_id: number
  before: unknown
  after: unknown
  ip: string
  user_agent: string
  result: 'success' | 'failed'
  message: string
  created_at: string
}

/** API Token（规格书 4.2.13）。 */
export interface APIToken {
  id: number
  name: string
  token?: string
  scopes: string[]
  ip_whitelist: string[]
  expire_at: string | null
  last_used_at: string | null
  enabled: boolean
  created_at: string
  updated_at: string
}

/** 迁移批次（规格书 7.5）。 */
export interface MigrateBatch {
  id: number
  source: string
  source_dsn: string
  target_dsn: string
  status: 'pending' | 'running' | 'finished' | 'failed' | 'rolled_back'
  stage: number
  dry_run: boolean
  total_rows: number
  migrated_rows: number
  report: unknown
  backup_path: string
  error: string
  started_at: string
  finished_at: string | null
}

/** 备份记录。 */
export interface Backup {
  id: number
  name: string
  path: string
  size: number
  checksum: string
  manifest: unknown
  with_secret: boolean
  created_at: string
}

/** 后台任务状态。 */
export interface TaskStatus {
  name: string
  desc: string
  interval_sec: number
  running: boolean
  last_run_at: string | null
  last_result: string
  last_error: string
  next_run_at: string | null
}

/** 系统信息。 */
export interface SystemInfo {
  version: string
  build: string
  uptime: number
  database: string
  node_count: number
  online_node_count: number
  rule_count: number
  user_count: number
  schema_version: number
  subscriber_count: number
  config_version: number
}

/** 系统健康状态。 */
export interface SystemStatus {
  healthy: boolean
  checks: Array<{
    name: string
    ok: boolean
    message: string
  }>
}

/** 登录结果。 */
export interface LoginResult {
  access_token: string
  refresh_token: string
  expires_in: number
  user: User
  captcha_required?: boolean
}

/** 规则导入预览结果（规格书 8.9）。 */
export interface ImportPreview {
  total: number
  will_create: number
  conflicts: Array<{
    line: number
    name: string
    reason: string
    suggestion: string
  }>
  invalid: Array<{
    line: number
    raw: string
    reason: string
  }>
}

/** 导入请求体。 */
export interface ImportRequest {
  /** 导入格式：text 为旧版文本格式，json 为新版 JSON 格式。 */
  format: 'text' | 'json'
  /** 原始内容。 */
  content: string
  /** 默认归属的入口设备组（文本格式未指定时使用）。 */
  inbound_group_id?: number
  /** 默认归属的出口设备组。 */
  outbound_group_id?: number
  /** 默认归属用户。 */
  user_id?: number
}

/** 批量操作请求（规格书 8.9）。 */
export interface BatchActionRequest {
  action:
    | 'enable'
    | 'disable'
    | 'delete'
    | 'set_rule_group'
    | 'set_multiplier'
    | 'scale_multiplier'
    | 'set_outbound_group'
    | 'set_limits'
    | 'set_chain'
  ids: number[]
  params?: Record<string, unknown>
}

/** 迁移预检请求。 */
export interface MigrationPrecheckRequest {
  source: 'nyanpass' | 'openroute'
  dsn: string
  rename_policy?: 'fail' | 'suffix' | 'skip'
}

/** 分页与筛选的通用查询参数。 */
export interface PageQuery {
  page?: number
  page_size?: number
  sort?: string
  order?: 'asc' | 'desc'
  keyword?: string
}
