import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import * as Vue from 'vue'
import { parse } from '@vue/compiler-sfc'
import { connectionDatabaseUtils, connectionDatabaseEditors } from './helpers/connection-database.mjs'
import { migrationReportUtils } from './helpers/migration-report.mjs'

function load(source, require) {
  const { outputText } = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } })
  const context = { exports: {}, require, FormData, Blob }
  vm.runInNewContext(outputText, context)
  return context.exports
}
const file = path => readFileSync(new URL(path, import.meta.url), 'utf8')

test('Oracle to Dameng submits data, views and objects with their selected schemas', async () => {
  const scope = Vue.effectScope()
  const requests = [], lists = []
  const submit = async payload => { requests.push(payload); return { data: { job_id: 'oracle-dameng-test', results: [] } } }
  const options = load(file('../src/utils/migrationOptions.ts'), () => {})
  const { descriptor } = parse(file('../src/views/MigrationView.vue'))
  const p = scope.run(() => load(`${descriptor.scriptSetup.content}\nexport { connections, supportedPairs, dataMigrate, viewMigrate, objMigrate, loadDstSchemas, vmLoadDstSchemas, omLoadDstSchemas, vmLoadViews, omLoadTables, startDataMigration, handleMigrateViews, startObjectMigration, checkPairSupport, vmCheckPairSupport, omCheckPairSupport };`, name => {
    switch (name) {
      case 'vue': return { ...Vue, provide() {}, onMounted() {}, onUnmounted() {} }
      case 'vue-router': return { useRoute: () => ({ query: {} }), onBeforeRouteLeave() {} }
      case '@arco-design/web-vue': return { Message: { error: message => assert.fail(message), success() {}, warning() {} }, Modal: {} }
      case '@/utils/connectionDatabase': return connectionDatabaseUtils
      case '@/composables/connectionDatabaseEditors': return connectionDatabaseEditors
      case '@/utils/migrationOptions': return options
      case '@/utils/migrationReport': return migrationReportUtils
      case '@/api/connections': return {
        listConnectionSchemas: async () => ({ data: ['DST'] }),
        listConnectionViews: async (...args) => { lists.push(args); return { data: ['V_PARENT'] } },
      }
      case '@/api/migration': return {
        startDataMigration: submit, migrateViews: submit, startObjectMigration: submit,
        createDataMigrateEventSource: () => ({ addEventListener() {}, close() {} }),
        listConnectionTables: async (...args) => { lists.push(args); return { data: ['PARENT'] } },
      }
      default: return {}
    }
  }))
  try {
    p.connections.value = [{ id: 1, db_type: 'oracle', database: 'SRC' }, { id: 2, db_type: 'dameng', database: 'DST' }]
    p.supportedPairs.value = [{ source: 'oracle', target: 'dameng' }]
    for (const [state, schemas, check, start] of [
      [p.dataMigrate, p.loadDstSchemas, p.checkPairSupport, p.startDataMigration],
      [p.viewMigrate, p.vmLoadDstSchemas, p.vmCheckPairSupport, p.handleMigrateViews],
      [p.objMigrate, p.omLoadDstSchemas, p.omCheckPairSupport, p.startObjectMigration],
    ]) {
      state.srcConnId = 1
      state.dstConnId = 2
      state.srcDatabase = 'SRC'
      await Vue.nextTick()
      await schemas(2)
      state.dstSchema = 'DST'
      await Vue.nextTick()
      if ('selected' in state) state.selected = ['views' in state ? 'V_PARENT' : 'PARENT']
      if ('objects' in state) state.objects = ['primary_keys', 'indexes', 'sequences', 'foreign_keys', 'comments']
      check()
      assert.equal(state.unsupportedMsg, '')
      const before = requests.length
      await start()
      assert.equal(requests.length, before + 1)
      const payload = requests.at(-1)
      assert.equal(payload.src_conn_id, 1)
      assert.equal(payload.dst_conn_id, 2)
      assert.equal(payload.src_database, 'SRC')
      assert.equal(payload.target_schema, 'DST')
      assert.equal(payload.lower_case_names, false)
      assert.equal(payload.change_owner, false)
    }
    await p.vmLoadViews()
    await p.omLoadTables()
    assert.ok(lists.length >= 2)
    for (const args of lists) {
      assert.equal(args[0], 1)
      assert.equal(args[1], 'SRC')
      assert.equal(args[2], 'dameng')
    }
  } finally { scope.stop() }
})
