import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import * as Vue from 'vue'
import { parse } from '@vue/compiler-sfc'

function transpile(source) { return ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } }).outputText }
const utilsContext = { exports: {} }
vm.runInNewContext(transpile(readFileSync(new URL('../src/utils/migrationRepair.ts', import.meta.url), 'utf8')), utilsContext)
const utils = utilsContext.exports
const descriptor = parse(readFileSync(new URL('../src/components/MigrationRepairPanel.vue', import.meta.url), 'utf8')).descriptor
const fields = 'records, binding, job, activeAttempt, editing, draftUnits, mappingForm, addColumnMapping, reviewed, editorVisible, executeConfirmVisible, executionSnapshot, executionKey, recoveryVisible, recoveryObject, recoveryForm, recoveryPlan, canRecover, attempts, openEditor, closeEditor, useSuggested, verify, prepareExecute, confirmExecute, saveDraft, openRecovery, reviewRelated, loadPreview, invalidatePreview, confirmRecovery, refresh, refreshHistory, exportHistory, bindingForm, openBinding, saveBinding'
function panel(props, overrides = {}) {
  const lifecycle = {}, messages = [], scheduled = []
  let uuid = 0
  const api = {
    listRepairs: async () => ({ data: { objects: [], binding: { configured: true } } }),
    listRepairAttempts: async () => ({ data: { items: [] } }),
    ...overrides,
  }
  const context = {
    exports: {}, defineProps: () => props, defineExpose() {},
    crypto: { randomUUID: () => `uuid-${++uuid}` },
    setTimeout: fn => { scheduled.push(fn); return scheduled.length }, clearTimeout() {},
    require: id => {
      if (id === 'vue') return { ...Vue, onUnmounted: fn => { lifecycle.unmount = fn } }
      if (id === '@/utils/migrationRepair') return utils
      if (id === '@/api/migrationRepair') return api
      if (id === '@/api/migration') return { getDataMigrationJob: async id => ({ data: { job_id: id, status: 'failed', dst_db_type: 'postgres' } }) }
      if (id === '@/api/connections') return { listConnections: async () => ({ data: [] }) }
      if (id === '@arco-design/web-vue') return { Message: { success: text => messages.push(text), warning: text => messages.push(text), error: text => messages.push(text), info: text => messages.push(text) } }
      throw new Error(`Unexpected import ${id}`)
    },
  }
  vm.runInNewContext(transpile(`${descriptor.scriptSetup.content}\nexport { ${fields} };`), context)
  return { ...context.exports, lifecycle, messages, scheduled }
}
const tick = async () => { await Vue.nextTick(); await Promise.resolve(); await Vue.nextTick() }
const unit = (id, status, extra = {}) => ({ id, status, sql: `CREATE ${id}`, original_sql: `CREATE ${id}`, selected: true, destructive: false, ...extra })
const record = (extra = {}) => ({ version: 1, object: { id: 'obj', kind: 'table', name: 'Orders', target_name: 'Orders', schema: 'public', ddl: 'CREATE TABLE Orders (id INT)', status: 'failed', units: [unit('table', 'failed')], dependencies: [], legacy: false, requires_review: false, ...extra } })

test('reopening repair drafts never selects committed or destructive steps and preserves program bodies', () => {
  const body = "BEGIN\n x := 'a;b';\n NULL;\nEND;"
  const cloned = utils.cloneRepairUnits([unit('done', 'succeeded'), unit('drop', 'failed', { destructive: true }), unit('procedure', 'failed', { sql: body })])
  assert.deepEqual(Array.from(cloned, item => item.selected), [false, false, true])
  assert.equal(cloned[2].sql, body)
  assert.equal(utils.mappingError({ a: 'id', b: 'id' }), '多个源列不能映射到同一个目标列')
  assert.equal(utils.mappingError({ a: '' }), '每个源列都需要指定目标列')
})

test('switching job drops old list, history and editor responses', async () => {
  const scope = Vue.effectScope()
  try {
    const props = Vue.reactive({ jobID: 'old' }), lists = [], histories = []
    const p = scope.run(() => panel(props, {
      listRepairs: id => new Promise(resolve => lists.push({ id, resolve })),
      listRepairAttempts: id => new Promise(resolve => histories.push({ id, resolve })),
    }))
    const history = p.refreshHistory()
    props.jobID = 'new'; await tick()
    lists[1].resolve({ data: { objects: [record({ name: 'new' })], binding: { configured: true } } }); await tick()
    lists[0].resolve({ data: { objects: [record({ name: 'old' })], binding: { configured: true } } })
    histories[0].resolve({ data: { items: [{ id: 'old-attempt' }] } }); await history; await tick()
    assert.equal(p.records.value[0].object.name, 'new')
    assert.equal(p.attempts.value.length, 0)
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('execution saves a versioned complete-unit snapshot and keyboard confirmation cannot send SQL directly', async () => {
  const scope = Vue.effectScope()
  try {
    const calls = []
    const p = scope.run(() => panel({ jobID: 'job' }, {
      saveRepairDraft: async (...args) => { calls.push(['save', ...args]); return { data: { version: 2, object: { ...record().object, units: args[2].units } } } },
      executeRepair: async (...args) => { calls.push(['execute', ...args]); return { data: { id: 'attempt', status: 'running' } } },
      getRepairAttempt: async () => ({ data: { id: 'attempt', status: 'running', units: [] } }),
    }))
    await tick()
    p.openEditor(record({ units: [unit('done', 'succeeded'), unit('drop', 'failed', { destructive: true }), unit('procedure', 'failed', { sql: 'BEGIN NULL; END;' })] }))
    await p.prepareExecute()
    assert.equal(p.executeConfirmVisible.value, true)
    assert.equal(calls.length, 1)
    assert.equal(p.executionSnapshot.value.length, 1)
    assert.equal(p.executionSnapshot.value[0].sql, 'BEGIN NULL; END;')
    await p.confirmExecute()
    assert.equal(calls[1][0], 'execute')
    assert.equal(calls[1][3].version, 2)
    assert.equal(calls[1][3].confirmed, true)
    assert.equal(calls[1][3].request_id, 'uuid-1')
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('editing invalidates confirmation and stale previews cannot authorize changed mappings', async () => {
  const scope = Vue.effectScope()
  try {
    let resolvePreview
    const p = scope.run(() => panel({ jobID: 'job' }, { previewRecovery: () => new Promise(resolve => { resolvePreview = resolve }) }))
    await tick()
    p.openEditor(record())
    p.executeConfirmVisible.value = true
    p.draftUnits.value[0].sql = 'CREATE TABLE changed (id INT)'
    assert.equal(p.executeConfirmVisible.value, false)
    p.openRecovery(record())
    p.recoveryForm.mapping = { id: 'renamed' }
    p.invalidatePreview()
    resolvePreview({ data: { token: 'stale', preview: { column_mapping: { id: 'id' } } } }); await tick()
    assert.equal(p.recoveryPlan.value, null)
    assert.equal(p.canRecover.value, false)
    assert.equal(p.recoveryForm.mapping.id, 'renamed')
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('nonempty target requires exact table confirmation and both stop-write confirmations', async () => {
  const scope = Vue.effectScope()
  try {
    const p = scope.run(() => panel({ jobID: 'job' })); await tick()
    p.recoveryPlan.value = { token: 'preview', preview: { data_required: true, target_rows: 4, qualified_target: 'public.Orders' } }
    p.recoveryForm.sourceQuiesced = true; p.recoveryForm.targetQuiesced = true; p.recoveryForm.reviewed = true
    assert.equal(p.canRecover.value, false)
    p.recoveryForm.truncate = true; p.recoveryForm.confirmation = 'Orders'
    assert.equal(p.canRecover.value, false)
    p.recoveryForm.confirmation = 'public.Orders'
    assert.equal(p.canRecover.value, true)
    p.recoveryForm.targetQuiesced = false
    assert.equal(p.canRecover.value, false)
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('report history includes original errors, drafts and immutable submitted SQL', () => {
  const object = record({ error: 'original failure', ddl: 'old DDL', units: [unit('new', 'pending', { sql: 'draft DDL' })] })
  const lines = utils.formatRepairHistory([object], [{ id: 'attempt', object_id: 'obj', created_at: 'today', kind: 'ddl', status: 'failed', units: [unit('new', 'failed', { sql: 'submitted DDL', error: 'database failure' })] }]).join('\n')
  for (const expected of ['original failure', 'old DDL', 'draft DDL', 'submitted DDL', 'database failure']) assert.ok(lines.includes(expected))
})

test('typing while a save is pending preserves newer edits and does not authorize the old snapshot', async () => {
  const scope = Vue.effectScope()
  try {
    let finishSave
    const p = scope.run(() => panel({ jobID: 'job' }, { saveRepairDraft: () => new Promise(resolve => { finishSave = resolve }) }))
    await tick(); p.openEditor(record())
    const pending = p.prepareExecute()
    p.draftUnits.value[0].sql = 'CREATE TABLE newer (id BIGINT)'
    finishSave({ data: { version: 2, object: record().object } })
    await pending
    assert.equal(p.editing.value.version, 2)
    assert.equal(p.draftUnits.value[0].sql, 'CREATE TABLE newer (id BIGINT)')
    assert.equal(p.executeConfirmVisible.value, false)
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('recovery cannot execute regenerated related candidates before their explicit review', async () => {
  const scope = Vue.effectScope()
  try {
    const p = scope.run(() => panel({ jobID: 'job' })); await tick()
    p.recoveryPlan.value = { token: 'preview', preview: { data_required: false, target_rows: 20, related: [{ id: 'pk', requires_review: true }] } }
    p.recoveryForm.sourceQuiesced = true; p.recoveryForm.targetQuiesced = true; p.recoveryForm.reviewed = true
    assert.equal(p.canRecover.value, false)
    p.recoveryPlan.value.preview.related[0].requires_review = false
    assert.equal(p.canRecover.value, true)
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('table DDL and renamed source-column mapping are saved together before execution', async () => {
  const scope = Vue.effectScope()
  try {
    let submitted
    const original = record({ source_columns: [{ Name: 'ROWID' }], column_mapping: { ROWID: 'ROW_ID' } })
    const p = scope.run(() => panel({ jobID: 'job' }, { saveRepairDraft: async (_job, _object, request) => {
      submitted = JSON.parse(JSON.stringify(request))
      return { data: { version: 2, object: { ...original.object, units: request.units, column_mapping: request.column_mapping } } }
    } }))
    await tick(); p.openEditor(original)
    p.draftUnits.value[0].sql = 'CREATE TABLE Orders (renamed_id INT)'
    p.mappingForm.values.ROWID = 'renamed_id'
    await p.prepareExecute()
    assert.equal(submitted.column_mapping.ROWID, 'renamed_id')
    assert.equal(submitted.units[0].sql, 'CREATE TABLE Orders (renamed_id INT)')
    assert.equal(p.executeConfirmVisible.value, true)
    p.mappingForm.values.ROWID = 'another_name'
    assert.equal(p.executeConfirmVisible.value, false)
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('mapping-only save on a completed table keeps every successful SQL unit unselected', async () => {
  const scope = Vue.effectScope()
  try {
    let submitted
    const original = record({ status: 'succeeded', units: [unit('create', 'succeeded')], column_mapping: { id: 'id' } })
    const p = scope.run(() => panel({ jobID: 'job' }, { saveRepairDraft: async (_job, _object, request) => {
      submitted = JSON.parse(JSON.stringify(request))
      return { data: { version: 2, object: { ...original.object, units: request.units, column_mapping: request.column_mapping } } }
    } }))
    await tick(); p.openEditor(original)
    p.mappingForm.values.id = 'renamed_id'
    await p.saveDraft()
    assert.equal(submitted.units[0].selected, false)
    assert.equal(submitted.units[0].sql, 'CREATE create')
    assert.equal(p.editing.value.object.status, 'succeeded')
    assert.equal(p.editing.value.object.column_mapping.id, 'renamed_id')
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('legacy tasks require explicit content selection and support manual source-column mappings', async () => {
  const scope = Vue.effectScope()
  try {
    let calls = 0
    const p = scope.run(() => panel({ jobID: 'job' }, { bindRepairConnections: async () => { calls++; return { data: {} } } }))
    await tick(); p.binding.value = { configured: false, legacy: true, dst_conn_id: 1 }
    await p.openBinding()
    assert.equal(p.bindingForm.config.Content, '')
    await p.saveBinding()
    assert.equal(calls, 0)
    p.bindingForm.config.Content = 'schema_only'
    await p.saveBinding()
    assert.equal(calls, 1)
    p.openEditor(record({ legacy: true, source_columns: undefined, columns: undefined }))
    p.mappingForm.source = 'ROWID'; p.mappingForm.target = 'ROW_ID'
    p.addColumnMapping()
    assert.equal(p.mappingForm.values.ROWID, 'ROW_ID')
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('schema-only recovery never submits stale truncate consent', async () => {
  const scope = Vue.effectScope()
  try {
    let submitted
    const p = scope.run(() => panel({ jobID: 'job' }, {
      startRecovery: async (_job, _object, request) => { submitted = request; return { data: { id: 'recovery', status: 'running' } } },
      getRepairAttempt: async () => ({ data: { id: 'recovery', status: 'running', units: [] } }),
    }))
    await tick()
    p.recoveryObject.value = record({ status: 'succeeded', data_status: 'not_required' })
    p.recoveryPlan.value = { token: 'preview', preview: { data_required: false, target_rows: 20, related: [] } }
    p.recoveryForm.sourceQuiesced = true; p.recoveryForm.targetQuiesced = true; p.recoveryForm.reviewed = true
    p.recoveryForm.truncate = true; p.recoveryForm.confirmation = 'public.Orders'
    await p.confirmRecovery()
    assert.equal(submitted.truncate, false)
    assert.equal(submitted.confirmation_text, undefined)
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('pending related objects can be reviewed one at a time and return to a usable recovery preview', async () => {
  const scope = Vue.effectScope()
  try {
    const table = record({ status: 'succeeded' })
    let index = { version: 1, object: { ...record().object, id: 'index', kind: 'index', table: 'Orders', name: 'ix_id', target_name: 'ix_id', requires_review: true } }
    const p = scope.run(() => panel({ jobID: 'job' }, {
      listRepairs: async () => ({ data: { objects: [table, index], binding: { configured: true } } }),
      previewRecovery: async (_job, _object, request) => ({ data: { token: 'preview', preview: { data_required: false, target_rows: 1, column_mapping: {}, related: request.related_object_ids.includes('index') ? [index.object] : [] } } }),
      saveRepairDraft: async (_job, _object, request) => {
        index = { version: 2, object: { ...index.object, units: request.units, requires_review: !request.reviewed } }
        return { data: index }
      },
    }))
    await tick(); p.openRecovery(table); await tick(); await tick()
    assert.equal(p.recoveryForm.related.index, false)
    p.recoveryForm.related.index = true; p.invalidatePreview(); await p.loadPreview()
    p.recoveryForm.sourceQuiesced = true; p.recoveryForm.targetQuiesced = true; p.recoveryForm.reviewed = true
    assert.equal(p.canRecover.value, false)
    p.reviewRelated('index')
    assert.equal(p.editorVisible.value, true)
    assert.equal(p.editing.value.object.id, 'index')
    p.reviewed.value = true; await p.saveDraft(); p.closeEditor(); await tick(); await tick()
    assert.equal(p.recoveryVisible.value, true)
    assert.equal(p.recoveryPlan.value.preview.related[0].requires_review, false)
    p.recoveryForm.reviewed = true
    assert.equal(p.canRecover.value, true)
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('legacy PostgreSQL schema snapshot is not reused as the source database', async () => {
  const scope = Vue.effectScope()
  try {
    const p = scope.run(() => panel({ jobID: 'job' })); await tick()
    p.job.value = { src_db_type: 'postgres', src_conn: { database: 'old_schema' } }
    p.binding.value = { configured: false, legacy: true }
    await p.openBinding()
    assert.equal(p.bindingForm.source_database, '')
    assert.equal(p.bindingForm.source_schema, '')
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('a conditional create awaiting verification is visible as unresolved and cannot be counted repaired', () => {
  assert.equal(utils.repairStatus('unverified'), '已执行待核验')
  assert.equal(utils.unitCommitted('unverified'), false)
})

test('verification can offer a MySQL column-comment DDL without executing or replacing original diagnostics', async () => {
  const scope = Vue.effectScope()
  try {
    const original = record({ kind: 'comment', ddl: '', units: [], error: 'original metadata failure' })
    const suggested = "ALTER TABLE Orders MODIFY id INT NOT NULL COMMENT 'identifier'"
    const p = scope.run(() => panel({ jobID: 'job' }, {
      verifyRepair: async () => ({ data: { version: 2, object: { ...original.object, suggested_ddl: suggested }, verification: { status: 'failed', message: '已从实际列定义生成修复建议' } } }),
    }))
    await tick(); p.records.value = [original]
    await p.verify(original)
    p.openEditor(p.records.value[0])
    assert.equal(p.draftUnits.value.length, 0)
    p.useSuggested()
    assert.equal(p.draftUnits.value.length, 1)
    assert.equal(p.draftUnits.value[0].sql, suggested)
    assert.equal(p.editing.value.object.ddl, '')
    assert.equal(p.executeConfirmVisible.value, false)
    p.openEditor(original); p.useSuggested()
    assert.equal(p.draftUnits.value.length, 0)
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('export distinguishes immutable original error from the latest repair failure', () => {
  const lines = utils.formatRepairHistory([record({ original_error: 'original create failed', error: 'dependency failed' })], []).join('\n')
  assert.match(lines, /原始错误：original create failed/)
  assert.match(lines, /当前错误：dependency failed/)
  assert.doesNotMatch(lines, /原始错误：dependency failed/)
  const pending = utils.formatRepairHistory([record({ original_status: 'pending', original_error: '', error: 'later failure' })], []).join('\n')
  assert.doesNotMatch(pending, /原始错误：later failure/)
  assert.match(pending, /当前错误：later failure/)
})
