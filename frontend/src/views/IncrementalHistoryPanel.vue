<template>
  <div class="toolbar"><a-button :loading="loading" @click="load"><template #icon><icon-refresh /></template>刷新</a-button></div>
  <a-table :data="jobs" :loading="loading" row-key="id" :pagination="{ pageSize: 20 }" :scroll="{ x: 1450 }">
    <template #columns>
      <a-table-column title="Job ID" :width="100"><template #cell="{ record }"><a-tooltip :content="record.job_id"><code>{{ record.job_id.slice(0, 6) }}…</code></a-tooltip></template></a-table-column>
      <a-table-column title="源库" data-index="src_database" :width="150" />
      <a-table-column title="目标 Schema" data-index="target_schema" :width="150" />
      <a-table-column title="模式" :width="120"><template #cell="{ record }">{{ record.start_mode === 'full_then_cdc' ? '全量 + 增量' : '仅增量' }}</template></a-table-column>
      <a-table-column title="状态" :width="130"><template #cell="{ record }"><a-tag :color="color(record.status)">{{ text(record.status) }}</a-tag></template></a-table-column>
      <a-table-column title="同步" :width="100"><template #cell="{ record }"><a-tag :color="record.caught_up ? 'green' : 'orange'">{{ record.caught_up ? '已追平' : '追赶中' }}</a-tag></template></a-table-column>
      <a-table-column title="I / U / D / 跳过" :width="180"><template #cell="{ record }">{{ record.insert_count }} / {{ record.update_count }} / {{ record.delete_count }} / {{ record.skipped_count }}</template></a-table-column>
      <a-table-column title="最后事件" :width="170"><template #cell="{ record }">{{ record.last_event_at ? date(record.last_event_at) : '—' }}</template></a-table-column>
      <a-table-column title="操作" :width="360" fixed="right">
        <template #cell="{ record }">
          <a-space>
            <a-button size="mini" @click="openDetail(record)">详情</a-button>
            <a-button v-if="pausable(record)" size="mini" @click="pause(record)">暂停</a-button>
            <a-button v-if="resumable(record)" size="mini" @click="resume(record)">恢复</a-button>
            <a-button v-if="record.status === 'paused_ddl'" size="mini" type="primary" @click="ack(record)">确认 DDL</a-button>
            <a-popconfirm v-if="preparable(record.status)" content="仅当整个源 MySQL 实例已停写、目标库也无业务写入时才能继续。确认？" @ok="prepare(record)">
              <a-button size="mini" type="primary">准备切换</a-button>
            </a-popconfirm>
            <a-button v-if="cancelable(record.status)" size="mini" @click="cancelCutover(record)">取消切换</a-button>
            <a-popconfirm v-if="completable(record.status)" :content="record.status === 'ready_with_warnings' ? '存在被跳过的无主键变更，确认已核对并接受风险？' : '确认源库仍保持停写并完成迁移？'" @ok="complete(record)">
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

  <a-drawer v-model:visible="drawer" title="增量任务详情" :width="820" @close="detail = null">
    <template v-if="detail">
      <a-descriptions :column="2" bordered>
        <a-descriptions-item label="Job ID" :span="2">{{ detail.job_id }}</a-descriptions-item>
        <a-descriptions-item label="状态">{{ text(detail.status) }}</a-descriptions-item>
        <a-descriptions-item label="阶段">{{ detail.phase }}</a-descriptions-item>
        <a-descriptions-item label="全量完成">{{ detail.bootstrap_completed ? '是' : '否' }}</a-descriptions-item>
        <a-descriptions-item label="同步状态">{{ detail.caught_up ? '已追平' : `追赶中（约 ${detail.lag_seconds || 0} 秒）` }}</a-descriptions-item>
        <a-descriptions-item label="目标 checkpoint" :span="2">{{ position(detail.checkpoint_file, detail.checkpoint_position, detail.checkpoint_gtid) }}</a-descriptions-item>
        <a-descriptions-item label="源端最新位点" :span="2">{{ position(detail.source_head_file, detail.source_head_position, detail.source_head_gtid) }}</a-descriptions-item>
        <a-descriptions-item v-if="detail.cutover_file || detail.cutover_gtid" label="切换边界" :span="2">{{ position(detail.cutover_file, detail.cutover_position, detail.cutover_gtid) }}</a-descriptions-item>
        <a-descriptions-item label="摘要" :span="2">{{ detail.summary || '—' }}</a-descriptions-item>
      </a-descriptions>
      <a-alert v-if="unsafeBootstrap(detail)" type="error" style="margin-top: 12px">
        SQLite 尚未记录全量完成。恢复时会先查目标 checkpoint：有完成位点才会续跑，否则拒绝恢复，不会自动删表重跑。
      </a-alert>
      <a-alert v-if="detail.last_error" type="error" style="margin-top: 12px">{{ detail.last_error }}</a-alert>
      <template v-if="detail.blocking_ddl"><a-divider>待处理 DDL</a-divider><pre class="ddl">{{ detail.blocking_ddl }}</pre></template>
      <template v-if="validation(detail).length">
        <a-divider>最终行数校验</a-divider>
        <a-table :data="validation(detail)" size="small" :pagination="false" :scroll="{ y: 300 }">
          <template #columns>
            <a-table-column title="表" data-index="table" />
            <a-table-column title="源库" data-index="source" :width="120" />
            <a-table-column title="目标库" data-index="target" :width="120" />
            <a-table-column title="结果" :width="100"><template #cell="{ record }"><a-tag :color="record.match ? 'green' : 'red'">{{ record.match ? '一致' : '不一致' }}</a-tag></template></a-table-column>
            <a-table-column title="错误" data-index="error" />
          </template>
        </a-table>
      </template>
    </template>
  </a-drawer>
</template>

<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue'
import { Message } from '@arco-design/web-vue'
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

interface ValidationRow { table: string; source: number; target: number; match: boolean; error?: string }

const jobs = ref<IncrementalJob[]>([])
const loading = ref(false)
const drawer = ref(false)
const detail = ref<IncrementalJob | null>(null)
let timer: number | undefined

async function load() {
  loading.value = true
  try {
    jobs.value = (await listIncrementalJobs()).data
    if (detail.value) detail.value = jobs.value.find(job => job.job_id === detail.value?.job_id) || detail.value
  } catch {
    Message.error('加载增量任务失败')
  } finally {
    loading.value = false
  }
}
async function act(action: () => Promise<unknown>, message: string) {
  try {
    await action()
    Message.success(message)
    await load()
  } catch (e: any) {
    Message.error(e?.response?.data?.error || '操作失败')
  }
}
const pause = (job: IncrementalJob) => act(() => pauseIncrementalJob(job.job_id), '正在安全暂停')
const resume = (job: IncrementalJob) => act(() => resumeIncrementalJob(job.job_id), '任务已恢复')
const ack = (job: IncrementalJob) => act(() => acknowledgeIncrementalDDL(job.job_id), 'DDL 已确认')
const prepare = (job: IncrementalJob) => act(() => prepareIncrementalCutover(job.job_id), '已锁定最终位点，正在追赶和校验')
const cancelCutover = (job: IncrementalJob) => act(() => cancelIncrementalCutover(job.job_id), '已取消切换并继续同步')
const complete = (job: IncrementalJob) => act(() => stopIncrementalJob(job.job_id, job.status === 'ready_with_warnings'), '迁移闭环已安全完成')
const abort = (job: IncrementalJob) => act(() => abortIncrementalJob(job.job_id), '任务已放弃')

function openDetail(job: IncrementalJob) { detail.value = job; drawer.value = true }
function pausable(job: IncrementalJob) { return ['catching_up', 'running', 'reconnecting'].includes(job.status) }
function unsafeBootstrap(job: IncrementalJob) { return job.start_mode === 'full_then_cdc' && !job.bootstrap_completed && ['paused_restart', 'failed'].includes(job.status) }
function resumable(job: IncrementalJob) { return ['paused_manual', 'paused_restart', 'failed'].includes(job.status) }
function preparable(status: string) { return ['running', 'catching_up'].includes(status) }
function cancelable(status: string) { return ['cutting_over', 'ready_to_cutover', 'ready_with_warnings', 'cutover_blocked'].includes(status) }
function completable(status: string) { return ['ready_to_cutover', 'ready_with_warnings'].includes(status) }
function abortable(status: string) { return !['validating', 'stopped', 'aborted'].includes(status) }
function validation(job: IncrementalJob): ValidationRow[] {
  if (!job.validation_json) return []
  try {
    const rows = JSON.parse(job.validation_json)
    return Array.isArray(rows) ? rows : []
  } catch {
    return []
  }
}
function position(file: string, pos: number, gtid: string) {
  const filePos = file ? `${file}:${pos || 0}` : '—'
  return gtid ? `${filePos} · GTID ${gtid}` : filePos
}

const labels: Record<string, string> = {
  initializing: '初始化', snapshot: '全量快照', catching_up: '追赶', running: '运行中', reconnecting: '重连中',
  pausing: '暂停中', paused_manual: '已暂停', paused_restart: '重启后暂停', paused_ddl: 'DDL 暂停',
  cutting_over: '追赶切换边界', validating: '最终校验', ready_to_cutover: '可完成切换',
  ready_with_warnings: '带风险待确认', cutover_blocked: '切换受阻', stopped: '已完成', aborted: '已放弃', failed: '失败',
}
const text = (status: string) => labels[status] || status
function color(status: string) {
  if (['running', 'ready_to_cutover', 'stopped'].includes(status)) return 'green'
  if (['failed', 'cutover_blocked'].includes(status)) return 'red'
  if (status.startsWith('paused') || status === 'ready_with_warnings') return 'orange'
  if (status === 'aborted') return 'gray'
  return 'blue'
}
const date = (value: string) => new Date(value).toLocaleString('zh-CN')

onMounted(() => { load(); timer = window.setInterval(load, 5000) })
onUnmounted(() => { if (timer) clearInterval(timer) })
</script>

<style scoped>
.toolbar { display: flex; justify-content: flex-end; margin-bottom: 16px; }
.ddl { padding: 12px; background: #f2f3f5; border-radius: 4px; white-space: pre-wrap; }
</style>
