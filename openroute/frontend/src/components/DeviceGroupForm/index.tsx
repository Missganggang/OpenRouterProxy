/**
 * 设备组动态表单（规格书 6.3、4.3、4.4、8.8）。
 *
 * 设计要点：
 *  1. **schema 驱动 + 硬编码兜底**：优先用 `deviceGroupApi.schemaByType(type)`
 *     渲染字段，schema 接口不可用（旧后端 / 网络异常）时退回到内置字段表，
 *     保证表单永远可用。
 *  2. **协议子配置单列**：`ws` / `tls` 是嵌套对象，字段名带点号无法直接用
 *     Form.Item 的 `name` 表达，因此单独用 `ws_*` / `tls_*` 平铺字段承接，
 *     提交时在 `buildConfig` 里组装回嵌套结构。
 *  3. **把规格书里的风险点做成 Alert**：入口组不做面板侧负载均衡（6.3）、
 *     IPv6 优先无回退（4.3）、SNI 整流与 VLESS 系协议不兼容（6.8）、
 *     链式出口不支持 UDP（6.7）。
 */
import { useCallback, useEffect, useMemo, useState } from 'react'
import {
  Alert,
  Button,
  Card,
  Col,
  Collapse,
  Divider,
  Form,
  Input,
  InputNumber,
  Modal,
  Row,
  Select,
  Space,
  Switch,
  Tag,
  Tooltip,
  Typography,
} from 'antd'
import { ExclamationCircleOutlined } from '@ant-design/icons'

import { deviceGroupApi, nodeApi, showApiError } from '../../api'
import type { FieldSchema, DeviceGroupSchema } from '../../api/modules/deviceGroup'
import type {
  Balance,
  Chfp,
  DeviceGroup,
  DeviceGroupType,
  InboundConfig,
  Node,
  OutboundConfig,
  TunnelProtocol,
} from '../../api/types'
import { useI18n } from '../../locales'

const { Text, Paragraph } = Typography

/** 表单值：平铺结构，协议子配置用前缀区分。 */
export interface DeviceGroupFormValues {
  name: string
  remark?: string
  node_ids: number[]
  // 通用
  protocol: TunnelProtocol
  udp_over_tcp: boolean
  // ── 入口组 ──
  allowed_host?: string[]
  blocked_host?: string[]
  blocked_path?: string[]
  blocked_protocol?: string[]
  tls_inbound_policy?: 0 | 1 | 2
  tls_reject_empty_sni?: boolean
  disable_udp?: boolean
  ipv6_group?: number[]
  max_fail?: number
  fail_timout_sec?: number
  reverse_group?: number[]
  // ── 出口组 ──
  connect_type?: 'dyn_ip4' | 'dyn_ip6' | 'static'
  connect_address?: string
  connect_port?: number
  balance?: Balance
  health_check_enable?: boolean
  health_check_interval?: number
  health_check_timeout?: number
  health_check_fail_count?: number
  health_check_succ_count?: number
  failover_group_id?: number
  // ── 协议子配置（平铺） ──
  ws_host?: string
  ws_path?: string
  ws_request?: string
  ws_response?: string
  tls_sni?: string
  tls_alpn?: string[]
  tls_chfp?: string
  /** SNI 整流选项（规格书 6.8 的 Shape_*）。 */
  shaping?: number[]
}

interface Props {
  /** 编辑已有组时传入；新建时为 undefined。 */
  group?: DeviceGroup | null
  /** 组类型（新建时由外部 Tab 决定）。 */
  type: DeviceGroupType
  /** 全部设备组，用于故障转移组与反向组选择。 */
  allGroups: DeviceGroup[]
  /** 提交中。 */
  submitting?: boolean
  onSubmit: (values: {
    name: string
    type: DeviceGroupType
    node_ids: number[]
    config: InboundConfig | OutboundConfig
    balance?: Balance
    health_check_enable?: boolean
    health_check_interval?: number
    health_check_timeout?: number
    health_check_fail_count?: number
    health_check_succ_count?: number
    failover_group_id?: number
    remark?: string
  }) => Promise<void> | void
  onCancel: () => void
}

/** 协议选项（规格书 4.5）。 */
const PROTOCOLS: Array<{ label: string; value: TunnelProtocol }> = [
  { label: 'tls', value: 'tls' },
  { label: 'ws', value: 'ws' },
  { label: 'http', value: 'http' },
  { label: 'direct（入口直出 / 不加密）', value: 'direct' },
]

/** 可选 TLS 指纹（规格书 4.5 / 附录 C）。 */
const CHFP_OPTIONS = [
  'chrome',
  'firefox',
  'safari',
  'ios',
  'android',
  'edge',
  '360',
  'qq',
].map((v) => ({ label: v, value: v }))

/** SNI 整流选项（规格书 6.8 整流选项表）。 */
const SHAPING_OPTIONS = [
  { label: 'Shape_RejectTLS12（拒绝 TLS 1.2）', value: 1 },
  { label: 'Shape_RejectTLS（拒绝全部 TLS 握手）', value: 2 },
  { label: 'Shape_RejectHttp1（拒绝 HTTP/1.x）', value: 3 },
  { label: 'Shape_RejectWs（拒绝 WebSocket 升级）', value: 4 },
  { label: 'Shape_SkipIPv6（跳过 IPv6 目标）', value: 2000 },
]

// 入口组的 IPv6 优先策略（规格书 4.3 的 ipv6_group）语义特殊：
// 空数组 = 不启用；[0] = 所有出口优先 IPv6；[0,1,2] = 所有出口优先 IPv6，
// 但节点 1、2 优先 IPv4。它不是「从固定选项里选」而是「填一组节点 ID」，
// 因此这里用可自由输入的标签选择控件（Select mode="tags"）呈现，
// 形如 [0]、[0,1,2]，并在下方给出无回退机制的风险提示。

/** 被阻止的入站协议类型（规格书 4.3）。 */
const BLOCKED_PROTOCOL_OPTIONS = ['socks', 'fet', 'http', 'tls'].map((v) => ({
  label: v,
  value: v,
}))

/**
 * 内置兜底字段表。
 *
 * 内容与规格书 4.3（入口）/ 4.4（出口）逐项对齐；
 * schema 接口正常时这份表不会用到。
 */
const FALLBACK_FIELDS: Record<DeviceGroupType, FieldSchema[]> = {
  inbound: [
    {
      key: 'allowed_host',
      label: '允许的目标域名（allowed_host）',
      type: 'array',
      default: [],
      help: '后缀匹配；空数组 = 不限制。SNI 分流时此处即 SNI 白名单。',
    },
    {
      key: 'blocked_host',
      label: '禁止的目标域名（blocked_host）',
      type: 'array',
      default: [],
      help: '后缀匹配，优先级高于白名单。',
    },
    {
      key: 'blocked_path',
      label: '禁止的路径前缀（blocked_path）',
      type: 'array',
      default: [],
    },
    {
      key: 'blocked_protocol',
      label: '禁止的入站协议（blocked_protocol）',
      type: 'select',
      default: [],
      options: BLOCKED_PROTOCOL_OPTIONS.map((o) => ({ label: o.label, value: o.value })),
    },
    {
      key: 'tls_inbound_policy',
      label: 'TLS 入站策略（tls_inbound_policy）',
      type: 'select',
      default: 0,
      options: [
        { label: '0 · 不处理', value: 0 },
        { label: '1 · 强制剥离 TLS', value: 1 },
        { label: '2 · SNI 分流', value: 2 },
      ],
      risk: true,
    },
    {
      key: 'tls_reject_empty_sni',
      label: '拒绝无 SNI 的握手（tls_reject_empty_sni）',
      type: 'boolean',
      default: false,
    },
    { key: 'disable_udp', label: '完全关闭 UDP（disable_udp）', type: 'boolean', default: false },
    { key: 'udp_over_tcp', label: 'UDP over TCP（udp_over_tcp）', type: 'boolean', default: false },
    {
      key: 'ipv6_group',
      label: 'IPv6 优先策略（ipv6_group）',
      type: 'array',
      default: [],
      risk: true,
    },
    { key: 'max_fail', label: '连续失败阈值（max_fail）', type: 'number', default: 3, min: 1 },
    {
      key: 'fail_timout_sec',
      label: '故障判定超时（fail_timout_sec）',
      type: 'number',
      default: 30,
      min: 1,
    },
    {
      key: 'reverse_group',
      label: '反向隧道出口组（reverse_group）',
      type: 'array',
      default: [],
      help: '出口主动连接入口（规格书 6.6），需对应节点已配置 is-outbound。',
    },
  ],
  outbound: [
    {
      key: 'connect_type',
      label: '连接方式（connect_type）',
      type: 'select',
      default: 'static',
      options: [
        { label: 'dyn_ip4 · 动态公网 IPv4', value: 'dyn_ip4' },
        { label: 'dyn_ip6 · 动态公网 IPv6', value: 'dyn_ip6' },
        { label: 'static · 静态指定', value: 'static' },
      ],
    },
    {
      key: 'connect_address',
      label: '静态连接地址（connect_address）',
      type: 'string',
      default: '',
      help: '域名或 IP；双机专线时填对端内网 IP（规格书 6.5）。',
    },
    {
      key: 'connect_port',
      label: '静态连接端口（connect_port）',
      type: 'number',
      default: 0,
      min: 0,
      max: 65535,
    },
    { key: 'udp_over_tcp', label: 'UDP over TCP（udp_over_tcp）', type: 'boolean', default: false },
  ],
}

export default function DeviceGroupForm({
  group,
  type,
  allGroups,
  submitting = false,
  onSubmit,
  onCancel,
}: Props) {
  const { t } = useI18n()
  const [form] = Form.useForm<DeviceGroupFormValues>()

  const [schemaFields, setSchemaFields] = useState<FieldSchema[]>(FALLBACK_FIELDS[type])
  const [schemaFromApi, setSchemaFromApi] = useState(false)
  const [nodes, setNodes] = useState<Node[]>([])
  const [loadingNodes, setLoadingNodes] = useState(false)
  /**
   * IPv6 优先策略的显式确认状态（规格书 4.3：IPv6 优先没有回退）。
   * 开启前必须让用户确认「目标不可达时不会自动回退 IPv4」，
   * 取消确认则清空该字段，避免误开。
   */
  const [ipv6Confirmed, setIpv6Confirmed] = useState(false)

  /** 当前表单里的协议与 UDP 开关，用于联动禁用与提示。 */
  const protocol = Form.useWatch('protocol', form) as TunnelProtocol | undefined
  const tlsPolicy = Form.useWatch('tls_inbound_policy', form) as 0 | 1 | 2 | undefined
  const blockingUdp = Form.useWatch('disable_udp', form) as boolean | undefined
  const udpOverTcp = Form.useWatch('udp_over_tcp', form) as boolean | undefined
  const connectType = Form.useWatch('connect_type', form) as string | undefined
  const healthEnable = Form.useWatch('health_check_enable', form) as boolean | undefined

  // 节点列表：需要展示角色，便于发现「入口组选了出口节点」这类错配。
  useEffect(() => {
    let alive = true
    setLoadingNodes(true)
    nodeApi
      .list({ page: 1, page_size: 500 })
      .then((res) => {
        if (alive) setNodes(res.items)
      })
      .catch((err) => showApiError(err, t('deviceGroup.loadNodesFailed')))
      .finally(() => {
        if (alive) setLoadingNodes(false)
      })
    return () => {
      alive = false
    }
  }, [t])

  // 按类型拉取 schema；失败时保留兜底字段表，不打断用户。
  useEffect(() => {
    let alive = true
    deviceGroupApi
      .schemaByType(type)
      .then((s: DeviceGroupSchema) => {
        if (!alive) return
        if (s?.fields?.length) {
          setSchemaFields(s.fields)
          setSchemaFromApi(true)
        } else {
          setSchemaFields(FALLBACK_FIELDS[type])
          setSchemaFromApi(false)
        }
      })
      .catch(() => {
        if (!alive) return
        setSchemaFields(FALLBACK_FIELDS[type])
        setSchemaFromApi(false)
      })
    return () => {
      alive = false
    }
  }, [type])

  /** 把组配置拆成平铺的表单初值。 */
  const initialValues = useMemo<DeviceGroupFormValues>(() => {
    const cfg = (group?.config ?? {}) as Partial<InboundConfig & OutboundConfig>
    const proto: TunnelProtocol = (cfg.protocol as TunnelProtocol) ?? 'tls'
    const ws = cfg.ws ?? {}
    const tls = cfg.tls ?? {}
    return {
      name: group?.name ?? '',
      remark: group?.remark ?? '',
      node_ids: group?.node_ids ?? [],
      protocol: proto,
      udp_over_tcp: Boolean(cfg.udp_over_tcp),
      // 入口组
      allowed_host: cfg.allowed_host ?? [],
      blocked_host: cfg.blocked_host ?? [],
      blocked_path: cfg.blocked_path ?? [],
      blocked_protocol: cfg.blocked_protocol ?? [],
      tls_inbound_policy: cfg.tls_inbound_policy ?? 0,
      tls_reject_empty_sni: Boolean(cfg.tls_reject_empty_sni),
      disable_udp: Boolean(cfg.disable_udp),
      ipv6_group: cfg.ipv6_group ?? [],
      max_fail: cfg.max_fail ?? 3,
      fail_timout_sec: cfg.fail_timout_sec ?? 30,
      reverse_group: cfg.reverse_group ?? [],
      // 出口组
      connect_type: cfg.connect_type ?? 'static',
      connect_address: cfg.connect_address ?? '',
      connect_port: cfg.connect_port ?? 0,
      balance: group?.balance ?? 'least_conn',
      health_check_enable: group?.health_check_enable ?? true,
      health_check_interval: group?.health_check_interval ?? 10,
      health_check_timeout: group?.health_check_timeout ?? 3,
      health_check_fail_count: group?.health_check_fail_count ?? 3,
      health_check_succ_count: group?.health_check_succ_count ?? 2,
      failover_group_id: group?.failover_group_id ?? 0,
      // 协议子配置
      ws_host: ws.host ?? '',
      ws_path: ws.path ?? '',
      ws_request: ws.request ?? '',
      ws_response: ws.response ?? '',
      tls_sni: tls.sni ?? '',
      tls_alpn: tls.alpn ?? [],
      tls_chfp: tls.chfp ?? 'chrome',
    }
  }, [group])

  // 切换编辑对象时重置表单。
  useEffect(() => {
    form.setFieldsValue(initialValues)
  }, [form, initialValues])

  /** 组装提交用的 config。 */
  const buildConfig = useCallback(
    (v: DeviceGroupFormValues): InboundConfig | OutboundConfig => {
      // ws 与 http 复用同一份配置（规格书附录 C）。
      const ws =
        v.protocol === 'ws' || v.protocol === 'http'
          ? {
              host: v.ws_host || undefined,
              path: v.ws_path || undefined,
              request: v.ws_request || undefined,
              response: v.ws_response || undefined,
            }
          : undefined
      const tls =
        v.protocol === 'tls'
          ? {
              sni: v.tls_sni || undefined,
              alpn: v.tls_alpn?.length ? v.tls_alpn : undefined,
              chfp: (v.tls_chfp as Chfp | undefined) || undefined,
            }
          : undefined

      if (type === 'inbound') {
        return {
          allowed_host: v.allowed_host ?? [],
          blocked_host: v.blocked_host ?? [],
          blocked_path: v.blocked_path ?? [],
          blocked_protocol: v.blocked_protocol ?? [],
          tls_inbound_policy: (v.tls_inbound_policy ?? 0) as 0 | 1 | 2,
          tls_reject_empty_sni: Boolean(v.tls_reject_empty_sni),
          disable_udp: Boolean(v.disable_udp),
          udp_over_tcp: Boolean(v.udp_over_tcp),
          ipv6_group: (v.ipv6_group ?? []).map((n) => Number(n)),
          max_fail: v.max_fail ?? 3,
          fail_timout_sec: v.fail_timout_sec ?? 30,
          reverse_group: (v.reverse_group ?? []).map((n) => Number(n)),
          protocol: v.protocol,
          ws,
          tls,
        } as InboundConfig
      }
      return {
        connect_type: v.connect_type ?? 'static',
        connect_address: v.connect_address || undefined,
        connect_port: v.connect_port || undefined,
        protocol: v.protocol,
        ws,
        tls,
        udp_over_tcp: Boolean(v.udp_over_tcp),
      } as OutboundConfig
    },
    [type],
  )

  /**
   * ipv6_group 变更拦截：从空变为非空时必须先确认「没有回退」。
   *
   * Select 的 onChange 先把值写进表单（antd 内部已改），
   * 这里在用户点「取消」时把值清回，避免出现「界面看着开了、实际没确认」的假象。
   */
  const onIpv6Change = (next: Array<string | number>) => {
    const wasEmpty = !ipv6Value?.length
    const nowEmpty = !next.length
    if (nowEmpty) {
      setIpv6Confirmed(false)
      return
    }
    if (wasEmpty && !ipv6Confirmed) {
      Modal.confirm({
        title: t('deviceGroup.ipv6ConfirmTitle'),
        icon: <ExclamationCircleOutlined style={{ color: 'var(--or-warning)' }} />,
        content: (
          <Space direction="vertical" size={6}>
            <Text>{t('deviceGroup.ipv6Warning')}</Text>
            <Text type="secondary" style={{ fontSize: 12 }}>
              {t('deviceGroup.ipv6WarningDetail')}
            </Text>
          </Space>
        ),
        okText: t('deviceGroup.ipv6ConfirmOk'),
        cancelText: t('common.cancel'),
        onOk: () => setIpv6Confirmed(true),
        onCancel: () => form.setFieldValue('ipv6_group', []),
      })
    }
  }

  const submit = async () => {
    let values: DeviceGroupFormValues
    try {
      values = await form.validateFields()
    } catch {
      // 校验失败时 antd 已经高亮了出错字段。
      return
    }
    const config = buildConfig(values)
    await onSubmit({
      name: values.name.trim(),
      type,
      node_ids: values.node_ids ?? [],
      config,
      ...(type === 'outbound'
        ? {
            balance: values.balance ?? 'least_conn',
            health_check_enable: Boolean(values.health_check_enable),
            health_check_interval: values.health_check_interval ?? 10,
            health_check_timeout: values.health_check_timeout ?? 3,
            health_check_fail_count: values.health_check_fail_count ?? 3,
            health_check_succ_count: values.health_check_succ_count ?? 2,
            failover_group_id: values.failover_group_id ?? 0,
          }
        : {}),
      remark: values.remark ?? '',
    })
  }

  /** 可选节点：按类型过滤，但保留当前已选节点，避免编辑时被清空。 */
  const nodeOptions = useMemo(() => {
    const want: Node['role'][] = type === 'inbound' ? ['inbound', 'both'] : ['outbound', 'both']
    return nodes
      .filter((n) => want.includes(n.role) || (group?.node_ids ?? []).includes(n.id))
      .map((n) => ({
        value: n.id,
        label: `${n.name} · ${nodeRoleLabel(n.role, t)} · ${n.online ? t('node.online') : t('node.offline')}${n.public_ipv4 ? ` · ${n.public_ipv4}` : ''}`,
      }))
  }, [nodes, type, group, t])

  /** 选中的节点里存在角色不匹配的情况（入口组选了纯出口节点）。 */
  const roleMismatch = useMemo(() => {
    const want: Node['role'][] = type === 'inbound' ? ['inbound', 'both'] : ['outbound', 'both']
    const selected = form.getFieldValue('node_ids') as number[] | undefined
    if (!selected?.length) return []
    return nodes.filter((n) => selected.includes(n.id) && !want.includes(n.role))
  }, [nodes, type, form, initialValues])

  /** 其它同类型组：可作为故障转移组 / 反向组。 */
  const siblingGroups = useMemo(
    () => allGroups.filter((g) => g.id !== group?.id && g.type === type),
    [allGroups, group, type],
  )

  // ipv6_group 非空时，在表单里给出醒目的「无回退」提示。
  const ipv6Value = Form.useWatch('ipv6_group', form) as number[] | undefined
  const ipv6Enabled = Boolean(ipv6Value?.length)
  /** 按 schema 条目渲染一个字段。 */
  const renderField = (f: FieldSchema) => {
    const label = (
      <Space size={4}>
        <span>{f.label}</span>
        {f.risk && (
          <Tooltip title={f.help || t('deviceGroup.riskField')}>
            <ExclamationCircleOutlined style={{ color: 'var(--or-warning)' }} />
          </Tooltip>
        )}
      </Space>
    )
    const help = f.help ? <Text type="secondary" style={{ fontSize: 12 }}>{f.help}</Text> : undefined

    if (f.type === 'boolean') {
      return (
        <Col span={12} key={f.key}>
          <Form.Item
            name={f.key}
            label={label}
            valuePropName="checked"
            tooltip={f.help}
            initialValue={Boolean(f.default)}
          >
            <Switch />
          </Form.Item>
        </Col>
      )
    }

    if (f.type === 'number') {
      return (
        <Col span={12} key={f.key}>
          <Form.Item name={f.key} label={label} tooltip={f.help} initialValue={f.default}>
            <InputNumber min={f.min} max={f.max} style={{ width: '100%' }} />
          </Form.Item>
        </Col>
      )
    }

    if (f.type === 'select') {
      return (
        <Col span={12} key={f.key}>
          <Form.Item name={f.key} label={label} tooltip={f.help} initialValue={f.default}>
            <Select
              options={(f.options ?? []).map((o) => ({
                label: String(o.label),
                value: o.value as string | number,
              }))}
              allowClear
            />
          </Form.Item>
        </Col>
      )
    }

    if (f.type === 'array') {
      // 数组字段一律用 tags 模式：允许自由输入，也允许粘贴逗号分隔的批量值。
      return (
        <Col span={24} key={f.key}>
          <Form.Item
            name={f.key}
            label={label}
            tooltip={f.help}
            initialValue={f.default ?? []}
            extra={help}
          >
            <Select
              mode="tags"
              open={false}
              tokenSeparators={[',', ' ', '\n']}
              placeholder={t('deviceGroup.arrayPlaceholder')}
              style={{ width: '100%' }}
              // ipv6_group 需要显式确认（规格书 4.3：无回退）
              onChange={
                f.key === 'ipv6_group'
                  ? (v: Array<string | number>) => onIpv6Change(v)
                  : undefined
              }
            />
          </Form.Item>
        </Col>
      )
    }

    if (f.type === 'text') {
      return (
        <Col span={24} key={f.key}>
          <Form.Item name={f.key} label={label} tooltip={f.help} initialValue={f.default}>
            <Input.TextArea rows={3} className="or-mono" />
          </Form.Item>
        </Col>
      )
    }

    // string / object 兜底按字符串处理。
    return (
      <Col span={12} key={f.key}>
        <Form.Item name={f.key} label={label} tooltip={f.help} initialValue={f.default ?? ''}>
          <Input />
        </Form.Item>
      </Col>
    )
  }

  return (
    <Form
      form={form}
      layout="vertical"
      initialValues={initialValues}
      autoComplete="off"
      onFinish={submit}
    >
      {/* ── 基本信息 ────────────────────────────────────── */}
      <Card size="small" title={t('deviceGroup.basicSection')} style={{ marginBottom: 12 }}>
        <Row gutter={16}>
          <Col span={12}>
            <Form.Item
              name="name"
              label={t('common.name')}
              rules={[
                { required: true, message: t('deviceGroup.nameRequired') },
                { max: 64, message: t('deviceGroup.nameTooLong') },
              ]}
            >
              <Input placeholder={type === 'inbound' ? 'HK-In' : 'JP-Out'} />
            </Form.Item>
          </Col>
          <Col span={12}>
            <Form.Item
              name="node_ids"
              label={t('deviceGroup.nodeCount')}
              rules={[
                { required: true, message: t('deviceGroup.nodeRequired') },
                { type: 'array', min: 1, message: t('deviceGroup.nodeRequired') },
              ]}
            >
              <Select
                mode="multiple"
                loading={loadingNodes}
                options={nodeOptions}
                placeholder={t('deviceGroup.nodePlaceholder')}
                optionFilterProp="label"
              />
            </Form.Item>
          </Col>
        </Row>

        {roleMismatch.length > 0 && (
          <Alert
            type="error"
            showIcon
            style={{ marginBottom: 12 }}
            message={t('deviceGroup.roleMismatch')}
            description={
              <Space direction="vertical" size={2}>
                {roleMismatch.map((n) => (
                  <Text key={n.id} style={{ fontSize: 12 }}>
                    {n.name} · {nodeRoleLabel(n.role, t)}
                  </Text>
                ))}
                <Text type="secondary" style={{ fontSize: 12 }}>
                  {t('deviceGroup.roleMismatchHint')}
                </Text>
              </Space>
            }
          />
        )}

        {group && (
          <Alert
            type="warning"
            showIcon
            message={t('deviceGroup.configChangeWarning')}
            description={<Text style={{ fontSize: 12 }}>{t('deviceGroup.configChangeWarningHint')}</Text>}
          />
        )}
      </Card>

      {/* ── 协议 ────────────────────────────────────────── */}
      <Card size="small" title={t('deviceGroup.protocolSection')} style={{ marginBottom: 12 }}>
        <Row gutter={16}>
          <Col span={12}>
            <Form.Item
              name="protocol"
              label={t('deviceGroup.protocol')}
              rules={[{ required: true }]}
              tooltip={t('deviceGroup.protocolHint')}
            >
              <Select options={PROTOCOLS} />
            </Form.Item>
          </Col>
          <Col span={12}>
            <Form.Item
              name="udp_over_tcp"
              label={t('deviceGroup.udpOverTcp')}
              valuePropName="checked"
              tooltip={t('deviceGroup.udpOverTcpHint')}
            >
              <Switch />
            </Form.Item>
          </Col>
        </Row>

        {/* ws / http 子配置 */}
        {(protocol === 'ws' || protocol === 'http') && (
          <div>
            <Divider orientation="left" plain style={{ marginTop: 0 }}>
              {protocol} {t('deviceGroup.subConfig')}
            </Divider>
            <Row gutter={16}>
              <Col span={12}>
                <Form.Item
                  name="ws_host"
                  label="ws.host"
                  tooltip={t('deviceGroup.wsHostHint')}
                >
                  <Input placeholder="cdn.example.com" />
                </Form.Item>
              </Col>
              <Col span={12}>
                <Form.Item name="ws_path" label="ws.path">
                  <Input placeholder="/ws" />
                </Form.Item>
              </Col>
              <Col span={12}>
                <Form.Item
                  name="ws_request"
                  label="ws.request"
                  tooltip={t('deviceGroup.wsRawHint')}
                >
                  <Input.TextArea rows={2} className="or-mono" placeholder={'GET / HTTP/1.5\r\n\r\n'} />
                </Form.Item>
              </Col>
              <Col span={12}>
                <Form.Item name="ws_response" label="ws.response">
                  <Input.TextArea
                    rows={2}
                    className="or-mono"
                    placeholder={'HTTP/1.5 200 OK\r\n\r\n'}
                  />
                </Form.Item>
              </Col>
            </Row>
          </div>
        )}

        {/* tls 子配置 */}
        {protocol === 'tls' && (
          <div>
            <Divider orientation="left" plain style={{ marginTop: 0 }}>
              tls {t('deviceGroup.subConfig')}
            </Divider>
            <Row gutter={16}>
              <Col span={8}>
                <Form.Item name="tls_sni" label="tls.sni">
                  <Input placeholder="some.host.com" />
                </Form.Item>
              </Col>
              <Col span={8}>
                <Form.Item name="tls_alpn" label="tls.alpn" extra={t('deviceGroup.alpnHint')}>
                  <Select
                    mode="tags"
                    open={false}
                    tokenSeparators={[',', ' ']}
                    placeholder="http/1.1, h2"
                  />
                </Form.Item>
              </Col>
              <Col span={8}>
                <Form.Item name="tls_chfp" label="tls.chfp" tooltip={t('deviceGroup.chfpHint')}>
                  <Select options={CHFP_OPTIONS} allowClear />
                </Form.Item>
              </Col>
            </Row>
          </div>
        )}

        {protocol === 'direct' && (
          <Alert
            type="info"
            showIcon
            style={{ marginTop: 4 }}
            message={t('deviceGroup.directHint')}
            description={<Text style={{ fontSize: 12 }}>{t('deviceGroup.directHintDetail')}</Text>}
          />
        )}
      </Card>

      {/* ── 入口组专属 ──────────────────────────────────── */}
      {type === 'inbound' && (
        <Card
          size="small"
          title={t('deviceGroup.inboundSection')}
          style={{ marginBottom: 12 }}
          extra={
            <Tag color={schemaFromApi ? 'blue' : 'default'}>
              {schemaFromApi ? t('deviceGroup.schemaFromApi') : t('deviceGroup.schemaFallback')}
            </Tag>
          }
        >
          {/* 规格书 6.3：入口组不做面板侧负载均衡，MUST 在 UI 上明确提示 */}
          <Alert
            type="info"
            showIcon
            style={{ marginBottom: 12 }}
            message={t('deviceGroup.inboundNoBalance')}
            description={
              <Text style={{ fontSize: 12 }}>{t('deviceGroup.inboundNoBalanceDetail')}</Text>
            }
          />

          {tlsPolicy === 2 && (
            <Alert
              type="warning"
              showIcon
              style={{ marginBottom: 12 }}
              message={t('deviceGroup.sniWarning')}
              description={
                <Space direction="vertical" size={2}>
                  <Text style={{ fontSize: 12 }}>{t('deviceGroup.sniSteps')}</Text>
                  <Text type="danger" style={{ fontSize: 12 }}>
                    {t('deviceGroup.vlessWarning')}
                  </Text>
                </Space>
              }
            />
          )}

          {ipv6Enabled && (
            /* 规格书 4.3：IPv6 优先没有回退，需显式确认 */
            <Alert
              type="warning"
              showIcon
              style={{ marginBottom: 12 }}
              message={t('deviceGroup.ipv6Warning')}
              description={
                <Text style={{ fontSize: 12 }}>{t('deviceGroup.ipv6WarningDetail')}</Text>
              }
            />
          )}

          <Row gutter={16}>
            {schemaFields.map((f) => renderField(f))}
          </Row>

          {/* SNI 整流选项：仅 SNI 分流策略下有意义 */}
          {tlsPolicy === 2 && (
            <>
              <Divider orientation="left" plain>
                {t('deviceGroup.shapingSection')}
              </Divider>
              <Alert
                type="error"
                showIcon
                style={{ marginBottom: 12 }}
                message={t('deviceGroup.vlessWarning')}
                description={
                  <Text style={{ fontSize: 12 }}>{t('deviceGroup.shapingHint')}</Text>
                }
              />
              <Form.Item name="shaping" label={t('deviceGroup.shaping')}>
                <Select
                  mode="multiple"
                  options={SHAPING_OPTIONS}
                  placeholder={t('deviceGroup.shapingPlaceholder')}
                />
              </Form.Item>
            </>
          )}
        </Card>
      )}

      {/* ── 出口组专属 ──────────────────────────────────── */}
      {type === 'outbound' && (
        <Card
          size="small"
          title={t('deviceGroup.outboundSection')}
          style={{ marginBottom: 12 }}
          extra={
            <Tag color={schemaFromApi ? 'blue' : 'default'}>
              {schemaFromApi ? t('deviceGroup.schemaFromApi') : t('deviceGroup.schemaFallback')}
            </Tag>
          }
        >
          <Row gutter={16}>
            {schemaFields.map((f) => renderField(f))}
          </Row>

          {connectType === 'static' && (
            <Alert
              type="info"
              showIcon
              style={{ marginBottom: 12 }}
              message={t('deviceGroup.staticConnectHint')}
            />
          )}
          {connectType === 'dyn_ip4' || connectType === 'dyn_ip6' ? (
            <Alert
              type="info"
              showIcon
              style={{ marginBottom: 12 }}
              message={t('deviceGroup.dynamicConnectHint')}
              description={
                <Text style={{ fontSize: 12 }}>{t('deviceGroup.dynamicConnectHintDetail')}</Text>
              }
            />
          ) : null}

          <Divider orientation="left" plain>
            {t('deviceGroup.balanceSection')}
          </Divider>
          <Row gutter={16}>
            <Col span={12}>
              <Form.Item
                name="balance"
                label={t('deviceGroup.balance')}
                tooltip={t('deviceGroup.balanceHint')}
                rules={[{ required: true }]}
              >
                <Select
                  options={[
                    { label: 'least_conn · 最少连接（默认）', value: 'least_conn' },
                    { label: 'round_robin · 平滑加权轮询', value: 'round_robin' },
                    { label: 'hash_ip · 客户端 IP 一致性哈希', value: 'hash_ip' },
                    { label: 'weighted · 纯权重随机', value: 'weighted' },
                  ]}
                />
              </Form.Item>
            </Col>
            <Col span={12}>
              <Form.Item
                name="failover_group_id"
                label={t('deviceGroup.failover')}
                tooltip={t('deviceGroup.failoverHint')}
              >
                <Select
                  allowClear
                  placeholder={t('deviceGroup.noFailover')}
                  options={siblingGroups.map((g) => ({ label: g.name, value: g.id }))}
                />
              </Form.Item>
            </Col>
          </Row>

          {/* 规格书 6.3：故障转移组必须有一致的入口权限配置 */}
          <Alert
            type="info"
            showIcon
            style={{ marginBottom: 12 }}
            message={t('deviceGroup.failoverCompat')}
            description={
              <Space direction="vertical" size={2}>
                <Text style={{ fontSize: 12 }}>{t('deviceGroup.failoverCompatDetail')}</Text>
                <Text type="secondary" style={{ fontSize: 12 }}>
                  {t('deviceGroup.failoverDelayHint')}
                </Text>
              </Space>
            }
          />

          <Divider orientation="left" plain>
            {t('deviceGroup.healthCheckSection')}
          </Divider>
          <Row gutter={16}>
            <Col span={8}>
              <Form.Item
                name="health_check_enable"
                label={t('deviceGroup.healthCheckEnable')}
                valuePropName="checked"
                tooltip={t('deviceGroup.healthCheckHint')}
              >
                <Switch />
              </Form.Item>
            </Col>
            <Col span={8}>
              <Form.Item
                name="health_check_interval"
                label={t('deviceGroup.healthInterval')}
                rules={[{ required: healthEnable, message: t('deviceGroup.required') }]}
              >
                <InputNumber min={1} max={3600} disabled={!healthEnable} style={{ width: '100%' }} />
              </Form.Item>
            </Col>
            <Col span={8}>
              <Form.Item
                name="health_check_timeout"
                label={t('deviceGroup.healthTimeout')}
                rules={[{ required: healthEnable, message: t('deviceGroup.required') }]}
              >
                <InputNumber min={1} max={600} disabled={!healthEnable} style={{ width: '100%' }} />
              </Form.Item>
            </Col>
            <Col span={8}>
              <Form.Item
                name="health_check_fail_count"
                label={t('deviceGroup.healthFailCount')}
                tooltip={t('deviceGroup.healthFailHint')}
              >
                <InputNumber min={1} max={100} style={{ width: '100%' }} />
              </Form.Item>
            </Col>
            <Col span={8}>
              <Form.Item
                name="health_check_succ_count"
                label={t('deviceGroup.healthSuccCount')}
                tooltip={t('deviceGroup.healthSuccHint')}
              >
                <InputNumber min={1} max={100} style={{ width: '100%' }} />
              </Form.Item>
            </Col>
          </Row>
        </Card>
      )}

      {/* ── 备注 ────────────────────────────────────────── */}
      <Card size="small" title={t('common.remark')} style={{ marginBottom: 12 }}>
        <Form.Item name="remark" noStyle>
          <Input.TextArea rows={2} placeholder={t('deviceGroup.remarkPlaceholder')} />
        </Form.Item>
      </Card>

      {/* 规格书 6.7：链式出口相关的限制提示（本表单只需告知 UDP 场景） */}
      <Collapse
        ghost
        size="small"
        items={[
          {
            key: 'notes',
            label: <Text type="secondary">{t('deviceGroup.moreNotes')}</Text>,
            children: (
              <Space direction="vertical" size={4}>
                <Paragraph style={{ fontSize: 12, margin: 0 }}>
                  · {t('rule.udpNotSupported')}
                </Paragraph>
                <Paragraph style={{ fontSize: 12, margin: 0 }}>
                  · {t('deviceGroup.chainUdpHint')}
                </Paragraph>
                <Paragraph style={{ fontSize: 12, margin: 0 }}>
                  · {t('rule.udpSpeedLimitHint')}
                </Paragraph>
                <Paragraph style={{ fontSize: 12, margin: 0 }}>
                  · {t('deviceGroup.reverseHint')}
                </Paragraph>
              </Space>
            ),
          },
        ]}
      />

      <Space style={{ marginTop: 12 }}>
        <Button type="primary" loading={submitting} onClick={() => void submit()}>
          {t('common.save')}
        </Button>
        <Button onClick={onCancel}>{t('common.cancel')}</Button>
        {blockingUdp && udpOverTcp && (
          <Text type="secondary" style={{ fontSize: 12 }}>
            {t('deviceGroup.disableUdpOverrides')}
          </Text>
        )}
      </Space>
    </Form>
  )
}

/** 节点角色文案。 */
function nodeRoleLabel(role: Node['role'], t: (k: string) => string): string {
  if (role === 'inbound') return t('node.role.inbound')
  if (role === 'outbound') return t('node.role.outbound')
  return t('node.role.both')
}

