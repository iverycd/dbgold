import { partitionPreflightResponse } from './helpers/partition-preflight.mjs'
import { connectionDatabaseUtils, connectionDatabaseEditors } from './helpers/connection-database.mjs'
import { migrationReportUtils } from './helpers/migration-report.mjs'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import { isAxiosError } from 'axios'
import * as Vue from 'vue'
import { parse } from '@vue/compiler-sfc'

const { reactive, nextTick, effectScope } = Vue

// Exercise the actual TypeScript helpers/API without a browser or live server.
function loadTS(path, require = () => { throw new Error('Unexpected import') }) {
  const source = readFileSync(new URL(path, import.meta.url), 'utf8')
  return loadSource(source, require, path)
}

function loadSource(source, require, filename) {
  const { outputText } = ts.transpileModule(source, {
    compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
  })
  const context = { exports: {}, require, AbortController, FormData, Blob, performance, window: { addEventListener() {} } }
  vm.runInNewContext(outputText, context, { filename })
  return context.exports
}

const { applyTargetMigrationDefaults, defaultBatchOwnerOptions } = loadTS('../src/utils/migrationOptions.ts')

test('only PG-compatible targets default owner on without changing other options', () => {
  const cases = [
    ...['postgres', 'gaussdb', 'seabox', 'highgo', 'vastbase', 'gbase', 'kingbase'].map(type => [type, true]),
    ...['dameng', 'mysql', 'oscar', 'sqlserver', 'oracle', '', 'unknown', undefined].map(type => [type, false]),
  ]
  for (const [type, expected] of cases) {
    const state = reactive({ lowerCaseNames: true, changeOwner: !expected, distributed: true })
    applyTargetMigrationDefaults(state, type)
    assert.equal(state.changeOwner, expected, String(type))
    assert.equal(state.lowerCaseNames, type !== 'dameng' && type !== 'oscar' && type !== 'oracle')
    assert.equal(state.distributed, type !== 'oscar' && type !== 'oracle' && type !== 'sqlserver')
  }
})

// Run the real form initializers, watchers, schema loaders and submit handlers.
// Stub only external services and lifecycle hooks; no browser or database needed.
function loadMigrationView(payloads, setup = {}) {
  const { descriptor } = parse(readFileSync(new URL('../src/views/MigrationView.vue', import.meta.url), 'utf8'))
  const submit = async payload => {
    payloads.push(payload)
    return { data: { job_id: 'test-job', results: [] } }
  }
  return loadSource(`${descriptor.scriptSetup.content}
    export { connections, srcConnections, dataMigrate, viewMigrate, objMigrate,
      startDataMigration, handleMigrateViews, startObjectMigration,
      checkPairSupport, vmCheckPairSupport, omCheckPairSupport, vmCanMigrate,
      loadSrcDatabases, vmLoadSrcDatabases, omLoadSrcDatabases,
      loadDstSchemas, vmLoadDstSchemas, omLoadDstSchemas };`, id => {
    switch (id) {
      case 'vue': return { ...Vue, provide() {}, onMounted(callback) { setup.onMounted?.(callback) }, onUnmounted() {} }
      case 'vue-router': return { useRoute: () => ({ query: setup.query ?? {} }), onBeforeRouteLeave() {} }
      case '@arco-design/web-vue': return {
        Message: { error: message => assert.fail(message), success() {}, warning() {} }, Modal: {},
      }
      case '@/utils/connectionDatabase': return connectionDatabaseUtils
      case '@/composables/connectionDatabaseEditors': return connectionDatabaseEditors
      case '@/utils/migrationOptions': return { applyTargetMigrationDefaults }
      case '@/utils/migrationReport': return migrationReportUtils
      case '@/utils/clipboard': return { copyText: () => assert.fail('Unexpected clipboard call') }
      case '@/api/schema': return { downloadRoutines: () => assert.fail('Unexpected routine export') }
      case '@/api/connections': return {
        listConnections: async () => ({ data: setup.connections ?? [] }),
        listConnectionSchemas: async () => ({ data: ['schema_a', 'schema_b'] }),
        listConnectionDatabases: async id => {
          setup.schemaRequests?.push(id)
          return { data: ['MixedSchema', 'MIXEDSCHEMA'] }
        },
      }
      case '@/api/migration': return {
        getSupportedPairs: async () => ({ data: setup.supportedPairs ?? [{ source: 'oscar', target: 'mysql' }] }),
        startDataMigration: submit, migrateViews: submit, startObjectMigration: submit,
        preflightDataMigration: async payload => {
          setup.preflightRequests?.push(payload)
          return partitionPreflightResponse()
        },
        createDataMigrateEventSource: () => ({ addEventListener() {}, close() {} }),
      }
      default: throw new Error(`Unexpected import: ${id}`)
    }
  }, 'MigrationView.vue')
}

test('Oscar uses an explicit source schema for MySQL data, view and object migrations', async () => {
  const scope = effectScope()
  try {
    const payloads = [], schemaRequests = [], preflightRequests = []
    const connections = [
      { id: 1, db_type: 'oscar', database: 'OSRDB' },
      { id: 2, db_type: 'mysql', database: 'target_app' },
    ]
    let mount
    const page = scope.run(() => loadMigrationView(payloads, {
      connections, schemaRequests, preflightRequests, query: { src: '1', dst: '2', srcdb: 'OSRDB' },
      onMounted: callback => { mount = callback },
    }))
    await mount()
    assert.equal(page.srcConnections.value.some(conn => conn.db_type === 'oscar'), true)
    assert.equal(page.dataMigrate.srcDatabase, '')
    assert.equal(page.dataMigrate.srcDatabases.includes('OSRDB'), false)
    assert.equal(page.dataMigrate.unsupportedMsg, '')
    for (const [state, loadSource, loadTarget, submit] of [
      [page.dataMigrate, page.loadSrcDatabases, page.loadDstSchemas, page.startDataMigration],
      [page.viewMigrate, page.vmLoadSrcDatabases, page.vmLoadDstSchemas, page.handleMigrateViews],
      [page.objMigrate, page.omLoadSrcDatabases, page.omLoadDstSchemas, page.startObjectMigration],
    ]) {
      state.srcConnId = 1
      state.dstConnId = 2
      await loadSource(1)
      await loadTarget(2)
      assert.deepEqual(Array.from(state.srcDatabases), ['MixedSchema', 'MIXEDSCHEMA'])
      state.srcDatabase = 'MixedSchema'
      state.changeOwner = true
      if ('distributed' in state) state.distributed = true
      if ('selected' in state) state.selected = ['sample']
      if ('objects' in state) state.objects = ['primary_keys']
      await submit()
      assert.equal(payloads.at(-1).src_database, 'MixedSchema')
      assert.equal(payloads.at(-1).target_schema, 'target_app')
      assert.equal(payloads.at(-1).change_owner, false)
      if ('distributed' in state) assert.equal(payloads.at(-1).distributed, false)
    }
    assert.equal(payloads.length, 3)
    assert.deepEqual(schemaRequests, [1, 1, 1, 1])
    assert.equal(preflightRequests.length, 1)
    assert.equal(preflightRequests[0].src_database, 'MixedSchema')
  } finally {
    scope.stop()
  }
})

test('Oscar to Dameng submits data and table objects while disabling standalone views', async () => {
  const scope = effectScope()
  try {
    const payloads = [], preflightRequests = []
    const connections = [
      { id: 1, db_type: 'oscar', database: 'OSRDB' },
      { id: 2, db_type: 'dameng', database: 'DM' },
    ]
    let mount
    const page = scope.run(() => loadMigrationView(payloads, {
      connections, preflightRequests,
      supportedPairs: [{ source: 'oscar', target: 'dameng', supports_view_migration: false }],
      onMounted: callback => { mount = callback },
    }))
    await mount()
    for (const [state, loadSource, loadTarget, check, submit] of [
      [page.dataMigrate, page.loadSrcDatabases, page.loadDstSchemas, page.checkPairSupport, page.startDataMigration],
      [page.objMigrate, page.omLoadSrcDatabases, page.omLoadDstSchemas, page.omCheckPairSupport, page.startObjectMigration],
    ]) {
      state.srcConnId = 1
      state.dstConnId = 2
      await loadSource(1)
      await loadTarget(2)
      assert.deepEqual(Array.from(state.srcDatabases), ['MixedSchema', 'MIXEDSCHEMA'])
      state.srcDatabase = 'MixedSchema'
      state.dstSchema = 'schema_b'
      state.changeOwner = true
      state.distributed = true
      if ('selected' in state) state.selected = ['sample']
      if ('objects' in state) state.objects = ['primary_keys']
      check()
      assert.equal(state.unsupportedMsg, '')
      const before = payloads.length
      await submit()
      assert.equal(payloads.length, before + 1)
      assert.equal(payloads.at(-1).src_database, 'MixedSchema')
      assert.equal(payloads.at(-1).target_schema, 'schema_b')
      assert.equal(payloads.at(-1).change_owner, false)
      assert.equal(payloads.at(-1).distributed, false)
    }
    assert.equal(preflightRequests.length, 1)
    assert.equal(preflightRequests[0].src_database, 'MixedSchema')
    page.viewMigrate.srcConnId = 1
    page.viewMigrate.dstConnId = 2
    page.viewMigrate.srcDatabase = 'MixedSchema'
    page.viewMigrate.dstSchema = 'schema_b'
    page.viewMigrate.selected = ['SampleView']
    page.vmCheckPairSupport()
    assert.match(page.viewMigrate.unsupportedMsg, /oscar → dameng.*视图/)
    assert.equal(page.vmCanMigrate.value, false)
    await page.handleMigrateViews()
    assert.equal(payloads.length, 2)
  } finally {
    scope.stop()
  }
})

test('all three forms apply target defaults and preserve manual choices through schema selection and submission', async () => {
  const scope = effectScope()
  try {
    const payloads = []
    const page = scope.run(() => loadMigrationView(payloads))
    page.connections.value = [{ id: 1, db_type: 'postgres' }, { id: 2, db_type: 'dameng' }, { id: 3, db_type: 'oscar' }]
    for (const [state, loadSchemas, submit] of [
      [page.dataMigrate, page.loadDstSchemas, page.startDataMigration],
      [page.viewMigrate, page.vmLoadDstSchemas, page.handleMigrateViews],
      [page.objMigrate, page.omLoadDstSchemas, page.startObjectMigration],
    ]) {
      assert.equal(state.changeOwner, false)
      state.srcConnId = 10
      if ('selected' in state) state.selected = ['example']
      if ('objects' in state) state.objects = ['primary_keys']
      for (const [targetId, expectedDefault] of [[1, true], [2, false], [1, true], [3, false]]) {
        state.dstConnId = targetId
        await nextTick()
        assert.equal(state.changeOwner, expectedDefault)
        state.changeOwner = !expectedDefault // deliberate opt-out or opt-in
        state.srcConnId += 1
        await loadSchemas(targetId)
        for (const schema of ['schema_a', 'schema_b']) {
          state.dstSchema = schema
          await nextTick()
          assert.equal(state.changeOwner, !expectedDefault)
        }
        const previousCount = payloads.length
        await submit()
        await nextTick()
        assert.equal(payloads.length, previousCount + 1)
        assert.equal(payloads.at(-1).change_owner, !expectedDefault)
        assert.equal(state.changeOwner, !expectedDefault)
      }
      state.dstConnId = undefined
      await nextTick()
      assert.equal(state.changeOwner, false)
    }
  } finally {
    scope.stop()
  }
})

test('mixed batch defaults and multipart payload keep Oscar override independent', () => {
  const defaults = defaultBatchOwnerOptions()
  assert.equal(defaults.change_owner, true)
  assert.equal(defaults.oscar_change_owner, false)
  const posts = []
  const api = { post: (url, form) => posts.push({ url, form }) }
  const { startBatch } = loadTS('../src/api/migration.ts', id => {
    if (id === 'axios') return { isAxiosError }
    assert.equal(id, './index')
    return { default: api }
  })
  const options = {
    ...defaults, migrate_content: 'both', page_size: 1000, max_parallel: 1,
    intra_table_parallel: 1, lower_case_names: true, char_in_length: false,
    use_nvarchar2: false, distributed: false, strip_view_schemas: '',
  }
  startBatch(new Blob(['fixture']), [], options)
  assert.equal(posts[0].form.get('change_owner'), 'true')
  assert.equal(posts[0].form.get('oscar_change_owner'), 'false')
  startBatch(new Blob(['fixture']), [], { ...options, change_owner: false, oscar_change_owner: true })
  assert.equal(posts[1].form.get('change_owner'), 'false')
  assert.equal(posts[1].form.get('oscar_change_owner'), 'true')
  delete options.oscar_change_owner
  startBatch(new Blob(['fixture']), [], options)
  assert.equal(posts[2].form.has('oscar_change_owner'), false)
})
