<template>
  <div>
    <h2>迁移 SQL 生成</h2>
    <a-tabs v-model:active-key="activeTab">
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

      <a-tab-pane key="data-migrate" title="数据迁移">
        <div style="margin-top: 12px">
          <!-- 源库 / 目标库选择 -->
          <a-row :gutter="16" style="margin-bottom: 16px">
            <a-col :span="11">
              <a-form-item label="源库（MySQL）">
                <a-select
                  v-model="dataMigrate.srcConnId"
                  placeholder="选择 MySQL 连接"
                  @change="checkPairSupport"
                >
                  <a-option
                    v-for="c in mysqlConnections"
                    :key="c.id"
                    :value="c.id"
                    :label="c.name"
                  />
                </a-select>
              </a-form-item>
            </a-col>
            <a-col :span="2" style="text-align:center;padding-top:4px;font-size:20px">→</a-col>
            <a-col :span="11">
              <a-form-item label="目标库（PostgreSQL）">
                <a-select
                  v-model="dataMigrate.dstConnId"
                  placeholder="选择 PostgreSQL 连接"
                  @change="checkPairSupport"
                >
                  <a-option
                    v-for="c in pgConnections"
                    :key="c.id"
                    :value="c.id"
                    :label="c.name"
                  />
                </a-select>
              </a-form-item>
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
            <a-input
              v-if="dataMigrate.mode !== 'all'"
              v-model="dataMigrate.filter"
              placeholder="逗号分隔表名，支持 * 通配符，如：*_log,tmp_*"
              style="margin-top: 8px"
            />
          </a-form-item>

          <!-- 高级设置 -->
          <a-collapse style="margin-bottom: 16px">
            <a-collapse-item key="advanced" header="高级设置">
              <a-row :gutter="16">
                <a-col :span="12">
                  <a-form-item label="每页行数 (pageSize)">
                    <a-input-number v-model="dataMigrate.pageSize" :min="1000" :max="500000" :step="1000" />
                  </a-form-item>
                </a-col>
                <a-col :span="12">
                  <a-form-item label="最大并发数 (maxParallel)">
                    <a-input-number v-model="dataMigrate.maxParallel" :min="1" :max="50" />
                  </a-form-item>
                </a-col>
              </a-row>
            </a-collapse-item>
          </a-collapse>

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
        </div>
      </a-tab-pane>
    </a-tabs>
  </div>
</template>

<script setup lang="ts">
import { ref, reactive, computed, nextTick, onMounted } from 'vue'
import { Message } from '@arco-design/web-vue'
import ConnectionSelect from '@/components/ConnectionSelect.vue'
import SqlPreview from '@/components/SqlPreview.vue'
import MigrationReportPanel from './MigrationReportPanel.vue'
import { runDiffMigration, runFullMigration } from '@/api/migration'
import { listConnections, type Connection } from '@/api/connections'
import {
  getSupportedPairs,
  startDataMigration as apiStartMigration,
  cancelDataMigration as apiCancelMigration,
  createDataMigrateEventSource,
  type SupportedPair,
} from '@/api/migration'

const activeTab = ref('diff')

const diffSrc = reactive({ connId: undefined as number | undefined, dbName: '' })
const diffDst = reactive({ connId: undefined as number | undefined, dbName: '' })
const diffLoading = ref(false)
const diffSqls = ref<string[]>([])

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
  mode: 'all' as 'all' | 'include' | 'exclude',
  filter: '',
  pageSize: 10000,
  maxParallel: 5,
  running: false,
  finished: false,
  logs: [] as string[],
  unsupportedMsg: '',
  currentJobId: '',
})

const mysqlConnections = computed(() =>
  connections.value.filter((c) => c.db_type === 'mysql')
)
const pgConnections = computed(() =>
  connections.value.filter((c) => c.db_type === 'postgres')
)

const canStartMigration = computed(() =>
  dataMigrate.srcConnId !== undefined &&
  dataMigrate.dstConnId !== undefined &&
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

function getLogClass(line: string): string {
  if (line.includes('[ERROR]')) return 'log-error'
  if (line.includes('[WARN]')) return 'log-warn'
  if (line.includes('[DONE]')) return 'log-done'
  return ''
}

async function startDataMigration() {
  dataMigrate.running = true
  dataMigrate.finished = false
  dataMigrate.logs = []
  try {
    const res = await apiStartMigration({
      src_conn_id: dataMigrate.srcConnId!,
      dst_conn_id: dataMigrate.dstConnId!,
      migrate_mode: dataMigrate.mode,
      table_filter: dataMigrate.filter,
      page_size: dataMigrate.pageSize,
      max_parallel: dataMigrate.maxParallel,
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

onMounted(async () => {
  const [connsRes, pairsRes] = await Promise.all([
    listConnections(),
    getSupportedPairs(),
  ])
  connections.value = connsRes.data
  supportedPairs.value = pairsRes.data
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
</style>
