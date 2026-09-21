/**
 * 节点卡片（规格书 9.2 仪表盘的「节点状态格子」、6.11 监控页缩略网格）。
 *
 * 一张卡片要在很小的面积里讲清「这台机器现在行不行」：
 * 在线点 → 名称 / 角色 → 健康度与漂移标记 → CPU / 内存 / 速率 / 连接数。
 */
import { Tooltip } from 'antd'
import { WarningOutlined } from '@ant-design/icons'

import type { Node } from '../api/types'
import { useI18n } from '../locales'
import { formatBytes } from './TrafficChart'

interface Props {
  node: Node
  onClick?: () => void
  /** 是否绘制缩略曲线（监控页与仪表盘开启）。 */
  showSparkline?: boolean
  /** 缩略曲线的数据点（字节/秒），为空时画一条水平基线。 */
  sparkline?: number[]
}

/** 健康度对应的颜色（规格书 6.1：红黄绿标识）。 */
export function healthColor(score: number): string {
  if (score >= 80) return 'var(--or-success)'
  if (score >= 60) return 'var(--or-warning)'
  return 'var(--or-error)'
}

/** 把秒数转成「12d 3h」「3h 20m」这类紧凑的可读时长（中英通用）。 */
export function formatUptime(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return '--'
  const days = Math.floor(seconds / 86400)
  const hours = Math.floor((seconds % 86400) / 3600)
  const minutes = Math.floor((seconds % 3600) / 60)
  if (days > 0) return `${days}d ${hours}h`
  if (hours > 0) return `${hours}h ${minutes}m`
  return `${minutes}m`
}

export default function NodeCard({ node, onClick, showSparkline = false, sparkline }: Props) {
  const { t } = useI18n()

  const memPercent =
    node.mem_total > 0 ? Math.min(100, (node.mem_used / node.mem_total) * 100) : 0
  const rate = (node.net_in_speed || 0) + (node.net_out_speed || 0)

  return (
    <div
      className="or-node-card"
      onClick={onClick}
      role={onClick ? 'button' : undefined}
      tabIndex={onClick ? 0 : undefined}
      onKeyDown={(e) => {
        if (onClick && (e.key === 'Enter' || e.key === ' ')) {
          e.preventDefault()
          onClick()
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
          <Tooltip title={node.name}>
            <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
              {node.name}
            </span>
          </Tooltip>
        </span>
        <span style={{ display: 'inline-flex', alignItems: 'center', gap: 4, flexShrink: 0 }}>
          {node.drift_detected && (
            <Tooltip title={t('node.drift')}>
              <WarningOutlined style={{ color: 'var(--or-warning)' }} />
            </Tooltip>
          )}
          <Tooltip title={`${t('node.healthScore')}: ${node.health_score}`}>
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
        </span>
      </div>

      {showSparkline && <Sparkline points={sparkline ?? []} />}

      <div className="or-node-card-metrics">
        <span>
          {t('node.cpu')} <b>{node.online ? `${formatPercent(node.cpu_usage)}%` : '--'}</b>
        </span>
        <span>
          {t('node.memory')}{' '}
          <b>{node.online && node.mem_total > 0 ? `${formatPercent(memPercent)}%` : '--'}</b>
        </span>
        <span>
          {t('node.speed')}{' '}
          <b className="or-mono">{node.online ? `${formatBytes(rate)}/s` : '--'}</b>
        </span>
        <span>
          {t('node.conn')} <b>{node.online ? node.current_conn : '--'}</b>
        </span>
      </div>
    </div>
  )
}

/** 百分比统一保留一位小数，避免卡片里出现「12.3456789」。 */
function formatPercent(value: number): string {
  if (!Number.isFinite(value)) return '0.0'
  return value.toFixed(1)
}

/**
 * 极简 sparkline。
 *
 * 刻意不用 @ant-design/plots 的 TinyLine：监控页一屏几十张卡片，
 * 每张都实例化一个 G2 画布会明显拖慢滚动，纯 SVG 折线足够表达趋势。
 */
function Sparkline({ points }: { points: number[] }) {
  const width = 180
  const height = 26

  if (points.length < 2) {
    return (
      <svg width="100%" height={height} viewBox={`0 0 ${width} ${height}`} preserveAspectRatio="none">
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
    )
  }

  const max = Math.max(...points, 1)
  const step = width / (points.length - 1)
  const path = points
    .map((p, i) => `${i === 0 ? 'M' : 'L'}${(i * step).toFixed(2)},${(height - (p / max) * (height - 2) - 1).toFixed(2)}`)
    .join(' ')

  return (
    <svg
      width="100%"
      height={height}
      viewBox={`0 0 ${width} ${height}`}
      preserveAspectRatio="none"
      style={{ display: 'block', marginBottom: 8 }}
    >
      <path d={path} fill="none" stroke="var(--or-primary)" strokeWidth={1.5} />
    </svg>
  )
}
