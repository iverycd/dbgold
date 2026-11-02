<template>
  <section v-if="repairs.length" class="auto-repair-panel" aria-label="自动修复明细">
    <a-alert type="info">
      以下为已自动修复或经校验复用的字段调整，请据此核对业务 SQL 中的字段引用与类型。
    </a-alert>
    <a-table
      :data="groups"
      row-key="key"
      :pagination="{ current: currentPage, pageSize: 10, showPageSize: false, showTotal: true }"
      :expandable="{ expandedRowKeys, width: 40 }"
      size="small"
      :scroll="{ x: 820 }"
      @expand="toggleGroup"
      @page-change="changePage"
    >
      <template #columns>
        <a-table-column title="源表" :width="220">
          <template #cell="{ record }"><code>{{ record.table }}</code></template>
        </a-table-column>
        <a-table-column title="目标表" :width="220">
          <template #cell="{ record }"><code>{{ record.target_table }}</code></template>
        </a-table-column>
        <a-table-column title="调整数" data-index="itemCount" :width="90" />
        <a-table-column title="修复情况" :width="190">
          <template #cell="{ record }">
            <div class="repair-status-summary">
              <span>本次已修复 {{ record.applied }} 项</span>
              <span>复用已有映射 {{ record.reused }} 项</span>
            </div>
          </template>
        </a-table-column>
        <a-table-column title="操作" :width="100">
          <template #cell="{ record }">
            <a-button type="text" size="mini" :aria-expanded="expandedRowKeys.includes(record.key)" @click="toggleGroup(record.key)">
              {{ expandedRowKeys.includes(record.key) ? '收起明细' : '查看明细' }}
            </a-button>
          </template>
        </a-table-column>
      </template>
      <template #expand-row="{ record }">
        <div class="repair-details">
          <p v-if="record.commonReason" class="repair-reason">调整原因：{{ record.commonReason }}</p>
          <a-table
            :key="record.key"
            :data="record.items"
            :pagination="{ current: detailPage, pageSize: 10, showPageSize: false, showTotal: true }"
            :scroll="{ x: record.commonReason ? 880 : 1120 }"
            size="small"
            @page-change="detailPage = $event"
          >
            <template #columns>
              <a-table-column title="源字段" :width="180">
                <template #cell="{ record: item }"><code>{{ item.source }}</code></template>
              </a-table-column>
              <a-table-column title="源列类型" :width="180">
                <template #cell="{ record: item }"><code>{{ item.source_type || '未记录' }}</code></template>
              </a-table-column>
              <a-table-column title="目标结果" :width="220">
                <template #cell="{ record: item }"><code>{{ item.target }}</code></template>
              </a-table-column>
              <a-table-column title="调整类型" :width="110">
                <template #cell="{ record: item }">{{ autoRepairType(item.type) }}</template>
              </a-table-column>
              <a-table-column v-if="!record.commonReason" title="调整原因" data-index="reason" />
              <a-table-column title="状态" :width="150">
                <template #cell="{ record: item }">
                  <a-tag :color="item.status === 'reused' ? 'blue' : 'green'">{{ autoRepairStatus(item.status) }}</a-tag>
                </template>
              </a-table-column>
            </template>
          </a-table>
        </div>
      </template>
    </a-table>
  </section>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import type { AutoRepair } from '@/api/migration'
import { autoRepairStatus, autoRepairType, groupAutoRepairs } from '@/utils/autoRepairs'

const props = defineProps<{ repairs: AutoRepair[] }>()
const groups = computed(() => groupAutoRepairs(props.repairs).map(group => {
  const reused = group.items.filter(item => item.status === 'reused').length
  const reason = group.items[0]?.reason
  return {
    ...group,
    itemCount: group.items.length,
    applied: group.items.length - reused,
    reused,
    commonReason: reason && group.items.every(item => item.reason === reason) ? reason : null,
  }
}))
const currentPage = ref(1)
const expandedRowKeys = ref<(string | number)[]>([])
const detailPage = ref(1)

function toggleGroup(key: string | number) {
  expandedRowKeys.value = expandedRowKeys.value.includes(key) ? [] : [key]
  detailPage.value = 1
}

function changePage(page: number) {
  currentPage.value = page
  expandedRowKeys.value = []
  detailPage.value = 1
}

watch(() => props.repairs, () => changePage(1), { deep: true })
</script>

<style scoped>
.auto-repair-panel { display: grid; gap: 12px; margin: 12px 0; min-width: 0; }
.repair-status-summary { display: grid; gap: 4px; color: var(--color-text-2); font-size: 12px; }
.repair-details { padding: 12px; background: var(--color-fill-1); }
.repair-reason { margin: 0 0 12px; color: var(--color-text-2); font-size: 13px; overflow-wrap: anywhere; }
.auto-repair-panel :deep(.arco-table-cell) { vertical-align: top; overflow-wrap: anywhere; }
code { color: var(--color-text-1); font-family: 'Menlo', 'Monaco', 'Courier New', monospace; white-space: pre-wrap; overflow-wrap: anywhere; }
</style>
