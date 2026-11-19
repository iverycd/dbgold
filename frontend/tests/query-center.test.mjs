import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import * as Vue from 'vue'
import { parse } from '@vue/compiler-sfc'

const { descriptor } = parse(readFileSync(new URL('../src/views/QueryCenterView.vue', import.meta.url), 'utf8'))
const { outputText } = ts.transpileModule(`${descriptor.scriptSetup.content}
  export { connections, connectionsLoaded, currentTab, currentRuntime, namespaces, namespaceStatus, objects, metadataLoading,
    metadataError, columnsLoading, filteredColumns, onConnectionChange, onNamespaceChange, toggleObject,
    lastResult, anyExecuting, runQuery, changeResultPage, pageRows, resultOffset,
    openHistory, history, historyScope, historyPage, historyLoading };`, {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
})

const connections = [
  { id: 1, name: '主库', db_type: 'postgres', database: 'main', env: '测试' },
  { id: 2, name: '归档库', db_type: 'postgres', database: 'archive', env: '生产' },
]
const sharedTable = { name: 'shared_table', type: 'table' }
const queryResult = {
  kind: 'rows', columns: [{ name: 'id', type: 'INT' }], rows: [[1]], row_count: 1,
  truncated: false, duration_ms: 15, audit_id: 1,
}

function deferred() {
  let resolve, reject
  const promise = new Promise((done, fail) => { resolve = done; reject = fail })
  return { promise, resolve, reject }
}

function setup(t, overrides = {}) {
  const api = {
    listQueryNamespaces: async () => ({ data: ['public', 'reporting', 'analytics'] }),
    listQueryObjects: async () => ({ data: [sharedTable] }),
    listQueryColumns: async () => ({ data: [] }),
    executeQuery: async () => ({ data: queryResult }),
    listQueryHistory: async () => ({ data: [] }),
    ...overrides,
  }
  const context = {
    exports: {}, AbortController,
    sessionStorage: { setItem() {} },
    require(id) {
      switch (id) {
        case 'vue': return { ...Vue, onMounted() {}, onBeforeUnmount() {}, onUnmounted() {} }
        case '@arco-design/web-vue': return {
          Message: { error() {}, warning() {} },
        }
        case '@/components/SqlEditor.vue': return {}
        case '@/composables/useQueryWorkspace': return { useQueryWorkspace: () => ({ sidebarOpen: Vue.ref(true) }) }
        case '@/api/connections': return { listConnections: async () => ({ data: connections }) }
        case '@/api/query': return api
        case '@/stores/auth': return { useAuthStore: () => ({ user: { role: 'admin' } }) }
        case '@/utils/clipboard': return { copyText: async () => {} }
        case '@/utils/dbType': return { getDbTypeColor: () => 'blue', getDbTypeLabel: type => type }
        default: throw new Error(`Unexpected import: ${id}`)
      }
    },
  }
  const scope = Vue.effectScope()
  t.after(() => scope.stop())
  scope.run(() => vm.runInNewContext(outputText, context, { filename: 'QueryCenterView.vue' }))
  const view = context.exports
  view.connections.value = connections.map(connection => ({ ...connection }))
  view.connectionsLoaded.value = true
  return view
}

test('out-of-order Schema responses cannot change the newest selection after switching A → B → A', async t => {
  const firstA = deferred(), requestB = deferred(), latestA = deferred()
  const responses = [firstA, requestB, latestA]
  const namespaceCalls = [], objectCalls = [], executeCalls = []
  const view = setup(t, {
    listQueryNamespaces(connectionId) {
      namespaceCalls.push(connectionId)
      return responses.shift().promise
    },
    async listQueryObjects(connectionId, namespace) {
      objectCalls.push([connectionId, namespace])
      return { data: [sharedTable] }
    },
    async executeQuery(payload) {
      executeCalls.push(payload)
      return { data: queryResult }
    },
  })

  const initial = view.onConnectionChange(1)
  const second = view.onConnectionChange(2)
  const latest = view.onConnectionChange(1)
  assert.deepEqual(namespaceCalls, [1, 2, 1])
  view.currentTab.value.sql = 'SELECT 1'
  await view.runQuery(view.currentTab.value.sql)
  assert.equal(executeCalls.length, 0, 'execution waits for the selected connection to resolve its Schema')

  requestB.reject(new Error('stale connection failed'))
  await second
  assert.equal(view.namespaceStatus.value, 'loading')
  assert.equal(view.metadataError.value, '')
  assert.deepEqual(Array.from(view.namespaces.value), [])

  latestA.resolve({ data: ['active_schema'] })
  await latest
  firstA.resolve({ data: ['stale_schema'] })
  await initial
  assert.equal(view.currentTab.value.connectionId, 1)
  assert.equal(view.currentTab.value.namespace, 'active_schema')
  assert.deepEqual(Array.from(view.namespaces.value), ['active_schema'])
  assert.deepEqual(objectCalls, [[1, 'active_schema']])
  assert.equal(view.namespaceStatus.value, 'ready')
  assert.equal(view.metadataLoading.value, false)
})

test('stale object lists and same-name table columns cannot overwrite the active Schema', async t => {
  const reportingObjects = deferred(), analyticsObjects = deferred()
  const analyticsColumns = deferred(), publicColumns = deferred()
  const view = setup(t, {
    listQueryObjects(_connectionId, namespace) {
      if (namespace === 'reporting') return reportingObjects.promise
      if (namespace === 'analytics') return analyticsObjects.promise
      return Promise.resolve({ data: [sharedTable] })
    },
    listQueryColumns(_connectionId, namespace) {
      return (namespace === 'analytics' ? analyticsColumns : publicColumns).promise
    },
  })
  await view.onConnectionChange(1)
  const reporting = view.onNamespaceChange('reporting')
  const analytics = view.onNamespaceChange('analytics')
  reportingObjects.resolve({ data: [{ name: 'stale_table', type: 'table' }] })
  await reporting
  assert.equal(view.metadataLoading.value, true)
  assert.deepEqual(Array.from(view.objects.value), [])
  analyticsObjects.resolve({ data: [sharedTable] })
  await analytics
  assert.equal(view.objects.value[0].name, sharedTable.name)

  const staleColumns = view.toggleObject(view.objects.value[0])
  await view.onNamespaceChange('public')
  const currentColumns = view.toggleObject(view.objects.value[0])
  analyticsColumns.resolve({ data: [{ name: 'stale_field', data_type: 'INT' }] })
  await staleColumns
  assert.deepEqual(Array.from(view.filteredColumns(sharedTable)), [])
  assert.equal(view.columnsLoading.size, 1)
  publicColumns.resolve({ data: [{ name: 'active_field', data_type: 'INT' }] })
  await currentColumns
  assert.deepEqual(Array.from(view.filteredColumns(sharedTable), column => column.name), ['active_field'])
  assert.equal(view.columnsLoading.size, 0)
})

test('query completion is independent of history loading and retains its execution target', async t => {
  const historyResponse = deferred(), executionResponse = deferred()
  const historyCalls = [], executionCalls = []
  const view = setup(t, {
    listQueryHistory(params) {
      historyCalls.push(params.scope)
      return historyResponse.promise
    },
    executeQuery(payload) {
      executionCalls.push(payload)
      return executionResponse.promise
    },
  })
  await view.onConnectionChange(1)
  const historyRequest = view.openHistory()
  view.currentTab.value.sql = 'SELECT 1'
  const execution = view.runQuery(view.currentTab.value.sql)
  assert.equal(view.anyExecuting.value, true)
  await view.onConnectionChange(2)
  await view.onNamespaceChange('reporting')
  executionResponse.resolve({ data: queryResult })
  await new Promise(done => setImmediate(done))
  try {
    assert.deepEqual(historyCalls, ['mine'])
    assert.equal(view.anyExecuting.value, false)
    assert.equal(view.lastResult.value.audit_id, queryResult.audit_id)
    assert.equal(executionCalls[0].connection_id, 1)
    assert.equal(executionCalls[0].namespace, 'public')
    assert.equal(view.currentRuntime.value.source.connectionId, 1)
    assert.equal(view.currentRuntime.value.source.connectionName, '主库')
    assert.equal(view.currentRuntime.value.source.env, '测试')
    assert.equal(view.currentRuntime.value.source.namespace, 'public')
    assert.equal(view.currentTab.value.connectionId, 2)
    assert.equal(view.currentTab.value.namespace, 'reporting')
  } finally {
    historyResponse.resolve({ data: [] })
    await Promise.all([execution, historyRequest])
  }
})

test('results show ten rows per page and a new query resets to the first page', async t => {
  const rows = Array.from({ length: 22 }, (_, index) => [index + 1])
  const view = setup(t, {
    executeQuery: async () => ({ data: { ...queryResult, rows, row_count: rows.length } }),
  })
  await view.onConnectionChange(1)
  view.currentTab.value.sql = 'SELECT id FROM shared_table'
  await view.runQuery(view.currentTab.value.sql)
  assert.equal(view.pageRows.value.length, 10)
  await view.changeResultPage(3)
  assert.equal(view.resultOffset.value, 20)
  assert.deepEqual(Array.from(view.pageRows.value, row => row[0]), [21, 22])
  await view.runQuery(view.currentTab.value.sql)
  assert.equal(view.resultOffset.value, 0)
  assert.deepEqual(Array.from(view.pageRows.value, row => row[0]), rows.slice(0, 10).map(row => row[0]))
})

test('a delayed all-users history response cannot replace the current personal records', async t => {
  const all = deferred(), mine = deferred()
  const view = setup(t, {
    listQueryHistory({ scope }) { return (scope === 'all' ? all : mine).promise },
  })
  view.historyScope.value = 'all'
  const allRequest = view.openHistory()
  view.historyPage.value = 2
  view.historyScope.value = 'mine'
  mine.resolve({ data: [{ id: 3 }] })
  await new Promise(done => setImmediate(done))
  assert.deepEqual(Array.from(view.history.value, record => record.id), [3])
  all.resolve({ data: [{ id: 7 }] })
  await allRequest
  assert.deepEqual(Array.from(view.history.value, record => record.id), [3])
  assert.equal(view.historyPage.value, 1)
  assert.equal(view.historyLoading.value, false)
})

test('an empty audit response leaves an empty history table', async t => {
  const view = setup(t, { listQueryHistory: async () => ({ data: null }) })
  await view.openHistory()
  assert.ok(Array.isArray(view.history.value))
  assert.equal(view.history.value.length, 0)
  assert.equal(view.historyLoading.value, false)
})
