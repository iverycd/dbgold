import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import * as Vue from 'vue'
import { parse } from '@vue/compiler-sfc'
import { migrationReportUtils } from './helpers/migration-report.mjs'
import { autoRepairUtils } from './helpers/auto-repairs.mjs'

const { reportCategories, formatReportCategories } = migrationReportUtils
const adaptation = extra => ({ recorded: true, total: 62, success: 58, failed: 0, blocked: 4, pending: 0, unknown: 0, ...extra })
function report(extra) {
  const value = { rowCounts: [] }
  for (const key of ['tables', 'data', 'primaryKeys', 'views', 'indexes', 'constraints', 'sequences', 'comments', 'triggers']) value[key] = { total: 0, success: 0, failed: 0, items: [] }
  value.triggers.total = 77
  value.sequences = { total: 34, success: 30, failed: 4, items: [2017, 2018, 2019, 2020].map(year => ({ name: `SQ_CODE_MAIN_${year}`, table: `CODE_MAIN_${year}`, stage: 'metadata', ddl: '', error: '源序列不可见' })) }
  value.indexes = { total: 1, success: 0, failed: 1, items: [{ name: 'IDX_FAILED', stage: 'execute', ddl: 'CREATE INDEX IDX_FAILED ON T(ID)', error: 'index failure' }] }
  if (extra) value.trigger_adaptation = extra
  return value
}

test('source trigger inventory and scoped sequence trigger outcomes are separate informational rows', () => {
  const value = report(adaptation())
  const rows = reportCategories(value)
  const source = rows.find(row => row.key === 'triggers')
  assert.equal(source.label, '源端触发器')
  assert.equal(source.totalLabel, '77')
  assert.equal(source.successLabel, '—')
  assert.equal(source.failedLabel, '—')
  assert.equal(source.statusLabel, '仅统计源端数量')
  assert.equal(source.isInformational, true)
  const migrated = rows.find(row => row.key === 'trigger_adaptation')
  assert.equal(migrated.label, '序列关联触发器')
  assert.equal(migrated.totalLabel, '62')
  assert.equal(migrated.successLabel, '58')
  assert.equal(migrated.failedLabel, '0')
  assert.equal(migrated.statusLabel, '部分适配，阻塞4个')
  assert.equal(migrated.isInformational, true)
  assert.equal(migrated.items.length, 0, 'binding outcomes do not create duplicate repair objects or details')
  const text = formatReportCategories(value).join('\n')
  assert.match(text, /源端触发器\s+总计 77\s+（仅统计源端数量）/)
  assert.match(text, /序列关联触发器\s+总计 62\s+成功 58\s+失败 0\s+状态：部分适配，阻塞4个/)
  assert.doesNotMatch(text, /阻塞19|未迁移19|待执行19/)
})

for (const [name, value, status, color] of [
  ['zero bindings', adaptation({ total: 0, success: 0, blocked: 0 }), '无关联触发器', 'gray'],
  ['all succeeded', adaptation({ success: 62, blocked: 0 }), '全部适配', 'green'],
  ['actual trigger failure', adaptation({ success: 58, failed: 3, blocked: 0, pending: 1 }), '部分适配，失败3个，待执行1个', 'orange'],
  ['all pending', adaptation({ success: 0, blocked: 0, pending: 62 }), '待执行62个', 'orange'],
  ['unknown outcomes', adaptation({ success: 58, blocked: 0, unknown: 4 }), '部分适配，结果未知4个', 'orange'],
]) {
  test(`trigger adaptation presents ${name} without inventing source-count arithmetic`, () => {
    const row = reportCategories(report(value)).find(row => row.key === 'trigger_adaptation')
    assert.equal(row.totalLabel, String(value.total))
    assert.equal(row.successLabel, String(value.success))
    assert.equal(row.failedLabel, String(value.failed))
    assert.equal(row.statusLabel, status)
    assert.equal(row.statusColor, color)
  })
}

test('unrecorded binding outcomes do not claim zero counts or successful migration', () => {
  const value = report(adaptation({ recorded: false }))
  const row = reportCategories(value).find(row => row.key === 'trigger_adaptation')
  assert.equal(row.totalLabel, '未记录')
  assert.equal(row.successLabel, '—')
  assert.equal(row.failedLabel, '—')
  assert.equal(row.statusLabel, '关联触发器结果未记录')
  assert.equal(row.statusColor, 'gray')
  const text = formatReportCategories(value).join('\n')
  assert.match(text, /序列关联触发器\s+总计 未记录\s+成功 —\s+失败 —\s+状态：关联触发器结果未记录/)
  assert.doesNotMatch(text, /序列关联触发器[^\n]*(?:总计 0|成功 0|全部成功|全部适配)/)
})

test('source inventory failure does not erase recorded binding execution results', () => {
  const value = report(adaptation())
  value.triggers.total = -1
  const rows = reportCategories(value)
  assert.equal(rows.find(row => row.key === 'triggers').totalLabel, '获取失败')
  assert.equal(rows.find(row => row.key === 'trigger_adaptation').successLabel, '58')
})

test('reports without adaptation information retain the historical trigger row and export', () => {
  const value = report()
  const rows = reportCategories(value)
  assert.equal(rows.find(row => row.key === 'triggers').label, '触发器')
  assert.equal(rows.some(row => row.key === 'trigger_adaptation'), false)
  assert.match(formatReportCategories(value).join('\n'), /触发器\s+总计 77\s+（未迁移）/)
})

function viewHarness(view) {
  const { descriptor } = parse(readFileSync(new URL(`../src/views/${view}.vue`, import.meta.url), 'utf8'))
  const fields = view === 'MigrationReportPanel' ? 'report, tableRows, failedCount, exportReport' : 'report, job, categoryRows, migratedCategories, objectTotalCount, objectSuccessCount, failedObjectCount, failedBusinessObjectCount, failedItems, exportReport'
  const downloads = []
  const context = {
    exports: {}, Blob, defineProps: () => ({ jobID: 'trigger-report' }), defineEmits: () => () => {}, defineExpose() {},
    URL: { createObjectURL: blob => { downloads.push(blob); return 'blob:trigger-report' }, revokeObjectURL() {} },
    document: { createElement: () => ({ click() {} }) },
    require: id => {
      if (id === 'vue') return { ...Vue, onMounted() {}, onUnmounted() {} }
      if (id === 'vue-router') return { useRoute: () => ({ params: { jobId: 'trigger-report' } }), useRouter: () => ({}) }
      if (id === '@/utils/migrationReport') return migrationReportUtils
      if (id === '@/utils/autoRepairs') return autoRepairUtils
      if (id === '@/api/migration' || id === '@/utils/clipboard') return {}
      if (id === '@arco-design/web-vue') return { Message: { error: message => assert.fail(message), success() {} } }
      throw new Error(`Unexpected report import ${id}`)
    },
  }
  vm.runInNewContext(ts.transpileModule(`${descriptor.scriptSetup.content}\nexport { ${fields} };`, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } }).outputText, context)
  return { panel: context.exports, downloads, descriptor }
}

for (const view of ['MigrationReportPanel', 'DataMigrationJobDetailView']) {
  test(`${view} shows adaptation but preserves five original failures and business totals`, async () => {
    const scope = Vue.effectScope()
    try {
      const { panel, downloads, descriptor } = scope.run(() => viewHarness(view))
      panel.report.value = report(adaptation({ success: 55, failed: 3 }))
      if (panel.job) panel.job.value = { job_id: 'trigger-report' }
      const rows = (panel.tableRows ?? panel.categoryRows).value
      assert.equal(rows.find(row => row.key === 'trigger_adaptation').failedLabel, '3')
      assert.equal((panel.failedCount ?? panel.failedObjectCount).value, 5, 'trigger unit outcomes must not duplicate sequence-level failures')
      if (panel.migratedCategories) {
        assert.equal(panel.migratedCategories.value.some(row => row.isInformational), false)
        assert.equal(panel.objectTotalCount.value, 35)
        assert.equal(panel.objectSuccessCount.value, 30)
        assert.equal(panel.failedBusinessObjectCount.value, 5)
        assert.equal(panel.failedItems.value.length, 5)
        assert.equal(panel.failedItems.value.some(item => item.category === 'trigger_adaptation'), false)
        assert.match(descriptor.template.content, /record\.isInformational \|\| record\.total <= 0/, 'informational trigger rows do not display a business success rate')
      }
      await panel.exportReport()
      const text = await downloads[0].text()
      assert.match(text, /源端触发器\s+总计 77\s+（仅统计源端数量）/)
      assert.match(text, /序列关联触发器\s+总计 62\s+成功 55\s+失败 3\s+状态：部分适配，失败3个，阻塞4个/)
      assert.equal((text.match(/失败阶段：/g) || []).length, 5)
    } finally { scope.stop() }
  })
}
