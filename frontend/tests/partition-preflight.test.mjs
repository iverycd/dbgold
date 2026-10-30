import { connectionDatabaseUtils, connectionDatabaseEditors } from './helpers/connection-database.mjs'
import { migrationReportUtils } from './helpers/migration-report.mjs'
import { partitionPreflightResponse } from './helpers/partition-preflight.mjs'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import * as Vue from 'vue'
import { parse } from '@vue/compiler-sfc'

function load(source, require) {
  const result = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } })
  const context = { exports: {}, require, AbortController, Blob, FormData }
  vm.runInNewContext(result.outputText, context)
  return context.exports
}
function page(preflight) {
  const starts = [], errors = []
  const { descriptor } = parse(readFileSync(new URL('../src/views/MigrationView.vue', import.meta.url), 'utf8'))
  const p = load(descriptor.scriptSetup.content + '\nexport { connections, dataMigrate, startDataMigration, partitionCheck, partitionCheckBusy, runPartitionCheck, resetDataMigration, pgChecks };', id => {
    switch (id) {
      case 'vue': return { ...Vue, onMounted() {}, onUnmounted() {}, provide() {} }
      case 'vue-router': return { useRoute: () => ({ query: {} }), onBeforeRouteLeave() {} }
      case '@arco-design/web-vue': return { Message: { error: error => errors.push(error) }, Modal: {} }
      case '@/utils/connectionDatabase': return connectionDatabaseUtils
      case '@/composables/connectionDatabaseEditors': return connectionDatabaseEditors
      case '@/utils/migrationOptions': return { applyTargetMigrationDefaults() {} }
      case '@/utils/migrationReport': return migrationReportUtils
      case '@/api/migration': return {
        preflightDataMigration: preflight,
        startDataMigration: async request => { starts.push(request); return { data: { job_id: 'job' } } },
        createDataMigrateEventSource: () => ({ addEventListener() {}, close() {} }),
      }
      default: return {}
    }
  })
  p.connections.value = [{ id: 1, db_type: 'mysql', database: 'source' }, { id: 2, db_type: 'postgres', database: 'target' }]
  Object.assign(p.dataMigrate, { srcConnId: 1, dstConnId: 2, srcDatabase: 'source', dstSchema: 'public' })
  return { p, starts, errors }
}

test('generic migration uses can_start for partial table readiness and retains configuration-bound preview', async () => {
  const scope = Vue.effectScope()
  try {
    let checks = 0
    const { p, starts, errors } = scope.run(() => page(async () => { checks++; return partitionPreflightResponse({ ready: false, can_start: true, tables: [{ source_table: 'Blocked', target_table: 'blocked', status: 'blocked', error: 'no native equivalent' }, { source_table: 'Healthy', target_table: 'healthy', status: 'ordinary' }] }) }))
    await Vue.nextTick()
    await p.runPartitionCheck()
    assert.equal(checks, 1)
    await p.startDataMigration()
    assert.equal(checks, 1)
    assert.equal(starts.length, 1)
    assert.equal(p.partitionCheck.value.tables[0].status, 'blocked')
    assert.deepEqual(errors, [])
  } finally { scope.stop() }
})

test('all-blocked generic preflight prevents launch and configuration changes invalidate it', async () => {
  const scope = Vue.effectScope()
  try {
    let ready = false, checks = 0
    const { p, starts } = scope.run(() => page(async () => { checks++; return partitionPreflightResponse({ ready, can_start: ready }) }))
    await Vue.nextTick()
    await p.startDataMigration()
    assert.equal(starts.length, 0)
    assert.equal(p.dataMigrate.running, false)
    assert.match(p.dataMigrate.logs.at(-1), /分区预检未通过/)
    p.dataMigrate.filter = 'Healthy'; ready = true
    await Vue.nextTick()
    assert.equal(p.partitionCheck.value, null)
    await p.startDataMigration()
    assert.equal(checks, 2)
    assert.equal(starts.length, 1)
  } finally { scope.stop() }
})

test('generic preflight aborts outdated requests and ignores late readiness', async () => {
  const scope = Vue.effectScope()
  try {
    let resolve, signal
    const { p } = scope.run(() => page((_, s) => { signal = s; return new Promise(done => { resolve = done }) }))
    await Vue.nextTick()
    const pending = p.runPartitionCheck()
    p.dataMigrate.lowerCaseNames = !p.dataMigrate.lowerCaseNames
    await Vue.nextTick()
    assert.equal(signal.aborted, true)
    resolve(partitionPreflightResponse())
    await pending
    assert.equal(p.partitionCheck.value, null)
    assert.equal(p.partitionCheckBusy.value, false)
  } finally { scope.stop() }
})

test('generic malformed readiness cannot bypass missing partition evidence', async () => {
  const scope = Vue.effectScope()
  try {
    const { p, starts, errors } = scope.run(() => page(async () => ({ data: { ready: true } })))
    await Vue.nextTick()
    await p.startDataMigration()
    assert.equal(starts.length, 0)
    assert.match(errors[0], /未返回分区预检/)
  } finally { scope.stop() }
})

for (const canStart of [true, false]) test(`PG/MySQL honors partition can_start=${canStart} while legacy ready is false`, async () => {
  const scope = Vue.effectScope()
  try {
    const { p, starts } = scope.run(() => page(async () => partitionPreflightResponse({ ready: false, can_start: canStart })))
    p.connections.value[0].db_type = 'postgres'; p.connections.value[1].db_type = 'mysql'
    await Vue.nextTick()
    await p.startDataMigration()
    assert.equal(starts.length, canStart ? 1 : 0)
  } finally { scope.stop() }
})

test('partition details show exact mapping counts, append baselines and native SQL Server ordinals', () => {
  const { descriptor } = parse(readFileSync(new URL('../src/views/PartitionPreflightPanel.vue', import.meta.url), 'utf8'))
  const source = descriptor.scriptSetup.content.replace(/const props = defineProps[^\n]+/, 'const props = require("test-props")').replace(/defineEmits[^\n]+/, '')
  const panel = load(source + '\nexport { mappingRows, statusLabel, plannedSQL };', id => id === 'vue' ? Vue : {})
  const table = {
    plan: { mapping: [{ source_schema: 'Source', source_name: 'P', target_name: '2', target_ordinal: 2 }], statements: [{ SQL: 'CREATE PARTITION FUNCTION ...' }] },
    source_rows: { 'Source.P': 4 }, target_rows: { '2': 9 }, baseline: { '2': 5 },
  }
  const rows = panel.mappingRows(table)
  assert.equal(rows[0].sourceRows, 4)
  assert.equal(rows[0].baselineRows, 5)
  assert.equal(rows[0].addedRows, 4)
  assert.equal(rows[0].target, '原生分区 2')
  assert.equal(panel.statusLabel('blocked'), '已阻止')
  assert.match(panel.plannedSQL(table), /CREATE PARTITION FUNCTION/)
  const interval = { plan: { interval_materialization: { seeds: [{ partition_name: 'P_AUTO' }] }, statements: [
    { SQL: 'CREATE TABLE t(k INT) PARTITION BY RANGE(k) INTERVAL(10)' },
    { SQL: 'ALTER TABLE "s"."t" /* DBGOLD_INTERVAL_MATERIALIZE frozen-plan */' },
  ] } }
  assert.match(panel.plannedSQL(interval), /INTERVAL\(10\)/)
  assert.doesNotMatch(panel.plannedSQL(interval), /DBGOLD_INTERVAL_MATERIALIZE/)
  const tables = descriptor.template.content.match(/<a-table\s[\s\S]*?>/g)
  assert.equal(tables.length, 2)
  for (const tableTemplate of tables) assert.match(tableTemplate, /pageSize: 10/)
})
