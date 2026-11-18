import assert from 'node:assert/strict'
import test from 'node:test'
import { migrationDataIssueUtils as utils } from './helpers/migration-report.mjs'

const row = (table, src, dst, match = src === dst) => ({ table, src, dst, match })
const issue = (kind, message, table = 'Orders', target_table = 'orders') => ({ table, target_table, kind, message })
const repair = (overrides = {}) => {
  const object = { kind: 'table', name: 'Orders', table: 'orders', source_table: 'Orders', target_name: 'orders', ...overrides }
  return { object, members: [{ object }] }
}
const report = (counts, extra = {}) => ({ rowCounts: counts, data: { failed: 0, items: [] }, ...extra })

test('eight reported discrepancies account for ten fewer target rows across 361 checked tables', () => {
  const values = [
    ['frame_visitlog', 398748, 398746], ['messages_center', 165, 164], ['workflow_activity_instance', 388, 386],
    ['workflow_context_value', 183, 182], ['workflow_pvi', 178, 177], ['workflow_pvi_material', 171, 170],
    ['workflow_transition_instance', 218, 217], ['workflow_workitem', 213, 212],
  ]
  const rows = [...values.map(([name, src, dst]) => row(name, src, dst)), ...Array.from({ length: 353 }, (_, i) => row('other' + i, 0, 0))]
  const result = utils.summarizeRowCounts(report(rows))
  assert.equal(result.text, '已核对 361 张表，其中 8 张行数不一致，校验时目标端合计少 10 行。')
  assert.equal(utils.formatRowCountDetail(rows[0]), '源 398,748 行，目标 398,746 行，少 2 行')
  const mixed = utils.summarizeRowCounts(report([...rows, row('added', 3, 8)]))
  assert.equal(mixed.fewer, 10)
  assert.equal(mixed.more, 5)
  assert.match(mixed.text, /少 10 行.*多 5 行/)
})

test('both new and legacy reports can isolate a count-only mismatch without treating it as a copy error', () => {
  const counts = [row('orders', 165, 164)]
  for (const extra of [{}, { data_issues: [issue('row_count_mismatch', '源行数 165，目标行数 164')], data: { failed: 1, items: [{ name: 'Orders', error: 'original detail' }] } }]) {
    const evidence = utils.issuesForRepairRow(utils.indexMigrationDataIssues(report(counts, extra)), repair())
    assert.equal(evidence.pureMismatch, true)
    assert.equal(evidence.rowCount.table, 'orders')
    assert.equal(evidence.issues.length, 1)
    assert.equal(evidence.issues[0].kind, 'row_count_mismatch')
  }
})

test('an explicit copy or content failure remains visible alongside the row difference', () => {
  const r = report([row('orders', 165, 164)], { data_issues: [
    issue('row_count_mismatch', ''), issue('copy_error', '写入第 5 页失败'), issue('content_validation', '字段值不同'),
  ] })
  const index = utils.indexMigrationDataIssues(r)
  const e = utils.issuesForRepairRow(index, repair())
  assert.equal(e.pureMismatch, false)
  const lines = utils.reportDataIssueLines(r, index).join('\n')
  for (const message of ['行数不一致', '数据复制失败 · 写入第 5 页失败', '数据内容有差异 · 字段值不同']) assert.ok(lines.includes(message))
})

test('a count error never contributes placeholder values to the row totals; a missing count still has an entry', () => {
  const r = report([row('orders', 165, 0)], { data_issues: [
    issue('row_count_error', '读取目标表行数失败'), issue('row_count_error', '读取源表行数失败', 'Other', 'other'),
  ] })
  const index = utils.indexMigrationDataIssues(r)
  const summary = utils.summarizeRowCounts(r, index)
  assert.equal(summary.checked, 0)
  assert.equal(summary.fewer, 0)
  assert.equal(summary.countErrors, 2)
  assert.equal(utils.rowCountEntries(r, index).length, 2)
  assert.equal(utils.rowCountEntries(r, index)[1].row, undefined)
  assert.equal(utils.issuesForRepairRow(index, repair()).pureMismatch, false)
  assert.equal(utils.rowCountPresentation(index, r.rowCounts[0]).detail, '读取目标表行数失败')
})

test('historical incomplete, ambiguous and invalid evidence cannot suggest a count-only repair', () => {
  const old = report([row('orders', 54, 53)], { data: { failed: 1, items: [] } })
  assert.equal(utils.issuesForRepairRow(utils.indexMigrationDataIssues(old), repair()).pureMismatch, false)
  const separate = [repair(), repair({ source_table: 'orders_alt', schema: 'other' })]
  assert.equal(utils.issuesForRepairRow(utils.indexMigrationDataIssues(report(old.rowCounts), separate), separate[0]), undefined)
  const ambiguous = report([row('Orders', 10, 5), row('orders', 20, 12)])
  assert.equal(utils.issuesForRepairRow(utils.indexMigrationDataIssues(ambiguous), repair()), undefined)
  for (const count of [row('orders', 0, 0, false), row('orders', 15, -1)]) {
    const index = utils.indexMigrationDataIssues(report([count]))
    assert.equal(utils.rowCountPresentation(index, count).comparable, false)
    assert.equal(utils.issuesForRepairRow(index, repair()).pureMismatch, false)
  }
})

test('an old unqualified failure keeps the original reason and does not claim the copy failed', () => {
  const r = report([row('orders', 5, 3)], { data: { failed: 1, items: [{ name: 'Orders', error: 'original failure' }] } })
  const e = utils.issuesForRepairRow(utils.indexMigrationDataIssues(r), repair())
  assert.equal(e.pureMismatch, false)
  assert.ok(e.issues.some(issue => issue.kind === 'unknown' && issue.message === 'original failure'))
  assert.ok(!e.issues.some(issue => issue.kind === 'copy_error'))
})

test('valid table counts survive an independent partition count failure', () => {
  for (const dst of [5, 4]) {
    const r = report([row('orders', 5, dst)], { data_issues: [issue('row_count_error', '分区明细计数失败')] })
    if (dst !== 5) r.data_issues.push(issue('row_count_mismatch', ''))
    const [entry] = utils.rowCountEntries(r)
    assert.equal(entry.comparable, true)
    assert.equal(entry.mismatch, dst !== 5)
    assert.equal(entry.detail, '分区明细计数失败')
    assert.equal(entry.color, 'red')
    const summary = utils.summarizeRowCounts(r)
    assert.equal(summary.checked, 1)
    assert.equal(summary.countErrors, 1)
  }
})

test('an independently recorded failed write page remains a copy error in a partially classified report', () => {
  const r = report([row('orders', 5, 4)], {
    data: { failed: 1, items: [{ name: 'Orders', failed_page: 3, error: 'duplicate primary key' }] },
    data_issues: [issue('row_count_mismatch', '')],
  })
  const e = utils.issuesForRepairRow(utils.indexMigrationDataIssues(r), repair())
  assert.ok(e.issues.some(i => i.kind === 'copy_error' && i.message === 'duplicate primary key'))
  assert.equal(e.pureMismatch, false)
})

test('a failed committed-row check can have two valid, equal counts', () => {
  const r = report([row('orders', 8, 8, false)], { data_issues: [issue('verification_error', '源 8，已提交 7，目标 8')] })
  const [entry] = utils.rowCountEntries(r)
  assert.equal(entry.comparable, true)
  assert.equal(entry.mismatch, false)
  assert.equal(entry.detail, '源 8，已提交 7，目标 8')
  assert.match(utils.summarizeRowCounts(r).text, /已核对 1 张表.*两端行数一致，但数据校验未通过/)
})
