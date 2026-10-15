import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'

const context = { exports: {} }
const source = readFileSync(new URL('../src/utils/migrationRepair.ts', import.meta.url), 'utf8')
vm.runInNewContext(ts.transpileModule(source, {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
}).outputText, context)
const utils = context.exports
const plain = value => JSON.parse(JSON.stringify(value))
const ids = rows => Array.from(rows, row => row.object.id).sort()
const memberIDs = row => Array.from(row.members, member => member.object.id).sort()

function table(extra = {}, version = 7) {
  return { version, object: {
    id: 'table-orders', kind: 'table', name: 'ORDERS', source_name: 'ORDERS', source_table: 'ORDERS',
    target_name: 'orders', table: 'orders', schema: 'public', ddl: 'CREATE TABLE orders (id INT)',
    status: 'succeeded', original_status: 'succeeded', data_status: 'failed',
    units: [{ id: 'create-orders', sql: 'CREATE TABLE orders (id INT)', original_sql: 'CREATE TABLE orders (id INT)', status: 'succeeded', selected: false, destructive: false }],
    dependencies: [], legacy: false, requires_review: false, ...extra,
  } }
}

function data(extra = {}, version = 3) {
  return { version, object: {
    id: 'data-orders', kind: 'data', name: 'ORDERS', source_name: 'ORDERS', source_table: 'ORDERS',
    target_name: 'orders', table: 'orders', schema: 'public', ddl: '',
    status: 'failed', original_status: 'failed', data_status: 'failed',
    original_error: 'column updatetime: numeric field overflow', error: 'column updatetime: numeric field overflow',
    units: [], dependencies: ['table-orders'], legacy: false, requires_review: false, ...extra,
  } }
}

function freeze(value) {
  if (value && typeof value === 'object') {
    Object.values(value).forEach(freeze)
    Object.freeze(value)
  }
  return value
}

test('table and dependent data failure form one immutable display row with the table execution identity', () => {
  const structure = freeze(table())
  const failure = freeze(data())
  const before = JSON.stringify([structure, failure])
  for (const records of [[structure, failure], [failure, structure]]) {
    const rows = utils.groupRepairRecords(records)
    assert.equal(rows.length, 1)
    assert.equal(rows[0].object, structure.object, 'editing and recovery retain the original table object')
    assert.equal(rows[0].version, structure.version, 'saving retains the table draft version')
    assert.deepEqual(memberIDs(rows[0]), ['data-orders', 'table-orders'])
    assert.ok(rows[0].members.includes(structure))
    assert.ok(rows[0].members.includes(failure))
    assert.deepEqual(plain(utils.repairDisplayStatuses(rows[0])), [
      { label: '表结构', status: 'succeeded', text: '执行成功' },
      { label: '数据', status: 'failed', text: '导入失败，待接续' },
    ])
    assert.deepEqual(plain(utils.repairDisplayErrors(rows[0])), [{ label: '数据', text: failure.object.original_error }])
  }
  assert.equal(JSON.stringify([structure, failure]), before)
})

test('a unique explicit table dependency resolves otherwise ambiguous target names', () => {
  const chosen = table()
  const other = table({ id: 'table-other' }, 12)
  const rows = utils.groupRepairRecords([other, data(), chosen])
  assert.equal(rows.length, 2)
  assert.deepEqual(memberIDs(rows.find(row => row.object.id === chosen.object.id)), ['data-orders', 'table-orders'])
  assert.deepEqual(memberIDs(rows.find(row => row.object.id === other.object.id)), ['table-other'])
})

test('without a table dependency, exact unique target identity groups legacy rows', () => {
  for (const kind of ['table', 'tables']) {
    const structure = table({ kind })
    const failure = data({ dependencies: [] })
    const rows = utils.groupRepairRecords([failure, structure])
    assert.equal(rows.length, 1, kind)
    assert.equal(rows[0].object, structure.object)
    assert.equal(rows[0].version, structure.version)
  }
})

test('different schemas, identifier case and conflicting source identities remain separate', () => {
  const cases = [
    ['schema', { schema: 'archive' }],
    ['target case', { target_name: 'Orders', table: 'Orders' }],
    ['source case', { source_name: 'Orders', source_table: 'Orders' }],
    ['source identity', { source_name: 'OLD_ORDERS', source_table: 'OLD_ORDERS' }],
    ['inconsistent target fields', { table: 'another_table' }],
    ['inconsistent source fields', { source_table: 'ANOTHER_TABLE' }],
  ]
  for (const [name, extra] of cases) {
    for (const dependencies of [[], ['table-orders']]) {
      const rows = utils.groupRepairRecords([table(), data({ ...extra, dependencies })])
      assert.equal(rows.length, 2, `${name}, dependencies=${dependencies}`)
      assert.deepEqual(ids(rows), ['data-orders', 'table-orders'])
    }
  }
})

test('conflicting data sources cannot merge through a table with no recorded source identity', () => {
  const structure = table({ source_name: undefined, source_table: undefined })
  const first = data()
  const second = data({ id: 'data-archive', source_name: 'ARCHIVE', source_table: 'ARCHIVE' })
  for (const records of [[structure, first, second], [structure, second, first], [first, second, structure]]) {
    const rows = utils.groupRepairRecords(records)
    assert.equal(rows.length, 3)
    assert.ok(rows.every(row => row.members.length === 1), 'the first data member must not win an ambiguous source identity')
  }
})

test('ambiguous target matches and multiple table dependencies do not select an arbitrary table', () => {
  const records = [table(), table({ id: 'table-other' })]
  for (const dependencies of [[], ['table-orders', 'table-other']]) {
    const rows = utils.groupRepairRecords([...records, data({ dependencies })])
    assert.equal(rows.length, 3)
    assert.deepEqual(ids(rows), ['data-orders', 'table-orders', 'table-other'])
  }
})

test('a conflicting explicit dependency does not fall back to another same-named table', () => {
  const wrong = table({ id: 'table-wrong', target_name: 'archive', table: 'archive', source_name: 'ARCHIVE', source_table: 'ARCHIVE' })
  const rows = utils.groupRepairRecords([table(), wrong, data({ dependencies: ['table-wrong'] })])
  assert.equal(rows.length, 3)
})

test('missing and non-table dependencies cannot claim another same-named table', () => {
  const view = table({ id: 'view', kind: 'view' })
  for (const dependencies of [['missing-table'], ['view'], ['table-orders', 'missing-table']]) {
    const rows = utils.groupRepairRecords([table(), view, data({ dependencies })])
    assert.equal(rows.length, 3, dependencies.join(','))
    assert.ok(rows.every(row => row.members.length === 1))
  }
})

test('diagnostic rows and rows depending on a diagnostic table remain independent', () => {
  for (const records of [
    [table(), data({ diagnostic_only: true })],
    [table({ diagnostic_only: true }), data()],
    [table({ diagnostic_only: true }), table({ id: 'table-other' }), data()],
  ]) {
    const rows = utils.groupRepairRecords(records)
    assert.equal(rows.length, records.length)
    assert.ok(rows.every(row => row.members.length === 1))
  }
})

test('multiple data failures retain every original member and deduplicate errors only within each phase', () => {
  const same = 'permission denied'
  const structure = table({ original_error: same })
  const failures = [
    data({ id: 'data-first', original_error: same }),
    data({ id: 'data-second', original_error: same }),
    data({ id: 'data-third', original_error: 'page 2: invalid timestamp' }),
  ]
  const rows = utils.groupRepairRecords([failures[0], structure, ...failures.slice(1)])
  assert.equal(rows.length, 1)
  assert.deepEqual(memberIDs(rows[0]), ['data-first', 'data-second', 'data-third', 'table-orders'])
  const errors = plain(utils.repairDisplayErrors(rows[0]))
  assert.deepEqual(errors.filter(error => error.label === '结构'), [{ label: '结构', text: same }])
  assert.deepEqual(errors.filter(error => error.label === '数据').sort((a, b) => a.text.localeCompare(b.text)), [
    { label: '数据', text: 'page 2: invalid timestamp' },
    { label: '数据', text: same },
  ].sort((a, b) => a.text.localeCompare(b.text)))
  const attempts = [
    { id: 'attempt-table', object_id: 'table-orders', kind: 'recovery', status: 'failed' },
    ...failures.map((failure, index) => ({ id: `attempt-data-${index}`, object_id: failure.object.id, kind: 'recovery', status: 'failed' })),
    { id: 'attempt-unrelated', object_id: 'another-table', kind: 'recovery', status: 'failed' },
  ]
  const members = new Set(rows[0].members.map(member => member.object.id))
  assert.equal(attempts.filter(attempt => members.has(attempt.object_id)).length, 4, 'all historical object IDs remain available')
})

test('a standalone legacy data object keeps its recovery identity, mappings and version', () => {
  const failure = data({ dependencies: [], legacy: true, column_mapping: { UPDATED_AT: 'updatetime' } }, 14)
  const row = utils.groupRepairRecords([failure])[0]
  assert.equal(row.object, failure.object)
  assert.equal(row.version, 14)
  assert.equal(row.object.column_mapping, failure.object.column_mapping)
  assert.deepEqual(memberIDs(row), ['data-orders'])
  assert.deepEqual(plain(utils.repairDisplayStatuses(row)), [{ label: '数据', status: 'failed', text: '导入失败，待接续' }])
})

test('non-table objects retain their individual status and errors', () => {
  const index = table({ id: 'orders-index', kind: 'index', target_name: 'orders_idx', status: 'blocked', data_status: undefined, original_error: 'table unavailable' })
  const rows = utils.groupRepairRecords([table(), data(), index])
  assert.equal(rows.length, 2)
  const row = rows.find(item => item.object.id === 'orders-index')
  assert.equal(row.object, index.object)
  assert.deepEqual(plain(utils.repairDisplayStatuses(row)), [{ label: '', status: 'blocked', text: '依赖阻塞' }])
  assert.deepEqual(plain(utils.repairDisplayErrors(row)), [{ label: '', text: 'table unavailable' }])
})

test('conflicting current import states require verification and remain in data and attention filters', () => {
  const pairs = [
    ['succeeded', 'failed'], ['failed', 'succeeded'],
    ['succeeded', 'unknown'], ['unknown', 'succeeded'],
    ['succeeded', 'running'], ['succeeded', 'pending'],
  ]
  for (const [tableStatus, dataStatus] of pairs) {
    const row = utils.groupRepairRecords([
      table({ data_status: tableStatus }),
      data({ data_status: dataStatus }),
    ])[0]
    assert.equal(row.members.length, 2)
    const status = utils.repairDisplayStatuses(row).find(item => item.label === '数据')
    assert.equal(status.status, 'needs_verification', `${tableStatus}/${dataStatus}`)
    assert.equal(status.text, '数据状态待核对')
    assert.equal(utils.needsRepairDisplayAttention(row), true)
    assert.equal(utils.matchesRepairDisplayFilter(row, 'attention'), true)
    assert.equal(utils.matchesRepairDisplayFilter(row, 'data'), true)
  }
})

test('matching current data failures and unknown outcomes preserve their specific presentation', () => {
  for (const [status, text] of [['failed', '导入失败，待接续'], ['unknown', '导入结果未知']]) {
    const row = utils.groupRepairRecords([table({ data_status: status }), data({ data_status: status })])[0]
    assert.deepEqual(plain(utils.repairDisplayStatuses(row)).find(item => item.label === '数据'), { label: '数据', status, text })
    assert.equal(utils.needsRepairDisplayAttention(row), true)
  }
})

test('successful recovery converges all current member states without historical failure keeping attention', () => {
  const structure = table({ data_status: 'succeeded' })
  const failure = data({ status: 'succeeded', data_status: 'succeeded' })
  const row = utils.groupRepairRecords([structure, failure])[0]
  assert.deepEqual(plain(utils.repairDisplayStatuses(row)), [
    { label: '表结构', status: 'succeeded', text: '执行成功' },
    { label: '数据', status: 'succeeded', text: '导入成功' },
  ])
  assert.equal(utils.needsRepairDisplayAttention(row), false)
  assert.equal(utils.matchesRepairDisplayFilter(row, 'data'), false)
  assert.equal(utils.matchesRepairDisplayFilter(row, 'attention'), false)
  assert.match(utils.repairDisplayErrors(row)[0].text, /numeric field overflow/)
})

test('successful status aliases do not create a false conflict', () => {
  for (const alias of ['success', 'repaired']) {
    const row = utils.groupRepairRecords([table({ data_status: 'succeeded' }), data({ status: alias, data_status: alias })])[0]
    const status = utils.repairDisplayStatuses(row).find(item => item.label === '数据')
    assert.equal(status.text, '导入成功')
    assert.equal(utils.needsRepairDisplayAttention(row), false)
  }
})

test('display filters match member results and count each group once', () => {
  const rows = utils.groupRepairRecords([
    table({ original_status: 'failed' }),
    data(), data({ id: 'data-second' }),
  ])
  for (const filter of ['all', 'attention', 'data', 'repaired']) {
    assert.equal(rows.filter(row => utils.matchesRepairDisplayFilter(row, filter)).length, 1, filter)
  }
  assert.equal(rows.filter(row => utils.needsRepairDisplayAttention(row)).length, 1)
  const recoveredRows = utils.groupRepairRecords([
    table({ data_status: 'succeeded' }),
    data({ status: 'succeeded', data_status: 'succeeded', original_status: undefined, original_error: '', error: '' }),
  ])
  assert.equal(utils.matchesRepairDisplayFilter(recoveredRows[0], 'repaired'), false)
  assert.equal(utils.matchesRepairDisplayFilter(recoveredRows[0], 'repaired', [
    { id: 'data-recovery', object_id: 'data-orders', kind: 'recovery', status: 'succeeded' },
  ]), true, 'a recovery attempt belonging to a data member is part of the display group')
})

test('duplicate raw IDs do not duplicate group membership or category counts', () => {
  const structure = table()
  const failure = data()
  const rows = utils.groupRepairRecords([structure, failure, structure, failure])
  assert.equal(rows.length, 1)
  assert.deepEqual(memberIDs(rows[0]), ['data-orders', 'table-orders'])
  assert.equal(rows.filter(row => utils.matchesRepairDisplayFilter(row, 'data')).length, 1)
})
