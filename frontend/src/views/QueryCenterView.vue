<template>
  <div ref="workbenchRef" class="query-center" :class="{ 'sidebar-open': sidebarOpen }">
    <aside id="query-object-panel" class="object-panel" aria-label="对象浏览器" tabindex="-1">
      <div class="panel-heading">
        <div>
          <div class="panel-title">对象浏览器</div>
          <div class="panel-subtitle">选择连接并浏览数据库对象</div>
        </div>
        <a-button size="mini" aria-label="刷新对象" title="刷新对象" :disabled="!activeConnection" :loading="metadataLoading" @click="loadNamespaces">
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
          :loading="namespaceStatus === 'loading'"
          :disabled="namespaceStatus !== 'ready' || !namespaces.length"
          @change="onNamespaceChange"
        >
          <a-option v-for="name in namespaces" :key="name" :value="name">{{ name }}</a-option>
        </a-select>
        <a-input v-model="objectSearch" size="small" allow-clear placeholder="搜索对象或已加载字段">
          <template #prefix><icon-search /></template>
        </a-input>
      </div>

      <div class="object-tree">
        <a-spin v-if="metadataLoading" />
        <a-empty v-else-if="!activeConnection" description="请先选择连接" />
        <a-alert v-else-if="metadataError" type="error">{{ metadataError }}</a-alert>
        <template v-else>
          <div v-for="group in objectGroups" :key="group.type" class="object-group">
            <div class="group-title">
              <component :is="group.type === 'table' ? 'icon-storage' : 'icon-file'" />
              {{ group.label }}
              <span>{{ group.items.length }}</span>
            </div>
            <div v-for="object in group.items" :key="object.type + ':' + object.name" class="object-entry">
              <button class="object-row" :aria-expanded="expandedObjects.has(objectKey(object))" @click="toggleObject(object)" @dblclick="insertObjectName(object.name)">
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
          <a-empty v-else-if="!objectGroups.length" description="没有匹配的对象或已加载字段" />
        </template>
      </div>
    </aside>
    <div class="object-panel-backdrop" aria-hidden="true" @click="toggleSidebar"></div>

    <section ref="workspaceRef" class="workspace" :style="{ '--query-editor-height': editorHeight }">
      <div id="query-editor-panel" class="editor-card">
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
            <a-button
              size="small"
              class="toggle-objects"
              :aria-expanded="sidebarOpen"
              aria-controls="query-object-panel"
              :aria-label="sidebarOpen ? '收起对象浏览器' : '展开对象浏览器'"
              :title="sidebarOpen ? '收起对象浏览器' : '展开对象浏览器'"
              @click="toggleSidebar"
            >
              <template #icon><icon-storage /></template>
            </a-button>
            <a-tag v-if="activeConnection" :color="getDbTypeColor(activeConnection.db_type)" size="small">
              {{ getDbTypeLabel(activeConnection.db_type) }}
            </a-tag>
            <span class="connection-name" :title="activeConnection?.name">{{ activeConnection?.name || '未选择连接' }}</span>
            <span v-if="currentTab.namespace" class="namespace-path" :title="currentTab.namespace">/ {{ currentTab.namespace }}</span>
            <a-tag v-if="activeConnection?.env" class="query-environment" :color="isProductionEnvironment(activeConnection.env) ? 'red' : undefined" size="small">
              {{ environmentLabel(activeConnection.env) }}
            </a-tag>
          </div>
          <div class="query-actions">
            <span class="shortcut">⌘/Ctrl + Enter</span>
            <a-button
              type="primary"
              size="small"
              class="execute-button"
              :loading="executing"
              :disabled="!canExecute"
              @mousedown.prevent
              @click="executeFromToolbar"
            >
              <template #icon><icon-play-arrow /></template>
              {{ hasSQLSelection ? '执行选区' : '执行当前语句' }}
            </a-button>
          </div>
        </div>

        <div class="editor-area">
          <SqlEditor
            ref="editorRef"
            v-model="currentTab.sql"
            :key="currentTab.id"
            :dialect="activeDialect"
            @selection-change="hasSQLSelection = $event"
            @execute="runQuery"
          />
        </div>
      </div>

      <div
        class="workspace-divider"
        role="separator"
        tabindex="0"
        aria-label="调整 SQL 编辑器和结果区高度"
        aria-controls="query-editor-panel"
        aria-orientation="horizontal"
        :aria-valuemin="separatorMin"
        :aria-valuemax="separatorMax"
        :aria-valuenow="separatorValue"
        @pointerdown="resizeOnPointer"
        @keydown="resizeOnKey"
      ></div>

      <div :key="currentTab.id" class="result-card">
        <div class="result-tabs">
          <div class="result-tab-buttons">
            <button :class="{ active: resultTab === 'result' }" @click="resultTab = 'result'">执行结果</button>
            <button :class="{ active: resultTab === 'history' }" @click="openHistory">执行历史</button>
          </div>
          <div v-if="resultTab === 'result' && currentRuntime.source" class="result-meta">
            <span class="result-source" :title="sourceLabel(currentRuntime.source)">{{ sourceLabel(currentRuntime.source) }}</span>
            <a-tag class="query-environment" :color="isProductionEnvironment(currentRuntime.source.env) ? 'red' : undefined" size="small">
              {{ environmentLabel(currentRuntime.source.env) }}
            </a-tag>
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
            已达到返回上限，当前结果仅包含前 {{ lastResult.row_count }} 行。
          </a-alert>
          <div v-if="executing" class="result-empty"><a-spin tip="正在执行 SQL…" /></div>
          <div v-else-if="lastResult?.kind === 'rows' && !lastResult.rows.length" class="result-empty">
            <icon-check-circle />
            <span>查询成功，返回 0 行</span>
          </div>
          <div v-else-if="lastResult?.kind === 'rows'" ref="resultGridRef" class="result-grid-wrap">
            <table class="result-grid">
              <thead>
                <tr>
                  <th class="row-number" scope="col">#</th>
                  <th v-for="(column, index) in lastResult.columns" :key="index" scope="col">
                    <span>{{ column.name }}</span>
                    <small>{{ column.type }}</small>
                  </th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="(row, rowIndex) in pageRows" :key="resultOffset + rowIndex">
                  <th class="row-number" scope="row">{{ resultOffset + rowIndex + 1 }}</th>
                  <td v-for="(value, columnIndex) in row" :key="columnIndex">
                    <button
                      type="button"
                      class="result-cell"
                      :class="{ 'null-value': value === null }"
                      :aria-label="'查看第 ' + (resultOffset + rowIndex + 1) + ' 行的 ' + lastResult.columns[columnIndex]?.name"
                      @click="openDetail(lastResult.columns[columnIndex]?.name, displayValue(value))"
                    >{{ displayValue(value) }}</button>
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
          <div v-if="lastResult?.kind === 'rows' && lastResult.rows.length > PAGE_SIZE" class="result-pagination">
            <a-pagination
              :current="currentRuntime.resultPage"
              :page-size="PAGE_SIZE"
              :total="lastResult.rows.length"
              :show-page-size="false"
              simple
              size="small"
              @change="changeResultPage"
            />
          </div>
        </div>

        <div v-else class="history-content">
          <div class="history-toolbar">
            <a-radio-group v-if="isAdmin" v-model="historyScope" size="small" type="button">
              <a-radio value="mine">我的记录</a-radio>
              <a-radio value="all">全部记录</a-radio>
            </a-radio-group>
            <span v-else>最近 {{ HISTORY_LIMIT }} 条执行记录</span>
            <a-button size="mini" :loading="historyLoading" @click="loadHistory">
              <template #icon><icon-refresh /></template>
              刷新
            </a-button>
          </div>
          <a-table
            class="history-table"
            :data="history"
            :loading="historyLoading"
            :pagination="{ current: historyPage, pageSize: PAGE_SIZE, showPageSize: false, size: 'small' }"
            :scroll="{ x: isAdmin && historyScope === 'all' ? 940 : 840 }"
            :scrollbar="false"
            row-key="id"
            size="small"
            @page-change="historyPage = $event"
          >
            <template #columns>
              <a-table-column title="状态" :width="76">
                <template #cell="{ record }">
                  <a-tag v-if="record.status === 'success'" color="green" size="small">成功</a-tag>
                  <button v-else type="button" class="history-failure" @click="showHistoryError(record)">失败详情</button>
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
      width="min(560px, calc(100vw - 32px))"
      @cancel="clearPendingConfirmation"
    >
      <a-alert :type="confirmation?.risk_level === 'dangerous' ? 'error' : 'warning'">
        {{ confirmation?.risk_level === 'dangerous' ? '该语句可能修改数据库结构或执行高风险操作。' : '该语句会修改数据库数据。' }}
      </a-alert>
      <div class="confirm-context">
        <template v-if="pendingExecution">
          <strong>{{ sourceLabel(pendingExecution.source) }}</strong>
          <a-tag class="query-environment" :color="isProductionEnvironment(pendingExecution.source.env) ? 'red' : undefined" size="small">
            {{ environmentLabel(pendingExecution.source.env) }}
          </a-tag>
        </template>
        <span>{{ confirmation?.statement_type?.toUpperCase() }}</span>
      </div>
      <pre class="confirm-sql">{{ pendingSQL }}</pre>
      <div v-if="confirmation?.confirmation_mode === 'type_connection_name'" class="typed-confirm">
        <label>这是生产环境连接，请输入连接名称 <strong>{{ confirmation.connection_name }}</strong>：</label>
        <a-input v-model="confirmationText" placeholder="输入连接名称以确认" />
      </div>
      <div class="modal-actions">
        <a-button @click="clearPendingConfirmation">取消</a-button>
        <a-button
          type="primary"
          status="danger"
          class="confirm-execute-button"
          :disabled="confirmation?.confirmation_mode === 'type_connection_name' && confirmationText !== confirmation.connection_name"
          @click="confirmAndExecute"
        >
          确认执行
        </a-button>
      </div>
    </a-modal>

    <a-modal
      :visible="Boolean(detail)"
      :title="detail?.title"
      :mask-closable="false"
      :footer="false"
      width="min(640px, calc(100vw - 32px))"
      @cancel="detail = null"
    >
      <pre v-if="detail?.text" class="query-detail-content">{{ detail.text }}</pre>
      <a-empty v-else description="空字符串" />
      <div class="modal-actions">
        <a-button @click="detail = null">关闭</a-button>
        <a-button type="primary" class="execute-button" @click="copyDetail">复制内容</a-button>
      </div>
    </a-modal>
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
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
import { copyText } from '@/utils/clipboard'
import { useAuthStore } from '@/stores/auth'
import { useQueryWorkspace } from '@/composables/useQueryWorkspace'

interface QueryTab {
  id: string
  title: string
  sql: string
  connectionId?: number
  namespace: string
}

interface TabRuntimeState {
  result: QueryResult | null
  source: ExecutionSource | null
  resultPage: number
  error: string
  resultPane: 'result' | 'history'
  executing: boolean
}

interface ExecutionSource {
  connectionId: number
  connectionName: string
  env: string
  dbType: string
  namespace: string
}

interface PendingExecution {
  tabId: string
  source: ExecutionSource
  sql: string
}

const SESSION_KEY = 'dbgold-query-center-tabs-v1'
const PAGE_SIZE = 10
const HISTORY_LIMIT = 50
const {
  workbenchRef, workspaceRef, sidebarOpen, editorHeight, separatorValue, separatorMin, separatorMax,
  toggleSidebar, resizeOnPointer, resizeOnKey,
} = useQueryWorkspace()
const auth = useAuthStore()
const isAdmin = computed(() => auth.user?.role === 'admin')
const connections = ref<Connection[]>([])
const connectionsLoaded = ref(false)
const envFilter = ref<string>()
const namespaces = ref<string[]>([])
const objects = ref<QueryObject[]>([])
const objectSearch = ref('')
const namespaceStatus = ref<'idle' | 'loading' | 'ready' | 'error'>('idle')
const objectsLoading = ref(false)
const metadataLoading = computed(() => namespaceStatus.value === 'loading' || objectsLoading.value)
const metadataError = ref('')
let metadataGeneration = 0
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
const hasSQLSelection = ref(false)
const resultGridRef = ref<HTMLElement | null>(null)
const runningTabId = ref<string | null>(null)
const history = ref<QueryAudit[]>([])
const historyLoading = ref(false)
const historyScope = ref<'mine' | 'all'>('mine')
const historyPage = ref(1)
let historyGeneration = 0
const confirmationVisible = ref(false)
const confirmation = ref<ConfirmationRequired | null>(null)
const confirmationText = ref('')
const pendingExecution = ref<PendingExecution | null>(null)
const detail = ref<{ title: string; text: string } | null>(null)

const currentTab = computed(() => tabs.value.find((tab) => tab.id === activeTabId.value) ?? tabs.value[0])
const currentRuntime = computed(() => ensureTabRuntime(activeTabId.value))
const lastResult = computed(() => currentRuntime.value.result)
const resultOffset = computed(() => (currentRuntime.value.resultPage - 1) * PAGE_SIZE)
const pageRows = computed(() => {
  const result = lastResult.value
  return result?.kind === 'rows' ? result.rows.slice(resultOffset.value, resultOffset.value + PAGE_SIZE) : []
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
const activeConnection = computed(() => connections.value.find((conn) => conn.id === currentTab.value?.connectionId))
const canExecute = computed(() =>
  Boolean(activeConnection.value) && namespaceStatus.value === 'ready' && !anyExecuting.value && !confirmationVisible.value,
)
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
  return { result: null, source: null, resultPage: 1, error: '', resultPane: 'result', executing: false }
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
  if (typeof value !== 'number' || currentTab.value.connectionId === value) return
  currentTab.value.connectionId = value
  currentTab.value.namespace = ''
  await loadNamespaces()
}

async function onNamespaceChange(value: unknown) {
  if (namespaceStatus.value !== 'ready' || typeof value !== 'string' || !namespaces.value.includes(value)) return
  currentTab.value.namespace = value
  await loadObjects()
}

async function loadNamespaces() {
  const generation = ++metadataGeneration
  const tab = currentTab.value
  const conn = activeConnection.value
  namespaces.value = []
  clearObjectState()
  if (!connectionsLoaded.value || !conn) {
    namespaceStatus.value = 'idle'
    return
  }
  namespaceStatus.value = 'loading'
  try {
    const { data } = await listQueryNamespaces(conn.id)
    if (generation !== metadataGeneration) return
    namespaces.value = data
    const preferred = tab.namespace || (conn.db_type === 'mysql' ? conn.database : 'public')
    tab.namespace = data.includes(preferred) ? preferred : (data[0] ?? '')
    namespaceStatus.value = 'ready'
  } catch (error) {
    if (generation !== metadataGeneration) return
    namespaceStatus.value = 'error'
    metadataError.value = errorMessage(error, '加载数据库命名空间失败')
    return
  }
  if (tab.namespace) await loadObjects(generation)
}

async function loadObjects(generation = ++metadataGeneration) {
  const { connectionId, namespace } = currentTab.value
  clearObjectState()
  if (!connectionId || !namespace) return
  objectsLoading.value = true
  try {
    const { data } = await listQueryObjects(connectionId, namespace)
    if (generation === metadataGeneration) objects.value = data
  } catch (error) {
    if (generation === metadataGeneration) metadataError.value = errorMessage(error, '加载对象列表失败')
  } finally {
    if (generation === metadataGeneration) objectsLoading.value = false
  }
}

function clearObjectState() {
  objects.value = []
  objectsLoading.value = false
  metadataError.value = ''
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
  const { connectionId, namespace } = currentTab.value
  if (columnsByObject[key] || columnsLoading.has(key) || !connectionId || !namespace) return
  const generation = metadataGeneration
  columnsLoading.add(key)
  try {
    const { data } = await listQueryColumns(connectionId, namespace, object.name)
    if (generation === metadataGeneration) columnsByObject[key] = data
  } catch (error) {
    if (generation === metadataGeneration) Message.error(errorMessage(error, '加载字段失败'))
  } finally {
    if (generation === metadataGeneration) columnsLoading.delete(key)
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
  if (anyExecuting.value || (!source && confirmationVisible.value)) return
  if (!sql.trim()) {
    Message.warning('当前没有可执行的 SQL')
    return
  }
  const conn = activeConnection.value
  if (!source && (!conn || namespaceStatus.value !== 'ready')) {
    Message.warning(conn ? '请先完成数据库或 Schema 加载' : '请先选择数据库连接')
    return
  }
  const target = source ?? (conn && {
    tabId: currentTab.value.id,
    source: {
      connectionId: conn.id,
      connectionName: conn.name,
      env: conn.env,
      dbType: conn.db_type,
      namespace: currentTab.value.namespace,
    },
    sql,
  })
  if (!target || !tabRuntime[target.tabId]) return
  const runtime = ensureTabRuntime(target.tabId)
  runningTabId.value = target.tabId
  runtime.executing = true
  runtime.error = ''
  runtime.resultPane = 'result'
  runtime.result = null
  runtime.resultPage = 1
  runtime.source = target.source
  try {
    const { data } = await executeQuery({
      connection_id: target.source.connectionId,
      namespace: target.source.namespace,
      sql: target.sql,
      confirmed,
      confirmation_text: confirmationInput,
    })
    if (tabRuntime[target.tabId] !== runtime) return
    runtime.result = data
    if (confirmed) clearPendingConfirmation()
  } catch (error) {
    if (tabRuntime[target.tabId] !== runtime) return
    const axiosError = error as AxiosError<ConfirmationRequired & { audit_id?: number }>
    if (axiosError?.response?.status === 409 && axiosError.response.data?.code === 'confirmation_required') {
      confirmation.value = axiosError.response.data
      pendingExecution.value = target
      confirmationText.value = ''
      confirmationVisible.value = true
    } else {
      runtime.error = errorMessage(error, 'SQL 执行失败')
      if (confirmed) clearPendingConfirmation()
    }
  } finally {
    if (tabRuntime[target.tabId] === runtime) runtime.executing = false
    if (runningTabId.value === target.tabId) runningTabId.value = null
  }
}

async function confirmAndExecute() {
  const pending = pendingExecution.value
  if (!pending || !tabs.value.some((tab) => tab.id === pending.tabId)) {
    clearPendingConfirmation()
    return
  }
  if (anyExecuting.value || (confirmation.value?.confirmation_mode === 'type_connection_name'
    && confirmationText.value !== confirmation.value.connection_name)) return
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
  const generation = ++historyGeneration
  const scope = historyScope.value
  historyLoading.value = true
  try {
    const { data } = await listQueryHistory({ scope, limit: HISTORY_LIMIT })
    if (generation !== historyGeneration) return
    const records = data ?? []
    history.value = records
    historyPage.value = Math.min(historyPage.value, Math.max(1, Math.ceil(records.length / PAGE_SIZE)))
  } catch (error) {
    if (generation === historyGeneration && resultTab.value === 'history') {
      Message.error(errorMessage(error, '加载执行历史失败'))
    }
  } finally {
    if (generation === historyGeneration) historyLoading.value = false
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

function changeResultPage(page: number) {
  currentRuntime.value.resultPage = page
  if (resultGridRef.value) resultGridRef.value.scrollTop = 0
}

function sourceLabel(source: ExecutionSource) {
  const namespace = source.namespace || (source.dbType === 'mysql' ? '默认数据库' : '默认 Schema')
  return source.connectionName + ' / ' + namespace
}

function isProductionEnvironment(env: string) {
  return /生产|prod/i.test(env)
}

function environmentLabel(env: string) {
  if (isProductionEnvironment(env) && env !== '生产') return '生产 · ' + env
  return env || '环境未标记'
}

function openDetail(title: string | undefined, text: string) {
  detail.value = { title: title || '单元格内容', text }
}

function showHistoryError(record: QueryAudit) {
  openDetail('执行失败详情', record.error || '暂无错误详情')
}

async function copyDetail() {
  if (!detail.value) return
  try {
    await copyText(detail.value.text)
    Message.success('已复制内容')
  } catch {
    Message.error('复制失败')
  }
}

function formatDate(value: string) {
  return new Date(value).toLocaleString('zh-CN', { hour12: false })
}

function errorMessage(error: unknown, fallback: string) {
  const axiosError = error as AxiosError<{ error?: string }>
  return axiosError?.response?.data?.error || fallback
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

watch(activeTabId, () => {
  ensureTabRuntime(activeTabId.value)
  hasSQLSelection.value = false
  void loadNamespaces()
  if (resultTab.value === 'history') void loadHistory()
}, { flush: 'sync' })

watch(historyScope, () => {
  history.value = []
  historyPage.value = 1
  void loadHistory()
}, { flush: 'sync' })

onMounted(async () => {
  restoreSession()
  try {
    const { data } = await listConnections()
    connections.value = data.filter((item) => ['mysql', 'postgres', 'gaussdb', 'vastbase', 'gbase'].includes(item.db_type))
    const available = new Set(connections.value.map((item) => item.id))
    tabs.value.forEach((tab) => {
      if (tab.connectionId && !available.has(tab.connectionId)) {
        tab.connectionId = undefined
        tab.namespace = ''
      }
    })
    if (!currentTab.value.connectionId && !currentTab.value.sql.trim() && connections.value[0]) {
      currentTab.value.connectionId = connections.value[0].id
    }
    connectionsLoaded.value = true
    await loadNamespaces()
  } catch (error) {
    Message.error(errorMessage(error, '加载连接列表失败'))
  }
})

onBeforeUnmount(() => {
  metadataGeneration += 1
  historyGeneration += 1
})
</script>

<style scoped src="@/styles/query-workspace.css"></style>
<style scoped>
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
.panel-heading { display: flex; justify-content: space-between; align-items: center; padding: 14px; border-bottom: 1px solid var(--border); }
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
.editor-card, .result-card { display: flex; flex-direction: column; min-height: 0; }
.tab-strip { flex: 0 0 38px; display: flex; align-items: flex-end; padding: 0 8px; border-bottom: 1px solid var(--border); background: var(--bg-surface2); overflow-x: auto; }
.query-tab, .add-tab { height: 32px; border: 0; background: transparent; color: var(--fg-muted); cursor: pointer; }
.query-tab { display: flex; align-items: center; gap: 7px; min-width: 110px; max-width: 180px; padding: 0 10px; border-radius: 6px 6px 0 0; border: 1px solid transparent; font-size: 12px; }
.query-tab.active { background: #fff; color: var(--fg-primary); border-color: var(--border); border-bottom-color: #fff; font-weight: 500; }
.status-mark { width: 6px; height: 6px; border-radius: 50%; background: var(--border-strong); }
.status-mark.dirty { background: var(--accent); }
.tab-close { margin-left: auto; display: inline-flex; align-items: center; }
.add-tab { flex: 0 0 34px; }
.query-toolbar { flex-shrink: 0; min-height: 44px; display: flex; justify-content: space-between; align-items: center; gap: 10px; padding: 6px 10px; border-bottom: 1px solid var(--border); }
.connection-summary { display: flex; align-items: center; gap: 8px; min-width: 0; color: var(--fg-secondary); font-size: 12px; }
.connection-summary > .arco-tag, .toggle-objects { flex-shrink: 0; }
.connection-name, .namespace-path, .result-source { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.connection-name { font-weight: 600; }
.namespace-path { color: var(--fg-muted); }
.query-environment.arco-tag-red { color: #991b1b; background: #fee2e2; }
.query-actions { display: flex; align-items: center; gap: 10px; flex-shrink: 0; }
.shortcut { color: var(--fg-muted); font-size: 11px; white-space: nowrap; }
.execute-button.arco-btn-primary:not(:disabled) { background: #15803d !important; border-color: #15803d !important; box-shadow: none !important; }
.execute-button.arco-btn-primary:not(:disabled):hover { background: #166534 !important; border-color: #166534 !important; }
.execute-button.arco-btn-primary:disabled,
.confirm-execute-button.arco-btn-primary:disabled { background: var(--bg-muted) !important; border-color: var(--border) !important; color: var(--fg-muted) !important; box-shadow: none !important; }
.confirm-execute-button.arco-btn-primary:not(:disabled) { background: #b91c1c !important; border-color: #b91c1c !important; box-shadow: none !important; }
.confirm-execute-button.arco-btn-primary:not(:disabled):hover { background: #991b1b !important; border-color: #991b1b !important; }
.editor-area { flex: 1; min-height: 0; }
.result-tabs { flex-shrink: 0; min-height: 39px; display: flex; flex-wrap: wrap; align-items: center; column-gap: 12px; padding: 0 10px; border-bottom: 1px solid var(--border); }
.result-tab-buttons { display: flex; align-self: stretch; }
.result-tab-buttons > button { min-height: 39px; padding: 0 12px; border: 0; border-bottom: 2px solid transparent; background: transparent; color: var(--fg-muted); cursor: pointer; white-space: nowrap; }
.result-tab-buttons > button.active { color: var(--accent-indigo); border-bottom-color: var(--accent-indigo); font-weight: 600; }
.result-meta { flex: 1; min-width: min(100%, 245px); min-height: 30px; display: flex; align-items: center; justify-content: flex-end; gap: 8px; color: var(--fg-muted); font-size: 12px; }
.result-meta > :not(.result-source) { flex-shrink: 0; white-space: nowrap; }
.result-source { color: var(--fg-secondary); }
.result-content, .history-content { flex: 1; min-height: 0; }
.result-content { display: flex; flex-direction: column; overflow: hidden; }
.result-content > .arco-alert { flex: 0 0 auto; width: auto; max-height: 65%; overflow: auto; margin: 8px 10px; }
.result-empty { flex: 1; min-height: 0; display: flex; flex-direction: column; align-items: center; justify-content: center; gap: 8px; color: var(--fg-muted); }
.result-empty > svg { font-size: 28px; color: var(--fg-muted); }
.result-grid-wrap { flex: 1; min-height: 0; overflow: auto; }
.result-grid { width: max-content; min-width: 100%; border-collapse: separate; border-spacing: 0; color: var(--fg-primary); font: 12px var(--font-mono); }
.result-grid th, .result-grid td { min-width: 108px; max-width: 300px; border-right: 1px solid var(--border); border-bottom: 1px solid var(--border); white-space: nowrap; text-align: left; }
.result-grid th { overflow: hidden; text-overflow: ellipsis; padding: 6px 10px; }
.result-grid td { padding: 0; }
.result-grid thead th { position: sticky; top: 0; z-index: 2; background: var(--bg-surface2); color: var(--fg-secondary); font-family: var(--font-sans); }
.result-grid th small { display: block; margin-top: 2px; color: var(--fg-muted); font-size: 10px; font-weight: 400; }
.result-grid .row-number { position: sticky; left: 0; z-index: 1; min-width: 42px; width: 42px; color: var(--fg-muted); text-align: right; background: var(--bg-surface2); font-weight: 500; }
.result-grid thead .row-number { z-index: 3; }
.result-grid tbody tr:nth-child(even) td { background: var(--bg-surface2); }
.result-grid tbody tr:hover td { background: var(--accent-indigo-dim); }
.result-cell { display: block; width: 100%; min-height: 30px; padding: 5px 10px; border: 0; background: transparent; color: inherit; font: inherit; text-align: left; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; cursor: pointer; }
.result-cell:hover { color: var(--accent-indigo); }
.result-cell:focus-visible { outline: 2px solid var(--accent-indigo); outline-offset: -2px; }
.result-cell.null-value { color: var(--fg-muted); font-style: italic; }
.result-pagination { flex: 0 0 auto; display: flex; justify-content: flex-end; padding: 5px 10px; border-top: 1px solid var(--border); }
.command-result { flex: 1; display: flex; align-items: center; justify-content: center; gap: 14px; color: #15803d; }
.command-result > svg { font-size: 34px; }
.command-result div { display: flex; flex-direction: column; gap: 4px; }
.command-result span { color: var(--fg-muted); font-size: 12px; }
.history-content { display: flex; flex-direction: column; overflow: hidden; }
.history-toolbar { flex: 0 0 auto; display: flex; justify-content: space-between; align-items: center; padding: 8px 12px; background: var(--bg-surface); color: var(--fg-muted); font-size: 12px; }
.history-table { flex: 1; min-height: 0; box-shadow: none !important; }
.history-table :deep(.arco-table-container) { flex: 1; min-height: 0; }
.history-table :deep(.arco-table-content) { overflow: auto; }
.history-table :deep(thead th) { position: sticky; top: 0; z-index: 1; }
.history-table :deep(.arco-table-pagination) { flex: 0 0 auto; margin: 0; padding: 5px 10px; border-top: 1px solid var(--border); }
.history-failure { border: 0; padding: 0; background: transparent; color: #b91c1c; font: inherit; cursor: pointer; white-space: nowrap; }
.history-failure:hover { text-decoration: underline; }
.history-connection { display: flex; flex-direction: column; }
.history-connection small { color: var(--fg-muted); }
.history-sql { display: block; max-width: 520px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; color: var(--fg-secondary); }
.confirm-context { display: flex; align-items: center; flex-wrap: wrap; gap: 8px; margin: 14px 0 8px; color: var(--fg-secondary); font-size: 12px; overflow-wrap: anywhere; }
.confirm-sql { max-height: 180px; overflow: auto; margin: 0; padding: 12px; border: 1px solid var(--border); border-radius: var(--radius-sm); background: #0F172A; color: #E2E8F0; font: 12px/1.6 var(--font-mono); white-space: pre-wrap; }
.typed-confirm { display: grid; gap: 7px; margin-top: 14px; color: var(--fg-secondary); font-size: 12px; }
.modal-actions { display: flex; justify-content: flex-end; gap: 8px; margin-top: 18px; }
.query-detail-content { max-height: min(55vh, 480px); overflow: auto; margin: 0; padding: 12px; border: 1px solid var(--border); border-radius: var(--radius-sm); background: var(--bg-surface2); color: var(--fg-primary); font: 12px/1.6 var(--font-mono); white-space: pre-wrap; overflow-wrap: anywhere; }
@media (max-width: 1100px) {
  .shortcut { display: none; }
}
@media (max-width: 520px) {
  .connection-summary > .arco-tag:first-of-type { display: none; }
  .query-toolbar { flex-wrap: wrap; }
}
</style>
