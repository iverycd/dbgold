<template>
  <div class="task-center-page">
    <a-tabs v-model:active-key="activeTab" class="task-tabs" @change="handleTabChange">
      <a-tab-pane key="data" title="单次迁移">
        <TaskListToolbar
          v-model:keyword="dataKeywordInput"
          v-model:status="dataStatus"
          v-model:origin="dataOrigin"
          :status-options="dataStatusOptions"
          :loading="dataJobsLoading"
          :last-updated="dataLastUpdatedText"
          show-origin
          @refresh="loadDataJobs(false)"
        />

        <a-table
          :data="dataJobs"
          :loading="dataJobsLoading"
          :pagination="dataPagination"
          row-key="id"
          size="small"
          class="task-table"
          @page-change="handleDataPageChange"
          @page-size-change="handleDataPageSizeChange"
        >
          <template #columns>
            <a-table-column title="任务" :width="165">
              <template #cell="{ record }">
                <div class="task-identity">
                  <a-tooltip :content="record.job_id" mini>
                    <router-link class="job-link" :to="dataDetailPath(record.job_id)">
                      {{ shortJobID(record.job_id) }}
                    </router-link>
                  </a-tooltip>
                  <div class="task-meta-row">
                    <a-tag size="small" color="arcoblue">{{ dataTaskType(record) }}</a-tag>
                    <a-tooltip v-if="record.batch_id" :content="`批次 ${record.batch_id}`" mini>
                      <a-tag size="small" color="purple">批量</a-tag>
                    </a-tooltip>
                    <a-tag v-else size="small" color="gray">单次</a-tag>
                  </div>
                </div>
              </template>
            </a-table-column>

            <a-table-column title="迁移链路">
              <template #cell="{ record }">
                <MigrationRouteCell :source="dataSource(record)" :destination="dataDestination(record)" />
              </template>
            </a-table-column>

            <a-table-column title="状态" :width="165">
              <template #cell="{ record }">
                <div class="status-cell">
                  <a-tag :color="dataJobStatusColor(record.status)">{{ dataJobStatusText(record.status) }}</a-tag>
                  <a-tooltip v-if="record.status === 'failed' && record.summary" :content="record.summary" mini>
                    <span class="status-summary">{{ record.summary }}</span>
                  </a-tooltip>
                </div>
              </template>
            </a-table-column>

            <a-table-column title="时间" :width="155">
              <template #cell="{ record }">
                <div class="time-cell">
                  <span>{{ formatDate(record.created_at) }}</span>
                  <span class="time-secondary">{{ dataDurationText(record) }}</span>
                </div>
              </template>
            </a-table-column>

            <a-table-column title="操作" :width="76" align="center">
              <template #cell="{ record }">
                <a-button size="mini" @click="viewReport(record)">详情</a-button>
              </template>
            </a-table-column>
          </template>
          <template #empty>
            <a-empty description="暂无符合条件的单次迁移任务" />
          </template>
        </a-table>
      </a-tab-pane>

      <a-tab-pane key="incremental" title="增量迁移">
        <IncrementalHistoryPanel :active="activeTab === 'incremental'" />
      </a-tab-pane>
    </a-tabs>
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { Message } from '@arco-design/web-vue'
import IncrementalHistoryPanel from './IncrementalHistoryPanel.vue'
import MigrationRouteCell, { type MigrationRouteEndpoint } from '@/components/MigrationRouteCell.vue'
import TaskListToolbar, { type TaskStatusOption } from '@/components/TaskListToolbar.vue'
import { listDataMigrationJobs, type DataMigrationJob, type DataMigrationListQuery } from '@/api/migration'

type PageSize = 20 | 50 | 100
type DataOrigin = 'all' | 'single' | 'batch'

const route = useRoute()
const router = useRouter()
const activeTab = ref(route.query.tab === 'incremental' ? 'incremental' : 'data')
const dataJobs = ref<DataMigrationJob[]>([])
const dataJobsLoading = ref(false)
const dataTotal = ref(0)
const dataLastUpdated = ref<Date | null>(null)
const dataKeywordInput = ref('')
const dataKeyword = ref('')
const dataStatus = ref('')
const dataOrigin = ref<DataOrigin>('all')
const dataPage = ref(1)
const dataPageSize = ref<PageSize>(20)

let keywordTimer: number | undefined
let pollTimer: number | undefined
let requestSerial = 0
let disposed = false
let dataInitialized = false

const dataStatusOptions: TaskStatusOption[] = [
  { value: 'running', label: '运行中' },
  { value: 'done', label: '成功' },
  { value: 'failed', label: '失败' },
  { value: 'cancelled', label: '已取消' },
]

const dataPagination = computed(() => ({
  current: dataPage.value,
  pageSize: dataPageSize.value,
  total: dataTotal.value,
  showTotal: true,
  showPageSize: true,
  pageSizeOptions: [20, 50, 100],
}))

const dataLastUpdatedText = computed(() => dataLastUpdated.value?.toLocaleTimeString('zh-CN', { hour12: false }) || '')

function queryString(value: unknown) {
  return typeof value === 'string' ? value : ''
}

function parsePositiveInt(value: unknown, fallback: number) {
  const parsed = Number(queryString(value))
  return Number.isInteger(parsed) && parsed > 0 ? parsed : fallback
}

function parsePageSize(value: unknown): PageSize {
  const parsed = parsePositiveInt(value, 20)
  return parsed === 50 || parsed === 100 ? parsed : 20
}

function readDataQuery() {
  const keyword = queryString(route.query.q)
  const status = dataStatusOptions.some(option => option.value === route.query.status) ? queryString(route.query.status) : ''
  const origin = ['all', 'single', 'batch'].includes(queryString(route.query.origin)) ? queryString(route.query.origin) as DataOrigin : 'all'
  dataKeywordInput.value = keyword
  dataKeyword.value = keyword
  dataStatus.value = status
  dataOrigin.value = origin
  dataPage.value = parsePositiveInt(route.query.page, 1)
  dataPageSize.value = parsePageSize(route.query.page_size)
}

function dataQueryObject() {
  const query: Record<string, string> = {}
  if (dataKeyword.value) query.q = dataKeyword.value
  if (dataStatus.value) query.status = dataStatus.value
  if (dataOrigin.value !== 'all') query.origin = dataOrigin.value
  if (dataPage.value !== 1) query.page = String(dataPage.value)
  if (dataPageSize.value !== 20) query.page_size = String(dataPageSize.value)
  return query
}

function syncDataQuery() {
  if (activeTab.value !== 'data') return
  const next = dataQueryObject()
  const current = Object.fromEntries(Object.entries(route.query).filter(([, value]) => typeof value === 'string')) as Record<string, string>
  if (JSON.stringify(next) !== JSON.stringify(current)) void router.replace({ path: '/history', query: next })
}

function scheduleDataPoll() {
  if (pollTimer) window.clearTimeout(pollTimer)
  pollTimer = undefined
  if (
    !disposed && activeTab.value === 'data' && document.visibilityState === 'visible'
    && dataJobs.value.some(job => job.status === 'running')
  ) {
    pollTimer = window.setTimeout(() => void loadDataJobs(true), 5000)
  }
}

async function loadDataJobs(silent = false) {
  if (disposed || activeTab.value !== 'data') return
  const serial = ++requestSerial
  if (!silent) dataJobsLoading.value = true
  const params: DataMigrationListQuery = {
    page: dataPage.value,
    page_size: dataPageSize.value,
    keyword: dataKeyword.value || undefined,
    status: dataStatus.value || undefined,
    origin: dataOrigin.value,
  }
  try {
    const { data } = await listDataMigrationJobs(params)
    if (disposed || serial !== requestSerial) return
    dataJobs.value = data.items
    dataTotal.value = data.total
    dataLastUpdated.value = new Date()
    if (data.items.length === 0 && data.page > 1 && data.total > 0) {
      dataPage.value = Math.max(1, Math.ceil(data.total / data.page_size))
      return
    }
  } catch {
    if (!silent && serial === requestSerial) Message.error('加载单次迁移任务失败')
  } finally {
    if (serial === requestSerial) {
      dataJobsLoading.value = false
      scheduleDataPoll()
    }
  }
}

function handleTabChange(key: string | number) {
  const tab = String(key) === 'incremental' ? 'incremental' : 'data'
  void router.replace({ path: '/history', query: tab === 'incremental' ? { tab: 'incremental' } : {} })
}

function handleDataPageChange(page: number) {
  dataPage.value = page
}

function handleDataPageSizeChange(size: number) {
  dataPageSize.value = size === 50 || size === 100 ? size : 20
  dataPage.value = 1
}

function dataDetailPath(jobID: string) {
  return `/history/data/${encodeURIComponent(jobID)}`
}

function viewReport(record: DataMigrationJob) {
  router.push(dataDetailPath(record.job_id))
}

function shortJobID(jobID: string) {
  return jobID.length > 10 ? `${jobID.slice(0, 8)}…` : jobID
}

function dataTaskType(record: DataMigrationJob) {
  return record.migrate_objects ? '对象迁移' : '数据迁移'
}

function dataJobStatusColor(status: string) {
  return { done: 'green', failed: 'red', running: 'blue', cancelled: 'gray' }[status] ?? 'gray'
}

function dataJobStatusText(status: string) {
  return { done: '成功', failed: '失败', running: '运行中', cancelled: '已取消' }[status] ?? status
}

function dataSource(record: DataMigrationJob): MigrationRouteEndpoint {
  return {
    dbType: record.src_db_type,
    name: record.src_conn?.name,
    host: record.src_conn?.host,
    port: record.src_conn?.port,
    database: record.src_conn?.database,
    username: record.src_conn?.username,
  }
}

function dataDestination(record: DataMigrationJob): MigrationRouteEndpoint {
  return {
    dbType: record.dst_db_type,
    name: record.dst_conn?.name,
    host: record.dst_conn?.host,
    port: record.dst_conn?.port,
    database: record.dst_conn?.database,
    username: record.dst_conn?.username,
    schema: record.dst_schema,
  }
}

function formatDate(value: string) {
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value || '—'
  const datePart = date.toLocaleDateString('zh-CN', { year: 'numeric', month: '2-digit', day: '2-digit' })
  const timePart = date.toLocaleTimeString('zh-CN', { hour12: false })
  return `${datePart} ${timePart}`
}

function durationText(start: string, end?: string) {
  const startTime = new Date(start).getTime()
  const endTime = end ? new Date(end).getTime() : Date.now()
  if (!Number.isFinite(startTime) || !Number.isFinite(endTime)) return '—'
  const seconds = Math.max(0, Math.floor((endTime - startTime) / 1000))
  if (seconds < 60) return `${seconds} 秒`
  if (seconds < 3600) return `${Math.floor(seconds / 60)} 分 ${seconds % 60} 秒`
  const hours = Math.floor(seconds / 3600)
  return `${hours} 小时 ${Math.floor((seconds % 3600) / 60)} 分`
}

function dataDurationText(record: DataMigrationJob) {
  const prefix = record.finished_at ? '耗时' : '已运行'
  return `${prefix} ${durationText(record.created_at, record.finished_at)}`
}

watch(dataKeywordInput, value => {
  if (keywordTimer) window.clearTimeout(keywordTimer)
  keywordTimer = window.setTimeout(() => {
    dataKeyword.value = value.trim()
    dataPage.value = 1
  }, 300)
})

watch([dataKeyword, dataStatus, dataOrigin, dataPage, dataPageSize], () => {
	if (!dataInitialized || activeTab.value !== 'data') return
  syncDataQuery()
  void loadDataJobs(false)
})

watch(() => route.query, () => {
  const nextTab = route.query.tab === 'incremental' ? 'incremental' : 'data'
  if (activeTab.value !== nextTab) activeTab.value = nextTab
  if (nextTab !== 'data') return
  const nextKeyword = queryString(route.query.q)
  const nextStatus = queryString(route.query.status)
  const nextOrigin = queryString(route.query.origin) || 'all'
  const nextPage = parsePositiveInt(route.query.page, 1)
  const nextPageSize = parsePageSize(route.query.page_size)
  if (
    nextKeyword !== dataKeyword.value || nextStatus !== dataStatus.value || nextOrigin !== dataOrigin.value
    || nextPage !== dataPage.value || nextPageSize !== dataPageSize.value
  ) readDataQuery()
})

watch(activeTab, tab => {
	if (tab === 'data') {
		dataInitialized = false
		readDataQuery()
		dataInitialized = true
		void loadDataJobs(false)
  } else if (pollTimer) {
    window.clearTimeout(pollTimer)
    pollTimer = undefined
  }
})

function handleVisibilityChange() {
  if (document.visibilityState === 'visible') {
    if (activeTab.value === 'data') void loadDataJobs(true)
  } else if (pollTimer) {
    window.clearTimeout(pollTimer)
    pollTimer = undefined
  }
}

onMounted(() => {
	readDataQuery()
	dataInitialized = true
	if (activeTab.value === 'data') void loadDataJobs(false)
  document.addEventListener('visibilitychange', handleVisibilityChange)
})

onBeforeUnmount(() => {
  disposed = true
  requestSerial++
  if (keywordTimer) window.clearTimeout(keywordTimer)
  if (pollTimer) window.clearTimeout(pollTimer)
  document.removeEventListener('visibilitychange', handleVisibilityChange)
})
</script>

<style scoped>
.task-center-page {
  min-width: 0;
  padding: 0;
}
.task-tabs :deep(.arco-tabs-nav) { margin-bottom: 14px; }
.task-tabs :deep(.arco-tabs-content) { padding-top: 0; }
.task-table { min-width: 0; }
.task-table :deep(.arco-table-container) { overflow-x: hidden; }
.task-table :deep(.arco-table-cell) { padding: 10px 12px; }
.task-table :deep(.arco-table-pagination) { margin: 12px 0 0; }
.task-identity,
.status-cell,
.time-cell {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: 5px;
  min-width: 0;
}
.job-link {
  color: #165dff;
  font-family: var(--font-mono);
  font-size: 12px;
  font-weight: 600;
  text-decoration: none;
}
.job-link:hover { text-decoration: underline; }
.job-link:focus-visible { border-radius: 3px; outline: 2px solid #165dff; outline-offset: 2px; }
.task-meta-row { display: flex; gap: 4px; }
.status-summary {
  display: block;
  max-width: 145px;
  overflow: hidden;
  color: var(--destructive);
  font-size: 11px;
  text-overflow: ellipsis;
  white-space: nowrap;
  cursor: default;
}
.time-cell { gap: 3px; font-size: 12px; white-space: nowrap; }
.time-secondary { color: var(--fg-muted); font-size: 11px; }
</style>
