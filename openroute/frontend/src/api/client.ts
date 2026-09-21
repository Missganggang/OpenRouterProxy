/**
 * Axios 实例与统一拦截器（规格书 9.3）。
 *
 * 职责：
 *  - 自动附带访问令牌与请求 ID
 *  - 统一解析后端响应包体，把 code != 0 转成抛出的错误
 *  - 401 时自动尝试刷新令牌，失败则跳转登录页
 *  - 统一错误提示：message 一行，details.hint 存在时拼第二行
 */
import axios, {
  type AxiosInstance,
  type AxiosRequestConfig,
  type AxiosResponse,
  type InternalAxiosRequestConfig,
} from 'axios'
import { message } from 'antd'

import type { ErrorDetails, ListData } from './types'

/** 后端统一响应包体。 */
export interface ApiBody<T = unknown> {
  code: number
  message: string
  data?: T
  details?: ErrorDetails
  request_id?: string
  timestamp?: string
}

/** 业务错误：携带后端错误码与定位信息，便于表单定位字段。 */
export class ApiError extends Error {
  code: number
  details?: ErrorDetails
  requestId?: string

  constructor(code: number, msg: string, details?: ErrorDetails, requestId?: string) {
    super(msg)
    this.name = 'ApiError'
    this.code = code
    this.details = details
    this.requestId = requestId
  }
}

/** 访问令牌在内存中的副本。
 *
 *  刻意不写入 localStorage：面板常被反向代理暴露在公网，
 *  内存存储可降低 XSS 场景下的令牌泄露面。刷新页面时由
 *  refresh_token（HttpOnly 之外的场景）或 Session Cookie 恢复登录态。
 */
let accessToken = ''

export function setAccessToken(token: string): void {
  accessToken = token
}

export function getAccessToken(): string {
  return accessToken
}

export function clearAccessToken(): void {
  accessToken = ''
}

/** 令牌刷新中的并发保护：多个 401 同时到达时只发起一次刷新。 */
let refreshing: Promise<boolean> | null = null

/** 未登录时的回调，由路由层注入，避免 api 层依赖 store。 */
let onUnauthorized: () => void = () => {}
export function setUnauthorizedHandler(fn: () => void): void {
  onUnauthorized = fn
}

/** 创建 axios 实例。 */
export const http: AxiosInstance = axios.create({
  baseURL: '/api/v1',
  timeout: 30000,
  // 携带 Session Cookie（HttpOnly，SameSite=Lax）。
  withCredentials: true,
  headers: {
    'Content-Type': 'application/json; charset=utf-8',
  },
})

/** 请求拦截器：附带令牌与请求 ID。 */
http.interceptors.request.use(
  (config: InternalAxiosRequestConfig) => {
    if (accessToken && config.headers) {
      config.headers.Authorization = `Bearer ${accessToken}`
    }
    // 生成请求 ID，便于用户报障时在日志中定位。
    if (config.headers && !config.headers['X-Request-ID']) {
      config.headers['X-Request-ID'] = `web_${Date.now().toString(36)}${Math.random()
        .toString(36)
        .slice(2, 10)}`
    }
    return config
  },
  (error) => Promise.reject(error),
)

/** 响应拦截器：统一解包与错误处理。 */
http.interceptors.response.use(
  (resp: AxiosResponse<ApiBody>) => {
    const body = resp.data
    // 后端始终返回统一包体；非 0 一律视为失败。
    if (body && typeof body.code === 'number' && body.code !== 0) {
      throw new ApiError(body.code, body.message || '请求失败', body.details, body.request_id)
    }
    return resp
  },
  async (error) => {
    const resp = error?.response as AxiosResponse<ApiBody> | undefined

    // 401：尝试刷新令牌后重放一次原请求。
    if (resp?.status === 401) {
      const original = error.config as InternalAxiosRequestConfig & { _retried?: boolean }
      // 登录接口本身的 401 不触发刷新，否则会递归。
      const isAuthEndpoint = (original?.url || '').includes('/auth/login') ||
        (original?.url || '').includes('/auth/refresh')

      if (!original?._retried && !isAuthEndpoint) {
        original._retried = true
        const ok = await refreshToken()
        if (ok) {
          return http.request(original)
        }
      }

      clearAccessToken()
      onUnauthorized()
      const body = resp.data
      throw new ApiError(
        body?.code ?? 40101,
        body?.message || '登录状态已失效，请重新登录',
        body?.details,
        body?.request_id,
      )
    }

    // 其余错误：优先使用后端返回的业务错误信息。
    if (resp?.data && typeof resp.data.code === 'number') {
      throw new ApiError(
        resp.data.code,
        resp.data.message || '请求失败',
        resp.data.details,
        resp.data.request_id,
      )
    }

    // 网络层错误。
    if (error.code === 'ECONNABORTED') {
      throw new ApiError(60003, '请求超时，请检查网络或面板负载')
    }
    throw new ApiError(50001, error?.message || '网络异常，无法连接面板')
  },
)

/** 用刷新令牌换取新的访问令牌。 */
async function refreshToken(): Promise<boolean> {
  if (refreshing) {
    return refreshing
  }
  refreshing = (async () => {
    try {
      const resp = await axios.post<ApiBody<{ access_token: string }>>(
        '/api/v1/auth/refresh',
        {},
        { withCredentials: true },
      )
      const token = resp.data?.data?.access_token
      if (token) {
        setAccessToken(token)
        return true
      }
      return false
    } catch {
      return false
    } finally {
      // 允许下一次刷新。
      refreshing = null
    }
  })()
  return refreshing
}

/** 展示错误提示。
 *
 *  规格书 9.3：用 message.error 展示 message 字段，
 *  details.hint 存在时拼在第二行。
 */
export function showApiError(err: unknown, fallback = '操作失败'): void {
  if (err instanceof ApiError) {
    const hint = err.details?.hint
    message.error(hint ? `${err.message}\n${hint}` : err.message)
    return
  }
  if (err instanceof Error && err.message) {
    message.error(err.message)
    return
  }
  message.error(fallback)
}

/** 解包 data 字段，失败时抛出 ApiError。 */
export async function unwrap<T>(promise: Promise<AxiosResponse<ApiBody<T>>>): Promise<T> {
  const resp = await promise
  return (resp.data.data ?? ({} as T)) as T
}

/** 解包列表响应，返回 items 与 pagination。 */
export async function unwrapList<T>(
  promise: Promise<AxiosResponse<ApiBody<ListData<T>>>>,
): Promise<ListData<T>> {
  const resp = await promise
  const data = resp.data.data
  return {
    items: data?.items ?? [],
    pagination: data?.pagination ?? { page: 1, page_size: 20, total: 0, total_pages: 0 },
  }
}

/** 发起 GET 请求并解包。 */
export function get<T>(url: string, params?: Record<string, unknown>, config?: AxiosRequestConfig) {
  return unwrap<T>(http.get<ApiBody<T>>(url, { params, ...config }))
}

/** 发起 GET 列表请求并解包。 */
export function getList<T>(url: string, params?: Record<string, unknown>) {
  return unwrapList<T>(http.get<ApiBody<ListData<T>>>(url, { params }))
}

/** 发起 POST 请求并解包。 */
export function post<T>(url: string, data?: unknown, config?: AxiosRequestConfig) {
  return unwrap<T>(http.post<ApiBody<T>>(url, data, config))
}

/** 发起 PUT 请求并解包。 */
export function put<T>(url: string, data?: unknown) {
  return unwrap<T>(http.put<ApiBody<T>>(url, data))
}

/** 发起 DELETE 请求并解包。 */
export function del<T>(url: string, data?: unknown) {
  return unwrap<T>(http.delete<ApiBody<T>>(url, { data }))
}

/** 下载文件（用于 CSV 导出与备份下载）。 */
export async function download(url: string, params?: Record<string, unknown>, filename?: string) {
  const resp = await http.get(url, { params, responseType: 'blob' })
  const blob = new Blob([resp.data as BlobPart])
  const link = document.createElement('a')
  link.href = URL.createObjectURL(blob)
  link.download = filename || guessFilename(resp.headers['content-disposition']) || 'download'
  document.body.appendChild(link)
  link.click()
  document.body.removeChild(link)
  URL.revokeObjectURL(link.href)
}

/** 从 Content-Disposition 响应头解析文件名。 */
function guessFilename(disposition?: string): string {
  if (!disposition) return ''
  const match = /filename\*?=(?:UTF-8'')?"?([^";]+)"?/i.exec(disposition)
  return match?.[1] ? decodeURIComponent(match[1]) : ''
}
