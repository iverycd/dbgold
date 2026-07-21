<template>
  <div class="detail-page">
    <a-spin v-if="loading && !job" class="page-loading" />

    <a-result v-else-if="loadError && !job" status="error" title="无法加载单次迁移任务" :subtitle="loadError">
      <template #extra>
        <a-space>
          <a-button @click="backToHistory">返回任务中心</a-button>
          <a-button type="primary" @click="load">重试</a-button>
        </a-space>
      </template>
    </a-result>

    <template v-else-if="job">
      <header class="task-header">
        <div class="task-heading">
          <a-button type="text" class="back-button" @click="backToHistory">
            <template #icon><icon-left /></template>
            返回单次迁移
          </a-button>
          <div class="title-line">
            <h1>单次迁移报告</h1>
            <a-tag :color="statusColor(job.status)">{{ statusText(job.status) }}</a-tag>
            <span v-if="refreshing" class="refreshing"><icon-loading /> 正在刷新</span>
          </div>
          <div class="job-identity">
            <code>{{ job.job_id }}</code>
            <a-button type="text" size="mini" aria-label="复制 Job ID" @click="copy(job.job_id, 'Job ID')"><icon-copy /></a-button>
          </div>
          <p class="task-summary">{{ summaryText }}</p>
        </div>
        <div class="task-actions">
          <a-button :disabled="!report" @click="exportReport"><template #icon><icon-download /></template>导出报告</a-button>
          <a-button :loading="refreshing" @click="load"><template #icon><icon-refresh /></template>刷新</a-button>
        </div>
      </header>

      <MigrationConnectionRoute
        :src-db-type="job.src_db_type"
        :dst-db-type="job.dst_db_type"
        :src-conn="job.src_conn"
        :dst-conn="job.dst_conn"
        :src-database="job.src_conn?.database"
        :dst-database="job.dst_conn?.database"
        :dst-schema="job.dst_schema"
        :mode-label="migrationModeText"
      />

      <section class="metrics-grid" aria-label="报告指标">
        <div class="metric-item metric-highlight">
          <span class="metric-label">报告状态</span>
          <strong>{{ reportHealthText }}</strong>
          <span class="metric-note">{{ report ? '报告已生成' : '等待迁移结果' }}</span>
        </div>
        <div class="metric-item">
          <span class="metric-label">迁移耗时</span>
          <strong>{{ durationText }}</strong>
          <span class="metric-note">开始 {{ formatDate(job.created_at) }}</span>
        </div>
        <div class="metric-item">
          <span class="metric-label">表结构</span>
          <strong>{{ report?.tables.success || 0 }} / {{ report?.tables.total || 0 }}</strong>
          <span class="metric-note">成功 / 总数</span>
        </div>
        <div class="metric-item">
          <span class="metric-label">报告项</span>
          <strong>{{ objectSuccessCount }} / {{ objectTotalCount }}</strong>
          <span class="metric-note">失败 {{ failedObjectCount }} 项</span>
        </div>
        <div class="metric-item">
          <span class="metric-label">行数对比</span>
          <strong>{{ matchedRows.length }} / {{ allRowCounts.length }}</strong>
          <span class="metric-note">不一致 {{ mismatchedRows.length }} 张表</span>
        </div>
        <div class="metric-item">
          <span class="metric-label">并行配置</span>
          <strong>{{ job.max_parallel || 0 }} / {{ job.intra_table_parallel || 0 }}</strong>
          <span class="metric-note">任务 / 表内并行 · 每页 {{ job.page_size || 0 }}</span>
        </div>
      </section>

      <section v-if="risks.length" class="risk-section">
        <div class="section-heading">
          <div><span class="eyebrow">需要处理</span><h2>报告风险与下一步</h2></div>
          <a-tag color="red">{{ risks.length }} 项</a-tag>
        </div>
        <div class="risk-list">
          <article v-for="risk in risks" :key="risk.key" class="risk-item" :class="`risk-${risk.level}`">
            <div class="risk-icon"><icon-exclamation-circle-fill /></div>
            <div class="risk-content">
              <strong>{{ risk.title }}</strong>
              <p>{{ risk.description }}</p>
            </div>
            <a-button v-if="risk.tab" size="small" @click="activeTab = risk.tab">{{ risk.action }}</a-button>
          </article>
        </div>
      </section>

      <section class="detail-tabs-surface">
        <a-alert v-if="reportError" type="warning" class="report-alert">
          {{ reportError }}
        </a-alert>

        <a-tabs v-model:active-key="activeTab" lazy-load>
          <a-tab-pane key="overview" title="迁移概览">
            <div v-if="report" class="tab-content">
              <div class="tab-toolbar">
                <div><h2>分类迁移结果</h2><p>按对象类型核对迁移数量和失败情况。</p></div>
              </div>
              <a-table :data="categoryRows" row-key="key" :pagination="false" size="small" :scroll="{ x: 760 }">
                <template #columns>
                  <a-table-column title="对象类型" data-index="label" :width="150" />
                  <a-table-column title="总数" data-index="total" :width="100" />
                  <a-table-column title="成功" :width="100"><template #cell="{ record }">{{ record.isTrigger ? '—' : record.success }}</template></a-table-column>
                  <a-table-column title="失败" :width="100"><template #cell="{ record }"><span :class="{ 'danger-text': record.failed > 0 }">{{ record.isTrigger ? '—' : record.failed }}</span></template></a-table-column>
                  <a-table-column title="成功率" :width="180">
                    <template #cell="{ record }">
                      <span v-if="record.isTrigger || record.total <= 0">—</span>
                      <a-progress v-else size="small" :percent="record.success / record.total" :show-text="false" :status="record.failed > 0 ? 'warning' : 'success'" />
                    </template>
                  </a-table-column>
                  <a-table-column title="状态">
                    <template #cell="{ record }">
                      <a-tag v-if="record.isTrigger" color="gray">{{ record.total === -1 ? '获取失败 · 未迁移' : `${record.total} 个 · 未迁移` }}</a-tag>
                      <a-tag v-else-if="record.failed > 0" color="orange">部分失败</a-tag>
                      <a-tag v-else-if="record.total === 0" color="gray">无对象</a-tag>
                      <a-tag v-else color="green">全部成功</a-tag>
                    </template>
                  </a-table-column>
                </template>
              </a-table>
            </div>
            <a-empty v-else :description="reportError || '报告尚未生成'" />
          </a-tab-pane>

          <a-tab-pane v-if="failedItems.length" key="failures">
            <template #title>失败对象 <a-badge :count="failedItems.length" /></template>
            <div class="tab-content">
              <div class="tab-toolbar filter-toolbar">
                <div><h2>失败对象</h2><p>筛选并查看完整错误与待修复 DDL。</p></div>
                <div class="filters">
                  <a-select v-model="failureCategory" style="width: 150px">
                    <a-option value="all">全部分类</a-option>
                    <a-option v-for="row in failedCategoryOptions" :key="row.key" :value="row.key">{{ row.label }}</a-option>
                  </a-select>
                  <a-input-search v-model="failureKeyword" allow-clear placeholder="搜索对象名称" style="width: 240px" />
                </div>
              </div>
              <a-table :data="filteredFailedItems" row-key="key" size="small" :pagination="{ pageSize: 30 }" :scroll="{ x: 900, y: 480 }" :expandable="failedExpandable">
                <template #columns>
                  <a-table-column title="对象" data-index="name" :width="260" />
                  <a-table-column title="分类" data-index="categoryLabel" :width="130" />
                  <a-table-column title="错误摘要"><template #cell="{ record }"><span class="error-clamp">{{ record.error || '—' }}</span></template></a-table-column>
                  <a-table-column title="DDL" :width="90"><template #cell="{ record }"><a-tag :color="record.ddl ? 'orange' : 'gray'">{{ record.ddl ? '有' : '无' }}</a-tag></template></a-table-column>
                </template>
                <template #expand-row="{ record }">
                  <div class="failure-detail">
                    <div><strong>完整错误</strong><pre>{{ record.error || '—' }}</pre><a-button size="mini" type="text" @click="copy(record.error, '错误详情')"><icon-copy /> 复制错误</a-button></div>
                    <div v-if="record.ddl"><strong>失败 DDL</strong><pre>{{ record.ddl }}</pre><a-button size="mini" type="text" @click="copy(record.ddl, '失败 DDL')"><icon-copy /> 复制 DDL</a-button></div>
                  </div>
                </template>
              </a-table>
              <a-empty v-if="!filteredFailedItems.length" description="没有符合筛选条件的失败对象" />
            </div>
          </a-tab-pane>

          <a-tab-pane v-if="allRowCounts.length" key="rowcounts">
            <template #title>行数对比 <a-badge v-if="mismatchedRows.length" :count="mismatchedRows.length" /></template>
            <div class="tab-content">
              <div class="tab-toolbar filter-toolbar">
                <div><h2>源端与目标端行数</h2><p>默认优先显示不一致表，可切换查看全部结果。</p></div>
                <div class="filters">
                  <a-radio-group v-model="rowCountFilter" type="button">
                    <a-radio value="mismatch">仅不一致</a-radio>
                    <a-radio value="all">全部</a-radio>
                  </a-radio-group>
                  <a-input-search v-model="rowCountKeyword" allow-clear placeholder="搜索表名" style="width: 220px" />
                </div>
              </div>
              <a-table :data="filteredRowCounts" row-key="table" size="small" :pagination="{ pageSize: 50 }" :scroll="{ x: 760, y: 520 }">
                <template #columns>
                  <a-table-column title="表名" data-index="table" :width="300" />
                  <a-table-column title="源行数" data-index="src" :width="140" />
                  <a-table-column title="目标行数" data-index="dst" :width="140" />
                  <a-table-column title="差异" :width="120"><template #cell="{ record }"><span :class="{ 'danger-text': !record.match }">{{ signedDifference(record.dst - record.src) }}</span></template></a-table-column>
                  <a-table-column title="结果"><template #cell="{ record }"><a-tag :color="record.match ? 'green' : 'orange'">{{ record.match ? '一致' : '不一致' }}</a-tag></template></a-table-column>
                </template>
              </a-table>
              <a-empty v-if="!filteredRowCounts.length" description="没有符合筛选条件的表" />
            </div>
          </a-tab-pane>

          <a-tab-pane key="config" title="任务配置">
            <div class="tab-content config-grid">
              <section class="config-section">
                <div class="section-heading compact"><div><span class="eyebrow">MIGRATION</span><h2>迁移范围</h2></div></div>
                <dl class="info-grid">
                  <div><dt>迁移类型</dt><dd>{{ migrationTypeText }}</dd></div>
                  <div><dt>迁移模式</dt><dd>{{ migrationModeText }}</dd></div>
                  <div class="full"><dt>表过滤条件</dt><dd class="mono-value">{{ job.table_filter || '全部表' }}</dd></div>
                  <div><dt>目标 Schema</dt><dd>{{ job.dst_schema || '使用连接默认值' }}</dd></div>
                  <div><dt>所属批次</dt><dd>{{ job.batch_id || '单任务' }}</dd></div>
                </dl>
              </section>
              <section class="config-section">
                <div class="section-heading compact"><div><span class="eyebrow">PERFORMANCE</span><h2>执行参数</h2></div></div>
                <dl class="info-grid">
                  <div><dt>分页大小</dt><dd>{{ job.page_size || '—' }}</dd></div>
                  <div><dt>最大并行</dt><dd>{{ job.max_parallel || '—' }}</dd></div>
                  <div><dt>表内并行</dt><dd>{{ job.intra_table_parallel || '—' }}</dd></div>
                  <div><dt>名称转小写</dt><dd>{{ yesNo(job.lower_case_names) }}</dd></div>
                  <div><dt>字符按长度映射</dt><dd>{{ yesNo(job.char_in_length) }}</dd></div>
                  <div><dt>使用 NVARCHAR2</dt><dd>{{ yesNo(job.use_nvarchar2) }}</dd></div>
                  <div><dt>修改对象 Owner</dt><dd>{{ yesNo(job.change_owner) }}</dd></div>
                </dl>
              </section>
              <section class="config-section full-width">
                <div class="section-heading compact"><div><span class="eyebrow">TIMELINE</span><h2>时间信息</h2></div></div>
                <dl class="info-grid timeline-grid">
                  <div><dt>开始时间</dt><dd>{{ formatDate(job.created_at) }}</dd></div>
                  <div><dt>结束时间</dt><dd>{{ formatDate(job.finished_at) }}</dd></div>
                  <div><dt>迁移耗时</dt><dd>{{ durationText }}</dd></div>
                </dl>
              </section>
            </div>
          </a-tab-pane>
        </a-tabs>
      </section>
    </template>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { Message } from '@arco-design/web-vue'
import MigrationConnectionRoute from '@/components/MigrationConnectionRoute.vue'
import { copyText } from '@/utils/clipboard'
import {
  getDataMigrationJob,
  getDataMigrationReport,
  type CategoryReport,
  type DataMigrationJob,
  type MigrationReport,
  type ObjectResult,
  type TableRowCount,
} from '@/api/migration'

interface CategoryRow extends CategoryReport { key: string; label: string; isTrigger: boolean }
interface FailedItem extends ObjectResult { key: string; category: string; categoryLabel: string }
interface RiskItem { key: string; level: 'warning' | 'danger'; title: string; description: string; tab?: string; action?: string }

const route = useRoute()
const router = useRouter()
const job = ref<DataMigrationJob | null>(null)
const report = ref<MigrationReport | null>(null)
const loading = ref(false)
const refreshing = ref(false)
const loadError = ref('')
const reportError = ref('')
const activeTab = ref('overview')
const failureCategory = ref('all')
const failureKeyword = ref('')
const rowCountFilter = ref<'mismatch' | 'all'>('mismatch')
const rowCountKeyword = ref('')
let timer: number | undefined
let requestRunning = false
let disposed = false

const jobID = computed(() => String(route.params.jobId || ''))
const categoryRows = computed<CategoryRow[]>(() => report.value ? [
  { key: 'tables', label: '表', ...report.value.tables, isTrigger: false },
  { key: 'data', label: '数据写入', ...report.value.data, isTrigger: false },
  { key: 'primaryKeys', label: '主键', ...report.value.primaryKeys, isTrigger: false },
  { key: 'views', label: '视图', ...report.value.views, isTrigger: false },
  { key: 'indexes', label: '索引', ...report.value.indexes, isTrigger: false },
  { key: 'constraints', label: '外键', ...report.value.constraints, isTrigger: false },
  { key: 'sequences', label: '序列', ...report.value.sequences, isTrigger: false },
  { key: 'comments', label: '注释', ...report.value.comments, isTrigger: false },
  { key: 'triggers', label: '触发器', ...report.value.triggers, isTrigger: true },
] : [])
const migratedCategories = computed(() => categoryRows.value.filter(row => !row.isTrigger))
const objectTotalCount = computed(() => migratedCategories.value.reduce((sum, row) => sum + Math.max(row.total, 0), 0))
const objectSuccessCount = computed(() => migratedCategories.value.reduce((sum, row) => sum + row.success, 0))
const failedObjectCount = computed(() => migratedCategories.value.reduce((sum, row) => sum + row.failed, 0))
const failedItems = computed<FailedItem[]>(() => migratedCategories.value.flatMap(row => row.items.map((item, index) => ({ ...item, key: `${row.key}-${item.name}-${index}`, category: row.key, categoryLabel: row.label }))))
const failedCategoryOptions = computed(() => migratedCategories.value.filter(row => row.items.length).map(row => ({ key: row.key, label: row.label })))
const filteredFailedItems = computed(() => {
  const keyword = failureKeyword.value.trim().toLowerCase()
  return failedItems.value.filter(item => (failureCategory.value === 'all' || item.category === failureCategory.value) && (!keyword || item.name.toLowerCase().includes(keyword)))
})
const failedExpandable = { icon: (_: boolean, record: any) => (record.error || record.ddl) ? undefined : null }

const allRowCounts = computed<TableRowCount[]>(() => report.value?.rowCounts || [])
const matchedRows = computed(() => allRowCounts.value.filter(row => row.match))
const mismatchedRows = computed(() => allRowCounts.value.filter(row => !row.match))
const filteredRowCounts = computed(() => {
  const keyword = rowCountKeyword.value.trim().toLowerCase()
  return allRowCounts.value.filter(row => (rowCountFilter.value === 'all' || !row.match) && (!keyword || row.table.toLowerCase().includes(keyword)))
})

const migrationTypeText = computed(() => job.value?.migrate_objects ? '仅对象迁移' : '单次迁移')
const migrationModeText = computed(() => {
  if (job.value?.migrate_objects) return '对象迁移'
  return ({ all: '全部表', include: '仅包含指定表', exclude: '排除指定表' } as Record<string, string>)[job.value?.migrate_mode || ''] || job.value?.migrate_mode || '单次迁移'
})
const durationText = computed(() => {
  if (!job.value?.finished_at) return job.value?.status === 'running' ? '进行中' : '—'
  const duration = Math.max(0, new Date(job.value.finished_at).getTime() - new Date(job.value.created_at).getTime())
  const seconds = Math.floor(duration / 1000)
  const hours = Math.floor(seconds / 3600)
  const minutes = Math.floor((seconds % 3600) / 60)
  const remainder = seconds % 60
  return hours ? `${hours} 小时 ${minutes} 分` : minutes ? `${minutes} 分 ${remainder} 秒` : `${remainder} 秒`
})
const reportHealthText = computed(() => {
  if (!report.value) return job.value?.status === 'running' ? '生成中' : '暂无报告'
  if (failedObjectCount.value || mismatchedRows.value.length) return '需要处理'
  return '全部通过'
})
const summaryText = computed(() => job.value?.summary || (job.value?.status === 'running' ? '迁移正在执行，完成后将在此生成对象与行数校验报告。' : reportHealthText.value === '全部通过' ? '迁移对象与行数校验均已通过。' : '迁移已结束，请核对失败对象和行数差异。'))
const risks = computed<RiskItem[]>(() => {
  const items: RiskItem[] = []
  if (job.value?.status === 'failed') items.push({ key: 'job-failed', level: 'danger', title: '迁移任务以失败状态结束', description: '请优先核对失败对象及错误信息，确认目标端是否需要清理或补建。', tab: failedItems.value.length ? 'failures' : 'overview', action: '查看结果' })
  if (failedObjectCount.value) items.push({ key: 'objects', level: 'danger', title: `${failedObjectCount.value} 个对象迁移失败`, description: failedItems.value.length ? '展开失败项可查看完整错误和原始 DDL，并复制后进行人工修复。' : '报告记录了失败对象，但没有保存可展开的错误明细，请结合任务日志进一步定位。', tab: failedItems.value.length ? 'failures' : 'overview', action: failedItems.value.length ? '查看失败对象' : '查看迁移概览' })
  if (mismatchedRows.value.length) items.push({ key: 'rows', level: 'warning', title: `${mismatchedRows.value.length} 张表行数不一致`, description: '在业务切换前核查源端写入、过滤范围和失败表，避免带着数据差异上线。', tab: 'rowcounts', action: '查看行数差异' })
  if (reportError.value && job.value?.status !== 'running') items.push({ key: 'report', level: 'warning', title: '报告数据不可用', description: reportError.value })
  return items
})

function statusText(status: string) { return ({ running: '迁移中', done: '已完成', failed: '失败', cancelled: '已取消' } as Record<string, string>)[status] || status }
function statusColor(status: string) { return ({ running: 'blue', done: 'green', failed: 'red', cancelled: 'gray' } as Record<string, string>)[status] || 'gray' }
function formatDate(value?: string) { return value ? new Date(value).toLocaleString('zh-CN') : '—' }
function signedDifference(value: number) { return value > 0 ? `+${value}` : String(value) }
function yesNo(value?: boolean) { return value ? '是' : '否' }

async function copy(value: string, label: string) {
  try {
    await copyText(value)
    Message.success(`${label}已复制`)
  } catch {
    Message.error('复制失败')
  }
}

async function loadReport(current: DataMigrationJob) {
  reportError.value = ''
  try {
    report.value = (await getDataMigrationReport(current.job_id)).data
  } catch (error: any) {
    report.value = null
    reportError.value = error?.response?.status === 404
      ? (current.status === 'running' ? '迁移尚未结束，报告将在任务完成后生成。' : '该任务没有可用的迁移报告。')
      : '迁移报告暂时无法加载，请稍后重试。'
  }
}

async function load() {
  if (requestRunning || disposed) return
  requestRunning = true
  if (job.value) refreshing.value = true
  else loading.value = true
  loadError.value = ''
  try {
    const current = (await getDataMigrationJob(jobID.value)).data
    if (disposed) return
    job.value = current
    await loadReport(current)
    if (current.status === 'running' && !timer) timer = window.setInterval(load, 5000)
    if (current.status !== 'running' && timer) {
      clearInterval(timer)
      timer = undefined
    }
  } catch (error: any) {
    loadError.value = error?.response?.data?.error || '单次迁移任务不存在或无权访问'
  } finally {
    requestRunning = false
    loading.value = false
    refreshing.value = false
  }
}

function exportReport() {
  if (!report.value || !job.value) return
  const lines = ['单次迁移报告', `Job ID: ${job.value.job_id}`, `导出时间: ${new Date().toLocaleString('zh-CN')}`, '', '=== 迁移概览 ===']
  for (const row of categoryRows.value) {
    lines.push(row.isTrigger
      ? `${row.label.padEnd(6)}总计 ${row.total === -1 ? '获取失败' : row.total}（未迁移）`
      : `${row.label.padEnd(6)}总计 ${row.total}  成功 ${row.success}  失败 ${row.failed}`)
  }
  if (failedItems.value.length) {
    lines.push('', '=== 失败对象详情 ===')
    for (const item of failedItems.value) {
      lines.push('', `【${item.categoryLabel}】${item.name}`, `失败原因：${item.error || '—'}`, `DDL：${item.ddl || '—'}`)
    }
  }
  if (allRowCounts.value.length) {
    lines.push('', '=== 行数对比 ===')
    for (const row of allRowCounts.value) lines.push(`${row.table}：源 ${row.src} / 目标 ${row.dst} / ${row.match ? '一致' : `差异 ${signedDifference(row.dst - row.src)}`}`)
  }
  const blob = new Blob([lines.join('\n')], { type: 'text/plain;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = `migration-report-${job.value.job_id.slice(0, 8)}.txt`
  anchor.click()
  URL.revokeObjectURL(url)
}

function backToHistory() { router.push({ path: '/history', query: { tab: 'data' } }) }

onMounted(load)
onUnmounted(() => {
  disposed = true
  if (timer) clearInterval(timer)
})
</script>

<style scoped>
.detail-page { width: min(100%, 1440px); margin: 0 auto; padding-bottom: 40px; }
.page-loading { display: flex; justify-content: center; padding: 120px 0; }
.task-header { position: sticky; z-index: 20; top: 0; display: flex; align-items: flex-start; justify-content: space-between; gap: 24px; padding: 16px 20px; margin: -16px -4px 18px; border: 1px solid var(--border); border-radius: var(--radius-md); background: rgba(255,255,255,.96); box-shadow: var(--shadow-sm); backdrop-filter: blur(12px); }
.task-heading { min-width: 0; }
.back-button { padding-left: 0; margin-bottom: 4px; color: var(--fg-muted); }
.title-line { display: flex; align-items: center; flex-wrap: wrap; gap: 10px; }
.title-line h1 { margin: 0; color: var(--fg-primary); font-size: 23px; line-height: 1.35; }
.refreshing { color: var(--fg-muted); font-size: 12px; }
.job-identity { display: flex; align-items: center; gap: 2px; min-width: 0; margin-top: 5px; }
.job-identity code { overflow: hidden; color: var(--fg-secondary); font-family: var(--font-mono); font-size: 12px; text-overflow: ellipsis; white-space: nowrap; }
.task-summary { max-width: 780px; margin: 6px 0 0; color: var(--fg-muted); line-height: 1.55; overflow-wrap: anywhere; }
.task-actions { display: flex; flex-wrap: wrap; justify-content: flex-end; gap: 8px; padding-top: 28px; }
.metrics-grid { display: grid; grid-template-columns: repeat(6,minmax(0,1fr)); margin-top: 16px; border: 1px solid var(--border); border-radius: var(--radius-md); background: var(--bg-surface); box-shadow: var(--shadow-sm); }
.metric-item { display: flex; flex-direction: column; min-width: 0; padding: 17px 18px; border-right: 1px solid var(--border); }
.metric-item:last-child { border-right: 0; }
.metric-highlight { background: linear-gradient(135deg,rgba(34,197,94,.08),transparent); }
.metric-label { color: var(--fg-muted); font-size: 11px; font-weight: 600; }
.metric-item strong { margin: 7px 0 4px; color: var(--fg-primary); font-family: var(--font-mono); font-size: 18px; overflow-wrap: anywhere; }
.metric-note { color: var(--fg-muted); font-size: 11px; line-height: 1.4; overflow-wrap: anywhere; }
.risk-section,.detail-tabs-surface { padding: 20px; margin-top: 16px; border: 1px solid var(--border); border-radius: var(--radius-md); background: var(--bg-surface); box-shadow: var(--shadow-sm); }
.section-heading { display: flex; align-items: center; justify-content: space-between; gap: 16px; margin-bottom: 14px; }
.section-heading.compact { margin-bottom: 10px; }
.section-heading h2,.tab-toolbar h2 { margin: 2px 0 0; font-size: 16px; }
.eyebrow { color: var(--fg-muted); font-size: 10px; font-weight: 700; letter-spacing: .1em; }
.risk-list { display: grid; gap: 10px; }
.risk-item { display: flex; align-items: flex-start; gap: 12px; padding: 14px; border-left: 3px solid; background: var(--bg-surface2); }
.risk-warning { border-color: #f59e0b; }
.risk-danger { border-color: var(--destructive); }
.risk-icon { flex: none; padding-top: 1px; color: #f59e0b; }
.risk-danger .risk-icon { color: var(--destructive); }
.risk-content { min-width: 0; flex: 1; }
.risk-content p { margin: 4px 0 0; color: var(--fg-secondary); line-height: 1.55; }
.detail-tabs-surface { min-height: 380px; }
.report-alert { margin-bottom: 12px; }
.tab-content { padding-top: 8px; }
.tab-toolbar { display: flex; align-items: flex-start; justify-content: space-between; gap: 16px; margin: 0 0 18px; }
.tab-toolbar p { margin: 5px 0 0; color: var(--fg-muted); }
.filter-toolbar { align-items: flex-end; }
.filters { display: flex; flex-wrap: wrap; justify-content: flex-end; gap: 10px; }
.danger-text { color: var(--destructive); font-weight: 600; }
.error-clamp { display: -webkit-box; overflow: hidden; line-height: 1.5; overflow-wrap: anywhere; -webkit-box-orient: vertical; -webkit-line-clamp: 2; }
.failure-detail { display: grid; gap: 14px; padding: 10px; }
.failure-detail pre { max-height: 280px; overflow: auto; padding: 12px; border-radius: 4px; background: #111827; color: #e5e7eb; font-family: var(--font-mono); font-size: 12px; line-height: 1.6; white-space: pre-wrap; overflow-wrap: anywhere; }
.config-grid { display: grid; grid-template-columns: repeat(2,minmax(0,1fr)); gap: 24px; }
.config-section { min-width: 0; }
.full-width { grid-column: 1 / -1; }
.info-grid { display: grid; grid-template-columns: repeat(2,minmax(0,1fr)); margin: 0; border-top: 1px solid var(--border); }
.info-grid div { padding: 12px 0; border-bottom: 1px solid var(--border); }
.info-grid div:nth-child(odd) { padding-right: 14px; }
.info-grid .full { grid-column: 1 / -1; padding-right: 0; }
.info-grid dt { color: var(--fg-muted); font-size: 11px; }
.info-grid dd { margin: 5px 0 0; color: var(--fg-primary); font-size: 13px; overflow-wrap: anywhere; }
.mono-value { font-family: var(--font-mono); }
.timeline-grid { grid-template-columns: repeat(3,minmax(0,1fr)); }

@media (max-width: 1200px) {
  .metrics-grid { grid-template-columns: repeat(3,minmax(0,1fr)); }
  .metric-item:nth-child(3) { border-right: 0; }
  .metric-item:nth-child(-n+3) { border-bottom: 1px solid var(--border); }
}
@media (max-width: 900px) {
  .task-header { position: static; flex-direction: column; }
  .task-actions { justify-content: flex-start; padding-top: 0; }
  .config-grid { grid-template-columns: 1fr; }
  .full-width { grid-column: auto; }
  .filter-toolbar { align-items: stretch; flex-direction: column; }
  .filters { justify-content: flex-start; }
}
@media (max-width: 680px) {
  .detail-page { padding-bottom: 24px; }
  .task-header,.risk-section,.detail-tabs-surface { padding: 16px; }
  .metrics-grid { grid-template-columns: repeat(2,minmax(0,1fr)); }
  .metric-item { border-bottom: 1px solid var(--border); }
  .metric-item:nth-child(2n) { border-right: 0; }
  .metric-item:nth-last-child(-n+2) { border-bottom: 0; }
  .risk-item { flex-wrap: wrap; }
  .risk-item .arco-btn { margin-left: 30px; }
  .filters { flex-direction: column; }
  .filters :deep(.arco-select),.filters :deep(.arco-input-wrapper) { width: 100% !important; }
  .info-grid,.timeline-grid { grid-template-columns: 1fr; }
  .info-grid .full { grid-column: auto; }
  .info-grid div { padding-right: 0 !important; }
}
</style>
