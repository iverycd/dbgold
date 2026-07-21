<template>
  <a-form :model="form" layout="vertical" class="incremental-panel">
    <a-alert type="warning" style="margin-bottom: 16px">
      “全量快照后持续同步”只在任务首次初始化时删除并重建所选目标表；全量完成后会立即把 checkpoint 写入目标库，恢复任务只会从 checkpoint 继续，不会再次删表。全量未完成的中断任务禁止直接恢复。
    </a-alert>

    <a-row :gutter="20">
      <a-col :span="12">
        <a-form-item label="源连接">
          <div style="width: 100%">
            <a-select
              v-model="srcEnvFilter"
              placeholder="按环境筛选"
              allow-clear
              style="width: 100%; margin-bottom: 10px"
            >
              <a-option v-for="e in envHistory" :key="e" :value="e" :label="e" />
            </a-select>
            <a-select
              v-model="form.src_conn_id"
              placeholder="选择源连接"
              allow-search
              style="width: 100%"
              @change="loadDatabases"
            >
              <a-option v-for="c in mysqlConnections" :key="c.id" :value="c.id">
                {{ c.name }} · {{ c.host }}:{{ c.port }}
              </a-option>
            </a-select>
            <div v-if="selectedSrc" class="conn-meta">
              <span class="conn-meta-item"><span class="conn-meta-label">地址</span>{{ selectedSrc.host }}:{{ selectedSrc.port }}</span>
              <span class="conn-meta-item"><span class="conn-meta-label">账号</span>{{ selectedSrc.username }}</span>
            </div>
          </div>
        </a-form-item>
        <a-form-item label="源数据库">
          <a-select v-model="form.src_database" allow-search>
            <a-option v-for="d in databases" :key="d" :value="d">{{ d }}</a-option>
          </a-select>
        </a-form-item>
      </a-col>
      <a-col :span="12">
        <a-form-item label="目标连接">
          <div style="width: 100%">
            <a-select
              v-model="dstEnvFilter"
              placeholder="按环境筛选"
              allow-clear
              style="width: 100%; margin-bottom: 10px"
            >
              <a-option v-for="e in envHistory" :key="e" :value="e" :label="e" />
            </a-select>
            <a-select
              v-model="form.dst_conn_id"
              placeholder="选择目标连接"
              allow-search
              style="width: 100%"
              @change="loadSchemas"
            >
              <a-option v-for="c in targetConnections" :key="c.id" :value="c.id">
                {{ c.name }} · {{ c.host }}:{{ c.port }}
              </a-option>
            </a-select>
            <div v-if="selectedDst" class="conn-meta">
              <span class="conn-meta-item"><span class="conn-meta-label">地址</span>{{ selectedDst.host }}:{{ selectedDst.port }}</span>
              <span class="conn-meta-item"><span class="conn-meta-label">数据库</span>{{ selectedDst.database }}</span>
              <span class="conn-meta-item"><span class="conn-meta-label">账号</span>{{ selectedDst.username }}</span>
            </div>
          </div>
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
      <a-button type="primary" :loading="starting" :disabled="!preflightResult?.ok" @click="start">启动增量任务</a-button>
    </a-space>

    <a-card v-if="preflightResult" title="预检结果" style="margin-bottom: 16px">
      <a-descriptions :column="5" size="small">
        <a-descriptions-item label="log_bin">{{ preflightResult.log_bin ? 'ON' : 'OFF' }}</a-descriptions-item>
        <a-descriptions-item label="format">{{ preflightResult.binlog_format }}</a-descriptions-item>
        <a-descriptions-item label="row image">{{ preflightResult.binlog_row_image }}</a-descriptions-item>
        <a-descriptions-item label="匹配表">{{ preflightResult.tables?.length || 0 }}</a-descriptions-item>
        <a-descriptions-item label="binlog 保留">{{ retentionText }}</a-descriptions-item>
      </a-descriptions>
      <a-descriptions :column="3" size="small" style="margin-top: 10px">
        <a-descriptions-item label="主键定位">{{ preflightLocatorCounts.primary }}</a-descriptions-item>
        <a-descriptions-item label="唯一索引定位">{{ preflightLocatorCounts.unique }}</a-descriptions-item>
        <a-descriptions-item label="更新前整行匹配">{{ preflightLocatorCounts.fullRow }}</a-descriptions-item>
      </a-descriptions>
      <a-alert v-for="e in preflightResult.errors" :key="e" type="error" style="margin-top: 8px">{{ e }}</a-alert>
      <a-alert v-for="w in preflightResult.warnings" :key="w" type="warning" style="margin-top: 8px">{{ w }}</a-alert>
      <a-alert v-if="preflightFullRowTables.length" type="warning" style="margin-top: 8px">
        以下表将按更新前完整行同步 UPDATE/DELETE，目标端可能全表扫描：{{ preflightFullRowTables.join(', ') }}
      </a-alert>
    </a-card>

    <a-modal v-model:visible="missingTableModalVisible" title="目标表不存在" :mask-closable="false" :footer="false">
      <a-alert type="error">
        从指定位点开始不会自动建表。确认排除后，这些表后续所有 INSERT、UPDATE、DELETE 都不会同步。
      </a-alert>
      <a-checkbox-group v-model="selectedMissingTables" class="missing-table-list">
        <a-checkbox v-for="table in preflightResult?.missing_target_tables || []" :key="table.source_table" :value="table.source_table">
          <span class="missing-table-name">{{ table.source_table }}</span>
          <span class="missing-table-target">→ {{ table.target_schema }}.{{ table.target_table }}</span>
        </a-checkbox>
      </a-checkbox-group>
      <div class="missing-table-actions">
        <a-button @click="missingTableModalVisible = false">暂不处理</a-button>
        <a-button :loading="checking" @click="retryAfterRepair">我已修复，重新预检</a-button>
        <a-button type="primary" status="danger" :loading="checking" :disabled="selectedMissingTables.length === 0" @click="excludeMissingAndRetry">
          排除所选并重新预检
        </a-button>
      </div>
    </a-modal>
  </a-form>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { Message, Modal } from '@arco-design/web-vue'
import { listConnections, listConnectionDatabases, listConnectionSchemas, type Connection } from '@/api/connections'
import {
  preflightIncremental,
  startIncremental,
  type IncrementalPreflight,
  type IncrementalRequest,
} from '@/api/migration'

type IncrementalForm = Omit<IncrementalRequest, 'src_conn_id' | 'dst_conn_id'> & {
  src_conn_id?: number
  dst_conn_id?: number
}

const connections = ref<Connection[]>([])
const databases = ref<string[]>([])
const schemas = ref<string[]>([])
const srcEnvFilter = ref<string | undefined>(undefined)
const dstEnvFilter = ref<string | undefined>(undefined)
const checking = ref(false)
const starting = ref(false)
const preflightResult = ref<IncrementalPreflight | null>(null)

const form = reactive<IncrementalForm>({
  src_conn_id: undefined,
  dst_conn_id: undefined,
  src_database: '',
  target_schema: '',
  start_mode: 'full_then_cdc',
  position_mode: 'gtid',
  start_gtid: '',
  start_file: '',
  start_position: 4,
  migrate_mode: 'all',
  table_filter: '',
  excluded_tables: [],
  lower_case_names: true,
  bootstrap_failure_policy: 'review_and_exclude',
  keyless_change_policy: 'full_row_match',
})

const envHistory = computed(() => {
  const set = new Set(connections.value.map(c => c.env).filter((env): env is string => !!env))
  return Array.from(set)
})
const mysqlConnections = computed(() => connections.value.filter(c =>
  c.db_type === 'mysql' && (!srcEnvFilter.value || c.env === srcEnvFilter.value)
))
const incrementalTargetTypes = new Set(['postgres', 'gaussdb', 'highgo', 'kingbase', 'seabox'])
const targetConnections = computed(() => connections.value.filter(c =>
  incrementalTargetTypes.has(c.db_type) && (!dstEnvFilter.value || c.env === dstEnvFilter.value)
))
const selectedSrc = computed(() => connections.value.find(c => c.id === form.src_conn_id))
const selectedDst = computed(() => connections.value.find(c => c.id === form.dst_conn_id))
const ready = computed(() => !!(form.src_conn_id && form.dst_conn_id && form.src_database && form.target_schema))
const retentionText = computed(() => {
  const seconds = preflightResult.value?.binlog_retention_seconds
  if (seconds == null) return '未知'
  if (seconds === 0) return '不自动清理'
  return `${(seconds / 3600).toFixed(1)} 小时`
})
const preflightLocatorCounts = computed(() => {
  const tables = preflightResult.value?.tables || []
  return {
    primary: tables.filter(table => table.locator_strategy === 'primary_key').length,
    unique: tables.filter(table => table.locator_strategy === 'unique_key').length,
    fullRow: tables.filter(table => table.locator_strategy === 'full_row').length,
  }
})
const preflightFullRowTables = computed(() => (preflightResult.value?.tables || [])
  .filter(table => table.locator_strategy === 'full_row').map(table => table.name))

const missingTableModalVisible = ref(false)
const selectedMissingTables = ref<string[]>([])

function resetPreflightState() {
  preflightResult.value = null
  missingTableModalVisible.value = false
  selectedMissingTables.value = []
}

watch(() => [
  form.src_conn_id, form.dst_conn_id, form.src_database, form.target_schema, form.start_mode, form.position_mode,
  form.start_gtid, form.start_file, form.start_position, form.migrate_mode, form.table_filter, form.lower_case_names,
  form.bootstrap_failure_policy, form.keyless_change_policy,
], () => {
  form.excluded_tables = []
  resetPreflightState()
})
watch(() => form.excluded_tables, resetPreflightState, { deep: true })

async function loadDatabases() {
  form.src_database = ''
  if (form.src_conn_id) databases.value = (await listConnectionDatabases(form.src_conn_id)).data || []
}
async function loadSchemas() {
  form.target_schema = ''
  if (form.dst_conn_id) schemas.value = (await listConnectionSchemas(form.dst_conn_id)).data || []
}
function buildRequest(): IncrementalRequest | null {
  if (!form.src_conn_id || !form.dst_conn_id) return null
  return {
    ...form,
    src_conn_id: form.src_conn_id,
    dst_conn_id: form.dst_conn_id,
  }
}
async function preflight() {
  const request = buildRequest()
  if (!request) return
  checking.value = true
  try {
    preflightResult.value = (await preflightIncremental(request)).data
    if (preflightResult.value.missing_target_tables?.length) {
      selectedMissingTables.value = []
      missingTableModalVisible.value = true
    } else {
      missingTableModalVisible.value = false
    }
    if (preflightResult.value.ok) Message.success('预检通过')
    else Message.error('预检未通过')
  } catch (e: any) {
    preflightResult.value = e?.response?.data?.preflight || null
    Message.error(e?.response?.data?.error || '预检失败')
  } finally {
    checking.value = false
  }
}
async function retryAfterRepair() {
  form.excluded_tables = []
  missingTableModalVisible.value = false
  await preflight()
}
async function excludeMissingAndRetry() {
  form.excluded_tables = Array.from(new Set([...(form.excluded_tables || []), ...selectedMissingTables.value]))
  missingTableModalVisible.value = false
  await preflight()
}
async function start() {
  const request = buildRequest()
  if (!request) return
  starting.value = true
  try {
    await startIncremental(request)
    preflightResult.value = null
    Message.success('增量任务已启动，请到任务中心的增量迁移中查看及操作')
  } catch (e: any) {
    const errorMessage = e?.response?.data?.error || '启动失败'
    if (e?.response?.status === 409) {
      Modal.error({
        title: '无法启动增量任务',
        content: errorMessage,
        okText: '知道了',
        hideCancel: true,
        maskClosable: false,
      })
    } else {
      Message.error(errorMessage)
    }
  } finally {
    starting.value = false
  }
}

onMounted(async () => {
  try {
    connections.value = (await listConnections()).data
  } catch {
    Message.error('加载连接列表失败')
  }
})
</script>

<style scoped>
.incremental-panel { margin-top: 12px; max-width: 1180px; }
.conn-meta { display: flex; flex-wrap: wrap; gap: 6px 12px; margin-top: 8px; }
.conn-meta-item { font-size: 12px; color: var(--color-text-3); }
.conn-meta-label { color: var(--color-text-4); margin-right: 3px; }
.missing-table-list { display: flex; flex-direction: column; gap: 10px; max-height: 320px; margin-top: 16px; overflow-y: auto; }
.missing-table-name { font-family: var(--font-mono); }
.missing-table-target { margin-left: 8px; color: var(--color-text-3); font-family: var(--font-mono); font-size: 12px; }
.missing-table-actions { display: flex; justify-content: flex-end; gap: 8px; margin-top: 20px; }
</style>
