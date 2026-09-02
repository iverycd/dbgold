import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import * as Vue from 'vue'
import { parse } from '@vue/compiler-sfc'

const { descriptor } = parse(readFileSync(new URL('../src/views/ConnectionsView.vue', import.meta.url), 'utf8'))
const { outputText } = ts.transpileModule(`${descriptor.scriptSetup.content}
  export { connections, searchKeyword, envFilter, envHistory, filteredConnections,
    page, pagination, handlePageChange, handlePageSizeChange, loadConnections,
    openCreate, openEdit, form, handleSubmit, handleDelete };`, {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
})

function connection(id, overrides = {}) {
  return {
    id, name: `连接-${id}`, db_type: 'mysql', host: 'localhost', port: 3306,
    database: 'test', username: 'root', owner_username: 'admin', env: '测试',
    created_at: '', ...overrides,
  }
}

// Execute the real component state/watchers with mocked APIs, following the
// existing frontend test harness; no backend or additional dependency required.
function setup(t, initialRows = []) {
  let rows = initialRows.map(row => ({ ...row }))
  const calls = []
  const api = {
    async listConnections() { calls.push('list'); return { data: rows.map(row => ({ ...row })) } },
    async createConnection(form) {
      calls.push('create')
      rows.push(connection(Math.max(0, ...rows.map(row => row.id)) + 1, { ...form }))
    },
    async updateConnection(id, form) {
      calls.push('update')
      rows = rows.map(row => row.id === id ? { ...row, ...form } : row)
    },
    async deleteConnection(id) { calls.push('delete'); rows = rows.filter(row => row.id !== id) },
  }
  const context = {
    exports: {},
    require(id) {
      switch (id) {
        case 'vue': return { ...Vue, onMounted() {} }
        case '@arco-design/web-vue': return {
          Message: { error(message) { assert.fail(message) }, success() {} },
        }
        case '@/api/connections': return api
        case '@/utils/dbType': return {}
        case '@/stores/auth': return { useAuthStore: () => ({ user: { role: 'admin' } }) }
        default: throw new Error(`Unexpected import: ${id}`)
      }
    },
  }
  const scope = Vue.effectScope()
  t.after(() => scope.stop())
  scope.run(() => vm.runInNewContext(outputText, context, { filename: 'ConnectionsView.vue' }))
  return { view: context.exports, calls }
}

function ids(view) { return Array.from(view.filteredConnections.value, row => row.id) }

test('search matches only name, host or database, ignoring case and surrounding whitespace', async t => {
  const { view, calls } = setup(t, [
    connection(1, { name: '开发库 MySQL' }),
    connection(2, { host: 'DB.Example.com\\SQL2016' }),
    connection(3, { database: 'Sales_Archive' }),
    connection(4, { name: '', host: null, database: undefined }),
  ])
  await view.loadConnections()
  for (const [keyword, expected] of [
    ['开发', [1]], ['  mYsQl  ', [1]], ['example.COM', [2]], ['\\sql2016', [2]],
    ['sALES_aRCHIVE', [3]], ['unknown', []], ['admin', []], ['3306', []], ['root', []],
    ['', [1, 2, 3, 4]], ['   ', [1, 2, 3, 4]],
  ]) {
    view.searchKeyword.value = keyword
    await Vue.nextTick()
    assert.deepEqual(ids(view), expected, keyword)
    assert.equal(view.pagination.value.total, expected.length)
  }
  assert.deepEqual(calls, ['list'], 'typing must not fetch the list again')
})

test('environment and search are intersected; clearing one preserves the other', async t => {
  const { view } = setup(t, [
    connection(1, { name: '开发库', env: '测试' }),
    connection(2, { name: '开发库', env: '生产' }),
    connection(3, { name: '归档库', env: '测试' }),
  ])
  await view.loadConnections()
  view.searchKeyword.value = '开发'
  view.envFilter.value = '测试'
  await Vue.nextTick()
  assert.deepEqual(ids(view), [1])
  assert.equal(view.envHistory.value.length, 2, 'environment options still include the full list')
  view.searchKeyword.value = ''
  await Vue.nextTick()
  assert.equal(view.envFilter.value, '测试')
  assert.deepEqual(ids(view), [1, 3])
  view.searchKeyword.value = '开发'
  view.envFilter.value = undefined
  await Vue.nextTick()
  assert.deepEqual(ids(view), [1, 2])
})

test('search covers all pages and each filter change resets pagination', async t => {
  const { view } = setup(t, Array.from({ length: 45 }, (_, i) => connection(i + 1)))
  await view.loadConnections()
  view.handlePageChange(2)
  view.searchKeyword.value = '连接-45'
  await Vue.nextTick()
  assert.deepEqual(ids(view), [45])
  assert.equal(view.pagination.value.current, 1)
  assert.equal(view.pagination.value.total, 1)
  view.searchKeyword.value = ''
  await Vue.nextTick()
  assert.equal(view.pagination.value.total, 45)
  view.handlePageChange(3)
  view.envFilter.value = '测试'
  await Vue.nextTick()
  assert.equal(view.page.value, 1)
  view.handlePageChange(3)
  view.envFilter.value = undefined
  await Vue.nextTick()
  assert.equal(view.page.value, 1)
  view.handlePageChange(3)
  view.handlePageSizeChange(50)
  assert.equal(view.page.value, 1)
  assert.equal(view.pagination.value.pageSize, 50)
  view.searchKeyword.value = '不存在'
  await Vue.nextTick()
  assert.equal(view.pagination.value.total, 0)
  assert.equal(view.page.value, 1)
})

test('create, edit and delete retain filters and correct pages after reloading', async t => {
  const { view, calls } = setup(t, Array.from({ length: 21 }, (_, i) => connection(i + 1)))
  await view.loadConnections()
  view.envFilter.value = '测试'
  view.searchKeyword.value = '连接-'
  await Vue.nextTick()
  view.handlePageChange(2)
  view.openEdit(view.connections.value[20])
  view.form.name = '不再匹配'
  await view.handleSubmit(closed => assert.equal(closed, true))
  assert.equal(view.pagination.value.total, 20)
  assert.equal(view.page.value, 1)
  view.openCreate()
  Object.assign(view.form, { name: '连接-新增', env: '测试' })
  await view.handleSubmit(closed => assert.equal(closed, true))
  assert.equal(view.pagination.value.total, 21)
  view.handlePageChange(2)
  await view.handleDelete(22)
  assert.equal(view.pagination.value.total, 20)
  assert.equal(view.page.value, 1)
  assert.equal(view.envFilter.value, '测试')
  assert.equal(view.searchKeyword.value, '连接-')
  assert.deepEqual(calls, ['list', 'update', 'list', 'create', 'list', 'delete', 'list'])
})

test('deleting the last match leaves an empty first page with the search intact', async t => {
  const { view } = setup(t, [connection(1)])
  await view.loadConnections()
  view.searchKeyword.value = '连接-1'
  await Vue.nextTick()
  await view.handleDelete(1)
  assert.equal(view.pagination.value.total, 0)
  assert.equal(view.page.value, 1)
  assert.equal(view.searchKeyword.value, '连接-1')
})
