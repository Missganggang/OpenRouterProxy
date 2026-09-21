import { migrationReport } from '../../api/reportAdapters'
/**
 * 迁移与备份页面（规格书 7.2、7.4、8.15、9.2「迁移」）。
 *
 * 页面按规格书第 7 章的手工迁移流程组织：
 *  1. 选择源（Nyanpass / OpenRoute）+ DSN + 重名策略 → 执行预检（只读）
 *  2. 预检报告分区块着色展示：将迁移的数据 / 冲突与风险 / 丢弃字段 /
 *     填充字段 / 示例转换 / 建议操作 / 原始文本报告
 *  3. 正式迁移：YES 强确认 → 按阶段轮询进度（阶段 1~7）→ 最终报告
 *  4. 迁移批次历史 + 按批次回滚（强确认）
 *  5. 备份与恢复：生成 / 下载 / 恢复（强确认，且提示恢复后必须重启进程）
 */
import { useCallback, useEffect, useMemo, useState } from 'react'
import {
  Alert,
  App,
  Button,
  Card,
  Col,
  Collapse,
  Descriptions,
  Empty,
  Input,
  Modal,
  Progress,
  Row,
  Select,
  Space,
  Steps,
  Switch,
  Table,
  Tag,
  Tooltip,
  Typography,
} from 'antd'
import type { ColumnsType } from 'antd/es/table'
import {
  CloudDownloadOutlined,
  CopyOutlined,
  DatabaseOutlined,
  ExportOutlined,
  FileZipOutlined,
  PlayCircleOutlined,
  ReloadOutlined,
  SafetyOutlined,
  WarningOutlined,
} from '@ant-design/icons'

import { migrationApi, showApiError } from '../../api'
import type { Backup, MigrateBatch } from '../../api/types'
import type { PrecheckInput, PrecheckReport } from '../../api/modules/migration'
import { useI18n } from '../../locales'
// 报告文本的复制由下方 RawReport 自行实现（等宽块 + 复制按钮），
// 不使用 CopyableCommand：那个组件是给「命令 / 链接」用的单行输入框形态，
// 套在整份多行报告上会挤压排版。
import { usePolling } from '../../hooks/usePolling'

const { Text, Paragraph } = Typography

/** 迁移阶段总数（规格书 7.2 的 7 个阶段）。 */
const STAGE_TOTAL = 7

/** 阶段序号 → i18n 键。 */
const STAGE_KEYS = [
  'migration.stage1',
  'migration.stage2',
  'migration.stage3',
  'migration.stage4',
  'migration.stage5',
  'migration.stage6',
  'migration.stage7',
] as const

/** 批次状态 → 标签配色。 */
const STATUS_COLOR: Record<MigrateBatch['status'], string> = {
  pending: 'default',
  running: 'processing',
  finished: 'green',
  failed: 'red',
  rolled_back: 'orange',
}

type Translate = (key: string, vars?: Record<string, string | number>) => string

export default function MigrationPage() {
  const { t } = useI18n()
  const { message } = App.useApp()

  /* ── 预检表单 ─────────────────────────────────────── */
  const [source, setSource] = useState<PrecheckInput['source']>('nyanpass')
  const [dsn, setDsn] = useState('')
  const [renamePolicy, setRenamePolicy] = useState<NonNullable<PrecheckInput['rename_policy']>>('fail')
  const [prechecking, setPrechecking] = useState(false)
  const [report, setReport] = useState<PrecheckReport | null>(null)

  /* ── 正式迁移 ─────────────────────────────────────── */
  const [runOpen, setRunOpen] = useState(false)
  const [runText, setRunText] = useState('')
  const [running, setRunning] = useState(false)
  /** 正在跟踪进度的批次（迁移进行中时非空）。 */
  const [activeBatchId, setActiveBatchId] = useState<number | null>(null)
  const [activeBatch, setActiveBatch] = useState<MigrateBatch | null>(null)

  /* ── 批次历史 ─────────────────────────────────────── */
  const [batches, setBatches] = useState<MigrateBatch[]>([])
  const [batchesLoading, setBatchesLoading] = useState(false)

  /* ── 回滚 / 批次报告 ──────────────────────────────── */
  const [rollbackTarget, setRollbackTarget] = useState<MigrateBatch | null>(null)
  const [rollbackText, setRollbackText] = useState('')
  const [rolling, setRolling] = useState(false)
  const [reportTarget, setReportTarget] = useState<MigrateBatch | null>(null)

  /* ── 备份 ─────────────────────────────────────────── */
  const [withSecret, setWithSecret] = useState(false)
  const [backupBusy, setBackupBusy] = useState(false)
  const [restoreTarget, setRestoreTarget] = useState<Backup | null>(null)
  const [restoreText, setRestoreText] = useState('')
  const [restoring, setRestoring] = useState(false)

  /** 拉取迁移批次列表。 */
  const loadBatches = useCallback(async () => {
    setBatchesLoading(true)
    try {
      const res = await migrationApi.list({ page: 1, page_size: 50, sort: 'created_at', order: 'desc' })
      setBatches(res.items)
    } catch (err) {
      showApiError(err, t('migration.loadHistoryFailed'))
    } finally {
      setBatchesLoading(false)
    }
  }, [t])

  useEffect(() => {
    void loadBatches()
  }, [loadBatches])

  /**
   * 迁移进度轮询（规格书 7.2：逐阶段进度条）。
   *
   * 每 1.5 秒拉一次 progress 接口；同时每三拍（约 4.5 秒）查一次批次本体，
   * 因为进度接口不返回批次状态，而终止轮询必须依赖 status。
   * 不在迁移中时 interval 为 0，usePolling 不会建定时器。
   */
  const { data: progress } = usePolling(
    useCallback(async () => {
      if (activeBatchId === null) return undefined
      return migrationApi.progress(activeBatchId)
    }, [activeBatchId]),
    { interval: activeBatchId === null ? 0 : 1500 },
  )

  useEffect(() => {
    if (activeBatchId === null) return
    let cancelled = false
    let tick = 0

    const checkBatch = async () => {
      try {
        const batch = await migrationApi.getOne(activeBatchId)
        if (cancelled) return
        setActiveBatch(batch)
        if (batch.status !== 'running' && batch.status !== 'pending') {
          setActiveBatchId(null)
          await loadBatches()
          if (batch.status === 'finished') message.success(t('migration.progressDone'))
          if (batch.status === 'failed') message.error(t('migration.progressFailed'))
        }
      } catch {
        // 单次查询失败不打断轮询：网络抖动后仍能继续跟踪。
      }
    }

    void checkBatch()
    const timer = window.setInterval(() => {
      if (document.hidden) return
      tick += 1
      if (tick % 3 === 0) void checkBatch()
    }, 1500)

    return () => {
      cancelled = true
      window.clearInterval(timer)
    }
  }, [activeBatchId, loadBatches, message, t])

  /** 执行预检（只读，不写任何数据）。 */
  const doPrecheck = async () => {
    if (!dsn.trim()) {
      message.warning(t('migration.dsnRequired'))
      return
    }
    setPrechecking(true)
    try {
      const result = await migrationApi.precheck({
        source,
        dsn: dsn.trim(),
        rename_policy: renamePolicy,
      })
      setReport(result)
      message.success(t('migration.precheckDone'))
    } catch (err) {
      showApiError(err, t('migration.precheckFailed'))
    } finally {
      setPrechecking(false)
    }
  }

  /** 打开正式迁移确认弹窗。 */
  const openRun = () => {
    if (!dsn.trim()) {
      message.warning(t('migration.dsnRequired'))
      return
    }
    setRunText('')
    setRunOpen(true)
  }

  /** 执行迁移（dry-run 或正式），强确认已在弹窗里完成。 */
  const doRun = async (dryRun: boolean) => {
    const answer = runText.trim()
    if (!dryRun && answer !== 'YES') {
      message.warning(t('snapshot.mismatch'))
      return
    }
    setRunning(true)
    try {
      const result = await migrationApi.run({
        source,
        dsn: dsn.trim(),
        rename_policy: renamePolicy,
        dry_run: dryRun,
      })
      setRunOpen(false)
      setRunText('')
      setReport(migrationReport(result.report))
      if (result.batch_id) {
        setActiveBatch(await migrationApi.getOne(result.batch_id))
        setActiveBatchId(result.batch_id)
      }
      if (result.passed) message.success(dryRun ? t('migration.precheckDone') : '迁移已完成')
      else message.warning('迁移校验未通过，请查看报告')
      await loadBatches()
    } catch (err) {
      showApiError(err, t('migration.runFailed'))
    } finally {
      setRunning(false)
    }
  }

  /** 回滚一个迁移批次。 */
  const doRollback = async () => {
    const row = rollbackTarget
    if (!row) return
    if (rollbackText.trim() !== 'YES') {
      message.warning(t('snapshot.mismatch'))
      return
    }
    setRolling(true)
    try {
      const res = await migrationApi.rollback(row.id)
      message.success(t('migration.rollbackDone', { n: res.deleted_rows }))
      setRollbackTarget(null)
      setRollbackText('')
      await loadBatches()
    } catch (err) {
      showApiError(err, t('migration.rollbackFailed'))
    } finally {
      setRolling(false)
    }
  }

  /* ── 备份（30 秒轮询保持新鲜）─────────────────────── */
  const {
    data: backupList,
    refresh: refreshBackups,
    loading: backupsLoading,
  } = usePolling(
    useCallback(async () => {
      try {
        const res = await migrationApi.backups({ page: 1, page_size: 100 })
        return res.items
      } catch (err) {
        showApiError(err, t('migration.loadBackupsFailed'))
        return []
      }
    }, [t]),
    { interval: 30000 },
  )

  /** 生成备份。 */
  const createBackup = async () => {
    setBackupBusy(true)
    try {
      await migrationApi.createBackup(withSecret)
      message.success(t('migration.backupDone'))
      await refreshBackups()
    } catch (err) {
      showApiError(err, t('migration.backupFailed'))
    } finally {
      setBackupBusy(false)
    }
  }

  /** 下载备份。 */
  const downloadBackup = async (row: Backup) => {
    try {
      await migrationApi.downloadBackup(row.id, row.name || `backup-${row.id}`)
    } catch (err) {
      showApiError(err, t('migration.downloadFailed'))
    }
  }

  /** 从备份恢复。 */
  const doRestore = async () => {
    const row = restoreTarget
    if (!row) return
    if (restoreText.trim() !== 'YES') {
      message.warning(t('snapshot.mismatch'))
      return
    }
    setRestoring(true)
    try {
      await migrationApi.restoreBackup(row.id)
      message.success(t('migration.restoreDone'))
      setRestoreTarget(null)
      setRestoreText('')
      await refreshBackups()
    } catch (err) {
      showApiError(err, t('migration.restoreFailed'))
    } finally {
      setRestoring(false)
    }
  }

  /* ───────────────────── 表格列 ───────────────────── */

  const batchColumns: ColumnsType<MigrateBatch> = useMemo(
    () => [
      {
        title: t('migration.colBatchId'),
        dataIndex: 'id',
        key: 'id',
        width: 90,
        render: (v: number) => <Text className="or-mono">#{v}</Text>,
      },
      {
        title: t('common.status'),
        dataIndex: 'status',
        key: 'status',
        width: 130,
        render: (v: MigrateBatch['status']) => (
          <Tag color={STATUS_COLOR[v] ?? 'default'}>{t(`migration.status.${v}`)}</Tag>
        ),
      },
      {
        title: t('migration.source'),
        dataIndex: 'source',
        key: 'source',
        width: 110,
        render: (v: string) => <Tag color="blue">{v}</Tag>,
      },
      {
        title: t('migration.colSourceDsn'),
        dataIndex: 'source_dsn',
        key: 'source_dsn',
        width: 260,
        render: (v: string) => (
          <Tooltip title={v}>
            <Text className="or-mono" style={{ fontSize: 12 }}>
              {truncate(v, 42)}
            </Text>
          </Tooltip>
        ),
      },
      {
        title: t('migration.colDryRun'),
        dataIndex: 'dry_run',
        key: 'dry_run',
        width: 110,
        render: (v: boolean) =>
          v ? <Tag color="purple">{t('migration.modeDry')}</Tag> : <Tag>{t('migration.modeReal')}</Tag>,
      },
      {
        title: t('migration.colStage'),
        dataIndex: 'stage',
        key: 'stage',
        width: 170,
        render: (v: number) => (
          <Text style={{ fontSize: 12 }}>{stageLabel(v, t)}</Text>
        ),
      },
      {
        title: t('migration.colRowsProgress'),
        key: 'rows',
        width: 150,
        render: (_, row) => (
          <Text className="or-mono" style={{ fontSize: 12 }}>
            {row.migrated_rows} / {row.total_rows}
          </Text>
        ),
      },
      {
        title: t('migration.colStartedAt'),
        dataIndex: 'started_at',
        key: 'started_at',
        width: 170,
        render: (v: string) => (
          <Text className="or-mono" style={{ fontSize: 12 }}>
            {formatTime(v)}
          </Text>
        ),
      },
      {
        title: t('migration.colFinishedAt'),
        dataIndex: 'finished_at',
        key: 'finished_at',
        width: 170,
        render: (v: string | null) => (
          <Text className="or-mono" style={{ fontSize: 12 }}>
            {v ? formatTime(v) : '-'}
          </Text>
        ),
      },
      {
        title: t('migration.colError'),
        dataIndex: 'error',
        key: 'error',
        width: 200,
        render: (v: string) =>
          v ? (
            <Tooltip title={v}>
              <Text type="danger" style={{ fontSize: 12 }}>
                {truncate(v, 30)}
              </Text>
            </Tooltip>
          ) : (
            <Text type="secondary">-</Text>
          ),
      },
      {
        title: t('common.actions'),
        key: 'actions',
        width: 230,
        fixed: 'right',
        render: (_, row) => {
          const rollbackable = row.status === 'finished' || row.status === 'failed'
          return (
            <div className="or-actions">
              <Tooltip title={t('migration.report')}>
                <Button
                  size="small"
                  type="text"
                  icon={<ExportOutlined />}
                  onClick={() => setReportTarget(row)}
                />
              </Tooltip>
              <Tooltip title={rollbackable ? t('migration.rollback') : t('migration.rollbackNotAllowed')}>
                <Button
                  size="small"
                  type="text"
                  danger
                  disabled={!rollbackable}
                  icon={<CloudDownloadOutlined />}
                  onClick={() => {
                    setRollbackText('')
                    setRollbackTarget(row)
                  }}
                />
              </Tooltip>
            </div>
          )
        },
      },
    ],
    [t],
  )

  const backupColumns: ColumnsType<Backup> = useMemo(
    () => [
      {
        title: t('migration.colBackupName'),
        dataIndex: 'name',
        key: 'name',
        render: (v: string, row) => (
          <Space direction="vertical" size={2}>
            <Text strong className="or-mono" style={{ fontSize: 12 }}>
              {v || row.path}
            </Text>
            <Text type="secondary" style={{ fontSize: 12 }}>
              {row.path}
            </Text>
          </Space>
        ),
      },
      {
        title: t('migration.colSize'),
        dataIndex: 'size',
        key: 'size',
        width: 120,
        render: (v: number) => <Text className="or-mono">{formatBytes(v)}</Text>,
      },
      {
        title: t('migration.colWithSecret'),
        dataIndex: 'with_secret',
        key: 'with_secret',
        width: 110,
        render: (v: boolean) =>
          v ? (
            <Tag color="orange" icon={<WarningOutlined />}>
              {t('common.yes')}
            </Tag>
          ) : (
            <Tag>{t('common.no')}</Tag>
          ),
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
        width: 200,
        fixed: 'right',
        render: (_, row) => (
          <div className="or-actions">
            <Tooltip title={t('migration.download')}>
              <Button
                size="small"
                type="text"
                icon={<CloudDownloadOutlined />}
                onClick={() => void downloadBackup(row)}
              />
            </Tooltip>
            <Tooltip title={t('migration.restore')}>
              <Button
                size="small"
                type="text"
                danger
                icon={<DatabaseOutlined />}
                onClick={() => {
                  setRestoreText('')
                  setRestoreTarget(row)
                }}
              />
            </Tooltip>
          </div>
        ),
      },
    ],
    [t],
  )

  /* ───────────────────── 渲染 ───────────────────── */

  const currentStage = progress?.stage ?? activeBatch?.stage ?? 0

  return (
    <div>
      <div className="or-page-header">
        <div>
          <h2 className="or-page-title">{t('migration.title')}</h2>
          <p className="or-page-desc">{t('migration.pageDesc')}</p>
        </div>
        <Space>
          <Button icon={<ReloadOutlined />} onClick={() => void loadBatches()}>
            {t('common.refresh')}
          </Button>
        </Space>
      </div>

      {/* ── 1. 源库与预检 ─────────────────────────────── */}
      <Card size="small" style={{ marginBottom: 12 }}>
        <Row gutter={16} align="bottom">
          <Col xs={24} md={6}>
            <div style={{ marginBottom: 4 }}>
              <Text style={{ fontSize: 13 }}>{t('migration.source')}</Text>
            </div>
            <Select
              style={{ width: '100%' }}
              value={source}
              onChange={(v) => setSource(v)}
              options={[
                { label: t('migration.source.nyanpass'), value: 'nyanpass' },
                { label: t('migration.source.openroute'), value: 'openroute' },
              ]}
            />
          </Col>
          <Col xs={24} md={10}>
            <div style={{ marginBottom: 4 }}>
              <Text style={{ fontSize: 13 }}>{t('migration.dsn')}</Text>
            </div>
            <Input
              value={dsn}
              className="or-mono"
              placeholder={t('migration.dsnPlaceholder')}
              onChange={(e) => setDsn(e.target.value)}
              onPressEnter={() => void doPrecheck()}
            />
          </Col>
          <Col xs={24} md={4}>
            <div style={{ marginBottom: 4 }}>
              <Text style={{ fontSize: 13 }}>{t('migration.renamePolicy')}</Text>
            </div>
            <Select
              style={{ width: '100%' }}
              value={renamePolicy}
              onChange={(v) => setRenamePolicy(v)}
              options={[
                { label: t('migration.rename.fail'), value: 'fail' },
                { label: t('migration.rename.suffix'), value: 'suffix' },
                { label: t('migration.rename.skip'), value: 'skip' },
              ]}
            />
          </Col>
          <Col xs={24} md={4}>
            <Space>
              <Button type="primary" loading={prechecking} onClick={() => void doPrecheck()}>
                {t('migration.precheck')}
              </Button>
              <Button danger icon={<PlayCircleOutlined />} onClick={openRun}>
                {t('migration.run')}
              </Button>
            </Space>
          </Col>
        </Row>
        <Paragraph type="secondary" style={{ fontSize: 12, margin: '8px 0 0' }}>
          {t('migration.dsnHint')} · {t('migration.renamePolicyHint')}
        </Paragraph>
      </Card>

      {/* ── 2. 预检报告 ───────────────────────────────── */}
      {!report ? (
        <Card size="small" style={{ marginBottom: 12 }}>
          <Empty
            className="or-empty"
            image={<SafetyOutlined style={{ fontSize: 40, color: 'var(--or-text-disabled)' }} />}
            description={
              <Space direction="vertical" size={4}>
                <Text>{t('migration.precheckIdle')}</Text>
                <Text type="secondary" style={{ fontSize: 12 }}>
                  {t('migration.precheckIdleHint')}
                </Text>
              </Space>
            }
          />
        </Card>
      ) : (
        <PrecheckReportView report={report} t={t} />
      )}

      {/* ── 3. 迁移进度（阶段 1~7）────────────────────── */}
      {activeBatchId !== null && (
        <Card size="small" title={t('migration.progressTitle')} style={{ marginBottom: 12 }}>
          <Space direction="vertical" size={12} style={{ width: '100%' }}>
            <Space align="center" size={12} wrap>
              <Tag color="processing">
                #{activeBatchId} {t('migration.status.running')}
              </Tag>
              <Text className="or-mono" style={{ fontSize: 12 }}>
                {t('migration.progressRows', {
                  done: progress?.migrated_rows ?? activeBatch?.migrated_rows ?? 0,
                  total: progress?.total_rows ?? activeBatch?.total_rows ?? 0,
                })}
              </Text>
              {progress?.message && (
                <Text type="secondary" style={{ fontSize: 12 }}>
                  {progress.message}
                </Text>
              )}
            </Space>

            <Steps
              size="small"
              current={Math.max(0, currentStage - 1)}
              status="process"
              items={STAGE_KEYS.map((key) => ({ title: t(key) }))}
            />

            <Progress
              percent={Math.round(progress?.percent ?? 0)}
              status="active"
              strokeColor="var(--or-primary)"
            />
          </Space>
        </Card>
      )}

      {/* ── 4. 迁移批次历史 ───────────────────────────── */}
      <Card
        size="small"
        title={t('migration.history')}
        style={{ marginBottom: 12 }}
        extra={
          <Button size="small" icon={<ReloadOutlined />} onClick={() => void loadBatches()}>
            {t('common.refresh')}
          </Button>
        }
      >
        <Table<MigrateBatch>
          rowKey="id"
          size="small"
          loading={batchesLoading}
          columns={batchColumns}
          dataSource={batches}
          scroll={{ x: 1750 }}
          pagination={{ pageSize: 10, showTotal: (n) => t('common.total', { n })}
          }
          locale={{
            emptyText: (
              <Empty
                className="or-empty"
                description={
                  <Space direction="vertical" size={4}>
                    <Text>{t('migration.historyEmpty')}</Text>
                    <Text type="secondary" style={{ fontSize: 12 }}>
                      {t('migration.historyEmptyHint')}
                    </Text>
                  </Space>
                }
              />
            ),
          }}
        />
      </Card>

      {/* ── 5. 备份与恢复 ─────────────────────────────── */}
      <Card
        size="small"
        title={
          <Space size={6}>
            <FileZipOutlined />
            {t('migration.backups')}
          </Space>
        }
        extra={
          <Space size={12} wrap>
            <Space size={6}>
              <Text style={{ fontSize: 12 }}>{t('migration.withSecret')}</Text>
              <Tooltip title={t('migration.withSecretHint')}>
                <Switch size="small" checked={withSecret} onChange={setWithSecret} />
              </Tooltip>
            </Space>
            <Button
              type="primary"
              size="small"
              loading={backupBusy}
              onClick={() => void createBackup()}
            >
              {t('migration.createBackup')}
            </Button>
          </Space>
        }
      >
        <Table<Backup>
          rowKey="id"
          size="small"
          loading={backupsLoading}
          columns={backupColumns}
          dataSource={backupList ?? []}
          scroll={{ x: 1000 }}
          pagination={{ pageSize: 10, showTotal: (n) => t('common.total', { n }) }}
          locale={{
            emptyText: (
              <Empty
                className="or-empty"
                description={
                  <Space direction="vertical" size={4}>
                    <Text>{t('migration.backupsEmpty')}</Text>
                    <Text type="secondary" style={{ fontSize: 12 }}>
                      {t('migration.backupsEmptyHint')}
                    </Text>
                  </Space>
                }
              >
                <Button type="primary" onClick={() => void createBackup()}>
                  {t('migration.createBackup')}
                </Button>
              </Empty>
            ),
          }}
        />
      </Card>

      {/* ── 正式迁移确认（YES 强确认）──────────────────── */}
      <Modal
        open={runOpen}
        width={620}
        title={t('migration.runConfirmTitle')}
        okText={t('migration.run')}
        okButtonProps={{ danger: true }}
        cancelText={t('common.cancel')}
        confirmLoading={running}
        onOk={() => void doRun(false)}
        onCancel={() => setRunOpen(false)}
        destroyOnClose
      >
        <Space direction="vertical" size={10} style={{ width: '100%' }}>
          <Alert
            type="error"
            showIcon
            message={t('migration.dangerHint')}
            description={
              <Space direction="vertical" size={4}>
                <Text style={{ fontSize: 12 }}>
                  {t('migration.runTitle', {
                    mode: t('migration.runReal'),
                    source: source === 'nyanpass' ? t('migration.source.nyanpass') : t('migration.source.openroute'),
                  })}
                </Text>
                <Text className="or-mono" style={{ fontSize: 12 }}>
                  {dsn}
                </Text>
              </Space>
            }
          />
          <div>
            <Text style={{ fontSize: 13 }}>{t('migration.runTypeYes')}</Text>
            <Input
              style={{ marginTop: 6 }}
              value={runText}
              placeholder="YES"
              onChange={(e) => setRunText(e.target.value)}
              onPressEnter={() => void doRun(false)}
            />
          </div>
          <Button onClick={() => void doRun(true)} loading={running} block>
            {t('migration.dryRun')}
          </Button>
        </Space>
      </Modal>

      {/* ── 批次回滚（YES 强确认）─────────────────────── */}
      <Modal
        open={Boolean(rollbackTarget)}
        width={600}
        title={
          rollbackTarget ? t('migration.rollbackTitle', { id: rollbackTarget.id }) : t('migration.rollback')
        }
        okText={t('migration.rollback')}
        okButtonProps={{ danger: true }}
        cancelText={t('common.cancel')}
        confirmLoading={rolling}
        onOk={() => void doRollback()}
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
            message={t('migration.rollbackConfirm', { id: rollbackTarget?.id ?? 0 })}
            description={
              <Text style={{ fontSize: 12 }}>{t('migration.rollbackImpact')}</Text>
            }
          />
          <div>
            <Text style={{ fontSize: 13 }}>{t('migration.rollbackHint')}</Text>
            <Input
              style={{ marginTop: 6 }}
              value={rollbackText}
              placeholder="YES"
              onChange={(e) => setRollbackText(e.target.value)}
              onPressEnter={() => void doRollback()}
            />
          </div>
        </Space>
      </Modal>

      {/* ── 从备份恢复（YES 强确认 + 重启提示）────────── */}
      <Modal
        open={Boolean(restoreTarget)}
        width={640}
        title={
          restoreTarget
            ? t('migration.restoreTitle', { name: restoreTarget.name || `#${restoreTarget.id}` })
            : t('migration.restore')
        }
        okText={t('migration.restore')}
        okButtonProps={{ danger: true }}
        cancelText={t('common.cancel')}
        confirmLoading={restoring}
        onOk={() => void doRestore()}
        onCancel={() => {
          setRestoreTarget(null)
          setRestoreText('')
        }}
        destroyOnClose
      >
        <Space direction="vertical" size={10} style={{ width: '100%' }}>
          <Alert
            type="error"
            showIcon
            message={t('migration.restoreConfirm', {
              name: restoreTarget?.name || `#${restoreTarget?.id ?? 0}`,
            })}
            description={
              <Space direction="vertical" size={4}>
                <Text style={{ fontSize: 12 }}>{t('migration.restoreImpact')}</Text>
                <Text strong style={{ fontSize: 12 }}>
                  {t('migration.restoreRestart')}
                </Text>
              </Space>
            }
          />
          <div>
            <Text style={{ fontSize: 13 }}>{t('migration.restoreHint')}</Text>
            <Input
              style={{ marginTop: 6 }}
              value={restoreText}
              placeholder="YES"
              onChange={(e) => setRestoreText(e.target.value)}
              onPressEnter={() => void doRestore()}
            />
          </div>
        </Space>
      </Modal>

      {/* ── 批次报告 ─────────────────────────────────── */}
      <Modal
        open={Boolean(reportTarget)}
        width={860}
        title={`${t('migration.batchReport')} #${reportTarget?.id ?? ''}`}
        footer={<Button onClick={() => setReportTarget(null)}>{t('common.ok')}</Button>}
        onCancel={() => setReportTarget(null)}
        destroyOnClose
      >
        <BatchReportView batch={reportTarget} t={t} />
      </Modal>
    </div>
  )
}

/* ═══════════════════════ 预检报告 ═══════════════════════ */

/**
 * 预检报告分区块着色展示（规格书 7.2 的「迁移预检报告」版式）。
 *
 * 区块顺序与规格书一致：将迁移的数据 → 冲突与风险 → 丢弃字段 →
 * 填充字段 → 示例转换 → 建议操作 → 原始文本。
 */
function PrecheckReportView({
  report,
  t,
}: {
  report: PrecheckReport
  t: Translate
}) {
  const { conflicts } = report
  const riskCount =
    (conflicts.name_conflicts?.length ?? 0) +
    (conflicts.port_conflicts?.length ?? 0) +
    (conflicts.missing_refs?.length ?? 0) +
    (conflicts.zero_multiplier_groups?.length ?? 0)

  return (
    <Space direction="vertical" size={12} style={{ width: '100%', marginBottom: 12 }}>
      {/* 阻断性冲突：用最醒目的方式提示 */}
      {report.has_blocking_conflict && (
        <Alert
          type="error"
          showIcon
          message={t('migration.blockingConflict')}
          description={<Text style={{ fontSize: 12 }}>{t('migration.blockingHint')}</Text>}
        />
      )}

      <Alert
        type={riskCount > 0 ? 'warning' : 'success'}
        showIcon
        message={
          riskCount > 0
            ? t('migration.riskFound', { n: riskCount })
            : t('migration.conflictClear')
        }
        description={
          <Descriptions size="small" column={2} style={{ marginTop: 4 }}>
            <Descriptions.Item label={t('migration.summary')}>
              <Text className="or-mono" style={{ fontSize: 12 }}>
                {report.summary?.source || '-'}
              </Text>
            </Descriptions.Item>
            <Descriptions.Item label={t('migration.summaryTable')}>
              <Text className="or-mono" style={{ fontSize: 12 }}>
                {report.summary?.target || '-'}
              </Text>
            </Descriptions.Item>
            <Descriptions.Item label={t('migration.summaryVersion')}>
              <Text className="or-mono" style={{ fontSize: 12 }}>
                {report.summary?.source_version || '-'}
              </Text>
            </Descriptions.Item>
            <Descriptions.Item label={t('migration.summaryMode')}>
              <Text className="or-mono" style={{ fontSize: 12 }}>
                {report.summary?.mode || '-'}
              </Text>
            </Descriptions.Item>
          </Descriptions>
        }
      />

      {/* ① 将迁移的数据 */}
      <Card
        size="small"
        title={t('migration.sectionTables')}
        style={{ borderLeft: '3px solid var(--or-primary)' }}
      >
        <Text type="secondary" style={{ fontSize: 12 }}>
          {t('migration.sectionTablesHint')}
        </Text>
        <Table<PrecheckReport['tables'][number]>
          style={{ marginTop: 8 }}
          size="small"
          rowKey={(row) => `${row.source_table}->${row.target_table}`}
          pagination={false}
          dataSource={report.tables ?? []}
          scroll={{ y: 260 }}
          columns={[
            {
              title: t('migration.colSourceTable'),
              dataIndex: 'source_table',
              key: 'source_table',
              width: 220,
              render: (v: string) => (
                <Text className="or-mono" style={{ fontSize: 12 }}>
                  {v}
                </Text>
              ),
            },
            {
              title: t('migration.colTargetTable'),
              dataIndex: 'target_table',
              key: 'target_table',
              width: 220,
              render: (v: string) => (
                <Text className="or-mono" style={{ fontSize: 12 }}>
                  {v}
                </Text>
              ),
            },
            {
              title: t('migration.colRows'),
              dataIndex: 'rows',
              key: 'rows',
              width: 110,
              align: 'right',
              render: (v: number) => <Text className="or-mono">{v}</Text>,
            },
            {
              title: t('migration.colNote'),
              dataIndex: 'note',
              key: 'note',
              render: (v?: string) => <Text style={{ fontSize: 12 }}>{v || '-'}</Text>,
            },
          ]}
          locale={{
            emptyText: <Empty className="or-empty" description={t('migration.precheckIdle')} />,
          }}
        />
      </Card>

      {/* ② 冲突与风险 */}
      <Card
        size="small"
        title={
          <Space size={6}>
            <WarningOutlined style={{ color: 'var(--or-warning)' }} />
            {t('migration.sectionConflicts')}
          </Space>
        }
        style={{ borderLeft: '3px solid var(--or-warning)' }}
      >
        <Text type="secondary" style={{ fontSize: 12 }}>
          {t('migration.sectionConflictsHint')}
        </Text>

        <div style={{ marginTop: 10 }}>
          <Text strong style={{ fontSize: 13 }}>
            {t('migration.nameConflicts')}
          </Text>
          <Text type="secondary" style={{ fontSize: 12, marginLeft: 8 }}>
            {t('migration.nameConflictCount', { n: conflicts.name_conflicts?.length ?? 0 })}
          </Text>
          <Table<PrecheckReport['conflicts']['name_conflicts'][number]>
            style={{ marginTop: 6 }}
            size="small"
            rowKey={(row) => `${row.kind}-${row.names?.join('|')}`}
            pagination={false}
            dataSource={conflicts.name_conflicts ?? []}
            columns={[
              {
                title: t('migration.conflictKind'),
                dataIndex: 'kind',
                key: 'kind',
                width: 140,
                render: (v: string) => <Tag color="orange">{v}</Tag>,
              },
              {
                title: t('migration.conflictNames'),
                dataIndex: 'names',
                key: 'names',
                render: (v: string[]) => (
                  <Space size={4} wrap>
                    {(v ?? []).map((n) => (
                      <Tag key={n} className="or-mono">
                        {n}
                      </Tag>
                    ))}
                  </Space>
                ),
              },
              {
                title: t('rule.suggestion'),
                dataIndex: 'suggestion',
                key: 'suggestion',
                width: 300,
                render: (v: string) => <Text style={{ fontSize: 12 }}>{v || '-'}</Text>,
              },
            ]}
            locale={{ emptyText: <Text type="secondary">{t('common.none')}</Text> }}
          />
        </div>

        <div style={{ marginTop: 10 }}>
          <Text strong style={{ fontSize: 13 }}>
            {t('migration.portConflicts')}
          </Text>
          <Text type="secondary" style={{ fontSize: 12, marginLeft: 8 }}>
            {t('migration.portConflictCount', { n: conflicts.port_conflicts?.length ?? 0 })}
          </Text>
          <Table<PrecheckReport['conflicts']['port_conflicts'][number]>
            style={{ marginTop: 6 }}
            size="small"
            rowKey={(row) => `${row.group}-${row.port}`}
            pagination={false}
            dataSource={conflicts.port_conflicts ?? []}
            columns={[
              {
                title: t('migration.portGroup'),
                dataIndex: 'group',
                key: 'group',
                width: 200,
                render: (v: string) => <Text className="or-mono">{v}</Text>,
              },
              {
                title: t('rule.listenPort'),
                dataIndex: 'port',
                key: 'port',
                width: 120,
                render: (v: number) => (
                  <Text className="or-mono" style={{ color: 'var(--or-warning)' }}>
                    {v}
                  </Text>
                ),
              },
              {
                title: t('migration.portRules'),
                dataIndex: 'rules',
                key: 'rules',
                render: (v: string[]) => (
                  <Space size={4} wrap>
                    {(v ?? []).map((r) => (
                      <Tag key={r} className="or-mono">
                        {r}
                      </Tag>
                    ))}
                  </Space>
                ),
              },
            ]}
            locale={{ emptyText: <Text type="secondary">{t('common.none')}</Text> }}
          />
        </div>

        <div style={{ marginTop: 10 }}>
          <Text strong style={{ fontSize: 13 }}>
            {t('migration.missingRefs')}
          </Text>
          <Text type="secondary" style={{ fontSize: 12, marginLeft: 8 }}>
            {t('migration.missingRefCount', { n: conflicts.missing_refs?.length ?? 0 })}
          </Text>
          <Table<PrecheckReport['conflicts']['missing_refs'][number]>
            style={{ marginTop: 6 }}
            size="small"
            rowKey={(row) => `${row.rule}-${row.missing}`}
            pagination={false}
            dataSource={conflicts.missing_refs ?? []}
            columns={[
              {
                title: t('migration.conflictRule'),
                dataIndex: 'rule',
                key: 'rule',
                width: 280,
                render: (v: string) => <Text className="or-mono">{v}</Text>,
              },
              {
                title: t('migration.missing'),
                dataIndex: 'missing',
                key: 'missing',
                render: (v: string) => (
                  <Text className="or-mono" style={{ color: 'var(--or-error)' }}>
                    {v}
                  </Text>
                ),
              },
            ]}
            locale={{ emptyText: <Text type="secondary">{t('common.none')}</Text> }}
          />
        </div>

        <div style={{ marginTop: 10 }}>
          <Text strong style={{ fontSize: 13 }}>
            {t('migration.zeroMultiplier')}
          </Text>
          <Text type="secondary" style={{ fontSize: 12, marginLeft: 8 }}>
            {t('migration.zeroMultiplierCount', {
              n: conflicts.zero_multiplier_groups?.length ?? 0,
            })}
          </Text>
          <div style={{ marginTop: 6 }}>
            <Space size={4} wrap>
              {(conflicts.zero_multiplier_groups ?? []).map((g) => (
                <Tag key={g} color="orange" className="or-mono">
                  {g}
                </Tag>
              ))}
              {!conflicts.zero_multiplier_groups?.length && (
                <Text type="secondary">{t('common.none')}</Text>
              )}
            </Space>
          </div>
          <Paragraph type="secondary" style={{ fontSize: 12, margin: '6px 0 0' }}>
            {t('migration.zeroMultiplierHint')}
          </Paragraph>
        </div>
      </Card>

      {/* ③ 不迁移的字段 */}
      <Card
        size="small"
        title={t('migration.sectionDiscarded')}
        style={{ borderLeft: '3px solid var(--or-text-tertiary)' }}
      >
        <Text type="secondary" style={{ fontSize: 12 }}>
          {t('migration.sectionDiscardedHint')}
        </Text>
        <div style={{ marginTop: 8 }}>
          <Space size={4} wrap>
            {(report.discarded_fields ?? []).map((f) => (
              <Tag key={f} color="red" className="or-mono">
                {f}
              </Tag>
            ))}
            {!report.discarded_fields?.length && <Text type="secondary">{t('common.none')}</Text>}
          </Space>
        </div>
      </Card>

      {/* ④ 使用默认值填充的字段 */}
      <Card
        size="small"
        title={t('migration.sectionFilled')}
        style={{ borderLeft: '3px solid var(--or-info)' }}
      >
        <Text type="secondary" style={{ fontSize: 12 }}>
          {t('migration.sectionFilledHint')}
        </Text>
        <div style={{ marginTop: 8 }}>
          <Space size={4} wrap>
            {(report.filled_fields ?? []).map((f) => (
              <Tag key={f} color="blue" className="or-mono">
                {f}
              </Tag>
            ))}
            {!report.filled_fields?.length && <Text type="secondary">{t('common.none')}</Text>}
          </Space>
        </div>
      </Card>

      {/* ⑤ 示例转换（before → after） */}
      <Card size="small" title={t('migration.sectionSamples')} style={{ borderLeft: '3px solid var(--or-primary)' }}>
        <Text type="secondary" style={{ fontSize: 12 }}>
          {t('migration.sectionSamplesHint')}
        </Text>
        <Space direction="vertical" size={10} style={{ width: '100%', marginTop: 8 }}>
          {(report.samples ?? []).map((s, idx) => (
            <div key={`sample-${idx}`} style={{ display: 'flex', gap: 12, alignItems: 'stretch' }}>
              <div style={{ flex: 1, minWidth: 0 }}>
                <Text type="secondary" style={{ fontSize: 12 }}>
                  {t('migration.sampleBefore')}
                </Text>
                <div className="or-diff" style={{ maxHeight: 220, marginTop: 4 }}>
                  {String(s.before ?? '')
                    .split('\n')
                    .map((line, i) => (
                      <div key={`b-${idx}-${i}`} className="or-diff-line or-diff-del">
                        {line}
                      </div>
                    ))}
                </div>
              </div>
              <div style={{ flex: 1, minWidth: 0 }}>
                <Text type="secondary" style={{ fontSize: 12 }}>
                  {t('migration.sampleAfter')}
                </Text>
                <div className="or-diff" style={{ maxHeight: 220, marginTop: 4 }}>
                  {String(s.after ?? '')
                    .split('\n')
                    .map((line, i) => (
                      <div key={`a-${idx}-${i}`} className="or-diff-line or-diff-add">
                        {line}
                      </div>
                    ))}
                </div>
              </div>
            </div>
          ))}
          {!report.samples?.length && <Text type="secondary">{t('common.none')}</Text>}
        </Space>
      </Card>

      {/* ⑥ 建议操作 */}
      <Card
        size="small"
        title={t('migration.sectionSuggestions')}
        style={{ borderLeft: '3px solid var(--or-success)' }}
      >
        {(report.suggestions ?? []).length === 0 ? (
          <Text type="secondary">{t('migration.noSuggestion')}</Text>
        ) : (
          <ol style={{ margin: 0, paddingLeft: 20 }}>
            {report.suggestions.map((s, i) => (
              <li key={`sug-${i}`}>
                <Text style={{ fontSize: 13 }}>{s}</Text>
              </li>
            ))}
          </ol>
        )}
      </Card>

      {/* ⑦ 原始文本报告（可折叠 + 复制） */}
      <Collapse
        items={[
          {
            key: 'text',
            label: t('migration.sectionText'),
            children: <RawReport text={report.text ?? ''} t={t} />,
          },
        ]}
      />
    </Space>
  )
}

/** 原始预检报告：等宽块 + 复制按钮。 */
function RawReport({ text, t }: { text: string; t: Translate }) {
  const { message } = App.useApp()

  const copy = async () => {
    try {
      if (navigator.clipboard?.writeText) {
        await navigator.clipboard.writeText(text)
      } else {
        // 非安全上下文（http 访问）下 clipboard API 不可用，退回 execCommand。
        const el = document.createElement('textarea')
        el.value = text
        el.style.position = 'fixed'
        el.style.opacity = '0'
        document.body.appendChild(el)
        el.select()
        document.execCommand('copy')
        document.body.removeChild(el)
      }
      message.success(t('common.copied'))
    } catch {
      message.warning(t('snapshot.exportFailed'))
    }
  }

  return (
    <Space direction="vertical" size={8} style={{ width: '100%' }}>
      <Space size={8}>
        <Button size="small" icon={<CopyOutlined />} onClick={() => void copy()}>
          {t('common.copy')}
        </Button>
        <Text type="secondary" style={{ fontSize: 12 }}>
          {t('migration.textHint')}
        </Text>
      </Space>
      <div className="or-report">{text || t('common.none')}</div>
    </Space>
  )
}

/** 批次最终报告：优先展示纯文本，其次回退到 JSON。 */
function BatchReportView({ batch, t }: { batch: MigrateBatch | null; t: Translate }) {
  if (!batch) return null
  const report = batch.report as { text?: string } | string | null | undefined

  if (typeof report === 'string' || typeof report?.text === 'string') {
    const text = typeof report === 'string' ? report : report.text ?? ''
    return <RawReport text={text} t={t} />
  }

  return (
    <Space direction="vertical" size={8} style={{ width: '100%' }}>
      <Descriptions size="small" column={2}>
        <Descriptions.Item label={t('migration.colStage')}>
          {stageLabel(batch.stage, t)}
        </Descriptions.Item>
        <Descriptions.Item label={t('migration.colRowsProgress')}>
          {batch.migrated_rows} / {batch.total_rows}
        </Descriptions.Item>
      </Descriptions>
      {report ? (
        <div className="or-report">{JSON.stringify(report, null, 2)}</div>
      ) : (
        <Empty className="or-empty" description={t('migration.batchReportEmpty')} />
      )}
    </Space>
  )
}

/* ═══════════════════════ 工具 ═══════════════════════ */

/** 阶段序号 → 文案（规格书 7.2 的阶段 1~7）。 */
function stageLabel(stage: number, t: Translate): string {
  if (!stage || stage < 1 || stage > STAGE_TOTAL) return '-'
  return t(STAGE_KEYS[stage - 1])
}

/** 字节 → 可读大小。 */
function formatBytes(bytes: number): string {
  if (!bytes || bytes <= 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let value = bytes
  let unit = 0
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024
    unit += 1
  }
  return `${value.toFixed(unit === 0 ? 0 : 1)} ${units[unit]}`
}

/** 截断长文本，配合 Tooltip 使用。 */
function truncate(text: string, keep: number): string {
  if (!text) return '-'
  return text.length <= keep ? text : `${text.slice(0, keep)}…`
}

/** RFC3339 → 本地时间。 */
function formatTime(iso: string): string {
  if (!iso) return '-'
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString()
}
