/**
 * 仪表盘（规格书 9.2）。
 *
 * 一屏回答三个问题：
 *  1. 今天跑了多少流量（今日 / 昨日 / 本月 / 累计）；
 *  2. 机器和线路现在什么状态（在线节点 / 在线用户 / 活跃规则 + 节点格子）；
 *  3. 有没有需要我立刻处理的事（同步失败规则 / 离线节点 / 活跃告警）。
 *
 * 数据用 trafficApi.dashboard() 一次拿全，30 秒轮询（规格书 9.3）。
 *
 * 关于字段口径（容易踩坑）：后端的每个统计周期是**对象**而不是数字，
 * 形如 {from, to, in, out, total, raw}。展示时取 .total（当前口径），
 * 切到「显示原始值」时取 .raw。
 */
import { useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import {
  Alert,
  Badge,
  Button,
  Card,
  Col,
  Empty,
  List,
  Row,
  Skeleton,
  Space,
  Tag,
  Tooltip,
  Typography,
} from 'antd'
import {
  ArrowRightOutlined,
  CloudServerOutlined,
  DisconnectOutlined,
  PlusOutlined,
  ReloadOutlined,
  SyncOutlined,
  TeamOutlined,
  ThunderboltOutlined,
  WarningOutlined,
} from '@ant-design/icons'

import { trafficApi } from '../../api'
import type { DashboardData, TrafficPeriod } from '../../api/types'
import { usePolling } from '../../hooks/usePolling'
import { useI18n } from '../../locales'
import TrafficChart, { formatBytes } from '../../components/TrafficChart'

const { Text } = Typography

/** 流量卡片的取数口径。 */
type TrafficKey = 'today' | 'yesterday' | 'thisMonth' | 'total'

/** 按当前口径取出一个周期对象的展示值。 */
function periodValue(p: TrafficPeriod | undefined, raw: boolean): number {
  if (!p) return 0
  return raw ? p.raw : p.total
}

export default function DashboardPage() {
  const navigate = useNavigate()
  const { t } = useI18n()
  const [showRaw, setShowRaw] = useState(false)

  const { data, loading, error, refresh } = usePolling<DashboardData>(
    () => trafficApi.dashboard(),
    { interval: 30000 },
  )

  const overview = data?.overview

  const trafficCards = useMemo(() => {
    if (!overview) {
      return [] as Array<{ key: TrafficKey; value: number; color: string }>
    }
    return [
      { key: 'today' as TrafficKey, value: periodValue(overview.today, showRaw), color: 'var(--or-primary)' },
      { key: 'yesterday' as TrafficKey, value: periodValue(overview.yesterday, showRaw), color: 'var(--or-info)' },
      { key: 'thisMonth' as TrafficKey, value: periodValue(overview.month, showRaw), color: 'var(--or-warning)' },
      { key: 'total' as TrafficKey, value: periodValue(overview.total, showRaw), color: 'var(--or-success)' },
    ]
  }, [overview, showRaw])

  // 待处理事项：后端把三类平铺在顶层，并已算好总数 pending_issues。
  //
  // 这里统一用 asArray 兜底：旧版前端假设数据在 pending.{...} 下，
  // 字段名对不上时拿到 undefined，随后 .slice() / .length 直接抛
  // "Cannot read properties of undefined"，React 卸载整棵树 → 白屏。
  // 接口字段可以变，但页面不能因此崩掉。
  const syncFailed = asArray(data?.sync_failed_rules)
  const offlineNodes = asArray(data?.offline_nodes)
  const activeAlerts = asArray(data?.active_alerts)
  const pendingCount = data?.pending_issues ?? 0

  // 「实时流量曲线」用 trend：按小时的 direction 维度序列。
  const trendPoints = useMemo(
    () =>
      (data?.trend ?? []).map((p) => ({
        time: p.time,
        bytes: p.total,
        raw_bytes: p.raw,
      })),
    [data],
  )

  return (
    <div>
      <div className="or-page-header">
        <div>
          <h1 className="or-page-title">{t('dashboard.title')}</h1>
          <p className="or-page-desc">{t('traffic.overview')}</p>
        </div>
        <Space>
          <Tooltip title={t('traffic.showRaw')}>
            <Button
              size="small"
              type={showRaw ? 'primary' : 'default'}
              ghost={showRaw}
              onClick={() => setShowRaw((v) => !v)}
            >
              {t('traffic.showRaw')}
            </Button>
          </Tooltip>
          <Button icon={<ReloadOutlined />} onClick={() => void refresh()} loading={loading}>
            {t('common.refresh')}
          </Button>
          <Button type="primary" icon={<PlusOutlined />} onClick={() => navigate('/nodes?action=create')}>
            {t('node.add')}
          </Button>
        </Space>
      </div>

      {error !== undefined && error !== null && (
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 16 }}
          message={t('common.refresh')}
          description={String((error as Error)?.message ?? '')}
        />
      )}

      {/* 流量卡片 */}
      <Row gutter={[12, 12]}>
        {loading && !overview
          ? [0, 1, 2, 3].map((i) => (
              <Col xs={24} sm={12} lg={6} key={i}>
                <Card size="small">
                  <Skeleton active paragraph={{ rows: 1 }} title={false} />
                </Card>
              </Col>
            ))
          : trafficCards.map((card) => (
              <Col xs={24} sm={12} lg={6} key={card.key}>
                <Card size="small" styles={{ body: { padding: 16 } }}>
                  <Text type="secondary" style={{ fontSize: 12 }}>
                    {t(`traffic.${card.key}`)}
                  </Text>
                  <div
                    className="or-mono"
                    style={{
                      fontSize: 24,
                      fontWeight: 600,
                      color: card.color,
                      marginTop: 4,
                      lineHeight: 1.3,
                    }}
                  >
                    {formatBytes(card.value)}
                  </div>
                </Card>
              </Col>
            ))}
      </Row>

      {/* 在线节点 / 在线用户 / 活跃规则 */}
      <Row gutter={[12, 12]} style={{ marginTop: 12 }}>
        <Col xs={24} sm={8}>
          <Card size="small" styles={{ body: { padding: 16 } }}>
            <Space align="center" size={12}>
              <CloudServerOutlined style={{ fontSize: 20, color: 'var(--or-success)' }} />
              <div>
                <Text type="secondary" style={{ fontSize: 12 }}>
                  {t('traffic.onlineNodes')}
                </Text>
                <div style={{ fontSize: 20, fontWeight: 600 }}>
                  {overview ? (
                    <>
                      {overview.online_nodes}
                      <Text type="secondary" style={{ fontSize: 13, fontWeight: 400 }}>
                        {' '}
                        / {overview.total_nodes}
                      </Text>
                    </>
                  ) : (
                    '--'
                  )}
                </div>
              </div>
            </Space>
          </Card>
        </Col>
        <Col xs={24} sm={8}>
          <Card size="small" styles={{ body: { padding: 16 } }}>
            <Space align="center" size={12}>
              <TeamOutlined style={{ fontSize: 20, color: 'var(--or-primary)' }} />
              <div>
                <Text type="secondary" style={{ fontSize: 12 }}>
                  {t('traffic.onlineUsers')}
                </Text>
                <div style={{ fontSize: 20, fontWeight: 600 }}>{overview?.online_users ?? '--'}</div>
              </div>
            </Space>
          </Card>
        </Col>
        <Col xs={24} sm={8}>
          <Card size="small" styles={{ body: { padding: 16 } }}>
            <Space align="center" size={12}>
              <ThunderboltOutlined style={{ fontSize: 20, color: 'var(--or-warning)' }} />
              <div>
                <Text type="secondary" style={{ fontSize: 12 }}>
                  {t('traffic.activeRules')}
                </Text>
                <div style={{ fontSize: 20, fontWeight: 600 }}>{overview?.active_rules ?? '--'}</div>
              </div>
            </Space>
          </Card>
        </Col>
      </Row>

      <Row gutter={[12, 12]} style={{ marginTop: 12 }}>
        {/* 实时流量曲线 */}
        <Col xs={24} xl={16}>
          <Card
            size="small"
            title={t('dashboard.traffic24h')}
            extra={
              <Space size={4}>
                <Badge status="processing" />
                <Text type="secondary" style={{ fontSize: 12 }}>
                  {showRaw ? t('traffic.showRaw') : t('rule.trafficTotal')}
                </Text>
              </Space>
            }
          >
            <TrafficChart points={trendPoints} loading={loading} height={272} showRaw={showRaw} />
          </Card>
        </Col>

        {/* 待处理事项 */}
        <Col xs={24} xl={8}>
          <Card
            size="small"
            title={
              <Space size={8}>
                <WarningOutlined
                  style={{
                    color: pendingCount > 0 ? 'var(--or-warning)' : 'var(--or-success)',
                  }}
                />
                <span>{t('dashboard.pending')}</span>
                {pendingCount > 0 && <Tag color="warning">{pendingCount}</Tag>}
              </Space>
            }
            style={{ height: '100%' }}
          >
            {!data ? (
              <Skeleton active paragraph={{ rows: 5 }} />
            ) : pendingCount === 0 ? (
              <div className="or-empty">
                <Empty
                  image={Empty.PRESENTED_IMAGE_SIMPLE}
                  description={t('dashboard.allClear')}
                />
              </div>
            ) : (
              <Space direction="vertical" size={12} style={{ width: '100%' }}>
                <PendingBlock
                  icon={<SyncOutlined style={{ color: 'var(--or-error)' }} />}
                  title={t('dashboard.syncFailedRules')}
                  items={syncFailed.slice(0, 3).map((r) => r.name)}
                  count={syncFailed.length}
                  onViewAll={() => navigate('/forward-rules?sync_status=failed')}
                  t={t}
                />
                <PendingBlock
                  icon={<DisconnectOutlined style={{ color: 'var(--or-text-disabled)' }} />}
                  title={t('dashboard.offlineNodes')}
                  items={offlineNodes.slice(0, 3).map((n) => n.name)}
                  count={offlineNodes.length}
                  onViewAll={() => navigate('/nodes?online=false')}
                  t={t}
                />
                <PendingBlock
                  icon={<WarningOutlined style={{ color: 'var(--or-warning)' }} />}
                  title={t('dashboard.activeAlerts')}
                  items={activeAlerts.slice(0, 3).map((a) => a.title)}
                  count={activeAlerts.length}
                  onViewAll={() => navigate('/alerts')}
                  t={t}
                />
              </Space>
            )}
          </Card>
        </Col>
      </Row>

      {/* 离线节点清单（首页只给出摘要，详情去节点页）*/}
      <Card
        size="small"
        style={{ marginTop: 12 }}
        title={
          <Space size={8}>
            <CloudServerOutlined />
            <span>{t('dashboard.nodeStatus')}</span>
            {overview && (
              <Text type="secondary" style={{ fontSize: 12, fontWeight: 400 }}>
                {t('monitor.onlineCount', { n: overview.online_nodes })}
              </Text>
            )}
          </Space>
        }
        extra={
          <Button type="link" size="small" onClick={() => navigate('/nodes')}>
            {t('dashboard.viewAll')} <ArrowRightOutlined />
          </Button>
        }
      >
        {!data ? (
          <Skeleton active paragraph={{ rows: 3 }} />
        ) : overview && overview.total_nodes === 0 ? (
          <div className="or-empty">
            <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={t('node.empty')}>
              <Button type="primary" icon={<PlusOutlined />} onClick={() => navigate('/nodes?action=create')}>
                {t('node.add')}
              </Button>
            </Empty>
          </div>
        ) : offlineNodes.length === 0 ? (
          <div className="or-empty">
            <Empty
              image={Empty.PRESENTED_IMAGE_SIMPLE}
              description={t('dashboard.allClear')}
            />
          </div>
        ) : (
          // 后端只在这里返回离线节点摘要（含离线秒数与最近错误），
          // 因此首页用它渲染状态格；完整指标（CPU/内存/速率）在节点详情页与监控页。
          <div className="or-node-grid">
            {offlineNodes.map((n) => (
              <div
                key={n.id}
                className="or-node-card"
                onClick={() => navigate(`/nodes/${n.id}`)}
                role="button"
                tabIndex={0}
                onKeyDown={(e) => {
                  if (e.key === 'Enter') navigate(`/nodes/${n.id}`)
                }}
              >
                <div className="or-node-card-title">
                  <Space size={6}>
                    <span className="or-dot or-dot-offline" />
                    <span>{n.name}</span>
                  </Space>
                  <Tag color="default">{t('node.offline')}</Tag>
                </div>
                <div className="or-node-card-metrics">
                  <div>
                    {t('node.role')}: <b>{t(`node.role.${n.role}`)}</b>
                  </div>
                  <div>
                    离线: <b>{formatOffline(n.offline_seconds)}</b>
                  </div>
                </div>
                {n.last_error && (
                  <div style={{ marginTop: 6 }}>
                    <Text type="danger" style={{ fontSize: 12 }} ellipsis={{ tooltip: n.last_error }}>
                      {n.last_error}
                    </Text>
                  </div>
                )}
              </div>
            ))}
          </div>
        )}
      </Card>
    </div>
  )
}

/** 待处理事项里的一个小分区。 */
function PendingBlock({
  icon,
  title,
  items,
  count,
  onViewAll,
  t,
}: {
  icon: React.ReactNode
  title: string
  items: string[]
  count: number
  onViewAll: () => void
  t: (key: string, vars?: Record<string, string | number>) => string
}) {
  return (
    <div>
      <div
        style={{
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'space-between',
          marginBottom: 4,
        }}
      >
        <Space size={6}>
          {icon}
          <Text strong style={{ fontSize: 13 }}>
            {title}
          </Text>
          <Tag color={count > 0 ? 'error' : 'default'} style={{ marginInlineEnd: 0 }}>
            {count}
          </Tag>
        </Space>
        {count > 0 && (
          <Button type="link" size="small" style={{ padding: 0 }} onClick={onViewAll}>
            {t('dashboard.viewAll')}
          </Button>
        )}
      </div>
      {items.length > 0 ? (
        <List
          size="small"
          split={false}
          dataSource={items}
          renderItem={(item) => (
            <List.Item style={{ padding: '2px 0' }}>
              <Text type="secondary" style={{ fontSize: 12 }} ellipsis={{ tooltip: item }}>
                • {item}
              </Text>
            </List.Item>
          )}
        />
      ) : (
        <Text type="secondary" style={{ fontSize: 12 }}>
          —
        </Text>
      )}
    </div>
  )
}

/** 把离线秒数转成「多久没心跳」的可读文本。 */
function formatOffline(seconds: number): string {
  if (!seconds || seconds <= 0) return '—'
  if (seconds < 60) return `${seconds} 秒`
  if (seconds < 3600) return `${Math.floor(seconds / 60)} 分钟`
  if (seconds < 86400) return `${Math.floor(seconds / 3600)} 小时`
  return `${Math.floor(seconds / 86400)} 天`
}

/**
 * 保证拿到一个数组。
 *
 * 接口字段缺失或改名时，`data?.xxx` 会得到 undefined，
 * 直接调用 .slice/.length/.map 会抛异常并让整个页面白屏。
 * 渲染路径上的所有列表都过这一层，界面最多是"这一块暂时没内容"，
 * 而不是"整页打不开"。
 */
function asArray<T>(v: T[] | undefined | null): T[] {
  return Array.isArray(v) ? v : []
}
