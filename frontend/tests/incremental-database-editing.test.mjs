import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import * as Vue from 'vue'
import { parse } from '@vue/compiler-sfc'

const { descriptor } = parse(readFileSync(new URL('../src/views/IncrementalMigrationPanel.vue', import.meta.url), 'utf8'))
const source = ts.transpileModule(`${descriptor.scriptSetup.content}
  export { form, schemas, ready, checking, starting, preflightResult, missingTableModalVisible,
    selectedMissingTables, preflight, start, retryAfterRepair, excludeMissingAndRetry };`, {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
}).outputText

const flushPromises = () => new Promise(resolve => setImmediate(resolve))

const deferred = () => {
  let resolve, reject
  const promise = new Promise((res, rej) => { resolve = res; reject = rej })
  return { promise, resolve, reject }
}

function panel(overrides = {}) {
  const props = Vue.reactive({ connections: [
    { id: 1, db_type: 'mysql', database: 'SourceCase', host: 'source' },
    { id: 2, db_type: 'postgres', database: 'TargetCase', host: 'target' },
    { id: 3, db_type: 'postgres', database: 'OtherTarget', host: 'other-target' },
  ] })
  const blocked = Vue.reactive(new Set())
  const locks = new Map()
  const checks = [], starts = [], errors = [], successes = [], lifecycle = {}
  const store = {
    isBlocked: id => blocked.has(id),
    acquireLock: ids => {
      for (const id of ids) locks.set(id, (locks.get(id) || 0) + 1)
      return () => { for (const id of ids) locks.set(id, locks.get(id) - 1) }
    },
  }
  const context = {
    exports: {},
    defineProps: () => props,
    require: id => {
      if (id === 'vue') return { ...Vue, onUnmounted: fn => { lifecycle.unmount = fn } }
      if (id === '@/composables/connectionDatabaseEditors') return { useConnectionDatabaseEditors: () => store }
      if (id === '@/api/connections') return {
        listConnectionSchemas: overrides.schemas || (async () => ({ data: ['public'] })),
      }
      if (id === '@/api/migration') return {
        preflightIncremental: request => {
          checks.push(structuredClone(JSON.parse(JSON.stringify(request))))
          return overrides.preflight ? overrides.preflight(request) : Promise.resolve({ data: { ok: true } })
        },
        startIncremental: request => {
          starts.push(structuredClone(JSON.parse(JSON.stringify(request))))
          return overrides.start ? overrides.start(request) : Promise.resolve({ data: {} })
        },
      }
      if (id === '@arco-design/web-vue') return {
        Message: { error: value => errors.push(value), success: value => successes.push(value) },
        Modal: { error: value => errors.push(value.content) },
      }
      throw new Error(`Unexpected import ${id}`)
    },
  }
  vm.runInNewContext(source, context)
  const p = context.exports
  Object.assign(p.form, { src_conn_id: 1, dst_conn_id: 2, target_schema: 'public' })
  return { p, props, blocked, locks, checks, starts, errors, successes, lifecycle }
}

async function withPanel(run, overrides) {
  const scope = Vue.effectScope()
  try { await run(scope.run(() => panel(overrides))) } finally { scope.stop() }
}

test('incremental database saves synchronize source names and invalidate schemas, exclusions and preflight', async () => {
  await withPanel(async ({ p, props, checks }) => {
    assert.equal(p.form.src_database, 'SourceCase')
    assert.equal(p.ready.value, true)
    await p.preflight()
    assert.equal(p.preflightResult.value.ok, true)
    p.form.excluded_tables = ['OldTable']
    p.preflightResult.value = { ok: true }
    p.missingTableModalVisible.value = true
    p.selectedMissingTables.value = ['OldTable']
    props.connections[0] = { ...props.connections[0], database: 'NewSourceCase' }
    assert.equal(p.form.src_database, 'NewSourceCase')
    assert.equal(p.preflightResult.value, null)
    assert.equal(p.form.excluded_tables.length, 0)
    assert.equal(p.missingTableModalVisible.value, false)
    assert.equal(p.selectedMissingTables.value.length, 0)
    props.connections[1] = { ...props.connections[1], database: 'NewTargetCase' }
    assert.equal(p.form.target_schema, '')
    assert.equal(p.schemas.value.length, 0)
    assert.equal(p.ready.value, false)
    await flushPromises()
    assert.deepEqual(Array.from(p.schemas.value), ['public'])
    p.form.target_schema = 'new_schema'
    await p.preflight()
    assert.equal(checks.at(-1).src_database, 'NewSourceCase')
    assert.equal(checks.at(-1).target_schema, 'new_schema')
  })
})

test('changing either database or connection resets the selected table scope', async () => {
  for (const change of [
    ({ props }) => { props.connections[0].database = 'NewSourceCase' },
    ({ props }) => { props.connections[1] = { ...props.connections[1], database: 'NewTargetCase' } },
    ({ p, props }) => {
      props.connections.push({ ...props.connections[0], id: 4 })
      p.form.src_conn_id = 4
    },
    ({ p }) => { p.form.dst_conn_id = 3 },
    ({ props }) => { props.connections[0].host = 'another-source-host' },
    ({ props }) => { props.connections[1].username = 'another-owner' },
  ]) {
    await withPanel(async context => {
      const { p } = context
      p.form.migrate_mode = 'include'
      p.form.table_filter = 'OldTable,*History'
      change(context)
      assert.equal(p.form.table_filter, '')
      assert.equal(p.form.migrate_mode, 'all')
    })
  }
})

test('connection display-name and environment refreshes preserve the selected table scope', async () => {
  await withPanel(async ({ p, props }) => {
    p.form.migrate_mode = 'exclude'
    p.form.table_filter = 'AuditLog,*History'
    props.connections[0] = { ...props.connections[0], name: 'Renamed source', env: 'production' }
    props.connections[1].name = 'Renamed target'
    assert.equal(p.form.table_filter, 'AuditLog,*History')
    assert.equal(p.form.migrate_mode, 'exclude')
  })
})

test('dirty or saving databases immediately invalidate preflight and prevent checks, retries and starts', async () => {
  await withPanel(async ({ p, blocked, checks, starts }) => {
    await p.preflight()
    assert.equal(p.preflightResult.value.ok, true)
    blocked.add(2)
    assert.equal(p.ready.value, false)
    assert.equal(p.preflightResult.value, null)
    // Even a stale result or programmatic invocation cannot bypass the guard.
    p.preflightResult.value = { ok: true }
    await p.preflight()
    await p.retryAfterRepair()
    await p.excludeMissingAndRetry()
    await p.start()
    assert.equal(checks.length, 1)
    assert.equal(starts.length, 0)
    blocked.delete(2)
    await p.preflight()
    await p.start()
    assert.equal(checks.length, 2)
    assert.equal(starts.length, 1)
    assert.equal(starts[0].src_database, 'SourceCase')
    assert.equal(p.preflightResult.value, null)
  })
})

test('a late schema response cannot replace the list from a newly selected target or saved catalog', async () => {
  const requests = []
  await withPanel(async ({ p, props }) => {
    p.form.dst_conn_id = 3
    requests[1].resolve({ data: ['current_schema'] })
    await flushPromises()
    p.form.target_schema = 'current_schema'
    requests[0].resolve({ data: ['old_schema'] })
    await flushPromises()
    assert.deepEqual(Array.from(p.schemas.value), ['current_schema'])
    assert.equal(p.form.target_schema, 'current_schema')
    props.connections[2].database = 'SavedCatalog'
    assert.equal(p.form.target_schema, '')
    requests[2].resolve({ data: ['saved_schema'] })
    await flushPromises()
    assert.deepEqual(Array.from(p.schemas.value), ['saved_schema'])
  }, { schemas: () => { const request = deferred(); requests.push(request); return request.promise } })
})

test('old successful or failed preflights cannot overwrite newer configuration and locks are released', async () => {
  const requests = []
  await withPanel(async ({ p, props, locks, errors }) => {
    const oldCheck = p.preflight()
    assert.equal(locks.get(1), 1)
    assert.equal(locks.get(2), 1)
    props.connections[0].database = 'NewSource'
    assert.equal(p.checking.value, false)
    const currentCheck = p.preflight()
    assert.equal(locks.get(1), 2)
    requests[1].resolve({ data: { ok: true, marker: 'current' } })
    await currentCheck
    requests[0].resolve({ data: { ok: false, missing_target_tables: [{ source_table: 'stale' }] } })
    await oldCheck
    assert.equal(p.preflightResult.value.marker, 'current')
    assert.equal(p.missingTableModalVisible.value, false)
    assert.equal(p.checking.value, false)
    assert.equal(locks.get(1), 0)
    const lateFailure = p.preflight()
    p.form.table_filter = 'new-*'
    requests[2].reject({ response: { data: { error: 'stale error', preflight: { ok: false } } } })
    await lateFailure
    assert.equal(p.preflightResult.value, null)
    assert.equal(errors.length, 0)
    assert.equal(locks.get(2), 0)
  }, { preflight: () => { const request = deferred(); requests.push(request); return request.promise } })
})

test('incremental start locks both connections until its snapshot request completes', async () => {
  const pending = deferred()
  await withPanel(async ({ p, locks, starts, props }) => {
    await p.start()
    assert.equal(starts.length, 0)
    await p.preflight()
    const running = p.start()
    assert.equal(p.starting.value, true)
    assert.equal(locks.get(1), 1)
    assert.equal(locks.get(2), 1)
    await p.start()
    assert.equal(starts.length, 1)
    props.connections[0].database = 'LaterDatabase'
    assert.equal(starts[0].src_database, 'SourceCase')
    pending.resolve({ data: {} })
    await running
    assert.equal(p.starting.value, false)
    assert.equal(locks.get(1), 0)
    assert.equal(locks.get(2), 0)
  }, { start: () => pending.promise })
})

test('unmounted incremental panels discard pending schema and preflight results', async () => {
  const schema = deferred(), check = deferred()
  await withPanel(async ({ p, lifecycle, successes, locks }) => {
    const checking = p.preflight()
    lifecycle.unmount()
    schema.resolve({ data: ['stale'] })
    check.resolve({ data: { ok: true } })
    await checking
    assert.equal(p.schemas.value.length, 0)
    assert.equal(p.preflightResult.value, null)
    assert.equal(successes.length, 0)
    assert.equal(locks.get(1), 0)
  }, { schemas: () => schema.promise, preflight: () => check.promise })
})


test('a connection conflict during start requires a new preflight and releases editor locks', async () => {
  await withPanel(async ({ p, locks, starts, errors }) => {
    await p.preflight()
    await p.start()
    assert.equal(p.preflightResult.value, null)
    assert.equal(p.starting.value, false)
    assert.equal(locks.get(1), 0)
    assert.equal(locks.get(2), 0)
    assert.deepEqual(errors, ['连接已变更，请重新预检'])
    await p.start()
    assert.equal(starts.length, 1)
  }, { start: async () => { throw { response: { status: 409, data: { error: '连接已变更，请重新预检' } } } } })
})
