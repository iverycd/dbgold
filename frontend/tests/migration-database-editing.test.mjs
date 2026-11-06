import { migrationReportUtils } from './helpers/migration-report.mjs'
import { partitionPreflightResponse } from './helpers/partition-preflight.mjs'
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
  handleMigrateViews, startObjectMigration, handleExportRoutines, dataExecutionRoute,
  clearMigrationResults, resetDataMigration, resetObjectMigration, migrationContextLocked,
  dataMigrateResultTab, objMigrateResultTab, onPGReportLoaded, connectSSE, omConnectSSE,
  dataMigrationStarting, dataMigrationChecking, pgBusy, nativeCheckBusy, partitionCheckBusy,
  srcEnvFilter, dstEnvFilter };`, {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
}).outputText

function page(overrides = {}) {
  const calls = { starts: [], exports: [], schemas: [], units: [], errors: [], streams: [] }
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
        case '@/utils/migrationReport': return migrationReportUtils
        case '@/utils/migrationOptions': return { applyTargetMigrationDefaults() {} }
        case '@/api/connections': return connectionAPI
        case '@/api/schema': return { downloadRoutines: async (...args) => calls.exports.push(args) }
        case '@/api/migration': return {
          startDataMigration: submit, migrateViews: submit, startObjectMigration: submit,
          listConnectionTables: async () => ({ data: ['Table1'] }),
          preflightDataMigration: async () => partitionPreflightResponse(),
          createDataMigrateEventSource: jobID => {
            const listeners = new Map()
            const stream = {
              jobID, closed: false, onerror: null,
              addEventListener(type, callback) { listeners.set(type, callback) },
              close() { this.closed = true },
              message(data) { listeners.get('message')?.({ data }) },
              error() { this.onerror?.() },
            }
            calls.streams.push(stream)
            return stream
          },
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

function workflowState(p, kind) {
  return kind === 'data' ? p.dataMigrate : kind === 'views' ? p.viewMigrate : p.objMigrate
}

async function completedResults(p, kind) {
  const state = workflowState(p, kind)
  if (kind === 'views') {
    state.results = [{ name: 'OldView' }]
    state.compatibilityFunctions = { failed: 1 }
  } else {
    Object.assign(state, { finished: true, logs: ['old migration log'], currentJobId: `old-${kind}` })
    if (kind === 'data') p.dataExecutionRoute.value = { src: { ...p.connections.value[0] }, dst: { ...p.connections.value[1] }, srcDatabase: 'SourceDb', srcSchema: '', dstSchema: 'public' }
    await flush()
    ;(kind === 'data' ? p.dataMigrateResultTab : p.objMigrateResultTab).value = 'report'
  }
}

function assertResultsCleared(p, kind) {
  const state = workflowState(p, kind)
  if (kind === 'views') {
    assert.equal(state.results.length, 0)
    assert.equal(state.compatibilityFunctions, undefined)
  } else {
    assert.equal(state.finished, false)
    assert.equal(state.logs.length, 0)
    assert.equal(state.currentJobId, '')
    assert.equal((kind === 'data' ? p.dataMigrateResultTab : p.objMigrateResultTab).value, 'logs')
    if (kind === 'data') assert.equal(p.dataExecutionRoute.value, null)
  }
}

function assertResultsPreserved(p, kind) {
  const state = workflowState(p, kind)
  if (kind === 'views') {
    assert.equal(state.results[0].name, 'OldView')
    assert.equal(state.compatibilityFunctions.failed, 1)
  } else {
    assert.equal(state.finished, true)
    assert.deepEqual(Array.from(state.logs), ['old migration log'])
    assert.equal(state.currentJobId, `old-${kind}`)
    assert.equal((kind === 'data' ? p.dataMigrateResultTab : p.objMigrateResultTab).value, 'report')
    if (kind === 'data') assert.equal(p.dataExecutionRoute.value.dst.database, 'TargetDb')
  }
}

test('Oracle source schemas load through the migration-unit API in all workflows without using the service name as a schema', async () => {
  const scope = Vue.effectScope()
  try {
    const requests = []
    const schemas = ['APP', 'MixedCaseSchema', 'EMPTY_APP']
    const { p, calls } = scope.run(() => page({ connections: {
      listConnectionDatabases: async id => { requests.push(id); return { data: schemas } },
    } }))
    Object.assign(p.connections.value[0], { db_type: 'oracle', database: 'ORCLPDB1', username: 'LOGIN_USER' })
    await flush()
    for (const [state, load] of [
      [p.dataMigrate, p.loadSrcDatabases],
      [p.viewMigrate, p.vmLoadSrcDatabases],
      [p.objMigrate, p.omLoadSrcDatabases],
    ]) {
      state.srcDatabase = 'ORCLPDB1'
      await load(1)
      assert.deepEqual(Array.from(state.srcDatabases), schemas)
      assert.equal(state.srcDatabase, '')
      assert.equal(p.connections.value[0].database, 'ORCLPDB1')
    }
    assert.deepEqual(requests, [1, 1, 1])
    assert.equal(calls.schemas.length, 0)
    assert.equal(calls.errors.length, 0)
  } finally { scope.stop() }
})

test('Oracle source schema failures show the backend error and clear previous schema selections in all workflows', async () => {
  const scope = Vue.effectScope()
  try {
    const backendError = '读取 Oracle Schema 列表超时，请检查网络连接和数据库负载'
    const { p, calls } = scope.run(() => page({ connections: {
      listConnectionDatabases: async () => {
        throw { response: { data: { error: backendError } }, message: 'Request failed with status code 504' }
      },
    } }))
    Object.assign(p.connections.value[0], { db_type: 'oracle', database: 'ORCLPDB1' })
    await flush()
    for (const [state, load] of [
      [p.dataMigrate, p.loadSrcDatabases],
      [p.viewMigrate, p.vmLoadSrcDatabases],
      [p.objMigrate, p.omLoadSrcDatabases],
    ]) {
      state.srcDatabase = 'PREVIOUS_APP'
      state.srcDatabases = ['PREVIOUS_APP']
      await load(1)
      assert.equal(state.srcDatabase, '')
      assert.equal(state.srcDatabases.length, 0)
    }
    assert.deepEqual(calls.errors, Array(3).fill(`获取源 Schema 失败: ${backendError}`))
  } finally { scope.stop() }
})

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

test('schema responses from before a database save are ignored', async () => {
  const scope = Vue.effectScope()
  try {
    let finishOldSchema
    const oldSchemas = new Promise(resolve => { finishOldSchema = resolve })
    let schemas = 0
    const { p } = scope.run(() => page({
      connections: { listConnectionSchemas: () => ++schemas === 1 ? oldSchemas : Promise.resolve({ data: ['NewSchema'] }) },
    }))
    await flush()
    const oldRequest = p.loadDstSchemas(2)
    const editor = p.databaseEditors.editorFor(p.connections.value[1])
    editor.setDraft('NewTarget')
    await editor.save()
    await flush()
    finishOldSchema({ data: ['OldSchema'] })
    await oldRequest
    assert.deepEqual(Array.from(p.dataMigrate.dstSchemas), ['NewSchema'])
    assert.equal(p.dataMigrate.dstSchema, '')
    assert.equal(p.pgChecks.data, null)
  } finally { scope.stop() }
})

test('completed report and route summary clear after saving a different database', async () => {
  const scope = Vue.effectScope()
  try {
    const { p, calls } = scope.run(() => page())
    await flush()
    await p.startDataMigration()
    const editor = p.databaseEditors.editorFor(p.connections.value[1])
    editor.setDraft('BlockedWhileRunning')
    assert.equal(editor.dirty, false)
    calls.streams[0].message('[STREAM_END]')
    assert.equal(p.dataMigrate.finished, true)
    editor.setDraft('LaterTarget')
    await editor.save()
    await flush()
    assert.equal(p.connections.value[1].database, 'LaterTarget')
    assertResultsCleared(p, 'data')
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

for (const kind of ['data', 'views', 'objects']) {
  for (const [field, value] of [
    ['srcConnId', 3], ['dstConnId', 4], ['srcDatabase', 'OtherSourceSchema'], ['dstSchema', 'OtherTargetSchema'],
  ]) {
    test(`${kind}: changing ${field} clears completed results`, async () => {
      const scope = Vue.effectScope()
      try {
        const { p } = scope.run(() => page())
        p.connections.value.push(
          { id: 3, db_type: 'mysql', database: 'AnotherSource' },
          { id: 4, db_type: 'postgres', database: 'AnotherTarget' },
        )
        await flush()
        await completedResults(p, kind)
        workflowState(p, kind)[field] = value
        await flush()
        assertResultsCleared(p, kind)
        assert.equal(workflowState(p, kind).running, false)
      } finally { scope.stop() }
    })
  }

  for (const connectionIndex of [0, 1]) {
    test(`${kind}: saving the ${connectionIndex === 0 ? 'source' : 'target'} database clears completed results`, async () => {
      const scope = Vue.effectScope()
      try {
        const { p } = scope.run(() => page())
        await flush()
        await completedResults(p, kind)
        const editor = p.databaseEditors.editorFor(p.connections.value[connectionIndex])
        editor.setDraft(`ChangedDatabase${connectionIndex}`)
        await editor.save()
        await flush()
        assert.equal(p.connections.value[connectionIndex].database, `ChangedDatabase${connectionIndex}`)
        assertResultsCleared(p, kind)
      } finally { scope.stop() }
    })
  }
}

test('drafts, unchanged context, filters, options and unrelated database saves preserve completed results', async () => {
  const scope = Vue.effectScope()
  try {
    const { p } = scope.run(() => page())
    p.connections.value.push({ id: 3, db_type: 'mysql', database: 'UnrelatedDb' })
    await flush()
    for (const kind of ['data', 'views', 'objects']) await completedResults(p, kind)
    const editor = p.databaseEditors.editorFor(p.connections.value[0])
    editor.setDraft('UnsavedDatabase')
    await flush()
    for (const kind of ['data', 'views', 'objects']) assertResultsPreserved(p, kind)
    editor.resetDraft()
    for (const state of [p.dataMigrate, p.viewMigrate, p.objMigrate]) {
      Object.assign(state, { srcConnId: 1, dstConnId: 2, srcDatabase: 'SourceDb', dstSchema: 'public' })
    }
    Object.assign(p.dataMigrate, { mode: 'exclude', filter: 'ExampleTable', pageSize: 1000, maxParallel: 2 })
    Object.assign(p.viewMigrate, { search: 'view', lowerCaseNames: false, selected: ['SomeView'] })
    Object.assign(p.objMigrate, { search: 'table', objects: ['indexes'], selected: ['SomeTable'] })
    p.srcEnvFilter.value = 'production'
    p.dstEnvFilter.value = 'development'
    await flush()
    for (const kind of ['data', 'views', 'objects']) assertResultsPreserved(p, kind)
    const unrelatedEditor = p.databaseEditors.editorFor(p.connections.value[2])
    unrelatedEditor.setDraft('ChangedElsewhere')
    await unrelatedEditor.save()
    await flush()
    for (const kind of ['data', 'views', 'objects']) assertResultsPreserved(p, kind)
  } finally { scope.stop() }
})

test('all migration routes and database editors use the workflow context lock', () => {
  const template = descriptor.template.content
  for (const [kind, state] of [['data', 'dataMigrate'], ['views', 'viewMigrate'], ['objects', 'objMigrate']]) {
    for (const field of ['srcConnId', 'dstConnId', 'srcDatabase', 'dstSchema']) {
      const select = Array.from(template.matchAll(/<a-select\b[\s\S]*?>/g)).map(match => match[0]).find(tag => tag.includes(`v-model="${state}.${field}"`))
      assert.ok(select, `${state}.${field} selector exists`)
      assert.ok(select.includes(`:disabled="migrationContextLocked('${kind}')"`), `${state}.${field} locks during execution and preflight`)
    }
    const selections = kind === 'data' ? ['selectedSrc', 'selectedDst'] : kind === 'views' ? ['vmSelectedSrc', 'vmSelectedDst'] : ['omSelectedSrc', 'omSelectedDst']
    for (const selection of selections) {
      const editor = Array.from(template.matchAll(/<DatabaseNameEditor\b[\s\S]*?\/>/g)).map(match => match[0]).find(tag => tag.includes(`:connection="${selection}"`))
      assert.ok(editor?.includes(`:disabled="migrationContextLocked('${kind}')"`), `${selection} editor locks during execution and preflight`)
    }
  }
})

test('execution and preflight lock each workflow context and shared connection database editors', async () => {
  const scope = Vue.effectScope()
  try {
    const { p } = scope.run(() => page())
    p.connections.value.push({ id: 3, db_type: 'mysql', database: 'UnrelatedDb' })
    await flush()
    const cases = [
      ...['data', 'views', 'objects'].map(kind => ({ kind, set: value => { workflowState(p, kind).running = value } })),
      ...['data', 'views', 'objects'].map(kind => ({ kind, set: value => { p.pgBusy[kind] = value } })),
      ...['dataMigrationStarting', 'dataMigrationChecking', 'nativeCheckBusy', 'partitionCheckBusy'].map(ref => ({ kind: 'data', set: value => { p[ref].value = value } })),
    ]
    for (const { kind, set } of cases) {
      set(true)
      assert.equal(p.migrationContextLocked(kind), true)
      for (const connection of p.connections.value.slice(0, 2)) {
        assert.equal(p.databaseEditors.isLocked(connection.id), true)
        const editor = p.databaseEditors.editorFor(connection)
        editor.setDraft('BlockedDatabase')
        assert.equal(editor.dirty, false)
      }
      assert.equal(p.databaseEditors.isLocked(3), false)
      set(false)
      assert.equal(p.migrationContextLocked(kind), false)
      assert.equal(p.databaseEditors.isLocked(1), false)
      assert.equal(p.databaseEditors.isLocked(2), false)
    }
  } finally { scope.stop() }
})

test('preflight locks database editing and ignores its response after a programmatic context change', async () => {
  const scope = Vue.effectScope()
  try {
    let finishPreflight
    const { p } = scope.run(() => page({ migration: {
      preflightDataMigration: () => new Promise(resolve => { finishPreflight = resolve }),
    } }))
    await flush()
    const request = p.runPGCheck('data')
    const editor = p.databaseEditors.editorFor(p.connections.value[1])
    editor.setDraft('BlockedTarget')
    assert.equal(editor.dirty, false)
    assert.equal(p.migrationContextLocked('data'), true)
    p.dataMigrate.dstSchema = 'ChangedProgrammatically'
    finishPreflight({ data: { ready: true } })
    await request
    await flush()
    assert.equal(p.pgChecks.data, null)
    assert.equal(p.migrationContextLocked('data'), false)
  } finally { scope.stop() }
})

for (const kind of ['data', 'objects']) {
  test(`${kind}: clearing and resetting detach streams and ignore late events and reports`, async () => {
    const scope = Vue.effectScope()
    try {
      const { p, calls } = scope.run(() => page())
      await flush()
      const state = workflowState(p, kind)
      const connect = kind === 'data' ? p.connectSSE : p.omConnectSSE
      const reset = kind === 'data' ? p.resetDataMigration : p.resetObjectMigration
      state.currentJobId = 'old-job'
      connect('old-job')
      const oldStream = calls.streams[0]
      oldStream.message('old live log')
      assert.equal(state.logs.at(-1), 'old live log')
      p.clearMigrationResults(kind)
      assert.equal(oldStream.closed, true)
      oldStream.message('late old log')
      oldStream.message('[STREAM_END]')
      oldStream.error()
      p.onPGReportLoaded(kind, 'old-job', { preflight: { ready: true } })
      await flush()
      assertResultsCleared(p, kind)

      state.currentJobId = 'new-job'
      state.running = true
      connect('new-job')
      const newStream = calls.streams[1]
      oldStream.message('late old log after reconnect')
      oldStream.message('[STREAM_END]')
      oldStream.error()
      assert.equal(state.running, true)
      assert.equal(newStream.closed, false)
      assert.equal(state.logs.length, 0)
      newStream.message('new live log')
      assert.deepEqual(Array.from(state.logs), ['new live log'])
      reset()
      assert.equal(newStream.closed, false, 'manual reset must not detach an active migration')
      assert.equal(state.running, true)
      newStream.message('[STREAM_END]')
      assert.equal(state.finished, true)
      assert.equal(state.running, false)
      reset()
      assert.equal(newStream.closed, true)
      newStream.message('late log after reset')
      newStream.error()
      await flush()
      assertResultsCleared(p, kind)

      if (kind === 'objects') Object.assign(state, { selected: ['Table1'], objects: ['indexes'] })
      await (kind === 'data' ? p.startDataMigration() : p.startObjectMigration())
      assert.equal(calls.starts.length, 1)
      assert.equal(state.currentJobId, 'job')
      const activeStream = calls.streams.at(-1)
      activeStream.message('fresh task log')
      assert.equal(state.logs.at(-1), 'fresh task log')
      activeStream.message('[STREAM_END]')
      assert.equal(state.finished, true)
      assert.equal(state.running, false)
    } finally { scope.stop() }
  })
}
