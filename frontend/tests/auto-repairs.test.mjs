import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import * as Vue from 'vue'
import { parse } from '@vue/compiler-sfc'
import { compile } from '@vue/compiler-dom'
import { renderToString } from '@vue/server-renderer'
import { autoRepairUtils } from './helpers/auto-repairs.mjs'
import { migrationReportUtils } from './helpers/migration-report.mjs'

const repairs = [
  { type: 'column_rename', table: 'gj_data', target_table: 'APP.GJ_DATA', source: 'ROWID', target: 'ROW_ID_2', reason: '达梦禁用列名', status: 'applied' },
  { type: 'column_rename', table: 'gj_data', target_table: 'APP.GJ_DATA', source: 'TrxId', target: 'TRX_ID', reason: '达梦禁用列名', status: 'applied' },
  { type: 'column_rename', table: 'Other', target_table: 'APP.OTHER', source: 'ROWID', target: 'ROW_ID', reason: '达梦禁用列名', status: 'reused' },
]

function execute(path, exported, props = {}) {
  const { descriptor } = parse(readFileSync(new URL(`../src/${path}.vue`, import.meta.url), 'utf8'))
  const source = ts.transpileModule(`${descriptor.scriptSetup.content}\nexport { ${exported} };`, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } }).outputText
  const downloads = []
  const context = {
    exports: {}, Blob, defineProps: () => props, defineEmits: () => () => {}, defineExpose() {},
    URL: { createObjectURL: blob => { downloads.push(blob); return 'blob:test' }, revokeObjectURL() {} },
    document: { createElement: () => ({ click() {} }) },
    require: id => {
      if (id === 'vue') return { ...Vue, onMounted() {}, onUnmounted() {} }
      if (id === 'vue-router') return { useRoute: () => ({ params: { jobId: 'job-repairs' } }), useRouter: () => ({}) }
      if (id === '@/utils/autoRepairs') return autoRepairUtils
      if (id === '@/utils/migrationReport') return migrationReportUtils
      if (id === '@/api/migration' || id === '@/utils/clipboard') return {}
      if (id === '@arco-design/web-vue') return { Message: {} }
      throw new Error(`Unexpected import ${id}`)
    },
  }
  vm.runInNewContext(source, context)
  return { values: context.exports, downloads, descriptor }
}

test('repair panel groups by table and renders mappings, reasons and applied/reused status', async () => {
  const scope = Vue.effectScope()
  try {
    const props = Vue.reactive({ repairs })
    const h = scope.run(() => execute('components/AutoRepairPanel', 'groups, autoRepairStatus', props))
    const code = compile(h.descriptor.template.content, { mode: 'function', prefixIdentifiers: true }).code
    const render = vm.runInNewContext(`(function() { ${code} })()`, { Vue })
    const app = Vue.createSSRApp({ setup: () => ({ ...h.values, ...Vue.toRefs(props) }), render })
    app.component('a-alert', { setup: (_, { slots }) => () => Vue.h('div', slots.default?.()) })
    app.component('a-table', { props: ['data'], setup: (p, { slots }) => () => Vue.h('div', p.data.map(record => Vue.h('div', (slots.columns?.() || []).map(column => Vue.h('div', column.children?.cell ? column.children.cell({ record }) : record[column.props?.['data-index']]))))) })
    app.component('a-table-column', { setup: () => () => null })
    const html = await renderToString(app)
    assert.equal(h.values.groups.value.length, 2)
    assert.match(html, /2 张表、3 列/)
    for (const text of ['gj_data', 'ROW_ID_2', 'TRX_ID', '本次已修复', '复用已有映射', '达梦禁用列名']) assert.ok(html.includes(text), text)
    props.repairs = []
    assert.doesNotMatch(await renderToString(app), /自动修复涉及|ROW_ID/)
  } finally { scope.stop() }
})

for (const view of ['MigrationReportPanel', 'DataMigrationJobDetailView']) {
  test(`${view} exports repair records and accepts old reports without them`, async () => {
    const scope = Vue.effectScope()
    try {
      const h = scope.run(() => execute(`views/${view}`, `report, exportReport${view === 'DataMigrationJobDetailView' ? ', job' : ''}`, { jobID: 'job-repairs' }))
      const empty = () => ({ total: 0, success: 0, failed: 0, items: [] })
      const report = { auto_repairs: repairs, rowCounts: [] }
      for (const key of ['tables','data','primaryKeys','indexes','constraints','comments','sequences','views','triggers']) report[key] = empty()
      h.values.report.value = report
      if (h.values.job) h.values.job.value = { job_id: 'job-repairs' }
      h.values.exportReport()
      const text = await h.downloads[0].text()
      assert.match(text, /自动修复（2 张表，3 列）/)
      assert.match(text, /ROWID → ROW_ID_2/)
      assert.match(text, /复用已有映射/)
      assert.match(h.descriptor.template.content, /AutoRepairPanel v-if="report\??\.auto_repairs\?\.length" :repairs="report.auto_repairs"/)
      delete h.values.report.value.auto_repairs
      h.values.exportReport()
      assert.doesNotMatch(await h.downloads[1].text(), /自动修复|ROW_ID/)
    } finally { scope.stop() }
  })
}

for (const view of ['MigrationReportPanel', 'DataMigrationJobDetailView']) {
  test(`${view} exports saved compatibility function results without changing business counts`, async () => {
    const scope = Vue.effectScope()
    try {
      const h = scope.run(() => execute(`views/${view}`, `report, exportReport${view === 'DataMigrationJobDetailView' ? ', job, categoryRows, failedItems' : ', tableRows'}`, { jobID: 'job-repairs' }))
      const empty = () => ({ total: 0, success: 0, failed: 0, items: [] })
      const r = { rowCounts: [], compatibility_functions: { total: 2, success: 1, failed: 1, created: 0, reused: 1, items: [
        { name: 'get_uuid', schema: 'saved_schema', status: 'reused', ddl: 'SAVED DEFINITION', error: '' },
        { name: 'GET_UUID', schema: 'saved_schema', status: 'failed', ddl: 'SAVED FAILED DDL', error: 'permission denied' },
      ] } }
      for (const key of ['tables', 'data', 'primaryKeys', 'views', 'indexes', 'constraints', 'sequences', 'comments', 'triggers']) r[key] = empty()
      h.values.report.value = r
      if (h.values.job) h.values.job.value = { job_id: 'job-repairs' }
      await h.values.exportReport()
      const text = await h.downloads[0].text()
      for (const value of ['saved_schema.get_uuid：复用', 'saved_schema.GET_UUID：失败', 'SAVED DEFINITION', 'SAVED FAILED DDL', 'permission denied']) assert.ok(text.includes(value), value)
      const rows = h.values.categoryRows?.value ?? h.values.tableRows.value
      assert.equal(rows.find(row => row.key === 'compatibility_functions').failed, 1)
      assert.equal(rows.find(row => row.key === 'views').total, 0)
      if (h.values.failedItems) assert.equal(h.values.failedItems.value.length, 0)
    } finally { scope.stop() }
  })
}
