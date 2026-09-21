/**
 * 告警页面（规格书 6.12、9.2）。
 *
 * 三个 Tab：告警规则（CRUD + 测试）/ 活跃告警（一键标记解决）/ 历史告警。
 * 告警类型与通知渠道严格对齐规格书 6.12 的枚举。
 */
import { useCallback, useEffect, useState } from 'react'
import {
  Alert,
  Badge,
  Button,
  Card,
  Col,
  Empty,
  Form,
  Input,
  InputNumber,
  Modal,
  Row,
  Segmented,
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
  CheckCircleOutlined,
  DeleteOutlined,
  EditOutlined,
  PlusOutlined,
  ReloadOutlined,
  SendOutlined,
} from '@ant-design/icons'

import { nodeApi, ruleApi, showApiError, systemApi, userApi } from '../../api'
import type { AlertChannel, AlertHistory, AlertRule, AlertType, ForwardRule, Node, User } from '../../api/types'
import { useI18n } from '../../locales'
import { usePolling } from '../../hooks/usePolling'

const { Text, Paragraph } = Typography

/** 告警类型 → 阈值单位与含义（规格书 6.12）。 */
interface AlertTypeMeta {
  label: string
  /** 阈值单位；空表示该类型不使用阈值。 */
  unit: string
  /** 阈值语义说明。 */
  hint: string
  /** 是否需要指定目标资源。 */
  target: 'node' | 'user' | 'rule' | 'none'
  /** 是否使用 duration（防抖）。 */
  debounce: boolean
}

const ALERT_TYPE_META: Record<AlertType, AlertTypeMeta> = {
  node_offline: {
    label: '节点离线',
    unit: '秒',
    hint: '节点心跳中断超过该秒数即触发',
    target: 'node',
    debounce: false,
  },
  node_cpu: {
    label: '节点 CPU 超阈值',
    unit: '%',
    hint: 'CPU 使用率持续高于该百分比',
    target: 'node',
    debounce: true,
  },
  node_mem: {
    label: '节点内存超阈值',
    unit: '%',
    hint: '内存使用率持续高于该百分比',
    target: 'node',
    debounce: true,
  },
  node_disk: {
    label: '节点磁盘超阈值',
    unit: '%',
    hint: '磁盘使用率持续高于该百分比',
    target: 'node',
    debounce: true,
  },
  rule_sync_failed: {
    label: '规则同步失败',
    unit: '条',
    hint: '失败规则条数超过该值即触发；0 表示有一条失败就触发',
    target: 'rule',
    debounce: false,
  },
  user_traffic_pct: {
    label: '用户流量百分比',
    unit: '%',
    hint: '已用流量占流量上限的百分比',
    target: 'user',
    debounce: false,
  },
  rule_traffic_pct: {
    label: '规则流量百分比',
    unit: '%',
    hint: '规则累计计费流量占本告警流量基准的百分比',
    target: 'rule',
    debounce: false,
  },
  node_traffic_pct: {
    label: '节点流量百分比',
    unit: '%',
    hint: '节点累计计费流量占本告警流量基准的百分比',
    target: 'node',
    debounce: false,
  },
  cert_expire: {
    label: '证书即将过期',
    unit: '天',
    hint: '检查面板和规则自定义 TLS 证书。留空检查全部，也可指定规则。',
    target: 'rule',
    debounce: false,
  },
}

/** 通知渠道类型。 */
type ChannelType = 'webhook' | 'telegram' | 'email'

/** 表单值。 */
interface AlertFormValues {
  name: string
  type: AlertType
  target_id?: number
  threshold: number
  traffic_limit?: number
  duration: number
  silence_for: number
  enabled: boolean
  channels: AlertChannel[]
}

export default function AlertsPage() {
  const { t } = useI18n()
  const [activeTab, setActiveTab] = useState('rules')

  const [rules, setRules] = useState<AlertRule[]>([])
  const [history, setHistory] = useState<AlertHistory[]>([])
  const [loading, setLoading] = useState(false)
  const [historyResolved, setHistoryResolved] = useState<boolean | undefined>(false)

  const [nodes, setNodes] = useState<Node[]>([])
  const [users, setUsers] = useState<User[]>([])
  const [forwardRules, setForwardRules] = useState<ForwardRule[]>([])

  const [editorOpen, setEditorOpen] = useState(false)
  const [editing, setEditing] = useState<AlertRule | null>(null)
  const [saving, setSaving] = useState(false)
  const [form] = Form.useForm<AlertFormValues>()
  const alertType = Form.useWatch('type', form) as AlertType | undefined

  /** 拉取规则与历史。 */
  const load = useCallback(async () => {
    setLoading(true)
    try {
      const [ruleRes, histRes] = await Promise.all([
        systemApi.alertRules({ page: 1, page_size: 200 }),
        systemApi.alertHistory({ page: 1, page_size: 200, resolved: historyResolved }),
      ])
      setRules(ruleRes.items)
      setHistory(histRes.items)
    } catch (err) {
      showApiError(err, t('alert.loadFailed'))
    } finally {
      setLoading(false)
    }
  }, [historyResolved, t])

  useEffect(() => {
    void load()
  }, [load])

  // 活跃告警用轮询保持新鲜：告警是需要「尽快看到」的信息。
  const { data: activeAlerts, refresh: refreshActive } = usePolling<AlertHistory[]>(
    useCallback(async () => {
      try {
        const res = await systemApi.alertHistory({ page: 1, page_size: 100, resolved: false })
        return res.items
      } catch {
        return []
      }
    }, []),
    { interval: 20000 },
  )

  useEffect(() => {
    nodeApi
      .list({ page: 1, page_size: 500 })
      .then((res) => setNodes(res.items))
      .catch(() => setNodes([]))
    userApi
      .list({ page: 1, page_size: 500 })
      .then((res) => setUsers(res.items))
      .catch(() => setUsers([]))
  }, [])

  useEffect(() => {
    void ruleApi.list({ page: 1, page_size: 500 }).then((res) => setForwardRules(res.items)).catch(() => setForwardRules([]))
  }, [])

  const nodeName = (id: number) => nodes.find((n) => n.id === id)?.name ?? (id ? `#${id}` : '-')
  const userName = (id: number) => users.find((u) => u.id === id)?.username ?? (id ? `#${id}` : '-')

  /* ───────────────────── CRUD ───────────────────── */

  const openCreate = () => {
    setEditing(null)
    form.resetFields()
    form.setFieldsValue({
      type: 'node_offline',
      threshold: 60,
      duration: 0,
      silence_for: 300,
      enabled: true,
      channels: [{ type: 'webhook', url: '' }],
    })
    setEditorOpen(true)
  }

  const openEdit = (r: AlertRule) => {
    setEditing(r)
    form.setFieldsValue({
      name: r.name,
      type: r.type,
      target_id: r.target_id || undefined,
      threshold: r.threshold,
      traffic_limit: r.traffic_limit ? r.traffic_limit / 1024 ** 3 : undefined,
      duration: r.duration,
      silence_for: r.silence_for,
      enabled: r.enabled,
      channels: r.channels?.length ? r.channels : [{ type: 'webhook', url: '' }],
    })
    setEditorOpen(true)
  }

  const submit = async () => {
    let values: AlertFormValues
    try {
      values = await form.validateFields()
    } catch {
      return
    }
    setSaving(true)
    try {
      const payload: Partial<AlertRule> = {
        name: values.name.trim(),
        type: values.type,
        target_id: values.target_id ?? 0,
        threshold: values.threshold ?? 0,
        traffic_limit: Math.round((values.traffic_limit ?? 0) * 1024 ** 3),
        duration: values.duration ?? 0,
        silence_for: values.silence_for ?? 0,
        enabled: values.enabled,
        channels: values.channels ?? [],
      }
      if (editing) {
        await systemApi.updateAlert(editing.id, payload)
        message.success(t('alert.updateSuccess'))
      } else {
        await systemApi.createAlert(payload)
        message.success(t('alert.createSuccess'))
      }
      setEditorOpen(false)
      setEditing(null)
      await load()
    } catch (err) {
      showApiError(err, t('alert.saveFailed'))
    } finally {
      setSaving(false)
    }
  }

  const remove = (r: AlertRule) => {
    Modal.confirm({
      title: t('alert.deleteConfirm', { name: r.name }),
      okText: t('common.delete'),
      okButtonProps: { danger: true },
      cancelText: t('common.cancel'),
      content: (
        <Space direction="vertical" size={6}>
          <Text>{t('alert.deleteImpact')}</Text>
          <Text type="secondary" style={{ fontSize: 12 }}>
            {t('alert.deleteImpactDetail')}
          </Text>
        </Space>
      ),
      onOk: async () => {
        try {
          await systemApi.deleteAlert(r.id)
          message.success(t('alert.deleteSuccess'))
          await load()
        } catch (err) {
          showApiError(err, t('alert.deleteFailed'))
          throw err
        }
      },
    })
  }

  /** 测试一条规则的渠道：逐渠道展示结果。 */
  const test = async (r: AlertRule) => {
    try {
      const results = await systemApi.testAlert(r.id)
      const failed = results.filter((x) => !x.ok)
      if (!failed.length) {
        message.success(t('alert.testAllOk', { n: results.length }))
        return
      }
      Modal.warning({
        title: t('alert.testPartial'),
        width: 560,
        content: (
          <Table
            size="small"
            rowKey={(x) => `${x.type}:${x.target}`}
            pagination={false}
            dataSource={results}
            columns={[
              { title: t('alert.channel'), dataIndex: 'type', key: 'type', width: 140 },
              {
                title: t('common.status'),
                key: 'ok',
                width: 100,
                render: (_, row) => (
                  <Tag color={row.ok ? 'green' : 'red'}>
                    {row.ok ? t('alert.testOk') : t('alert.testFail')}
                  </Tag>
                ),
              },
              {
                title: t('alert.errorDetail'),
                dataIndex: 'error',
                key: 'error',
                render: (v: string) => <Text className="or-mono" style={{ fontSize: 12 }}>{v || '-'}</Text>,
              },
            ]}
          />
        ),
      })
    } catch (err) {
      showApiError(err, t('alert.testFailed'))
    }
  }

  /** 标记告警已解决。 */
  const resolve = async (h: AlertHistory) => {
    try {
      await systemApi.resolveAlert(h.id)
      message.success(t('alert.resolvedToast'))
      await refreshActive()
      await load()
    } catch (err) {
      showApiError(err, t('alert.resolveFailed'))
    }
  }

  /* ───────────────────── 表格列 ───────────────────── */

  const ruleColumns: ColumnsType<AlertRule> = [
    {
      title: t('common.name'),
      dataIndex: 'name',
      key: 'name',
      width: 220,
      render: (v: string, row) => (
        <Space direction="vertical" size={2}>
          <Text strong>{v}</Text>
          {!row.enabled && <Tag>{t('common.disabled')}</Tag>}
        </Space>
      ),
    },
    {
      title: t('alert.type'),
      dataIndex: 'type',
      key: 'type',
      width: 170,
      render: (v: AlertType) => <Tag color="blue">{alertTypeLabel(v, t)}</Tag>,
    },
    {
      title: t('alert.target'),
      key: 'target',
      width: 160,
      render: (_, row) => {
        const meta = ALERT_TYPE_META[row.type]
        if (meta?.target === 'node' && row.target_id) {
          return <Text>{nodeName(row.target_id)}</Text>
        }
        if (meta?.target === 'user' && row.target_id) {
          return <Text>{userName(row.target_id)}</Text>
        }
        if (meta?.target === 'rule' && row.target_id) {
          return <Text>{forwardRules.find((r) => r.id === row.target_id)?.name ?? `#${row.target_id}`}</Text>
        }
        return <Text type="secondary">{t('alert.globalScope')}</Text>
      },
    },
    {
      title: t('alert.threshold'),
      key: 'threshold',
      width: 130,
      render: (_, row) => (
        <Text className="or-mono">
          {row.threshold}
          {ALERT_TYPE_META[row.type]?.unit ?? ''}
          {!!row.traffic_limit && <div>基准 {(row.traffic_limit / 1024 ** 3).toFixed(2)} GiB</div>}
          {!row.traffic_limit && (row.type === 'node_traffic_pct' || row.type === 'rule_traffic_pct') && <Tag color="warning">请设置流量基准</Tag>}
        </Text>
      ),
    },
    {
      title: t('alert.duration'),
      dataIndex: 'duration',
      key: 'duration',
      width: 110,
      render: (v: number) =>
        v > 0 ? <Text className="or-mono">{v}s</Text> : <Text type="secondary">{t('alert.immediate')}</Text>,
    },
    {
      title: t('alert.silenceFor'),
      dataIndex: 'silence_for',
      key: 'silence_for',
      width: 120,
      render: (v: number) => <Text className="or-mono">{formatDuration(v)}</Text>,
    },
    {
      title: t('alert.channels'),
      dataIndex: 'channels',
      key: 'channels',
      width: 190,
      render: (cs: AlertChannel[]) => (
        <Space size={4} wrap>
          {(cs ?? []).map((c, i) => (
            <Tag key={`${c.type}-${i}`} color={channelColor(c.type)}>
              {channelLabel(c.type)}
            </Tag>
          ))}
          {!cs?.length && <Text type="secondary">{t('alert.noChannel')}</Text>}
        </Space>
      ),
    },
    {
      title: t('alert.lastFired'),
      dataIndex: 'last_fired_at',
      key: 'last_fired_at',
      width: 170,
      render: (v: string | null) =>
        v ? <Text className="or-mono" style={{ fontSize: 12 }}>{formatTime(v)}</Text> : <Text type="secondary">-</Text>,
    },
    {
      title: t('common.actions'),
      key: 'actions',
      width: 150,
      fixed: 'right',
      render: (_, row) => (
        <div className="or-actions">
          <Tooltip title={t('alert.test')}>
            <Button size="small" type="text" icon={<SendOutlined />} onClick={() => void test(row)} />
          </Tooltip>
          <Tooltip title={t('common.edit')}>
            <Button size="small" type="text" icon={<EditOutlined />} onClick={() => openEdit(row)} />
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

  const historyColumns: ColumnsType<AlertHistory> = [
    {
      title: t('alert.level'),
      dataIndex: 'level',
      key: 'level',
      width: 100,
      render: (v: AlertHistory['level']) => (
        <Tag color={v === 'critical' ? 'red' : v === 'warning' ? 'orange' : 'blue'}>
          {v}
        </Tag>
      ),
    },
    {
      title: t('alert.title'),
      dataIndex: 'title',
      key: 'title',
      width: 240,
      render: (v: string, row) => (
        <Space direction="vertical" size={2}>
          <Text strong>{v}</Text>
          <Text type="secondary" style={{ fontSize: 12 }}>
            {row.content}
          </Text>
        </Space>
      ),
    },
    {
      title: t('alert.resource'),
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
      title: t('alert.firedAt'),
      dataIndex: 'fired_at',
      key: 'fired_at',
      width: 170,
      render: (v: string) => <Text className="or-mono" style={{ fontSize: 12 }}>{formatTime(v)}</Text>,
    },
    {
      title: t('common.status'),
      key: 'resolved',
      width: 120,
      render: (_, row) =>
        row.resolved ? (
          <Tag color="green">{t('alert.resolved')}</Tag>
        ) : (
          <Badge status="processing" text={t('alert.active')} />
        ),
    },
    {
      title: t('alert.resolvedAt'),
      dataIndex: 'resolved_at',
      key: 'resolved_at',
      width: 170,
      render: (v: string | null) =>
        v ? <Text className="or-mono" style={{ fontSize: 12 }}>{formatTime(v)}</Text> : <Text type="secondary">-</Text>,
    },
    {
      title: t('common.actions'),
      key: 'actions',
      width: 130,
      fixed: 'right',
      render: (_, row) =>
        row.resolved ? (
          <Text type="secondary">-</Text>
        ) : (
          <Button size="small" icon={<CheckCircleOutlined />} onClick={() => void resolve(row)}>
            {t('alert.markResolved')}
          </Button>
        ),
    },
  ]

  return (
    <div>
      <div className="or-page-header">
        <div>
          <h2 className="or-page-title">{t('alert.title')}</h2>
          <p className="or-page-desc">{t('alert.pageDesc')}</p>
        </div>
        <Space>
          <Button icon={<ReloadOutlined />} onClick={() => void load()}>
            {t('common.refresh')}
          </Button>
          <Button type="primary" icon={<PlusOutlined />} onClick={openCreate}>
            {t('alert.addRule')}
          </Button>
        </Space>
      </div>

      <Tabs
        activeKey={activeTab}
        onChange={setActiveTab}
        items={[
          {
            key: 'rules',
            label: t('alert.rules'),
            children: (
              <Table<AlertRule>
                rowKey="id"
                size="small"
                loading={loading}
                columns={ruleColumns}
                dataSource={rules}
                scroll={{ x: 1500 }}
                pagination={{ pageSize: 20, showTotal: (n) => t('common.total', { n }) }}
                locale={{
                  emptyText: (
                    <Empty
                      className="or-empty"
                      description={
                        <Space direction="vertical" size={4}>
                          <Text>{t('alert.noRules')}</Text>
                          <Text type="secondary" style={{ fontSize: 12 }}>
                            {t('alert.noRulesHint')}
                          </Text>
                        </Space>
                      }
                    >
                      <Button type="primary" icon={<PlusOutlined />} onClick={openCreate}>
                        {t('alert.addRule')}
                      </Button>
                    </Empty>
                  ),
                }}
              />
            ),
          },
          {
            key: 'active',
            label: (
              <Space size={4}>
                {t('alert.active')}
                <Badge count={activeAlerts?.length ?? 0} size="small" />
              </Space>
            ),
            children: (
              <>
                <Alert
                  type="info"
                  showIcon
                  style={{ marginBottom: 12 }}
                  message={t('alert.activeHint')}
                />
                <Table<AlertHistory>
                  rowKey="id"
                  size="small"
                  columns={historyColumns}
                  dataSource={activeAlerts ?? []}
                  scroll={{ x: 1300 }}
                  pagination={{ pageSize: 20, showTotal: (n) => t('common.total', { n }) }}
                  locale={{
                    emptyText: (
                      <Empty
                        className="or-empty"
                        description={
                          <Space direction="vertical" size={4}>
                            <Text>{t('alert.noActive')}</Text>
                            <Text type="secondary" style={{ fontSize: 12 }}>
                              {t('alert.noActiveHint')}
                            </Text>
                          </Space>
                        }
                      />
                    ),
                  }}
                />
              </>
            ),
          },
          {
            key: 'history',
            label: t('alert.history'),
            children: (
              <>
                <Card size="small" style={{ marginBottom: 12 }}>
                  <Space wrap>
                    <Text type="secondary" style={{ fontSize: 12 }}>
                      {t('alert.filterResolved')}
                    </Text>
                    <Segmented
                      value={historyResolved === undefined ? 'all' : historyResolved ? 'yes' : 'no'}
                      onChange={(v) =>
                        setHistoryResolved(v === 'all' ? undefined : v === 'yes')
                      }
                      options={[
                        { label: t('alert.unresolved'), value: 'no' },
                        { label: t('alert.resolved'), value: 'yes' },
                        { label: t('common.none'), value: 'all' },
                      ]}
                    />
                  </Space>
                </Card>
                <Table<AlertHistory>
                  rowKey="id"
                  size="small"
                  loading={loading}
                  columns={historyColumns}
                  dataSource={history}
                  scroll={{ x: 1300 }}
                  pagination={{ pageSize: 20, showTotal: (n) => t('common.total', { n }) }}
                  locale={{
                    emptyText: <Empty className="or-empty" description={t('alert.empty')} />,
                  }}
                />
              </>
            ),
          },
        ]}
      />

      {/* ── 规则编辑 ─────────────────────────────────── */}
      <Modal
        open={editorOpen}
        width={720}
        title={editing ? `${t('common.edit')} · ${editing.name}` : t('alert.addRule')}
        onCancel={() => {
          setEditorOpen(false)
          setEditing(null)
        }}
        onOk={() => void submit()}
        confirmLoading={saving}
        okText={t('common.save')}
        cancelText={t('common.cancel')}
        destroyOnClose
      >
        <Form form={form} layout="vertical" autoComplete="off">
          <Row gutter={16}>
            <Col span={12}>
              <Form.Item
                name="name"
                label={t('common.name')}
                rules={[{ required: true, message: t('alert.nameRequired') }]}
              >
                <Input placeholder={t('alert.namePlaceholder')} />
              </Form.Item>
            </Col>
            <Col span={12}>
              <Form.Item name="type" label={t('alert.type')} rules={[{ required: true }]}>
                <Select
                  onChange={(type: AlertType) => form.setFieldsValue({
                    target_id: undefined,
                    threshold: type === 'cert_expire' ? 30 : type === 'rule_sync_failed' ? 0 : type === 'node_offline' ? 60 : 80,
                  })}
                  options={(Object.keys(ALERT_TYPE_META) as AlertType[]).map((k) => ({
                    label: alertTypeLabel(k, t),
                    value: k,
                  }))}
                />
              </Form.Item>
            </Col>
          </Row>

          {alertType && (
            <Alert
              type="info"
              showIcon
              style={{ marginBottom: 12 }}
              message={alertTypeLabel(alertType, t)}
              description={
                <Space direction="vertical" size={2}>
                  <Text style={{ fontSize: 12 }}>{ALERT_TYPE_META[alertType].hint}</Text>
                  {ALERT_TYPE_META[alertType].target === 'node' && (
                    <Text type="secondary" style={{ fontSize: 12 }}>
                      {t('alert.nodeTargetHint')}
                    </Text>
                  )}
                  {ALERT_TYPE_META[alertType].target === 'user' && (
                    <Text type="secondary" style={{ fontSize: 12 }}>
                      {t('alert.userTargetHint')}
                    </Text>
                  )}
                </Space>
              }
            />
          )}

          <Row gutter={16}>
            {alertType && ALERT_TYPE_META[alertType].target !== 'none' && (
              <Col span={12}>
                <Form.Item
                  name="target_id"
                  label={t('alert.target')}
                  tooltip={t('alert.targetHint')}
                >
                  {ALERT_TYPE_META[alertType].target === 'user' ? (
                    <Select
                      allowClear
                      showSearch
                      optionFilterProp="label"
                      placeholder={t('alert.allUsers')}
                      options={users.map((u) => ({ label: u.username, value: u.id }))}
                    />
                  ) : ALERT_TYPE_META[alertType].target === 'rule' ? (
                    <Select allowClear showSearch optionFilterProp="label" placeholder={alertType === 'cert_expire' ? '面板和全部规则证书' : '全部规则'} options={forwardRules.map((r) => ({ label: r.name, value: r.id }))} />
                  ) : (
                    <Select
                      allowClear
                      showSearch
                      optionFilterProp="label"
                      placeholder={t('alert.allNodes')}
                      options={nodes.map((n) => ({ label: n.name, value: n.id }))}
                    />
                  )}
                </Form.Item>
              </Col>
            )}
            <Col span={12}>
              <Form.Item
                name="threshold"
                label={t('alert.threshold')}
                tooltip={alertType ? ALERT_TYPE_META[alertType].hint : undefined}
                rules={[{ required: true, message: t('deviceGroup.required') }]}
              >
                <InputNumber
                  min={0}
                  style={{ width: '100%' }}
                  addonAfter={alertType ? ALERT_TYPE_META[alertType].unit : undefined}
                />
              </Form.Item>
            </Col>
            <Col span={12}>
              <Form.Item
                name="duration"
                label={t('alert.duration')}
                tooltip={t('alert.durationHint')}
                extra={<Text type="secondary" style={{ fontSize: 12 }}>{t('alert.durationHint')}</Text>}
              >
                <InputNumber
                  min={0}
                  style={{ width: '100%' }}
                  addonAfter="s"
                  disabled={alertType ? !ALERT_TYPE_META[alertType].debounce : false}
                />
              </Form.Item>
            </Col>
            <Col span={12}>
              <Form.Item
                name="silence_for"
                label={t('alert.silenceFor')}
                tooltip={t('alert.silenceForHint')}
                extra={<Text type="secondary" style={{ fontSize: 12 }}>{t('alert.silenceForHint')}</Text>}
              >
                <InputNumber min={0} style={{ width: '100%' }} addonAfter="s" />
              </Form.Item>
            </Col>
            <Col span={12}>
              <Form.Item name="enabled" label={t('common.enable')} valuePropName="checked">
                <Switch />
              </Form.Item>
            </Col>
          </Row>

          {(alertType === 'rule_traffic_pct' || alertType === 'node_traffic_pct') && (
            <Form.Item name="traffic_limit" label="流量告警基准" extra="按累计计费流量计算，每个目标分别使用此基准；此设置不限制转发流量。" rules={[{ required: true, type: 'number', min: 0.000001, message: '请填写大于零的流量基准' }]}>
              <InputNumber min={0.000001} addonAfter="GiB" style={{ width: '100%' }} />
            </Form.Item>
          )}
          <Form.List name="channels">
            {(fields, { add, remove: removeChannel }) => (
              <Space direction="vertical" style={{ width: '100%' }} size={8}>
                <Text strong style={{ fontSize: 13 }}>
                  {t('alert.channels')}
                </Text>
                {fields.map(({ key, name, ...restField }) => (
                  <ChannelRow
                    key={key}
                    name={name}
                    restField={restField}
                    onRemove={() => removeChannel(name)}
                    form={form}
                    t={t}
                  />
                ))}
                <Space wrap>
                  <Button
                    type="dashed"
                    size="small"
                    icon={<PlusOutlined />}
                    onClick={() => add({ type: 'webhook', url: '' })}
                  >
                    Webhook
                  </Button>
                  <Button
                    type="dashed"
                    size="small"
                    icon={<PlusOutlined />}
                    onClick={() => add({ type: 'telegram', token: '', chat: '' })}
                  >
                    Telegram
                  </Button>
                  <Button
                    type="dashed"
                    size="small"
                    icon={<PlusOutlined />}
                    onClick={() => add({ type: 'email', host: '', port: 465, to: '' })}
                  >
                    {t('alert.email')}
                  </Button>
                </Space>
                <Text type="secondary" style={{ fontSize: 12 }}>
                  {t('alert.channelHint')}
                </Text>
              </Space>
            )}
          </Form.List>
        </Form>
      </Modal>
    </div>
  )
}

/* ═══════════════════════ 渠道行 ═══════════════════════ */

interface ChannelRowProps {
  name: number
  /** Form.List 透传的 restField，原样展开到 Form.Item 上。 */
  restField: Record<string, unknown>
  onRemove: () => void
  form: ReturnType<typeof Form.useForm<AlertFormValues>>[0]
  t: (key: string, vars?: Record<string, string | number>) => string
}

/** 单个通知渠道的字段行：按渠道类型展示不同字段。 */
function ChannelRow({ name, restField, onRemove, form, t }: ChannelRowProps) {
  const type = Form.useWatch(['channels', name, 'type'], form) as ChannelType | undefined

  return (
    <div
      style={{
        border: '1px solid var(--or-border)',
        borderRadius: 'var(--or-radius)',
        padding: 8,
      }}
    >
      <Row gutter={8} align="middle">
        <Col span={5}>
          <Form.Item {...restField} name={[name, 'type']} noStyle>
            <Select
              options={[
                { label: 'Webhook', value: 'webhook' },
                { label: 'Telegram', value: 'telegram' },
                { label: t('alert.email'), value: 'email' },
              ]}
            />
          </Form.Item>
        </Col>

        {type === 'webhook' && (
          <Col span={17}>
            <Form.Item
              {...restField}
              name={[name, 'url']}
              noStyle
              rules={[{ type: 'url', message: t('alert.urlInvalid') }]}
            >
              <Input placeholder="https://example.com/hook" />
            </Form.Item>
          </Col>
        )}

        {type === 'telegram' && (
          <>
            <Col span={8}>
              <Form.Item {...restField} name={[name, 'token']} noStyle>
                <Input placeholder="Bot Token" />
              </Form.Item>
            </Col>
            <Col span={9}>
              <Form.Item {...restField} name={[name, 'chat']} noStyle>
                <Input placeholder="Chat ID" />
              </Form.Item>
            </Col>
          </>
        )}

        {type === 'email' && (
          <>
            <Col span={7}>
              <Form.Item {...restField} name={[name, 'host']} noStyle>
                <Input placeholder="smtp.example.com" />
              </Form.Item>
            </Col>
            <Col span={4}>
              <Form.Item {...restField} name={[name, 'port']} noStyle>
                <InputNumber min={1} max={65535} style={{ width: '100%' }} placeholder="465" />
              </Form.Item>
            </Col>
            <Col span={6}>
              <Form.Item {...restField} name={[name, 'user']} noStyle>
                <Input placeholder={t('alert.smtpUser')} />
              </Form.Item>
            </Col>
            <Col span={6}>
              <Form.Item {...restField} name={[name, 'pass']} noStyle>
                <Input.Password placeholder="SMTP 密码" />
              </Form.Item>
            </Col>
            <Col span={6}>
              <Form.Item {...restField} name={[name, 'to']} noStyle>
                <Input placeholder={t('alert.mailTo')} />
              </Form.Item>
            </Col>
          </>
        )}

        <Col span={2}>
          <Button size="small" danger type="text" icon={<DeleteOutlined />} onClick={onRemove} />
        </Col>
      </Row>

      {type === 'email' && (
        <Paragraph type="secondary" style={{ fontSize: 12, margin: '4px 0 0' }}>
          {t('alert.smtpHint')}
        </Paragraph>
      )}
    </div>
  )
}

/* ═══════════════════════ 工具 ═══════════════════════ */

/** 告警类型文案。 */
function alertTypeLabel(
  type: AlertType,
  t: (k: string, vars?: Record<string, string | number>) => string,
): string {
  const map: Record<AlertType, string> = {
    node_offline: 'alert.type.nodeOffline',
    node_cpu: 'alert.type.nodeCpu',
    node_mem: 'alert.type.nodeMem',
    node_disk: 'alert.type.nodeDisk',
    rule_sync_failed: 'alert.type.ruleSyncFailed',
    user_traffic_pct: 'alert.type.userTraffic',
    rule_traffic_pct: 'alert.type.ruleTraffic',
    node_traffic_pct: 'alert.type.nodeTraffic',
    cert_expire: 'alert.type.certExpire',
  }
  return t(map[type] ?? 'alert.type.unknown')
}

/** 渠道文案。 */
function channelLabel(type: ChannelType): string {
  if (type === 'email') return 'Email'
  if (type === 'telegram') return 'Telegram'
  return 'Webhook'
}

/** 渠道标签配色。 */
function channelColor(type: ChannelType): string {
  if (type === 'email') return 'purple'
  if (type === 'telegram') return 'blue'
  return 'geekblue'
}

/** 秒 → 时长文本。 */
function formatDuration(sec: number): string {
  if (!sec || sec <= 0) return '-'
  if (sec < 60) return `${sec}s`
  if (sec < 3600) return `${Math.round(sec / 60)}min`
  return `${(sec / 3600).toFixed(1)}h`
}

/** RFC3339 → 本地时间。 */
function formatTime(iso: string): string {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString()
}
