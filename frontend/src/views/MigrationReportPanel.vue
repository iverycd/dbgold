<template>
  <div class="migration-report-panel">
    <div v-if="loading" style="text-align: center; padding: 24px">
      <a-spin />
    </div>

    <a-alert v-else-if="fetchError" type="error">{{ fetchError }} <a-button type="text" size="small" @click="loadReport">重试</a-button></a-alert>
    <a-empty v-else-if="noReport" description="暂无报告数据，请查看执行日志" />

    <template v-else-if="report">
      <div class="report-heading">
        <div><h3>迁移报告</h3><span>任务 {{ jobID }}</span></div>
        <a-button size="small" @click="exportReport">
          <template #icon><icon-download /></template>
          导出报告
        </a-button>
      </div>
      <div class="report-stats">
        <div><span>迁移表数</span><strong>{{ tableRows[0]?.totalLabel }}</strong></div>
        <div><span>原始失败项</span><strong :class="{ 'report-danger': failedCount > 0 }">{{ failedCount }}</strong></div>
        <div><span>行数不一致</span><strong :class="{ 'report-danger': mismatchedRows.length > 0 }">{{ hasRowCounts ? mismatchedRows.length : '—' }}</strong></div>
        <div><span>迁移告警</span><strong>{{ report.warnings?.length || 0 }}</strong></div>
      </div>
      <div class="report-repair-summary" aria-live="polite">
        <span>原始失败 {{ failedCount }} 项，</span>
        <span v-if="repairSummary.state === 'loading'">修复状态加载中</span>
        <span v-else-if="repairSummary.state === 'unknown'">修复状态未知，请在对象修复中刷新重试</span>
        <template v-else>
          <span>已修复 {{ repairSummary.repaired }} 项，</span>
          <span>{{ repairSummary.attention ? `当前待处理 ${repairSummary.attention} 个对象` : '当前无待处理对象' }}</span>
        </template>
      </div>
      <AutoRepairPanel v-if="report.auto_repairs?.length" :repairs="report.auto_repairs" />
      <a-tabs v-model:active-key="activeTab" :lazy-load="false" :destroy-on-hide="false">
        <!-- ===== 迁移概览 ===== -->
        <a-tab-pane key="overview" title="迁移概览">
          <a-table
            :data="tableRows"
            row-key="key"
            :pagination="false"
            :scroll="{ x: 600 }"
            size="small"
            :row-class="record => (record.isCompatibility || record.failed > 0) && record.items?.length > 0 ? '' : 'report-row-no-details'"
            :expandable="{ icon: (_, record) => (record.isCompatibility || record.failed > 0) && record.items?.length > 0 ? undefined : null }"
          >
            <template #columns>
              <a-table-column title="对象类型" data-index="label" :width="120" />
              <a-table-column title="总数" data-index="totalLabel" :width="80" />
              <a-table-column title="成功" :width="80">
                <template #cell="{ record }">
                  <span v-if="record.isTrigger">—</span>
                  <span v-else>{{ record.success }}</span>
                </template>
              </a-table-column>
              <a-table-column title="失败" :width="80">
                <template #cell="{ record }">
                  <span v-if="record.isTrigger">—</span>
                  <span v-else :style="record.failed > 0 ? 'color: #f53f3f' : ''">{{ record.failedLabel }}</span>
                </template>
              </a-table-column>
              <a-table-column title="状态">
                <template #cell="{ record }">
                  <span v-if="record.isTrigger" style="color: #86909c">
                    ⊘ 未迁移（{{ record.total === -1 ? '获取失败' : record.total + ' 个' }}）
                  </span>
                  <template v-else><a-tag :color="record.statusColor">{{ record.statusLabel }}</a-tag><span v-if="record.isCompatibility"> 新建 {{ record.created }}，复用 {{ record.reused }}</span></template>
                </template>
              </a-table-column>
            </template>

            <template #expand-row="{ record }">
              <CompatibilityFunctionsPanel v-if="record.isCompatibility && report.compatibility_functions" :report="report.compatibility_functions" />
              <div v-else class="failure-list">
                <div
                  v-for="item in record.items"
                  :key="item.key"
                  class="failure-item"
                >
                  <div class="failure-heading">
                    <div class="failure-name">{{ item.nameLabel }}{{ item.name }}</div>
                    <a-button type="text" size="mini" @click="goToRepair(record.key, item)">去修复</a-button>
                  </div>
                  <template v-if="record.key === 'indexes'">
                    <div v-if="item.source_name" class="failure-context">源索引名称：{{ item.source_name }}</div>
                    <div v-if="item.target_name" class="failure-context">{{ item.targetNameLabel }}{{ item.target_name }}</div>
                    <div v-if="item.targetTableLabel" class="failure-context">目标表：{{ item.targetTableLabel }}</div>
                  </template>
                  <div v-if="item.table" class="failure-context">所属表：{{ item.table }}</div>
                  <div class="failure-context">失败阶段：{{ item.stageLabel }}</div>
                  <div class="failure-error">失败原因：{{ item.error }}</div>
                  <div class="failure-ddl">
                    <template v-if="item.ddl?.trim()">
                      <div class="ddl-header">
                        <a-button size="mini" type="text" @click="copyDDL(item.ddl)">复制 DDL</a-button>
                        <span>目标 DDL：</span>
                      </div>
                      <pre class="ddl-code">{{ item.ddl }}</pre>
                    </template>
                    <span v-else style="color: #86909c">目标 DDL：{{ item.emptyDDLMessage }}</span>
                  </div>
                  <div v-if="item.source_ddl?.trim()" class="failure-source-ddl">
                    <template v-if="item.sourceIndexDDL">
                      <div class="ddl-header">
                        <a-button size="mini" type="text" @click="copyDDL(item.sourceIndexDDL)">复制源端索引定义</a-button>
                        <span>源端失败索引定义（仅供参考，需适配目标库）：</span>
                      </div>
                      <pre class="ddl-code">{{ item.sourceIndexDDL }}</pre>
                    </template>
                    <details v-if="item.sourceIndexDDL" class="source-table-details">
                      <summary>查看完整源端表定义</summary>
                      <div class="ddl-header">
                        <a-button size="mini" type="text" @click="copyDDL(item.source_ddl)">复制源端表定义</a-button>
                        <span>{{ item.sourceDDLLabel }}</span>
                      </div>
                      <pre class="ddl-code">{{ item.source_ddl }}</pre>
                    </details>
                    <template v-else>
                      <div class="ddl-header">
                        <a-button size="mini" type="text" @click="copyDDL(item.source_ddl)">复制源端表定义</a-button>
                        <span>{{ item.sourceDDLLabel }}</span>
                      </div>
                      <pre class="ddl-code">{{ item.source_ddl }}</pre>
                    </template>
                  </div>
                  <div v-if="item.suggestion || item.suggested_ddl || item.column_renames?.length" class="failure-suggestion">
                    <div class="ddl-header"><span>{{ item.suggested_ddl || item.column_renames?.length ? '建议 DDL（手动创建）' : '处理建议' }}</span></div>
                    <a-alert v-if="item.suggestion" type="warning">{{ item.suggestion }}</a-alert>
                    <ul v-if="item.column_renames?.length" class="column-renames">
                      <li v-for="(rename, index) in item.column_renames" :key="index">
                        <code>{{ rename.source }}</code> → <code>{{ rename.target }}</code>
                      </li>
                    </ul>
                    <template v-if="item.suggested_ddl">
                      <div class="ddl-header">
                        <a-button size="mini" type="text" @click="copyDDL(item.suggested_ddl)">复制建议 DDL</a-button>
                      </div>
                      <pre class="ddl-code">{{ item.suggested_ddl }}</pre>
                    </template>
                  </div>
                </div>
              </div>
            </template>
          </a-table>
        </a-tab-pane>

        <!-- ===== 行数对比 ===== -->
        <a-tab-pane v-if="hasRowCounts" key="rowcount">
          <template #title>
            行数对比
            <a-badge
              v-if="mismatchedRows.length > 0"
              :count="mismatchedRows.length"
              style="margin-left: 6px"
            />
          </template>

          <div class="row-count-summary">
            <span>共 {{ allRowCounts.length }} 张表</span>
            <span :class="{ 'row-count-mismatch': mismatchedRows.length > 0 }">{{ mismatchedRows.length }} 张表行数不一致</span>
            <span>不一致项优先展示</span>
          </div>
          <a-table
            :data="sortedRowCounts"
            :pagination="{ pageSize: 20 }"
            :scroll="{ x: 760, maxHeight: '420px' }"
            size="small"
            row-key="table"
          >
            <template #columns>
              <a-table-column title="表名" data-index="table" />
              <a-table-column title="源行数" data-index="src" :width="120" />
              <a-table-column title="目标行数" data-index="dst" :width="120" />
              <a-table-column title="差异" :width="100">
                <template #cell="{ record }">
                  <span :class="{ 'row-count-mismatch': !record.match }">{{ record.dst - record.src }}</span>
                </template>
              </a-table-column>
              <a-table-column title="状态" :width="100">
                <template #cell="{ record }">
                  <a-tag v-if="record.match" color="green">✓ 一致</a-tag>
                  <a-tag v-else color="orange">⚠ 不一致</a-tag>
                </template>
              </a-table-column>
            </template>
          </a-table>

        </a-tab-pane>
        <a-tab-pane key="repair">
          <template #title>
            对象修复
            <a-badge v-if="repairSummary.state === 'ready' && repairSummary.attention > 0" :count="repairSummary.attention" style="margin-left: 6px" />
          </template>
          <MigrationRepairPanel ref="repairPanel" :jobID="jobID" @summary-change="updateRepairSummary" />
        </a-tab-pane>
        <a-tab-pane v-if="report.warnings?.length" key="warnings" title="迁移告警">
          <a-alert type="warning" style="margin-bottom: 12px">以下转换已记录；数据转换数量仅统计已提交的分页。</a-alert>
          <a-table :data="report.warnings" :pagination="{ pageSize: 20 }" size="small">
            <template #columns>
              <a-table-column title="表" data-index="table" />
              <a-table-column title="列" data-index="column" />
              <a-table-column title="原因" data-index="reason" />
              <a-table-column title="数量" data-index="count" :width="100" />
            </template>
          </a-table>
        </a-tab-pane>
      </a-tabs>
      <div v-if="report.preflight" ref="preflightSection" style="margin-top: 16px" tabindex="-1" aria-label="迁移前预检记录">
        <a-collapse v-model:active-key="preflightActiveKeys">
          <a-collapse-item key="preflight" :header="`迁移前预检记录（${report.preflight.ready ? '已通过' : '未通过'}）`">
            <PGMySQLPreflightPanel :result="report.preflight" :excluded="report.preflight.excluded" readonly />
          </a-collapse-item>
        </a-collapse>
      </div>
    </template>
  </div>
</template>

<script setup lang="ts">
import MigrationRepairPanel from '@/components/MigrationRepairPanel.vue'
import CompatibilityFunctionsPanel from '@/components/CompatibilityFunctionsPanel.vue'
import AutoRepairPanel from '@/components/AutoRepairPanel.vue'
import { formatAutoRepairs } from '@/utils/autoRepairs'
import { reportCategories, formatReportCategories } from '@/utils/migrationReport'
import type { RepairFocusTarget, RepairSummary } from '@/utils/migrationRepair'
import PGMySQLPreflightPanel from './PGMySQLPreflightPanel.vue'
import { ref, computed, nextTick, onMounted, onUnmounted, watch } from 'vue'
import { Message } from '@arco-design/web-vue'
import {
  getDataMigrationReport,
  type MigrationReport,
  type TableRowCount,
} from '@/api/migration'
import { copyText } from '@/utils/clipboard'

const props = defineProps<{ jobID: string }>()
const emit = defineEmits<{ loaded: [jobID: string, report: MigrationReport] }>()

const repairPanel = ref<InstanceType<typeof MigrationRepairPanel> | null>(null)
const activeTab = ref('overview')
const repairSummary = ref<RepairSummary>({ jobID: props.jobID, state: 'loading', repaired: 0, attention: 0 })
const report = ref<MigrationReport | null>(null)
const loading = ref(false)
const fetchError = ref('')
const noReport = ref(false)
const preflightSection = ref<HTMLElement | null>(null)
const preflightActiveKeys = ref<string[]>([])

function updateRepairSummary(summary: RepairSummary) {
  if (summary.jobID === props.jobID) repairSummary.value = summary
}
async function goToRepair(kind: string, item: Omit<RepairFocusTarget, 'kind'>) {
  const jobID = props.jobID
  activeTab.value = 'repair'
  await nextTick()
  if (jobID !== props.jobID) return
  await repairPanel.value?.focusObject({ ...item, kind })
}

async function showPreflight() {
  if (!report.value?.preflight) return
  preflightActiveKeys.value = ['preflight']
  await nextTick()
  preflightSection.value?.focus({ preventScroll: true })
  preflightSection.value?.scrollIntoView({ behavior: 'smooth', block: 'start' })
}
defineExpose({ showPreflight })

const tableRows = computed(() => report.value ? reportCategories(report.value) : [])

const hasRowCounts = computed(() => (report.value?.rowCounts?.length ?? 0) > 0)
const allRowCounts = computed<TableRowCount[]>(() => report.value?.rowCounts ?? [])
const mismatchedRows = computed<TableRowCount[]>(() =>
  report.value?.rowCounts?.filter((r) => !r.match) ?? []
)
const sortedRowCounts = computed<TableRowCount[]>(() => [
  ...mismatchedRows.value,
  ...allRowCounts.value.filter(row => row.match),
])
const failedCount = computed(() => tableRows.value.reduce((total, row) => total + (row.isTrigger ? 0 : row.failed), 0))
let reportRequest = 0

async function loadReport() {
  const request = ++reportRequest
  const jobID = props.jobID
  loading.value = true
  fetchError.value = ''
  noReport.value = false
  report.value = null
  activeTab.value = 'overview'
  repairSummary.value = { jobID, state: 'loading', repaired: 0, attention: 0 }
  preflightActiveKeys.value = []
  try {
    const res = await getDataMigrationReport(jobID)
    if (request !== reportRequest) return
    report.value = res.data
    preflightActiveKeys.value = res.data.preflight && !res.data.preflight.ready ? ['preflight'] : []
    emit('loaded', jobID, res.data)
  } catch (e: any) {
    if (request !== reportRequest) return
    if (e?.response?.status === 404) {
      noReport.value = true
    } else {
      fetchError.value = '暂无报告数据'
    }
  } finally {
    if (request === reportRequest) loading.value = false
  }
}

async function exportReport() {
  if (!report.value) return
  const r = report.value
  const lines: string[] = []

  lines.push('单次迁移报告')
  lines.push(`Job ID: ${props.jobID}`)
  lines.push(`导出时间: ${new Date().toLocaleString('zh-CN')}`)
  lines.push('')

  lines.push(...formatAutoRepairs(r.auto_repairs))
  if (r.preflight) { lines.push('=== 兼容预检与计划 ===', JSON.stringify(r.preflight, null, 2), '') }
  if (r.warnings?.length) {
    lines.push('=== 迁移告警 ===')
    for (const warning of r.warnings) lines.push(`${warning.table}.${warning.column}: ${warning.reason}（${warning.count}）`)
    lines.push('')
  }
  lines.push(...formatReportCategories(r))

  if (r.rowCounts && r.rowCounts.length > 0) {
    lines.push('=== 行数对比 ===')
    const mismatched = r.rowCounts.filter((rc) => !rc.match)
    if (mismatched.length > 0) {
      lines.push(`\n不一致的表（${mismatched.length} 张）：`)
      for (const rc of mismatched) {
        lines.push(`  ${rc.table}：源 ${rc.src} 行 → 目标 ${rc.dst} 行（差 ${rc.dst - rc.src}）`)
      }
    }
    lines.push(`\n全部表（${r.rowCounts.length} 张）：`)
    for (const rc of r.rowCounts) {
      lines.push(`  ${rc.table}：${rc.src} / ${rc.dst}  ${rc.match ? '✓' : '✗'}`)
    }
  }

  if (repairPanel.value) {
    try { lines.push(...await repairPanel.value.exportHistory()) }
    catch (error: any) { Message.error(error?.response?.data?.error || error?.message || '修复历史加载失败，请重试导出'); return }
  }
  const blob = new Blob([lines.join('\n')], { type: 'text/plain;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = `migration-report-${props.jobID.slice(0, 8)}.txt`
  a.click()
  URL.revokeObjectURL(url)
}

async function copyDDL(ddl: string) {
  try {
    await copyText(ddl)
    Message.success('DDL 已复制')
  } catch {
    Message.error('复制失败')
  }
}

onMounted(loadReport)
watch(() => props.jobID, loadReport)
onUnmounted(() => { reportRequest++ })
</script>

<style scoped>
.migration-report-panel {
  min-width: 0;
  container-type: inline-size;
}
.failure-list {
  padding: 8px 16px;
  background: #f7f8fa;
}
.failure-item {
  padding: 8px 0;
  border-bottom: 1px solid #e5e6eb;
}
.failure-item:last-child {
  border-bottom: none;
}
.failure-name {
  font-weight: 600;
  margin-bottom: 4px;
}
.failure-heading { display: flex; align-items: baseline; justify-content: space-between; gap: 12px; }
.report-repair-summary { margin: -8px 0 20px; color: var(--color-text-2); font-size: 13px; }
.failure-error {
  color: #f53f3f;
  margin-bottom: 4px;
  font-size: 13px;
}
.failure-suggestion { margin-top: 12px; }
.failure-source-ddl { margin-top: 12px; }
.source-table-details { margin-top: 12px; }
.source-table-details summary { cursor: pointer; color: var(--color-text-2); margin-bottom: 8px; }
.failure-context { color: var(--color-text-2); font-size: 13px; margin-bottom: 4px; overflow-wrap: anywhere; }
.column-renames { padding-left: 20px; overflow-wrap: anywhere; }
.ddl-header {
  display: flex;
  justify-content: flex-start;
  align-items: center;
  gap: 8px;
  margin-bottom: 4px;
}
.ddl-code {
  background: #1d1d1d;
  color: #d4d4d4;
  padding: 8px 12px;
  border-radius: 4px;
  font-size: 12px;
  font-family: 'Menlo', 'Monaco', 'Courier New', monospace;
  overflow: auto;
  max-height: 320px;
  white-space: pre;
  margin: 0;
}

.report-heading { display: flex; align-items: center; justify-content: space-between; flex-wrap: wrap; gap: 12px; margin-bottom: 20px; }
.report-heading h3 { margin: 0 0 6px; font-size: 16px; font-weight: 600; }
.report-heading span { color: var(--color-text-3); font-size: 12px; overflow-wrap: anywhere; }
.report-stats { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 12px; margin-bottom: 20px; }
.report-stats > div { padding: 16px; border: 1px solid var(--color-border-2); border-radius: 8px; background: var(--color-bg-2); }
.report-stats span { display: block; color: var(--color-text-3); font-size: 12px; margin-bottom: 8px; }
.report-stats strong { font-size: 24px; font-weight: 600; font-variant-numeric: tabular-nums; }
.report-danger { color: #dc2626; }
.row-count-summary { display: flex; flex-wrap: wrap; gap: 8px 16px; margin-bottom: 12px; color: var(--color-text-2); font-size: 13px; }
.row-count-mismatch { color: #ff7d00; }
.failure-error, .failure-name { overflow-wrap: anywhere; }
.migration-report-panel :deep(.arco-table-cell) { overflow-wrap: anywhere; }
/* Arco 2.58 falls back to its default icon for null. Keep the icon contract,
   and remove non-actionable buttons from both pointer and keyboard navigation. */
.migration-report-panel :deep(.report-row-no-details .arco-table-expand-btn) { display: none; }
.migration-report-panel :deep(.arco-tabs-tab-active), .migration-report-panel :deep(.arco-tabs-tab:hover) { color: #15803d; }
.migration-report-panel :deep(.arco-tabs-nav-ink) { background: #16a34a; }
@container (max-width: 767px) { .report-stats { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
</style>
