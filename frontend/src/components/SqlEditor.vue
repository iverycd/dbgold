<template>
  <div class="sql-editor" :class="{ 'sql-editor-auto-height': autoHeight }">
    <div v-if="formatDbType" class="sql-format-toolbar">
      <a-button size="mini" :disabled="formatDisabled || readOnly || !modelValue.trim()" @mousedown.prevent @click="formatSQL">格式化 SQL</a-button>
      <span>Ctrl / ⌘ + Shift + F · 选中时格式化选区，否则格式化全文</span>
    </div>
    <div ref="editorHost" class="sql-editor-host"></div>
  </div>
</template>

<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { basicSetup } from 'codemirror'
import { EditorState, Compartment, Prec } from '@codemirror/state'
import { EditorView, keymap } from '@codemirror/view'
import { sql, MySQL, PostgreSQL } from '@codemirror/lang-sql'
import { Message } from '@arco-design/web-vue'
import { createSQLFormatTransaction } from '@/utils/sqlFormatting'

const props = defineProps<{
  modelValue: string
  dialect: 'mysql' | 'postgres' | 'gaussdb'
  executionMode?: 'statement' | 'document'
  formatDbType?: string
  formatDisabled?: boolean
  autoHeight?: boolean
  readOnly?: boolean
}>()

const emit = defineEmits<{
  'update:modelValue': [value: string]
  'selection-change': [hasSelection: boolean]
  execute: [sql: string]
}>()

const editorHost = ref<HTMLElement | null>(null)
const dialectCompartment = new Compartment()
const readOnlyCompartment = new Compartment()
const lineSeparatorCompartment = new Compartment()
let view: EditorView | null = null

function languageExtension() {
  return sql({ dialect: props.dialect === 'mysql' ? MySQL : PostgreSQL })
}

function selectedSQL(state: EditorState): string {
  const { from, to } = state.selection.main
  return from === to ? '' : state.sliceDoc(from, to).trim()
}

function selectedOrCurrentSQL(state: EditorState): string {
  const selection = selectedSQL(state)
  if (selection) return selection
  const text = state.doc.toString()
  const cursor = state.selection.main.head
  let start = 0
  let previousStatement = ''
  let hasContent = false
  let quote = ''
  let lineComment = false
  let blockComment = false
  for (let i = 0; i < text.length; i += 1) {
    const ch = text[i]
    const next = text[i + 1]
    if (lineComment) {
      if (ch === '\n') lineComment = false
      continue
    }
    if (blockComment) {
      if (ch === '*' && next === '/') {
        blockComment = false
        i += 1
      }
      continue
    }
    if (quote) {
      if (ch === quote && next === quote) {
        i += 1
      } else if (ch === quote && text[i - 1] !== '\\') {
        quote = ''
      }
      continue
    }
    if ((ch === '-' && next === '-') || ch === '#') {
      lineComment = true
      if (ch === '-') i += 1
      continue
    }
    if (ch === '/' && next === '*') {
      blockComment = true
      i += 1
      continue
    }
    if (ch === "'" || ch === '"' || ch === '`') {
      quote = ch
      hasContent = true
      continue
    }
    if (ch === ';') {
      const statement = hasContent ? text.slice(start, i + 1).trim() : ''
      if (i >= cursor) return statement
      if (statement) previousStatement = statement
      start = i + 1
      hasContent = false
    } else if (!hasContent && !/\s/.test(ch)) {
      hasContent = true
    }
  }
  return hasContent ? text.slice(start).trim() : previousStatement
}

function documentSeparator(value: string): string {
  // CodeMirror splits on this separator and uses it to serialize the document.
  // Using LF for mixed endings also preserves carriage returns in CRLF lines.
  return value.includes('\r\n') && !/(^|[^\r])\n|\r(?!\n)/.test(value) ? '\r\n' : '\n'
}
function getExecutableSQL(): string {
  if (props.executionMode === 'document') return view ? view.state.sliceDoc() : props.modelValue
  return view ? selectedOrCurrentSQL(view.state) : props.modelValue.trim()
}

function formatSQL(): void {
  if (!view || !props.formatDbType || props.formatDisabled || props.readOnly) return
  try {
    const transaction = createSQLFormatTransaction(view.state, props.formatDbType)
    if (transaction) view.dispatch(transaction)
    view.focus()
  } catch {
    Message.warning('格式化失败，请检查 SQL 或目标数据库语法')
  }
}

defineExpose({ getExecutableSQL })

onMounted(() => {
  if (!editorHost.value) return
  const executeKeymap = keymap.of([{
    key: 'Mod-Enter',
    run: () => {
      emit('execute', getExecutableSQL())
      return true
    },
  }])
  view = new EditorView({
    parent: editorHost.value,
    state: EditorState.create({
      doc: props.modelValue,
      extensions: [
        basicSetup,
        lineSeparatorCompartment.of(props.executionMode === 'document' ? EditorState.lineSeparator.of(documentSeparator(props.modelValue)) : []),
        dialectCompartment.of(languageExtension()),
        readOnlyCompartment.of([EditorState.readOnly.of(!!props.readOnly), EditorView.editable.of(!props.readOnly)]),
        Prec.highest(keymap.of([{
          key: 'Mod-Shift-f',
          run: () => {
            if (!props.formatDbType) return false
            formatSQL()
            return true
          },
        }])),
        Prec.highest(executeKeymap),
        EditorView.updateListener.of((update) => {
          if (update.docChanged) emit('update:modelValue', update.state.sliceDoc())
          if (update.docChanged || update.selectionSet) {
            const hasSelection = Boolean(selectedSQL(update.state))
            if (hasSelection !== Boolean(selectedSQL(update.startState))) emit('selection-change', hasSelection)
          }
        }),
        EditorView.theme({
          '&': { height: '100%', fontSize: '13px' },
          '.cm-scroller': { fontFamily: 'var(--font-mono)', overflow: 'auto' },
          '.cm-gutters': { background: '#F8FAFC', borderRight: '1px solid #E2E8F0' },
          '&.cm-focused': { outline: 'none' },
        }),
      ],
    }),
  })
  emit('selection-change', Boolean(selectedSQL(view.state)))
})

watch(() => props.modelValue, (value) => {
  if (!view || value === view.state.sliceDoc()) return
  if (props.executionMode === 'document' && view.state.lineBreak !== documentSeparator(value)) {
    view.dispatch({ effects: lineSeparatorCompartment.reconfigure(EditorState.lineSeparator.of(documentSeparator(value))) })
  }
  view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: value } })
})

watch(() => props.dialect, () => {
  view?.dispatch({ effects: dialectCompartment.reconfigure(languageExtension()) })
})
watch(() => props.readOnly, value => {
  view?.dispatch({ effects: readOnlyCompartment.reconfigure([EditorState.readOnly.of(!!value), EditorView.editable.of(!value)]) })
})

onBeforeUnmount(() => view?.destroy())
</script>

<style scoped>
.sql-editor {
  display: flex;
  flex-direction: column;
  height: 100%;
  min-height: 220px;
  background: #fff;
}
.sql-format-toolbar {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
  padding: 6px 10px;
  border-bottom: 1px solid var(--color-border-2);
  font-size: 12px;
  color: var(--color-text-3);
}
.sql-editor-host {
  flex: 1;
  min-height: 0;
  overflow: hidden;
}
.sql-editor :deep(.cm-editor) {
  height: 100%;
}
.sql-editor-auto-height {
  height: auto;
  min-height: 0;
}
.sql-editor-auto-height .sql-editor-host {
  flex: none;
}
.sql-editor-auto-height :deep(.cm-editor) {
  height: auto;
}
.sql-editor-auto-height :deep(.cm-scroller) {
  min-height: 64px;
  max-height: 240px;
  overflow: auto;
}
</style>
