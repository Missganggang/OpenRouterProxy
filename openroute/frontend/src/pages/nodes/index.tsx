/**
 * 节点管理列表（规格书 6.1 / 9.2）。
 *
 * 关键点：
 *  - 在线状态走「5 秒轮询 + WebSocket 事件」双保险：轮询保证最终一致，
 *    WS 事件让上下线在一秒内反映到表格上（规格书 6.1 要求实时刷新）；
 *  - 一键对接：创建成功后立刻弹出安装命令弹窗（可复制 + 二维码）；
 *  - 删除、重置密钥、批量操作都给出影响范围，不用「确定吗？」这种无信息量的确认。
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useNavigate, useSearchParams } from 'react-router-dom'
import {
  Alert,
  Button,
  Checkbox,
  Col,
  Divider,
  Empty,
  Form,
  Input,
  InputNumber,
  Modal,
  Popconfirm,
  Popover,
  Progress,
  Row,
  Select,
  Space,
  Table,
  Tag,
  Tooltip,
  Typography,
  message,
} from 'antd'
import type { ColumnsType } from 'antd/es/table'
import {
  AudioOutlined,
  CloudDownloadOutlined,
  CopyOutlined,
  DeleteOutlined,
  EditOutlined,
  KeyOutlined,
  PlusOutlined,
  PoweroffOutlined,
  RedoOutlined,
  ReloadOutlined,
  SettingOutlined,
  SyncOutlined,
  WarningOutlined,
} from '@ant-design/icons'

import { nodeApi, nodeGroupApi, showApiError } from '../../api'
import type { Node, NodeGroup, NodeRole } from '../../api/types'
import type { NodeCreateInput } from '../../api/modules/node'
import { usePolling } from '../../hooks/usePolling'
import { useWebSocket } from '../../hooks/useWebSocket'
import { useI18n } from '../../locales'
import CopyableCommand from '../../components/CopyableCommand'
import { formatBytes } from '../../components/TrafficChart'
import { healthColor } from '../../components/NodeCard'

const { Text } = Typography

/** 表格一次拉取的最大行数：节点数量是「几十台」量级，单页拉全更利于筛选与批量操作。 */
const PAGE_SIZE = 100

const LISTENER_PORTS = [
  ['direct_port', 'direct', 28080],
  ['ws_port', 'ws / http', 28081],
  ['tls_port', 'tls', 28082],
  ['udp_port', 'udp', 28083],
  ['rev_port', 'reverse', 28084],
] as const

/** 可自定义显示的列。 */
const ALL_COLUMNS = [
  'role',
  'groups',
  'ip',
  'status',
  'last_seen',
  'cpu',
  'mem',
  'disk',
  'conn',
  'speed',
  'version',
  'health',
  'drift',
] as const

type ColumnKey = (typeof ALL_COLUMNS)[number]

/** 批量操作接口的统一返回形状。 */
interface BatchResult {
  succeeded?: number[]
  failed?: Array<{ id: number; reason: string }>
  updated?: number
}

export default function NodesPage() {
  const navigate = useNavigate()
  const { t } = useI18n()
  const [searchParams, setSearchParams] = useSearchParams()

  const [form] = Form.useForm<NodeCreateInput>()

  // ── 筛选条件 ────────────────────────────────────────────────────────────
  const [keyword, setKeyword] = useState('')
  const [groupFilter, setGroupFilter] = useState<number | undefined>()
  const [roleFilter, setRoleFilter] = useState<NodeRole | undefined>()
  const [onlineFilter, setOnlineFilter] = useState<boolean | undefined>(() => {
    const v = searchParams.get('online')
    return v === 'false' ? false : v === 'true' ? true : undefined
  })
  const [page, setPage] = useState(1)

  const [visibleColumns, setVisibleColumns] = useState<ColumnKey[]>([...ALL_COLUMNS])
  const [selectedKeys, setSelectedKeys] = useState<React.Key[]>([])

  // ── 弹窗状态 ────────────────────────────────────────────────────────────
  const [formOpen, setFormOpen] = useState(false)
  const [editing, setEditing] = useState<Node | null>(null)
  const [submitting, setSubmitting] = useState(false)
  const [installModal, setInstallModal] = useState<{ node: Node; command: string } | null>(null)
  const [batchGroupOpen, setBatchGroupOpen] = useState(false)
  const [batchWeightOpen, setBatchWeightOpen] = useState(false)
  const [batchGroupIds, setBatchGroupIds] = useState<number[]>([])
  const [batchWeight, setBatchWeight] = useState<number>(1)
  const [running, setRunning] = useState(false)

  // ── 数据 ────────────────────────────────────────────────────────────────
  const { data, loading, refresh } = usePolling(
    () =>
      nodeApi.list({
        page,
        page_size: PAGE_SIZE,
        keyword: keyword || undefined,
        group_id: groupFilter,
        role: roleFilter,
        online: onlineFilter,
      }),
    { interval: 5000 },
  )

  const { data: groups } = usePolling(() => nodeGroupApi.list({ page_size: 200 }), {
    interval: 60000,
  })

  const rows = data?.items ?? []
  const total = data?.pagination.total ?? 0

  // WS：节点上下线事件到达后立刻补一次列表，让状态点实时变化（规格书 6.1）。
  const refreshRef = useRef(refresh)
  refreshRef.current = refresh
  const wsHandled = useRef(false)
  const { status: wsStatus } = useWebSocket('/api/v1/nodes/stream', {
    onEvent: (ev) => {
      if (ev.type === 'node_online' || ev.type === 'node_offline') {
        wsHandled.current = true
        void refreshRef.current()
      }
    },
  })

  // 首次进入时由 URL 触发创建弹窗（仪表盘 / 空状态的引导按钮）。
  useEffect(() => {
    if (searchParams.get('action') === 'create') {
      openCreate()
      const next = new URLSearchParams(searchParams)
      next.delete('action')
      setSearchParams(next, { replace: true })
    }
    // 仅在挂载时读取一次；后续由用户主动操作。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const groupName = useCallback(
    (id: number) => groups?.items.find((g) => g.id === id)?.name ?? `#${id}`,
    [groups],
  )

  /** 打开「新建节点」。 */
  function openCreate() {
    setEditing(null)
    form.resetFields()
    form.setFieldsValue({ role: 'both', weight: 1, max_conn: 0, group_ids: [], direct_port: 0, ws_port: 0, tls_port: 0, udp_port: 0, rev_port: 0 })
    setFormOpen(true)
  }

  /** 打开「编辑节点」。 */
  function openEdit(node: Node) {
    setEditing(node)
    form.setFieldsValue({
      name: node.name,
      role: node.role,
      group_ids: node.group_ids ?? [],
      weight: node.weight,
      max_conn: node.max_conn,
      connect_host: node.connect_host,
      is_static: node.is_static,
      direct_port: node.direct_port,
      ws_port: node.ws_port,
      tls_port: node.tls_port,
      udp_port: node.udp_port,
      rev_port: node.rev_port,
      remark: node.remark,
    })
    setFormOpen(true)
  }

  /** 提交新建 / 编辑。 */
  async function submit() {
    let values: NodeCreateInput
    try {
      values = await form.validateFields()
    } catch {
      return
    }
    setSubmitting(true)
    try {
      if (editing) {
        await nodeApi.update(editing.id, values)
        message.success(t('common.save'))
        setFormOpen(false)
        await refresh()
      } else {
        const created = await nodeApi.create(values)
        setFormOpen(false)
        // 一键对接：创建成功立刻把安装命令摆到用户面前。
        setInstallModal({
          node: {
            ...(rows.find((r) => r.id === created.id) ?? ({} as Node)),
            id: created.id,
            name: created.name,
          } as Node,
          command: created.install_command,
        })
        await refresh()
      }
    } catch (err) {
      showApiError(err)
    } finally {
      setSubmitting(false)
    }
  }

  /** 拉取（重新生成）安装命令。 */
  async function fetchInstall(node: Node) {
    try {
      const res = await nodeApi.installCommand(node.id)
      setInstallModal({ node, command: res.install_command })
    } catch (err) {
      showApiError(err)
    }
  }

  /** 升级单个节点。 */
  function confirmUpgrade(node: Node) {
    Modal.confirm({
      title: t('node.upgradeConfirm', { name: node.name }),
      okText: t('common.ok'),
      cancelText: t('common.cancel'),
      onOk: async () => {
        try {
          await nodeApi.upgrade(node.id)
          message.success(t('common.save'))
          void refresh()
        } catch (err) {
          showApiError(err)
          throw err
        }
      },
    })
  }

  /** 重启单个节点。 */
  function confirmRestart(node: Node) {
    Modal.confirm({
      title: t('node.restartConfirm', { name: node.name }),
      okText: t('common.ok'),
      cancelText: t('common.cancel'),
      onOk: async () => {
        try {
          await nodeApi.restart(node.id)
          message.success(t('common.save'))
          void refresh()
        } catch (err) {
          showApiError(err)
          throw err
        }
      },
    })
  }

  /** 重置节点密钥。 */
  function confirmResetToken(node: Node) {
    Modal.confirm({
      title: t('node.resetTokenConfirm', { name: node.name }),
      content: t('node.resetTokenImpact'),
      okText: t('common.ok'),
      okButtonProps: { danger: true },
      cancelText: t('common.cancel'),
      onOk: async () => {
        try {
          const res = await nodeApi.resetToken(node.id)
          Modal.info({
            title: t('node.newToken'),
            width: 560,
            content: <CopyableCommand command={res.token} showQrcode={false} multiline />,
            okText: t('common.ok'),
          })
        } catch (err) {
          showApiError(err)
          throw err
        }
      },
    })
  }

  /** 删除节点。 */
  function confirmDelete(node: Node) {
    Modal.confirm({
      title: t('node.deleteConfirm', { name: node.name }),
      content: t('node.deleteImpact'),
      okText: t('common.delete'),
      okButtonProps: { danger: true },
      cancelText: t('common.cancel'),
      onOk: async () => {
        try {
          await nodeApi.remove(node.id)
          message.success(t('common.delete'))
          setSelectedKeys((keys) => keys.filter((k) => k !== node.id))
          await refresh()
        } catch (err) {
          showApiError(err)
          throw err
        }
      },
    })
  }

  /** 批量操作统一的结果提示。 */
  function reportBatchResult(res: BatchResult) {
    const ok = (res.succeeded?.length ?? 0) + (res.updated ?? 0)
    const fail = res.failed?.length ?? 0
    const text = t('node.batchResult', { ok, fail })
    if (fail > 0) {
      Modal.warning({
        title: text,
        content: (
          <ul style={{ paddingInlineStart: 18 }}>
            {res.failed?.map((f) => (
              <li key={f.id}>
                <Text code>#{f.id}</Text> {f.reason}
              </li>
            ))}
          </ul>
        ),
      })
    } else {
      message.success(text)
    }
  }

  const selectedIds = useMemo(
    () => selectedKeys.map((k) => Number(k)).filter((n) => Number.isFinite(n)),
    [selectedKeys],
  )

  /** 批量升级 / 批量重启。 */
  async function runBatch(exec: (ids: number[]) => Promise<BatchResult>) {
    if (selectedIds.length === 0) return
    setRunning(true)
    try {
      const res = await exec(selectedIds)
      reportBatchResult(res)
      void refresh()
    } catch (err) {
      showApiError(err)
    } finally {
      setRunning(false)
    }
  }

  /** 批量停用节点的转发能力；保留监控连接。 */
  async function batchDisable() {
    if (selectedIds.length === 0) return
    setRunning(true)
    try {
      const settled = await Promise.allSettled(
        selectedIds.map((id) => nodeApi.update(id, { disabled: true })),
      )
      const succeeded: number[] = []
      const failed: Array<{ id: number; reason: string }> = []
      settled.forEach((r, i) => {
        if (r.status === 'fulfilled') succeeded.push(selectedIds[i])
        else
          failed.push({
            id: selectedIds[i],
            reason: (r.reason as Error)?.message ?? String(r.reason),
          })
      })
      reportBatchResult({ succeeded, failed })
      await refresh()
    } catch (err) {
      showApiError(err)
    } finally {
      setRunning(false)
    }
  }

  // ── 表格列 ──────────────────────────────────────────────────────────────
  const columns: ColumnsType<Node> = useMemo(() => {
    const all: Record<ColumnKey | 'name' | 'actions', ColumnsType<Node>[number]> = {
      name: {
        title: t('common.name'),
        dataIndex: 'name',
        fixed: 'left',
        width: 200,
        render: (_v, node) => (
          <Space size={6} align="center">
            <span
              className={node.online ? 'or-dot or-dot-online' : 'or-dot or-dot-offline'}
              aria-hidden
            />
            <div style={{ minWidth: 0 }}>
              <div>
                <a onClick={() => navigate(`/nodes/${node.id}`)} style={{ fontWeight: 500 }}>
                  {node.name}
                </a>
                {node.drift_detected && (
                  <Tooltip title={t('node.drift')}>
                    <WarningOutlined
                      style={{ color: 'var(--or-warning)', marginInlineStart: 6 }}
                    />
                  </Tooltip>
                )}
              </div>
              {!node.online && node.last_error && (
                <Tooltip title={node.last_error}>
                  <Text type="danger" style={{ fontSize: 12 }} ellipsis>
                    {node.last_error}
                  </Text>
                </Tooltip>
              )}
            </div>
          </Space>
        ),
      },
      role: {
        title: t('node.role'),
        dataIndex: 'role',
        width: 90,
        render: (role: NodeRole) => <Tag>{t(`node.role.${role}`)}</Tag>,
      },
      groups: {
        title: t('node.group'),
        dataIndex: 'group_ids',
        width: 160,
        render: (ids: number[] | null) =>
          ids && ids.length > 0 ? (
            <Space size={4} wrap>
              {ids.map((id) => (
                <Tag key={id} color="blue" style={{ marginInlineEnd: 0 }}>
                  {groupName(id)}
                </Tag>
              ))}
            </Space>
          ) : (
            <Text type="secondary">—</Text>
          ),
      },
      ip: {
        title: t('node.publicIp'),
        dataIndex: 'public_ipv4',
        width: 150,
        render: (ip: string, node) => (
          <Tooltip title={node.public_ipv6 || undefined}>
            <span className="or-mono">{ip || '—'}</span>
          </Tooltip>
        ),
      },
      status: {
        title: t('common.status'),
        dataIndex: 'online',
        width: 150,
        render: (online: boolean, node) => (
          <Space size={4}>
          {node.disabled && <Tag color="warning">已停用</Tag>}
          <Tag
            color={online ? 'success' : 'default'}
            style={{ marginInlineEnd: 0 }}
            icon={<span className={online ? 'or-dot or-dot-online' : 'or-dot or-dot-offline'} />}
          >
            {online ? t('node.online') : t('node.offline')}
          </Tag>
          </Space>
        ),
      },
      last_seen: {
        title: t('node.lastSeen'),
        dataIndex: 'last_seen',
        width: 150,
        render: (v: string | null) => (
          <Text type="secondary" style={{ fontSize: 12 }}>
            {formatTime(v)}
          </Text>
        ),
      },
      cpu: {
        title: t('node.cpu'),
        dataIndex: 'cpu_usage',
        width: 100,
        render: (v: number, node) => (node.online ? <UsageBar percent={v} /> : <Text type="secondary">—</Text>),
      },
      mem: {
        title: t('node.memory'),
        dataIndex: 'mem_used',
        width: 110,
        render: (used: number, node) =>
          node.online ? (
            <UsageBar
              percent={node.mem_total > 0 ? (used / node.mem_total) * 100 : 0}
              tip={`${formatBytes(used)} / ${formatBytes(node.mem_total)}`}
            />
          ) : (
            <Text type="secondary">—</Text>
          ),
      },
      disk: {
        title: t('node.disk'),
        dataIndex: 'disk_used',
        width: 110,
        render: (used: number, node) =>
          node.online ? (
            <UsageBar
              percent={node.disk_total > 0 ? (used / node.disk_total) * 100 : 0}
              tip={`${formatBytes(used)} / ${formatBytes(node.disk_total)}`}
            />
          ) : (
            <Text type="secondary">—</Text>
          ),
      },
      conn: {
        title: t('node.conn'),
        dataIndex: 'current_conn',
        width: 90,
        render: (v: number, node) => (node.online ? v : <Text type="secondary">—</Text>),
      },
      speed: {
        title: t('node.speed'),
        dataIndex: 'net_in_speed',
        width: 150,
        render: (_v, node) =>
          node.online ? (
            <Tooltip
              title={`${t('nodeDetail.netIn')}: ${formatBytes(node.net_in_speed)}/s · ${t(
                'nodeDetail.netOut',
              )}: ${formatBytes(node.net_out_speed)}/s`}
            >
              <span className="or-mono" style={{ fontSize: 12 }}>
                ↓{formatBytes(node.net_in_speed)}/s ↑{formatBytes(node.net_out_speed)}/s
              </span>
            </Tooltip>
          ) : (
            <Text type="secondary">—</Text>
          ),
      },
      version: {
        title: t('node.version'),
        dataIndex: 'client_ver',
        width: 110,
        render: (v: string) => <span className="or-mono">{v || '—'}</span>,
      },
      health: {
        title: t('node.healthScore'),
        dataIndex: 'health_score',
        width: 100,
        sorter: (a, b) => a.health_score - b.health_score,
        render: (v: number) => (
          <Tooltip title={`${v} / 100`}>
            <span>
              <Progress
                percent={v}
                size="small"
                showInfo={false}
                strokeColor={healthColor(v)}
                style={{ width: 56, marginInlineEnd: 6 }}
              />
              <Text style={{ color: healthColor(v), fontSize: 12 }}>{v}</Text>
            </span>
          </Tooltip>
        ),
      },
      drift: {
        title: t('node.drift'),
        dataIndex: 'drift_detected',
        width: 96,
        render: (v: boolean) =>
          v ? (
            <Tag color="warning" style={{ marginInlineEnd: 0 }}>
              {t('node.drift')}
            </Tag>
          ) : (
            <Text type="secondary">—</Text>
          ),
      },
      actions: {
        title: t('common.actions'),
        key: 'actions',
        fixed: 'right',
        width: 210,
        render: (_v, node) => (
          <div className="or-actions">
            <Tooltip title={node.disabled ? '启用节点转发' : '停用节点转发'}>
              <Popconfirm title={node.disabled ? '启用此节点？' : '停用此节点的转发？'} onConfirm={async () => {
                try { await nodeApi.update(node.id, { disabled: !node.disabled }); await refresh() } catch (error) { showApiError(error) }
              }}>
                <Button type="text" size="small" danger={!node.disabled} icon={<PoweroffOutlined />} />
              </Popconfirm>
            </Tooltip>
            <Tooltip title={t('common.edit')}>
              <Button type="text" size="small" icon={<EditOutlined />} onClick={() => openEdit(node)} />
            </Tooltip>
            <Tooltip title={t('node.installCommand')}>
              <Button
                type="text"
                size="small"
                icon={<CloudDownloadOutlined />}
                onClick={() => void fetchInstall(node)}
              />
            </Tooltip>
            <Tooltip title={t('node.batchUpgrade')}>
              <Button
                type="text"
                size="small"
                icon={<SyncOutlined />}
                onClick={() => confirmUpgrade(node)}
              />
            </Tooltip>
            <Tooltip title={t('node.batchRestart')}>
              <Button
                type="text"
                size="small"
                icon={<RedoOutlined />}
                onClick={() => confirmRestart(node)}
              />
            </Tooltip>
            <Tooltip title={t('user.resetToken')}>
              <Button
                type="text"
                size="small"
                icon={<KeyOutlined />}
                onClick={() => confirmResetToken(node)}
              />
            </Tooltip>
            <Tooltip title={t('common.delete')}>
              <Button
                type="text"
                size="small"
                danger
                icon={<DeleteOutlined />}
                onClick={() => confirmDelete(node)}
              />
            </Tooltip>
          </div>
        ),
      },
    }

    const ordered: ColumnsType<Node> = [all.name]
    for (const key of ALL_COLUMNS) {
      if (visibleColumns.includes(key)) ordered.push(all[key])
    }
    ordered.push(all.actions)
    return ordered
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [t, visibleColumns, groupName, navigate, rows])

  // ── 渲染 ────────────────────────────────────────────────────────────────
  return (
    <div>
      <div className="or-page-header">
        <div>
          <h1 className="or-page-title">{t('node.title')}</h1>
          <p className="or-page-desc">
            {t('common.total', { n: total })} ·{' '}
            <Tooltip
              title={
                wsStatus === 'open' ? t('terminal.connected') : t('terminal.closed')
              }
            >
              <span
                className={wsStatus === 'open' ? 'or-dot or-dot-online' : 'or-dot or-dot-warning'}
              />
            </Tooltip>
            {t('monitor.realtime')}
          </p>
        </div>
        <Space>
          <Button icon={<ReloadOutlined />} onClick={() => void refresh()} loading={loading && rows.length === 0}>
            {t('common.refresh')}
          </Button>
          <Button type="primary" icon={<PlusOutlined />} onClick={openCreate}>
            {t('node.add')}
          </Button>
        </Space>
      </div>

      {/* 工具栏 */}
      <div className="or-toolbar">
        <Input.Search
          allowClear
          style={{ width: 220 }}
          placeholder={t('node.searchPlaceholder')}
          onSearch={(v) => {
            setKeyword(v.trim())
            setPage(1)
          }}
        />
        <Select
          allowClear
          style={{ width: 170 }}
          placeholder={t('node.filterGroup')}
          value={groupFilter}
          onChange={(v) => {
            setGroupFilter(v)
            setPage(1)
          }}
          options={(groups?.items ?? []).map((g: NodeGroup) => ({ value: g.id, label: g.name }))}
        />
        <Select
          allowClear
          style={{ width: 140 }}
          placeholder={t('node.filterOnline')}
          value={onlineFilter}
          onChange={(v) => {
            setOnlineFilter(v)
            setPage(1)
          }}
          options={[
            { value: true, label: t('node.online') },
            { value: false, label: t('node.offline') },
          ]}
        />
        <Select
          allowClear
          style={{ width: 120 }}
          placeholder={t('node.role')}
          value={roleFilter}
          onChange={(v) => {
            setRoleFilter(v)
            setPage(1)
          }}
          options={[
            { value: 'inbound', label: t('node.role.inbound') },
            { value: 'outbound', label: t('node.role.outbound') },
            { value: 'both', label: t('node.role.both') },
          ]}
        />
        <Popover
          trigger="click"
          placement="bottomRight"
          title={t('common.actions')}
          content={
            <Checkbox.Group
              value={visibleColumns}
              onChange={(v) => setVisibleColumns(v as ColumnKey[])}
              options={[
                { value: 'role', label: t('node.role') },
                { value: 'groups', label: t('node.group') },
                { value: 'ip', label: t('node.publicIp') },
                { value: 'status', label: t('common.status') },
                { value: 'last_seen', label: t('node.lastSeen') },
                { value: 'cpu', label: t('node.cpu') },
                { value: 'mem', label: t('node.memory') },
                { value: 'disk', label: t('node.disk') },
                { value: 'conn', label: t('node.conn') },
                { value: 'speed', label: t('node.speed') },
                { value: 'version', label: t('node.version') },
                { value: 'health', label: t('node.healthScore') },
                { value: 'drift', label: t('node.drift') },
              ]}
            />
          }
        >
          <Button icon={<SettingOutlined />} />
        </Popover>
      </div>

      {/* 批量操作栏 */}
      {selectedIds.length > 0 && (
        <Alert
          type="info"
          showIcon
          style={{ marginBottom: 12 }}
          message={
            <Space wrap size={8} split={<Divider type="vertical" />}>
              <Text strong>{t('common.selected', { n: selectedIds.length })}</Text>
              <Button
                size="small"
                icon={<SyncOutlined />}
                loading={running}
                onClick={() => void runBatch((ids) => nodeApi.batchUpgrade(ids))}
              >
                {t('node.batchUpgrade')}
              </Button>
              <Popconfirm
                title={t('node.batchRestartHint')}
                okText={t('common.ok')}
                cancelText={t('common.cancel')}
                onConfirm={() => void runBatch((ids) => nodeApi.batchRestart(ids))}
              >
                <Button size="small" icon={<PoweroffOutlined />} loading={running}>
                  {t('node.batchRestart')}
                </Button>
              </Popconfirm>
              <Button size="small" icon={<SettingOutlined />} onClick={() => setBatchGroupOpen(true)}>
                {t('node.batchGroup')}
              </Button>
              <Button size="small" onClick={() => setBatchWeightOpen(true)}>
                {t('node.batchWeight')}
              </Button>
              <Popconfirm
                title={t('node.batchDisableConfirm', { n: selectedIds.length })}
                description={t('node.batchDisableImpact')}
                okText={t('common.ok')}
                okButtonProps={{ danger: true }}
                cancelText={t('common.cancel')}
                onConfirm={() => void batchDisable()}
              >
                <Button size="small" danger loading={running}>
                  {t('node.batchDisable')}
                </Button>
              </Popconfirm>
              <Button size="small" type="link" onClick={() => setSelectedKeys([])}>
                {t('common.reset')}
              </Button>
            </Space>
          }
        />
      )}

      <Table<Node>
        rowKey="id"
        size="small"
        columns={columns}
        dataSource={rows}
        loading={loading && rows.length === 0}
        scroll={{ x: 1600 }}
        rowSelection={{
          selectedRowKeys: selectedKeys,
          onChange: setSelectedKeys,
        }}
        pagination={{
          current: page,
          pageSize: PAGE_SIZE,
          total,
          showSizeChanger: false,
          onChange: setPage,
          showTotal: (n) => t('common.total', { n }),
        }}
        locale={{
          emptyText: (
            <div className="or-empty">
              <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={t('node.empty')}>
                <Button type="primary" icon={<PlusOutlined />} onClick={openCreate}>
                  {t('node.add')}
                </Button>
              </Empty>
            </div>
          ),
        }}
      />

      {/* 新建 / 编辑节点 */}
      <Modal
        open={formOpen}
        title={editing ? t('node.editTitle') : t('node.add')}
        onCancel={() => setFormOpen(false)}
        onOk={() => void submit()}
        confirmLoading={submitting}
        okText={t('common.save')}
        cancelText={t('common.cancel')}
        destroyOnClose
        width={560}
      >
        <Form form={form} layout="vertical" preserve={false}>
          <Form.Item
            name="name"
            label={t('common.name')}
            rules={[{ required: true, message: t('common.name') }]}
          >
            <Input placeholder="hk-01" autoComplete="off" />
          </Form.Item>
          <Form.Item name="role" label={t('node.role')} rules={[{ required: true }]}>
            <Select
              options={[
                { value: 'inbound', label: t('node.role.inbound') },
                { value: 'outbound', label: t('node.role.outbound') },
                { value: 'both', label: t('node.role.both') },
              ]}
            />
          </Form.Item>
          <Form.Item name="group_ids" label={t('node.group')}>
            <Select
              mode="multiple"
              allowClear
              placeholder={t('node.filterGroup')}
              options={(groups?.items ?? []).map((g: NodeGroup) => ({ value: g.id, label: g.name }))}
            />
          </Form.Item>
          <Space size={12} style={{ display: 'flex' }}>
            <Form.Item name="weight" label={t('node.weight')} style={{ flex: 1 }}>
              <InputNumber min={0} max={1000} style={{ width: '100%' }} />
            </Form.Item>
            <Form.Item name="max_conn" label={t('node.maxConn')} style={{ flex: 1 }}>
              <InputNumber
                min={0}
                max={1000000}
                style={{ width: '100%' }}
                placeholder={t('node.maxConn')}
              />
            </Form.Item>
          </Space>
          <Form.Item name="connect_host" label={t('node.connectHost')} extra={t('node.connectHostHelp')}>
            <Input placeholder="10.0.0.2" autoComplete="off" />
          </Form.Item>
          {editing?.reported_network?.connect_host && (
            <Alert type="info" showIcon style={{ marginBottom: 12 }}
              message={t('node.reportedHostValue', { host: editing.reported_network.connect_host })}
              description={t('node.addressPriority')} />
          )}
          <Form.Item name="is_static" label={t('node.isStatic')} valuePropName="checked">
            <Checkbox />
          </Form.Item>
          <Divider orientation="left" plain>{t('node.listenerPorts')}</Divider>
          <Typography.Paragraph type="secondary">{t('node.listenerPortsHelp')}</Typography.Paragraph>
          <Row gutter={12}>
            {LISTENER_PORTS.map(([field, protocol, defaultPort]) => (
              <Col xs={12} sm={8} key={field}>
                <Form.Item name={field} label={protocol}
                  rules={[{ type: 'integer', min: 0, max: 65535, message: t('node.portRange') }]}
                  extra={editing?.reported_network?.[field]
                    ? t('node.clientPortOverride', { port: editing.reported_network[field]! })
                    : t('node.defaultPort', { port: defaultPort })}>
                  <InputNumber min={0} max={65535} precision={0} placeholder="0" style={{ width: '100%' }} />
                </Form.Item>
              </Col>
            ))}
          </Row>
          <Form.Item name="remark" label={t('common.remark')}>
            <Input.TextArea rows={2} maxLength={255} showCount />
          </Form.Item>
        </Form>
      </Modal>

      {/* 一键对接：安装命令 */}
      <Modal
        open={!!installModal}
        title={installModal ? t('node.installTitle', { name: installModal.node.name }) : ''}
        onCancel={() => setInstallModal(null)}
        footer={
          <Space>
            <Button
              icon={<CopyOutlined />}
              onClick={() => setInstallModal(null)}
            >
              {t('common.ok')}
            </Button>
          </Space>
        }
        width={620}
      >
        <Alert
          type="info"
          showIcon
          icon={<AudioOutlined />}
          message={t('node.installHint')}
          style={{ marginBottom: 12 }}
        />
        <CopyableCommand command={installModal?.command ?? ''} multiline showQrcode />
      </Modal>

      {/* 批量改分组 */}
      <Modal
        open={batchGroupOpen}
        title={t('node.batchGroupTitle', { n: selectedIds.length })}
        onCancel={() => setBatchGroupOpen(false)}
        okText={t('common.save')}
        cancelText={t('common.cancel')}
        confirmLoading={running}
        onOk={async () => {
          setRunning(true)
          try {
            const res = await nodeApi.batchGroup(selectedIds, batchGroupIds)
            reportBatchResult(res as BatchResult)
            setBatchGroupOpen(false)
            await refresh()
          } catch (err) {
            showApiError(err)
          } finally {
            setRunning(false)
          }
        }}
      >
        <Select
          mode="multiple"
          style={{ width: '100%' }}
          placeholder={t('node.batchGroupPlaceholder')}
          value={batchGroupIds}
          onChange={setBatchGroupIds}
          options={(groups?.items ?? []).map((g: NodeGroup) => ({ value: g.id, label: g.name }))}
        />
      </Modal>

      {/* 批量设权重 */}
      <Modal
        open={batchWeightOpen}
        title={t('node.batchWeight')}
        onCancel={() => setBatchWeightOpen(false)}
        okText={t('common.save')}
        cancelText={t('common.cancel')}
        confirmLoading={running}
        onOk={async () => {
          setRunning(true)
          try {
            const res = await nodeApi.batchWeight(selectedIds, batchWeight)
            reportBatchResult(res as BatchResult)
            setBatchWeightOpen(false)
            await refresh()
          } catch (err) {
            showApiError(err)
          } finally {
            setRunning(false)
          }
        }}
      >
        <InputNumber
          min={0}
          max={1000}
          style={{ width: '100%' }}
          placeholder={t('node.batchWeightPlaceholder')}
          value={batchWeight}
          onChange={(v) => setBatchWeight(Number(v ?? 1))}
        />
      </Modal>
    </div>
  )
}

/** 资源占用条：超过 80% 转黄、超过 90% 转红。 */
function UsageBar({ percent, tip }: { percent: number; tip?: string }) {
  const value = Number.isFinite(percent) ? Math.max(0, Math.min(100, percent)) : 0
  const color = value >= 90 ? 'var(--or-error)' : value >= 80 ? 'var(--or-warning)' : 'var(--or-primary)'
  const bar = (
    <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}>
      <Progress
        percent={value}
        size="small"
        showInfo={false}
        strokeColor={color}
        style={{ width: 56, margin: 0 }}
      />
      <Text style={{ fontSize: 12 }}>{value.toFixed(1)}%</Text>
    </span>
  )
  return tip ? <Tooltip title={tip}>{bar}</Tooltip> : bar
}

/** 时间展示：当天显示时分秒，其余显示完整时间。 */
function formatTime(value: string | null): string {
  if (!value) return '—'
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return value
  const now = new Date()
  const sameDay =
    d.getFullYear() === now.getFullYear() &&
    d.getMonth() === now.getMonth() &&
    d.getDate() === now.getDate()
  return sameDay
    ? d.toLocaleTimeString()
    : d.toLocaleString(undefined, { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' })
}
