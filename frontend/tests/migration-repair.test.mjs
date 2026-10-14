import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import * as Vue from 'vue'
import { parse } from '@vue/compiler-sfc'
import { compile } from '@vue/compiler-dom'
import { renderToString } from '@vue/server-renderer'
import Arco from '@arco-design/web-vue'
import { formatDialect, postgresql } from 'sql-formatter'

function transpile(source) { return ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } }).outputText }
const utilsContext = { exports: {} }
vm.runInNewContext(transpile(readFileSync(new URL('../src/utils/migrationRepair.ts', import.meta.url), 'utf8')), utilsContext)
const utils = utilsContext.exports
const descriptor = parse(readFileSync(new URL('../src/components/MigrationRepairPanel.vue', import.meta.url), 'utf8')).descriptor
const fields = 'records, binding, job, activeAttempt, editing, draftUnits, mappingForm, addColumnMapping, reviewed, editorVisible, executeConfirmVisible, executionSnapshot, executionKey, recoveryVisible, recoveryObject, recoveryForm, recoveryPlan, canRecover, canExecute, stats, addUnit, restoreOriginal, attempts, openEditor, closeEditor, useSuggested, prepareExecute, confirmExecute, saveDraft, openRecovery, reviewRelated, loadPreview, invalidatePreview, confirmRecovery, refresh, refreshHistory, exportHistory, bindingForm, openBinding, saveBinding, summary, focusObject, page, highlightedObjectID, pendingFocus, showAllObjects, repairSection'
const renderFields = 'loading, error, filter, keyword, busy, cancelling, saving, submitting, executeVersion, historyVisible, loadingHistory, visibleAttempts, visibleRecords, editorDialect, qualified, objectStatus, dependencyNames, statusColor, objectName, openHistory, cancelAttempt, selectSourceConnection, sourceConnections, targetConnections, relatedCandidates, bindingVisible, previewing, selectRepairFilter, emptyDescription'
function panel(props, overrides = {}) {
  const lifecycle = {}, messages = [], scheduled = [], emitted = []
  let uuid = 0
  const api = {
    listRepairs: async () => ({ data: { objects: [], binding: { configured: true } } }),
    listRepairAttempts: async () => ({ data: { items: [] } }),
    ...overrides,
  }
  const context = {
    exports: {}, defineProps: () => props, defineExpose() {}, defineEmits: () => (...args) => emitted.push(args),
    crypto: { randomUUID: () => `uuid-${++uuid}` },
    setTimeout: fn => { scheduled.push(fn); return scheduled.length }, clearTimeout() {},
    require: id => {
      if (id === 'vue') return { ...Vue, onUnmounted: fn => { lifecycle.unmount = fn } }
      if (id === '@/utils/migrationRepair') return utils
      if (id === '@/utils/dbType') return { getDbTypeLabel: value => value }
      if (id === '@/api/migrationRepair') return api
      if (id === '@/api/migration') return { getDataMigrationJob: async id => ({ data: { job_id: id, status: 'failed', dst_db_type: 'postgres' } }) }
      if (id === '@/api/connections') return { listConnections: async () => ({ data: [] }) }
      if (id === '@arco-design/web-vue') return { Message: { success: text => messages.push(text), warning: text => messages.push(text), error: text => messages.push(text), info: text => messages.push(text) } }
      throw new Error(`Unexpected import ${id}`)
    },
  }
  vm.runInNewContext(transpile(`${descriptor.scriptSetup.content}\nexport { ${fields}, ${renderFields} };`), context)
  return { ...context.exports, lifecycle, messages, scheduled, emitted }
}
const tick = async () => { await Vue.nextTick(); await Promise.resolve(); await Vue.nextTick() }
const unit = (id, status, extra = {}) => ({ id, status, sql: `CREATE ${id}`, original_sql: `CREATE ${id}`, selected: true, destructive: false, ...extra })
const record = (extra = {}) => ({ version: 1, object: { id: 'obj', kind: 'table', name: 'Orders', target_name: 'Orders', schema: 'public', ddl: 'CREATE TABLE Orders (id INT)', status: 'failed', units: [unit('table', 'failed')], dependencies: [], legacy: false, requires_review: false, ...extra } })

test('summary counts attention objects once and updates after DDL and data completion', async () => {
  const scope = Vue.effectScope()
  try {
    const p = scope.run(() => panel({ jobID: 'job' })); await tick()
    const table = record({ id: 'table', data_status: 'failed' })
    p.records.value = [table, table, record({ id: 'blocked', status: 'blocked' }), record({ id: 'data', status: 'succeeded', original_status: 'failed', data_status: 'pending' }), record({ id: 'ok', status: 'succeeded', original_status: 'succeeded' })]
    await tick()
    assert.equal(p.summary.value.attention, 3)
    assert.equal(new Set(p.visibleRecords.value.map(item => item.object.id)).size, 3)
    assert.equal(p.summary.value.repaired, 1)
    assert.equal(p.emitted.at(-1)[0], 'summary-change')
    assert.equal(p.emitted.at(-1)[1].attention, 3)
    p.records.value = [record({ status: 'succeeded', original_status: 'failed', data_status: 'pending' })]
    await tick()
    assert.equal(p.summary.value.repaired, 1)
    assert.equal(p.summary.value.attention, 1, 'DDL success still needs data')
    p.records.value[0].object.data_status = 'succeeded'
    await tick()
    assert.equal(p.summary.value.attention, 0)
    assert.equal(p.emitted.at(-1)[1].attention, 0)
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('repair state is unknown on failed loading and becomes ready only after retry', async () => {
  const scope = Vue.effectScope()
  try {
    let failed = true
    const p = scope.run(() => panel({ jobID: 'job' }, { listRepairs: async () => {
      if (failed) throw new Error('network down')
      return { data: { objects: [], binding: { configured: true } } }
    } }))
    assert.equal(p.emitted[0][1].state, 'loading')
    await tick()
    assert.equal(p.summary.value.state, 'unknown')
    assert.doesNotMatch(await renderPanel(p), /当前无待处理对象/)
    failed = false; await p.refresh(); await tick()
    assert.equal(p.summary.value.state, 'ready')
    assert.match(await renderPanel(p), /当前无待处理对象/)
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('repair matching preserves case and distinguishes types, tables, schemas and renamed identifiers', () => {
  const records = [
    record({ id: 'selected', kind: 'index', name: 'orders_Ix', source_name: 'Ix', target_name: 'orders_Ix', source_table: 'Orders', table: 'orders', schema: 'dst' }),
    record({ id: 'other-table', kind: 'index', name: 'orders_Ix', source_name: 'Ix', target_name: 'orders_Ix', table: 'Archive', schema: 'dst' }),
    record({ id: 'other-kind', kind: 'primary_key', name: 'orders_Ix', source_name: 'Ix', target_name: 'orders_Ix', table: 'orders', schema: 'dst' }),
    record({ id: 'other-schema', kind: 'index', name: 'orders_Ix', source_name: 'Ix', target_name: 'orders_Ix', table: 'orders', schema: 'other' }),
    record({ id: 'other-case', kind: 'index', name: 'orders_ix', source_name: 'ix', target_name: 'orders_ix', table: 'orders', schema: 'dst' }),
  ]
  const target = { kind: 'indexes', name: 'orders_Ix', source_name: 'Ix', target_name: 'orders_Ix', table: 'Orders', target_table: 'orders', target_schema: 'dst' }
  assert.deepEqual(Array.from(utils.matchingRepairRecords(records, target), item => item.object.id), ['selected'])
  assert.equal(utils.matchingRepairRecords(records, { ...target, name: 'ORDERS_IX', source_name: 'IX', target_name: 'ORDERS_IX' }).length, 0)
  assert.equal(utils.matchingRepairRecords(records, { kind: 'indexes', name: 'orders_Ix' }).length, 3, 'ambiguous records are returned for user selection')
  assert.equal(utils.matchingRepairRecords([record()], { kind: 'data', name: 'Orders' }).length, 1, 'data failures can locate the table recovery record')
})

test('focus includes repaired objects, selects the matching page and focuses its highlighted anchor', async () => {
  const scope = Vue.effectScope()
  try {
    const p = scope.run(() => panel({ jobID: 'job' })); await tick()
    p.records.value = Array.from({ length: 23 }, (_, index) => record({ id: `object-${index}`, name: `Index${index}`, kind: 'index', table: 'Orders', target_name: `Index${index}`, status: 'succeeded', original_status: 'failed' }))
    const actions = []
    p.repairSection.value = { querySelectorAll: () => [{ dataset: { repairObjectId: 'object-22' }, focus: () => actions.push('focus'), scrollIntoView: () => actions.push('scroll') }] }
    p.keyword.value = 'unrelated'; await tick()
    await p.focusObject({ kind: 'indexes', name: 'Index22', table: 'Orders' })
    assert.equal(p.filter.value, 'all')
    assert.equal(p.keyword.value, '')
    assert.equal(p.highlightedObjectID.value, 'object-22')
    assert.equal(p.page.value, 3)
    assert.deepEqual(actions, ['focus', 'scroll'])
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('ambiguous and missing legacy matches filter by name without opening an editor', async () => {
  const scope = Vue.effectScope()
  try {
    const p = scope.run(() => panel({ jobID: 'job' })); await tick()
    p.records.value = [record({ id: 'one', kind: 'index', name: 'Ix', target_name: 'Ix', table: 'Orders' }), record({ id: 'two', kind: 'index', name: 'Ix', target_name: 'Ix', table: 'Archive' })]
    await p.focusObject({ kind: 'indexes', name: 'Ix' })
    assert.equal(p.keyword.value, 'Ix')
    assert.equal(p.visibleRecords.value.length, 2)
    assert.equal(p.highlightedObjectID.value, '')
    assert.equal(p.editorVisible.value, false)
    await p.focusObject({ kind: 'indexes', name: 'Missing' })
    assert.equal(p.keyword.value, 'Missing')
    assert.equal(p.visibleRecords.value.length, 0)
    assert.match(await renderPanel(p), /没有匹配的对象/)
    p.showAllObjects(); await tick()
    assert.equal(p.visibleRecords.value.length, 2)
    p.filter.value = 'attention'; p.records.value.forEach(item => { item.object.status = 'succeeded' })
    await tick()
    const html = await renderPanel(p)
    assert.match(html, /当前无待处理对象/)
    assert.match(html, /查看全部对象/)
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('focus waits for loading and task changes clear pending navigation and old summary data', async () => {
  const scope = Vue.effectScope()
  try {
    const props = Vue.reactive({ jobID: 'old' }), requests = []
    const p = scope.run(() => panel(props, { listRepairs: id => new Promise(resolve => requests.push({ id, resolve })) }))
    await p.focusObject({ kind: 'tables', name: 'Orders' })
    assert.ok(p.pendingFocus.value)
    requests[0].resolve({ data: { objects: [record()], binding: { configured: true } } }); await tick(); await tick()
    assert.equal(p.highlightedObjectID.value, 'obj')
    assert.equal(p.filter.value, 'all')
    p.selectRepairFilter('blocked'); p.keyword.value = 'Orders'; p.page.value = 4
    const refresh = p.refresh()
    await p.focusObject({ kind: 'tables', name: 'Orders' })
    props.jobID = 'new'; await tick()
    assert.equal(p.pendingFocus.value, null)
    assert.equal(p.highlightedObjectID.value, '')
    assert.equal(p.keyword.value, '')
    assert.equal(p.filter.value, 'attention')
    assert.equal(p.page.value, 1)
    assert.equal(p.summary.value.jobID, 'new')
    assert.equal(p.summary.value.state, 'loading')
    requests[2].resolve({ data: { objects: [], binding: { configured: true } } }); await tick()
    requests[1].resolve({ data: { objects: [record()], binding: { configured: true } } }); await refresh; await tick()
    assert.equal(p.summary.value.attention, 0)
    assert.equal(p.summary.value.state, 'ready')
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

async function renderPanel(p, onRender, onEditor) {
  const code = compile(descriptor.template.content, { mode: 'function', prefixIdentifiers: true }).code
  const render = vm.runInNewContext(`(function() { ${code} })()`, { Vue })
  const app = Vue.createSSRApp({
    setup: () => ({ ...p, ...utils, getDbTypeLabel: value => value === 'gaussdb' ? 'GaussDB' : value }),
    render(...args) { const tree = render.apply(this, args); onRender?.(tree); return tree },
  })
  const passthrough = { setup: (_, { slots }) => () => Vue.h('div', slots.default?.()) }
  for (const name of ['a-tooltip', 'a-textarea', 'a-space', 'a-button', 'a-alert', 'a-tag', 'a-spin', 'a-select', 'a-option', 'a-input-search', 'a-collapse', 'a-collapse-item', 'a-checkbox', 'a-form', 'a-form-item', 'a-input', 'a-input-number', 'a-switch']) app.component(name, passthrough)
  app.component('a-empty', Arco.Empty)
  for (const name of ['a-modal', 'a-drawer']) app.component(name, { props: ['visible'], setup: (props, { slots }) => () => props.visible ? Vue.h('div', slots.default?.()) : null })
  app.component('a-table-column', { setup: () => () => null })
  app.component('a-table', {
    props: ['data'],
    setup: (props, { slots }) => () => Vue.h('div', props.data?.length ? props.data.flatMap(record => (slots.columns?.() || []).map(column => Vue.h('div', column.children?.cell?.({ record })))) : slots.empty?.()),
  })
  app.component('SqlEditor', { props: ['modelValue', 'formatDbType', 'formatDisabled', 'executionMode'], setup: props => () => { onEditor?.(props); return Vue.h('textarea', props.modelValue) } })
  return renderToString(app)
}

function findNodes(tree, predicate) {
  const children = Array.isArray(tree.children) ? tree.children.flatMap(child => findNodes(child, predicate)) : []
  return predicate(tree) ? [tree, ...children] : children
}

test('repair formatting uses the target dialect and honors read-only and busy states', async () => {
  const scope = Vue.effectScope()
  try {
    const p = scope.run(() => panel({ jobID: 'job' })); await tick()
    p.job.value.dst_db_type = 'dameng'
    p.openEditor(record({ units: [unit('pending', 'pending'), unit('success', 'succeeded'), unit('danger', 'pending', { destructive: true })] }))
    let editors
    const inspect = async () => { editors = []; return renderPanel(p, undefined, props => { editors.push(props) }) }
    await inspect()
    assert.equal(editors.length, 1, 'successful and destructive units stay read-only')
    assert.equal(editors[0].formatDbType, 'dameng')
    assert.equal(editors[0].formatDisabled, false)
    p.saving.value = true; await inspect()
    assert.equal(editors[0].formatDisabled, true)
    p.saving.value = false; p.job.value.status = 'running'; await inspect()
    assert.equal(editors[0].formatDisabled, true)
    p.job.value.status = 'failed'; p.editing.value.object.status = 'unknown'; await inspect()
    assert.equal(editors[0].formatDisabled, true)
    p.openEditor(record({ diagnostic_only: true })); await inspect()
    assert.equal(editors.length, 0)
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('formatted SQL persists through saving, reopening and execution confirmation', async () => {
  const scope = Vue.effectScope()
  try {
    const original = 'Create View "OrdersView" AS select Id, Name from "Orders" where Id > 1;'
    let saved = record({ kind: 'view', ddl: original, units: [unit('view', 'failed', { sql: original, original_sql: original })] })
    const p = scope.run(() => panel({ jobID: 'job' }, {
      listRepairs: async () => ({ data: { objects: [saved], binding: { configured: true } } }),
      saveRepairDraft: async (_, __, draft) => {
        saved = { version: saved.version + 1, object: { ...saved.object, units: draft.units.map(item => ({ ...item })) } }
        return { data: saved }
      },
    })); await tick()
    p.openEditor(saved)
    const formatted = formatDialect(original, { dialect: postgresql, keywordCase: 'preserve', tabWidth: 2 })
    p.draftUnits.value[0].sql = formatted
    assert.equal(await p.saveDraft(), true)
    p.closeEditor(); p.openEditor(saved)
    assert.equal(p.draftUnits.value[0].sql, formatted)
    assert.equal(p.draftUnits.value[0].original_sql, original)
    assert.equal(p.editing.value.object.ddl, original)
    await p.prepareExecute()
    assert.equal(p.executeConfirmVisible.value, true)
    assert.equal(p.executionSnapshot.value[0].sql, formatted)
    p.draftUnits.value[0].sql = original
    assert.equal(p.executeConfirmVisible.value, false, 'editing invalidates the previous confirmation')
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('category buttons and dropdown filter the same objects and keep counts independent of search', async () => {
  const scope = Vue.effectScope()
  try {
    const p = scope.run(() => panel({ jobID: 'job' })); await tick()
    p.records.value = [
      record({ id: 'fixed', status: 'succeeded', original_status: 'failed', data_status: 'pending' }),
      record({ id: 'pending', status: 'pending' }),
      record({ id: 'failed', status: 'failed' }),
      record({ id: 'draft', status: 'draft' }),
      record({ id: 'unknown', status: 'unknown' }),
      record({ id: 'blocked', status: 'blocked', data_status: 'blocked' }),
      record({ id: 'original-success', status: 'succeeded', original_status: 'succeeded' }),
      record({ id: 'diagnostic', diagnostic_only: true, data_status: 'failed' }),
      record({ id: 'index', kind: 'index', data_status: 'failed' }),
    ]
    const expected = {
      repaired: ['fixed'], pending: ['pending', 'failed', 'draft', 'unknown', 'diagnostic', 'index'],
      blocked: ['blocked'], data: ['fixed', 'blocked'],
    }
    let buttons, dropdown
    await renderPanel(p, tree => {
      buttons = findNodes(tree, node => node.type === 'button' && node.props?.class?.includes('repair-stat'))
      dropdown = findNodes(tree, node => node.props?.style?.width === '150px' && node.props?.['onUpdate:modelValue'])[0]
    })
    assert.equal(buttons.length, 4)
    assert.ok(dropdown)
    for (const [index, category] of utils.repairCategories.entries()) {
      p.keyword.value = 'unrelated'; p.page.value = 5
      p.highlightedObjectID.value = 'fixed'; p.pendingFocus.value = { kind: 'tables', name: 'Orders' }
      buttons[index].props.onClick()
      await tick()
      assert.equal(p.filter.value, category.key)
      assert.equal(p.keyword.value, '')
      assert.equal(p.page.value, 1)
      assert.equal(p.highlightedObjectID.value, '')
      assert.equal(p.pendingFocus.value, null)
      assert.deepEqual(Array.from(p.visibleRecords.value, item => item.object.id), expected[category.key])
      assert.equal(p.stats.value[category.key], expected[category.key].length)
      let renderedDropdown
      const html = await renderPanel(p, tree => {
        renderedDropdown = findNodes(tree, node => node.props?.style?.width === '150px' && node.props?.['onUpdate:modelValue'])[0]
      })
      assert.equal(renderedDropdown.props.modelValue, category.key)
      const renderedButtons = html.match(/<button\b[^>]*>.*?<\/button>/gs)
      assert.equal(renderedButtons.length, 4)
      assert.match(renderedButtons[index], /type="button"/)
      assert.match(renderedButtons[index], /aria-pressed="true"/)
      assert.match(renderedButtons[index], /repair-stat-active/)
      assert.doesNotMatch(renderedButtons[index], /disabled/)
    }
    p.keyword.value = 'does-not-match'
    dropdown.props['onUpdate:modelValue']('pending')
    await tick()
    assert.equal(p.filter.value, 'pending')
    assert.equal(p.keyword.value, 'does-not-match')
    assert.equal(p.visibleRecords.value.length, 0)
    assert.equal(p.stats.value.pending, expected.pending.length)
    assert.equal(p.emptyDescription.value, '没有匹配的对象')
    // Clicking the already selected category still clears the search and page.
    p.page.value = 3; buttons[1].props.onClick(); await tick()
    assert.equal(p.page.value, 1)
    assert.equal(p.visibleRecords.value.length, expected.pending.length)
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('zero-count categories stay clickable and explain empty results with a reset action', async () => {
  const scope = Vue.effectScope()
  try {
    const p = scope.run(() => panel({ jobID: 'job' })); await tick()
    p.records.value = [record({ status: 'succeeded', original_status: 'succeeded' })]
    let buttons
    await renderPanel(p, tree => { buttons = findNodes(tree, node => node.type === 'button') })
    for (const [index, category] of utils.repairCategories.entries()) {
      assert.equal(p.stats.value[category.key], 0)
      assert.equal(buttons[index].props.disabled, undefined)
      buttons[index].props.onClick(); await tick()
      const html = await renderPanel(p)
      assert.equal(p.visibleRecords.value.length, 0)
      assert.ok(html.includes(category.empty))
      assert.ok(html.includes('查看全部对象'))
    }
    p.showAllObjects(); await tick()
    assert.equal(p.filter.value, 'all')
    assert.equal(p.visibleRecords.value.length, 1)
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('repaired category preserves legacy execution evidence and excludes verification-only success', () => {
  const object = record({ status: 'succeeded', units: [] }).object
  assert.equal(utils.matchesRepairFilter(object, 'repaired'), false)
  for (const extra of [{ original_status: 'failed' }, { original_error: 'failed migration' }, { error: 'legacy failure' }, { units: [unit('create', 'succeeded')] }]) {
    assert.equal(utils.matchesRepairFilter({ ...object, ...extra }, 'repaired'), true)
  }
  for (const kind of ['ddl', 'recovery']) {
    assert.equal(utils.matchesRepairFilter(object, 'repaired', [{ object_id: 'obj', kind, status: 'succeeded' }]), true)
  }
  for (const kind of ['verification', 'manual_verification']) {
    assert.equal(utils.matchesRepairFilter(object, 'repaired', [{ object_id: 'obj', kind, status: 'succeeded' }]), false)
  }
  assert.equal(utils.matchesRepairFilter(object, 'repaired', [{ object_id: 'other', kind: 'ddl', status: 'succeeded' }]), false)
  assert.equal(utils.matchesRepairFilter({ ...object, original_status: 'succeeded', units: [unit('create', 'succeeded')] }, 'repaired'), false)
  assert.equal(utils.matchesRepairFilter({ ...object, status: 'unknown', original_status: 'failed' }, 'repaired'), false)
  for (const status of ['success', 'repaired']) assert.equal(utils.matchesRepairFilter({ ...object, status, original_status: 'failed' }, 'repaired'), true)
  for (const data_status of [undefined, 'not_required', 'succeeded', 'success', 'repaired']) assert.equal(utils.matchesRepairFilter({ ...object, data_status }, 'data'), false)
})

test('refresh keeps category and search while repairs leave it and pagination shrinks', async () => {
  const scope = Vue.effectScope()
  try {
    let saved = Array.from({ length: 12 }, (_, index) => record({ id: `obj-${index}`, original_status: 'failed', original_error: 'original failure' }))
    let fail = false
    const p = scope.run(() => panel({ jobID: 'job' }, { listRepairs: async () => {
      if (fail) throw new Error('refresh unavailable')
      return { data: { objects: saved, binding: { configured: true } } }
    } })); await tick()
    p.selectRepairFilter('pending'); p.keyword.value = 'Orders'; await tick(); p.page.value = 2
    saved = saved.map((item, index) => index >= 9 ? { ...item, object: { ...item.object, status: 'succeeded', units: [unit('create', 'succeeded')] } } : item)
    await p.refresh(); await tick()
    assert.equal(p.filter.value, 'pending')
    assert.equal(p.keyword.value, 'Orders')
    assert.equal(p.visibleRecords.value.length, 9)
    assert.equal(p.page.value, 1)
    assert.equal(p.stats.value.pending, 9)
    assert.equal(p.stats.value.repaired, 3)
    assert.equal(p.emitted.at(-1)[1].attention, 9)
    fail = true; await p.refresh(); await tick()
    assert.equal(p.summary.value.state, 'unknown')
    assert.equal(p.emptyDescription.value, '修复状态未知，请刷新重试')
    assert.equal(p.filter.value, 'pending')
    assert.equal(p.keyword.value, 'Orders')
    fail = false
    saved = saved.map(item => ({ ...item, object: { ...item.object, status: 'succeeded', units: [unit('create', 'succeeded')] } }))
    await p.refresh(); await tick()
    assert.equal(p.summary.value.state, 'ready')
    assert.equal(p.visibleRecords.value.length, 0)
    assert.equal(p.stats.value.pending, 0)
    assert.equal(p.stats.value.repaired, 12)
    assert.equal(p.emitted.at(-1)[1].attention, 0)
    assert.equal(saved[0].object.original_error, 'original failure')
    p.selectRepairFilter('repaired'); await tick()
    assert.equal(p.visibleRecords.value.length, 12)
    p.page.value = 2; await p.refresh(); await tick()
    assert.equal(p.page.value, 2, 'valid page is preserved during refresh')
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('data recovery completion updates category membership and summary through polling', async () => {
  const scope = Vue.effectScope()
  try {
    let saved = record({ status: 'succeeded', original_status: 'failed', data_status: 'pending' })
    let completed = false
    const p = scope.run(() => panel({ jobID: 'job' }, {
      listRepairs: async () => ({ data: { objects: [saved], binding: { configured: true } } }),
      startRecovery: async () => ({ data: { id: 'recovery', kind: 'recovery', object_id: 'obj', status: 'running', units: [] } }),
      getRepairAttempt: async () => ({ data: { id: 'recovery', kind: 'recovery', object_id: 'obj', status: completed ? 'succeeded' : 'running', units: [] } }),
    })); await tick()
    p.selectRepairFilter('repaired'); await tick()
    assert.equal(p.visibleRecords.value.length, 1)
    assert.equal(p.stats.value.data, 1)
    p.selectRepairFilter('data'); await tick()
    assert.equal(p.visibleRecords.value.length, 1)
    p.recoveryObject.value = saved
    p.recoveryPlan.value = { token: 'preview', preview: { data_required: true, target_rows: 0, related: [] } }
    p.recoveryForm.sourceQuiesced = true; p.recoveryForm.targetQuiesced = true; p.recoveryForm.reviewed = true
    await p.confirmRecovery()
    assert.equal(p.activeAttempt.value.status, 'running')
    completed = true; saved = { ...saved, object: { ...saved.object, data_status: 'succeeded' } }
    p.scheduled.at(-1)()
    await new Promise(resolve => setImmediate(resolve)); await tick()
    assert.equal(p.activeAttempt.value.status, 'succeeded')
    assert.equal(p.filter.value, 'data')
    assert.equal(p.visibleRecords.value.length, 0)
    assert.equal(p.stats.value.data, 0)
    assert.equal(p.stats.value.repaired, 1)
    assert.equal(p.summary.value.attention, 0)
    assert.equal(p.emitted.at(-1)[1].attention, 0)
    assert.match(await renderPanel(p), /当前无待补数据对象/)
    p.selectRepairFilter('repaired'); await tick()
    assert.equal(p.visibleRecords.value.length, 1)
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('rendered manual repair shows its entry and editor while keeping source definition read-only', async () => {
  const scope = Vue.effectScope()
  try {
    const failed = record({ kind: 'index', table: 'Orders', target_name: 'ix', ddl: '', units: [], source_ddl: 'SOURCE REFERENCE SQL' })
    const p = scope.run(() => panel({ jobID: 'job' })); await tick()
    p.job.value.dst_db_type = 'gaussdb'; p.records.value = [failed]
    assert.ok((await renderPanel(p)).includes('手动编写 DDL'))
    p.openEditor(failed)
    const html = await renderPanel(p)
    for (const label of ['添加执行单元', '保存草稿', '确认执行步骤', 'GaussDB', 'Schema：public', '所属表：Orders', '源端定义（只读', 'SOURCE REFERENCE SQL']) assert.ok(html.includes(label), label)
    assert.match(html, /<textarea\b[^>]*><\/textarea>/)
    assert.ok(!html.includes('恢复原文'))
    p.openEditor(record({ diagnostic_only: true, units: [], ddl: '' }))
    const diagnostic = await renderPanel(p)
    for (const label of ['添加执行单元', '保存草稿', '确认执行步骤']) assert.ok(!diagnostic.includes(label), label)
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('unsupported index accepts manual target SQL only after saving and explicit confirmation', async () => {
  const scope = Vue.effectScope()
  try {
    const calls = [], source = 'CREATE TABLE source_t(id INT, KEY ix ((source_expression)))'
    const failed = record({ kind: 'index', table: 'Orders', target_name: 'ix', ddl: '', units: [], source_ddl: source, original_error: 'unsupported expression' })
    const p = scope.run(() => panel({ jobID: 'job' }, {
      saveRepairDraft: async (...args) => { calls.push(['save', ...args]); return { data: { version: 2, object: { ...failed.object, units: args[2].units } } } },
      executeRepair: async (...args) => { calls.push(['execute', ...args]); return { data: { id: 'attempt', status: 'running' } } },
      getRepairAttempt: async () => ({ data: { id: 'attempt', status: 'running', units: [] } }),
    }))
    await tick(); p.openEditor(failed)
    assert.equal(p.draftUnits.value.length, 1)
    assert.equal(p.draftUnits.value[0].sql, '')
    assert.equal(p.draftUnits.value[0].original_sql, '')
    assert.equal(p.canExecute.value, false)
    assert.equal(p.editing.value.object.source_ddl, source)
    const sql = 'CREATE INDEX ix ON public.Orders ((lower(name)))'
    p.draftUnits.value[0].sql = sql
    await p.prepareExecute()
    assert.equal(calls.length, 1)
    assert.equal(p.executionSnapshot.value[0].sql, sql)
    assert.equal(p.executeConfirmVisible.value, true)
    await p.confirmExecute()
    assert.equal(calls[1][0], 'execute')
    assert.equal(calls[1][3].version, 2)
    p.openEditor({ version: 2, object: { ...failed.object, units: [{ ...p.executionSnapshot.value[0], status: 'pending' }] } })
    assert.equal(p.draftUnits.value.length, 1)
    assert.equal(p.draftUnits.value[0].sql, sql)
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('diagnostics, data failures and indexes without table identity cannot submit DDL', async () => {
  const scope = Vue.effectScope()
  try {
    let saved = 0
    const p = scope.run(() => panel({ jobID: 'job' }, { saveRepairDraft: async () => { saved++; throw new Error('unexpected save') } }))
    await tick()
    for (const object of [record({ diagnostic_only: true }), record({ kind: 'data' }), record({ kind: 'index', ddl: '', units: [] })]) {
      assert.equal(utils.canEditRepairDDL(object.object), false)
      p.openEditor(object); p.addUnit()
      assert.equal(p.canExecute.value, false)
      assert.equal(await p.saveDraft(), false)
      await p.prepareExecute()
    }
    assert.equal(saved, 0)
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('successful manual repairs are counted and never reselect their SQL', async () => {
  const scope = Vue.effectScope()
  try {
    const object = record({ kind: 'index', table: 'Orders', status: 'succeeded', original_status: 'failed', units: [unit('ix', 'succeeded', { original_sql: '', sql: 'CREATE INDEX ix ON Orders ((id+1))' })] })
    const p = scope.run(() => panel({ jobID: 'job' }, { listRepairs: async () => ({ data: { objects: [object], binding: { configured: true } } }) }))
    await tick(); p.openEditor(object)
    assert.equal(p.stats.value.repaired, 1)
    assert.equal(p.stats.value.pending, 0)
    assert.equal(p.visibleRecords.value.length, 0)
    assert.equal(p.draftUnits.value[0].selected, false)
    assert.equal(p.canExecute.value, false)
    const history = utils.formatRepairHistory([object], [{ id: 'a', object_id: 'obj', kind: 'ddl', created_at: 'now', status: 'succeeded', units: object.object.units, verification: { status: 'unknown', message: 'check expression manually' } }]).join('\n')
    assert.ok(history.includes('执行成功'))
    assert.ok(history.includes('历史核验（旧版本'))
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('restoring original SQL preserves successful and manually added steps', async () => {
  const scope = Vue.effectScope()
  try {
    const p = scope.run(() => panel({ jobID: 'job' })); await tick()
    p.openEditor(record({ units: [unit('done', 'succeeded', { sql: 'committed edited SQL', original_sql: 'old failing SQL' }), unit('edit', 'failed', { sql: 'edited', original_sql: 'original' })] }))
    p.addUnit(); p.draftUnits.value[2].sql = 'manual SQL'
    p.restoreOriginal()
    assert.equal(p.draftUnits.value[0].sql, 'committed edited SQL')
    assert.equal(p.draftUnits.value[0].selected, false)
    assert.equal(p.draftUnits.value[1].sql, 'original')
    assert.equal(p.draftUnits.value[2].sql, 'manual SQL')
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('reopening repair drafts never selects committed or destructive steps and preserves program bodies', () => {
  const body = "BEGIN\n x := 'a;b';\n NULL;\nEND;"
  const cloned = utils.cloneRepairUnits([unit('done', 'succeeded'), unit('drop', 'failed', { destructive: true }), unit('procedure', 'failed', { sql: body })])
  assert.deepEqual(Array.from(cloned, item => item.selected), [false, false, true])
  assert.equal(cloned[2].sql, body)
  assert.equal(utils.mappingError({ a: 'id', b: 'id' }), '多个源列不能映射到同一个目标列')
  assert.equal(utils.mappingError({ a: '' }), '每个源列都需要指定目标列')
})

test('switching job drops old list, history and editor responses', async () => {
  const scope = Vue.effectScope()
  try {
    const props = Vue.reactive({ jobID: 'old' }), lists = [], histories = []
    const p = scope.run(() => panel(props, {
      listRepairs: id => new Promise(resolve => lists.push({ id, resolve })),
      listRepairAttempts: id => new Promise(resolve => histories.push({ id, resolve })),
    }))
    const history = p.refreshHistory()
    props.jobID = 'new'; await tick()
    lists[1].resolve({ data: { objects: [record({ name: 'new' })], binding: { configured: true } } }); await tick()
    lists[0].resolve({ data: { objects: [record({ name: 'old' })], binding: { configured: true } } })
    histories[0].resolve({ data: { items: [{ id: 'old-attempt' }] } }); await history; await tick()
    assert.equal(p.records.value[0].object.name, 'new')
    assert.equal(p.attempts.value.length, 0)
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('execution saves a versioned complete-unit snapshot and keyboard confirmation cannot send SQL directly', async () => {
  const scope = Vue.effectScope()
  try {
    const calls = []
    const p = scope.run(() => panel({ jobID: 'job' }, {
      saveRepairDraft: async (...args) => { calls.push(['save', ...args]); return { data: { version: 2, object: { ...record().object, units: args[2].units } } } },
      executeRepair: async (...args) => { calls.push(['execute', ...args]); return { data: { id: 'attempt', status: 'running' } } },
      getRepairAttempt: async () => ({ data: { id: 'attempt', status: 'running', units: [] } }),
    }))
    await tick()
    p.openEditor(record({ units: [unit('done', 'succeeded'), unit('drop', 'failed', { destructive: true }), unit('procedure', 'failed', { sql: 'BEGIN NULL; END;' })] }))
    await p.prepareExecute()
    assert.equal(p.executeConfirmVisible.value, true)
    assert.equal(calls.length, 1)
    assert.equal(p.executionSnapshot.value.length, 1)
    assert.equal(p.executionSnapshot.value[0].sql, 'BEGIN NULL; END;')
    await p.confirmExecute()
    assert.equal(calls[1][0], 'execute')
    assert.equal(calls[1][3].version, 2)
    assert.equal(calls[1][3].confirmed, true)
    assert.equal(calls[1][3].request_id, 'uuid-1')
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('editing invalidates confirmation and stale previews cannot authorize changed mappings', async () => {
  const scope = Vue.effectScope()
  try {
    let resolvePreview
    const p = scope.run(() => panel({ jobID: 'job' }, { previewRecovery: () => new Promise(resolve => { resolvePreview = resolve }) }))
    await tick()
    p.openEditor(record())
    p.executeConfirmVisible.value = true
    p.draftUnits.value[0].sql = 'CREATE TABLE changed (id INT)'
    assert.equal(p.executeConfirmVisible.value, false)
    p.openRecovery(record())
    p.recoveryForm.mapping = { id: 'renamed' }
    p.invalidatePreview()
    resolvePreview({ data: { token: 'stale', preview: { column_mapping: { id: 'id' } } } }); await tick()
    assert.equal(p.recoveryPlan.value, null)
    assert.equal(p.canRecover.value, false)
    assert.equal(p.recoveryForm.mapping.id, 'renamed')
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('nonempty target requires exact table confirmation and both stop-write confirmations', async () => {
  const scope = Vue.effectScope()
  try {
    const p = scope.run(() => panel({ jobID: 'job' })); await tick()
    p.recoveryPlan.value = { token: 'preview', preview: { data_required: true, target_rows: 4, qualified_target: 'public.Orders' } }
    p.recoveryForm.sourceQuiesced = true; p.recoveryForm.targetQuiesced = true; p.recoveryForm.reviewed = true
    assert.equal(p.canRecover.value, false)
    p.recoveryForm.truncate = true; p.recoveryForm.confirmation = 'Orders'
    assert.equal(p.canRecover.value, false)
    p.recoveryForm.confirmation = 'public.Orders'
    assert.equal(p.canRecover.value, true)
    p.recoveryForm.targetQuiesced = false
    assert.equal(p.canRecover.value, false)
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('report history includes original errors, drafts and immutable submitted SQL', () => {
  const object = record({ error: 'original failure', ddl: 'old DDL', units: [unit('new', 'pending', { sql: 'draft DDL' })] })
  const lines = utils.formatRepairHistory([object], [{ id: 'attempt', object_id: 'obj', created_at: 'today', kind: 'ddl', status: 'failed', units: [unit('new', 'failed', { sql: 'submitted DDL', error: 'database failure' })] }]).join('\n')
  for (const expected of ['original failure', 'old DDL', 'draft DDL', 'submitted DDL', 'database failure']) assert.ok(lines.includes(expected))
})

test('typing while a save is pending preserves newer edits and does not authorize the old snapshot', async () => {
  const scope = Vue.effectScope()
  try {
    let finishSave
    const p = scope.run(() => panel({ jobID: 'job' }, { saveRepairDraft: () => new Promise(resolve => { finishSave = resolve }) }))
    await tick(); p.openEditor(record())
    const pending = p.prepareExecute()
    p.draftUnits.value[0].sql = 'CREATE TABLE newer (id BIGINT)'
    finishSave({ data: { version: 2, object: record().object } })
    await pending
    assert.equal(p.editing.value.version, 2)
    assert.equal(p.draftUnits.value[0].sql, 'CREATE TABLE newer (id BIGINT)')
    assert.equal(p.executeConfirmVisible.value, false)
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('recovery cannot execute regenerated related candidates before their explicit review', async () => {
  const scope = Vue.effectScope()
  try {
    const p = scope.run(() => panel({ jobID: 'job' })); await tick()
    p.recoveryPlan.value = { token: 'preview', preview: { data_required: false, target_rows: 20, related: [{ id: 'pk', requires_review: true }] } }
    p.recoveryForm.sourceQuiesced = true; p.recoveryForm.targetQuiesced = true; p.recoveryForm.reviewed = true
    assert.equal(p.canRecover.value, false)
    p.recoveryPlan.value.preview.related[0].requires_review = false
    assert.equal(p.canRecover.value, true)
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('table DDL and renamed source-column mapping are saved together before execution', async () => {
  const scope = Vue.effectScope()
  try {
    let submitted
    const original = record({ source_columns: [{ Name: 'ROWID' }], column_mapping: { ROWID: 'ROW_ID' } })
    const p = scope.run(() => panel({ jobID: 'job' }, { saveRepairDraft: async (_job, _object, request) => {
      submitted = JSON.parse(JSON.stringify(request))
      return { data: { version: 2, object: { ...original.object, units: request.units, column_mapping: request.column_mapping } } }
    } }))
    await tick(); p.openEditor(original)
    p.draftUnits.value[0].sql = 'CREATE TABLE Orders (renamed_id INT)'
    p.mappingForm.values.ROWID = 'renamed_id'
    await p.prepareExecute()
    assert.equal(submitted.column_mapping.ROWID, 'renamed_id')
    assert.equal(submitted.units[0].sql, 'CREATE TABLE Orders (renamed_id INT)')
    assert.equal(p.executeConfirmVisible.value, true)
    p.mappingForm.values.ROWID = 'another_name'
    assert.equal(p.executeConfirmVisible.value, false)
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('mapping-only save on a completed table keeps every successful SQL unit unselected', async () => {
  const scope = Vue.effectScope()
  try {
    let submitted
    const original = record({ status: 'succeeded', units: [unit('create', 'succeeded')], column_mapping: { id: 'id' } })
    const p = scope.run(() => panel({ jobID: 'job' }, { saveRepairDraft: async (_job, _object, request) => {
      submitted = JSON.parse(JSON.stringify(request))
      return { data: { version: 2, object: { ...original.object, units: request.units, column_mapping: request.column_mapping } } }
    } }))
    await tick(); p.openEditor(original)
    p.mappingForm.values.id = 'renamed_id'
    await p.saveDraft()
    assert.equal(submitted.units[0].selected, false)
    assert.equal(submitted.units[0].sql, 'CREATE create')
    assert.equal(p.editing.value.object.status, 'succeeded')
    assert.equal(p.editing.value.object.column_mapping.id, 'renamed_id')
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('legacy tasks require explicit content selection and support manual source-column mappings', async () => {
  const scope = Vue.effectScope()
  try {
    let calls = 0
    const p = scope.run(() => panel({ jobID: 'job' }, { bindRepairConnections: async () => { calls++; return { data: {} } } }))
    await tick(); p.binding.value = { configured: false, legacy: true, dst_conn_id: 1 }
    await p.openBinding()
    assert.equal(p.bindingForm.config.Content, '')
    await p.saveBinding()
    assert.equal(calls, 0)
    p.bindingForm.config.Content = 'schema_only'
    await p.saveBinding()
    assert.equal(calls, 1)
    p.openEditor(record({ legacy: true, source_columns: undefined, columns: undefined }))
    p.mappingForm.source = 'ROWID'; p.mappingForm.target = 'ROW_ID'
    p.addColumnMapping()
    assert.equal(p.mappingForm.values.ROWID, 'ROW_ID')
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('schema-only recovery never submits stale truncate consent', async () => {
  const scope = Vue.effectScope()
  try {
    let submitted
    const p = scope.run(() => panel({ jobID: 'job' }, {
      startRecovery: async (_job, _object, request) => { submitted = request; return { data: { id: 'recovery', status: 'running' } } },
      getRepairAttempt: async () => ({ data: { id: 'recovery', status: 'running', units: [] } }),
    }))
    await tick()
    p.recoveryObject.value = record({ status: 'succeeded', data_status: 'not_required' })
    p.recoveryPlan.value = { token: 'preview', preview: { data_required: false, target_rows: 20, related: [] } }
    p.recoveryForm.sourceQuiesced = true; p.recoveryForm.targetQuiesced = true; p.recoveryForm.reviewed = true
    p.recoveryForm.truncate = true; p.recoveryForm.confirmation = 'public.Orders'
    await p.confirmRecovery()
    assert.equal(submitted.truncate, false)
    assert.equal(submitted.confirmation_text, undefined)
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('pending related objects can be reviewed one at a time and return to a usable recovery preview', async () => {
  const scope = Vue.effectScope()
  try {
    const table = record({ status: 'succeeded' })
    let index = { version: 1, object: { ...record().object, id: 'index', kind: 'index', table: 'Orders', name: 'ix_id', target_name: 'ix_id', requires_review: true } }
    const p = scope.run(() => panel({ jobID: 'job' }, {
      listRepairs: async () => ({ data: { objects: [table, index], binding: { configured: true } } }),
      previewRecovery: async (_job, _object, request) => ({ data: { token: 'preview', preview: { data_required: false, target_rows: 1, column_mapping: {}, related: request.related_object_ids.includes('index') ? [index.object] : [] } } }),
      saveRepairDraft: async (_job, _object, request) => {
        index = { version: 2, object: { ...index.object, units: request.units, requires_review: !request.reviewed } }
        return { data: index }
      },
    }))
    await tick(); p.openRecovery(table); await tick(); await tick()
    assert.equal(p.recoveryForm.related.index, false)
    p.recoveryForm.related.index = true; p.invalidatePreview(); await p.loadPreview()
    p.recoveryForm.sourceQuiesced = true; p.recoveryForm.targetQuiesced = true; p.recoveryForm.reviewed = true
    assert.equal(p.canRecover.value, false)
    p.reviewRelated('index')
    assert.equal(p.editorVisible.value, true)
    assert.equal(p.editing.value.object.id, 'index')
    p.reviewed.value = true; await p.saveDraft(); p.closeEditor(); await tick(); await tick()
    assert.equal(p.recoveryVisible.value, true)
    assert.equal(p.recoveryPlan.value.preview.related[0].requires_review, false)
    p.recoveryForm.reviewed = true
    assert.equal(p.canRecover.value, true)
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('legacy PostgreSQL schema snapshot is not reused as the source database', async () => {
  const scope = Vue.effectScope()
  try {
    const p = scope.run(() => panel({ jobID: 'job' })); await tick()
    p.job.value = { src_db_type: 'postgres', src_conn: { database: 'old_schema' } }
    p.binding.value = { configured: false, legacy: true }
    await p.openBinding()
    assert.equal(p.bindingForm.source_database, '')
    assert.equal(p.bindingForm.source_schema, '')
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('unknown execution results are unresolved and cannot be counted as success', () => {
  assert.equal(utils.repairStatus('unknown'), '执行结果未知')
  assert.equal(utils.repairStatus('unverified'), '执行结果未知')
  assert.equal(utils.unitCommitted('unknown'), false)
  assert.equal(utils.unitCommitted('unverified'), false)
})

test('export distinguishes immutable original error from the latest repair failure', () => {
  const lines = utils.formatRepairHistory([record({ original_error: 'original create failed', error: 'dependency failed' })], []).join('\n')
  assert.match(lines, /原始错误：original create failed/)
  assert.match(lines, /当前错误：dependency failed/)
  assert.doesNotMatch(lines, /原始错误：dependency failed/)
  const pending = utils.formatRepairHistory([record({ original_status: 'pending', original_error: '', error: 'later failure' })], []).join('\n')
  assert.doesNotMatch(pending, /原始错误：later failure/)
  assert.match(pending, /当前错误：later failure/)
})


test('manual expression-index execution immediately completes the repair without verification', async () => {
  const scope = Vue.effectScope()
  try {
    const original = record({ kind: 'index', table: 'Orders', target_name: 'ix_expr', original_status: 'failed', original_error: 'unsupported expression', units: [], ddl: '' })
    let saved = original
    const calls = []
    const p = scope.run(() => panel({ jobID: 'job' }, {
      listRepairs: async () => ({ data: { objects: [saved], binding: { configured: true } } }),
      saveRepairDraft: async (_job, _object, body) => { saved = { version: 2, object: { ...original.object, status: 'draft', units: body.units } }; return { data: saved } },
      executeRepair: async (...args) => {
        calls.push(args)
        saved = { version: 3, object: { ...saved.object, status: 'succeeded', units: saved.object.units.map(unit => ({ ...unit, status: 'succeeded', selected: false })) } }
        return { data: { id: 'ddl-attempt', object_id: 'obj', kind: 'ddl', status: 'succeeded', units: saved.object.units } }
      },
      getRepairAttempt: async () => ({ data: { id: 'ddl-attempt', object_id: 'obj', kind: 'ddl', status: 'succeeded', units: saved.object.units } }),
    }))
    await tick(); p.selectRepairFilter('pending'); p.openEditor(original)
    p.draftUnits.value[0].sql = 'CREATE UNIQUE INDEX ix_expr ON public.Orders ((id+1))'
    await p.prepareExecute(); await p.confirmExecute()
    await tick()
    assert.equal(calls.length, 1)
    assert.equal(p.filter.value, 'pending')
    assert.equal(p.stats.value.repaired, 1)
    assert.equal(p.stats.value.pending, 0)
    assert.equal(p.visibleRecords.value.length, 0)
    assert.equal(p.emitted.at(-1)[1].repaired, 1)
    assert.equal(p.emitted.at(-1)[1].attention, 0)
    await p.refresh()
    p.selectRepairFilter('repaired')
    assert.equal(p.visibleRecords.value.length, 1)
    const html = await renderPanel(p)
    assert.ok(html.includes('执行成功'))
    assert.ok(html.includes('编辑并执行'))
    assert.ok(html.includes('历史'))
    for (const text of ['重新核验', '人工确认已核验', '已执行待核验', '当前对象核验']) assert.ok(!html.includes(text), text)
    p.openEditor(p.records.value[0])
    assert.equal(p.draftUnits.value[0].selected, false)
    assert.equal(p.canExecute.value, false)
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('old verification metadata cannot change current execution success or its repair count', async () => {
  const scope = Vue.effectScope()
  try {
    const object = record({ status: 'succeeded', original_status: undefined, error: '', verification: { status: 'failed', message: 'old definition check failed' }, manual_verification: { confirmed_by: 7, evidence: 'old evidence' }, units: [unit('create', 'succeeded')] })
    const p = scope.run(() => panel({ jobID: 'job' })); await tick()
    p.records.value = [object]; p.filter.value = 'all'
    p.activeAttempt.value = { id: 'old', object_id: 'obj', kind: 'ddl', status: 'succeeded', verification: { status: 'unknown', message: 'old warning' } }
    assert.equal(p.stats.value.repaired, 1)
    const html = await renderPanel(p)
    assert.ok(html.includes('执行成功'))
    for (const text of ['old definition check failed', 'old evidence', 'old warning', '已执行待核验']) assert.ok(!html.includes(text), text)
    p.records.value = [record({ status: 'unknown', units: [unit('create', 'unknown')] })]
    assert.equal(p.stats.value.repaired, 0)
    assert.ok((await renderPanel(p)).includes('执行结果未知'))
    p.openEditor(p.records.value[0])
    assert.equal(p.canExecute.value, false)
    p.lifecycle.unmount()
  } finally { scope.stop() }
})

test('legacy verification history remains explicitly historical in the drawer and export', async () => {
  const scope = Vue.effectScope()
  try {
    const object = record({ status: 'succeeded' })
    const attempts = [
      { id: 'ddl', kind: 'ddl', object_id: 'obj', status: 'succeeded', units: [], created_at: 'execution-time', verification: { status: 'unknown', message: 'execution-time limitation' } },
      { id: 'auto', kind: 'verification', object_id: 'obj', status: 'unknown', units: [], created_at: 'check-time', verification: { status: 'unknown', message: 'old check' } },
      { id: 'manual', kind: 'manual_verification', object_id: 'obj', owner_id: 7, status: 'succeeded', units: [unit('create', 'succeeded')], created_at: 'manual-time', finished_at: 'confirmed-time', verification: { status: 'succeeded', message: 'old evidence' } },
    ]
    const p = scope.run(() => panel({ jobID: 'job' })); await tick()
    p.records.value = [object]; p.attempts.value = attempts; p.historyVisible.value = true
    const html = await renderPanel(p)
    const exported = utils.formatRepairHistory([object], attempts).join('\n')
    for (const text of ['历史自动核验（旧版本）', '历史人工核验（旧版本）', '历史核验（旧版本', '用户 #7', 'confirmed-time', 'old evidence', 'execution-time limitation']) {
      assert.ok(html.includes(text), text)
      assert.ok(exported.includes(text), text)
    }
    p.lifecycle.unmount()
  } finally { scope.stop() }
})
