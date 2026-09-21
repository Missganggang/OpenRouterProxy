/**
 * 应用根组件：装配路由。
 *
 * 路由分为两组：
 *  - 公开路由（登录页）：未登录也可访问；
 *  - 受保护路由：未登录重定向到登录页，并按权限过滤菜单。
 */
import { Navigate, Route, Routes, useLocation } from 'react-router-dom'
import { Spin } from 'antd'

import ErrorBoundary from './components/ErrorBoundary'
import BasicLayout from './layouts/BasicLayout'
import BlankLayout from './layouts/BlankLayout'
import LoginPage from './pages/login'
import DashboardPage from './pages/dashboard'
import NodesPage from './pages/nodes'
import NodeDetailPage from './pages/nodes/Detail'
import NodeGroupsPage from './pages/node-groups'
import DeviceGroupsPage from './pages/device-groups'
import ForwardRulesPage from './pages/forward-rules'
import RuleGroupsPage from './pages/rule-groups'
import UsersPage from './pages/users'
import TrafficPage from './pages/traffic'
import MonitorPage from './pages/monitor'
import AlertsPage from './pages/alerts'
import SnapshotsPage from './pages/snapshots'
import MigrationPage from './pages/migration'
import SettingsPage from './pages/settings'
import NotFoundPage from './pages/NotFound'

import { useAuthStore } from './store/auth'

/** 路由守卫：未登录时重定向到登录页，并记录来源以便登录后跳回。 */
function RequireAuth({ children }: { children: React.ReactNode }) {
  const loading = useAuthStore((s) => s.loading)
  const loggedIn = useAuthStore((s) => s.loggedIn)
  const location = useLocation()

  if (loading) {
    return (
      <div
        style={{
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'center',
          height: '100vh',
        }}
      >
        <Spin size="large" tip="正在恢复登录状态…">
          <div style={{ padding: 24 }} />
        </Spin>
      </div>
    )
  }

  if (!loggedIn) {
    return <Navigate to="/login" replace state={{ from: location.pathname }} />
  }

  return <>{children}</>
}

function RequireAdmin({ children }: { children: React.ReactNode }) {
  const user = useAuthStore((state) => state.user)
  return user?.role === 'admin' ? <>{children}</> : <Navigate to="/forward-rules" replace />
}

export default function App() {
  return (
    <Routes>
      {/* 公开路由 */}
      <Route
        element={
          <ErrorBoundary>
            <BlankLayout />
          </ErrorBoundary>
        }
      >
        <Route path="/login" element={<LoginPage />} />
      </Route>

      {/* 受保护路由 */}
      <Route
        element={
          <RequireAuth>
            {/* 错误边界包在布局外层：任何页面渲染异常都会显示可读错误而不是白屏 */}
            <ErrorBoundary>
              <BasicLayout />
            </ErrorBoundary>
          </RequireAuth>
        }
      >
        <Route path="/" element={<Navigate to="/dashboard" replace />} />
        <Route path="/dashboard" element={<DashboardPage />} />

        <Route path="/nodes" element={<RequireAdmin><NodesPage /></RequireAdmin>} />
        <Route path="/nodes/:id" element={<RequireAdmin><NodeDetailPage /></RequireAdmin>} />
        <Route path="/node-groups" element={<RequireAdmin><NodeGroupsPage /></RequireAdmin>} />

        <Route path="/device-groups" element={<RequireAdmin><DeviceGroupsPage /></RequireAdmin>} />

        <Route path="/forward-rules" element={<ForwardRulesPage />} />
        <Route path="/rule-groups" element={<RequireAdmin><RuleGroupsPage /></RequireAdmin>} />

        <Route path="/users" element={<RequireAdmin><UsersPage /></RequireAdmin>} />

        <Route path="/traffic" element={<TrafficPage />} />
        <Route path="/monitor" element={<RequireAdmin><MonitorPage /></RequireAdmin>} />
        <Route path="/alerts" element={<RequireAdmin><AlertsPage /></RequireAdmin>} />
        <Route path="/snapshots" element={<RequireAdmin><SnapshotsPage /></RequireAdmin>} />
        <Route path="/migration" element={<RequireAdmin><MigrationPage /></RequireAdmin>} />
        <Route path="/settings" element={<RequireAdmin><SettingsPage /></RequireAdmin>} />
      </Route>

      <Route path="*" element={<NotFoundPage />} />
    </Routes>
  )
}
