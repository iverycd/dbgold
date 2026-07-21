<template>
  <div>
    <TaskListToolbar
      v-model:keyword="keywordInput"
      v-model:status="statusFilter"
      :status-options="statusOptions"
      :loading="loading"
      :last-updated="lastUpdatedText"
      @refresh="load(false)"
    />

    <a-table
      :data="jobs"
      :loading="loading"
      :pagination="pagination"
      :row-class="incrementalRowClass"
      row-key="id"
      size="small"
      class="task-table incremental-table"
      @page-change="handlePageChange"
      @page-size-change="handlePageSizeChange"
    >
      <template #columns>
        <a-table-column title="任务" :width="145">
          <template #cell="{ record }">
            <div class="task-identity">
              <a-tooltip :content="record.job_id" mini>
                <router-link class="job-link" :to="detailPath(record.job_id)">
                  {{ shortJobID(record.job_id) }}
                </router-link>
              </a-tooltip>
              <a-tag size="small" color="arcoblue">{{ startModeText(record.start_mode) }}</a-tag>
            </div>
          </template>
        </a-table-column>

        <a-table-column title="迁移链路">
          <template #cell="{ record }">
            <MigrationRouteCell :source="sourceEndpoint(record)" :destination="destinationEndpoint(record)" />
          </template>
        </a-table-column>

        <a-table-column title="状态与进度" :width="165">
          <template #cell="{ record }">
            <div class="status-cell">
              <a-tooltip :content="record.last_error || record.summary || incrementalStatusText(record.status)" mini>
                <a-tag :color="incrementalStatusColor(record.status)">
                  {{ record.locator_strategy_version !== 1 ? '版本升级后已废弃' : incrementalStatusText(record.status) }}
                </a-tag>
              </a-tooltip>
              <span :class="['progress-note', { caught: record.caught_up }]">{{ progressText(record) }}</span>
            </div>
          </template>
        </a-table-column>

        <a-table-column title="事件统计" :width="145">
          <template #cell="{ record }">
            <a-tooltip :content="`插入 ${record.insert_count} · 更新 ${record.update_count} · 删除 ${record.delete_count} · 跳过 ${record.skipped_count}`" mini>
              <div class="event-stats" aria-label="增量事件统计">
                <span><b>I</b>{{ compactNumber(record.insert_count) }}</span>
                <span><b>U</b>{{ compactNumber(record.update_count) }}</span>
                <span><b>D</b>{{ compactNumber(record.delete_count) }}</span>
                <span class="skipped"><b>S</b>{{ compactNumber(record.skipped_count) }}</span>
              </div>
            </a-tooltip>
          </template>
        </a-table-column>

        <a-table-column title="最后活动" :width="135">
          <template #cell="{ record }">
            <a-tooltip :content="formatIncrementalDate(record.last_event_at || record.updated_at)" mini>
              <div class="activity-cell">
                <span>{{ relativeTime(record.last_event_at || record.updated_at) }}</span>
                <span class="activity-date">{{ compactDate(record.last_event_at || record.updated_at) }}</span>
              </div>
            </a-tooltip>
          </template>
        </a-table-column>

        <a-table-column title="操作" :width="130" align="right">
          <template #cell="{ record }">
            <div class="row-actions">
              <a-button
                v-if="primaryAction(record)"
                size="mini"
                :type="primaryAction(record)?.type"
                @click="runAction(primaryAction(record)!.value, record)"
              >
                {{ primaryAction(record)?.label }}
              </a-button>
              <a-dropdown
                v-if="secondaryActions(record).length"
                trigger="click"
                @select="value => runAction(String(value), record)"
              >
                <a-button size="mini" aria-label="更多任务操作">
                  更多<icon-down />
                </a-button>
                <template #content>
                  <a-doption
                    v-for="action in secondaryActions(record)"
                    :key="action.value"
                    :value="action.value"
                    :class="{ 'danger-option': action.value === 'abort' }"
                  >
                    {{ action.label }}
                  </a-doption>
                </template>
              </a-dropdown>
              <span v-if="!primaryAction(record) && !secondaryActions(record).length" class="no-action">—</span>
            </div>
          </template>
        </a-table-column>
      </template>
      <template #empty>
        <a-empty description="暂无符合条件的增量迁移任务" />
      </template>
    </a-table>
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { Message, Modal } from '@arco-design/web-vue'
import MigrationRouteCell, { type MigrationRouteEndpoint } from '@/components/MigrationRouteCell.vue'
import TaskListToolbar, { type TaskStatusOption } from '@/components/TaskListToolbar.vue'
import {
  abortIncrementalJob,
  acknowledgeIncrementalDDL,
  cancelIncrementalCutover,
  listIncrementalJobs,
  pauseIncrementalJob,
  prepareIncrementalCutover,
  resumeIncrementalJob,
  stopIncrementalJob,
  type IncrementalJob,
  type TaskListQuery,
} from '@/api/migration'
import {
  abortable,
  cancelable,
  completable,
  formatIncrementalDate,
  incrementalStatusColor,
  incrementalStatusText,
  pausable,
  preparable,
  resumable,
} from '@/utils/incrementalJob'

type PageSize = 20 | 50 | 100
type ActionValue = 'detail' | 'pause' | 'resume' | 'ack' | 'prepare' | 'cancel' | 'complete' | 'abort'
interface RowAction { value: ActionValue; label: string; type?: 'primary' | 'secondary' }

const props = defineProps<{ active: boolean }>()
const route = useRoute()
const router = useRouter()
const jobs = ref<IncrementalJob[]>([])
const loading = ref(false)
const total = ref(0)
const keywordInput = ref('')
const keyword = ref('')
const statusFilter = ref('')
const page = ref(1)
const pageSize = ref<PageSize>(20)
const lastUpdated = ref<Date | null>(null)

let keywordTimer: number | undefined
let pollTimer: number | undefined
let requestSerial = 0
let disposed = false
let initialized = false

const statusOptions: TaskStatusOption[] = [
  { value: 'active', label: '运行中' },
  { value: 'attention', label: '需要处理' },
  { value: 'completed', label: '已完成' },
  { value: 'aborted', label: '已放弃' },
]

const activeStatuses = new Set([
  'initializing', 'snapshot', 'catching_up', 'running', 'reconnecting', 'pausing', 'cutting_over', 'validating',
])

const attentionStatuses = new Set([
  'paused_manual', 'paused_restart', 'paused_ddl', 'paused_row_conflict', 'paused_bootstrap_review',
  'ready_to_cutover', 'ready_with_warnings', 'cutover_blocked', 'failed',
])

const pagination = computed(() => ({
  current: page.value,
  pageSize: pageSize.value,
  total: total.value,
  showTotal: true,
  showPageSize: true,
  pageSizeOptions: [20, 50, 100],
}))

const lastUpdatedText = computed(() => lastUpdated.value?.toLocaleTimeString('zh-CN', { hour12: false }) || '')

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

function readQuery() {
  const nextKeyword = queryString(route.query.q)
  const nextStatus = statusOptions.some(option => option.value === route.query.status) ? queryString(route.query.status) : ''
  keywordInput.value = nextKeyword
  keyword.value = nextKeyword
  statusFilter.value = nextStatus
  page.value = parsePositiveInt(route.query.page, 1)
  pageSize.value = parsePageSize(route.query.page_size)
}

function queryObject() {
  const query: Record<string, string> = { tab: 'incremental' }
  if (keyword.value) query.q = keyword.value
  if (statusFilter.value) query.status = statusFilter.value
  if (page.value !== 1) query.page = String(page.value)
  if (pageSize.value !== 20) query.page_size = String(pageSize.value)
  return query
}

function syncQuery() {
  if (!props.active) return
  const next = queryObject()
  const current = Object.fromEntries(Object.entries(route.query).filter(([, value]) => typeof value === 'string')) as Record<string, string>
  if (JSON.stringify(next) !== JSON.stringify(current)) void router.replace({ path: '/history', query: next })
}

function schedulePoll() {
  if (pollTimer) window.clearTimeout(pollTimer)
  pollTimer = undefined
  if (
    !disposed && props.active && document.visibilityState === 'visible'
    && jobs.value.some(job => activeStatuses.has(job.status))
  ) pollTimer = window.setTimeout(() => void load(true), 5000)
}

async function load(silent = false) {
  if (disposed || !props.active) return
  const serial = ++requestSerial
  if (!silent) loading.value = true
  const params: TaskListQuery = {
    page: page.value,
    page_size: pageSize.value,
    keyword: keyword.value || undefined,
    status: statusFilter.value || undefined,
  }
  try {
    const { data } = await listIncrementalJobs(params)
    if (disposed || serial !== requestSerial) return
    jobs.value = data.items
    total.value = data.total
    lastUpdated.value = new Date()
    if (data.items.length === 0 && data.page > 1 && data.total > 0) {
      page.value = Math.max(1, Math.ceil(data.total / data.page_size))
      return
    }
  } catch {
    if (!silent && serial === requestSerial) Message.error('加载增量任务失败')
  } finally {
    if (serial === requestSerial) {
      loading.value = false
      schedulePoll()
    }
  }
}

async function act(action: () => Promise<unknown>, message: string) {
  try {
    await action()
    Message.success(message)
    await load(false)
	} catch (error: any) {
		Message.error(error?.response?.data?.error || '操作失败')
	}
}

function availableActions(job: IncrementalJob): RowAction[] {
  const actions: RowAction[] = []
  if (pausable(job)) actions.push({ value: 'pause', label: '暂停', type: 'secondary' })
  if (resumable(job)) actions.push({ value: 'resume', label: '恢复', type: 'primary' })
  if (job.status === 'paused_ddl') actions.push({ value: 'ack', label: '确认 DDL', type: 'primary' })
  if (job.status === 'paused_bootstrap_review') actions.push({ value: 'detail', label: '处理', type: 'primary' })
  if (preparable(job.status)) actions.push({ value: 'prepare', label: '准备切换', type: 'primary' })
  if (cancelable(job.status)) actions.push({ value: 'cancel', label: '取消切换', type: 'secondary' })
  if (completable(job.status)) actions.push({ value: 'complete', label: '完成切换', type: 'primary' })
  if (abortable(job.status)) actions.push({ value: 'abort', label: '放弃任务' })
  return actions
}

function preferredAction(job: IncrementalJob) {
  const preference: ActionValue[] = ['ack', 'complete', 'detail', 'resume', 'cancel', 'pause']
  const actions = availableActions(job)
  return preference.map(value => actions.find(action => action.value === value)).find(Boolean)
}

function primaryAction(job: IncrementalJob) {
  return preferredAction(job)
}

function secondaryActions(job: IncrementalJob) {
  const primary = preferredAction(job)
  return availableActions(job).filter(action => action.value !== primary?.value)
}

function confirmAction(title: string, content: string, action: () => Promise<unknown>, danger = false) {
  Modal.confirm({
    title,
    content,
    maskClosable: false,
    okText: '确认',
    cancelText: '取消',
    okButtonProps: danger ? { status: 'danger' } : undefined,
    onOk: action,
  })
}

function runAction(value: string, job: IncrementalJob) {
  switch (value as ActionValue) {
    case 'detail':
      openDetail(job)
      break
    case 'pause':
      void act(() => pauseIncrementalJob(job.job_id), '正在安全暂停')
      break
    case 'resume':
      void act(() => resumeIncrementalJob(job.job_id), '任务已恢复')
      break
    case 'ack':
      void act(() => acknowledgeIncrementalDDL(job.job_id), 'DDL 已确认')
      break
    case 'prepare':
      confirmAction('准备切换', '仅当整个源 MySQL 实例已停写、目标库也无业务写入时才能继续。', () => act(() => prepareIncrementalCutover(job.job_id), '已锁定最终位点，正在追赶和校验'))
      break
    case 'cancel':
      void act(() => cancelIncrementalCutover(job.job_id), '已取消切换并继续同步')
      break
    case 'complete':
      confirmAction(
        '完成切换',
        job.status === 'ready_with_warnings'
          ? `存在同步警告或 ${job.excluded_table_count || 0} 张排除表，请确认已核对并接受当前迁移范围。`
          : '请确认源库仍保持停写，并完成迁移切换。',
        () => act(() => stopIncrementalJob(job.job_id, job.status === 'ready_with_warnings', job.excluded_table_count > 0), '迁移闭环已安全完成'),
      )
      break
    case 'abort':
      confirmAction('放弃任务', '放弃后不能恢复，目标端数据不会自动删除。', () => act(() => abortIncrementalJob(job.job_id), '任务已放弃'), true)
      break
  }
}

function handlePageChange(nextPage: number) {
  page.value = nextPage
}

function handlePageSizeChange(size: number) {
  pageSize.value = size === 50 || size === 100 ? size : 20
  page.value = 1
}

function detailPath(jobID: string) {
  return `/history/incremental/${encodeURIComponent(jobID)}`
}

function openDetail(job: IncrementalJob) {
  router.push(detailPath(job.job_id))
}

function shortJobID(jobID: string) {
  return jobID.length > 10 ? `${jobID.slice(0, 8)}…` : jobID
}

function startModeText(mode: string) {
  return mode === 'full_then_cdc' ? '全量 + 增量' : '仅增量'
}

function sourceEndpoint(job: IncrementalJob): MigrationRouteEndpoint {
  return {
    dbType: job.src_db_type,
    name: job.src_conn?.name || undefined,
    host: job.src_conn?.host,
    port: job.src_conn?.port,
    database: job.src_conn?.database || job.src_database,
    username: job.src_conn?.username,
  }
}

function destinationEndpoint(job: IncrementalJob): MigrationRouteEndpoint {
  return {
    dbType: job.dst_db_type,
    name: job.dst_conn?.name || undefined,
    host: job.dst_conn?.host,
    port: job.dst_conn?.port,
    database: job.dst_conn?.database,
    username: job.dst_conn?.username,
    schema: job.target_schema,
  }
}

function progressText(job: IncrementalJob) {
  if (job.status === 'stopped') return '迁移已闭环'
  if (job.status === 'aborted') return '任务已终止'
  if (job.status.startsWith('paused')) return '等待人工处理'
  if (job.caught_up) return '已追平 · 延迟 0 秒'
  if (job.lag_seconds > 0) return `延迟 ${job.lag_seconds} 秒`
  return '正在同步状态'
}

function compactNumber(value: number) {
  if (value >= 1_000_000) return `${(value / 1_000_000).toFixed(1)}m`
  if (value >= 1_000) return `${(value / 1_000).toFixed(1)}k`
  return String(value || 0)
}

function relativeTime(value?: string) {
  if (!value) return '暂无事件'
  const seconds = Math.max(0, Math.floor((Date.now() - new Date(value).getTime()) / 1000))
  if (!Number.isFinite(seconds)) return '—'
  if (seconds < 60) return `${seconds} 秒前`
  if (seconds < 3600) return `${Math.floor(seconds / 60)} 分钟前`
  if (seconds < 86400) return `${Math.floor(seconds / 3600)} 小时前`
  return `${Math.floor(seconds / 86400)} 天前`
}

function compactDate(value?: string) {
  if (!value) return '—'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return '—'
  return date.toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hour12: false })
}

function incrementalRowClass(record: IncrementalJob) {
  return attentionStatuses.has(record.status) ? 'needs-attention' : ''
}

watch(keywordInput, value => {
  if (keywordTimer) window.clearTimeout(keywordTimer)
  keywordTimer = window.setTimeout(() => {
    keyword.value = value.trim()
    page.value = 1
  }, 300)
})

watch([keyword, statusFilter, page, pageSize], () => {
	if (!initialized || !props.active) return
  syncQuery()
  void load(false)
})

watch(() => route.query, () => {
  if (!props.active || route.query.tab !== 'incremental') return
  const nextKeyword = queryString(route.query.q)
  const nextStatus = queryString(route.query.status)
  const nextPage = parsePositiveInt(route.query.page, 1)
  const nextPageSize = parsePageSize(route.query.page_size)
  if (nextKeyword !== keyword.value || nextStatus !== statusFilter.value || nextPage !== page.value || nextPageSize !== pageSize.value) readQuery()
})

watch(() => props.active, active => {
	if (active) {
		initialized = false
		readQuery()
		initialized = true
		void load(false)
  } else if (pollTimer) {
    window.clearTimeout(pollTimer)
    pollTimer = undefined
  }
})

function handleVisibilityChange() {
  if (document.visibilityState === 'visible') {
    if (props.active) void load(true)
  } else if (pollTimer) {
    window.clearTimeout(pollTimer)
    pollTimer = undefined
  }
}

onMounted(() => {
	if (props.active) {
		readQuery()
		initialized = true
		void load(false)
	} else {
		initialized = true
	}
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
.task-table { min-width: 0; }
.task-table :deep(.arco-table-container) { overflow-x: hidden; }
.task-table :deep(.arco-table-cell) { padding: 10px 10px; }
.task-table :deep(.arco-table-pagination) { margin: 12px 0 0; }
.task-identity,
.status-cell,
.activity-cell {
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
.progress-note { color: var(--fg-muted); font-size: 11px; white-space: nowrap; }
.progress-note.caught { color: #15803d; }
.event-stats {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 4px 10px;
  color: var(--fg-secondary);
  font-family: var(--font-mono);
  font-size: 11px;
  cursor: default;
}
.event-stats span { display: flex; gap: 4px; min-width: 0; }
.event-stats b { color: #165dff; font-weight: 600; }
.event-stats .skipped b { color: var(--fg-muted); }
.activity-cell { gap: 2px; font-size: 12px; cursor: default; }
.activity-date { color: var(--fg-muted); font-size: 11px; }
.row-actions { display: flex; align-items: center; justify-content: flex-end; gap: 6px; white-space: nowrap; }
.no-action { color: var(--fg-muted); }
.danger-option { color: var(--destructive); }
.incremental-table :deep(.needs-attention .arco-table-td:first-child) { box-shadow: inset 3px 0 0 #f59e0b; }
.incremental-table :deep(.needs-attention .arco-table-td) { background: rgba(245, 158, 11, 0.025); }
</style>
