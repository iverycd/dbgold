<template>
  <div class="toolbar">
    <a-button :loading="loading" @click="load">
      <template #icon><icon-refresh /></template>
      刷新
    </a-button>
  </div>

  <a-table :data="jobs" :loading="loading" row-key="id" :pagination="{ pageSize: 20 }" :scroll="{ x: 1800 }">
    <template #columns>
      <a-table-column title="Job ID" :width="100">
        <template #cell="{ record }">
          <a-tooltip :content="record.job_id"><code>{{ record.job_id.slice(0, 6) }}…</code></a-tooltip>
        </template>
      </a-table-column>
      <a-table-column title="源连接" :width="290">
        <template #cell="{ record }">
          <div class="history-conn-cell">
            <a-tag v-if="record.src_db_type" :color="getDbTypeColor(record.src_db_type)" size="small">{{ getDbTypeLabel(record.src_db_type) }}</a-tag>
            <a-tooltip v-if="record.src_conn" :content="record.src_conn.name" mini>
              <span class="conn-name">{{ record.src_conn.name }}</span>
            </a-tooltip>
            <span v-else class="conn-deleted">连接已删除</span>
          </div>
          <div v-if="record.src_conn" class="conn-detail">
            <span class="conn-label">地址</span>
            <a-tooltip :content="connectionEndpoint(record.src_conn)" mini>
              <span class="conn-detail-val address-val">{{ connectionEndpoint(record.src_conn) }}</span>
            </a-tooltip>
          </div>
          <div class="conn-detail">
            <span class="conn-label">库</span>
            <a-tooltip :content="record.src_conn?.database || record.src_database" mini>
              <span class="conn-detail-val">{{ record.src_conn?.database || record.src_database || '—' }}</span>
            </a-tooltip>
            <template v-if="record.src_conn?.username">
              <span class="conn-detail-sep">·</span><span class="conn-label">账号</span>
              <a-tooltip :content="record.src_conn.username" mini><span class="conn-detail-val">{{ record.src_conn.username }}</span></a-tooltip>
            </template>
          </div>
        </template>
      </a-table-column>
      <a-table-column title="目标连接" :width="320">
        <template #cell="{ record }">
          <div class="history-conn-cell">
            <a-tag v-if="record.dst_db_type" :color="getDbTypeColor(record.dst_db_type)" size="small">{{ getDbTypeLabel(record.dst_db_type) }}</a-tag>
            <a-tooltip v-if="record.dst_conn" :content="record.dst_conn.name" mini><span class="conn-name">{{ record.dst_conn.name }}</span></a-tooltip>
            <span v-else class="conn-deleted">连接已删除</span>
          </div>
          <div v-if="record.dst_conn" class="conn-detail">
            <span class="conn-label">地址</span>
            <a-tooltip :content="connectionEndpoint(record.dst_conn)" mini><span class="conn-detail-val address-val">{{ connectionEndpoint(record.dst_conn) }}</span></a-tooltip>
          </div>
          <div class="conn-detail">
            <template v-if="record.dst_conn?.database">
              <span class="conn-label">库</span>
              <a-tooltip :content="record.dst_conn.database" mini><span class="conn-detail-val">{{ record.dst_conn.database }}</span></a-tooltip>
              <span class="conn-detail-sep">·</span>
            </template>
            <span class="conn-label">Schema</span>
            <a-tooltip :content="record.target_schema" mini><span class="conn-detail-val schema-val">{{ record.target_schema || '—' }}</span></a-tooltip>
            <template v-if="record.dst_conn?.username">
              <span class="conn-detail-sep">·</span><span class="conn-label">账号</span>
              <a-tooltip :content="record.dst_conn.username" mini><span class="conn-detail-val">{{ record.dst_conn.username }}</span></a-tooltip>
            </template>
          </div>
        </template>
      </a-table-column>
      <a-table-column title="模式" :width="120">
        <template #cell="{ record }">{{ record.start_mode === 'full_then_cdc' ? '全量 + 增量' : '仅增量' }}</template>
      </a-table-column>
      <a-table-column title="状态" :width="150">
        <template #cell="{ record }">
          <a-tag :color="incrementalStatusColor(record.status)">{{ record.locator_strategy_version !== 1 ? '版本升级后已废弃' : incrementalStatusText(record.status) }}</a-tag>
        </template>
      </a-table-column>
      <a-table-column title="同步" :width="100">
        <template #cell="{ record }"><a-tag :color="record.caught_up ? 'green' : 'orange'">{{ record.caught_up ? '已追平' : '追赶中' }}</a-tag></template>
      </a-table-column>
      <a-table-column title="I / U / D / 跳过" :width="180">
        <template #cell="{ record }">{{ record.insert_count }} / {{ record.update_count }} / {{ record.delete_count }} / {{ record.skipped_count }}</template>
      </a-table-column>
      <a-table-column title="最后事件" :width="170">
        <template #cell="{ record }">{{ formatIncrementalDate(record.last_event_at) }}</template>
      </a-table-column>
      <a-table-column title="操作" :width="360" fixed="right">
        <template #cell="{ record }">
          <a-space>
            <a-button size="mini" type="primary" @click="openDetail(record)">详情</a-button>
            <a-button v-if="pausable(record)" size="mini" @click="pause(record)">暂停</a-button>
            <a-button v-if="resumable(record)" size="mini" @click="resume(record)">恢复</a-button>
            <a-button v-if="record.status === 'paused_ddl'" size="mini" type="primary" @click="ack(record)">确认 DDL</a-button>
            <a-popconfirm v-if="preparable(record.status)" content="仅当整个源 MySQL 实例已停写、目标库也无业务写入时才能继续。确认？" @ok="prepare(record)">
              <a-button size="mini" type="primary">准备切换</a-button>
            </a-popconfirm>
            <a-button v-if="cancelable(record.status)" size="mini" @click="cancelCutover(record)">取消切换</a-button>
            <a-popconfirm v-if="completable(record.status)" :content="record.status === 'ready_with_warnings' ? `存在同步警告或 ${record.excluded_table_count || 0} 张排除表，确认已核对并接受当前迁移范围？` : '确认源库仍保持停写并完成迁移？'" @ok="complete(record)">
              <a-button size="mini" type="primary" status="success">完成切换</a-button>
            </a-popconfirm>
            <a-popconfirm v-if="abortable(record.status)" content="放弃后不能恢复，目标端数据不会自动删除。确认放弃？" @ok="abort(record)">
              <a-button size="mini" status="danger">放弃</a-button>
            </a-popconfirm>
          </a-space>
        </template>
      </a-table-column>
    </template>
  </a-table>
</template>

<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { Message } from '@arco-design/web-vue'
import { getDbTypeColor, getDbTypeLabel } from '@/utils/dbType'
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
} from '@/api/migration'
import {
  abortable,
  cancelable,
  completable,
  connectionEndpoint,
  formatIncrementalDate,
  incrementalStatusColor,
  incrementalStatusText,
  pausable,
  preparable,
  resumable,
} from '@/utils/incrementalJob'

const router = useRouter()
const jobs = ref<IncrementalJob[]>([])
const loading = ref(false)
let timer: number | undefined
let loadRunning = false
let loadPending = false
let disposed = false

async function load() {
  if (disposed) return
  if (loadRunning) {
    loadPending = true
    return
  }
  loadRunning = true
  loading.value = true
  try {
    do {
      loadPending = false
      const nextJobs = (await listIncrementalJobs()).data
      if (!disposed) jobs.value = nextJobs
    } while (loadPending && !disposed)
  } catch {
    Message.error('加载增量任务失败')
  } finally {
    loading.value = false
    loadRunning = false
    if (!disposed && loadPending) void load()
  }
}

async function act(action: () => Promise<unknown>, message: string) {
  try {
    await action()
    Message.success(message)
    await load()
  } catch (error: any) {
    Message.error(error?.response?.data?.error || '操作失败')
  }
}

function openDetail(job: IncrementalJob) {
  router.push(`/history/incremental/${encodeURIComponent(job.job_id)}`)
}

const pause = (job: IncrementalJob) => act(() => pauseIncrementalJob(job.job_id), '正在安全暂停')
const resume = (job: IncrementalJob) => act(() => resumeIncrementalJob(job.job_id), '任务已恢复')
const ack = (job: IncrementalJob) => act(() => acknowledgeIncrementalDDL(job.job_id), 'DDL 已确认')
const prepare = (job: IncrementalJob) => act(() => prepareIncrementalCutover(job.job_id), '已锁定最终位点，正在追赶和校验')
const cancelCutover = (job: IncrementalJob) => act(() => cancelIncrementalCutover(job.job_id), '已取消切换并继续同步')
const complete = (job: IncrementalJob) => act(() => stopIncrementalJob(job.job_id, job.status === 'ready_with_warnings', job.excluded_table_count > 0), '迁移闭环已安全完成')
const abort = (job: IncrementalJob) => act(() => abortIncrementalJob(job.job_id), '任务已放弃')

onMounted(() => {
  void load()
  timer = window.setInterval(load, 5000)
})
onUnmounted(() => {
  disposed = true
  if (timer) clearInterval(timer)
})
</script>

<style scoped>
.toolbar { display: flex; justify-content: flex-end; margin-bottom: 16px; }
.history-conn-cell { display: flex; align-items: center; gap: 6px; min-width: 0; }
.conn-name { max-width: 190px; overflow: hidden; color: var(--fg-primary); font-size: 13px; font-weight: 500; text-overflow: ellipsis; white-space: nowrap; cursor: default; }
.conn-deleted { color: #86909c; font-size: 12px; }
.conn-detail { display: flex; align-items: center; gap: 3px; min-width: 0; margin-top: 3px; color: var(--fg-muted); font-size: 11px; }
.conn-label { flex-shrink: 0; color: var(--fg-muted); }
.conn-detail-val { max-width: 76px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; cursor: default; }
.address-val { max-width: 225px; }
.schema-val { max-width: 76px; color: #165dff; font-weight: 500; }
.conn-detail-sep { flex-shrink: 0; color: var(--border-strong); }
</style>
