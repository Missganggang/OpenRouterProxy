/**
 * 配置快照页面（规格书 6.13、9.2「快照」）。
 *
 * 能力：
 *  - 列表：名称、触发原因、规则/设备组/节点数量、校验和、创建时间
 *  - 手动生成快照（可自定义名称）
 *  - 差异对比：左右分栏，新增 / 删除 / 修改三类差异分别着色
 *  - 一键回滚：强确认（输入快照名或 YES）——回滚前后端会自动再留一份快照
 *  - 删除、导出 JSON
 *
 * 规格书 6.13 明确要求「回滚前 MUST 再生成一份回滚前快照，保证可逆」，
 * 因此回滚弹窗里必须把这一点讲清楚，用户才敢点。
 */
import { useCallback, useEffect, useMemo, useState } from 'react'
import {
  Alert,
  App,
  Button,
  Empty,
  Input,
  Modal,
  Space,
  Table,
  Tag,
  Tooltip,
  Typography,
} from 'antd'
import type { ColumnsType } from 'antd/es/table'
import {
  CloudUploadOutlined,
  DeleteOutlined,
  DownloadOutlined,
  DiffOutlined,
  PlusOutlined,
  ReloadOutlined,
  RollbackOutlined,
} from '@ant-design/icons'

import { showApiError, systemApi } from '../../api'
import type { ConfigSnapshot } from '../../api/types'
import { useI18n } from '../../locales'

const { Text } = Typography

/** 快照差异结构（与 systemApi.snapshotDiff 的返回一致）。 */
type SnapshotDiff = Awaited<ReturnType<typeof systemApi.snapshotDiff>>

/** 快照触发原因 → i18n 键。 */
const REASON_KEY: Record<string, string> = {
  manual: 'snapshot.reason.manual',
  auto_daily: 'snapshot.reason.auto_daily',
  auto_before_sync: 'snapshot.reason.auto_before_sync',
  before_migrate: 'snapshot.reason.before_migrate',
}

/** 触发原因标签配色：手动为品牌蓝，自动留档用中性色，迁移前用警示色。 */
const REASON_COLOR: Record<string, string> = {
  manual: 'blue',
  auto_daily: 'default',
  auto_before_sync: 'default',
  before_migrate: 'orange',
}

export default function SnapshotsPage() {
  const { t } = useI18n()
  const { message } = App.useApp()

  const [items, setItems] = useState<ConfigSnapshot[]>([])
  const [loading, setLoading] = useState(false)

  // 生成快照弹窗
  const [createOpen, setCreateOpen] = useState(false)
  const [createName, setCreateName] = useState('')
  const [creating, setCreating] = useState(false)

  // 差异对比弹窗
  const [diffTarget, setDiffTarget] = useState<ConfigSnapshot | null>(null)
  const [diff, setDiff] = useState<SnapshotDiff | null>(null)
  const [diffLoading, setDiffLoading] = useState(false)

  // 回滚弹窗
  const [rollbackTarget, setRollbackTarget] = useState<ConfigSnapshot | null>(null)
  const [rollbackText, setRollbackText] = useState('')
  const [rolling, setRolling] = useState(false)

  /** 拉取快照列表。 */
  const load = useCallback(async () => {
    setLoading(true)
    try {
      const res = await systemApi.snapshots({ limit: 200 })
      setItems(res.items)
    } catch (err) {
      showApiError(err, t('snapshot.loadFailed'))
    } finally {
      setLoading(false)
    }
  }, [t])

  useEffect(() => { void load() }, [load])

  /** 生成快照。 */
  const create = async () => {
    setCreating(true)
    try {
      await systemApi.createSnapshot(createName.trim() || undefined)
      message.success(t('snapshot.createSuccess'))
      setCreateOpen(false)
      setCreateName('')
      await load()
    } catch (err) {
      showApiError(err, t('snapshot.loadFailed'))
    } finally {
      setCreating(false)
    }
  }

  /** 打开差异对比：先开弹窗再加载，避免用户以为点击没反应。 */
  const openDiff = async (row: ConfigSnapshot) => {
    setDiffTarget(row)
    setDiff(null)
    setDiffLoading(true)
    try {
      const result = await systemApi.snapshotDiff(row.id)
      setDiff(result)
    } catch (err) {
      showApiError(err, t('snapshot.diffFailed'))
      setDiffTarget(null)
    } finally {
      setDiffLoading(false)
    }
  }

  /** 执行回滚（强确认已在弹窗里完成）。 */
  const confirmRollback = async () => {
    const row = rollbackTarget
    if (!row) return
    const answer = rollbackText.trim()
    // 强确认：必须输入快照名或全大写 YES，避免误触。
    if (answer !== row.name && answer !== 'YES') {
      message.warning(t('snapshot.mismatch'))
      return
    }
    setRolling(true)
    try {
      await systemApi.rollbackSnapshot(row.id)
      message.success(t('snapshot.rollbackDone', { name: row.name }))
      setRollbackTarget(null)
      setRollbackText('')
      await load()
    } catch (err) {
      showApiError(err, t('snapshot.rollbackFailed'))
    } finally {
      setRolling(false)
    }
  }

  /** 删除快照（Modal.confirm 写明资源与影响）。 */
  const remove = (row: ConfigSnapshot) => {
    Modal.confirm({
      title: t('snapshot.deleteConfirm', { name: row.name }),
      okText: t('common.delete'),
      okButtonProps: { danger: true },
      cancelText: t('common.cancel'),
      content: (
        <Space direction="vertical" size={6}>
          <Text>{t('snapshot.deleteImpact')}</Text>
          <Text type="secondary" style={{ fontSize: 12 }}>
            {formatTime(row.created_at)} · {row.rule_count} / {row.group_count} / {row.node_count}
          </Text>
        </Space>
      ),
      onOk: async () => {
        try {
          await systemApi.deleteSnapshot(row.id)
          message.success(t('snapshot.deleteSuccess'))
          await load()
        } catch (err) {
          showApiError(err, t('snapshot.deleteFailed'))
          throw err
        }
      },
    })
  }

  /**
   * 导出快照 JSON。
   *
   * 后端没有单独的导出接口，这里导出「快照详情 + 与当前配置的差异」，
   * 信息量足够用于线下比对与存档。
   */
  const exportJson = async (row: ConfigSnapshot) => {
    try {
      const [detail, difference] = await Promise.all([
        systemApi.getSnapshot(row.id),
        systemApi.snapshotDiff(row.id).catch(() => null),
      ])
      const payload = {
        exported_at: new Date().toISOString(),
        snapshot: detail,
        diff_against_current: difference,
      }
      const blob = new Blob([JSON.stringify(payload, null, 2)], {
        type: 'application/json;charset=utf-8',
      })
      const url = URL.createObjectURL(blob)
      const link = document.createElement('a')
      link.href = url
      link.download = `snapshot-${row.id}-${safeFileName(row.name)}.json`
      document.body.appendChild(link)
      link.click()
      document.body.removeChild(link)
      URL.revokeObjectURL(url)
      message.success(t('snapshot.exportDone'))
    } catch (err) {
      showApiError(err, t('snapshot.exportFailed'))
    }
  }

  /* ───────────────────── 表格列 ───────────────────── */

  const columns: ColumnsType<ConfigSnapshot> = useMemo(
    () => [
      {
        title: t('common.name'),
        dataIndex: 'name',
        key: 'name',
        width: 240,
        render: (v: string, row) => (
          <Space direction="vertical" size={2}>
            <Text strong>{v || `#${row.id}`}</Text>
            <Text type="secondary" style={{ fontSize: 12 }}>
              #{row.id}
            </Text>
          </Space>
        ),
      },
      {
        title: t('snapshot.reason'),
        dataIndex: 'reason',
        key: 'reason',
        width: 150,
        render: (v: string) => (
          <Tag color={REASON_COLOR[v] ?? 'default'}>
            {v ? t(REASON_KEY[v] ?? 'snapshot.reason.other') : t('snapshot.reason.other')}
          </Tag>
        ),
      },
      {
        title: t('snapshot.ruleCount'),
        dataIndex: 'rule_count',
        key: 'rule_count',
        width: 100,
        align: 'right',
        render: (v: number) => <Text className="or-mono">{v}</Text>,
      },
      {
        title: t('snapshot.groupCount'),
        dataIndex: 'group_count',
        key: 'group_count',
        width: 110,
        align: 'right',
        render: (v: number) => <Text className="or-mono">{v}</Text>,
      },
      {
        title: t('snapshot.nodeCount'),
        dataIndex: 'node_count',
        key: 'node_count',
        width: 100,
        align: 'right',
        render: (v: number) => <Text className="or-mono">{v}</Text>,
      },
      {
        title: t('snapshot.checksum'),
        dataIndex: 'checksum',
        key: 'checksum',
        width: 180,
        render: (v: string) => {
          if (!v) return <Text type="secondary">-</Text>
          return (
            <Tooltip title={v}>
              <Text className="or-mono" style={{ fontSize: 12 }} copyable={{ text: v, tooltips: [t('common.copy'), t('common.copied')] }}>
                {truncate(v, 12)}
              </Text>
            </Tooltip>
          )
        },
      },
      {
        title: t('common.createdAt'),
        dataIndex: 'created_at',
        key: 'created_at',
        width: 175,
        render: (v: string) => (
          <Text className="or-mono" style={{ fontSize: 12 }}>
            {formatTime(v)}
          </Text>
        ),
      },
      {
        title: t('common.actions'),
        key: 'actions',
        width: 230,
        fixed: 'right',
        render: (_, row) => (
          <div className="or-actions">
            <Tooltip title={t('snapshot.diff')}>
              <Button size="small" type="text" icon={<DiffOutlined />} onClick={() => void openDiff(row)} />
            </Tooltip>
            <Tooltip title={t('snapshot.rollback')}>
              <Button
                size="small"
                type="text"
                danger
                icon={<RollbackOutlined />}
                onClick={() => {
                  setRollbackText('')
                  setRollbackTarget(row)
                }}
              />
            </Tooltip>
            <Tooltip title={t('snapshot.exportJson')}>
              <Button size="small" type="text" icon={<DownloadOutlined />} onClick={() => void exportJson(row)} />
            </Tooltip>
            <Tooltip title={t('common.delete')}>
              <Button size="small" type="text" danger icon={<DeleteOutlined />} onClick={() => remove(row)} />
            </Tooltip>
          </div>
        ),
      },
    ],
    // t 变化时重建列，保证语言切换后表头立即更新。
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [t],
  )

  return (
    <div>
      <div className="or-page-header">
        <div>
          <h2 className="or-page-title">{t('snapshot.title')}</h2>
          <p className="or-page-desc">{t('snapshot.pageDesc')}</p>
        </div>
        <Space>
          <Button icon={<ReloadOutlined />} onClick={() => void load()}>
            {t('common.refresh')}
          </Button>
          <Button
            type="primary"
            icon={<PlusOutlined />}
            onClick={() => {
              setCreateName('')
              setCreateOpen(true)
            }}
          >
            {t('snapshot.create')}
          </Button>
        </Space>
      </div>

      <Table<ConfigSnapshot>
        rowKey="id"
        size="small"
        loading={loading}
        columns={columns}
        dataSource={items}
        scroll={{ x: 1400 }}
        pagination={{ pageSize: 20, showTotal: (n) => t('common.total', { n }) }}
        locale={{
          emptyText: (
            <Empty
              className="or-empty"
              image={<CloudUploadOutlined style={{ fontSize: 40, color: 'var(--or-text-disabled)' }} />}
              description={
                <Space direction="vertical" size={4}>
                  <Text>{t('snapshot.empty')}</Text>
                  <Text type="secondary" style={{ fontSize: 12 }}>
                    {t('snapshot.emptyHint')}
                  </Text>
                </Space>
              }
            >
              <Button
                type="primary"
                icon={<PlusOutlined />}
                onClick={() => {
                  setCreateName('')
                  setCreateOpen(true)
                }}
              >
                {t('snapshot.create')}
              </Button>
            </Empty>
          ),
        }}
      />

      {/* ── 生成快照 ─────────────────────────────────── */}
      <Modal
        open={createOpen}
        title={t('snapshot.create')}
        okText={t('common.confirm')}
        cancelText={t('common.cancel')}
        confirmLoading={creating}
        onOk={() => void create()}
        onCancel={() => setCreateOpen(false)}
        destroyOnClose
      >
        <Space direction="vertical" size={8} style={{ width: '100%' }}>
          <Text type="secondary" style={{ fontSize: 12 }}>
            {t('snapshot.createHint')}
          </Text>
          <Input
            value={createName}
            maxLength={128}
            placeholder={t('snapshot.createPlaceholder')}
            onChange={(e) => setCreateName(e.target.value)}
            onPressEnter={() => void create()}
          />
        </Space>
      </Modal>

      {/* ── 差异对比（左右分栏）───────────────────────── */}
      <Modal
        open={Boolean(diffTarget)}
        width={960}
        title={
          diffTarget
            ? t('snapshot.diffTitle', { name: diffTarget.name || `#${diffTarget.id}` })
            : t('snapshot.diff')
        }
        footer={<Button onClick={() => setDiffTarget(null)}>{t('common.ok')}</Button>}
        onCancel={() => setDiffTarget(null)}
        destroyOnClose
      >
        {diffLoading ? (
          <Text type="secondary">{t('snapshot.diffLoading')}</Text>
        ) : !diff ? (
          <Text type="secondary">{t('snapshot.diffFailed')}</Text>
        ) : (
          <DiffView diff={diff} t={t} />
        )}
      </Modal>

      {/* ── 一键回滚（强确认）────────────────────────── */}
      <Modal
        open={Boolean(rollbackTarget)}
        width={620}
        title={
          rollbackTarget
            ? t('snapshot.rollbackTitle', { name: rollbackTarget.name || `#${rollbackTarget.id}` })
            : t('snapshot.rollback')
        }
        okText={t('snapshot.rollback')}
        okButtonProps={{ danger: true }}
        cancelText={t('common.cancel')}
        confirmLoading={rolling}
        onOk={() => void confirmRollback()}
        onCancel={() => {
          setRollbackTarget(null)
          setRollbackText('')
        }}
        destroyOnClose
      >
        <Space direction="vertical" size={10} style={{ width: '100%' }}>
          <Alert
            type="warning"
            showIcon
            message={t('snapshot.rollbackConfirm', {
              name: rollbackTarget?.name ?? '',
            })}
            description={
              <Space direction="vertical" size={4}>
                <Text style={{ fontSize: 12 }}>{t('snapshot.rollbackImpact')}</Text>
                <Text style={{ fontSize: 12 }}>{t('snapshot.rollbackReversible')}</Text>
              </Space>
            }
          />
          <div>
            <Text style={{ fontSize: 13 }}>{t('snapshot.rollbackHint')}</Text>
            <Input
              style={{ marginTop: 6 }}
              value={rollbackText}
              placeholder={rollbackTarget?.name ?? 'YES'}
              onChange={(e) => setRollbackText(e.target.value)}
              onPressEnter={() => void confirmRollback()}
            />
          </div>
        </Space>
      </Modal>
    </div>
  )
}

/* ═══════════════════════ 差异视图 ═══════════════════════ */

interface DiffViewProps {
  diff: SnapshotDiff
  t: (key: string, vars?: Record<string, string | number>) => string
}

/**
 * 左右分栏的差异对比。
 *
 * 左栏放「快照侧」（被删除的与修改前的值），右栏放「当前侧」
 * （新增的与修改后的值），与规格书 9.2 要求的「左右分栏 diff」一致。
 */
function DiffView({ diff, t }: DiffViewProps) {
  const added = diff.added ?? []
  const removed = diff.removed ?? []
  const changed = diff.changed ?? []
  const empty = !added.length && !removed.length && !changed.length

  if (empty) {
    return <Alert type="success" showIcon message={t('snapshot.diffNone')} />
  }

  return (
    <Space direction="vertical" size={12} style={{ width: '100%' }}>
      <Text type="secondary" style={{ fontSize: 12 }}>
        {t('snapshot.diffSummary', {
          added: added.length,
          removed: removed.length,
          changed: changed.length,
        })}
      </Text>

      <div style={{ display: 'flex', gap: 12, alignItems: 'flex-start' }}>
        {/* 左：快照侧 */}
        <div style={{ flex: 1, minWidth: 0 }}>
          <Text strong style={{ fontSize: 13 }}>
            {t('snapshot.diffRemoved')}
          </Text>
          <div className="or-diff" style={{ maxHeight: 400, marginTop: 6 }}>
            {removed.length === 0 && (
              <div className="or-diff-line">
                <Text type="secondary" style={{ fontSize: 12 }}>
                  {t('common.none')}
                </Text>
              </div>
            )}
            {removed.map((line) => (
              <div key={`rm-${line}`} className="or-diff-line or-diff-del">
                {`- ${line}`}
              </div>
            ))}
            {changed.map((c) => (
              <div key={`ch-before-${c.path}`} className="or-diff-line or-diff-del">
                {`~ ${c.path}: ${stringify(c.from)}`}
              </div>
            ))}
          </div>
        </div>

        {/* 右：当前侧 */}
        <div style={{ flex: 1, minWidth: 0 }}>
          <Text strong style={{ fontSize: 13 }}>
            {t('snapshot.diffAdded')}
          </Text>
          <div className="or-diff" style={{ maxHeight: 400, marginTop: 6 }}>
            {added.length === 0 && (
              <div className="or-diff-line">
                <Text type="secondary" style={{ fontSize: 12 }}>
                  {t('common.none')}
                </Text>
              </div>
            )}
            {added.map((line) => (
              <div key={`add-${line}`} className="or-diff-line or-diff-add">
                {`+ ${line}`}
              </div>
            ))}
            {changed.map((c) => (
              <div key={`ch-after-${c.path}`} className="or-diff-line or-diff-add">
                {`~ ${c.path}: ${stringify(c.to)}`}
              </div>
            ))}
          </div>
        </div>
      </div>

      {changed.length > 0 && (
        <div>
          <Text strong style={{ fontSize: 13 }}>
            {t('snapshot.diffChanged')}
          </Text>
          <Table<{ path: string; from: unknown; to: unknown }>
            style={{ marginTop: 6 }}
            size="small"
            rowKey="path"
            pagination={false}
            scroll={{ y: 260 }}
            dataSource={changed}
            columns={[
              {
                title: t('common.name'),
                dataIndex: 'path',
                key: 'path',
                width: 260,
                render: (v: string) => (
                  <Text className="or-mono" style={{ fontSize: 12 }}>
                    {v}
                  </Text>
                ),
              },
              {
                title: 'before',
                dataIndex: 'from',
                key: 'from',
                render: (v: unknown) => (
                  <Text className="or-mono" style={{ fontSize: 12, color: 'var(--or-error)' }}>
                    {stringify(v)}
                  </Text>
                ),
              },
              {
                title: 'after',
                dataIndex: 'to',
                key: 'to',
                render: (v: unknown) => (
                  <Text className="or-mono" style={{ fontSize: 12, color: 'var(--or-success)' }}>
                    {stringify(v)}
                  </Text>
                ),
              },
            ]}
          />
        </div>
      )}
    </Space>
  )
}

/* ═══════════════════════ 工具 ═══════════════════════ */

/** 任意值 → 单行可读文本。 */
function stringify(value: unknown): string {
  if (value === null || value === undefined) return '-'
  if (typeof value === 'string') return value
  if (typeof value === 'number' || typeof value === 'boolean') return String(value)
  try {
    return JSON.stringify(value)
  } catch {
    return String(value)
  }
}

/** 截断长文本，配合 Tooltip 使用。 */
function truncate(text: string, keep: number): string {
  return text.length <= keep ? text : `${text.slice(0, keep)}…`
}

/** 文件名安全化：去掉路径分隔符等非法字符。 */
function safeFileName(name: string): string {
  return (name || 'snapshot').replace(/[\\/:*?"<>|\s]+/g, '_').slice(0, 40)
}

/** RFC3339 → 本地时间。 */
function formatTime(iso: string): string {
  if (!iso) return '-'
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString()
}
