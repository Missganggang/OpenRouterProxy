/** 设备组接口（规格书 8.8）。 */
import { get, getList, post, put, del } from '../client'
import type { DeviceGroup, DeviceGroupType, InboundConfig, OutboundConfig } from '../types'

/** 设备组列表查询参数。 */
export interface DeviceGroupQuery {
  type?: DeviceGroupType
  keyword?: string
  page?: number
  page_size?: number
}

/** 创建设备组的请求体。 */
export interface DeviceGroupInput {
  name: string
  type: DeviceGroupType
  node_ids: number[]
  config?: InboundConfig | OutboundConfig
  balance?: string
  health_check_enable?: boolean
  health_check_interval?: number
  health_check_timeout?: number
  health_check_fail_count?: number
  health_check_succ_count?: number
  failover_group_id?: number
  remark?: string
}

/** 字段 schema 描述，供前端动态渲染表单（规格书 8.8 /schema）。 */
export interface FieldSchema {
  key: string
  label: string
  type: 'string' | 'number' | 'boolean' | 'array' | 'select' | 'text' | 'object'
  default?: unknown
  options?: Array<{ label: string; value: unknown }>
  required?: boolean
  min?: number
  max?: number
  /** 字段说明（含风险提示）。 */
  help?: string
  /** 是否标记为风险项（前端高亮）。 */
  risk?: boolean
}

/** 设备组配置 schema。 */
export interface DeviceGroupSchema {
  type: DeviceGroupType
  fields: FieldSchema[]
}

/** 校验结果。 */
export interface ValidateResult {
  valid: boolean
  errors: Array<{ field: string; message: string }>
  warnings: string[]
}

/** 组健康状态。 */
export interface GroupHealth {
  group_id: number
  group_name: string
  nodes: Array<{
    node_id: number
    node_name: string
    online: boolean
    healthy: boolean
    current_conn: number
    weight: number
    /** 该节点在负载均衡中的实际占比（0~1）。 */
    load_ratio: number
    /** 连续失败次数（用于故障转移判定）。 */
    fail_count: number
  }>
  available: number
  total: number
}

/** 设备组列表。 */
export function list(params?: DeviceGroupQuery) {
  return getList<DeviceGroup>('/device-groups', params as Record<string, unknown>)
}

/** 设备组详情。 */
export function getOne(id: number) {
  return get<DeviceGroup>(`/device-groups/${id}`)
}

/** 创建设备组。 */
export function create(input: DeviceGroupInput) {
  return post<DeviceGroup>('/device-groups', input)
}

/** 更新设备组。 */
export function update(id: number, input: Partial<DeviceGroupInput>) {
  return put<DeviceGroup>(`/device-groups/${id}`, input)
}

/** 删除设备组（被规则引用时返回 40903）。 */
export function remove(id: number) {
  return del<null>(`/device-groups/${id}`)
}

/** 取某个已存在组的配置字段 schema。 */
export function schema(id: number) {
  return get<DeviceGroupSchema>(`/device-groups/${id}/schema`)
}

/** 按类型取配置字段 schema（新建组时使用）。 */
export function schemaByType(type: DeviceGroupType) {
  return get<DeviceGroupSchema>('/device-groups-schema', { type })
}

/** 校验配置合法性（含故障转移组兼容性）。 */
export function validate(id: number, config: InboundConfig | OutboundConfig) {
  return post<ValidateResult>(`/device-groups/${id}/validate`, { config })
}

/** 组内节点健康状态与负载分布。 */
export function health(id: number) {
  return get<GroupHealth>(`/device-groups/${id}/health`)
}

/** 调整组内节点顺序。 */
export function reorder(id: number, nodeIds: number[]) {
  return post<null>(`/device-groups/${id}/reorder`, { node_ids: nodeIds })
}

// 组内节点的完整信息由 health(id) 返回（含在线状态、负载占比、失败计数），
// 因此这里不再单独提供「只取节点列表」的接口，避免出现两份口径不一致的数据。
