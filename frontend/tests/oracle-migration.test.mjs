import { connectionDatabaseUtils, connectionDatabaseEditors } from './helpers/connection-database.mjs'
import assert from 'node:assert/strict'
import { autoRepairUtils } from './helpers/auto-repairs.mjs'
import { migrationReportUtils } from './helpers/migration-report.mjs'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import * as Vue from 'vue'
import { parse } from '@vue/compiler-sfc'

function execute(source, require, globals = {}) {
  const { outputText } = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } })
  const context = { exports: {}, require, Blob, FormData, ...globals }
  vm.runInNewContext(outputText, context)
  return context.exports
}
const { applyTargetMigrationDefaults } = execute(readFileSync(new URL('../src/utils/migrationOptions.ts', import.meta.url), 'utf8'), () => {})

test('Oracle is selectable in all three forms and submits fixed target defaults', async () => {
  const scope = Vue.effectScope()
  const payloads = [], schemaRequests = []
  const submit = async payload => { payloads.push(payload); return { data: { job_id: 'oracle-test', results: [] } } }
  const { descriptor } = parse(readFileSync(new URL('../src/views/MigrationView.vue', import.meta.url), 'utf8'))
  const page = scope.run(() => execute(`${descriptor.scriptSetup.content}
    export { connections, supportedPairs, pgConnections, dataMigrate, viewMigrate, objMigrate,
      loadDstSchemas, vmLoadDstSchemas, omLoadDstSchemas, startDataMigration, handleMigrateViews, startObjectMigration,
      checkPairSupport, vmCheckPairSupport, omCheckPairSupport };`, name => {
    switch (name) {
      case 'vue': return { ...Vue, provide() {}, onMounted() {}, onUnmounted() {} }
      case 'vue-router': return { useRoute: () => ({ query: {} }), onBeforeRouteLeave() {} }
      case '@arco-design/web-vue': return { Message: { error: message => assert.fail(message), success() {}, warning() {} }, Modal: {} }
      case '@/utils/connectionDatabase': return connectionDatabaseUtils
      case '@/composables/connectionDatabaseEditors': return connectionDatabaseEditors
      case '@/utils/migrationOptions': return { applyTargetMigrationDefaults }
      case '@/api/connections': return { listConnectionSchemas: async id => { schemaRequests.push(id); return { data: ['APP', 'EMPTY_APP'] } } }
      case '@/api/migration': return { startDataMigration: submit, migrateViews: submit, startObjectMigration: submit, createDataMigrateEventSource: () => ({ addEventListener() {}, close() {} }) }
      default: return {}
    }
  }))
  try {
    page.connections.value = [{ id: 1, db_type: 'mysql' }, { id: 2, db_type: 'oracle' }]
    page.supportedPairs.value = [{ source: 'mysql', target: 'oracle' }]
    assert.equal(page.pgConnections.value.some(c => c.id === 2), true)
    for (const [state, load, start, check] of [
      [page.dataMigrate, page.loadDstSchemas, page.startDataMigration, page.checkPairSupport],
      [page.viewMigrate, page.vmLoadDstSchemas, page.handleMigrateViews, page.vmCheckPairSupport],
      [page.objMigrate, page.omLoadDstSchemas, page.startObjectMigration, page.omCheckPairSupport],
    ]) {
      state.srcConnId = 1
      state.dstConnId = 2
      state.lowerCaseNames = state.changeOwner = true
      if ('distributed' in state) state.distributed = true
      await Vue.nextTick()
      assert.equal(state.lowerCaseNames, false)
      assert.equal(state.changeOwner, false)
      if ('distributed' in state) assert.equal(state.distributed, false)
      await load(2)
      assert.deepEqual([...state.dstSchemas], ['APP', 'EMPTY_APP'])
      state.srcDatabase = 'SourceDB'
      state.dstSchema = 'APP'
      if ('selected' in state) state.selected = ['Orders']
      if ('objects' in state) state.objects = ['primary_keys']
      check()
      assert.equal(state.unsupportedMsg, '')
      const before = payloads.length
      await start()
      assert.equal(payloads.length, before + 1)
      const payload = payloads.at(-1)
      assert.equal(payload.target_schema, 'APP')
      assert.equal(payload.lower_case_names, false)
      assert.equal(payload.change_owner, false)
      if ('distributed' in payload) assert.equal(payload.distributed, false)
      state.dstSchema = ''
      await start()
      assert.equal(payloads.length, before + 2)
      assert.equal(payloads.at(-1).target_schema, undefined)
    }
    assert.equal(schemaRequests.length, 3)
    assert.equal(payloads[0].char_in_length, true)
  } finally { scope.stop() }
})

test('report export includes Oracle warnings and still accepts historical reports', async () => {
  const blobs = []
  const { descriptor } = parse(readFileSync(new URL('../src/views/MigrationReportPanel.vue', import.meta.url), 'utf8'))
  const panel = execute(`${descriptor.scriptSetup.content}\nexport { report, exportReport };`, name => {
    if (name === '@/utils/autoRepairs') return autoRepairUtils
    if (name === '@/utils/migrationReport') return migrationReportUtils
    if (name === 'vue') return { ...Vue, onMounted() {}, onUnmounted() {} }
    return {}
  }, {
    defineProps: () => ({ jobID: 'oracle-report' }),
    defineEmits: () => () => {},
    defineExpose() {},
    URL: { createObjectURL: blob => { blobs.push(blob); return 'blob:test' }, revokeObjectURL() {} },
    document: { createElement: () => ({ click() {} }) },
  })
  const report = { rowCounts: [] }
  for (const key of ['tables', 'data', 'primaryKeys', 'views', 'indexes', 'constraints', 'sequences', 'comments', 'triggers']) report[key] = { total: 0, success: 0, failed: 0, items: [] }
  panel.report.value = report
  panel.exportReport()
  assert.equal((await blobs[0].text()).includes('迁移告警'), false)
  panel.report.value = { ...report, warnings: [{ table: 'ORDERS', column: 'LABEL', reason: '空字符串转 NULL', count: 3 }] }
  panel.exportReport()
  assert.match(await blobs[1].text(), /ORDERS.LABEL: 空字符串转 NULL（3）/)
})
