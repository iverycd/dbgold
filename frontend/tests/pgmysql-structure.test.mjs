import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import * as Vue from 'vue'
import { renderToString } from 'vue/server-renderer'
import { parse, compileScript } from '@vue/compiler-sfc'

const { descriptor } = parse(readFileSync(new URL('../src/views/PGMySQLPreflightPanel.vue', import.meta.url), 'utf8'))
function load(source, globals = {}) {
  const context = {
    exports: {}, ...globals,
    require: id => {
      if (id === 'vue') return Vue
      if (id === '@arco-design/web-vue') return { Message: {} }
      if (id === '@/utils/clipboard') return {}
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
