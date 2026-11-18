<template>
  <div ref="panel" class="row-count-panel" tabindex="-1" aria-label="行数对比">
    <p class="row-summary">{{ summary }}</p>
    <div class="row-filters">
      <a-radio-group v-model="filter" type="button" aria-label="筛选行数对比">
        <a-radio value="all">全部</a-radio>
        <a-radio value="attention">仅需核对</a-radio>
      </a-radio-group>
      <a-input-search v-model="keyword" allow-clear placeholder="搜索表名" aria-label="搜索行数对比表名" />
    </div>
    <a-table
      :data="visibleRows" row-key="table" size="small" :scroll="{ x: 740 }"
      :pagination="{ pageSize: 10, current: page }" @page-change="page = $event"
      :row-class="record => record.table === focusedTable ? 'focused-count-row' : ''"
    >
      <template #columns>
        <a-table-column title="表名"><template #cell="{ record }"><span tabindex="-1" data-count-name>{{ record.table }}</span></template></a-table-column>
        <a-table-column title="源行数" :width="120"><template #cell="{ record }">{{ record.comparable ? record.row.src.toLocaleString('zh-CN') : '—' }}</template></a-table-column>
        <a-table-column title="目标行数" :width="120"><template #cell="{ record }">{{ record.comparable ? record.row.dst.toLocaleString('zh-CN') : '—' }}</template></a-table-column>
        <a-table-column title="差异" :width="100">
          <template #cell="{ record }"><span :class="{ 'row-difference': record.mismatch }">{{ record.comparable ? signedDifference(record.row.dst - record.row.src) : '—' }}</span></template>
        </a-table-column>
        <a-table-column title="校验结果" :width="200">
          <template #cell="{ record }">
            <a-tag :color="record.color">{{ record.label }}</a-tag>
            <div v-if="record.detail && (!record.mismatch || record.color === 'red')" class="issue-reason">{{ record.detail }}</div>
          </template>
        </a-table-column>
      </template>
    </a-table>
  </div>
</template>

<script setup lang="ts">
import { computed, nextTick, ref, watch } from 'vue'
import type { rowCountEntries } from '@/utils/migrationDataIssues'

const props = defineProps<{ entries: ReturnType<typeof rowCountEntries>; summary: string; attentionOnly?: boolean }>()
const panel = ref<HTMLElement | null>(null)
const page = ref(1)
const filter = ref<'all' | 'attention'>(props.attentionOnly && props.entries.some(entry => entry.color !== 'green') ? 'attention' : 'all')
const keyword = ref('')
const focusedTable = ref('')
const visibleRows = computed(() => {
  const search = keyword.value.trim().toLowerCase()
  const shown = props.entries.filter(entry => (filter.value === 'all' || entry.color !== 'green') && entry.table.toLowerCase().includes(search))
  return [...shown.filter(entry => entry.color !== 'green'), ...shown.filter(entry => entry.color === 'green')]
})
watch([filter, keyword], () => { page.value = 1; focusedTable.value = '' }, { flush: 'sync' })

function signedDifference(count: number) {
  return count.toLocaleString('zh-CN', { signDisplay: 'exceptZero' })
}
async function focusTable(table?: string) {
  keyword.value = ''
  filter.value = table ? 'all' : 'attention'
  focusedTable.value = table || ''
  const position = table ? visibleRows.value.findIndex(entry => entry.table === table) : 0
  page.value = Math.floor(Math.max(position, 0) / 10) + 1
  await nextTick()
  const target = panel.value?.querySelector<HTMLElement>('.focused-count-row [data-count-name]') || panel.value
  target?.focus({ preventScroll: true })
  target?.scrollIntoView({ block: 'nearest' })
}
defineExpose({ focusTable })
</script>

<style scoped>
.row-summary { margin: 0 0 12px; color: var(--color-text-2); }
.row-filters { display: flex; flex-wrap: wrap; align-items: center; gap: 8px 16px; margin-bottom: 12px; }
.row-filters :deep(.arco-input-wrapper) { width: 220px; }
.row-difference { color: rgb(var(--orange-7)); }
.issue-reason { margin-top: 4px; color: var(--color-text-2); overflow-wrap: anywhere; }
.row-count-panel :deep(.focused-count-row .arco-table-td) { background: var(--color-warning-light-1); }
</style>
