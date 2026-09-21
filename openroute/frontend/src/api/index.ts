/**
 * API 层的统一出口。
 *
 * 按资源组织命名空间，页面里写成 `nodeApi.list(...)`、`ruleApi.create(...)`，
 * 比逐个 import 具名函数更易读，也避免命名冲突。
 */
import * as authApi from './modules/auth'
import * as nodeApi from './modules/node'
import * as nodeGroupApi from './modules/group'
import * as deviceGroupApi from './modules/deviceGroup'
import * as ruleApi from './modules/rule'
import * as userApi from './modules/user'
import * as trafficApi from './modules/traffic'
import * as systemApi from './modules/system'
import * as migrationApi from './modules/migration'

export {
  authApi,
  nodeApi,
  nodeGroupApi,
  deviceGroupApi,
  ruleApi,
  userApi,
  trafficApi,
  systemApi,
  migrationApi,
}

/** 设置接口单独包装一层，供主题与站点配置使用。 */
export const settingApi = {
  /** 读取全部设置。 */
  getAll: () => systemApi.getSettings(),
  /** 批量更新设置。 */
  update: (values: Record<string, unknown>) => systemApi.updateSettings(values),
}

// ApiError 必须作为「值」导出（不能写成 export type）：
// 页面里用 `err instanceof ApiError` 做分支判断，类型导出在运行期会被擦除。
export { ApiError, showApiError } from './client'
export type { ApiBody } from './client'
export * from './types'
