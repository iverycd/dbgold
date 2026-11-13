import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'

const source = readFileSync(new URL('../src/utils/incrementalJob.ts', import.meta.url), 'utf8')
const context = { exports: {} }
vm.runInNewContext(ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } }).outputText, context)
const { resumable, legacyDamengSchema } = context.exports

test('old Dameng jobs cannot resume, while current snapshots and other targets remain eligible', () => {
  const job = { dst_db_type: 'dameng', status: 'paused_restart', locator_strategy_version: 1 }
  for (const version of [undefined, 0, 2]) {
    assert.equal(legacyDamengSchema({ ...job, source_schema_version: version }), true)
    assert.equal(resumable({ ...job, source_schema_version: version }), false)
  }
  assert.equal(resumable({ ...job, source_schema_version: 1 }), true)
  assert.equal(resumable({ ...job, dst_db_type: 'postgres', source_schema_version: 0 }), true)
  assert.equal(legacyDamengSchema({ ...job, dst_db_type: 'postgres' }), false)
  for (const status of ['aborted', 'stopped', 'paused_ddl', 'running']) {
    assert.equal(resumable({ ...job, status, source_schema_version: 1 }), false)
  }
})
