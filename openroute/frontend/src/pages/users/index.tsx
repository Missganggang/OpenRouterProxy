/**
 * 用户管理页面（规格书 9.2、8.11）。
 *
 * 面板没有注册入口：管理员手工建号（规格书 1.4 保留用户表、去掉注册/充值/到期计费）。
 * 订阅链接没有独立页面，放在用户详情抽屉里，通过 CopyableCommand 展示与复制。
 */
import { useCallback, useEffect, useState } from 'react'
import {
  Alert,
  Button,
  Card,
  Col,
  Descriptions,
  Drawer,
  Empty,
  Form,
  Input,
  InputNumber,
  Modal,
  Row,
  Select,
  Space,
  Switch,
  Table,
  Tabs,
  Tag,
  Tooltip,
  Typography,
  message,
} from 'antd'
import type { ColumnsType } from 'antd/es/table'
import {
  DeleteOutlined,
  EditOutlined,
  ExclamationCircleOutlined,
  KeyOutlined,
  PlusOutlined,
  ProfileOutlined,
  ReloadOutlined,
} from '@ant-design/icons'

import { showApiError, userApi } from '../../api'
import type { ForwardRule, User, UserGroup, UserTraffic } from '../../api/types'
import CopyableCommand from '../../components/CopyableCommand'
import SyncStatusTag from '../../components/SyncStatusTag'
import { useI18n } from '../../locales'

const { Text, Paragraph } = Typography

/** 用户表单值。 */
interface UserFormValues {
  username: string
  password?: string
  nickname?: string
  role: 'admin' | 'user'
  group_id?: number
  traffic_limit: number
  speed_limit: number
  ip_limit: number
  device_limit: number
  conn_limit: number
  status: boolean
  remark?: string
}

export default function UsersPage() {
  const { t } = useI18n()

  const [users, setUsers] = useState<User[]>([])
  const [groups, setGroups] = useState<UserGroup[]>([])
  const [loading, setLoading] = useState(false)
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)
  const [total, setTotal] = useState(0)
  const [keyword, setKeyword] = useState('')
  const [groupFilter, setGroupFilter] = useState<number | undefined>()

  const [editorOpen, setEditorOpen] = useState(false)
  const [editing, setEditing] = useState<User | null>(null)
  const [saving, setSaving] = useState(false)
  const [form] = Form.useForm<UserFormValues>()

  /** 详情抽屉：该用户的规则与流量 + 订阅链接。 */
  const [detailUser, setDetailUser] = useState<User | null>(null)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const res = await userApi.list({
        page,
        page_size: pageSize,
        keyword: keyword || undefined,
        group_id: groupFilter,
      })
      setUsers(res.items)
      setTotal(res.pagination.total)
    } catch (err) {
      showApiError(err, t('user.loadFailed'))
    } finally {
      setLoading(false)
    }
  }, [page, pageSize, keyword, groupFilter, t])

  const loadGroups = useCallback(async () => {
    try {
      const res = await userApi.groups({ page: 1, page_size: 200 })
      setGroups(res.items)
    } catch {
      // 分组只是为了展示与筛选，失败不阻断页面。
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  useEffect(() => {
    void loadGroups()
  }, [loadGroups])

  const groupName = (id: number | undefined) =>
    groups.find((g) => g.id === id)?.name ?? (id ? `#${id}` : '-')

  /* ───────────────────── 创建 / 编辑 ───────────────────── */

  const openCreate = () => {
    setEditing(null)
    form.resetFields()
    form.setFieldsValue({
      role: 'user',
      traffic_limit: 0,
      speed_limit: 0,
      ip_limit: 0,
      device_limit: 0,
      conn_limit: 0,
      status: true,
    })
    setEditorOpen(true)
  }

  const openEdit = (u: User) => {
    setEditing(u)
    form.setFieldsValue({
      username: u.username,
      nickname: u.nickname,
      role: u.role,
      group_id: u.group_id || undefined,
      traffic_limit: u.traffic_limit,
      speed_limit: u.speed_limit,
      ip_limit: u.ip_limit,
      device_limit: u.device_limit,
      conn_limit: u.conn_limit,
      status: u.status === 1,
      remark: u.remark,
    })
    setEditorOpen(true)
  }

  const submit = async () => {
    let values: UserFormValues
    try {
      values = await form.validateFields()
    } catch {
      return
    }
    setSaving(true)
    try {
      // 表单里的 status 是「启用」开关（boolean），接口需要 1/0，这里统一转换。
      const statusValue: 0 | 1 = values.status ? 1 : 0

      if (editing) {
        // 编辑时不通过此接口改密码（有独立的重置密码流程）。
        const { password: _pwd, ...rest } = values
        void _pwd
        await userApi.update(editing.id, {
          ...rest,
          status: statusValue,
        })
        message.success(t('user.updateSuccess'))
      } else {
        const created = await userApi.create({
          ...values,
          status: statusValue,
        })
        message.success(t('user.createSuccess'))
        // 后端可能一次性回传明文密码，直接展示便于交付给用户。
        if (created?.password) {
          showOneTimeSecret(t('user.initialPassword'), created.password, created.username, t)
        }
      }
      setEditorOpen(false)
      setEditing(null)
      await load()
    } catch (err) {
      showApiError(err, t('user.saveFailed'))
    } finally {
      setSaving(false)
    }
  }

  /* ───────────────────── 危险操作 ───────────────────── */

  const resetPassword = (u: User) => {
    Modal.confirm({
      title: t('user.resetPasswordConfirm', { name: u.username }),
      okText: t('user.resetPassword'),
      cancelText: t('common.cancel'),
      icon: <ExclamationCircleOutlined style={{ color: 'var(--or-warning)' }} />,
      content: (
        <Space direction="vertical" size={6}>
          <Text>{t('user.resetPasswordImpact')}</Text>
          <Text type="secondary" style={{ fontSize: 12 }}>
            {t('user.resetPasswordNote')}
          </Text>
        </Space>
      ),
      onOk: async () => {
        try {
          const res = await userApi.resetPassword(u.id)
          showOneTimeSecret(t('user.newPassword'), res.password, u.username, t)
          await load()
        } catch (err) {
          showApiError(err, t('user.resetPasswordFailed'))
          throw err
        }
      },
    })
  }

  const resetToken = (u: User) => {
    Modal.confirm({
      title: t('user.resetTokenConfirm', { name: u.username }),
      okText: t('user.resetToken'),
      cancelText: t('common.cancel'),
      icon: <ExclamationCircleOutlined style={{ color: 'var(--or-warning)' }} />,
      content: (
        <Space direction="vertical" size={6}>
          <Text>{t('user.resetTokenImpact')}</Text>
          <Text type="secondary" style={{ fontSize: 12 }}>
            {t('user.resetTokenNote')}
          </Text>
        </Space>
      ),
      onOk: async () => {
        try {
          const res = await userApi.resetToken(u.id)
          showOneTimeSecret(t('user.newToken'), res.token, u.username, t)
          await load()
        } catch (err) {
          showApiError(err, t('user.resetTokenFailed'))
          throw err
        }
      },
    })
  }

  const toggleStatus = (u: User, next: boolean) => {
    if (next) {
      Modal.confirm({
        title: t('user.enableConfirm', { name: u.username }),
        okText: t('common.enable'),
        cancelText: t('common.cancel'),
        onOk: async () => {
          try {
            // 启用走普通更新接口（后端只在禁用时有独立端点）。
            await userApi.update(u.id, { status: 1 })
            message.success(t('user.enabledToast'))
            await load()
          } catch (err) {
            showApiError(err, t('user.opFailed'))
            throw err
          }
        },
      })
      return
    }
    Modal.confirm({
      title: t('user.disableConfirm', { name: u.username }),
      okText: t('common.disable'),
      okButtonProps: { danger: true },
      cancelText: t('common.cancel'),
      content: (
        <Space direction="vertical" size={6}>
          <Text>{t('user.disableImpact')}</Text>
          <Text type="secondary" style={{ fontSize: 12 }}>
            {t('user.disableImpactDetail')}
          </Text>
        </Space>
      ),
      onOk: async () => {
        try {
          await userApi.disable(u.id)
          message.success(t('user.disabledToast'))
          await load()
        } catch (err) {
          showApiError(err, t('user.opFailed'))
          throw err
        }
      },
    })
  }

  const remove = (u: User) => {
    Modal.confirm({
      title: t('user.deleteConfirm', { name: u.username }),
      okText: t('common.delete'),
      okButtonProps: { danger: true },
      cancelText: t('common.cancel'),
      icon: <ExclamationCircleOutlined style={{ color: 'var(--or-error)' }} />,
      width: 520,
      content: (
        <Space direction="vertical" size={6}>
          <Text>{t('user.deleteImpact')}</Text>
          <Text type="secondary" style={{ fontSize: 12 }}>
            {t('user.deleteImpactDetail')}
          </Text>
        </Space>
      ),
      onOk: async () => {
        try {
          await userApi.remove(u.id)
          message.success(t('user.deleteSuccess'))
          await load()
        } catch (err) {
          showApiError(err, t('user.deleteFailed'))
          throw err
        }
      },
    })
  }

  const columns: ColumnsType<User> = [
    {
      title: t('auth.username'),
      dataIndex: 'username',
      key: 'username',
      width: 190,
      fixed: 'left',
      render: (v: string, row) => (
        <Space direction="vertical" size={2}>
          <Space size={4} wrap>
            <Text strong>{v}</Text>
            {row.role === 'admin' && <Tag color="gold">{t('user.admin')}</Tag>}
            {row.status === 0 && <Tag color="default">{t('common.disabled')}</Tag>}
            {/* 规格书 7.4：哈希不兼容的用户迁移后强制改密 */}
            {row.password_reset_required && (
              <Tooltip title={t('user.passwordResetRequiredHint')}>
                <Tag color="orange">{t('user.passwordResetRequired')}</Tag>
              </Tooltip>
            )}
            {/* 规格书 7.2：迁移后节点需重装 */}
            {row.pending_reinstall && (
              <Tooltip title={t('user.pendingReinstallHint')}>
                <Tag color="volcano">{t('user.pendingReinstall')}</Tag>
              </Tooltip>
            )}
          </Space>
          {row.nickname ? (
            <Text type="secondary" style={{ fontSize: 12 }}>
              {row.nickname}
            </Text>
          ) : null}
        </Space>
      ),
    },
    {
      title: t('user.group'),
      dataIndex: 'group_id',
      key: 'group_id',
      width: 140,
      render: (v: number) => <Text>{groupName(v)}</Text>,
    },
    {
      title: t('user.trafficUsed'),
      key: 'traffic',
      width: 180,
      render: (_, row) => (
        <Space direction="vertical" size={0}>
          <Text className="or-mono" style={{ fontSize: 12 }}>
            {formatBytes(row.traffic_used)}
            {row.traffic_limit > 0 ? ` / ${formatBytes(row.traffic_limit)}` : ''}
          </Text>
          {row.traffic_limit > 0 && (
            <Text type="secondary" style={{ fontSize: 11 }}>
              {t('user.remaining', {
                v: formatBytes(Math.max(0, row.traffic_limit - row.traffic_used)),
              })}
            </Text>
          )}
        </Space>
      ),
    },
    {
      title: t('user.speedLimit'),
      dataIndex: 'speed_limit',
      key: 'speed_limit',
      width: 120,
      render: (v: number) =>
        v > 0 ? <Text className="or-mono">{formatSpeed(v)}</Text> : <Text type="secondary">-</Text>,
    },
    {
      title: t('user.ipConnLimit'),
      key: 'limits',
      width: 150,
      render: (_, row) => (
        <Text className="or-mono" style={{ fontSize: 12 }}>
          {row.ip_limit || '-'} / {row.conn_limit || '-'} / {row.device_limit || '-'}
        </Text>
      ),
    },
    {
      title: t('user.expireAt'),
      dataIndex: 'expire_at',
      key: 'expire_at',
      width: 160,
      render: (v: string | null) =>
        v ? <Text>{formatTime(v)}</Text> : <Text type="secondary">{t('user.neverExpire')}</Text>,
    },
    {
      title: t('user.lastLogin'),
      key: 'last_login',
      width: 190,
      render: (_, row) => (
        <Space direction="vertical" size={0}>
          <Text style={{ fontSize: 12 }}>{row.last_login_at ? formatTime(row.last_login_at) : '-'}</Text>
          {row.last_login_ip && (
            <Text type="secondary" className="or-mono" style={{ fontSize: 11 }}>
              {row.last_login_ip}
            </Text>
          )}
        </Space>
      ),
    },
    {
      title: t('common.status'),
      key: 'status',
      width: 90,
      render: (_, row) => (
        <Switch
          size="small"
          checked={row.status === 1}
          onChange={(v) => toggleStatus(row, v)}
        />
      ),
    },
    {
      title: t('common.actions'),
      key: 'actions',
      width: 200,
      fixed: 'right',
      render: (_, row) => (
        <div className="or-actions">
          <Tooltip title={t('common.detail')}>
            <Button
              size="small"
              type="text"
              icon={<ProfileOutlined />}
              onClick={() => setDetailUser(row)}
            />
          </Tooltip>
          <Tooltip title={t('common.edit')}>
            <Button size="small" type="text" icon={<EditOutlined />} onClick={() => openEdit(row)} />
          </Tooltip>
          <Tooltip title={t('user.resetPassword')}>
            <Button
              size="small"
              type="text"
              icon={<KeyOutlined />}
              onClick={() => resetPassword(row)}
            />
          </Tooltip>
          <Tooltip title={t('user.resetToken')}>
            <Button
              size="small"
              type="text"
              icon={<ReloadOutlined />}
              onClick={() => resetToken(row)}
            />
          </Tooltip>
          <Tooltip title={t('common.delete')}>
            <Button
              size="small"
              type="text"
              danger
              icon={<DeleteOutlined />}
              onClick={() => remove(row)}
            />
          </Tooltip>
        </div>
      ),
    },
  ]

  return (
    <div>
      <div className="or-page-header">
        <div>
          <h2 className="or-page-title">{t('user.title')}</h2>
          <p className="or-page-desc">{t('user.pageDesc')}</p>
        </div>
        <Space>
          <Button icon={<ReloadOutlined />} onClick={() => void load()}>
            {t('common.refresh')}
          </Button>
          <Button type="primary" icon={<PlusOutlined />} onClick={openCreate}>
            {t('user.add')}
          </Button>
        </Space>
      </div>

      <Card size="small" style={{ marginBottom: 12 }}>
        <Space wrap>
          <Input.Search
            allowClear
            style={{ width: 240 }}
            placeholder={t('user.searchPlaceholder')}
            onSearch={(v) => {
              setPage(1)
              setKeyword(v)
            }}
          />
          <Select
            allowClear
            style={{ width: 180 }}
            placeholder={t('user.group')}
            value={groupFilter}
            options={groups.map((g) => ({ label: g.name, value: g.id }))}
            onChange={(v: number | undefined) => {
              setPage(1)
              setGroupFilter(v)
            }}
          />
          <Text type="secondary" style={{ fontSize: 12 }}>
            {t('common.total', { n: total })}
          </Text>
        </Space>
      </Card>

      <Table<User>
        rowKey="id"
        size="small"
        loading={loading}
        columns={columns}
        dataSource={users}
        scroll={{ x: 1600 }}
        pagination={{
          current: page,
          pageSize,
          total,
          showSizeChanger: true,
          onChange: (p, ps) => {
            setPage(p)
            setPageSize(ps)
          },
          showTotal: (n) => t('common.total', { n }),
        }}
        locale={{
          emptyText: (
            <Empty
              className="or-empty"
              description={
                <Space direction="vertical" size={4}>
                  <Text>{t('user.empty')}</Text>
                  <Text type="secondary" style={{ fontSize: 12 }}>
                    {t('user.emptyHint')}
                  </Text>
                </Space>
              }
            >
              <Button type="primary" icon={<PlusOutlined />} onClick={openCreate}>
                {t('user.add')}
              </Button>
            </Empty>
          ),
        }}
      />

      {/* ── 创建 / 编辑 ──────────────────────────────── */}
      <Drawer
        destroyOnClose
        width={640}
        open={editorOpen}
        title={editing ? `${t('common.edit')} · ${editing.username}` : t('user.add')}
        onClose={() => {
          setEditorOpen(false)
          setEditing(null)
        }}
        extra={
          <Space>
            <Button
              onClick={() => {
                setEditorOpen(false)
                setEditing(null)
              }}
            >
              {t('common.cancel')}
            </Button>
            <Button type="primary" loading={saving} onClick={() => void submit()}>
              {t('common.save')}
            </Button>
          </Space>
        }
      >
        <Alert
          type="info"
          showIcon
          style={{ marginBottom: 12 }}
          message={t('user.noRegistration')}
          description={<Text style={{ fontSize: 12 }}>{t('user.noRegistrationDetail')}</Text>}
        />
        <Form form={form} layout="vertical" autoComplete="off">
          <Row gutter={16}>
            <Col span={12}>
              <Form.Item
                name="username"
                label={t('auth.username')}
                rules={[
                  { required: true, message: t('user.usernameRequired') },
                  { pattern: /^[a-zA-Z0-9_.-]{3,32}$/, message: t('user.usernameRule') },
                ]}
              >
                <Input disabled={Boolean(editing)} placeholder="alice" />
              </Form.Item>
            </Col>
            <Col span={12}>
              <Form.Item name="nickname" label={t('user.nickname')}>
                <Input placeholder={t('user.nicknamePlaceholder')} />
              </Form.Item>
            </Col>
            {!editing && (
              <Col span={12}>
                <Form.Item
                  name="password"
                  label={t('auth.password')}
                  rules={[{ min: 8, message: t('user.passwordRule') }]}
                  extra={<Text type="secondary" style={{ fontSize: 12 }}>{t('user.passwordHint')}</Text>}
                >
                  <Input.Password placeholder={t('user.passwordPlaceholder')} />
                </Form.Item>
              </Col>
            )}
            <Col span={12}>
              <Form.Item name="role" label={t('user.role')} rules={[{ required: true }]}>
                <Select
                  options={[
                    { label: t('user.admin'), value: 'admin' },
                    { label: t('user.normal'), value: 'user' },
                  ]}
                />
              </Form.Item>
            </Col>
            <Col span={12}>
              <Form.Item name="group_id" label={t('user.group')} tooltip={t('user.groupHint')}>
                <Select
                  allowClear
                  placeholder={t('user.noGroup')}
                  options={groups.map((g) => ({ label: g.name, value: g.id }))}
                />
              </Form.Item>
            </Col>
            <Col span={12}>
              <Form.Item name="status" label={t('common.status')} valuePropName="checked">
                <Switch checkedChildren={t('common.enabled')} unCheckedChildren={t('common.disabled')} />
              </Form.Item>
            </Col>
          </Row>

          <Card size="small" title={t('user.limitsSection')} style={{ marginBottom: 12 }}>
            <Row gutter={16}>
              <Col span={12}>
                <Form.Item
                  name="traffic_limit"
                  label={t('user.trafficLimit')}
                  tooltip={t('user.trafficLimitHint')}
                >
                  <InputNumber min={0} style={{ width: '100%' }} addonAfter="B" placeholder="0" />
                </Form.Item>
              </Col>
              <Col span={12}>
                <Form.Item
                  name="speed_limit"
                  label={t('user.speedLimit')}
                  tooltip={t('user.speedLimitHint')}
                >
                  <InputNumber min={0} style={{ width: '100%' }} addonAfter="KB/s" placeholder="0" />
                </Form.Item>
              </Col>
              <Col span={8}>
                <Form.Item name="ip_limit" label={t('rule.ipLimit')}>
                  <InputNumber min={0} style={{ width: '100%' }} placeholder="0" />
                </Form.Item>
              </Col>
              <Col span={8}>
                <Form.Item name="conn_limit" label={t('rule.connLimit')}>
                  <InputNumber min={0} style={{ width: '100%' }} placeholder="0" />
                </Form.Item>
              </Col>
              <Col span={8}>
                <Form.Item name="device_limit" label={t('user.deviceLimit')}>
                  <InputNumber min={0} style={{ width: '100%' }} placeholder="0" />
                </Form.Item>
              </Col>
            </Row>
            <Text type="secondary" style={{ fontSize: 12 }}>
              {t('user.limitZeroHint')}
            </Text>
          </Card>

          <Form.Item name="remark" label={t('common.remark')}>
            <Input.TextArea rows={2} />
          </Form.Item>
        </Form>
      </Drawer>

      {/* ── 详情抽屉 ─────────────────────────────────── */}
      <UserDetailDrawer
        user={detailUser}
        onClose={() => setDetailUser(null)}
        t={t}
        onEdit={(u) => {
          setDetailUser(null)
          openEdit(u)
        }}
      />
    </div>
  )
}

/* ═══════════════════════ 用户详情 ═══════════════════════ */

interface DetailProps {
  user: User | null
  onClose: () => void
  onEdit: (u: User) => void
  t: (key: string, vars?: Record<string, string | number>) => string
}

/** 用户详情：订阅链接 + 该用户的规则 + 流量合计与按规则分布。 */
function UserDetailDrawer({ user, onClose, onEdit, t }: DetailProps) {
  const [rules, setRules] = useState<ForwardRule[]>([])
  const [traffic, setTraffic] = useState<UserTraffic | null>(null)
  const [loading, setLoading] = useState(false)
  const [activeTab, setActiveTab] = useState('overview')

  useEffect(() => {
    if (!user) {
      setRules([])
      setTraffic(null)
      return
    }
    let alive = true
    setLoading(true)
    Promise.all([
      userApi.rules(user.id).catch(() => [] as ForwardRule[]),
      // 用户维度返回的是各周期合计 + 按规则的分布，不是时间序列。
      userApi.traffic(user.id).catch(() => null),
    ])
      .then(([r, tr]) => {
        if (!alive) return
        setRules(r ?? [])
        setTraffic(tr)
      })
      .finally(() => {
        if (alive) setLoading(false)
      })
    return () => {
      alive = false
    }
  }, [user])

  const subURL = user ? userApi.subscribeURL(user.token) : ''
  // 各周期合计直接取后端算好的值，避免前端重复求和导致口径不一致。
  const totalRaw = traffic?.total.raw ?? 0
  const totalBilled = traffic?.total.total ?? 0

  return (
    <Drawer
      width={760}
      open={Boolean(user)}
      onClose={onClose}
      title={`${t('user.detailTitle')} · ${user?.username ?? ''}`}
      extra={
        user && (
          <Button onClick={() => onEdit(user)} icon={<EditOutlined />}>
            {t('common.edit')}
          </Button>
        )
      }
    >
      {user && (
        <Tabs
          activeKey={activeTab}
          onChange={setActiveTab}
          items={[
            {
              key: 'overview',
              label: t('user.tabOverview'),
              children: (
                <Space direction="vertical" style={{ width: '100%' }} size={16}>
                  <Descriptions size="small" column={2} bordered>
                    <Descriptions.Item label={t('auth.username')}>{user.username}</Descriptions.Item>
                    <Descriptions.Item label={t('user.nickname')}>
                      {user.nickname || '-'}
                    </Descriptions.Item>
                    <Descriptions.Item label={t('user.role')}>
                      {user.role === 'admin' ? t('user.admin') : t('user.normal')}
                    </Descriptions.Item>
                    <Descriptions.Item label={t('common.status')}>
                      <Tag color={user.status === 1 ? 'green' : 'default'}>
                        {user.status === 1 ? t('common.enabled') : t('common.disabled')}
                      </Tag>
                    </Descriptions.Item>
                    <Descriptions.Item label={t('user.trafficUsed')}>
                      {formatBytes(user.traffic_used)}
                    </Descriptions.Item>
                    <Descriptions.Item label={t('user.trafficLimit')}>
                      {user.traffic_limit > 0 ? formatBytes(user.traffic_limit) : t('rule.unlimited')}
                    </Descriptions.Item>
                    <Descriptions.Item label={t('user.speedLimit')}>
                      {user.speed_limit > 0 ? formatSpeed(user.speed_limit) : t('rule.unlimited')}
                    </Descriptions.Item>
                    <Descriptions.Item label={t('user.ipConnLimit')}>
                      {user.ip_limit || '-'} / {user.conn_limit || '-'} / {user.device_limit || '-'}
                    </Descriptions.Item>
                    <Descriptions.Item label={t('user.expireAt')}>
                      {user.expire_at ? formatTime(user.expire_at) : t('user.neverExpire')}
                    </Descriptions.Item>
                    <Descriptions.Item label={t('user.lastLogin')}>
                      {user.last_login_at ? formatTime(user.last_login_at) : '-'}
                    </Descriptions.Item>
                  </Descriptions>

                  {(user.password_reset_required || user.pending_reinstall) && (
                    <Space direction="vertical" size={6} style={{ width: '100%' }}>
                      {user.password_reset_required && (
                        <Alert
                          type="warning"
                          showIcon
                          message={t('user.passwordResetRequired')}
                          description={
                            <Text style={{ fontSize: 12 }}>
                              {t('user.passwordResetRequiredDetail')}
                            </Text>
                          }
                        />
                      )}
                      {user.pending_reinstall && (
                        <Alert
                          type="warning"
                          showIcon
                          message={t('user.pendingReinstall')}
                          description={
                            <Text style={{ fontSize: 12 }}>{t('user.pendingReinstallDetail')}</Text>
                          }
                        />
                      )}
                    </Space>
                  )}

                  <Card size="small" title={t('user.subscription')}>
                    <Space direction="vertical" style={{ width: '100%' }} size={8}>
                      <Text type="secondary" style={{ fontSize: 12 }}>
                        {t('user.subscriptionHint')}
                      </Text>
                      <CopyableCommand command={subURL} multiline />
                      <Text type="secondary" style={{ fontSize: 12 }}>
                        {t('user.subscriptionUnsafe')}
                      </Text>
                    </Space>
                  </Card>

                  <Card size="small" title={t('user.trafficSummary')}>
                    <Space direction="vertical" size={4} style={{ width: '100%' }}>
                      <Text className="or-mono" style={{ fontSize: 12 }}>
                        {t('user.trafficFromSeries', {
                          raw: formatBytes(totalRaw),
                          billed: formatBytes(totalBilled),
                        })}
                      </Text>
                      <Text type="secondary" style={{ fontSize: 12 }}>
                        {t('user.trafficSeriesHint')}
                      </Text>
                    </Space>
                  </Card>
                </Space>
              ),
            },
            {
              key: 'rules',
              label: t('user.tabRules', { n: rules.length }),
              children: (
                <Table<ForwardRule>
                  rowKey="id"
                  size="small"
                  loading={loading}
                  pagination={false}
                  dataSource={rules}
                  scroll={{ x: 640 }}
                  columns={[
                    { title: t('common.name'), dataIndex: 'name', key: 'name', width: 180 },
                    {
                      title: t('rule.listenPort'),
                      dataIndex: 'listen_port',
                      key: 'listen_port',
                      width: 110,
                      render: (v: number) => <Text className="or-mono">{v === 0 ? t('rule.randomPort') : v}</Text>,
                    },
                    {
                      title: t('rule.syncStatus'),
                      key: 'sync',
                      width: 120,
                      render: (_, row) => (
                        <SyncStatusTag
                          status={row.sync_status}
                          error={row.sync_error}
                          syncedAt={row.synced_at}
                          showIcon={false}
                        />
                      ),
                    },
                    {
                      title: t('common.enable'),
                      key: 'enable',
                      width: 80,
                      render: (_, row) => (
                        <Tag color={row.enable ? 'green' : 'default'}>
                          {row.enable ? t('common.enabled') : t('common.disabled')}
                        </Tag>
                      ),
                    },
                  ]}
                  locale={{
                    emptyText: (
                      <Empty
                        className="or-empty"
                        description={t('user.noRules')}
                      />
                    ),
                  }}
                />
              ),
            },
          ]}
        />
      )}
    </Drawer>
  )
}

/* ═══════════════════════ 工具 ═══════════════════════ */

/**
 * 展示一次性明文（密码 / Token）。
 *
 * 用 Modal 而非 message：明文较长时需要可复制、可阅读，
 * 并明确提醒「关闭后无法再次查看」。
 */
function showOneTimeSecret(
  title: string,
  secret: string,
  username: string,
  t: (key: string, vars?: Record<string, string | number>) => string,
) {
  Modal.info({
    title,
    width: 560,
    okText: t('user.iCopied'),
    icon: <KeyOutlined style={{ color: 'var(--or-warning)' }} />,
    content: (
      <Space direction="vertical" style={{ width: '100%' }} size={10}>
        <Paragraph style={{ margin: 0 }}>
          {t('user.oneTimeSecretFor', { name: username })}
        </Paragraph>
        <CopyableCommand command={secret} showQrcode={false} />
        <Alert
          type="warning"
          showIcon
          message={t('user.oneTimeSecretWarning')}
        />
      </Space>
    ),
  })
}

/** 字节数 → 人类可读。 */
function formatBytes(bytes: number): string {
  const n = Number(bytes ?? 0)
  if (!Number.isFinite(n) || n <= 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB']
  let value = n
  let i = 0
  while (value >= 1024 && i < units.length - 1) {
    value /= 1024
    i += 1
  }
  return `${value.toFixed(value >= 100 || i === 0 ? 0 : 2)} ${units[i]}`
}

/** KB/s → 友好展示（自动升到 MB/s、GB/s）。 */
function formatSpeed(kbps: number): string {
  if (kbps >= 1024 * 1024) return `${(kbps / 1024 / 1024).toFixed(2)} GB/s`
  if (kbps >= 1024) return `${(kbps / 1024).toFixed(2)} MB/s`
  return `${kbps} KB/s`
}

/** RFC3339 → 本地时间。 */
function formatTime(iso: string): string {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString()
}
