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
      <a-table :data="sortedTables" row-key="source_table" size="small" :pagination="{ pageSize: 10 }" :scroll="{ x: 1120 }"
        :expandable="{ icon: (_, record) => record.error || record.note || record.samples?.length ? undefined : null }">
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
            <a-alert v-if="record.note" type="info">{{ record.note }}</a-alert>
            <template v-if="record.samples?.length">
              <div class="difference-toolbar">
                <strong>差异样例 <span class="sample-count">{{ record.samples.length }} 条</span></strong>
                <a-tooltip :content-style="{ maxWidth: '360px' }" :trigger="['hover', 'focus']">
                  <span class="difference-help" tabindex="0">查看说明 <icon-question-circle /></span>
                  <template #content>最多记录 20 条样例；长值悬停可查看报告中保存的内容。长文本可能已截断，二进制仅记录长度及摘要。无主键表按整行及重复次数比较，差异显示为缺失或多余。</template>
                </a-tooltip>
              </div>
              <div :key="JSON.stringify([record.source_table, samplePage(record)])" class="difference-samples" tabindex="0" :aria-label="`${record.source_table} 的差异样例`">
                <div v-for="(sample, index) in pagedSamples(record)" :key="index" class="difference-sample">
                  <div class="sample-heading">
                    <span class="sample-number">#{{ (samplePage(record) - 1) * samplePageSize + index + 1 }}</span>
                    <a-tag size="small" :color="sample.type === 'duplicate_key' ? 'red' : 'orange'">{{ differenceLabel(sample.type) }}</a-tag>
                    <a-tooltip v-if="formatSampleKey(sample)" :content="formatSampleKey(sample)" :content-style="{ maxWidth: 'min(560px, 80vw)', overflowWrap: 'anywhere' }" :trigger="['hover', 'focus']">
                      <span class="sample-key" tabindex="0">主键：{{ formatSampleKey(sample) }}</span>
                    </a-tooltip>
                    <span v-else class="sample-key sample-key-empty">未记录主键值</span>
                    <span v-if="sampleFieldCount(sample)" class="sample-field-count">{{ sampleFieldCount(sample) }} 个差异字段</span>
                  </div>
                  <div v-if="sample.fields?.length" class="field-comparison" role="table" aria-label="字段差异对比">
                    <div class="comparison-header" role="row">
                      <span role="columnheader">字段名</span><span role="columnheader" class="source-value">源值</span><span role="columnheader" class="target-value">目标值</span>
                    </div>
                    <div v-for="field in sample.fields" :key="field.column" class="field-difference" role="row">
                      <strong class="field-name" role="rowheader" :title="field.column">{{ field.column }}</strong>
                      <div v-for="side in valueSides" :key="side" :class="['comparison-value', side === 'source' ? 'source-value' : 'target-value']" role="cell">
                        <a-tooltip :content="formatDataValidationValue(field[side])" :content-style="{ maxWidth: 'min(560px, 80vw)', whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }" :trigger="['hover', 'focus']">
                          <span class="value-preview" tabindex="0" :aria-label="`${side === 'source' ? '源' : '目标'}：${formatDataValidationValue(field[side])}`">{{ formatDataValidationValue(field[side]) }}</span>
                        </a-tooltip>
                      </div>
                    </div>
                  </div>
                  <div v-else-if="sample.columns?.length" class="sample-columns">涉及字段：{{ sample.columns.join('、') }}</div>
                  <details v-if="sample.key_hash || sample.row_hash" class="sample-technical">
                    <summary>技术信息</summary>
                    <div v-if="sample.key_hash" class="fingerprint"><span>主键指纹</span><code>{{ sample.key_hash }}</code></div>
                    <div v-if="sample.row_hash" class="fingerprint"><span>行指纹</span><code>{{ sample.row_hash }}</code></div>
                  </details>
                </div>
              </div>
              <div v-if="record.samples.length > samplePageSize" class="sample-pagination">
                <a-pagination :current="samplePage(record)" :page-size="samplePageSize" :total="record.samples.length" :show-page-size="false" show-total size="small" @change="setSamplePage(record, $event)" />
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
import { computed, ref, watch } from 'vue'
import type { DataValidationDifference, DataValidationReport, DataValidationTable } from '@/api/migration'
import { dataValidationLabel, dataValidationNeedsAttention, dataValidationStatus, formatDataValidationValue } from '@/utils/migrationReport'

const props = defineProps<{ report?: DataValidationReport; enabled?: boolean }>()
const passedTables = computed(() => props.report?.tables.filter(table => table.status === 'passed').length || 0)
const sortedTables = computed(() => [...(props.report?.tables || [])].sort((a, b) => Number(a.status === 'passed') - Number(b.status === 'passed')))
const samplePageSize = 10
const valueSides = ['source', 'target'] as const
const samplePages = ref(new Map<string, number>())
watch(() => props.report, () => { samplePages.value = new Map() })

function samplePage(table: DataValidationTable): number {
  const lastPage = Math.max(1, Math.ceil((table.samples?.length || 0) / samplePageSize))
  return Math.min(samplePages.value.get(table.source_table) || 1, lastPage)
}
function pagedSamples(table: DataValidationTable): DataValidationDifference[] {
  const start = (samplePage(table) - 1) * samplePageSize
  return (table.samples || []).slice(start, start + samplePageSize)
}
function setSamplePage(table: DataValidationTable, page: number) {
  samplePages.value.set(table.source_table, page)
}
function formatSampleKey(sample: DataValidationDifference): string {
  return Object.entries(sample.key || {}).map(([column, value]) => `${column}=${formatDataValidationValue(value)}`).join('，')
}
function sampleFieldCount(sample: DataValidationDifference): number {
  return new Set([...(sample.columns || []), ...(sample.fields || []).map(field => field.column)]).size
}
function differenceLabel(type: string) { return ({ missing: '目标缺失', extra: '目标多余', changed: '字段变化', duplicate_key: '重复主键' } as Record<string, string>)[type] || type }
</script>

<style scoped>
.data-validation-panel { min-width: 0; container-type: inline-size; }
.validation-summary { display: flex; align-items: center; flex-wrap: wrap; gap: 12px; margin-bottom: 16px; }
.validation-notice { margin-bottom: 12px; }
.target-table, .validation-footnote { color: var(--color-text-3); font-size: 12px; }
.validation-detail { box-sizing: border-box; width: 100%; max-width: calc(100cqi - 32px); min-width: 0; padding: 4px 0; overflow-wrap: anywhere; }
.validation-detail > .arco-alert { margin-bottom: 8px; }
.difference-toolbar { display: flex; align-items: center; justify-content: space-between; flex-wrap: wrap; gap: 8px; margin-bottom: 8px; font-size: 12px; }
.sample-count { margin-left: 6px; color: var(--color-text-3); font-weight: 400; }
.difference-help { display: inline-flex; align-items: center; gap: 4px; color: var(--color-text-3); cursor: help; }
.difference-samples { display: flex; flex-direction: column; gap: 8px; max-height: min(400px, 50vh); overflow: auto; scrollbar-gutter: stable; }
.difference-sample { flex-shrink: 0; min-width: 0; background: var(--color-bg-1); border: 1px solid var(--color-border-2); border-radius: 6px; overflow: hidden; }
.sample-heading { display: flex; align-items: center; gap: 8px; padding: 8px 10px; font-size: 12px; }
.sample-number { flex-shrink: 0; color: var(--color-text-4); font-variant-numeric: tabular-nums; }
.sample-heading > .arco-tag { flex-shrink: 0; }
.sample-key { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; color: var(--color-text-2); }
.sample-key-empty { color: var(--color-text-4); }
.sample-field-count { flex-shrink: 0; color: var(--color-text-3); }
.comparison-header, .field-difference { display: grid; grid-template-columns: minmax(100px, 1fr) minmax(0, 2fr) minmax(0, 2fr); font-size: 12px; line-height: 20px; }
.comparison-header { color: var(--color-text-3); border-top: 1px solid var(--color-border-2); }
.comparison-header > span { padding: 3px 10px; }
.field-difference { border-top: 1px solid var(--color-border-1); }
.field-name { min-width: 0; padding: 5px 10px; overflow-wrap: anywhere; font-weight: 500; }
.comparison-value { min-width: 0; padding: 5px 10px; }
.source-value { background: var(--color-fill-1); }
.target-value { background: var(--color-primary-light-1); }
.value-preview { display: -webkit-box; -webkit-line-clamp: 2; -webkit-box-orient: vertical; overflow: hidden; white-space: pre-wrap; overflow-wrap: anywhere; font-family: var(--font-mono, monospace); }
.sample-columns { padding: 0 10px 6px; font-size: 12px; color: var(--color-text-3); }
.sample-technical { padding: 4px 10px; border-top: 1px solid var(--color-border-1); color: var(--color-text-3); font-size: 12px; line-height: 20px; }
.sample-technical summary { width: fit-content; cursor: pointer; user-select: none; }
.fingerprint { display: flex; align-items: baseline; gap: 10px; padding-top: 4px; }
.fingerprint > span { flex: 0 0 52px; }
.fingerprint code { min-width: 0; font-family: var(--font-mono, monospace); overflow-wrap: anywhere; }
.sample-pagination { display: flex; justify-content: flex-end; margin-top: 10px; }
.difference-help:focus-visible, .sample-key:focus-visible, .value-preview:focus-visible, .difference-samples:focus-visible { outline: 2px solid var(--color-primary-6); outline-offset: -2px; }
@container (max-width: 768px) {
  .sample-heading { flex-wrap: wrap; }
  .sample-key { order: 1; flex-basis: 100%; }
  .sample-field-count { margin-left: auto; }
  .comparison-header, .field-difference { grid-template-columns: minmax(80px, 1fr) minmax(0, 2fr) minmax(0, 2fr); }
}
</style>
