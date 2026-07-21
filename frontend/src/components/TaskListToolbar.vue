<template>
  <div class="task-toolbar">
    <div class="task-filters">
      <a-input
        v-model="keyword"
        allow-clear
        class="task-search"
        placeholder="任务 ID / 连接 / IP / 库 / Schema"
        aria-label="搜索迁移任务"
      >
        <template #prefix><icon-search /></template>
      </a-input>
      <a-select
        v-model="status"
        allow-clear
        class="task-filter-select"
        placeholder="全部状态"
        aria-label="按状态筛选"
      >
        <a-option v-for="option in statusOptions" :key="option.value" :value="option.value">
          {{ option.label }}
        </a-option>
      </a-select>
      <a-select
        v-if="showOrigin"
        v-model="origin"
        class="task-filter-select"
        aria-label="按任务来源筛选"
      >
        <a-option value="all">全部来源</a-option>
        <a-option value="single">单次创建</a-option>
        <a-option value="batch">批量子任务</a-option>
      </a-select>
    </div>

    <div class="task-toolbar-meta">
      <span v-if="lastUpdated" class="last-updated">更新于 {{ lastUpdated }}</span>
      <a-button :loading="loading" aria-label="刷新任务列表" @click="$emit('refresh')">
        <template #icon><icon-refresh /></template>
        刷新
      </a-button>
    </div>
  </div>
</template>

<script setup lang="ts">
export interface TaskStatusOption {
  value: string
  label: string
}

withDefaults(defineProps<{
  statusOptions: TaskStatusOption[]
  showOrigin?: boolean
  loading?: boolean
  lastUpdated?: string
}>(), {
  showOrigin: false,
  loading: false,
  lastUpdated: '',
})

defineEmits<{ refresh: [] }>()

const keyword = defineModel<string>('keyword', { required: true })
const status = defineModel<string>('status', { required: true })
const origin = defineModel<'all' | 'single' | 'batch'>('origin', { default: 'all' })
</script>

<style scoped>
.task-toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  margin-bottom: 12px;
}
.task-filters,
.task-toolbar-meta {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
}
.task-search { width: min(320px, 34vw); }
.task-filter-select { width: 132px; }
.last-updated {
  color: var(--fg-muted);
  font-size: 12px;
  white-space: nowrap;
}
@media (max-width: 900px) {
  .task-toolbar { align-items: stretch; flex-direction: column; }
  .task-toolbar-meta { justify-content: flex-end; }
  .task-search { flex: 1; width: auto; }
}
</style>
