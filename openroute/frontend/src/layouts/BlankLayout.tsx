/**
 * 空白布局：用于登录页等无需侧边栏的页面。
 *
 * 提供居中的卡片容器与主题切换入口，
 * 背景使用主题变量，因此透明主题下会透出渐变背景。
 */
import { Outlet } from 'react-router-dom'
import { Layout, Space } from 'antd'

import ThemeSwitch from '../components/ThemeSwitch'
import LanguageSwitch from '../components/LanguageSwitch'

export default function BlankLayout() {
  return (
    <Layout style={{ minHeight: '100vh', background: 'transparent' }}>
      <div
        style={{
          position: 'fixed',
          top: 16,
          right: 20,
          display: 'flex',
          gap: 12,
          alignItems: 'center',
          zIndex: 10,
        }}
      >
        <Space size={8}>
          <LanguageSwitch />
          <ThemeSwitch />
        </Space>
      </div>
      <Outlet />
    </Layout>
  )
}
