import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import * as Vue from 'vue'
import { parse } from '@vue/compiler-sfc'

const file = path => readFileSync(new URL(path, import.meta.url), 'utf8')
const transpile = source => ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } }).outputText
const utilityContext = { exports: {} }
vm.runInNewContext(transpile(file('../src/utils/migrationRepair.ts')), utilityContext)
const batchContext = { exports: {}, require: () => utilityContext.exports }
vm.runInNewContext(transpile(file('../src/utils/migrationRepairBatch.ts')), batchContext)
const { descriptor } = parse(file('../src/components/MigrationRepairPanel.vue'))
const fields = 'records, job, binding, editing, draftUnits, reviewed, sequencePlanPending, sequencePlanMessage, canRefreshSequencePlan, canExecute, refreshingSequencePlan, openEditor, closeEditor, refreshSequencePlan, saveDraft, restoreOriginal'

function panel(overrides = {}) {
  const messages = []
  const api = { listRepairs: async () => ({ data: { objects: [], binding: { configured: true } } }), listRepairAttempts: async () => ({ data: { items: [] } }), ...overrides }
  const context = {
    exports: {}, defineProps: () => ({ jobID: 'job' }), defineExpose() {}, defineEmits: () => () => {},
    setTimeout: () => 0, clearTimeout() {}, crypto: { randomUUID: () => 'new-unit' },
    require: id => {
      if (id === 'vue') return { ...Vue, onUnmounted() {} }
      if (id === '@/utils/migrationRepair') return utilityContext.exports
      if (id === '@/utils/migrationRepairBatch') return batchContext.exports
      if (id === '@/utils/dbType') return { getDbTypeLabel: value => value }
      if (id === '@/api/migrationRepair') return api
      if (id === '@/api/migration') return { getDataMigrationJob: async () => ({ data: { job_id: 'job', status: 'failed', src_db_type: 'oracle', dst_db_type: 'dameng' } }) }
      if (id === '@/api/connections') return { listConnections: async () => ({ data: [] }) }
      if (id === '@arco-design/web-vue') return { Message: Object.fromEntries(['success', 'warning', 'error', 'info'].map(key => [key, message => messages.push(message)])) }
      throw new Error(`Unexpected import ${id}`)
    },
  }
  vm.runInNewContext(transpile(`${descriptor.scriptSetup.content}\nexport { ${fields} };`), context)
  return { ...context.exports, messages }
}
const tick = async () => { await Vue.nextTick(); await Promise.resolve(); await Vue.nextTick() }
const unit = (id, sql) => ({ id, sql, original_sql: sql, status: 'failed', selected: true, destructive: false })
const record = () => ({ version: 1, object: { id: 'seq', kind: 'sequence', name: 'S', target_name: 'S', schema: 'APP', table: 'T', ddl: 'original generated SQL', status: 'failed', units: [unit('create', 'CREATE SEQUENCE "APP"."S" START WITH 1')], dependencies: [], legacy: false, requires_review: false, sequence: { Native: { Name: 'S' } } } })
const candidate = () => ({ version: 1, object: { ...record().object, requires_review: true, units: [unit('alter', 'ALTER SEQUENCE "APP"."S" CURRENT VALUE 42')] }, has_changes: true, message: '请核对并保存序列计划' })

test('sequence plan API encodes object identity and passes draft version', async () => {
  const requests = []
  const context = { exports: {}, require: () => ({ default: { post: async (...args) => requests.push(args) } }) }
  vm.runInNewContext(transpile(file('../src/api/migrationRepair.ts')), context)
  await context.exports.refreshSequenceRepairPlan('job/a', 'seq b', { version: 4 })
  assert.equal(requests[0][0], '/migration/data-migrate/job%2Fa/repairs/seq%20b/sequence-plan')
  assert.equal(requests[0][1].version, 4)
})

test('refresh stays local and requires review before saving generated origins', async () => {
  const saves = []
  const app = panel({
    refreshSequenceRepairPlan: async () => ({ data: candidate() }),
    saveRepairDraft: async (_job, _object, data) => {
      saves.push(data)
      return { data: { version: 2, object: { ...candidate().object, requires_review: false, units: data.units } } }
    },
  })
  await tick()
  const original = record()
  app.records.value = [original]
  app.openEditor(original)
  assert.equal(app.canRefreshSequencePlan.value, true)
  await app.refreshSequencePlan()
  assert.equal(saves.length, 0)
  assert.equal(app.records.value[0].object.units[0].id, 'create', 'read-only planning does not replace persisted records')
  assert.equal(app.draftUnits.value[0].id, 'alter')
  assert.equal(app.sequencePlanPending.value, true)
  assert.equal(app.reviewed.value, false)
  assert.equal(app.canExecute.value, false)
  app.reviewed.value = true
  assert.equal(await app.saveDraft(), true)
  assert.equal(saves[0].sequence_plan, true)
  assert.equal(saves[0].reviewed, true)
  assert.equal(app.records.value[0].version, 2)
  assert.equal(app.sequencePlanPending.value, false)
})

test('manual or unknown drafts cannot refresh, and local edits are never overwritten', async () => {
  let resolve
  const app = panel({ refreshSequenceRepairPlan: () => new Promise(done => { resolve = done }) })
  await tick()
  const original = record()
  original.object.units[0].sql += ' -- manual'
  app.openEditor(original)
  assert.equal(app.canRefreshSequencePlan.value, false)
  const unknown = record()
  unknown.object.status = 'unknown'
  app.openEditor(unknown)
  assert.equal(app.canRefreshSequencePlan.value, false)
  const preflight = record()
  preflight.object.sequence.Unsupported = 'old target bound error'
  preflight.object.sequence.FailureStage = 'preflight'
  app.openEditor(preflight)
  assert.equal(app.canRefreshSequencePlan.value, true, 'old target preflight failures can be revalidated')
  const metadata = record()
  metadata.object.sequence.Unsupported = 'missing source metadata'
  metadata.object.sequence.FailureStage = 'metadata'
  app.openEditor(metadata)
  assert.equal(app.canRefreshSequencePlan.value, false)
  app.openEditor(record())
  const request = app.refreshSequencePlan()
  app.draftUnits.value[0].sql = 'my unsaved SQL'
  resolve({ data: candidate() })
  await request
  assert.equal(app.draftUnits.value[0].sql, 'my unsaved SQL')
  assert.equal(app.sequencePlanPending.value, false)
  assert.ok(app.messages.some(message => message.includes('未覆盖')))
})

test('editing or restoring a generated plan cannot bypass server plan validation', async () => {
  const saves = []
  const app = panel({
    refreshSequenceRepairPlan: async () => ({ data: candidate() }),
    saveRepairDraft: async (_job, _object, data) => {
      saves.push(data)
      if (data.units[0].sql !== candidate().object.units[0].sql) throw new Error('序列计划已变化或执行单元已被编辑')
      return { data: { version: 2, object: { ...candidate().object, requires_review: false, units: data.units } } }
    },
  })
  await tick()
  const original = record()
  app.records.value = [original]
  app.openEditor(original)
  await app.refreshSequencePlan()
  app.reviewed.value = true
  app.draftUnits.value[0].sql += ' -- formatted by user'
  assert.equal(app.sequencePlanPending.value, true)
  assert.equal(await app.saveDraft(), false)
  assert.equal(saves[0].sequence_plan, true, 'edited generated SQL still requires fresh server plan validation')
  assert.equal(app.records.value[0].version, 1)
  assert.ok(app.draftUnits.value[0].sql.includes('formatted by user'))
  app.restoreOriginal()
  assert.equal(app.draftUnits.value[0].sql, candidate().object.units[0].sql)
  assert.equal(app.sequencePlanPending.value, true, 'restoring SQL must not downgrade the candidate to manual execution')
  assert.equal(await app.saveDraft(), true)
  assert.equal(saves[1].sequence_plan, true)
  assert.equal(app.sequencePlanPending.value, false)
})

test('a late plan response cannot replace another object editor', async () => {
  let resolve
  const app = panel({ refreshSequenceRepairPlan: () => new Promise(done => { resolve = done }) })
  await tick()
  app.openEditor(record())
  const request = app.refreshSequencePlan()
  app.closeEditor()
  const other = record()
  other.object.id = 'other'
  app.openEditor(other)
  resolve({ data: candidate() })
  await request
  assert.equal(app.editing.value.object.id, 'other')
  assert.equal(app.draftUnits.value[0].id, 'create')
  assert.equal(app.sequencePlanPending.value, false)
})
