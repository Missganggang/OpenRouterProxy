/** Snapshot actions describe rollback; the comparison displays snapshot → current. */
export interface SnapshotDiffResponse {
  snapshot_id: number
  checksum: string
  created_at: string
  entries: Array<{
    resource: string
    id: number
    name: string
    action: 'add' | 'remove' | 'update' | 'same'
    fields?: Array<{ field: string; current: string; snapshot: string }>
  }>
  added: number
  removed: number
  updated: number
  same: number
  has_diff: boolean
}

export function snapshotComparison(report: SnapshotDiffResponse) {
  const entries = report.entries ?? []
  const label = (entry: SnapshotDiffResponse['entries'][number]) =>
    `${entry.resource}/${entry.id} ${entry.name}`
  return {
    // In the left-to-right comparison, rollback additions are snapshot-only removals.
    added: entries.filter((entry) => entry.action === 'remove').map(label),
    removed: entries.filter((entry) => entry.action === 'add').map(label),
    changed: entries.filter((entry) => entry.action === 'update').flatMap((entry) =>
      (entry.fields ?? []).map((field) => ({
        path: `${label(entry)}.${field.field}`,
        from: field.snapshot,
        to: field.current,
      })),
    ),
    report,
  }
}

export interface MigrationReportResponse {
  source: string
  source_version: string
  target: string
  dry_run: boolean
  status: string
  summary: { total_rows: number; migrated_rows: number; conflict_count: number }
  tables?: Array<{ label: string; count: number; target: string; extra?: string; source_table?: string; target_table?: string }>
  name_conflicts?: Array<{ resource: string; left: string; right: string; suggestion: string }>
  port_conflicts?: Array<{ inbound_group_id: number; inbound_group_name: string; port: number; rules: string[] }>
  missing_refs?: Array<{ rule_id: number; rule_name: string; field: string; ref_id: number; message: string }>
  zero_multipliers?: Array<{ id: number; name: string; type: string; rate: number }>
  discards?: Array<{ source: string; reason: string; count?: number }>
  fills?: Array<{ target: string; default: unknown; reason: string }>
  examples?: Array<{ before: unknown; after: unknown }>
  suggestions?: string[]
  text: string
  error?: string
}

function reportText(value: unknown): string {
  return typeof value === 'string' ? value : JSON.stringify(value, null, 2) ?? ''
}

export function migrationReport(report: MigrationReportResponse) {
  return {
    summary: { source: report.source, source_version: report.source_version, target: report.target, mode: report.dry_run ? 'dry-run' : 'write' },
    tables: (report.tables ?? []).map((table) => ({ source_table: table.source_table || table.label, target_table: table.target_table || table.target, rows: table.count, note: table.extra })),
    conflicts: {
      name_conflicts: (report.name_conflicts ?? []).map((item) => ({ kind: item.resource, names: [item.left, item.right], suggestion: item.suggestion })),
      port_conflicts: (report.port_conflicts ?? []).map((item) => ({ group: item.inbound_group_name || `#${item.inbound_group_id}`, port: item.port, rules: item.rules })),
      missing_refs: (report.missing_refs ?? []).map((item) => ({ rule: item.rule_name || `#${item.rule_id}`, missing: item.message || `${item.field}: #${item.ref_id}` })),
      zero_multiplier_groups: (report.zero_multipliers ?? []).map((item) => item.name || `#${item.id}`),
    },
    discarded_fields: (report.discards ?? []).map((item) => `${item.source}: ${item.reason}`),
    filled_fields: (report.fills ?? []).map((item) => `${item.target} = ${reportText(item.default)}: ${item.reason}`),
    samples: (report.examples ?? []).map((item) => ({ before: reportText(item.before), after: reportText(item.after) })),
    suggestions: report.suggestions ?? [],
    text: report.text ?? '',
    has_blocking_conflict: Boolean(report.error || report.status === 'failed'),
  }
}
