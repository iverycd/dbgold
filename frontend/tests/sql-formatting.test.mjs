import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import * as Vue from 'vue'
import { parse } from '@vue/compiler-sfc'

const require = createRequire(import.meta.url)
const { EditorState, EditorSelection } = require('@codemirror/state')
const { EditorView, keymap } = require('@codemirror/view')
const { history, undo, redo } = require('@codemirror/commands')
const transpile = source => ts.transpileModule(source, {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
}).outputText
const utilsContext = { exports: {}, require }
vm.runInNewContext(transpile(readFileSync(new URL('../src/utils/sqlFormatting.ts', import.meta.url), 'utf8')), utilsContext)
const { createSQLFormatTransaction } = utilsContext.exports

const longSQL = `Create View "LongView" AS select a."OrderId", case when a.Status = 'MiXeD;value' then Coalesce(b.Name, 'Default') else 'Other' end as Label from "Orders" a left join (select OrderId, Name from Details where Enabled = 1) b on a."OrderId" = b.OrderId where a.Status <> 'Closed' and a.Total > 100; -- Keep Comment`

function state(doc, selection) {
  return EditorState.create({ doc, selection, extensions: [history()] })
}
function formatState(input, dbType = 'postgres') {
  const transaction = createSQLFormatTransaction(input, dbType)
  return transaction ? input.update(transaction).state : input
}

test('long view SQL is indented while preserving mixed case, comments and literals', () => {
  const formatted = formatState(state(longSQL)).doc.toString()
  assert.ok(formatted.split('\n').length > 15)
  assert.match(formatted, /\n  a\."OrderId",/)
  assert.match(formatted, /\n  left join \(\n    select/)
  for (const text of ['Create View "LongView" AS', 'select', 'Coalesce', "'MiXeD;value'", "'Default'", '-- Keep Comment']) assert.ok(formatted.includes(text), text)
  assert.equal(createSQLFormatTransaction(state(formatted), 'postgres'), null, 'formatting is idempotent')
})

test('each target database uses its compatible dialect', () => {
  const cases = [
    ['mysql', 'select `OrderId` from `Orders` where Name = \'MiXeD\';'],
    ['postgres', 'select "OrderId"::integer from "Orders";'],
    ['sqlserver', 'select top 10 [OrderId] from [dbo].[Orders];'],
    ['oracle', 'select NVL("OrderId", 0) from "Orders";'],
    ['dameng', 'select NVL("OrderId", 0) from "Orders";'],
    ['oscar', 'select "OrderId" from "Orders";'],
  ]
  for (const [dbType, sql] of cases) assert.ok(formatState(state(sql), dbType).doc.toString().includes('\n'), dbType)
  const pgSQL = 'select "OrderId"::integer from "Orders";'
  const expected = formatState(state(pgSQL)).doc.toString()
  for (const dbType of ['gaussdb', 'highgo', 'vastbase', 'gbase', 'seabox', 'kingbase']) assert.equal(formatState(state(pgSQL), dbType).doc.toString(), expected, dbType)
})

test('selected SQL remains selected and surrounding text is unchanged', () => {
  const prefix = '-- untouched prefix\n', selected = 'select Id, Name from Orders where Id > 1;', suffix = '\nselect unchanged from Other;'
  const input = state(prefix + selected + suffix, EditorSelection.range(prefix.length + selected.length, prefix.length))
  const output = formatState(input)
  assert.ok(output.doc.toString().startsWith(prefix))
  assert.ok(output.doc.toString().endsWith(suffix))
  assert.equal(output.selection.main.from, prefix.length)
  assert.equal(output.selection.main.to, output.doc.length - suffix.length)
  assert.equal(output.sliceDoc(output.selection.main.from, output.selection.main.to), formatState(state(selected)).doc.toString())
})

test('empty text, whitespace selection and parse failures leave state untouched', () => {
  for (const text of ['', ' \n\t ']) assert.equal(createSQLFormatTransaction(state(text), 'mysql'), null)
  const whitespaceSelection = state('select Id from Orders;   ', EditorSelection.range(22, 25))
  assert.equal(createSQLFormatTransaction(whitespaceSelection, 'postgres'), null)
  const input = state("select 'unterminated", EditorSelection.cursor(4))
  assert.throws(() => createSQLFormatTransaction(input, 'postgres'), /Parse error/)
  assert.equal(input.doc.toString(), "select 'unterminated")
  assert.equal(input.selection.main.head, 4)
})

test('formatting is one undo step separate from typing before and after it', () => {
  const original = 'select Id from Orders'
  const target = { state: state(original), dispatch: transaction => { target.state = transaction.state } }
  target.state = target.state.update({ changes: { from: original.length, insert: ';' }, userEvent: 'input.type' }).state
  const beforeFormatting = target.state
  target.state = formatState(target.state)
  const formatted = target.state.doc.toString()
  assert.notEqual(formatted, beforeFormatting.doc.toString())
  assert.equal(undo(target), true)
  assert.equal(target.state.doc.toString(), beforeFormatting.doc.toString())
  assert.equal(redo(target), true)
  assert.equal(target.state.doc.toString(), formatted)
  target.state = target.state.update({ changes: { from: target.state.doc.length, insert: ' ' }, userEvent: 'input.type' }).state
  undo(target)
  assert.equal(target.state.doc.toString(), formatted)
  undo(target)
  assert.equal(target.state.doc.toString(), beforeFormatting.doc.toString())
  undo(target)
  assert.equal(target.state.doc.toString(), original)
})

const descriptor = parse(readFileSync(new URL('../src/components/SqlEditor.vue', import.meta.url), 'utf8')).descriptor
function editor(initialProps) {
  const lifecycle = {}, messages = [], emitted = [], props = Vue.reactive(initialProps)
  let view
  class TestView {
    static theme = EditorView.theme
    static updateListener = EditorView.updateListener
    constructor(config) { this.state = config.state; view = this }
    dispatch = (spec) => {
      const transaction = spec.state ? spec : this.state.update(spec)
      this.state = transaction.state
      for (const listener of this.state.facet(EditorView.updateListener)) listener({ docChanged: transaction.docChanged, state: this.state })
    }
    focus() {}
    destroy() {}
  }
  const context = {
    exports: {}, defineProps: () => props, defineExpose() {},
    defineEmits: () => (event, value) => { emitted.push([event, value]); if (event === 'update:modelValue') props.modelValue = value },
    require: id => {
      if (id === 'vue') return { ...Vue, onMounted: fn => { lifecycle.mount = fn }, onBeforeUnmount() {} }
      if (id === '@codemirror/view') return { ...require(id), EditorView: TestView }
      if (id === '@/utils/sqlFormatting') return utilsContext.exports
      if (id === '@arco-design/web-vue') return { Message: { warning: text => messages.push(text) } }
      return require(id)
    },
  }
  vm.runInNewContext(transpile(`${descriptor.scriptSetup.content}\nexport { editorHost, formatSQL, getExecutableSQL };`), context)
  context.exports.editorHost.value = {}
  lifecycle.mount()
  return { ...context.exports, view, messages, props, emitted, shortcut: () => view.state.facet(keymap).flat().find(binding => binding.key === 'Mod-Shift-f').run(view) }
}

test('editor button command and shortcut update only their unit and support undo', async () => {
  const scope = Vue.effectScope()
  try {
    const first = scope.run(() => editor({ modelValue: longSQL, dialect: 'postgres', formatDbType: 'gaussdb', executionMode: 'document' }))
    const second = scope.run(() => editor({ modelValue: 'select OtherId from Other;', dialect: 'postgres', formatDbType: 'postgres' }))
    assert.equal(first.view.state.doc.toString(), longSQL, 'mount does not format automatically')
    first.formatSQL()
    await Vue.nextTick()
    const formatted = first.props.modelValue
    assert.ok(formatted.includes('\n'))
    assert.equal(first.getExecutableSQL(), formatted)
    assert.equal(second.props.modelValue, 'select OtherId from Other;')
    undo(first.view)
    await Vue.nextTick()
    assert.equal(first.props.modelValue, longSQL)
    assert.equal(first.shortcut(), true)
    await Vue.nextTick()
    assert.equal(first.props.modelValue, formatted)
    assert.ok(first.emitted.every(([event]) => event === 'update:modelValue'), 'formatting never executes SQL')
  } finally { scope.stop() }
})

test('editor disables formatting dynamically and does not claim the query center shortcut', () => {
  const scope = Vue.effectScope()
  try {
    const e = scope.run(() => editor({ modelValue: 'select Id from Orders;', dialect: 'postgres', formatDbType: 'postgres', formatDisabled: true }))
    e.formatSQL(); e.shortcut()
    assert.equal(e.emitted.length, 0)
    e.props.formatDisabled = false
    e.props.formatDbType = undefined
    assert.equal(e.shortcut(), false)
    e.formatSQL()
    assert.equal(e.emitted.length, 0)
    e.props.formatDbType = 'postgres'
    e.shortcut()
    assert.equal(e.emitted.length, 1)
  } finally { scope.stop() }
})

test('editor reports parse errors without changing its document or selection', () => {
  const scope = Vue.effectScope()
  try {
    const e = scope.run(() => editor({ modelValue: "select 'unterminated", dialect: 'postgres', formatDbType: 'postgres' }))
    e.view.dispatch({ selection: EditorSelection.range(0, e.props.modelValue.length) })
    const selection = e.view.state.selection
    e.formatSQL()
    assert.equal(e.view.state.selection, selection)
    assert.equal(e.view.state.doc.toString(), "select 'unterminated")
    assert.equal(e.emitted.length, 0)
    assert.deepEqual(e.messages, ['格式化失败，请检查 SQL 或目标数据库语法'])
  } finally { scope.stop() }
})
