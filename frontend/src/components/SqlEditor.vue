<template>
  <div ref="editorHost" class="sql-editor"></div>
</template>

<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { basicSetup } from 'codemirror'
import { EditorState, Compartment } from '@codemirror/state'
import { EditorView, keymap } from '@codemirror/view'
import { sql, MySQL, PostgreSQL } from '@codemirror/lang-sql'

const props = defineProps<{
  modelValue: string
  dialect: 'mysql' | 'postgres' | 'gaussdb'
}>()

const emit = defineEmits<{
  'update:modelValue': [value: string]
  execute: [sql: string]
}>()

const editorHost = ref<HTMLElement | null>(null)
const dialectCompartment = new Compartment()
let view: EditorView | null = null

function languageExtension() {
  return sql({ dialect: props.dialect === 'mysql' ? MySQL : PostgreSQL })
}

function selectedOrCurrentSQL(state: EditorState): string {
  const selection = state.sliceDoc(state.selection.main.from, state.selection.main.to).trim()
  if (selection) return selection
  const text = state.doc.toString()
  const cursor = state.selection.main.head
  let start = 0
  let end = text.length
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
      continue
    }
    if (ch === ';') {
      if (i < cursor) start = i + 1
      else {
        end = i + 1
        break
      }
    }
  }
  return text.slice(start, end).trim()
}

function getExecutableSQL(): string {
  return view ? selectedOrCurrentSQL(view.state) : props.modelValue.trim()
}

defineExpose({ getExecutableSQL })

onMounted(() => {
  if (!editorHost.value) return
  view = new EditorView({
    parent: editorHost.value,
    state: EditorState.create({
      doc: props.modelValue,
      extensions: [
        basicSetup,
        dialectCompartment.of(languageExtension()),
        keymap.of([{
          key: 'Mod-Enter',
          run: (stateView) => {
            emit('execute', selectedOrCurrentSQL(stateView.state))
            return true
          },
        }]),
        EditorView.updateListener.of((update) => {
          if (update.docChanged) emit('update:modelValue', update.state.doc.toString())
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
})

watch(() => props.modelValue, (value) => {
  if (!view || value === view.state.doc.toString()) return
  view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: value } })
})

watch(() => props.dialect, () => {
  view?.dispatch({ effects: dialectCompartment.reconfigure(languageExtension()) })
})

onBeforeUnmount(() => view?.destroy())
</script>

<style scoped>
.sql-editor {
  height: 100%;
  min-height: 220px;
  background: #fff;
}
.sql-editor :deep(.cm-editor) {
  height: 100%;
}
</style>
