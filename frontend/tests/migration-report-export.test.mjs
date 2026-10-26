import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import * as Vue from 'vue'
import { parse } from '@vue/compiler-sfc'
import { migrationReportUtils } from './helpers/migration-report.mjs'
import { autoRepairUtils } from './helpers/auto-repairs.mjs'

const views = ['MigrationReportPanel', 'DataMigrationJobDetailView']
const largeLineCount = 200000
const jobID = '12345678-report-export'
const scripts = new Map(views.map(view => {
  const { descriptor } = parse(readFileSync(new URL(`../src/views/${view}.vue`, import.meta.url), 'utf8'))
  const fields = `report, repairPanel, exportReport${view === 'DataMigrationJobDetailView' ? ', job' : ', loadReport, timingJob, timingLoading, timingUnavailable, exporting'}`
  return [view, ts.transpileModule(`${descriptor.scriptSetup.content}\nexport { ${fields} };`, {
    compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
  }).outputText]
}))

function emptyReport() {
  const report = { rowCounts: [] }
  for (const key of ['tables', 'data', 'primaryKeys', 'views', 'indexes', 'constraints', 'sequences', 'comments', 'triggers']) {
    report[key] = { total: 0, success: 0, failed: 0, items: [] }
  }
  return report
}

function failureReport(item) {
  const report = emptyReport()
  report.tables = { total: 1, success: 0, failed: 1, items: [{ name: 'failed_table', stage: 'execute', error: 'permission denied', ...item }] }
  return report
}

function numberedText(prefix, count = largeLineCount) {
  return Array.from({ length: count }, (_, index) => `${prefix}${index}`).join('\n')
}

function assertNumberedLines(text, prefix, count = largeLineCount, indent = '') {
  const expression = new RegExp(`^${indent}${prefix}(\\d+)$`, 'gm')
  let seen = 0
  for (const match of text.matchAll(expression)) {
    assert.equal(Number(match[1]), seen, `unexpected ${prefix} line at position ${seen}`)
    seen++
  }
  assert.equal(seen, count, `all ${prefix} lines are preserved`)
}

function harness(view, overrides = {}) {
  const downloads = [], clicks = [], messages = [], revoked = []
  const scope = Vue.effectScope()
  const props = Vue.reactive({ jobID })
  const route = Vue.reactive({ params: { jobId: jobID } })
  const context = {
    exports: {}, Blob, defineProps: () => props, defineEmits: () => () => {}, defineExpose() {},
    URL: {
      createObjectURL(blob) { downloads.push(blob); return 'blob:report-export' },
      revokeObjectURL(url) { revoked.push(url) },
    },
    document: { createElement: () => ({ click() { clicks.push({ href: this.href, download: this.download }) } }) },
    require: id => {
      if (id === 'vue') return { ...Vue, onMounted() {}, onUnmounted() {} }
      if (id === 'vue-router') return { useRoute: () => route, useRouter: () => ({}) }
      if (id === '@/utils/migrationReport') return { ...migrationReportUtils, ...overrides.reportUtils }
      if (id === '@/utils/autoRepairs') return { ...autoRepairUtils, ...overrides.autoRepairUtils }
      if (id === '@/api/migration') return {
        getDataMigrationJob: async id => ({ data: { job_id: id } }),
        getDataMigrationReport: async () => ({ data: emptyReport() }),
        ...overrides.api,
      }
      if (id === '@/utils/clipboard') return {}
      if (id === '@arco-design/web-vue') return { Message: { error: message => messages.push(message), success() {} } }
      throw new Error(`Unexpected report import ${id}`)
    },
  }
  try {
    scope.run(() => vm.runInNewContext(scripts.get(view), context))
    const page = context.exports
    page.report.value = emptyReport()
    if (page.job) page.job.value = { job_id: jobID }
    return { page, props, route, downloads, clicks, messages, revoked, stop: () => scope.stop() }
  } catch (error) {
    scope.stop()
    throw error
  }
}

async function exportedText(h) {
  await h.page.exportReport()
  assert.deepEqual(h.messages, [], 'successful export does not display an error')
  assert.equal(h.downloads.length, 1)
  assert.deepEqual(h.clicks, [{ href: 'blob:report-export', download: 'migration-report-12345678.txt' }])
  assert.deepEqual(h.revoked, ['blob:report-export'])
  assert.equal(h.downloads[0].type, 'text/plain;charset=utf-8')
  return h.downloads[0].text()
}

for (const view of views) {
  for (const status of ['done', 'failed', 'cancelled']) {
    test(`${view} exports the original start, finish, and cross-day duration for a ${status} task`, async () => {
      const h = harness(view)
      try {
        const timing = {
          job_id: jobID, status,
          created_at: '2026-10-25T08:02:03', finished_at: '2026-10-26T09:04:06',
        }
        if (h.page.job) h.page.job.value = timing
        else h.page.timingJob.value = timing
        const text = await exportedText(h)
        assert.match(text, /^迁移开始时间：2026-10-25 08:02:03$/m)
        assert.match(text, /^迁移结束时间：2026-10-26 09:04:06$/m)
        assert.match(text, /^总耗时：1 天 1 小时 2 分 3 秒$/m)
        assert.match(text, /^导出时间: /m, 'the export time remains separate from migration timing')
      } finally { h.stop() }
    })
  }

  test(`${view} exports an unfinished task without inventing a finish time or elapsed duration`, async () => {
    const h = harness(view)
    try {
      const timing = { job_id: jobID, status: 'running', created_at: '2026-10-26T09:04:06' }
      if (h.page.job) h.page.job.value = timing
      else h.page.timingJob.value = timing
      const text = await exportedText(h)
      assert.match(text, /^迁移开始时间：2026-10-26 09:04:06$/m)
      assert.match(text, /^迁移结束时间：尚未结束$/m)
      assert.match(text, /^总耗时：进行中$/m)
    } finally { h.stop() }
  })

  test(`${view} discards an export if the task changes while repair history is loading`, async () => {
    const h = harness(view)
    try {
      let resolveHistory
      h.page.repairPanel.value = { exportHistory: () => new Promise(resolve => { resolveHistory = resolve }) }
      const exporting = h.page.exportReport()
      await Vue.nextTick()
      assert.equal(typeof resolveHistory, 'function')
      if (view === 'MigrationReportPanel') h.props.jobID = '87654321-next-task'
      else h.route.params.jobId = '87654321-next-task'
      await Vue.nextTick()
      resolveHistory(['original task repair history'])
      await exporting
      assert.deepEqual(h.downloads, [])
      assert.deepEqual(h.clicks, [])
      assert.deepEqual(h.messages, [])
    } finally { h.stop() }
  })

  test(`${view} exports every line of a large repair history without swallowing a stack error`, async () => {
    const h = harness(view)
    try {
      const history = Array.from({ length: largeLineCount }, (_, index) => `HISTORY_LINE_${index}`)
      let historyRequests = 0
      h.page.repairPanel.value = { exportHistory: async () => { historyRequests++; return history } }
      const text = await exportedText(h)
      assert.equal(historyRequests, 1)
      assertNumberedLines(text, 'HISTORY_LINE_')
      assert.ok(text.endsWith(`HISTORY_LINE_${largeLineCount - 1}`))
    } finally { h.stop() }
  })

  test(`${view} exports all failed objects when the real category report exceeds the argument limit`, async () => {
    const h = harness(view)
    try {
      const count = 50000
      const report = emptyReport()
      report.tables = {
        total: count, success: 0, failed: count,
        items: Array.from({ length: count }, (_, index) => ({
          name: `FAILED_OBJECT_${index}`, stage: 'execute', error: 'permission denied', ddl: `CREATE TABLE failed_${index} (id INT);`,
        })),
      }
      assert.ok(migrationReportUtils.formatReportCategories(report).length > largeLineCount)
      h.page.report.value = report
      const text = await exportedText(h)
      assertNumberedLines(text, 'FAILED_OBJECT_', count, '  ● ')
      let seen = 0
      for (const match of text.matchAll(/^      CREATE TABLE failed_(\d+) \(id INT\);$/gm)) {
        assert.equal(Number(match[1]), seen++)
      }
      assert.equal(seen, count)
      assert.match(text, /表\s+总计 50000\s+成功 0\s+失败 50000/)
    } finally { h.stop() }
  })

  test(`${view} exports a long target DDL with all lines and indentation intact`, async () => {
    const h = harness(view)
    try {
      h.page.report.value = failureReport({ ddl: numberedText('TARGET_DDL_LINE_') })
      const text = await exportedText(h)
      assert.match(text, /目标 DDL：\n      TARGET_DDL_LINE_0\n/)
      assertNumberedLines(text, 'TARGET_DDL_LINE_', largeLineCount, '      ')
    } finally { h.stop() }
  })

  for (const section of ['auto repairs', 'content validation']) {
    test(`${view} appends an entire large ${section} section`, async () => {
      const prefix = section === 'auto repairs' ? 'AUTO_REPAIR_LINE_' : 'VALIDATION_LINE_'
      // Isolate the caller's unbounded section append without creating hundreds
      // of thousands of unrelated reactive records or validation samples.
      const lines = Array.from({ length: largeLineCount }, (_, index) => `${prefix}${index}`)
      const overrides = section === 'auto repairs'
        ? { autoRepairUtils: { formatAutoRepairs: () => lines } }
        : { reportUtils: { formatDataValidation: () => lines } }
      const h = harness(view, overrides)
      try {
        const text = await exportedText(h)
        assertNumberedLines(text, prefix)
      } finally { h.stop() }
    })
  }

  test(`${view} still exports an old report without optional sections or repair panel`, async () => {
    const h = harness(view)
    try {
      h.page.report.value = {}
      const text = await exportedText(h)
      assert.match(text, /^单次迁移报告\nJob ID: 12345678-report-export\n/)
      assert.match(text, /=== 迁移概览 ===/)
      assert.match(text, /=== 数据内容校验 ===\n未启用/)
      assert.match(text, /^迁移开始时间：未记录$/m)
      assert.match(text, /^迁移结束时间：未记录$/m)
      assert.match(text, /^总耗时：—$/m)
      assert.doesNotMatch(text, /undefined|null|HISTORY_LINE_/)
    } finally { h.stop() }
  })

  test(`${view} keeps a repair history request failure visible and does not download a partial report`, async () => {
    const h = harness(view)
    try {
      h.page.repairPanel.value = { exportHistory: async () => { throw new Error('修复历史读取失败') } }
      await h.page.exportReport()
      assert.deepEqual(h.messages, ['修复历史读取失败'])
      assert.deepEqual(h.downloads, [])
      assert.deepEqual(h.clicks, [])
      assert.deepEqual(h.revoked, [])
    } finally { h.stop() }
  })
}

test('report export waits for pending task timing and then includes the fetched values', async () => {
  let resolveTiming
  const h = harness('MigrationReportPanel', {
    api: { getDataMigrationJob: () => new Promise(resolve => { resolveTiming = resolve }) },
  })
  try {
    await h.page.loadReport()
    assert.equal(h.page.timingLoading.value, true)
    const exporting = h.page.exportReport()
    await Vue.nextTick()
    assert.equal(h.page.exporting.value, true)
    assert.deepEqual(h.downloads, [])
    resolveTiming({ data: {
      job_id: jobID, status: 'done', created_at: '2026-10-26T08:00:00', finished_at: '2026-10-26T08:01:02',
    } })
    await exporting
    assert.equal(h.downloads.length, 1)
    assert.equal(h.page.exporting.value, false)
    const text = await h.downloads[0].text()
    assert.match(text, /^迁移开始时间：2026-10-26 08:00:00$/m)
    assert.match(text, /^迁移结束时间：2026-10-26 08:01:02$/m)
    assert.match(text, /^总耗时：1 分 2 秒$/m)
  } finally { h.stop() }
})

test('report export preserves the report and explicitly marks unavailable timing when its request fails', async () => {
  let rejectTiming
  const h = harness('MigrationReportPanel', {
    api: { getDataMigrationJob: () => new Promise((_, reject) => { rejectTiming = reject }) },
  })
  try {
    await h.page.loadReport()
    const exporting = h.page.exportReport()
    rejectTiming(new Error('timing request failed'))
    await exporting
    assert.equal(h.page.timingUnavailable.value, true)
    assert.equal(h.page.exporting.value, false)
    assert.equal(h.downloads.length, 1)
    assert.deepEqual(h.messages, [])
    const text = await h.downloads[0].text()
    assert.match(text, /^迁移开始时间：时间信息暂不可用$/m)
    assert.match(text, /^迁移结束时间：时间信息暂不可用$/m)
    assert.match(text, /^总耗时：—$/m)
    assert.match(text, /=== 迁移概览 ===/)
  } finally { h.stop() }
})

test('switching tasks while export waits for timing prevents a mismatched download and allows the new export', async () => {
  const pending = []
  const h = harness('MigrationReportPanel', {
    api: { getDataMigrationJob: id => new Promise(resolve => pending.push({ id, resolve })) },
  })
  try {
    await h.page.loadReport()
    const oldExport = h.page.exportReport()
    h.props.jobID = '87654321-next-task'
    await Vue.nextTick()
    assert.equal(pending[1].id, '87654321-next-task')
    pending[0].resolve({ data: { job_id: jobID, created_at: '2020-01-01T00:00:00' } })
    await oldExport
    assert.equal(h.page.exporting.value, false)
    assert.deepEqual(h.downloads, [])
    pending[1].resolve({ data: {
      job_id: '87654321-next-task', status: 'done', created_at: '2026-10-26T10:00:00', finished_at: '2026-10-26T10:00:10',
    } })
    await Vue.nextTick()
    await h.page.exportReport()
    assert.equal(h.downloads.length, 1)
    assert.equal(h.clicks[0].download, 'migration-report-87654321.txt')
    const text = await h.downloads[0].text()
    assert.match(text, /^Job ID: 87654321-next-task$/m)
    assert.match(text, /^迁移开始时间：2026-10-26 10:00:00$/m)
    assert.doesNotMatch(text, /2020-01-01|12345678-report-export/)
  } finally { h.stop() }
})

for (const [field, prefix, label] of [
  ['source_ddl', 'SOURCE_DDL_LINE_', '源端表定义'],
  ['suggested_ddl', 'SUGGESTED_DDL_LINE_', '建议 DDL'],
]) {
  test(`category formatter preserves all lines of a large ${field}`, () => {
    const text = migrationReportUtils.formatReportCategories(failureReport({ [field]: numberedText(prefix) })).join('\n')
    assert.ok(text.includes(label))
    assertNumberedLines(text, prefix, largeLineCount, '      ')
  })
}

test('category formatter preserves all lines of a large compatibility routine DDL', () => {
  const report = emptyReport()
  report.compatibility_functions = {
    total: 1, success: 0, failed: 1, created: 0, reused: 0,
    items: [{ name: 'long_function', schema: 'app', status: 'failed', ddl: numberedText('FUNCTION_DDL_LINE_'), error: 'permission denied' }],
  }
  const text = migrationReportUtils.formatReportCategories(report).join('\n')
  assert.match(text, /函数 DDL：\n      FUNCTION_DDL_LINE_0\n/)
  assertNumberedLines(text, 'FUNCTION_DDL_LINE_', largeLineCount, '      ')
})

test('category formatter preserves a large source index definition and its complete source table DDL', () => {
  const report = emptyReport()
  const columns = Array.from({ length: largeLineCount }, (_, index) => `\`id\` /* INDEX_LINE_${index} */`).join(',\n')
  report.indexes = {
    total: 1, success: 0, failed: 1,
    items: [{
      name: 'idx_long', source_name: 'idx_long', stage: 'execute', error: 'index rejected',
      source_ddl: `CREATE TABLE \`source_table\` (\`id\` int, KEY \`idx_long\` (\n${columns}\n))`,
    }],
  }
  const text = migrationReportUtils.formatReportCategories(report).join('\n')
  assert.match(text, /源端失败索引定义（仅供参考，需适配目标库）：\n      KEY `idx_long` \(/)
  assert.match(text, /源端表定义（仅供参考，需适配目标库）：\n      CREATE TABLE `source_table`/)
  let seen = 0
  for (const match of text.matchAll(/^      `id` \/\* INDEX_LINE_(\d+) \*\/,?$/gm)) {
    assert.equal(Number(match[1]), seen % largeLineCount)
    seen++
  }
  assert.equal(seen, largeLineCount * 2, 'the isolated index and source table both retain every line')
})
