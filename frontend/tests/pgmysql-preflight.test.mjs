import { connectionDatabaseUtils, connectionDatabaseEditors } from './helpers/connection-database.mjs'
import { migrationReportUtils } from './helpers/migration-report.mjs'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import * as Vue from 'vue'
import { parse } from '@vue/compiler-sfc'

function load(source, require) {
  const compiled = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } })
  const context = { exports: {}, require, AbortController, FormData, Blob }
  vm.runInNewContext(compiled.outputText, context)
  return context.exports
}
function page(preflight, starts, errors, options = {}) {
  const helpers = load(readFileSync(new URL('../src/utils/migrationOptions.ts', import.meta.url), 'utf8'), () => {})
  const { descriptor } = parse(readFileSync(new URL('../src/views/MigrationView.vue', import.meta.url), 'utf8'))
  return load(`${descriptor.scriptSetup.content}
    export { connections, dataMigrate, viewMigrate, vmCompatibilityGroups, objMigrate, pgChecks, pgBusy, pgExclusions, runPGCheck, excludePGObject, restorePGObject, startDataMigration, handleMigrateViews, startObjectMigration, pgTaskSummaries, onPGReportLoaded, resetDataMigration, resetObjectMigration, showPGTaskResult, dataMigrateResultTab, objMigrateResultTab, dataReportPanel, objectReportPanel };`, id => {
    switch (id) {
      case 'vue': return { ...Vue, provide() {}, onMounted() {}, onUnmounted() {} }
      case 'vue-router': return { useRoute: () => ({ query: {} }), onBeforeRouteLeave() {} }
      case '@arco-design/web-vue': return { Message: { error: message => errors ? errors.push(message) : assert.fail(message), success() {}, warning: message => options.warnings?.push(message) }, Modal: {} }
      case '@/utils/connectionDatabase': return connectionDatabaseUtils
      case '@/composables/connectionDatabaseEditors': return connectionDatabaseEditors
      case '@/utils/migrationOptions': return helpers
      case '@/utils/migrationReport': return migrationReportUtils
      case '@/utils/clipboard': return {}
      case '@/api/schema': return {}
      case '@/api/connections': return {}
      case '@/api/migration': return {
        preflightDataMigration: preflight,
        startDataMigration: options.start ?? (async request => { starts.push(request); return { data: { job_id: 'job' } } }),
        startObjectMigration: options.start ?? (async request => { starts.push(request); return { data: { job_id: 'job' } } }),
        migrateViews: options.migrateViews ?? (async request => { starts.push(request); return { data: { results: [] } } }),
        createDataMigrateEventSource: () => ({ addEventListener() {}, close() {} }),
      }
      default: throw new Error(`Unexpected import ${id}`)
    }
  })
}
async function select(p) {
  p.connections.value = [{ id: 1, db_type: 'gaussdb' }, { id: 2, db_type: 'mysql' }]
  p.dataMigrate.srcConnId = 1
  p.dataMigrate.dstConnId = 2
  p.dataMigrate.srcDatabase = 'public'
  p.dataMigrate.dstSchema = 'target'
  await Vue.nextTick()
}

test('PG/MySQL launch blocks on preflight and carries explicit exclusions into a fresh check and launch', async () => {
  const scope = Vue.effectScope()
  try {
    let ready = false
    const checks = [], starts = []
    const p = scope.run(() => page(async request => { checks.push(request); return { data: { ready, issues: [] } } }, starts))
    await select(p)
    await p.startDataMigration()
    assert.equal(checks.length, 1)
    assert.equal(starts.length, 0)
    const object = { kind: 'index', table: 'Mixed', name: 'PartialIndex' }
    p.excludePGObject('data', object)
    await Vue.nextTick()
    assert.equal(p.pgChecks.data, null)
    ready = true
    await p.startDataMigration()
    assert.equal(checks.length, 2)
    assert.equal(starts.length, 1)
    assert.equal(starts[0].excluded_objects[0].name, 'PartialIndex')
    p.dataMigrate.srcDatabase = 'different_schema'
    await Vue.nextTick()
    assert.equal(p.pgExclusions.data.length, 0)
    assert.equal(p.pgChecks.data, null)
  } finally { scope.stop() }
})

test('changing migration configuration cancels pending checks and discards late results', async () => {
  const scope = Vue.effectScope()
  try {
    let resolve, signal
    const p = scope.run(() => page((_, s) => { signal = s; return new Promise(r => { resolve = r }) }, []))
    await select(p)
    const pending = p.runPGCheck('data')
    p.dataMigrate.lowerCaseNames = !p.dataMigrate.lowerCaseNames
    await Vue.nextTick()
    assert.equal(signal.aborted, true)
    resolve({ data: { ready: true } })
    await pending
    assert.equal(p.pgChecks.data, null)
  } finally { scope.stop() }
})

for (const [kind, stateKey, launchKey] of [
  ['data', 'dataMigrate', 'startDataMigration'],
  ['views', 'viewMigrate', 'handleMigrateViews'],
  ['objects', 'objMigrate', 'startObjectMigration'],
]) {
  test(`${kind}: launch reuses a successful preview only while configuration is unchanged`, async () => {
    const scope = Vue.effectScope()
    try {
      const checks = [], starts = []
      const p = scope.run(() => page(async request => { checks.push(request); return { data: { ready: true } } }, starts))
      await select(p)
      const state = p[stateKey]
      Object.assign(state, { srcConnId: 1, dstConnId: 2, srcDatabase: 'public', dstSchema: 'target' })
      if (kind !== 'data') state.selected = ['Mixed']
      if (kind === 'objects') state.objects = ['indexes']
      await Vue.nextTick()
      await p.runPGCheck(kind)
      await p[launchKey]()
      assert.equal(checks.length, 1, 'launch must not repeat the successful preview')
      assert.equal(starts.length, 1)
      // Change options and launch in the same tick, before watchers have run.
      state.lowerCaseNames = !state.lowerCaseNames
      await p[launchKey]()
      assert.equal(checks.length, 2, 'changed options require a fresh preview')
      assert.equal(starts.length, 2)
      await p.runPGCheck(kind)
      assert.equal(checks.length, 3, 'manual recheck always sends a request')
      await p[launchKey]()
      assert.equal(checks.length, 3)
      assert.equal(starts.length, 3)
    } finally { scope.stop() }
  })
}

test('excluding and restoring an object invalidates a previously successful preview', async () => {
  const scope = Vue.effectScope()
  try {
    const checks = [], starts = []
    const p = scope.run(() => page(async request => { checks.push(JSON.parse(JSON.stringify(request))); return { data: { ready: true } } }, starts))
    await select(p)
    await p.runPGCheck('data')
    const object = { kind: 'index', table: 'Mixed', name: 'PartialIndex' }
    p.excludePGObject('data', object)
    await p.startDataMigration()
    assert.equal(checks.length, 2)
    assert.equal(checks[1].excluded_objects.length, 1)
    p.restorePGObject('data', object)
    await p.startDataMigration()
    assert.equal(checks.length, 3)
    assert.equal(checks[2].excluded_objects.length, 0)
  } finally { scope.stop() }
})

test('a failed manual recheck cannot leave a previous successful result reusable', async () => {
  const scope = Vue.effectScope()
  try {
    let count = 0
    const starts = [], errors = []
    const p = scope.run(() => page(async () => {
      count++
      if (count > 1) throw new Error('connection unavailable')
      return { data: { ready: true } }
    }, starts, errors))
    await select(p)
    await p.runPGCheck('data')
    await p.runPGCheck('data')
    assert.equal(p.pgChecks.data, null)
    await p.startDataMigration()
    assert.equal(count, 3)
    assert.equal(starts.length, 0)
    assert.equal(errors.length, 2)
  } finally { scope.stop() }
})

test('batch request serializes exclusions under the intended Excel row', () => {
  const requests = []
  const api = load(readFileSync(new URL('../src/api/migration.ts', import.meta.url), 'utf8'), () => ({ default: { post: (url, data) => requests.push({ url, data }) } }))
  const exclusions = { 2: [{ kind: 'view', name: 'ComplexView' }] }
  api.startBatch(new Blob(['fixture']), [], { excluded_objects_by_row: exclusions })
  assert.deepEqual(JSON.parse(requests[0].data.get('excluded_objects_by_row')), exclusions)
})

for (const [kind, stateKey, launchKey] of [
  ['data', 'dataMigrate', 'startDataMigration'],
  ['objects', 'objMigrate', 'startObjectMigration'],
]) {
  test(`${kind}: a failed new launch cannot display the previous job report`, async () => {
    const scope = Vue.effectScope()
    try {
      const starts = []
      const p = scope.run(() => page(async () => ({ data: { ready: true } }), starts))
      await select(p)
      const state = p[stateKey]
      Object.assign(state, { srcConnId: 1, dstConnId: 2, srcDatabase: 'public', dstSchema: 'target' })
      if (kind === 'objects') Object.assign(state, { selected: ['Orders'], objects: ['indexes'] })
      await p[launchKey]()
      assert.equal(state.currentJobId, 'job')
      Object.assign(state, { finished: true, running: false })
      starts.push = () => { throw new Error('launch unavailable') }
      await p[launchKey]()
      assert.equal(state.currentJobId, '')
      assert.equal(state.finished, true)
      assert.match(state.logs.at(-1), /launch unavailable/)
    } finally { scope.stop() }
  })
}

for (const [kind, stateKey, launchKey, resetKey, tabKey, panelKey] of [
  ['data', 'dataMigrate', 'startDataMigration', 'resetDataMigration', 'dataMigrateResultTab', 'dataReportPanel'],
  ['objects', 'objMigrate', 'startObjectMigration', 'resetObjectMigration', 'objMigrateResultTab', 'objectReportPanel'],
]) {
  test(`${kind}: summary waits for the job, uses its saved preflight, and resets with configuration`, async () => {
    const scope = Vue.effectScope()
    try {
      let resolveStart
      const preview = { ready: true, issues: [{ severity: 'warning' }], check_scope: 'structure' }
      const p = scope.run(() => page(async () => ({ data: preview }), [], [], {
        start: () => new Promise(resolve => { resolveStart = resolve }),
      }))
      await select(p)
      const state = p[stateKey]
      Object.assign(state, { srcConnId: 1, dstConnId: 2, srcDatabase: 'public', dstSchema: 'target' })
      if (kind === 'objects') Object.assign(state, { selected: ['Orders'], objects: ['indexes'] })
      await Vue.nextTick()
      const start = p[launchKey]()
      while (!resolveStart) await Vue.nextTick()
      assert.equal(p.pgTaskSummaries.value[kind], null, 'pending start keeps the full preview')
      resolveStart({ data: { job_id: 'first-job' } })
      await start
      assert.equal(p.pgTaskSummaries.value[kind].preview.ready, true)
      assert.equal(p.pgTaskSummaries.value[kind].preflight, null, 'running or missing report uses the page result')
      // Completion/cancellation alone must not invent a backend preflight result.
      Object.assign(state, { running: false, finished: true })
      assert.equal(p.pgTaskSummaries.value[kind].preflight, null)
      const failure = { ready: false, issues: [{ severity: 'error' }, { severity: 'error' }] }
      p.onPGReportLoaded(kind, 'old-job', { preflight: failure })
      assert.equal(p.pgTaskSummaries.value[kind].preflight, null)
      p.onPGReportLoaded(kind, 'first-job', { preflight: failure })
      assert.equal(p.pgTaskSummaries.value[kind].preflight.ready, false)
      assert.equal(p.pgTaskSummaries.value[kind].preflight.issues.length, 2)
      let opened = 0
      p[panelKey].value = { showPreflight: async () => { opened++ } }
      p[tabKey].value = 'logs'
      await p.showPGTaskResult(kind, 'report')
      assert.equal(p[tabKey].value, 'report')
      assert.equal(opened, 1)
      p.onPGReportLoaded(kind, 'first-job', { preflight: preview })
      assert.equal(p.pgTaskSummaries.value[kind].preflight.ready, true)
      state.lowerCaseNames = !state.lowerCaseNames
      assert.equal(p.pgTaskSummaries.value[kind], null, 'even before watchers flush, edited config hides the old summary')
      await Vue.nextTick()
      p.onPGReportLoaded(kind, 'first-job', { preflight: failure })
      assert.equal(p.pgTaskSummaries.value[kind], null)
      state.lowerCaseNames = !state.lowerCaseNames
      await Vue.nextTick()
      assert.equal(p.pgTaskSummaries.value[kind], null, 'restoring options does not revive the old summary')
      resolveStart = null
      const second = p[launchKey]()
      while (!resolveStart) await Vue.nextTick()
      resolveStart({ data: { job_id: 'second-job' } })
      await second
      p.onPGReportLoaded(kind, 'first-job', { preflight: failure })
      assert.equal(p.pgTaskSummaries.value[kind].jobID, 'second-job')
      assert.equal(p.pgTaskSummaries.value[kind].preflight, null)
      p.onPGReportLoaded(kind, 'second-job', {})
      assert.equal(p.pgTaskSummaries.value[kind].preflight, null, 'legacy report without preflight retains page attribution')
      p[resetKey]()
      assert.equal(p.pgTaskSummaries.value[kind], null)
      assert.equal(state.currentJobId, '')
    } finally { scope.stop() }
  })

  test(`${kind}: editing configuration while launch is pending cannot attach its summary to new options`, async () => {
    const scope = Vue.effectScope()
    try {
      let resolveStart
      const p = scope.run(() => page(async () => ({ data: { ready: true } }), [], [], {
        start: () => new Promise(resolve => { resolveStart = resolve }),
      }))
      await select(p)
      const state = p[stateKey]
      Object.assign(state, { srcConnId: 1, dstConnId: 2, srcDatabase: 'public', dstSchema: 'target' })
      if (kind === 'objects') Object.assign(state, { selected: ['Orders'], objects: ['indexes'] })
      await Vue.nextTick()
      const start = p[launchKey]()
      while (!resolveStart) await Vue.nextTick()
      state.lowerCaseNames = !state.lowerCaseNames
      await Vue.nextTick()
      resolveStart({ data: { job_id: 'job' } })
      await start
      assert.equal(state.currentJobId, 'job', 'original task remains available')
      assert.equal(p.pgTaskSummaries.value[kind], null)
    } finally { scope.stop() }
  })
}

for (const [kind, stateKey, launchKey, resetKey] of [
  ['data', 'dataMigrate', 'startDataMigration', 'resetDataMigration'],
  ['objects', 'objMigrate', 'startObjectMigration', 'resetObjectMigration'],
]) {
  test(kind + ': restart keeps configuration but clears preview, exclusions and cached success', async () => {
    const scope = Vue.effectScope()
    try {
      let checks = 0
      const p = scope.run(() => page(async () => { checks++; return { data: { ready: true } } }, []))
      await select(p)
      const state = p[stateKey]
      Object.assign(state, { srcConnId: 1, dstConnId: 2, srcDatabase: 'public', dstSchema: 'target', lowerCaseNames: false })
      if (kind === 'objects') Object.assign(state, { selected: ['Orders'], objects: ['indexes'] })
      await Vue.nextTick()
      state.lowerCaseNames = false
      p.pgExclusions[kind] = [{ kind: 'index', name: 'old_index' }]
      await Vue.nextTick()
      await p[launchKey]()
      Object.assign(state, { running: false, finished: true })
      p[resetKey]()
      assert.equal(p.pgChecks[kind], null)
      assert.equal(p.pgBusy[kind], false)
      assert.equal(p.pgExclusions[kind].length, 0)
      assert.equal(p.pgTaskSummaries.value[kind], null)
      assert.equal(state.srcConnId, 1)
      assert.equal(state.dstConnId, 2)
      assert.equal(state.srcDatabase, 'public')
      assert.equal(state.dstSchema, 'target')
      assert.equal(state.lowerCaseNames, false)
      await p[launchKey]()
      assert.equal(checks, 2)
      // Reset again without exclusions: a matching request key must not reuse success.
      p[resetKey]()
      await p[launchKey]()
      assert.equal(checks, 3)
    } finally { scope.stop() }
  })

  test(kind + ': reset aborts pending preview and late completion leaves newer request busy', async () => {
    const scope = Vue.effectScope()
    try {
      const pending = []
      const p = scope.run(() => page((request, signal) => new Promise(resolve => pending.push({ signal, resolve })), []))
      await select(p)
      const old = p.runPGCheck(kind)
      p[resetKey]()
      assert.equal(pending[0].signal.aborted, true)
      assert.equal(p.pgBusy[kind], false)
      await Vue.nextTick()
      const fresh = p.runPGCheck(kind)
      pending[0].resolve({ data: { ready: true, marker: 'old' } })
      await old
      assert.equal(p.pgChecks[kind], null)
      assert.equal(p.pgBusy[kind], true)
      pending[1].resolve({ data: { ready: true, marker: 'new' } })
      await fresh
      assert.equal(p.pgChecks[kind].marker, 'new')
      assert.equal(p.pgBusy[kind], false)
    } finally { scope.stop() }
  })
}

test('clearing panel results resets filters, pages and selected DDL', async () => {
  const scope = Vue.effectScope()
  try {
    const props = Vue.reactive({ result: { ready: true, issues: [], objects: [] }, excluded: [] })
    const { descriptor } = parse(readFileSync(new URL('../src/views/PGMySQLPreflightPanel.vue', import.meta.url), 'utf8'))
    const source = descriptor.scriptSetup.content
      .replace(/const props = withDefaults[^\n]+/, 'const props = require("test-props")')
      .replace(/defineEmits[^\n]+/, '')
    const panel = scope.run(() => load(source + '\nexport { activeTab, severity, issueSearch, issuePage, objectSearch, objectPage, selectedObjectKey, selectedSQL };', id => {
      if (id === 'vue') return Vue
      if (id === 'test-props') return props
      return {}
    }))
    for (const [key, value] of Object.entries({ activeTab: 'objects', severity: 'error', issueSearch: 'old', issuePage: 3, objectSearch: 'old', objectPage: 4, selectedObjectKey: 'old' })) panel[key].value = value
    await Vue.nextTick()
    props.result = null
    await Vue.nextTick()
    assert.equal(panel.activeTab.value, 'issues')
    assert.equal(panel.severity.value, 'all')
    assert.equal(panel.issueSearch.value, '')
    assert.equal(panel.objectSearch.value, '')
    assert.equal(panel.issuePage.value, 1)
    assert.equal(panel.objectPage.value, 1)
    assert.equal(panel.selectedObjectKey.value, '')
    assert.equal(panel.selectedSQL.value, '')
  } finally { scope.stop() }
})


test('view migration retains actual successes and failures when compatibility preparation fails', async () => {
 const scope = Vue.effectScope()
 try {
  const warnings = [], errors = [], starts = []
  const compatibility = { total: 2, success: 0, failed: 2, created: 0, reused: 0, items: [] }
  const results = [{ name: 'plain', error: '', ddl: 'CREATE VIEW plain AS SELECT 1' }, { name: 'dependent', error: 'function get_uuid does not exist', ddl: 'CREATE VIEW dependent AS SELECT get_uuid()' }]
  const p = scope.run(() => page(async () => assert.fail('no PG/MySQL preflight'), starts, errors, {
   warnings,
   migrateViews: async request => { starts.push(request); return { data: { results, compatibility_functions: compatibility } } },
  }))
  p.connections.value = [{ id: 1, db_type: 'mysql' }, { id: 2, db_type: 'gaussdb' }]
  p.viewMigrate.srcConnId = 1; p.viewMigrate.dstConnId = 2; p.viewMigrate.dstSchema = 'app'
  await Vue.nextTick()
  p.viewMigrate.selected = ['plain', 'dependent']
  await p.handleMigrateViews()
  assert.equal(starts.length, 1)
  assert.equal(p.viewMigrate.results.length, 2)
  assert.equal(p.viewMigrate.results[0].error, '')
  assert.equal(p.viewMigrate.compatibilityFunctions.failed, 2)
  assert.equal(errors.length, 0)
  assert.match(warnings[0], /视图迁移成功 1 个，失败 1 个/)
  assert.doesNotMatch(warnings[0], /未执行/)
 } finally { scope.stop() }
})

test('view migration shows procedure failures separately while preserving view results', async () => {
  const scope = Vue.effectScope()
  try {
    const warnings = [], errors = [], starts = []
    const compatibility = { total: 11, success: 10, failed: 1, created: 8, reused: 2, items: [
      { kind: 'procedure', name: 'changedatatohistory', schema: 'app', status: 'failed', ddl: '', error: 'unsupported procedure' },
    ] }
    const p = scope.run(() => page(async () => assert.fail('no PG/MySQL preflight'), starts, errors, {
      warnings,
      migrateViews: async request => { starts.push(request); return { data: { results: [{ name: 'plain', error: '', ddl: 'CREATE VIEW plain AS SELECT 1' }], compatibility_functions: compatibility } } },
    }))
    p.connections.value = [{ id: 1, db_type: 'mysql' }, { id: 2, db_type: 'postgres' }]
    Object.assign(p.viewMigrate, { srcConnId: 1, dstConnId: 2, dstSchema: 'app' })
    await Vue.nextTick()
    p.viewMigrate.selected = ['plain']
    await p.handleMigrateViews()
    assert.equal(errors.length, 0)
    assert.equal(p.viewMigrate.results.length, 1)
    assert.equal(p.vmCompatibilityGroups.value.length, 2)
    assert.equal(p.vmCompatibilityGroups.value[0].report.failed, 0)
    assert.equal(p.vmCompatibilityGroups.value[1].report.failed, 1)
    assert.match(warnings[0], /^兼容存储过程初始化失败；视图迁移成功 1 个，失败 0 个/)
    assert.doesNotMatch(warnings[0], /兼容函数初始化失败/)
  } finally { scope.stop() }
})
