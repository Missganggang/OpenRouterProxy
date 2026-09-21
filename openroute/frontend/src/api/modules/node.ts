/** 节点相关接口（规格书 8.6）。 */
import { get, getList, post, put, del } from '../client'
import type { Node, PageQuery, ForwardRule, ProbeMetric } from '../types'

/** 节点列表查询参数。 */
export interface NodeQuery extends PageQuery {
  group_id?: number
  online?: boolean
  role?: string
}

/** 创建节点的请求体。 */
export interface NodeCreateInput {
  name: string
  role: 'inbound' | 'outbound' | 'both'
  group_ids?: number[]
  weight?: number
  max_conn?: number
  connect_host?: string
  is_static?: boolean
  remark?: string
}

/** 创建节点的响应：含一次性返回的 token 与安装命令。 */
export interface NodeCreateResult {
  id: number
  name: string
  token: string
  install_command: string
  status: string
}

/** 获取节点列表。 */
export function list(params?: NodeQuery) {
  return getList<Node>('/nodes', params as Record<string, unknown>)
}

/** 获取单个节点。 */
export function getOne(id: number) {
  return get<Node>(`/nodes/${id}`)
}

/** 创建节点。 */
export function create(input: NodeCreateInput) {
  return post<NodeCreateResult>('/nodes', input)
}

/** 更新节点。 */
export function update(id: number, input: Partial<NodeCreateInput>) {
  return put<Node>(`/nodes/${id}`, input)
}

/** 删除节点。 */
export function remove(id: number) {
  return del<null>(`/nodes/${id}`)
}

/** 生成（重新获取）节点的安装命令。 */
export function installCommand(id: number) {
  return post<{ install_command: string; token: string }>(`/nodes/${id}/install-command`)
}

/** 升级节点客户端。 */
export function upgrade(id: number) {
  return post<null>(`/nodes/${id}/upgrade`)
}

/** 重启节点服务。 */
export function restart(id: number) {
  return post<null>(`/nodes/${id}/restart`)
}

/** 在节点上执行命令。 */
export function exec(id: number, command: string) {
  return post<{ task_id: number }>(`/nodes/${id}/exec`, { command })
}

/** 重置节点密钥。 */
export function resetToken(id: number) {
  return post<{ token: string }>(`/nodes/${id}/reset-token`)
}

/** 查询节点探针数据。 */
export function metrics(
  id: number,
  params?: { from?: string; to?: string; interval?: string },
) {
  return get<ProbeMetric[]>(`/nodes/${id}/metrics`, params as Record<string, unknown>)
}

/** 查询节点实时指标。 */
export function realtimeMetrics(id: number) {
  return get<ProbeMetric>(`/nodes/${id}/metrics/realtime`)
}

/** 查询节点上运行的规则。 */
export function rules(id: number) {
  return get<ForwardRule[]>(`/nodes/${id}/rules`)
}

/** 查询节点日志。 */
export function logs(id: number, lines = 200) {
  return get<{ lines: string[] }>(`/nodes/${id}/logs`, { lines })
}

/** 查询配置漂移详情。 */
export function drift(id: number) {
  return get<{ drifted: boolean; details: unknown }>(`/nodes/${id}/drift`)
}

/** 一键纠正配置漂移。 */
export function fixDrift(id: number) {
  return post<null>(`/nodes/${id}/drift/fix`)
}

/** 批量升级。 */
export function batchUpgrade(ids: number[]) {
  return post<{ succeeded: number[]; failed: Array<{ id: number; reason: string }> }>(
    '/nodes/batch/upgrade',
    { ids },
  )
}

/** 批量重启节点服务。 */
export function batchRestart(ids: number[]) {
  return post<{ succeeded: number[]; failed: Array<{ id: number; reason: string }> }>(
    '/nodes/batch/restart',
    { ids },
  )
}

/** 批量执行命令。 */
export function batchExec(ids: number[], command: string) {
  return post<{ succeeded: number[]; failed: Array<{ id: number; reason: string }> }>(
    '/nodes/batch/exec',
    { ids, command },
  )
}

/** 批量改分组。 */
export function batchGroup(ids: number[], groupIds: number[]) {
  return post<{ updated: number }>('/nodes/batch/group', {
    ids,
    group_ids: groupIds,
  })
}

/** 批量改权重。 */
export function batchWeight(ids: number[], weight: number) {
  return post<{ updated: number }>('/nodes/batch/weight', { ids, weight })
}
