/**
 * 转发规则页面（规格书 9.2、6.4、6.7、6.8、8.9）。
 *
 * 本页是面板里最重的一页，包含四块：
 *  1. 规则表格：列与查询条件严格对齐规格书 6.4 与 8.9；
 *  2. 创建 / 编辑抽屉：覆盖 6.4 表单字段表的全部字段；
 *  3. 批量操作栏：8.9 的九种 action，删除走「输入 YES」强确认；
 *  4. 导入 / 导出：旧文本格式与新 JSON 双支持，导入带预览步骤。
 */
import { useCallback, useEffect, useMemo, useState } from 'react'
import {
  Alert,
  Button,
  Card,
  Col,
  Divider,
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
  Upload,
  message,
} from 'antd'
import type { ColumnsType } from 'antd/es/table'
import {
  CloudUploadOutlined,
  DeleteOutlined,
  DownloadOutlined,
  EditOutlined,
  ExclamationCircleOutlined,
  EyeOutlined,
  PlusOutlined,
  ReloadOutlined,
  SyncOutlined,
  ThunderboltOutlined,
  UploadOutlined,
} from '@ant-design/icons'

import { deviceGroupApi, ruleApi, showApiError, userApi } from '../../api'
import type {
  DeviceGroup,
  ForwardRule,
  ImportPreview,
  RuleGroup,
  Target,
  TargetBalance,
  User,
} from '../../api/types'
import TargetList, { isValidHost } from '../../components/TargetList'
import JsonEditor from '../../components/JsonEditor'
import SyncStatusTag from '../../components/SyncStatusTag'
import { useI18n } from '../../locales'

const { Text } = Typography

/** 目标负载均衡策略（规格书 6.3）。 */
const TARGET_BALANCE_OPTIONS: Array<{ label: string; value: TargetBalance }> = [
  { label: 'failover · 主备（按顺序故障转移）', value: 'failover' },
  { label: 'least_conn · 最少连接', value: 'least_conn' },
  { label: 'round_robin · 轮询', value: 'round_robin' },
  { label: 'weighted · 加权随机', value: 'weighted' },
  { label: 'hash_ip · 客户端 IP 哈希', value: 'hash_ip' },
]

/** 速度限制单位（规格书 6.4）。 */
type SpeedUnit = 'KB' | 'MB' | 'Gbps'

/** 各单位相对 KB/s 的换算系数。 */
const SPEED_FACTORS: Record<SpeedUnit, number> = {
  KB: 1,
  MB: 1024,
  Gbps: 125000, // 1 Gbps = 125000 KB/s
}

/** SNI 分流主规则的固定端口（规格书 6.8 示例为 443）。 */
const SNI_SHARED_PORT = 443

/** 后端返回的 SNI 白名单校验错误码。 */
const CODE_SNI_NOT_ALLOWED = 42204

/** 会话条目（后端 /forward-rules/:id/sessions 返回）。 */
interface RuleSession {
  id?: number
  rule_id?: number
  client_ip?: string
  target?: string
  started_at?: string
  duration?: number
  bytes_in?: number
  bytes_out?: number
}

/** 表单值（平铺，便于 InputNumber / Switch 直接绑定）。 */
interface RuleFormValues {
  name: string
  user_id?: number
  rule_group_id?: number
  inbound_group_id: number
  listen_port: number
  listen_port_end?: number
  outbound_group_id?: number
  targets: Target[]
  target_balance: TargetBalance
  inbound_multiplier: number
  outbound_multiplier: number
  speed_limit: number
  speed_unit: SpeedUnit
  conn_limit: number
  ip_limit: number
  reverse_enable: boolean
  reverse_port?: number
  reverse_group_id?: number
  chain_groups: number[]
  is_sub_rule: boolean
  sni?: string
  enable: boolean
  remark?: string
  options_text?: string
}

export default function ForwardRulesPage() {
  const { t } = useI18n()

  // ── 列表状态 ─────────────────────────────────────────
  const [rules, setRules] = useState<ForwardRule[]>([])
  const [loading, setLoading] = useState(false)
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)
  const [total, setTotal] = useState(0)
  const [selectedKeys, setSelectedKeys] = useState<React.Key[]>([])
  const [expandedKeys, setExpandedKeys] = useState<React.Key[]>([])

  // ── 高级筛选 ─────────────────────────────────────────
  const [filters, setFilters] = useState<{
    keyword?: string
    user_id?: number
    rule_group_id?: number
    inbound_group_id?: number
    outbound_group_id?: number
    sync_status?: string
    enable?: boolean
  }>({})
  const [filterForm] = Form.useForm()

  // ── 关联数据 ─────────────────────────────────────────
  const [groups, setGroups] = useState<DeviceGroup[]>([])
  const [ruleGroups, setRuleGroups] = useState<RuleGroup[]>([])
  const [users, setUsers] = useState<User[]>([])

  // ── 编辑抽屉 ─────────────────────────────────────────
  const [editorOpen, setEditorOpen] = useState(false)
  const [editing, setEditing] = useState<ForwardRule | null>(null)
  const [submitting, setSubmitting] = useState(false)
  const [form] = Form.useForm<RuleFormValues>()

  // ── 会话（展开行懒加载）──────────────────────────────
  const [sessions, setSessions] = useState<Record<number, RuleSession[]>>({})
  const [sessionsLoading, setSessionsLoading] = useState<Record<number, boolean>>({})

  // ── 导入 / 导出 ──────────────────────────────────────
  const [importOpen, setImportOpen] = useState(false)

  /** 拉取规则列表。 */
  const load = useCallback(async () => {
    setLoading(true)
    try {
      const res = await ruleApi.list({ page, page_size: pageSize, ...filters })
      setRules(res.items)
      setTotal(res.pagination.total)
    } catch (err) {
      showApiError(err, t('rule.loadFailed'))
    } finally {
      setLoading(false)
    }
  }, [page, pageSize, filters, t])

  /** 关联数据只需在进入页面时拉一次。 */
  const loadRefs = useCallback(async () => {
    try {
      const [g, rg, u] = await Promise.all([
        deviceGroupApi.list({ page: 1, page_size: 300 }),
        ruleApi.listGroups({ page: 1, page_size: 200 }),
        userApi.list({ page: 1, page_size: 500 }),
      ])
      setGroups(g.items)
      setRuleGroups(rg.items)
      setUsers(u.items)
    } catch (err) {
      showApiError(err, t('rule.loadRefsFailed'))
    }
  }, [t])

  useEffect(() => {
    void loadRefs()
  }, [loadRefs])

  useEffect(() => {
    void load()
  }, [load])

  const inboundGroups = useMemo(
    () => groups.filter((g) => g.type === 'inbound'),
    [groups],
  )
  const outboundGroups = useMemo(
    () => groups.filter((g) => g.type === 'outbound'),
    [groups],
  )
  const groupName = useCallback(
    (id: number | undefined) => groups.find((g) => g.id === id)?.name ?? (id ? `#${id}` : '-'),
    [groups],
  )
  const userName = useCallback(
    (id: number | undefined) =>
      users.find((u) => u.id === id)?.username ?? (id ? `#${id}` : '-'),
    [users],
  )
  const ruleGroupName = useCallback(
    (id: number | undefined) =>
      ruleGroups.find((g) => g.id === id)?.name ?? (id ? `#${id}` : '-'),
    [ruleGroups],
  )

  /* ───────────────────── 打开编辑 ───────────────────── */

  const openCreate = () => {
    setEditing(null)
    form.resetFields()
    setEditorOpen(true)
  }

  const openEdit = async (rule: ForwardRule) => {
    try {
      // 列表接口可能不返回完整 targets/options，编辑前拉一次详情。
      const full = await ruleApi.getOne(rule.id)
      setEditing(full)
      form.setFieldsValue(toFormValues(full))
      setEditorOpen(true)
    } catch (err) {
      // 详情拉取失败时退化为用列表行数据编辑。
      showApiError(err, t('rule.detailFailed'))
      setEditing(rule)
      form.setFieldsValue(toFormValues(rule))
      setEditorOpen(true)
    }
  }

  /* ───────────────────── 提交 ───────────────────── */

  const submit = async () => {
    let values: RuleFormValues
    try {
      values = await form.validateFields()
    } catch {
      return
    }

    // options 是自由 JSON：非法时不提交，并把错误定位到该字段。
    let options: Record<string, unknown> | undefined
    const rawOptions = values.options_text?.trim()
    if (rawOptions) {
      try {
        const parsed: unknown = JSON.parse(rawOptions)
        if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) {
          form.setFields([{ name: 'options_text', errors: [t('rule.optionsMustBeObject')] }])
          return
        }
        options = parsed as Record<string, unknown>
      } catch {
        form.setFields([{ name: 'options_text', errors: [t('rule.optionsInvalid')] }])
        return
      }
    }

    const payload = {
      name: values.name.trim(),
      user_id: values.user_id,
      rule_group_id: values.rule_group_id || 0,
      inbound_group_id: values.inbound_group_id,
      listen_port: values.listen_port ?? 0,
      listen_port_end: values.listen_port_end || undefined,
      outbound_group_id: values.outbound_group_id || undefined,
      targets: (values.targets ?? []).filter((x) => x.host && x.port),
      target_balance: values.target_balance,
      inbound_multiplier: Number(values.inbound_multiplier ?? 1),
      outbound_multiplier: Number(values.outbound_multiplier ?? 1),
      // 统一换算成 KB/s 提交（规格书 6.4：单位可选 KB/s、MB/s、Gbps）
      speed_limit: Math.round(
        Number(values.speed_limit ?? 0) * SPEED_FACTORS[values.speed_unit ?? 'KB'],
      ),
      conn_limit: values.conn_limit ?? 0,
      ip_limit: values.ip_limit ?? 0,
      reverse_enable: Boolean(values.reverse_enable),
      reverse_port: values.reverse_enable ? values.reverse_port || 0 : 0,
      reverse_group_id: values.reverse_enable ? values.reverse_group_id || 0 : 0,
      chain_groups: values.chain_groups ?? [],
      sni: values.is_sub_rule ? values.sni || '' : '',
      enable: values.enable ?? true,
      remark: values.remark ?? '',
      ...(options ? { options } : {}),
    }

    setSubmitting(true)
    try {
      if (editing) {
        await ruleApi.update(editing.id, payload)
        message.success(t('rule.updateSuccess'))
      } else {
        await ruleApi.create(payload)
        message.success(t('rule.createSuccess'))
      }
      setEditorOpen(false)
      setEditing(null)
      await load()
    } catch (err) {
      // 后端返回的 field 用于定位到具体表单项（规格书 9.3）。
      if (err instanceof Error && 'code' in err) {
        const code = (err as { code: number }).code
        const field = (err as { details?: { field?: string } }).details?.field
        if (code === CODE_SNI_NOT_ALLOWED || field === 'sni') {
          form.setFields([
            { name: 'sni', errors: [(err as Error).message || t('rule.sniNotAllowed')] },
          ])
        } else if (field === 'name') {
          form.setFields([{ name: 'name', errors: [(err as Error).message] }])
        }
      }
      showApiError(err, t('rule.saveFailed'))
    } finally {
      setSubmitting(false)
    }
  }

  /* ───────────────────── 单条操作 ───────────────────── */

  const toggleEnable = async (rule: ForwardRule, next: boolean) => {
    try {
      if (next) {
        await ruleApi.enable(rule.id)
      } else {
        await ruleApi.disable(rule.id)
      }
      message.success(next ? t('rule.enabledToast') : t('rule.disabledToast'))
      await load()
    } catch (err) {
      showApiError(err, t('rule.opFailed'))
      await load()
    }
  }

  const resync = async (rule: ForwardRule) => {
    try {
      await ruleApi.resync(rule.id)
      message.success(t('rule.resyncQueued'))
      await load()
    } catch (err) {
      showApiError(err, t('rule.opFailed'))
    }
  }

  const removeRule = (rule: ForwardRule) => {
    Modal.confirm({
      title: t('rule.deleteConfirm', { name: rule.name }),
      okText: t('common.delete'),
      okButtonProps: { danger: true },
      cancelText: t('common.cancel'),
      icon: <ExclamationCircleOutlined style={{ color: 'var(--or-error)' }} />,
      content: (
        <Space direction="vertical" size={6}>
          <Text>{t('rule.deleteImpact', { user: userName(rule.user_id) })}</Text>
          <Text type="secondary" style={{ fontSize: 12 }}>
            {t('rule.deleteImpactDetail')}
          </Text>
        </Space>
      ),
      onOk: async () => {
        try {
          await ruleApi.remove(rule.id)
          message.success(t('rule.deleteSuccess'))
          await load()
        } catch (err) {
          showApiError(err, t('rule.deleteFailed'))
          throw err
        }
      },
    })
  }

  /* ───────────────────── 批量操作 ───────────────────── */

  const selectedIds = useMemo(() => selectedKeys.map((k) => Number(k)), [selectedKeys])

  const runBatch = async (
    action: Parameters<typeof ruleApi.batch>[0]['action'],
    params?: Record<string, unknown>,
  ) => {
    if (!selectedIds.length) {
      message.warning(t('rule.selectFirst'))
      return
    }
    try {
      const res = await ruleApi.batch({ action, ids: selectedIds, params })
      if (res.failed?.length) {
        Modal.warning({
          title: t('rule.batchPartial'),
          width: 640,
          content: (
            <Space direction="vertical" size={4} style={{ width: '100%' }}>
              <Text>
                {t('rule.batchResult', { ok: res.succeeded?.length ?? 0, bad: res.failed.length })}
              </Text>
              <Table
                size="small"
                rowKey="id"
                pagination={false}
                dataSource={res.failed}
                columns={[
                  { title: 'ID', dataIndex: 'id', key: 'id', width: 80 },
                  { title: t('rule.failReason'), dataIndex: 'reason', key: 'reason' },
                ]}
              />
            </Space>
          ),
        })
      } else {
        message.success(t('rule.batchDone', { n: res.succeeded?.length ?? selectedIds.length }))
      }
      setSelectedKeys([])
      await load()
    } catch (err) {
      showApiError(err, t('rule.batchFailed'))
    }
  }

  /** 批量删除：要求输入 YES 或选中条数（规格书 9.3 强确认）。 */
  const batchDelete = () => {
    if (!selectedIds.length) {
      message.warning(t('rule.selectFirst'))
      return
    }
    const affectedUsers = new Set(
      rules.filter((r) => selectedIds.includes(r.id)).map((r) => userName(r.user_id)),
    )
    let typed = ''
    Modal.confirm({
      title: t('rule.batchDeleteTitle', { n: selectedIds.length }),
      okText: t('common.delete'),
      okButtonProps: { danger: true },
      cancelText: t('common.cancel'),
      icon: <ExclamationCircleOutlined style={{ color: 'var(--or-error)' }} />,
      width: 560,
      content: (
        <Space direction="vertical" size={8} style={{ width: '100%' }}>
          <Alert
            type="error"
            showIcon
            message={t('rule.batchDeleteDanger')}
            description={
              <Space direction="vertical" size={2}>
                <Text style={{ fontSize: 12 }}>
                  {t('rule.batchDeleteUsers', { users: [...affectedUsers].join('、') || '-' })}
                </Text>
                <Text style={{ fontSize: 12 }}>{t('rule.deleteImpactDetail')}</Text>
              </Space>
            }
          />
          <Text>{t('rule.typeToConfirm', { n: selectedIds.length })}</Text>
          <Input
            autoFocus
            placeholder="YES"
            onChange={(e) => {
              typed = e.target.value
            }}
          />
        </Space>
      ),
      onOk: async () => {
        const ok = typed.trim() === 'YES' || typed.trim() === String(selectedIds.length)
        if (!ok) {
          message.error(t('rule.confirmMismatch'))
          throw new Error('confirm mismatch')
        }
        await runBatch('delete')
      },
    })
  }

  /** 批量调分组。 */
  const batchRuleGroup = () => {
    let value: number | undefined
    Modal.confirm({
      title: t('rule.batchSetGroup'),
      content: (
        <Space direction="vertical" style={{ width: '100%' }}>
          <Text type="secondary" style={{ fontSize: 12 }}>
            {t('rule.batchSetGroupHint', { n: selectedIds.length })}
          </Text>
          <Select
            style={{ width: '100%' }}
            placeholder={t('rule.pickRuleGroup')}
            options={ruleGroups.map((g) => ({ label: g.name, value: g.id }))}
            onChange={(v: number) => {
              value = v
            }}
          />
        </Space>
      ),
      onOk: async () => {
        if (!value) {
          message.warning(t('rule.pickRuleGroup'))
          throw new Error('no value')
        }
        await runBatch('set_rule_group', { rule_group_id: value })
      },
    })
  }

  /** 批量改倍率（按值）。 */
  const batchMultiplier = () => {
    let inV = 1
    let outV = 1
    Modal.confirm({
      title: t('rule.batchSetMultiplier'),
      width: 520,
      content: (
        <Row gutter={12} style={{ marginTop: 12 }}>
          <Col span={12}>
            <Text type="secondary" style={{ fontSize: 12 }}>
              {t('rule.inMultiplier')}
            </Text>
            <InputNumber
              defaultValue={1}
              min={0}
              max={100}
              step={0.1}
              style={{ width: '100%' }}
              onChange={(v) => {
                inV = typeof v === 'number' ? v : 1
              }}
            />
          </Col>
          <Col span={12}>
            <Text type="secondary" style={{ fontSize: 12 }}>
              {t('rule.outMultiplier')}
            </Text>
            <InputNumber
              defaultValue={1}
              min={0}
              max={100}
              step={0.1}
              style={{ width: '100%' }}
              onChange={(v) => {
                outV = typeof v === 'number' ? v : 1
              }}
            />
          </Col>
        </Row>
      ),
      onOk: () => runBatch('set_multiplier', { inbound_multiplier: inV, outbound_multiplier: outV }),
    })
  }

  /** 批量按比例缩放倍率（×1.5 即输入 150%）。 */
  const batchScaleMultiplier = () => {
    let percent = 150
    Modal.confirm({
      title: t('rule.batchScaleMultiplier'),
      width: 520,
      content: (
        <Space direction="vertical" style={{ width: '100%' }} size={8}>
          <Text type="secondary" style={{ fontSize: 12 }}>
            {t('rule.batchScaleHint', { n: selectedIds.length })}
          </Text>
          <Space>
            <Text>{t('rule.scalePercent')}</Text>
            <InputNumber
              defaultValue={150}
              min={1}
              max={10000}
              addonAfter="%"
              onChange={(v) => {
                percent = typeof v === 'number' ? v : 150
              }}
            />
            <Text type="secondary">= ×{(percent / 100).toFixed(2)}</Text>
          </Space>
          <Alert type="warning" showIcon message={t('rule.batchScaleWarning')} />
        </Space>
      ),
      onOk: async () => {
        if (!selectedIds.length) {
          message.warning(t('rule.selectFirst'))
          throw new Error('empty')
        }
        try {
          const res = await ruleApi.batchMultiplier(selectedIds, percent / 100)
          message.success(t('rule.scaleDone', { n: res.updated }))
          setSelectedKeys([])
          await load()
        } catch (err) {
          showApiError(err, t('rule.batchFailed'))
          throw err
        }
      },
    })
  }

  /** 批量迁出口组。 */
  const batchOutboundGroup = () => {
    let value: number | undefined
    Modal.confirm({
      title: t('rule.batchSetOutbound'),
      content: (
        <Space direction="vertical" style={{ width: '100%' }}>
          <Text type="secondary" style={{ fontSize: 12 }}>
            {t('rule.batchSetOutboundHint', { n: selectedIds.length })}
          </Text>
          <Select
            allowClear
            style={{ width: '100%' }}
            placeholder={t('rule.singleEnd')}
            options={outboundGroups.map((g) => ({ label: g.name, value: g.id }))}
            onChange={(v: number | undefined) => {
              value = v
            }}
          />
        </Space>
      ),
      onOk: () => runBatch('set_outbound_group', { outbound_group_id: value ?? 0 }),
    })
  }

  /** 批量改限制。 */
  const batchLimits = () => {
    let speed = 0
    let conn = 0
    let ip = 0
    Modal.confirm({
      title: t('rule.batchSetLimits'),
      width: 560,
      content: (
        <Space direction="vertical" size={8} style={{ width: '100%' }}>
          <Text type="secondary" style={{ fontSize: 12 }}>
            {t('rule.batchSetLimitsHint', { n: selectedIds.length })}
          </Text>
          <Row gutter={12}>
            <Col span={8}>
              <Text style={{ fontSize: 12 }}>{t('rule.speedLimit')} (KB/s)</Text>
              <InputNumber
                defaultValue={0}
                min={0}
                style={{ width: '100%' }}
                onChange={(v) => {
                  speed = typeof v === 'number' ? v : 0
                }}
              />
            </Col>
            <Col span={8}>
              <Text style={{ fontSize: 12 }}>{t('rule.connLimit')}</Text>
              <InputNumber
                defaultValue={0}
                min={0}
                style={{ width: '100%' }}
                onChange={(v) => {
                  conn = typeof v === 'number' ? v : 0
                }}
              />
            </Col>
            <Col span={8}>
              <Text style={{ fontSize: 12 }}>{t('rule.ipLimit')}</Text>
              <InputNumber
                defaultValue={0}
                min={0}
                style={{ width: '100%' }}
                onChange={(v) => {
                  ip = typeof v === 'number' ? v : 0
                }}
              />
            </Col>
          </Row>
          <Text type="secondary" style={{ fontSize: 12 }}>
            {t('rule.udpSpeedLimitHint')}；0 表示不限制。
          </Text>
        </Space>
      ),
      onOk: () =>
        runBatch('set_limits', { speed_limit: speed, conn_limit: conn, ip_limit: ip }),
    })
  }

  /** 批量设置链式出口。 */
  const batchChain = () => {
    let chain: number[] = []
    Modal.confirm({
      title: t('rule.batchSetChain'),
      width: 560,
      content: (
        <Space direction="vertical" style={{ width: '100%' }} size={8}>
          <Text type="secondary" style={{ fontSize: 12 }}>
            {t('rule.batchSetChainHint', { n: selectedIds.length })}
          </Text>
          <Select
            mode="multiple"
            style={{ width: '100%' }}
            placeholder={t('rule.chainPlaceholder')}
            options={outboundGroups.map((g) => ({ label: g.name, value: g.id }))}
            onChange={(v: number[]) => {
              chain = v
            }}
          />
          <Alert type="warning" showIcon message={t('rule.udpNotSupported')} />
          <Alert type="warning" showIcon message={t('rule.chainMultiplierHint')} />
        </Space>
      ),
      onOk: async () => {
        if (chain.length && (chain.length < 2 || chain.length > 3)) {
          message.error(t('rule.chainLengthInvalid'))
          throw new Error('chain length')
        }
        await runBatch('set_chain', { chain_groups: chain })
      },
    })
  }

  /* ───────────────────── 表格列 ───────────────────── */

  const columns: ColumnsType<ForwardRule> = [
    {
      title: t('common.name'),
      dataIndex: 'name',
      key: 'name',
      width: 200,
      fixed: 'left',
      render: (name: string, row) => (
        <Space direction="vertical" size={0}>
          <Space size={4}>
            <Text strong>{name}</Text>
            {row.is_sub_rule && (
              <Tooltip title={`${t('rule.sni')}: ${row.sni}`}>
                <Tag color="purple" style={{ margin: 0 }}>
                  {t('rule.subRuleTag')}
                </Tag>
              </Tooltip>
            )}
            {row.chain_groups?.length > 0 && (
              <Tooltip title={t('rule.chain')}>
                <Tag color="geekblue" style={{ margin: 0 }}>
                  ×{row.chain_groups.length + 1}
                </Tag>
              </Tooltip>
            )}
            {row.reverse_enable && (
              <Tooltip title={`${t('rule.reverse')} :${row.reverse_port}`}>
                <Tag color="cyan" style={{ margin: 0 }}>
                  {t('rule.reverseShort')}
                </Tag>
              </Tooltip>
            )}
          </Space>
          {row.remark ? (
            <Text type="secondary" style={{ fontSize: 12 }}>
              {row.remark}
            </Text>
          ) : null}
        </Space>
      ),
    },
    {
      title: t('rule.owner'),
      dataIndex: 'user_id',
      key: 'user_id',
      width: 120,
      render: (v: number) => <Text>{userName(v)}</Text>,
    },
    {
      title: t('rule.ruleGroup'),
      dataIndex: 'rule_group_id',
      key: 'rule_group_id',
      width: 130,
      render: (v: number) => (v ? <Text>{ruleGroupName(v)}</Text> : <Text type="secondary">-</Text>),
    },
    {
      title: t('rule.inboundGroup'),
      dataIndex: 'inbound_group_id',
      key: 'inbound_group_id',
      width: 140,
      render: (v: number) => <Tag color="blue">{groupName(v)}</Tag>,
    },
    {
      title: t('rule.listenPort'),
      key: 'listen_port',
      width: 120,
      render: (_, row) => (
        <Text className="or-mono">
          {row.listen_port === 0
            ? t('rule.randomPort')
            : row.listen_port_end && row.listen_port_end > row.listen_port
              ? `${row.listen_port}-${row.listen_port_end}`
              : row.listen_port}
        </Text>
      ),
    },
    {
      title: t('rule.outboundGroup'),
      dataIndex: 'outbound_group_id',
      key: 'outbound_group_id',
      width: 140,
      render: (v: number) =>
        v ? <Tag color="green">{groupName(v)}</Tag> : <Tag>{t('rule.singleEnd')}</Tag>,
    },
    {
      title: t('rule.targets'),
      dataIndex: 'targets',
      key: 'targets',
      width: 220,
      render: (targets: Target[], row) => {
        if (!targets?.length) return <Text type="secondary">-</Text>
        const first = targets[0]
        return (
          <Tooltip
            title={
              <Space direction="vertical" size={0}>
                {targets.map((tg, i) => (
                  <span key={`${tg.host}-${tg.port}-${i}`}>
                    {i + 1}. {tg.host}:{tg.port}
                    {tg.status === 'down' ? ` (${t('common.disabled')})` : ''}
                  </span>
                ))}
              </Space>
            }
          >
            <Space direction="vertical" size={0}>
              <Text className="or-mono" style={{ fontSize: 12 }}>
                {first ? `${first.host}:${first.port}` : '-'}
              </Text>
              {targets.length > 1 && (
                <Text type="secondary" style={{ fontSize: 11 }}>
                  {t('rule.moreTargets', { n: targets.length - 1 })} ·{' '}
                  {targetBalanceLabel(row.target_balance, t)}
                </Text>
              )}
            </Space>
          </Tooltip>
        )
      },
    },
    {
      title: t('deviceGroup.protocol'),
      key: 'protocol',
      width: 90,
      render: (_, row) => {
        const g = groups.find((x) => x.id === row.inbound_group_id)
        const cfg = g?.config as { protocol?: string } | undefined
        return <Tag className="or-mono">{cfg?.protocol ?? '-'}</Tag>
      },
    },
    {
      title: t('rule.multiplier'),
      key: 'multiplier',
      width: 110,
      render: (_, row) => (
        <Text className="or-mono">
          {formatMultiplier(row.inbound_multiplier)} / {formatMultiplier(row.outbound_multiplier)}
        </Text>
      ),
    },
    {
      title: t('rule.trafficToday'),
      key: 'today',
      width: 110,
      render: (_, row) => <Text className="or-mono">{formatBytes(Number(row.traffic_in ?? 0))}</Text>,
    },
    {
      title: t('rule.trafficTotal'),
      key: 'total',
      width: 110,
      render: (_, row) => (
        <Text className="or-mono">
          {formatBytes(Number(row.traffic_in ?? 0) + Number(row.traffic_out ?? 0))}
        </Text>
      ),
    },
    {
      title: t('rule.syncStatus'),
      key: 'sync_status',
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
        <Switch
          size="small"
          checked={row.enable}
          onChange={(v) => void toggleEnable(row, v)}
        />
      ),
    },
    {
      title: t('common.actions'),
      key: 'actions',
      width: 150,
      fixed: 'right',
      render: (_, row) => (
        <div className="or-actions">
          <Tooltip title={t('common.edit')}>
            <Button
              size="small"
              type="text"
              icon={<EditOutlined />}
              onClick={() => void openEdit(row)}
            />
          </Tooltip>
          <Tooltip title={t('rule.resync')}>
            <Button
              size="small"
              type="text"
              icon={<SyncOutlined />}
              onClick={() => void resync(row)}
            />
          </Tooltip>
          <Tooltip title={t('common.delete')}>
            <Button
              size="small"
              type="text"
              danger
              icon={<DeleteOutlined />}
              onClick={() => removeRule(row)}
            />
          </Tooltip>
        </div>
      ),
    },
  ]

  /** 展开行：目标列表 + 实时连接数（规格书 9.2）。 */
  const expandedRowRender = (row: ForwardRule) => {
    const list = sessions[row.id] ?? []
    const busy = sessionsLoading[row.id]
    return (
      <Row gutter={16}>
        <Col span={12}>
          <Text strong style={{ fontSize: 13 }}>
            {t('rule.targets')}
          </Text>
          <Table
            style={{ marginTop: 8 }}
            size="small"
            rowKey={(r, i) => `${r.host}-${r.port}-${i}`}
            pagination={false}
            dataSource={row.targets ?? []}
            columns={[
              {
                title: '#',
                key: 'idx',
                width: 46,
                render: (_, __, i) => <Text className="or-mono">{i + 1}</Text>,
              },
              { title: t('target.host'), dataIndex: 'host', key: 'host' },
              { title: t('target.port'), dataIndex: 'port', key: 'port', width: 80 },
              { title: t('node.weight'), dataIndex: 'weight', key: 'weight', width: 70 },
              {
                title: t('common.status'),
                key: 'status',
                width: 90,
                render: (_, tg) => (
                  <Tag color={tg.status === 'down' ? 'red' : 'green'}>
                    {tg.status === 'down' ? t('common.disabled') : t('common.enabled')}
                  </Tag>
                ),
              },
            ]}
            locale={{ emptyText: <Text type="secondary">{t('target.empty')}</Text> }}
          />
        </Col>
        <Col span={12}>
          <Space style={{ justifyContent: 'space-between', width: '100%' }}>
            <Text strong style={{ fontSize: 13 }}>
              {t('rule.liveSessions')}
            </Text>
            <Space size={8}>
              <Text type="secondary" style={{ fontSize: 12 }}>
                {t('rule.connCount', { n: list.length })}
              </Text>
              <Button
                size="small"
                icon={<EyeOutlined />}
                onClick={() => void loadSessions(row.id)}
              >
                {t('common.refresh')}
              </Button>
            </Space>
          </Space>
          <Table
            style={{ marginTop: 8 }}
            size="small"
            loading={busy}
            rowKey={(r, i) => String(r.id ?? i)}
            pagination={false}
            dataSource={list}
            columns={[
              { title: t('rule.clientIp'), dataIndex: 'client_ip', key: 'client_ip', width: 130 },
              { title: t('rule.targetAddr'), dataIndex: 'target', key: 'target' },
              {
                title: t('rule.duration'),
                dataIndex: 'duration',
                key: 'duration',
                width: 90,
                render: (v: number) => <Text className="or-mono">{formatDuration(v)}</Text>,
              },
            ]}
            locale={{
              emptyText: (
                <Text type="secondary">{busy ? t('common.loading') : t('rule.noSessions')}</Text>
              ),
            }}
          />
        </Col>
      </Row>
    )
  }

  const loadSessions = async (ruleId: number) => {
    setSessionsLoading((s) => ({ ...s, [ruleId]: true }))
    try {
      // ruleApi.sessions 的返回类型来自 api 层的 Session，这里按本地结构断言使用。
      const res = (await ruleApi.sessions(ruleId)) as unknown as RuleSession[]
      setSessions((s) => ({ ...s, [ruleId]: res ?? [] }))
    } catch (err) {
      showApiError(err, t('rule.sessionsFailed'))
    } finally {
      setSessionsLoading((s) => ({ ...s, [ruleId]: false }))
    }
  }

  /** 展开时按需拉取会话，避免一次请求打满后端。 */
  const onExpand = (expanded: boolean, row: ForwardRule) => {
    setExpandedKeys(expanded ? [row.id] : [])
    if (expanded && !sessions[row.id]) {
      void loadSessions(row.id)
    }
  }

  return (
    <div>
      <div className="or-page-header">
        <div>
          <h2 className="or-page-title">{t('rule.title')}</h2>
          <p className="or-page-desc">{t('rule.pageDesc')}</p>
        </div>
        <Space wrap>
          <Button icon={<ReloadOutlined />} onClick={() => void load()}>
            {t('common.refresh')}
          </Button>
          <Button icon={<UploadOutlined />} onClick={() => setImportOpen(true)}>
            {t('common.import')}
          </Button>
          <Button
            icon={<DownloadOutlined />}
            onClick={() => void doExport()}
          >
            {t('rule.exportJson')}
          </Button>
          <Button type="primary" icon={<PlusOutlined />} onClick={openCreate}>
            {t('rule.add')}
          </Button>
        </Space>
      </div>

      {/* ── 高级筛选 ─────────────────────────────────── */}
      <Card size="small" style={{ marginBottom: 12 }}>
        <Form
          form={filterForm}
          layout="inline"
          onFinish={(v: Record<string, unknown>) => {
            setPage(1)
            setFilters({
              keyword: (v.keyword as string) || undefined,
              user_id: (v.user_id as number) || undefined,
              rule_group_id: (v.rule_group_id as number) || undefined,
              inbound_group_id: (v.inbound_group_id as number) || undefined,
              outbound_group_id: (v.outbound_group_id as number) || undefined,
              sync_status: (v.sync_status as string) || undefined,
              enable: typeof v.enable === 'boolean' ? v.enable : undefined,
            })
          }}
          style={{ rowGap: 8 }}
        >
          <Form.Item name="keyword">
            <Input.Search
              allowClear
              style={{ width: 220 }}
              placeholder={t('rule.searchPlaceholder')}
            />
          </Form.Item>
          <Form.Item name="user_id">
            <Select
              allowClear
              style={{ width: 150 }}
              placeholder={t('rule.owner')}
              options={users.map((u) => ({ label: u.username, value: u.id }))}
            />
          </Form.Item>
          <Form.Item name="rule_group_id">
            <Select
              allowClear
              style={{ width: 150 }}
              placeholder={t('rule.ruleGroup')}
              options={ruleGroups.map((g) => ({ label: g.name, value: g.id }))}
            />
          </Form.Item>
          <Form.Item name="inbound_group_id">
            <Select
              allowClear
              style={{ width: 160 }}
              placeholder={t('rule.inboundGroup')}
              options={inboundGroups.map((g) => ({ label: g.name, value: g.id }))}
            />
          </Form.Item>
          <Form.Item name="outbound_group_id">
            <Select
              allowClear
              style={{ width: 160 }}
              placeholder={t('rule.outboundGroup')}
              options={outboundGroups.map((g) => ({ label: g.name, value: g.id }))}
            />
          </Form.Item>
          <Form.Item name="sync_status">
            <Select
              allowClear
              style={{ width: 140 }}
              placeholder={t('rule.syncStatus')}
              options={[
                { label: t('rule.sync.unsynced'), value: 'unsynced' },
                { label: t('rule.sync.syncing'), value: 'syncing' },
                { label: t('rule.sync.normal'), value: 'normal' },
                { label: t('rule.sync.failed'), value: 'failed' },
              ]}
            />
          </Form.Item>
          <Form.Item name="enable">
            <Select
              allowClear
              style={{ width: 130 }}
              placeholder={t('common.status')}
              options={[
                { label: t('common.enabled'), value: true },
                { label: t('common.disabled'), value: false },
              ]}
            />
          </Form.Item>
          <Form.Item>
            <Space>
              <Button type="primary" htmlType="submit">
                {t('common.search')}
              </Button>
              <Button
                onClick={() => {
                  filterForm.resetFields()
                  setFilters({})
                  setPage(1)
                }}
              >
                {t('common.reset')}
              </Button>
            </Space>
          </Form.Item>
        </Form>
      </Card>

      {/* ── 批量操作栏 ───────────────────────────────── */}
      {selectedIds.length > 0 && (
        <Card size="small" style={{ marginBottom: 12, borderColor: 'var(--or-primary)' }}>
          <Space wrap size={8}>
            <Text strong>{t('common.selected', { n: selectedIds.length })}</Text>
            <Divider type="vertical" />
            <Button size="small" onClick={() => void runBatch('enable')}>
              {t('common.enable')}
            </Button>
            <Button size="small" onClick={() => void runBatch('disable')}>
              {t('common.disable')}
            </Button>
            <Button size="small" onClick={batchRuleGroup}>
              {t('rule.batchSetGroup')}
            </Button>
            <Button size="small" onClick={batchMultiplier}>
              {t('rule.batchSetMultiplier')}
            </Button>
            <Tooltip title={t('rule.batchScaleTooltip')}>
              <Button size="small" icon={<ThunderboltOutlined />} onClick={batchScaleMultiplier}>
                {t('rule.batchScaleMultiplier')}
              </Button>
            </Tooltip>
            <Button size="small" onClick={batchOutboundGroup}>
              {t('rule.batchSetOutbound')}
            </Button>
            <Button size="small" onClick={batchLimits}>
              {t('rule.batchSetLimits')}
            </Button>
            <Button size="small" onClick={batchChain}>
              {t('rule.batchSetChain')}
            </Button>
            <Divider type="vertical" />
            <Button size="small" danger icon={<DeleteOutlined />} onClick={batchDelete}>
              {t('common.delete')}
            </Button>
            <Button size="small" type="link" onClick={() => setSelectedKeys([])}>
              {t('common.cancel')}
            </Button>
          </Space>
        </Card>
      )}

      <Table<ForwardRule>
        rowKey="id"
        size="small"
        loading={loading}
        columns={columns}
        dataSource={rules}
        scroll={{ x: 1900 }}
        rowSelection={{
          selectedRowKeys: selectedKeys,
          onChange: setSelectedKeys,
        }}
        expandable={{
          expandedRowKeys: expandedKeys,
          onExpand,
          expandedRowRender,
        }}
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
                  <Text>{t('rule.empty')}</Text>
                  <Text type="secondary" style={{ fontSize: 12 }}>
                    {t('rule.emptyHint')}
                  </Text>
                </Space>
              }
            >
              <Space>
                <Button type="primary" icon={<PlusOutlined />} onClick={openCreate}>
                  {t('rule.add')}
                </Button>
                <Button icon={<UploadOutlined />} onClick={() => setImportOpen(true)}>
                  {t('common.import')}
                </Button>
              </Space>
            </Empty>
          ),
        }}
      />

      {/* ── 创建 / 编辑抽屉 ──────────────────────────── */}
      <Drawer
        destroyOnClose
        width={920}
        open={editorOpen}
        title={editing ? `${t('common.edit')} · ${editing.name}` : t('rule.add')}
        onClose={() => {
          setEditorOpen(false)
          setEditing(null)
        }}
      >
        <RuleEditor
          form={form}
          editing={editing}
          users={users}
          ruleGroups={ruleGroups}
          inboundGroups={inboundGroups}
          outboundGroups={outboundGroups}
          submitting={submitting}
          onSubmit={submit}
          onCancel={() => {
            setEditorOpen(false)
            setEditing(null)
          }}
          t={t}
        />
      </Drawer>

      {/* ── 导入 ─────────────────────────────────────── */}
      <ImportModal
        open={importOpen}
        onClose={() => setImportOpen(false)}
        onDone={() => {
          setImportOpen(false)
          void load()
        }}
        inboundGroups={inboundGroups}
        outboundGroups={outboundGroups}
        users={users}
        t={t}
      />
    </div>
  )

  /** 导出 JSON。 */
  async function doExport() {
    try {
      const res = await ruleApi.exportRules(filters)
      const blob = new Blob([JSON.stringify(res, null, 2)], {
        type: 'application/json;charset=utf-8',
      })
      const link = document.createElement('a')
      link.href = URL.createObjectURL(blob)
      link.download = `rules-${new Date().toISOString().slice(0, 10)}.json`
      document.body.appendChild(link)
      link.click()
      document.body.removeChild(link)
      URL.revokeObjectURL(link.href)
      message.success(t('rule.exportDone', { n: res.rules?.length ?? 0 }))
    } catch (err) {
      showApiError(err, t('rule.exportFailed'))
    }
  }
}

/* ═══════════════════════ 规则编辑表单 ═══════════════════════ */

interface RuleEditorProps {
  form: ReturnType<typeof Form.useForm<RuleFormValues>>[0]
  editing: ForwardRule | null
  users: User[]
  ruleGroups: RuleGroup[]
  inboundGroups: DeviceGroup[]
  outboundGroups: DeviceGroup[]
  submitting: boolean
  onSubmit: () => void | Promise<void>
  onCancel: () => void
  t: (key: string, vars?: Record<string, string | number>) => string
}

/** 规则创建 / 编辑表单（规格书 6.4 字段表逐项覆盖）。 */
function RuleEditor({
  form,
  editing,
  users,
  ruleGroups,
  inboundGroups,
  outboundGroups,
  submitting,
  onSubmit,
  onCancel,
  t,
}: RuleEditorProps) {
  const inboundGroupId = Form.useWatch('inbound_group_id', form) as number | undefined
  const chainGroups = Form.useWatch('chain_groups', form) as number[] | undefined
  const isSubRule = Form.useWatch('is_sub_rule', form) as boolean | undefined
  const reverseEnable = Form.useWatch('reverse_enable', form) as boolean | undefined
  const sniValue = Form.useWatch('sni', form) as string | undefined
  const targetBalance = Form.useWatch('target_balance', form) as TargetBalance | undefined

  const chainActive = Boolean(chainGroups?.length)

  /** 当前入口组的 allowed_host，用于 SNI 白名单校验（规格书 6.8）。 */
  const inboundGroup = useMemo(
    () => inboundGroups.find((g) => g.id === inboundGroupId),
    [inboundGroups, inboundGroupId],
  )
  const allowedHosts = useMemo(() => {
    const cfg = inboundGroup?.config as { allowed_host?: string[] } | undefined
    return cfg?.allowed_host ?? []
  }, [inboundGroup])
  const tlsPolicy = useMemo(() => {
    const cfg = inboundGroup?.config as { tls_inbound_policy?: number } | undefined
    return cfg?.tls_inbound_policy ?? 0
  }, [inboundGroup])

  /** SNI 是否落在白名单内（后缀匹配）。 */
  const sniError = useMemo(() => {
    if (!isSubRule) return ''
    const sni = (sniValue ?? '').trim()
    if (!sni) return t('rule.sniRequired')
    if (!isValidHost(sni)) return t('rule.sniInvalid')
    if (!allowedHosts.length) return t('rule.sniNoWhitelist')
    const hit = allowedHosts.some((h) => {
      const suffix = h.startsWith('.') ? h : `.${h}`
      return sni === h.replace(/^\./, '') || sni.endsWith(suffix)
    })
    return hit ? '' : t('rule.sniNotAllowed')
  }, [isSubRule, sniValue, allowedHosts, t])

  return (
    <Form
      form={form}
      layout="vertical"
      autoComplete="off"
      initialValues={DEFAULT_RULE_VALUES}
      onFinish={() => void onSubmit()}
    >
      {/* ── 基本信息 ─────────────────────────────────── */}
      <Card size="small" title={t('rule.basicSection')} style={{ marginBottom: 12 }}>
        <Row gutter={16}>
          <Col span={12}>
            <Form.Item
              name="name"
              label={t('common.name')}
              tooltip={t('rule.nameUniqueHint')}
              rules={[
                { required: true, message: t('rule.nameRequired') },
                { max: 64, message: t('rule.nameTooLong') },
                {
                  pattern: /^[\w.\- ]+$/,
                  message: t('rule.nameCharset'),
                },
              ]}
              extra={<Text type="secondary" style={{ fontSize: 12 }}>{t('rule.nameUniqueHint')}</Text>}
            >
              <Input placeholder="rule-hk-jp-01" />
            </Form.Item>
          </Col>
          <Col span={12}>
            <Form.Item name="user_id" label={t('rule.owner')} tooltip={t('rule.ownerHint')}>
              <Select
                allowClear
                showSearch
                optionFilterProp="label"
                placeholder={t('rule.ownerSelf')}
                options={users.map((u) => ({
                  label: `${u.username}${u.nickname ? ` (${u.nickname})` : ''}`,
                  value: u.id,
                }))}
              />
            </Form.Item>
          </Col>
          <Col span={12}>
            <Form.Item name="rule_group_id" label={t('rule.ruleGroup')}>
              <Select
                allowClear
                placeholder={t('rule.noGroup')}
                options={ruleGroups.map((g) => ({ label: g.name, value: g.id }))}
              />
            </Form.Item>
          </Col>
          <Col span={12}>
            <Form.Item
              name="enable"
              label={t('common.enable')}
              valuePropName="checked"
              tooltip={t('rule.enableHint')}
            >
              <Switch />
            </Form.Item>
          </Col>
        </Row>
      </Card>

      {/* ── 入口 ─────────────────────────────────────── */}
      <Card size="small" title={t('rule.inboundSection')} style={{ marginBottom: 12 }}>
        <Row gutter={16}>
          <Col span={12}>
            <Form.Item
              name="inbound_group_id"
              label={t('rule.inboundGroup')}
              rules={[{ required: true, message: t('rule.inboundRequired') }]}
              tooltip={t('rule.inboundHint')}
            >
              <Select
                showSearch
                optionFilterProp="label"
                placeholder={t('rule.inboundPlaceholder')}
                options={inboundGroups.map((g) => ({
                  label: `${g.name} · ${(g.config as { protocol?: string })?.protocol ?? '-'}`,
                  value: g.id,
                }))}
              />
            </Form.Item>
          </Col>
          <Col span={12}>
            <Form.Item
              name="listen_port"
              label={t('rule.listenPort')}
              rules={[
                { required: true, message: t('rule.portRequired') },
                {
                  validator: (_, v: number) => {
                    if (v === 0) return Promise.resolve()
                    if (typeof v !== 'number' || v < 1 || v > 65535) {
                      return Promise.reject(new Error(t('rule.portInvalid')))
                    }
                    return Promise.resolve()
                  },
                },
              ]}
              extra={
                <Text type="secondary" style={{ fontSize: 12 }}>
                  {t('rule.portRangeHint')}
                </Text>
              }
            >
              <InputNumber min={0} max={65535} style={{ width: '100%' }} placeholder="8443 或 0" />
            </Form.Item>
          </Col>
          <Col span={12}>
            <Form.Item
              name="listen_port_end"
              label={t('rule.portEnd')}
              tooltip={t('rule.portEndHint')}
              dependencies={['listen_port']}
              rules={[
                ({ getFieldValue }) => ({
                  validator: (_, v: number) => {
                    const start = getFieldValue('listen_port') as number
                    if (!v) return Promise.resolve()
                    if (v <= start) {
                      return Promise.reject(new Error(t('rule.portEndInvalid')))
                    }
                    if (v > 65535) {
                      return Promise.reject(new Error(t('rule.portInvalid')))
                    }
                    return Promise.resolve()
                  },
                }),
              ]}
            >
              <InputNumber
                min={1}
                max={65535}
                style={{ width: '100%' }}
                placeholder={t('rule.portEndPlaceholder')}
              />
            </Form.Item>
          </Col>
          <Col span={12}>
            <Form.Item
              name="outbound_group_id"
              label={t('rule.outboundGroup')}
              tooltip={t('rule.outboundHint')}
            >
              <Select
                allowClear
                showSearch
                optionFilterProp="label"
                placeholder={t('rule.singleEnd')}
                options={outboundGroups.map((g) => ({ label: g.name, value: g.id }))}
              />
            </Form.Item>
          </Col>
        </Row>
        {!form.getFieldValue('outbound_group_id') && (
          <Alert
            type="info"
            showIcon
            message={t('rule.singleEndHint')}
            description={<Text style={{ fontSize: 12 }}>{t('rule.singleEndHintDetail')}</Text>}
          />
        )}
      </Card>

      {/* ── 目标 ─────────────────────────────────────── */}
      <Card size="small" title={t('rule.targets')} style={{ marginBottom: 12 }}>
        <Form.Item name="target_balance" label={t('target.balance')} style={{ maxWidth: 420 }}>
          <Select options={TARGET_BALANCE_OPTIONS} />
        </Form.Item>
        <Form.Item
          name="targets"
          rules={[
            {
              validator: (_, v: Target[]) => {
                if (!v?.length) return Promise.reject(new Error(t('rule.targetsRequired')))
                const bad = v.find((x) => !x.host || !x.port)
                if (bad) return Promise.reject(new Error(t('rule.targetIncomplete')))
                return Promise.resolve()
              },
            },
          ]}
        >
          <TargetList showWeight={(targetBalance ?? 'failover') !== 'failover'} />
        </Form.Item>
      </Card>

      {/* ── 倍率与限制 ───────────────────────────────── */}
      <Card size="small" title={t('rule.limitSection')} style={{ marginBottom: 12 }}>
        <Row gutter={16}>
          <Col span={8}>
            <Form.Item
              name="inbound_multiplier"
              label={t('rule.inMultiplier')}
              tooltip={t('rule.multiplierHint')}
              rules={[
                { required: true, message: t('deviceGroup.required') },
                {
                  validator: (_, v: number) =>
                    v >= 0 && v <= 100 && hasAtMostTwoDecimals(v)
                      ? Promise.resolve()
                      : Promise.reject(new Error(t('rule.multiplierInvalid'))),
                },
              ]}
            >
              <InputNumber min={0} max={100} step={0.1} precision={2} style={{ width: '100%' }} />
            </Form.Item>
          </Col>
          <Col span={8}>
            <Form.Item
              name="outbound_multiplier"
              label={t('rule.outMultiplier')}
              tooltip={t('rule.multiplierHint')}
              rules={[
                { required: true, message: t('deviceGroup.required') },
                {
                  validator: (_, v: number) =>
                    v >= 0 && v <= 100 && hasAtMostTwoDecimals(v)
                      ? Promise.resolve()
                      : Promise.reject(new Error(t('rule.multiplierInvalid'))),
                },
              ]}
            >
              <InputNumber min={0} max={100} step={0.1} precision={2} style={{ width: '100%' }} />
            </Form.Item>
          </Col>
          <Col span={8}>
            <Form.Item label={t('rule.speedLimit')} required style={{ marginBottom: 0 }}>
              <Space.Compact style={{ width: '100%' }}>
                <Form.Item name="speed_limit" noStyle>
                  <InputNumber min={0} style={{ width: '60%' }} placeholder="0" />
                </Form.Item>
                <Form.Item name="speed_unit" noStyle>
                  <Select
                    style={{ width: '40%' }}
                    options={[
                      { label: 'KB/s', value: 'KB' },
                      { label: 'MB/s', value: 'MB' },
                      { label: 'Gbps', value: 'Gbps' },
                    ]}
                  />
                </Form.Item>
              </Space.Compact>
              <Text type="secondary" style={{ fontSize: 12 }}>
                {t('rule.udpSpeedLimitHint')}；0 = {t('rule.unlimited')}
              </Text>
            </Form.Item>
          </Col>
        </Row>
        <Row gutter={16} style={{ marginTop: 16 }}>
          <Col span={8}>
            <Form.Item name="conn_limit" label={t('rule.connLimit')} tooltip={t('rule.connLimitHint')}>
              <InputNumber min={0} style={{ width: '100%' }} placeholder="0" />
            </Form.Item>
          </Col>
          <Col span={8}>
            <Form.Item name="ip_limit" label={t('rule.ipLimit')} tooltip={t('rule.ipLimitHint')}>
              <InputNumber min={0} style={{ width: '100%' }} placeholder="0" />
            </Form.Item>
          </Col>
        </Row>
        <Alert
          type="info"
          showIcon
          message={t('rule.limitSemantics')}
          description={
            <Space direction="vertical" size={2}>
              <Text style={{ fontSize: 12 }}>{t('rule.limitSemanticsDetail')}</Text>
              <Text style={{ fontSize: 12 }}>{t('rule.speedUdpNote')}</Text>
            </Space>
          }
        />
      </Card>

      {/* ── 反向隧道 ─────────────────────────────────── */}
      <Card size="small" title={t('rule.reverse')} style={{ marginBottom: 12 }}>
        <Row gutter={16}>
          <Col span={8}>
            <Form.Item
              name="reverse_enable"
              label={t('rule.reverseEnable')}
              valuePropName="checked"
              tooltip={t('rule.reverseHint')}
            >
              <Switch />
            </Form.Item>
          </Col>
          <Col span={8}>
            <Form.Item
              name="reverse_port"
              label={t('rule.reversePort')}
              rules={[
                {
                  validator: (_, v: number) =>
                    !reverseEnable || (v >= 1 && v <= 65535)
                      ? Promise.resolve()
                      : Promise.reject(new Error(t('rule.portInvalid'))),
                },
              ]}
            >
              <InputNumber
                min={1}
                max={65535}
                disabled={!reverseEnable}
                style={{ width: '100%' }}
              />
            </Form.Item>
          </Col>
          <Col span={8}>
            <Form.Item
              name="reverse_group_id"
              label={t('rule.reverseGroup')}
              tooltip={t('rule.reverseGroupHint')}
            >
              <Select
                allowClear
                disabled={!reverseEnable}
                placeholder={t('rule.reverseGroupPlaceholder')}
                options={outboundGroups.map((g) => ({ label: g.name, value: g.id }))}
              />
            </Form.Item>
          </Col>
        </Row>
        {reverseEnable && (
          <Alert type="info" showIcon message={t('rule.reverseDetail')} />
        )}
      </Card>

      {/* ── 链式出口 ─────────────────────────────────── */}
      <Card size="small" title={t('rule.chain')} style={{ marginBottom: 12 }}>
        <Form.Item
          name="chain_groups"
          label={t('rule.chainGroups')}
          tooltip={t('rule.chainHint')}
          rules={[
            {
              validator: (_, v: number[]) => {
                if (!v?.length) return Promise.resolve()
                if (v.length < 2 || v.length > 3) {
                  return Promise.reject(new Error(t('rule.chainLengthInvalid')))
                }
                return Promise.resolve()
              },
            },
          ]}
        >
          <Select
            mode="multiple"
            placeholder={t('rule.chainPlaceholder')}
            options={outboundGroups.map((g) => ({ label: g.name, value: g.id }))}
          />
        </Form.Item>

        {chainActive && (
          <Space direction="vertical" style={{ width: '100%' }} size={8}>
            <Alert
              type="warning"
              showIcon
              message={t('rule.udpNotSupported')}
              description={
                <Text style={{ fontSize: 12 }}>{t('rule.udpNotSupportedDetail')}</Text>
              }
            />
            <Alert
              type="warning"
              showIcon
              message={t('rule.chainFailoverDisabled')}
              description={
                <Text style={{ fontSize: 12 }}>{t('rule.chainFailoverDisabledDetail')}</Text>
              }
            />
            <Alert
              type="info"
              showIcon
              message={t('rule.chainMultiplierHint')}
            />
            <Alert type="info" showIcon message={t('rule.chainMuxHint')} />
          </Space>
        )}
      </Card>

      {/* ── SNI 子规则 ───────────────────────────────── */}
      <Card size="small" title={t('rule.sniSection')} style={{ marginBottom: 12 }}>
        <Row gutter={16}>
          <Col span={8}>
            <Form.Item
              name="is_sub_rule"
              label={t('rule.isSubRule')}
              valuePropName="checked"
              tooltip={t('rule.isSubRuleHint')}
            >
              <Switch />
            </Form.Item>
          </Col>
          {isSubRule && (
            <Col span={16}>
              <Form.Item
                name="sni"
                label={t('rule.sni')}
                validateStatus={sniError ? 'error' : undefined}
                help={sniError || t('rule.sniHint')}
                rules={[{ required: true, message: t('rule.sniRequired') }]}
              >
                <Input className="or-mono" placeholder="sub.example.com" />
              </Form.Item>
            </Col>
          )}
        </Row>

        {isSubRule && (
          <Space direction="vertical" style={{ width: '100%' }} size={8}>
            <Alert
              type="info"
              showIcon
              message={t('rule.sniFlow')}
              description={
                <Space direction="vertical" size={2}>
                  <Text style={{ fontSize: 12 }}>{t('rule.sniFlowDetail')}</Text>
                  <Text style={{ fontSize: 12 }}>
                    {t('rule.sniPortFixed', { port: SNI_SHARED_PORT })}
                  </Text>
                </Space>
              }
            />
            {tlsPolicy !== 2 && (
              <Alert
                type="error"
                showIcon
                message={t('rule.sniPolicyMismatch')}
                description={
                  <Text style={{ fontSize: 12 }}>{t('rule.sniPolicyMismatchDetail')}</Text>
                }
              />
            )}
            {allowedHosts.length > 0 && (
              <Alert
                type="success"
                showIcon
                message={t('rule.sniWhitelist', { hosts: allowedHosts.join(', ') })}
              />
            )}
            <Alert type="warning" showIcon message={t('deviceGroup.vlessWarning')} />
          </Space>
        )}
      </Card>

      {/* ── 高级选项 ─────────────────────────────────── */}
      <Card size="small" title={t('rule.advancedSection')} style={{ marginBottom: 12 }}>
        <Form.Item
          name="options_text"
          label="options (JSON)"
          tooltip={t('rule.optionsHint')}
          extra={
            <Text type="secondary" style={{ fontSize: 12 }}>
              {t('rule.optionsHint')}
            </Text>
          }
        >
          {/* JsonEditor 与 Form.Item 通过 value/onChange 对接 */}
          <JsonEditor rows={6} placeholder={'{\n  "keep_alive": true\n}'} />
        </Form.Item>
      </Card>

      {/* ── 备注 ─────────────────────────────────────── */}
      <Card size="small" title={t('common.remark')} style={{ marginBottom: 12 }}>
        <Form.Item name="remark" noStyle>
          <Input.TextArea rows={2} placeholder={t('rule.remarkPlaceholder')} />
        </Form.Item>
      </Card>

      <Space>
        <Button
          type="primary"
          loading={submitting}
          disabled={Boolean(sniError) && Boolean(isSubRule)}
          onClick={() => void onSubmit()}
        >
          {t('common.save')}
        </Button>
        <Button onClick={onCancel}>{t('common.cancel')}</Button>
        {editing && (
          <Text type="secondary" style={{ fontSize: 12 }}>
            {t('rule.editingId', { id: editing.id })}
          </Text>
        )}
      </Space>
    </Form>
  )
}

/* ═══════════════════════ 导入弹窗 ═══════════════════════ */

interface ImportModalProps {
  open: boolean
  onClose: () => void
  onDone: () => void
  inboundGroups: DeviceGroup[]
  outboundGroups: DeviceGroup[]
  users: User[]
  t: (key: string, vars?: Record<string, string | number>) => string
}

/**
 * 规则导入（规格书 8.9、6.4）。
 *
 * 两步式：先 `preview=true` 拿 `{total, will_create, conflicts, invalid}`，
 * 用表格把冲突原因与建议、非法行的行号与原文本列清楚，
 * 用户确认后才用 `preview=false` 真正落库。
 */
function ImportModal({
  open,
  onClose,
  onDone,
  inboundGroups,
  outboundGroups,
  users,
  t,
}: ImportModalProps) {
  const [format, setFormat] = useState<'text' | 'json'>('text')
  const [content, setContent] = useState('')
  const [preview, setPreview] = useState<ImportPreview | null>(null)
  const [previewLoading, setPreviewLoading] = useState(false)
  const [importing, setImporting] = useState(false)
  const [inboundId, setInboundId] = useState<number | undefined>()
  const [outboundId, setOutboundId] = useState<number | undefined>()
  const [userId, setUserId] = useState<number | undefined>()

  useEffect(() => {
    if (!open) {
      setPreview(null)
      setContent('')
      setFormat('text')
      setInboundId(undefined)
      setOutboundId(undefined)
      setUserId(undefined)
    }
  }, [open])

  /** 自动识别格式：以 `{` / `[` 开头即认为用户粘的是 JSON。 */
  const guessedFormat = useMemo<'text' | 'json'>(() => {
    const head = content.trim().slice(0, 1)
    return head === '{' || head === '[' ? 'json' : 'text'
  }, [content])

  const doPreview = async () => {
    if (!content.trim()) {
      message.warning(t('rule.importEmpty'))
      return
    }
    setPreviewLoading(true)
    try {
      const res = await ruleApi.importRules(
        {
          format: guessedFormat,
          content,
          inbound_group_id: inboundId,
          outbound_group_id: outboundId,
          user_id: userId,
        },
        true,
      )
      setPreview(res as ImportPreview)
    } catch (err) {
      showApiError(err, t('rule.importPreviewFailed'))
    } finally {
      setPreviewLoading(false)
    }
  }

  const doImport = async () => {
    setImporting(true)
    try {
      await ruleApi.importRules(
        {
          format: guessedFormat,
          content,
          inbound_group_id: inboundId,
          outbound_group_id: outboundId,
          user_id: userId,
        },
        false,
      )
      message.success(t('rule.importDone'))
      onDone()
    } catch (err) {
      showApiError(err, t('rule.importFailed'))
    } finally {
      setImporting(false)
    }
  }

  /** 读取本地文件填充内容。 */
  const readFile = (file: File) => {
    const reader = new FileReader()
    reader.onload = () => setContent(String(reader.result ?? ''))
    reader.onerror = () => message.error(t('rule.fileReadFailed'))
    reader.readAsText(file, 'utf-8')
    return false
  }

  return (
    <Modal
      open={open}
      width={880}
      title={t('rule.importTitle')}
      onCancel={onClose}
      footer={
        <Space>
          <Button onClick={onClose}>{t('common.cancel')}</Button>
          <Button loading={previewLoading} onClick={() => void doPreview()}>
            {t('rule.preview')}
          </Button>
          <Button
            type="primary"
            loading={importing}
            disabled={!preview || preview.will_create <= 0}
            onClick={() => void doImport()}
          >
            {t('rule.confirmImport', { n: preview?.will_create ?? 0 })}
          </Button>
        </Space>
      }
    >
      <Tabs
        activeKey={format}
        onChange={(k) => setFormat(k as 'text' | 'json')}
        items={[
          {
            key: 'text',
            label: t('rule.textFormat'),
            children: (
              <Space direction="vertical" style={{ width: '100%' }} size={8}>
                <Alert
                  type="info"
                  showIcon
                  message={t('rule.textFormatHint')}
                  description={
                    <pre className="or-mono" style={{ margin: 0, fontSize: 12 }}>
                      {'规则名#监听端口#目标地址#目标端口\n规则名##目标地址#目标端口'}
                    </pre>
                  }
                />
                <Text type="secondary" style={{ fontSize: 12 }}>
                  {t('rule.textFormatDetected', {
                    format: guessedFormat === 'json' ? 'JSON' : t('rule.textFormat'),
                  })}
                </Text>
              </Space>
            ),
          },
          {
            key: 'json',
            label: t('rule.jsonFormat'),
            children: (
              <Space direction="vertical" style={{ width: '100%' }} size={8}>
                <Alert
                  type="info"
                  showIcon
                  message={t('rule.jsonFormatHint')}
                  description={
                    <pre className="or-mono" style={{ margin: 0, fontSize: 12 }}>
                      {`{\n  "name": "my-rule",\n  "listen_port": 0,\n  "targets": [{ "host": "1.2.3.4", "port": 443, "weight": 1 }],\n  "inbound_group_id": 1,\n  "outbound_group_id": 2,\n  "inbound_multiplier": 1.5,\n  "outbound_multiplier": 0.5,\n  "speed_limit": 0,\n  "remark": ""\n}`}
                    </pre>
                  }
                />
                <Text type="secondary" style={{ fontSize: 12 }}>
                  {t('rule.jsonMultipleHint')}
                </Text>
              </Space>
            ),
          },
        ]}
      />

      <Row gutter={12} style={{ marginTop: 12 }}>
        <Col span={8}>
          <Text style={{ fontSize: 12 }}>{t('rule.defaultInbound')}</Text>
          <Select
            allowClear
            style={{ width: '100%' }}
            value={inboundId}
            placeholder={t('rule.textMayOmit')}
            options={inboundGroups.map((g) => ({ label: g.name, value: g.id }))}
            onChange={(v: number | undefined) => setInboundId(v)}
          />
        </Col>
        <Col span={8}>
          <Text style={{ fontSize: 12 }}>{t('rule.defaultOutbound')}</Text>
          <Select
            allowClear
            style={{ width: '100%' }}
            value={outboundId}
            placeholder={t('rule.singleEnd')}
            options={outboundGroups.map((g) => ({ label: g.name, value: g.id }))}
            onChange={(v: number | undefined) => setOutboundId(v)}
          />
        </Col>
        <Col span={8}>
          <Text style={{ fontSize: 12 }}>{t('rule.defaultUser')}</Text>
          <Select
            allowClear
            style={{ width: '100%' }}
            value={userId}
            placeholder={t('rule.ownerSelf')}
            options={users.map((u) => ({ label: u.username, value: u.id }))}
            onChange={(v: number | undefined) => setUserId(v)}
          />
        </Col>
      </Row>

      <Input.TextArea
        className="or-mono"
        rows={8}
        style={{ marginTop: 12 }}
        value={content}
        placeholder={t('rule.pastePlaceholder')}
        onChange={(e) => setContent(e.target.value)}
      />

      <Space style={{ marginTop: 8 }}>
        <Upload beforeUpload={readFile} showUploadList={false} accept=".txt,.json,.csv">
          <Button size="small" icon={<CloudUploadOutlined />}>
            {t('rule.pickFile')}
          </Button>
        </Upload>
        <Text type="secondary" style={{ fontSize: 12 }}>
          {t('rule.pasteOrFile')}
        </Text>
      </Space>

      {preview && (
        <div style={{ marginTop: 16 }}>
          <Divider orientation="left" plain>
            {t('rule.previewResult')}
          </Divider>
          <Space size={16} wrap>
            <Tag color="blue">
              {t('rule.previewTotal')}: {preview.total}
            </Tag>
            <Tag color="green">
              {t('rule.previewCreate')}: {preview.will_create}
            </Tag>
            <Tag color="orange">
              {t('rule.previewConflict')}: {preview.conflicts?.length ?? 0}
            </Tag>
            <Tag color="red">{t('rule.previewInvalid')}: {preview.invalid?.length ?? 0}</Tag>
          </Space>

          {preview.conflicts?.length > 0 && (
            <div style={{ marginTop: 12 }}>
              <Text strong style={{ fontSize: 13 }}>
                {t('rule.conflictList')}
              </Text>
              <Table
                style={{ marginTop: 8 }}
                size="small"
                rowKey={(r) => `c-${r.line}-${r.name}`}
                pagination={false}
                dataSource={preview.conflicts}
                columns={[
                  { title: t('rule.lineNo'), dataIndex: 'line', key: 'line', width: 70 },
                  { title: t('common.name'), dataIndex: 'name', key: 'name', width: 180 },
                  { title: t('rule.reason'), dataIndex: 'reason', key: 'reason' },
                  { title: t('rule.suggestion'), dataIndex: 'suggestion', key: 'suggestion' },
                ]}
              />
            </div>
          )}

          {preview.invalid?.length > 0 && (
            <div style={{ marginTop: 12 }}>
              <Text strong style={{ fontSize: 13 }}>
                {t('rule.invalidList')}
              </Text>
              <Table
                style={{ marginTop: 8 }}
                size="small"
                rowKey={(r) => `i-${r.line}-${r.raw}`}
                pagination={false}
                dataSource={preview.invalid}
                columns={[
                  { title: t('rule.lineNo'), dataIndex: 'line', key: 'line', width: 70 },
                  {
                    title: t('rule.rawLine'),
                    dataIndex: 'raw',
                    key: 'raw',
                    render: (v: string) => (
                      <Text className="or-mono" style={{ fontSize: 12 }}>
                        {v}
                      </Text>
                    ),
                  },
                  { title: t('rule.reason'), dataIndex: 'reason', key: 'reason', width: 260 },
                ]}
              />
            </div>
          )}

          {preview.conflicts?.length === 0 && preview.invalid?.length === 0 && (
            <Alert
              style={{ marginTop: 12 }}
              type="success"
              showIcon
              message={t('rule.previewClean')}
            />
          )}

          <Alert
            style={{ marginTop: 12 }}
            type="warning"
            showIcon
            message={t('rule.previewNotWritten')}
          />
        </div>
      )}
    </Modal>
  )
}

/* ═══════════════════════ 工具函数 ═══════════════════════ */

/** 表单默认值（规格书 6.4：倍率默认 1）。 */
const DEFAULT_RULE_VALUES: Partial<RuleFormValues> = {
  listen_port: 0,
  targets: [],
  target_balance: 'failover',
  inbound_multiplier: 1,
  outbound_multiplier: 1,
  speed_limit: 0,
  speed_unit: 'KB',
  conn_limit: 0,
  ip_limit: 0,
  reverse_enable: false,
  chain_groups: [],
  is_sub_rule: false,
  enable: true,
}

/** 规则 → 表单值。 */
function toFormValues(rule: ForwardRule): RuleFormValues {
  return {
    name: rule.name,
    user_id: rule.user_id || undefined,
    rule_group_id: rule.rule_group_id || undefined,
    inbound_group_id: rule.inbound_group_id,
    listen_port: rule.listen_port,
    listen_port_end: rule.listen_port_end || undefined,
    outbound_group_id: rule.outbound_group_id || undefined,
    targets: rule.targets ?? [],
    target_balance: rule.target_balance ?? 'failover',
    inbound_multiplier: Number(rule.inbound_multiplier ?? 1),
    outbound_multiplier: Number(rule.outbound_multiplier ?? 1),
    speed_limit: Number(rule.speed_limit ?? 0),
    // 编辑时统一以 KB/s 展示，用户可自行切换单位。
    speed_unit: 'KB',
    conn_limit: Number(rule.conn_limit ?? 0),
    ip_limit: Number(rule.ip_limit ?? 0),
    reverse_enable: Boolean(rule.reverse_enable),
    reverse_port: rule.reverse_port || undefined,
    reverse_group_id: rule.reverse_group_id || undefined,
    chain_groups: rule.chain_groups ?? [],
    is_sub_rule: Boolean(rule.is_sub_rule),
    sni: rule.sni ?? '',
    enable: rule.enable ?? true,
    remark: rule.remark ?? '',
    options_text:
      rule.options && Object.keys(rule.options).length
        ? JSON.stringify(rule.options, null, 2)
        : '',
  }
}

/** 倍率是否最多两位小数（规格书 6.4）。 */
function hasAtMostTwoDecimals(v: number): boolean {
  if (typeof v !== 'number' || !Number.isFinite(v)) return false
  return Math.round(v * 100) === Number((v * 100).toFixed(4)) * 1
}

/** 倍率展示：去掉多余的小数位。 */
function formatMultiplier(v: number): string {
  const n = Number(v ?? 1)
  return Number.isInteger(n) ? String(n) : n.toFixed(2).replace(/0+$/, '').replace(/\.$/, '')
}

/** 字节数 → 人类可读。 */
function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes <= 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB']
  let value = bytes
  let i = 0
  while (value >= 1024 && i < units.length - 1) {
    value /= 1024
    i += 1
  }
  return `${value.toFixed(value >= 100 || i === 0 ? 0 : 2)} ${units[i]}`
}

/** 秒 → 时长文本。 */
function formatDuration(sec: number | undefined): string {
  if (!sec || sec <= 0) return '-'
  if (sec < 60) return `${Math.round(sec)}s`
  if (sec < 3600) return `${Math.floor(sec / 60)}m${Math.round(sec % 60)}s`
  return `${Math.floor(sec / 3600)}h${Math.floor((sec % 3600) / 60)}m`
}

/** 目标负载均衡策略文案。 */
function targetBalanceLabel(v: TargetBalance, t: (k: string) => string): string {
  const found = TARGET_BALANCE_OPTIONS.find((o) => o.value === v)
  return found ? found.label.split(' · ')[0] : t('common.none')
}
