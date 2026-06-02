<template>
  <div>
    <h2>迁移 SQL 生成</h2>
    <a-tabs v-model:active-key="activeTab">
      <a-tab-pane key="data-migrate" title="数据迁移">
        <a-form :model="dataMigrate" layout="vertical" style="margin-top: 12px">
          <!-- 源库 / 目标库选择 -->
          <a-row :gutter="20" align="stretch" style="margin-bottom: 16px">
            <a-col :span="11">
              <a-card class="conn-card" :body-style="{ padding: '16px' }">
                <div class="conn-card-header">
                  <a-tag color="orange" size="small">源库</a-tag>
                  <span class="conn-card-title">源库</span>
                </div>
                <a-select
                  v-model="dataMigrate.srcConnId"
                  placeholder="选择源库连接"
                  style="width: 100%; margin-top: 10px"
                  @change="(val: number) => { checkPairSupport(); loadSrcDatabases(val) }"
                >
                  <a-option v-for="c in srcConnections" :key="c.id" :value="c.id" :label="c.name" />
                </a-select>
                <div v-if="selectedSrc" class="conn-meta">
                  <span class="conn-meta-item"><span class="conn-meta-label">地址</span>{{ selectedSrc.host }}:{{ selectedSrc.port }}</span>
                  <span class="conn-meta-item"><span class="conn-meta-label">账号</span>{{ selectedSrc.username }}</span>
                </div>
                <a-select
                  v-if="dataMigrate.srcDatabases.length > 0"
                  v-model="dataMigrate.srcDatabase"
                  placeholder="选择要迁移的数据库"
                  style="width: 100%; margin-top: 10px"
                  allow-search
                >
                  <a-option v-for="db in dataMigrate.srcDatabases" :key="db" :value="db" :label="db" />
                </a-select>
              </a-card>
            </a-col>
            <a-col :span="2" style="display:flex;align-items:center;justify-content:center">
              <icon-arrow-right style="font-size: 28px; color: #165dff" />
            </a-col>
            <a-col :span="11">
              <a-card class="conn-card" :body-style="{ padding: '16px' }">
                <div class="conn-card-header">
                  <a-tag color="blue" size="small">PG / GaussDB</a-tag>
                  <span class="conn-card-title">目标库</span>
                </div>
                <a-select
                  v-model="dataMigrate.dstConnId"
                  placeholder="选择 PostgreSQL / GaussDB 连接"
                  style="width: 100%; margin-top: 10px"
                  @change="(val: number) => { checkPairSupport(); loadDstSchemas(val) }"
                >
                  <a-option v-for="c in pgConnections" :key="c.id" :value="c.id" :label="c.name" />
                </a-select>
                <div v-if="selectedDst" class="conn-meta">
                  <span class="conn-meta-item"><span class="conn-meta-label">地址</span>{{ selectedDst.host }}:{{ selectedDst.port }}</span>
                  <span class="conn-meta-item"><span class="conn-meta-label">数据库</span>{{ selectedDst.database }}</span>
                  <span class="conn-meta-item"><span class="conn-meta-label">账号</span>{{ selectedDst.username }}</span>
                </div>
                <a-select
                  v-if="dataMigrate.dstConnId"
                  v-model="dataMigrate.dstSchema"
                  placeholder="请选择目标 Schema"
                  style="width: 100%; margin-top: 10px"
                  allow-search
                >
                  <a-option v-for="s in dataMigrate.dstSchemas" :key="s" :value="s" :label="s" />
                </a-select>
                <div v-if="dataMigrate.dstConnId" class="schema-permission-tip">
                  <icon-info-circle style="flex-shrink:0" />
                  请确保目标 Schema 拥有创建对象的权限，否则请在目标数据库中自行处理模式权限后再执行迁移。
                </div>
              </a-card>
            </a-col>
          </a-row>

          <!-- 不支持提示 -->
          <a-alert
            v-if="dataMigrate.unsupportedMsg"
            type="error"
            :content="dataMigrate.unsupportedMsg"
            style="margin-bottom: 16px"
          />

          <!-- 迁移范围 -->
          <a-form-item label="迁移范围" style="margin-bottom: 16px">
            <a-radio-group v-model="dataMigrate.mode">
              <a-radio value="all">全库迁移</a-radio>
              <a-radio value="exclude">排除指定表</a-radio>
              <a-radio value="include">仅迁移指定表</a-radio>
            </a-radio-group>
            <template v-if="dataMigrate.mode !== 'all'">
              <a-input
                v-model="dataMigrate.filter"
                placeholder="逗号分隔表名，支持 * 通配符，如：*_log,tmp_*"
                style="margin-top: 8px; max-width: 400px"
                @input="validateTableFilter"
              />
              <div v-if="tableFilterError" style="color: rgb(var(--danger-6)); font-size: 12px; margin-top: 4px">
                {{ tableFilterError }}
              </div>
            </template>
          </a-form-item>

          <!-- 迁移内容 -->
          <a-form-item label="迁移内容" style="margin-bottom: 16px">
            <a-radio-group v-model="dataMigrate.content">
              <a-radio value="both">表结构 + 数据行</a-radio>
              <a-radio value="schema_only">仅创建表结构</a-radio>
              <a-radio value="data_only">仅迁移数据行</a-radio>
            </a-radio-group>
          </a-form-item>

          <!-- 高级设置 -->
          <a-collapse :default-active-key="['advanced']" style="margin-bottom: 16px; max-width: 560px">
            <a-collapse-item key="advanced" header="高级设置">
              <a-row :gutter="16">
                <a-col :span="12">
                  <a-form-item label="每页行数 (pageSize)">
                    <a-input-number v-model="dataMigrate.pageSize" :min="1000" :max="500000" :step="1000" style="width: 140px" />
                  </a-form-item>
                </a-col>
                <a-col :span="12">
                  <a-form-item label="最大并发数 (maxParallel)">
                    <a-input-number v-model="dataMigrate.maxParallel" :min="1" :max="50" style="width: 140px" />
                  </a-form-item>
                </a-col>
                <a-col :span="12">
                  <a-form-item label="表内分页并发数">
                    <a-input-number v-model="dataMigrate.intraTableParallel" :min="1" :max="20" style="width: 140px" />
                  </a-form-item>
                </a-col>
                <a-col :span="12">
                  <a-form-item style="margin-bottom: 4px">
                    <a-checkbox v-model="dataMigrate.lowerCaseNames">对象名转小写</a-checkbox>
                  </a-form-item>
                </a-col>
                <a-col :span="12">
                  <a-form-item style="margin-bottom: 4px">
                    <a-checkbox v-model="dataMigrate.charInLength">char 长度单位（CHAR）</a-checkbox>
                  </a-form-item>
                </a-col>
                <a-col :span="12">
                  <a-form-item style="margin-bottom: 4px">
                    <a-checkbox v-model="dataMigrate.useNvarchar2">使用 nvarchar2</a-checkbox>
                  </a-form-item>
                </a-col>
                <a-col :span="12">
                  <a-form-item style="margin-bottom: 4px">
                    <a-checkbox v-model="dataMigrate.distributed">分布式模式（DISTRIBUTE BY hash）</a-checkbox>
                  </a-form-item>
                </a-col>
              </a-row>
              <a-divider style="margin: 12px 0 8px">连接池配置（0 表示使用默认值）</a-divider>
              <a-row :gutter="16">
                <a-col :span="24" style="margin-bottom: 8px">
                  <span style="font-size: 12px; color: var(--color-text-3)">源库连接池</span>
                </a-col>
                <a-col :span="8">
                  <a-form-item label="最大连接数">
                    <a-input-number v-model="dataMigrate.srcMaxOpenConns" :min="0" :max="500" placeholder="默认 50" style="width: 120px" />
                  </a-form-item>
                </a-col>
                <a-col :span="8">
                  <a-form-item label="最大空闲连接数">
                    <a-input-number v-model="dataMigrate.srcMaxIdleConns" :min="0" :max="500" placeholder="默认 25" style="width: 120px" />
                  </a-form-item>
                </a-col>
                <a-col :span="8">
                  <a-form-item label="连接生命周期（秒）">
                    <a-input-number v-model="dataMigrate.srcConnMaxLifetime" :min="0" placeholder="默认 3600" style="width: 120px" />
                  </a-form-item>
                </a-col>
                <a-col :span="24" style="margin-bottom: 8px">
                  <span style="font-size: 12px; color: var(--color-text-3)">目标库连接池</span>
                </a-col>
                <a-col :span="8">
                  <a-form-item label="最大连接数">
                    <a-input-number v-model="dataMigrate.dstMaxOpenConns" :min="0" :max="500" placeholder="默认 50" style="width: 120px" />
                  </a-form-item>
                </a-col>
                <a-col :span="8">
                  <a-form-item label="最大空闲连接数">
                    <a-input-number v-model="dataMigrate.dstMaxIdleConns" :min="0" :max="500" placeholder="默认 25" style="width: 120px" />
                  </a-form-item>
                </a-col>
                <a-col :span="8">
                  <a-form-item label="连接生命周期（秒）">
                    <a-input-number v-model="dataMigrate.dstConnMaxLifetime" :min="0" placeholder="默认 3600" style="width: 120px" />
                  </a-form-item>
                </a-col>
              </a-row>
            </a-collapse-item>
          </a-collapse>

          <!-- 目标表删除重建警告 -->
          <a-alert type="warning" style="margin-bottom: 16px">
            <template #title>注意：迁移前目标表将被删除重建</template>
            迁移开始时会对目标库中同名表执行 <strong>DROP TABLE IF EXISTS ... CASCADE</strong>，再重新建表并导入数据。目标表中的现有数据将被清空，请确认目标库中无需保留的数据已备份。
          </a-alert>

          <!-- 操作按钮 -->
          <a-space style="margin-bottom: 16px">
            <a-button
              type="primary"
              :disabled="!canStartMigration"
              :loading="dataMigrate.running"
              @click="startDataMigration"
            >开始迁移</a-button>
            <a-button
              v-if="dataMigrate.running"
              status="danger"
              @click="cancelDataMigration"
            >停止迁移</a-button>
            <a-button
              v-if="dataMigrate.finished"
              @click="resetDataMigration"
            >重新迁移</a-button>
          </a-space>

          <!-- 日志区 -->
          <div v-if="dataMigrate.logs.length > 0">
            <a-space style="margin-bottom: 8px">
              <span style="font-weight:500">迁移日志</span>
              <a-button size="mini" @click="copyLogs">复制日志</a-button>
            </a-space>
            <div ref="logContainer" class="migration-log-container">
              <div
                v-for="(line, i) in dataMigrate.logs"
                :key="i"
                :class="getLogClass(line)"
                class="log-line"
              >{{ line }}</div>
            </div>
          </div>

          <!-- 迁移报告 -->
          <div v-if="dataMigrate.finished && dataMigrate.currentJobId" style="margin-top: 16px">
            <a-divider>迁移报告</a-divider>
            <MigrationReportPanel :jobID="dataMigrate.currentJobId" />
          </div>
        </a-form>
      </a-tab-pane>

      <a-tab-pane key="diff" title="Diff 迁移">
        <a-space direction="vertical" fill style="width: 100%; margin-top: 12px">
          <a-row :gutter="24">
            <a-col :span="11">
              <a-card title="源">
                <connection-select v-model:connection-id="diffSrc.connId" v-model:database="diffSrc.dbName" />
              </a-card>
            </a-col>
            <a-col :span="2" style="display:flex;align-items:center;justify-content:center">
              <icon-arrow-right style="font-size: 24px; color: #165dff" />
            </a-col>
            <a-col :span="11">
              <a-card title="目标">
                <connection-select v-model:connection-id="diffDst.connId" v-model:database="diffDst.dbName" />
              </a-card>
            </a-col>
          </a-row>
          <a-checkbox v-model="schemaMigrateLowerCase">对象名转小写</a-checkbox>
          <a-button
            type="primary"
            :loading="diffLoading"
            :disabled="!(diffSrc.connId && diffSrc.dbName && diffDst.connId && diffDst.dbName)"
            @click="handleDiffMigration"
          >
            生成迁移 SQL
          </a-button>
          <sql-preview :sqls="diffSqls" />
        </a-space>
      </a-tab-pane>

      <a-tab-pane key="full" title="全量迁移">
        <a-space direction="vertical" fill style="width: 100%; margin-top: 12px">
          <a-card title="目标数据库（将为此库生成完整建表 SQL）">
            <connection-select v-model:connection-id="fullDst.connId" v-model:database="fullDst.dbName" />
          </a-card>
          <a-checkbox v-model="schemaMigrateLowerCase">对象名转小写</a-checkbox>
          <a-button
            type="primary"
            :loading="fullLoading"
            :disabled="!(fullDst.connId && fullDst.dbName)"
            @click="handleFullMigration"
          >
            生成全量 SQL
          </a-button>
          <sql-preview :sqls="fullSqls" />
        </a-space>
      </a-tab-pane>
    </a-tabs>
  </div>
</template>

<script setup lang="ts">
import { ref, reactive, computed, nextTick, onMounted, onUnmounted } from 'vue'
import { onBeforeRouteLeave } from 'vue-router'
import { Message, Modal } from '@arco-design/web-vue'
import ConnectionSelect from '@/components/ConnectionSelect.vue'
import SqlPreview from '@/components/SqlPreview.vue'
import MigrationReportPanel from './MigrationReportPanel.vue'
import { runDiffMigration, runFullMigration } from '@/api/migration'
import { listConnections, listConnectionDatabases, listConnectionSchemas, type Connection } from '@/api/connections'
import {
  getSupportedPairs,
  startDataMigration as apiStartMigration,
  cancelDataMigration as apiCancelMigration,
  createDataMigrateEventSource,
  type SupportedPair,
} from '@/api/migration'

const activeTab = ref('data-migrate')

const diffSrc = reactive({ connId: undefined as number | undefined, dbName: '' })
const diffDst = reactive({ connId: undefined as number | undefined, dbName: '' })
const diffLoading = ref(false)
const diffSqls = ref<string[]>([])
const schemaMigrateLowerCase = ref(true)

const fullDst = reactive({ connId: undefined as number | undefined, dbName: '' })
const fullLoading = ref(false)
const fullSqls = ref<string[]>([])

async function handleDiffMigration() {
  if (!diffSrc.connId || !diffSrc.dbName || !diffDst.connId || !diffDst.dbName) return
  diffLoading.value = true
  diffSqls.value = []
  try {
    const res = await runDiffMigration({
      src_connection_id: diffSrc.connId,
      src_database: diffSrc.dbName,
      dst_connection_id: diffDst.connId,
      dst_database: diffDst.dbName,
      lower_case_names: schemaMigrateLowerCase.value,
    })
    diffSqls.value = res.data.sql_statements
    Message.success(`已生成 ${diffSqls.value.length} 条 SQL`)
  } catch {
    Message.error('生成失败')
  } finally {
    diffLoading.value = false
  }
}

async function handleFullMigration() {
  if (!fullDst.connId || !fullDst.dbName) return
  fullLoading.value = true
  fullSqls.value = []
  try {
    const res = await runFullMigration({
      dst_connection_id: fullDst.connId,
      dst_database: fullDst.dbName,
      lower_case_names: schemaMigrateLowerCase.value,
    })
    fullSqls.value = res.data.sql_statements
    Message.success(`已生成 ${fullSqls.value.length} 条 SQL`)
  } catch {
    Message.error('生成失败')
  } finally {
    fullLoading.value = false
  }
}

// ===== 数据迁移 Tab =====
const connections = ref<Connection[]>([])
const supportedPairs = ref<SupportedPair[]>([])
const logContainer = ref<HTMLElement | null>(null)
let currentEventSource: EventSource | null = null

const dataMigrate = reactive({
  srcConnId: undefined as number | undefined,
  dstConnId: undefined as number | undefined,
  srcDatabase: '',
  srcDatabases: [] as string[],
  dstSchema: '',
  dstSchemas: [] as string[],
  mode: 'all' as 'all' | 'include' | 'exclude',
  filter: '',
  content: 'both' as 'both' | 'schema_only' | 'data_only',
  pageSize: 20000,
  maxParallel: 10,
  intraTableParallel: 8,
  lowerCaseNames: true,
  charInLength: false,
  useNvarchar2: false,
  distributed: false,
  srcMaxOpenConns: 50,
  srcMaxIdleConns: 25,
  srcConnMaxLifetime: 3600,
  dstMaxOpenConns: 50,
  dstMaxIdleConns: 25,
  dstConnMaxLifetime: 3600,
  running: false,
  finished: false,
  logs: [] as string[],
  unsupportedMsg: '',
  currentJobId: '',
})

const tableFilterError = ref('')

function validateTableFilter(): boolean {
  if (!dataMigrate.filter || dataMigrate.mode === 'all') {
    tableFilterError.value = ''
    return true
  }
  const parts = dataMigrate.filter.split(',')
  for (const part of parts) {
    const trimmed = part.trim()
    if (trimmed && !/^[a-zA-Z0-9_*%\-]+$/.test(trimmed)) {
      tableFilterError.value = '表名只能包含字母、数字、下划线和通配符 *，分隔符只能使用英文逗号'
      return false
    }
  }
  tableFilterError.value = ''
  return true
}

const srcConnections = computed(() =>
  connections.value.filter(
    (c) => c.db_type === 'mysql' || c.db_type === 'sqlserver' || c.db_type === 'dameng'
  )
)
const pgConnections = computed(() =>
  connections.value.filter((c) => c.db_type === 'postgres' || c.db_type === 'gaussdb')
)
const selectedSrc = computed(() =>
  connections.value.find((c) => c.id === dataMigrate.srcConnId)
)
const selectedDst = computed(() =>
  connections.value.find((c) => c.id === dataMigrate.dstConnId)
)

const canStartMigration = computed(() =>
  dataMigrate.srcConnId !== undefined &&
  dataMigrate.dstConnId !== undefined &&
  dataMigrate.srcDatabase !== '' &&
  !dataMigrate.unsupportedMsg &&
  !dataMigrate.running
)

function checkPairSupport() {
  if (!dataMigrate.srcConnId || !dataMigrate.dstConnId) {
    dataMigrate.unsupportedMsg = ''
    return
  }
  const src = connections.value.find((c) => c.id === dataMigrate.srcConnId)
  const dst = connections.value.find((c) => c.id === dataMigrate.dstConnId)
  if (!src || !dst) return
  const supported = supportedPairs.value.some(
    (p) => p.source === src.db_type && p.target === dst.db_type
  )
  dataMigrate.unsupportedMsg = supported
    ? ''
    : `当前不支持 ${src.db_type} → ${dst.db_type} 的数据迁移`
}

async function loadSrcDatabases(connId: number) {
  dataMigrate.srcDatabase = ''
  dataMigrate.srcDatabases = []
  try {
    const res = await listConnectionDatabases(connId)
    dataMigrate.srcDatabases = res.data ?? []
  } catch {
    // 不支持列库时静默忽略，用户仍可迁移连接默认数据库
  }
}

async function loadDstSchemas(connId: number) {
  dataMigrate.dstSchema = ''
  dataMigrate.dstSchemas = []
  const dst = connections.value.find((c) => c.id === connId)
  if (!dst || (dst.db_type !== 'postgres' && dst.db_type !== 'gaussdb')) return
  try {
    const res = await listConnectionSchemas(connId)
    dataMigrate.dstSchemas = res.data ?? []
  } catch {
    // 列 schema 失败时静默忽略
  }
}

function getLogClass(line: string): string {
  if (line.includes('[ERROR]')) return 'log-error'
  if (line.includes('[WARN]')) return 'log-warn'
  if (line.includes('[DONE]')) return 'log-done'
  return ''
}

async function startDataMigration() {
  if (!validateTableFilter()) return
  if (dataMigrate.dstConnId && !dataMigrate.dstSchema) {
    Message.error('请选择目标 Schema')
    return
  }
  dataMigrate.running = true
  dataMigrate.finished = false
  dataMigrate.logs = []
  try {
    const res = await apiStartMigration({
      src_conn_id: dataMigrate.srcConnId!,
      dst_conn_id: dataMigrate.dstConnId!,
      migrate_mode: dataMigrate.mode,
      table_filter: dataMigrate.filter,
      migrate_content: dataMigrate.content,
      page_size: dataMigrate.pageSize,
      max_parallel: dataMigrate.maxParallel,
      intra_table_parallel: dataMigrate.intraTableParallel,
      lower_case_names: dataMigrate.lowerCaseNames,
      char_in_length: dataMigrate.charInLength,
      use_nvarchar2: dataMigrate.useNvarchar2,
      distributed: dataMigrate.distributed,
      src_database: dataMigrate.srcDatabase,
      target_schema: dataMigrate.dstSchema || undefined,
      src_max_open_conns: dataMigrate.srcMaxOpenConns || undefined,
      src_max_idle_conns: dataMigrate.srcMaxIdleConns || undefined,
      src_conn_max_lifetime: dataMigrate.srcConnMaxLifetime || undefined,
      dst_max_open_conns: dataMigrate.dstMaxOpenConns || undefined,
      dst_max_idle_conns: dataMigrate.dstMaxIdleConns || undefined,
      dst_conn_max_lifetime: dataMigrate.dstConnMaxLifetime || undefined,
    })
    dataMigrate.currentJobId = res.data.job_id
    connectSSE(res.data.job_id)
  } catch (e: any) {
    dataMigrate.logs.push(`[ERROR] 启动失败: ${e?.response?.data?.error ?? e?.message ?? e}`)
    dataMigrate.running = false
    dataMigrate.finished = true
  }
}

function connectSSE(jobID: string) {
  currentEventSource = createDataMigrateEventSource(jobID)
  currentEventSource.addEventListener('message', (e) => {
    if (e.data === '[STREAM_END]') {
      dataMigrate.running = false
      dataMigrate.finished = true
      currentEventSource?.close()
      currentEventSource = null
      return
    }
    dataMigrate.logs.push(e.data)
    nextTick(() => {
      if (logContainer.value) {
        logContainer.value.scrollTop = logContainer.value.scrollHeight
      }
    })
  })
  currentEventSource.onerror = () => {
    dataMigrate.running = false
    dataMigrate.finished = true
    currentEventSource?.close()
    currentEventSource = null
  }
}

async function cancelDataMigration() {
  if (!dataMigrate.currentJobId) return
  try {
    await apiCancelMigration(dataMigrate.currentJobId)
  } catch {
    // 取消失败时 SSE 自然会断开
  }
}

function resetDataMigration() {
  dataMigrate.running = false
  dataMigrate.finished = false
  dataMigrate.logs = []
  dataMigrate.currentJobId = ''
}

function copyLogs() {
  navigator.clipboard.writeText(dataMigrate.logs.join('\n'))
}

function handleBeforeUnload(e: BeforeUnloadEvent) {
  if (dataMigrate.running) {
    e.preventDefault()
    e.returnValue = ''
  }
}

onBeforeRouteLeave(() => {
  if (!dataMigrate.running) return true
  return new Promise<boolean>((resolve) => {
    Modal.confirm({
      title: '迁移正在进行中',
      content: '离开页面后迁移将继续在后台运行，但您将无法在此页面查看进度。确定要离开吗？',
      okText: '确定离开',
      cancelText: '留在此页',
      maskClosable: false,
      onOk: () => resolve(true),
      onCancel: () => resolve(false),
    })
  })
})

onMounted(async () => {
  window.addEventListener('beforeunload', handleBeforeUnload)
  const [connsRes, pairsRes] = await Promise.all([
    listConnections(),
    getSupportedPairs(),
  ])
  connections.value = connsRes.data
  supportedPairs.value = pairsRes.data
})

onUnmounted(() => {
  window.removeEventListener('beforeunload', handleBeforeUnload)
  currentEventSource?.close()
  currentEventSource = null
})
</script>

<style scoped>
.migration-log-container {
  background: #1a1a1a;
  color: #d4d4d4;
  font-family: 'Menlo', 'Monaco', 'Courier New', monospace;
  font-size: 12px;
  padding: 12px;
  border-radius: 4px;
  height: 400px;
  overflow-y: auto;
}
.log-line {
  line-height: 1.6;
  white-space: pre-wrap;
  word-break: break-all;
}
.log-error { color: #f47174; }
.log-warn  { color: #e5c07b; }
.log-done  { color: #98c379; }
.conn-card {
  border: 1px solid var(--color-border-2);
  border-radius: 6px;
  height: 100%;
}
.conn-card-header {
  display: flex;
  align-items: center;
  gap: 8px;
}
.conn-card-title {
  font-size: 14px;
  font-weight: 600;
  color: var(--color-text-1);
}
.conn-meta {
  display: flex;
  flex-wrap: wrap;
  gap: 6px 12px;
  margin-top: 8px;
}
.conn-meta-item {
  font-size: 12px;
  color: var(--color-text-3);
}
.conn-meta-label {
  color: var(--color-text-4);
  margin-right: 3px;
}
.schema-permission-tip {
  display: flex;
  align-items: flex-start;
  gap: 5px;
  margin-top: 10px;
  padding: 7px 10px;
  background: #fffbe6;
  border: 1px solid #ffe58f;
  border-radius: 4px;
  font-size: 12px;
  color: #7d5a00;
  line-height: 1.5;
}
</style>
