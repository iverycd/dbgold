<template>
  <section class="migration-preflight" aria-label="兼容预检">
    <div class="preflight-heading">
      <div>
        <h3>{{ readonly ? 'PG → MySQL 兼容检查结果' : 'PG → MySQL 兼容预检' }}</h3>
        <p>{{ readonly ? '本次迁移执行前保存的检查记录' : '检查对象结构兼容性，确认目标库执行计划' }}</p>
      </div>
      <a-space wrap>
        <a-button v-if="!readonly" :loading="busy" :disabled="disabled" @click="$emit('check')">{{ result ? '重新预检' : '运行预检' }}</a-button>
        <a-button v-if="result" @click="download"><template #icon><icon-download /></template>导出预检与 DDL</a-button>
      </a-space>
    </div>
    <a-alert v-if="!readonly" type="info" class="preflight-notice">
      预检只读检查对象结构、默认值和执行条件，不扫描全量业务数据。请在迁移和校验期间停止源库写入及结构变更。
      预检通过且配置未变化时，开始迁移会复用页面预检结果；后端仍会在执行前重新检查，存在阻止项时不会修改目标表。
      数据在复制或创建约束时验证；失败会停止迁移，已建表和已提交批次可能保留。
    </a-alert>
    <a-alert v-else type="info" class="preflight-notice">查看报告不会重新执行预检。<template v-if="structureCheck">此记录仅包含结构预检，不代表全量数据校验通过。</template></a-alert>
    <div v-if="excluded.length" class="preflight-excluded">
      <span>已明确排除 {{ excluded.length }} 项</span>
      <a-tag v-for="item in excluded" :key="objectKey(item)" :closable="!readonly && !busy && !disabled" color="orange" @close="$emit('restore', item)">
        {{ objectLabel(item) }}
      </a-tag>
    </div>
    <div class="preflight-stats" aria-live="polite">
      <div class="preflight-stat"><span>检查状态</span><strong :class="busy ? '' : result ? (result.ready ? 'text-success' : 'text-danger') : ''">{{ busy ? '结构检查中' : result ? (result.ready ? (structureCheck ? '结构预检通过' : '预检通过') : '存在阻止项') : '待预检' }}</strong></div>
      <div class="preflight-stat"><span>阻止项</span><strong :class="{ 'text-danger': errorCount > 0 }">{{ result ? errorCount : '—' }}</strong></div>
      <div class="preflight-stat"><span>提示项</span><strong>{{ result ? warningCount : '—' }}</strong></div>
      <div class="preflight-stat"><span>{{ structureCheck ? '计划对象数' : '已检查行数' }}</span><strong>{{ result ? (structureCheck ? result.objects.length : result.rows_checked).toLocaleString() : '—' }}</strong></div>
    </div>
    <template v-if="result">
      <a-collapse class="preflight-versions">
        <a-collapse-item key="versions" header="数据库版本与兼容信息">
          <dl class="preflight-metadata">
            <dt>源库</dt><dd>{{ result.source.product }} · {{ result.source.version }} · {{ result.source.mode || '默认兼容模式' }}</dd>
            <dt>目标库</dt><dd>MySQL {{ result.target.version }} / {{ result.target.database }} · {{ result.target.target_charset || '' }} / {{ result.target.target_collation }}</dd>
            <dt>验证说明</dt><dd>{{ result.source.verification }}；{{ result.target.verification }}</dd>
          </dl>
        </a-collapse-item>
      </a-collapse>
      <a-tabs v-model:active-key="activeTab" class="preflight-tabs" :destroy-on-hide="false">
        <a-tab-pane key="issues" :title="`检查问题（${sortedIssues.length}）`">
          <div class="preflight-toolbar">
            <a-input v-model="issueSearch" placeholder="搜索对象名称" allow-clear aria-label="搜索预检对象"><template #prefix><icon-search /></template></a-input>
            <a-radio-group v-model="severity" type="button" aria-label="问题级别">
              <a-radio value="all">全部</a-radio><a-radio value="error">阻止 {{ errorCount }}</a-radio><a-radio value="warning">提示 {{ warningCount }}</a-radio>
            </a-radio-group>
            <span class="preflight-count">共 {{ filteredIssues.length }} 项</span>
          </div>
          <a-table :data="filteredIssues" :pagination="{ current: issuePage, pageSize: 10, showTotal: true }" @page-change="issuePage = $event" :scroll="{ x: 900 }" size="small">
            <template #columns>
              <a-table-column title="级别" :width="80"><template #cell="{ record }"><a-tag :color="record.severity === 'error' ? 'red' : 'orange'">{{ record.severity === 'error' ? '阻止' : '提示' }}</a-tag></template></a-table-column>
              <a-table-column title="对象" :width="220"><template #cell="{ record }"><span class="preflight-object-name">{{ objectLabel(record.object) }}</span></template></a-table-column>
              <a-table-column title="原因" :width="320">
                <template #cell="{ record }">
                  {{ record.reason }}
                  <details v-if="record.definition" class="preflight-sql-details"><summary>查看原始 SQL</summary><pre class="preflight-original-sql">{{ record.definition }}</pre></details>
                </template>
              </a-table-column>
              <a-table-column title="建议" data-index="suggestion" :width="300" />
              <a-table-column v-if="!readonly && sortedIssues.some(issue => issue.excludable)" title="处理" :width="150"><template #cell="{ record }"><a-button v-if="record.excludable" type="text" status="danger" :disabled="busy || disabled" @click="$emit('exclude', record.object)">明确排除此对象</a-button></template></a-table-column>
            </template>
            <template #empty><a-empty :description="sortedIssues.length ? '没有匹配的问题' : '未发现兼容问题'" /></template>
          </a-table>
        </a-tab-pane>
        <a-tab-pane key="ddl" :title="`对象映射与 DDL（${result.objects.length}）`">
          <div class="preflight-ddl-browser">
            <aside class="preflight-object-list" aria-label="计划对象">
              <a-input v-model="objectSearch" placeholder="搜索对象或目标名称" allow-clear aria-label="搜索 DDL 对象"><template #prefix><icon-search /></template></a-input>
              <div class="preflight-object-items">
                <button v-for="object in pagedObjects" :key="objectKey(object.object)" type="button" class="preflight-object" :class="{ 'is-selected': selectedObjectKey === objectKey(object.object) }" :aria-pressed="selectedObjectKey === objectKey(object.object)" @click="selectedObjectKey = objectKey(object.object)">
                  <span>{{ object.object.kind }}</span><strong>{{ object.object.table ? object.object.table + '.' : '' }}{{ object.object.name }}</strong>
                </button>
                <a-empty v-if="!filteredObjects.length" description="没有匹配的对象" />
              </div>
              <a-pagination v-if="filteredObjects.length" v-model:current="objectPage" :total="filteredObjects.length" :page-size="10" simple size="small" />
            </aside>
            <div class="preflight-ddl-detail">
              <template v-if="selectedObject">
                <div class="preflight-ddl-heading"><div><span>对象映射</span><strong>{{ objectLabel(selectedObject.object) }} → {{ selectedObject.target }}</strong></div><a-button size="small" @click="copySQL"><template #icon><icon-copy /></template>复制 SQL</a-button></div>
                <pre class="preflight-planned-sql" tabindex="0" aria-label="计划 SQL">{{ selectedSQL || '此对象没有计划 SQL' }}</pre>
              </template>
              <a-empty v-else description="暂无对象映射与计划 DDL" />
            </div>
          </div>
        </a-tab-pane>
      </a-tabs>
    </template>
  </section>
</template>
<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { Message } from '@arco-design/web-vue'
import { copyText } from '@/utils/clipboard'
import type { MigrationObjectRef, PGMySQLPreflight } from '@/api/migration'
const props = withDefaults(defineProps<{ result?: PGMySQLPreflight | null; excluded?: MigrationObjectRef[]; busy?: boolean; disabled?: boolean; readonly?: boolean }>(), { excluded: () => [] })
const structureCheck = computed(() => props.result ? props.result.check_scope === 'structure' : !props.readonly)
defineEmits<{ check: []; exclude: [item: MigrationObjectRef]; restore: [item: MigrationObjectRef] }>()
const activeTab = ref('issues')
const severity = ref('all')
const issueSearch = ref('')
const issuePage = ref(1)
const objectSearch = ref('')
const objectPage = ref(1)
const selectedObjectKey = ref('')
const sortedIssues = computed(() => [...(props.result?.issues || [])].sort((a, b) => Number(b.severity === 'error') - Number(a.severity === 'error')))
const errorCount = computed(() => sortedIssues.value.filter(issue => issue.severity === 'error').length)
const warningCount = computed(() => sortedIssues.value.length - errorCount.value)
const filteredIssues = computed(() => sortedIssues.value.filter(issue => (severity.value === 'all' || issue.severity === severity.value) && objectLabel(issue.object).toLowerCase().includes(issueSearch.value.trim().toLowerCase())))
const filteredObjects = computed(() => (props.result?.objects || []).filter(object => `${objectLabel(object.object)} ${object.target}`.toLowerCase().includes(objectSearch.value.trim().toLowerCase())))
const pagedObjects = computed(() => filteredObjects.value.slice((objectPage.value - 1) * 10, objectPage.value * 10))
const selectedObject = computed(() => pagedObjects.value.find(object => objectKey(object.object) === selectedObjectKey.value))
const selectedSQL = computed(() => selectedObject.value?.statements.map(statement => statement.SQL + ';').join('\n') || '')
watch([issueSearch, severity, () => props.result], () => { issuePage.value = 1 })
watch([objectSearch, () => props.result], () => { objectPage.value = 1 })
watch(pagedObjects, objects => {
  if (!objects.some(object => objectKey(object.object) === selectedObjectKey.value)) selectedObjectKey.value = objects[0] ? objectKey(objects[0].object) : ''
}, { immediate: true })
watch(() => props.result, () => { activeTab.value = 'issues' })
function objectLabel(item: MigrationObjectRef) { return `${item.kind}: ${item.table ? item.table + '.' : ''}${item.name}` }
function objectKey(item: MigrationObjectRef) { return JSON.stringify([item.kind, item.table || '', item.name]) }
async function copySQL() {
  try { await copyText(selectedSQL.value); Message.success('SQL 已复制') } catch { Message.error('复制失败') }
}
function download() {
  const url = URL.createObjectURL(new Blob([JSON.stringify(props.result, null, 2)], { type: 'application/json;charset=utf-8' }))
  const a = document.createElement('a'); a.href = url; a.download = 'pg-mysql-preflight.json'; a.click(); URL.revokeObjectURL(url)
}
</script>
<style scoped>
.migration-preflight { container-type: inline-size; min-width: 0; padding: 24px; margin: 20px 0; background: var(--color-bg-1); border: 1px solid var(--color-border-2); border-radius: 10px; }
h3, p { margin: 0; } h3 { font-size: 16px; font-weight: 600; } p { margin-top: 6px; color: var(--color-text-3); font-size: 13px; }
.preflight-heading { display: flex; justify-content: space-between; align-items: center; flex-wrap: wrap; gap: 16px; margin-bottom: 20px; }
.preflight-notice { margin-bottom: 20px; }
.preflight-excluded { display: flex; flex-wrap: wrap; align-items: center; gap: 8px; margin-bottom: 16px; font-size: 13px; }
.preflight-excluded :deep(.arco-tag) { max-width: 100%; height: auto; white-space: normal; overflow-wrap: anywhere; }
.preflight-stats { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 1px; border: 1px solid var(--color-border-2); border-radius: 8px; background: var(--color-border-2); overflow: hidden; margin-bottom: 20px; }
.preflight-stat { display: flex; flex-direction: column; gap: 10px; padding: 18px 20px; background: var(--color-bg-2); }
.preflight-stat span { font-size: 12px; color: var(--color-text-3); } .preflight-stat strong { font-size: 22px; line-height: 1.4; font-variant-numeric: tabular-nums; }
.text-success { color: #15803d; } .text-danger { color: #dc2626; }
.preflight-versions { margin-bottom: 12px; } .preflight-metadata { display: grid; grid-template-columns: 80px minmax(0, 1fr); gap: 12px; font-size: 13px; line-height: 1.7; margin: 0; }
.preflight-metadata dt { color: var(--color-text-3); } .preflight-metadata dd { margin: 0; overflow-wrap: anywhere; }
.preflight-toolbar { display: flex; align-items: center; gap: 12px; flex-wrap: wrap; margin: 4px 0 16px; }
.preflight-toolbar > :deep(.arco-input-wrapper) { width: 260px; max-width: 100%; } .preflight-count { margin-left: auto; font-size: 12px; color: var(--color-text-3); }
.preflight-object-name { overflow-wrap: anywhere; } .preflight-sql-details { margin-top: 8px; } summary { cursor: pointer; color: #15803d; }
.preflight-original-sql { max-height: 280px; overflow: auto; white-space: pre-wrap; overflow-wrap: anywhere; }
.preflight-ddl-browser { display: grid; grid-template-columns: 280px minmax(0, 1fr); border: 1px solid var(--color-border-2); border-radius: 8px; overflow: hidden; }
.preflight-object-list { display: flex; flex-direction: column; gap: 12px; padding: 16px; background: var(--color-bg-2); border-right: 1px solid var(--color-border-2); min-width: 0; }
.preflight-object-items { height: 360px; overflow-y: auto; }
.preflight-object { display: flex; flex-direction: column; text-align: left; gap: 4px; width: 100%; padding: 10px 12px; background: transparent; border: 1px solid transparent; border-radius: 6px; color: var(--color-text-1); cursor: pointer; }
.preflight-object span { font-size: 11px; color: var(--color-text-3); } .preflight-object strong { font-size: 13px; font-weight: 500; overflow-wrap: anywhere; }
.preflight-object:hover { background: var(--color-fill-2); } .preflight-object.is-selected { background: #f0fdf4; border-color: #bbf7d0; color: #15803d; } .preflight-object:focus-visible { outline: 2px solid #16a34a; outline-offset: -2px; }
.preflight-ddl-detail { min-width: 0; display: flex; flex-direction: column; justify-content: center; }
.preflight-ddl-heading { display: flex; justify-content: space-between; gap: 12px; align-items: center; padding: 16px 20px; border-bottom: 1px solid var(--color-border-2); }
.preflight-ddl-heading > div { min-width: 0; } .preflight-ddl-heading span { display: block; color: var(--color-text-3); font-size: 12px; margin-bottom: 6px; } .preflight-ddl-heading strong { font-size: 13px; overflow-wrap: anywhere; } .preflight-ddl-heading :deep(.arco-btn) { flex-shrink: 0; }
.preflight-planned-sql { flex: 1; height: 360px; min-height: 200px; max-height: 420px; margin: 0; padding: 20px; overflow: auto; background: #f8fafc; color: #334155; font: 13px/1.8 var(--font-mono, monospace); tab-size: 2; }
.migration-preflight :deep(.arco-tabs-content), .migration-preflight :deep(.arco-tabs-pane) { min-width: 0; }
.migration-preflight :deep(.arco-table-cell) { overflow-wrap: anywhere; }
.migration-preflight :deep(.arco-tabs-tab-active), .migration-preflight :deep(.arco-tabs-tab:hover) { color: #15803d; }
.migration-preflight :deep(.arco-tabs-nav-ink) { background: #16a34a; }
@container (max-width: 767px) {
  .preflight-ddl-browser { grid-template-columns: minmax(0, 1fr); } .preflight-object-list { border-right: 0; border-bottom: 1px solid var(--color-border-2); } .preflight-object-items { height: 180px; }
  .preflight-stats { grid-template-columns: repeat(2, minmax(0, 1fr)); } .preflight-stat { padding: 14px; } .preflight-stat strong { font-size: 20px; }
}
@media (max-width: 767px) { .migration-preflight { padding: 16px; } }
</style>
