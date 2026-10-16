import { connectionDatabaseUtils, connectionDatabaseEditors } from './helpers/connection-database.mjs'
import assert from 'node:assert/strict'
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

test('SQL Server is selectable in all three forms and submits fixed target defaults', async () => {
  const scope = Vue.effectScope()
  const payloads = [], schemaRequests = []
  const submit = async payload => { payloads.push(payload); return { data: { job_id: 'sqlserver-test', results: [] } } }
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
      case '@/api/connections': return { listConnectionSchemas: async id => { schemaRequests.push(id); return { data: ['dbo', 'APP'] } } }
      case '@/api/migration': return { startDataMigration: submit, migrateViews: submit, startObjectMigration: submit, createDataMigrateEventSource: () => ({ addEventListener() {}, close() {} }) }
      default: return {}
    }
  }))
  try {
    page.connections.value = [{ id: 1, db_type: 'mysql' }, { id: 2, db_type: 'sqlserver' }]
    page.supportedPairs.value = [{ source: 'mysql', target: 'sqlserver' }]
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
      assert.equal(state.lowerCaseNames, true)
      assert.equal(state.changeOwner, false)
      if ('distributed' in state) assert.equal(state.distributed, false)
      await load(2)
      assert.deepEqual([...state.dstSchemas], ['dbo', 'APP'])
      assert.equal(state.dstSchema, 'dbo')
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
      assert.equal(payload.lower_case_names, true)
      assert.equal(payload.change_owner, false)
      if ('distributed' in payload) assert.equal(payload.distributed, false)

    }
    assert.equal(schemaRequests.length, 3)
    assert.equal(payloads[0].char_in_length, false)
  } finally { scope.stop() }
})

