import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import * as Vue from 'vue'
import { parse } from '@vue/compiler-sfc'
import { compile } from '@vue/compiler-dom'
import { renderToString } from '@vue/server-renderer'
import { migrationReportUtils as utils } from './helpers/migration-report.mjs'
import { autoRepairUtils } from './helpers/auto-repairs.mjs'
import { connectionDatabaseUtils, connectionDatabaseEditors } from './helpers/connection-database.mjs'

function execute(source, require = () => ({})) {
  const context = { exports: {}, require, FormData, Blob }
  vm.runInNewContext(ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } }).outputText, context)
  return context.exports
}

test('content validation distinguishes disabled, incomplete, differences and full success', () => {
  assert.equal(utils.dataValidationLabel(), '未启用')
  assert.equal(utils.dataValidationNeedsAttention(), false)
  assert.equal(utils.dataValidationLabel(undefined, true), '未完成')
  assert.equal(utils.dataValidationNeedsAttention(undefined, true), true)
  for (const status of ['mismatch', 'error', 'unsupported', 'cancelled', 'incomplete']) {
    assert.equal(utils.dataValidationNeedsAttention({ status, tables: [] }), true)
  }
  const report = { status: 'passed', tables: [{ status: 'passed' }] }
  assert.equal(utils.dataValidationLabel(report), '全量校验通过')
  assert.equal(utils.dataValidationNeedsAttention(report), false)
  report.tables.push({ status: 'error' })
  assert.equal(utils.dataValidationNeedsAttention(report), true)
  assert.notEqual(utils.dataValidationLabel(report), '全量校验通过')
  assert.match(utils.formatDataValidation().join('\n'), /未启用[\s\S]*不代表内容一致/)
})

test('content validation report export preserves null, missing, bounded text and binary summaries', () => {
  const report = { status: 'mismatch', tables: [{ source_table: 'Orders', target_table: 'orders', status: 'mismatch', source_rows: 2, target_rows: 2, missing_rows: 0, extra_rows: 0, changed_rows: 1, duplicate_keys: 0, samples: [{ type: 'changed', key_hash: 'key', key: { id: { text: '1' } }, columns: ['amount'], fields: [{ column: 'amount', source: { text: '199.90' }, target: { text: '199.00' } }] }] }] }
  const text = utils.formatDataValidation(report).join('\n')
  assert.match(text, /Orders → orders：内容不一致/)
  assert.match(text, /amount：源 "199.90" \/ 目标 "199.00"/)
  assert.equal(utils.formatDataValidationValue({ text: '', null: true }), 'NULL')
  assert.equal(utils.formatDataValidationValue(), '不存在')
  assert.equal(utils.formatDataValidationValue({ text: '' }), '""')
  assert.match(utils.formatDataValidationValue({ text: 'abc', truncated: true, length: 1000 }), /已截断.*1000/)
  assert.match(utils.formatDataValidationValue({ text: '', length: 12, sha256: 'digest' }), /12 字节.*digest/)
})

test('single migration opts in explicitly and resets validation for schema-only', async () => {
  const scope = Vue.effectScope()
  const { descriptor } = parse(readFileSync(new URL('../src/views/MigrationView.vue', import.meta.url), 'utf8'))
  const page = scope.run(() => execute(`${descriptor.scriptSetup.content}\nexport { dataMigrate, dataMigrationRequest };`, id => {
    if (id === 'vue') return { ...Vue, provide() {}, onMounted() {}, onUnmounted() {} }
    if (id === 'vue-router') return { useRoute: () => ({ query: {} }), onBeforeRouteLeave() {} }
    if (id === '@/utils/connectionDatabase') return connectionDatabaseUtils
    if (id === '@/composables/connectionDatabaseEditors') return connectionDatabaseEditors
    if (id === '@/utils/migrationOptions') return { applyTargetMigrationDefaults() {} }
    return {}
  }))
  try {
    assert.equal(page.dataMigrate.validateData, false)
    assert.equal(page.dataMigrationRequest().validate_data, false)
    page.dataMigrate.validateData = true
    assert.equal(page.dataMigrationRequest().validate_data, true)
    page.dataMigrate.content = 'schema_only'
    await Vue.nextTick()
    assert.equal(page.dataMigrate.validateData, false)
    page.dataMigrate.content = 'data_only'
    page.dataMigrate.validateData = true
    assert.equal(page.dataMigrationRequest().validate_data, true)
  } finally { scope.stop() }
})

test('batch multipart defaults content validation off and transmits explicit opt-in', async () => {
  const requests = []
  const api = execute(readFileSync(new URL('../src/api/migration.ts', import.meta.url), 'utf8'), () => ({ default: { post: async (_, data) => { requests.push(data); return {} } } }))
  for (const enabled of [undefined, false, true]) {
    await api.startBatch(new Blob(['excel']), [], { migrate_content: 'both', validate_data: enabled })
    assert.equal(requests.at(-1).get('validate_data'), String(!!enabled))
  }
})

function reportView(name) {
  const { descriptor } = parse(readFileSync(new URL(`../src/views/${name}.vue`, import.meta.url), 'utf8'))
  const downloads = []
  const fields = name === 'MigrationReportPanel' ? 'report, exportReport' : 'report, job, reportHealthText, risks, contentValidationLabel, exportReport'
  const context = {
    exports: {}, Blob, defineProps: () => ({ jobID: 'content-report' }), defineEmits: () => () => {}, defineExpose() {},
    URL: { createObjectURL(blob) { downloads.push(blob); return 'blob:report' }, revokeObjectURL() {} },
    document: { createElement: () => ({ click() {} }) },
    require: id => {
      if (id === 'vue') return { ...Vue, onMounted() {}, onUnmounted() {} }
      if (id === 'vue-router') return { useRoute: () => ({ params: { jobId: 'content-report' } }), useRouter: () => ({}) }
      if (id === '@/utils/migrationReport') return utils
      if (id === '@/utils/autoRepairs') return autoRepairUtils
      if (id === '@arco-design/web-vue') return { Message: { error: message => assert.fail(message) } }
      return {}
    },
  }
  vm.runInNewContext(ts.transpileModule(`${descriptor.scriptSetup.content}\nexport { ${fields} };`, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } }).outputText, context)
  return { page: context.exports, downloads }
}

for (const name of ['MigrationReportPanel', 'DataMigrationJobDetailView']) {
  test(`${name} exports saved content results and keeps legacy reports unvalidated`, async () => {
    const scope = Vue.effectScope()
    try {
      const { page, downloads } = scope.run(() => reportView(name))
      const report = { rowCounts: [] }
      for (const key of ['tables', 'data', 'primaryKeys', 'views', 'indexes', 'constraints', 'sequences', 'comments', 'triggers']) report[key] = { total: 0, success: 0, failed: 0, items: [] }
      page.report.value = report
      if (page.job) page.job.value = { job_id: 'content-report', validate_data: false }
      await page.exportReport()
      assert.match(await downloads.at(-1).text(), /数据内容校验[\s\S]*未启用/)
      report.dataValidation = { status: 'mismatch', tables: [] }
      page.report.value = { ...report }
      if (page.job) {
        page.job.value.validate_data = true
        assert.equal(page.reportHealthText.value, '需要处理')
        assert.equal(page.contentValidationLabel.value, '内容不一致')
        assert.ok(page.risks.value.some(risk => risk.key === 'content-validation' && risk.tab === 'validation'))
      }
      await page.exportReport()
      assert.match(await downloads.at(-1).text(), /数据内容校验[\s\S]*内容不一致/)
    } finally { scope.stop() }
  })
}

test('shared content panel renders bounded field values safely and only expands details', async () => {
  const { descriptor } = parse(readFileSync(new URL('../src/components/DataValidationPanel.vue', import.meta.url), 'utf8'))
  const render = vm.runInNewContext(`(function() { ${compile(descriptor.template.content, { mode: 'function', prefixIdentifiers: true }).code} })()`, { Vue })
  const report = { status: 'mismatch', tables: [{ source_table: 'Orders', target_table: 'orders', status: 'mismatch', samples: [{ type: 'changed', key: { id: { text: '1' } }, columns: ['text'], fields: [{ column: 'text', source: { text: '<script>bad()</script>' }, target: { text: '', null: true } }] }] }, { source_table: 'Passed', target_table: 'passed', status: 'passed' }] }
  const props = { report }
  const context = { exports: {}, defineProps: () => props, require: id => id === 'vue' ? Vue : utils }
  vm.runInNewContext(ts.transpileModule(`${descriptor.scriptSetup.content}\nexport { passedTables, sortedTables, differenceLabel };`, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } }).outputText, context)
  const app = Vue.createSSRApp({ setup: () => ({ ...props, ...utils, ...context.exports }), render })
  const passthrough = { setup: (_, { slots }) => () => Vue.h('div', slots.default?.()) }
  for (const name of ['a-alert', 'a-tag', 'a-table-column']) app.component(name, passthrough)
  app.component('a-table', {
    props: ['data', 'expandable'],
    setup: (props, { slots }) => () => Vue.h('div', props.data.flatMap(record => {
      const expected = record.error || record.samples?.length ? undefined : null
      assert.equal(props.expandable.icon(false, record), expected)
      return expected === null ? [] : slots['expand-row']({ record })
    })),
  })
  const html = await renderToString(app)
  assert.match(html, /内容不一致/)
  assert.match(html, /&lt;script&gt;bad\(\)&lt;\/script&gt;/)
  assert.doesNotMatch(html, /<script>/)
  assert.match(html, /目标：NULL/)
  assert.match(html, /id=&quot;1&quot;/)
})
