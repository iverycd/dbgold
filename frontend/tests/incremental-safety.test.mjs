import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'

const source = readFileSync(new URL('../src/utils/incrementalJob.ts', import.meta.url), 'utf8')
const context = { exports: {} }
vm.runInNewContext(ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } }).outputText, context)
const { resumable, legacyIncrementalSchema, completable, cutoverContentNeedsAcknowledgment } = context.exports

test('old incremental jobs cannot resume without a current source schema', () => {
  const job = { dst_db_type: 'dameng', status: 'paused_restart', locator_strategy_version: 1 }
  for (const version of [undefined, 0, 2]) {
    assert.equal(legacyIncrementalSchema({ ...job, source_schema_version: version }), true)
    assert.equal(resumable({ ...job, source_schema_version: version }), false)
  }
  assert.equal(resumable({ ...job, source_schema_version: 1 }), true)
  assert.equal(resumable({ ...job, dst_db_type: 'postgres', source_schema_version: 0 }), false)
  assert.equal(resumable({ ...job, dst_db_type: 'postgres', source_schema_version: 1 }), true)
  for (const status of ['aborted', 'stopped', 'paused_ddl', 'running']) {
    assert.equal(resumable({ ...job, status, source_schema_version: 1 }), false)
  }
})

test('cutover requires current content results and passing hard checks', () => {
  const job = { status: 'ready_with_warnings', validation_state: 'passed' }
  assert.equal(completable(job), false)
  assert.equal(completable({ ...job, content_validation_state: 'running' }), false)
  assert.equal(completable({ ...job, content_validation_state: 'passed' }), true)
  const issue = { ...job, content_validation_state: 'needs_acknowledgment' }
  assert.equal(completable(issue), true)
  assert.equal(cutoverContentNeedsAcknowledgment(issue), true)
  assert.equal(completable({ ...issue, validation_state: 'mismatch' }), false)
  assert.equal(completable({ ...issue, status: 'cutover_blocked' }), false)
})
