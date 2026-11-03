<template>
  <a-table :data="rows" row-key="key" :pagination="{ pageSize: 10 }" size="small" :scroll="{ x: 1000 }">
    <template #columns>
      <a-table-column title="源触发器 / 序列" :width="250">
        <template #cell="{ record }">
          <div>{{ record.trigger }}</div>
          <div>{{ record.sequence_owner }}.{{ record.sequence }}</div>
          <a-tag v-if="record.shared_sequence" color="orange">共享序列 → 各表独立自增</a-tag>
          <div v-if="record.shared_sequence">新 ID 不再保证跨表唯一</div>
        </template>
      </a-table-column>
      <a-table-column title="源列 → 目标列" :width="250">
        <template #cell="{ record }">{{ record.table }}.{{ record.column }} → {{ record.target_table }}.{{ record.target_column }}</template>
      </a-table-column>
      <a-table-column title="转换结果" :width="200">
        <template #cell="{ record }">{{ autoIncrementStatus(record.conversion_status) }}</template>
      </a-table-column>
      <a-table-column title="自增起点校准" :width="180">
        <template #cell="{ record }">
          <div>{{ autoIncrementStatus(record.calibration_status) }}</div>
          <div>起点：{{ record.start ?? '未记录' }}</div>
        </template>
      </a-table-column>
      <a-table-column title="原因" data-index="error" />
    </template>
  </a-table>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import type { OracleMySQLAutoResult } from '@/api/migration'
import { autoIncrementStatus } from '@/utils/migrationReport'
const props = defineProps<{ items: OracleMySQLAutoResult[] }>()
const rows = computed(() => props.items.map((item, index) => ({ ...item, key: JSON.stringify([item.table, item.trigger, index]) })))
</script>
