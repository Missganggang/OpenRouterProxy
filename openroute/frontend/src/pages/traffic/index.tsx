/**
 * 流量统计页面（规格书 6.10、9.2）。
 *
 * 四个统计维度（用户 / 规则 / 节点 / 方向）+ 两种粒度（小时 / 天），
 * 并提供规格书 6.10「统计口径」要求的原始值 / 乘倍率值切换。
 *
 * 折线图刻意内联实现（SVG），不依赖图表库也不复用其它页面的组件：
 * 本页只需要「一条面积折线 + 悬浮读值」，引入通用图表库的收益不抵包体积。
 */
import { useCallback, useEffect, useMemo, useState } from 'react'
import {
  Alert,
  Button,
  Card,
  Col,
  DatePicker,
  Empty,
  Row,
  Segmented,
  Space,
  Statistic,
  Switch,
  Table,
  Tag,
  Tooltip,
  Typography,
  message,
} from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { DownloadOutlined, ReloadOutlined } from '@ant-design/icons'
import dayjs, { type Dayjs } from 'dayjs'

import { showApiError, trafficApi } from '../../api'
import type {
  TrafficOverview,
  TrafficPeriod,
  TrafficPoint,
  TrafficTopItem,
} from '../../api/types'

/**
 * 按当前口径取出一个统计周期对象的展示值。
 *
 * 后端的周期是 {from,to,in,out,total,raw} 结构，而不是裸数字：
 * 直接 formatBytes(period) 会得到 NaN / 空值，必须显式取 .total 或 .raw。
 */
function periodValue(p: TrafficPeriod | undefined, raw: boolean): number {
  if (!p) return 0
  return raw ? p.raw : p.total
}
import { useI18n } from '../../locales'

const { Text } = Typography

/** 统计维度。 */
type Dimension = 'user' | 'rule' | 'node' | 'direction'

/** 时间粒度。 */
type Interval = 'hour' | 'day'

/** 快捷时间范围。 */
type RangeKey = '24h' | '7d' | '30d' | 'custom'

export default function TrafficPage() {
  const { t } = useI18n()

  const [dimension, setDimension] = useState<Dimension>('user')
  const [interval, setInterval] = useState<Interval>('hour')
  const [rangeKey, setRangeKey] = useState<RangeKey>('24h')
  const [customRange, setCustomRange] = useState<[Dayjs, Dayjs] | null>(null)
  /** 统计口径开关：true 显示乘倍率后的值（默认），false 显示原始字节。 */
  const [showRaw, setShowRaw] = useState(false)

  const [points, setPoints] = useState<TrafficPoint[]>([])
  const [top, setTop] = useState<TrafficTopItem[]>([])
  // 用后端真实的概览结构：每个周期是对象而不是数字。
  const [overview, setOverview] = useState<TrafficOverview | null>(null)
  const [loading, setLoading] = useState(false)

  /** 把快捷范围换算成 from/to。 */
  const range = useMemo<{ from: string; to: string }>(() => {
    const now = dayjs()
    if (rangeKey === 'custom' && customRange) {
      return {
        from: customRange[0].startOf('day').format('YYYY-MM-DD HH:mm:ss'),
        to: customRange[1].endOf('day').format('YYYY-MM-DD HH:mm:ss'),
      }
    }
    const days = rangeKey === '7d' ? 7 : rangeKey === '30d' ? 30 : 1
    // 24 小时档按小时展示，更长范围自动切到天，避免曲线点过于稀疏或密集。
    return {
      from: now.subtract(days, 'day').format('YYYY-MM-DD HH:mm:ss'),
      to: now.format('YYYY-MM-DD HH:mm:ss'),
    }
  }, [rangeKey, customRange])

  // 快捷范围与粒度联动：超过 3 天时默认按天，用户仍可手动改回。
  useEffect(() => {
    if (rangeKey === '7d' || rangeKey === '30d') setInterval('day')
    else if (rangeKey === '24h') setInterval('hour')
  }, [rangeKey])

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const [series, topRes, ov] = await Promise.all([
        trafficApi.timeseries({
          from: range.from,
          to: range.to,
          interval,
          group_by: dimension,
        }),
        trafficApi.top({
          dimension: dimension === 'direction' ? 'user' : dimension,
          limit: 20,
          from: range.from,
          to: range.to,
        }),
        trafficApi.overview(),
      ])
      setPoints(series.points ?? [])
      setTop(topRes ?? [])
      setOverview(ov)
    } catch (err) {
      showApiError(err, t('traffic.loadFailed'))
    } finally {
      setLoading(false)
    }
  }, [range.from, range.to, interval, dimension, t])

  useEffect(() => {
    void load()
  }, [load])

  /** 当前口径下每个点的取值。 */
  const valueOf = useCallback(
    (p: TrafficPoint) => (showRaw ? Number(p.raw_bytes ?? 0) : Number(p.bytes ?? 0)),
    [showRaw],
  )

  const rangeTotal = useMemo(
    () => points.reduce((sum, p) => sum + valueOf(p), 0),
    [points, valueOf],
  )
  const peak = useMemo(() => {
    let max = 0
    let at = ''
    points.forEach((p) => {
      const v = valueOf(p)
      if (v > max) {
        max = v
        at = p.time
      }
    })
    return { max, at }
  }, [points, valueOf])

  /** 导出当前筛选下的 CSV（口径跟随开关，避免导出与界面不一致）。 */
  const exportCSV = () => {
    if (!points.length) {
      message.warning(t('traffic.nothingToExport'))
      return
    }
    // 表头同时给出两种口径，当前口径写在 exported_bytes 里，
    // 用户拿到的 CSV 无论怎么切换都能看到对应数值。
    const header = ['time', 'raw_bytes', 'billed_bytes', 'exported_bytes'].join(',')
    const lines = points.map((p) =>
      [p.time, p.raw_bytes ?? 0, p.bytes ?? 0, valueOf(p)].join(','),
    )
    const csv = `﻿${header}\n${lines.join('\n')}`
    const blob = new Blob([csv], { type: 'text/csv;charset=utf-8' })
    const link = document.createElement('a')
    link.href = URL.createObjectURL(blob)
    link.download = `traffic-${dimension}-${interval}-${dayjs().format('YYYYMMDD-HHmm')}.csv`
    document.body.appendChild(link)
    link.click()
    document.body.removeChild(link)
    URL.revokeObjectURL(link.href)
    message.success(t('traffic.exportDone', { n: points.length }))
  }

  /** 直接从后端导出（服务端 CSV，字段更全）。 */
  const exportServerCSV = async () => {
    try {
      await trafficApi.exportCSV({
        from: range.from,
        to: range.to,
        interval,
        group_by: dimension,
      })
      message.success(t('traffic.exportQueued'))
    } catch (err) {
      showApiError(err, t('traffic.exportFailed'))
    }
  }

  const seriesColumns: ColumnsType<TrafficPoint> = [
    {
      title: t('traffic.timeBucket'),
      dataIndex: 'time',
      key: 'time',
      width: 200,
      render: (v: string) => <Text className="or-mono">{v}</Text>,
    },
    {
      title: t('traffic.rawBytes'),
      dataIndex: 'raw_bytes',
      key: 'raw_bytes',
      width: 140,
      render: (v: number) => <Text className="or-mono">{formatBytes(v)}</Text>,
    },
    {
      title: t('traffic.billedBytes'),
      dataIndex: 'bytes',
      key: 'bytes',
      width: 140,
      render: (v: number) => <Text className="or-mono">{formatBytes(v)}</Text>,
    },
    {
      title: t('traffic.multiplierRatio'),
      key: 'ratio',
      width: 120,
      render: (_, row) => {
        const raw = Number(row.raw_bytes ?? 0)
        const billed = Number(row.bytes ?? 0)
        if (!raw) return <Text type="secondary">-</Text>
        const ratio = billed / raw
        return (
          <Tag color={ratio > 1 ? 'orange' : ratio < 1 ? 'blue' : 'default'}>
            ×{ratio.toFixed(3)}
          </Tag>
        )
      },
    },
  ]

  const topColumns: ColumnsType<TrafficTopItem> = [
    {
      title: '#',
      key: 'index',
      width: 60,
      render: (_, __, i) => <Text className="or-mono">{i + 1}</Text>,
    },
    {
      title: t('traffic.name'),
      dataIndex: 'name',
      key: 'name',
      render: (v: string, row) => (
        <Space direction="vertical" size={0}>
          <Text>{v || `#${row.id}`}</Text>
          {row.id ? (
            <Text type="secondary" className="or-mono" style={{ fontSize: 11 }}>
              #{row.id}
            </Text>
          ) : null}
        </Space>
      ),
    },
    {
      title: t('traffic.rawBytes'),
      dataIndex: 'raw_bytes',
      key: 'raw_bytes',
      width: 140,
      render: (v: number) => <Text className="or-mono">{formatBytes(v ?? 0)}</Text>,
    },
    {
      title: t('traffic.billedBytes'),
      dataIndex: 'bytes',
      key: 'bytes',
      width: 140,
      render: (v: number) => <Text className="or-mono">{formatBytes(v ?? 0)}</Text>,
    },
  ]

  return (
    <div>
      <div className="or-page-header">
        <div>
          <h2 className="or-page-title">{t('traffic.title')}</h2>
          <p className="or-page-desc">{t('traffic.pageDesc')}</p>
        </div>
        <Space>
          <Button icon={<ReloadOutlined />} onClick={() => void load()}>
            {t('common.refresh')}
          </Button>
          <Button icon={<DownloadOutlined />} onClick={exportCSV}>
            {t('traffic.exportCSV')}
          </Button>
          <Tooltip title={t('traffic.exportServerHint')}>
            <Button icon={<DownloadOutlined />} onClick={() => void exportServerCSV()}>
              {t('traffic.exportServer')}
            </Button>
          </Tooltip>
        </Space>
      </div>

      {/* ── 概览卡片 ─────────────────────────────────── */}
      {/* 注意：overview 的每个周期是对象 {from,to,in,out,total,raw}，
          取展示值时要用 .total / .raw，不能直接当数字用（会渲染成 undefined）。 */}
      <Row gutter={12} style={{ marginBottom: 12 }}>
        <Col span={6}>
          <Card size="small">
            <Statistic
              title={t('traffic.today')}
              value={formatBytes(periodValue(overview?.today, showRaw))}
              valueStyle={{ fontSize: 20 }}
            />
          </Card>
        </Col>
        <Col span={6}>
          <Card size="small">
            <Statistic
              title={t('traffic.thisMonth')}
              value={formatBytes(periodValue(overview?.month, showRaw))}
              valueStyle={{ fontSize: 20 }}
            />
          </Card>
        </Col>
        <Col span={6}>
          <Card size="small">
            <Statistic
              title={t('traffic.total')}
              value={formatBytes(periodValue(overview?.total, showRaw))}
              valueStyle={{ fontSize: 20 }}
            />
          </Card>
        </Col>
        <Col span={6}>
          <Card size="small">
            <Statistic
              title={t('traffic.activeRules')}
              value={overview?.active_rules ?? 0}
              suffix={
                <Text type="secondary" style={{ fontSize: 12 }}>
                  / {t('traffic.onlineNodes')} {overview?.online_nodes ?? 0}
                </Text>
              }
              valueStyle={{ fontSize: 20 }}
            />
          </Card>
        </Col>
      </Row>

      {/* ── 筛选 ─────────────────────────────────────── */}
      <Card size="small" style={{ marginBottom: 12 }}>
        <Space wrap size={12}>
          <Space size={4}>
            <Text type="secondary" style={{ fontSize: 12 }}>
              {t('traffic.timeRange')}
            </Text>
            <Segmented
              value={rangeKey}
              onChange={(v) => setRangeKey(v as RangeKey)}
              options={[
                { label: t('traffic.last24h'), value: '24h' },
                { label: t('traffic.last7d'), value: '7d' },
                { label: t('traffic.last30d'), value: '30d' },
                { label: t('traffic.custom'), value: 'custom' },
              ]}
            />
          </Space>

          {rangeKey === 'custom' && (
            <DatePicker.RangePicker
              value={customRange}
              onChange={(v) => {
                if (v && v[0] && v[1]) setCustomRange([v[0], v[1]])
              }}
            />
          )}

          <Space size={4}>
            <Text type="secondary" style={{ fontSize: 12 }}>
              {t('traffic.dimension')}
            </Text>
            <Segmented
              value={dimension}
              onChange={(v) => setDimension(v as Dimension)}
              options={[
                { label: t('traffic.byUser'), value: 'user' },
                { label: t('traffic.byRule'), value: 'rule' },
                { label: t('traffic.byNode'), value: 'node' },
                { label: t('traffic.byDirection'), value: 'direction' },
              ]}
            />
          </Space>

          <Space size={4}>
            <Text type="secondary" style={{ fontSize: 12 }}>
              {t('traffic.interval')}
            </Text>
            <Segmented
              value={interval}
              onChange={(v) => setInterval(v as Interval)}
              options={[
                { label: t('traffic.hour'), value: 'hour' },
                { label: t('traffic.day'), value: 'day' },
              ]}
            />
          </Space>

          {/* 规格书 6.10 统计口径：默认展示乘倍率后的值，可切换原始值 */}
          <Space size={4}>
            <Switch size="small" checked={showRaw} onChange={setShowRaw} />
            <Tooltip title={t('traffic.showRawHint')}>
              <Text style={{ fontSize: 12 }}>{t('traffic.showRaw')}</Text>
            </Tooltip>
          </Space>

          <Tag color={showRaw ? 'blue' : 'orange'}>
            {showRaw ? t('traffic.caliberRaw') : t('traffic.caliberBilled')}
          </Tag>
        </Space>
      </Card>

      <Alert
        type="info"
        showIcon
        style={{ marginBottom: 12 }}
        message={t('traffic.caliberNote')}
        description={<Text style={{ fontSize: 12 }}>{t('traffic.caliberNoteDetail')}</Text>}
      />

      {/* ── 曲线 ─────────────────────────────────────── */}
      <Card
        size="small"
        title={`${t('traffic.seriesTitle')} · ${dimensionLabel(dimension, t)}`}
        style={{ marginBottom: 12 }}
        extra={
          <Space size={16}>
            <Text type="secondary" style={{ fontSize: 12 }}>
              {t('traffic.rangeTotal')}: <Text strong>{formatBytes(rangeTotal)}</Text>
            </Text>
            <Text type="secondary" style={{ fontSize: 12 }}>
              {t('traffic.peak')}: <Text strong>{formatBytes(peak.max)}</Text>
              {peak.at ? ` @ ${peak.at}` : ''}
            </Text>
          </Space>
        }
      >
        {points.length === 0 ? (
          <Empty
            className="or-empty"
            description={
              <Space direction="vertical" size={4}>
                <Text>{t('traffic.emptySeries')}</Text>
                <Text type="secondary" style={{ fontSize: 12 }}>
                  {t('traffic.emptySeriesHint')}
                </Text>
              </Space>
            }
          />
        ) : (
          <LineAreaChart
            data={points.map((p) => ({ label: p.time, value: valueOf(p) }))}
            height={260}
          />
        )}
      </Card>

      {/* ── 明细 / 排行 ──────────────────────────────── */}
      <Row gutter={12}>
        <Col span={14}>
          <Card size="small" title={t('traffic.seriesTable')} loading={loading}>
            <Table<TrafficPoint>
              rowKey="time"
              size="small"
              pagination={{ pageSize: 12, showSizeChanger: false }}
              dataSource={points}
              columns={seriesColumns}
              scroll={{ x: 600 }}
              locale={{
                emptyText: <Empty className="or-empty" description={t('traffic.emptySeries')} />,
              }}
            />
          </Card>
        </Col>
        <Col span={10}>
          <Card
            size="small"
            title={`${t('traffic.topTitle')} · ${dimensionLabel(dimension, t)}`}
            loading={loading}
          >
            <Table<TrafficTopItem>
              rowKey={(r, i) => `${r.id}-${i}`}
              size="small"
              pagination={false}
              dataSource={top}
              columns={topColumns}
              scroll={{ x: 420 }}
              locale={{
                emptyText: <Empty className="or-empty" description={t('traffic.emptyTop')} />,
              }}
            />
            <Text type="secondary" style={{ fontSize: 12 }}>
              {t('traffic.topHint')}
            </Text>
          </Card>
        </Col>
      </Row>
    </div>
  )
}

/* ═══════════════════════ 内联折线图 ═══════════════════════ */

interface ChartPoint {
  label: string
  value: number
}

/**
 * 极简面积折线图（SVG）。
 *
 * 只做三件事：按最大值归一化、画出折线与渐变填充、鼠标悬浮时显示该点读数。
 * 用 SVG 而非 canvas：数据点通常只有几十个，SVG 足以支撑且能直接跟随 CSS 变量换色。
 */
function LineAreaChart({ data, height = 240 }: { data: ChartPoint[]; height?: number }) {
  const [hover, setHover] = useState<number | null>(null)
  const width = 1000
  const paddingX = 8
  const paddingY = 16

  const max = Math.max(1, ...data.map((d) => d.value))
  const stepX = data.length > 1 ? (width - paddingX * 2) / (data.length - 1) : 0

  /** 把索引映射为坐标；只有 1 个点时画在中间。 */
  const coords = data.map((d, i) => {
    const x = data.length > 1 ? paddingX + i * stepX : width / 2
    const y = height - paddingY - (d.value / max) * (height - paddingY * 2)
    return { x, y, ...d }
  })

  const linePath = coords.map((c, i) => `${i === 0 ? 'M' : 'L'}${c.x.toFixed(1)},${c.y.toFixed(1)}`).join(' ')
  const areaPath = `${linePath} L${coords[coords.length - 1]?.x.toFixed(1) ?? 0},${height - paddingY} L${coords[0]?.x.toFixed(1) ?? 0},${height - paddingY} Z`

  const hovered = hover !== null ? coords[hover] : undefined

  return (
    <div style={{ position: 'relative' }}>
      <svg
        viewBox={`0 0 ${width} ${height}`}
        width="100%"
        height={height}
        preserveAspectRatio="none"
        style={{ display: 'block', overflow: 'visible' }}
        onMouseLeave={() => setHover(null)}
        onMouseMove={(e) => {
          // 把鼠标位置换算成最近的数据点索引。
          const rect = (e.target as SVGElement).ownerSVGElement?.getBoundingClientRect()
          if (!rect) return
          const relX = ((e.clientX - rect.left) / rect.width) * width
          const idx = stepX > 0 ? Math.round((relX - paddingX) / stepX) : 0
          setHover(Math.max(0, Math.min(data.length - 1, idx)))
        }}
      >
        <defs>
          <linearGradient id="or-traffic-fill" x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stopColor="var(--or-primary)" stopOpacity="0.28" />
            <stop offset="100%" stopColor="var(--or-primary)" stopOpacity="0.02" />
          </linearGradient>
        </defs>

        {/* 三条水平参考线：0 / 50% / 100% */}
        {[0, 0.5, 1].map((r) => {
          const y = height - paddingY - r * (height - paddingY * 2)
          return (
            <line
              key={r}
              x1={0}
              x2={width}
              y1={y}
              y2={y}
              stroke="var(--or-border)"
              strokeWidth={1}
              strokeDasharray={r === 0 ? undefined : '4 6'}
              vectorEffect="non-scaling-stroke"
            />
          )
        })}

        <path d={areaPath} fill="url(#or-traffic-fill)" />
        <path
          d={linePath}
          fill="none"
          stroke="var(--or-primary)"
          strokeWidth={2}
          vectorEffect="non-scaling-stroke"
          strokeLinejoin="round"
        />

        {hovered && (
          <>
            <line
              x1={hovered.x}
              x2={hovered.x}
              y1={paddingY}
              y2={height - paddingY}
              stroke="var(--or-primary)"
              strokeWidth={1}
              strokeDasharray="3 3"
              vectorEffect="non-scaling-stroke"
            />
            <circle cx={hovered.x} cy={hovered.y} r={4} fill="var(--or-primary)" />
          </>
        )}
      </svg>

      {/* 纵轴：最大值标在左上，便于快速读数量级 */}
      <Text
        type="secondary"
        style={{ position: 'absolute', top: -4, left: 4, fontSize: 11 }}
      >
        {formatBytes(max)}
      </Text>

      {/* 横轴：首尾时间 */}
      <Space
        style={{ justifyContent: 'space-between', width: '100%', marginTop: 4 }}
        size={0}
      >
        <Text type="secondary" style={{ fontSize: 11 }}>
          {data[0]?.label ?? ''}
        </Text>
        <Text type="secondary" style={{ fontSize: 11 }}>
          {data[data.length - 1]?.label ?? ''}
        </Text>
      </Space>

      {hovered && (
        <div
          style={{
            position: 'absolute',
            top: 0,
            right: 0,
            background: 'var(--or-bg-elevated)',
            border: '1px solid var(--or-border)',
            borderRadius: 'var(--or-radius)',
            padding: '4px 8px',
            fontSize: 12,
            pointerEvents: 'none',
          }}
        >
          <Text className="or-mono" style={{ fontSize: 12 }}>
            {hovered.label} · {formatBytes(hovered.value)}
          </Text>
        </div>
      )}
    </div>
  )
}

/* ═══════════════════════ 工具 ═══════════════════════ */

/** 维度文案。 */
function dimensionLabel(
  d: Dimension,
  t: (k: string, vars?: Record<string, string | number>) => string,
): string {
  switch (d) {
    case 'user':
      return t('traffic.byUser')
    case 'rule':
      return t('traffic.byRule')
    case 'node':
      return t('traffic.byNode')
    default:
      return t('traffic.byDirection')
  }
}

/** 字节 → 人类可读。 */
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
