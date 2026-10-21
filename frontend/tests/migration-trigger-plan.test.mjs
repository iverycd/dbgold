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
const fields = 'records, job, binding, editing, draftUnits, editorVisible, triggerPlanCandidate, triggerPlanReviewed, canRefreshTriggerPlan, canExecute, refreshingTriggerPlan, openEditor, closeEditor, refreshTriggerPlan, saveTriggerPlan, prepareExecute, executeConfirmVisible'

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
      if (id === '@/api/migration') return { getDataMigrationJob: async () => ({ data: { job_id: 'job', status: 'failed', src_db_type: 'mysql', dst_db_type: 'postgres' } }) }
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
const record = () => ({ version: 1, object: { id: 'trg', kind: 'trigger', name: 'update_isepoint', source_name: 'update_isepoint', source_table: 'frame_user_extendinfo', target_name: 'update_isepoint', schema: 'app', table: 'frame_user_extendinfo', source_ddl: 'CREATE TRIGGER update_isepoint ...', ddl: '', diagnostic_only: true, status: 'failed', original_error: '字符比较依赖源库排序规则，需要人工适配', units: [], dependencies: [], legacy: false, requires_review: false } })
const candidate = () => ({ version: 1, binding_version: 3, plan_hash: 'frozen-hash', message: '请核对源定义和完整触发器方案', object: { ...record().object, diagnostic_only: false, status: 'pending', mysql_pg_trigger: { Version: 2 }, units: [unit('function', 'CREATE FUNCTION app.audit() ...'), unit('trigger', 'CREATE TRIGGER update_isepoint ...')] } })

test('trigger preview and save use encoded identities and never send client SQL', async () => {
  const requests = []
  const context = { exports: {}, require: () => ({ default: { post: async (...args) => requests.push(['post', ...args]), put: async (...args) => requests.push(['put', ...args]) } }) }
  vm.runInNewContext(transpile(file('../src/api/migrationRepair.ts')), context)
  await context.exports.previewTriggerRepairPlan('job/a', 'trg b', { version: 4 })
  await context.exports.saveTriggerRepairPlan('job/a', 'trg b', { version: 4, binding_version: 3, plan_hash: 'h', reviewed: true })
  assert.equal(requests[0][1], '/migration/data-migrate/job%2Fa/repairs/trg%20b/trigger-plan')
  assert.equal(requests[1][0], 'put')
  assert.equal(requests[1][1], requests[0][1])
  assert.equal(requests[1][2].reviewed, true)
  assert.equal(requests[1][2].units, undefined)
})

test('failed historical trigger is previewed locally, reviewed, saved and executed as two units', async () => {
  const saves = []
  const app = panel({
    previewTriggerRepairPlan: async () => ({ data: candidate() }),
    saveTriggerRepairPlan: async (_job, _object, data) => { saves.push(data); return { data: { version: 2, object: candidate().object } } },
  })
  await tick()
  const original = record()
  app.records.value = [original]
  app.openEditor(original)
  assert.equal(app.canRefreshTriggerPlan.value, true)
  await app.refreshTriggerPlan()
  assert.equal(saves.length, 0)
  assert.equal(app.records.value[0].object.diagnostic_only, true)
  assert.equal(app.editing.value.object.original_error, original.object.original_error)
  assert.equal(app.triggerPlanCandidate.value.object.units.length, 2)
  assert.equal(app.canExecute.value, false)
  assert.equal(await app.saveTriggerPlan(), false)
  app.triggerPlanReviewed.value = true
  assert.equal(await app.saveTriggerPlan(), true)
  assert.equal(saves[0].plan_hash, 'frozen-hash')
  assert.equal(saves[0].binding_version, 3)
  assert.equal(saves[0].units, undefined)
  assert.equal(app.records.value[0].version, 2)
  assert.equal(app.triggerPlanCandidate.value, null)
  assert.equal(app.canExecute.value, true)
  await app.prepareExecute()
  assert.equal(app.executeConfirmVisible.value, true)
  assert.equal(app.draftUnits.value.length, 2)
})

test('unknown, generated and manually edited records cannot regenerate', async () => {
  const app = panel()
  await tick()
  for (const changes of [{ status: 'unknown' }, { status: 'succeeded' }, { mysql_pg_trigger: {} }, { ddl: 'manual SQL' }, { units: [unit('manual', 'SQL')] }, { source_ddl: '' }]) {
    app.openEditor({ version: 1, object: { ...record().object, ...changes } })
    assert.equal(app.canRefreshTriggerPlan.value, false)
  }
  app.openEditor(record())
  app.binding.value.configured = false
  assert.equal(app.canRefreshTriggerPlan.value, false)
})

test('preview failures retain diagnostics and closing discards unsaved or late candidates', async () => {
  let resolve
  const app = panel({ previewTriggerRepairPlan: () => new Promise(done => { resolve = done }) })
  await tick()
  const original = record()
  app.openEditor(original)
  const pending = app.refreshTriggerPlan()
  app.closeEditor()
  const other = record(); other.object.id = 'other'
  app.openEditor(other)
  resolve({ data: candidate() }); await pending
  assert.equal(app.editing.value.object.id, 'other')
  assert.equal(app.triggerPlanCandidate.value, null)
  assert.equal(app.editing.value.object.original_error, original.object.original_error)
})

test('stale save errors preserve the candidate and original report for review', async () => {
  const app = panel({ previewTriggerRepairPlan: async () => ({ data: candidate() }), saveTriggerRepairPlan: async () => { throw new Error('源定义或绑定版本已变化') } })
  await tick()
  app.records.value = [record()]
  app.openEditor(app.records.value[0]); await app.refreshTriggerPlan()
  app.triggerPlanReviewed.value = true
  assert.equal(await app.saveTriggerPlan(), false)
  assert.equal(app.records.value[0].version, 1)
  assert.equal(app.triggerPlanCandidate.value.plan_hash, 'frozen-hash')
  assert.ok(app.messages.some(message => message.includes('版本已变化')))
  app.closeEditor()
  assert.equal(app.triggerPlanCandidate.value, null)
})
