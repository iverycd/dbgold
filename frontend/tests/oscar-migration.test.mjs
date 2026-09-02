import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
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
  const context = { exports: {}, require, FormData, Blob }
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
    assert.equal(state.lowerCaseNames, type !== 'dameng' && type !== 'oscar')
    assert.equal(state.distributed, type !== 'oscar')
  }
})

// Run the real form initializers, watchers, schema loaders and submit handlers.
// Stub only external services and lifecycle hooks; no browser or database needed.
function loadMigrationView(payloads) {
  const { descriptor } = parse(readFileSync(new URL('../src/views/MigrationView.vue', import.meta.url), 'utf8'))
  const submit = async payload => {
    payloads.push(payload)
    return { data: { job_id: 'test-job', results: [] } }
  }
  return loadSource(`${descriptor.scriptSetup.content}
    export { connections, dataMigrate, viewMigrate, objMigrate,
      startDataMigration, handleMigrateViews, startObjectMigration,
      loadDstSchemas, vmLoadDstSchemas, omLoadDstSchemas };`, id => {
    switch (id) {
      case 'vue': return { ...Vue, onMounted() {}, onUnmounted() {} }
      case 'vue-router': return { useRoute: () => ({ query: {} }), onBeforeRouteLeave() {} }
      case '@arco-design/web-vue': return {
        Message: { error: message => assert.fail(message), success() {}, warning() {} }, Modal: {},
      }
      case '@/utils/migrationOptions': return { applyTargetMigrationDefaults }
      case '@/utils/clipboard': return { copyText: () => assert.fail('Unexpected clipboard call') }
      case '@/api/schema': return { downloadRoutines: () => assert.fail('Unexpected routine export') }
      case '@/api/connections': return { listConnectionSchemas: async () => ({ data: ['schema_a', 'schema_b'] }) }
      case '@/api/migration': return {
        startDataMigration: submit, migrateViews: submit, startObjectMigration: submit,
        createDataMigrateEventSource: () => ({ addEventListener() {}, close() {} }),
      }
      default: throw new Error(`Unexpected import: ${id}`)
    }
  }, 'MigrationView.vue')
}

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
