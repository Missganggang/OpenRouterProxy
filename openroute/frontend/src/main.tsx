/**
 * 应用入口。
 *
 * 在 React 挂载前先应用主题、注册 401 回调，
 * 避免首屏出现主题闪烁或未处理的鉴权失败。
 */
import React from 'react'
import ReactDOM from 'react-dom/client'
import { ConfigProvider, App as AntdApp } from 'antd'
import zhCN from 'antd/locale/zh_CN'
import enUS from 'antd/locale/en_US'
import { BrowserRouter } from 'react-router-dom'

import App from './App'
import { antdTheme } from './styles/antdTheme'
import { applyTheme, readCachedTheme, useThemeStore } from './store/theme'
import { registerUnauthorizedHandler, useAuthStore } from './store/auth'
import { useI18n } from './locales'

import './styles/variables.css'
import './styles/global.css'

// 首屏立即应用缓存的主题，避免白屏闪一下再变色。
applyTheme(readCachedTheme())

// 注册 401 回调：令牌失效时清空登录态，路由守卫会跳转登录页。
registerUnauthorizedHandler()

/** 根组件：负责语言与主题的 Provider 装配。 */
function Root() {
  const lang = useI18n((s) => s.lang)
  const theme = useThemeStore((s) => s.theme)

  return (
    <ConfigProvider
      locale={lang === 'en-US' ? enUS : zhCN}
      theme={antdTheme(theme)}
      // 全局组件默认尺寸，紧凑一些更适合管理后台。
      componentSize="middle"
    >
      <AntdApp>
        <BrowserRouter>
          <App />
        </BrowserRouter>
      </AntdApp>
    </ConfigProvider>
  )
}

const container = document.getElementById('root')
if (!container) {
  throw new Error('找不到 #root 容器，请检查 index.html')
}

// 启动时拉取一次站点配置与登录态。
void useThemeStore.getState().loadSiteConfig()
void useAuthStore.getState().bootstrap()

ReactDOM.createRoot(container).render(
  <React.StrictMode>
    <Root />
  </React.StrictMode>,
)
