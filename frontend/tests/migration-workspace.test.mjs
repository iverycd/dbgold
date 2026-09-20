import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import * as Vue from 'vue'
import { parse } from '@vue/compiler-sfc'

function panel(name, props, exports, { api = {}, emit = () => {}, copied = [], lifecycle = {} } = {}) {
  const { descriptor } = parse(readFileSync(new URL(`../src/views/${name}.vue`, import.meta.url), 'utf8'))
  const source = ts.transpileModule(`${descriptor.scriptSetup.content}\nexport { ${exports} };`, {
    compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
  }).outputText
  const context = {
    exports: {},
    defineProps: () => props,
    withDefaults: value => value,
    defineEmits: () => emit,
    defineExpose() {},
    require: id => {
      if (id === 'vue') return { ...Vue, onMounted: fn => { lifecycle.mount = fn }, onUnmounted: fn => { lifecycle.unmount = fn } }
      if (id === '@/api/migration') return api
      if (id === '@/utils/clipboard') return { copyText: async text => copied.push(text) }
      if (id === '@arco-design/web-vue') return { Message: { success() {}, error() {} } }
      throw new Error(`Unexpected import ${id}`)
    },
  }
  vm.runInNewContext(source, context)
  return context.exports
}
function preflight() {
  return {
    ready: true, rows_checked: 100, source: {}, target: {}, excluded: [],
    issues: Array.from({ length: 24 }, (_, i) => ({ object: { kind: 'table', name: `Orders_${i}` }, severity: i === 23 ? 'error' : 'warning' })),
    objects: Array.from({ length: 612 }, (_, i) => ({ object: { kind: 'table', name: `Orders_${i}` }, target: `dst.Orders_${i}`, statements: [{ SQL: `CREATE TABLE Orders_${i} (id INT)` }] })),
  }
}

test('preflight filters search all pages and reset pagination; DDL selection and copying follow the visible object', async () => {
  const scope = Vue.effectScope()
  try {
    const props = Vue.reactive({ result: preflight(), excluded: [] })
    const copied = []
    const p = scope.run(() => panel('PGMySQLPreflightPanel', props,
      'issueSearch, issuePage, severity, filteredIssues, objectSearch, objectPage, pagedObjects, selectedObject, selectedSQL, copySQL', { copied }))
    p.issuePage.value = 3
    p.severity.value = 'error'
    await Vue.nextTick()
    assert.equal(p.issuePage.value, 1)
    assert.equal(p.filteredIssues.value.length, 1)
    p.issueSearch.value = ' ORDERS_23 '
    assert.equal(p.filteredIssues.value[0].object.name, 'Orders_23')
    p.objectPage.value = 62
    await Vue.nextTick()
    assert.equal(p.pagedObjects.value.length, 2)
    assert.equal(p.selectedObject.value.object.name, 'Orders_610')
    p.objectSearch.value = ' dst.ORDERS_611 '
    await Vue.nextTick()
    assert.equal(p.objectPage.value, 1)
    assert.equal(p.selectedObject.value.object.name, 'Orders_611')
    await p.copySQL()
    assert.equal(copied[0], 'CREATE TABLE Orders_611 (id INT);')
    p.objectSearch.value = 'no-match'
    await Vue.nextTick()
    assert.equal(p.selectedObject.value, undefined)
    assert.equal(p.selectedSQL.value, '')
  } finally { scope.stop() }
})

test('invalidating a preflight removes the old DDL and resets the selected results tab', async () => {
  const scope = Vue.effectScope()
  try {
    const props = Vue.reactive({ result: preflight(), excluded: [] })
    const p = scope.run(() => panel('PGMySQLPreflightPanel', props, 'activeTab, objectPage, selectedObject, selectedSQL'))
    p.activeTab.value = 'ddl'
    p.objectPage.value = 20
    await Vue.nextTick()
    props.result = null
    await Vue.nextTick()
    assert.equal(p.activeTab.value, 'issues')
    assert.equal(p.objectPage.value, 1)
    assert.equal(p.selectedObject.value, undefined)
    assert.equal(p.selectedSQL.value, '')
  } finally { scope.stop() }
})

test('changing report jobs discards late responses and never emits loaded for an old or unmounted task', async () => {
  const scope = Vue.effectScope()
  try {
    const props = Vue.reactive({ jobID: 'old-job' })
    const pending = [], emitted = [], lifecycle = {}
    const p = scope.run(() => panel('MigrationReportPanel', props, 'loadReport, report, loading', {
      lifecycle, emit: (...args) => emitted.push(args),
      api: { getDataMigrationReport: id => new Promise(resolve => pending.push({ id, resolve })) },
    }))
    const oldRequest = p.loadReport()
    props.jobID = 'new-job'
    await Vue.nextTick()
    assert.equal(pending[1].id, 'new-job')
    pending[1].resolve({ data: { marker: 'new-report' } })
    await Vue.nextTick()
    pending[0].resolve({ data: { marker: 'old-report' } })
    await oldRequest
    assert.equal(p.report.value.marker, 'new-report')
    assert.deepEqual(emitted, [['loaded', 'new-job', { marker: 'new-report' }]])
    assert.equal(p.loading.value, false)
    const last = p.loadReport()
    lifecycle.unmount()
    pending[2].resolve({ data: { marker: 'unmounted-report' } })
    await last
    assert.equal(emitted.length, 1)
  } finally { scope.stop() }
})

test('report errors and missing reports have distinct states and a successful retry emits loaded', async () => {
  const scope = Vue.effectScope()
  try {
    let attempt = 0
    const emitted = []
    const p = scope.run(() => panel('MigrationReportPanel', { jobID: 'job' }, 'loadReport, fetchError, noReport, report, loading', {
      emit: (...args) => emitted.push(args),
      api: { getDataMigrationReport: async () => {
        attempt++
        if (attempt === 1) throw { response: { status: 500 } }
        if (attempt === 2) throw { response: { status: 404 } }
        return { data: { marker: 'success' } }
      } },
    }))
    await p.loadReport()
    assert.equal(p.fetchError.value, '暂无报告数据')
    assert.equal(p.loading.value, false)
    await p.loadReport()
    assert.equal(p.fetchError.value, '')
    assert.equal(p.noReport.value, true)
    await p.loadReport()
    assert.equal(p.noReport.value, false)
    assert.equal(p.report.value.marker, 'success')
    assert.deepEqual(emitted, [['loaded', 'job', { marker: 'success' }]])
  } finally { scope.stop() }
})

test('report preflight expands failures by default and can focus a saved successful record', async () => {
  const scope = Vue.effectScope()
  try {
    let ready = false
    const p = scope.run(() => panel('MigrationReportPanel', { jobID: 'job' }, 'loadReport, preflightActiveKeys, preflightSection, showPreflight', {
      api: { getDataMigrationReport: async () => ({ data: { preflight: { ready } } }) },
    }))
    await p.loadReport()
    assert.deepEqual(Array.from(p.preflightActiveKeys.value), ['preflight'])
    ready = true
    await p.loadReport()
    assert.equal(p.preflightActiveKeys.value.length, 0)
    const actions = []
    p.preflightSection.value = { focus: () => actions.push('focus'), scrollIntoView: () => actions.push('scroll') }
    await p.showPreflight()
    assert.deepEqual(Array.from(p.preflightActiveKeys.value), ['preflight'])
    assert.deepEqual(actions, ['focus', 'scroll'])
  } finally { scope.stop() }
})
