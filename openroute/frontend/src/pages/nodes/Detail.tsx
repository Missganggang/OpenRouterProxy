/**
 * 节点详情（规格书 9.2 / 6.1 / 6.11 / 6.14）。
 *
 * 标签页：概览 / 监控曲线 / 运行规则 / 日志 / 终端 / 配置。
 *
 * 数据形状说明：本页的类型直接对齐后端 handler 的真实 JSON
 * （internal/api/handler_node.go、internal/api/handler_probe.go），
 * 而不是 src/api/modules/node.ts 里声明的简化类型：
 *  - /nodes/:id/metrics        → { points: ProbePoint[] }
 *  - /nodes/:id/metrics/realtime → 节点行上的即时字段 + { metric: ProbeMetric | null }
 *  - /nodes/:id/rules          → { items: NodeRunningRule[] }
 *  - /nodes/:id/logs           → { logs: string[], empty, message }
 *  - /nodes/:id/drift          → DriftReport
 * 这些差异无法通过修改 api 模块修正（该文件不归本页维护），因此在页面内部
 * 用 `as unknown as` 收口一次，页面其余部分仍然保持强类型。
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import {
  Alert,
  Badge,
  Button,
  Card,
  Col,
  Descriptions,
  Empty,
  Modal,
  Progress,
  Row,
  Select,
  Skeleton,
  Space,
  Statistic,
  Table,
  Tabs,
  Tag,
  Tooltip,
  Typography,
} from 'antd'
import type { ColumnsType } from 'antd/es/table'
import {
  ArrowLeftOutlined,
  CloudDownloadOutlined,
  ExclamationCircleOutlined,
  KeyOutlined,
  RedoOutlined,
  ReloadOutlined,
  SyncOutlined,
  ToolOutlined,
} from '@ant-design/icons'

import { nodeApi, showApiError } from '../../api'
import type { Node } from '../../api/types'
import { formatBytes } from '../../components/TrafficChart'
import { healthColor, formatUptime } from '../../components/NodeCard'
import CopyableCommand from '../../components/CopyableCommand'
import SyncStatusTag from '../../components/SyncStatusTag'
import Terminal from '../../components/Terminal'
import { usePolling } from '../../hooks/usePolling'
import { useI18n } from '../../locales'

const { Text } = Typography

// ───────────────────────── 后端真实响应形状 ─────────────────────────

/** 探针曲线上的一个聚合点（app.ProbePoint）。 */
interface ProbePoint {
  timestamp: number
  time: string
  cpu: number
  mem_used: number
  mem_total: number
  swap_used: number
  swap_total: number
  disk_used: number
  disk_total: number
  net_in: number
  net_out: number
  net_in_speed: number
  net_out_speed: number
  load1: number
  load5: number
  load15: number
  tcp_conn: number
  udp_conn: number
  uptime: number
  samples?: number
}

/** /nodes/:id/metrics 的响应。 */
interface MetricsResponse {
  node_id: number
  from: string
  to: string
  interval: string
  points: ProbePoint[] | null
}

/** /nodes/:id/metrics/realtime 的响应。 */
interface RealtimeResponse {
  node_id: number
  online: boolean
  last_seen: string | null
  current_conn: number
  net_in_speed: number
  net_out_speed: number
  cpu_usage: number
  mem_used: number
  mem_total: number
  disk_used: number
  disk_total: number
  load1: number
  uptime: number
  health_score: number
  /** 最近一次采集样本；探针未上报时为 null。 */
  metric: ProbePoint | null
}

/** 节点上运行中的规则（api.NodeRunningRule）。 */
interface NodeRunningRule {
  id: number
  name: string
  user_id: number
  rule_group_id: number
  inbound_group_id: number
  listen_port: number
  listen_port_end: number
  outbound_group_id: number
  protocol: string
  target_count: number
  sync_status: string
  sync_error: string
  current_conn: number
  enable: boolean
  drifted: boolean
}

/** /nodes/:id/rules 的响应。 */
interface RulesResponse {
  node_id: number
  node_name: string
  online: boolean
  drift_detected: boolean
  inbound_groups: number[] | null
  items: NodeRunningRule[]
  count: number
}

/** /nodes/:id/logs 的响应。 */
interface LogsResponse {
  node_id: number
  node_name: string
  lines: number
  logs: string[] | null
  empty: boolean
  source: string
  message: string
}

/** 漂移差异项（app.DriftItem）。 */
interface DriftItem {
  kind: string
  rule_id?: number
  expected: string
  actual: string
  message: string
}

/** 漂移报告（app.DriftReport）。 */
interface DriftReport {
  node_id: number
  name: string
  drifted: boolean
  items: DriftItem[] | null
  checked_at: string
}

/** 可选的日志行数。 */
const LOG_LINE_OPTIONS = [100, 200, 500, 1000] as const

export default function NodeDetailPage() {
  const { id: idParam } = useParams<{ id: string }>()
  const navigate = useNavigate()
  const { t } = useI18n()

  // 路由参数是字符串，非法值直接当作 0，由后端给出明确错误。
  const nodeId = Number(idParam)
  const validId = Number.isInteger(nodeId) && nodeId > 0

  const [activeTab, setActiveTab] = useState('overview')
  const [actionLoading, setActionLoading] = useState('')
  const [installCommand, setInstallCommand] = useState<string | null>(null)
  const [tokenModal, setTokenModal] = useState<string | null>(null)
  const [refreshing, setRefreshing] = useState(false)

  // ── 节点主体：5 秒轮询，保证在线状态与基础指标跟着心跳走（规格书 6.1） ──
  const fetchNode = useCallback(
    () => (validId ? nodeApi.getOne(nodeId) : Promise.reject(new Error('invalid id'))),
    [nodeId, validId],
  )
  const { data: node, loading, error, refresh } = usePolling<Node>(fetchNode, { interval: 5000 })

  const nodeName = node?.name ?? `#${idParam}`

  /** 顶部「刷新」：把当前标签页依赖的数据全部重取一次。 */
  const handleRefresh = useCallback(async () => {
    setRefreshing(true)
    try {
      await refresh()
    } finally {
      setRefreshing(false)
    }
  }, [refresh])

  /** 危险操作的统一确认框（规格书 9.3：标题写明资源名）。 */
  const confirmAction = useCallback(
    (options: {
      title: string
      content?: string
      okText?: string
      danger?: boolean
      run: () => Promise<void>
    }) => {
      Modal.confirm({
        title: options.title,
        content: options.content,
        okText: options.okText ?? t('common.ok'),
        okButtonProps: options.danger ? { danger: true } : undefined,
        cancelText: t('common.cancel'),
        onOk: async () => {
          try {
            await options.run()
          } catch (err) {
            showApiError(err)
            // 抛出以阻止弹窗关闭，用户可直接重试。
            throw err
          }
        },
      })
    },
    [t],
  )

  /** 重新生成安装命令。 */
  const doInstallCommand = useCallback(async () => {
    if (!node) return
    setActionLoading('install')
    try {
      const res = await nodeApi.installCommand(node.id)
      setInstallCommand(res.install_command)
    } catch (err) {
      showApiError(err)
    } finally {
      setActionLoading('')
    }
  }, [node])

  const confirmUpgrade = useCallback(() => {
    if (!node) return
    confirmAction({
      title: t('node.upgradeConfirm', { name: node.name }),
      content: t('node.upgradeImpact'),
      run: async () => {
        await nodeApi.upgrade(node.id)
        await refresh()
      },
    })
  }, [node, confirmAction, refresh, t])

  const confirmRestart = useCallback(() => {
    if (!node) return
    confirmAction({
      title: t('node.restartConfirm', { name: node.name }),
      content: t('node.restartImpact'),
      danger: true,
      run: async () => {
        await nodeApi.restart(node.id)
        await refresh()
      },
    })
  }, [node, confirmAction, refresh, t])

  const confirmResetToken = useCallback(() => {
    if (!node) return
    confirmAction({
      title: t('node.resetTokenConfirm', { name: node.name }),
      content: t('node.resetTokenImpact'),
      danger: true,
      run: async () => {
        const res = await nodeApi.resetToken(node.id)
        setTokenModal(res.token)
      },
    })
  }, [node, confirmAction, t])

  // ── 概览 ────────────────────────────────────────────────────────────────
  if (loading && !node) {
    return <Skeleton active paragraph={{ rows: 10 }} />
  }

  if (!node && error) {
    return (
      <Alert
        type="error"
        showIcon
        message={t('nodeDetail.loadFailed', { id: String(idParam) })}
        description={error instanceof Error ? error.message : undefined}
        action={
          <Space>
            <Button onClick={() => navigate('/nodes')}>{t('nodeDetail.backToList')}</Button>
            <Button type="primary" onClick={() => void refresh()}>
              {t('common.refresh')}
            </Button>
          </Space>
        }
      />
    )
  }

  const roleTag = <Tag>{t(`node.role.${node?.role ?? 'both'}`)}</Tag>

  return (
    <div>
      <div className="or-page-header">
        <div>
          <Space size={8} align="center">
            <Button
              type="text"
              size="small"
              icon={<ArrowLeftOutlined />}
              onClick={() => navigate('/nodes')}
            />
            <span
              className={node?.online ? 'or-dot or-dot-online' : 'or-dot or-dot-offline'}
              aria-hidden
            />
            <h1 className="or-page-title" style={{ margin: 0 }}>
              {nodeName}
            </h1>
            {roleTag}
            {node?.drift_detected && (
              <Tooltip title={t('nodeDetail.driftFound')}>
                <Tag color="warning" style={{ margin: 0 }}>
                  {t('node.drift')}
                </Tag>
              </Tooltip>
            )}
            {node?.health_score !== undefined && (
              <Tooltip title={`${t('node.healthScore')}: ${node.health_score} / 100`}>
                <Tag color="default" style={{ margin: 0, color: healthColor(node.health_score) }}>
                  {node.health_score}
                </Tag>
              </Tooltip>
            )}
          </Space>
          <p className="or-page-desc">
            {node?.online ? t('node.online') : t('node.offline')}
            {node?.last_seen ? ` · ${t('node.lastSeen')}: ${formatTime(node.last_seen)}` : ''}
            {node?.last_error ? ` · ${t('nodeDetail.lastError')}: ${node.last_error}` : ''}
          </p>
        </div>
        <Space wrap>
          <Button icon={<ReloadOutlined />} loading={refreshing} onClick={() => void handleRefresh()}>
            {t('common.refresh')}
          </Button>
          <Button
            icon={<CloudDownloadOutlined />}
            loading={actionLoading === 'install'}
            onClick={() => void doInstallCommand()}
          >
            {t('node.installCommand')}
          </Button>
          <Button
            icon={<SyncOutlined />}
            loading={actionLoading === 'upgrade'}
            onClick={confirmUpgrade}
          >
            {t('node.upgrade')}
          </Button>
          <Button
            icon={<RedoOutlined />}
            loading={actionLoading === 'restart'}
            onClick={confirmRestart}
          >
            {t('node.restart')}
          </Button>
          <Button
            danger
            icon={<KeyOutlined />}
            loading={actionLoading === 'token'}
            onClick={confirmResetToken}
          >
            {t('node.resetToken')}
          </Button>
        </Space>
      </div>

      <Card size="small" styles={{ body: { paddingTop: 8 } }}>
        <Tabs
          activeKey={activeTab}
          onChange={setActiveTab}
          destroyInactiveTabPane
          items={[
            {
              key: 'overview',
              label: t('nodeDetail.tabOverview'),
              children: node ? <OverviewTab node={node} /> : null,
            },
            {
              key: 'metrics',
              label: t('nodeDetail.tabMetrics'),
              children: validId ? <MetricsTab nodeId={nodeId} /> : null,
            },
            {
              key: 'rules',
              label: t('nodeDetail.tabRules'),
              children: validId ? <RulesTab nodeId={nodeId} /> : null,
            },
            {
              key: 'logs',
              label: t('nodeDetail.tabLogs'),
              children: validId ? <LogsTab nodeId={nodeId} /> : null,
            },
            {
              key: 'terminal',
              label: t('nodeDetail.tabTerminal'),
              children: validId ? (
                <div>
                  <Alert
                    type="info"
                    showIcon
                    style={{ marginBottom: 12 }}
                    message={t('terminal.auditHint')}
                    description={t('terminal.auditHintDetail')}
                  />
                  <Terminal nodeId={nodeId} />
                </div>
              ) : null,
            },
            {
              key: 'config',
              label: t('nodeDetail.tabConfig'),
              children: node ? <ConfigTab node={node} onFixed={() => void refresh()} /> : null,
            },
          ]}
        />
      </Card>

      {/* 安装命令（可复制 + 二维码，规格书 6.1） */}
      <Modal
        open={installCommand !== null}
        title={t('node.installTitle', { name: nodeName })}
        onCancel={() => setInstallCommand(null)}
        footer={
          <Button type="primary" onClick={() => setInstallCommand(null)}>
            {t('common.ok')}
          </Button>
        }
        width={620}
        destroyOnClose
      >
        <Alert
          type="info"
          showIcon
          style={{ marginBottom: 12 }}
          message={t('node.installHint')}
        />
        <CopyableCommand command={installCommand ?? ''} multiline showQrcode />
      </Modal>

      {/* 重置后的新密钥：仅本次显示 */}
      <Modal
        open={tokenModal !== null}
        title={t('node.newToken')}
        onCancel={() => setTokenModal(null)}
        footer={
          <Button type="primary" onClick={() => setTokenModal(null)}>
            {t('common.ok')}
          </Button>
        }
        width={560}
        destroyOnClose
      >
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 12 }}
          message={t('node.newTokenHint')}
        />
        <CopyableCommand command={tokenModal ?? ''} showQrcode={false} multiline />
      </Modal>
    </div>
  )
}

// ───────────────────────── 概览 ─────────────────────────

/** 概览标签页：系统信息 + 即时指标 + 端口与密钥。 */
function OverviewTab({ node }: { node: Node }) {
  const { t } = useI18n()
  const [secretOpen, setSecretOpen] = useState(false)

  const memPercent = node.mem_total > 0 ? (node.mem_used / node.mem_total) * 100 : 0
  const diskPercent = node.disk_total > 0 ? (node.disk_used / node.disk_total) * 100 : 0

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      {/* 即时指标：镜像 /nodes/:id/metrics/realtime 里由心跳直接写入的字段 */}
      <Row gutter={[12, 12]}>
        <Col xs={12} md={6}>
          <Card size="small">
            <Statistic
              title={t('nodeDetail.cpuUsage')}
              value={node.online ? node.cpu_usage : 0}
              precision={1}
              suffix="%"
              valueStyle={{ color: node.online ? healthColor(node.health_score) : undefined }}
            />
          </Card>
        </Col>
        <Col xs={12} md={6}>
          <Card size="small">
            <Statistic
              title={t('nodeDetail.memUsage')}
              value={node.online ? memPercent : 0}
              precision={1}
              suffix="%"
            />
            <Text type="secondary" style={{ fontSize: 12 }}>
              {formatBytes(node.mem_used)} / {formatBytes(node.mem_total)}
            </Text>
          </Card>
        </Col>
        <Col xs={12} md={6}>
          <Card size="small">
            <Statistic
              title={t('nodeDetail.diskTotal')}
              value={diskPercent}
              precision={1}
              suffix="%"
            />
            <Text type="secondary" style={{ fontSize: 12 }}>
              {formatBytes(node.disk_used)} / {formatBytes(node.disk_total)}
            </Text>
          </Card>
        </Col>
        <Col xs={12} md={6}>
          <Card size="small">
            <Statistic title={t('node.conn')} value={node.online ? node.current_conn : 0} />
            <Text type="secondary" style={{ fontSize: 12 }}>
              {t('nodeDetail.load')}: {node.online ? node.load1.toFixed(2) : '--'}
            </Text>
          </Card>
        </Col>
      </Row>

      <Row gutter={[12, 12]}>
        <Col xs={24} md={8}>
          <Card size="small" title={t('nodeDetail.netSpeed')}>
            <Space direction="vertical" size={4} style={{ width: '100%' }}>
              <Text>
                {t('nodeDetail.netIn')}:{' '}
                <Text strong className="or-mono">
                  {node.online ? `${formatBytes(node.net_in_speed)}/s` : '--'}
                </Text>
              </Text>
              <Text>
                {t('nodeDetail.netOut')}:{' '}
                <Text strong className="or-mono">
                  {node.online ? `${formatBytes(node.net_out_speed)}/s` : '--'}
                </Text>
              </Text>
            </Space>
          </Card>
        </Col>
        <Col xs={24} md={8}>
          <Card size="small" title={t('node.healthScore')}>
            <Progress
              percent={node.health_score}
              size="small"
              strokeColor={healthColor(node.health_score)}
              format={(v) => `${v}`}
            />
            <Text type="secondary" style={{ fontSize: 12 }}>
              {t('nodeDetail.healthHint')}
            </Text>
          </Card>
        </Col>
        <Col xs={24} md={8}>
          <Card size="small" title={t('nodeDetail.ports')}>
            <Space size={6} wrap>
              <Tag className="or-mono" style={{ margin: 0 }}>
                direct {node.direct_port || '—'}
              </Tag>
              <Tag className="or-mono" style={{ margin: 0 }}>
                ws {node.ws_port || '—'}
              </Tag>
              <Tag className="or-mono" style={{ margin: 0 }}>
                tls {node.tls_port || '—'}
              </Tag>
              <Tag className="or-mono" style={{ margin: 0 }}>
                udp {node.udp_port || '—'}
              </Tag>
              <Tag className="or-mono" style={{ margin: 0 }}>
                rev {node.rev_port || '—'}
              </Tag>
            </Space>
          </Card>
        </Col>
      </Row>

      <Descriptions
        title={t('nodeDetail.systemInfo')}
        bordered
        size="small"
        column={{ xs: 1, sm: 2, md: 3 }}
      >
        <Descriptions.Item label={t('nodeDetail.os')}>{node.os || '—'}</Descriptions.Item>
        <Descriptions.Item label={t('nodeDetail.arch')}>{node.arch || '—'}</Descriptions.Item>
        <Descriptions.Item label={t('nodeDetail.kernel')}>
          <span className="or-mono">{node.kernel_ver || '—'}</span>
        </Descriptions.Item>
        <Descriptions.Item label={t('nodeDetail.cpuModel')} span={2}>
          {node.cpu_model || '—'}
        </Descriptions.Item>
        <Descriptions.Item label={t('nodeDetail.cpuCores')}>{node.cpu_cores || '—'}</Descriptions.Item>
        <Descriptions.Item label={t('nodeDetail.memTotal')}>
          {formatBytes(node.mem_total)}
        </Descriptions.Item>
        <Descriptions.Item label={t('nodeDetail.diskTotal')}>
          {formatBytes(node.disk_total)}
        </Descriptions.Item>
        <Descriptions.Item label={t('nodeDetail.bootTime')}>
          {formatTime(node.boot_time)}
        </Descriptions.Item>
        <Descriptions.Item label={t('nodeDetail.uptime')}>
          {node.online ? formatUptime(node.uptime) : '—'}
        </Descriptions.Item>
        <Descriptions.Item label={t('node.version')}>
          <span className="or-mono">{node.client_ver || '—'}</span>
        </Descriptions.Item>
        <Descriptions.Item label={t('nodeDetail.configVersion')}>
          <span className="or-mono">v{node.config_version}</span>
        </Descriptions.Item>
      </Descriptions>

      <Descriptions
        title={t('nodeDetail.configSummary')}
        bordered
        size="small"
        column={{ xs: 1, sm: 2, md: 3 }}
      >
        <Descriptions.Item label={t('node.publicIp')}>
          <span className="or-mono">{node.public_ipv4 || '—'}</span>
        </Descriptions.Item>
        <Descriptions.Item label="IPv6">
          <span className="or-mono">{node.public_ipv6 || '—'}</span>
        </Descriptions.Item>
        <Descriptions.Item label={t('node.privateIp')}>
          <span className="or-mono">{node.private_ip || '—'}</span>
        </Descriptions.Item>
        <Descriptions.Item label={t('node.connectHost')}>
          <span className="or-mono">{node.connect_host || '—'}</span>
          {node.is_static && (
            <Tag color="blue" style={{ marginInlineStart: 6 }}>
              static
            </Tag>
          )}
        </Descriptions.Item>
        <Descriptions.Item label={t('node.weight')}>{node.weight}</Descriptions.Item>
        <Descriptions.Item label={t('node.maxConn')}>
          {node.max_conn > 0 ? node.max_conn : t('rule.unlimited')}
        </Descriptions.Item>
        <Descriptions.Item label={t('node.group')}>
          {node.group_ids && node.group_ids.length > 0 ? (
            <Space size={4} wrap>
              {node.group_ids.map((gid) => (
                <Tag key={gid} color="blue" style={{ margin: 0 }}>
                  #{gid}
                </Tag>
              ))}
            </Space>
          ) : (
            '—'
          )}
        </Descriptions.Item>
        <Descriptions.Item label={t('common.createdAt')}>
          {formatTime(node.created_at)}
        </Descriptions.Item>
        <Descriptions.Item label={t('common.updatedAt')}>
          {formatTime(node.updated_at)}
        </Descriptions.Item>
        <Descriptions.Item label={t('node.token')} span={3}>
          {secretOpen ? (
            <span className="or-secret">{node.token}</span>
          ) : (
            <Space size={8}>
              <Text type="secondary">••••••••••••••••</Text>
              <Button size="small" type="link" onClick={() => setSecretOpen(true)}>
                {t('nodeDetail.showSecret')}
              </Button>
            </Space>
          )}
        </Descriptions.Item>
        {node.remark && (
          <Descriptions.Item label={t('common.remark')} span={3}>
            {node.remark}
          </Descriptions.Item>
        )}
      </Descriptions>
    </Space>
  )
}

// ───────────────────────── 监控曲线 ─────────────────────────

/** 监控曲线标签页：最近 5 分钟的 CPU / 内存 / 网络曲线 + 5 秒实时指标。 */
function MetricsTab({ nodeId }: { nodeId: number }) {
  const { t } = useI18n()

  // 历史曲线：5 分钟粒度，30 秒轮询一次即可（规格书 9.3 的默认轮询节奏）。
  const {
    data: metrics,
    loading: metricsLoading,
    refresh: refreshMetrics,
  } = usePolling(
    () =>
      nodeApi.metrics(nodeId, { interval: '5m' }) as unknown as Promise<MetricsResponse>,
    { interval: 30000 },
  )

  // 实时指标：5 秒轮询（规格书 6.11 的实时窗口）。
  const { data: realtime } = usePolling(
    () => nodeApi.realtimeMetrics(nodeId) as unknown as Promise<RealtimeResponse>,
    { interval: 5000 },
  )

  const points = metrics?.points ?? []
  const rt = realtime

  const cpuSeries = useMemo(
    () => points.map((p) => ({ label: pointLabel(p), value: p.cpu })),
    [points],
  )
  const memSeries = useMemo(
    () =>
      points.map((p) => ({
        label: pointLabel(p),
        value: p.mem_total > 0 ? (p.mem_used / p.mem_total) * 100 : 0,
      })),
    [points],
  )
  const netInSeries = useMemo(
    () => points.map((p) => ({ label: pointLabel(p), value: p.net_in_speed })),
    [points],
  )
  const netOutSeries = useMemo(
    () => points.map((p) => ({ label: pointLabel(p), value: p.net_out_speed })),
    [points],
  )

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      {/* 实时指标：来自 /nodes/:id/metrics/realtime（心跳直接写入的字段） */}
      <Row gutter={[12, 12]}>
        <Col xs={12} md={6}>
          <Card size="small">
            <Statistic
              title={t('nodeDetail.cpuUsage')}
              value={rt?.cpu_usage ?? 0}
              precision={1}
              suffix="%"
            />
          </Card>
        </Col>
        <Col xs={12} md={6}>
          <Card size="small">
            <Statistic
              title={t('nodeDetail.memUsage')}
              value={
                rt && rt.mem_total > 0 ? (rt.mem_used / rt.mem_total) * 100 : 0
              }
              precision={1}
              suffix="%"
            />
            <Text type="secondary" style={{ fontSize: 12 }}>
              {formatBytes(rt?.mem_used ?? 0)} / {formatBytes(rt?.mem_total ?? 0)}
            </Text>
          </Card>
        </Col>
        <Col xs={12} md={6}>
          <Card size="small">
            <Statistic
              title={t('nodeDetail.tcpConn')}
              value={rt?.metric?.tcp_conn ?? rt?.current_conn ?? 0}
            />
            <Text type="secondary" style={{ fontSize: 12 }}>
              {t('nodeDetail.udpConn')}: {rt?.metric?.udp_conn ?? '--'}
            </Text>
          </Card>
        </Col>
        <Col xs={12} md={6}>
          <Card size="small">
            <Statistic
              title={t('nodeDetail.load')}
              value={rt?.metric?.load1 ?? rt?.load1 ?? 0}
              precision={2}
            />
            <Text type="secondary" style={{ fontSize: 12 }}>
              {t('nodeDetail.uptime')}: {formatUptime(rt?.uptime ?? 0)}
            </Text>
          </Card>
        </Col>
      </Row>

      <Space style={{ width: '100%', justifyContent: 'space-between' }} wrap>
        <Text type="secondary">
          {t('monitor.last5min')} · {t('nodeDetail.samples', { n: points.length })}
        </Text>
        <Button size="small" icon={<ReloadOutlined />} onClick={() => void refreshMetrics()}>
          {t('common.refresh')}
        </Button>
      </Space>

      {metricsLoading && points.length === 0 ? (
        <Skeleton active paragraph={{ rows: 6 }} />
      ) : points.length === 0 ? (
        <div className="or-empty">
          <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={t('nodeDetail.noMetrics')}>
            <Text type="secondary" style={{ fontSize: 12 }}>
              {t('nodeDetail.noMetricsHint')}
            </Text>
          </Empty>
        </div>
      ) : (
        <Row gutter={[12, 12]}>
          <Col xs={24} lg={12}>
            <SeriesCard
              title={t('nodeDetail.cpuUsage')}
              unit="%"
              max={100}
              series={[{ name: 'CPU', color: '#165dff', points: cpuSeries }]}
            />
          </Col>
          <Col xs={24} lg={12}>
            <SeriesCard
              title={t('nodeDetail.memUsage')}
              unit="%"
              max={100}
              series={[{ name: t('node.memory'), color: '#00b42a', points: memSeries }]}
            />
          </Col>
          <Col xs={24}>
            <SeriesCard
              title={t('nodeDetail.netSpeed')}
              format={formatBytes}
              series={[
                { name: t('nodeDetail.netIn'), color: '#165dff', points: netInSeries },
                { name: t('nodeDetail.netOut'), color: '#ff7d00', points: netOutSeries },
              ]}
            />
          </Col>
        </Row>
      )}
    </Space>
  )
}

/** 曲线上的一个点。 */
interface SeriesPoint {
  label: string
  value: number
}

/** 一条曲线。 */
interface Series {
  name: string
  color: string
  points: SeriesPoint[]
}

/**
 * 轻量 SVG 曲线。
 *
 * 刻意不用 @ant-design/plots：本页要画 4 条同源曲线，
 * 一屏内实例化多个 G2 画布的开销明显大于一条 poyline，
 * 而这里需要的只有「趋势 + 当前值」。
 */
function SeriesCard({
  title,
  series,
  unit = '',
  max,
  format = (v: number) => v.toFixed(1),
}: {
  title: string
  series: Series[]
  unit?: string
  /** 固定 Y 轴上限（例如 CPU / 内存的 100%）；不传则按数据自适应。 */
  max?: number
  format?: (v: number) => string
}) {
  const width = 600
  const height = 140
  const padding = { top: 12, right: 12, bottom: 18, left: 8 }

  const allValues = series.flatMap((s) => s.points.map((p) => p.value))
  const dataMax = allValues.length > 0 ? Math.max(...allValues) : 0
  const upper = max ?? (dataMax > 0 ? dataMax : 1)
  const count = Math.max(...series.map((s) => s.points.length), 0)

  const innerW = width - padding.left - padding.right
  const innerH = height - padding.top - padding.bottom

  const toPath = (points: SeriesPoint[]) =>
    points
      .map((p, i) => {
        const x = padding.left + (count > 1 ? (i / (count - 1)) * innerW : innerW / 2)
        const ratio = upper > 0 ? Math.min(1, Math.max(0, p.value / upper)) : 0
        const y = padding.top + innerH - ratio * innerH
        return `${i === 0 ? 'M' : 'L'}${x.toFixed(2)},${y.toFixed(2)}`
      })
      .join(' ')

  const latest = series.map((s) => ({
    name: s.name,
    color: s.color,
    value: s.points.length > 0 ? s.points[s.points.length - 1].value : 0,
  }))

  return (
    <Card size="small" title={title}>
      <Space size={16} style={{ marginBottom: 4 }} wrap>
        {latest.map((l) => (
          <Text key={l.name} style={{ fontSize: 12 }}>
            <span style={{ color: l.color }}>●</span> {l.name}{' '}
            <Text strong className="or-mono">
              {format(l.value)}
              {unit}
            </Text>
          </Text>
        ))}
      </Space>
      <svg
        viewBox={`0 0 ${width} ${height}`}
        width="100%"
        height={height}
        preserveAspectRatio="none"
        role="img"
        aria-label={title}
      >
        {/* 三档水平参考线，避免出现只有一条斜线的空浮感 */}
        {[0.25, 0.5, 0.75, 1].map((r) => (
          <line
            key={r}
            x1={padding.left}
            x2={width - padding.right}
            y1={padding.top + innerH - r * innerH}
            y2={padding.top + innerH - r * innerH}
            stroke="var(--or-border)"
            strokeWidth={0.5}
            strokeDasharray="3 3"
          />
        ))}
        {series.map((s) =>
          s.points.length >= 2 ? (
            <path
              key={s.name}
              d={toPath(s.points)}
              fill="none"
              stroke={s.color}
              strokeWidth={1.6}
              vectorEffect="non-scaling-stroke"
            />
          ) : null,
        )}
      </svg>
      <Space style={{ width: '100%', justifyContent: 'space-between' }}>
        <Text type="secondary" style={{ fontSize: 11 }}>
          {series[0]?.points[0]?.label ?? ''}
        </Text>
        <Text type="secondary" style={{ fontSize: 11 }}>
          {upper > 0 ? `${format(upper)}${unit}` : ''}
        </Text>
        <Text type="secondary" style={{ fontSize: 11 }}>
          {series[0]?.points[series[0].points.length - 1]?.label ?? ''}
        </Text>
      </Space>
    </Card>
  )
}

// ───────────────────────── 运行规则 ─────────────────────────

/** 运行规则标签页：该节点所属入口组上的启用规则。 */
function RulesTab({ nodeId }: { nodeId: number }) {
  const { t } = useI18n()

  const { data, loading, refresh } = usePolling(
    () => nodeApi.rules(nodeId) as unknown as Promise<RulesResponse>,
    { interval: 30000 },
  )

  const rows = data?.items ?? []

  const columns: ColumnsType<NodeRunningRule> = useMemo(
    () => [
      {
        title: t('common.name'),
        dataIndex: 'name',
        render: (v: string, row) => (
          <Space size={6}>
            <Text strong>{v}</Text>
            {!row.enable && (
              <Tag color="default" style={{ margin: 0 }}>
                {t('common.disabled')}
              </Tag>
            )}
            {row.drifted && (
              <Tooltip title={t('nodeDetail.driftFound')}>
                <Tag color="warning" style={{ margin: 0 }}>
                  {t('node.drift')}
                </Tag>
              </Tooltip>
            )}
          </Space>
        ),
      },
      {
        title: t('rule.listenPort'),
        dataIndex: 'listen_port',
        width: 140,
        render: (port: number, row) => (
          <span className="or-mono">
            {row.listen_port_end > port ? `${port}-${row.listen_port_end}` : port}
          </span>
        ),
      },
      {
        title: t('deviceGroup.protocol'),
        dataIndex: 'protocol',
        width: 96,
        render: (v: string) => <Tag style={{ margin: 0 }}>{v || '—'}</Tag>,
      },
      {
        title: t('rule.targets'),
        dataIndex: 'target_count',
        width: 90,
        align: 'right',
      },
      {
        title: t('rule.liveSessions'),
        dataIndex: 'current_conn',
        width: 110,
        align: 'right',
        render: (v: number) => <Text className="or-mono">{v}</Text>,
      },
      {
        title: t('rule.syncStatus'),
        dataIndex: 'sync_status',
        width: 120,
        render: (_v, row) => (
          <SyncStatusTag
            // 后端返回的是字符串，收敛到 SyncStatusTag 支持的四种状态。
            status={normalizeSyncStatus(row.sync_status)}
            error={row.sync_error}
            showIcon
          />
        ),
      },
    ],
    [t],
  )

  return (
    <Space direction="vertical" size={8} style={{ width: '100%' }}>
      <Space style={{ width: '100%', justifyContent: 'space-between' }} wrap>
        <Text type="secondary">{t('nodeDetail.rulesHint')}</Text>
        <Button size="small" icon={<ReloadOutlined />} onClick={() => void refresh()}>
          {t('common.refresh')}
        </Button>
      </Space>
      <Table<NodeRunningRule>
        rowKey="id"
        size="small"
        columns={columns}
        dataSource={rows}
        loading={loading && rows.length === 0}
        pagination={false}
        locale={{
          emptyText: (
            <div className="or-empty">
              <Empty
                image={Empty.PRESENTED_IMAGE_SIMPLE}
                description={t('nodeDetail.rulesEmpty')}
              />
            </div>
          ),
        }}
      />
    </Space>
  )
}

// ───────────────────────── 日志 ─────────────────────────

/** 日志标签页：按行数拉取节点日志尾部。 */
function LogsTab({ nodeId }: { nodeId: number }) {
  const { t } = useI18n()
  const [lines, setLines] = useState<number>(200)

  const { data, loading, refresh } = usePolling(
    () => nodeApi.logs(nodeId, lines) as unknown as Promise<LogsResponse>,
    { interval: 0, immediate: true },
  )

  // 切换行数后立刻重取一次；usePolling 的 interval=0 不会自动轮询。
  useEffect(() => {
    void refresh()
  }, [lines, refresh])

  const content = (data?.logs ?? []).join('\n')

  return (
    <Space direction="vertical" size={8} style={{ width: '100%' }}>
      <div className="or-toolbar">
        <Text type="secondary">{t('nodeDetail.logLines')}</Text>
        <Select
          style={{ width: 120 }}
          value={lines}
          onChange={setLines}
          options={LOG_LINE_OPTIONS.map((n) => ({ value: n, label: `${n}` }))}
        />
        <Button icon={<ReloadOutlined />} loading={loading} onClick={() => void refresh()}>
          {t('common.refresh')}
        </Button>
        <Text type="secondary" style={{ fontSize: 12 }}>
          {t('nodeDetail.logsSource')}
        </Text>
      </div>

      {/* 后端明确说明「面板不保存节点日志」时，把它的原文摆出来，而不是伪造空日志 */}
      {data?.empty && data.message && (
        <Alert type="info" showIcon message={data.message} />
      )}

      {content ? (
        <pre className="or-report" style={{ maxHeight: 560, overflow: 'auto' }}>
          {content}
        </pre>
      ) : (
        <div className="or-empty">
          <Empty
            image={Empty.PRESENTED_IMAGE_SIMPLE}
            description={loading ? t('common.loading') : t('nodeDetail.logsEmpty')}
          />
        </div>
      )}
    </Space>
  )
}

// ───────────────────────── 配置（漂移） ─────────────────────────

/** 配置标签页：漂移状态 + 差异详情 + 一键纠正（规格书 6.14）。 */
function ConfigTab({ node, onFixed }: { node: Node; onFixed: () => void }) {
  const { t } = useI18n()
  const [fixing, setFixing] = useState(false)
  const fixedRef = useRef(false)

  const { data, loading, refresh } = usePolling(
    () => nodeApi.drift(node.id) as unknown as Promise<DriftReport>,
    { interval: 0, immediate: true },
  )

  const drifted = data?.drifted ?? node.drift_detected
  const items = data?.items ?? []

  /** 一键纠正：确认后重新下发期望配置。 */
  const handleFix = useCallback(() => {
    Modal.confirm({
      title: t('nodeDetail.driftFixConfirm', { name: node.name }),
      content: t('nodeDetail.driftFixImpact'),
      okText: t('nodeDetail.driftFix'),
      cancelText: t('common.cancel'),
      onOk: async () => {
        setFixing(true)
        try {
          await nodeApi.fixDrift(node.id)
          fixedRef.current = true
          await refresh()
          onFixed()
        } catch (err) {
          showApiError(err)
          throw err
        } finally {
          setFixing(false)
        }
      },
    })
  }, [node.id, node.name, refresh, onFixed, t])

  const columns: ColumnsType<DriftItem> = useMemo(
    () => [
      {
        title: t('nodeDetail.driftKind'),
        dataIndex: 'kind',
        width: 140,
        render: (v: string) => <Tag style={{ margin: 0 }}>{t(`nodeDetail.driftKind.${v}`)}</Tag>,
      },
      {
        title: 'Rule',
        dataIndex: 'rule_id',
        width: 90,
        render: (v?: number) => (v ? <span className="or-mono">#{v}</span> : <Text type="secondary">—</Text>),
      },
      {
        title: t('nodeDetail.expectedConfig'),
        dataIndex: 'expected',
        render: (v: string) => <span className="or-mono">{v || '—'}</span>,
      },
      {
        title: t('nodeDetail.actualConfig'),
        dataIndex: 'actual',
        render: (v: string) => <span className="or-mono">{v || '—'}</span>,
      },
      {
        title: t('rule.reason'),
        dataIndex: 'message',
        render: (v: string) => <Text type="secondary">{v}</Text>,
      },
    ],
    [t],
  )

  return (
    <Space direction="vertical" size={12} style={{ width: '100%' }}>
      <Alert
        type={drifted ? 'warning' : 'success'}
        showIcon
        icon={drifted ? <ExclamationCircleOutlined /> : undefined}
        message={drifted ? t('nodeDetail.driftFound') : t('nodeDetail.driftOk')}
        description={
          data?.checked_at
            ? `${t('nodeDetail.checkedAt')}: ${formatTime(data.checked_at)}${
                fixedRef.current ? ` · ${t('nodeDetail.driftFixed')}` : ''
              }`
            : undefined
        }
        action={
          <Space>
            <Button size="small" icon={<ReloadOutlined />} onClick={() => void refresh()}>
              {t('common.refresh')}
            </Button>
            {drifted && (
              <Button
                size="small"
                type="primary"
                icon={<ToolOutlined />}
                loading={fixing}
                onClick={handleFix}
              >
                {t('nodeDetail.driftFix')}
              </Button>
            )}
          </Space>
        }
      />

      <Descriptions bordered size="small" column={{ xs: 1, sm: 2, md: 3 }}>
        <Descriptions.Item label={t('nodeDetail.configVersion')}>
          <span className="or-mono">v{node.config_version}</span>
        </Descriptions.Item>
        <Descriptions.Item label={t('node.version')}>
          <span className="or-mono">{node.client_ver || '—'}</span>
        </Descriptions.Item>
        <Descriptions.Item label={t('nodeDetail.drift')}>
          <Badge
            status={drifted ? 'warning' : 'success'}
            text={drifted ? t('nodeDetail.driftFound') : t('nodeDetail.driftOk')}
          />
        </Descriptions.Item>
      </Descriptions>

      <Table<DriftItem>
        rowKey={(row) => `${row.kind}-${row.rule_id ?? 0}-${row.expected}-${row.actual}`}
        size="small"
        columns={columns}
        dataSource={items}
        loading={loading && items.length === 0}
        pagination={false}
        locale={{
          emptyText: (
            <div className="or-empty">
              <Empty
                image={Empty.PRESENTED_IMAGE_SIMPLE}
                description={t('nodeDetail.driftOk')}
              />
            </div>
          ),
        }}
      />

      <Alert
        type="info"
        showIcon
        message={t('nodeDetail.driftExplain')}
      />
    </Space>
  )
}

// ───────────────────────── 工具函数 ─────────────────────────

/** 后端同步状态字符串 → SyncStatusTag 的四种状态。 */
function normalizeSyncStatus(status: string): 'unsynced' | 'syncing' | 'normal' | 'failed' {
  switch (status) {
    case 'normal':
    case 'syncing':
    case 'failed':
    case 'unsynced':
      return status
    case 'success':
    case 'synced':
      return 'normal'
    default:
      return 'unsynced'
  }
}

/** 曲线点的横轴标签：优先用后端给的 RFC3339 文本，缺失时退回时间戳。 */
function pointLabel(point: ProbePoint): string {
  if (point.time) {
    const d = new Date(point.time)
    if (!Number.isNaN(d.getTime())) {
      return d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' })
    }
    return point.time
  }
  const d = new Date(point.timestamp * 1000)
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleTimeString()
}

/** 时间展示：当天只显示时分秒，其余显示完整时间。 */
function formatTime(value: string | null | undefined): string {
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
    : d.toLocaleString(undefined, {
        year: 'numeric',
        month: '2-digit',
        day: '2-digit',
        hour: '2-digit',
        minute: '2-digit',
      })
}

