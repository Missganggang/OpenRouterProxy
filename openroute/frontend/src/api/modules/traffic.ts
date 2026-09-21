/** 流量统计与监控接口（规格书 8.12、8.13）。 */
import { get, getList, download } from '../client'
import type {
  TrafficOverview,
  TrafficPoint,
  TrafficSeriesPoint,
  TrafficSeriesResult,
  TrafficTopItem,
  TrafficTopResult,
  DashboardData,
  ProbeMetric,
  PageQuery,
} from '../types'

/** 流量筛选条件。 */
export interface TrafficFilter {
  from?: string
  to?: string
  interval?: 'hour' | 'day'
  group_by?: 'user' | 'rule' | 'node' | 'direction' | 'none'
  user_id?: number
  rule_id?: number
  node_id?: number
  direction?: 'in' | 'out'
  raw?: boolean
}

/**
 * 把后端的明细序列折算成「一个时刻一个值」的绘图数据。
 *
 * 后端返回的是「桶 + 分组键」的明细行（direction 维度下同一时刻有 in/out 两行），
 * 而图表与表格只需要每个时刻的合计值。这里统一聚合：
 *   - 按 time 归并；
 *   - bytes 取当前口径的合计（total 已是 in+out）；
 *   - raw_bytes 取 raw。
 *
 * 之所以放在 API 层：仪表盘、流量页、用户详情、规则详情四处都要用，
 * 放在这里只需实现一次，也避免各页面各自把求和写错。
 */
export function toPoints(items: TrafficSeriesPoint[] | undefined): TrafficPoint[] {
  const merged = new Map<string, TrafficPoint>()
  for (const it of items ?? []) {
    const key = it.time
    const prev = merged.get(key)
    if (prev) {
      prev.bytes += Number(it.total ?? 0)
      prev.raw_bytes += Number(it.raw ?? 0)
    } else {
      merged.set(key, {
        time: key,
        bytes: Number(it.total ?? 0),
        raw_bytes: Number(it.raw ?? 0),
      })
    }
  }
  // 按时间升序输出，保证折线不会来回跳。
  return [...merged.values()].sort((a, b) => a.time.localeCompare(b.time))
}

/** 全站流量概览。 */
export function overview() {
  return get<TrafficOverview>('/traffic/overview')
}

/**
 * 时间序列。
 *
 * 返回值已折算成 TrafficPoint[]，页面无需关心后端的分组明细结构。
 */
export async function timeseries(filter?: TrafficFilter): Promise<{ points: TrafficPoint[] }> {
  const res = await get<TrafficSeriesResult>(
    '/traffic/timeseries',
    filter as Record<string, unknown>,
  )
  return { points: toPoints(res?.items) }
}

/** Top-N 排行。 */
export async function top(params: TrafficFilter & {
  dimension: 'user' | 'rule' | 'node'
  limit?: number
  from?: string
  to?: string
}): Promise<TrafficTopItem[]> {
  const res = await get<TrafficTopResult>('/traffic/top', { ...params })
  return res?.items ?? []
}

/** 导出 CSV。 */
export function exportCSV(filter?: TrafficFilter) {
  return download('/traffic/export', filter as Record<string, unknown>, 'traffic.csv')
}

/** 首页仪表盘聚合数据（一次请求拿全）。 */
export function dashboard() {
  return get<DashboardData>('/traffic/dashboard')
}

// ───────────────────────── 探针与监控 ─────────────────────────

/** 探针总览里的单个节点条目（规格书 8.13）。 */
export interface ProbeNodeOverview {
  node_id: number
  name: string
  role: string
  online: boolean
  public_ipv4: string
  group_ids: number[]
  cpu_usage: number
  mem_used: number
  mem_total: number
  disk_used: number
  disk_total: number
  net_in_speed: number
  net_out_speed: number
  load1: number
  tcp_conn: number
  udp_conn: number
  uptime: number
  current_conn: number
  health_score: number
  client_ver: string
  last_seen: string
  updated_at: string
  /** 最近一段时间的迷你曲线。 */
  sparkline: TrafficSeriesPoint[] | ProbeMetric[]
}

/** 探针总览响应。 */
export interface ProbeOverviewResult {
  items: ProbeNodeOverview[]
  total: number
  online: number
  offline: number
  unhealthy: number
  probe_enabled: boolean
  keep_days: number
  sparkline_size: number
  generated_at: string
}

/**
 * 探针总览：所有节点的最新指标与迷你曲线。
 *
 * 注意后端把列表放在 items 里（而不是 nodes），并且同时返回在线 / 离线统计，
 * 这里如实透出，页面直接用即可。
 */
export function probeOverview() {
  return get<ProbeOverviewResult>('/probe/overview')
}

/** 节点指标序列响应。 */
export interface NodeMetricsResult {
  node_id: number
  from: string
  to: string
  interval: string
  points: ProbeMetric[]
}

/** 单个节点的探针数据。 */
export async function probeNode(
  id: number,
  params?: { from?: string; to?: string; interval?: string },
): Promise<ProbeMetric[]> {
  const res = await get<NodeMetricsResult>(
    `/probe/nodes/${id}`,
    params as Record<string, unknown>,
  )
  return res?.points ?? []
}

/** 手动触发探针数据清理。 */
export function probeCleanup() {
  return get<{ deleted: number }>('/probe/cleanup')
}

/** 会话列表（全站在线会话）。 */
export function sessions(params?: PageQuery & { rule_id?: number; user_id?: number }) {
  return getList<unknown>('/forward-rules/0/sessions', params as Record<string, unknown>)
}
