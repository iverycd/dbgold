import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import * as Vue from 'vue'
import { parse } from '@vue/compiler-sfc'
import { compile } from '@vue/compiler-dom'
import { renderToString } from '@vue/server-renderer'

const { descriptor } = parse(readFileSync(new URL('../src/views/MigrationView.vue', import.meta.url), 'utf8'))
const modalTemplate = descriptor.template.content.match(/<a-modal[\s\S]*?<\/a-modal>/)[0]
const renderModal = new Function('Vue', compile(modalTemplate, { mode: 'function' }).code)(Vue)

function page({ start, preflight, objectStart } = {}) {
  const starts = [], checks = [], errors = []
  const compiled = ts.transpileModule(`${descriptor.scriptSetup.content}
    export { connections, dataMigrate, canStartMigration, requestDataMigration, confirmDataMigration,
      dataMigrationConfirmVisible, dataMigrationStarting, dataMigrationContentLabel,
      dataMigrationScopeLabel, selectedDst, targetDatabaseName, objMigrate, startObjectMigration };`, {
    compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
  })
  const context = {
    exports: {}, AbortController,
    require: id => {
      switch (id) {
        case 'vue': return { ...Vue, onMounted() {}, onUnmounted() {} }
        case 'vue-router': return { useRoute: () => ({ query: {} }), onBeforeRouteLeave() {} }
        case '@arco-design/web-vue': return { Message: { error: msg => errors.push(msg) }, Modal: {} }
        case '@/utils/migrationOptions': return { applyTargetMigrationDefaults() {} }
        case '@/api/migration': return {
          startDataMigration: async request => { starts.push(request); return start ? start(request) : { data: { job_id: 'new-job' } } },
          startObjectMigration: async request => objectStart ? objectStart(request) : { data: { job_id: 'object-job' } },
          preflightDataMigration: async request => { checks.push(request); return preflight ? preflight(request) : { data: { ready: true } } },
          createDataMigrateEventSource: () => ({ addEventListener() {}, close() {} }),
        }
        default: return {}
      }
    },
  }
  vm.runInNewContext(compiled.outputText, context)
  const p = context.exports
  p.connections.value = [
    { id: 1, name: '源连接', db_type: 'mysql', host: 'source', port: 3306, database: 'source_db' },
    { id: 2, name: '目标连接', db_type: 'postgres', host: 'target', port: 5432, database: 'target_db' },
  ]
  Object.assign(p.dataMigrate, { srcConnId: 1, dstConnId: 2, srcDatabase: 'source_db', dstSchema: 'public' })
  return { p, starts, checks, errors }
}

async function renderConfirmation(p) {
  let modalProps, alertProps
  const app = Vue.createSSRApp({ render: () => renderModal(Vue.proxyRefs(p), []) })
  app.component('a-modal', {
    props: ['visible', 'title', 'width', 'maskClosable', 'closable', 'escToClose', 'okText', 'cancelText', 'okButtonProps'],
    setup(props, { slots }) { modalProps = props; return () => Vue.h('section', slots.default()) },
  })
  app.component('a-alert', {
    props: ['type'],
    setup(props, { slots }) { alertProps = props; return () => Vue.h('aside', slots.default()) },
  })
  return { html: await renderToString(app), modalProps, alertProps }
}

test('confirmation shows the selected destination, content, range and mode-specific warning', async () => {
  const scope = Vue.effectScope()
  try {
    const { p } = scope.run(() => page())
    for (const [content, label, status] of [
      ['both', '表结构 + 数据行', 'danger'],
      ['schema_only', '仅创建表结构', 'danger'],
      ['data_only', '仅迁移数据行', 'normal'],
    ]) {
      p.dataMigrate.content = content
      p.requestDataMigration()
      const { html, modalProps, alertProps } = await renderConfirmation(p)
      for (const text of ['目标连接', 'target:5432', 'target_db', 'public', label, '全库迁移']) assert.ok(html.includes(text), text)
      assert.equal(modalProps.title, '确认开始迁移')
      assert.equal(modalProps.maskClosable, false)
      assert.equal(modalProps.closable, true)
      assert.equal(modalProps.escToClose, true)
      assert.equal(modalProps.okText, '确认并开始')
      assert.equal(modalProps.cancelText, '取消')
      assert.equal(modalProps.okButtonProps.status, status)
      assert.equal(alertProps.type, content === 'data_only' ? 'info' : 'warning')
      assert.equal(html.includes('被删除重建'), content !== 'data_only')
      assert.equal(html.includes('现有数据会丢失'), content !== 'data_only')
      if (content === 'data_only') assert.ok(html.includes('向目标表写入数据'))
      p.dataMigrationConfirmVisible.value = false
    }
    for (const [mode, label] of [['include', '仅迁移指定表'], ['exclude', '排除指定表']]) {
      Object.assign(p.dataMigrate, { mode, filter: 'Orders,Audit_*' })
      assert.equal(p.dataMigrationScopeLabel.value, `${label}：Orders,Audit_*`)
    }
    p.connections.value[1].db_type = 'mysql'
    p.dataMigrate.dstSchema = 'another_database'
    const { html } = await renderConfirmation(p)
    assert.ok(html.includes('another_database'))
    assert.ok(!html.includes('目标 Schema'))
  } finally { scope.stop() }
})

test('invalid configuration is rejected before showing confirmation', () => {
  const scope = Vue.effectScope()
  try {
    const { p, starts, checks, errors } = scope.run(() => page())
    Object.assign(p.dataMigrate, { mode: 'include', filter: 'Orders;DROP' })
    p.requestDataMigration()
    assert.equal(p.dataMigrationConfirmVisible.value, false)
    p.dataMigrate.filter = 'Orders'
    p.dataMigrate.dstSchema = ''
    p.requestDataMigration()
    assert.equal(p.dataMigrationConfirmVisible.value, false)
    assert.deepEqual(errors, ['请选择目标 Schema'])
    p.connections.value[1].db_type = 'oracle'
    p.requestDataMigration()
    assert.equal(p.dataMigrationConfirmVisible.value, true, 'Oracle permits its default schema')
    assert.equal(starts.length, 0)
    assert.equal(checks.length, 0)
  } finally { scope.stop() }
})

test('opening and dismissing confirmation preserves existing configuration, logs and report', async () => {
  const scope = Vue.effectScope()
  try {
    const { p, starts, checks } = scope.run(() => page())
    Object.assign(p.dataMigrate, { finished: true, logs: ['old log'], currentJobId: 'old-job' })
    await Vue.nextTick()
    const before = JSON.stringify(p.dataMigrate)
    p.requestDataMigration()
    assert.equal(p.dataMigrationConfirmVisible.value, true)
    assert.equal(p.canStartMigration.value, false)
    p.requestDataMigration()
    assert.equal(JSON.stringify(p.dataMigrate), before)
    // Cancel, close and Escape all update the modal's visible model without launching.
    p.dataMigrationConfirmVisible.value = false
    await p.confirmDataMigration()
    assert.equal(JSON.stringify(p.dataMigrate), before)
    assert.equal(starts.length, 0)
    assert.equal(checks.length, 0)
    assert.equal(p.canStartMigration.value, true)
  } finally { scope.stop() }
})

test('confirmation submits once and blocks repeated clicks while the request or job is running', async () => {
  const scope = Vue.effectScope()
  try {
    let resolveStart
    const { p, starts } = scope.run(() => page({ start: () => new Promise(resolve => { resolveStart = resolve }) }))
    p.requestDataMigration()
    const pending = p.confirmDataMigration()
    assert.equal(p.dataMigrationConfirmVisible.value, false)
    assert.equal(p.dataMigrationStarting.value, true)
    assert.match(p.dataMigrate.logs[0], /正在提交迁移任务/)
    assert.equal(p.dataMigrate.currentJobId, '', 'submission feedback precedes the task ID and log stream')
    p.requestDataMigration()
    await p.confirmDataMigration()
    assert.equal(starts.length, 1)
    resolveStart({ data: { job_id: 'new-job' } })
    await pending
    assert.equal(p.dataMigrationStarting.value, false)
    assert.equal(p.dataMigrate.currentJobId, 'new-job')
    assert.match(p.dataMigrate.logs.at(-1), /迁移任务已创建/)
    p.requestDataMigration()
    await p.confirmDataMigration()
    assert.equal(starts.length, 1)
    assert.equal(p.canStartMigration.value, false)
  } finally { scope.stop() }
})

test('object migration shows submission feedback while awaiting the task ID', async () => {
  const scope = Vue.effectScope()
  try {
    let resolveStart
    const { p } = scope.run(() => page({ objectStart: () => new Promise(resolve => { resolveStart = resolve }) }))
    Object.assign(p.objMigrate, { srcConnId: 1, dstConnId: 2, dstSchema: 'public', selected: ['MixedTable'], objects: ['indexes'] })
    const pending = p.startObjectMigration()
    assert.equal(p.objMigrate.running, true)
    assert.match(p.objMigrate.logs[0], /正在提交迁移任务/)
    assert.equal(p.objMigrate.currentJobId, '')
    resolveStart({ data: { job_id: 'object-job' } })
    await pending
    assert.equal(p.objMigrate.currentJobId, 'object-job')
    assert.match(p.objMigrate.logs.at(-1), /迁移任务已创建/)
  } finally { scope.stop() }
})

test('PG/MySQL preflight runs only after confirmation and a blocked check allows another attempt', async () => {
  const scope = Vue.effectScope()
  try {
    let ready = false
    const { p, starts, checks } = scope.run(() => page({ preflight: async () => ({ data: { ready } }) }))
    p.connections.value[0].db_type = 'postgres'
    p.connections.value[1].db_type = 'mysql'
    await Vue.nextTick()
    p.requestDataMigration()
    assert.equal(checks.length, 0)
    await p.confirmDataMigration()
    assert.equal(checks.length, 1)
    assert.equal(starts.length, 0)
    assert.equal(p.canStartMigration.value, true)
    assert.match(p.dataMigrate.logs.at(-1), /迁移预检未通过，尚未启动任务/)
    ready = true
    p.requestDataMigration()
    await p.confirmDataMigration()
    assert.equal(checks.length, 2)
    assert.equal(starts.length, 1)
  } finally { scope.stop() }
})

test('a failed start releases the submission guard and can be retried with confirmation', async () => {
  const scope = Vue.effectScope()
  try {
    let fail = true
    const { p, starts } = scope.run(() => page({ start: async () => {
      if (fail) throw new Error('connection unavailable')
      return { data: { job_id: 'retried-job' } }
    } }))
    p.requestDataMigration()
    await p.confirmDataMigration()
    assert.equal(p.dataMigrate.finished, true)
    assert.match(p.dataMigrate.logs.at(-1), /connection unavailable/)
    assert.equal(p.canStartMigration.value, true)
    fail = false
    p.requestDataMigration()
    await p.confirmDataMigration()
    assert.equal(starts.length, 2)
    assert.equal(p.dataMigrate.currentJobId, 'retried-job')
  } finally { scope.stop() }
})
