import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import * as Vue from 'vue'
import { parse } from '@vue/compiler-sfc'
import { compile } from '@vue/compiler-dom'
import { renderToString } from '@vue/server-renderer'
import { connectionDatabaseUtils, loadConnectionDatabaseEditors } from './helpers/connection-database.mjs'

const original = {
  id: 5, name: 'Primary', db_type: 'mysql', host: 'localhost', port: 3306,
  database: 'OriginalDB', username: 'user', created_at: '', env: 'dev',
}
const deferred = () => {
  let resolve, reject
  const promise = new Promise((a, b) => { resolve = a; reject = b })
  return { promise, resolve, reject }
}

function workspace(api = {}, listener, locked) {
  const connections = Vue.ref([{ ...original }])
  const { createConnectionDatabaseEditors } = loadConnectionDatabaseEditors(api)
  const store = createConnectionDatabaseEditors(connections, listener, locked)
  return { connections, store, editor: store.editorFor(connections.value[0]) }
}

test('database capability distinguishes catalogs from Oracle and Dameng owners', () => {
  for (const type of ['mysql', 'sqlserver', 'postgres', 'gaussdb', 'highgo', 'vastbase', 'gbase', 'seabox', 'kingbase', 'oscar']) {
    assert.equal(connectionDatabaseUtils.supportsDatabaseEditing(type), true, type)
  }
  for (const type of ['oracle', 'dameng', 'sqlite', 'unknown']) assert.equal(connectionDatabaseUtils.supportsDatabaseEditing(type), false, type)
  assert.equal(connectionDatabaseUtils.isPostgresDatabase('postgres'), true)
  assert.equal(connectionDatabaseUtils.isPostgresDatabase('sqlserver'), false)
})

test('both ends share live dirty state, including consumers evaluated before the editor mounts', () => {
  const connections = Vue.ref([{ ...original }])
  const { createConnectionDatabaseEditors } = loadConnectionDatabaseEditors()
  const store = createConnectionDatabaseEditors(connections)
  const blocked = Vue.computed(() => store.isBlocked(5))
  assert.equal(blocked.value, false)
  const source = store.editorFor(connections.value[0])
  const destination = store.editorFor({ ...original })
  source.setDraft('MixedCaseDB')
  assert.equal(destination.draft, 'MixedCaseDB')
  assert.equal(blocked.value, true)
  destination.resetDraft()
  assert.equal(source.draft, 'OriginalDB')
  assert.equal(blocked.value, false)
  store.dispose()
})

test('successful save preserves case, updates the shared list, then notifies every subscriber', async () => {
  const calls = [], observations = []
  const w = workspace({
    updateConnectionDatabase: async (...args) => {
      calls.push(args)
      return { data: { ...original, database: args[1] } }
    },
  }, c => observations.push(['parent', c.database, w.connections.value[0].database]))
  const unsubscribe = w.store.onSaved(c => observations.push(['child', c.database, w.connections.value[0].database]))
  w.editor.setDraft('MixedCaseDB')
  await w.editor.save()
  assert.deepEqual(calls, [[5, 'MixedCaseDB', 'OriginalDB']])
  assert.deepEqual(observations, [['parent', 'MixedCaseDB', 'MixedCaseDB'], ['child', 'MixedCaseDB', 'MixedCaseDB']])
  assert.equal(w.store.isBlocked(5), false)
  assert.equal(w.editor.savedDatabase, 'MixedCaseDB')
  unsubscribe()
  w.editor.setDraft('NextDB')
  await w.editor.save()
  assert.equal(observations.length, 3)
  w.store.dispose()
})

test('validation errors and conflicts retain the unsaved draft and original connection', async () => {
  for (const status of [400, 403, 409, 500]) {
    let saved = false
    const w = workspace({ updateConnectionDatabase: async () => {
      throw { response: { status, data: { error: `Failure ${status}` } } }
    } }, () => { saved = true })
    w.editor.setDraft('NewDB')
    await w.editor.save()
    assert.equal(w.editor.draft, 'NewDB')
    assert.equal(w.editor.savedDatabase, 'OriginalDB')
    assert.equal(w.connections.value[0].database, 'OriginalDB')
    assert.equal(w.editor.error, `Failure ${status}`)
    assert.equal(w.editor.saving, false)
    assert.equal(w.store.isBlocked(5), true)
    assert.equal(saved, false)
    w.store.dispose()
  }
})

test('failed catalog listing and an empty list still permit manual database validation', async () => {
  let failures = true
  const w = workspace({
    listConnectionCatalogs: async () => {
      if (failures) throw { response: { data: { error: 'Insufficient permission' } } }
      return { data: [] }
    },
    updateConnectionDatabase: async (_, database) => ({ data: { ...original, database } }),
  })
  await w.editor.loadCatalogs()
  assert.equal(w.editor.catalogError, 'Insufficient permission')
  assert.equal(w.editor.loadingCatalogs, false)
  w.editor.setDraft('ManualDB')
  await w.editor.save()
  assert.equal(w.connections.value[0].database, 'ManualDB')
  failures = false
  await w.editor.loadCatalogs(true)
  assert.equal(w.editor.catalogError, '')
  assert.equal(w.editor.catalogs.length, 0)
  w.store.dispose()
})

test('catalog requests preserve case, deduplicate and reject reordered or superseded responses', async () => {
  const requests = []
  const w = workspace({ listConnectionCatalogs: () => {
    const request = deferred(); requests.push(request); return request.promise
  } })
  const first = w.editor.loadCatalogs()
  const second = w.editor.loadCatalogs(true)
  requests[1].resolve({ data: ['NewDB', 'newdb', 'NewDB'] })
  await second
  requests[0].resolve({ data: ['OldDB'] })
  await first
  assert.deepEqual([...w.editor.catalogs], ['NewDB', 'newdb'])
  const third = w.editor.loadCatalogs(true)
  w.connections.value = [{ ...original, database: 'ChangedDB' }]
  requests[2].resolve({ data: ['StaleDB'] })
  await third
  assert.deepEqual([...w.editor.catalogs], [])
  assert.equal(w.editor.draft, 'ChangedDB')
  assert.equal(w.editor.loadingCatalogs, false)
  w.store.dispose()
})

test('saving blocks duplicate requests and editing, and ignores responses after external changes', async () => {
  const request = deferred()
  let calls = 0, notifications = 0
  const w = workspace({ updateConnectionDatabase: () => { calls++; return request.promise } }, () => { notifications++ })
  w.editor.setDraft('PendingDB')
  const saving = w.editor.save()
  assert.equal(w.store.isBlocked(5), true)
  w.editor.setDraft('AnotherDB')
  await w.editor.save()
  assert.equal(calls, 1)
  assert.equal(w.editor.draft, 'PendingDB')
  w.connections.value = [{ ...original, host: 'other-host', database: 'ExternalDB' }]
  request.resolve({ data: { ...original, database: 'PendingDB' } })
  await saving
  assert.equal(w.connections.value[0].database, 'ExternalDB')
  assert.equal(w.editor.draft, 'ExternalDB')
  assert.equal(notifications, 0)
  w.store.dispose()
})

test('workspace disposal ignores in-flight requests and removes listeners', async () => {
  const request = deferred()
  let notifications = 0
  const w = workspace({ updateConnectionDatabase: () => request.promise }, () => { notifications++ })
  w.editor.setDraft('PendingDB')
  const saving = w.editor.save()
  w.store.dispose()
  request.resolve({ data: { ...original, database: 'PendingDB' } })
  await saving
  assert.equal(w.connections.value[0].database, 'OriginalDB')
  assert.equal(notifications, 0)
})

test('reference counted action locks protect every editor for a connection', async () => {
  const w = workspace()
  w.editor.setDraft('UnsavedDB')
  const release1 = w.store.acquireLock([5, 5, undefined])
  const release2 = w.store.acquireLock([5])
  assert.equal(w.store.isLocked(5), true)
  w.editor.setDraft('Ignored')
  await w.editor.save()
  assert.equal(w.editor.draft, 'UnsavedDB')
  release1(); release1()
  assert.equal(w.store.isLocked(5), true)
  release2()
  assert.equal(w.store.isLocked(5), false)
  w.editor.resetDraft()
  assert.equal(w.editor.draft, 'OriginalDB')
  w.store.dispose()
})

test('empty input is invalid and unsupported source owners never save', async () => {
  const w = workspace()
  w.editor.setDraft('   ')
  await w.editor.save()
  assert.equal(w.editor.error, '请输入数据库名称')
  w.connections.value = [{ ...original, db_type: 'oracle' }]
  w.editor.setDraft('AnotherOwner')
  await w.editor.save()
  assert.equal(w.connections.value[0].database, 'OriginalDB')
  w.store.dispose()
})

function component(store, props) {
  const { descriptor } = parse(readFileSync(new URL('../src/components/DatabaseNameEditor.vue', import.meta.url), 'utf8'))
  const module = loadConnectionDatabaseEditors({}, { inject: () => store })
  const messages = []
  const context = {
    exports: {}, defineProps: () => props,
    require: id => {
      if (id === 'vue') return Vue
      if (id === '@arco-design/web-vue') return { Message: { success: value => messages.push(value) } }
      if (id === '@/composables/connectionDatabaseEditors') return module
      if (id === '@/utils/connectionDatabase') return connectionDatabaseUtils
      throw new Error(`Unexpected import ${id}`)
    },
  }
  vm.runInNewContext(ts.transpileModule(`${descriptor.scriptSetup.content}\nexport { editor, locked, save, supportsDatabaseEditing };`, {
    compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
  }).outputText, context)
  const code = compile(descriptor.template.content, { mode: 'function', prefixIdentifiers: true }).code
  const render = vm.runInNewContext(`(function() { ${code} })()`, { Vue })
  const controls = {}
  const app = Vue.createSSRApp({ setup: () => ({ ...context.exports, ...Vue.toRefs(props) }), render })
  app.component('a-auto-complete', {
    props: ['modelValue', 'data', 'disabled', 'filterOption', 'allowClear'],
    setup: (p, { attrs }) => () => {
      controls.input = { props: p, update: attrs['onUpdate:modelValue'] }
      return Vue.h('input', { value: p.modelValue, disabled: p.disabled })
    },
  })
  app.component('a-button', {
    props: ['disabled', 'loading', 'type'],
    setup: (p, { attrs, slots }) => () => {
      const children = slots.default?.()
      if (children?.some(child => String(child.children).includes('验证并保存'))) controls.save = { props: p, click: attrs.onClick }
      return Vue.h('button', { disabled: p.disabled }, children)
    },
  })
  app.component('a-link', { setup: (_, { slots }) => () => Vue.h('a', slots.default?.()) })
  app.component('a-alert', {
    props: ['type', 'title'],
    setup: (p, { slots }) => () => Vue.h('div', { class: `arco-alert-${p.type}` }, [
      slots.icon?.(), Vue.h('strong', p.title), slots.default?.(),
    ]),
  })
  app.component('a-spin', { setup: () => () => Vue.h('span', { class: 'arco-spin' }) })
  return { values: context.exports, messages, controls, render: () => renderToString(app) }
}

test('editor input immediately updates the shared draft and saves even when it is absent from the list', async () => {
  const scope = Vue.effectScope()
  try {
    const w = workspace({ updateConnectionDatabase: async (_, database) => ({ data: { ...original, database } }) })
    const props = Vue.reactive({ connection: w.connections.value[0], disabled: false })
    const ui = scope.run(() => component(w.store, props))
    await ui.render()
    assert.equal(ui.controls.save.props.disabled, true)
    ui.controls.input.update('HandTypedDB')
    const dirtyHTML = await ui.render()
    assert.equal(w.store.isBlocked(5), true)
    assert.equal(ui.controls.save.props.disabled, false)
    assert.match(dirtyHTML, /尚未保存/)
    assert.match(dirtyHTML, /历史任务修复/)
    await ui.controls.save.click()
    assert.equal(w.connections.value[0].database, 'HandTypedDB')
    assert.deepEqual(ui.messages, ['数据库名称已验证并保存'])
    w.store.dispose()
  } finally { scope.stop() }
})

test('editor follows connection changes, hides owner-only databases and respects workspace locks', async () => {
  const scope = Vue.effectScope()
  try {
    const pending = deferred()
    const w = workspace({ listConnectionCatalogs: id => id === 5 ? pending.promise : Promise.resolve({ data: ['SecondDB'] }) })
    const second = { ...original, id: 6, database: 'SecondDB' }
    w.connections.value.push(second)
    const props = Vue.reactive({ connection: w.connections.value[0], disabled: false })
    const ui = scope.run(() => component(w.store, props))
    await ui.render()
    props.connection = second
    await Vue.nextTick()
    await ui.render()
    assert.equal(ui.controls.input.props.modelValue, 'SecondDB')
    pending.resolve({ data: ['StaleDB'] })
    await Vue.nextTick()
    assert.deepEqual([...ui.values.editor.value.catalogs], ['SecondDB'])
    const release = w.store.acquireLock([6])
    await ui.render()
    assert.equal(ui.controls.input.props.disabled, true)
    release()
    props.disabled = true
    await ui.render()
    assert.equal(ui.controls.input.props.disabled, true)
    props.connection = { ...original, db_type: 'oracle' }
    await Vue.nextTick()
    assert.doesNotMatch(await ui.render(), /数据库名称|验证并保存/)
    w.store.dispose()
  } finally { scope.stop() }
})
