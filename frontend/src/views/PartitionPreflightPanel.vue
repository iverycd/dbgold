<template>
  <section class="partition-preflight" aria-label="分区迁移预检与核验">
    <div class="partition-heading">
      <div><h3>{{ readonly ? '分区迁移记录' : '分区兼容性检查结果' }}</h3><p>{{ readonly ? '本次任务保存的分区计划、名称映射及核验结果' : '开始迁移时自动检查原生分区结构与目标路由是否能够等价保留' }}</p></div>
      <a-space wrap>
        <a-button v-if="result" @click="download">导出分区记录</a-button>
      </a-space>
    </div>
    <a-alert v-if="!readonly" type="info">开始迁移时会自动检查分区兼容性，检查本身不会修改数据库。迁移和校验期间请停止源库与目标库的业务写入及结构变更。</a-alert>
    <a-alert v-if="result?.fatal_error" type="error">{{ result.fatal_error }}<template v-if="!readonly"> 请处理问题后再次点击“开始迁移”。</template></a-alert>
    <a-alert v-else-if="result && !result.ready" :type="result.can_start ? 'warning' : 'error'">
      {{ readonly ? '本次执行前存在阻止项，请结合下方各表的执行和核验状态查看。' : result.can_start ? '部分表已被阻止，将跳过受影响表及其依赖对象，其余表继续迁移。请展开明细核对受影响的表。' : '当前任务不能开始，请处理阻止原因后再次点击“开始迁移”。' }}
    </a-alert>
    <template v-if="result">
      <div class="partition-summary" aria-live="polite">
        <span>{{ result.source_type }} → {{ result.target_type }}</span>
        <span>逻辑表 {{ result.tables.length }}</span><span>分区表 {{ partitionCount }}</span>
        <span>阻止 {{ blockedCount }}</span><span>核验失败 {{ failedCount }}</span>
      </div>
      <a-table :data="result.tables" row-key="source_table" :pagination="{ pageSize: 10, showTotal: true }" :scroll="{ x: 850 }" size="small"
        :expandable="{ icon: (_, record) => record.source || record.plan || record.error ? undefined : null }">
        <template #columns>
          <a-table-column title="源逻辑表" data-index="source_table" />
          <a-table-column title="目标表" data-index="target_table" />
          <a-table-column title="源分区方式" :width="160"><template #cell="{ record }">{{ record.source?.method || '普通表' }}<span v-if="record.source?.automatic"> · 自动扩展</span></template></a-table-column>
          <a-table-column title="状态" :width="130"><template #cell="{ record }"><a-tag :color="statusColor(record.status)">{{ statusLabel(record.status) }}</a-tag></template></a-table-column>
          <a-table-column title="说明"><template #cell="{ record }">{{ record.error || (record.plan ? '已生成原生分区计划' : '按普通表流程执行') }}</template></a-table-column>
        </template>
        <template #expand-row="{ record }">
          <div class="partition-detail">
            <a-alert v-if="record.error" type="error">{{ record.error }}</a-alert>
            <template v-if="record.plan">
              <p>{{ ruleLabel(record.plan.rule) }}。结构和分区名称映射随任务保存；目标使用默认物理存储设置。</p>
              <p v-if="record.plan.interval_materialization">迁移器还将恢复 {{ record.plan.interval_materialization.seeds.length }} 个现有自动叶分区（包括空叶），并保留后续自动扩展规则。下方显示可直接执行的建表 SQL；完整恢复计划保存在导出的分区记录中。</p>
              <a-table :data="mappingRows(record)" :pagination="{ pageSize: 10 }" size="small" :scroll="{ x: 760 }">
                <template #columns>
                  <a-table-column title="源叶分区" data-index="source" />
                  <a-table-column title="目标叶分区 / 原生序号" data-index="target" />
                  <a-table-column title="源行数" data-index="sourceRows" :width="110" />
                  <a-table-column title="目标原有行数" data-index="baselineRows" :width="130" />
                  <a-table-column title="目标新增行数" data-index="addedRows" :width="130" />
                </template>
              </a-table>
              <details><summary>查看目标建表 SQL</summary><pre tabindex="0">{{ plannedSQL(record) }}</pre></details>
              <details><summary>查看目标分区结构</summary><pre tabindex="0">{{ JSON.stringify(record.plan.target, null, 2) }}</pre></details>
            </template>
            <details v-if="record.source"><summary>查看源分区结构与边界</summary><pre tabindex="0">{{ JSON.stringify(record.source, null, 2) }}</pre></details>
          </div>
        </template>
      </a-table>
    </template>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import type { PartitionPreflight, PartitionTableResult } from '@/api/migration'

const props = defineProps<{ result?: PartitionPreflight | null; readonly?: boolean }>()
const partitionCount = computed(() => props.result?.tables.filter(table => table.source).length ?? 0)
const blockedCount = computed(() => props.result?.tables.filter(table => table.status === 'blocked').length ?? 0)
const failedCount = computed(() => props.result?.tables.filter(table => table.status === 'failed' || table.status === 'incomplete').length ?? 0)
function statusLabel(status: string) {
  return ({ ordinary: '普通表', ready: '可迁移', blocked: '已阻止', succeeded: '核验通过', failed: '核验失败', incomplete: '未完成' } as Record<string, string>)[status] ?? status
}
function statusColor(status: string) {
  return ({ ordinary: 'gray', ready: 'green', blocked: 'red', succeeded: 'green', failed: 'red', incomplete: 'orange' } as Record<string, string>)[status] ?? 'gray'
}
function ruleLabel(rule: string) {
  return ({ 'native-declarative-partitions': '使用目标原生声明式分区', 'native-inline-partitions': '使用目标原生建表分区语法', 'native-range-ordinal-map': '使用目标原生 RANGE 分区，按序号保存源名称映射' } as Record<string, string>)[rule] ?? '使用目标原生分区'
}
function nativeCount(rows: Record<string, number> | undefined, schema: string | undefined, name: string, ordinal?: number) {
  if (!rows) return undefined
  const keys = [schema ? `${schema}.${name}` : name, name, ...(ordinal ? [String(ordinal)] : [])]
  for (const key of keys) if (Object.prototype.hasOwnProperty.call(rows, key)) return rows[key]
  return undefined
}
function mappingRows(table: PartitionTableResult) {
  return (table.plan?.mapping ?? []).map(mapping => {
    const sourceRows = nativeCount(table.source_rows, mapping.source_schema, mapping.source_name)
    const targetRows = nativeCount(table.target_rows, mapping.target_schema, mapping.target_name, mapping.target_ordinal)
    const baselineRows = nativeCount(table.baseline, mapping.target_schema, mapping.target_name, mapping.target_ordinal) ?? 0
    return {
      source: mapping.source_schema ? `${mapping.source_schema}.${mapping.source_name}` : mapping.source_name,
      target: mapping.target_ordinal ? `原生分区 ${mapping.target_ordinal}` : mapping.target_schema ? `${mapping.target_schema}.${mapping.target_name}` : mapping.target_name,
      sourceRows: sourceRows ?? '—', baselineRows: table.target_rows || table.baseline ? baselineRows : '—',
      addedRows: targetRows === undefined ? '—' : targetRows - baselineRows,
    }
  })
}
function plannedSQL(table: PartitionTableResult) {
  return table.plan?.statements
    .filter(statement => !table.plan?.interval_materialization || !statement.SQL.includes('/* DBGOLD_INTERVAL_MATERIALIZE '))
    .map(statement => statement.SQL).join(';\n') ?? ''
}
function download() {
  const url = URL.createObjectURL(new Blob([JSON.stringify(props.result, null, 2)], { type: 'application/json;charset=utf-8' }))
  const link = document.createElement('a')
  link.href = url; link.download = 'partition-migration.json'; link.click(); URL.revokeObjectURL(url)
}
</script>

<style scoped>
.partition-preflight { margin: 20px 0; padding: 20px; border: 1px solid var(--color-border-2); border-radius: 8px; background: var(--color-bg-1); min-width: 0; }
.partition-heading { display: flex; justify-content: space-between; align-items: center; flex-wrap: wrap; gap: 12px; margin-bottom: 14px; }
.partition-heading h3 { margin: 0 0 6px; font-size: 16px; }.partition-heading p { margin: 0; color: var(--color-text-3); font-size: 13px; }
.partition-preflight :deep(.arco-alert) { margin-bottom: 12px; }.partition-summary { display: flex; flex-wrap: wrap; gap: 8px 20px; padding: 12px 0; color: var(--color-text-2); font-size: 13px; }
.partition-detail { padding: 12px; min-width: 0; }.partition-detail p { color: var(--color-text-2); font-size: 13px; }.partition-detail details { margin-top: 14px; }summary { cursor: pointer; color: var(--color-text-2); }
pre { max-height: 360px; padding: 12px; overflow: auto; white-space: pre-wrap; overflow-wrap: anywhere; background: var(--color-fill-1); font: 12px/1.7 monospace; }
.partition-preflight :deep(.arco-table-cell) { overflow-wrap: anywhere; }
</style>
