import test from 'node:test'
import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import ts from 'typescript'

const source = await readFile(new URL('../src/api/reportAdapters.ts', import.meta.url), 'utf8')
const compiled = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 } }).outputText
const { snapshotComparison, migrationReport } = await import(`data:text/javascript;base64,${Buffer.from(compiled).toString('base64')}`)

test('snapshot comparison preserves snapshot/current orientation and all fields', () => {
  const report = { entries: [
    {resource: 'rule', id: 1, name: 'snapshot only', action: 'add'},
    {resource: 'node', id: 2, name: 'current only', action: 'remove'},
    {resource: 'rule', id: 3, name: 'edited', action: 'update', fields: [{field: 'listen_port', snapshot: '80', current: '443'}]},
  ], added: 1, removed: 1, updated: 1, has_diff: true }
  const comparison = snapshotComparison(report)
  assert.deepEqual(comparison.removed, ['rule/1 snapshot only'])
  assert.deepEqual(comparison.added, ['node/2 current only'])
  assert.deepEqual(comparison.changed, [{path: 'rule/3 edited.listen_port', from: '80', to: '443'}])
  assert.equal(comparison.report, report)
})

test('migration API report populates risk and table sections without a conflicts wrapper', () => {
  const result = migrationReport({source: 'nyanpass', source_version: '1', target: 'sqlite', dry_run: true, status: 'finished',
    tables: [{label: 'Users', count: 2, target: 'users'}],
    name_conflicts: [{resource: 'user', left: 'Alice', right: 'alice', suggestion: 'rename'}],
    port_conflicts: [{inbound_group_id: 1, inbound_group_name: 'entry', port: 443, rules: ['a', 'b']}],
    missing_refs: [{rule_id: 3, rule_name: 'orphan', field: 'user_id', ref_id: 7, message: 'missing owner'}],
    fills: [{target: 'token', default: 'random', reason: 'required'}],
    examples: [{before: {name: 'old'}, after: {name: 'new'}}], text: 'report',
  })
  assert.equal(result.tables[0].rows, 2)
  assert.deepEqual(result.conflicts.name_conflicts[0].names, ['Alice', 'alice'])
  assert.equal(result.conflicts.missing_refs[0].missing, 'missing owner')
  assert.equal(result.conflicts.port_conflicts[0].group, 'entry')
  assert.equal(result.summary.mode, 'dry-run')
  assert.match(result.samples[0].after, /new/)
  assert.equal(result.has_blocking_conflict, false)
})

test('empty optional report sections remain renderable and failed status is visible', () => {
  const result = migrationReport({status: 'failed', error: 'validation error'})
  assert.deepEqual(result.conflicts.name_conflicts, [])
  assert.deepEqual(result.samples, [])
  assert.equal(result.has_blocking_conflict, true)
})
