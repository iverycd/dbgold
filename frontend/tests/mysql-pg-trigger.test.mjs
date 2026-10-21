import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import * as Vue from 'vue'
import { parse } from '@vue/compiler-sfc'
import { connectionDatabaseUtils, connectionDatabaseEditors } from './helpers/connection-database.mjs'
import { migrationReportUtils } from './helpers/migration-report.mjs'

function execute(source, require = () => ({})) {
  const context = { exports: {}, require, FormData, Blob }
  vm.runInNewContext(ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } }).outputText, context)
  return context.exports
}

test('trigger capability controls options and resets data-only or unsupported selections', async () => {
  const scope = Vue.effectScope()
  const { descriptor } = parse(readFileSync(new URL('../src/views/MigrationView.vue', import.meta.url), 'utf8'))
  const page = scope.run(() => execute(`${descriptor.scriptSetup.content}\nexport { connections, supportedPairs, dataMigrate, objMigrate, dataSupportsTriggers, objectTypeOptions, dataMigrationRequest };`, id => {
    if (id === 'vue') return { ...Vue, provide() {}, onMounted() {}, onUnmounted() {} }
    if (id === 'vue-router') return { useRoute: () => ({ query: {} }), onBeforeRouteLeave() {} }
    if (id === '@/utils/connectionDatabase') return connectionDatabaseUtils
    if (id === '@/composables/connectionDatabaseEditors') return connectionDatabaseEditors
    if (id === '@/utils/migrationOptions') return { applyTargetMigrationDefaults() {} }
    return {}
  }))
  try {
    page.connections.value = [{ id: 1, db_type: 'mysql' }, { id: 2, db_type: 'postgres' }, { id: 3, db_type: 'dameng' }]
    page.supportedPairs.value = [{ source: 'mysql', target: 'postgres', supports_triggers: true }, { source: 'mysql', target: 'dameng', supports_triggers: false }]
    Object.assign(page.dataMigrate, { srcConnId: 1, dstConnId: 2 })
    Object.assign(page.objMigrate, { srcConnId: 1, dstConnId: 2 })
    await Vue.nextTick()
    assert.equal(page.dataMigrate.migrateTriggers, false)
    assert.equal(page.dataSupportsTriggers.value, true)
    assert.ok(page.objectTypeOptions.value.some(option => option.value === 'triggers'))
    page.dataMigrate.migrateTriggers = true
    assert.equal(page.dataMigrationRequest().migrate_triggers, true)
    page.dataMigrate.content = 'data_only'
    await Vue.nextTick()
    assert.equal(page.dataMigrate.migrateTriggers, false)
    page.objMigrate.objects = ['indexes', 'triggers']
    page.objMigrate.dstConnId = 3
    page.dataMigrate.dstConnId = 3
    await Vue.nextTick()
    assert.equal(page.dataSupportsTriggers.value, false)
    assert.deepEqual([...page.objMigrate.objects], ['indexes'])
    assert.ok(!page.objectTypeOptions.value.some(option => option.value === 'triggers'))
  } finally { scope.stop() }
})

test('batch multipart explicitly preserves the trigger opt-in and defaults off', async () => {
  const requests = []
  const api = execute(readFileSync(new URL('../src/api/migration.ts', import.meta.url), 'utf8'), () => ({ default: { post: async (url, data) => { requests.push(data); return {} } } }))
  for (const enabled of [undefined, true]) {
    await api.startBatch(new Blob(['excel']), [], { migrate_content: 'both', migrate_triggers: enabled })
    assert.equal(requests.at(-1).get('migrate_triggers'), String(!!enabled))
  }
})

const category = () => ({ total: 0, success: 0, failed: 0, items: [] })
function report(enabled) {
  const value = { trigger_migration_enabled: enabled, rowCounts: [] }
  for (const key of ['tables', 'data', 'primaryKeys', 'views', 'indexes', 'constraints', 'sequences', 'comments', 'triggers']) value[key] = category()
  value.triggers = { total: 3, success: 2, failed: 1, items: [{ name: 'trg_audit', source_name: 'Trg_Audit', table: 'Orders', stage: 'unsupported', ddl: '', source_ddl: 'CREATE TRIGGER Trg_Audit ...', error: '不支持游标' }] }
  return value
}

test('real trigger results contribute failures and details while old reports stay inventory only', () => {
  for (const enabled of [false, true]) {
    const value = report(enabled)
    const row = migrationReportUtils.reportCategories(value).find(item => item.key === 'triggers')
    assert.equal(row.isInformational, !enabled)
    assert.equal(row.success, enabled ? 2 : 0)
    assert.equal(row.failed, enabled ? 1 : 0)
    assert.equal(row.items.length, enabled ? 1 : 0)
    const text = migrationReportUtils.formatReportCategories(value).join('\n')
    if (enabled) {
      assert.match(text, /触发器\s+总计 3\s+成功 2\s+失败 1/)
      assert.match(text, /源端触发器定义/)
      assert.match(text, /不支持游标/)
    } else assert.match(text, /触发器\s+总计 3\s+（未迁移）/)
  }
})


test('interrupted trigger inventories never claim all succeeded while work remains', () => {
  for (const [success, failed, expected] of [[0, 0, '尚未执行，待执行3个'], [1, 0, '部分完成，待执行2个'], [1, 1, '部分失败，待执行1个']]) {
    const value = report(true)
    Object.assign(value.triggers, { success, failed, items: [] })
    const row = migrationReportUtils.reportCategories(value).find(item => item.key === 'triggers')
    assert.equal(row.statusLabel, expected)
    assert.notEqual(row.statusColor, 'green')
    assert.ok(migrationReportUtils.formatReportCategories(value).join('\n').includes(expected))
  }
})
