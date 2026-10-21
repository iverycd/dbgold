<template>
  <div v-if="connection && editor && supportsDatabaseEditing(connection.db_type)" class="database-editor">
    <div class="database-editor-label">
      <span>数据库</span>
      <a-tooltip :trigger="['hover', 'focus']" content="修改后将更新此连接的默认数据库；数据库必须已存在。">
        <button type="button" class="database-editor-help" aria-label="数据库修改说明">
          <icon-info-circle aria-hidden="true" />
        </button>
      </a-tooltip>
    </div>
    <div class="database-editor-controls">
      <a-auto-complete
        :model-value="editor.draft"
        :data="editor.catalogs"
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
    <a-alert
      v-if="editor.loadingCatalogs"
      type="info"
      title="正在加载数据库列表"
      class="database-editor-status database-editor-status-loading"
      role="status"
      aria-live="polite"
    >
      <template #icon><a-spin :size="16" aria-hidden="true" /></template>
      无需等待，可直接输入数据库名称。
    </a-alert>
    <a-alert
      v-else-if="editor.catalogError"
      type="warning"
      title="数据库列表加载失败"
      class="database-editor-status"
    >
      <div>{{ editor.catalogError }}</div>
      <div class="database-editor-status-actions">
        <span>可手动输入数据库名称。</span>
        <a-button type="text" size="mini" :disabled="editor.loadingCatalogs || locked" @click="editor.loadCatalogs(true)">重试</a-button>
      </div>
    </a-alert>
    <p v-else-if="editor.dirty" class="database-editor-hint">
      数据库名称尚未保存，请验证并保存后继续。
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
.database-editor-label { display: flex; align-items: center; gap: 4px; min-height: 18px; margin-bottom: 6px; color: var(--color-text-2); font-size: 12px; }
.database-editor-help { display: inline-flex; align-items: center; justify-content: center; width: 18px; height: 18px; padding: 0; border: 0; border-radius: 4px; background: transparent; color: var(--color-text-3); font-size: 13px; cursor: help; }
.database-editor-help:hover { background: var(--color-fill-2); color: var(--color-text-2); }
.database-editor-help:focus-visible { outline: 2px solid rgb(var(--primary-6)); outline-offset: 2px; }
.database-editor-controls { display: flex; align-items: center; gap: 8px; }
.database-editor-input { flex: 1; min-width: 0; }
.database-editor-status { margin-top: 10px; padding: 12px; border-radius: 6px; }
.database-editor-status :deep(.arco-alert-body) { min-width: 0; }
.database-editor-status :deep(.arco-alert-title) { font-size: 14px; font-weight: 600; }
.database-editor-status :deep(.arco-alert-content) { font-size: 13px; line-height: 1.6; overflow-wrap: anywhere; }
.database-editor-status-loading { background-color: rgb(var(--arcoblue-1)); border-color: rgb(var(--arcoblue-3)); }
.database-editor-status-loading :deep(.arco-alert-title),
.database-editor-status-loading :deep(.arco-spin-icon) { color: rgb(var(--arcoblue-7)); }
.database-editor-status-actions { display: flex; align-items: center; flex-wrap: wrap; gap: 4px 8px; margin-top: 4px; }
.database-editor-hint, .database-editor-error { margin: 6px 0 0; font-size: 12px; line-height: 1.5; }
.database-editor-hint { color: var(--color-text-3); }
.database-editor-error { color: rgb(var(--danger-6)); }
@media (max-width: 600px) {
  .database-editor-controls { flex-wrap: wrap; }
  .database-editor-input { flex-basis: 100%; }
}
</style>
