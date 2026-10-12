import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import * as Vue from 'vue'
import { parse } from '@vue/compiler-sfc'
import { compile } from '@vue/compiler-dom'
import { renderToString } from '@vue/server-renderer'

const { descriptor } = parse(readFileSync(new URL('../src/views/MigrationReportPanel.vue', import.meta.url), 'utf8'))
const script = ts.transpileModule(`${descriptor.scriptSetup.content}\nexport { report, loading, fetchError, noReport, failedCount, tableRows, mismatchedRows, hasRowCounts, allRowCounts, preflightActiveKeys, copyDDL, exportReport };`, {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
}).outputText
const renderCode = compile(descriptor.template.content, { mode: 'function', prefixIdentifiers: true }).code

function harness() {
  const copied = [], downloads = [], clicks = []
  const context = {
    exports: {}, Blob, Vue,
    defineProps: () => ({ jobID: 'job-manual-ddl' }),
    defineEmits: () => () => {}, defineExpose() {},
    URL: { createObjectURL(blob) { downloads.push(blob); return 'blob:report' }, revokeObjectURL() {} },
    document: { createElement: () => ({ click() {} }) },
    require: id => {
      if (id === 'vue') return { ...Vue, onMounted() {}, onUnmounted() {} }
      if (id === '@/api/migration') return {}
      if (id === '@/utils/clipboard') return { copyText: async value => copied.push(value) }
      if (id === '@arco-design/web-vue') return { Message: { success() {}, error: msg => assert.fail(msg) } }
      throw new Error(`Unexpected import ${id}`)
    },
  }
  vm.runInNewContext(script, context)
  const p = context.exports
  const render = vm.runInNewContext(`(function() { ${renderCode} })()`, { Vue })
  const app = Vue.createSSRApp({ setup: () => ({ ...p, jobID: 'job-manual-ddl' }), render })
  const passthrough = { setup: (_, { slots }) => () => Vue.h('div', slots.default?.()) }
  for (const name of ['a-alert', 'a-tabs', 'a-tab-pane', 'a-tag', 'a-empty', 'a-badge', 'a-collapse', 'a-collapse-item', 'icon-download', 'a-spin', 'a-table-column', 'PGMySQLPreflightPanel']) app.component(name, passthrough)
  app.component('a-button', {
    setup: (_, { slots, attrs }) => () => {
      const children = slots.default?.() ?? []
      const label = children.map(v => typeof v.children === 'string' ? v.children : '').join('')
      clicks.push({ label, handler: attrs.onClick })
      return Vue.h('button', children)
    },
  })
  app.component('a-table', {
    props: ['data', 'expandable'],
    setup: (props, { slots }) => () => Vue.h('div', (props.data ?? []).flatMap(record => {
      if (!slots['expand-row']) return []
      assert.equal(props.expandable.icon(null, record), record.failed > 0 && record.items?.length > 0 ? undefined : null)
      return record.failed > 0 ? slots['expand-row']({ record }) : []
    })),
  })
  return { p, app, copied, downloads, clicks }
}

function report(item) {
  const empty = () => ({ total: 0, success: 0, failed: 0, items: [] })
  return {
    tables: { total: 1, success: 0, failed: 1, items: [item] },
    data: empty(), primaryKeys: empty(), views: empty(), indexes: empty(), constraints: empty(),
    sequences: empty(), comments: empty(), triggers: empty(), rowCounts: [],
  }
}

test('failure details render manual DDL, copy exactly the suggestion, and export both versions', async () => {
  const scope = Vue.effectScope()
  try {
    const h = scope.run(harness)
    const original = 'DROP TABLE "T";\nCREATE TABLE "T" ("ROWID" INT)'
    const suggested = 'CREATE TABLE "T" ("SRC_ROWID" INT);'
    h.p.report.value = report({ name: 'T', ddl: original, error: '达梦禁止的列名: ROWID',
      suggested_ddl: suggested, suggestion: '字段已建议改名；当前迁移工具不会自动应用此映射。',
      column_renames: [{ source: 'RowId', target: 'SRC_ROWID' }],
    })
    const html = await renderToString(h.app)
    assert.match(html, /建议 DDL（手动创建）/)
    assert.match(html, /RowId<\/code> → <code>SRC_ROWID/)
    assert.match(html, /不会自动应用此映射/)
    const copy = h.clicks.find(c => c.label === '复制建议 DDL')
    assert.ok(copy)
    await copy.handler()
    assert.deepEqual(h.copied, [suggested])
    h.p.exportReport()
    const text = await h.downloads[0].text()
    assert.ok(text.includes(original.split('\n')[0]))
    assert.ok(text.includes('建议 DDL（手动创建）：'))
    assert.ok(text.includes(suggested))
    assert.ok(text.includes('建议列名：RowId → SRC_ROWID'))
    assert.ok(text.includes('不会自动应用此映射'))
  } finally { scope.stop() }
})

test('old reports omit the suggestion section and remain exportable', async () => {
  const scope = Vue.effectScope()
  try {
    const h = scope.run(harness)
    h.p.report.value = report({ name: 'T', ddl: 'CREATE TABLE T (id INT)', error: 'old failure' })
    const html = await renderToString(h.app)
    assert.match(html, /old failure/)
    assert.doesNotMatch(html, /建议 DDL|复制建议|column-renames/)
    h.p.exportReport()
    const text = await h.downloads[0].text()
    assert.ok(text.includes('CREATE TABLE T (id INT)'))
    assert.ok(!text.includes('建议 DDL'))
  } finally { scope.stop() }
})

test('incomplete suggestion shows the reason and renames without a copy button', async () => {
  const scope = Vue.effectScope()
  try {
    const h = scope.run(harness)
    h.p.report.value = report({ name: 'T', ddl: '', error: '达梦禁止的列名: ROWID',
      suggestion: '无法生成完整建议 DDL：不支持转换到达梦', column_renames: [{ source: 'ROWID', target: 'SRC_ROWID' }],
    })
    const html = await renderToString(h.app)
    assert.match(html, /无法生成完整建议 DDL/)
    assert.match(html, /SRC_ROWID/)
    assert.ok(!h.clicks.some(c => c.label === '复制建议 DDL'))
  } finally { scope.stop() }
})
