/** 系统、设置、审计、任务、告警、快照、API Token 接口（规格书 8.14）。 */
import { get, getList, post, put, del } from '../client'
import type {
  SystemInfo,
  SystemStatus,
  AuditLog,
  TaskStatus,
  AlertRule,
  AlertHistory,
  ConfigSnapshot,
  APIToken,
  PageQuery,
} from '../types'

/** 错误码字典条目。 */
export interface ErrorDictEntry {
  code: number
  message: string
  http_status: number
}

/** 系统信息。 */
export function info() {
  return get<SystemInfo>('/system/info')
}

/** 系统健康检查。 */
export function status() {
  return get<SystemStatus>('/system/status')
}

/** 后端与节点客户端版本清单。 */
export function version() {
  return get<{ backend: string; node_client: string; build: string }>('/system/version')
}

/** 完整错误码字典（供前端统一处理与文档展示）。 */
export function errorDict() {
  return get<ErrorDictEntry[]>('/system/errors')
}

// ───────────────────────── 设置 ─────────────────────────

/** 读取全部设置。 */
export function getSettings() {
  return get<Record<string, unknown>>('/settings')
}

/** 批量更新设置。 */
export function updateSettings(values: Record<string, unknown>) {
  return put<Record<string, unknown>>('/settings', values)
}

// ───────────────────────── 审计日志 ─────────────────────────

/** 审计日志查询参数。 */
export interface AuditQuery extends PageQuery {
  user_id?: number
  action?: string
  resource?: string
  result?: string
  from?: string
  to?: string
}

/** 审计日志列表。 */
export function auditLogs(params?: AuditQuery) {
  return getList<AuditLog>('/audit-logs', params as Record<string, unknown>)
}

// ───────────────────────── 任务 ─────────────────────────

/** 后台任务列表与状态。 */
export function tasks() {
  return get<TaskStatus[]>('/tasks')
}

/** 手动触发任务。 */
export function runTask(name: string) {
  return post<null>(`/tasks/${name}/run`)
}

// ───────────────────────── 告警 ─────────────────────────

/** 告警规则与历史（一次拿全，供告警中心页使用）。 */
export function alerts(params?: PageQuery) {
  return get<{ rules: AlertRule[]; active: AlertHistory[] }>('/alerts', {
    ...(params as Record<string, unknown>),
    include: 'rules',
  })
}

/** 告警规则列表。 */
export function alertRules(params?: PageQuery) {
  return getList<AlertRule>('/alerts', params as Record<string, unknown>)
}

/** 创建告警规则。 */
export function createAlert(input: Partial<AlertRule>) {
  return post<AlertRule>('/alerts', input)
}

/** 更新告警规则。 */
export function updateAlert(id: number, input: Partial<AlertRule>) {
  return put<AlertRule>(`/alerts/${id}`, input)
}

/** 删除告警规则。 */
export function deleteAlert(id: number) {
  return del<null>(`/alerts/${id}`)
}

/** 测试告警通知渠道。 */
export function testAlert(id: number) {
  return post<{ channel: string; ok: boolean; error?: string }[]>(`/alerts/${id}/test`)
}

/** 告警历史列表。 */
export function alertHistory(params?: PageQuery & { resolved?: boolean }) {
  return getList<AlertHistory>('/alerts/history', params as Record<string, unknown>)
}

/** 标记告警已解决。 */
export function resolveAlert(id: number) {
  return post<null>(`/alerts/${id}/resolve`)
}

/** 测试 Webhook 地址。 */
export function testWebhook(url: string) {
  return post<{ ok: boolean; status?: number; error?: string }>('/webhooks/test', { url })
}

// ───────────────────────── 快照 ─────────────────────────

/** 快照列表。 */
export function snapshots(params?: PageQuery) {
  return getList<ConfigSnapshot>('/snapshots', params as Record<string, unknown>)
}

/** 手动生成快照。 */
export function createSnapshot(name?: string) {
  return post<ConfigSnapshot>('/snapshots', { name })
}

/** 快照详情。 */
export function getSnapshot(id: number) {
  return get<ConfigSnapshot>(`/snapshots/${id}`)
}

/** 删除快照。 */
export function deleteSnapshot(id: number) {
  return del<null>(`/snapshots/${id}`)
}

/** 快照与当前配置的差异。 */
export function snapshotDiff(id: number) {
  return get<{
    added: string[]
    removed: string[]
    changed: Array<{ path: string; from: unknown; to: unknown }>
  }>(`/snapshots/${id}/diff`)
}

/** 一键回滚到快照。 */
export function rollbackSnapshot(id: number) {
  return post<{ pre_rollback_snapshot_id: number }>(`/snapshots/${id}/rollback`)
}

// ───────────────────────── API Token ─────────────────────────

/** API Token 列表。 */
export function apiTokens(params?: PageQuery) {
  return getList<APIToken>('/api-tokens', params as Record<string, unknown>)
}

/** 创建 API Token（明文仅返回一次）。 */
export function createAPIToken(input: {
  name: string
  scopes: string[]
  ip_whitelist?: string[]
  expire_at?: string | null
}) {
  return post<APIToken>('/api-tokens', input)
}

/** 删除 API Token。 */
export function deleteAPIToken(id: number) {
  return del<null>(`/api-tokens/${id}`)
}

/** 全部可用 Scope（供创建 Token 时多选）。 */
export const ALL_SCOPES = [
  'node:read',
  'node:write',
  'node:exec',
  'group:read',
  'group:write',
  'rule:read',
  'rule:write',
  'user:read',
  'user:write',
  'traffic:read',
  'system:read',
  'system:write',
  'migrate:run',
  'backup:run',
] as const
