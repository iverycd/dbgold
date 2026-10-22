<template>
  <div class="data-validation-panel">
    <a-alert v-if="!report" :type="enabled ? 'warning' : 'info'">
      {{ enabled ? '本任务已启用数据内容校验，但尚未记录完整结果。' : '数据内容校验未启用。本报告中的行数一致不代表数据内容一致。' }}
    </a-alert>
    <template v-else>
      <div class="validation-summary">
        <a-tag :color="dataValidationNeedsAttention(report) ? 'orange' : 'green'">{{ dataValidationLabel(report) }}</a-tag>
        <span>通过 {{ passedTables }} / {{ report.tables.length }} 张表</span>
      </div>
      <a-alert v-if="dataValidationNeedsAttention(report)" type="warning" class="validation-notice">内容校验未全部通过，请核查差异与失败原因后再进行业务切换。</a-alert>
      <a-table :data="sortedTables" row-key="source_table" size="small" :pagination="{ pageSize: 20 }" :scroll="{ x: 1120 }"
        :expandable="{ icon: (_, record) => record.error || record.samples?.length ? undefined : null }">
        <template #columns>
          <a-table-column title="源表 / 目标表" :width="240"><template #cell="{ record }"><div>{{ record.source_table }}</div><div class="target-table">→ {{ record.target_table }}</div></template></a-table-column>
          <a-table-column title="状态" :width="145"><template #cell="{ record }"><a-tag :color="dataValidationStatus(record.status).color">{{ dataValidationStatus(record.status).label }}</a-tag></template></a-table-column>
          <a-table-column title="源读取行数" data-index="source_rows" :width="120" />
          <a-table-column title="目标读取行数" data-index="target_rows" :width="120" />
          <a-table-column title="目标缺失" data-index="missing_rows" :width="100" />
          <a-table-column title="目标多余" data-index="extra_rows" :width="100" />
          <a-table-column title="字段变化行数" data-index="changed_rows" :width="120" />
          <a-table-column title="重复主键" data-index="duplicate_keys" :width="100" />
        </template>
        <template #expand-row="{ record }">
          <div class="validation-detail">
            <a-alert v-if="record.error" type="error">{{ record.error }}</a-alert>
            <template v-if="record.samples?.length">
              <p>差异样例最多显示 20 条；长文本截断展示，二进制显示长度及摘要。无主键表按整行及重复次数比较，差异显示为缺失或多余。</p>
              <div v-for="(sample, index) in record.samples" :key="index" class="difference-sample">
                <strong>{{ differenceLabel(sample.type) }}</strong>
                <div v-if="sample.key">主键：<span v-for="(value, column) in sample.key" :key="column">{{ column }}={{ formatDataValidationValue(value) }} </span></div>
                <div v-for="field in sample.fields || []" :key="field.column" class="field-difference"><strong>{{ field.column }}</strong><div>源：{{ formatDataValidationValue(field.source) }}</div><div>目标：{{ formatDataValidationValue(field.target) }}</div></div>
                <div v-if="sample.key_hash">主键指纹：<code>{{ sample.key_hash }}</code></div>
                <div v-if="sample.row_hash">行指纹：<code>{{ sample.row_hash }}</code></div>
                <div v-if="sample.columns?.length">差异字段：{{ sample.columns.join('、') }}</div>
              </div>
            </template>
          </div>
        </template>
      </a-table>
      <p class="validation-footnote">读取失败、取消或不支持的表没有通过全量校验；读取行数可能是中断前的部分结果。</p>
    </template>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import type { DataValidationReport } from '@/api/migration'
import { dataValidationLabel, dataValidationNeedsAttention, dataValidationStatus, formatDataValidationValue } from '@/utils/migrationReport'

const props = defineProps<{ report?: DataValidationReport; enabled?: boolean }>()
const passedTables = computed(() => props.report?.tables.filter(table => table.status === 'passed').length || 0)
const sortedTables = computed(() => [...(props.report?.tables || [])].sort((a, b) => Number(a.status === 'passed') - Number(b.status === 'passed')))
function differenceLabel(type: string) { return ({ missing: '目标缺失', extra: '目标多余', changed: '字段变化', duplicate_key: '重复主键' } as Record<string, string>)[type] || type }
</script>

<style scoped>
.validation-summary { display: flex; align-items: center; flex-wrap: wrap; gap: 12px; margin-bottom: 16px; }
.validation-notice { margin-bottom: 12px; }
.target-table, .validation-footnote { color: var(--color-text-3); font-size: 12px; }
.validation-detail { padding: 12px; overflow-wrap: anywhere; }
.validation-detail p { color: var(--color-text-2); }
.difference-sample { margin-top: 12px; padding: 12px; background: var(--color-bg-2); border: 1px solid var(--color-border-2); border-radius: 6px; }
.difference-sample code { font-size: 12px; }
.field-difference { margin: 8px 0; white-space: pre-wrap; }
</style>
