import assert from 'node:assert/strict'
import test from 'node:test'
import { migrationReportUtils } from './helpers/migration-report.mjs'

const { failureStage, describeFailure, categoryPresentation, reportCategories, formatReportCategories, sourceIndexDefinition } = migrationReportUtils
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

test('data failures retain column diagnostics and use their category for legacy stages', () => {
  const error = 'pq: numeric field overflow\nSQLSTATE: 22003\nDetail: A field with precision 8, scale 2 must round to an absolute value less than 10^6.\nWhere: COPY cg_purchasebudgetandou, line 42, column Amount: "1000000"'
  const r = report(empty())
  r.data = { total: 1, success: 0, failed: 1, items: [failure({ name: 'cg_purchasebudgetandou', error })] }
  const item = reportCategories(r).find(row => row.key === 'data').items[0]
  assert.equal(item.stageLabel, '数据迁移失败')
  assert.equal(item.showTargetDDL, false)
  assert.equal(item.error, error)
  const text = formatReportCategories(r).join('\n')
  assert.match(text, /失败阶段：数据迁移失败/)
  assert.ok(text.includes(error))
  assert.doesNotMatch(text, /目标 DDL：|失败阶段未记录/)
})

test('data failures preserve recorded stages and target DDL when available', () => {
  const r = report(empty())
  r.data = { total: 1, success: 0, failed: 1, items: [failure({ stage: 'metadata' })] }
  assert.equal(reportCategories(r).find(row => row.key === 'data').items[0].stageLabel, '元数据读取失败')
  r.data.items = [failure({ ddl: 'CREATE TABLE t (Amount numeric(8, 2))' })]
  const item = reportCategories(r).find(row => row.key === 'data').items[0]
  assert.equal(item.showTargetDDL, true)
  assert.match(formatReportCategories(r).join('\n'), /目标 DDL：\n\s+CREATE TABLE t/)
})

test('same-name objects from different tables get distinct stable detail keys', () => {
  const r = report({ total: 3, success: 0, failed: 3, items: [failure({ table: 'Orders' }), failure({ table: 'Archive' }), failure({ table: 'Orders' })] })
  const keys = () => reportCategories(r).find(row => row.key === 'indexes').items.map(item => item.key)
  assert.equal(new Set(keys()).size, 3)
  assert.deepEqual(keys(), keys())
})

test('failed index details and exported reports identify both ends when names change', () => {
  const definition = 'UNIQUE KEY `IxExpression` ((case when (`del_flag` = \'0\') then upper(`label_code`) else NULL end))'
  const ddl = `CREATE TABLE \`Orders\` (\`id\` int, ${definition}, KEY \`Good\` (\`id\`))`
  const item = failure({ name: 'orders_ixexpression', source_name: 'IxExpression', target_name: 'orders_ixexpression', table: 'Orders', target_table: 'orders', target_schema: 'dst', stage: 'unsupported', source_ddl: ddl })
  const r = report({ total: 2, success: 1, failed: 1, items: [item] })
  const detail = reportCategories(r).find(row => row.key === 'indexes').items[0]
  assert.equal(detail.nameLabel, '索引名称：')
  assert.equal(detail.sourceIndexDDL, definition)
  assert.equal(detail.targetTableLabel, 'dst.orders')
  const text = formatReportCategories(r).join('\n')
  assert.match(text, /源索引名称：IxExpression/)
  assert.match(text, /目标索引名称（计划）：orders_ixexpression/)
  assert.match(text, /目标表：dst.orders/)
  assert.ok(text.includes(`源端失败索引定义（仅供参考，需适配目标库）：\n      ${definition}`))
  assert.match(text, /未生成目标 DDL/)
})

test('executed index details show the actual target identity alongside failed SQL', () => {
  const r = report({ total: 1, success: 0, failed: 1, items: [failure({ source_name: 'IxOrders', target_name: 'idx_orders', target_table: 'orders', stage: 'execute', ddl: 'CREATE INDEX idx_orders ON orders(id)' })] })
  const text = formatReportCategories(r).join('\n')
  assert.match(text, /目标索引名称：idx_orders/)
  assert.match(text, /目标表：orders/)
  assert.doesNotMatch(text, /（计划）|undefined/)
})

test('source index extraction preserves nested expressions, quoting and multiline definitions', () => {
  const definition = "UNIQUE KEY `Ix``Expr` (\n  (concat('a,b)''c', `Name`, 'd\\\'e'))\n) COMMENT '原始,定义'"
  const ddl = `CREATE TABLE \`Orders(archive)\` (\`Name\` varchar(100) DEFAULT 'KEY fake (x)', ${definition}, KEY \`Other\` (\`Name\`))`
  assert.equal(sourceIndexDefinition(ddl, 'Ix`Expr'), definition)
  assert.equal(sourceIndexDefinition(ddl, 'ix`expr'), definition, 'legacy normalized index names remain supported')
  assert.equal(sourceIndexDefinition(ddl, 'Missing'), '')
  assert.equal(sourceIndexDefinition('CREATE INDEX Ix ON t(id)', 'Ix'), '')
  assert.equal(sourceIndexDefinition('CREATE TABLE t(id int, KEY `Ix` (id), KEY `IX` (id))', 'ix'), '', 'ambiguous legacy names must not select another index')
})

test('legacy reports isolate their failed index without inventing missing target metadata', () => {
  const definition = 'KEY `IxOrders` ((abs(`id`)))'
  const r = report({ total: 1, success: 0, failed: 1, items: [failure({ name: 'ixorders', stage: 'unsupported', source_ddl: `CREATE TABLE t(id int, ${definition})` })] })
  const item = reportCategories(r).find(row => row.key === 'indexes').items[0]
  assert.equal(item.sourceIndexDDL, definition)
  const text = formatReportCategories(r).join('\n')
  assert.match(text, /索引名称：ixorders/)
  assert.doesNotMatch(text, /目标索引名称|目标表：|源索引名称/)
})

for (const status of ['created', 'reused']) {
  test(`single lowercase compatibility function preserves ${status} counts and definition in exports`, () => {
    const r = report(empty())
    const created = status === 'created' ? 1 : 0
    const reused = status === 'reused' ? 1 : 0
    const ddl = 'CREATE FUNCTION app.get_uuid() RETURNS varchar AS actual_definition'
    r.compatibility_functions = { total: 1, success: 1, failed: 0, created, reused, items: [
      { name: 'get_uuid', schema: 'app', status, ddl, error: '' },
    ] }
    const row = reportCategories(r).find(row => row.isCompatibility)
    assert.equal(row.totalLabel, '1')
    assert.equal(row.success, 1)
    assert.equal(row.failed, 0)
    assert.equal(row.created, created)
    assert.equal(row.reused, reused)
    assert.equal(row.items.length, 1)
    assert.equal(row.items[0].name, 'get_uuid')
    const text = formatReportCategories(r).join('\n')
    assert.match(text, /兼容函数\s+总计 1\s+成功 1\s+失败 0/)
    assert.ok(text.includes(`新建 ${created}，复用 ${reused}`))
    assert.ok(text.includes(`app.get_uuid：${created ? '新建' : '复用'}`))
    assert.ok(text.includes(ddl))
    assert.doesNotMatch(text, /GET_UUID|失败项详情|失败阶段：/)
  })
}

test('historical compatibility functions show new and reused successes with actual definitions', () => {
  const r = report(empty())
  r.compatibility_functions = { total: 2, success: 2, failed: 0, created: 1, reused: 1, items: [
    { name: 'get_uuid', schema: 'app', status: 'created', generator: 'extensions.uuid_generate_v4', ddl: 'CREATE FUNCTION app.get_uuid()', error: '' },
    { name: 'GET_UUID', schema: 'app', status: 'reused', ddl: 'EXISTING BUSINESS FUNCTION', error: '' },
  ], extensions: [{ name: 'uuid-ossp', schema: 'extensions', status: 'installed', sql: 'CREATE EXTENSION IF NOT EXISTS "uuid-ossp"' }] }
  const rows = reportCategories(r)
  const row = rows.find(row => row.key === 'compatibility_functions')
  assert.equal(row.label, '兼容函数')
  assert.equal(row.isCompatibility, true)
  assert.equal(row.failed, 0)
  assert.equal(row.created, 1)
  assert.equal(row.reused, 1)
  assert.equal(row.items.length, 2)
  const text = formatReportCategories(r).join('\n')
  for (const expected of ['兼容函数明细', '新建 1，复用 1', 'app.get_uuid：新建', 'app.GET_UUID：复用', 'EXISTING BUSINESS FUNCTION', '底层函数：extensions.uuid_generate_v4', '扩展 extensions.uuid-ossp：已安装', 'CREATE EXTENSION IF NOT EXISTS']) assert.ok(text.includes(expected), expected)
  assert.doesNotMatch(text, /失败项详情|失败阶段：/)
})

test('compatibility failure and unreadable definitions remain explicit in exports', () => {
  const r = report(empty())
  r.compatibility_functions = { total: 2, success: 1, failed: 1, created: 0, reused: 1, items: [
    { name: 'get_uuid', schema: 'app', status: 'reused', ddl: '', error: '', definition_warning: '无法读取已有函数定义: denied' },
    { name: 'GET_UUID', schema: 'app', status: 'failed', ddl: 'CREATE FUNCTION app."GET_UUID"()', error: 'transaction rolled back' },
  ], extensions: [{ name: 'uuid-ossp', schema: 'app', status: 'rolled_back', sql: 'CREATE EXTENSION', error: 'transaction rolled back' }] }
  const row = reportCategories(r).find(row => row.isCompatibility)
  assert.equal(row.failed, 1)
  const text = formatReportCategories(r).join('\n')
  for (const expected of ['无法读取已有函数定义: denied', 'transaction rolled back', 'app.GET_UUID：失败', '已回滚']) assert.ok(text.includes(expected), expected)
})

test('old and non-PG reports do not acquire compatibility function rows', () => {
  assert.equal(reportCategories(report(empty())).some(row => row.isCompatibility), false)
  assert.doesNotMatch(formatReportCategories(report(empty())).join('\n'), /兼容函数/)
})
