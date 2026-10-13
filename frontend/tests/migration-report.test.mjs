import assert from 'node:assert/strict'
import test from 'node:test'
import { migrationReportUtils } from './helpers/migration-report.mjs'

const { failureStage, describeFailure, categoryPresentation, reportCategories, formatReportCategories } = migrationReportUtils
const empty = () => ({ total: 0, success: 0, failed: 0, items: [] })
const failure = overrides => ({ name: 'idx_orders', ddl: '', error: 'index error', ...overrides })
function report(indexes) {
  const result = { rowCounts: [] }
  for (const key of ['tables', 'data', 'primaryKeys', 'views', 'indexes', 'constraints', 'sequences', 'comments', 'triggers']) result[key] = empty()
  result.indexes = indexes
  return result
}

for (const [label, category] of [
  ['new metadata failure', { total: 0, total_unknown: true, success: 0, failed: 1, items: [failure({ name: '元数据读取', stage: 'metadata' })] }],
  ['legacy metadata failure', { total: 1, success: 0, failed: 1, items: [failure({ name: '元数据读取' })] }],
]) {
  test(`${label} shows unknown object counts and preserves failure event count`, () => {
    const r = report(category)
    const row = reportCategories(r).find(row => row.key === 'indexes')
    assert.equal(row.totalLabel, '未知')
    assert.equal(row.failedLabel, '未知')
    assert.equal(row.failed, 1)
    assert.equal(row.statusLabel, '元数据读取失败')
    assert.equal(row.items[0].stageLabel, '元数据读取失败')
    assert.match(row.items[0].emptyDDLMessage, /尚未生成目标 DDL/)
    const text = formatReportCategories(r).join('\n')
    assert.match(text, /索引\s+总计 未知\s+成功 0\s+失败 未知\s+状态：元数据读取失败/)
    assert.match(text, /失败阶段：元数据读取失败/)
    assert.doesNotMatch(text, /索引\s+总计 1/)
  })
}

test('unsupported objects retain known counts and source-only DDL is clearly separated', () => {
  const ddl = 'CREATE TABLE `Orders` (`id` int, KEY `idx_expr` ((abs(`id`))))'
  const item = failure({ name: 'idx_expr', table: 'Orders', stage: 'unsupported', source_ddl: ddl })
  const r = report({ total: 5, success: 4, failed: 1, items: [item] })
  const row = reportCategories(r).find(row => row.key === 'indexes')
  assert.equal(row.totalLabel, '5')
  assert.equal(row.failedLabel, '1')
  assert.equal(row.items[0].stageLabel, '不支持自动转换')
  const text = formatReportCategories(r).join('\n')
  assert.match(text, /所属表：Orders/)
  assert.match(text, /源端表定义（仅供参考，需适配目标库）：/)
  assert.ok(text.includes(ddl))
  assert.match(text, /目标 DDL：该对象暂不支持自动转换，未生成目标 DDL。/)
  assert.doesNotMatch(text, /元数据读取失败/)
})

test('execution, generic no-DDL and legacy cases are not misclassified as metadata failures', () => {
  assert.equal(describeFailure(failure({ stage: 'execute' })).stageLabel, 'DDL 执行失败')
  assert.equal(describeFailure(failure({})).emptyDDLMessage, '未记录 DDL。')
  assert.equal(failureStage(failure({ name: '元数据读取', stage: 'execute' })), 'execute')
  assert.equal(failureStage(failure({ name: '元数据读取', ddl: 'CREATE INDEX idx ON t(id)' })), undefined)
  const r = report({ total: 2, success: 1, failed: 1, items: [failure({ stage: 'execute', ddl: 'CREATE INDEX idx ON t(id)' })] })
  const text = formatReportCategories(r).join('\n')
  assert.match(text, /失败阶段：DDL 执行失败/)
  assert.match(text, /目标 DDL：\n\s+CREATE INDEX idx ON t\(id\)/)
  assert.doesNotMatch(text, /未知|元数据读取失败/)
})

test('unknown counts without metadata details are never labeled successful', () => {
  assert.equal(categoryPresentation({ ...empty(), total_unknown: true }).statusLabel, '对象总数未知')
})

test('same-name objects from different tables get distinct stable detail keys', () => {
  const r = report({ total: 3, success: 0, failed: 3, items: [failure({ table: 'Orders' }), failure({ table: 'Archive' }), failure({ table: 'Orders' })] })
  const keys = () => reportCategories(r).find(row => row.key === 'indexes').items.map(item => item.key)
  assert.equal(new Set(keys()).size, 3)
  assert.deepEqual(keys(), keys())
})
