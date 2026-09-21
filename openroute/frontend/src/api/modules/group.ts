/** 节点分组接口（规格书 8.7）。 */
import { get, getList, post, put, del, download } from '../client'
import type { NodeGroup, Node, PageQuery } from '../types'

/** 节点分组列表。 */
export function list(params?: PageQuery) {
  return getList<NodeGroup>('/node-groups', params as Record<string, unknown>)
}

/** 节点分组详情。 */
export function getOne(id: number) {
  return get<NodeGroup>(`/node-groups/${id}`)
}

/** 创建节点分组。 */
export function create(input: { name: string; remark?: string }) {
  return post<NodeGroup>('/node-groups', input)
}

/** 更新节点分组。 */
export function update(id: number, input: { name?: string; remark?: string }) {
  return put<NodeGroup>(`/node-groups/${id}`, input)
}

/** 删除节点分组。 */
export function remove(id: number) {
  return del<null>(`/node-groups/${id}`)
}

/** 分组内的节点。 */
export function nodes(id: number) {
  return get<Node[]>(`/node-groups/${id}/nodes`)
}

/** 导出分组内节点为 CSV。 */
export function exportCSV(id: number, name: string) {
  return download(`/api/v1/node-groups/${id}/export`, undefined, `${name}.csv`)
}
