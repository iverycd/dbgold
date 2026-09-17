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
        <div><span>迁移表数</span><strong>{{ report.tables.total }}</strong></div>
        <div><span>失败项</span><strong :class="{ 'report-danger': failedCount > 0 }">{{ failedCount }}</strong></div>
        <div><span>行数不一致</span><strong :class="{ 'report-danger': mismatchedRows.length > 0 }">{{ hasRowCounts ? mismatchedRows.length : '—' }}</strong></div>
        <div><span>迁移告警</span><strong>{{ report.warnings?.length || 0 }}</strong></div>
      </div>
      <a-tabs default-active-key="overview" :destroy-on-hide="false">
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
        <!-- ===== 迁移概览 ===== -->
        <a-tab-pane key="overview" title="迁移概览">
          <a-table
            :data="tableRows"
            row-key="key"
            :pagination="false"
            :scroll="{ x: 600 }"
            size="small"
            :row-class="record => record.failed > 0 && record.items?.length > 0 ? '' : 'report-row-no-details'"
            :expandable="{ icon: (_, record) => record.failed > 0 && record.items?.length > 0 ? undefined : null }"
          >
            <template #columns>
              <a-table-column title="对象类型" data-index="label" :width="120" />
              <a-table-column title="总数" data-index="total" :width="80" />
              <a-table-column title="成功" :width="80">
                <template #cell="{ record }">
                  <span v-if="record.isTrigger">—</span>
                  <span v-else>{{ record.success }}</span>
                </template>
              </a-table-column>
              <a-table-column title="失败" :width="80">
                <template #cell="{ record }">
                  <span v-if="record.isTrigger">—</span>
                  <span v-else :style="record.failed > 0 ? 'color: #f53f3f' : ''">{{ record.failed }}</span>
                </template>
              </a-table-column>
              <a-table-column title="状态">
                <template #cell="{ record }">
                  <span v-if="record.isTrigger" style="color: #86909c">
                    ⊘ 未迁移（{{ record.total === -1 ? '获取失败' : record.total + ' 个' }}）
                  </span>
                  <a-tag v-else-if="record.failed > 0" color="orange">⚠ 部分失败</a-tag>
                  <a-tag v-else-if="record.total === 0" color="gray">无对象</a-tag>
                  <a-tag v-else color="green">✓ 全部成功</a-tag>
                </template>
              </a-table-column>
            </template>

            <template #expand-row="{ record }">
              <div class="failure-list">
                <div
                  v-for="item in record.items"
                  :key="item.name"
                  class="failure-item"
                >
                  <div class="failure-name">{{ item.name }}</div>
                  <div class="failure-error">失败原因：{{ item.error }}</div>
                  <div class="failure-ddl">
                    <template v-if="item.ddl">
                      <div class="ddl-header">
                        <a-button size="mini" type="text" @click="copyDDL(item.ddl)">复制 DDL</a-button>
                        <span>DDL：</span>
                      </div>
                      <pre class="ddl-code">{{ item.ddl }}</pre>
                    </template>
                    <span v-else style="color: #86909c">DDL：—</span>
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

          <a-table
            :data="allRowCounts"
            :pagination="{ pageSize: 50 }"
            :scroll="{ x: 600 }"
            size="small"
            row-key="table"
          >
            <template #columns>
              <a-table-column title="表名" data-index="table" />
              <a-table-column title="源行数" data-index="src" :width="120" />
              <a-table-column title="目标行数" data-index="dst" :width="120" />
              <a-table-column title="状态" :width="100">
                <template #cell="{ record }">
                  <a-tag v-if="record.match" color="green">✓ 一致</a-tag>
                  <a-tag v-else color="orange">⚠ 不一致</a-tag>
                </template>
              </a-table-column>
            </template>
          </a-table>

          <template v-if="mismatchedRows.length > 0">
            <div style="color: #ff7d00; margin: 16px 0 8px">
              ⚠ {{ mismatchedRows.length }} 张表行数不一致
            </div>
            <a-table :data="mismatchedRows" :pagination="false" size="small" row-key="table">
              <template #columns>
                <a-table-column title="表名" data-index="table" />
                <a-table-column title="源行数" data-index="src" :width="120" />
                <a-table-column title="目标行数" data-index="dst" :width="120" />
                <a-table-column title="差异" :width="100">
                  <template #cell="{ record }">
                    <span style="color: #ff7d00">{{ record.dst - record.src }}</span>
                  </template>
                </a-table-column>
              </template>
            </a-table>
          </template>
        </a-tab-pane>
      </a-tabs>
      <a-collapse
        v-if="report.preflight"
        :default-active-key="report.preflight.ready ? [] : ['preflight']"
        style="margin-top: 16px"
      >
        <a-collapse-item key="preflight" :header="`迁移前预检记录（${report.preflight.ready ? '已通过' : '未通过'}）`">
          <PGMySQLPreflightPanel :result="report.preflight" :excluded="report.preflight.excluded" readonly />
        </a-collapse-item>
      </a-collapse>
    </template>
  </div>
</template>

<script setup lang="ts">
import PGMySQLPreflightPanel from './PGMySQLPreflightPanel.vue'
import { ref, computed, onMounted, onUnmounted, watch } from 'vue'
import { Message } from '@arco-design/web-vue'
import {
  getDataMigrationReport,
  type MigrationReport,
  type ObjectResult,
  type TableRowCount,
} from '@/api/migration'
import { copyText } from '@/utils/clipboard'

const props = defineProps<{ jobID: string }>()
const emit = defineEmits<{ loaded: [jobID: string] }>()

const report = ref<MigrationReport | null>(null)
const loading = ref(false)
const fetchError = ref('')
const noReport = ref(false)

interface ReportRow {
  key: string
  label: string
  total: number
  success: number
  failed: number
  items: ObjectResult[]
  isTrigger: boolean
}

const tableRows = computed<ReportRow[]>(() => {
  if (!report.value) return []
  const r = report.value
  return [
    { key: 'tables', label: '表', ...r.tables, isTrigger: false },
    { key: 'data', label: '数据写入', ...r.data, isTrigger: false },
    { key: 'primaryKeys', label: '主键', ...r.primaryKeys, isTrigger: false },
    { key: 'views', label: '视图', ...r.views, isTrigger: false },
    { key: 'indexes', label: '索引', ...r.indexes, isTrigger: false },
    { key: 'constraints', label: '外键', ...r.constraints, isTrigger: false },
    { key: 'sequences', label: '序列', ...r.sequences, isTrigger: false },
    { key: 'comments', label: '注释', ...r.comments, isTrigger: false },
    {
      key: 'triggers',
      label: '触发器',
      total: r.triggers.total,
      success: 0,
      failed: 0,
      items: [],
      isTrigger: true,
    },
  ]
})

const hasRowCounts = computed(() => (report.value?.rowCounts?.length ?? 0) > 0)
const allRowCounts = computed<TableRowCount[]>(() => report.value?.rowCounts ?? [])
const mismatchedRows = computed<TableRowCount[]>(() =>
  report.value?.rowCounts?.filter((r) => !r.match) ?? []
)
const failedCount = computed(() => tableRows.value.reduce((total, row) => total + (row.isTrigger ? 0 : row.failed), 0))
let reportRequest = 0

async function loadReport() {
  const request = ++reportRequest
  const jobID = props.jobID
  loading.value = true
  fetchError.value = ''
  noReport.value = false
  report.value = null
  try {
    const res = await getDataMigrationReport(jobID)
    if (request !== reportRequest) return
    report.value = res.data
    emit('loaded', jobID)
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

function exportReport() {
  if (!report.value) return
  const r = report.value
  const lines: string[] = []

  lines.push('单次迁移报告')
  lines.push(`Job ID: ${props.jobID}`)
  lines.push(`导出时间: ${new Date().toLocaleString('zh-CN')}`)
  lines.push('')

  if (r.preflight) { lines.push('=== 兼容预检与计划 ===', JSON.stringify(r.preflight, null, 2), '') }
  if (r.warnings?.length) {
    lines.push('=== 迁移告警 ===')
    for (const warning of r.warnings) lines.push(`${warning.table}.${warning.column}: ${warning.reason}（${warning.count}）`)
    lines.push('')
  }
  lines.push('=== 迁移概览 ===')
  const categories = [
    { label: '表', cat: r.tables, isTrigger: false },
    { label: '数据写入', cat: r.data, isTrigger: false },
    { label: '主键', cat: r.primaryKeys, isTrigger: false },
    { label: '视图', cat: r.views, isTrigger: false },
    { label: '索引', cat: r.indexes, isTrigger: false },
    { label: '外键', cat: r.constraints, isTrigger: false },
    { label: '序列', cat: r.sequences, isTrigger: false },
    { label: '注释', cat: r.comments, isTrigger: false },
    { label: '触发器', cat: r.triggers, isTrigger: true },
  ]
  for (const { label, cat, isTrigger } of categories) {
    if (isTrigger) {
      const totalStr = cat.total === -1 ? '获取失败' : String(cat.total)
      lines.push(`${label.padEnd(6)}总计 ${totalStr}  （未迁移）`)
    } else {
      lines.push(`${label.padEnd(6)}总计 ${cat.total}  成功 ${cat.success}  失败 ${cat.failed}`)
    }
  }
  lines.push('')

  const failedCategories = categories.filter(({ cat }) => cat.failed > 0 && cat.items.length > 0)
  if (failedCategories.length > 0) {
    lines.push('=== 失败对象详情 ===')
    for (const { label, cat } of failedCategories) {
      lines.push('')
      lines.push(`【${label}】`)
      for (const item of cat.items) {
        lines.push(`  ● ${item.name}`)
        lines.push(`    失败原因：${item.error}`)
        if (item.ddl) {
          lines.push('    DDL：')
          for (const ddlLine of item.ddl.split('\n')) {
            lines.push(`      ${ddlLine}`)
          }
        } else {
          lines.push('    DDL：（无）')
        }
      }
    }
    lines.push('')
  }

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
.failure-error {
  color: #f53f3f;
  margin-bottom: 4px;
  font-size: 13px;
}
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
.failure-error, .failure-name { overflow-wrap: anywhere; }
.migration-report-panel :deep(.arco-table-cell) { overflow-wrap: anywhere; }
/* Arco 2.58 falls back to its default icon for null. Keep the icon contract,
   and remove non-actionable buttons from both pointer and keyboard navigation. */
.migration-report-panel :deep(.report-row-no-details .arco-table-expand-btn) { display: none; }
.migration-report-panel :deep(.arco-tabs-tab-active), .migration-report-panel :deep(.arco-tabs-tab:hover) { color: #15803d; }
.migration-report-panel :deep(.arco-tabs-nav-ink) { background: #16a34a; }
@container (max-width: 767px) { .report-stats { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
</style>
