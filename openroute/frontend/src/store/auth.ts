/**
 * 认证与权限状态（Zustand）。
 *
 * 只保存「当前用户」与「加载状态」，令牌本身由 api/client 的内存变量持有，
 * 避免令牌散落在多个地方造成不一致。
 */
import { create } from 'zustand'

import { authApi } from '../api'
import { clearAccessToken, setUnauthorizedHandler } from '../api/client'
import type { User } from '../api/types'

interface AuthState {
  /** 当前登录用户，未登录为 null。 */
  user: User | null
  /** 是否正在恢复登录态（首次进入页面时）。 */
  loading: boolean
  /** 是否已登录。 */
  loggedIn: boolean

  /** 恢复登录态：页面刷新后通过 Session Cookie 或 /auth/me 拉取用户。 */
  bootstrap: () => Promise<void>
  /** 登录。 */
  login: (username: string, password: string, captcha?: string, captchaId?: string) => Promise<void>
  /** 登出。 */
  logout: () => Promise<void>
  /** 刷新当前用户信息。 */
  refreshUser: () => Promise<void>
}

export const useAuthStore = create<AuthState>((set) => ({
  user: null,
  loading: true,
  loggedIn: false,

  bootstrap: async () => {
    set({ loading: true })
    try {
      const user = await authApi.me()
      set({ user, loggedIn: true, loading: false })
    } catch {
      // 未登录是正常状态，不弹错误提示。
      set({ user: null, loggedIn: false, loading: false })
    }
  },

  login: async (username, password, captcha, captchaId) => {
    const result = await authApi.login({
      username,
      password,
      captcha,
      captcha_id: captchaId,
    })
    set({ user: result.user, loggedIn: true, loading: false })
  },

  logout: async () => {
    try {
      await authApi.logout()
    } finally {
      clearAccessToken()
      set({ user: null, loggedIn: false, loading: false })
    }
  },

  refreshUser: async () => {
    try {
      const user = await authApi.me()
      set({ user, loggedIn: true })
    } catch {
      set({ user: null, loggedIn: false })
    }
  },
}))

/** 注册 401 回调：令牌失效时清空登录态，由路由层跳转登录页。 */
export function registerUnauthorizedHandler(): void {
  setUnauthorizedHandler(() => {
    useAuthStore.setState({ user: null, loggedIn: false, loading: false })
  })
}

/** 判断当前用户是否具备指定角色。 */
export function hasRole(user: User | null, role: 'admin' | 'user'): boolean {
  if (!user) return false
  return user.role === role
}
