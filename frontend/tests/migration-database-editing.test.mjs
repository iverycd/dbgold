import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import * as Vue from 'vue'
import { parse } from '@vue/compiler-sfc'
import { connectionDatabaseUtils, loadConnectionDatabaseEditors } from './helpers/connection-database.mjs'

const { descriptor } = parse(readFileSync(new URL('../src/views/MigrationView.vue', import.meta.url), 'utf8'))
const source = ts.transpileModule(`${descriptor.scriptSetup.content}
export { connections, dataMigrate, viewMigrate, objMigrate, databaseEditors, canStartMigration,
  loadSrcDatabases, loadDstSchemas, vmLoadSrcDatabases, vmLoadDstSchemas, omLoadSrcDatabases,
  vmLoadViews, omLoadTables, pgChecks, pgExclusions, runPGCheck, startDataMigration,
  handleMigrateViews, startObjectMigration, handleExportRoutines, dataExecutionRoute };`, {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
}).outputText

function page(overrides = {}) {
  const calls = { starts: [], exports: [], schemas: [], units: [], errors: [] }
  let p
  const editors = loadConnectionDatabaseEditors({
    updateConnectionDatabase: async (id, database) => ({ data: { ...p.connections.value.find(c => c.id === id), database } }),
  })
  const connectionAPI = {
    listConnectionSchemas: async id => { calls.schemas.push(id); return { data: ['dbo', 'public'] } },
    listConnectionDatabases: async id => { calls.units.push(id); return { data: ['AppSchema'] } },
    listConnectionViews: async () => ({ data: ['View1'] }),
    ...overrides.connections,
  }
  const submit = async req => { calls.starts.push(JSON.parse(JSON.stringify(req))); return { data: { job_id: 'job', results: [] } } }
  const context = {
    exports: {}, AbortController,
    require(id) {
      switch (id) {
        case 'vue': return { ...Vue, provide() {}, onMounted() {}, onUnmounted() {} }
        case 'vue-router': return { useRoute: () => ({ query: {} }), onBeforeRouteLeave() {} }
        case '@arco-design/web-vue': return { Message: { error: msg => calls.errors.push(msg), success() {}, warning() {} } }
        case '@/utils/connectionDatabase': return connectionDatabaseUtils
        case '@/composables/connectionDatabaseEditors': return editors
        case '@/utils/migrationOptions': return { applyTargetMigrationDefaults() {} }
        case '@/api/connections': return connectionAPI
        case '@/api/schema': return { downloadRoutines: async (...args) => calls.exports.push(args) }
        case '@/api/migration': return {
          startDataMigration: submit, migrateViews: submit, startObjectMigration: submit,
          listConnectionTables: async () => ({ data: ['Table1'] }),
          preflightDataMigration: async () => ({ data: { ready: true } }),
          createDataMigrateEventSource: () => ({ addEventListener() {}, close() {} }),
          ...overrides.migration,
        }
        default: return {}
      }
    },
  }
  vm.runInNewContext(source, context)
  p = context.exports
  p.connections.value = [
    { id: 1, db_type: 'mysql', database: 'SourceDb', host: 'source', name: 'source', port: 3306 },
    { id: 2, db_type: 'postgres', database: 'TargetDb', host: 'target', name: 'target', port: 5432 },
  ]
  for (const state of [p.dataMigrate, p.viewMigrate, p.objMigrate]) {
    Object.assign(state, { srcConnId: 1, dstConnId: 2, srcDatabase: 'SourceDb', dstSchema: 'public' })
  }
  return { p, calls }
}
const flush = async () => { await Vue.nextTick(); await Promise.resolve(); await Vue.nextTick() }

test('saved source database synchronizes all workflows and discards old selections and preflight', async () => {
  const scope = Vue.effectScope()
  try {
    const { p, calls } = scope.run(() => page())
    await flush()
    assert.equal(p.canStartMigration.value, true) // evaluate before the editor mounts
    const editor = p.databaseEditors.editorFor(p.connections.value[0])
    p.viewMigrate.selected = ['OldView']
    p.objMigrate.selected = ['OldTable']
    p.objMigrate.manualTables = 'OldManual'
    p.pgChecks.data = { ready: true }
    editor.setDraft('MixedCaseDb')
    assert.equal(p.canStartMigration.value, false)
    await p.startDataMigration()
    await p.handleMigrateViews()
    await p.startObjectMigration()
    await p.handleExportRoutines()
    await p.runPGCheck('data')
    assert.equal(calls.starts.length, 0)
    assert.equal(calls.exports.length, 0)
    assert.equal(p.pgChecks.data, null)
    await editor.save()
    await flush()
    for (const state of [p.dataMigrate, p.viewMigrate, p.objMigrate]) assert.equal(state.srcDatabase, 'MixedCaseDb')
    assert.equal(p.viewMigrate.selected.length, 0)
    assert.equal(p.objMigrate.selected.length, 0)
    assert.equal(p.objMigrate.manualTables, '')
    assert.deepEqual(Array.from(p.viewMigrate.views), ['View1'])
    assert.deepEqual(Array.from(p.objMigrate.tables), ['Table1'])
    assert.equal(calls.units.length, 0) // real databases do not reload a second migration-unit selector
    await p.handleExportRoutines()
    assert.deepEqual(calls.exports[0], [1, 'MixedCaseDb'])
  } finally { scope.stop() }
})

test('saving a MySQL connection used at both ends updates source and target database without duplicate schema selection', async () => {
  const scope = Vue.effectScope()
  try {
    const { p, calls } = scope.run(() => page())
    p.dataMigrate.dstConnId = 1
    await flush()
    const editor = p.databaseEditors.editorFor(p.connections.value[0])
    editor.setDraft('OtherDb')
    await editor.save()
    await flush()
    assert.equal(p.dataMigrate.srcDatabase, 'OtherDb')
    assert.equal(p.dataMigrate.dstSchema, 'OtherDb')
    assert.equal(calls.schemas.length, 0)
    await p.startDataMigration()
    assert.equal(calls.starts[0].src_database, 'OtherDb')
    assert.equal(calls.starts[0].target_schema, 'OtherDb')
  } finally { scope.stop() }
})

test('PostgreSQL database saves clear the source schema and reject object lists from the previous database', async () => {
  const scope = Vue.effectScope()
  try {
    let finishOld
    const old = new Promise(resolve => { finishOld = resolve })
    const { p } = scope.run(() => page({ connections: { listConnectionViews: () => old } }))
    p.connections.value[0].db_type = 'postgres'
    p.viewMigrate.srcDatabase = 'OldSchema'
    await flush()
    const request = p.vmLoadViews()
    const editor = p.databaseEditors.editorFor(p.connections.value[0])
    editor.setDraft('NewCatalog')
    await editor.save()
    await flush()
    assert.equal(p.viewMigrate.srcDatabase, '')
    assert.deepEqual(Array.from(p.viewMigrate.srcDatabases), ['AppSchema'])
    finishOld({ data: ['WrongView'] })
    await request
    assert.equal(p.viewMigrate.views.length, 0)
    assert.equal(p.connections.value[0].database, 'NewCatalog')
  } finally { scope.stop() }
})

test('changing only the destination schema does not discard a valid source object list', async () => {
  const scope = Vue.effectScope()
  try {
    let finish
    const pending = new Promise(resolve => { finish = resolve })
    const { p } = scope.run(() => page({ connections: { listConnectionViews: () => pending } }))
    await flush()
    const request = p.vmLoadViews()
    p.viewMigrate.dstSchema = 'AnotherSchema'
    finish({ data: ['ValidView'] })
    await request
    assert.deepEqual(Array.from(p.viewMigrate.views), ['ValidView'])
  } finally { scope.stop() }
})

test('schema responses and preflight responses from before a database save are ignored', async () => {
  const scope = Vue.effectScope()
  try {
    let finishOldSchema, finishPreflight
    const oldSchemas = new Promise(resolve => { finishOldSchema = resolve })
    let schemas = 0
    const { p } = scope.run(() => page({
      connections: { listConnectionSchemas: () => ++schemas === 1 ? oldSchemas : Promise.resolve({ data: ['NewSchema'] }) },
      migration: { preflightDataMigration: () => new Promise(resolve => { finishPreflight = resolve }) },
    }))
    await flush()
    const oldRequest = p.loadDstSchemas(2)
    const check = p.runPGCheck('data')
    const editor = p.databaseEditors.editorFor(p.connections.value[1])
    editor.setDraft('NewTarget')
    await editor.save()
    await flush()
    finishOldSchema({ data: ['OldSchema'] })
    finishPreflight({ data: { ready: true } })
    await Promise.all([oldRequest, check])
    assert.deepEqual(Array.from(p.dataMigrate.dstSchemas), ['NewSchema'])
    assert.equal(p.dataMigrate.dstSchema, '')
    assert.equal(p.pgChecks.data, null)
  } finally { scope.stop() }
})

test('completed report connection summary stays frozen after saving a different database', async () => {
  const scope = Vue.effectScope()
  try {
    const { p } = scope.run(() => page())
    await flush()
    await p.startDataMigration()
    const editor = p.databaseEditors.editorFor(p.connections.value[1])
    editor.setDraft('BlockedWhileRunning')
    assert.equal(editor.dirty, false)
    p.dataMigrate.running = false
    editor.setDraft('LaterTarget')
    await editor.save()
    await flush()
    assert.equal(p.connections.value[1].database, 'LaterTarget')
    assert.equal(p.dataExecutionRoute.value.dst.database, 'TargetDb')
    assert.equal(p.dataExecutionRoute.value.dstSchema, 'public')
  } finally { scope.stop() }
})

test('saving an unrelated connection preserves the current workflow preflight', async () => {
  const scope = Vue.effectScope()
  try {
    const { p } = scope.run(() => page())
    const other = { id: 3, db_type: 'mysql', database: 'UnrelatedDb' }
    p.connections.value.push(other)
    await flush()
    p.pgChecks.data = { ready: true, marker: 'keep' }
    const editor = p.databaseEditors.editorFor(other)
    editor.setDraft('ChangedElsewhere')
    await editor.save()
    await flush()
    assert.equal(p.pgChecks.data.marker, 'keep')
  } finally { scope.stop() }
})
