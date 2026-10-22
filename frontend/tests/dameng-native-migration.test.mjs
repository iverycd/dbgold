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

test('Dameng native task submits complete schema and preserves an explicit overwrite choice', async () => {
  const scope = Vue.effectScope()
  const { descriptor } = parse(readFileSync(new URL('../src/views/MigrationView.vue', import.meta.url), 'utf8'))
  const page = scope.run(() => execute(`${descriptor.scriptSetup.content}\nexport { connections, dataMigrate, isDamengNative, dataMigrationRequest };`, id => {
    if (id === 'vue') return { ...Vue, provide() {}, onMounted() {}, onUnmounted() {} }
    if (id === 'vue-router') return { useRoute: () => ({ query: {} }), onBeforeRouteLeave() {} }
    if (id === '@/utils/connectionDatabase') return connectionDatabaseUtils
    if (id === '@/composables/connectionDatabaseEditors') return connectionDatabaseEditors
    if (id === '@/utils/migrationOptions') return { applyTargetMigrationDefaults() {} }
    return {}
  }))
  try {
    page.connections.value = [{ id: 1, db_type: 'dameng' }, { id: 2, db_type: 'dameng' }]
    Object.assign(page.dataMigrate, { srcConnId: 1, dstConnId: 2, srcDatabase: 'MixedOwner', dstSchema: 'TargetOwner' })
    await Vue.nextTick()
    assert.equal(page.isDamengNative.value, true)
    const initial = page.dataMigrationRequest()
    assert.equal(initial.overwrite_existing, false)
    assert.equal(initial.migrate_mode, 'all')
    assert.equal(initial.migrate_content, 'both')
    assert.equal(initial.migrate_triggers, true)
    assert.equal(initial.src_database, 'MixedOwner')
    assert.equal(initial.target_schema, 'TargetOwner')
    for (const flag of ['lower_case_names', 'char_in_length', 'use_nvarchar2', 'distributed', 'change_owner']) assert.equal(initial[flag], false)
    page.dataMigrate.overwriteExisting = true
    assert.equal(page.dataMigrationRequest().overwrite_existing, true)
  } finally { scope.stop() }
})

test('native extra objects remain reportable and verification errors require attention', () => {
  const report = { native_schema: { categories: [{ kind: 'package_body', label: '包体', total: 2, success: 1, failed: 1, items: [{ name: 'PKG', ddl: 'CREATE PACKAGE BODY PKG ...', error: 'compile error', stage: 'execute' }] }], verification: [{ severity: 'error', reason: 'missing synonym' }] } }
  const row = migrationReportUtils.reportCategories(report).find(row => row.key === 'native:package_body')
  assert.equal(row.failed, 1)
  assert.equal(row.items[0].repairKind, 'package_body')
  assert.match(migrationReportUtils.formatReportCategories(report).join('\n'), /包体.*总计 2.*成功 1.*失败 1/)
  assert.equal(migrationReportUtils.nativeSchemaNeedsAttention(report), true)
  report.native_schema.verification[0].severity = 'warning'
  assert.equal(migrationReportUtils.nativeSchemaNeedsAttention(report), false)
  report.native_schema.preflight = { ready: false }
  assert.equal(migrationReportUtils.nativeSchemaNeedsAttention(report), true)
})

test('native batch overwrite serializes false by default and true when selected', async () => {
  const requests = []
  const api = execute(readFileSync(new URL('../src/api/migration.ts', import.meta.url), 'utf8'), () => ({ default: { post: async (url, data) => { requests.push(data); return {} } } }))
  for (const choice of [undefined, true]) {
    await api.startBatch(new Blob(['excel']), [], { migrate_content: 'both', overwrite_existing: choice })
    assert.equal(requests.at(-1).get('overwrite_existing'), String(!!choice))
  }
})

test('native procedural retry is read-only, refuses changed units and selects only unfinished units', () => {
  const utils = execute(readFileSync(new URL('../src/utils/migrationRepair.ts', import.meta.url), 'utf8'))
  const object = { native: { schema: 'Dst', name: 'P', units: [{ sql: 'CREATE PROCEDURE P' }, { sql: 'ALTER PROCEDURE P COMPILE' }] }, native_fingerprint: 'server-fingerprint', schema: 'Dst', target_name: 'P', kind: 'procedure', status: 'failed', units: [{ sql: 'CREATE PROCEDURE P', original_sql: 'CREATE PROCEDURE P', status: 'succeeded' }, { sql: 'ALTER PROCEDURE P COMPILE', original_sql: 'ALTER PROCEDURE P COMPILE', status: 'failed' }] }
  assert.equal(utils.canEditRepairDDL(object), false)
  assert.equal(utils.canExecuteRepairDDL(object), true)
  assert.deepEqual([...utils.cloneRepairExecutionUnits(object).map(unit => unit.selected)], [false, true])
  object.status = 'unknown'
  object.units[1].status = 'unknown'
  assert.equal(utils.canExecuteRepairDDL(object), true, '可信原生计划由服务端先目录对账，再决定是否可重试')
  assert.deepEqual([...utils.cloneRepairExecutionUnits(object).map(unit => unit.selected)], [false, true])
  object.units[1].sql += ' changed'
  assert.equal(utils.canExecuteRepairDDL(object), false)
})
