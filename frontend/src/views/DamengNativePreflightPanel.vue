<template>
  <section class="native-preview" aria-label="达梦 Schema 全对象迁移预检">
    <div class="native-heading"><h3>达梦 Schema 全对象迁移</h3><a-button v-if="!readonly" :loading="busy" :disabled="disabled" @click="$emit('check')">{{ result ? '重新预检' : '预检并查看对象计划' }}</a-button></div>
    <p>迁移所选 Schema 的全部对象和数据，保留对象名称与达梦原生定义。迁移期间请保持源库停写，并提前创建目标 Schema。</p>
    <template v-if="result">
      <a-alert :type="result.ready ? 'success' : 'error'">{{ result.ready ? '预检通过；启动任务时将重新核对源库和目标库。' : '预检未通过，请先处理下方问题。' }}</a-alert>
      <p>源：{{ result.source.schema }} · {{ result.source.version }} → 目标：{{ result.target.schema }} · {{ result.target.version }}</p>
      <p>发现 {{ result.objects?.length || 0 }} 个对象，同名冲突 {{ result.conflicts?.length || 0 }} 个，计划删除重建 {{ result.drops?.length || 0 }} 个。</p>
      <a-alert v-if="result.drops?.length" type="warning">覆盖已启用，以下对象将删除后重建，已有对象及数据会被替换。请核对删除清单。</a-alert>
      <a-table v-if="result.drops?.length" :data="result.drops" row-key="id" :pagination="{ pageSize: 10 }" size="small" style="margin-top:12px">
        <template #columns><a-table-column title="类型" data-index="kind" /><a-table-column title="Schema" data-index="schema" /><a-table-column title="将删除的对象" data-index="name" /><a-table-column title="删除语句"><template #cell="{ record }"><pre>{{ record.drop_sql }}</pre></template></a-table-column></template>
      </a-table>
    </template>
    <a-table v-if="issues.length" :data="issues" :pagination="{ pageSize: 10 }" size="small" style="margin-top:12px">
      <template #columns><a-table-column title="阶段" :width="90"><template #cell="{record}"><a-tag :color="record.severity === 'warning' ? 'orange' : 'red'">{{ record.severity === 'warning' ? '告警' : '阻止项' }}</a-tag></template></a-table-column><a-table-column title="对象" data-index="name" /><a-table-column title="原因" data-index="reason" /></template>
    </a-table>
    <a-table v-if="objects.length" :data="objects" row-key="id" :pagination="{ pageSize: 20, showTotal: true }" size="small" :expandable="{ icon: (_, record) => record.units?.length ? undefined : null }" style="margin-top:12px">
      <template #columns><a-table-column title="对象类型" data-index="kind" /><a-table-column title="Schema" data-index="schema" /><a-table-column title="对象名" data-index="name" /><a-table-column title="所属表" data-index="table" /><a-table-column title="状态"><template #cell="{record}">{{ statusLabel(record.status, record.managed) }}</template></a-table-column></template>
      <template #expand-row="{record}"><pre v-for="(unit,index) in record.units" :key="index">{{ unit.sql }}</pre></template>
    </a-table>
  </section>
</template>
<script setup lang="ts">
import { computed } from 'vue'
import type { DamengNativePreflight, DamengNativeObject, DamengNativeIssue } from '@/api/migration'
const props = withDefaults(defineProps<{ result?: DamengNativePreflight | null; inventory?: DamengNativeObject[]; verification?: DamengNativeIssue[]; busy?: boolean; disabled?: boolean; readonly?: boolean }>(), { inventory: () => [], verification: () => [] })
defineEmits<{ check: [] }>()
const objects = computed(() => props.inventory?.length ? props.inventory : props.result?.objects ?? [])
const issues = computed(() => [...(props.result?.issues ?? []), ...(props.verification ?? [])])
function statusLabel(status?: string, managed?: boolean) {
  return ({ succeeded: '已完成', failed: '失败', pending: '待执行', blocked: '依赖阻塞', unknown: '结果未知', cancelled: '已取消', VALID: '源端有效', INVALID: '源端无效', Y: '源端有效', N: '源端无效' } as Record<string, string>)[status ?? ''] || status || (managed ? '随所属对象创建' : '待执行')
}
</script>
<style scoped>
.native-preview { padding: 20px; border: 1px solid var(--color-border-2); border-radius: 12px; margin: 16px 0; }
.native-heading { display: flex; justify-content: space-between; align-items: center; gap: 12px; }
h3 { margin: 0; }
p { color: var(--color-text-2); line-height: 1.7; }
pre { white-space: pre-wrap; word-break: break-word; max-height: 360px; overflow: auto; }
</style>
