<template>
  <div class="query-center">
    <aside class="object-panel">
      <div class="panel-heading">
        <div>
          <div class="panel-title">对象浏览器</div>
          <div class="panel-subtitle">选择连接并浏览数据库对象</div>
        </div>
        <a-button size="mini" :disabled="!currentTab.connectionId" :loading="metadataLoading" @click="refreshMetadata">
          <template #icon><icon-refresh /></template>
        </a-button>
      </div>

      <div class="connection-controls">
        <a-select v-model="envFilter" placeholder="全部环境" allow-clear size="small">
          <a-option v-for="env in environments" :key="env" :value="env">{{ env }}</a-option>
        </a-select>
        <a-select
          :model-value="currentTab.connectionId"
          placeholder="选择数据库连接"
          allow-search
          size="small"
          @change="onConnectionChange"
        >
          <a-option v-for="conn in filteredConnections" :key="conn.id" :value="conn.id">
            {{ conn.name }} · {{ getDbTypeLabel(conn.db_type) }}
          </a-option>
        </a-select>
        <a-select
          :model-value="currentTab.namespace"
          :placeholder="namespaceLabel"
          allow-search
          size="small"
          :loading="metadataLoading"
          :disabled="!currentTab.connectionId"
          @change="onNamespaceChange"
        >
          <a-option v-for="name in namespaces" :key="name" :value="name">{{ name }}</a-option>
        </a-select>
        <a-input v-model="objectSearch" size="small" allow-clear placeholder="搜索表、视图或字段">
          <template #prefix><icon-search /></template>
        </a-input>
      </div>

      <div class="object-tree">
        <a-empty v-if="!currentTab.connectionId" description="请先选择连接" />
        <a-spin v-else-if="metadataLoading" />
        <template v-else>
          <div v-for="group in objectGroups" :key="group.type" class="object-group">
            <div class="group-title">
              <component :is="group.type === 'table' ? 'icon-storage' : 'icon-file'" />
              {{ group.label }}
              <span>{{ group.items.length }}</span>
            </div>
            <div v-for="object in group.items" :key="object.type + ':' + object.name" class="object-entry">
              <button class="object-row" @click="toggleObject(object)" @dblclick="insertObjectName(object.name)">
                <icon-down v-if="expandedObjects.has(objectKey(object))" />
                <icon-right v-else />
                <span :title="object.name">{{ object.name }}</span>
              </button>
              <div v-if="expandedObjects.has(objectKey(object))" class="column-list">
                <a-spin v-if="columnsLoading.has(objectKey(object))" :size="14" />
                <button
                  v-for="column in filteredColumns(object)"
                  :key="column.name"
                  class="column-row"
                  @dblclick="insertColumnName(column.name)"
                >
                  <icon-key v-if="column.primary_key" class="pk-icon" />
                  <span v-else class="column-dot"></span>
                  <span class="column-name">{{ column.name }}</span>
                  <span class="column-type">{{ column.data_type }}</span>
                </button>
              </div>
            </div>
          </div>
          <a-empty v-if="objects.length === 0" description="当前命名空间暂无表或视图" />
        </template>
      </div>
    </aside>

    <section class="workspace">
      <div class="editor-card">
        <div class="tab-strip">
          <button
            v-for="tab in tabs"
            :key="tab.id"
            class="query-tab"
            :class="{ active: tab.id === activeTabId }"
            @click="activeTabId = tab.id"
          >
            <span class="status-mark" :class="{ dirty: tab.sql.trim() }"></span>
            <span>{{ tab.title }}</span>
            <span v-if="tabs.length > 1" class="tab-close" @click.stop>
              <a-popconfirm
                v-if="needsCloseConfirmation(tab)"
                :content="closeConfirmationText(tab)"
                position="bottom"
                @ok="closeTab(tab.id)"
              >
                <icon-close />
              </a-popconfirm>
              <icon-close v-else @click="closeTab(tab.id)" />
            </span>
          </button>
          <button class="add-tab" title="新建查询" @click="newTab()"><icon-plus /></button>
        </div>

        <div class="query-toolbar">
          <div class="connection-summary">
            <a-tag v-if="activeConnection" :color="getDbTypeColor(activeConnection.db_type)" size="small">
              {{ getDbTypeLabel(activeConnection.db_type) }}
            </a-tag>
            <span>{{ activeConnection?.name || '未选择连接' }}</span>
            <span v-if="currentTab.namespace" class="namespace-path">/ {{ currentTab.namespace }}</span>
            <a-tag v-if="activeConnection?.env" size="small">{{ activeConnection.env }}</a-tag>
          </div>
          <a-space>
            <span class="shortcut">⌘/Ctrl + Enter</span>
            <a-button
              type="primary"
              size="small"
              :loading="executing"
              :disabled="!currentTab.connectionId || !currentTab.sql.trim() || anyExecuting"
              @click="executeFromToolbar"
            >
              <template #icon><icon-play-arrow /></template>
              执行
            </a-button>
          </a-space>
        </div>

        <div class="editor-area">
          <SqlEditor
            ref="editorRef"
            v-model="currentTab.sql"
            :key="currentTab.id"
            :dialect="activeDialect"
            @execute="runQuery"
          />
        </div>
      </div>

      <div class="result-card">
        <div class="result-tabs">
          <button :class="{ active: resultTab === 'result' }" @click="resultTab = 'result'">执行结果</button>
          <button :class="{ active: resultTab === 'history' }" @click="openHistory">
            执行历史
          </button>
          <div class="result-meta">
            <template v-if="lastResult">
              <span>{{ lastResult.duration_ms }} ms</span>
              <span v-if="lastResult.kind === 'rows'">{{ lastResult.row_count }} 行</span>
              <span v-else>影响 {{ lastResult.affected_rows }} 行</span>
            </template>
          </div>
        </div>

        <div v-if="resultTab === 'result'" class="result-content">
          <a-alert v-if="executionError" type="error" closable @close="executionError = ''">
            {{ executionError }}
          </a-alert>
          <a-alert v-else-if="lastResult?.kind === 'rows' && lastResult.truncated" type="warning">
            结果已达到 1000 行上限，仅展示前 1000 行。
          </a-alert>
          <div v-if="executing" class="result-empty"><a-spin tip="正在执行 SQL…" /></div>
          <div v-else-if="lastResult?.kind === 'rows'" class="result-grid-wrap">
            <table class="result-grid">
              <thead>
                <tr>
                  <th class="row-number">#</th>
                  <th v-for="(column, index) in lastResult.columns" :key="index">
                    <span>{{ column.name }}</span>
                    <small>{{ column.type }}</small>
                  </th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="(row, rowIndex) in lastResult.rows" :key="rowIndex">
                  <td class="row-number">{{ rowIndex + 1 }}</td>
                  <td v-for="(value, columnIndex) in row" :key="columnIndex" :title="displayValue(value)">
                    <span v-if="value === null" class="null-value">NULL</span>
                    <span v-else>{{ displayValue(value) }}</span>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
          <div v-else-if="lastResult?.kind === 'command'" class="command-result">
            <icon-check-circle-fill />
            <div>
              <strong>执行成功</strong>
              <span>影响 {{ lastResult.affected_rows }} 行，用时 {{ lastResult.duration_ms }} ms</span>
            </div>
          </div>
          <div v-else-if="!executionError" class="result-empty">
            <icon-code-square />
            <span>执行 SQL 后，结果会显示在这里</span>
          </div>
        </div>

        <div v-else class="history-content">
          <div class="history-toolbar">
            <a-radio-group v-if="isAdmin" v-model="historyScope" size="small" type="button" @change="loadHistory">
              <a-radio value="mine">我的记录</a-radio>
              <a-radio value="all">全部记录</a-radio>
            </a-radio-group>
            <span v-else>最近 50 条执行记录</span>
            <a-button size="mini" :loading="historyLoading" @click="loadHistory">
              <template #icon><icon-refresh /></template>
              刷新
            </a-button>
          </div>
          <a-table :data="history" :loading="historyLoading" :pagination="false" row-key="id" size="small">
            <template #columns>
              <a-table-column title="状态" :width="76">
                <template #cell="{ record }">
                  <a-tag :color="record.status === 'success' ? 'green' : 'red'" size="small">
                    {{ record.status === 'success' ? '成功' : '失败' }}
                  </a-tag>
                </template>
              </a-table-column>
              <a-table-column v-if="isAdmin && historyScope === 'all'" title="用户" data-index="username" :width="100" />
              <a-table-column title="连接" :width="150">
                <template #cell="{ record }">
                  <div class="history-connection">{{ record.connection_name }}<small>{{ record.namespace }}</small></div>
                </template>
              </a-table-column>
              <a-table-column title="SQL">
                <template #cell="{ record }"><code class="history-sql">{{ record.sql }}</code></template>
              </a-table-column>
              <a-table-column title="类型" data-index="statement_type" :width="90" />
              <a-table-column title="耗时" :width="85">
                <template #cell="{ record }">{{ record.duration_ms }} ms</template>
              </a-table-column>
              <a-table-column title="时间" :width="170">
                <template #cell="{ record }">{{ formatDate(record.created_at) }}</template>
              </a-table-column>
              <a-table-column title="操作" :width="80">
                <template #cell="{ record }">
                  <a-button size="mini" @click="restoreHistory(record)">再次编辑</a-button>
                </template>
              </a-table-column>
            </template>
          </a-table>
        </div>
      </div>
    </section>

    <a-modal
      v-model:visible="confirmationVisible"
      title="确认执行风险 SQL"
      :mask-closable="false"
      :footer="false"
      width="560px"
      @cancel="clearPendingConfirmation"
    >
      <a-alert :type="confirmation?.risk_level === 'dangerous' ? 'error' : 'warning'">
        {{ confirmation?.risk_level === 'dangerous' ? '该语句可能修改数据库结构或执行高风险操作。' : '该语句会修改数据库数据。' }}
      </a-alert>
      <div class="confirm-context">
        <span>连接：<strong>{{ confirmation?.connection_name }}</strong></span>
        <span>语句类型：{{ confirmation?.statement_type?.toUpperCase() }}</span>
      </div>
      <pre class="confirm-sql">{{ pendingSQL }}</pre>
      <div v-if="confirmation?.confirmation_mode === 'type_connection_name'" class="typed-confirm">
        <label>这是生产环境连接，请输入连接名称 <strong>{{ confirmation.connection_name }}</strong>：</label>
        <a-input v-model="confirmationText" placeholder="输入连接名称以确认" />
      </div>
      <div class="modal-actions">
        <a-button @click="confirmationVisible = false">取消</a-button>
        <a-button
          type="primary"
          status="danger"
          :loading="confirmationExecuting"
          :disabled="confirmation?.confirmation_mode === 'type_connection_name' && confirmationText !== confirmation.connection_name"
          @click="confirmAndExecute"
        >
          确认执行
        </a-button>
      </div>
    </a-modal>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { Message } from '@arco-design/web-vue'
import type { AxiosError } from 'axios'
import SqlEditor from '@/components/SqlEditor.vue'
import { listConnections, type Connection } from '@/api/connections'
import {
  executeQuery,
  listQueryColumns,
  listQueryHistory,
  listQueryNamespaces,
  listQueryObjects,
  type ConfirmationRequired,
  type QueryAudit,
  type QueryColumn,
  type QueryObject,
  type QueryResult,
} from '@/api/query'
import { getDbTypeColor, getDbTypeLabel } from '@/utils/dbType'
import { useAuthStore } from '@/stores/auth'

interface QueryTab {
  id: string
  title: string
  sql: string
  connectionId?: number
  namespace: string
}

interface TabRuntimeState {
  result: QueryResult | null
  error: string
  resultPane: 'result' | 'history'
  executing: boolean
}

interface PendingExecution {
  tabId: string
  connectionId: number
  namespace: string
  sql: string
}

const SESSION_KEY = 'dbgold-query-center-tabs-v1'
const auth = useAuthStore()
const isAdmin = computed(() => auth.user?.role === 'admin')
const connections = ref<Connection[]>([])
const envFilter = ref<string>()
const namespaces = ref<string[]>([])
const objects = ref<QueryObject[]>([])
const objectSearch = ref('')
const metadataLoading = ref(false)
const columnsByObject = reactive<Record<string, QueryColumn[]>>({})
const columnsLoading = reactive(new Set<string>())
const expandedObjects = reactive(new Set<string>())
let tabSequence = 1
const initialTab = makeTab()
const tabs = ref<QueryTab[]>([initialTab])
const activeTabId = ref(initialTab.id)
const tabRuntime = reactive<Record<string, TabRuntimeState>>({
  [initialTab.id]: createTabRuntime(),
})
const editorRef = ref<InstanceType<typeof SqlEditor> | null>(null)
const runningTabId = ref<string | null>(null)
const history = ref<QueryAudit[]>([])
const historyLoading = ref(false)
const historyScope = ref<'mine' | 'all'>('mine')
const confirmationVisible = ref(false)
const confirmation = ref<ConfirmationRequired | null>(null)
const confirmationText = ref('')
const pendingExecution = ref<PendingExecution | null>(null)

const currentTab = computed(() => tabs.value.find((tab) => tab.id === activeTabId.value) ?? tabs.value[0])
const currentRuntime = computed(() => ensureTabRuntime(activeTabId.value))
const lastResult = computed({
  get: () => currentRuntime.value.result,
  set: (value: QueryResult | null) => { currentRuntime.value.result = value },
})
const executionError = computed({
  get: () => currentRuntime.value.error,
  set: (value: string) => { currentRuntime.value.error = value },
})
const resultTab = computed({
  get: () => currentRuntime.value.resultPane,
  set: (value: 'result' | 'history') => { currentRuntime.value.resultPane = value },
})
const executing = computed(() => currentRuntime.value.executing)
const anyExecuting = computed(() => runningTabId.value !== null)
const pendingSQL = computed(() => pendingExecution.value?.sql ?? '')
const confirmationExecuting = computed(() => {
  const tabId = pendingExecution.value?.tabId
  return tabId ? (tabRuntime[tabId]?.executing ?? false) : false
})
const activeConnection = computed(() => connections.value.find((conn) => conn.id === currentTab.value?.connectionId))
const activeDialect = computed<'mysql' | 'postgres' | 'gaussdb'>(() => {
  const type = activeConnection.value?.db_type
  return type === 'mysql' || type === 'gaussdb' ? type : 'postgres'
})
const namespaceLabel = computed(() => activeConnection.value?.db_type === 'mysql' ? '选择数据库' : '选择 Schema')
const environments = computed(() => Array.from(new Set(connections.value.map((item) => item.env).filter(Boolean))).sort())
const filteredConnections = computed(() => connections.value.filter((item) => !envFilter.value || item.env === envFilter.value))

const objectGroups = computed(() => {
  const keyword = objectSearch.value.toLowerCase()
  const filter = (type: QueryObject['type']) => objects.value.filter((object) => {
    if (object.type !== type) return false
    if (!keyword) return true
    if (object.name.toLowerCase().includes(keyword)) return true
    return (columnsByObject[objectKey(object)] ?? []).some((column) => column.name.toLowerCase().includes(keyword))
  })
  return [
    { type: 'table' as const, label: '数据表', items: filter('table') },
    { type: 'view' as const, label: '视图', items: filter('view') },
  ].filter((group) => group.items.length > 0)
})

function makeTab(overrides: Partial<QueryTab> = {}): QueryTab {
  const id = 'query-' + Date.now() + '-' + tabSequence
  const title = '查询 ' + tabSequence
  tabSequence += 1
  return { id, title, sql: '', namespace: '', ...overrides }
}

function createTabRuntime(): TabRuntimeState {
  return { result: null, error: '', resultPane: 'result', executing: false }
}

function ensureTabRuntime(tabId: string): TabRuntimeState {
  if (!tabRuntime[tabId]) tabRuntime[tabId] = createTabRuntime()
  return tabRuntime[tabId]
}

function resetTabRuntime() {
  Object.keys(tabRuntime).forEach((tabId) => delete tabRuntime[tabId])
  tabs.value.forEach((tab) => { tabRuntime[tab.id] = createTabRuntime() })
}

function needsCloseConfirmation(tab: QueryTab): boolean {
  const runtime = tabRuntime[tab.id]
  return Boolean(
    tab.sql.trim()
    || runtime?.result
    || runtime?.error
    || runtime?.executing,
  )
}

function closeConfirmationText(tab: QueryTab): string {
  const runtime = tabRuntime[tab.id]
  if (runtime?.executing) {
    return '查询仍在执行。关闭后不会主动取消数据库端查询，返回结果将被忽略，确认关闭？'
  }
  const hasSQL = Boolean(tab.sql.trim())
  const hasExecutionState = Boolean(runtime?.result || runtime?.error)
  if (hasSQL && hasExecutionState) return '关闭后将丢失此 Tab 的 SQL 内容和执行结果，确认关闭？'
  if (hasSQL) return '关闭后将丢失此 Tab 中的 SQL 内容，确认关闭？'
  return '关闭后将丢失此 Tab 的执行结果，确认关闭？'
}

function newTab(overrides: Partial<QueryTab> = {}) {
  const source = currentTab.value
  const tab = makeTab({
    connectionId: source?.connectionId,
    namespace: source?.namespace ?? '',
    ...overrides,
  })
  tabs.value.push(tab)
  tabRuntime[tab.id] = createTabRuntime()
  activeTabId.value = tab.id
}

function closeTab(id: string) {
  const index = tabs.value.findIndex((tab) => tab.id === id)
  if (index < 0 || tabs.value.length === 1) return
  if (pendingExecution.value?.tabId === id) clearPendingConfirmation()
  delete tabRuntime[id]
  tabs.value.splice(index, 1)
  if (activeTabId.value === id) activeTabId.value = tabs.value[Math.max(0, index - 1)].id
}

async function onConnectionChange(value: unknown) {
  if (!currentTab.value || typeof value !== 'number') return
  currentTab.value.connectionId = value
  currentTab.value.namespace = ''
  await loadNamespaces()
}

async function onNamespaceChange(value: unknown) {
  if (!currentTab.value || typeof value !== 'string') return
  currentTab.value.namespace = value
  await loadObjects()
}

async function loadNamespaces() {
  const tab = currentTab.value
  if (!tab?.connectionId) return
  metadataLoading.value = true
  objects.value = []
  namespaces.value = []
  clearObjectState()
  try {
    const { data } = await listQueryNamespaces(tab.connectionId)
    namespaces.value = data
    const conn = connections.value.find((item) => item.id === tab.connectionId)
    const preferred = tab.namespace || (conn?.db_type === 'mysql' ? conn.database : 'public')
    tab.namespace = data.includes(preferred) ? preferred : (data[0] ?? '')
    if (tab.namespace) await loadObjects(false)
  } catch (error) {
    Message.error(errorMessage(error, '加载数据库命名空间失败'))
  } finally {
    metadataLoading.value = false
  }
}

async function loadObjects(showLoading = true) {
  const tab = currentTab.value
  if (!tab?.connectionId || !tab.namespace) return
  if (showLoading) metadataLoading.value = true
  clearObjectState()
  try {
    const { data } = await listQueryObjects(tab.connectionId, tab.namespace)
    objects.value = data
  } catch (error) {
    Message.error(errorMessage(error, '加载对象列表失败'))
  } finally {
    if (showLoading) metadataLoading.value = false
  }
}

async function refreshMetadata() {
  if (!currentTab.value?.connectionId) return
  await loadNamespaces()
}

function clearObjectState() {
  objects.value = []
  expandedObjects.clear()
  columnsLoading.clear()
  Object.keys(columnsByObject).forEach((key) => delete columnsByObject[key])
}

function objectKey(object: QueryObject) {
  return object.type + ':' + object.name
}

async function toggleObject(object: QueryObject) {
  const key = objectKey(object)
  if (expandedObjects.has(key)) {
    expandedObjects.delete(key)
    return
  }
  expandedObjects.add(key)
  if (columnsByObject[key] || !currentTab.value?.connectionId) return
  columnsLoading.add(key)
  try {
    const { data } = await listQueryColumns(currentTab.value.connectionId, currentTab.value.namespace, object.name)
    columnsByObject[key] = data
  } catch (error) {
    Message.error(errorMessage(error, '加载字段失败'))
  } finally {
    columnsLoading.delete(key)
  }
}

function filteredColumns(object: QueryObject) {
  const list = columnsByObject[objectKey(object)] ?? []
  const keyword = objectSearch.value.toLowerCase()
  return keyword ? list.filter((column) => column.name.toLowerCase().includes(keyword)) : list
}

function quoteIdentifier(name: string) {
  if (activeDialect.value === 'mysql') return '`' + name.split('`').join('``') + '`'
  return '"' + name.split('"').join('""') + '"'
}

function insertObjectName(name: string) {
  if (!currentTab.value) return
  const qualified = quoteIdentifier(currentTab.value.namespace) + '.' + quoteIdentifier(name)
  currentTab.value.sql += (currentTab.value.sql && !currentTab.value.sql.endsWith(' ') ? ' ' : '') + qualified
}

function insertColumnName(name: string) {
  if (!currentTab.value) return
  currentTab.value.sql += (currentTab.value.sql && !currentTab.value.sql.endsWith(' ') ? ' ' : '') + quoteIdentifier(name)
}

function executeFromToolbar() {
  runQuery(editorRef.value?.getExecutableSQL() ?? currentTab.value.sql)
}

async function runQuery(sql: string, confirmed = false, confirmationInput = '', source?: PendingExecution) {
  const tab = currentTab.value
  const target: PendingExecution | null = source ?? (tab?.connectionId ? {
    tabId: tab.id,
    connectionId: tab.connectionId,
    namespace: tab.namespace,
    sql,
  } : null)
  if (!target || !sql.trim() || anyExecuting.value || !tabs.value.some((item) => item.id === target.tabId)) return
  const runtime = ensureTabRuntime(target.tabId)
  runningTabId.value = target.tabId
  runtime.executing = true
  runtime.error = ''
  runtime.resultPane = 'result'
  if (!confirmed) runtime.result = null
  try {
    const { data } = await executeQuery({
      connection_id: target.connectionId,
      namespace: target.namespace,
      sql: target.sql,
      confirmed,
      confirmation_text: confirmationInput,
    })
    if (!tabs.value.some((item) => item.id === target.tabId) || !tabRuntime[target.tabId]) return
    tabRuntime[target.tabId].result = data
    confirmationVisible.value = false
    pendingExecution.value = null
    await loadHistory()
  } catch (error) {
    if (!tabs.value.some((item) => item.id === target.tabId) || !tabRuntime[target.tabId]) return
    const axiosError = error as AxiosError<ConfirmationRequired & { audit_id?: number }>
    if (axiosError.response?.status === 409 && axiosError.response.data.code === 'confirmation_required') {
      confirmation.value = axiosError.response.data
      pendingExecution.value = target
      confirmationText.value = ''
      confirmationVisible.value = true
    } else {
      tabRuntime[target.tabId].error = errorMessage(error, 'SQL 执行失败')
      if (confirmed) {
        confirmation.value = null
        confirmationText.value = ''
        pendingExecution.value = null
      }
      await loadHistory()
    }
  } finally {
    if (tabRuntime[target.tabId]) tabRuntime[target.tabId].executing = false
    if (runningTabId.value === target.tabId) runningTabId.value = null
  }
}

async function confirmAndExecute() {
  const pending = pendingExecution.value
  if (!pending || !tabs.value.some((tab) => tab.id === pending.tabId)) {
    clearPendingConfirmation()
    return
  }
  confirmationVisible.value = false
  await runQuery(pending.sql, true, confirmationText.value, pending)
}

function clearPendingConfirmation() {
  confirmationVisible.value = false
  confirmation.value = null
  confirmationText.value = ''
  pendingExecution.value = null
}

async function openHistory() {
  resultTab.value = 'history'
  await loadHistory()
}

async function loadHistory() {
  historyLoading.value = true
  try {
    const { data } = await listQueryHistory({ scope: historyScope.value, limit: 50 })
    history.value = data
  } catch (error) {
    if (resultTab.value === 'history') Message.error(errorMessage(error, '加载执行历史失败'))
  } finally {
    historyLoading.value = false
  }
}

function restoreHistory(record: QueryAudit) {
  const connectionExists = connections.value.some((item) => item.id === record.connection_id)
  newTab({
    title: record.statement_type.toUpperCase(),
    sql: record.sql,
    connectionId: connectionExists ? record.connection_id : undefined,
    namespace: connectionExists ? record.namespace : '',
  })
  resultTab.value = 'result'
  if (!connectionExists) Message.warning('原连接已不存在，请重新选择连接')
}

function displayValue(value: unknown) {
  if (value === null) return 'NULL'
  if (typeof value === 'object') return JSON.stringify(value)
  return String(value)
}

function formatDate(value: string) {
  return new Date(value).toLocaleString('zh-CN', { hour12: false })
}

function errorMessage(error: unknown, fallback: string) {
  const axiosError = error as AxiosError<{ error?: string }>
  return axiosError.response?.data?.error || fallback
}

function restoreSession() {
  try {
    const saved = JSON.parse(sessionStorage.getItem(SESSION_KEY) ?? '{}') as { tabs?: QueryTab[]; activeTabId?: string }
    if (saved.tabs?.length) {
      tabs.value = saved.tabs
      resetTabRuntime()
      activeTabId.value = saved.tabs.some((tab) => tab.id === saved.activeTabId) ? saved.activeTabId! : saved.tabs[0].id
      tabSequence = saved.tabs.length + 1
      return
    }
  } catch {
    sessionStorage.removeItem(SESSION_KEY)
  }
  activeTabId.value = tabs.value[0].id
}

watch([tabs, activeTabId], () => {
  sessionStorage.setItem(SESSION_KEY, JSON.stringify({ tabs: tabs.value, activeTabId: activeTabId.value }))
}, { deep: true })

watch(activeTabId, async () => {
  ensureTabRuntime(activeTabId.value)
  if (currentTab.value?.connectionId) await loadNamespaces()
  else {
    namespaces.value = []
    clearObjectState()
  }
})

onMounted(async () => {
  restoreSession()
  try {
    const { data } = await listConnections()
    connections.value = data.filter((item) => ['mysql', 'postgres', 'gaussdb'].includes(item.db_type))
    const available = new Set(connections.value.map((item) => item.id))
    tabs.value.forEach((tab) => {
      if (tab.connectionId && !available.has(tab.connectionId)) {
        tab.connectionId = undefined
        tab.namespace = ''
      }
    })
    if (!currentTab.value.connectionId && connections.value[0]) currentTab.value.connectionId = connections.value[0].id
    if (currentTab.value.connectionId) await loadNamespaces()
  } catch (error) {
    Message.error(errorMessage(error, '加载连接列表失败'))
  }
})
</script>

<style scoped>
.query-center {
  display: grid;
  grid-template-columns: 280px minmax(0, 1fr);
  gap: 14px;
  height: calc(100vh - 112px);
  min-height: 620px;
}
.object-panel,
.editor-card,
.result-card {
  background: var(--bg-surface);
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
  box-shadow: var(--shadow-sm);
  overflow: hidden;
}
.object-panel { display: flex; flex-direction: column; min-width: 0; }
.panel-heading { display: flex; justify-content: space-between; align-items: center; padding: 16px; border-bottom: 1px solid var(--border); }
.panel-title { font-size: 14px; font-weight: 600; }
.panel-subtitle { margin-top: 3px; color: var(--fg-muted); font-size: 11px; }
.connection-controls { display: grid; gap: 8px; padding: 12px; border-bottom: 1px solid var(--border); background: var(--bg-surface2); }
.object-tree { flex: 1; overflow: auto; padding: 8px; }
.object-tree > .arco-spin, .object-tree > .arco-empty { display: flex; justify-content: center; margin-top: 48px; }
.object-group { margin-bottom: 10px; }
.group-title { display: flex; align-items: center; gap: 6px; padding: 7px 8px; color: var(--fg-secondary); font-size: 12px; font-weight: 600; }
.group-title span:last-child { margin-left: auto; color: var(--fg-muted); font-weight: 400; }
.object-entry { margin: 1px 0; }
.object-row, .column-row { width: 100%; border: 0; background: transparent; color: var(--fg-secondary); cursor: pointer; text-align: left; }
.object-row { display: flex; align-items: center; gap: 5px; padding: 6px 8px; border-radius: var(--radius-sm); font-size: 12px; }
.object-row:hover, .column-row:hover { background: var(--accent-indigo-dim); color: var(--accent-indigo); }
.object-row span { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.column-list { margin-left: 18px; border-left: 1px solid var(--border); }
.column-row { display: flex; align-items: center; gap: 6px; padding: 5px 8px 5px 12px; font-size: 11px; }
.column-name { overflow: hidden; text-overflow: ellipsis; }
.column-type { margin-left: auto; color: var(--fg-muted); font-family: var(--font-mono); font-size: 10px; }
.column-dot { width: 5px; height: 5px; border-radius: 50%; background: var(--border-strong); }
.pk-icon { color: #F59E0B; }
.workspace { display: grid; grid-template-rows: minmax(300px, 48%) minmax(250px, 52%); gap: 14px; min-width: 0; min-height: 0; }
.editor-card, .result-card { display: flex; flex-direction: column; min-height: 0; }
.tab-strip { height: 38px; display: flex; align-items: flex-end; padding: 0 8px; border-bottom: 1px solid var(--border); background: var(--bg-surface2); overflow-x: auto; }
.query-tab, .add-tab { height: 32px; border: 0; background: transparent; color: var(--fg-muted); cursor: pointer; }
.query-tab { display: flex; align-items: center; gap: 7px; min-width: 110px; max-width: 180px; padding: 0 10px; border-radius: 6px 6px 0 0; border: 1px solid transparent; font-size: 12px; }
.query-tab.active { background: #fff; color: var(--fg-primary); border-color: var(--border); border-bottom-color: #fff; font-weight: 500; }
.status-mark { width: 6px; height: 6px; border-radius: 50%; background: var(--border-strong); }
.status-mark.dirty { background: var(--accent); }
.tab-close { margin-left: auto; display: inline-flex; align-items: center; }
.add-tab { width: 34px; }
.query-toolbar { min-height: 46px; display: flex; justify-content: space-between; align-items: center; padding: 6px 12px; border-bottom: 1px solid var(--border); }
.connection-summary { display: flex; align-items: center; gap: 8px; min-width: 0; color: var(--fg-secondary); font-size: 12px; }
.namespace-path { color: var(--fg-muted); }
.shortcut { color: var(--fg-muted); font-size: 11px; }
.editor-area { flex: 1; min-height: 0; }
.result-tabs { min-height: 42px; display: flex; align-items: flex-end; padding: 0 12px; border-bottom: 1px solid var(--border); }
.result-tabs > button { height: 42px; padding: 0 16px; border: 0; border-bottom: 2px solid transparent; background: transparent; color: var(--fg-muted); cursor: pointer; }
.result-tabs > button.active { color: var(--accent-indigo); border-bottom-color: var(--accent-indigo); font-weight: 600; }
.result-meta { margin-left: auto; height: 42px; display: flex; align-items: center; gap: 14px; color: var(--fg-muted); font-size: 11px; }
.result-content, .history-content { flex: 1; min-height: 0; overflow: auto; position: relative; }
.result-content > .arco-alert { margin: 10px; }
.result-empty { height: 100%; display: flex; flex-direction: column; align-items: center; justify-content: center; gap: 8px; color: var(--fg-muted); }
.result-empty > svg { font-size: 32px; color: var(--border-strong); }
.result-grid-wrap { height: 100%; overflow: auto; }
.result-grid { width: max-content; min-width: 100%; border-collapse: separate; border-spacing: 0; font-family: var(--font-mono); font-size: 11px; }
.result-grid th, .result-grid td { min-width: 120px; max-width: 360px; padding: 7px 10px; border-right: 1px solid var(--border); border-bottom: 1px solid var(--border); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; text-align: left; }
.result-grid th { position: sticky; top: 0; z-index: 1; background: var(--bg-surface2); color: var(--fg-secondary); font-family: var(--font-sans); }
.result-grid th small { display: block; margin-top: 2px; color: var(--fg-muted); font-size: 9px; font-weight: 400; }
.result-grid .row-number { min-width: 48px; width: 48px; color: var(--fg-muted); text-align: right; background: var(--bg-surface2); }
.null-value { color: #94A3B8; font-style: italic; }
.command-result { height: 100%; display: flex; align-items: center; justify-content: center; gap: 14px; color: var(--accent); }
.command-result > svg { font-size: 34px; }
.command-result div { display: flex; flex-direction: column; gap: 4px; }
.command-result span { color: var(--fg-muted); font-size: 12px; }
.history-toolbar { display: flex; justify-content: space-between; align-items: center; padding: 8px 12px; color: var(--fg-muted); font-size: 12px; }
.history-connection { display: flex; flex-direction: column; }
.history-connection small { color: var(--fg-muted); }
.history-sql { display: block; max-width: 520px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; color: var(--fg-secondary); }
.confirm-context { display: flex; gap: 20px; margin: 14px 0 8px; color: var(--fg-secondary); font-size: 12px; }
.confirm-sql { max-height: 180px; overflow: auto; margin: 0; padding: 12px; border: 1px solid var(--border); border-radius: var(--radius-sm); background: #0F172A; color: #E2E8F0; font: 12px/1.6 var(--font-mono); white-space: pre-wrap; }
.typed-confirm { display: grid; gap: 7px; margin-top: 14px; color: var(--fg-secondary); font-size: 12px; }
.modal-actions { display: flex; justify-content: flex-end; gap: 8px; margin-top: 18px; }
@media (max-width: 1100px) {
  .query-center { grid-template-columns: 240px minmax(0, 1fr); }
  .shortcut { display: none; }
}
</style>
