import assert from 'node:assert/strict'
import { autoRepairUtils } from './helpers/auto-repairs.mjs'
import { migrationReportUtils } from './helpers/migration-report.mjs'
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
      if (id === '@/utils/autoRepairs') return autoRepairUtils
      if (id === '@/utils/migrationReport') return migrationReportUtils
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

function failureReport(count) {
  return {
    tables: {
      total: count, success: 0, failed: count,
      items: Array.from({ length: count }, (_, index) => ({ name: `Orders_${index}`, ddl: `CREATE TABLE Orders_${index} (id INT)`, error: `failure_${index}` })),
    },
  }
}

test('failure detail pages cover boundary counts and all 350 objects exactly once', () => {
  const scope = Vue.effectScope()
  try {
    const p = scope.run(() => panel('MigrationReportPanel', { jobID: 'job' },
      'report, tableRows, getFailurePagination, setFailurePage, pagedFailureItems'))
    for (const count of [0, 5, 6, 350]) {
      const original = failureReport(count)
      p.report.value = original
      p.setFailurePage('tables', 1)
      const row = p.tableRows.value.find(row => row.key === 'tables')
      assert.equal(p.getFailurePagination('tables').pageSize, 5)
      assert.equal(p.pagedFailureItems(row).length, Math.min(5, count))
      const visited = []
      for (let page = 1; page <= Math.ceil(count / 5); page++) {
        p.setFailurePage('tables', page)
        const items = p.pagedFailureItems(row)
        assert.equal(items.length, Math.min(5, count - (page - 1) * 5))
        visited.push(...Array.from(items, item => item.name))
      }
      assert.deepEqual(visited, original.tables.items.map(item => item.name))
      assert.equal(new Set(visited).size, count)
      assert.equal(row.items.length, count, 'pagination preserves the complete report category')
    }
    assert.equal(p.getFailurePagination('tables').current, 70)
  } finally { scope.stop() }
})

test('failure detail pagination isolates categories and resets the page when size changes', () => {
  const scope = Vue.effectScope()
  try {
    const p = scope.run(() => panel('MigrationReportPanel', { jobID: 'job' },
      'report, tableRows, getFailurePagination, setFailurePage, setFailurePageSize, pagedFailureItems'))
    const original = failureReport(65)
    original.indexes = { ...original.tables, items: original.tables.items.map(item => ({ ...item, name: `idx_${item.name}` })) }
    p.report.value = original
    const row = key => p.tableRows.value.find(row => row.key === key)
    p.setFailurePage('tables', 3)
    p.setFailurePage('indexes', 2)
    assert.equal(p.pagedFailureItems(row('tables'))[0].name, 'Orders_10')
    assert.equal(p.pagedFailureItems(row('indexes'))[0].name, 'idx_Orders_5')
    p.setFailurePageSize('tables', 20)
    assert.equal(p.getFailurePagination('tables').current, 1)
    assert.equal(p.getFailurePagination('tables').pageSize, 20)
    assert.equal(p.pagedFailureItems(row('tables')).length, 20)
    assert.equal(p.pagedFailureItems(row('tables'))[0].name, 'Orders_0')
    assert.equal(p.getFailurePagination('indexes').current, 2)
    assert.equal(p.getFailurePagination('indexes').pageSize, 5)
    p.setFailurePage('tables', 3)
    assert.equal(p.pagedFailureItems(row('tables'))[0].name, 'Orders_40')
    p.setFailurePageSize('tables', 50)
    assert.equal(p.getFailurePagination('tables').current, 1)
    assert.equal(p.pagedFailureItems(row('tables')).length, 50)
    p.setFailurePage('tables', 2)
    assert.equal(p.pagedFailureItems(row('tables')).length, 15)
    assert.equal(p.pagedFailureItems(row('tables'))[0].name, 'Orders_50')
    assert.equal(original.tables.items.length, 65)
    assert.equal(original.indexes.items.length, 65)
  } finally { scope.stop() }
})

test('reloading a report and switching jobs reset all failure pages and page sizes', async () => {
  const scope = Vue.effectScope()
  try {
    const props = Vue.reactive({ jobID: 'old-job' }), pending = []
    const p = scope.run(() => panel('MigrationReportPanel', props,
      'loadReport, failurePagination, getFailurePagination, setFailurePage, setFailurePageSize', {
        api: { getDataMigrationReport: id => new Promise(resolve => pending.push({ id, resolve })) },
      }))
    p.setFailurePage('tables', 70)
    p.setFailurePageSize('indexes', 50)
    p.setFailurePage('indexes', 3)
    const reload = p.loadReport()
    assert.equal(Object.keys(p.failurePagination.value).length, 0, 'reset immediately when reloading starts')
    assert.equal(p.getFailurePagination('tables').current, 1)
    assert.equal(p.getFailurePagination('indexes').pageSize, 5)
    pending[0].resolve({ data: failureReport(350) })
    await reload
    p.setFailurePageSize('tables', 20)
    p.setFailurePage('tables', 8)
    props.jobID = 'new-job'
    await Vue.nextTick()
    assert.equal(pending[1].id, 'new-job')
    assert.equal(Object.keys(p.failurePagination.value).length, 0)
    assert.equal(p.getFailurePagination('tables').current, 1)
    assert.equal(p.getFailurePagination('tables').pageSize, 5)
    pending[1].resolve({ data: failureReport(6) })
    await Vue.nextTick()
  } finally { scope.stop() }
})

test('row counts put late mismatches on the first page without changing report order', () => {
  const scope = Vue.effectScope()
  try {
    const p = scope.run(() => panel('MigrationReportPanel', { jobID: 'job' },
      'report, allRowCounts, sortedRowCounts, mismatchedRows, hasRowCounts'))
    const rows = Array.from({ length: 65 }, (_, i) => ({
      table: `Orders_${i}`, src: 10, dst: i === 24 || i === 64 ? 9 : 10, match: i !== 24 && i !== 64,
    }))
    const original = structuredClone(rows)
    p.report.value = { rowCounts: rows }
    const names = values => Array.from(values, row => row.table)
    assert.deepEqual(names(p.sortedRowCounts.value.slice(0, 2)), ['Orders_24', 'Orders_64'])
    assert.deepEqual(names(p.sortedRowCounts.value.slice(2)), names(rows.filter(row => row.match)))
    assert.equal(p.sortedRowCounts.value.slice(0, 20).filter(row => !row.match).length, 2)
    assert.deepEqual(names(p.allRowCounts.value), names(original))
    assert.deepEqual(rows, original)
    for (const sample of [rows.slice(0, 2), rows.filter(row => !row.match), []]) {
      p.report.value = { rowCounts: sample }
      assert.deepEqual(names(p.sortedRowCounts.value), names(sample))
      assert.equal(p.mismatchedRows.value.length, sample.filter(row => !row.match).length)
      assert.equal(p.hasRowCounts.value, sample.length > 0)
    }
  } finally { scope.stop() }
})

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

test('changing report jobs resets repair navigation and rejects late summary events and links', async () => {
  const scope = Vue.effectScope()
  try {
    const props = Vue.reactive({ jobID: 'old' }), focused = []
    const p = scope.run(() => panel('MigrationReportPanel', props, 'loadReport, activeTab, repairSummary, updateRepairSummary, goToRepair, repairPanel', {
      api: { getDataMigrationReport: async () => ({ data: {} }) },
    }))
    await p.loadReport()
    p.updateRepairSummary({ jobID: 'old', state: 'ready', repaired: 1, attention: 0 })
    p.repairPanel.value = { focusObject: target => focused.push(target) }
    const navigate = p.goToRepair('indexes', { name: 'OldIndex' })
    props.jobID = 'new'
    await navigate; await Vue.nextTick()
    assert.equal(focused.length, 0)
    assert.equal(p.activeTab.value, 'overview')
    assert.equal(p.repairSummary.value.jobID, 'new')
    assert.equal(p.repairSummary.value.state, 'loading')
    assert.equal(p.repairSummary.value.repaired, 0)
    p.updateRepairSummary({ jobID: 'old', state: 'ready', repaired: 99, attention: 99 })
    assert.equal(p.repairSummary.value.repaired, 0)
  } finally { scope.stop() }
})
