<template>
  <section v-if="repairs.length" class="auto-repair-panel" aria-label="自动修复明细">
    <a-alert type="info">
      自动修复涉及 {{ groups.length }} 张表、{{ repairs.length }} 列。下方列出本次已修复或经校验复用的列名映射，请据此核对业务 SQL 中的字段引用。
    </a-alert>
    <a-table :data="groups" row-key="table" :pagination="{ pageSize: 20 }" size="small" :scroll="{ x: 760 }">
      <template #columns>
        <a-table-column title="源表" data-index="table" :width="180" />
        <a-table-column title="目标表" data-index="target_table" :width="220" />
        <a-table-column title="列名修复明细">
          <template #cell="{ record }">
            <div v-for="item in record.items" :key="item.source" class="repair-item">
              <div><code>{{ item.source }}</code> → <code>{{ item.target }}</code></div>
              <div class="repair-reason">{{ item.reason }} · {{ autoRepairStatus(item.status) }}</div>
            </div>
          </template>
        </a-table-column>
      </template>
    </a-table>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import type { AutoRepair } from '@/api/migration'
import { autoRepairStatus, groupAutoRepairs } from '@/utils/autoRepairs'

const props = defineProps<{ repairs: AutoRepair[] }>()
const groups = computed(() => groupAutoRepairs(props.repairs))
</script>

<style scoped>
.auto-repair-panel { display: grid; gap: 12px; margin: 16px 0; }
.repair-item { padding: 6px 0; overflow-wrap: anywhere; }
.repair-item + .repair-item { border-top: 1px solid var(--color-border-2); }
.repair-reason { margin-top: 4px; color: var(--color-text-3); font-size: 12px; }
code { color: var(--color-text-1); }
</style>
