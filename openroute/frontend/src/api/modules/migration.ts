/** 迁移与备份接口（规格书 8.15）。 */
import { get, getList, post, download } from '../client'
import type { MigrateBatch, Backup, PageQuery } from '../types'

/** 迁移预检结果（对应第 7 章的预检报告）。 */
export interface PrecheckReport {
  /** 结构化报告，供分区块着色展示。 */
  summary: {
    source: string
    source_version: string
    target: string
    mode: string
  }
  /** 将迁移的数据：按表统计行数。 */
  tables: Array<{
    source_table: string
    target_table: string
    rows: number
    note?: string
  }>
  /** 冲突与风险。 */
  conflicts: {
    name_conflicts: Array<{ kind: string; names: string[]; suggestion: string }>
    port_conflicts: Array<{ group: string; port: number; rules: string[] }>
    missing_refs: Array<{ rule: string; missing: string }>
    zero_multiplier_groups: string[]
  }
  /** 不迁移的字段（丢弃清单）。 */
  discarded_fields: string[]
  /** 目标缺失字段的填充清单。 */
  filled_fields: string[]
  /** 三条示例转换结果。 */
  samples: Array<{ before: string; after: string }>
  /** 建议操作。 */
  suggestions: string[]
  /** 纯文本报告（终端同款，便于直接展示与复制）。 */
  text: string
  /** 是否存在必须人工处理的冲突。 */
  has_blocking_conflict: boolean
}

/** 预检请求。 */
export interface PrecheckInput {
  source: 'nyanpass' | 'openroute'
  dsn: string
  rename_policy?: 'fail' | 'suffix' | 'skip'
}

/** 执行迁移预检（不写任何数据）。 */
export function precheck(input: PrecheckInput) {
  return post<PrecheckReport>('/migrations/precheck', input)
}

/** 执行迁移；dry_run 为 true 时只预览。 */
export function run(input: PrecheckInput & { dry_run?: boolean }) {
  return post<MigrateBatch>('/migrations/run', input, {
    params: { dry_run: input.dry_run ?? false },
  })
}

/** 迁移批次列表。 */
export function list(params?: PageQuery) {
  return getList<MigrateBatch>('/migrations', params as Record<string, unknown>)
}

/** 批次详情与报告。 */
export function getOne(id: number) {
  return get<MigrateBatch>(`/migrations/${id}`)
}

/** 实时进度。 */
export function progress(id: number) {
  return get<{
    batch_id: number
    stage: number
    stage_name: string
    total_rows: number
    migrated_rows: number
    percent: number
    message: string
  }>(`/migrations/${id}/progress`)
}

/** 回滚迁移批次。 */
export function rollback(id: number) {
  return post<{ deleted_rows: number }>(`/migrations/${id}/rollback`)
}

// ───────────────────────── 备份 ─────────────────────────

/** 备份列表。 */
export function backups(params?: PageQuery) {
  return getList<Backup>('/backups', params as Record<string, unknown>)
}

/** 生成备份。 */
export function createBackup(withSecret = false) {
  return post<Backup>('/backups', { with_secret: withSecret })
}

/** 下载备份。 */
export function downloadBackup(id: number, name: string) {
  return download(`/api/v1/backups/${id}/download`, undefined, `${name}.zip`)
}

/** 从备份恢复。 */
export function restoreBackup(id: number) {
  return post<{ restored: boolean; backup_path: string }>(`/backups/${id}/restore`)
}
