import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import * as Vue from 'vue'
import { renderToString } from 'vue/server-renderer'
import { parse, compileScript } from '@vue/compiler-sfc'
import { migrationReportUtils } from './helpers/migration-report.mjs'

const { descriptor } = parse(readFileSync(new URL('../src/views/PGMySQLPreflightPanel.vue', import.meta.url), 'utf8'))
function load(source, globals = {}) {
  const context = {
    exports: {}, ...globals,
    require: id => {
      if (id === 'vue') return Vue
      if (id === '@arco-design/web-vue') return { Message: {} }
      if (id === '@/utils/clipboard') return {}
      if (id === '@/utils/migrationReport') return migrationReportUtils
      throw new Error(`Unexpected import ${id}`)
    },
  }
  vm.runInNewContext(ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } }).outputText, context)
  return context.exports
}
const component = load(compileScript(descriptor, { id: 'preflight-test', inlineTemplate: true }).content).default
const legacy = { ready: true, rows_checked: 2543, objects: [], issues: [], excluded: [], source: {}, target: {} }
async function render(result, props = {}) {
  const app = Vue.createSSRApp(component, { result, readonly: true, ...props })
  // Render the actual template with lightweight component shells.
  for (const name of ['ASpace', 'AButton', 'AAlert', 'ACollapse', 'ACollapseItem', 'ATabs', 'ATabPane', 'AInput', 'ARadioGroup', 'ARadio', 'ATable', 'ATableColumn', 'AEmpty', 'APagination', 'ATag', 'IconDownload', 'IconSearch', 'IconCopy']) {
    app.component(name, { setup: (_, { slots }) => () => Vue.h('div', slots.default?.()) })
  }
  return renderToString(app)
}

test('shared preflight panel renders Oracle metadata and retains MySQL legacy headings', async () => {
  const result = { ...legacy, kind: 'pg_oracle', check_scope: 'structure', target: { product: 'oracle', version: '11.2.0.4', schema: 'APP', charset: 'AL32UTF8', ncharset: 'AL16UTF16', verification: '待实库验证' } }
  const html = await render(result)
  assert.match(html, /PG → Oracle 兼容检查结果/)
  assert.match(html, /11\.2\.0\.4 \/ APP/)
  assert.match(html, /AL32UTF8 \/ AL16UTF16/)
  assert.match(html, /空字符串、自增列及默认值/)
  assert.doesNotMatch(html, /MySQL|undefined/)
  assert.match(await render(null, { readonly: false, targetType: 'oracle' }), /PG → Oracle 兼容预检/)
  assert.match(await render(legacy), /PG → MySQL 兼容检查结果/)
})

test('Oracle generated trigger failures stay visible and CHECK results have an accurate label', () => {
  const report = {
    pg_oracle_preflight: { ...legacy, kind: 'pg_oracle' }, trigger_migration_enabled: true, rowCounts: [],
    triggers: { total: 1, success: 0, failed: 1, items: [{ name: 'TRG_ORDERS_ID', stage: 'create', ddl: 'CREATE TRIGGER TRG_ORDERS_ID ...', error: '创建失败' }] },
    constraints: { total: 1, success: 1, failed: 0, items: [] },
  }
  const rows = migrationReportUtils.reportCategories(report)
  const trigger = rows.find(row => row.key === 'triggers')
  assert.equal(trigger.label, '自动编号触发器')
  assert.equal(trigger.failed, 1)
  assert.equal(trigger.isInformational, false)
  assert.equal(trigger.items[0].ddl, 'CREATE TRIGGER TRG_ORDERS_ID ...')
  assert.equal(rows.find(row => row.key === 'constraints').label, '外键与 CHECK 约束')
  const exported = migrationReportUtils.formatReportCategories(report).join('\n')
  assert.match(exported, /自动编号触发器\s+总计 1\s+成功 0\s+失败 1/)
  assert.match(exported, /创建失败/)
})

test('structure and legacy report templates distinguish planned objects from checked rows', async () => {
  const oldHTML = await render(legacy)
  assert.match(oldHTML, /已检查行数/)
  assert.match(oldHTML, /2,543/)
  assert.doesNotMatch(oldHTML, /结构预检通过|计划对象数/)
  const result = { ...legacy, check_scope: 'structure', rows_checked: 0, objects: [{ object: { kind: 'table', name: 'T' }, target: 'T', statements: [] }] }
  const newHTML = await render(result)
  assert.match(newHTML, /结构预检通过/)
  assert.match(newHTML, /计划对象数/)
  assert.match(newHTML, /不代表全量数据校验通过/)
  assert.doesNotMatch(newHTML, /已检查行数/)
})

test('export preserves optional structure scope and legacy rows without changing DDL', async () => {
  for (const result of [legacy, { ...legacy, check_scope: 'structure', rows_checked: 0 }]) {
    let blob, clicked = false, revoked = false
    const scope = Vue.effectScope()
    try {
      const module = scope.run(() => load(`${descriptor.scriptSetup.content}\nexport { download };`, {
        defineProps: () => ({ result, excluded: [] }), withDefaults: props => props, defineEmits: () => () => {}, Blob,
        URL: { createObjectURL: value => { blob = value; return 'blob:test' }, revokeObjectURL: () => { revoked = true } },
        document: { createElement: () => ({ click: () => { clicked = true } }) },
      }))
      module.download()
      assert.equal(clicked && revoked, true)
      assert.deepEqual(JSON.parse(await blob.text()), result)
    } finally { scope.stop() }
  }
})

test('compact preview and report summaries show attribution and counts without duplicating full details', async () => {
  const result = { ...legacy, check_scope: 'structure', issues: [{ severity: 'warning', object: { kind: 'table', name: 'T' } }] }
  const preview = await render(result, { compact: true, summarySource: 'page' })
  assert.match(preview, /页面结构预检通过/)
  assert.match(preview, /阻止 0/)
  assert.match(preview, /提示 1/)
  assert.match(preview, /查看执行日志/)
  assert.doesNotMatch(preview, /检查问题|对象映射与 DDL|导出预检与 DDL|重新预检|明确排除|查看预检记录/)
  const blocked = await render({ ...result, ready: false, issues: [{ severity: 'error', object: { kind: 'table', name: 'T' } }] }, { compact: true, summarySource: 'report' })
  assert.match(blocked, /执行前预检未通过/)
  assert.match(blocked, /阻止 1/)
  assert.match(blocked, /查看预检记录/)
  assert.doesNotMatch(blocked, /页面结构预检通过|查看执行日志|检查问题/)
})

test('partial preflight is visible in full and compact panels without marking it fully passed', async () => {
  const result = { ...legacy, ready: false, can_start: true, check_scope: 'structure', issues: [{ severity: 'error', object: { kind: 'table', name: 'Bad' }, reason: 'bad type' }] }
  const full = await render(result)
  assert.match(full, /部分可执行/)
  assert.match(full, /其余表继续迁移/)
  assert.doesNotMatch(full, /结构预检通过/)
  const compact = await render(result, { compact: true, summarySource: 'report' })
  assert.match(compact, /执行前部分可执行/)
  assert.match(compact, /text-warning/)
  const blocked = await render({ ...result, can_start: false, partition_preflight: { can_start: true } })
  assert.match(blocked, /预检未通过/)
  assert.doesNotMatch(blocked, /部分可执行/)
})
