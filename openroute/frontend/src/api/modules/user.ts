/** 用户与用户分组接口（规格书 8.11）。 */
import { get, getList, post, put, del } from '../client'
import type { User, UserGroup, PageQuery, ForwardRule, UserTraffic } from '../types'

/** 用户列表查询参数。 */
export interface UserQuery extends PageQuery {
  group_id?: number
  role?: string
  status?: number
}

/** 创建 / 更新用户的请求体。 */
export interface UserInput {
  username: string
  password?: string
  nickname?: string
  role?: 'admin' | 'user'
  group_id?: number
  traffic_limit?: number
  speed_limit?: number
  ip_limit?: number
  device_limit?: number
  conn_limit?: number
  /** 账号状态：1 启用 0 禁用（与后端 model.User.Status 一致）。 */
  status?: 0 | 1
  expire_at?: string | null
  remark?: string
}

/** 用户列表。 */
export function list(params?: UserQuery) {
  return getList<User>('/users', params as Record<string, unknown>)
}

/** 用户详情。 */
export function getOne(id: number) {
  return get<User>(`/users/${id}`)
}

/** 创建用户。 */
export function create(input: UserInput) {
  return post<User & { password?: string }>('/users', input)
}

/** 更新用户。 */
export function update(id: number, input: Partial<UserInput>) {
  return put<User>(`/users/${id}`, input)
}

/** 删除用户。 */
export function remove(id: number) {
  return del<null>(`/users/${id}`)
}

/** 重置密码（返回一次性明文）。 */
export function resetPassword(id: number) {
  return post<{ password: string }>(`/users/${id}/reset-password`)
}

/** 重置订阅 Token。 */
export function resetToken(id: number) {
  return post<{ token: string }>(`/users/${id}/reset-token`)
}

/** 禁用用户。 */
export function disable(id: number) {
  return post<null>(`/users/${id}/disable`)
}

/**
 * 用户的流量统计。
 *
 * 返回的是各周期**合计**与按规则的分布，不是时间序列
 * （用户维度要的是「已用 / 上限 / 剩余」，见 UserTraffic 的注释）。
 */
export function traffic(id: number, params?: { from?: string; to?: string; interval?: string }) {
  return get<UserTraffic>(`/users/${id}/traffic`, params as Record<string, unknown>)
}

/** 用户拥有的规则。 */
export function rules(id: number) {
  return get<ForwardRule[]>(`/users/${id}/rules`)
}

// ───────────────────────── 用户分组 ─────────────────────────

/** 用户分组列表。 */
export async function groups(params?: PageQuery) {
  const items = (await get<UserGroup[]>('/user-groups', params as Record<string, unknown>)) ?? []
  return { items, pagination: { page: 1, page_size: items.length, total: items.length } }
}

/** 用户分组详情。 */
export function getGroup(id: number) {
  return get<UserGroup>(`/user-groups/${id}`)
}

/** 创建用户分组。 */
export function createGroup(input: Partial<UserGroup>) {
  return post<UserGroup>('/user-groups', input)
}

/** 更新用户分组。 */
export function updateGroup(id: number, input: Partial<UserGroup>) {
  return put<UserGroup>(`/user-groups/${id}`, input)
}

/** 删除用户分组。 */
export function removeGroup(id: number) {
  return del<null>(`/user-groups/${id}`)
}

/** 构造用户的订阅地址。 */
export function subscribeURL(token: string): string {
  return `${window.location.origin}/sub/${token}`
}
