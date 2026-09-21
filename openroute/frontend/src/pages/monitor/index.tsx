/**
 * 全局监控（规格书 6.11 / 9.2）。
 *
 * 一屏看清「哪台机器现在不行了」：
 *  - 顶部汇总条：总数 / 在线 / 离线 / 亚健康；
 *  - 卡片网格：每台节点一张卡片，含关键指标与最近 5 分钟的迷你曲线；
 *  - 点卡片进节点详情。
 *
 * 数据来源是 trafficApi.probeOverview()（后端 GET /probe/overview），
 * 返回体是 { items: NodeOverview[], total, online, offline, unhealthy, ... }。
 * 缩略曲线直接用内联 SVG 画，不引入任何图表依赖：几十张卡片同屏时，
 * 每张都实例化一个 G2 画布会明显拖慢滚动。
 */
import { useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { Alert, Button, Empty, Segmented, Skeleton, Space, Tag, Tooltip, Typography } from 'antd'
import { ReloadOutlined, WarningOutlined } from '@ant-design/icons'

import { trafficApi } from '../../api'
import { formatBytes } from '../../components/TrafficChart'
import { healthColor, formatUptime } from '../../components/NodeCard'
import { usePolling } from '../../hooks/usePolling'
import { useI18n } from '../../locales'

const { Text } = Typography

// ───────────────────────── 后端真实响应形状 ─────────────────────────

/** 探针曲线上的一个点（app.ProbePoint）。 */
interface ProbePoint {
  timestamp: number
  time: string
  cpu: number
  mem_used: number
  mem_total: number
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

/** 单张卡片的数据（app.NodeOverview）。 */
interface NodeOverview {
  node_id: number
  name: string
  role: 'inbound' | 'outbound' | 'both' | string
  online: boolean
  public_ipv4: string
  group_ids: number[] | null

  cpu_usage: number
  mem_used: number
  mem_total: number
  disk_used: number
  disk_total: number
  net_in_speed: number
  net_out_speed: number
  load1: number
  tcp_conn: number
  udp_conn: number
  uptime: number

  current_conn: number
  health_score: number
  client_ver: string
  last_seen: string
  updated_at: string

  /** 最近 5 分钟的迷你曲线（后端返回原始探针点）。 */
  sparkline: ProbePoint[] | null
}

/** /probe/overview 的响应。 */
interface ProbeOverviewResponse {
  items: NodeOverview[] | null
  total: number
  online: number
  offline: number
  unhealthy: number
  probe_enabled: boolean
  keep_days: number
  sparkline_size: number
  generated_at: string
}

/** 卡片上画哪条曲线。 */
type MetricKey = 'cpu' | 'net' | 'mem'

export default function MonitorPage() {
  const { t } = useI18n()
  const navigate = useNavigate()
  const [metric, setMetric] = useState<MetricKey>('cpu')

  const { data, loading, error, refresh } = usePolling(
    () => trafficApi.probeOverview() as unknown as Promise<ProbeOverviewResponse>,
    { interval: 10000 },
  )

  const items = useMemo(() => data?.items ?? [], [data])

  // 汇总数字优先用后端的计数，缺失时按本地列表兜底计算。
  const total = data?.total ?? items.length
  const online = data?.online ?? items.filter((n) => n.online).length
  const offline = data?.offline ?? items.filter((n) => !n.online).length
  const unhealthy =
    data?.unhealthy ?? items.filter((n) => n.health_score < 60).length

  /** 曲线数据：把探针点映射成数值序列。 */
  const seriesOf = (node: NodeOverview): number[] => {
    const points = node.sparkline ?? []
    switch (metric) {
      case 'cpu':
        return points.map((p) => p.cpu)
      case 'mem':
        return points.map((p) => (p.mem_total > 0 ? (p.mem_used / p.mem_total) * 100 : 0))
      case 'net':
      default:
        return points.map((p) => (p.net_in_speed || 0) + (p.net_out_speed || 0))
    }
  }

  const renderHeader = () => (
    <div className="or-page-header">
      <div>
        <h1 className="or-page-title">{t('monitor.title')}</h1>
        <p className="or-page-desc">
          <Space size={12} wrap>
            <span>
              {t('monitor.totalCount')}{' '}
              <Text strong className="or-mono">
                {total}
              </Text>
            </span>
            <span>
              <span className="or-dot or-dot-online" aria-hidden />
              {t('node.online')}{' '}
              <Text strong className="or-mono">
                {online}
              </Text>
            </span>
            <span>
              <span className="or-dot or-dot-offline" aria-hidden />
              {t('node.offline')}{' '}
              <Text strong className="or-mono">
                {offline}
              </Text>
            </span>
            {unhealthy > 0 && (
              <span>
                <span className="or-dot or-dot-warning" aria-hidden />
                {t('monitor.unhealthy')}{' '}
                <Text strong className="or-mono" style={{ color: 'var(--or-warning)' }}>
                  {unhealthy}
                </Text>
              </span>
            )}
            {data?.generated_at && (
              <Text type="secondary" style={{ fontSize: 12 }}>
                {t('monitor.lastUpdate')}: {formatClock(data.generated_at)}
              </Text>
            )}
          </Space>
        </p>
      </div>
      <Space>
        <Segmented
          value={metric}
          onChange={(v) => setMetric(v as MetricKey)}
          options={[
            { value: 'cpu', label: t('node.cpu') },
            { value: 'mem', label: t('node.memory') },
            { value: 'net', label: t('nodeDetail.netSpeed') },
          ]}
        />
        <Button icon={<ReloadOutlined />} loading={loading} onClick={() => void refresh()}>
          {t('common.refresh')}
        </Button>
      </Space>
    </div>
  )

  // ── 空状态 / 加载 / 错误 ────────────────────────────────────────────────
  if (loading && items.length === 0) {
    return (
      <div>
        {renderHeader()}
        <Skeleton active paragraph={{ rows: 8 }} />
      </div>
    )
  }

  if (error && items.length === 0) {
    return (
      <div>
        {renderHeader()}
        <Alert
          type="error"
          showIcon
          message={t('monitor.loadFailed')}
          description={error instanceof Error ? error.message : undefined}
          action={
            <Button type="primary" onClick={() => void refresh()}>
              {t('common.refresh')}
            </Button>
          }
        />
      </div>
    )
  }

  return (
    <div>
      {renderHeader()}

      {/* 探针关闭时接口仍然返回 200，这里明确提示「没有数据的原因」 */}
      {data && !data.probe_enabled && (
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 12 }}
          message={t('monitor.probeDisabled')}
          description={t('monitor.probeDisabledHint')}
        />
      )}

      {items.length === 0 ? (
        <div className="or-empty">
          <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={t('monitor.empty')}>
            <Space direction="vertical" size={8}>
              <Text type="secondary" style={{ fontSize: 12 }}>
                {t('monitor.emptyHint')}
              </Text>
              <Button type="primary" onClick={() => navigate('/nodes')}>
                {t('monitor.goNodes')}
              </Button>
            </Space>
          </Empty>
        </div>
      ) : (
        <div className="or-node-grid">
          {items.map((node) => (
            <div
              key={node.node_id}
              className="or-node-card"
              role="button"
              tabIndex={0}
              onClick={() => navigate(`/nodes/${node.node_id}`)}
              onKeyDown={(e) => {
                if (e.key === 'Enter' || e.key === ' ') {
                  e.preventDefault()
                  navigate(`/nodes/${node.node_id}`)
                }
              }}
            >
              <div className="or-node-card-title">
                <span
                  style={{
                    display: 'inline-flex',
                    alignItems: 'center',
                    minWidth: 0,
                    overflow: 'hidden',
                  }}
                >
                  <span
                    className={node.online ? 'or-dot or-dot-online' : 'or-dot or-dot-offline'}
                    aria-hidden
                  />
                  <Tooltip title={`${node.name} · ${node.public_ipv4 || '—'}`}>
                    <span
                      style={{
                        overflow: 'hidden',
                        textOverflow: 'ellipsis',
                        whiteSpace: 'nowrap',
                      }}
                    >
                      {node.name}
                    </span>
                  </Tooltip>
                </span>
                <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6, flexShrink: 0 }}>
                  <Tag style={{ margin: 0, fontSize: 11 }}>{t(`node.role.${node.role}`)}</Tag>
                  <Tooltip title={`${t('node.healthScore')}: ${node.health_score} / 100`}>
                    <span
                      style={{
                        fontSize: 12,
                        fontWeight: 600,
                        color: healthColor(node.health_score),
                        fontFamily: 'ui-monospace, Menlo, Consolas, monospace',
                      }}
                    >
                      {node.health_score}
                    </span>
                  </Tooltip>
                  {node.health_score < 60 && (
                    <WarningOutlined style={{ color: 'var(--or-warning)', fontSize: 12 }} />
                  )}
                </span>
              </div>

              <Sparkline
                points={seriesOf(node)}
                color={metric === 'cpu' ? '#165dff' : metric === 'mem' ? '#00b42a' : '#ff7d00'}
                emptyLabel={t('monitor.noData')}
              />

              <div className="or-node-card-metrics">
                <span>
                  {t('node.cpu')}{' '}
                  <b>{node.online ? `${node.cpu_usage.toFixed(1)}%` : '--'}</b>
                </span>
                <span>
                  {t('node.memory')}{' '}
                  <b>
                    {node.online && node.mem_total > 0
                      ? `${((node.mem_used / node.mem_total) * 100).toFixed(1)}%`
                      : '--'}
                  </b>
                </span>
                <span>
                  {t('nodeDetail.netIn')}{' '}
                  <b className="or-mono">{node.online ? `${formatBytes(node.net_in_speed)}/s` : '--'}</b>
                </span>
                <span>
                  {t('nodeDetail.netOut')}{' '}
                  <b className="or-mono">
                    {node.online ? `${formatBytes(node.net_out_speed)}/s` : '--'}
                  </b>
                </span>
                <span>
                  {t('node.conn')} <b>{node.online ? node.current_conn : '--'}</b>
                </span>
                <span>
                  {t('nodeDetail.tcpConn')} <b>{node.online ? node.tcp_conn : '--'}</b>
                </span>
                <span>
                  {t('nodeDetail.load')}{' '}
                  <b className="or-mono">{node.online ? node.load1.toFixed(2) : '--'}</b>
                </span>
                <span>
                  {t('node.disk')}{' '}
                  <b>
                    {node.disk_total > 0
                      ? `${((node.disk_used / node.disk_total) * 100).toFixed(1)}%`
                      : '--'}
                  </b>
                </span>
                <span>
                  {t('nodeDetail.uptime')}{' '}
                  <b className="or-mono">{node.online ? formatUptime(node.uptime) : '--'}</b>
                </span>
                <span>
                  {t('node.version')} <b className="or-mono">{node.client_ver || '--'}</b>
                </span>
                <span>
                  {t('node.lastSeen')}{' '}
                  <b className="or-mono">{node.last_seen ? formatClock(node.last_seen) : '--'}</b>
                </span>
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}

/**
 * 迷你曲线。
 *
 * 纯 SVG，无依赖：点数不足 2 个时画一条虚线基线，
 * 提示「有卡片但还没有曲线数据」，而不是留给用户一块空白。
 */
function Sparkline({
  points,
  color,
  emptyLabel,
}: {
  points: number[]
  color: string
  emptyLabel: string
}) {
  const width = 200
  const height = 28
  const values = points.filter((p) => Number.isFinite(p))

  if (values.length < 2) {
    return (
      <Tooltip title={emptyLabel}>
        <svg
          width="100%"
          height={height}
          viewBox={`0 0 ${width} ${height}`}
          preserveAspectRatio="none"
          style={{ display: 'block', marginBottom: 8 }}
        >
          <line
            x1={0}
            y1={height - 1}
            x2={width}
            y2={height - 1}
            stroke="var(--or-border)"
            strokeWidth={1}
            strokeDasharray="3 3"
          />
        </svg>
      </Tooltip>
    )
  }

  const max = Math.max(...values, 1)
  const step = width / (values.length - 1)
  const path = values
    .map((p, i) => {
      const x = (i * step).toFixed(2)
      const y = (height - (p / max) * (height - 2) - 1).toFixed(2)
      return `${i === 0 ? 'M' : 'L'}${x},${y}`
    })
    .join(' ')

  const area = `${path} L${width},${height} L0,${height} Z`

  return (
    <svg
      width="100%"
      height={height}
      viewBox={`0 0 ${width} ${height}`}
      preserveAspectRatio="none"
      style={{ display: 'block', marginBottom: 8 }}
      role="img"
      aria-label={emptyLabel}
    >
      <path d={area} fill={color} fillOpacity={0.12} stroke="none" />
      <path d={path} fill="none" stroke={color} strokeWidth={1.5} vectorEffect="non-scaling-stroke" />
    </svg>
  )
}

/** 时间展示：只保留时分秒，监控页看的是「刚刚」。 */
function formatClock(value: string): string {
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return value
  return d.toLocaleTimeString()
}
