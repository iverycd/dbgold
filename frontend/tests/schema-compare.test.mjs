import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import * as Vue from 'vue'
import { parse } from '@vue/compiler-sfc'

const { descriptor } = parse(readFileSync(new URL('../src/views/SchemaCompareView.vue', import.meta.url), 'utf8'))
const { outputText } = ts.transpileModule(`${descriptor.scriptSetup.content}
  export { form, plan, planUsed, rows, selectedKeys, missingDependencies, selectedChanges, canPreview,
    previewVisible, applyResults, applyError, loadSetup, selectConnection, runCompare, openPreview,
    selectDependencies, runApply, namespaceOptions, namespaceErrors, sideConnections };`, {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
})

const connections = [
  { id: 1, db_type: 'mysql' }, { id: 2, db_type: 'postgres' }, { id: 3, db_type: 'oracle' },
]

const changes = [
  { id: 'fk', table: 'orders', kind: 'foreign', status: 'applicable', depends_on: ['index'], order: 50, execution_order: 4 },
  { id: 'extra', table: 'archive', kind: 'table_extra', status: 'manual' },
  { id: 'index', table: 'orders', kind: 'index', status: 'applicable', depends_on: ['table'], order: 30, execution_order: 2 },
  { id: 'table', table: 'orders', kind: 'table_missing', status: 'applicable', order: 10, execution_order: 1 },
  { id: 'later-table', table: 'users', kind: 'table_missing', status: 'applicable', order: 10, execution_order: 3 },
]

function setup(t, options = {}) {
  const compareCalls = [], applyCalls = []
  const context = {
    exports: {},
    require(id) {
      switch (id) {
        case 'vue': return { ...Vue, onMounted() {}, onBeforeUnmount() {} }
        case '@/api/connections': return {
          listConnections: async () => ({ data: connections }),
          listConnectionSchemas: options.listConnectionSchemas ?? (async id => ({ data: [id === 1 ? 'Src' : 'Dst'] })),
        }
        case '@/api/schema': return {
          getSchemaComparePairs: async () => ({ data: [{ source: 'mysql', target: 'postgres' }] }),
          compareSchema: async request => {
            compareCalls.push(JSON.parse(JSON.stringify(request)))
            return { data: { plan_id: 'plan', expires_at: new Date(Date.now() + 60_000).toISOString(), changes } }
          },
          applySchemaChanges: async (planID, ids) => {
            applyCalls.push({ planID, ids: Array.from(ids) })
            if (options.applyError) throw options.applyError
            return { data: { results: [{ ...changes[3], status: 'applied' }, { ...changes[2], status: 'failed' }] } }
          },
        }
        case '@/utils/connectionDatabase': return { isPostgresDatabase: type => type === 'postgres' }
        case '@arco-design/web-vue':
        case '@/utils/clipboard':
        case '@/utils/dbType': return {}
        default: throw new Error(`Unexpected import: ${id}`)
      }
    },
  }
  const scope = Vue.effectScope()
  t.after(() => scope.stop())
  scope.run(() => vm.runInNewContext(outputText, context))
  return { view: context.exports, compareCalls, applyCalls }
}

async function compare(view) {
  await view.loadSetup()
  await view.selectConnection('source', 1)
  await view.selectConnection('target', 2)
  view.form.tableInput = 'MixedCaseTable'
  await view.runCompare()
}

test('preview and execution preserve scope and apply only selected changes in dependency order', async t => {
  const { view, compareCalls, applyCalls } = setup(t)
  await compare(view)
  assert.deepEqual(Array.from(view.sideConnections.value.source, connection => connection.id), [1])
  assert.deepEqual(Array.from(view.sideConnections.value.target, connection => connection.id), [2])
  assert.deepEqual(compareCalls, [{
    source: { connection_id: 1, namespace: 'Src' },
    target: { connection_id: 2, namespace: 'Dst' },
    tables: ['MixedCaseTable'], preserve_case: false, char_in_length: false, use_nvarchar2: false,
  }])
  assert.equal(view.rows.value.find(change => change.id === 'extra').disabled, true)
  view.selectedKeys.value = ['fk', 'extra', 'later-table']
  assert.deepEqual(Array.from(view.missingDependencies.value), ['index', 'table'])
  view.openPreview()
  assert.equal(view.previewVisible.value, false)
  view.selectDependencies()
  assert.deepEqual(Array.from(view.selectedChanges.value, change => change.id), ['table', 'index', 'later-table', 'fk'])
  await view.runApply()
  assert.equal(applyCalls.length, 0)
  view.openPreview()
  await view.runApply()
  assert.deepEqual(applyCalls, [{ planID: 'plan', ids: ['table', 'index', 'later-table', 'fk'] }])
  assert.deepEqual(Array.from(view.applyResults.value, result => result.status), ['applied', 'failed'])
  assert.equal(view.canPreview.value, false)
})

test('changing scope clears the comparison and an apply failure requires a new plan', async t => {
  const { view, applyCalls, compareCalls } = setup(t, { applyError: { response: { data: { error: '目标结构已变化，请重新比对' } } } })
  await compare(view)
  view.selectedKeys.value = ['table']
  view.openPreview()
  view.form.target.namespace = 'NextSchema'
  assert.equal(view.plan.value, null)
  assert.equal(view.previewVisible.value, false)
  assert.equal(view.selectedKeys.value.length, 0)
  view.form.tables = []
  await view.runCompare()
  assert.equal('tables' in compareCalls[1], false)
  view.selectedKeys.value = ['table']
  view.openPreview()
  await view.runApply()
  assert.equal(view.planUsed.value, true)
  assert.match(view.applyError.value, /目标结构已变化/)
  view.openPreview()
  await view.runApply()
  assert.equal(applyCalls.length, 1)
})

test('a stale namespace failure cannot overwrite the latest selection when choosing a connection again', async t => {
  let rejectOld
  const old = new Promise((_, reject) => { rejectOld = reject })
  let calls = 0
  const { view } = setup(t, {
    listConnectionSchemas: () => ++calls === 1 ? old : Promise.resolve({ data: ['Current'] }),
  })
  await view.loadSetup()
  const stale = view.selectConnection('source', 1)
  await view.selectConnection('source', undefined)
  await view.selectConnection('source', 1)
  rejectOld(new Error('stale failure'))
  await stale
  assert.equal(view.form.source.namespace, 'Current')
  assert.deepEqual(Array.from(view.namespaceOptions.source), ['Current'])
  assert.equal(view.namespaceErrors.source, '')
})
