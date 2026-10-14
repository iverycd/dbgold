import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'

const transpile = source => ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } }).outputText
const utilsContext = { exports: {} }
vm.runInNewContext(transpile(readFileSync(new URL('../src/utils/migrationRepair.ts', import.meta.url), 'utf8')), utilsContext)
const batchContext = { exports: {}, setTimeout, clearTimeout, require: id => {
  assert.equal(id, '@/utils/migrationRepair')
  return utilsContext.exports
} }
vm.runInNewContext(transpile(readFileSync(new URL('../src/utils/migrationRepairBatch.ts', import.meta.url), 'utf8')), batchContext)
const { batchIneligibleReason, batchBindingKey, buildRepairBatch, createRepairBatchQueue } = batchContext.exports
const clone = value => JSON.parse(JSON.stringify(value))
const binding = { src_conn_id: 1, dst_conn_id: 2, source_database: 'source', source_schema: 'src', target_schema: 'dst', configured: true, legacy: false }
const unit = (id, extra = {}) => ({ id, sql: `CREATE ${id}`, original_sql: `ORIGINAL ${id}`, status: 'failed', selected: true, destructive: false, ...extra })
const record = (id, extra = {}) => ({ version: 1, object: { id, kind: 'view', name: id, target_name: id, schema: 'dst', ddl: `OLD ${id}`, units: [unit(id)], dependencies: [], legacy: false, requires_review: true, status: 'failed', ...extra } })
const attempt = (id, status = 'succeeded', extra = {}) => ({ id: `attempt-${id}`, job_id: 'job', object_id: id, kind: 'ddl', status, units: [], created_at: '', ...extra })
const deferred = () => { let resolve, reject; const promise = new Promise((yes, no) => { resolve = yes; reject = no }); return { promise, resolve, reject } }
const waitFor = async predicate => { for (let i = 0; i < 100; i++) { if (predicate()) return; await new Promise(resolve => setTimeout(resolve, 0)) }; assert.fail('condition was not reached') }

function setup(source, overrides = {}, selected = source.map(item => item.object.id)) {
  let key = 0
  const state = { records: clone(source), updates: [], attempts: [], errors: [], calls: [], listBinding: clone(binding) }
  const items = buildRepairBatch(source, selected, () => `request-${++key}`)
  const deps = {
    pollDelayMs: 0,
    load: async () => { state.calls.push(['load']); return { list: { objects: clone(state.records), binding: clone(state.listBinding) }, jobRunning: false } },
    save: async (id, snapshot) => {
      state.calls.push(['save', id, clone(snapshot)])
      const saved = clone(snapshot); saved.version++; saved.object.requires_review = false; saved.object.status = 'draft'
      state.records[state.records.findIndex(item => item.object.id === id)] = clone(saved)
      return saved
    },
    execute: async (id, version, requestID) => {
      state.calls.push(['execute', id, version, requestID])
      state.records.find(item => item.object.id === id).object.status = 'succeeded'
      return attempt(id)
    },
    getAttempt: async id => { state.calls.push(['poll', id]); return attempt(id.replace('attempt-', '')) },
    onItems: value => state.updates.push(clone(value)),
    onAttempt: value => state.attempts.push(clone(value)),
    onError: value => state.errors.push(value),
    ...overrides,
  }
  const queue = createRepairBatchQueue(items, batchBindingKey(binding), deps)
  return { state, items, deps, queue, latest: () => state.updates.at(-1), actions: kind => state.calls.filter(call => call[0] === kind) }
}

test('eligibility rejects unknown history anywhere, non-executable objects, and blank selected SQL', () => {
  for (const status of ['unknown', 'unverified', 'needs_verification']) {
    assert.match(batchIneligibleReason(record('x', { status }).object), /未知/)
    assert.match(batchIneligibleReason(record('x', { units: [unit('first'), unit('old', { status, selected: false })] }).object), /未知/)
    assert.match(batchIneligibleReason(record('x', { data_status: status }).object), /未知/)
  }
  for (const status of ['running', 'queued', 'cancelling']) assert.match(batchIneligibleReason(record('x', { units: [unit('old', { status, selected: false })] }).object), /正在执行/)
  assert.match(batchIneligibleReason(record('x', { diagnostic_only: true }).object), /诊断/)
  assert.match(batchIneligibleReason(record('x', { status: 'succeeded' }).object), /成功/)
  assert.match(batchIneligibleReason(record('x', { schema: '' }).object), /目标对象/)
  assert.match(batchIneligibleReason(record('x', { units: [], suggested_ddl: 'CREATE VIEW x' }).object), /没有/)
  assert.match(batchIneligibleReason(record('x', { units: [unit('x'), unit('blank', { sql: ' ' })] }).object), /空 SQL/)
  assert.equal(batchIneligibleReason(record('x', { status: 'blocked', kind: 'owner' }).object), '')
})

test('batch snapshot preserves complete SQL and deselections without replaying success or destructive units', () => {
  const records = [record('x', { units: [unit('done', { status: 'succeeded' }), unit('drop', { destructive: true }), unit('off', { selected: false }), unit('on', { sql: 'BEGIN\n  do_it();\nEND;' })] })]
  const [item] = buildRepairBatch(records, ['x', 'x'], () => 'request')
  assert.equal(item.status, 'pending')
  assert.deepEqual(Array.from(item.record.object.units, step => step.selected), [false, false, false, true])
  assert.equal(item.record.object.units[3].sql, 'BEGIN\n  do_it();\nEND;')
  records[0].object.units[3].sql = 'CHANGED'
  assert.notEqual(item.record.object.units[3].sql, 'CHANGED')
  assert.equal(records[0].object.units[0].selected, true, 'source records are never mutated')
})

test('planner orders dependencies, permits stale blocked Owner, and explains missing, external, and cyclic dependencies', () => {
  const records = [
    record('owner', { kind: 'owner', status: 'blocked', dependencies: ['view'] }), record('view'),
    record('external', { dependencies: ['not-selected'] }), record('not-selected'),
    record('missing', { dependencies: ['gone'] }),
    record('cycle-a', { dependencies: ['cycle-b'] }), record('cycle-b', { dependencies: ['cycle-a'] }),
    record('downstream', { dependencies: ['cycle-a'] }),
    record('ready-owner', { kind: 'owner', status: 'blocked', dependencies: ['ready-view'] }), record('ready-view', { status: 'succeeded' }),
  ]
  const items = buildRepairBatch(records, ['owner', 'external', 'view', 'missing', 'cycle-a', 'cycle-b', 'downstream', 'ready-owner'], () => 'key')
  const byID = Object.fromEntries(items.map(item => [item.record.object.id, item]))
  assert.ok(items.indexOf(byID.view) < items.indexOf(byID.owner))
  assert.equal(byID.owner.status, 'pending')
  assert.equal(byID['ready-owner'].status, 'pending')
  assert.match(byID.external.reason, /未选入/)
  assert.match(byID.missing.reason, /缺少依赖/)
  assert.match(byID['cycle-a'].reason, /依赖环/)
  assert.match(byID['cycle-b'].reason, /依赖环/)
  assert.match(byID.downstream.reason, /依赖不可执行/)
  assert.equal(items.some(item => item.record.object.id === 'not-selected'), false)
})

test('binding identity does not depend on property insertion order and includes every execution location', () => {
  assert.equal(batchBindingKey(binding), batchBindingKey(Object.fromEntries(Object.entries(binding).reverse())))
  for (const [key, value] of Object.entries(binding)) assert.notEqual(batchBindingKey(binding), batchBindingKey({ ...binding, [key]: typeof value === 'boolean' ? !value : `${value}-changed` }))
})

test('queue waits for actual terminal progress before saving and executing a dependent', async () => {
  let polls = 0
  const h = setup([record('owner', { kind: 'owner', status: 'blocked', dependencies: ['view'] }), record('view')])
  h.deps.execute = async (id, version, requestID) => { h.state.calls.push(['execute', id, version, requestID]); return attempt(id, id === 'view' ? 'running' : 'succeeded') }
  h.deps.getAttempt = async id => {
    h.state.calls.push(['poll', id]); polls++
    if (polls < 3) return attempt('view', polls === 1 ? 'queued' : 'running')
    h.state.records.find(row => row.object.id === 'view').object.status = 'succeeded'
    return attempt('view')
  }
  await h.queue.run()
  assert.deepEqual(h.actions('execute').map(call => call.slice(1)), [['view', 2, 'request-2'], ['owner', 2, 'request-1']])
  const ownerSave = h.state.calls.findIndex(call => call[0] === 'save' && call[1] === 'owner')
  assert.equal(h.state.calls.slice(0, ownerSave).filter(call => call[0] === 'poll').length, 3)
  assert.deepEqual(h.latest().map(item => item.status), ['succeeded', 'succeeded'])
  assert.equal(h.actions('save')[0][2].object.requires_review, true, 'reviewed:true is applied by the save adapter without altering the confirmed snapshot')
  assert.equal(h.latest()[0].record.version, 1, 'confirmed version remains available for the preview')
})

test('ordinary failure skips downstream objects while independent objects continue', async () => {
  const h = setup([record('child', { dependencies: ['base'] }), record('base'), record('other')])
  h.deps.execute = async id => { h.state.calls.push(['execute', id]); return attempt(id, id === 'base' ? 'failed' : 'succeeded', { error: id === 'base' ? 'bad SQL' : '' }) }
  await h.queue.run()
  assert.deepEqual(h.actions('execute').map(call => call[1]), ['base', 'other'])
  assert.deepEqual(h.latest().map(item => item.status), ['failed', 'skipped', 'succeeded'])
  assert.match(h.latest()[1].reason, /依赖尚未执行成功/)
  assert.equal(h.state.errors.length, 0)
})

test('changed versions, SQL, target, binding, or concurrent execution halt before saving', async t => {
  for (const [name, mutate] of [
    ['version', h => h.state.records[0].version++],
    ['SQL', h => h.state.records[0].object.units[0].sql = 'ALTERED'],
    ['target', h => h.state.records[0].object.schema = 'other'],
    ['binding', h => h.state.listBinding.dst_conn_id++],
    ['unconfigured', h => h.state.listBinding.configured = false],
    ['job running', h => h.deps.load = async () => ({ list: { binding, objects: h.state.records }, jobRunning: true })],
    ['attempt running', h => h.deps.load = async () => ({ list: { binding, objects: h.state.records, running_attempt_id: 'elsewhere' }, jobRunning: false })],
  ]) await t.test(name, async () => {
    const h = setup([record('a'), record('b')]); mutate(h); await h.queue.run()
    assert.equal(h.actions('save').length, 0)
    assert.equal(h.actions('execute').length, 0)
    assert.deepEqual(h.latest().map(item => item.status), ['stopped', 'stopped'])
    assert.equal(h.state.errors.length, 1)
  })
})

test('save conflict and unexpected saved SQL cannot submit or overwrite the reviewed execution', async t => {
  await t.test('save conflict', async () => {
    const h = setup([record('a'), record('b')], { save: async () => { throw { response: { data: { error: '版本冲突' } } } } })
    await h.queue.run(); assert.equal(h.actions('execute').length, 0); assert.match(h.state.errors[0], /版本冲突/)
  })
  await t.test('changed response', async () => {
    const h = setup([record('a'), record('b')], { save: async (_, snapshot) => { snapshot.object.units[0].sql = 'CHANGED'; return snapshot } })
    await h.queue.run(); assert.equal(h.actions('execute').length, 0); assert.match(h.state.errors[0], /确认内容不一致/)
  })
})

test('load failure, submission response loss, and polling failure stop without retrying', async t => {
  for (const phase of ['load', 'execute', 'getAttempt']) await t.test(phase, async () => {
    let calls = 0
    const h = setup([record('a'), record('b')])
    if (phase === 'getAttempt') h.deps.execute = async id => attempt(id, 'running')
    h.deps[phase] = async () => { calls++; throw new Error('connection lost') }
    const first = h.queue.run()
    assert.equal(h.queue.run(), first, 'duplicate confirmation must reuse the same run')
    await first; await h.queue.run()
    assert.equal(calls, 1)
    assert.equal(h.latest()[0].status, phase === 'load' ? 'stopped' : 'unknown')
    assert.equal(h.latest()[1].status, 'stopped')
    assert.match(h.state.errors[0], /不会自动重新提交/)
  })
})

test('unknown and cancelled terminal results halt the queue, including unknown unselected units', async t => {
  for (const [status, units, result] of [['unknown', [], 'unknown'], ['failed', [unit('old', { status: 'unknown', selected: false })], 'unknown'], ['cancelled', [], 'stopped'], ['unrecognized', [], 'unknown']]) await t.test(`${status}-${units.length}`, async () => {
    const h = setup([record('a'), record('b')], { execute: async id => attempt(id, status, { units }) })
    await h.queue.run()
    assert.equal(h.latest()[0].status, result)
    assert.equal(h.latest()[1].status, 'stopped')
  })
})

test('stopping during load or save prevents the next mutation', async t => {
  for (const phase of ['load', 'save']) await t.test(phase, async () => {
    const gate = deferred(); let started = false
    const h = setup([record('a'), record('b')])
    const original = h.deps[phase]
    h.deps[phase] = async (...args) => { started = true; await gate.promise; return original(...args) }
    const run = h.queue.run(); await waitFor(() => started); h.queue.stop(); gate.resolve(); await run
    assert.equal(h.actions('execute').length, 0)
    if (phase === 'load') assert.equal(h.actions('save').length, 0)
    assert.deepEqual(h.latest().map(item => item.status), ['stopped', 'stopped'])
  })
})

test('stopping during an execute request keeps polling its accepted attempt but never submits the next object', async () => {
  const gate = deferred(); let started = false
  const h = setup([record('a'), record('b')], { execute: async id => { started = true; await gate.promise; return attempt(id, 'running') } })
  const run = h.queue.run(); await waitFor(() => started); h.queue.stop(); gate.resolve(); await run
  assert.equal(h.actions('save').length, 1)
  assert.equal(h.actions('poll').length, 1)
  assert.deepEqual(h.latest().map(item => item.status), ['succeeded', 'stopped'])
})

test('disposing at every HTTP boundary settles run and suppresses late callbacks and submissions', async t => {
  for (const phase of ['load', 'save', 'execute', 'getAttempt']) await t.test(phase, async () => {
    const gate = deferred(); let started = false
    const h = setup([record('a'), record('b')])
    if (phase === 'getAttempt') h.deps.execute = async id => attempt(id, 'running')
    const original = h.deps[phase]
    h.deps[phase] = async (...args) => { started = true; await gate.promise; return original(...args) }
    const run = h.queue.run(); await waitFor(() => started)
    h.queue.dispose(); await run
    const counts = [h.state.updates.length, h.state.attempts.length, h.state.errors.length]
    gate.resolve(); await new Promise(resolve => setTimeout(resolve, 0))
    assert.deepEqual([h.state.updates.length, h.state.attempts.length, h.state.errors.length], counts)
    assert.equal(h.actions('execute').some(call => call[1] === 'b'), false)
  })
})

test('disposing during poll delay cancels its timer and does not issue another request', async () => {
  const h = setup([record('a'), record('b')], { pollDelayMs: 60_000, execute: async id => attempt(id, 'running') })
  const run = h.queue.run(); await waitFor(() => h.latest()?.[0].status === 'running')
  h.queue.dispose(); await run
  assert.equal(h.actions('poll').length, 0)
})

test('published snapshots cannot mutate the queue confirmation or submitted SQL', async () => {
  const h = setup([record('a')])
  const original = h.deps.onItems
  h.deps.onItems = value => { original(value); value[0].record.object.units[0].sql = 'UNREVIEWED'; value[0].requestID = 'other' }
  await h.queue.run()
  assert.equal(h.actions('save')[0][2].object.units[0].sql, 'CREATE a')
  assert.equal(h.actions('execute')[0][3], 'request-1')
})
