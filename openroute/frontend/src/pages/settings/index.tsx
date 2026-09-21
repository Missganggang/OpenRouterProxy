/**
 * 设置页面（规格书 9.2「设置」、8.14）。
 *
 * 六个 Tab：
 *  基础设置 —— 站点名 / Logo / favicon / 公告 / 主题 / 语言
 *  安全设置 —— 修改自己的密码 + API Token 管理
 *  通知设置 —— Webhook 地址与测试
 *  系统信息 —— info / status / version + 错误码字典
 *  审计日志 —— 可筛选的操作审计，展开行展示 before / after
 *  任务     —— 后台定时任务列表与手动触发
 *
 * 支持 `?tab=security` 深链（顶栏的「修改密码」入口就是这么跳的），
 * Tab 切换会把参数写回 URL，刷新后停留在同一个 Tab。
 */
import { useCallback, useEffect, useMemo, useState } from 'react'
import {
  Alert,
  App,
  Badge,
  Button,
  Card,
  Col,
  DatePicker,
  Descriptions,
  Empty,
  Form,
  Input,
  Modal,
  Row,
  Select,
  Space,
  Table,
  Tabs,
  Tag,
  Tooltip,
  Typography,
} from 'antd'
import type { ColumnsType } from 'antd/es/table'
import {
  CheckCircleOutlined,
  CloseCircleOutlined,
  DeleteOutlined,
  KeyOutlined,
  LockOutlined,
  PlusOutlined,
  ReloadOutlined,
  SearchOutlined,
  SendOutlined,
} from '@ant-design/icons'
import { useSearchParams } from 'react-router-dom'
import type { Dayjs } from 'dayjs'

import { authApi, settingApi, showApiError, systemApi } from '../../api'
import type { ErrorDictEntry } from '../../api/modules/system'
import type { APIToken, AuditLog, SystemInfo, SystemStatus, TaskStatus } from '../../api/types'
import { useI18n, type Lang } from '../../locales'
import { useAuthStore } from '../../store/auth'
import { useThemeStore, type ThemeName } from '../../store/theme'
import CopyableCommand from '../../components/CopyableCommand'

const { Text, Paragraph } = Typography
const { RangePicker } = DatePicker

/** 合法 Tab 键，用于校验 URL 上的 ?tab= 参数。 */
const TAB_KEYS = ['general', 'security', 'notification', 'system', 'audit', 'tasks'] as const
type TabKey = (typeof TAB_KEYS)[number]

/** 基础设置表单值。 */
interface GeneralFormValues {
  site_name: string
  logo: string
  favicon: string
  announcement: string
  theme: ThemeName
  lang: Lang
}

/** 修改密码表单值。 */
interface PasswordFormValues {
  old_password: string
  new_password: string
  confirm_password: string
}

/** 创建令牌表单值。 */
interface TokenFormValues {
  name: string
  scopes: string[]
  ip_whitelist?: string[]
  expire_at?: Dayjs | null
}

/** 从 URL 参数解析 Tab 键，非法值一律回落到「基础设置」。 */
function parseTab(value: string | null): TabKey {
  return TAB_KEYS.includes(value as TabKey) ? (value as TabKey) : 'general'
}

export default function SettingsPage() {
  const { t, lang, setLang } = useI18n()
  // message 由各 Tab 组件各自通过 App.useApp() 获取（每个 Tab 独立提示自己的结果），
  // 这里不需要再取一份。
  const [searchParams, setSearchParams] = useSearchParams()

  /**
   * 当前 Tab 以 URL 的 ?tab= 为主源，本地状态兜底。
   *
   * 兜底是必要的：顶栏的「修改密码」入口执行 navigate('/settings?tab=security')，
   * 若用户已经停留在 /settings，只有搜索参数变化，必须保证两种来源都能生效。
   */
  const [localTab, setLocalTab] = useState<TabKey>(() => parseTab(searchParams.get('tab')))
  const hasTabParam = searchParams.get('tab') !== null
  const activeTab: TabKey = hasTabParam ? parseTab(searchParams.get('tab')) : localTab

  /** 切换 Tab：同时写回 URL，保证刷新与分享链接后仍在同一页。 */
  const onTabChange = (key: string) => {
    setLocalTab(key as TabKey)
    const next = new URLSearchParams(searchParams)
    next.set('tab', key)
    setSearchParams(next, { replace: true })
  }

  return (
    <div>
      <div className="or-page-header">
        <div>
          <h2 className="or-page-title">{t('settings.title')}</h2>
          <p className="or-page-desc">{t('settings.pageDesc')}</p>
        </div>
      </div>

      <Tabs
        activeKey={activeTab}
        onChange={onTabChange}
        destroyInactiveTabPane={false}
        items={[
          { key: 'general', label: t('settings.tabGeneral'), children: <GeneralTab t={t} lang={lang} setLang={setLang} /> },
          { key: 'security', label: t('settings.tabSecurity'), children: <SecurityTab t={t} /> },
          { key: 'notification', label: t('settings.tabNotification'), children: <NotificationTab t={t} /> },
          { key: 'system', label: t('settings.tabSystem'), children: <SystemTab t={t} /> },
          { key: 'audit', label: t('settings.tabAudit'), children: <AuditTab t={t} /> },
          { key: 'tasks', label: t('settings.tabTasks'), children: <TasksTab t={t} /> },
        ]}
      />
    </div>
  )
}

/* ═══════════════════════ 基础设置 ═══════════════════════ */

interface TabProps {
  t: (key: string, vars?: Record<string, string | number>) => string
}

function GeneralTab({ t, lang, setLang }: TabProps & { lang: Lang; setLang: (l: Lang) => void }) {
  const { message } = App.useApp()
  const [form] = Form.useForm<GeneralFormValues>()
  const [loading, setLoading] = useState(false)
  const [saving, setSaving] = useState(false)

  const siteName = useThemeStore((s) => s.site_name)
  const theme = useThemeStore((s) => s.theme)
  const logo = useThemeStore((s) => s.logo)
  const favicon = useThemeStore((s) => s.favicon)
  const announcement = useThemeStore((s) => s.announcement)
  const setTheme = useThemeStore((s) => s.setTheme)
  const patchSiteConfig = useThemeStore((s) => s.patchSiteConfig)

  /** 把后端返回的设置回填到表单。 */
  const load = useCallback(async () => {
    setLoading(true)
    try {
      const settings = await settingApi.getAll()
      form.setFieldsValue({
        site_name: asString(settings.site_name) || siteName,
        logo: asString(settings.logo),
        favicon: asString(settings.favicon),
        announcement: asString(settings.announcement),
        theme: normalizeTheme(settings.theme),
        lang,
      })
    } catch (err) {
      showApiError(err, t('settings.loadFailed'))
    } finally {
      setLoading(false)
    }
    // 只在挂载时拉一次；后续以本地 store 为准。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [form, t])

  useEffect(() => {
    void load()
  }, [load])

  /** 保存基础设置。
   *
   *  顺序：先调后端，成功后立刻 patch 本地站点配置，
   *  这样侧边栏标题 / favicon 无需刷新就能更新（规格书 9.3「切换无需重新加载」）。
   */
  const save = async () => {
    let values: GeneralFormValues
    try {
      values = await form.validateFields()
    } catch {
      return
    }
    setSaving(true)
    try {
      await settingApi.update({
        site_name: values.site_name?.trim() ?? '',
        logo: values.logo?.trim() ?? '',
        favicon: values.favicon?.trim() ?? '',
        announcement: values.announcement ?? '',
        theme: values.theme,
      })
      patchSiteConfig({
        site_name: values.site_name?.trim() || siteName,
        logo: values.logo?.trim() ?? '',
        favicon: values.favicon?.trim() ?? '',
        announcement: values.announcement ?? '',
        theme: values.theme,
      })
      // 主题走 store 的方法，保证 <html> class 与 localStorage 同时更新。
      if (values.theme !== theme) {
        await setTheme(values.theme)
      }
      if (values.lang !== lang) {
        setLang(values.lang)
      }
      message.success(t('settings.saved'))
    } catch (err) {
      showApiError(err, t('settings.saveFailed'))
    } finally {
      setSaving(false)
    }
  }

  return (
    <Card size="small" title={t('settings.basicSection')} loading={loading}>
      <Form
        form={form}
        layout="vertical"
        style={{ maxWidth: 720 }}
        initialValues={{
          site_name: siteName,
          logo,
          favicon,
          announcement,
          theme,
          lang,
        }}
      >
        <Form.Item
          name="site_name"
          label={t('settings.siteName')}
          extra={<Text type="secondary" style={{ fontSize: 12 }}>{t('settings.siteNameHint')}</Text>}
        >
          <Input placeholder={t('settings.siteNamePlaceholder')} maxLength={64} />
        </Form.Item>

        <Row gutter={16}>
          <Col xs={24} md={12}>
            <Form.Item
              name="logo"
              label={t('settings.logo')}
              extra={<Text type="secondary" style={{ fontSize: 12 }}>{t('settings.logoHint')}</Text>}
            >
              <Input className="or-mono" placeholder={t('settings.logoPlaceholder')} />
            </Form.Item>
          </Col>
          <Col xs={24} md={12}>
            <Form.Item
              name="favicon"
              label={t('settings.favicon')}
              extra={<Text type="secondary" style={{ fontSize: 12 }}>{t('settings.faviconHint')}</Text>}
            >
              <Input className="or-mono" placeholder={t('settings.faviconPlaceholder')} />
            </Form.Item>
          </Col>
        </Row>

        <Form.Item
          name="announcement"
          label={t('settings.announcement')}
          extra={<Text type="secondary" style={{ fontSize: 12 }}>{t('settings.announcementHint')}</Text>}
        >
          <Input.TextArea
            rows={4}
            maxLength={500}
            showCount
            placeholder={t('settings.announcementPlaceholder')}
          />
        </Form.Item>

        <Row gutter={16}>
          <Col xs={24} md={12}>
            <Form.Item
              name="theme"
              label={t('settings.theme')}
              extra={<Text type="secondary" style={{ fontSize: 12 }}>{t('settings.themeHint')}</Text>}
            >
              <Select
                onChange={(v: ThemeName) => void setTheme(v)}
                options={[
                  { label: t('settings.theme.classic'), value: 'classic' },
                  { label: t('settings.theme.transparent'), value: 'transparent' },
                ]}
              />
            </Form.Item>
          </Col>
          <Col xs={24} md={12}>
            <Form.Item
              name="lang"
              label={t('settings.language')}
              extra={<Text type="secondary" style={{ fontSize: 12 }}>{t('settings.languageHint')}</Text>}
            >
              <Select
                options={[
                  { label: '简体中文', value: 'zh-CN' },
                  { label: 'English', value: 'en-US' },
                ]}
              />
            </Form.Item>
          </Col>
        </Row>

        <Space>
          <Button type="primary" loading={saving} onClick={() => void save()}>
            {t('common.save')}
          </Button>
          <Button icon={<ReloadOutlined />} onClick={() => void load()}>
            {t('common.reset')}
          </Button>
        </Space>
      </Form>

      <Alert
        type="info"
        showIcon
        style={{ marginTop: 16, maxWidth: 720 }}
        message={`${t('settings.announcement')}: ${announcement || t('settings.announcementEmpty')}`}
        description={
          <Text type="secondary" style={{ fontSize: 12 }}>
            {t('settings.announcementHint')}
          </Text>
        }
      />
    </Card>
  )
}

/* ═══════════════════════ 安全设置 ═══════════════════════ */

function SecurityTab({ t }: TabProps) {
  const { message } = App.useApp()
  const user = useAuthStore((s) => s.user)
  const [form] = Form.useForm<PasswordFormValues>()
  const [changing, setChanging] = useState(false)

  const changePassword = async () => {
    let values: PasswordFormValues
    try {
      values = await form.validateFields()
    } catch {
      return
    }
    setChanging(true)
    try {
      await authApi.changePassword(values.old_password, values.new_password)
      message.success(t('settings.passwordChanged'))
      form.resetFields()
    } catch (err) {
      showApiError(err, t('settings.passwordFailed'))
    } finally {
      setChanging(false)
    }
  }

  return (
    <Space direction="vertical" size={12} style={{ width: '100%' }}>
      <Card size="small" title={t('settings.changePassword')}>
        <Row gutter={24}>
          <Col xs={24} md={12}>
            <Form form={form} layout="vertical" autoComplete="off">
              <Form.Item label={t('settings.currentUser')}>
                <Text className="or-mono">{user?.username ?? '-'}</Text>
              </Form.Item>
              <Form.Item
                name="old_password"
                label={t('settings.oldPassword')}
                rules={[{ required: true, message: t('settings.oldPasswordRequired') }]}
              >
                <Input.Password autoComplete="current-password" />
              </Form.Item>
              <Form.Item
                name="new_password"
                label={t('settings.newPassword')}
                rules={[
                  { required: true, message: t('settings.newPasswordRequired') },
                  { min: 8, max: 64, message: t('settings.passwordLength') },
                ]}
              >
                <Input.Password autoComplete="new-password" />
              </Form.Item>
              <Form.Item
                name="confirm_password"
                label={t('settings.confirmPassword')}
                dependencies={['new_password']}
                rules={[
                  { required: true, message: t('settings.confirmRequired') },
                  ({ getFieldValue }) => ({
                    validator(_, value) {
                      if (!value || getFieldValue('new_password') === value) {
                        return Promise.resolve()
                      }
                      return Promise.reject(new Error(t('settings.passwordMismatch')))
                    },
                  }),
                ]}
              >
                <Input.Password autoComplete="new-password" />
              </Form.Item>
              <Space>
                <Button
                  type="primary"
                  icon={<LockOutlined />}
                  loading={changing}
                  onClick={() => void changePassword()}
                >
                  {t('settings.changePassword')}
                </Button>
              </Space>
              <Paragraph type="secondary" style={{ fontSize: 12, marginTop: 8 }}>
                {t('settings.passwordHint')}
              </Paragraph>
            </Form>
          </Col>
        </Row>
      </Card>

      <ApiTokenPanel t={t} />
    </Space>
  )
}

/** API Token 管理：列表 + 创建（明文仅返回一次）+ 删除。 */
function ApiTokenPanel({ t }: TabProps) {
  const { message } = App.useApp()
  const [tokens, setTokens] = useState<APIToken[]>([])
  const [loading, setLoading] = useState(false)
  const [createOpen, setCreateOpen] = useState(false)
  const [creating, setCreating] = useState(false)
  /** 创建成功后拿到的明文令牌，关闭弹窗即丢弃。 */
  const [plainToken, setPlainToken] = useState<string | null>(null)
  const [form] = Form.useForm<TokenFormValues>()

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const res = await systemApi.apiTokens({ page: 1, page_size: 100 })
      setTokens(res.items)
    } catch (err) {
      showApiError(err, t('settings.loadFailed'))
    } finally {
      setLoading(false)
    }
  }, [t])

  useEffect(() => {
    void load()
  }, [load])

  const create = async () => {
    let values: TokenFormValues
    try {
      values = await form.validateFields()
    } catch {
      return
    }
    setCreating(true)
    try {
      const created = await systemApi.createAPIToken({
        name: values.name.trim(),
        scopes: values.scopes ?? [],
        ip_whitelist: values.ip_whitelist ?? [],
        expire_at: values.expire_at?.toISOString() ?? null,
      })
      setCreateOpen(false)
      form.resetFields()
      message.success(t('settings.tokenCreated'))
      // 明文只在响应里出现一次，必须立刻展示出来。
      if (created?.token) {
        setPlainToken(created.token)
      }
      await load()
    } catch (err) {
      showApiError(err, t('settings.tokenCreateFailed'))
    } finally {
      setCreating(false)
    }
  }

  const remove = (row: APIToken) => {
    Modal.confirm({
      title: t('settings.tokenDeleteConfirm', { name: row.name }),
      okText: t('common.delete'),
      okButtonProps: { danger: true },
      cancelText: t('common.cancel'),
      content: (
        <Space direction="vertical" size={6}>
          <Text>{t('settings.tokenDeleteImpact')}</Text>
          <Text type="secondary" style={{ fontSize: 12 }}>
            {t('settings.tokenScopes')}: {(row.scopes ?? []).join(', ') || t('common.none')}
          </Text>
        </Space>
      ),
      onOk: async () => {
        try {
          await systemApi.deleteAPIToken(row.id)
          message.success(t('settings.tokenDeleteSuccess'))
          await load()
        } catch (err) {
          showApiError(err, t('settings.tokenDeleteFailed'))
          throw err
        }
      },
    })
  }

  const columns: ColumnsType<APIToken> = [
    {
      title: t('settings.tokenName'),
      dataIndex: 'name',
      key: 'name',
      width: 180,
      render: (v: string, row) => (
        <Space direction="vertical" size={2}>
          <Space size={6}>
            <KeyOutlined style={{ color: 'var(--or-text-tertiary)' }} />
            <Text strong>{v}</Text>
          </Space>
          <Text type="secondary" style={{ fontSize: 12 }}>
            #{row.id}
          </Text>
        </Space>
      ),
    },
    {
      title: t('settings.tokenScopes'),
      dataIndex: 'scopes',
      key: 'scopes',
      width: 320,
      render: (v: string[]) => (
        <Space size={4} wrap>
          {(v ?? []).map((s) => (
            <Tag key={s} color="geekblue" className="or-mono">
              {s}
            </Tag>
          ))}
          {!v?.length && <Text type="secondary">{t('common.none')}</Text>}
        </Space>
      ),
    },
    {
      title: t('settings.tokenIpWhitelist'),
      dataIndex: 'ip_whitelist',
      key: 'ip_whitelist',
      width: 200,
      render: (v: string[]) => (
        <Space size={4} wrap>
          {(v ?? []).map((ip) => (
            <Tag key={ip} className="or-mono">
              {ip}
            </Tag>
          ))}
          {!v?.length && <Text type="secondary">{t('common.none')}</Text>}
        </Space>
      ),
    },
    {
      title: t('settings.tokenExpire'),
      dataIndex: 'expire_at',
      key: 'expire_at',
      width: 170,
      render: (v: string | null) =>
        v ? (
          <Text className="or-mono" style={{ fontSize: 12 }}>
            {formatTime(v)}
          </Text>
        ) : (
          <Text type="secondary">{t('settings.tokenExpireHint')}</Text>
        ),
    },
    {
      title: t('settings.tokenLastUsed'),
      dataIndex: 'last_used_at',
      key: 'last_used_at',
      width: 170,
      render: (v: string | null) =>
        v ? (
          <Text className="or-mono" style={{ fontSize: 12 }}>
            {formatTime(v)}
          </Text>
        ) : (
          <Text type="secondary">-</Text>
        ),
    },
    {
      title: t('common.status'),
      dataIndex: 'enabled',
      key: 'enabled',
      width: 110,
      render: (v: boolean) =>
        v ? (
          <Tag color="green" icon={<CheckCircleOutlined />}>
            {t('common.enabled')}
          </Tag>
        ) : (
          <Tag icon={<CloseCircleOutlined />}>{t('common.disabled')}</Tag>
        ),
    },
    {
      title: t('common.actions'),
      key: 'actions',
      width: 90,
      fixed: 'right',
      render: (_, row) => (
        <Tooltip title={t('common.delete')}>
          <Button size="small" type="text" danger icon={<DeleteOutlined />} onClick={() => remove(row)} />
        </Tooltip>
      ),
    },
  ]

  return (
    <Card
      size="small"
      title={t('settings.apiTokens')}
      extra={
        <Space size={12}>
          <Text type="secondary" style={{ fontSize: 12 }}>
            {t('settings.tokenCount', { n: tokens.length })}
          </Text>
          <Button
            type="primary"
            size="small"
            icon={<PlusOutlined />}
            onClick={() => {
              form.resetFields()
              setCreateOpen(true)
            }}
          >
            {t('settings.createToken')}
          </Button>
        </Space>
      }
    >
      <Paragraph type="secondary" style={{ fontSize: 12, margin: '0 0 8px' }}>
        {t('settings.apiTokensHint')}
      </Paragraph>

      <Table<APIToken>
        rowKey="id"
        size="small"
        loading={loading}
        columns={columns}
        dataSource={tokens}
        scroll={{ x: 1200 }}
        pagination={{ pageSize: 10, showTotal: (n) => t('common.total', { n }) }}
        expandable={{
          expandedRowRender: (row) => (
            <Space direction="vertical" size={4} style={{ fontSize: 12 }}>
              <Text type="secondary">
                {t('settings.tokenExpire')}: {row.expire_at ? formatTime(row.expire_at) : t('settings.tokenExpireHint')}
              </Text>
              <Text type="secondary">
                {t('common.createdAt')}: {formatTime(row.created_at)}
              </Text>
            </Space>
          ),
        }}
        locale={{
          emptyText: (
            <Empty
              className="or-empty"
              image={<KeyOutlined style={{ fontSize: 40, color: 'var(--or-text-disabled)' }} />}
              description={
                <Space direction="vertical" size={4}>
                  <Text>{t('settings.tokensEmpty')}</Text>
                  <Text type="secondary" style={{ fontSize: 12 }}>
                    {t('settings.tokensEmptyHint')}
                  </Text>
                </Space>
              }
            >
              <Button
                type="primary"
                icon={<PlusOutlined />}
                onClick={() => {
                  form.resetFields()
                  setCreateOpen(true)
                }}
              >
                {t('settings.createToken')}
              </Button>
            </Empty>
          ),
        }}
      />

      {/* 创建令牌 */}
      <Modal
        open={createOpen}
        width={640}
        title={t('settings.createToken')}
        okText={t('common.confirm')}
        cancelText={t('common.cancel')}
        confirmLoading={creating}
        onOk={() => void create()}
        onCancel={() => setCreateOpen(false)}
        destroyOnClose
      >
        <Form form={form} layout="vertical" autoComplete="off">
          <Form.Item
            name="name"
            label={t('settings.tokenName')}
            rules={[{ required: true, message: t('settings.tokenNameRequired') }]}
          >
            <Input maxLength={64} placeholder={t('settings.tokenNamePlaceholder')} />
          </Form.Item>
          <Form.Item
            name="scopes"
            label={t('settings.tokenScopes')}
            extra={<Text type="secondary" style={{ fontSize: 12 }}>{t('settings.scopesHint')}</Text>}
            rules={[{ required: true, message: t('settings.tokenScopesRequired') }]}
          >
            <Select
              mode="multiple"
              allowClear
              placeholder={t('settings.tokenScopes')}
              options={systemApi.ALL_SCOPES.map((s) => ({ label: s, value: s }))}
            />
          </Form.Item>
          <Form.Item name="expire_at" label={t('settings.tokenExpire')} extra={t('settings.tokenExpireHint')}
            rules={[{ validator: (_, value: Dayjs | null) => !value || value.valueOf() > Date.now() ? Promise.resolve() : Promise.reject(new Error('过期时间必须晚于当前时间')) }]}>
            <DatePicker showTime allowClear style={{ width: '100%' }} />
          </Form.Item>
          <Form.Item
            name="ip_whitelist"
            label={t('settings.tokenIpWhitelist')}
            extra={<Text type="secondary" style={{ fontSize: 12 }}>{t('settings.tokenIpWhitelistHint')}</Text>}
          >
            <Select
              mode="tags"
              allowClear
              open={false}
              tokenSeparators={[',', ' ']}
              placeholder={t('settings.tokenIpPlaceholder')}
            />
          </Form.Item>
        </Form>
      </Modal>

      {/* 明文令牌：仅此一次 */}
      <Modal
        open={Boolean(plainToken)}
        width={680}
        title={
          <Space size={6}>
            <KeyOutlined />
            {t('settings.tokenPlainOnce')}
          </Space>
        }
        footer={
          <Button type="primary" onClick={() => setPlainToken(null)}>
            {t('common.ok')}
          </Button>
        }
        onCancel={() => setPlainToken(null)}
        maskClosable={false}
      >
        <Space direction="vertical" size={10} style={{ width: '100%' }}>
          <Alert type="warning" showIcon message={t('settings.tokenPlainWarning')} />
          <CopyableCommand command={plainToken ?? ''} multiline showQrcode />
        </Space>
      </Modal>
    </Card>
  )
}

/* ═══════════════════════ 通知设置 ═══════════════════════ */

function NotificationTab({ t }: TabProps) {
  const { message } = App.useApp()
  const [form] = Form.useForm<{ webhook: string }>()
  const [loading, setLoading] = useState(false)
  const [saving, setSaving] = useState(false)
  const [testing, setTesting] = useState(false)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const settings = await settingApi.getAll()
      form.setFieldsValue({ webhook: asString(settings.webhook_url ?? settings.webhook) })
    } catch (err) {
      showApiError(err, t('settings.loadFailed'))
    } finally {
      setLoading(false)
    }
  }, [form, t])

  useEffect(() => {
    void load()
  }, [load])

  /** 测试 Webhook：结果按成功/失败分别提示（规格书 8.18）。 */
  const test = async () => {
    const url = form.getFieldValue('webhook') as string | undefined
    if (!url?.trim()) {
      message.warning(t('settings.testWebhookNeedUrl'))
      return
    }
    setTesting(true)
    try {
      const result = await systemApi.testWebhook(url.trim())
      if (result.ok) {
        message.success(t('settings.testWebhookOk', { status: result.status ?? 200 }))
      } else {
        message.error(t('settings.testWebhookFail', { error: result.error || `HTTP ${result.status ?? '-'}` }))
      }
    } catch (err) {
      showApiError(err, t('settings.testWebhookFailed'))
    } finally {
      setTesting(false)
    }
  }

  const save = async () => {
    let values: { webhook: string }
    try {
      values = await form.validateFields()
    } catch {
      return
    }
    setSaving(true)
    try {
      await settingApi.update({ webhook_url: values.webhook?.trim() ?? '' })
      message.success(t('settings.saved'))
    } catch (err) {
      showApiError(err, t('settings.saveFailed'))
    } finally {
      setSaving(false)
    }
  }

  return (
    <Card size="small" title={t('settings.notification')} loading={loading}>
      <Alert
        type="info"
        showIcon
        style={{ marginBottom: 16, maxWidth: 720 }}
        message={t('settings.webhookHint')}
        description={
          <Text type="secondary" style={{ fontSize: 12 }}>
            node.online / node.offline / rule.sync_failed / alert.fired / migration.finished / backup.finished
          </Text>
        }
      />
      <Form form={form} layout="vertical" style={{ maxWidth: 720 }}>
        <Form.Item
          name="webhook"
          label={t('settings.webhook')}
          rules={[{ type: 'url', message: t('settings.testWebhookNeedUrl') }]}
        >
          <Input className="or-mono" placeholder="https://example.com/hook" allowClear />
        </Form.Item>
        <Space>
          <Button type="primary" loading={saving} onClick={() => void save()}>
            {t('settings.saveWebhook')}
          </Button>
          <Button icon={<SendOutlined />} loading={testing} onClick={() => void test()}>
            {t('settings.testWebhook')}
          </Button>
        </Space>
      </Form>
    </Card>
  )
}

/* ═══════════════════════ 系统信息 ═══════════════════════ */

function SystemTab({ t }: TabProps) {
  const [info, setInfo] = useState<SystemInfo | null>(null)
  const [status, setStatus] = useState<SystemStatus | null>(null)
  const [versions, setVersions] = useState<{ backend: string; node_client: string; build: string } | null>(null)
  const [errors, setErrors] = useState<ErrorDictEntry[]>([])
  const [loading, setLoading] = useState(false)
  const [keyword, setKeyword] = useState('')

  const load = useCallback(async () => {
    setLoading(true)
    // 三个接口互不依赖，任何一个失败都不应影响其它区块的展示。
    const [infoRes, statusRes, versionRes, errorRes] = await Promise.allSettled([
      systemApi.info(),
      systemApi.status(),
      systemApi.version(),
      systemApi.errorDict(),
    ])
    if (infoRes.status === 'fulfilled') setInfo(infoRes.value)
    else showApiError(infoRes.reason, t('settings.sysInfoFail'))
    if (statusRes.status === 'fulfilled') setStatus(statusRes.value)
    else showApiError(statusRes.reason, t('settings.sysHealthFail'))
    if (versionRes.status === 'fulfilled') setVersions(versionRes.value)
    if (errorRes.status === 'fulfilled') setErrors(errorRes.value ?? [])
    setLoading(false)
  }, [t])

  useEffect(() => {
    void load()
  }, [load])

  const filteredErrors = useMemo(() => {
    const kw = keyword.trim().toLowerCase()
    if (!kw) return errors
    return errors.filter(
      (e) =>
        String(e.code).includes(kw) ||
        (e.message ?? '').toLowerCase().includes(kw) ||
        String(e.http_status).includes(kw),
    )
  }, [errors, keyword])

  return (
    <Space direction="vertical" size={12} style={{ width: '100%' }}>
      <Card
        size="small"
        title={t('settings.systemInfo')}
        loading={loading}
        extra={
          <Button size="small" icon={<ReloadOutlined />} onClick={() => void load()}>
            {t('common.refresh')}
          </Button>
        }
      >
        <Descriptions size="small" column={{ xs: 1, sm: 2, lg: 3 }} bordered>
          <Descriptions.Item label={t('settings.sysVersion')}>
            <Text className="or-mono">{info?.version ?? versions?.backend ?? '-'}</Text>
          </Descriptions.Item>
          <Descriptions.Item label={t('settings.sysBuild')}>
            <Text className="or-mono">{info?.build ?? versions?.build ?? '-'}</Text>
          </Descriptions.Item>
          <Descriptions.Item label={t('settings.nodeClient')}>
            <Text className="or-mono">{versions?.node_client ?? '-'}</Text>
          </Descriptions.Item>
          <Descriptions.Item label={t('settings.sysUptime')}>
            <Text className="or-mono">{formatUptime(info?.uptime ?? 0)}</Text>
          </Descriptions.Item>
          <Descriptions.Item label={t('settings.sysDatabase')}>
            <Text className="or-mono">{info?.database ?? '-'}</Text>
          </Descriptions.Item>
          <Descriptions.Item label={t('settings.sysSchemaVersion')}>
            <Text className="or-mono">{info?.schema_version ?? '-'}</Text>
          </Descriptions.Item>
          <Descriptions.Item label={t('settings.sysConfigVersion')}>
            <Text className="or-mono">{info?.config_version ?? '-'}</Text>
          </Descriptions.Item>
          <Descriptions.Item label={t('settings.sysNodes')}>
            <Text className="or-mono">
              {info ? `${info.node_count}（${t('settings.sysOnlineNodes')} ${info.online_node_count}）` : '-'}
            </Text>
          </Descriptions.Item>
          <Descriptions.Item label={t('settings.sysRules')}>
            <Text className="or-mono">{info?.rule_count ?? '-'}</Text>
          </Descriptions.Item>
          <Descriptions.Item label={t('settings.sysUsers')}>
            <Text className="or-mono">{info?.user_count ?? '-'}</Text>
          </Descriptions.Item>
          <Descriptions.Item label={t('settings.sysSubscribers')}>
            <Text className="or-mono">{info?.subscriber_count ?? '-'}</Text>
          </Descriptions.Item>
        </Descriptions>
      </Card>

      <Card size="small" title={t('settings.sysHealth')}>
        {!status?.checks?.length ? (
          <Text type="secondary">{t('common.none')}</Text>
        ) : (
          <Space direction="vertical" size={6} style={{ width: '100%' }}>
            <Space size={8}>
              {status.healthy ? (
                <Tag color="green" icon={<CheckCircleOutlined />}>
                  {t('settings.healthOk')}
                </Tag>
              ) : (
                <Tag color="red" icon={<CloseCircleOutlined />}>
                  {t('settings.healthBad')}
                </Tag>
              )}
            </Space>
            <Table<SystemStatus['checks'][number]>
              size="small"
              rowKey="name"
              pagination={false}
              dataSource={status.checks}
              columns={[
                { title: t('settings.taskName'), dataIndex: 'name', key: 'name', width: 220 },
                {
                  title: t('common.status'),
                  dataIndex: 'ok',
                  key: 'ok',
                  width: 120,
                  render: (v: boolean) =>
                    v ? (
                      <Tag color="green">{t('settings.healthOk')}</Tag>
                    ) : (
                      <Tag color="red">{t('settings.healthBad')}</Tag>
                    ),
                },
                {
                  title: t('settings.taskLastResult'),
                  dataIndex: 'message',
                  key: 'message',
                  render: (v: string) => <Text style={{ fontSize: 12 }}>{v || '-'}</Text>,
                },
              ]}
            />
          </Space>
        )}
      </Card>

      <Card
        size="small"
        title={t('settings.errorDict')}
        extra={
          <Input
            size="small"
            allowClear
            style={{ width: 240 }}
            prefix={<SearchOutlined />}
            placeholder={t('settings.errorSearch')}
            value={keyword}
            onChange={(e) => setKeyword(e.target.value)}
          />
        }
      >
        <Paragraph type="secondary" style={{ fontSize: 12, margin: '0 0 8px' }}>
          {t('settings.errorDictHint')}
        </Paragraph>
        <Table<ErrorDictEntry>
          size="small"
          rowKey="code"
          dataSource={filteredErrors}
          pagination={{ pageSize: 15, showTotal: (n) => t('common.total', { n }) }}
          scroll={{ y: 420 }}
          columns={[
            {
              title: t('settings.errorCode'),
              dataIndex: 'code',
              key: 'code',
              width: 120,
              render: (v: number) => <Text className="or-mono">{v}</Text>,
            },
            {
              title: t('settings.errorMessage'),
              dataIndex: 'message',
              key: 'message',
              render: (v: string) => <Text style={{ fontSize: 13 }}>{v}</Text>,
            },
            {
              title: t('settings.errorHttp'),
              dataIndex: 'http_status',
              key: 'http_status',
              width: 100,
              render: (v: number) => <Text className="or-mono">{v}</Text>,
            },
          ]}
          locale={{ emptyText: <Empty className="or-empty" description={t('settings.errorEmpty')} /> }}
        />
      </Card>
    </Space>
  )
}

/* ═══════════════════════ 审计日志 ═══════════════════════ */

function AuditTab({ t }: TabProps) {
  const [logs, setLogs] = useState<AuditLog[]>([])
  const [loading, setLoading] = useState(false)
  const [page, setPage] = useState(1)
  const [total, setTotal] = useState(0)

  /* 筛选条件 */
  const [action, setAction] = useState('')
  const [resource, setResource] = useState('')
  const [username, setUsername] = useState('')
  const [result, setResult] = useState<string>('')
  const [range, setRange] = useState<[Dayjs | null, Dayjs | null] | null>(null)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const res = await systemApi.auditLogs({
        page,
        page_size: 20,
        sort: 'created_at',
        order: 'desc',
        action: action.trim() || undefined,
        resource: resource.trim() || undefined,
        result: result || undefined,
        username: username.trim() || undefined,
        from: range?.[0]?.toISOString(),
        to: range?.[1]?.toISOString(),
      })
      setLogs(res.items)
      setTotal(res.pagination.total)
    } catch (err) {
      showApiError(err, t('settings.auditLoadFailed'))
    } finally {
      setLoading(false)
    }
  }, [action, page, range, resource, result, t, username])

  useEffect(() => {
    void load()
  }, [load])

  /** 重置筛选：同时把页码拉回第一页，避免停在空页。 */
  const reset = () => {
    setAction('')
    setResource('')
    setUsername('')
    setResult('')
    setRange(null)
    setPage(1)
  }

  const columns: ColumnsType<AuditLog> = [
    {
      title: t('settings.auditTime'),
      dataIndex: 'created_at',
      key: 'created_at',
      width: 175,
      render: (v: string) => (
        <Text className="or-mono" style={{ fontSize: 12 }}>
          {formatTime(v)}
        </Text>
      ),
    },
    {
      title: t('settings.auditUsername'),
      dataIndex: 'username',
      key: 'username',
      width: 130,
      render: (v: string) => <Text>{v || '-'}</Text>,
    },
    {
      title: t('settings.auditAction'),
      dataIndex: 'action',
      key: 'action',
      width: 110,
      render: (v: string) => <Tag color={actionColor(v)}>{v}</Tag>,
    },
    {
      title: t('settings.auditResource'),
      key: 'resource',
      width: 170,
      render: (_, row) => (
        <Text className="or-mono" style={{ fontSize: 12 }}>
          {row.resource}
          {row.resource_id ? ` #${row.resource_id}` : ''}
        </Text>
      ),
    },
    {
      title: t('settings.auditResult'),
      dataIndex: 'result',
      key: 'result',
      width: 100,
      render: (v: string) =>
        v === 'success' ? (
          <Tag color="green">{t('settings.auditSuccess')}</Tag>
        ) : (
          <Tag color="red">{t('settings.auditFailed')}</Tag>
        ),
    },
    {
      title: t('settings.auditIp'),
      dataIndex: 'ip',
      key: 'ip',
      width: 150,
      render: (v: string) => (
        <Text className="or-mono" style={{ fontSize: 12 }}>
          {v || '-'}
        </Text>
      ),
    },
    {
      title: t('settings.taskLastResult'),
      dataIndex: 'message',
      key: 'message',
      render: (v: string) => (
        <Text style={{ fontSize: 12 }} type={v ? undefined : 'secondary'}>
          {v || '-'}
        </Text>
      ),
    },
  ]

  return (
    <Card size="small">
      <div className="or-toolbar">
        <Input
          style={{ width: 180 }}
          allowClear
          placeholder={t('settings.auditAction')}
          value={action}
          onChange={(e) => {
            setAction(e.target.value)
            setPage(1)
          }}
        />
        <Input
          style={{ width: 200 }}
          allowClear
          placeholder={t('settings.resourcePlaceholder')}
          value={resource}
          onChange={(e) => {
            setResource(e.target.value)
            setPage(1)
          }}
        />
        <Input
          style={{ width: 180 }}
          allowClear
          placeholder={t('settings.usernamePlaceholder')}
          value={username}
          onChange={(e) => {
            setUsername(e.target.value)
            setPage(1)
          }}
        />
        <Select
          style={{ width: 140 }}
          value={result}
          onChange={(v) => {
            setResult(v)
            setPage(1)
          }}
          options={[
            { label: t('settings.auditAll'), value: '' },
            { label: t('settings.auditSuccess'), value: 'success' },
            { label: t('settings.auditFailed'), value: 'failed' },
          ]}
        />
        <RangePicker
          showTime
          value={range}
          onChange={(v) => {
            setRange(v as [Dayjs | null, Dayjs | null] | null)
            setPage(1)
          }}
        />
        <Button icon={<SearchOutlined />} type="primary" onClick={() => void load()}>
          {t('common.search')}
        </Button>
        <Button icon={<ReloadOutlined />} onClick={reset}>
          {t('common.reset')}
        </Button>
      </div>

      <Paragraph type="secondary" style={{ fontSize: 12, margin: '0 0 8px' }}>
        {t('settings.auditHint')}
      </Paragraph>

      <Table<AuditLog>
        rowKey="id"
        size="small"
        loading={loading}
        columns={columns}
        dataSource={logs}
        scroll={{ x: 1200 }}
        pagination={{
          current: page,
          pageSize: 20,
          total,
          onChange: setPage,
          showTotal: (n) => t('common.total', { n }),
        }}
        expandable={{
          expandedRowRender: (row) => <AuditDetail row={row} t={t} />,
          rowExpandable: (row) => Boolean(row.before) || Boolean(row.after),
        }}
        locale={{
          emptyText: (
            <Empty
              className="or-empty"
              description={
                <Space direction="vertical" size={4}>
                  <Text>{t('settings.auditEmpty')}</Text>
                  <Text type="secondary" style={{ fontSize: 12 }}>
                    {t('settings.auditEmptyHint')}
                  </Text>
                </Space>
              }
            />
          ),
        }}
      />
    </Card>
  )
}

/** 审计详情：左右分栏展示 before / after。 */
function AuditDetail({ row, t }: { row: AuditLog; t: TabProps['t'] }) {
  if (!row.before && !row.after) {
    return <Text type="secondary">{t('settings.auditNoDetail')}</Text>
  }
  return (
    <Space direction="vertical" size={6} style={{ width: '100%' }}>
      <Text strong style={{ fontSize: 13 }}>
        {t('settings.auditDetail')}
      </Text>
      <div style={{ display: 'flex', gap: 12, alignItems: 'flex-start' }}>
        <div style={{ flex: 1, minWidth: 0 }}>
          <Text type="secondary" style={{ fontSize: 12 }}>
            before
          </Text>
          <div className="or-diff" style={{ maxHeight: 260, marginTop: 4 }}>
            {prettyJson(row.before)
              .split('\n')
              .map((line, i) => (
                <div key={`before-${i}`} className="or-diff-line or-diff-del">
                  {line}
                </div>
              ))}
          </div>
        </div>
        <div style={{ flex: 1, minWidth: 0 }}>
          <Text type="secondary" style={{ fontSize: 12 }}>
            after
          </Text>
          <div className="or-diff" style={{ maxHeight: 260, marginTop: 4 }}>
            {prettyJson(row.after)
              .split('\n')
              .map((line, i) => (
                <div key={`after-${i}`} className="or-diff-line or-diff-add">
                  {line}
                </div>
              ))}
          </div>
        </div>
      </div>
    </Space>
  )
}

/* ═══════════════════════ 任务 ═══════════════════════ */

function TasksTab({ t }: TabProps) {
  const { message } = App.useApp()
  const [tasks, setTasks] = useState<TaskStatus[]>([])
  const [loading, setLoading] = useState(false)
  const [runningName, setRunningName] = useState<string | null>(null)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const res = await systemApi.tasks()
      setTasks(res ?? [])
    } catch (err) {
      showApiError(err, t('settings.tasksLoadFailed'))
    } finally {
      setLoading(false)
    }
  }, [t])

  useEffect(() => {
    void load()
  }, [load])

  /** 手动触发一次任务：先确认再执行，执行后刷新列表。 */
  const runNow = (row: TaskStatus) => {
    Modal.confirm({
      title: t('settings.taskRunConfirm', { name: row.name }),
      okText: t('settings.taskRunNow'),
      cancelText: t('common.cancel'),
      content: <Text>{t('settings.taskRunImpact')}</Text>,
      onOk: async () => {
        setRunningName(row.name)
        try {
          await systemApi.runTask(row.name)
          message.success(t('settings.taskRunDone'))
          await load()
        } catch (err) {
          showApiError(err, t('settings.taskRunFailed'))
          throw err
        } finally {
          setRunningName(null)
        }
      },
    })
  }

  const columns: ColumnsType<TaskStatus> = [
    {
      title: t('settings.taskName'),
      dataIndex: 'name',
      key: 'name',
      width: 200,
      render: (v: string) => (
        <Text className="or-mono" strong>
          {v}
        </Text>
      ),
    },
    {
      title: t('settings.taskDesc'),
      dataIndex: 'desc',
      key: 'desc',
      width: 280,
      render: (v: string) => <Text style={{ fontSize: 12 }}>{v || '-'}</Text>,
    },
    {
      title: t('settings.taskInterval'),
      dataIndex: 'interval_sec',
      key: 'interval_sec',
      width: 120,
      render: (v: number) => <Text className="or-mono">{formatInterval(v, t)}</Text>,
    },
    {
      title: t('common.status'),
      dataIndex: 'running',
      key: 'running',
      width: 110,
      render: (v: boolean, row) =>
        v || runningName === row.name ? (
          <Badge status="processing" text={t('settings.taskRunning')} />
        ) : (
          <Badge status="default" text={t('common.none')} />
        ),
    },
    {
      title: t('settings.taskLastRun'),
      dataIndex: 'last_run_at',
      key: 'last_run_at',
      width: 170,
      render: (v: string | null) =>
        v ? (
          <Text className="or-mono" style={{ fontSize: 12 }}>
            {formatTime(v)}
          </Text>
        ) : (
          <Text type="secondary">-</Text>
        ),
    },
    {
      title: t('settings.taskNextRun'),
      dataIndex: 'next_run_at',
      key: 'next_run_at',
      width: 170,
      render: (v: string | null) =>
        v ? (
          <Text className="or-mono" style={{ fontSize: 12 }}>
            {formatTime(v)}
          </Text>
        ) : (
          <Text type="secondary">-</Text>
        ),
    },
    {
      title: t('settings.taskLastResult'),
      dataIndex: 'last_result',
      key: 'last_result',
      width: 200,
      render: (v: string) => (
        <Tooltip title={v}>
          <Text style={{ fontSize: 12 }}>{v ? truncate(v, 24) : '-'}</Text>
        </Tooltip>
      ),
    },
    {
      title: t('settings.taskLastError'),
      dataIndex: 'last_error',
      key: 'last_error',
      width: 220,
      render: (v: string) =>
        v ? (
          <Tooltip title={v}>
            <Text type="danger" style={{ fontSize: 12 }}>
              {truncate(v, 26)}
            </Text>
          </Tooltip>
        ) : (
          <Text type="secondary">-</Text>
        ),
    },
    {
      title: t('common.actions'),
      key: 'actions',
      width: 130,
      fixed: 'right',
      render: (_, row) => (
        <Button
          size="small"
          icon={<SendOutlined />}
          loading={runningName === row.name}
          onClick={() => runNow(row)}
        >
          {t('settings.taskRunNow')}
        </Button>
      ),
    },
  ]

  return (
    <Card
      size="small"
      extra={
        <Button size="small" icon={<ReloadOutlined />} onClick={() => void load()}>
          {t('common.refresh')}
        </Button>
      }
    >
      <Paragraph type="secondary" style={{ fontSize: 12, margin: '0 0 8px' }}>
        {t('settings.tasksHint')}
      </Paragraph>
      <Table<TaskStatus>
        rowKey="name"
        size="small"
        loading={loading}
        columns={columns}
        dataSource={tasks}
        scroll={{ x: 1600 }}
        pagination={{ pageSize: 20, showTotal: (n) => t('common.total', { n }) }}
        locale={{
          emptyText: (
            <Empty
              className="or-empty"
              description={
                <Space direction="vertical" size={4}>
                  <Text>{t('settings.tasksEmpty')}</Text>
                  <Text type="secondary" style={{ fontSize: 12 }}>
                    {t('settings.tasksEmptyHint')}
                  </Text>
                </Space>
              }
            />
          ),
        }}
      />
    </Card>
  )
}

/* ═══════════════════════ 工具 ═══════════════════════ */

/** 任意设置值 → 字符串，null/undefined 归空串。 */
function asString(value: unknown): string {
  if (value === null || value === undefined) return ''
  return typeof value === 'string' ? value : String(value)
}

/** 主题值归一化。 */
function normalizeTheme(value: unknown): ThemeName {
  return value === 'transparent' ? 'transparent' : 'classic'
}

/** RFC3339 → 本地时间。 */
function formatTime(iso: string): string {
  if (!iso) return '-'
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString()
}

/** 秒 → 可读时长。 */
function formatInterval(sec: number, t: TabProps['t']): string {
  if (!sec || sec <= 0) return t('settings.na')
  if (sec < 60) return `${sec}s`
  if (sec < 3600) return `${Math.round(sec / 60)} min`
  return `${(sec / 3600).toFixed(1)} h`
}

/** 秒 → 「3d 4h」形式；不足一分钟时按秒展示。 */
function formatUptime(sec: number): string {
  if (!sec || sec <= 0) return '-'
  const days = Math.floor(sec / 86400)
  const hours = Math.floor((sec % 86400) / 3600)
  const minutes = Math.floor((sec % 3600) / 60)
  if (days > 0) return `${days}d ${hours}h`
  if (hours > 0) return `${hours}h ${minutes}m`
  if (minutes > 0) return `${minutes}m`
  return `${Math.floor(sec)}s`
}

/** 审计动作配色。 */
function actionColor(action: string): string {
  const map: Record<string, string> = {
    create: 'green',
    update: 'blue',
    delete: 'red',
    login: 'purple',
    sync: 'cyan',
    migrate: 'orange',
  }
  return map[action] ?? 'default'
}

/** 截断长文本。 */
function truncate(text: string, keep: number): string {
  if (!text) return '-'
  return text.length <= keep ? text : `${text.slice(0, keep)}…`
}

/** 任意值 → 缩进 JSON 文本，用于 before / after 展示。 */
function prettyJson(value: unknown): string {
  if (value === null || value === undefined) return '-'
  if (typeof value === 'string') {
    // 后端可能把 JSON 存成字符串，尝试再解析一层。
    try {
      return JSON.stringify(JSON.parse(value), null, 2)
    } catch {
      return value
    }
  }
  try {
    return JSON.stringify(value, null, 2)
  } catch {
    return String(value)
  }
}
