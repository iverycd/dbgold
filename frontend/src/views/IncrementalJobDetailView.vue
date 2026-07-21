<template>
  <div class="detail-page">
    <a-spin v-if="loading && !job" class="page-loading" />

    <a-result v-else-if="loadError && !job" status="error" title="无法加载增量任务" :subtitle="loadError">
      <template #extra>
        <a-space>
          <a-button @click="backToHistory">返回迁移历史</a-button>
          <a-button type="primary" @click="loadJob()">重试</a-button>
        </a-space>
      </template>
    </a-result>

    <template v-else-if="job">
      <header class="task-header">
        <div class="task-heading">
          <a-button type="text" class="back-button" @click="backToHistory">
            <template #icon><icon-left /></template>
            返回增量迁移
          </a-button>
          <div class="title-line">
            <h1>增量迁移任务</h1>
            <a-tag :color="incrementalStatusColor(job.status)">
              {{ job.locator_strategy_version !== 1 ? '版本升级后已废弃' : incrementalStatusText(job.status) }}
            </a-tag>
            <span v-if="refreshing" class="refreshing"><icon-loading /> 正在刷新</span>
          </div>
          <div class="job-identity">
            <code>{{ job.job_id }}</code>
            <a-button type="text" size="mini" aria-label="复制 Job ID" @click="copy(job.job_id, 'Job ID')"><icon-copy /></a-button>
          </div>
          <p class="task-summary">{{ job.summary || '暂无任务摘要' }}</p>
        </div>

        <div class="task-actions">
          <a-button v-if="pausable(job)" :loading="acting" @click="pause">暂停</a-button>
          <a-button v-if="resumable(job)" type="primary" :loading="acting" @click="resume">恢复任务</a-button>
          <a-button v-if="job.status === 'paused_ddl'" type="primary" :loading="acting" @click="ack">确认 DDL</a-button>
          <a-popconfirm v-if="preparable(job.status)" content="仅当整个源 MySQL 实例已停写、目标库也无业务写入时才能继续。确认？" @ok="prepare">
            <a-button type="primary" :loading="acting">准备切换</a-button>
          </a-popconfirm>
          <a-button v-if="cancelable(job.status)" :loading="acting" @click="cancelCutover">取消切换</a-button>
          <a-popconfirm v-if="completable(job.status)" :content="completeConfirmText" @ok="complete">
            <a-button type="primary" status="success" :loading="acting">完成切换</a-button>
          </a-popconfirm>
          <a-popconfirm v-if="abortable(job.status)" content="放弃后不能恢复，目标端数据不会自动删除。确认放弃？" @ok="abort">
            <a-button status="danger" :loading="acting">放弃任务</a-button>
          </a-popconfirm>
          <a-button :loading="refreshing" @click="loadJob()"><template #icon><icon-refresh /></template>刷新</a-button>
        </div>
      </header>

      <MigrationConnectionRoute
        :src-db-type="job.src_db_type"
        :dst-db-type="job.dst_db_type"
        :src-conn="job.src_conn"
        :dst-conn="job.dst_conn"
        :src-database="job.src_conn?.database || job.src_database"
        :dst-database="job.dst_conn?.database"
        :dst-schema="job.target_schema"
        :mode-label="job.start_mode === 'full_then_cdc' ? '全量 + 增量' : '仅增量'"
      />

      <section class="phase-surface" aria-label="迁移阶段">
        <div v-for="(phase, index) in phases" :key="phase" class="phase-item" :class="phaseClass(index)">
          <span class="phase-dot"><icon-check v-if="index < currentPhaseIndex" /></span>
          <span class="phase-name">{{ phase }}</span>
          <span v-if="index < phases.length - 1" class="phase-connector"></span>
        </div>
      </section>

      <section class="metrics-grid" aria-label="任务指标">
        <div class="metric-item metric-highlight">
          <span class="metric-label">同步状态</span>
          <strong>{{ job.caught_up ? '已追平' : '追赶中' }}</strong>
          <span class="metric-note">{{ job.caught_up ? '目标端已到源端最新位点' : `约落后 ${job.lag_seconds || 0} 秒` }}</span>
        </div>
        <div class="metric-item">
          <span class="metric-label">变更统计 I / U / D</span>
          <strong>{{ job.insert_count }} / {{ job.update_count }} / {{ job.delete_count }}</strong>
          <span class="metric-note">跳过 {{ job.skipped_count }} 条</span>
        </div>
        <div class="metric-item">
          <span class="metric-label">迁移范围</span>
          <strong>{{ job.effective_table_count || 0 }} 张表</strong>
          <span class="metric-note">排除 {{ job.excluded_table_count || 0 }} 张</span>
        </div>
        <div class="metric-item">
          <span class="metric-label">定位策略</span>
          <strong>{{ job.primary_locator_count || 0 }} / {{ job.unique_locator_count || 0 }} / {{ job.full_row_locator_count || 0 }}</strong>
          <span class="metric-note">主键 / 唯一键 / 整行</span>
        </div>
        <div class="metric-item">
          <span class="metric-label">失败对象</span>
          <strong :class="{ 'danger-text': job.failed_object_count > 0 }">{{ job.failed_object_count || 0 }}</strong>
          <span class="metric-note">含 DDL {{ job.failed_ddl_count || 0 }} 个</span>
        </div>
        <div class="metric-item">
          <span class="metric-label">最后事件</span>
          <strong class="metric-date">{{ formatIncrementalDate(job.last_event_at) }}</strong>
          <span class="metric-note">任务更新 {{ formatIncrementalDate(job.updated_at) }}</span>
        </div>
      </section>

      <section v-if="risks.length" class="risk-section">
        <div class="section-heading">
          <div><span class="eyebrow">需要处理</span><h2>当前风险与下一步</h2></div>
          <a-tag color="red">{{ risks.length }} 项</a-tag>
        </div>
        <div class="risk-list">
          <article v-for="risk in risks" :key="risk.key" class="risk-item" :class="`risk-${risk.level}`">
            <div class="risk-icon"><icon-exclamation-circle-fill /></div>
            <div class="risk-content">
              <strong>{{ risk.title }}</strong>
              <p>{{ risk.description }}</p>
              <div v-if="risk.detail" class="long-text-shell">
                <pre :class="{ collapsed: !expandedText[risk.key] }">{{ risk.detail }}</pre>
                <div class="long-text-actions">
                  <a-button type="text" size="mini" @click="toggleText(risk.key)">{{ expandedText[risk.key] ? '收起' : '展开完整内容' }}</a-button>
                  <a-button type="text" size="mini" @click="copy(risk.detail, risk.title)"><icon-copy /> 复制</a-button>
                </div>
              </div>
            </div>
          </article>
        </div>
      </section>

      <section class="detail-tabs-surface">
        <a-tabs v-model:active-key="activeTab" lazy-load>
          <a-tab-pane key="overview" title="概览">
            <div class="overview-grid">
              <section class="content-section">
                <div class="section-heading compact"><div><span class="eyebrow">CDC POSITION</span><h2>同步位点</h2></div></div>
                <PositionRow label="目标 Checkpoint" :value="positionText(job.checkpoint_file, job.checkpoint_position, job.checkpoint_gtid)" @copy="copy" />
                <PositionRow label="源端最新位点" :value="positionText(job.source_head_file, job.source_head_position, job.source_head_gtid)" @copy="copy" />
                <PositionRow v-if="job.cutover_file || job.cutover_gtid" label="切换边界" :value="positionText(job.cutover_file, job.cutover_position, job.cutover_gtid)" @copy="copy" />
              </section>

              <section class="content-section">
                <div class="section-heading compact"><div><span class="eyebrow">TASK INFO</span><h2>任务信息</h2></div></div>
                <dl class="info-grid">
                  <div><dt>当前阶段</dt><dd>{{ job.phase || '—' }}</dd></div>
                  <div><dt>全量完成</dt><dd>{{ job.bootstrap_completed ? '是' : '否' }}</dd></div>
                  <div><dt>启动模式</dt><dd>{{ job.start_mode === 'full_then_cdc' ? '全量 + 增量' : '仅增量' }}</dd></div>
                  <div><dt>校验状态</dt><dd>{{ validationStateText }}</dd></div>
                  <div><dt>创建时间</dt><dd>{{ formatIncrementalDate(job.created_at) }}</dd></div>
                  <div><dt>完成时间</dt><dd>{{ formatIncrementalDate(job.finished_at) }}</dd></div>
                </dl>
              </section>
            </div>
          </a-tab-pane>

          <a-tab-pane v-if="job.start_mode === 'full_then_cdc'" key="scope" title="全量与失败对象">
            <div class="tab-toolbar">
              <div>
                <h2>全量迁移范围</h2>
                <p>核对成功表、排除表和需要手工修复的对象。</p>
              </div>
              <a-button v-if="canExportFailedDDL" :loading="exportingFailedDDL" @click="exportFailedDDL">
                <template #icon><icon-download /></template>导出修复 SQL
              </a-button>
            </div>

            <a-alert v-if="bootstrapReviewError" type="warning" class="inline-alert">{{ bootstrapReviewError }}</a-alert>
            <template v-if="bootstrapReview">
              <div class="scope-stats">
                <div><span>原始范围</span><strong>{{ bootstrapReview.requested_count }}</strong></div>
                <div><span>成功表</span><strong>{{ bootstrapReview.effective_tables.length }}</strong></div>
                <div><span>排除表</span><strong>{{ bootstrapReview.excluded_tables.length }}</strong></div>
                <div><span>失败对象</span><strong>{{ failedObjects.length }}</strong></div>
              </div>

              <PositionRow label="原始快照位点" :value="positionText(bootstrapReview.position.file, bootstrapReview.position.position, bootstrapReview.position.gtid)" @copy="copy" />

              <a-alert v-if="job.status === 'paused_bootstrap_review'" type="warning" class="inline-alert">
                全量存在失败表，确认排除前不会启动 CDC。请确保原始快照位点的 binlog 仍被保留。
              </a-alert>

              <a-table v-if="failedObjects.length" :data="failedObjects" row-key="key" size="small" :pagination="false" :scroll="{ x: 920, y: 360 }" :expandable="failedObjectExpandable">
                <template #columns>
                  <a-table-column title="对象" data-index="name" :width="220" />
                  <a-table-column title="类型" :width="130"><template #cell="{ record }">{{ failedCategoryText(record.category) }}</template></a-table-column>
                  <a-table-column title="阶段" :width="120"><template #cell="{ record }">{{ bootstrapStageText(record.stage) }}</template></a-table-column>
                  <a-table-column title="错误摘要"><template #cell="{ record }"><span class="error-clamp">{{ record.error || '—' }}</span></template></a-table-column>
                  <a-table-column title="DDL" :width="80"><template #cell="{ record }"><a-tag :color="record.ddl ? 'orange' : 'gray'">{{ record.ddl ? '有' : '无' }}</a-tag></template></a-table-column>
                </template>
                <template #expand-row="{ record }">
                  <div class="failure-detail">
                    <div><strong>完整错误</strong><pre>{{ record.error || '—' }}</pre><a-button size="mini" type="text" @click="copy(record.error, '错误详情')"><icon-copy /> 复制错误</a-button></div>
                    <div v-if="record.ddl"><strong>失败 DDL</strong><pre>{{ record.ddl }}</pre><a-button size="mini" type="text" @click="copy(record.ddl, '失败 DDL')"><icon-copy /> 复制 DDL</a-button></div>
                  </div>
                </template>
              </a-table>
              <a-empty v-else description="全量迁移没有失败对象" />

              <a-alert v-for="warning in bootstrapReview.warnings" :key="warning" type="warning" class="inline-alert">{{ warning }}</a-alert>

              <div v-if="job.status === 'paused_bootstrap_review'" class="review-confirm">
                <a-checkbox v-model="reviewAccepted">我确认排除以上失败表，仅同步成功表</a-checkbox>
                <a-button type="primary" :loading="acceptingReview" :disabled="!reviewAccepted" @click="acceptReview">接受排除并继续</a-button>
              </div>
            </template>
            <a-empty v-else-if="!bootstrapReviewError" description="暂无全量审阅数据" />
          </a-tab-pane>

          <a-tab-pane v-if="job.start_mode === 'full_then_cdc'" key="logs" title="迁移日志">
            <IncrementalMigrationLogPanel
              v-if="activeTab === 'logs'"
              :key="job.job_id"
              :jobID="job.job_id"
              :polling="bootstrapLogPolling(job)"
              :refresh-token="logRefreshToken"
              class="detail-log-panel"
            />
          </a-tab-pane>

          <a-tab-pane key="validation" title="最终校验">
            <div class="tab-toolbar"><div><h2>最终行数校验</h2><p>切换前核对源端与目标端的数据一致性。</p></div></div>
            <a-table v-if="validationRows.length" :data="validationRows" size="small" :pagination="false" :scroll="{ x: 820, y: 480 }">
              <template #columns>
                <a-table-column title="表" data-index="table" :width="240" />
                <a-table-column title="源库" data-index="source" :width="140" />
                <a-table-column title="目标库" data-index="target" :width="140" />
                <a-table-column title="结果" :width="110"><template #cell="{ record }"><a-tag :color="record.match ? 'green' : 'red'">{{ record.match ? '一致' : '不一致' }}</a-tag></template></a-table-column>
                <a-table-column title="错误"><template #cell="{ record }"><span class="error-wrap">{{ record.error || '—' }}</span></template></a-table-column>
              </template>
            </a-table>
            <a-empty v-else :description="validationEmptyText" />
          </a-tab-pane>
        </a-tabs>
      </section>
    </template>
  </div>
</template>

<script setup lang="ts">
import { computed, defineComponent, h, onMounted, onUnmounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { Message } from '@arco-design/web-vue'
import IncrementalMigrationLogPanel from '@/components/IncrementalMigrationLogPanel.vue'
import { copyText } from '@/utils/clipboard'
import MigrationConnectionRoute from '@/components/MigrationConnectionRoute.vue'
import {
  abortIncrementalJob,
  acceptIncrementalBootstrapExclusions,
  acknowledgeIncrementalDDL,
  cancelIncrementalCutover,
  downloadIncrementalFailedDDL,
  getIncrementalBootstrapReview,
  getIncrementalJob,
  pauseIncrementalJob,
  prepareIncrementalCutover,
  resumeIncrementalJob,
  stopIncrementalJob,
  type BootstrapFailedObject,
  type BootstrapReview,
  type IncrementalJob,
} from '@/api/migration'
import {
  abortable,
  bootstrapLogPolling,
  bootstrapStageText,
  cancelable,
  completable,
  formatIncrementalDate,
  incrementalStatusColor,
  incrementalStatusText,
  pausable,
  positionText,
  preparable,
  resumable,
  unsafeBootstrap,
} from '@/utils/incrementalJob'

interface ValidationRow { table: string; source: number; target: number; match: boolean; error?: string }
interface FailedObjectRow extends BootstrapFailedObject { key: string }
interface RiskItem { key: string; level: 'warning' | 'danger'; title: string; description: string; detail?: string }

const PositionRow = defineComponent({
  name: 'PositionRow',
  props: { label: { type: String, required: true }, value: { type: String, required: true } },
  emits: ['copy'],
  setup(props, { emit }) {
    return () => h('div', { class: 'position-row' }, [
      h('span', { class: 'position-label' }, props.label),
      h('code', props.value),
      h('button', { class: 'inline-copy', type: 'button', onClick: () => emit('copy', props.value, props.label) }, '复制'),
    ])
  },
})

const route = useRoute()
const router = useRouter()
const job = ref<IncrementalJob | null>(null)
const bootstrapReview = ref<BootstrapReview | null>(null)
const loading = ref(false)
const refreshing = ref(false)
const acting = ref(false)
const loadError = ref('')
const bootstrapReviewError = ref('')
const activeTab = ref('overview')
const reviewAccepted = ref(false)
const acceptingReview = ref(false)
const exportingFailedDDL = ref(false)
const logRefreshToken = ref(0)
const expandedText = ref<Record<string, boolean>>({})
let timer: number | undefined
let disposed = false
let requestRunning = false

const jobID = computed(() => String(route.params.jobId || ''))
const completeConfirmText = computed(() => job.value?.status === 'ready_with_warnings'
  ? `存在同步警告或 ${job.value?.excluded_table_count || 0} 张排除表，确认已核对并接受当前迁移范围？`
  : '确认源库仍保持停写并完成迁移？')
const canExportFailedDDL = computed(() => !!job.value && (
  job.value.failed_object_count > 0 || (bootstrapReview.value?.excluded_tables.length || 0) > 0 || (bootstrapReview.value?.warnings.length || 0) > 0
))

const phases = computed(() => job.value?.start_mode === 'full_then_cdc'
  ? ['全量快照', '增量追赶', '切换准备', '最终校验', '完成']
  : ['增量追赶', '切换准备', '最终校验', '完成'])
const currentPhaseIndex = computed(() => {
  if (!job.value) return 0
  const offset = job.value.start_mode === 'full_then_cdc' ? 1 : 0
  if (['initializing', 'snapshot', 'paused_bootstrap_review'].includes(job.value.status)) return 0
  if (['catching_up', 'running', 'reconnecting', 'pausing', 'paused_manual', 'paused_restart', 'paused_ddl', 'paused_row_conflict', 'failed'].includes(job.value.status)) return offset
  if (job.value.status === 'cutting_over') return offset + 1
  if (['validating', 'ready_to_cutover', 'ready_with_warnings', 'cutover_blocked'].includes(job.value.status)) return offset + 2
  if (['stopped', 'aborted'].includes(job.value.status)) return phases.value.length - 1
  return 0
})

const validationRows = computed<ValidationRow[]>(() => {
  if (!job.value?.validation_json) return []
  try {
    const rows = JSON.parse(job.value.validation_json)
    return Array.isArray(rows) ? rows : []
  } catch {
    return []
  }
})
const validationStateText = computed(() => ({ pending: '等待校验', running: '校验中', passed: '已通过', failed: '失败', mismatch: '存在不一致' }[job.value?.validation_state || ''] || job.value?.validation_state || '未开始'))
const validationEmptyText = computed(() => job.value?.validation_state ? `当前校验状态：${validationStateText.value}` : '任务尚未进入最终校验阶段')

const failedObjects = computed<FailedObjectRow[]>(() => {
  const rows = bootstrapReview.value?.failed_objects?.length
    ? bootstrapReview.value.failed_objects
    : (bootstrapReview.value?.excluded_tables || []).map(item => ({ category: 'table' as const, name: item.table, error: item.error, ddl: item.ddl, stage: item.stage === 'row_count' ? 'validation' as const : (item.stage === 'data' ? 'data' as const : 'schema' as const) }))
  return rows.map((item, index) => ({ ...item, key: `${item.category}-${item.name}-${index}` }))
})
const failedObjectExpandable = { icon: (_: boolean, record: any) => hasFailureDetail(record as FailedObjectRow) ? undefined : null }

const risks = computed<RiskItem[]>(() => {
  if (!job.value) return []
  const items: RiskItem[] = []
  if (unsafeBootstrap(job.value)) items.push({ key: 'bootstrap', level: 'danger', title: '全量完成状态不安全', description: '恢复前会检查目标 Checkpoint；没有完成位点时任务将拒绝恢复。' })
  if (job.value.locator_strategy_version !== 1) items.push({ key: 'legacy', level: 'warning', title: '旧版定位策略任务已废弃', description: '该任务不能恢复，目标表、Checkpoint、日志和迁移数据均未自动删除。' })
  if (job.value.last_error) items.push({ key: 'last-error', level: 'danger', title: '最近一次错误', description: '请根据错误信息修复环境或数据后再执行恢复。', detail: job.value.last_error })
  if (job.value.status === 'paused_row_conflict') items.push({
    key: 'row-conflict', level: 'danger', title: `表 ${job.value.conflict_table || '—'} 发生行定位冲突`,
    description: `${(job.value.conflict_action || '').toUpperCase()} 无法唯一定位更新前记录，Checkpoint 尚未推进。`,
    detail: `位点：${positionText(job.value.conflict_file, job.value.conflict_position, job.value.conflict_gtid)}\n旧行摘要：${job.value.conflict_before_hash || '—'}\n${job.value.conflict_error || ''}`,
  })
  if (job.value.blocking_ddl) items.push({ key: 'blocking-ddl', level: 'warning', title: '检测到待处理 DDL', description: '核对并在目标端手工处理后，再确认继续增量同步。', detail: job.value.blocking_ddl })
  if (job.value.status === 'ready_with_warnings') items.push({ key: 'cutover-warning', level: 'warning', title: '切换前仍有风险项', description: `当前包含 ${job.value.warning_count || 0} 条警告和 ${job.value.excluded_table_count || 0} 张排除表，请核对范围后再完成切换。` })
  return items
})

function phaseClass(index: number) {
  return {
    completed: index < currentPhaseIndex.value,
    current: index === currentPhaseIndex.value,
    failed: index === currentPhaseIndex.value && ['failed', 'cutover_blocked'].includes(job.value?.status || ''),
  }
}

function hasFailureDetail(record: FailedObjectRow) { return !!record.error || !!record.ddl }
function failedCategoryText(category: string) {
  return ({ table: '表', data: '数据', primary_key: '主键', view: '视图', index: '索引', foreign_key: '外键', sequence: '序列', comment: '注释', row_count: '行数校验', cdc_compatibility: 'CDC 兼容性' } as Record<string, string>)[category] || category
}
function toggleText(key: string) { expandedText.value[key] = !expandedText.value[key] }

async function copy(value: string, label = '内容') {
  try {
    await copyText(value)
    Message.success(`${label}已复制`)
  } catch {
    Message.error('复制失败')
  }
}

async function loadBootstrapReview(current: IncrementalJob) {
  bootstrapReviewError.value = ''
  if (current.start_mode !== 'full_then_cdc' || (current.status !== 'paused_bootstrap_review' && current.excluded_table_count <= 0 && current.failed_object_count <= 0)) {
    bootstrapReview.value = null
    return
  }
  try {
    const review = (await getIncrementalBootstrapReview(current.job_id)).data
    if (!disposed && job.value?.job_id === current.job_id) bootstrapReview.value = review
  } catch (error: any) {
    if (!disposed && job.value?.job_id === current.job_id) {
      bootstrapReview.value = null
      bootstrapReviewError.value = error?.response?.data?.error || '全量审阅数据暂时无法加载'
    }
  }
}

async function loadJob(silent = false) {
  if (requestRunning || disposed) return
  requestRunning = true
  if (job.value) refreshing.value = true
  else loading.value = true
  if (!silent) loadError.value = ''
  try {
    const current = (await getIncrementalJob(jobID.value)).data
    if (disposed) return
    job.value = current
    loadError.value = ''
    await loadBootstrapReview(current)
  } catch (error: any) {
    if (!silent) loadError.value = error?.response?.data?.error || '增量任务不存在或无权访问'
  } finally {
    requestRunning = false
    loading.value = false
    refreshing.value = false
  }
}

async function act(action: () => Promise<unknown>, success: string) {
  if (!job.value) return
  acting.value = true
  try {
    await action()
    logRefreshToken.value++
    Message.success(success)
    await loadJob()
  } catch (error: any) {
    Message.error(error?.response?.data?.error || '操作失败')
  } finally {
    acting.value = false
  }
}

const pause = () => act(() => pauseIncrementalJob(job.value!.job_id), '正在安全暂停')
const resume = () => act(() => resumeIncrementalJob(job.value!.job_id), '任务已恢复')
const ack = () => act(() => acknowledgeIncrementalDDL(job.value!.job_id), 'DDL 已确认')
const prepare = () => act(() => prepareIncrementalCutover(job.value!.job_id), '已锁定最终位点，正在追赶和校验')
const cancelCutover = () => act(() => cancelIncrementalCutover(job.value!.job_id), '已取消切换并继续同步')
const complete = () => act(() => stopIncrementalJob(job.value!.job_id, job.value!.status === 'ready_with_warnings', job.value!.excluded_table_count > 0), '迁移闭环已安全完成')
const abort = () => act(() => abortIncrementalJob(job.value!.job_id), '任务已放弃')

async function acceptReview() {
  if (!job.value || !bootstrapReview.value) return
  acceptingReview.value = true
  try {
    await acceptIncrementalBootstrapExclusions(job.value.job_id, bootstrapReview.value.manifest_hash)
    logRefreshToken.value++
    reviewAccepted.value = false
    Message.success('已确认排除失败表，正在追赶 binlog')
    await loadJob()
  } catch (error: any) {
    Message.error(error?.response?.data?.error || '确认失败表排除失败')
  } finally {
    acceptingReview.value = false
  }
}

async function exportFailedDDL() {
  if (!job.value) return
  exportingFailedDDL.value = true
  try {
    await downloadIncrementalFailedDDL(job.value.job_id)
    Message.success('修复 SQL 已导出')
  } catch (error: any) {
    Message.error(error?.message || '导出修复 SQL 失败')
  } finally {
    exportingFailedDDL.value = false
  }
}

function backToHistory() { router.push({ path: '/history', query: { tab: 'incremental' } }) }

onMounted(async () => {
  await loadJob()
  timer = window.setInterval(() => void loadJob(true), 5000)
})
onUnmounted(() => {
  disposed = true
  if (timer) clearInterval(timer)
})
</script>

<style scoped>
.detail-page { width: min(100%, 1440px); margin: 0 auto; padding-bottom: 40px; }
.page-loading { display: flex; justify-content: center; padding: 120px 0; }
.task-header { position: sticky; z-index: 20; top: 0; display: flex; align-items: flex-start; justify-content: space-between; gap: 24px; padding: 16px 20px; margin: -16px -4px 18px; border: 1px solid var(--border); border-radius: var(--radius-md); background: rgba(255, 255, 255, 0.96); box-shadow: var(--shadow-sm); backdrop-filter: blur(12px); }
.task-heading { min-width: 0; }
.back-button { padding-left: 0; margin-bottom: 4px; color: var(--fg-muted); }
.title-line { display: flex; align-items: center; flex-wrap: wrap; gap: 10px; }
.title-line h1 { margin: 0; color: var(--fg-primary); font-size: 23px; line-height: 1.35; }
.refreshing { color: var(--fg-muted); font-size: 12px; }
.job-identity { display: flex; align-items: center; gap: 2px; min-width: 0; margin-top: 5px; }
.job-identity code { overflow: hidden; color: var(--fg-secondary); font-family: var(--font-mono); font-size: 12px; text-overflow: ellipsis; white-space: nowrap; }
.task-summary { max-width: 760px; margin: 6px 0 0; color: var(--fg-muted); line-height: 1.55; overflow-wrap: anywhere; }
.task-actions { display: flex; flex-wrap: wrap; justify-content: flex-end; gap: 8px; padding-top: 28px; }
.phase-surface { display: flex; align-items: flex-start; padding: 18px 24px; margin-top: 16px; border: 1px solid var(--border); border-radius: var(--radius-md); background: var(--bg-surface); }
.phase-item { display: grid; position: relative; flex: 1; grid-template-rows: 24px auto; justify-items: center; min-width: 0; color: var(--fg-muted); }
.phase-dot { z-index: 2; display: flex; align-items: center; justify-content: center; width: 22px; height: 22px; border: 2px solid var(--border-strong); border-radius: 50%; background: white; font-size: 12px; }
.phase-connector { position: absolute; z-index: 1; top: 11px; left: calc(50% + 11px); width: calc(100% - 22px); height: 2px; background: var(--border); }
.phase-name { margin-top: 6px; font-size: 12px; font-weight: 600; text-align: center; }
.phase-item.completed .phase-dot, .phase-item.completed .phase-connector { border-color: var(--accent); background: var(--accent); color: white; }
.phase-item.current { color: var(--accent-indigo); }
.phase-item.current .phase-dot { border-color: var(--accent-indigo); box-shadow: 0 0 0 4px var(--accent-indigo-dim); }
.phase-item.failed { color: var(--destructive); }
.phase-item.failed .phase-dot { border-color: var(--destructive); box-shadow: 0 0 0 4px rgba(220, 38, 38, .08); }
.metrics-grid { display: grid; grid-template-columns: repeat(6, minmax(0, 1fr)); margin-top: 16px; border: 1px solid var(--border); border-radius: var(--radius-md); background: var(--bg-surface); box-shadow: var(--shadow-sm); }
.metric-item { display: flex; flex-direction: column; min-width: 0; padding: 17px 18px; border-right: 1px solid var(--border); }
.metric-item:last-child { border-right: 0; }
.metric-highlight { background: linear-gradient(135deg, rgba(34,197,94,.08), transparent); }
.metric-label { color: var(--fg-muted); font-size: 11px; font-weight: 600; }
.metric-item strong { margin: 7px 0 4px; color: var(--fg-primary); font-family: var(--font-mono); font-size: 18px; overflow-wrap: anywhere; }
.metric-item .metric-date { font-family: var(--font-sans); font-size: 14px; }
.metric-note { color: var(--fg-muted); font-size: 11px; line-height: 1.4; }
.danger-text { color: var(--destructive) !important; }
.risk-section, .detail-tabs-surface { padding: 20px; margin-top: 16px; border: 1px solid var(--border); border-radius: var(--radius-md); background: var(--bg-surface); box-shadow: var(--shadow-sm); }
.section-heading { display: flex; align-items: center; justify-content: space-between; gap: 16px; margin-bottom: 14px; }
.section-heading.compact { margin-bottom: 10px; }
.section-heading h2, .tab-toolbar h2 { margin: 2px 0 0; font-size: 16px; }
.eyebrow { color: var(--fg-muted); font-size: 10px; font-weight: 700; letter-spacing: .1em; }
.risk-list { display: grid; gap: 10px; }
.risk-item { display: flex; gap: 12px; padding: 14px; border-left: 3px solid; background: var(--bg-surface2); }
.risk-warning { border-color: #f59e0b; }
.risk-danger { border-color: var(--destructive); }
.risk-icon { flex: none; padding-top: 1px; color: #f59e0b; }
.risk-danger .risk-icon { color: var(--destructive); }
.risk-content { min-width: 0; flex: 1; }
.risk-content p { margin: 4px 0 0; color: var(--fg-secondary); line-height: 1.55; }
.long-text-shell { margin-top: 10px; border: 1px solid var(--border); border-radius: var(--radius-sm); background: #111827; }
.long-text-shell pre { max-height: 420px; overflow: auto; padding: 12px; margin: 0; color: #e5e7eb; font-family: var(--font-mono); font-size: 12px; line-height: 1.6; white-space: pre-wrap; overflow-wrap: anywhere; }
.long-text-shell pre.collapsed { max-height: 76px; overflow: hidden; }
.long-text-actions { display: flex; justify-content: flex-end; gap: 4px; padding: 4px 8px; border-top: 1px solid #374151; }
.long-text-actions :deep(.arco-btn) { color: #93c5fd; }
.detail-tabs-surface { min-height: 360px; }
.overview-grid { display: grid; grid-template-columns: minmax(0, 1.25fr) minmax(320px, .75fr); gap: 24px; padding-top: 8px; }
.content-section { min-width: 0; }
:deep(.position-row) { display: grid; grid-template-columns: 150px minmax(0, 1fr) auto; align-items: start; gap: 12px; padding: 12px 0; border-bottom: 1px solid var(--border); }
:deep(.position-row:last-child) { border-bottom: 0; }
:deep(.position-label) { color: var(--fg-muted); font-size: 12px; font-weight: 600; }
:deep(.position-row code) { min-width: 0; color: var(--fg-secondary); font-family: var(--font-mono); font-size: 12px; line-height: 1.6; overflow-wrap: anywhere; }
.info-grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); margin: 0; border-top: 1px solid var(--border); }
.info-grid div { padding: 12px 0; border-bottom: 1px solid var(--border); }
.info-grid div:nth-child(odd) { padding-right: 14px; }
.info-grid dt { color: var(--fg-muted); font-size: 11px; }
.info-grid dd { margin: 5px 0 0; color: var(--fg-primary); font-size: 13px; overflow-wrap: anywhere; }
.tab-toolbar { display: flex; align-items: flex-start; justify-content: space-between; gap: 16px; margin: 8px 0 18px; }
.tab-toolbar p { margin: 5px 0 0; color: var(--fg-muted); }
.scope-stats { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); margin-bottom: 16px; border: 1px solid var(--border); border-radius: var(--radius-sm); }
.scope-stats div { display: flex; flex-direction: column; gap: 6px; padding: 14px; border-right: 1px solid var(--border); }
.scope-stats div:last-child { border-right: 0; }
.scope-stats span { color: var(--fg-muted); font-size: 11px; }
.scope-stats strong { font-family: var(--font-mono); font-size: 18px; }
.inline-alert { margin: 12px 0; }
.error-clamp { display: -webkit-box; overflow: hidden; line-height: 1.5; overflow-wrap: anywhere; -webkit-box-orient: vertical; -webkit-line-clamp: 2; }
.error-wrap { white-space: normal; overflow-wrap: anywhere; }
.failure-detail { display: grid; gap: 14px; padding: 10px; }
.failure-detail pre { max-height: 260px; overflow: auto; padding: 12px; border-radius: 4px; background: #111827; color: #e5e7eb; font-family: var(--font-mono); font-size: 12px; line-height: 1.6; white-space: pre-wrap; overflow-wrap: anywhere; }
.review-confirm { display: flex; align-items: center; justify-content: space-between; gap: 16px; padding: 14px; margin-top: 16px; border: 1px solid #fde68a; border-radius: var(--radius-sm); background: #fffbeb; }
.detail-log-panel { margin-top: 8px; border: 0; }
.detail-log-panel :deep(.migration-log-container) { height: clamp(420px, 58vh, 720px); }

@media (max-width: 1200px) {
  .metrics-grid { grid-template-columns: repeat(3, minmax(0, 1fr)); }
  .metric-item:nth-child(3) { border-right: 0; }
  .metric-item:nth-child(-n+3) { border-bottom: 1px solid var(--border); }
}
@media (max-width: 900px) {
  .task-header { position: static; flex-direction: column; }
  .task-actions { justify-content: flex-start; padding-top: 0; }
  .overview-grid { grid-template-columns: 1fr; }
  .scope-stats { grid-template-columns: repeat(2, minmax(0, 1fr)); }
  .scope-stats div:nth-child(2) { border-right: 0; }
  .scope-stats div:nth-child(-n+2) { border-bottom: 1px solid var(--border); }
}
@media (max-width: 680px) {
  .detail-page { padding-bottom: 24px; }
  .task-header, .risk-section, .detail-tabs-surface { padding: 16px; }
  .metrics-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); }
  .metric-item { border-bottom: 1px solid var(--border); }
  .metric-item:nth-child(2n) { border-right: 0; }
  .metric-item:nth-last-child(-n+2) { border-bottom: 0; }
  .phase-surface { overflow-x: auto; padding-inline: 12px; }
  .phase-item { min-width: 110px; }
  .review-confirm, .tab-toolbar { align-items: stretch; flex-direction: column; }
  :deep(.position-row) { grid-template-columns: 1fr auto; }
  :deep(.position-label) { grid-column: 1 / -1; }
}
</style>
