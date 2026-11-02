import { connectionDatabaseUtils, connectionDatabaseEditors } from './helpers/connection-database.mjs'
import { migrationReportUtils } from './helpers/migration-report.mjs'
import { partitionPreflightResponse } from './helpers/partition-preflight.mjs'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import * as Vue from 'vue'
import { parse, compileScript } from '@vue/compiler-sfc'
import { baseParse, compile } from '@vue/compiler-dom'
import { renderToString } from '@vue/server-renderer'

function load(source, require, globals = {}) {
  const result = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } })
  const context = { exports: {}, require, AbortController, Blob, FormData, ...globals }
  vm.runInNewContext(result.outputText, context)
  return context.exports
}
const { descriptor } = parse(readFileSync(new URL('../src/views/MigrationView.vue', import.meta.url), 'utf8'))
const { descriptor: panelDescriptor } = parse(readFileSync(new URL('../src/views/PartitionPreflightPanel.vue', import.meta.url), 'utf8'))
function descendants(node) {
  return [node, ...(node.children ?? []).flatMap(descendants)]
}
const pageNodes = descendants(baseParse(descriptor.template.content))
const resultsNode = pageNodes.find(node => node.tag === 'section' && node.props.some(prop => prop.name === 'ref' && prop.value?.content === 'dataResultsSection'))
const actionNode = pageNodes.find(node => node.tag === 'MigrationActionBar')
const renderResults = new Function('Vue', compile(resultsNode.loc.source + actionNode.loc.source, { mode: 'function', prefixIdentifiers: true }).code)(Vue)
const partitionPanel = load(compileScript(panelDescriptor, { id: 'partition-results-test', inlineTemplate: true }).content, id => id === 'vue' ? Vue : {}).default

function registerShells(app, controls = {}) {
  for (const name of ['a-space', 'a-alert', 'a-tag', 'a-tabs', 'a-tab-pane', 'a-empty', 'icon-arrow-right', 'MigrationReportPanel']) {
    app.component(name, { setup: (_, { slots }) => () => Vue.h('div', slots.default?.()) })
  }
  app.component('a-button', {
    props: ['disabled', 'loading'],
    setup: (props, { slots }) => () => Vue.h('button', { disabled: props.disabled }, slots.default?.()),
  })
  app.component('a-table', {
    props: ['data', 'pagination', 'expandable'],
    setup: (props, { slots }) => () => {
      controls.tables?.push(props)
      const columns = slots.columns?.() ?? []
      return Vue.h('div', { class: 'test-table' }, (props.data ?? []).map(record => Vue.h('div', [
        ...columns.map(column => column.children?.cell
          ? column.children.cell({ record })
          : String(record[column.props?.dataIndex ?? column.props?.['data-index']] ?? '')),
        ...(props.expandable?.icon(null, record) !== null ? slots['expand-row']?.({ record }) ?? [] : []),
      ])))
    },
  })
  app.component('a-table-column', { setup: () => () => null })
}
async function renderPage(p) {
  const app = Vue.createSSRApp({ render: () => renderResults(Vue.proxyRefs(p), []) })
  registerShells(app)
  app.component('PartitionPreflightPanel', partitionPanel)
  app.component('MigrationActionBar', {
    props: ['status', 'busy'],
    setup: (props, { slots }) => () => Vue.h('aside', [props.status, slots.default?.()]),
  })
  return renderToString(app)
}
async function renderPanel(result, readonly = false) {
  const tables = []
  const app = Vue.createSSRApp(partitionPanel, { result, readonly })
  registerShells(app, { tables })
  return { html: await renderToString(app), tables }
}
function page(preflight) {
  const starts = [], errors = []
  const p = load(descriptor.scriptSetup.content + `\nexport {
    connections, dataMigrate, startDataMigration, partitionCheck, partitionCheckBusy,
    runPartitionCheck, resetDataMigration, pgChecks, dataPartitionCheck, partitionCheckIssues,
    partitionCheckError, dataMigrationChecking, dataMigrateResultTab, getLogClass,
    canStartMigration, requestDataMigration, cancelDataMigration,
  };`, id => {
    switch (id) {
      case 'vue': return { ...Vue, onMounted() {}, onUnmounted() {}, provide() {} }
      case 'vue-router': return { useRoute: () => ({ query: {} }), onBeforeRouteLeave() {} }
      case '@arco-design/web-vue': return { Message: { error: error => errors.push(error) }, Modal: {} }
      case '@/utils/connectionDatabase': return connectionDatabaseUtils
      case '@/composables/connectionDatabaseEditors': return connectionDatabaseEditors
      case '@/utils/migrationOptions': return { applyTargetMigrationDefaults() {} }
      case '@/utils/migrationReport': return migrationReportUtils
      case '@/api/migration': return {
        preflightDataMigration: preflight,
        startDataMigration: async request => { starts.push(request); return { data: { job_id: 'job' } } },
        createDataMigrateEventSource: () => ({ addEventListener() {}, close() {} }),
      }
      default: return {}
    }
  })
  p.connections.value = [{ id: 1, db_type: 'mysql', database: 'source' }, { id: 2, db_type: 'postgres', database: 'target' }]
  Object.assign(p.dataMigrate, { srcConnId: 1, dstConnId: 2, srcDatabase: 'source', dstSchema: 'public' })
  return { p, starts, errors }
}

test('the migration page has no manual partition check and shows issues only inside execution results', async () => {
  const scope = Vue.effectScope()
  try {
    const { p } = scope.run(() => page(async () => partitionPreflightResponse()))
    const panels = pageNodes.filter(node => node.tag === 'PartitionPreflightPanel')
    assert.equal(panels.length, 1)
    assert.ok(descendants(resultsNode).includes(panels[0]))
    assert.equal(panels[0].props.some(prop => prop.type === 7 && prop.name === 'on' && prop.arg?.content === 'check'), false)
    const html = await renderPage(p)
    assert.match(html, /开始迁移后将自动检查分区兼容性/)
    assert.doesNotMatch(html, /分区兼容性检查结果|运行分区预检|重新预检|运行预检后|test-table/)
  } finally { scope.stop() }
})

test('starting directly checks compatibility and reports skipped tables while launching the rest', async () => {
  const scope = Vue.effectScope()
  try {
    let checks = 0
    const { p, starts, errors } = scope.run(() => page(async () => { checks++; return partitionPreflightResponse({ ready: false, can_start: true, tables: [{ source_table: 'Blocked', target_table: 'blocked', status: 'blocked', error: 'no native equivalent' }, { source_table: 'Healthy', target_table: 'healthy', status: 'ordinary' }] }) }))
    await Vue.nextTick()
    assert.equal(p.partitionCheckIssues.value, null)
    await p.startDataMigration()
    assert.equal(checks, 1)
    assert.equal(starts.length, 1)
    assert.equal(p.partitionCheck.value.tables[0].status, 'blocked')
    assert.equal(p.partitionCheckIssues.value, p.partitionCheck.value)
    assert.ok(p.dataMigrate.logs.some(line => /跳过/.test(line) && /Blocked/.test(line) && /依赖/.test(line)))
    assert.ok(p.dataMigrate.logs.some(line => /其余.*继续/.test(line)))
    const html = await renderPage(p)
    assert.match(html, /分区兼容性检查结果/)
    assert.match(html, /no native equivalent/)
    assert.doesNotMatch(html, /运行分区预检|重新预检/)
    assert.deepEqual(errors, [])
  } finally { scope.stop() }
})

for (const partitioned of [false, true]) test(`a successful automatic ${partitioned ? 'partitioned' : 'ordinary'} table check launches without a separate result table`, async () => {
  const scope = Vue.effectScope()
  try {
    let checks = 0
    const table = { source_table: 'Orders', target_table: 'orders', status: partitioned ? 'ready' : 'ordinary', ...(partitioned ? { source: { method: 'RANGE' }, plan: { mapping: [], statements: [] } } : {}) }
    const { p, starts } = scope.run(() => page(async () => { checks++; return partitionPreflightResponse({ tables: [table] }) }))
    await Vue.nextTick()
    await p.startDataMigration()
    assert.equal(checks, 1)
    assert.equal(starts.length, 1)
    assert.equal(p.dataPartitionCheck.value.tables[0].source_table, 'Orders')
    assert.equal(p.partitionCheckIssues.value, null)
    const html = await renderPage(p)
    assert.doesNotMatch(html, /分区兼容性检查结果|test-table/)
  } finally { scope.stop() }
})

test('automatic checking shows its own progress and cannot submit until the check finishes', async () => {
  const scope = Vue.effectScope()
  try {
    let resolve, entered
    const checking = new Promise(done => { entered = done })
    const { p, starts } = scope.run(() => page(() => {
      entered()
      return new Promise(done => { resolve = done })
    }))
    await Vue.nextTick()
    const pending = p.startDataMigration()
    await checking
    assert.equal(p.dataMigrationChecking.value, true)
    assert.equal(p.partitionCheckBusy.value, true)
    assert.equal(starts.length, 0)
    assert.match(await renderPage(p), /正在检查迁移条件/)
    assert.ok(!p.dataMigrate.logs.some(line => /正在提交迁移任务/.test(line)))
    resolve(partitionPreflightResponse())
    await pending
    assert.equal(p.dataMigrationChecking.value, false)
    assert.equal(starts.length, 1)
    assert.ok(p.dataMigrate.logs.some(line => /正在提交迁移任务/.test(line)))
  } finally { scope.stop() }
})

test('all-blocked generic preflight prevents launch and configuration changes invalidate it', async () => {
  const scope = Vue.effectScope()
  try {
    let ready = false, checks = 0
    const { p, starts } = scope.run(() => page(async () => { checks++; return partitionPreflightResponse({ ready, can_start: ready }) }))
    await Vue.nextTick()
    await p.startDataMigration()
    assert.equal(starts.length, 0)
    assert.equal(p.dataMigrate.running, false)
    assert.equal(p.dataMigrationChecking.value, false)
    assert.match(p.dataMigrate.logs.at(-1), /分区兼容性检查未通过/)
    assert.match(await renderPage(p), /处理后再次点击开始迁移/)
    p.dataMigrate.filter = 'Healthy'; ready = true
    await Vue.nextTick()
    assert.equal(p.partitionCheck.value, null)
    assert.equal(p.partitionCheckIssues.value, null)
    await p.startDataMigration()
    assert.equal(checks, 2)
    assert.equal(starts.length, 1)
  } finally { scope.stop() }
})

test('fatal partition inspection failures are shown without submitting a migration', async () => {
  const scope = Vue.effectScope()
  try {
    const { p, starts } = scope.run(() => page(async () => partitionPreflightResponse({ ready: false, can_start: false, fatal_error: 'cannot read partition catalog' })))
    await Vue.nextTick()
    await p.startDataMigration()
    assert.equal(starts.length, 0)
    assert.equal(p.dataMigrate.running, false)
    const html = await renderPage(p)
    assert.match(html, /分区兼容性检查结果/)
    assert.match(html, /cannot read partition catalog/)
  } finally { scope.stop() }
})

test('a failed check request preserves the concrete error and retries through start migration', async () => {
  const scope = Vue.effectScope()
  try {
    let failed = true, checks = 0
    const { p, starts, errors } = scope.run(() => page(async () => {
      checks++
      if (failed) throw { response: { data: { error: 'catalog permission denied' } } }
      return partitionPreflightResponse()
    }))
    await Vue.nextTick()
    await p.startDataMigration()
    assert.equal(starts.length, 0)
    assert.equal(p.dataMigrationChecking.value, false)
    assert.match(errors[0], /catalog permission denied/)
    assert.match(p.partitionCheckError.value, /catalog permission denied/)
    assert.match(await renderPage(p), /catalog permission denied/)
    assert.match(p.dataMigrate.logs.at(-1), /catalog permission denied/)
    failed = false
    await p.startDataMigration()
    assert.equal(checks, 2)
    assert.equal(starts.length, 1)
    assert.equal(p.partitionCheckError.value, '')
  } finally { scope.stop() }
})

test('generic preflight aborts outdated requests and ignores late readiness', async () => {
  const scope = Vue.effectScope()
  try {
    let resolve, signal, entered
    const checking = new Promise(done => { entered = done })
    const { p, starts } = scope.run(() => page((_, s) => {
      signal = s
      entered()
      return new Promise(done => { resolve = done })
    }))
    await Vue.nextTick()
    const pending = p.startDataMigration()
    await checking
    p.dataMigrate.lowerCaseNames = !p.dataMigrate.lowerCaseNames
    await Vue.nextTick()
    assert.equal(signal.aborted, true)
    resolve(partitionPreflightResponse())
    await pending
    assert.equal(p.partitionCheck.value, null)
    assert.equal(p.partitionCheckBusy.value, false)
    assert.equal(p.dataMigrationChecking.value, false)
    assert.equal(starts.length, 0)
    assert.equal(p.partitionCheckIssues.value, null)
  } finally { scope.stop() }
})

test('generic malformed readiness cannot bypass missing partition evidence', async () => {
  const scope = Vue.effectScope()
  try {
    const { p, starts, errors } = scope.run(() => page(async () => ({ data: { ready: true } })))
    await Vue.nextTick()
    await p.startDataMigration()
    assert.equal(starts.length, 0)
    assert.match(errors[0], /未返回分区兼容性检查/)
    assert.match(await renderPage(p), /未返回分区兼容性检查/)
  } finally { scope.stop() }
})

for (const canStart of [true, false]) test(`PG/MySQL honors partition can_start=${canStart} while legacy ready is false`, async () => {
  const scope = Vue.effectScope()
  try {
    const { p, starts } = scope.run(() => page(async () => partitionPreflightResponse({ ready: false, can_start: canStart })))
    p.connections.value[0].db_type = 'postgres'; p.connections.value[1].db_type = 'mysql'
    await Vue.nextTick()
    await p.startDataMigration()
    assert.equal(starts.length, canStart ? 1 : 0)
    assert.equal(p.dataPartitionCheck.value, p.pgChecks.data.partition_preflight)
    assert.equal(p.partitionCheckIssues.value, p.pgChecks.data.partition_preflight)
    assert.match(await renderPage(p), /分区兼容性检查结果/)
  } finally { scope.stop() }
})

test('partition details show exact mapping counts, append baselines and native SQL Server ordinals', () => {
  const source = panelDescriptor.scriptSetup.content.replace(/const props = defineProps[^\n]+/, 'const props = require("test-props")').replace(/defineEmits[^\n]+/, '')
  const panel = load(source + '\nexport { mappingRows, statusLabel, plannedSQL };', id => id === 'vue' ? Vue : {})
  const table = {
    plan: { mapping: [{ source_schema: 'Source', source_name: 'P', target_name: '2', target_ordinal: 2 }], statements: [{ SQL: 'CREATE PARTITION FUNCTION ...' }] },
    source_rows: { 'Source.P': 4 }, target_rows: { '2': 9 }, baseline: { '2': 5 },
  }
  const rows = panel.mappingRows(table)
  assert.equal(rows[0].sourceRows, 4)
  assert.equal(rows[0].baselineRows, 5)
  assert.equal(rows[0].addedRows, 4)
  assert.equal(rows[0].target, '原生分区 2')
  assert.equal(panel.statusLabel('blocked'), '已阻止')
  assert.match(panel.plannedSQL(table), /CREATE PARTITION FUNCTION/)
  const interval = { plan: { interval_materialization: { seeds: [{ partition_name: 'P_AUTO' }] }, statements: [
    { SQL: 'CREATE TABLE t(k INT) PARTITION BY RANGE(k) INTERVAL(10)' },
    { SQL: 'ALTER TABLE "s"."t" /* DBGOLD_INTERVAL_MATERIALIZE frozen-plan */' },
  ] } }
  assert.match(panel.plannedSQL(interval), /INTERVAL\(10\)/)
  assert.doesNotMatch(panel.plannedSQL(interval), /DBGOLD_INTERVAL_MATERIALIZE/)
  const tables = panelDescriptor.template.content.match(/<a-table\s[\s\S]*?>/g)
  assert.equal(tables.length, 2)
  for (const tableTemplate of tables) assert.match(tableTemplate, /pageSize: 10/)
})

test('readonly history keeps partition definitions, mapping, verification, export and expandable rows', async () => {
  const result = partitionPreflightResponse({ tables: [
    { source_table: 'Ordinary', target_table: 'ordinary', status: 'ordinary' },
    { source_table: 'Orders', target_table: 'orders', status: 'succeeded', source: { method: 'RANGE', partitions: [{ name: 'P_OLD' }] },
      plan: { target: { method: 'RANGE', partitions: [{ name: 'P_NEW' }] }, mapping: [{ source_name: 'P_OLD', target_name: 'P_NEW' }], statements: [{ SQL: 'CREATE TABLE orders(k INT) PARTITION BY RANGE(k)' }] },
      source_rows: { P_OLD: 4 }, target_rows: { P_NEW: 4 },
    },
  ] }).data.partition_preflight
  const { html, tables } = await renderPanel(result, true)
  for (const text of ['分区迁移记录', '核验通过', 'P_OLD', 'P_NEW', '查看目标建表 SQL', 'CREATE TABLE orders', '查看源分区结构与边界', '导出分区记录']) assert.ok(html.includes(text), text)
  assert.doesNotMatch(html, /运行分区预检|重新预检|再次点击开始迁移/)
  assert.ok(tables.length >= 2)
  for (const table of tables) assert.equal(table.pagination.pageSize, 10)
  assert.equal(tables[0].expandable.icon(null, result.tables[0]), null)
  assert.equal(tables[0].expandable.icon(null, result.tables[1]), undefined)

  let blob, clicked = false, revoked = false
  const panel = load(panelDescriptor.scriptSetup.content + '\nexport { download };', id => id === 'vue' ? Vue : {}, {
    defineProps: () => ({ result, readonly: true }),
    URL: { createObjectURL: value => { blob = value; return 'blob:partition-record' }, revokeObjectURL: () => { revoked = true } },
    document: { createElement: () => ({ click: () => { clicked = true } }) },
  })
  panel.download()
  assert.equal(clicked && revoked, true)
  assert.deepEqual(JSON.parse(await blob.text()), result)
})
