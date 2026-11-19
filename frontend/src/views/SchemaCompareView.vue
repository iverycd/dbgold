<template>
  <div class="schema-workspace">
    <section class="schema-surface" aria-label="比对范围">
      <a-alert v-if="setupError" type="error">
        {{ setupError }}
        <a-button type="text" size="small" @click="loadSetup">重新加载</a-button>
      </a-alert>
      <a-form :model="form" layout="vertical" :disabled="busy || initializing" @submit="runCompare">
        <div class="schema-route">
          <div v-for="side in sides" :key="side.key" class="schema-side">
            <h2>{{ side.title }}</h2>
            <a-form-item label="连接" required>
              <a-select
                :model-value="form[side.key].connectionId"
                :disabled="side.key === 'target' && !sourceConnection"
                :loading="initializing"
                :placeholder="side.key === 'target' && !sourceConnection ? '请先选择源库连接' : '选择数据库连接'"
                allow-search
                allow-clear
                @change="value => selectConnection(side.key, typeof value === 'number' ? value : undefined)"
              >
                <a-option v-for="conn in sideConnections[side.key]" :key="conn.id" :value="conn.id" :label="conn.name">
                  <a-tag :color="getDbTypeColor(conn.db_type)" size="small">{{ getDbTypeLabel(conn.db_type) }}</a-tag>
                  {{ conn.name }}<span v-if="conn.env" class="connection-env"> · {{ conn.env }}</span>
                </a-option>
              </a-select>
            </a-form-item>
            <p v-if="chosenConnections[side.key]" class="connection-meta">
              <span>{{ chosenConnections[side.key]?.host }}:{{ chosenConnections[side.key]?.port }}</span>
              <span v-if="chosenConnections[side.key]?.database">{{ chosenConnections[side.key]?.database }}</span>
              <span>{{ chosenConnections[side.key]?.username }}</span>
            </p>
            <a-form-item :label="chosenConnections[side.key]?.db_type === 'mysql' ? '数据库' : 'Schema'" required>
              <a-select
                v-model="form[side.key].namespace"
                :disabled="!form[side.key].connectionId || !namespaceOptions[side.key].length"
                :loading="namespaceLoading[side.key]"
                :placeholder="form[side.key].connectionId ? '选择比对范围' : '请先选择连接'"
                allow-search
              >
                <a-option v-for="name in namespaceOptions[side.key]" :key="name" :value="name">{{ name }}</a-option>
              </a-select>
            </a-form-item>
            <a-alert v-if="namespaceErrors[side.key]" type="error">
              {{ namespaceErrors[side.key] }}
              <a-button type="text" size="small" @click="loadNamespaces(side.key)">重试</a-button>
            </a-alert>
          </div>
        </div>

        <a-form-item label="指定表">
          <a-input-tag
            v-model="form.tables"
            v-model:input-value="form.tableInput"
            :retain-input-value="{ blur: true }"
            placeholder="留空比对全部表，输入原始表名后按回车添加"
            allow-clear
            @blur="commitTableInput"
          />
        </a-form-item>
        <a-form-item v-if="preserveCaseAllowed || damengTarget" label="结构映射">
          <a-space wrap>
            <a-checkbox v-if="preserveCaseAllowed" v-model="form.preserveCase">保留源对象名称大小写</a-checkbox>
            <template v-if="damengTarget">
              <a-checkbox v-model="form.charInLength">字符长度使用 CHAR 单位</a-checkbox>
              <a-checkbox v-model="form.useNvarchar2">使用 NVARCHAR</a-checkbox>
              <span class="muted">达梦对象名称按大写比对。</span>
            </template>
          </a-space>
        </a-form-item>

        <div class="schema-actions">
          <a-button type="primary" html-type="submit" :loading="comparing" :disabled="!canCompare">
            <template #icon><icon-search /></template>
            {{ plan || applyResults.length ? '重新比对' : '开始比对' }}
          </a-button>
          <span v-if="sameScope" class="muted">请选择不同的源端和目标端范围。</span>
        </div>
      </a-form>
    </section>

    <a-alert v-if="compareError" type="error">{{ compareError }}</a-alert>

    <section v-if="plan" class="schema-surface" aria-label="差异明细">
      <div class="schema-heading">
        <div>
          <h2>差异明细</h2>
          <span class="muted">共 {{ rows.length }} 项 · {{ applicableCount }} 项可应用 · {{ rows.length - applicableCount }} 项需人工处理</span>
        </div>
        <a-button v-if="applicableCount" type="primary" :disabled="!canPreview" @click="openPreview">
          预览并应用（{{ selectedChanges.length }}）
        </a-button>
      </div>
      <a-alert v-if="planUsed" type="info" class="schema-notice">本次预览已提交。重新比对可查看目标库的最新结构。</a-alert>
      <a-alert v-else-if="expired" type="warning" class="schema-notice">比对结果已过期，请重新比对。</a-alert>
      <a-empty v-if="!rows.length" description="所选范围内未发现表结构差异" />
      <template v-else>
        <div class="schema-filters">
          <a-input v-model="search" placeholder="搜索表名或对象名" allow-clear aria-label="搜索表名或对象名">
            <template #prefix><icon-search /></template>
          </a-input>
          <a-select v-model="statusFilter" aria-label="筛选差异状态">
            <a-option value="all">全部状态</a-option>
            <a-option value="applicable">可应用</a-option>
            <a-option value="manual">需人工处理</a-option>
          </a-select>
          <a-button v-if="selectedKeys.length" size="small" :disabled="busy" @click="selectedKeys = []">清除选择</a-button>
        </div>
        <a-alert v-if="missingDependencies.length" type="warning" class="schema-notice">
          还需选择 {{ missingDependencies.length }} 项前置差异。可在差异详情中核对前置项。
          <a-button type="text" size="small" :disabled="busy" @click="selectDependencies">一并选中前置差异</a-button>
        </a-alert>
        <a-table
          v-model:selected-keys="selectedKeys"
          :row-selection="planUsed || expired ? undefined : { type: 'checkbox', onlyCurrent: false, showCheckedAll: true }"
          :data="visibleRows"
          row-key="id"
          :pagination="{ current: page, pageSize: 10, showPageSize: false, showTotal: true }"
          :scroll="{ x: 840 }"
          size="small"
          @page-change="page = $event"
        >
          <template #columns>
            <a-table-column title="表" data-index="table" :width="180" />
            <a-table-column title="对象" :width="150">
              <template #cell="{ record }">{{ record.object || '—' }}</template>
            </a-table-column>
            <a-table-column title="差异类型" :width="140">
              <template #cell="{ record }">{{ kindLabel(record.kind) }}</template>
            </a-table-column>
            <a-table-column title="处理方式" :width="130">
              <template #cell="{ record }">
                <a-tag :color="record.status === 'applicable' ? 'green' : 'orange'">{{ record.status === 'applicable' ? '可应用' : '需人工处理' }}</a-tag>
              </template>
            </a-table-column>
            <a-table-column title="说明" data-index="reason" />
            <a-table-column title="操作" :width="85">
              <template #cell="{ record }">
                <a-button type="text" size="small" @click="openDetails(record)">详情</a-button>
              </template>
            </a-table-column>
          </template>
        </a-table>
      </template>
    </section>

    <a-alert v-if="applyError" type="error">{{ applyError }}</a-alert>

    <section v-if="applyResults.length" class="schema-surface" aria-label="应用结果">
      <div class="schema-heading">
        <div>
          <h2>应用结果</h2>
          <span class="muted">成功 {{ appliedCount }} 项 · 失败 {{ failedCount }} 项 · 跳过 {{ skippedCount }} 项 · 待核实 {{ unknownCount }} 项</span>
        </div>
      </div>
      <a-table :data="applyResults" row-key="id" :pagination="{ pageSize: 10, showPageSize: false, showTotal: true }" :scroll="{ x: 680 }" size="small">
        <template #columns>
          <a-table-column title="表" data-index="table" :width="160" />
          <a-table-column title="对象" :width="140">
            <template #cell="{ record }">{{ record.object || '—' }}</template>
          </a-table-column>
          <a-table-column title="差异类型" :width="140">
            <template #cell="{ record }">{{ kindLabel(record.kind) }}</template>
          </a-table-column>
          <a-table-column title="执行状态" :width="100">
            <template #cell="{ record }">
              <a-tag :color="resultBadge(record.status).color">{{ resultBadge(record.status).label }}</a-tag>
            </template>
          </a-table-column>
          <a-table-column title="原因" data-index="error" />
        </template>
      </a-table>
    </section>

    <a-drawer
      v-model:visible="detailVisible"
      :width="detailWidth"
      title="差异详情"
      :footer="false"
      @close="activeChange = null"
    >
      <template v-if="activeChange">
        <div class="schema-heading">
          <h2>{{ changeTitle(activeChange) }}</h2>
          <a-tag>{{ kindLabel(activeChange.kind) }}</a-tag>
        </div>
        <a-alert v-if="activeChange.reason" :type="activeChange.status === 'manual' ? 'warning' : 'info'" class="schema-notice">{{ activeChange.reason }}</a-alert>
        <p v-if="activeChange.depends_on?.length" class="notice-detail">依赖 {{ activeChange.depends_on.map(dependencyLabel).join('、') }}</p>
        <div class="schema-definitions">
          <div v-for="definition in definitions" :key="definition.label">
            <h3>{{ definition.label }}</h3>
            <pre>{{ definition.value }}</pre>
          </div>
        </div>
        <template v-if="activeChange.sql?.length">
          <div class="schema-heading sql-heading">
            <h3>目标库 DDL</h3>
            <a-button type="text" size="small" @click="copySQL(activeChange.sql)">复制 SQL</a-button>
          </div>
          <pre>{{ sqlText(activeChange.sql) }}</pre>
        </template>
      </template>
    </a-drawer>

    <a-modal
      v-model:visible="previewVisible"
      :width="detailWidth"
      title="确认应用到目标库"
      :mask-closable="false"
      :closable="!applying"
      :esc-to-close="!applying"
      :footer="false"
    >
      <div v-if="targetConnection" class="preview-target">
        <strong>{{ targetConnection.name }}</strong>
        <a-tag :color="getDbTypeColor(targetConnection.db_type)">{{ getDbTypeLabel(targetConnection.db_type) }}</a-tag>
        <a-tag v-if="targetConnection.env">{{ targetConnection.env }}</a-tag>
        <span>{{ targetConnection.host }}:{{ targetConnection.port }}</span>
        <span v-if="targetConnection.db_type !== 'mysql' && targetConnection.database">{{ targetConnection.database }} / {{ form.target.namespace }}</span>
        <span v-else>{{ form.target.namespace }}</span>
      </div>
      <p class="notice-detail">将按下列顺序执行 {{ selectedChanges.length }} 项差异。请核对目标库及 DDL。</p>
      <a-alert v-if="expired" type="warning" class="schema-notice">比对结果已过期，请重新比对。</a-alert>
      <a-table :data="selectedChanges" row-key="id" :pagination="{ pageSize: 10, showPageSize: false, showTotal: true }" size="small">
        <template #columns>
          <a-table-column title="差异" :width="170">
            <template #cell="{ record }">
              <strong>{{ changeTitle(record) }}</strong>
              <div class="muted">{{ kindLabel(record.kind) }}</div>
            </template>
          </a-table-column>
          <a-table-column title="执行 SQL">
            <template #cell="{ record }"><pre>{{ sqlText(record.sql) }}</pre></template>
          </a-table-column>
        </template>
      </a-table>
      <div class="preview-actions">
        <a-button type="text" :disabled="applying" @click="copySQL(selectedChanges.flatMap(change => change.sql ?? []))">复制全部 SQL</a-button>
        <div class="schema-actions">
          <a-button :disabled="applying" @click="previewVisible = false">取消</a-button>
          <a-button type="primary" :loading="applying" :disabled="!canPreview" @click="runApply">确认应用</a-button>
        </div>
      </div>
    </a-modal>
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { Message } from '@arco-design/web-vue'
import { listConnections, listConnectionSchemas, type Connection } from '@/api/connections'
import {
  applySchemaChanges, compareSchema, getSchemaComparePairs,
  type SchemaApplyResult, type SchemaChange, type SchemaComparePair, type SchemaComparePlan,
} from '@/api/schema'
import { copyText } from '@/utils/clipboard'
import { isPostgresDatabase } from '@/utils/connectionDatabase'
import { getDbTypeColor, getDbTypeLabel } from '@/utils/dbType'

const sides = [{ key: 'source', title: '源库' }, { key: 'target', title: '目标库' }] as const
type Side = typeof sides[number]['key']
type Scope = { connectionId?: number; namespace: string }
const form = reactive({
  source: { namespace: '' } as Scope,
  target: { namespace: '' } as Scope,
  tables: [] as string[],
  tableInput: '',
  preserveCase: false,
  charInLength: false,
  useNvarchar2: false,
})
const namespaceOptions = reactive<Record<Side, string[]>>({ source: [], target: [] })
const namespaceLoading = reactive<Record<Side, boolean>>({ source: false, target: false })
const namespaceErrors = reactive<Record<Side, string>>({ source: '', target: '' })
const namespaceRequest = { source: 0, target: 0 }
const connections = ref<Connection[]>([])
const pairs = ref<SchemaComparePair[]>([])
const initializing = ref(false)
const setupError = ref('')
const compareError = ref('')
const applyError = ref('')
const comparing = ref(false)
const applying = ref(false)
const activeChange = ref<SchemaChange | null>(null)
const detailVisible = ref(false)
const plan = ref<SchemaComparePlan | null>(null)
const planUsed = ref(false)
const clock = ref(Date.now())
const selectedKeys = ref<(number | string)[]>([])
const search = ref('')
const statusFilter = ref('all')
const page = ref(1)
const previewVisible = ref(false)
const applyResults = ref<SchemaApplyResult[]>([])
const detailWidth = 'min(860px, 96vw)'

const sourceConnection = computed(() => connections.value.find(conn => conn.id === form.source.connectionId))
const targetConnection = computed(() => connections.value.find(conn => conn.id === form.target.connectionId))
const chosenConnections = computed(() => ({ source: sourceConnection.value, target: targetConnection.value }))
const sideConnections = computed(() => ({
  source: connections.value.filter(conn => pairs.value.some(pair => pair.source === conn.db_type)),
  target: connections.value.filter(conn => pairs.value.some(pair => pair.source === sourceConnection.value?.db_type && pair.target === conn.db_type)),
}))
const preserveCaseAllowed = computed(() => sourceConnection.value?.db_type === 'mysql' && isPostgresDatabase(targetConnection.value?.db_type ?? ''))
const damengTarget = computed(() => targetConnection.value?.db_type === 'dameng')
const sameScope = computed(() => !!form.source.namespace && form.source.connectionId === form.target.connectionId && form.source.namespace === form.target.namespace)
const busy = computed(() => comparing.value || applying.value)
const canCompare = computed(() => !!sourceConnection.value && !!targetConnection.value && !!form.source.namespace && !!form.target.namespace && !sameScope.value && !busy.value)
const rows = computed(() => (plan.value?.changes ?? []).map(change => ({ ...change, disabled: change.status !== 'applicable' })))
const applicableCount = computed(() => rows.value.filter(row => !row.disabled).length)
const changeById = computed(() => new Map(rows.value.map(row => [row.id, row])))
const expired = computed(() => !!plan.value && Date.parse(plan.value.expires_at) <= clock.value)
const visibleRows = computed(() => {
  const needle = search.value.trim().toLocaleLowerCase()
  return rows.value.filter(row => (statusFilter.value === 'all' || row.status === statusFilter.value)
    && (row.table.toLocaleLowerCase().includes(needle) || row.object?.toLocaleLowerCase().includes(needle)))
})
const selectedChanges = computed(() => {
  const selected = new Set(selectedKeys.value)
  return rows.value.filter(row => !row.disabled && selected.has(row.id))
    .sort((left, right) => executionOrder(left) - executionOrder(right))
})
const missingDependencies = computed(() => {
  const selected = new Set(selectedChanges.value.map(change => change.id))
  const missing = new Set<string>()
  const pending = selectedChanges.value.flatMap(change => change.depends_on ?? [])
  for (const id of pending) {
    if (selected.has(id) || missing.has(id)) continue
    missing.add(id)
    pending.push(...(changeById.value.get(id)?.depends_on ?? []))
  }
  return [...missing]
})
const canPreview = computed(() => !!plan.value?.plan_id && !!selectedChanges.value.length && !missingDependencies.value.length && !busy.value && !expired.value && !planUsed.value)
const definitions = computed(() => [
  { label: '源库定义', value: formatDefinition(activeChange.value?.source) },
  { label: '预期目标定义', value: formatDefinition(activeChange.value?.expected) },
  { label: '当前目标定义', value: formatDefinition(activeChange.value?.target) },
])
const appliedCount = computed(() => applyResults.value.filter(result => result.status === 'applied').length)
const failedCount = computed(() => applyResults.value.filter(result => result.status === 'failed').length)
const skippedCount = computed(() => applyResults.value.filter(result => result.status === 'skipped').length)
const unknownCount = computed(() => applyResults.value.length - appliedCount.value - failedCount.value - skippedCount.value)

watch([search, statusFilter], () => { page.value = 1 })
watch(form, invalidatePlan, { flush: 'sync' })

let timer: ReturnType<typeof setInterval>
onMounted(() => {
  void loadSetup()
  timer = setInterval(() => { clock.value = Date.now() }, 1000)
})
onBeforeUnmount(() => clearInterval(timer))

async function loadSetup() {
  initializing.value = true
  setupError.value = ''
  try {
    const [connectionResponse, pairResponse] = await Promise.all([listConnections(), getSchemaComparePairs()])
    connections.value = connectionResponse.data ?? []
    pairs.value = pairResponse.data ?? []
    if (!sideConnections.value.source.length) setupError.value = '没有可用于结构比对的源库连接，请先在连接管理中添加受支持的连接。'
  } catch (error) {
    setupError.value = messageFor(error, '加载连接或支持的组合失败')
  } finally {
    initializing.value = false
  }
}

async function selectConnection(side: Side, id?: number) {
  form[side].connectionId = id
  if (side === 'source' && !sideConnections.value.target.some(conn => conn.id === form.target.connectionId)) {
    form.target.connectionId = undefined
    void loadNamespaces('target')
  }
  await loadNamespaces(side)
}

async function loadNamespaces(side: Side) {
  const id = form[side].connectionId
  const request = ++namespaceRequest[side]
  form[side].namespace = ''
  namespaceOptions[side] = []
  namespaceErrors[side] = ''
  namespaceLoading[side] = !!id
  if (!id) return
  try {
    const { data } = await listConnectionSchemas(id)
    if (namespaceRequest[side] !== request) return
    namespaceOptions[side] = data ?? []
    if (namespaceOptions[side].length === 1) form[side].namespace = namespaceOptions[side][0]
    if (!namespaceOptions[side].length) namespaceErrors[side] = '此连接没有可用的比对范围。'
  } catch (error) {
    if (namespaceRequest[side] === request) namespaceErrors[side] = messageFor(error, '加载比对范围失败')
  } finally {
    if (namespaceRequest[side] === request) namespaceLoading[side] = false
  }
}

function invalidatePlan() {
  plan.value = null
  planUsed.value = false
  selectedKeys.value = []
  search.value = ''
  statusFilter.value = 'all'
  page.value = 1
  activeChange.value = null
  detailVisible.value = false
  previewVisible.value = false
  applyResults.value = []
  compareError.value = ''
  applyError.value = ''
}

async function runCompare() {
  if (!canCompare.value || !sourceConnection.value || !targetConnection.value) return
  commitTableInput()
  const request = {
    source: { connection_id: sourceConnection.value.id, namespace: form.source.namespace },
    target: { connection_id: targetConnection.value.id, namespace: form.target.namespace },
    tables: form.tables.length ? [...form.tables] : undefined,
    preserve_case: preserveCaseAllowed.value && form.preserveCase,
    char_in_length: damengTarget.value && form.charInLength,
    use_nvarchar2: damengTarget.value && form.useNvarchar2,
  }
  invalidatePlan()
  comparing.value = true
  try {
    const { data } = await compareSchema(request)
    clock.value = Date.now()
    plan.value = data
  } catch (error) {
    compareError.value = messageFor(error, '表结构比对失败')
  } finally {
    comparing.value = false
  }
}

function commitTableInput() {
  const name = form.tableInput
  if (name && !form.tables.includes(name)) form.tables.push(name)
  form.tableInput = ''
}

function selectDependencies() {
  const dependencies = missingDependencies.value.filter(id => changeById.value.get(id)?.status === 'applicable')
  selectedKeys.value = [...new Set([...selectedKeys.value, ...dependencies])]
}

function executionOrder(change: SchemaChange) {
  return change.execution_order ?? change.order ?? 0
}

function openDetails(change: SchemaChange) {
  activeChange.value = change
  detailVisible.value = true
}

function openPreview() {
  clock.value = Date.now()
  if (canPreview.value) previewVisible.value = true
}

async function runApply() {
  clock.value = Date.now()
  if (!canPreview.value || !plan.value || !previewVisible.value) return
  applying.value = true
  applyError.value = ''
  try {
    const { data } = await applySchemaChanges(plan.value.plan_id, selectedChanges.value.map(change => change.id))
    applyResults.value = data.results ?? []
  } catch (error) {
    applyError.value = messageFor(error, '应用请求未返回执行结果，请重新比对确认目标库结构')
  } finally {
    planUsed.value = true
    selectedKeys.value = []
    previewVisible.value = false
    applying.value = false
  }
}

const kindLabels: Record<string, string> = {
  table_missing: '目标缺表', table_extra: '目标额外表', table_unsupported: '结构待核查', table_definition: '表定义不同',
  column_missing: '目标缺列', column_extra: '目标额外列', column_definition: '列定义不同',
  primary: '主键不同', unique: '唯一约束不同', foreign: '外键不同', index: '索引不同', check: '检查约束待核查',
  table_comment: '表注释不同', column_comment: '列注释不同',
}

function kindLabel(kind: string) {
  return kindLabels[kind] ?? kind
}

function changeTitle(change: SchemaChange) {
  return change.object ? `${change.table} · ${change.object}` : change.table
}

function dependencyLabel(id: string) {
  const change = changeById.value.get(id)
  return change ? `${changeTitle(change)}（${kindLabel(change.kind)}）` : id
}

function formatDefinition(value: unknown) {
  if (value == null || value === '') return '—'
  return typeof value === 'string' ? value : JSON.stringify(value, null, 2)
}

function resultBadge(status: SchemaApplyResult['status']) {
  return {
    applied: { label: '已应用', color: 'green' },
    failed: { label: '失败', color: 'red' },
    skipped: { label: '已跳过', color: 'gray' },
    unknown: { label: '待核实', color: 'orange' },
  }[status]
}

function sqlText(statements?: string[]) {
  return (statements ?? []).map(statement => statement.trimEnd().endsWith(';') ? statement : `${statement};`).join('\n\n')
}

async function copySQL(statements?: string[]) {
  try {
    await copyText(sqlText(statements))
    Message.success('SQL 已复制')
  } catch {
    Message.error('复制 SQL 失败')
  }
}

function messageFor(error: unknown, fallback: string) {
  const response = (error as { response?: { data?: { error?: unknown } } } | undefined)?.response
  return typeof response?.data?.error === 'string' ? response.data.error : fallback
}
</script>

<style scoped>
.schema-workspace { display: grid; gap: 16px; align-content: start; }
.schema-surface { padding: 20px; background: var(--bg-surface); border: 1px solid var(--border); border-radius: var(--radius-md); box-shadow: var(--shadow-sm); min-width: 0; }
.schema-route, .schema-definitions { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 16px; margin-bottom: 16px; }
.schema-side { padding: 16px; background: var(--bg-surface2); border: 1px solid var(--border); border-radius: var(--radius-sm); }
.schema-side :deep(.arco-form-item:last-child) { margin-bottom: 0; }
h2, h3 { margin: 0 0 8px; font-size: 15px; color: var(--fg-primary); overflow-wrap: anywhere; }
h3 { font-size: 13px; }
.schema-side h2 { margin-bottom: 12px; }
.schema-actions, .preview-actions, .schema-heading, .schema-filters, .preview-target { display: flex; align-items: center; flex-wrap: wrap; gap: 10px; }
.schema-heading, .preview-actions { justify-content: space-between; }
.schema-heading, .preview-actions, .schema-filters, .schema-notice { margin-bottom: 12px; }
.preview-actions { margin-top: 16px; margin-bottom: 0; }
.schema-heading h2, .schema-heading h3 { margin-bottom: 0; }
.schema-heading h2 + .muted { display: inline-block; margin-top: 5px; }
.schema-filters :deep(.arco-input-wrapper), .schema-filters :deep(.arco-select) { width: auto; }
.schema-filters :deep(.arco-input-wrapper) { min-width: 220px; }
.muted, .connection-meta, .connection-env { color: var(--fg-muted); font-size: 12px; }
.connection-meta { display: flex; flex-wrap: wrap; gap: 0 12px; margin: -10px 0 12px; overflow-wrap: anywhere; }
.notice-detail { margin: 8px 0 12px; color: var(--fg-secondary); overflow-wrap: anywhere; }
.schema-definitions { grid-template-columns: repeat(3, minmax(0, 1fr)); }
pre { box-sizing: border-box; min-width: 0; max-height: 320px; overflow: auto; margin: 0; padding: 10px 12px; border-radius: var(--radius-sm); border: 1px solid var(--border); background: var(--bg-surface2); font: 12px/1.55 var(--font-mono); white-space: pre-wrap; overflow-wrap: anywhere; }
.sql-heading { margin-top: 16px; }
@media (max-width: 960px) {
  .schema-definitions { grid-template-columns: 1fr; gap: 8px; }
}
@media (max-width: 660px) {
  .schema-route { grid-template-columns: 1fr; gap: 8px; }
  .schema-surface { padding: 14px; }
}
</style>
