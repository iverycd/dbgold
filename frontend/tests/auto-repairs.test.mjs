import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import * as Vue from 'vue'
import { parse } from '@vue/compiler-sfc'
import { compile } from '@vue/compiler-dom'
import { renderToString } from '@vue/server-renderer'
import Arco from '@arco-design/web-vue'
import { autoRepairUtils } from './helpers/auto-repairs.mjs'
import { migrationReportUtils } from './helpers/migration-report.mjs'

const repairs = [
  { type: 'column_rename', table: 'gj_data', target_table: 'APP.GJ_DATA', source: 'ROWID', target: 'ROW_ID_2', reason: '达梦禁用列名', status: 'applied' },
  { type: 'column_rename', table: 'gj_data', target_table: 'APP.GJ_DATA', source: 'TrxId', target: 'TRX_ID', reason: '达梦禁用列名', status: 'applied' },
  { type: 'column_rename', table: 'Other', target_table: 'APP.OTHER', source: 'ROWID', target: 'ROW_ID', reason: '达梦禁用列名', status: 'reused' },
]

function execute(path, exported, props = {}) {
  const { descriptor } = parse(readFileSync(new URL(`../src/${path}.vue`, import.meta.url), 'utf8'))
  if (!exported) {
    const parsed = ts.createSourceFile('setup.ts', descriptor.scriptSetup.content, ts.ScriptTarget.Latest, true, ts.ScriptKind.TS)
    const bindings = []
    for (const statement of parsed.statements) {
      if (ts.isVariableStatement(statement)) for (const declaration of statement.declarationList.declarations) {
        if (ts.isIdentifier(declaration.name)) bindings.push(declaration.name.text)
      }
      if (ts.isFunctionDeclaration(statement)) bindings.push(statement.name.text)
      if (ts.isImportDeclaration(statement) && !statement.importClause?.isTypeOnly) {
        const named = statement.importClause?.namedBindings
        if (named && ts.isNamedImports(named)) for (const item of named.elements) {
          if (!item.isTypeOnly) bindings.push(item.name.text)
        }
      }
    }
    exported = bindings.join(', ')
  }
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

const largeRepairs = Array.from({ length: 20 }, (_, table) =>
  Array.from({ length: table === 0 ? 22 : 8 }, (_, column) => ({
    type: 'oracle_mysql_longtext', table: `SOURCE_TABLE_${table}_END`, target_table: `target_table_${table}_end`,
    source: `FIELD_${table}_${column}_END`, target: `field_${table}_${column}_end LONGTEXT`,
    reason: '整表声明行宽超过 MySQL 65535 字节上限', status: column % 2 ? 'reused' : 'applied',
  })),
).flat()

function appFor(h, props, bindings = {}) {
  const code = compile(h.descriptor.template.content, { mode: 'function', prefixIdentifiers: true }).code
  const render = vm.runInNewContext(`(function() { ${code} })()`, { Vue })
  const app = Vue.createSSRApp({ setup: () => ({ ...h.values, ...Vue.toRefs(props), ...bindings }), render })
  app.use(Arco.default)
  // TableColumn registers through watchers, which do not flush during SSR.
  // Convert those declarations to the equivalent columns prop; retain Arco's
  // actual row expansion, rendering and pagination implementations.
  app.component('a-table', {
    props: Arco.Table.props,
    setup: (props, { slots, attrs }) => () => {
      const columns = slots.columns?.().filter(node => node.type !== Vue.Comment).map(node => ({
        ...node.props, dataIndex: node.props?.['data-index'], slots: node.children,
      }))
      const forwarded = { ...slots }
      delete forwarded.columns
      return Vue.h(Arco.Table, { ...attrs, ...props, columns }, forwarded)
    },
  })
  return app
}

function panelHarness(records = repairs) {
  const scope = Vue.effectScope()
  const props = Vue.reactive({ repairs: records })
  const h = scope.run(() => execute('components/AutoRepairPanel', 'groups, autoRepairSourceType, autoRepairStatus, autoRepairType, currentPage, expandedRowKeys, detailPage, toggleGroup, changePage', props))
  return { ...h, props, get app() { return appFor(h, props) }, stop: () => scope.stop() }
}

function emptyReport(records) {
  const report = { rowCounts: [] }
  if (records) report.auto_repairs = records
  for (const key of ['tables', 'data', 'primaryKeys', 'indexes', 'constraints', 'comments', 'sequences', 'views', 'triggers']) {
    report[key] = { total: 0, success: 0, failed: 0, items: [] }
  }
  return report
}

test('source column types render separately and old reports show an explicit missing value', async () => {
  const records = [
    { ...repairs[0], source_type: 'varchar(6000)' },
    { ...repairs[1], source_type: 'NUMBER(18,2)' },
    { ...repairs[0], source: 'Legacy', target: 'LEGACY' },
  ]
  const h = panelHarness(records)
  try {
    h.values.toggleGroup(h.values.groups.value[0].key)
    const html = await renderToString(h.app)
    assert.match(html, /源列类型/)
    assert.match(html, /varchar\(6000\)/)
    assert.match(html, /NUMBER\(18,2\)/)
    assert.match(html, /未记录/)
    const exported = autoRepairUtils.formatAutoRepairs(records).join('\n')
    assert.match(exported, /ROWID varchar\(6000\) → ROW_ID_2/)
    assert.match(exported, /TrxId NUMBER\(18,2\) → TRX_ID/)
    assert.match(exported, /Legacy → LEGACY.*源列类型未记录/)
  } finally { h.stop() }
})

const commentRepair = {
  type: 'oracle_mysql_comment_truncate', table: 'Orders', target_table: 'orders',
  source: 'Title', target: 'title', reason: '列注释由 1200 字符截断为 1024 字符', status: 'applied',
}

test('comment truncations show an inapplicable source type and preserve the truncation reason', async () => {
  const h = panelHarness([commentRepair])
  try {
    h.values.toggleGroup(h.values.groups.value[0].key)
    const html = await renderToString(h.app)
    assert.match(html, /字段与注释调整/)
    assert.match(html, /Title[\s\S]*不适用[\s\S]*title[\s\S]*注释截断/)
    assert.match(html, /列注释由 1200 字符截断为 1024 字符/)
    assert.doesNotMatch(html, /未记录/)
    assert.equal(autoRepairUtils.autoRepairSourceType({ ...commentRepair, source_type: 'VARCHAR2(200)' }), '不适用')
    const exported = autoRepairUtils.formatAutoRepairs([commentRepair]).join('\n')
    assert.match(exported, /Title → title；注释截断；列注释由 1200 字符截断为 1024 字符；本次已修复；源列类型不适用/)
    assert.doesNotMatch(exported, /源列类型未记录/)
  } finally { h.stop() }
})

test('real Arco tables initially show ten compact groups and paginate all 174 adjustments', async () => {
  const h = panelHarness(largeRepairs)
  try {
    assert.equal(largeRepairs.length, 174)
    assert.equal(h.values.groups.value.length, 20)
    let html = await renderToString(h.app)
    for (let table = 0; table < 10; table++) assert.ok(html.includes(`SOURCE_TABLE_${table}_END`))
    assert.ok(!html.includes('SOURCE_TABLE_10_END'))
    assert.doesNotMatch(html, /FIELD_0_0_END|目标结果|arco-select-view/)
    assert.equal(h.values.expandedRowKeys.value.length, 0)
    h.values.toggleGroup(h.values.groups.value[0].key)
    html = await renderToString(h.app)
    for (let column = 0; column < 10; column++) assert.ok(html.includes(`FIELD_0_${column}_END`))
    assert.ok(!html.includes('FIELD_0_10_END'))
    assert.match(html, /field_0_0_end LONGTEXT/)
    assert.match(html, /类型调整/)
    assert.equal(html.split('整表声明行宽超过 MySQL 65535 字节上限').length - 1, 1)
    assert.doesNotMatch(html, /arco-select-view/)
    h.values.detailPage.value = 2
    html = await renderToString(h.app)
    assert.match(html, /FIELD_0_10_END/)
    assert.match(html, /FIELD_0_19_END/)
    assert.doesNotMatch(html, /FIELD_0_0_END|FIELD_0_20_END/)
    h.values.detailPage.value = 3
    html = await renderToString(h.app)
    assert.match(html, /FIELD_0_20_END/)
    assert.match(html, /FIELD_0_21_END/)
    assert.doesNotMatch(html, /FIELD_0_19_END/)
    h.values.changePage(2)
    html = await renderToString(h.app)
    assert.equal(h.values.expandedRowKeys.value.length, 0)
    assert.equal(h.values.detailPage.value, 1)
    assert.doesNotMatch(html, /SOURCE_TABLE_0_END|FIELD_0_20_END/)
    for (let table = 10; table < 20; table++) assert.ok(html.includes(`SOURCE_TABLE_${table}_END`))
  } finally { h.stop() }
})

test('expansion distinguishes target identities, opens one group, and resets for new records', async () => {
  const name = `MiXeD_${'LONG_NAME_'.repeat(20)}`
  const h = panelHarness([
    { ...repairs[0], table: name, target_table: 'APP.FIRST' },
    { ...repairs[1], table: name, target_table: 'APP.SECOND', status: 'reused' },
  ])
  try {
    const groups = h.values.groups.value
    assert.notEqual(groups[0].key, groups[1].key)
    h.values.toggleGroup(groups[0].key)
    let html = await renderToString(h.app)
    assert.ok(html.includes(name))
    assert.match(html, /ROW_ID_2/)
    assert.doesNotMatch(html, />TRX_ID</)
    assert.match(html, /列名修复/)
    assert.equal(groups[0].applied, 1)
    assert.equal(groups[1].reused, 1)
    h.values.detailPage.value = 2
    h.values.toggleGroup(groups[1].key)
    assert.deepEqual(Array.from(h.values.expandedRowKeys.value), [groups[1].key])
    assert.equal(h.values.detailPage.value, 1)
    html = await renderToString(h.app)
    assert.match(html, />TRX_ID</)
    assert.doesNotMatch(html, /ROW_ID_2/)
    h.values.toggleGroup(groups[1].key)
    assert.equal(h.values.expandedRowKeys.value.length, 0)
    h.values.toggleGroup(groups[0].key)
    h.values.currentPage.value = 2
    h.props.repairs = [repairs[2]]
    await Vue.nextTick()
    assert.equal(h.values.currentPage.value, 1)
    assert.equal(h.values.detailPage.value, 1)
    assert.equal(h.values.expandedRowKeys.value.length, 0)
    html = await renderToString(h.app)
    assert.match(html, /APP.OTHER/)
    assert.ok(!html.includes(name))
    h.props.repairs = []
    await Vue.nextTick()
    assert.doesNotMatch(await renderToString(h.app), /自动修复明细|APP.OTHER/)
  } finally { h.stop() }
})

test('different reasons and statuses remain attached to their fields', async () => {
  const h = panelHarness([
    repairs[0], { ...repairs[1], type: 'oracle_mysql_longtext', target: 'trxid LONGTEXT', reason: '另一种调整原因', status: 'reused' },
  ])
  try {
    const group = h.values.groups.value[0]
    assert.equal(group.applied, 1)
    assert.equal(group.reused, 1)
    assert.equal(group.commonReason, null)
    h.values.toggleGroup(group.key)
    const html = await renderToString(h.app)
    assert.match(html, /ROWID[\s\S]*ROW_ID_2[\s\S]*列名修复[\s\S]*达梦禁用列名[\s\S]*本次已修复/)
    assert.match(html, /TrxId[\s\S]*trxid LONGTEXT[\s\S]*类型调整[\s\S]*另一种调整原因[\s\S]*复用已有映射/)
    assert.equal(html.split('达梦禁用列名').length - 1, 1)
    assert.equal(html.split('另一种调整原因').length - 1, 1)
  } finally { h.stop() }
})

for (const view of ['MigrationReportPanel', 'DataMigrationJobDetailView']) {
  test(`${view} offers a compact summary and switches to the following detail tab`, async () => {
    const scope = Vue.effectScope()
    try {
      const props = Vue.reactive({ jobID: 'job-repairs' })
      const h = scope.run(() => execute(`views/${view}`, null, props))
      h.values.report.value = emptyReport(largeRepairs)
      if (h.values.job) h.values.job.value = { job_id: 'job-repairs' }
      const buttons = [], tabs = [], detailKeys = []
      function makeApp() {
        const app = appFor(h, props)
        for (const name of new Set([...h.descriptor.template.content.matchAll(/<([A-Z][\w]*)/g)].map(match => match[1]))) {
          if (name !== 'AutoRepairPanel') app.component(name, { setup: () => () => null })
        }
        app.component('AutoRepairPanel', { props: ['repairs'], setup: props => {
          detailKeys.push(Vue.getCurrentInstance().vnode.key)
          return () => Vue.h('section', { class: 'auto-repair-test-details' }, `${props.repairs.length} 项字段调整内容`)
        } })
        app.component('a-tabs', { props: ['activeKey', 'lazyLoad', 'destroyOnHide'], setup: (props, { slots }) => () => {
          const panes = slots.default()
          tabs.push(panes.filter(pane => pane.type !== Vue.Comment).map(pane => pane.key))
          // Arco's SSR nav does not flush its pane-registration watcher.
          // Render the declared titles explicitly while using its real panes.
          const titles = panes.filter(pane => pane.type !== Vue.Comment).map(pane =>
            Vue.h('span', pane.children?.title?.() || pane.props?.title),
          )
          return Vue.h('div', [Vue.h('nav', titles), Vue.h(Arco.Tabs, props, { default: () => panes })])
        } })
        app.component('a-button', { setup: (_, { slots, attrs }) => () => {
          const children = slots.default?.() || []
          if (children.some(child => child.children === '查看明细')) buttons.push(attrs.onClick)
          return Vue.h(Arco.Button, attrs, { default: () => children })
        } })
        for (const name of new Set([...h.descriptor.template.content.matchAll(/<(icon-[\w-]+)/g)].map(match => match[1]))) app.component(name, { setup: () => () => null })
        return app
      }
      let html = await renderToString(makeApp())
      assert.equal(h.values.activeTab.value, 'overview')
      const text = html.replace(/<[^>]*>/g, '')
      assert.match(text, /自动修复涉及 20 张表、174 项字段调整/)
      assert.match(text, /自动修复明细（174）/)
      assert.deepEqual(Array.from(tabs.at(-1).slice(0, 2)), ['overview', 'auto-repairs'])
      assert.equal(buttons.length, 1)
      buttons[0]()
      assert.equal(h.values.activeTab.value, 'auto-repairs')
      html = await renderToString(makeApp())
      assert.match(html, /174 项字段调整内容/)
      assert.ok(detailKeys.every(key => key === 'job-repairs'))
      assert.ok(html.indexOf('class="arco-tabs') < html.indexOf('class="auto-repair-test-details'))
      for (const records of [[], undefined]) {
        h.values.report.value = emptyReport(records)
        await Vue.nextTick()
        assert.equal(h.values.activeTab.value, 'overview')
        html = await renderToString(makeApp())
        assert.ok(!tabs.at(-1).includes('auto-repairs'))
        assert.doesNotMatch(html, /自动修复涉及|自动修复明细（|auto-repair-test-details/)
      }
      h.values.report.value = emptyReport(largeRepairs)
      await Vue.nextTick()
      h.values.activeTab.value = 'auto-repairs'
      h.values.report.value = null
      await Vue.nextTick()
      assert.equal(h.values.activeTab.value, 'overview')
    } finally { scope.stop() }
  })
}

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
      delete h.values.report.value.auto_repairs
      h.values.exportReport()
      assert.doesNotMatch(await h.downloads[1].text(), /自动修复|ROW_ID/)
      h.values.report.value.auto_repairs = largeRepairs
      await h.values.exportReport()
      const complete = await h.downloads[2].text()
      assert.match(complete, /自动修复（20 张表，174 列）/)
      for (const item of largeRepairs) {
        assert.ok(complete.includes(`${item.source} → ${item.target}；${item.reason}`), item.source)
      }
      h.values.report.value.auto_repairs = [{ ...repairs[0], source_type: 'VARCHAR2(6000)' }]
      await h.values.exportReport()
      assert.match(await h.downloads[3].text(), /ROWID VARCHAR2\(6000\) → ROW_ID_2/)
      h.values.report.value.auto_repairs = [commentRepair]
      await h.values.exportReport()
      const commentExport = await h.downloads[4].text()
      assert.match(commentExport, /Title → title；注释截断；列注释由 1200 字符截断为 1024 字符/)
      assert.match(commentExport, /源列类型不适用/)
      assert.doesNotMatch(commentExport, /源列类型未记录/)
    } finally { scope.stop() }
  })
}

for (const view of ['MigrationReportPanel', 'DataMigrationJobDetailView']) {
  test(`${view} separates compatibility procedures without inflating totals or ordinary failures`, async () => {
    const scope = Vue.effectScope()
    try {
      const exports = view === 'DataMigrationJobDetailView'
        ? 'job, categoryRows, failedItems, failedCategoryOptions, objectTotalCount, objectSuccessCount, failedObjectCount, risks'
        : 'tableRows, failedCount'
      const h = scope.run(() => execute(`views/${view}`, `report, exportReport, ${exports}`, { jobID: 'job-repairs' }))
      const r = { rowCounts: [], compatibility_functions: {
        total: 11, success: 10, failed: 1, created: 8, reused: 2,
        items: [{ kind: 'procedure', name: 'changedatatohistory', schema: 'app', status: 'failed', ddl: 'SAVED PROCEDURE DDL', error: 'procedure unavailable' }],
      } }
      for (const key of ['tables', 'data', 'primaryKeys', 'views', 'indexes', 'constraints', 'sequences', 'comments', 'triggers']) r[key] = { total: 0, success: 0, failed: 0, items: [] }
      h.values.report.value = r
      if (h.values.job) h.values.job.value = { job_id: 'job-repairs' }
      const rows = h.values.categoryRows?.value ?? h.values.tableRows.value
      assert.equal(rows.filter(row => row.isCompatibility).length, 2)
      assert.equal(rows.reduce((sum, row) => sum + row.total, 0), 11)
      assert.equal(rows.find(row => row.key === 'compatibility_functions').failed, 0)
      assert.equal(rows.find(row => row.key === 'compatibility_procedures').failed, 1)
      assert.match(h.descriptor.template.content, /:report="record.compatibilityReport" :kind="record.compatibilityKind"/)
      if (h.values.failedItems) {
        assert.equal(h.values.objectTotalCount.value, 11)
        assert.equal(h.values.objectSuccessCount.value, 10)
        assert.equal(h.values.failedObjectCount.value, 1)
        assert.equal(h.values.failedItems.value.length, 0)
        assert.equal(h.values.failedCategoryOptions.value.length, 0)
        assert.equal(h.values.risks.value.length, 1)
        assert.equal(h.values.risks.value[0].title, '兼容存储过程初始化失败')
        h.values.report.value.indexes = { total: 1, success: 0, failed: 1, items: [{ name: 'bad_index', ddl: '', error: 'index error' }] }
        assert.equal(h.values.failedObjectCount.value, 2)
        assert.equal(h.values.failedItems.value.length, 1)
        assert.equal(h.values.risks.value.find(item => item.key === 'objects').title, '1 个对象迁移失败')
      } else {
        assert.equal(h.values.failedCount.value, 1)
      }
      h.values.exportReport()
      const text = await h.downloads[0].text()
      assert.match(text, /兼容函数\s+总计 10\s+成功 10\s+失败 0/)
      assert.match(text, /兼容存储过程\s+总计 1\s+成功 0\s+失败 1/)
      assert.equal(text.split('SAVED PROCEDURE DDL').length - 1, 1)
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
