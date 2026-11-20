<template>
  <section ref="repairSection" class="repair-panel" aria-label="对象修复" tabindex="-1">
    <div class="repair-heading">
      <div><h3>对象修复</h3><p>修复对象后，可按需接续数据与关联对象迁移。原始报告保持不变。</p></div>
      <a-space wrap>
        <a-button size="small" :disabled="busy" @click="openBinding">连接与配置</a-button>
        <a-button size="small" @click="openHistory()">执行历史</a-button>
        <a-button size="small" :loading="loading" @click="refresh">刷新</a-button>
      </a-space>
    </div>
    <a-alert v-if="error" type="error">{{ error }}</a-alert>
    <a-alert v-if="job?.status === 'running'" type="info">原迁移仍在运行，结束后才能修复。</a-alert>
    <a-alert v-else-if="binding && !binding.configured" type="warning">此历史或批量任务需要先绑定匹配的已保存连接，并确认原迁移配置。</a-alert>
    <div v-if="records.length" class="repair-stats" role="group" aria-label="按修复状态筛选对象">
      <button v-for="category in repairCategories" :key="category.key" type="button" class="repair-stat" :class="{ 'repair-stat-active': filter === category.key }" :aria-pressed="filter === category.key" @click="selectRepairFilter(category.key)">
        {{ category.label }} <strong>{{ stats[category.key] }}</strong>
      </button>
    </div>
    <div v-if="activeAttempt" class="attempt-banner">
      <a-tag :color="statusColor(activeAttempt.status)">{{ repairExecutionStatus(activeAttempt.status) }}</a-tag>
      <span>{{ activeAttempt.kind === 'recovery' ? '接续迁移' : 'DDL 修复' }} · {{ activeAttempt.id }}</span>
      <a-button size="mini" @click="openHistory(activeAttempt.object_id)">查看进度</a-button>
      <a-button v-if="attemptRunning(activeAttempt.status)" size="mini" status="danger" :loading="cancelling" @click="cancelAttempt">取消执行</a-button>
    </div>
    <div v-if="records.length" class="repair-toolbar">
      <div class="repair-filters">
        <a-select v-model="filter" style="width: 150px" size="small" aria-label="筛选修复状态">
          <a-option value="attention">需要处理</a-option><a-option value="all">全部对象</a-option>
          <a-option v-for="category in repairCategories" :key="category.key" :value="category.key">{{ category.label }}</a-option>
        </a-select>
        <a-input-search v-model="keyword" class="repair-search" placeholder="搜索对象、模式或错误" aria-label="搜索模式名、对象、所属表或错误" allow-clear size="small" />
      </div>
      <div class="batch-toolbar">
        <a-tooltip content="仅使用已保存的 SQL，按依赖顺序逐项执行" :trigger="['hover', 'focus']">
          <a-button type="primary" size="small" :loading="batchPreparing" :disabled="busy || loading || saving || !binding?.configured || !selectedObjectIDs.length" @click="prepareBatch">批量执行（{{ selectedObjectIDs.length }}）</a-button>
        </a-tooltip>
        <a-button size="small" :disabled="busy || loading" @click="selectAllFiltered">全选当前筛选结果</a-button>
        <a-button type="text" size="small" :disabled="busy || !selectedObjectIDs.length" @click="clearBatchSelection">清空选择</a-button>
      </div>
    </div>
    <section v-if="batchHasRun" class="batch-progress" aria-label="批量修复进度" aria-live="polite">
      <div class="unit-heading">
        <strong>{{ batchRunning ? (batchStopped ? '正在结束当前执行' : '批量执行中') : '本次批量执行结果' }} · {{ batchCompletedCount }} / {{ batchItems.length }}</strong>
        <a-button v-if="batchRunning" size="small" :disabled="batchStopped" @click="stopBatch">停止后续执行</a-button>
      </div>
      <p class="muted">刷新页面、关闭报告或离开页面后，尚未提交的对象停止排队；已提交的对象继续执行。</p>
      <a-alert v-if="batchError" type="warning">{{ batchError }}</a-alert>
      <div class="batch-results">
        <div v-for="item in batchItems" :key="item.record.object.id" class="batch-result">
          <a-tag :color="statusColor(item.status)">{{ batchItemStatus(item.status) }}</a-tag>
          <span class="batch-object-name">{{ qualified(item.record.object) }}</span>
          <a-button v-if="item.attemptID || item.status === 'unknown'" type="text" size="mini" @click="openHistory(item.record.object.id)">历史</a-button>
          <span v-if="item.reason" class="muted batch-reason">{{ item.reason }}</span>
        </div>
      </div>
    </section>
    <a-table v-if="records.length" class="repair-table" v-model:selected-keys="selectedObjectIDs" :row-selection="{ type: 'checkbox', onlyCurrent: false, showCheckedAll: true }" :data="visibleRecords" row-key="key" :pagination="{ pageSize: 10, current: page }" :scroll="{ x: 1280 }" size="small" :row-class="record => record.object.id === highlightedObjectID ? 'repair-row-highlighted' : ''" @page-change="page = $event">
      <template #empty>
        <div class="repair-empty">
          <a-empty :description="emptyDescription" />
          <slot name="empty-action" />
          <a-button v-if="filter !== 'all' || keyword.trim()" type="text" size="small" @click="showAllObjects">查看全部对象</a-button>
        </div>
      </template>
      <template #columns>
        <a-table-column title="对象" :width="360">
          <template #cell="{ record }">
            <div class="repair-object-cell">
              <div class="repair-object-heading">
                <a-tooltip>
                  <template #content><span class="repair-name-tooltip">{{ record.object.target_name }}</span></template>
                  <strong class="repair-name" :data-repair-object-id="record.object.id" tabindex="-1">{{ record.object.target_name }}</strong>
                </a-tooltip>
                <span class="repair-kind">{{ repairKind(record.object.kind) }}</span>
                <a-tooltip v-if="record.batchReason" :trigger="['hover', 'focus']">
                  <template #content><span class="repair-name-tooltip">不可批量：{{ record.batchReason }}</span></template>
                  <button type="button" class="repair-batch-hint" :aria-label="`不可批量：${record.batchReason}`"><icon-info-circle /></button>
                </a-tooltip>
              </div>
              <div class="repair-object-meta">
                <a-tooltip>
                  <template #content><span class="repair-name-tooltip">模式：{{ record.object.schema || '—' }}</span></template>
                  <span class="repair-name">{{ record.object.schema || '—' }}</span>
                </a-tooltip>
                <template v-if="record.object.table && (!isTableRepair(record.object) || record.object.table !== record.object.target_name)">
                  <span class="repair-meta-divider" aria-hidden="true">·</span>
                  <a-tooltip>
                    <template #content><span class="repair-name-tooltip">所属表：{{ record.object.table }}</span></template>
                    <span class="repair-name">所属表：{{ record.object.table }}</span>
                  </a-tooltip>
                </template>
              </div>
            </div>
          </template>
        </a-table-column>
        <a-table-column title="原始错误与校验信息"><template #cell="{ record }">
          <div v-if="record.errors.length" class="repair-error-cell">
            <div class="repair-error-summary"><span v-if="record.errors[0].label" class="repair-error-phase">{{ record.errors[0].label }}：</span>{{ record.errors[0].text.replace(/\s+/g, ' ') }}</div>
            <div class="repair-error-actions">
              <span v-if="record.errors.length > 1" class="muted">共 {{ record.errors.length }} 条</span>
              <a-button type="text" size="mini" :aria-label="`查看 ${qualified(record.object)} 的原始错误与校验信息`" @click="openErrorDetails(record)">详情</a-button>
            </div>
          </div>
          <span v-else class="muted">—</span>
        </template></a-table-column>
        <a-table-column title="修复状态" :width="360">
          <template #cell="{ record }">
            <div class="repair-statuses">
              <div v-for="state in repairDisplayStatuses(record)" :key="state.label" class="repair-status-line">
                <span v-if="state.label" class="repair-status-label">{{ state.label }}：</span>
                <a-tag :color="statusColor(state.status)">{{ state.text }}</a-tag>
              </div>
            </div>
            <div v-if="record.difference?.current" class="repair-count-detail">{{ record.difference.detail }}</div>
          </template>
        </a-table-column>
        <a-table-column title="操作" :width="290"><template #cell="{ record }"><div class="repair-row-actions">
          <a-button v-if="record.difference" type="text" size="mini" @click="emit('show-row-difference', record.difference.table)">查看行数差异</a-button>
          <a-button v-if="canEditRepairDDL(record.object)" type="text" size="mini" :disabled="busy" @click="openEditor(primaryRecord(record))">{{ record.object.kind === 'trigger' ? '编辑 SQL' : hasRepairDDL(record.object) ? '编辑 DDL' : '编写 DDL' }}</a-button>
          <a-button v-else-if="isGeneratedMySQLTrigger(record.object) || record.object.native || record.object.manual_trigger" type="text" size="mini" :disabled="busy" @click="openEditor(primaryRecord(record))">查看 / 重试</a-button>
          <a-button v-else type="text" size="mini" @click="openEditor(primaryRecord(record))">查看诊断</a-button>
          <a-button v-if="isTableRepair(record.object) && !record.object.diagnostic_only" type="text" size="mini" :disabled="busy || !binding?.configured" @click="openRecovery(primaryRecord(record))">接续迁移</a-button>
          <a-button type="text" size="mini" @click="openHistory(record.object.id)">历史</a-button>
        </div></template></a-table-column>
      </template>
    </a-table>
    <a-spin v-else-if="loading" />
    <div v-else-if="!error" class="repair-empty">
      <a-empty :description="emptyDescription" />
      <slot name="empty-action" />
    </div>

    <a-modal v-model:visible="errorDetailsVisible" :mask-closable="false" width="min(800px, calc(100vw - 48px))" title="原始错误与校验信息" :body-style="{ maxHeight: '60vh', overflow: 'auto' }" @close="closeErrorDetails">
      <template v-if="errorDetailRecord">
        <p class="repair-error-object">{{ qualified(errorDetailRecord.object) }}</p>
        <p class="muted">以下是原始迁移时的记录。当前结果见修复状态。</p>
        <div class="repair-error-details">
          <article v-for="(detail, index) in repairDisplayErrors(errorDetailRecord)" :key="`${detail.label}:${detail.text}`" class="repair-error-detail">
            <h4>{{ index + 1 }}. {{ detail.label || '原始错误' }}</h4>
            <pre>{{ detail.text }}</pre>
          </article>
        </div>
      </template>
      <template #footer><a-button @click="closeErrorDetails">关闭</a-button></template>
    </a-modal>

    <a-modal v-model:visible="batchPreviewVisible" :mask-closable="false" width="min(960px, calc(100vw - 48px))" title="确认批量执行 DDL" :footer="false">
      <p>目标数据库：{{ getDbTypeLabel(job?.dst_db_type || '') }} · 共选择 {{ batchItems.length }} 个对象，可执行 {{ batchExecutableCount }} 个</p>
      <a-alert type="info">以下已保存的执行单元将依次提交。执行失败后跳过依赖它的对象，继续其他独立项。</a-alert>
      <p class="muted">请保持当前页面打开。刷新页面、关闭报告或离开页面后，尚未提交的对象停止排队。</p>
      <div class="batch-preview-items">
        <article v-for="(item, index) in batchItems" :key="item.record.object.id" class="batch-preview-item">
          <div class="unit-heading"><strong>{{ index + 1 }}. {{ qualified(item.record.object) }}</strong><a-tag>{{ batchItemStatus(item.status) }}</a-tag></div>
          <p class="muted">{{ repairKind(item.record.object.kind) }} · Schema：{{ item.record.object.schema }} · 草稿版本 {{ item.record.version }}</p>
          <p v-if="item.record.object.dependencies?.length" class="muted">依赖：{{ dependencyNames(item.record.object).join('、') }}</p>
          <a-alert v-if="item.reason" type="warning">{{ item.reason }}</a-alert>
          <pre v-for="unit in item.record.object.units.filter(unit => unit.selected && !unitCommitted(unit.status) && !unit.destructive)" :key="unit.id" class="sql-preview">{{ unit.sql }}</pre>
        </article>
      </div>
      <a-checkbox v-model="batchReviewed" :disabled="!batchExecutableCount">我已核对以上执行单元、目标对象及历史提交状态</a-checkbox>
      <div class="modal-actions"><a-button @click="batchPreviewVisible = false">返回列表</a-button><a-button type="primary" :disabled="!batchReviewed || !batchExecutableCount || busy || !binding?.configured" @click="confirmBatch">执行以上 {{ batchExecutableCount }} 个对象</a-button></div>
    </a-modal>

    <a-modal
      v-model:visible="editorVisible"
      :mask-closable="false"
      width="min(1120px, calc(100vw - 48px))"
      :align-center="true"
      :modal-style="{ display: 'inline-flex', flexDirection: 'column', boxSizing: 'border-box', maxHeight: 'calc(100dvh - 48px)' }"
      :body-style="{ flex: '1 1 auto', minHeight: '0', overflow: 'auto' }"
      :title="`对象修复 · ${editing ? qualified(editing.object) : ''}`"
      @close="closeEditor"
    >
      <template v-if="editing">
        <p>目标数据库：{{ getDbTypeLabel(job?.dst_db_type || '') }} · Schema：{{ editing.object.schema || '—' }} · 对象：{{ editing.object.target_name }}<span v-if="editing.object.table"> · 所属表：{{ editing.object.table }}</span></p>
        <a-alert v-if="pgTriggerEditor" type="info">请使用目标数据库语法填写完整的 CREATE FUNCTION 和 CREATE TRIGGER。保存并执行时，两条语句将在同一个事务中提交。</a-alert>
        <a-alert v-else-if="canEditRepairDDL(editing.object) && editing.object.kind === 'trigger'" type="info">使用达梦语法编辑此触发器的完整 CREATE 语句；点击“保存并执行”后直接提交。目标存在同名触发器时会先核对定义。</a-alert>
        <a-alert v-else-if="canEditRepairDDL(editing.object)" type="info">请使用目标数据库语法编写 DDL，每个执行单元填写一条完整语句。点击“保存并执行”后直接提交；表数据及关联对象按需单独接续。</a-alert>
        <a-alert v-else-if="isManualPGTrigger(editing.object) && editing.object.status !== 'unknown'" type="info">以下为已保存的触发器函数和触发器 SQL。</a-alert>
        <a-alert v-else-if="editing.object.manual_trigger && !isManualPGTrigger(editing.object)" type="info">当前已保存的触发器 SQL 为只读；执行结果未知时可核对目标状态后重试同一份 SQL。</a-alert>
        <a-alert v-else-if="editing.object.native" type="info">达梦原生 SQL 为只读，仅重试原任务冻结的完整执行单元；已成功步骤不会再次执行。</a-alert>
        <a-alert v-else-if="isMySQLDamengTrigger(editing.object)" type="info">触发器 SQL 为只读，按原生成的单个完整语句执行。达梦 DDL 可能自动提交；重试前会核对目标定义、编译状态和依赖，不会覆盖同名冲突对象。</a-alert>
        <a-alert v-else-if="isMySQLPGTrigger(editing.object)" type="info">触发器 SQL 为只读。重试会在一个事务中一起执行原生成函数和触发器；不支持编辑 SQL 或单独勾选步骤。</a-alert>
        <a-alert v-else-if="!isManualPGTrigger(editing.object)" type="info">此记录仅供诊断；请先处理连接、元数据或预检问题。数据失败可使用“接续迁移”。</a-alert>
        <a-alert v-if="editing.object.status === 'unknown'" type="warning">{{ unknownExecutionMessage(editing.object) }}</a-alert>
        <a-alert v-if="editing.object.original_error ?? editing.object.error" type="error">原始错误：{{ editing.object.original_error ?? editing.object.error }}</a-alert>
        <a-alert v-if="editing.object.original_error !== undefined && editing.object.error && editing.object.original_error !== editing.object.error" type="warning">当前错误：{{ editing.object.error }}</a-alert>
        <a-alert v-if="editing.object.requires_review && !canEditRepairDDL(editing.object) && !editing.object.native && !isGeneratedMySQLTrigger(editing.object)" type="warning">请按数据库驱动可一次执行的完整语句整理执行步骤。历史脚本不会按分号自动拆分，过去提交结果不能确定时禁止自动重放。</a-alert>
        <a-alert v-if="indexPlanMessage" type="info">{{ indexPlanMessage }}</a-alert>
        <a-alert v-if="sequencePlanMessage" type="info">{{ sequencePlanMessage }}</a-alert>
        <p v-if="editing.object.dependencies?.length">依赖：{{ dependencyNames(editing.object).join('、') }}</p>
        <p v-if="editing.object.foreign_key">引用：{{ editing.object.foreign_key.Columns.join(', ') }} → {{ editing.object.foreign_key.RefSchema }}.{{ editing.object.foreign_key.RefTable }} ({{ editing.object.foreign_key.RefColumns.join(', ') }})</p>
        <a-collapse>
          <a-collapse-item v-if="editing.object.source_ddl" key="source" header="源端定义（只读，仅供手工适配参考）"><pre class="sql-preview">{{ editing.object.source_ddl }}</pre></a-collapse-item>
          <a-collapse-item v-if="editing.object.suggested_ddl" key="suggested" :header="editing.object.kind === 'data' ? '建议 DDL（手动执行）' : '建议 DDL（需显式采用或整理到对应执行单元）'"><pre class="sql-preview">{{ editing.object.suggested_ddl }}</pre></a-collapse-item>
          <a-collapse-item key="original" header="原始目标 DDL（只读）"><pre class="sql-preview">{{ editing.object.ddl || (canEditRepairDDL(editing.object) ? '未生成目标 DDL，请根据源端定义手动编写。' : '该记录没有可执行的对象 DDL。') }}</pre></a-collapse-item>
        </a-collapse>
        <div v-if="triggerPlanAvailable" class="editor-toolbar">
          <a-button size="small" :loading="refreshingTriggerPlan" :disabled="!canRefreshTriggerPlan" @click="refreshTriggerPlan">重新生成触发器方案</a-button>
          <span class="muted">读取当前源定义，核对后保存方案，再确认执行。</span>
        </div>
        <template v-if="triggerPlanCandidate">
          <a-alert type="info">{{ triggerPlanCandidate.message }}</a-alert>
          <a-collapse><a-collapse-item key="trigger-source" header="本次核对的源定义（只读）"><pre class="sql-preview">{{ triggerPlanCandidate.object.source_ddl }}</pre></a-collapse-item></a-collapse>
          <div v-for="(unit, index) in triggerPlanCandidate.object.units" :key="`candidate:${unit.id}`" class="unit-editor">
            <div class="unit-heading"><strong>新方案 · {{ pgTriggerUnitLabel(index) }}</strong></div>
            <pre class="sql-preview">{{ unit.sql }}</pre>
          </div>
        </template>
        <a-form v-if="isTableRepair(editing.object) && canEditRepairDDL(editing.object)" :model="mappingForm" layout="vertical" class="editor-mapping">
          <a-form-item label="源列 → 目标列映射（修改建表列名时同时保存）">
            <div class="mapping-list"><div v-for="source in Object.keys(mappingForm.values)" :key="source" class="mapping-row editable-mapping"><code>{{ source }}</code><span>→</span><a-input v-model="mappingForm.values[source]" placeholder="修复 DDL 中的目标列名" /><a-button type="text" size="mini" :disabled="(editing.object.source_columns || editing.object.columns || []).some(column => column.Name === source)" @click="delete mappingForm.values[source]">移除</a-button></div><span v-if="!Object.keys(mappingForm.values).length" class="muted">旧报告没有列元数据时，可按源表与修复 DDL 显式补充映射；接续预检会核对实际列。</span></div>
          </a-form-item>
          <a-form-item label="补充列映射"><div class="mapping-row"><a-input v-model="mappingForm.source" placeholder="源列原始名称" /><span>→</span><a-input v-model="mappingForm.target" placeholder="目标列原始名称" /></div><a-button size="small" style="margin-left: 8px" :disabled="!mappingForm.source.trim() || !mappingForm.target.trim()" @click="addColumnMapping">添加</a-button></a-form-item>
          <a-alert v-if="mappingError(mappingForm.values)" type="error">{{ mappingError(mappingForm.values) }}</a-alert>
        </a-form>
        <div v-if="canEditRepairDDL(editing.object)" class="editor-toolbar">
          <a-space wrap><a-button v-if="gaussIndexPlanAvailable" size="small" :loading="refreshingIndexPlan" :disabled="!canRefreshIndexPlan" @click="refreshIndexPlan">重新生成索引计划</a-button><a-button v-if="nativeOracleDamengSequence" size="small" :loading="refreshingSequencePlan" :disabled="!canRefreshSequencePlan" @click="refreshSequencePlan">重新生成序列计划</a-button><a-button v-if="draftUnits.some(unit => unit.original_sql)" size="small" :disabled="busy || saving" @click="restoreOriginal">恢复原文</a-button><a-button v-if="editing.object.suggested_ddl && !pgTriggerEditor" size="small" :disabled="busy || saving" @click="useSuggested">采用建议 DDL</a-button><a-button v-if="editing.object.kind !== 'trigger'" size="small" :disabled="busy || saving || editing.object.status === 'unknown'" @click="addUnit">添加执行单元</a-button></a-space>
          <span class="muted">草稿版本 {{ editing.version }} · Ctrl / ⌘ + Enter 保存并执行</span>
        </div>
        <div v-for="(unit, index) in !canEditRepairDDL(editing.object) && (isGeneratedMySQLTrigger(editing.object) || editing.object.native || editing.object.manual_trigger) ? editing.object.units : []" :key="`trigger:${unit.id}`" class="unit-editor">
          <div class="unit-heading"><strong>{{ isMySQLPGTrigger(editing.object) || isManualPGTrigger(editing.object) ? pgTriggerUnitLabel(index) : `执行步骤 ${index + 1}` }}</strong><a-tag :color="statusColor(unit.status)">{{ repairStatus(unit.status) }}</a-tag></div>
          <p v-if="unit.error" class="error-text">{{ unit.error }}</p><pre class="sql-preview">{{ unit.sql }}</pre>
        </div>
        <div v-for="(unit, index) in canEditRepairDDL(editing.object) ? draftUnits : []" :key="unit.id" class="unit-editor">
          <div class="unit-heading"><strong v-if="editing.object.kind === 'trigger'">{{ pgTriggerEditor ? pgTriggerUnitLabel(index) : '触发器 SQL' }}</strong><a-checkbox v-else v-model="unit.selected" :disabled="busy || saving || unitCommitted(unit.status) || unit.destructive">执行步骤 {{ index + 1 }}</a-checkbox><a-tag :color="statusColor(unit.status)">{{ repairStatus(unit.status) }}</a-tag><span v-if="unit.destructive" class="error-text">破坏性步骤禁止重放</span><a-button v-if="editing.object.kind !== 'trigger' && !unitCommitted(unit.status) && !unit.destructive" type="text" size="mini" :disabled="busy || saving" @click="draftUnits.splice(index, 1)">移除此单元</a-button></div>
          <p v-if="unit.error" class="error-text">{{ unit.error }}</p>
          <pre v-if="unitCommitted(unit.status) || unit.destructive" class="sql-preview">{{ unit.sql }}</pre>
          <SqlEditor v-else v-model="unit.sql" class="repair-sql-editor" auto-height :read-only="busy || saving || !!triggerPlanCandidate" :dialect="editorDialect" :format-db-type="job?.dst_db_type" :format-disabled="busy || saving || !!triggerPlanCandidate || editing.object.status === 'unknown'" execution-mode="document" @execute="prepareExecute()" />
        </div>
      </template>
      <template #footer>
        <div v-if="editing" class="repair-editor-footer">
          <a-checkbox v-if="triggerPlanCandidate" v-model="triggerPlanReviewed" :disabled="saving || refreshingTriggerPlan">我已核对当前源定义和完整触发器方案</a-checkbox>
          <a-button v-if="triggerPlanCandidate" type="primary" :loading="saving" :disabled="busy || saving || !triggerPlanReviewed || !binding?.configured" @click="saveTriggerPlan">核对并保存触发器方案</a-button>
          <div class="modal-actions">
            <a-button @click="editorVisible = false">关闭</a-button>
            <a-button v-if="triggerPlanCandidate" :disabled="saving" @click="clearTriggerPlan">取消预览</a-button>
            <a-button v-if="pgTriggerEditor && canRetryMySQLPGTrigger(editing.object)" :disabled="!canRetryOriginalPGTrigger" @click="prepareExecute(true)">重试原 SQL</a-button>
            <a-button v-if="canEditRepairDDL(editing.object) && (draftUnits.length || Object.keys(mappingForm.values).length)" :loading="saving" :disabled="busy || !!triggerPlanCandidate || !pgTriggerDraftComplete || editing.object.status === 'unknown' || !!mappingError(mappingForm.values)" @click="saveDraft()">保存草稿</a-button>
            <a-button v-if="canExecuteRepairDDL(editing.object) && draftUnits.length" type="primary" :loading="submitting" :disabled="!canExecute" @click="prepareExecute()">{{ canEditRepairDDL(editing.object) ? '保存并执行' : editing.object.manual_trigger && editing.object.status === 'unknown' ? '核对并重试' : '重试原 SQL' }}</a-button>
          </div>
        </div>
      </template>
    </a-modal>

    <a-modal v-model:visible="bindingVisible" :mask-closable="false" :width="720" title="修复连接与原迁移配置" :footer="false">
      <a-alert type="info">仅可绑定与原任务数据库及 schema 匹配的已保存连接。连接身份由后端核验。</a-alert>
      <a-form :model="bindingForm" layout="vertical">
        <a-form-item label="源连接（接续数据需要）"><a-select v-model="bindingForm.src_conn_id" @change="selectSourceConnection" allow-search allow-clear placeholder="选择匹配的已保存源连接"><a-option v-for="conn in sourceConnections" :key="conn.id" :value="conn.id">{{ connectionLabel(conn) }}</a-option></a-select></a-form-item>
        <a-form-item label="目标连接"><a-select v-model="bindingForm.dst_conn_id" allow-search placeholder="选择匹配的已保存目标连接"><a-option v-for="conn in targetConnections" :key="conn.id" :value="conn.id">{{ connectionLabel(conn) }}</a-option></a-select></a-form-item>
        <template v-if="binding?.legacy && !binding.configured">
          <a-alert type="warning">旧任务缺少完整配置。请核对数据库与 schema，PG 数据库名不能用 schema 代替。</a-alert>
          <a-form-item label="源数据库"><a-input v-model="bindingForm.source_database" /></a-form-item>
          <a-form-item label="源 schema / owner"><a-input v-model="bindingForm.source_schema" /></a-form-item>
          <a-form-item label="目标 schema（与原任务相同）"><a-input v-model="bindingForm.config.TargetSchema" /></a-form-item>
          <a-form-item label="原迁移内容"><a-select v-model="bindingForm.config.Content" placeholder="请选择原任务的迁移内容"><a-option value="both">结构和数据</a-option><a-option value="schema_only">仅结构</a-option><a-option value="data_only">仅数据</a-option></a-select></a-form-item>
          <a-form-item label="仅对象模式（原任务不是仅对象时留空）"><a-select v-model="bindingForm.config.Objects" multiple><a-option value="primary_keys">主键</a-option><a-option value="indexes">索引</a-option><a-option value="sequences">序列</a-option><a-option value="foreign_keys">外键</a-option><a-option value="comments">注释</a-option><a-option v-if="job?.src_db_type === 'mysql' && ['postgres', 'gaussdb', 'highgo', 'vastbase', 'gbase', 'seabox', 'kingbase', 'dameng'].includes(job.dst_db_type)" value="triggers">触发器</a-option></a-select></a-form-item>
          <a-form-item label="每批行数"><a-input-number v-model="bindingForm.config.PageSize" :min="1" :max="100000" /></a-form-item>
          <a-form-item label="名称与类型设置"><a-space wrap><a-checkbox v-model="bindingForm.config.LowerCaseNames">名称转小写</a-checkbox><a-checkbox v-model="bindingForm.config.CharInLength">字符按长度</a-checkbox><a-checkbox v-model="bindingForm.config.UseNvarchar2">NVARCHAR2</a-checkbox><a-checkbox v-model="bindingForm.config.Distributed">分布属性</a-checkbox><a-checkbox v-model="bindingForm.config.ChangeOwner">修改 Owner</a-checkbox></a-space></a-form-item>
        </template>
      </a-form>
      <div class="modal-actions"><a-button @click="bindingVisible = false">取消</a-button><a-button type="primary" :loading="saving" :disabled="busy || !bindingForm.dst_conn_id || !!(binding?.legacy && !binding.configured && !bindingForm.config.Content)" @click="saveBinding">核验并保存绑定</a-button></div>
    </a-modal>

    <TableRecoveryModal v-if="recoveryObject" :key="recoveryObject.object.id" :visible="recoveryVisible"
      :object="recoveryObject.object" :form="recoveryForm" :choices="relatedChoices" :preview="recoveryPlan?.preview"
      :issue="recoveryIssue" :error="recoveryError" :busy="busy" :previewing="previewing" :submitting="submitting"
      @close="closeRecovery" @invalidate="invalidatePreview" @preview="loadPreview" @review="reviewRelated" @confirm="confirmRecovery"
    />

    <a-drawer v-model:visible="historyVisible" :width="760" title="修复执行历史" :footer="false">
      <a-button size="small" :loading="loadingHistory" @click="refreshHistory">刷新历史</a-button>
      <a-empty v-if="!visibleAttempts.length" description="尚无修复执行记录" />
      <article v-for="attempt in visibleAttempts" :key="attempt.id" class="history-attempt">
        <div class="unit-heading"><strong>{{ repairAttemptKind(attempt.kind) }}</strong><a-tag :color="statusColor(attempt.status)">{{ repairAttemptStatus(attempt) }}</a-tag><span>{{ attempt.created_at }}</span></div>
        <p class="muted">{{ objectName(attempt.object_id) }} · {{ attempt.id }}</p>
        <a-alert v-if="attempt.error" type="error">{{ attempt.error }}</a-alert>
        <a-alert v-if="attempt.verification" type="info">历史核验（旧版本）：{{ repairVerificationStatus(attempt.verification.status) }}：{{ attempt.verification.message }}</a-alert>
        <p v-if="attempt.kind === 'manual_verification'" class="muted">确认人：用户 #{{ attempt.owner_id }} · 确认时间：{{ attempt.finished_at || attempt.created_at }} · 核验依据：{{ attempt.verification?.message }}</p>
        <p v-if="attempt.result">已提交 {{ attempt.result.committed_rows }} 行 · 源 {{ attempt.result.source_rows }} 行 / 目标 {{ attempt.result.target_rows }} 行<span v-if="attempt.result.error"> · {{ attempt.result.error }}</span></p>
        <div v-for="(unit, index) in attempt.units" :key="unit.id"><p>步骤 {{ index + 1 }} · {{ repairExecutionStatus(unit.status) }}</p><pre class="sql-preview">{{ unit.sql }}</pre><p v-if="unit.error" class="error-text">{{ unit.error }}</p></div>
      </article>
    </a-drawer>
  </section>
</template>

<script setup lang="ts">
import { computed, defineAsyncComponent, nextTick, onUnmounted, reactive, ref, watch } from 'vue'
import { Message } from '@arco-design/web-vue'
import { getDataMigrationJob, type DataMigrationJob, type MigrationReport } from '@/api/migration'
import { listConnections, type Connection } from '@/api/connections'
import { bindRepairConnections, cancelRepairAttempt, executeRepair, getRepairAttempt, listRepairAttempts, listRepairs, previewRecovery, previewTriggerRepairPlan, saveTriggerRepairPlan, refreshSequenceRepairPlan, refreshIndexRepairPlan, saveRepairDraft, startRecovery, type TriggerRepairPlan, type RepairAttempt, type RepairBinding, type RepairConfig, type RepairObject, type RepairRecord, type RepairUnit, type RecoveryPlan } from '@/api/migrationRepair'
import { attemptRunning, canEditRepairDDL, canExecuteRepairDDL, canRegenerateTriggerPlan, canRetryMySQLPGTrigger, canRetryMySQLDamengTrigger, canRetryManualTrigger, isMySQLPGTrigger, isMySQLDamengTrigger, isGeneratedMySQLTrigger, isManualPGTrigger, cloneRepairUnits, cloneRepairExecutionUnits, formatRepairHistory, firstRecoveryIssue, qualifiedRepairName as qualified, recoveryUnavailableReason, type RecoveryForm, presentRepairRecords, hasRepairDDL, isTableRepair, mappingError, matchesRepairDisplayFilter, matchingRepairRecords, needsRepairDisplayAttention, repairAttemptKind, repairAttemptStatus, repairCategories, repairCountDifference, repairDisplayErrors, repairDisplayStatuses, repairExecutionStatus, repairKind, repairStatus, repairVerificationStatus, unitCommitted, type RepairCategory, type RepairDisplayRow, type RepairFilter, type RepairFocusTarget, type RepairSummary } from '@/utils/migrationRepair'
import TableRecoveryModal from './TableRecoveryModal.vue'
import { getDbTypeLabel } from '@/utils/dbType'
import { formatSQLText } from '@/utils/sqlFormatting'
import { batchIneligibleReason, batchBindingKey, buildRepairBatch, createRepairBatchQueue, type RepairBatchItem } from '@/utils/migrationRepairBatch'

const SqlEditor = defineAsyncComponent(() => import('@/components/SqlEditor.vue'))
const props = defineProps<{ jobID: string; report?: MigrationReport | null }>()
const emit = defineEmits<{ 'summary-change': [summary: RepairSummary]; 'show-row-difference': [table: string] }>()
const repairSection = ref<HTMLElement | null>(null)
const page = ref(1)
const highlightedObjectID = ref('')
const pendingFocus = ref<RepairFocusTarget | null>(null)
const records = ref<RepairRecord[]>([])
const binding = ref<RepairBinding | null>(null)
const job = ref<DataMigrationJob | null>(null)
const error = ref('')
const loading = ref(true)
const saving = ref(false)
const submitting = ref(false)
const cancelling = ref(false)
const activeAttempt = ref<RepairAttempt | null>(null)
const filter = ref<RepairFilter>('attention')
const keyword = ref('')
const errorDetailsVisible = ref(false)
const errorObjectID = ref('')
const editorVisible = ref(false)
const editing = ref<RepairRecord | null>(null)
const draftUnits = ref<RepairUnit[]>([])
const mappingForm = reactive({ values: {} as Record<string, string>, source: '', target: '' })
const refreshingIndexPlan = ref(false)
const indexPlanPending = ref(false)
const indexPlanMessage = ref('')
const refreshingSequencePlan = ref(false)
const sequencePlanPending = ref(false)
const sequencePlanMessage = ref('')
const refreshingTriggerPlan = ref(false)
const triggerPlanCandidate = ref<TriggerRepairPlan | null>(null)
const triggerPlanReviewed = ref(false)
const executionSnapshot = ref<RepairUnit[]>([])
const executeVersion = ref(0)
const executionKey = ref('')
const bindingVisible = ref(false)
const connections = ref<Connection[]>([])
const bindingForm = reactive({ src_conn_id: undefined as number | undefined, dst_conn_id: undefined as number | undefined, source_database: '', source_schema: '', config: { Content: 'both', MigrateTriggers: false, PageSize: 1000, LowerCaseNames: false, CharInLength: false, UseNvarchar2: false, Distributed: false, ChangeOwner: false, TargetSchema: '', Objects: [] } as RepairConfig })
const recoveryVisible = ref(false)
const recoveryObject = ref<RepairRecord | null>(null)
const recoveryError = ref('')
const recoveryForm = reactive<RecoveryForm>({ mapping: {}, related: {}, truncate: false, confirmation: '', sourceQuiesced: false, targetQuiesced: false, reviewed: false })
const recoveryPlan = ref<RecoveryPlan | null>(null)
const recoveryKey = ref('')
const returnToRecovery = ref(false)
const previewing = ref(false)
const historyVisible = ref(false)
const loadingHistory = ref(false)
const historyObjectID = ref('')
const attempts = ref<RepairAttempt[]>([])
const selectedObjectIDs = ref<string[]>([])
const batchPreviewVisible = ref(false)
const batchReviewed = ref(false)
const batchPreparing = ref(false)
const batchRunning = ref(false)
const batchStopped = ref(false)
const batchHasRun = ref(false)
const batchItems = ref<RepairBatchItem[]>([])
const batchError = ref('')
let batchExpectedBinding = ''
let batchSerial = 0
let batchQueue: ReturnType<typeof createRepairBatchQueue> | undefined
let epoch = 0
let refreshSerial = 0
let previewSerial = 0
let editorSerial = 0
let draftRevision = 0
let historySerial = 0
let timer: ReturnType<typeof setTimeout> | undefined
let pollSerial = 0
let disposed = false
let pendingExecution: { jobID: string; objectID: string; version: number; signature: string; key: string; retryOriginal: boolean } | null = null

const busy = computed(() => job.value?.status === 'running' || !!activeAttempt.value && attemptRunning(activeAttempt.value.status) || submitting.value || batchRunning.value || batchPreparing.value || refreshingSequencePlan.value || refreshingTriggerPlan.value || refreshingIndexPlan.value)
const pgTriggerJob = computed(() => job.value?.src_db_type === 'mysql' && ['postgres', 'gaussdb', 'highgo', 'vastbase', 'gbase', 'seabox', 'kingbase'].includes(job.value.dst_db_type))
const pgTriggerEditor = computed(() => !!editing.value && canEditPGTrigger(editing.value.object))
const pgTriggerDraftComplete = computed(() => !pgTriggerEditor.value || draftUnits.value.length === 2 && draftUnits.value.every(unit => !!unit.sql.trim()))
const triggerPlanAvailable = computed(() => pgTriggerJob.value && !!editing.value && canRegenerateTriggerPlan(editing.value.object))
const canRefreshTriggerPlan = computed(() => triggerPlanAvailable.value && !busy.value && !saving.value && !!binding.value?.configured && (!pgTriggerEditor.value || draftUnits.value.every(unit => !unit.sql.trim())))
const nativeOracleDamengSequence = computed(() => job.value?.src_db_type === 'oracle' && job.value?.dst_db_type === 'dameng' && editing.value?.object.kind === 'sequence' && !!editing.value.object.sequence?.Native?.Name)
const canRefreshSequencePlan = computed(() => {
  const object = editing.value?.object
  if (!nativeOracleDamengSequence.value || !object || busy.value || saving.value || !binding.value?.configured || object.sequence?.Unsupported && object.sequence?.FailureStage !== 'preflight' || object.sequence?.FailureStage === 'metadata') return false
  if (['unknown', 'running', 'unverified', 'needs_verification'].includes(object.status) || object.units.some(unit => ['unknown', 'running', 'unverified', 'needs_verification'].includes(unit.status) || unit.sql !== unit.original_sql)) return false
  const units = draftUnits.value.filter(unit => unit.sql.trim())
  return units.length === object.units.length && units.every((unit, index) => unit.id === object.units[index].id && (unit.sql === object.units[index].sql || unit.sql === formattedDraftSQL(object.units[index].sql)))
})
const gaussIndexPlanAvailable = computed(() => job.value?.src_db_type === 'mysql' && job.value?.dst_db_type === 'gaussdb' && !!editing.value?.object.index && ['primary_key', 'index'].includes(editing.value.object.kind) && (editing.value.object.index.IsPrimary || editing.value.object.index.IsUnique))
const canRefreshIndexPlan = computed(() => {
  const object = editing.value?.object
  if (!gaussIndexPlanAvailable.value || !object || busy.value || saving.value || !binding.value?.configured || indexPlanPending.value) return false
  const unsafe = (status: string) => ['unknown', 'running', 'unverified', 'needs_verification', 'succeeded'].includes(status)
  if (unsafe(object.status) || object.units.some(unit => unsafe(unit.status) || unit.sql !== unit.original_sql)) return false
  const units = draftUnits.value.filter(unit => unit.sql.trim())
  return units.length === object.units.length && units.every((unit, index) => unit.id === object.units[index].id && (unit.sql === object.units[index].sql || unit.sql === formattedDraftSQL(object.units[index].sql)))
})
const editorDialect = computed(() => job.value?.dst_db_type === 'mysql' ? 'mysql' as const : 'postgres' as const)
const canExecute = computed(() => {
  const object = editing.value?.object
  if (!object || busy.value || saving.value || triggerPlanCandidate.value || !binding.value?.configured || !canExecuteRepairDDL(object) || !pgTriggerDraftComplete.value || mappingError(mappingForm.values)) return false
  if (object.status === 'unknown' && !canRetryManualTrigger(object) && !object.native && !isMySQLDamengTrigger(object)) return false
  return draftUnits.value.some(unit => unit.selected && unit.sql.trim() && !unitCommitted(unit.status) && !unit.destructive)
})
const canRetryOriginalPGTrigger = computed(() => pgTriggerEditor.value && canExecute.value && !!editing.value && canRetryMySQLPGTrigger(editing.value.object) && editing.value.object.units.every((unit, index) => unit.sql === draftUnits.value[index]?.sql))
const historyAvailable = ref(false)
const displayRecords = computed(() => presentRepairRecords(records.value, attempts.value, props.report, historyAvailable.value))
const errorDetailRecord = computed(() => displayRecords.value.find(record => record.object.id === errorObjectID.value))
const visibleRecords = computed(() => displayRecords.value.filter(record => matchesRepairDisplayFilter(record, filter.value, attempts.value) && record.members.some(({ object }) => `${object.schema || ''} ${object.name} ${object.source_name || ''} ${object.target_name} ${object.table || ''} ${object.source_table || ''} ${object.original_error || ''} ${object.error || ''}`.toLowerCase().includes(keyword.value.trim().toLowerCase()))).map(record => {
  const batchReason = batchIneligibleReason(record.object)
  return { ...record, key: record.object.id, errors: repairDisplayErrors(record), difference: repairCountDifference(record), batchReason, disabled: busy.value || loading.value || !!batchReason }
}))
const batchExecutableCount = computed(() => batchItems.value.filter(item => item.status === 'pending').length)
const batchCompletedCount = computed(() => batchItems.value.filter(item => !['pending', 'saving', 'running'].includes(item.status)).length)
const stats = computed(() => {
  const counts: Record<RepairCategory, number> = { repaired: 0, pending: 0, blocked: 0, data: 0, review: 0 }
  for (const { key } of repairCategories) counts[key] = displayRecords.value.filter(record => matchesRepairDisplayFilter(record, key, attempts.value)).length
  return counts
})
const emptyDescription = computed(() => error.value ? '修复状态未知，请刷新重试' : loading.value ? '修复状态加载中' : keyword.value.trim() ? '没有匹配的对象' : filter.value === 'attention' ? '当前无待处理对象' : repairCategories.find(category => category.key === filter.value)?.empty || '当前无对象')
const summary = computed<RepairSummary>(() => ({ jobID: props.jobID, state: error.value ? 'unknown' : loading.value ? 'loading' : 'ready', repaired: stats.value.repaired, review: stats.value.review, attention: displayRecords.value.filter(needsRepairDisplayAttention).length }))
const sourceConnections = computed(() => connections.value.filter(conn => !job.value?.src_db_type || conn.db_type === job.value.src_db_type))
const targetConnections = computed(() => connections.value.filter(conn => !job.value?.dst_db_type || conn.db_type === job.value.dst_db_type))
const relatedChoices = computed(() => {
  const table = recoveryObject.value?.object
  if (!table) return []
  return records.value.filter(({ object }) => {
    if (object.id === table.id || isTableRepair(object) || unitCommitted(object.status)) return false
    const sameTable = object.table && (object.table === table.target_name || object.table === table.name)
    const sameSchema = !object.schema || !table.schema || object.schema === table.schema
    return object.dependencies?.includes(table.id) || (sameTable && sameSchema)
  }).map(({ object }) => ({ object, reason: recoveryUnavailableReason(object) }))
})
const relatedCandidates = computed(() => relatedChoices.value.filter(choice => !choice.reason))
const selectedRelatedIDs = computed(() => relatedCandidates.value.filter(({ object }) => recoveryForm.related[object.id]).map(({ object }) => object.id))
const historyObjectIDs = computed(() => {
  if (!historyObjectID.value) return null
  const row = displayRecords.value.find(record => record.members.some(member => member.object.id === historyObjectID.value))
  return new Set(row ? row.members.map(member => member.object.id) : [historyObjectID.value])
})
const visibleAttempts = computed(() => attempts.value.filter(attempt => !historyObjectIDs.value || historyObjectIDs.value.has(attempt.object_id)))
const recoveryIssue = computed(() => firstRecoveryIssue(recoveryForm, recoveryPlan.value?.preview))
const canRecover = computed(() => !!recoveryPlan.value && !recoveryIssue.value && !busy.value && !previewing.value)
const current = (scope: number, id: string) => !disposed && scope === epoch && id === props.jobID
const errorText = (e: any) => e?.response?.data?.error || e?.message || '请求失败，请重试'
function showError(e: unknown) { error.value = errorText(e); Message.error(error.value) }
function statusColor(status: string) { return unitCommitted(status) ? 'green' : attemptRunning(status) ? 'blue' : status === 'failed' ? 'red' : status === 'not_required' ? 'gray' : 'orange' }
function primaryRecord(row: RepairDisplayRow): RepairRecord { return row.members.find(member => member.object.id === row.object.id)! }
function openErrorDetails(row: RepairDisplayRow) { errorObjectID.value = row.object.id; errorDetailsVisible.value = true }
function closeErrorDetails() { errorDetailsVisible.value = false; errorObjectID.value = '' }
function objectName(id: string) { const object = records.value.find(record => record.object.id === id)?.object; return object ? qualified(object) : id }
function dependencyNames(object: RepairObject) { return (object.dependencies || []).map(objectName) }
function connectionLabel(conn: Connection) { return `${conn.name} · ${conn.host}:${conn.port}/${conn.database} · ${conn.username}` }
function newKey() {
  if (typeof crypto.randomUUID === 'function') return crypto.randomUUID()
  const bytes = crypto.getRandomValues(new Uint8Array(16))
  bytes[6] = (bytes[6] & 15) | 64; bytes[8] = (bytes[8] & 63) | 128
  const hex = Array.from(bytes, byte => byte.toString(16).padStart(2, '0')).join('')
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`
}
function replaceRecord(record: RepairRecord) { records.value = records.value.map(item => item.object.id === record.object.id ? record : item) }

function batchItemStatus(status: RepairBatchItem['status']) {
  return ({ pending: '等待执行', saving: '保存已审阅 SQL', running: '执行中', succeeded: '执行成功', failed: '执行失败', skipped: '已跳过', stopped: '未提交', unknown: '结果待核对' })[status]
}
function clearBatchSelection() {
  selectedObjectIDs.value = []
  if (!batchRunning.value) {
    batchSerial++
    batchPreparing.value = false
    batchPreviewVisible.value = false
    batchReviewed.value = false
  }
}
function selectAllFiltered() {
  if (busy.value || loading.value) return
  selectedObjectIDs.value = visibleRecords.value.filter(record => !batchIneligibleReason(record.object)).map(record => record.object.id)
}
async function prepareBatch() {
  if (busy.value || saving.value || loading.value || previewing.value || editorVisible.value || recoveryVisible.value || bindingVisible.value || !binding.value?.configured || !selectedObjectIDs.value.length) return
  const scope = epoch, id = props.jobID, serial = ++batchSerial, selected = [...selectedObjectIDs.value]
  batchPreparing.value = true
  batchReviewed.value = false
  try {
    const [data, detail] = await Promise.all([listRepairs(id), getDataMigrationJob(id)])
    if (!current(scope, id) || serial !== batchSerial) return
    records.value = data.data.objects || []; binding.value = data.data.binding; job.value = detail.data
    if (!binding.value.configured || job.value.status === 'running' || data.data.running_attempt_id) throw new Error('连接未配置或目标正在执行其他任务，请刷新后重试')
    batchExpectedBinding = batchBindingKey(binding.value)
    batchItems.value = buildRepairBatch(records.value, selected, newKey)
    batchHasRun.value = false; batchStopped.value = false; batchError.value = ''
    batchPreviewVisible.value = true
  } catch (e) { if (current(scope, id) && serial === batchSerial) showError(e) }
  finally { if (current(scope, id) && serial === batchSerial) batchPreparing.value = false }
}
function receiveAttempt(attempt: RepairAttempt) {
  activeAttempt.value = attempt
  attempts.value = [attempt, ...attempts.value.filter(item => item.id !== attempt.id)]
  if (unitCommitted(attempt.status) && editing.value?.object.id === attempt.object_id) editorVisible.value = false
  // A stopped queue may still have a server-side attempt. Refresh its result without resuming it.
  if (!batchRunning.value) batchItems.value = batchItems.value.map(item => {
    if (item.attemptID !== attempt.id) return item
    const status = unitCommitted(attempt.status) ? 'succeeded' : attemptRunning(attempt.status) ? 'running' : attempt.status === 'failed' ? 'failed' : attempt.status === 'cancelled' ? 'stopped' : 'unknown'
    return { ...item, status, reason: attempt.error || (status === 'unknown' ? '执行结果需在目标数据库核对' : status === 'stopped' ? '执行已取消' : '') }
  })
}
function stopAttemptPolling() {
  pollSerial++
  if (timer) clearTimeout(timer)
  timer = undefined
}
async function confirmBatch() {
  if (!batchPreviewVisible.value || !batchReviewed.value || !batchExecutableCount.value || busy.value || !binding.value?.configured) return
  const scope = epoch, id = props.jobID, serial = ++batchSerial
  const valid = () => current(scope, id) && serial === batchSerial
  batchRunning.value = true; batchStopped.value = false; batchHasRun.value = true
  batchPreviewVisible.value = false; batchError.value = ''; selectedObjectIDs.value = []
  stopAttemptPolling()
  const queue = createRepairBatchQueue(batchItems.value, batchExpectedBinding, {
    load: async () => {
      const [data, detail] = await Promise.all([listRepairs(id), getDataMigrationJob(id)])
      if (valid()) { records.value = data.data.objects || []; binding.value = data.data.binding; job.value = detail.data }
      return { list: data.data, jobRunning: detail.data.status === 'running' }
    },
    save: async (objectID, record) => {
      if (record.object.manual_trigger && !record.object.requires_review) return record
      if (!record.object.manual_trigger && (isGeneratedMySQLTrigger(record.object) || record.object.native)) return record
      const manual = record.object.manual_trigger
      const units = manual ? [{ id: record.object.units.find(unit => unit.sql === manual.sql)?.id || 'manual-trigger', sql: manual.sql, original_sql: '', status: 'pending', selected: true, destructive: false }] : cloneRepairUnits(record.object.units)
      const saved = (await saveRepairDraft(id, objectID, { version: record.version, units, reviewed: true, ...(manual ? { mode: 'manual_trigger' as const } : {}) })).data
      if (valid()) replaceRecord(saved)
      return saved
    },
    execute: async (objectID, version, requestID) => (await executeRepair(id, objectID, { version, request_id: requestID, confirmed: true })).data,
    getAttempt: async attemptID => (await getRepairAttempt(id, attemptID)).data,
    onItems: items => { if (valid()) batchItems.value = items },
    onAttempt: attempt => { if (valid()) receiveAttempt(attempt) },
    onError: message => { if (valid()) { batchError.value = message; batchStopped.value = true } },
  })
  batchQueue = queue
  try { await queue.run() }
  finally {
    if (valid()) {
      batchQueue = undefined; batchRunning.value = false
      await refresh()
    }
  }
}
function stopBatch() {
  if (!batchRunning.value) return
  batchStopped.value = true
  batchQueue?.stop()
}
function disposeBatch() {
  batchSerial++
  batchQueue?.dispose(); batchQueue = undefined
  batchRunning.value = false; batchPreparing.value = false
}

function selectRepairFilter(value: RepairFilter) {
  filter.value = value; keyword.value = ''; page.value = 1
  highlightedObjectID.value = ''; pendingFocus.value = null
}
function showAllObjects() { selectRepairFilter('all') }
async function focusObject(target: RepairFocusTarget) {
  pendingFocus.value = target
  await applyPendingFocus()
}
async function applyPendingFocus() {
  if (!pendingFocus.value || loading.value || error.value) return
  const scope = epoch, id = props.jobID, target = pendingFocus.value
  pendingFocus.value = null
  showAllObjects()
  const matchedIDs = new Set(matchingRepairRecords(records.value, target).map(record => record.object.id))
  const matches = displayRecords.value.filter(record => record.members.some(member => matchedIDs.has(member.object.id)))
  const match = matches.length === 1 ? matches[0] : undefined
  highlightedObjectID.value = match?.object.id || ''
  if (!match) keyword.value = target.target_name || target.source_name || target.name
  // Let filter watchers reset pagination before selecting the matching page.
  await nextTick()
  if (!current(scope, id)) return
  if (match) page.value = Math.floor(visibleRecords.value.findIndex(record => record.object.id === match.object.id) / 10) + 1
  await nextTick()
  if (!current(scope, id) || highlightedObjectID.value !== (match?.object.id || '')) return
  const anchor = match ? Array.from(repairSection.value?.querySelectorAll<HTMLElement>('[data-repair-object-id]') || []).find(element => element.dataset.repairObjectId === match.object.id) : repairSection.value
  anchor?.focus({ preventScroll: true })
  anchor?.scrollIntoView({ behavior: 'smooth', block: 'nearest' })
}

async function refresh() {
  const scope = epoch, id = props.jobID, serial = ++refreshSerial, historyRequest = ++historySerial
  loading.value = true
  error.value = ''
  try {
    const [data, detail, history] = await Promise.all([listRepairs(id), getDataMigrationJob(id), listRepairAttempts(id).catch(() => null)])
    if (!current(scope, id) || serial !== refreshSerial) return
    records.value = data.data.objects || []
    binding.value = data.data.binding
    if (historyRequest === historySerial) {
      loadingHistory.value = false
      historyAvailable.value = !!history
      if (history) attempts.value = history.data.items || []
    }
    job.value = detail.data
    const attemptID = data.data.running_attempt_id || (activeAttempt.value && attemptRunning(activeAttempt.value.status) ? activeAttempt.value.id : '')
    if (!batchRunning.value && attemptID && (activeAttempt.value?.id !== attemptID || attemptRunning(activeAttempt.value.status))) await pollAttempt(attemptID, scope, id)
  } catch (e) { if (current(scope, id) && serial === refreshSerial) error.value = errorText(e) }
  finally { if (current(scope, id) && serial === refreshSerial) loading.value = false }
}
async function pollAttempt(attemptID: string, scope = epoch, id = props.jobID) {
  if (batchRunning.value || !current(scope, id)) return
  const serial = ++pollSerial
  if (timer) clearTimeout(timer)
  try {
    const attempt = (await getRepairAttempt(id, attemptID)).data
    if (!current(scope, id) || serial !== pollSerial || batchRunning.value) return
    receiveAttempt(attempt)
    if (attemptRunning(attempt.status)) timer = setTimeout(() => void pollAttempt(attemptID, scope, id), 2000)
    else {
      await refresh()
      if (current(scope, id) && editorVisible.value && editing.value?.object.id === attempt.object_id) {
        const saved = records.value.find(record => record.object.id === attempt.object_id)
        if (saved) { editing.value = saved; draftUnits.value = editableDraftUnits(saved.object) }
      }
    }
  } catch (e) { if (current(scope, id) && serial === pollSerial && !batchRunning.value) { error.value = `进度暂时无法获取：${errorText(e)}。不会重新提交执行。`; timer = setTimeout(() => void pollAttempt(attemptID, scope, id), 5000) } }
}
async function cancelAttempt() {
  if (!activeAttempt.value) return
  stopBatch()
  const scope = epoch, id = props.jobID, attemptID = activeAttempt.value.id
  cancelling.value = true
  try { await cancelRepairAttempt(id, attemptID); if (current(scope, id) && !batchRunning.value) await pollAttempt(attemptID, scope, id) }
  catch (e) { if (current(scope, id)) showError(e) }
  finally { if (current(scope, id)) cancelling.value = false }
}
function openEditor(record: RepairRecord) {
  if (batchRunning.value || batchPreparing.value || batchPreviewVisible.value) return
  editorSerial++
  editing.value = record
  sequencePlanPending.value = false; sequencePlanMessage.value = ''; refreshingSequencePlan.value = false
  indexPlanPending.value = false; indexPlanMessage.value = ''; refreshingIndexPlan.value = false
  clearTriggerPlan()
  draftUnits.value = editableDraftUnits(record.object)
  if (canEditRepairDDL(record.object) && !draftUnits.value.length && !hasRepairDDL(record.object) && !unitCommitted(record.object.status) && record.object.status !== 'unknown') addUnit()
  mappingForm.values = { ...(record.object.column_mapping || {}) }
  for (const column of record.object.source_columns || record.object.columns || []) {
    if (!mappingForm.values[column.Name]) mappingForm.values[column.Name] = job.value?.lower_case_names ? column.Name.toLowerCase() : column.Name
  }
  mappingForm.source = ''; mappingForm.target = ''
  editorVisible.value = true
}
function canEditPGTrigger(object: RepairObject): boolean {
  return pgTriggerJob.value && object.kind === 'trigger' && canEditRepairDDL(object)
}
function pgTriggerUnitLabel(index: number): string {
  return index === 0 ? '触发器函数 SQL' : '创建触发器 SQL'
}
function unknownExecutionMessage(object: RepairObject): string {
  if (object.manual_trigger && !isManualPGTrigger(object)) return '上次执行结果不确定，当前 SQL 为只读。点击“核对并重试”先核对目标定义、编译和启用状态；定义一致则复用，确认不存在才重试原 SQL，冲突或无法核实时保留未知状态。'
  if (isMySQLDamengTrigger(object)) return '上次执行结果不确定。重试先核对目标目录；原定义一致、编译有效且已启用时复用，确认不存在才执行冻结的 CREATE；冲突或无法核实时保留未知状态。'
  if (object.native) return '上次执行结果不确定。重试时服务端先核对冻结计划与目标目录；只有能证明安全重试的步骤才会执行，已成功步骤不会重放。'
  if (isMySQLPGTrigger(object) || isManualPGTrigger(object)) return '上次提交结果不确定，请在目标数据库检查实际执行结果。'
  return '上次提交结果不确定，禁止再次提交这些执行单元；请在目标数据库检查实际执行结果。'
}
function editableDraftUnits(object: RepairObject): RepairUnit[] {
  let units = cloneRepairExecutionUnits(object)
  if (canEditPGTrigger(object)) {
    if ((isMySQLPGTrigger(object) || isManualPGTrigger(object)) && units.length === 2) return units.map(unit => ({ ...unit, selected: true }))
    return ['trigger-function', 'create-trigger'].map(id => ({ id, sql: '', original_sql: '', status: 'pending', selected: true, destructive: false }))
  }
  if (object.kind === 'trigger' && canEditRepairDDL(object)) {
    const create = units.find(unit => /^\s*(?:(?:--[^\n]*(?:\n|$)|\/\*[\s\S]*?\*\/)\s*)*CREATE\s+(?:OR\s+REPLACE\s+)?TRIGGER\b/i.test(unit.sql))
    const sql = object.manual_trigger?.sql ?? create?.sql ?? object.ddl ?? ''
    units = [{ id: create?.id || 'manual-trigger', sql, original_sql: create?.original_sql || object.ddl || sql, status: object.status === 'failed' ? 'failed' : 'pending', error: create?.error || object.error, selected: true, destructive: false }]
  }
  if (!canEditRepairDDL(object) || object.index?.gaussdb) return units
  return units.map(unit => {
    if (unitCommitted(unit.status) || unit.destructive || !unit.sql.trim()) return unit
    return { ...unit, sql: formattedDraftSQL(unit.sql) }
  })
}
function formattedDraftSQL(sql: string): string {
  try { return formatSQLText(sql, job.value?.dst_db_type || '') }
  catch { return sql }
}
function closeEditor() {
  editorSerial++; editing.value = null; draftUnits.value = []
  sequencePlanPending.value = false; sequencePlanMessage.value = ''; refreshingSequencePlan.value = false
  indexPlanPending.value = false; indexPlanMessage.value = ''; refreshingIndexPlan.value = false
  clearTriggerPlan()
  if (returnToRecovery.value) { returnToRecovery.value = false; recoveryVisible.value = true; void loadPreview() }
}
function addColumnMapping() {
  const source = mappingForm.source.trim(), target = mappingForm.target.trim()
  if (!source || !target) return
  if (Object.prototype.hasOwnProperty.call(mappingForm.values, source)) { Message.warning('该源列已有映射，请直接修改目标列'); return }
  mappingForm.values = { ...mappingForm.values, [source]: target }
  mappingForm.source = ''; mappingForm.target = ''
}
function addUnit() { if (editing.value && canEditRepairDDL(editing.value.object)) draftUnits.value.push({ id: newKey(), sql: '', original_sql: '', status: 'pending', selected: true, destructive: false }) }
function restoreOriginal() {
  if (!editing.value) return
  const originals = new Map((editing.value.object.units || []).map(unit => [unit.id, unit.original_sql]))
  draftUnits.value = draftUnits.value.map(unit => {
    const original = originals.get(unit.id) || unit.original_sql
    return !unitCommitted(unit.status) && !unit.destructive && original ? { ...unit, sql: pgTriggerEditor.value ? original : formattedDraftSQL(original) } : unit
  })
}
function useSuggested() {
  if (!editing.value?.object.suggested_ddl) return
  const editable = draftUnits.value.filter(unit => !unitCommitted(unit.status) && !unit.destructive)
  if (editable.length > 1) { Message.warning('多步骤建议需人工放入对应完整执行单元，避免覆盖已成功步骤'); return }
  const sql = formattedDraftSQL(editing.value.object.suggested_ddl)
  if (editable.length === 1) { editable[0].sql = sql; editable[0].selected = true }
  else draftUnits.value.push({ id: newKey(), sql, original_sql: '', status: 'pending', selected: true, destructive: false })
}
async function saveDraft(options: { silent?: boolean } = {}): Promise<boolean> {
  if (batchRunning.value || batchPreparing.value || !editing.value || !canEditRepairDDL(editing.value.object) || saving.value || triggerPlanCandidate.value || !pgTriggerDraftComplete.value || mappingError(mappingForm.values)) return false
  const scope = epoch, id = props.jobID, objectID = editing.value.object.id, serial = editorSerial, revision = draftRevision
  saving.value = true
  try {
    const units = draftUnits.value.map(unit => {
      const frozen = sequencePlanPending.value || indexPlanPending.value || editing.value!.object.index?.gaussdb ? editing.value!.object.units.find(original => original.id === unit.id) : undefined
      return { ...unit, sql: frozen && unit.sql === formattedDraftSQL(frozen.sql) ? frozen.sql : unit.sql }
    })
    const saved = (await saveRepairDraft(id, objectID, { version: editing.value.version, units, column_mapping: { ...mappingForm.values }, reviewed: true, ...(editing.value.object.kind === 'trigger' ? { mode: 'manual_trigger' as const } : {}), ...(sequencePlanPending.value ? { sequence_plan: true } : {}), ...(indexPlanPending.value ? { index_plan: true } : {}) })).data
    if (!current(scope, id) || serial !== editorSerial) return false
    replaceRecord(saved); editing.value = saved
    if (draftRevision !== revision) { Message.info('已保存提交时的草稿，当前新修改仍保留，请再次保存'); return false }
    draftUnits.value = editableDraftUnits(saved.object)
    sequencePlanPending.value = false; sequencePlanMessage.value = ''
    indexPlanPending.value = false; indexPlanMessage.value = ''
    mappingForm.values = { ...(saved.object.column_mapping || {}) }
    if (!options.silent) Message.success('草稿已保存')
    return true
  } catch (e) { if (current(scope, id) && serial === editorSerial) showError(e); return false }
  finally { if (current(scope, id)) saving.value = false }
}
async function refreshIndexPlan() {
  if (!canRefreshIndexPlan.value || !editing.value) return
  const scope = epoch, id = props.jobID, objectID = editing.value.object.id, serial = editorSerial, revision = draftRevision
  refreshingIndexPlan.value = true
  try {
    const candidate = (await refreshIndexRepairPlan(id, objectID, { version: editing.value.version })).data
    if (!current(scope, id) || serial !== editorSerial) return
    if (revision !== draftRevision) { Message.info('草稿已变化，未覆盖当前修改'); return }
    editing.value = { object: candidate.object, version: candidate.version }
    draftUnits.value = cloneRepairUnits(candidate.object.units)
    indexPlanPending.value = true; indexPlanMessage.value = candidate.message
  } catch (e) { if (current(scope, id) && serial === editorSerial) showError(e) }
  finally { if (current(scope, id) && serial === editorSerial) refreshingIndexPlan.value = false }
}
async function refreshSequencePlan() {
  if (!canRefreshSequencePlan.value || !editing.value) return
  const scope = epoch, id = props.jobID, objectID = editing.value.object.id, serial = editorSerial, revision = draftRevision
  refreshingSequencePlan.value = true
  try {
    const candidate = (await refreshSequenceRepairPlan(id, objectID, { version: editing.value.version })).data
    if (!current(scope, id) || serial !== editorSerial) return
    if (revision !== draftRevision) { Message.info('草稿已变化，未覆盖当前修改，请重新核对后生成计划'); return }
    editing.value = { object: candidate.object, version: candidate.version }
    draftUnits.value = cloneRepairUnits(candidate.object.units)
    sequencePlanPending.value = true; sequencePlanMessage.value = candidate.message
  } catch (e) { if (current(scope, id) && serial === editorSerial) showError(e) }
  finally { if (current(scope, id) && serial === editorSerial) refreshingSequencePlan.value = false }
}
function clearTriggerPlan() {
  triggerPlanCandidate.value = null; triggerPlanReviewed.value = false; refreshingTriggerPlan.value = false
}
async function refreshTriggerPlan() {
  if (!canRefreshTriggerPlan.value || !editing.value) return
  const scope = epoch, id = props.jobID, objectID = editing.value.object.id, serial = editorSerial, revision = draftRevision
  refreshingTriggerPlan.value = true; triggerPlanReviewed.value = false
  try {
    const candidate = (await previewTriggerRepairPlan(id, objectID, { version: editing.value.version })).data
    if (!current(scope, id) || serial !== editorSerial || !editorVisible.value) return
    if (revision !== draftRevision) { Message.info('草稿已变化，未覆盖当前修改'); return }
    triggerPlanCandidate.value = candidate
  } catch (e) { if (current(scope, id) && serial === editorSerial) showError(e) }
  finally { if (current(scope, id) && serial === editorSerial) refreshingTriggerPlan.value = false }
}
async function saveTriggerPlan(): Promise<boolean> {
  const candidate = triggerPlanCandidate.value
  if (!candidate || !editing.value || !triggerPlanReviewed.value || busy.value || saving.value || !binding.value?.configured) return false
  const scope = epoch, id = props.jobID, objectID = editing.value.object.id, serial = editorSerial
  saving.value = true
  try {
    const saved = (await saveTriggerRepairPlan(id, objectID, { version: candidate.version, binding_version: candidate.binding_version, plan_hash: candidate.plan_hash, reviewed: true })).data
    if (!current(scope, id) || serial !== editorSerial) return false
    replaceRecord(saved); editing.value = saved
    draftUnits.value = editableDraftUnits(saved.object)
    clearTriggerPlan()
    Message.success('触发器方案已保存，请核对执行步骤后确认执行'); return true
  } catch (e) { if (current(scope, id) && serial === editorSerial) showError(e); return false }
  finally { if (current(scope, id)) saving.value = false }
}
async function prepareExecute(retryOriginal = false) {
  if (!canExecute.value || !editing.value || retryOriginal && !canRetryOriginalPGTrigger.value) return
  if (draftUnits.value.some(unit => unit.selected && !unit.sql.trim())) { Message.error('选中的执行单元不能为空'); return }
  const scope = epoch, id = props.jobID, serial = editorSerial, objectID = editing.value.object.id
  const signature = () => JSON.stringify([draftUnits.value.map(unit => [unit.id, unit.sql, unit.selected]), mappingForm.values])
  const samePending = pendingExecution?.jobID === id && pendingExecution.objectID === objectID && pendingExecution.version === editing.value.version && pendingExecution.signature === signature() && pendingExecution.retryOriginal === retryOriginal
  submitting.value = true
  try {
    if (retryOriginal) {
      draftUnits.value = cloneRepairExecutionUnits(editing.value.object)
    } else if (canEditRepairDDL(editing.value.object)) {
      if (!samePending && !await saveDraft({ silent: true })) return
    } else if (editing.value.object.manual_trigger) {
      draftUnits.value = cloneRepairExecutionUnits(editing.value.object)
    } else if (isMySQLPGTrigger(editing.value.object)) {
      if (!canRetryMySQLPGTrigger(editing.value.object)) return
      draftUnits.value = editing.value.object.units.map(unit => ({ ...unit, selected: true }))
    } else if (isMySQLDamengTrigger(editing.value.object)) {
      if (!canRetryMySQLDamengTrigger(editing.value.object)) return
      draftUnits.value = cloneRepairExecutionUnits(editing.value.object)
    } else if (editing.value.object.native) {
      if (!canExecuteRepairDDL(editing.value.object)) return
      draftUnits.value = cloneRepairExecutionUnits(editing.value.object)
    }
    if (!current(scope, id) || serial !== editorSerial || !editing.value || editing.value.object.id !== objectID) return
    executionSnapshot.value = draftUnits.value.filter(unit => unit.selected && !unitCommitted(unit.status) && !unit.destructive).map(unit => ({ ...unit }))
    executeVersion.value = editing.value.version
    if (!samePending) pendingExecution = { jobID: id, objectID, version: executeVersion.value, signature: signature(), key: newKey(), retryOriginal }
    executionKey.value = pendingExecution!.key
    await confirmExecute()
  } finally { if (current(scope, id)) submitting.value = false }
}
async function confirmExecute() {
  if (batchRunning.value || batchPreparing.value || !editing.value || !submitting.value || !executionKey.value) return
  const scope = epoch, id = props.jobID, objectID = editing.value.object.id
  try {
    const attempt = (await executeRepair(id, objectID, { version: executeVersion.value, request_id: executionKey.value, confirmed: true })).data
    if (!current(scope, id)) return
    pendingExecution = null; receiveAttempt(attempt)
    await pollAttempt(attempt.id, scope, id)
  } catch (e) { if (current(scope, id)) showError(e) }
}
async function openBinding() {
  if (busy.value || batchPreviewVisible.value) return
  const scope = epoch, id = props.jobID
  try {
    const result = await listConnections()
    if (!current(scope, id) || busy.value || batchPreviewVisible.value) return
    connections.value = result.data
    bindingForm.src_conn_id = binding.value?.src_conn_id || undefined
    bindingForm.dst_conn_id = binding.value?.dst_conn_id || undefined
    const sourceConnection = connections.value.find(conn => conn.id === bindingForm.src_conn_id)
    const isPGSource = ['postgres', 'gaussdb', 'highgo', 'kingbase', 'gbase', 'vastbase', 'seabox'].includes(job.value?.src_db_type || '')
    // Older PG and Dameng snapshots contain the selected schema/owner, not the connection database.
    bindingForm.source_database = binding.value?.source_database || job.value?.source_database || sourceConnection?.database || (!isPGSource && job.value?.src_db_type !== 'dameng' ? job.value?.src_conn?.database : '') || ''
    bindingForm.source_schema = binding.value?.source_schema || job.value?.source_schema || ''
    bindingForm.config = { MigrateTriggers: !!job.value?.migrate_triggers, Content: binding.value?.legacy && !binding.value.configured ? '' : 'both', PageSize: job.value?.page_size || 1000, LowerCaseNames: !!job.value?.lower_case_names, CharInLength: !!job.value?.char_in_length, UseNvarchar2: !!job.value?.use_nvarchar2, Distributed: false, ChangeOwner: !!job.value?.change_owner, TargetSchema: binding.value?.target_schema || job.value?.dst_schema || '', Objects: (job.value?.migrate_objects || '').split(',').map(value => value.trim()).filter(Boolean) }
    bindingVisible.value = true
  } catch (e) { if (current(scope, id)) showError(e) }
}
function selectSourceConnection(value: unknown) {
  const connection = connections.value.find(conn => conn.id === Number(value))
  if (connection && binding.value?.legacy && !binding.value.configured) bindingForm.source_database = connection.database
}
async function saveBinding() {
  if (busy.value || saving.value || batchPreviewVisible.value) return
  if (binding.value?.legacy && !binding.value.configured && !bindingForm.config.Content) { Message.error('请选择原任务的迁移内容'); return }
  const scope = epoch, id = props.jobID
  saving.value = true
  try {
    await bindRepairConnections(id, { src_conn_id: bindingForm.src_conn_id || 0, dst_conn_id: bindingForm.dst_conn_id || 0, ...(binding.value?.legacy && !binding.value.configured ? { source_database: bindingForm.source_database, source_schema: bindingForm.source_schema, config: bindingForm.config } : {}) })
    if (!current(scope, id)) return
    bindingVisible.value = false; invalidatePreview(); await refresh(); Message.success('连接与配置已核验')
  } catch (e) { if (current(scope, id)) showError(e) }
  finally { if (current(scope, id)) saving.value = false }
}
function invalidatePreview() {
  previewSerial++
  recoveryPlan.value = null
  recoveryKey.value = ''
  recoveryError.value = ''
  recoveryForm.reviewed = false
  recoveryForm.truncate = false
  recoveryForm.confirmation = ''
  previewing.value = false
}
function closeRecovery() {
  recoveryVisible.value = false
  invalidatePreview()
}
function openRecovery(record: RepairRecord) {
  if (batchRunning.value || batchPreparing.value || batchPreviewVisible.value) return
  returnToRecovery.value = false
  invalidatePreview(); recoveryObject.value = record
  recoveryForm.mapping = { ...(record.object.column_mapping || {}) }
  recoveryForm.related = Object.fromEntries(relatedCandidates.value.map(item => [item.object.id, false]))
  recoveryForm.sourceQuiesced = false; recoveryForm.targetQuiesced = false
  recoveryVisible.value = true
  void loadPreview()
}
async function loadPreview() {
  if (busy.value || !recoveryObject.value || previewing.value || recoveryIssue.value?.field === 'mapping') return
  const scope = epoch, id = props.jobID, objectID = recoveryObject.value.object.id, serial = ++previewSerial
  recoveryError.value = ''; previewing.value = true; recoveryPlan.value = null; recoveryForm.reviewed = false
  try {
    const result = (await previewRecovery(id, objectID, { version: recoveryObject.value.version, ...(Object.keys(recoveryForm.mapping).length ? { column_mapping: { ...recoveryForm.mapping } } : {}), related_object_ids: selectedRelatedIDs.value })).data
    if (!current(scope, id) || serial !== previewSerial || !recoveryVisible.value) return
    await refresh()
    if (!current(scope, id) || serial !== previewSerial || !recoveryVisible.value) return
    recoveryObject.value = records.value.find(record => record.object.id === objectID) || recoveryObject.value
    recoveryForm.mapping = { ...result.preview.column_mapping }
    recoveryPlan.value = result; recoveryKey.value = newKey()
    if (!result.preview.data_required || result.preview.target_rows === 0) { recoveryForm.truncate = false; recoveryForm.confirmation = '' }
  } catch (e) {
    if (current(scope, id) && serial === previewSerial) {
      recoveryError.value = errorText(e)
      showError(e)
    }
  }
  finally { if (current(scope, id) && serial === previewSerial) previewing.value = false }
}
function reviewRelated(objectID: string) {
  const record = records.value.find(item => item.object.id === objectID)
  if (!record) { Message.error('关联对象已变化，请刷新后重新预览'); return }
  returnToRecovery.value = true; closeRecovery(); openEditor(record)
}
async function confirmRecovery() {
  if (!canRecover.value || !recoveryObject.value || !recoveryPlan.value || submitting.value) return
  const scope = epoch, id = props.jobID, objectID = recoveryObject.value.object.id
  const truncate = recoveryPlan.value.preview.data_required && recoveryPlan.value.preview.target_rows > 0 && recoveryForm.truncate
  recoveryError.value = ''; submitting.value = true
  try {
    const attempt = (await startRecovery(id, objectID, { token: recoveryPlan.value.token, request_id: recoveryKey.value, confirmed: true, source_quiesced: recoveryForm.sourceQuiesced, target_quiesced: recoveryForm.targetQuiesced, truncate, confirmation_text: truncate ? recoveryForm.confirmation : undefined })).data
    if (!current(scope, id)) return
    recoveryVisible.value = false; activeAttempt.value = attempt; await pollAttempt(attempt.id, scope, id)
  } catch (e) {
    if (current(scope, id)) {
      recoveryError.value = errorText(e)
      showError(e)
    }
  }
  finally { if (current(scope, id)) submitting.value = false }
}
async function refreshHistory() {
  const scope = epoch, id = props.jobID, serial = ++historySerial
  loadingHistory.value = true
  try {
    const result = await listRepairAttempts(id)
    if (current(scope, id) && serial === historySerial) {
      attempts.value = result.data.items || []
      historyAvailable.value = true
    }
  }
  catch (e) { if (current(scope, id) && serial === historySerial) showError(e) }
  finally { if (current(scope, id) && serial === historySerial) loadingHistory.value = false }
}
function openHistory(objectID = '') { historyObjectID.value = objectID; historyVisible.value = true; void refreshHistory() }
async function exportHistory() {
  const scope = epoch, id = props.jobID
  const [objects, history] = await Promise.all([listRepairs(id), listRepairAttempts(id)])
  if (!current(scope, id)) throw new Error('任务已切换，请重新导出')
  return formatRepairHistory(objects.data.objects || [], history.data.items || [], props.report)
}
defineExpose({ exportHistory, focusObject })
watch(summary, value => emit('summary-change', value), { immediate: true })
watch(errorDetailRecord, value => { if (!value) closeErrorDetails() })
watch(loading, value => { if (!value) void applyPendingFocus() })
watch(relatedCandidates, choices => {
  if (!recoveryVisible.value) return
  const eligible = new Set(choices.map(({ object }) => object.id))
  const removed = Object.keys(recoveryForm.related).filter(id => recoveryForm.related[id] && !eligible.has(id))
  if (!removed.length) return
  for (const id of removed) delete recoveryForm.related[id]
  invalidatePreview()
  recoveryError.value = '关联对象状态已变化，已移除不可选对象，请重新生成预览'
})
watch([filter, keyword], () => { page.value = 1; clearBatchSelection() }, { flush: 'sync' })
watch(() => visibleRecords.value.length, length => { page.value = Math.min(page.value, Math.max(1, Math.ceil(length / 10))) })
watch([draftUnits, () => mappingForm.values], () => {
  draftRevision++
}, { deep: true, flush: 'sync' })
watch(() => props.jobID, () => {
  disposeBatch(); stopAttemptPolling()
  epoch++; refreshSerial++; previewSerial++; editorSerial++; historySerial++
  selectedObjectIDs.value = []; batchItems.value = []; batchPreviewVisible.value = false; batchReviewed.value = false
  batchHasRun.value = false; batchStopped.value = false; batchError.value = ''; batchExpectedBinding = ''
  records.value = []; binding.value = null; job.value = null; activeAttempt.value = null; attempts.value = []; historyAvailable.value = false
  filter.value = 'attention'; keyword.value = ''; page.value = 1; highlightedObjectID.value = ''; pendingFocus.value = null
  closeErrorDetails()
  editorVisible.value = false; recoveryVisible.value = false; bindingVisible.value = false; historyVisible.value = false
  loading.value = false; saving.value = false; submitting.value = false; cancelling.value = false; previewing.value = false; loadingHistory.value = false
  sequencePlanPending.value = false; sequencePlanMessage.value = ''; refreshingSequencePlan.value = false
  indexPlanPending.value = false; indexPlanMessage.value = ''; refreshingIndexPlan.value = false
  clearTriggerPlan()
  mappingForm.values = {}; mappingForm.source = ''; mappingForm.target = ''
  returnToRecovery.value = false; editing.value = null; recoveryObject.value = null; draftUnits.value = []; recoveryPlan.value = null
  void refresh()
}, { immediate: true })
function leaveBatchPage() {
  const wasRunning = batchRunning.value
  stopBatch()
  disposeBatch(); stopAttemptPolling()
  if (wasRunning) {
    batchItems.value = batchItems.value.map(item => ['saving', 'running'].includes(item.status) ? { ...item, status: 'unknown', reason: '页面已离开，请通过执行历史核对提交结果' } : item)
    batchError.value = '后续排队已停止，已提交对象的结果可通过刷新或执行历史查看。'
  }
}
function restoreBatchPage(event: PageTransitionEvent) {
  if (event.persisted && !disposed) void refresh()
}
if (typeof window !== 'undefined') {
  window.addEventListener('pagehide', leaveBatchPage)
  window.addEventListener('pageshow', restoreBatchPage)
}
onUnmounted(() => {
  disposed = true; epoch++; leaveBatchPage()
  if (typeof window !== 'undefined') {
    window.removeEventListener('pagehide', leaveBatchPage)
    window.removeEventListener('pageshow', restoreBatchPage)
  }
})
</script>

<style scoped>
.repair-panel { min-width: 0; padding: 20px; margin: 18px 0; border: 1px solid var(--color-border-2); border-radius: 10px; background: var(--color-bg-1); container-type: inline-size; }
.repair-heading, .unit-heading, .editor-toolbar, .attempt-banner, .repair-filters, .modal-actions { display: flex; align-items: center; flex-wrap: wrap; gap: 10px; }
.repair-toolbar { display: flex; align-items: center; justify-content: space-between; flex-wrap: wrap; gap: 12px 20px; margin: 16px 0 12px; }
.repair-filters { flex: 1 1 400px; min-width: 0; gap: 8px; }
.repair-search { flex: 1 1 200px; min-width: 180px; max-width: 320px; }
.batch-toolbar { display: flex; align-items: center; flex-wrap: wrap; gap: 8px; }
.batch-progress { border: 1px solid var(--color-border-2); border-radius: 6px; padding: 12px; margin: 12px 0; }
.batch-results { max-height: 260px; overflow: auto; }
.batch-result { display: flex; align-items: center; flex-wrap: wrap; gap: 8px; padding: 6px 0; }
.batch-object-name { overflow-wrap: anywhere; }
.batch-reason { flex-basis: 100%; }
.batch-preview-items { max-height: 55vh; overflow: auto; margin: 16px 0; }
.batch-preview-item { border-bottom: 1px solid var(--color-border-2); padding: 10px 0; }
.batch-preview-item:first-child { padding-top: 0; }
.repair-heading, .editor-toolbar { justify-content: space-between; }
.repair-heading h3 { margin: 0; font-size: 16px; }
.repair-heading p, .muted { font-size: 12px; color: var(--color-text-3); overflow-wrap: anywhere; }
.repair-heading p { margin: 6px 0 0; line-height: 20px; }
.repair-stats { display: flex; flex-wrap: wrap; gap: 8px; margin-top: 18px; }
.repair-stat { display: inline-flex; align-items: center; gap: 10px; padding: 7px 12px; border: 1px solid var(--color-border-2); border-radius: 6px; background: var(--color-bg-2); color: var(--color-text-2); font: inherit; font-size: 12px; line-height: 20px; cursor: pointer; transition: background-color 150ms, border-color 150ms; }
.repair-stat:hover { background: var(--color-fill-2); }
.repair-stat-active { border-color: rgb(var(--primary-6)); background: var(--color-primary-light-1); color: rgb(var(--primary-6)); }
.repair-stat:focus-visible { outline: 2px solid rgb(var(--primary-6)); outline-offset: 2px; }
.repair-stats strong { font-size: 16px; font-weight: 600; font-variant-numeric: tabular-nums; }
.attempt-banner, .editor-toolbar { margin: 12px 0; }
.repair-count-detail { margin-top: 4px; color: var(--color-text-2); font-size: 12px; }
.repair-statuses { display: flex; align-items: center; flex-wrap: nowrap; gap: 12px; white-space: nowrap; }
.repair-status-line { display: flex; align-items: center; flex-shrink: 0; gap: 4px; white-space: nowrap; }
.repair-status-label { flex-shrink: 0; font-size: 12px; color: var(--color-text-3); }
.repair-status-line :deep(.arco-tag) { height: auto; min-height: 20px; padding: 0 6px; font-size: 12px; line-height: 20px; white-space: nowrap; overflow-wrap: normal; }
.error-text { color: var(--color-danger-6); overflow-wrap: anywhere; white-space: pre-wrap; }
.repair-error-cell { display: flex; align-items: center; gap: 8px; min-width: 0; }
.repair-error-summary { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; color: var(--color-text-2); line-height: 22px; }
.repair-error-phase { color: var(--color-text-3); }
.repair-error-actions { display: flex; align-items: center; flex-shrink: 0; gap: 8px; height: 22px; white-space: nowrap; }
.repair-error-actions :deep(.arco-btn) { height: 22px; padding: 0; font-size: 12px; }
.repair-error-object { margin: 0; font-weight: 600; overflow-wrap: anywhere; }
.repair-error-details { display: grid; gap: 12px; margin-top: 16px; }
.repair-error-detail { padding: 14px; border: 1px solid var(--color-border-2); border-radius: 6px; background: var(--color-bg-2); }
.repair-error-detail h4 { margin: 0 0 8px; font-size: 13px; color: var(--color-text-2); }
.repair-error-detail pre { margin: 0; font: 13px/1.7 var(--font-mono, monospace); color: var(--color-text-1); white-space: pre-wrap; overflow-wrap: anywhere; user-select: text; }
.repair-panel :deep(.arco-table-cell) { overflow-wrap: anywhere; }
.repair-table :deep(.arco-table-td) { height: 44px; box-sizing: border-box; }
.repair-table :deep(.arco-table-td .arco-table-cell) { padding: 9px 12px; }
.repair-table :deep(.arco-table-th .arco-table-cell) { padding: 10px 12px; color: var(--color-text-3); font-size: 12px; }
.repair-object-cell { display: flex; align-items: center; gap: 10px; min-width: 0; white-space: nowrap; }
.repair-object-heading { display: flex; align-items: center; flex: 0 1 auto; gap: 7px; min-width: 0; line-height: 22px; }
.repair-object-heading > .repair-name { flex: 0 1 auto; font-size: 13px; font-weight: 600; }
.repair-kind { flex-shrink: 0; padding: 0 5px; border-radius: 4px; background: var(--color-fill-2); color: var(--color-text-3); font-size: 11px; line-height: 18px; }
.repair-batch-hint { display: inline-flex; align-items: center; justify-content: center; flex-shrink: 0; width: 22px; height: 22px; margin: 0; padding: 0; border: 0; border-radius: 4px; background: transparent; color: var(--color-text-4); font-size: 14px; cursor: help; }
.repair-batch-hint:hover { color: var(--color-text-2); background: var(--color-fill-2); }
.repair-batch-hint:focus-visible { outline: 2px solid rgb(var(--primary-6)); outline-offset: 1px; }
.repair-object-meta { display: flex; align-items: center; flex: 0 0 auto; gap: 6px; min-width: 64px; max-width: 45%; font-size: 12px; line-height: 22px; color: var(--color-text-3); }
.repair-meta-divider { flex-shrink: 0; color: var(--color-text-4); }
.repair-row-actions { display: flex; align-items: center; flex-wrap: nowrap; gap: 2px; white-space: nowrap; }
.repair-row-actions :deep(.arco-btn) { flex-shrink: 0; padding: 0 6px; }
.repair-name { display: block; min-width: 0; max-width: 100%; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.repair-name-tooltip { display: block; max-width: min(360px, calc(100vw - 48px)); overflow-wrap: anywhere; white-space: normal; }
.repair-empty { text-align: center; }
.repair-panel :deep(.repair-row-highlighted .arco-table-td) { background: var(--color-primary-light-1); }
.repair-panel [data-repair-object-id]:focus { outline: 2px solid rgb(var(--primary-6)); outline-offset: 3px; }
.unit-editor { border: 1px solid var(--color-border-2); border-radius: 4px; margin: 8px 0; overflow: hidden; }
.unit-heading { padding: 10px; background: var(--color-fill-1); }
.unit-editor > .unit-heading { padding: 6px 10px; gap: 8px; }
.unit-editor > p { padding: 0 12px; }
.sql-preview { padding: 12px; border-radius: 4px; background: #111827; color: #e5e7eb; font-size: 12px; line-height: 1.6; white-space: pre-wrap; overflow-wrap: anywhere; max-height: 320px; overflow: auto; }
.unit-editor > .sql-preview { box-sizing: border-box; margin: 0; max-height: 240px; }
.unit-editor .repair-sql-editor :deep(.cm-scroller) { min-height: min(180px, 30dvh); max-height: min(320px, 40dvh); }
.modal-actions { justify-content: flex-end; margin-top: 18px; }
.repair-editor-footer { display: flex; flex-direction: column; gap: 12px; text-align: left; }
.repair-editor-footer .modal-actions { margin-top: 0; }
.editor-mapping { margin-top: 16px; }
.mapping-list { width: 100%; display: grid; gap: 8px; }
.mapping-row { display: grid; grid-template-columns: minmax(100px, 1fr) 20px minmax(120px, 1fr); gap: 8px; align-items: center; }
.editable-mapping { grid-template-columns: minmax(100px, 1fr) 20px minmax(120px, 1fr) 50px; }
.mapping-row code { overflow-wrap: anywhere; }
.history-attempt { margin: 16px 0; padding-bottom: 12px; border-bottom: 1px solid var(--color-border-2); }
@container (max-width: 767px) {
  .repair-filters { flex-basis: 100%; }
  .repair-search { max-width: none; }
  .batch-toolbar { width: 100%; }
}
</style>
