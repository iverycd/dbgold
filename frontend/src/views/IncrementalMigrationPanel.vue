<template>
  <a-form :model="form" layout="vertical" class="incremental-panel">
    <a-alert type="warning" style="margin-bottom: 16px">
      “全量快照后持续同步”只在任务首次初始化时删除并重建所选目标表；全量完成后会立即把 checkpoint 写入目标 PostgreSQL，恢复任务只会从 checkpoint 继续，不会再次删表。全量未完成的中断任务禁止直接恢复。
    </a-alert>

    <a-row :gutter="20">
      <a-col :span="12">
        <a-form-item label="MySQL 源连接">
          <a-select v-model="form.src_conn_id" allow-search @change="loadDatabases">
            <a-option v-for="c in mysqlConnections" :key="c.id" :value="c.id">
              {{ c.name }} · {{ c.host }}:{{ c.port }}
            </a-option>
          </a-select>
        </a-form-item>
        <a-form-item label="源数据库">
          <a-select v-model="form.src_database" allow-search>
            <a-option v-for="d in databases" :key="d" :value="d">{{ d }}</a-option>
          </a-select>
        </a-form-item>
      </a-col>
      <a-col :span="12">
        <a-form-item label="PostgreSQL 目标连接">
          <a-select v-model="form.dst_conn_id" allow-search @change="loadSchemas">
            <a-option v-for="c in postgresConnections" :key="c.id" :value="c.id">
              {{ c.name }} · {{ c.host }}:{{ c.port }}
            </a-option>
          </a-select>
        </a-form-item>
        <a-form-item label="目标 Schema">
          <a-select v-model="form.target_schema" allow-search>
            <a-option v-for="s in schemas" :key="s" :value="s">{{ s }}</a-option>
          </a-select>
        </a-form-item>
      </a-col>
    </a-row>

    <a-form-item label="启动方式">
      <a-radio-group v-model="form.start_mode">
        <a-radio value="full_then_cdc">全量快照后持续同步</a-radio>
        <a-radio value="incremental_only">从指定位点开始</a-radio>
      </a-radio-group>
      <template #extra>
        全量快照会长时间保持一致性读事务，但全局读锁仅用于建立快照和记录起始位点。binlog 保留时间必须覆盖全量耗时、追赶耗时和安全余量；即使未启用自动清理，迁移期间也不能人工执行 PURGE BINARY LOGS。
      </template>
    </a-form-item>

    <a-form-item v-if="form.start_mode === 'full_then_cdc'" label="全量失败处理">
      <a-radio-group v-model="form.bootstrap_failure_policy">
        <a-radio value="review_and_exclude">暂停审阅，确认排除后继续</a-radio>
        <a-radio value="fail_all">任意关键表失败则终止</a-radio>
      </a-radio-group>
      <template #extra>审阅模式不会静默缩小范围；只有你明确确认失败表清单后，成功表才会开始增量同步。</template>
    </a-form-item>

    <a-row v-if="form.start_mode === 'incremental_only'" :gutter="16">
      <a-col :span="6">
        <a-form-item label="位点类型">
          <a-radio-group v-model="form.position_mode">
            <a-radio value="gtid">GTID</a-radio>
            <a-radio value="file">文件</a-radio>
          </a-radio-group>
        </a-form-item>
      </a-col>
      <a-col v-if="form.position_mode === 'gtid'" :span="18">
        <a-form-item label="GTID Set"><a-input v-model="form.start_gtid" placeholder="uuid:1-100" /></a-form-item>
      </a-col>
      <template v-else>
        <a-col :span="10"><a-form-item label="Binlog 文件"><a-input v-model="form.start_file" placeholder="mysql-bin.000001" /></a-form-item></a-col>
        <a-col :span="8"><a-form-item label="位置"><a-input-number v-model="form.start_position" :min="4" style="width: 100%" /></a-form-item></a-col>
      </template>
    </a-row>

    <a-row :gutter="16">
      <a-col :span="8">
        <a-form-item label="表范围">
          <a-select v-model="form.migrate_mode">
            <a-option value="all">全部表</a-option>
            <a-option value="include">仅包含</a-option>
            <a-option value="exclude">排除</a-option>
          </a-select>
        </a-form-item>
      </a-col>
      <a-col :span="12"><a-form-item label="表过滤"><a-input v-model="form.table_filter" :disabled="form.migrate_mode === 'all'" placeholder="逗号分隔，支持 *" /></a-form-item></a-col>
      <a-col :span="4"><a-form-item label="名称"><a-checkbox v-model="form.lower_case_names">转小写</a-checkbox></a-form-item></a-col>
    </a-row>

    <a-space style="margin-bottom: 16px">
      <a-button :loading="checking" :disabled="!ready" @click="preflight">运行预检</a-button>
      <a-button type="primary" :loading="starting" :disabled="!preflightResult?.ok || !!currentJob" @click="start">启动增量任务</a-button>
      <a-button v-if="currentJob && terminalStatus" @click="newTask">新建任务</a-button>
    </a-space>

    <a-card v-if="preflightResult" title="预检结果" style="margin-bottom: 16px">
      <a-descriptions :column="5" size="small">
        <a-descriptions-item label="log_bin">{{ preflightResult.log_bin ? 'ON' : 'OFF' }}</a-descriptions-item>
        <a-descriptions-item label="format">{{ preflightResult.binlog_format }}</a-descriptions-item>
        <a-descriptions-item label="row image">{{ preflightResult.binlog_row_image }}</a-descriptions-item>
        <a-descriptions-item label="匹配表">{{ preflightResult.tables?.length || 0 }}</a-descriptions-item>
        <a-descriptions-item label="binlog 保留">{{ retentionText }}</a-descriptions-item>
      </a-descriptions>
      <a-alert v-for="e in preflightResult.errors" :key="e" type="error" style="margin-top: 8px">{{ e }}</a-alert>
      <a-alert v-for="w in preflightResult.warnings" :key="w" type="warning" style="margin-top: 8px">{{ w }}</a-alert>
      <div v-if="preflightResult.no_primary_key_tables?.length" class="hint">
        无主键表：{{ preflightResult.no_primary_key_tables.join(', ') }}。这类表只同步 INSERT，UPDATE/DELETE 会跳过并阻止无风险完成切换。
      </div>
    </a-card>

    <a-card v-if="currentJob" title="当前增量任务">
      <template #extra><a-tag :color="statusColor(currentJob.status)">{{ statusText(currentJob.status) }}</a-tag></template>
      <a-descriptions :column="3" size="small">
        <a-descriptions-item label="阶段">{{ currentJob.phase }}</a-descriptions-item>
        <a-descriptions-item label="同步状态">
          <a-tag :color="currentJob.caught_up ? 'green' : 'orange'">{{ currentJob.caught_up ? '已追平' : '追赶中' }}</a-tag>
        </a-descriptions-item>
        <a-descriptions-item label="估算延迟">{{ currentJob.caught_up ? '0 秒' : `${currentJob.lag_seconds || 0} 秒` }}</a-descriptions-item>
        <a-descriptions-item label="INSERT">{{ currentJob.insert_count }}</a-descriptions-item>
        <a-descriptions-item label="UPDATE">{{ currentJob.update_count }}</a-descriptions-item>
        <a-descriptions-item label="DELETE">{{ currentJob.delete_count }}</a-descriptions-item>
        <a-descriptions-item label="跳过 / 告警">{{ currentJob.skipped_count }} / {{ currentJob.warning_count }}</a-descriptions-item>
        <a-descriptions-item label="最后事件">{{ currentJob.last_event_at ? formatDate(currentJob.last_event_at) : '—' }}</a-descriptions-item>
        <a-descriptions-item label="全量完成">{{ currentJob.bootstrap_completed ? '是' : '否' }}</a-descriptions-item>
        <a-descriptions-item label="有效 / 排除表">{{ currentJob.effective_table_count || 0 }} / {{ currentJob.excluded_table_count || 0 }}</a-descriptions-item>
        <a-descriptions-item v-if="currentJob.pending_file || currentJob.pending_gtid" label="全量起始位点" :span="2">{{ positionText(currentJob.pending_file, currentJob.pending_position, currentJob.pending_gtid) }}</a-descriptions-item>
        <a-descriptions-item label="目标 checkpoint" :span="3">{{ positionText(currentJob.checkpoint_file, currentJob.checkpoint_position, currentJob.checkpoint_gtid) }}</a-descriptions-item>
        <a-descriptions-item label="源端最新位点" :span="3">{{ positionText(currentJob.source_head_file, currentJob.source_head_position, currentJob.source_head_gtid) }}</a-descriptions-item>
        <a-descriptions-item v-if="currentJob.cutover_file || currentJob.cutover_gtid" label="切换边界" :span="3">{{ positionText(currentJob.cutover_file, currentJob.cutover_position, currentJob.cutover_gtid) }}</a-descriptions-item>
        <a-descriptions-item label="摘要" :span="3">{{ currentJob.summary || '—' }}</a-descriptions-item>
      </a-descriptions>

      <IncrementalMigrationLogPanel
        v-if="currentJob.start_mode === 'full_then_cdc'"
        :key="currentJob.job_id"
        :jobID="currentJob.job_id"
        :polling="bootstrapLogPolling"
        :refresh-token="logRefreshToken"
      />

      <a-alert v-if="unsafeBootstrapResume" type="error" style="margin-top: 12px">
        SQLite 尚未记录全量完成。点击“恢复”时系统会先查询目标 checkpoint：若全量已完成则从位点续跑；若没有完成位点则拒绝恢复，绝不会自动删表重跑。
      </a-alert>
      <a-alert v-if="currentJob.last_error" type="error" style="margin-top: 10px">{{ currentJob.last_error }}</a-alert>
      <a-alert v-if="cutoverInProgress" type="warning" style="margin-top: 10px">
        已锁定最终位点。必须继续保持整个源 MySQL 实例停写，且目标库不能有业务写入；系统会追到边界并执行序列校正和逐表行数校验。
      </a-alert>
      <a-alert v-if="currentJob.status === 'ready_to_cutover'" type="success" style="margin-top: 10px">
        已到达最终边界且校验通过。保持源库停写，点击“完成切换”后再把业务流量切向 PostgreSQL。
      </a-alert>
      <a-alert v-if="currentJob.status === 'ready_with_warnings'" type="warning" style="margin-top: 10px">
        行数一致，但存在被排除表或无主键 UPDATE/DELETE 被跳过。请检查影响后明确确认风险。
      </a-alert>

      <div v-if="bootstrapReview" class="bootstrap-review">
        <div class="section-title">全量迁移范围审阅</div>
        <a-alert v-if="currentJob.status === 'paused_bootstrap_review'" type="warning">
          全量已结束但尚未启动 CDC。确认前请确保源库保留从 {{ positionText(bootstrapReview.position.file, bootstrapReview.position.position, bootstrapReview.position.gtid) }} 开始的 binlog。
        </a-alert>
        <a-descriptions :column="3" size="small" style="margin-top: 10px">
          <a-descriptions-item label="原始范围">{{ bootstrapReview.requested_count }}</a-descriptions-item>
          <a-descriptions-item label="成功表">{{ bootstrapReview.effective_tables.length }}</a-descriptions-item>
          <a-descriptions-item label="排除表">{{ bootstrapReview.excluded_tables.length }}</a-descriptions-item>
        </a-descriptions>
        <a-table v-if="bootstrapReview.excluded_tables.length" :data="bootstrapReview.excluded_tables" size="small" :pagination="false" :scroll="{ y: 280 }">
          <template #columns>
            <a-table-column title="表" data-index="table" :width="180" />
            <a-table-column title="失败阶段" :width="140"><template #cell="{ record }">{{ bootstrapStageText(record.stage) }}</template></a-table-column>
            <a-table-column title="错误" data-index="error" />
            <a-table-column title="DDL" :width="90"><template #cell="{ record }"><a-popover v-if="record.ddl" title="失败 DDL"><a-button size="mini">查看</a-button><template #content><pre class="ddl ddl-popover">{{ record.ddl }}</pre></template></a-popover><span v-else>—</span></template></a-table-column>
          </template>
        </a-table>
        <a-alert v-for="warning in bootstrapReview.warnings" :key="warning" type="warning" style="margin-top: 8px">{{ warning }}</a-alert>
        <div v-if="currentJob.status === 'paused_bootstrap_review'" class="cutover-confirm">
          <a-checkbox v-model="bootstrapExclusionsAccepted">我确认排除以上失败表，仅对成功表启动增量同步</a-checkbox>
          <a-button type="primary" :loading="acceptingBootstrap" :disabled="!bootstrapExclusionsAccepted" style="margin-left: 12px" @click="acceptBootstrap">接受排除并继续</a-button>
        </div>
      </div>

      <div v-if="currentJob.blocking_ddl" style="margin-top: 12px">
        <div class="hint">检测到 DDL，请先在目标库人工处理：</div>
        <pre class="ddl">{{ currentJob.blocking_ddl }}</pre>
      </div>

      <div v-if="validationRows.length" style="margin-top: 14px">
        <div class="section-title">最终行数校验</div>
        <a-table :data="validationRows" size="small" :pagination="false" :scroll="{ y: 260 }">
          <template #columns>
            <a-table-column title="表" data-index="table" />
            <a-table-column title="源库" data-index="source" :width="130" />
            <a-table-column title="目标库" data-index="target" :width="130" />
            <a-table-column title="结果" :width="100"><template #cell="{ record }"><a-tag :color="record.match ? 'green' : 'red'">{{ record.match ? '一致' : '不一致' }}</a-tag></template></a-table-column>
            <a-table-column title="错误" data-index="error" />
          </template>
        </a-table>
      </div>

      <div v-if="canPrepare" class="cutover-confirm">
        <a-checkbox v-model="sourceWritesStopped">我已停止整个源 MySQL 实例的业务写入，并确认目标库无业务写入</a-checkbox>
      </div>
      <div v-if="currentJob.status === 'ready_with_warnings'" class="cutover-confirm">
        <a-checkbox v-model="warningsAccepted">我已核对同步警告并接受风险</a-checkbox>
      </div>
      <div v-if="canComplete && currentJob.excluded_table_count > 0" class="cutover-confirm">
        <a-checkbox v-model="finalExclusionsAccepted">我再次确认本次切换不包含 {{ currentJob.excluded_table_count }} 张被排除表</a-checkbox>
      </div>

      <a-space wrap style="margin-top: 12px">
        <a-button v-if="canPause" @click="pause">暂停</a-button>
        <a-button v-if="canResume" @click="resume">恢复</a-button>
        <a-button v-if="currentJob.status === 'paused_ddl'" type="primary" @click="ackDDL">确认已处理并恢复</a-button>
        <a-button v-if="canPrepare" type="primary" :disabled="!sourceWritesStopped" @click="prepareCutover">准备切换</a-button>
        <a-button v-if="canCancelCutover" @click="cancelCutover">取消切换并继续同步</a-button>
        <a-button v-if="canComplete" type="primary" status="success" :disabled="(currentJob.status === 'ready_with_warnings' && !warningsAccepted) || (currentJob.excluded_table_count > 0 && !finalExclusionsAccepted)" @click="completeCutover">完成切换</a-button>
        <a-popconfirm v-if="canAbort" content="放弃后任务不能恢复；目标端已迁移的数据不会自动删除。确认放弃？" @ok="abort">
          <a-button status="danger">放弃任务</a-button>
        </a-popconfirm>
      </a-space>
    </a-card>
  </a-form>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import { Message } from '@arco-design/web-vue'
import { listConnections, listConnectionDatabases, listConnectionSchemas, type Connection } from '@/api/connections'
import IncrementalMigrationLogPanel from '@/components/IncrementalMigrationLogPanel.vue'
import {
  acceptIncrementalBootstrapExclusions,
  abortIncrementalJob,
  acknowledgeIncrementalDDL,
  cancelIncrementalCutover,
  getIncrementalJob,
  getIncrementalBootstrapReview,
  listIncrementalJobs,
  pauseIncrementalJob,
  preflightIncremental,
  prepareIncrementalCutover,
  resumeIncrementalJob,
  startIncremental,
  stopIncrementalJob,
  type IncrementalJob,
  type BootstrapReview,
  type IncrementalPreflight,
  type IncrementalRequest,
} from '@/api/migration'

interface ValidationRow { table: string; source: number; target: number; match: boolean; error?: string }

const connections = ref<Connection[]>([])
const databases = ref<string[]>([])
const schemas = ref<string[]>([])
const checking = ref(false)
const starting = ref(false)
const preflightResult = ref<IncrementalPreflight | null>(null)
const currentJob = ref<IncrementalJob | null>(null)
const bootstrapReview = ref<BootstrapReview | null>(null)
const sourceWritesStopped = ref(false)
const warningsAccepted = ref(false)
const bootstrapExclusionsAccepted = ref(false)
const finalExclusionsAccepted = ref(false)
const acceptingBootstrap = ref(false)
const logRefreshToken = ref(0)
let timer: number | undefined
let refreshRunning = false
let refreshPending = false
let pollGeneration = 0
let disposed = false

const form = reactive<IncrementalRequest>({
  src_conn_id: 0,
  dst_conn_id: 0,
  src_database: '',
  target_schema: '',
  start_mode: 'full_then_cdc',
  position_mode: 'gtid',
  start_gtid: '',
  start_file: '',
  start_position: 4,
  migrate_mode: 'all',
  table_filter: '',
  lower_case_names: true,
  bootstrap_failure_policy: 'review_and_exclude',
})

const mysqlConnections = computed(() => connections.value.filter(c => c.db_type === 'mysql'))
const postgresConnections = computed(() => connections.value.filter(c => c.db_type === 'postgres'))
const ready = computed(() => !!(form.src_conn_id && form.dst_conn_id && form.src_database && form.target_schema))
const canPause = computed(() => ['catching_up', 'running', 'reconnecting'].includes(currentJob.value?.status || ''))
const unsafeBootstrapResume = computed(() => !!currentJob.value && currentJob.value.start_mode === 'full_then_cdc' && !currentJob.value.bootstrap_completed && ['paused_restart', 'failed'].includes(currentJob.value.status))
const canResume = computed(() => ['paused_manual', 'paused_restart', 'failed'].includes(currentJob.value?.status || ''))
const canPrepare = computed(() => ['running', 'catching_up'].includes(currentJob.value?.status || ''))
const cutoverInProgress = computed(() => ['cutting_over', 'validating'].includes(currentJob.value?.status || ''))
const canCancelCutover = computed(() => ['cutting_over', 'ready_to_cutover', 'ready_with_warnings', 'cutover_blocked'].includes(currentJob.value?.status || ''))
const canComplete = computed(() => ['ready_to_cutover', 'ready_with_warnings'].includes(currentJob.value?.status || ''))
const terminalStatus = computed(() => ['stopped', 'aborted'].includes(currentJob.value?.status || ''))
const bootstrapLogPolling = computed(() => ['initializing', 'snapshot', 'paused_bootstrap_review'].includes(currentJob.value?.status || ''))
const canAbort = computed(() => !!currentJob.value && !['validating', 'stopped', 'aborted'].includes(currentJob.value.status))
const retentionText = computed(() => {
  const seconds = preflightResult.value?.binlog_retention_seconds
  if (seconds == null) return '未知'
  if (seconds === 0) return '不自动清理'
  return `${(seconds / 3600).toFixed(1)} 小时`
})
const validationRows = computed<ValidationRow[]>(() => {
  if (!currentJob.value?.validation_json) return []
  try {
    const rows = JSON.parse(currentJob.value.validation_json)
    return Array.isArray(rows) ? rows : []
  } catch {
    return []
  }
})

watch(form, () => { preflightResult.value = null }, { deep: true })
watch(() => currentJob.value?.status, () => {
  sourceWritesStopped.value = false
  warningsAccepted.value = false
  bootstrapExclusionsAccepted.value = false
  finalExclusionsAccepted.value = false
  void loadBootstrapReview()
})

async function loadDatabases() {
  form.src_database = ''
  if (form.src_conn_id) databases.value = (await listConnectionDatabases(form.src_conn_id)).data || []
}
async function loadSchemas() {
  form.target_schema = ''
  if (form.dst_conn_id) schemas.value = (await listConnectionSchemas(form.dst_conn_id)).data || []
}
async function preflight() {
  checking.value = true
  try {
    preflightResult.value = (await preflightIncremental(form)).data
    if (preflightResult.value.ok) Message.success('预检通过')
    else Message.error('预检未通过')
  } catch (e: any) {
    preflightResult.value = e?.response?.data?.preflight || null
    Message.error(e?.response?.data?.error || '预检失败')
  } finally {
    checking.value = false
  }
}
async function start() {
  starting.value = true
  try {
    const response = await startIncremental(form)
    const job = (await getIncrementalJob(response.data.job_id)).data
    pollGeneration++
    currentJob.value = job
    beginPoll()
    Message.success('增量任务已启动')
  } catch (e: any) {
    Message.error(e?.response?.data?.error || '启动失败')
  } finally {
    starting.value = false
  }
}
async function refresh() {
  if (disposed) return
  if (refreshRunning) {
    refreshPending = true
    return
  }
  refreshRunning = true
  try {
    do {
      refreshPending = false
      const jobID = currentJob.value?.job_id
      if (!jobID) return
      const generation = pollGeneration
      const job = (await getIncrementalJob(jobID)).data
      if (!disposed && generation === pollGeneration && currentJob.value?.job_id === jobID) currentJob.value = job
    } while (refreshPending)
  } finally {
    refreshRunning = false
    if (!disposed && refreshPending) void refresh().catch(() => {})
  }
}
async function loadBootstrapReview() {
  if (!currentJob.value || (currentJob.value.status !== 'paused_bootstrap_review' && currentJob.value.excluded_table_count <= 0)) {
    bootstrapReview.value = null
    return
  }
  const jobID = currentJob.value.job_id
  try {
    const review = (await getIncrementalBootstrapReview(jobID)).data
    if (!disposed && currentJob.value?.job_id === jobID) bootstrapReview.value = review
  } catch {
    if (!disposed && currentJob.value?.job_id === jobID) bootstrapReview.value = null
  }
}
function beginPoll() {
  if (timer) clearInterval(timer)
  timer = window.setInterval(() => refresh().catch(() => {}), 2000)
}
async function runAction(action: () => Promise<unknown>, success: string) {
  try {
    await action()
    logRefreshToken.value++
    Message.success(success)
    await refresh()
  } catch (e: any) {
    Message.error(e?.response?.data?.error || '操作失败')
  }
}
const pause = () => runAction(() => pauseIncrementalJob(currentJob.value!.job_id), '正在安全暂停')
const resume = () => runAction(() => resumeIncrementalJob(currentJob.value!.job_id), '任务已恢复')
const ackDDL = () => runAction(() => acknowledgeIncrementalDDL(currentJob.value!.job_id), 'DDL 已确认，正在恢复')
const prepareCutover = () => runAction(() => prepareIncrementalCutover(currentJob.value!.job_id), '已锁定最终位点，正在追赶和校验')
const cancelCutover = () => runAction(() => cancelIncrementalCutover(currentJob.value!.job_id), '已取消切换并继续同步')
async function acceptBootstrap() {
  if (!currentJob.value || !bootstrapReview.value) return
  acceptingBootstrap.value = true
  try {
    await acceptIncrementalBootstrapExclusions(currentJob.value.job_id, bootstrapReview.value.manifest_hash)
    logRefreshToken.value++
    Message.success('已确认排除失败表，正在追赶 binlog')
    await refresh()
    await loadBootstrapReview()
  } catch (e: any) {
    Message.error(e?.response?.data?.error || '确认失败表排除失败')
  } finally {
    acceptingBootstrap.value = false
  }
}
const completeCutover = () => runAction(() => stopIncrementalJob(currentJob.value!.job_id, warningsAccepted.value, finalExclusionsAccepted.value), '迁移闭环已安全完成')
const abort = () => runAction(() => abortIncrementalJob(currentJob.value!.job_id), '任务已放弃')
function newTask() {
  pollGeneration++
  currentJob.value = null
  preflightResult.value = null
  bootstrapReview.value = null
  if (timer) clearInterval(timer)
}
function positionText(file: string, pos: number, gtid: string) {
  const filePos = file ? `${file}:${pos || 0}` : '—'
  return gtid ? `${filePos} · GTID ${gtid}` : filePos
}
const labels: Record<string, string> = {
  initializing: '初始化', snapshot: '全量快照', catching_up: '追赶', running: '运行中', reconnecting: '重连中',
  pausing: '暂停中', paused_manual: '已暂停', paused_restart: '重启后暂停', paused_ddl: 'DDL 暂停',
  paused_bootstrap_review: '全量待确认',
  cutting_over: '追赶切换边界', validating: '最终校验', ready_to_cutover: '可完成切换',
  ready_with_warnings: '带风险待确认', cutover_blocked: '切换受阻', stopped: '已完成', aborted: '已放弃', failed: '失败',
}
const bootstrapStageLabels: Record<string, string> = { schema: '建表', data: '数据复制', row_count: '行数校验', cdc_compatibility: 'CDC 兼容性' }
function bootstrapStageText(stage: string) { return bootstrapStageLabels[stage] || stage }
function statusText(status: string) { return labels[status] || status }
function statusColor(status: string) {
  if (['running', 'ready_to_cutover', 'stopped'].includes(status)) return 'green'
  if (['failed', 'cutover_blocked'].includes(status)) return 'red'
  if (status.startsWith('paused') || status === 'ready_with_warnings') return 'orange'
  if (status === 'aborted') return 'gray'
  return 'blue'
}
function formatDate(value: string) { return new Date(value).toLocaleString('zh-CN') }

onMounted(async () => {
  const [connectionResult, jobResult] = await Promise.allSettled([
    listConnections(),
    listIncrementalJobs(),
  ])
  if (disposed) return
  if (connectionResult.status === 'fulfilled') {
    connections.value = connectionResult.value.data
  } else {
    Message.error('加载连接列表失败')
  }
  if (jobResult.status === 'fulfilled') {
    const latest = [...(jobResult.value.data || [])]
      .filter(job => !['stopped', 'aborted'].includes(job.status))
      .sort((a, b) => new Date(b.updated_at).getTime() - new Date(a.updated_at).getTime())[0]
    if (latest) {
      pollGeneration++
      currentJob.value = latest
      beginPoll()
    }
  }
})
onUnmounted(() => {
  disposed = true
  pollGeneration++
  if (timer) clearInterval(timer)
})
</script>

<style scoped>
.incremental-panel { margin-top: 12px; max-width: 1180px; }
.hint { margin-top: 8px; color: var(--color-text-3); font-size: 12px; }
.ddl { padding: 10px; background: #f2f3f5; white-space: pre-wrap; border-radius: 4px; }
.section-title { margin-bottom: 8px; font-weight: 500; }
.cutover-confirm { margin-top: 14px; padding: 10px 12px; background: var(--color-warning-light-1); border-radius: 4px; }
.bootstrap-review { margin-top: 16px; padding: 14px; border: 1px solid var(--color-border-2); border-radius: 6px; }
.ddl-popover { max-width: 680px; max-height: 360px; overflow: auto; }
</style>
