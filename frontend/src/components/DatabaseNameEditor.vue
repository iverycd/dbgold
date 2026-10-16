<template>
  <div v-if="connection && editor && supportsDatabaseEditing(connection.db_type)" class="database-editor">
    <label class="database-editor-label">数据库</label>
    <div class="database-editor-controls">
      <a-auto-complete
        :model-value="editor.draft"
        :data="editor.catalogs"
        :filter-option="false"
        :disabled="locked || editor.saving"
        allow-clear
        placeholder="选择或输入已有数据库名称"
        aria-label="数据库名称"
        class="database-editor-input"
        @update:model-value="editor.setDraft"
      />
      <a-button
        type="primary"
        :loading="editor.saving"
        :disabled="locked || !editor.dirty || !editor.draft.trim()"
        @click="save"
      >验证并保存</a-button>
      <a-button v-if="editor.dirty" :disabled="locked || editor.saving" @click="editor.resetDraft">撤销</a-button>
    </div>
    <p v-if="editor.error" class="database-editor-error" role="alert">{{ editor.error }}</p>
    <p v-if="editor.catalogError" class="database-editor-hint">
      {{ editor.catalogError }}，仍可手动输入数据库名称。
      <a-link :disabled="editor.loadingCatalogs || locked" @click="editor.loadCatalogs(true)">重试</a-link>
    </p>
    <p v-else-if="editor.loadingCatalogs" class="database-editor-hint">正在加载数据库列表，可直接输入名称。</p>
    <p v-else class="database-editor-hint">
      {{ editor.dirty ? '数据库名称尚未保存，请验证并保存后继续。' : '修改后将更新此连接的默认数据库；数据库必须已存在。' }}
    </p>
    <p v-if="editor.dirty" class="database-editor-hint">历史任务修复仍需使用指向原数据库的连接。</p>
  </div>
</template>

<script setup lang="ts">
import { computed, watch } from 'vue'
import { Message } from '@arco-design/web-vue'
import type { Connection } from '@/api/connections'
import { useConnectionDatabaseEditors } from '@/composables/connectionDatabaseEditors'
import { supportsDatabaseEditing } from '@/utils/connectionDatabase'

const props = defineProps<{ connection?: Connection | null; disabled?: boolean }>()
const editors = useConnectionDatabaseEditors()
const editor = computed(() => props.connection && supportsDatabaseEditing(props.connection.db_type)
  ? editors.editorFor(props.connection)
  : undefined)
const locked = computed(() => Boolean(props.disabled || editors.isLocked(props.connection?.id)))

watch(() => [props.connection?.id, props.connection?.database, props.connection?.host, props.connection?.port, props.connection?.username], () => {
  void editor.value?.loadCatalogs()
}, { immediate: true })

async function save() {
  if (locked.value) return
  const saved = await editor.value?.save()
  if (saved) Message.success('数据库名称已验证并保存')
}
</script>

<style scoped>
.database-editor { margin-top: 12px; }
.database-editor-label { display: block; margin-bottom: 6px; color: var(--color-text-2); font-size: 12px; }
.database-editor-controls { display: flex; align-items: center; gap: 8px; }
.database-editor-input { flex: 1; min-width: 0; }
.database-editor-hint, .database-editor-error { margin: 6px 0 0; font-size: 12px; line-height: 1.5; }
.database-editor-hint { color: var(--color-text-3); }
.database-editor-error { color: rgb(var(--danger-6)); }
@media (max-width: 600px) {
  .database-editor-controls { flex-wrap: wrap; }
  .database-editor-input { flex-basis: 100%; }
}
</style>
