/** 认证相关接口（规格书 8.5）。 */
import { http, post, get, setAccessToken, clearAccessToken, type ApiBody } from '../client'
import type { LoginResult, User } from '../types'
import type { AxiosResponse } from 'axios'

/** 登录请求参数。 */
export interface LoginParams {
  username: string
  password: string
  captcha?: string
  captcha_id?: string
}

/** 登录并记录访问令牌。 */
export async function login(params: LoginParams): Promise<LoginResult> {
  const result = await post<LoginResult>('/auth/login', params)
  if (result?.access_token) {
    setAccessToken(result.access_token)
  }
  return result
}

/** 登出并清除本地令牌。 */
export async function logout(): Promise<void> {
  try {
    await post<null>('/auth/logout')
  } finally {
    clearAccessToken()
  }
}

/** 获取当前登录用户信息与权限。 */
export function me(): Promise<User> {
  return get<User>('/auth/me')
}

/** 修改自己的密码。 */
export function changePassword(oldPassword: string, newPassword: string): Promise<null> {
  return post<null>('/auth/password', {
    old_password: oldPassword,
    new_password: newPassword,
  })
}

/** 图形验证码响应。 */
export interface CaptchaResult {
  captcha_id: string
  svg: string
}

/** 获取图形验证码（连续失败 3 次后由后端强制要求）。 */
export function captcha(): Promise<CaptchaResult> {
  return get<CaptchaResult>('/auth/captcha')
}

/** 刷新令牌。 */
export async function refresh(): Promise<void> {
  const resp: AxiosResponse<ApiBody<LoginResult>> = await http.post('/auth/refresh')
  const token = resp.data?.data?.access_token
  if (token) {
    setAccessToken(token)
  }
}
