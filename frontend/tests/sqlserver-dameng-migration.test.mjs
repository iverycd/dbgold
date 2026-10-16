import { connectionDatabaseUtils, connectionDatabaseEditors } from './helpers/connection-database.mjs'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import * as Vue from 'vue'
import { parse } from '@vue/compiler-sfc'

function load(source, require) {
  const { outputText } = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } })
  const context = { exports: {}, require, FormData, Blob }
  vm.runInNewContext(outputText, context)
  return context.exports
}
const file = path => readFileSync(new URL(path, import.meta.url), 'utf8')

test('table/view list API preserves optional destination and database parameters', async () => {
  const requests = []
  const api = { get: async (...args) => { requests.push(args); return { data: [] } } }
  const migration = load(file('../src/api/migration.ts'), () => ({ default: api }))
  const connections = load(file('../src/api/connections.ts'), () => ({ default: api }))
  await migration.listConnectionTables(7, 'SourceDb', 'dameng')
  await connections.listConnectionViews(7, 'SourceDb', 'dameng')
  for (const [url, options] of requests) {
    assert.match(url, /^\/connections\/7\/(tables|views)$/)
    assert.equal(options.params.database, 'SourceDb')
    assert.equal(options.params.target_type, 'dameng')
  }
  await connections.listConnectionViews(7)
  assert.equal(requests.at(-1)[1].params.target_type, undefined)
})

function page(requests, queue) {
  const { descriptor } = parse(file('../src/views/MigrationView.vue'))
  const helper = load(file('../src/utils/migrationOptions.ts'), () => {})
  const list = async (kind, id, db, target) => {
    requests.push({ kind, id, db, target })
    if (queue?.length) return queue.shift()
    return { data: target === 'dameng' ? ['dboOnly'] : ['oldScope'] }
  }
  return load(`${descriptor.scriptSetup.content}\nexport { connections, viewMigrate, objMigrate, vmLoadViews, omLoadTables };`, id => {
    switch (id) {
      case 'vue': return { ...Vue, provide() {}, onMounted() {}, onUnmounted() {} }
      case 'vue-router': return { useRoute: () => ({ query: {} }), onBeforeRouteLeave() {} }
      case '@arco-design/web-vue': return { Message: { error: msg => assert.fail(msg), success() {}, warning() {} }, Modal: {} }
      case '@/utils/connectionDatabase': return connectionDatabaseUtils
      case '@/composables/connectionDatabaseEditors': return connectionDatabaseEditors
      case '@/utils/migrationOptions': return helper
      case '@/utils/clipboard': return {}
      case '@/api/schema': return {}
      case '@/api/connections': return { listConnectionViews: (...args) => list('views', ...args) }
      case '@/api/migration': return { listConnectionTables: (...args) => list('tables', ...args) }
      default: throw new Error(`Unexpected import ${id}`)
    }
  })
}

test('switching SQL Server destination reloads dbo lists and clears selections', async () => {
  const scope = Vue.effectScope()
  try {
    const requests = []
    const p = scope.run(() => page(requests))
    p.connections.value = [{ id: 1, db_type: 'sqlserver' }, { id: 2, db_type: 'dameng' }, { id: 3, db_type: 'postgres' }]
    for (const state of [p.viewMigrate, p.objMigrate]) {
      state.srcConnId = 1
      state.srcDatabase = 'SourceDb'
      state.selected = ['oldSelection']
      state.dstConnId = 2
    }
    await Vue.nextTick()
    await new Promise(resolve => setImmediate(resolve))
    assert.equal(requests.length, 2)
    for (const request of requests) assert.equal(request.target, 'dameng')
    assert.deepEqual(Array.from(p.viewMigrate.views), ['dboOnly'])
    assert.deepEqual(Array.from(p.objMigrate.tables), ['dboOnly'])
    assert.equal(p.viewMigrate.selected.length, 0)
    assert.equal(p.objMigrate.selected.length, 0)
  } finally { scope.stop() }
})

test('older list responses cannot overwrite the current destination scope', async () => {
  const scope = Vue.effectScope()
  try {
    let finishOld
    const old = new Promise(resolve => { finishOld = resolve })
    const p = scope.run(() => page([], [old, Promise.resolve({ data: ['current'] })]))
    p.viewMigrate.srcConnId = 1
    p.viewMigrate.srcDatabase = 'SourceDb'
    const first = p.vmLoadViews()
    await p.vmLoadViews()
    finishOld({ data: ['stale'] })
    await first
    assert.deepEqual(Array.from(p.viewMigrate.views), ['current'])
  } finally { scope.stop() }
})
