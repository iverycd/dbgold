import assert from 'node:assert/strict'
import test from 'node:test'
import { migrationReportUtils } from './helpers/migration-report.mjs'

const { reportCategories, formatReportCategories, autoIncrementStatus } = migrationReportUtils
function report(overrides = {}) {
  const r = { rowCounts: [] }
  for (const key of ['tables', 'data', 'primaryKeys', 'views', 'indexes', 'constraints', 'sequences', 'comments', 'triggers']) r[key] = { total: 0, success: 0, failed: 0, items: [] }
  r.triggers.total = 5
  r.trigger_adaptation = {
    mode: 'mysql_auto_increment', recorded: true, total: 1, success: 1, failed: 0, blocked: 0, pending: 0, unknown: 0,
    items: [{ trigger: 'TG_T_ID', sequence_owner: 'APP', sequence: 'HIBERNATE_SEQUENCE', table: 'T', column: 'ID', target_table: 't', target_column: 'id', shared_sequence: true, conversion_status: 'converted', calibration_status: 'succeeded', start: '9007199254740994' }],
    ...overrides,
  }
  return r
}

test('Oracle MySQL report exposes successful conversions separately from business triggers', () => {
  const rows = reportCategories(report())
  const row = rows.find(row => row.key === 'trigger_adaptation')
  assert.equal(row.label, '自增列形式的触发器')
  assert.equal(row.successLabel, '1')
  assert.match(row.statusLabel, /AUTO_INCREMENT.*已校准/)
  assert.equal(row.autoIncrementItems.length, 1)
  assert.equal(row.autoIncrementItems[0].start, '9007199254740994')
  assert.equal(row.isInformational, true)
  assert.match(rows.find(row => row.key === 'triggers').statusLabel, /普通触发器未迁移/)
})

test('Oracle MySQL failed calibration never reports the conversion as complete', () => {
  const r = report({ success: 0, failed: 1 })
  r.trigger_adaptation.items[0].calibration_status = 'failed'
  r.trigger_adaptation.items[0].error = '权限不足'
  r.sequences.failed = 1
  const rows = reportCategories(r)
  assert.doesNotMatch(rows.find(row => row.key === 'trigger_adaptation').statusLabel, /起点已校准/)
  assert.equal(rows.reduce((count, row) => count + (row.isInformational ? 0 : row.failed), 0), 1)
  const text = formatReportCategories(r).join('\n')
  assert.match(text, /转换：已转换为 AUTO_INCREMENT；校准：失败/)
  assert.match(text, /权限不足/)
})

test('Oracle MySQL export retains exact starts and shared sequence warning', () => {
  const text = formatReportCategories(report()).join('\n')
  assert.match(text, /TG_T_ID：APP.HIBERNATE_SEQUENCE → t.id AUTO_INCREMENT/)
  assert.match(text, /9007199254740994/)
  assert.match(text, /各表独立自增，新 ID 不再保证跨表唯一/)
})

test('historical reports and disabled triggers are not inferred to be successful', () => {
  const row = reportCategories(report({ recorded: false, items: [] })).find(row => row.key === 'trigger_adaptation')
  assert.equal(row.totalLabel, '未记录')
  assert.equal(row.successLabel, '—')
  assert.equal(row.autoIncrementItems.length, 0)
  assert.equal(autoIncrementStatus('disabled'), '源触发器已禁用，未转换')
})
