import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import * as Vue from 'vue'
import { parse } from '@vue/compiler-sfc'

function load(source, require) {
  const compiled = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } })
  const context = { exports: {}, require, AbortController, FormData, Blob }
  vm.runInNewContext(compiled.outputText, context)
  return context.exports
}
function page(preflight, starts) {
  const helpers = load(readFileSync(new URL('../src/utils/migrationOptions.ts', import.meta.url), 'utf8'), () => {})
  const { descriptor } = parse(readFileSync(new URL('../src/views/MigrationView.vue', import.meta.url), 'utf8'))
  return load(`${descriptor.scriptSetup.content}
    export { connections, dataMigrate, pgChecks, pgExclusions, runPGCheck, excludePGObject, startDataMigration };`, id => {
    switch (id) {
      case 'vue': return { ...Vue, onMounted() {}, onUnmounted() {} }
      case 'vue-router': return { useRoute: () => ({ query: {} }), onBeforeRouteLeave() {} }
      case '@arco-design/web-vue': return { Message: { error: message => assert.fail(message), success() {}, warning() {} }, Modal: {} }
      case '@/utils/migrationOptions': return helpers
      case '@/utils/clipboard': return {}
      case '@/api/schema': return {}
      case '@/api/connections': return {}
      case '@/api/migration': return {
        preflightDataMigration: preflight,
        startDataMigration: async request => { starts.push(request); return { data: { job_id: 'job' } } },
        createDataMigrateEventSource: () => ({ addEventListener() {}, close() {} }),
      }
      default: throw new Error(`Unexpected import ${id}`)
    }
  })
}
async function select(p) {
  p.connections.value = [{ id: 1, db_type: 'gaussdb' }, { id: 2, db_type: 'mysql' }]
  p.dataMigrate.srcConnId = 1
  p.dataMigrate.dstConnId = 2
  p.dataMigrate.srcDatabase = 'public'
  p.dataMigrate.dstSchema = 'target'
  await Vue.nextTick()
}

test('PG/MySQL launch blocks on preflight and carries explicit exclusions into a fresh check and launch', async () => {
  const scope = Vue.effectScope()
  try {
    let ready = false
    const checks = [], starts = []
    const p = scope.run(() => page(async request => { checks.push(request); return { data: { ready, issues: [] } } }, starts))
    await select(p)
    await p.startDataMigration()
    assert.equal(checks.length, 1)
    assert.equal(starts.length, 0)
    const object = { kind: 'index', table: 'Mixed', name: 'PartialIndex' }
    p.excludePGObject('data', object)
    await Vue.nextTick()
    assert.equal(p.pgChecks.data, null)
    ready = true
    await p.startDataMigration()
    assert.equal(checks.length, 2)
    assert.equal(starts.length, 1)
    assert.equal(starts[0].excluded_objects[0].name, 'PartialIndex')
    p.dataMigrate.srcDatabase = 'different_schema'
    await Vue.nextTick()
    assert.equal(p.pgExclusions.data.length, 0)
    assert.equal(p.pgChecks.data, null)
  } finally { scope.stop() }
})

test('changing migration configuration cancels pending checks and discards late results', async () => {
  const scope = Vue.effectScope()
  try {
    let resolve, signal
    const p = scope.run(() => page((_, s) => { signal = s; return new Promise(r => { resolve = r }) }, []))
    await select(p)
    const pending = p.runPGCheck('data')
    p.dataMigrate.lowerCaseNames = !p.dataMigrate.lowerCaseNames
    await Vue.nextTick()
    assert.equal(signal.aborted, true)
    resolve({ data: { ready: true } })
    await pending
    assert.equal(p.pgChecks.data, null)
  } finally { scope.stop() }
})

test('batch request serializes exclusions under the intended Excel row', () => {
  const requests = []
  const api = load(readFileSync(new URL('../src/api/migration.ts', import.meta.url), 'utf8'), () => ({ default: { post: (url, data) => requests.push({ url, data }) } }))
  const exclusions = { 2: [{ kind: 'view', name: 'ComplexView' }] }
  api.startBatch(new Blob(['fixture']), [], { excluded_objects_by_row: exclusions })
  assert.deepEqual(JSON.parse(requests[0].data.get('excluded_objects_by_row')), exclusions)
})
