<template>
  <a-alert type="warning" class="warning-note">报告记录迁移告警及需要人工处理的对象。数据转换数量仅统计已提交的分页。</a-alert>
  <a-table :data="warnings" :pagination="{ pageSize: 10 }" size="small">
    <template #columns>
      <a-table-column title="表或 Schema" data-index="table" />
      <a-table-column title="列或对象" data-index="column" />
      <a-table-column title="原因">
        <template #cell="{ record }">
          <span>{{ record.reason }}</span>
          <details v-if="record.source_ddl" class="warning-source">
            <summary>查看源端 DDL</summary>
            <a-button size="mini" type="text" @click="copyDDL(record.source_ddl)">复制源端 DDL</a-button>
            <pre>{{ record.source_ddl }}</pre>
          </details>
        </template>
      </a-table-column>
      <a-table-column title="数量" data-index="count" :width="100" />
    </template>
  </a-table>
</template>

<script setup lang="ts">
import { Message } from '@arco-design/web-vue'
import type { MigrationReport } from '@/api/migration'
import { copyText } from '@/utils/clipboard'

defineProps<{ warnings: NonNullable<MigrationReport['warnings']> }>()

async function copyDDL(ddl: string) {
  try {
    await copyText(ddl)
    Message.success('DDL 已复制')
  } catch {
    Message.error('复制失败')
  }
}
</script>

<style scoped>
.warning-note { margin-bottom: 12px; }
.warning-source { margin-top: 8px; }
.warning-source summary { cursor: pointer; color: var(--color-text-2); }
.warning-source pre {
  color: var(--color-text-1);
  background: var(--color-fill-1);
  border-radius: 4px;
  padding: 8px 12px;
  margin: 0;
  max-height: 320px;
  overflow: auto;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}
</style>
