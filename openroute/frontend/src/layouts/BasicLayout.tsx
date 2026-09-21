/**
 * 主布局：侧边栏 + 顶栏 + 内容区（规格书 9.1）。
 *
 * 要点：
 *  - 菜单按权限过滤（普通用户看不到用户管理、迁移、设置中的敏感项）；
 *  - 响应式：窗口 < 1280px 时自动折叠侧边栏（规格书 9.3）；
 *  - WebSocket 订阅节点状态变化，实时刷新顶栏的在线节点数。
 */
import { useEffect, useMemo, useState } from 'react'
import { Outlet, useLocation, useNavigate } from 'react-router-dom'
import {
  Avatar,
  Badge,
  Dropdown,
  Layout,
  Menu,
  Modal,
  Space,
  Tooltip,
  Typography,
  theme as antdTheme,
} from 'antd'
import {
  AlertOutlined,
  ApiOutlined,
  AppstoreOutlined,
  ClusterOutlined,
  CloudServerOutlined,
  DashboardOutlined,
  DatabaseOutlined,
  DeploymentUnitOutlined,
  GlobalOutlined,
  HistoryOutlined,
  LineChartOutlined,
  MenuFoldOutlined,
  MenuUnfoldOutlined,
  MonitorOutlined,
  SettingOutlined,
  TeamOutlined,
  UserOutlined,
  LogoutOutlined,
  ReloadOutlined,
} from '@ant-design/icons'

import { useAuthStore } from '../store/auth'
import { useThemeStore } from '../store/theme'
import { useI18n } from '../locales'
import { useWebSocket } from '../hooks/useWebSocket'
import LanguageSwitch from '../components/LanguageSwitch'
import ThemeSwitch from '../components/ThemeSwitch'

const { Header, Sider, Content } = Layout

/** 菜单项定义。 */
interface MenuItemDef {
  key: string
  i18nKey: string
  icon: React.ReactNode
  /** 仅管理员可见。 */
  adminOnly?: boolean
}

/** 菜单分组。 */
const MENU_GROUPS: Array<{ titleKey?: string; items: MenuItemDef[] }> = [
  {
    items: [
      { key: '/dashboard', i18nKey: 'nav.dashboard', icon: <DashboardOutlined /> },
    ],
  },
  {
    titleKey: 'nav.nodes',
    items: [
      { key: '/nodes', i18nKey: 'nav.nodes', icon: <CloudServerOutlined /> },
      { key: '/node-groups', i18nKey: 'nav.nodeGroups', icon: <ClusterOutlined /> },
      { key: '/monitor', i18nKey: 'nav.monitor', icon: <MonitorOutlined /> },
    ],
  },
  {
    titleKey: 'nav.forwardRules',
    items: [
      { key: '/device-groups', i18nKey: 'nav.deviceGroups', icon: <DeploymentUnitOutlined /> },
      { key: '/forward-rules', i18nKey: 'nav.forwardRules', icon: <ApiOutlined /> },
      { key: '/rule-groups', i18nKey: 'nav.ruleGroups', icon: <AppstoreOutlined /> },
    ],
  },
  {
    titleKey: 'nav.users',
    items: [
      { key: '/users', i18nKey: 'nav.users', icon: <TeamOutlined />, adminOnly: true },
      { key: '/traffic', i18nKey: 'nav.traffic', icon: <LineChartOutlined /> },
    ],
  },
  {
    titleKey: 'nav.settings',
    items: [
      { key: '/alerts', i18nKey: 'nav.alerts', icon: <AlertOutlined /> },
      { key: '/snapshots', i18nKey: 'nav.snapshots', icon: <HistoryOutlined /> },
      { key: '/migration', i18nKey: 'nav.migration', icon: <DatabaseOutlined />, adminOnly: true },
      { key: '/settings', i18nKey: 'nav.settings', icon: <SettingOutlined />, adminOnly: true },
    ],
  },
]

export default function BasicLayout() {
  const navigate = useNavigate()
  const location = useLocation()
  const { token } = antdTheme.useToken()

  const { t } = useI18n()
  const user = useAuthStore((s) => s.user)
  const logout = useAuthStore((s) => s.logout)

  const collapsed = useThemeStore((s) => s.collapsed)
  const toggleCollapsed = useThemeStore((s) => s.toggleCollapsed)
  const siteName = useThemeStore((s) => s.site_name)
  const logo = useThemeStore((s) => s.logo)
  const announcement = useThemeStore((s) => s.announcement)

  const [onlineNodes, setOnlineNodes] = useState<number | null>(null)

  const isAdmin = user?.role === 'admin'

  // WebSocket：接收节点上下线事件，实时更新顶栏徽标（规格书 9.3）。
  const { status: wsStatus } = useWebSocket('/api/v1/nodes/stream', {
    enabled: !!user,
    onEvent: (ev) => {
      if (ev.type === 'node_online') {
        setOnlineNodes((n) => (n === null ? null : n + 1))
      } else if (ev.type === 'node_offline') {
        setOnlineNodes((n) => (n === null ? null : Math.max(0, n - 1)))
      }
    },
  })

  // 响应式：窗口宽度小于 1280px 时自动折叠侧边栏。
  useEffect(() => {
    const onResize = () => {
      const shouldCollapse = window.innerWidth < 1280
      if (shouldCollapse && !useThemeStore.getState().collapsed) {
        useThemeStore.setState({ collapsed: true })
      }
    }
    onResize()
    window.addEventListener('resize', onResize)
    return () => window.removeEventListener('resize', onResize)
  }, [])

  // 公告：进入面板时提示一次（每次登录会话提示一次即可）。
  useEffect(() => {
    if (!announcement) return
    const key = 'openroute_announcement_seen'
    if (sessionStorage.getItem(key) === announcement) return
    Modal.info({
      title: '公告',
      content: <div style={{ whiteSpace: 'pre-wrap' }}>{announcement}</div>,
      okText: t('common.ok'),
      onOk: () => sessionStorage.setItem(key, announcement),
    })
  }, [announcement, t])

  // 根据权限过滤菜单。
  const menuItems = useMemo(
    () =>
      MENU_GROUPS.flatMap((group) =>
        group.items
          .filter((item) => !item.adminOnly || isAdmin)
          .map((item) => ({
            key: item.key,
            icon: item.icon,
            label: t(item.i18nKey),
          })),
      ),
    [isAdmin, t],
  )

  // 当前选中的菜单项：取路径第一段匹配，保证子页面（如 /nodes/12）也高亮。
  const selectedKey = useMemo(() => {
    const path = location.pathname
    const match = menuItems
      .map((m) => m.key as string)
      .filter((k) => path === k || path.startsWith(`${k}/`))
      .sort((a, b) => b.length - a.length)[0]
    return match ?? '/dashboard'
  }, [location.pathname, menuItems])

  const userMenu = {
    items: [
      {
        key: 'profile',
        icon: <UserOutlined />,
        label: `${user?.nickname || user?.username || ''} (${user?.role === 'admin' ? '管理员' : '用户'})`,
        disabled: true,
      },
      { type: 'divider' as const },
      {
        key: 'language',
        label: <LanguageSwitch />,
      },
      {
        key: 'theme',
        label: <ThemeSwitch />,
      },
      { type: 'divider' as const },
      {
        key: 'password',
        icon: <SettingOutlined />,
        label: '修改密码',
      },
      {
        key: 'logout',
        icon: <LogoutOutlined />,
        label: t('nav.logout'),
        danger: true,
      },
    ],
    onClick: ({ key }: { key: string }) => {
      if (key === 'logout') {
        Modal.confirm({
          title: t('auth.logoutConfirm'),
          okText: t('common.ok'),
          cancelText: t('common.cancel'),
          onOk: async () => {
            await logout()
            navigate('/login', { replace: true })
          },
        })
      } else if (key === 'password') {
        navigate('/settings?tab=security')
      }
    },
  }

  return (
    <Layout className="or-layout">
      <Sider
        className="or-sider or-glass"
        theme="light"
        collapsible
        collapsed={collapsed}
        onCollapse={toggleCollapsed}
        trigger={null}
        width={208}
        collapsedWidth={56}
      >
        <div className="or-logo">
          {logo ? (
            <img src={logo} alt={siteName} />
          ) : (
            <GlobalOutlined style={{ fontSize: 20, color: 'var(--or-primary)' }} />
          )}
          {!collapsed && <span>{siteName}</span>}
        </div>
        <Menu
          mode="inline"
          selectedKeys={[selectedKey]}
          items={menuItems}
          onClick={({ key }) => navigate(key)}
          style={{ borderInlineEnd: 'none', background: 'transparent' }}
        />
      </Sider>

      <Layout>
        <Header
          className="or-header or-glass"
          style={{ background: 'var(--or-bg-header)', paddingInline: 16 }}
        >
          <Space size={12}>
            {collapsed ? (
              <MenuUnfoldOutlined
                onClick={toggleCollapsed}
                style={{ cursor: 'pointer', fontSize: 16 }}
              />
            ) : (
              <MenuFoldOutlined
                onClick={toggleCollapsed}
                style={{ cursor: 'pointer', fontSize: 16 }}
              />
            )}
          </Space>

          <Space size={16} align="center">
            <Tooltip title={wsStatus === 'open' ? '实时推送已连接' : '实时推送未连接（状态可能延迟）'}>
              <Badge
                status={wsStatus === 'open' ? 'success' : 'default'}
                text={<span style={{ fontSize: 12, color: token.colorTextTertiary }}>实时</span>}
              />
            </Tooltip>

            {onlineNodes !== null && (
              <Badge
                count={onlineNodes}
                showZero
                color="var(--or-success)"
                overflowCount={9999}
                offset={[4, -2]}
              >
                <Tooltip title={t('traffic.onlineNodes')}>
                  <CloudServerOutlined style={{ fontSize: 16 }} />
                </Tooltip>
              </Badge>
            )}

            <Tooltip title={t('common.refresh')}>
              <ReloadOutlined
                style={{ fontSize: 16, cursor: 'pointer' }}
                onClick={() => window.location.reload()}
              />
            </Tooltip>

            <Dropdown menu={userMenu} placement="bottomRight">
              <Space style={{ cursor: 'pointer' }} size={8}>
                <Avatar size={28} style={{ background: 'var(--or-primary)' }}>
                  {(user?.nickname || user?.username || 'U').slice(0, 1).toUpperCase()}
                </Avatar>
                <Typography.Text style={{ fontSize: 13 }}>
                  {user?.nickname || user?.username}
                </Typography.Text>
              </Space>
            </Dropdown>
          </Space>
        </Header>

        <Content className="or-content">
          <Outlet />
        </Content>
      </Layout>
    </Layout>
  )
}
