/** 转发规则与规则分组接口（规格书 8.9、8.10）。 */
import { get, getList, post, put, del, download } from '../client'
import { toPoints } from './traffic'
import type {
  ForwardRule,
  RuleGroup,
  PageQuery,
  ImportPreview,
  ImportRequest,
  BatchActionRequest,
  Session,
  TrafficPoint,
  TrafficSeriesResult,
  Target,
  TargetBalance,
} from '../types'

/** 规则列表查询参数。 */
export interface RuleQuery extends PageQuery {
  user_id?: number
  rule_group_id?: number
  inbound_group_id?: number
  outbound_group_id?: number
  sync_status?: string
  enable?: boolean
}

/** 创建/更新规则的请求体。 */
export interface RuleInput {
  name: string
  user_id?: number
  rule_group_id?: number
  inbound_group_id: number
  listen_port: number
  listen_port_end?: number
  outbound_group_id?: number
  targets: Target[]
  target_balance?: TargetBalance
  inbound_multiplier?: number
  outbound_multiplier?: number
  speed_limit?: number
  conn_limit?: number
  ip_limit?: number
  options?: Record<string, unknown>
  chain_groups?: number[]
  reverse_enable?: boolean
  reverse_port?: number
  reverse_group_id?: number
  sni?: string
  shaping?: number[]
  enable?: boolean
  remark?: string
}

/** 规则列表。 */
export function list(params?: RuleQuery) {
  return getList<ForwardRule>('/forward-rules', params as Record<string, unknown>)
}

/** 规则详情。 */
export function getOne(id: number) {
  return get<ForwardRule>(`/forward-rules/${id}`)
}

/** 创建规则。 */
export function create(input: RuleInput) {
  return post<ForwardRule>('/forward-rules', input)
}

/** 更新规则。 */
export function update(id: number, input: Partial<RuleInput>) {
  return put<ForwardRule>(`/forward-rules/${id}`, input)
}

/** 删除规则。 */
export function remove(id: number) {
  return del<null>(`/forward-rules/${id}`)
}

/** 启用规则。 */
export function enable(id: number) {
  return post<null>(`/forward-rules/${id}/enable`)
}

/** 禁用规则。 */
export function disable(id: number) {
  return post<null>(`/forward-rules/${id}/disable`)
}

/** 强制重新下发。 */
export function resync(id: number) {
  return post<null>(`/forward-rules/${id}/resync`)
}

/**
 * 规则的流量时间序列。
 *
 * 后端返回的是「桶 + 分组键」的明细序列，这里折算成「一个时刻一个值」，
 * 与全站流量页保持同一形状（折算逻辑见 traffic.ts 的 toPoints）。
 */
export async function traffic(
  id: number,
  params?: { from?: string; to?: string; interval?: 'hour' | 'day' },
): Promise<{ points: TrafficPoint[] }> {
  const res = await get<{ series: TrafficSeriesResult['items'] }>(
    `/forward-rules/${id}/traffic`,
    params as Record<string, unknown>,
  )
  return { points: toPoints(res?.series) }
}

/** 规则当前会话列表。 */
export function sessions(id: number) {
  return get<Session[]>(`/forward-rules/${id}/sessions`)
}

/** 批量操作。 */
export function batch(input: BatchActionRequest) {
  return post<{ succeeded: number[]; failed: Array<{ id: number; reason: string }> }>(
    '/forward-rules/batch',
    input,
  )
}

/** 批量调整倍率（按比例，如全部 ×1.5）。 */
export function batchMultiplier(ids: number[], scale: number) {
  return post<{ updated: number }>('/forward-rules/batch-multiplier', { ids, scale })
}

/** 导入规则；preview=true 时只返回预览而不写库。 */
export function importRules(input: ImportRequest, preview = false) {
  return post<ImportPreview | { created: number; skipped: number }>(
    '/forward-rules/import',
    input,
    { params: { preview } },
  )
}

/** 导出规则为 JSON。 */
export function exportRules(params?: RuleQuery) {
  return get<{ rules: ForwardRule[] }>(
    '/forward-rules/export',
    params as Record<string, unknown>,
  )
}

/** 下载规则导出文件。 */
export function downloadExport(params?: RuleQuery) {
  return download('/forward-rules/export', params as Record<string, unknown>, 'rules.json')
}

// ───────────────────────── 规则分组 ─────────────────────────

/** 规则分组列表。 */
export function listGroups(params?: PageQuery) {
  return getList<RuleGroup>('/rule-groups', params as Record<string, unknown>)
}

/** 创建规则分组。 */
export function createGroup(input: { name: string; sort?: number; remark?: string }) {
  return post<RuleGroup>('/rule-groups', input)
}

/** 更新规则分组。 */
export function updateGroup(
  id: number,
  input: { name?: string; sort?: number; remark?: string },
) {
  return put<RuleGroup>(`/rule-groups/${id}`, input)
}

/** 删除规则分组。 */
export function removeGroup(id: number) {
  return del<null>(`/rule-groups/${id}`)
}

/** 调整规则分组顺序。 */
export function reorderGroups(ids: number[]) {
  return post<null>('/rule-groups/reorder', { ids })
}
