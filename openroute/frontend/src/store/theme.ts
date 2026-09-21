/**
 * 主题与站点信息状态。
 *
 * 规格书 6.16 要求保留「经典 / 透明」两套主题，
 * 且切换时无需重新加载页面——通过切换 <html> 上的 class 实现，
 * 具体样式由 styles/variables.css 中的 CSS 变量定义。
 */
import { create } from 'zustand'

import { settingApi } from '../api'

/** 可用主题。 */
export type ThemeName = 'classic' | 'transparent'

/** 站点配置。 */
export interface SiteConfig {
  site_name: string
  theme: ThemeName
  logo: string
  favicon: string
  announcement: string
}

const THEME_STORAGE_KEY = 'openroute_theme'

interface ThemeState extends SiteConfig {
  /** 是否已从后端加载过站点配置。 */
  loaded: boolean
  /** 侧边栏是否折叠（响应式）。 */
  collapsed: boolean

  /** 从后端加载站点配置。 */
  loadSiteConfig: () => Promise<void>
  /** 切换主题（本地立即生效，并回写后端设置）。 */
  setTheme: (theme: ThemeName) => Promise<void>
  /** 更新本地站点信息（保存设置后调用）。 */
  patchSiteConfig: (patch: Partial<SiteConfig>) => void
  /** 切换侧边栏折叠状态。 */
  toggleCollapsed: () => void
}

/** 把主题应用到 <html> 元素。
 *
 *  单独抽出以便在 store 创建前也能调用（避免首屏闪烁）。
 */
export function applyTheme(theme: ThemeName): void {
  const root = document.documentElement
  root.classList.remove('theme-classic', 'theme-transparent')
  root.classList.add(`theme-${theme}`)
  // antd 的暗色算法在透明主题下不启用，这里只做标记，
  // 具体颜色由 CSS 变量控制。
  root.setAttribute('data-theme', theme)
}

/** 读取本地缓存的主题，用于首屏在接口返回前避免闪烁。 */
export function readCachedTheme(): ThemeName {
  const cached = localStorage.getItem(THEME_STORAGE_KEY)
  return cached === 'transparent' ? 'transparent' : 'classic'
}

export const useThemeStore = create<ThemeState>((set, get) => ({
  site_name: 'OpenRoute',
  theme: readCachedTheme(),
  logo: '',
  favicon: '',
  announcement: '',
  loaded: false,
  collapsed: false,

  loadSiteConfig: async () => {
    try {
      const settings = await settingApi.getAll()
      const theme = normalizeTheme(settings.theme)
      set({
        site_name: asString(settings.site_name) || 'OpenRoute',
        theme,
        logo: asString(settings.logo),
        favicon: asString(settings.favicon),
        announcement: asString(settings.announcement),
        loaded: true,
      })
      applyTheme(theme)
      localStorage.setItem(THEME_STORAGE_KEY, theme)
      applySiteTitle(asString(settings.site_name) || 'OpenRoute', asString(settings.favicon))
    } catch {
      // 站点配置加载失败不应阻断页面渲染，沿用默认值。
      set({ loaded: true })
      applyTheme(get().theme)
    }
  },

  setTheme: async (theme) => {
    // 先本地生效，再回写后端，保证切换是「瞬时」的。
    applyTheme(theme)
    localStorage.setItem(THEME_STORAGE_KEY, theme)
    set({ theme })
    try {
      await settingApi.update({ theme })
    } catch {
      // 回写失败时仍保留本地主题，下次进入设置页可重试。
    }
  },

  patchSiteConfig: (patch) => {
    set(patch)
    if (patch.site_name !== undefined || patch.favicon !== undefined) {
      applySiteTitle(
        patch.site_name ?? get().site_name,
        patch.favicon ?? get().favicon,
      )
    }
  },

  toggleCollapsed: () => set((s) => ({ collapsed: !s.collapsed })),
}))

/** 把任意设置值规范化为主题名。 */
function normalizeTheme(value: unknown): ThemeName {
  return value === 'transparent' ? 'transparent' : 'classic'
}

/** 把任意设置值转为字符串，null/undefined 归为空串。 */
function asString(value: unknown): string {
  if (value === null || value === undefined) return ''
  if (typeof value === 'string') return value
  return String(value)
}

/** 更新浏览器标题与 favicon。 */
export function applySiteTitle(name: string, favicon: string): void {
  document.title = name
  if (!favicon) return
  let link = document.querySelector<HTMLLinkElement>('link[rel="icon"]')
  if (!link) {
    link = document.createElement('link')
    link.rel = 'icon'
    document.head.appendChild(link)
  }
  link.href = favicon
}
