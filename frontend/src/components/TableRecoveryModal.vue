<template>
  <a-modal
    :visible="visible" title="接续此表迁移" width="min(920px, calc(100vw - 32px))"
    :modal-style="{ display: 'inline-flex', flexDirection: 'column', maxHeight: '85dvh', boxSizing: 'border-box' }"
    :body-style="{ minHeight: 0, overflowY: 'auto', padding: '16px 20px' }"
    :mask-closable="false" :closable="!submitting" :esc-to-close="!submitting" unmount-on-close @cancel="emit('close')"
  >
    <div ref="content" class="recovery-content" :inert="submitting">
      <p class="table-route">源表 <strong>{{ preview?.source_table || object.source_table || object.source_name || object.table || object.name }}</strong> → 目标 <strong>{{ preview?.qualified_target || qualifiedRepairName(object) }}</strong></p>
      <a-alert type="warning">接续读取当前源数据，请在执行期间保持源表和目标表停写。导入中断会保留已提交数据，目标表非空时需确认清空重导。</a-alert>

      <details ref="mappingDetails" class="mapping-details">
        <summary>列映射 · 共 {{ sources.length }} 列</summary>
        <div v-if="sources.length" class="mapping-grid">
          <div v-for="(source, index) in pageItems(sources, pages.mapping)" :key="source" class="mapping-row">
            <code>{{ source }}</code><span aria-hidden="true">→</span>
            <div :data-target="'mapping-' + index">
              <a-input v-model="form.mapping[source]" size="small" :input-attrs="{ 'aria-label': source + ' 的目标列' }" placeholder="目标列原始名称" @input="emit('invalidate')" />
            </div>
          </div>
          <a-pagination v-if="sources.length > 10" v-model:current="pages.mapping" :page-size="10" :total="sources.length" size="small" simple aria-label="列映射分页" />
        </div>
        <p v-else class="muted">生成预览后可查看源列及默认映射。</p>
      </details>

      <section aria-label="关联对象" class="related-section">
        <div class="section-heading">
          <h4>关联对象</h4><span class="muted">已选 {{ selectedCount }} / 可选 {{ selectable.length }}</span>
          <div v-if="selectable.length" class="bulk-actions">
            <a-button type="text" size="small" :disabled="selectedCount === selectable.length" @click="selectRelated(true)">全选可选对象</a-button>
            <a-button type="text" size="small" :disabled="!selectedCount" @click="selectRelated(false)">清空选择</a-button>
          </div>
        </div>
        <div v-if="groups.length" class="kind-controls" role="group" aria-label="按类型选择关联对象">
          <span class="muted">按类型选择</span>
          <a-checkbox v-for="group in groups" :key="group.label" :model-value="group.selected === group.total"
            :indeterminate="group.selected > 0 && group.selected < group.total" @change="selectRelated($event === true, group.label)">
            {{ group.label }}（{{ group.total }}）
          </a-checkbox>
        </div>
        <div v-for="{ object: item, reason } in pageItems(choices, pages.related)" :key="item.id" class="choice-row">
          <a-checkbox v-model="form.related[item.id]" :disabled="!!reason" @change="emit('invalidate')">
            <span class="choice-kind">{{ repairKind(item.kind) }}</span> {{ qualifiedRepairName(item) }}
          </a-checkbox>
          <span class="muted">{{ reason ? '不可选 · ' + reason : repairStatus(item.status) }}</span>
        </div>
        <p v-if="!choices.length" class="muted">无待执行关联对象</p>
        <a-pagination v-if="choices.length > 10" v-model:current="pages.related" :page-size="10" :total="choices.length" size="small" simple aria-label="关联对象分页" />
      </section>

      <template v-if="preview">
        <section class="preview-section" aria-label="接续预览">
          <h4>接续预览</h4>
          <p>源表 {{ preview.source_rows }} 行 · 目标表 {{ preview.target_rows }} 行 · {{ preview.data_required ? '完整导入此表数据' : '仅处理选中的关联对象' }}</p>
          <a-alert v-for="warning in preview.warnings" :key="warning" type="warning">{{ warning }}</a-alert>
          <div v-for="(item, index) in pageItems(preview.related || [], pages.ddl)" :key="item.id" class="ddl-row">
            <details>
              <summary>{{ repairKind(item.kind) }} · {{ qualifiedRepairName(item) }}</summary>
              <pre>{{ (item.units || []).map(unit => unit.sql).join('\n\n') || item.ddl }}</pre>
            </details>
            <div v-if="item.requires_review" :data-target="'reviewDDL-' + index">
              <a-button size="small" @click="emit('review', item.id)">编辑并复核</a-button>
            </div>
          </div>
          <a-pagination v-if="(preview.related?.length || 0) > 10" v-model:current="pages.ddl" :page-size="10" :total="preview.related.length" size="small" simple aria-label="关联对象 DDL 分页" />
        </section>

        <a-form :model="form" layout="vertical" class="consent-form">
          <a-form-item v-if="preview.data_required && preview.target_rows > 0" label="目标表已有数据">
            <div class="truncate-consent">
              <div data-target="truncate"><a-checkbox v-model="form.truncate">清空本表后完整重导，保留修复后的结构</a-checkbox></div>
              <div>请输入目标完整表名 <strong>{{ preview.qualified_target }}</strong></div>
              <div data-target="confirmation"><a-input v-model="form.confirmation" :input-attrs="{ 'aria-label': '目标完整表名' }" placeholder="逐字输入上方完整表名" /></div>
            </div>
          </a-form-item>
          <a-form-item label="执行前确认">
            <div class="consents">
              <div data-target="sourceQuiesced"><a-checkbox v-model="form.sourceQuiesced">源表已停止写入</a-checkbox></div>
              <div data-target="targetQuiesced"><a-checkbox v-model="form.targetQuiesced">目标表已停止写入</a-checkbox></div>
              <div data-target="reviewed"><a-checkbox v-model="form.reviewed">已核对列映射和关联对象 DDL</a-checkbox></div>
            </div>
          </a-form-item>
        </a-form>
      </template>
    </div>
    <template #footer>
      <div class="recovery-footer">
        <span class="action-feedback" role="status" aria-live="polite">{{ feedback }}</span>
        <div class="footer-buttons">
          <a-button v-if="preview" type="text" :disabled="busy" @click="emit('preview')">刷新预览</a-button>
          <a-button :disabled="submitting" @click="emit('close')">关闭</a-button>
          <a-button type="primary" :loading="previewing || submitting" :class="{ processing: previewing || submitting }" @click="primaryAction">
            {{ submitting ? '正在提交…' : previewing ? '正在生成预览…' : preview ? '确认接续迁移' : '生成预览' }}
          </a-button>
        </div>
      </div>
    </template>
  </a-modal>
</template>

<script setup lang="ts">
import { computed, nextTick, reactive, ref, watch } from 'vue'
import type { RecoveryPreview, RepairObject } from '@/api/migrationRepair'
import { qualifiedRepairName, repairKind, repairStatus, type RecoveryForm, type RecoveryIssue } from '@/utils/migrationRepair'

const props = defineProps<{
  visible: boolean
  object: RepairObject
  form: RecoveryForm
  choices: { object: RepairObject; reason: string }[]
  preview?: RecoveryPreview
  issue?: RecoveryIssue
  error: string
  busy: boolean
  previewing: boolean
  submitting: boolean
}>()
const emit = defineEmits<{ close: []; invalidate: []; preview: []; confirm: []; review: [id: string] }>()
const sources = computed(() => Object.keys(props.form.mapping))
const selectable = computed(() => props.choices.filter(choice => !choice.reason))
const selectedCount = computed(() => selectable.value.filter(({ object }) => props.form.related[object.id]).length)
const groups = computed(() => {
  const counts = new Map<string, { label: string; total: number; selected: number }>()
  for (const { object } of selectable.value) {
    const label = repairKind(object.kind)
    const group = counts.get(label) || { label, total: 0, selected: 0 }
    group.total++
    if (props.form.related[object.id]) group.selected++
    counts.set(label, group)
  }
  return [...counts.values()]
})
function selectRelated(checked: boolean, label?: string) {
  for (const { object } of selectable.value) {
    if (!label || repairKind(object.kind) === label) props.form.related[object.id] = checked
  }
  emit('invalidate')
}

const pages = reactive({ mapping: 1, related: 1, ddl: 1 })
function pageItems<T>(items: T[], page: number) { return items.slice((page - 1) * 10, page * 10) }
const content = ref<HTMLElement>()
const mappingDetails = ref<HTMLDetailsElement>()
const attempted = ref(false)
const feedback = computed(() => {
  if (props.error) return props.error
  if (props.busy && !props.submitting) return '当前有任务正在执行，请等待完成后再操作'
  if (attempted.value || props.issue?.field === 'mapping') return props.issue?.message
})
watch(() => props.visible, () => {
  attempted.value = false
  Object.assign(pages, { mapping: 1, related: 1, ddl: 1 })
})
watch(() => props.preview, () => { pages.ddl = 1; attempted.value = false })
watch([() => sources.value.length, () => props.choices.length], ([columns, choices]) => {
  pages.mapping = Math.min(pages.mapping, Math.max(1, Math.ceil(columns / 10)))
  pages.related = Math.min(pages.related, Math.max(1, Math.ceil(choices / 10)))
})

async function primaryAction() {
  if (props.previewing || props.busy || props.submitting) return
  const issue = props.issue
  if (!issue) {
    if (props.preview) emit('confirm')
    else emit('preview')
    return
  }
  attempted.value = true
  let target: string = issue.field
  if (issue.index !== undefined) {
    const page = Math.floor(issue.index / 10) + 1
    if (issue.field === 'mapping') {
      pages.mapping = page
      if (mappingDetails.value) mappingDetails.value.open = true
    } else pages.ddl = page
    target += '-' + issue.index % 10
  }
  await nextTick()
  const field = content.value?.querySelector<HTMLElement>(`[data-target="${target}"]`)
  field?.scrollIntoView({ block: 'nearest' })
  field?.querySelector<HTMLElement>('input, button')?.focus({ preventScroll: true })
}
</script>

<style scoped>
.recovery-content { display: grid; gap: 14px; }
.recovery-content h4, .recovery-content p { margin: 0; }
.table-route, .recovery-content summary, .choice-row, .truncate-consent { overflow-wrap: anywhere; }
.muted, .choice-kind { color: var(--color-text-3); font-size: 12px; }
.mapping-details { padding: 8px 12px; background: var(--color-fill-1); border-radius: 4px; }
summary { cursor: pointer; }
.mapping-grid, .truncate-consent { display: grid; gap: 6px; }
.mapping-grid { padding-top: 8px; }
.mapping-row { display: grid; grid-template-columns: minmax(0, 1fr) auto minmax(0, 1fr); gap: 8px; align-items: center; overflow-wrap: anywhere; }
.section-heading, .bulk-actions, .kind-controls, .consents, .recovery-footer, .footer-buttons { display: flex; align-items: center; flex-wrap: wrap; gap: 8px; }
.bulk-actions { margin-inline-start: auto; }
.kind-controls { background: var(--color-fill-1); padding: 6px 8px; margin: 4px 0; border-radius: 4px; }
.choice-row, .ddl-row { display: flex; gap: 8px; align-items: baseline; justify-content: space-between; padding: 4px 0; border-bottom: 1px solid var(--color-border-2); }
.choice-row > .muted { flex-shrink: 0; max-width: 40%; }
.choice-row :deep(.arco-checkbox-label) { min-width: 0; }
.recovery-content :deep(.arco-pagination) { justify-content: flex-end; margin-top: 8px; }
.preview-section { display: grid; gap: 6px; }
.ddl-row details { min-width: 0; flex: 1; }
.ddl-row pre { max-height: 200px; overflow: auto; margin: 8px 0 2px; padding: 8px; background: var(--color-fill-1); white-space: pre-wrap; overflow-wrap: anywhere; }
.consent-form :deep(.arco-form-item) { margin-bottom: 8px; }
.recovery-footer { justify-content: space-between; text-align: start; }
.action-feedback { flex: 1 1 160px; color: rgb(var(--danger-6)); overflow-wrap: anywhere; max-height: 12dvh; overflow: auto; }
.action-feedback:empty { display: none; }
.footer-buttons { margin-inline-start: auto; justify-content: flex-end; }
.processing { cursor: progress; }
</style>
