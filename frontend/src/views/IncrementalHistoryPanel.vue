<template>
  <div class="toolbar"><a-button :loading="loading" @click="load"><template #icon><icon-refresh /></template>刷新</a-button></div>
  <a-table :data="jobs" :loading="loading" row-key="id" :pagination="{ pageSize: 20 }" :scroll="{ x: 1800 }">
    <template #columns>
      <a-table-column title="Job ID" :width="100"><template #cell="{ record }"><a-tooltip :content="record.job_id"><code>{{ record.job_id.slice(0, 6) }}…</code></a-tooltip></template></a-table-column>
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
            <a-tooltip :content="endpoint(record.src_conn)" mini>
              <span class="conn-detail-val address-val">{{ endpoint(record.src_conn) }}</span>
            </a-tooltip>
          </div>
          <div class="conn-detail">
            <span class="conn-label">库</span>
            <a-tooltip :content="record.src_conn?.database || record.src_database" mini>
              <span class="conn-detail-val">{{ record.src_conn?.database || record.src_database || '—' }}</span>
            </a-tooltip>
            <template v-if="record.src_conn?.username">
              <span class="conn-detail-sep">·</span>
              <span class="conn-label">账号</span>
              <a-tooltip :content="record.src_conn.username" mini>
                <span class="conn-detail-val">{{ record.src_conn.username }}</span>
              </a-tooltip>
            </template>
          </div>
        </template>
      </a-table-column>
      <a-table-column title="目标连接" :width="320">
        <template #cell="{ record }">
          <div class="history-conn-cell">
            <a-tag v-if="record.dst_db_type" :color="getDbTypeColor(record.dst_db_type)" size="small">{{ getDbTypeLabel(record.dst_db_type) }}</a-tag>
            <a-tooltip v-if="record.dst_conn" :content="record.dst_conn.name" mini>
              <span class="conn-name">{{ record.dst_conn.name }}</span>
            </a-tooltip>
            <span v-else class="conn-deleted">连接已删除</span>
          </div>
          <div v-if="record.dst_conn" class="conn-detail">
            <span class="conn-label">地址</span>
            <a-tooltip :content="endpoint(record.dst_conn)" mini>
              <span class="conn-detail-val address-val">{{ endpoint(record.dst_conn) }}</span>
            </a-tooltip>
          </div>
          <div class="conn-detail">
            <template v-if="record.dst_conn?.database">
              <span class="conn-label">库</span>
              <a-tooltip :content="record.dst_conn.database" mini>
                <span class="conn-detail-val">{{ record.dst_conn.database }}</span>
              </a-tooltip>
              <span class="conn-detail-sep">·</span>
            </template>
            <span class="conn-label">Schema</span>
            <a-tooltip :content="record.target_schema" mini>
              <span class="conn-detail-val schema-val">{{ record.target_schema || '—' }}</span>
            </a-tooltip>
            <template v-if="record.dst_conn?.username">
              <span class="conn-detail-sep">·</span>
              <span class="conn-label">账号</span>
              <a-tooltip :content="record.dst_conn.username" mini>
                <span class="conn-detail-val">{{ record.dst_conn.username }}</span>
              </a-tooltip>
            </template>
          </div>
        </template>
      </a-table-column>
      <a-table-column title="模式" :width="120"><template #cell="{ record }">{{ record.start_mode === 'full_then_cdc' ? '全量 + 增量' : '仅增量' }}</template></a-table-column>
      <a-table-column title="状态" :width="150"><template #cell="{ record }"><a-tag :color="color(record.status)">{{ record.locator_strategy_version !== 1 ? '版本升级后已废弃' : text(record.status) }}</a-tag></template></a-table-column>
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

  <a-drawer v-model:visible="drawer" title="增量任务详情" :width="1000" @close="closeDetail">
    <template v-if="detail">
      <div class="route-summary">
        <div class="route-row">
          <span class="route-label">源连接</span>
          <a-tag v-if="detail.src_db_type" :color="getDbTypeColor(detail.src_db_type)" size="small">{{ getDbTypeLabel(detail.src_db_type) }}</a-tag>
          <template v-if="detail.src_conn">
            <a-tooltip :content="detail.src_conn.name" mini><strong class="route-name">{{ detail.src_conn.name }}</strong></a-tooltip>
            <a-tooltip :content="endpoint(detail.src_conn)" mini><span class="route-endpoint">{{ endpoint(detail.src_conn) }}</span></a-tooltip>
            <span class="route-meta">数据库：{{ detail.src_conn.database }}　账号：{{ detail.src_conn.username }}</span>
          </template>
          <template v-else>
            <span class="conn-deleted">连接已删除</span>
            <span class="route-meta">数据库：{{ detail.src_database || '—' }}</span>
          </template>
        </div>
        <div class="route-row">
          <span class="route-label">目标连接</span>
          <a-tag v-if="detail.dst_db_type" :color="getDbTypeColor(detail.dst_db_type)" size="small">{{ getDbTypeLabel(detail.dst_db_type) }}</a-tag>
          <template v-if="detail.dst_conn">
            <a-tooltip :content="detail.dst_conn.name" mini><strong class="route-name">{{ detail.dst_conn.name }}</strong></a-tooltip>
            <a-tooltip :content="endpoint(detail.dst_conn)" mini><span class="route-endpoint">{{ endpoint(detail.dst_conn) }}</span></a-tooltip>
            <span class="route-meta">数据库：{{ detail.dst_conn.database }}　账号：{{ detail.dst_conn.username }}</span>
          </template>
          <span v-else class="conn-deleted">连接已删除</span>
          <a-tooltip :content="detail.target_schema" mini>
            <span class="route-schema">Schema：{{ detail.target_schema || '—' }}</span>
          </a-tooltip>
        </div>
      </div>
      <a-descriptions :column="2" bordered>
        <a-descriptions-item label="Job ID" :span="2">{{ detail.job_id }}</a-descriptions-item>
        <a-descriptions-item label="状态">{{ text(detail.status) }}</a-descriptions-item>
        <a-descriptions-item label="阶段">{{ detail.phase }}</a-descriptions-item>
        <a-descriptions-item label="全量完成">{{ detail.bootstrap_completed ? '是' : '否' }}</a-descriptions-item>
        <a-descriptions-item label="有效 / 排除表">{{ detail.effective_table_count || 0 }} / {{ detail.excluded_table_count || 0 }}</a-descriptions-item>
        <a-descriptions-item label="定位策略（主键 / 唯一 / 整行）">{{ detail.primary_locator_count || 0 }} / {{ detail.unique_locator_count || 0 }} / {{ detail.full_row_locator_count || 0 }}</a-descriptions-item>
        <a-descriptions-item v-if="detail.failed_object_count > 0" label="失败对象 / 含 DDL">{{ detail.failed_object_count }} / {{ detail.failed_ddl_count }}</a-descriptions-item>
        <a-descriptions-item label="同步状态">{{ detail.caught_up ? '已追平' : `追赶中（约 ${detail.lag_seconds || 0} 秒）` }}</a-descriptions-item>
        <a-descriptions-item label="目标 checkpoint" :span="2">{{ position(detail.checkpoint_file, detail.checkpoint_position, detail.checkpoint_gtid) }}</a-descriptions-item>
        <a-descriptions-item label="源端最新位点" :span="2">{{ position(detail.source_head_file, detail.source_head_position, detail.source_head_gtid) }}</a-descriptions-item>
        <a-descriptions-item v-if="detail.cutover_file || detail.cutover_gtid" label="切换边界" :span="2">{{ position(detail.cutover_file, detail.cutover_position, detail.cutover_gtid) }}</a-descriptions-item>
        <a-descriptions-item label="摘要" :span="2">{{ detail.summary || '—' }}</a-descriptions-item>
      </a-descriptions>
      <div v-if="canExportFailedDDL" style="margin-top: 10px; display: flex; align-items: center; gap: 10px">
        <a-button size="small" :loading="exportingFailedDDL" @click="exportFailedDDL">
          <template #icon><icon-download /></template>
          导出修复 SQL
        </a-button>
        <span style="color: var(--color-text-3); font-size: 12px">文件包含可执行语句，DROP 等破坏性操作默认禁用</span>
      </div>
      <IncrementalMigrationLogPanel
        v-if="detail.start_mode === 'full_then_cdc'"
        :key="detail.job_id"
        :jobID="detail.job_id"
        :polling="bootstrapLogPolling(detail)"
        :refresh-token="logRefreshToken"
      />
      <a-alert v-if="unsafeBootstrap(detail)" type="error" style="margin-top: 12px">
        SQLite 尚未记录全量完成。恢复时会先查目标 checkpoint：有完成位点才会续跑，否则拒绝恢复，不会自动删表重跑。
      </a-alert>
      <a-alert v-if="detail.locator_strategy_version !== 1" type="warning" style="margin-top: 12px">
        CDC 定位策略已经升级，此旧任务已废弃且不能恢复；目标表、checkpoint、日志和迁移数据均未自动删除。
      </a-alert>
      <a-alert v-if="detail.last_error" type="error" style="margin-top: 12px">{{ detail.last_error }}</a-alert>
      <a-alert v-if="detail.status === 'paused_row_conflict'" type="error" style="margin-top: 12px">
        表 {{ detail.conflict_table }} 的 {{ detail.conflict_action.toUpperCase() }} 无法定位更新前记录；位点
        {{ position(detail.conflict_file, detail.conflict_position, detail.conflict_gtid) }}，旧行摘要 {{ detail.conflict_before_hash }}。
        修复目标数据后可恢复，checkpoint 尚未推进。
      </a-alert>
      <template v-if="bootstrapReview">
        <a-divider>全量迁移范围</a-divider>
        <a-alert v-if="detail.status === 'paused_bootstrap_review'" type="warning">
          全量存在失败表，确认排除前不会启动 CDC。请确保原始快照位点的 binlog 仍被保留。
        </a-alert>
        <a-descriptions :column="3" size="small" style="margin-top: 10px">
          <a-descriptions-item label="原始范围">{{ bootstrapReview.requested_count }}</a-descriptions-item>
          <a-descriptions-item label="成功表">{{ bootstrapReview.effective_tables.length }}</a-descriptions-item>
          <a-descriptions-item label="排除表">{{ bootstrapReview.excluded_tables.length }}</a-descriptions-item>
          <a-descriptions-item label="失败对象 / 含 DDL">{{ bootstrapReview.failed_objects?.length || detail.failed_object_count || 0 }} / {{ bootstrapReview.failed_objects?.filter(item => !!item.ddl?.trim()).length || detail.failed_ddl_count || 0 }}</a-descriptions-item>
          <a-descriptions-item label="原始快照位点" :span="3">{{ position(bootstrapReview.position.file, bootstrapReview.position.position, bootstrapReview.position.gtid) }}</a-descriptions-item>
        </a-descriptions>
        <a-table :data="bootstrapReview.excluded_tables" size="small" :pagination="false" :scroll="{ y: 260 }">
          <template #columns>
            <a-table-column title="表" data-index="table" :width="170" />
            <a-table-column title="阶段" :width="120"><template #cell="{ record }">{{ bootstrapStageText(record.stage) }}</template></a-table-column>
            <a-table-column title="错误" data-index="error" />
            <a-table-column title="DDL" :width="90">
              <template #cell="{ record }">
                <a-popover v-if="record.ddl" title="失败 DDL">
                  <a-button size="mini">查看</a-button>
                  <template #content><pre class="ddl ddl-popover">{{ record.ddl }}</pre></template>
                </a-popover>
                <span v-else>—</span>
              </template>
            </a-table-column>
          </template>
        </a-table>
        <a-alert v-for="warning in bootstrapReview.warnings" :key="warning" type="warning" style="margin-top: 8px">{{ warning }}</a-alert>
        <div v-if="detail.status === 'paused_bootstrap_review'" class="review-confirm">
          <a-checkbox v-model="reviewAccepted">我确认排除以上失败表，仅同步成功表</a-checkbox>
          <a-button type="primary" :loading="acceptingReview" :disabled="!reviewAccepted" @click="acceptReview">接受排除并继续</a-button>
        </div>
      </template>
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
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { Message } from '@arco-design/web-vue'
import IncrementalMigrationLogPanel from '@/components/IncrementalMigrationLogPanel.vue'
import { getDbTypeColor, getDbTypeLabel } from '@/utils/dbType'
import {
  acceptIncrementalBootstrapExclusions,
  abortIncrementalJob,
  acknowledgeIncrementalDDL,
  cancelIncrementalCutover,
  getIncrementalBootstrapReview,
  downloadIncrementalFailedDDL,
  listIncrementalJobs,
  pauseIncrementalJob,
  prepareIncrementalCutover,
  resumeIncrementalJob,
  stopIncrementalJob,
  type IncrementalJob,
  type BootstrapReview,
} from '@/api/migration'

interface ValidationRow { table: string; source: number; target: number; match: boolean; error?: string }

const jobs = ref<IncrementalJob[]>([])
const loading = ref(false)
const drawer = ref(false)
const detail = ref<IncrementalJob | null>(null)
const bootstrapReview = ref<BootstrapReview | null>(null)
const reviewAccepted = ref(false)
const acceptingReview = ref(false)
const exportingFailedDDL = ref(false)
const logRefreshToken = ref(0)
let timer: number | undefined
let loadRunning = false
let loadPending = false
let disposed = false
const canExportFailedDDL = computed(() => !!detail.value && detail.value.start_mode === 'full_then_cdc' && (
  detail.value.failed_object_count > 0 || (bootstrapReview.value?.excluded_tables.length || 0) > 0 || (bootstrapReview.value?.warnings.length || 0) > 0
))

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
      const detailJobID = detail.value?.job_id
      const nextJobs = (await listIncrementalJobs()).data
      if (disposed) return
      jobs.value = nextJobs
      if (detailJobID && detail.value?.job_id === detailJobID) {
        detail.value = nextJobs.find(job => job.job_id === detailJobID) || detail.value
        await loadBootstrapReview(detail.value)
      }
    } while (loadPending)
  } catch {
    Message.error('加载增量任务失败')
  } finally {
    loading.value = false
    loadRunning = false
    if (!disposed && loadPending) void load()
  }
}
async function act(jobID: string, action: () => Promise<unknown>, message: string) {
  try {
    await action()
    if (detail.value?.job_id === jobID) logRefreshToken.value++
    Message.success(message)
    await load()
  } catch (e: any) {
    Message.error(e?.response?.data?.error || '操作失败')
  }
}
const pause = (job: IncrementalJob) => act(job.job_id, () => pauseIncrementalJob(job.job_id), '正在安全暂停')
const resume = (job: IncrementalJob) => act(job.job_id, () => resumeIncrementalJob(job.job_id), '任务已恢复')
const ack = (job: IncrementalJob) => act(job.job_id, () => acknowledgeIncrementalDDL(job.job_id), 'DDL 已确认')
const prepare = (job: IncrementalJob) => act(job.job_id, () => prepareIncrementalCutover(job.job_id), '已锁定最终位点，正在追赶和校验')
const cancelCutover = (job: IncrementalJob) => act(job.job_id, () => cancelIncrementalCutover(job.job_id), '已取消切换并继续同步')
const complete = (job: IncrementalJob) => act(job.job_id, () => stopIncrementalJob(job.job_id, job.status === 'ready_with_warnings', job.excluded_table_count > 0), '迁移闭环已安全完成')
const abort = (job: IncrementalJob) => act(job.job_id, () => abortIncrementalJob(job.job_id), '任务已放弃')

async function loadBootstrapReview(job: IncrementalJob) {
  if (job.status !== 'paused_bootstrap_review' && job.excluded_table_count <= 0) {
    bootstrapReview.value = null
    return
  }
  const jobID = job.job_id
  try {
    const review = (await getIncrementalBootstrapReview(jobID)).data
    if (!disposed && detail.value?.job_id === jobID) bootstrapReview.value = review
  } catch {
    if (!disposed && detail.value?.job_id === jobID) bootstrapReview.value = null
  }
}
async function openDetail(job: IncrementalJob) {
  detail.value = job
  reviewAccepted.value = false
  drawer.value = true
  await loadBootstrapReview(job)
}
function closeDetail() {
  detail.value = null
  bootstrapReview.value = null
  reviewAccepted.value = false
}
async function acceptReview() {
  if (!detail.value || !bootstrapReview.value) return
  acceptingReview.value = true
  try {
    await acceptIncrementalBootstrapExclusions(detail.value.job_id, bootstrapReview.value.manifest_hash)
    logRefreshToken.value++
    Message.success('已确认排除失败表，正在追赶 binlog')
    reviewAccepted.value = false
    await load()
  } catch (e: any) {
    Message.error(e?.response?.data?.error || '确认失败表排除失败')
  } finally {
    acceptingReview.value = false
  }
}
async function exportFailedDDL() {
  if (!detail.value) return
  exportingFailedDDL.value = true
  try {
    await downloadIncrementalFailedDDL(detail.value.job_id)
    Message.success('修复 SQL 已导出')
  } catch (e: any) {
    Message.error(e?.message || '导出修复 SQL 失败')
  } finally {
    exportingFailedDDL.value = false
  }
}
function pausable(job: IncrementalJob) { return ['catching_up', 'running', 'reconnecting'].includes(job.status) }
function unsafeBootstrap(job: IncrementalJob) { return job.start_mode === 'full_then_cdc' && !job.bootstrap_completed && ['paused_restart', 'failed'].includes(job.status) }
function resumable(job: IncrementalJob) { return ['paused_manual', 'paused_restart', 'failed', 'paused_row_conflict'].includes(job.status) && job.locator_strategy_version === 1 }
function preparable(status: string) { return ['running', 'catching_up'].includes(status) }
function cancelable(status: string) { return ['cutting_over', 'ready_to_cutover', 'ready_with_warnings', 'cutover_blocked'].includes(status) }
function completable(status: string) { return ['ready_to_cutover', 'ready_with_warnings'].includes(status) }
function abortable(status: string) { return !['validating', 'stopped', 'aborted'].includes(status) }
function bootstrapLogPolling(job: IncrementalJob) { return ['initializing', 'snapshot', 'paused_bootstrap_review'].includes(job.status) }
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
function endpoint(conn: { host: string; port: number }) {
  const host = conn.host.includes(':') && !conn.host.startsWith('[') ? `[${conn.host}]` : conn.host
  return `${host}:${conn.port}`
}

const labels: Record<string, string> = {
  initializing: '初始化', snapshot: '全量快照', catching_up: '追赶', running: '运行中', reconnecting: '重连中',
  pausing: '暂停中', paused_manual: '已暂停', paused_restart: '重启后暂停', paused_ddl: 'DDL 暂停',
  paused_row_conflict: '行冲突暂停',
  paused_bootstrap_review: '全量待确认',
  cutting_over: '追赶切换边界', validating: '最终校验', ready_to_cutover: '可完成切换',
  ready_with_warnings: '带风险待确认', cutover_blocked: '切换受阻', stopped: '已完成', aborted: '已放弃', failed: '失败',
}
const bootstrapStageLabels: Record<string, string> = { schema: '建表', data: '数据复制', row_count: '行数校验', cdc_compatibility: 'CDC 兼容性' }
const bootstrapStageText = (stage: string) => bootstrapStageLabels[stage] || stage
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
onUnmounted(() => {
  disposed = true
  if (timer) clearInterval(timer)
})
</script>

<style scoped>
.toolbar { display: flex; justify-content: flex-end; margin-bottom: 16px; }
.history-conn-cell { display: flex; align-items: center; gap: 6px; min-width: 0; }
.conn-name {
  max-width: 190px;
  overflow: hidden;
  color: var(--fg-primary);
  font-size: 13px;
  font-weight: 500;
  text-overflow: ellipsis;
  white-space: nowrap;
  cursor: default;
}
.conn-deleted { color: #86909c; font-size: 12px; }
.conn-detail {
  display: flex;
  align-items: center;
  gap: 3px;
  min-width: 0;
  margin-top: 3px;
  color: var(--fg-muted);
  font-size: 11px;
}
.conn-label { flex-shrink: 0; color: var(--fg-muted); }
.conn-detail-val {
  max-width: 76px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  cursor: default;
}
.address-val { max-width: 225px; }
.schema-val { max-width: 76px; color: #165dff; font-weight: 500; }
.conn-detail-sep { flex-shrink: 0; color: var(--border-strong); }
.route-summary {
  padding: 12px 0 16px;
  margin-bottom: 4px;
  border-bottom: 1px solid var(--color-border-2);
}
.route-row { display: flex; flex-wrap: wrap; align-items: baseline; gap: 6px; margin-bottom: 7px; font-size: 13px; }
.route-row:last-child { margin-bottom: 0; }
.route-label { min-width: 52px; color: var(--color-text-2); font-weight: 600; }
.route-name, .route-endpoint {
  display: inline-block;
  max-width: 190px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  vertical-align: bottom;
  cursor: default;
}
.route-meta { min-width: 0; margin-left: 4px; color: var(--color-text-3); font-size: 12px; overflow-wrap: anywhere; }
.route-schema {
  display: inline-block;
  max-width: 210px;
  overflow: hidden;
  padding: 1px 8px;
  margin-left: 4px;
  border: 1px solid #bedaff;
  border-radius: 4px;
  background: #e8f3ff;
  color: #165dff;
  font-size: 12px;
  font-weight: 600;
  text-overflow: ellipsis;
  white-space: nowrap;
  vertical-align: bottom;
}
.ddl { padding: 12px; background: #f2f3f5; border-radius: 4px; white-space: pre-wrap; }
.ddl-popover { max-width: 680px; max-height: 360px; overflow: auto; }
.review-confirm { display: flex; align-items: center; justify-content: space-between; margin-top: 12px; padding: 10px 12px; background: var(--color-warning-light-1); border-radius: 4px; }
</style>
