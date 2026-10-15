import assert from 'node:assert/strict'
import { autoRepairUtils } from './helpers/auto-repairs.mjs'
import { migrationReportUtils } from './helpers/migration-report.mjs'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import * as Vue from 'vue'
import { parse } from '@vue/compiler-sfc'
import { compile } from '@vue/compiler-dom'
import { renderToString } from '@vue/server-renderer'
import Arco from '@arco-design/web-vue'
import { createCompatibilityPanel } from './helpers/compatibility-functions.mjs'

const { descriptor } = parse(readFileSync(new URL('../src/views/MigrationReportPanel.vue', import.meta.url), 'utf8'))
const script = ts.transpileModule(`${descriptor.scriptSetup.content}\nexport { report, loading, fetchError, noReport, failedCount, tableRows, mismatchedRows, hasRowCounts, allRowCounts, sortedRowCounts, preflightActiveKeys, copyDDL, exportReport, activeTab, repairSummary, updateRepairSummary, goToRepair, repairPanel };`, {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
}).outputText
const renderCode = compile(descriptor.template.content, { mode: 'function', prefixIdentifiers: true }).code

function harness() {
  const copied = [], downloads = [], clicks = [], tabStates = [], repairInstances = []
  const context = {
    exports: {}, Blob, Vue,
    defineProps: () => ({ jobID: 'job-manual-ddl' }),
    defineEmits: () => () => {}, defineExpose() {},
    URL: { createObjectURL(blob) { downloads.push(blob); return 'blob:report' }, revokeObjectURL() {} },
    document: { createElement: () => ({ click() {} }) },
    require: id => {
      if (id === 'vue') return { ...Vue, onMounted() {}, onUnmounted() {} }
      if (id === '@/api/migration') return {}
      if (id === '@/utils/autoRepairs') return autoRepairUtils
      if (id === '@/utils/migrationReport') return migrationReportUtils
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
  for (const name of ['a-alert', 'a-tag', 'a-empty', 'a-badge', 'a-collapse', 'a-collapse-item', 'icon-download', 'a-spin', 'a-table-column', 'PGMySQLPreflightPanel', 'AutoRepairPanel']) app.component(name, passthrough)
  app.component('a-tabs', {
    props: ['activeKey', 'lazyLoad', 'destroyOnHide'],
    setup: (props, { slots }) => () => {
      const panes = slots.default()
      tabStates.push({ ...props, keys: panes.filter(pane => pane.type !== Vue.Comment).map(pane => pane.key), panes })
      return Vue.h(Arco.Tabs, props, { default: () => panes })
    },
  })
  app.component('a-tab-pane', Arco.TabPane)
  app.component('CompatibilityFunctionsPanel', createCompatibilityPanel(copied))
  app.component('a-space', passthrough)
  app.component('MigrationRepairPanel', { props: ['jobID'], setup: props => {
    repairInstances.push(props.jobID)
    return () => Vue.h('div', '修复操作面板')
  } })
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
      assert.equal(props.expandable.icon(null, record), (record.isCompatibility || record.failed > 0) && record.items?.length > 0 ? undefined : null)
      return (record.isCompatibility || record.failed > 0) ? slots['expand-row']({ record }) : []
    })),
  })
  return { p, app, copied, downloads, clicks, tabStates, repairInstances }
}

function report(item, category = 'tables') {
  const empty = () => ({ total: 0, success: 0, failed: 0, items: [] })
  const result = {
    tables: empty(),
    data: empty(), primaryKeys: empty(), views: empty(), indexes: empty(), constraints: empty(),
    sequences: empty(), comments: empty(), triggers: empty(), rowCounts: [],
  }
  result[category] = { total: 1, success: 0, failed: 1, items: [item] }
  return result
}

function memoryRenderer() {
  const element = tag => ({ tag, children: [], style: {}, offsetWidth: 0, offsetHeight: 0, offsetTop: 0, offsetLeft: 0 })
  const renderer = Vue.createRenderer({
    createElement: element, createText: text => ({ text }), createComment: text => ({ text }),
    setText: (node, text) => { node.text = text }, setElementText: (node, text) => { node.text = text },
    patchProp: (node, key, previous, value) => { node[key] = value },
    insert(node, parent, anchor) {
      if (node.parent) node.parent.children.splice(node.parent.children.indexOf(node), 1)
      node.parent = parent
      const index = parent.children.indexOf(anchor)
      parent.children.splice(index < 0 ? parent.children.length : index, 0, node)
    },
    remove: node => { node.parent?.children.splice(node.parent.children.indexOf(node), 1) },
    parentNode: node => node.parent,
    nextSibling: node => node.parent?.children[node.parent.children.indexOf(node) + 1],
  })
  return { renderer, root: element('root') }
}

test('mounted Arco tabs retain repair execution state and export history before opening repair', async () => {
  const scope = Vue.effectScope()
  let app
  try {
    const h = scope.run(harness), lifecycle = [], focused = []
    h.p.report.value = report({ name: 'Orders', ddl: '', error: 'original error' })
    const host = memoryRenderer()
    app = host.renderer.createApp(h.app._component)
    for (const [name, component] of Object.entries(h.app._context.components)) {
      if (name !== 'MigrationRepairPanel') app.component(name, component)
    }
    let complete
    app.component('MigrationRepairPanel', {
      props: ['jobID'], emits: ['summary-change'],
      setup(props, { expose, emit }) {
        const status = Vue.ref('running')
        Vue.onMounted(() => lifecycle.push('mounted'))
        Vue.onUnmounted(() => lifecycle.push('unmounted'))
        complete = () => { status.value = 'succeeded'; emit('summary-change', { jobID: props.jobID, state: 'ready', repaired: 1, attention: 0 }) }
        expose({ exportHistory: async () => [`修复执行状态：${status.value}`], focusObject: target => focused.push(target) })
        return () => Vue.h('div', status.value)
      },
    })
    app.mount(host.root); await Vue.nextTick()
    const repairInstance = h.p.repairPanel.value
    assert.ok(repairInstance, 'hidden repair tab exposes its history immediately')
    await h.p.exportReport()
    assert.match(await h.downloads[0].text(), /修复执行状态：running/)
    await h.p.goToRepair('tables', { name: 'Orders' })
    assert.equal(focused[0].name, 'Orders')
    h.p.activeTab.value = 'overview'; await Vue.nextTick()
    assert.equal(h.p.repairPanel.value, repairInstance)
    assert.deepEqual(lifecycle, ['mounted'])
    complete(); await Vue.nextTick()
    assert.equal(h.p.repairSummary.value.repaired, 1, 'hidden repair still updates the report')
    await h.p.exportReport()
    assert.match(await h.downloads[1].text(), /修复执行状态：succeeded/)
    app.unmount(); app = null
    assert.deepEqual(lifecycle, ['mounted', 'unmounted'])
  } finally { app?.unmount(); scope.stop() }
})

test('report renders overview first and mounts repair in an inactive persistent tab', async () => {
  const scope = Vue.effectScope()
  try {
    const h = scope.run(harness)
    h.p.report.value = report({ name: 'Orders', ddl: '', error: 'original error' })
    h.p.report.value.rowCounts = [{ table: 'Orders', src: 1, dst: 1, match: true }]
    h.p.report.value.warnings = [{ table: 'Orders', column: 'Name', reason: 'warning', count: 1 }]
    const html = await renderToString(h.app)
    assert.equal(h.p.activeTab.value, 'overview')
    assert.deepEqual(Array.from(h.tabStates[0].keys), ['overview', 'rowcount', 'repair', 'warnings'])
    assert.equal(h.tabStates[0].lazyLoad, false)
    assert.equal(h.tabStates[0].destroyOnHide, false)
    assert.deepEqual(h.repairInstances, ['job-manual-ddl'], 'repair loads before its tab is opened')
    assert.match(html, /原始失败项/)
    assert.match(html, /class="arco-tabs-content-item arco-tabs-content-item-active"[^]*original error/)
    assert.match(html, /class="arco-tabs-content-item"><div class="arco-tabs-pane"><!--\[-->[^]*修复操作面板/)
    const focused = []
    h.p.repairPanel.value = { focusObject: target => focused.push(target), exportHistory: async () => ['修复导出记录'] }
    await h.p.exportReport()
    assert.match(await h.downloads[0].text(), /修复导出记录/)
    assert.equal(h.p.activeTab.value, 'overview', 'export does not open the repair tab')
    const link = h.clicks.find(button => button.label === '去修复')
    assert.ok(link)
    await link.handler()
    assert.equal(h.p.activeTab.value, 'repair')
    assert.equal(focused[0].name, 'Orders')
    assert.equal(focused[0].kind, 'tables')
  } finally { scope.stop() }
})

test('report summary and repair badge distinguish loaded, failed and completed repair state', async () => {
  const scope = Vue.effectScope()
  try {
    const h = scope.run(harness)
    h.p.report.value = report({ name: 'Orders', ddl: '', error: 'original error' })
    h.p.updateRepairSummary({ jobID: 'job-manual-ddl', state: 'ready', repaired: 1, attention: 2 })
    let html = await renderToString(h.app)
    assert.match(html, /原始失败 1 项，[^]*已修复 1 项，[^]*当前待处理 2 个对象/)
    const titleBadge = () => h.tabStates.at(-1).panes.find(pane => pane.key === 'repair').children.title().find(vnode => vnode.props?.count)
    assert.equal(titleBadge().props.count, 2)
    h.p.updateRepairSummary({ jobID: 'job-manual-ddl', state: 'unknown', repaired: 1, attention: 0 })
    html = await renderToString(h.app)
    assert.match(html, /修复状态未知/)
    assert.doesNotMatch(html, /当前无待处理对象/)
    assert.equal(titleBadge(), undefined)
    h.p.updateRepairSummary({ jobID: 'job-manual-ddl', state: 'ready', repaired: 1, attention: 0 })
    html = await renderToString(h.app)
    assert.match(html, /已修复 1 项，[^]*当前无待处理对象/)
    assert.equal(titleBadge(), undefined)
    assert.equal(h.p.failedCount.value, 1, 'original report failure is retained')
  } finally { scope.stop() }
})

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

test('data failures display and export multiline column diagnostics without an empty DDL section', async () => {
  const scope = Vue.effectScope()
  try {
    const h = scope.run(harness)
    const error = 'pq: numeric field overflow\nSQLSTATE: 22003\nDetail: precision 8, scale 2\nWhere: COPY cg_purchasebudgetandou, line 42, column Amount: "1000000"'
    h.p.report.value = report({ name: 'cg_purchasebudgetandou', ddl: '', error }, 'data')
    const html = await renderToString(h.app)
    assert.match(html, /失败阶段：数据迁移失败/)
    assert.match(html, /numeric field overflow\nSQLSTATE: 22003\nDetail: precision 8, scale 2\nWhere: COPY cg_purchasebudgetandou, line 42, column Amount/)
    assert.doesNotMatch(html, /目标 DDL：|失败阶段未记录/)
    assert.ok(!h.clicks.some(c => c.label === '复制 DDL'))
    h.p.exportReport()
    const text = await h.downloads[0].text()
    assert.ok(text.includes(error))
    assert.match(text, /失败阶段：数据迁移失败/)
    assert.doesNotMatch(text, /目标 DDL：/)
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

test('unsupported index renders and copies source definition separately from target DDL', async () => {
  const scope = Vue.effectScope()
  try {
    const h = scope.run(harness)
    const sourceDDL = 'CREATE TABLE `Orders` (`id` int, KEY `idx_expr` ((abs(`id`))))'
    h.p.report.value = report({ name: 'orders_idx_expr', source_name: 'idx_expr', target_name: 'orders_idx_expr',
      target_table: 'orders', target_schema: 'dst', table: 'Orders', stage: 'unsupported', ddl: '',
      source_ddl: sourceDDL, error: '表达式索引暂不支持自动转换',
      suggestion: '请参考源端表定义，手动适配目标库索引。',
    }, 'indexes')
    const html = await renderToString(h.app)
    assert.match(html, /索引名称：orders_idx_expr/)
    assert.match(html, /源索引名称：idx_expr/)
    assert.match(html, /目标索引名称（计划）：orders_idx_expr/)
    assert.match(html, /目标表：dst.orders/)
    assert.match(html, /源端失败索引定义（仅供参考，需适配目标库）/)
    assert.match(html, /<details[^>]*><summary>查看完整源端表定义<\/summary>/)
    assert.match(html, /所属表：Orders/)
    assert.match(html, /失败阶段：不支持自动转换/)
    assert.match(html, /目标 DDL：该对象暂不支持自动转换，未生成目标 DDL。/)
    assert.match(html, /源端表定义（仅供参考，需适配目标库）/)
    assert.match(html, /处理建议/)
    assert.doesNotMatch(html, /建议 DDL（手动创建）/)
    const copy = h.clicks.find(c => c.label === '复制源端表定义')
    const indexCopy = h.clicks.find(c => c.label === '复制源端索引定义')
    assert.ok(copy)
    assert.ok(indexCopy)
    assert.ok(!h.clicks.some(c => c.label === '复制 DDL'))
    await copy.handler()
    await indexCopy.handler()
    assert.deepEqual(h.copied, [sourceDDL, 'KEY `idx_expr` ((abs(`id`)))'])
    await h.p.exportReport()
    const text = await h.downloads[0].text()
    assert.ok(text.includes(sourceDDL))
    assert.match(text, /源端表定义（仅供参考，需适配目标库）：/)
    assert.match(text, /失败阶段：不支持自动转换/)
  } finally { scope.stop() }
})

test('legacy metadata failure explains absent DDL and keeps counting failure events', async () => {
  const scope = Vue.effectScope()
  try {
    const h = scope.run(harness)
    h.p.report.value = report({ name: '元数据读取', ddl: '', error: 'converting NULL to string is unsupported' })
    const html = await renderToString(h.app)
    assert.match(html, /元数据读取失败，尚未生成目标 DDL。/)
    assert.doesNotMatch(html, /DDL：—/)
    assert.equal(h.p.tableRows.value[0].totalLabel, '未知')
    assert.equal(h.p.tableRows.value[0].failedLabel, '未知')
    assert.equal(h.p.failedCount.value, 1)
    assert.ok(!h.clicks.some(c => c.label.includes('复制')))
    await h.p.exportReport()
    assert.match(await h.downloads[0].text(), /总计 未知\s+成功 0\s+失败 未知/)
  } finally { scope.stop() }
})


test('successful compatibility functions expand, copy actual DDL and export without repair actions', async () => {
 const scope = Vue.effectScope()
 try {
  const h = scope.run(harness)
  const r = report({ name: 'unused', error: '' })
  r.tables = { total: 0, success: 0, failed: 0, items: [] }
  const ddl = 'CREATE FUNCTION app.get_uuid() RETURNS varchar AS actual_definition'
  r.compatibility_functions = { total: 2, success: 2, failed: 0, created: 1, reused: 1, items: [
   { name: 'get_uuid', schema: 'app', status: 'created', generator: 'extensions.uuid_generate_v4', ddl, error: '' },
   { name: 'GET_UUID', schema: 'app', status: 'reused', ddl: '', error: '', definition_warning: '无法读取已有函数定义: denied' },
  ], extensions: [{ name: 'uuid-ossp', schema: 'extensions', status: 'installed', sql: 'CREATE EXTENSION IF NOT EXISTS "uuid-ossp"' }] }
  h.p.report.value = r
  const html = await renderToString(h.app)
  for (const text of ['兼容函数', 'app.get_uuid()', 'app.GET_UUID()', '新建', '复用', 'actual_definition', '无法读取已有函数定义: denied', 'uuid-ossp', '已安装']) assert.ok(html.includes(text), text)
  assert.equal(h.p.failedCount.value, 0)
  assert.equal(h.clicks.some(button => button.label === '去修复'), false)
  const copy = h.clicks.find(button => button.label === '复制 DDL')
  assert.ok(copy)
  await copy.handler()
  assert.equal(h.copied[0], ddl)
  await h.p.exportReport()
  const exported = await h.downloads[0].text()
  assert.ok(exported.includes(ddl))
  assert.ok(exported.includes('app.GET_UUID：复用'))
  assert.ok(exported.includes('CREATE EXTENSION IF NOT EXISTS'))
 } finally { scope.stop() }
})
