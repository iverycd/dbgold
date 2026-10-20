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
    <a-table v-if="records.length" class="repair-table" v-model:selected-keys="selectedObjectIDs" :row-selection="{ type: 'checkbox', onlyCurrent: false, showCheckedAll: true }" :data="visibleRecords" row-key="key" :pagination="{ pageSize: 10, current: page }" :scroll="{ x: 1040 }" size="small" :row-class="record => record.object.id === highlightedObjectID ? 'repair-row-highlighted' : ''" @page-change="page = $event">
      <template #empty>
        <div class="repair-empty">
          <a-empty :description="emptyDescription" />
          <a-button v-if="filter !== 'all' || keyword.trim()" type="text" size="small" @click="showAllObjects">查看全部对象</a-button>
        </div>
      </template>
      <template #columns>
        <a-table-column title="对象" :width="280">
          <template #cell="{ record }">
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
          </template>
        </a-table-column>
        <a-table-column title="原始错误"><template #cell="{ record }">
          <div v-if="record.errors.length" class="repair-error-cell">
            <div class="repair-error-summary"><span v-if="record.errors[0].label" class="repair-error-phase">{{ record.errors[0].label }}：</span>{{ record.errors[0].text.replace(/\s+/g, ' ') }}</div>
            <div class="repair-error-actions">
              <span v-if="record.errors.length > 1" class="muted">共 {{ record.errors.length }} 条</span>
              <a-button type="text" size="mini" :aria-label="`查看 ${qualified(record.object)} 的全部原始错误`" @click="openErrorDetails(record)">详情</a-button>
            </div>
          </div>
          <span v-else class="muted">—</span>
        </template></a-table-column>
        <a-table-column title="修复状态" :width="210">
          <template #cell="{ record }">
            <div class="repair-statuses">
              <div v-for="state in repairDisplayStatuses(record)" :key="state.label" class="repair-status-line">
                <span v-if="state.label" class="repair-status-label">{{ state.label }}：</span>
                <a-tag :color="statusColor(state.status)">{{ state.text }}</a-tag>
              </div>
            </div>
          </template>
        </a-table-column>
        <a-table-column title="操作" :width="220"><template #cell="{ record }"><div class="repair-row-actions">
          <a-button v-if="canEditRepairDDL(record.object)" type="text" size="mini" :disabled="busy" @click="openEditor(primaryRecord(record))">{{ hasRepairDDL(record.object) ? '编辑 DDL' : '编写 DDL' }}</a-button>
          <a-button v-else type="text" size="mini" @click="openEditor(primaryRecord(record))">查看诊断</a-button>
          <a-button v-if="isTableRepair(record.object) && !record.object.diagnostic_only" type="text" size="mini" :disabled="busy || !binding?.configured" @click="openRecovery(primaryRecord(record))">接续迁移</a-button>
          <a-button type="text" size="mini" @click="openHistory(record.object.id)">历史</a-button>
        </div></template></a-table-column>
      </template>
    </a-table>
    <a-spin v-else-if="loading" />
    <a-empty v-else-if="!error" :description="emptyDescription" />

    <a-modal v-model:visible="errorDetailsVisible" :mask-closable="false" width="min(800px, calc(100vw - 48px))" title="原始错误详情" :body-style="{ maxHeight: '60vh', overflow: 'auto' }" @close="closeErrorDetails">
      <template v-if="errorDetailRecord">
        <p class="repair-error-object">{{ qualified(errorDetailRecord.object) }}</p>
        <p class="muted">原始迁移错误保留供排查，当前结果以修复状态为准。</p>
        <div class="repair-error-details">
          <article v-for="(detail, index) in repairDisplayErrors(errorDetailRecord)" :key="`${detail.label}:${detail.text}`" class="repair-error-detail">
            <h4>{{ index + 1 }}. {{ detail.label ? `${detail.label}错误` : '原始错误' }}</h4>
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
      width="min(960px, calc(100vw - 48px))"
      :align-center="true"
      :modal-style="{ display: 'inline-flex', flexDirection: 'column', boxSizing: 'border-box', maxHeight: 'calc(100dvh - 48px)' }"
      :body-style="{ flex: '1 1 auto', minHeight: '0', overflow: 'auto' }"
      :title="`对象修复 · ${editing ? qualified(editing.object) : ''}`"
      @close="closeEditor"
    >
      <template v-if="editing">
        <p>目标数据库：{{ getDbTypeLabel(job?.dst_db_type || '') }} · Schema：{{ editing.object.schema || '—' }} · 对象：{{ editing.object.target_name }}<span v-if="editing.object.table"> · 所属表：{{ editing.object.table }}</span></p>
        <a-alert v-if="canEditRepairDDL(editing.object)" type="info">请使用目标数据库语法编写 DDL，每个执行单元填写一条完整语句。只重新创建或修复此对象；表数据及关联对象按需单独接续。</a-alert>
        <a-alert v-else type="info">此记录仅供诊断；请先处理连接、元数据或预检问题。数据失败可使用“接续迁移”。</a-alert>
        <a-alert v-if="editing.object.status === 'unknown'" type="warning">上次提交结果不确定，禁止再次提交这些执行单元；请在目标数据库检查实际执行结果。</a-alert>
        <a-alert v-if="editing.object.original_error ?? editing.object.error" type="error">原始错误：{{ editing.object.original_error ?? editing.object.error }}</a-alert>
        <a-alert v-if="editing.object.original_error !== undefined && editing.object.error && editing.object.original_error !== editing.object.error" type="warning">当前错误：{{ editing.object.error }}</a-alert>
        <a-alert v-if="editing.object.requires_review" type="warning">请按数据库驱动可一次执行的完整语句整理执行步骤。历史脚本不会按分号自动拆分，过去提交结果不能确定时禁止自动重放。</a-alert>
        <p v-if="editing.object.dependencies?.length">依赖：{{ dependencyNames(editing.object).join('、') }}</p>
        <p v-if="editing.object.foreign_key">引用：{{ editing.object.foreign_key.Columns.join(', ') }} → {{ editing.object.foreign_key.RefSchema }}.{{ editing.object.foreign_key.RefTable }} ({{ editing.object.foreign_key.RefColumns.join(', ') }})</p>
        <a-collapse><a-collapse-item v-if="editing.object.source_ddl" key="source" header="源端定义（只读，仅供手工适配参考）"><pre class="sql-preview">{{ editing.object.source_ddl }}</pre></a-collapse-item><a-collapse-item v-if="editing.object.suggested_ddl" key="suggested" header="建议 DDL（需显式采用或整理到对应执行单元）"><pre class="sql-preview">{{ editing.object.suggested_ddl }}</pre></a-collapse-item><a-collapse-item key="original" header="原始目标 DDL（只读）"><pre class="sql-preview">{{ editing.object.ddl || (canEditRepairDDL(editing.object) ? '未生成目标 DDL，请根据源端定义手动编写。' : '该记录没有可执行的对象 DDL。') }}</pre></a-collapse-item></a-collapse>
        <a-form v-if="isTableRepair(editing.object) && canEditRepairDDL(editing.object)" :model="mappingForm" layout="vertical" class="editor-mapping">
          <a-form-item label="源列 → 目标列映射（修改建表列名时同时保存）">
            <div class="mapping-list"><div v-for="source in Object.keys(mappingForm.values)" :key="source" class="mapping-row editable-mapping"><code>{{ source }}</code><span>→</span><a-input v-model="mappingForm.values[source]" placeholder="修复 DDL 中的目标列名" /><a-button type="text" size="mini" :disabled="(editing.object.source_columns || editing.object.columns || []).some(column => column.Name === source)" @click="delete mappingForm.values[source]">移除</a-button></div><span v-if="!Object.keys(mappingForm.values).length" class="muted">旧报告没有列元数据时，可按源表与修复 DDL 显式补充映射；接续预检会核对实际列。</span></div>
          </a-form-item>
          <a-form-item label="补充列映射"><div class="mapping-row"><a-input v-model="mappingForm.source" placeholder="源列原始名称" /><span>→</span><a-input v-model="mappingForm.target" placeholder="目标列原始名称" /></div><a-button size="small" style="margin-left: 8px" :disabled="!mappingForm.source.trim() || !mappingForm.target.trim()" @click="addColumnMapping">添加</a-button></a-form-item>
          <a-alert v-if="mappingError(mappingForm.values)" type="error">{{ mappingError(mappingForm.values) }}</a-alert>
        </a-form>
        <div v-if="canEditRepairDDL(editing.object)" class="editor-toolbar">
          <a-space wrap><a-button v-if="editing.object.units?.some(unit => unit.original_sql)" size="small" @click="restoreOriginal">恢复原文</a-button><a-button v-if="editing.object.suggested_ddl" size="small" @click="useSuggested">采用建议 DDL</a-button><a-button size="small" :disabled="busy || editing.object.status === 'unknown'" @click="addUnit">添加执行单元</a-button></a-space>
          <span class="muted">草稿版本 {{ editing.version }} · Ctrl / ⌘ + Enter 打开执行确认</span>
        </div>
        <div v-for="(unit, index) in canEditRepairDDL(editing.object) ? draftUnits : []" :key="unit.id" class="unit-editor">
          <div class="unit-heading"><a-checkbox v-model="unit.selected" :disabled="unitCommitted(unit.status) || unit.destructive">执行步骤 {{ index + 1 }}</a-checkbox><a-tag :color="statusColor(unit.status)">{{ repairStatus(unit.status) }}</a-tag><span v-if="unit.destructive" class="error-text">破坏性步骤禁止重放</span><a-button v-if="!unitCommitted(unit.status) && !unit.destructive" type="text" size="mini" @click="draftUnits.splice(index, 1)">移除此单元</a-button></div>
          <p v-if="unit.error" class="error-text">{{ unit.error }}</p>
          <pre v-if="unitCommitted(unit.status) || unit.destructive" class="sql-preview">{{ unit.sql }}</pre>
          <SqlEditor v-else v-model="unit.sql" auto-height :dialect="editorDialect" :format-db-type="job?.dst_db_type" :format-disabled="busy || saving || editing.object.status === 'unknown'" execution-mode="document" @execute="prepareExecute" />
        </div>
      </template>
      <template #footer>
        <div v-if="editing" class="repair-editor-footer">
          <a-checkbox v-if="canEditRepairDDL(editing.object) && editing.object.requires_review" v-model="reviewed">我已核对完整执行单元、目标对象及历史提交状态</a-checkbox>
          <div class="modal-actions"><a-button @click="editorVisible = false">关闭</a-button><a-button v-if="canEditRepairDDL(editing.object) && (draftUnits.length || Object.keys(mappingForm.values).length)" :loading="saving" :disabled="busy || editing.object.status === 'unknown' || !!mappingError(mappingForm.values)" @click="saveDraft">保存草稿</a-button><a-button v-if="canEditRepairDDL(editing.object) && draftUnits.length" type="primary" :loading="saving" :disabled="!canExecute" @click="prepareExecute">确认执行步骤</a-button></div>
        </div>
      </template>
    </a-modal>

    <a-modal v-model:visible="executeConfirmVisible" :mask-closable="false" :width="800" title="确认执行 DDL" :footer="false">
      <p>目标：<strong>{{ editing ? qualified(editing.object) : '' }}</strong> · 草稿版本 {{ executeVersion }}</p>
      <a-alert type="warning">以下完整执行单元将依次提交。已成功和破坏性步骤不会重放；部分数据库无法整体回滚。</a-alert>
      <pre v-for="unit in executionSnapshot" :key="unit.id" class="sql-preview">{{ unit.sql }}</pre>
      <div class="modal-actions"><a-button @click="executeConfirmVisible = false">返回编辑</a-button><a-button type="primary" :loading="submitting" @click="confirmExecute">执行以上 DDL</a-button></div>
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
          <a-form-item label="仅对象模式（原任务不是仅对象时留空）"><a-select v-model="bindingForm.config.Objects" multiple><a-option value="primary_keys">主键</a-option><a-option value="indexes">索引</a-option><a-option value="sequences">序列</a-option><a-option value="foreign_keys">外键</a-option><a-option value="comments">注释</a-option></a-select></a-form-item>
          <a-form-item label="每批行数"><a-input-number v-model="bindingForm.config.PageSize" :min="1" :max="100000" /></a-form-item>
          <a-form-item label="名称与类型设置"><a-space wrap><a-checkbox v-model="bindingForm.config.LowerCaseNames">名称转小写</a-checkbox><a-checkbox v-model="bindingForm.config.CharInLength">字符按长度</a-checkbox><a-checkbox v-model="bindingForm.config.UseNvarchar2">NVARCHAR2</a-checkbox><a-checkbox v-model="bindingForm.config.Distributed">分布属性</a-checkbox><a-checkbox v-model="bindingForm.config.ChangeOwner">修改 Owner</a-checkbox></a-space></a-form-item>
        </template>
      </a-form>
      <div class="modal-actions"><a-button @click="bindingVisible = false">取消</a-button><a-button type="primary" :loading="saving" :disabled="busy || !bindingForm.dst_conn_id || !!(binding?.legacy && !binding.configured && !bindingForm.config.Content)" @click="saveBinding">核验并保存绑定</a-button></div>
    </a-modal>

    <a-modal v-model:visible="recoveryVisible" :mask-closable="false" :width="920" title="接续此表迁移" :footer="false" @close="invalidatePreview">
      <a-alert type="warning">补迁读取当前源数据。请在执行期间停止源表和目标表写入。导入中断后保留已提交数据；非空表必须确认清空重导。</a-alert>
      <p v-if="recoveryObject">源表 {{ recoveryObject.object.source_name || recoveryObject.object.table || recoveryObject.object.name }} → 目标 {{ qualified(recoveryObject.object) }}</p>
      <a-form :model="recoveryForm" layout="vertical">
        <a-form-item label="源列 → 目标列映射">
          <div class="mapping-list"><div v-for="source in Object.keys(recoveryForm.mapping)" :key="source" class="mapping-row"><code>{{ source }}</code><span>→</span><a-input v-model="recoveryForm.mapping[source]" placeholder="目标列原始名称" @input="invalidatePreview" /></div><span v-if="!Object.keys(recoveryForm.mapping).length" class="muted">先获取预览，读取源列及默认映射。</span></div>
        </a-form-item>
        <a-form-item label="此次接续的关联对象"><div class="related-list"><a-checkbox v-for="record in relatedCandidates" :key="record.object.id" v-model="recoveryForm.related[record.object.id]" @change="invalidatePreview">{{ repairKind(record.object.kind) }} · {{ qualified(record.object) }}（{{ repairStatus(record.object.status) }}）</a-checkbox><span v-if="!relatedCandidates.length" class="muted">无待执行关联对象</span></div></a-form-item>
      </a-form>
      <a-button :loading="previewing" :disabled="!!mappingError(recoveryForm.mapping)" @click="loadPreview">{{ recoveryPlan ? '重新生成预览' : '检查列映射并生成预览' }}</a-button>
      <p v-if="mappingError(recoveryForm.mapping)" class="error-text">{{ mappingError(recoveryForm.mapping) }}</p>
      <template v-if="recoveryPlan">
        <dl class="preview-facts"><div><dt>源表</dt><dd>{{ recoveryPlan.preview.source_table }}（{{ recoveryPlan.preview.source_rows }} 行）</dd></div><div><dt>目标表</dt><dd>{{ recoveryPlan.preview.qualified_target }}（{{ recoveryPlan.preview.target_rows }} 行）</dd></div><div><dt>数据步骤</dt><dd>{{ recoveryPlan.preview.data_required ? '完整导入此表数据' : '数据无需重导，仅处理确认的关联对象' }}</dd></div></dl>
        <a-alert v-for="warning in recoveryPlan.preview.warnings" :key="warning" type="warning">{{ warning }}</a-alert>
        <a-alert v-if="recoveryPlan.preview.related?.some(object => object.requires_review)" type="warning">部分关联对象因列映射变化需要逐项复核草稿。保存复核后重新生成接续预览。</a-alert>
        <a-collapse v-if="recoveryPlan.preview.related?.length"><a-collapse-item v-for="object in recoveryPlan.preview.related" :key="object.id" :header="`${repairKind(object.kind)} · ${qualified(object)}${object.requires_review ? ' · 需要复核' : ''}`"><a-button v-if="object.requires_review" size="small" @click="reviewRelated(object.id)">编辑并复核此对象</a-button><pre class="sql-preview">{{ (object.units || []).map(unit => unit.sql).join('\n\n') || object.ddl }}</pre></a-collapse-item></a-collapse>
        <a-form :model="recoveryForm" layout="vertical">
          <a-form-item v-if="recoveryPlan.preview.data_required && recoveryPlan.preview.target_rows > 0" label="目标表已有数据">
            <div><a-checkbox v-model="recoveryForm.truncate">清空本表后完整重导，保留修复后的结构</a-checkbox><p>请输入目标完整表名 <strong>{{ recoveryPlan.preview.qualified_target }}</strong></p><a-input v-model="recoveryForm.confirmation" placeholder="逐字输入上方完整表名" /></div>
          </a-form-item>
          <a-form-item label="执行前确认"><a-space direction="vertical"><a-checkbox v-model="recoveryForm.sourceQuiesced">源表已停止写入</a-checkbox><a-checkbox v-model="recoveryForm.targetQuiesced">目标表已停止写入</a-checkbox><a-checkbox v-model="recoveryForm.reviewed">已核对列映射和以上关联对象 DDL</a-checkbox></a-space></a-form-item>
        </a-form>
      </template>
      <div class="modal-actions"><a-button @click="recoveryVisible = false">关闭</a-button><a-button type="primary" :disabled="!canRecover" :loading="submitting" @click="confirmRecovery">确认接续迁移</a-button></div>
    </a-modal>

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
import { getDataMigrationJob, type DataMigrationJob } from '@/api/migration'
import { listConnections, type Connection } from '@/api/connections'
import { bindRepairConnections, cancelRepairAttempt, executeRepair, getRepairAttempt, listRepairAttempts, listRepairs, previewRecovery, saveRepairDraft, startRecovery, type RepairAttempt, type RepairBinding, type RepairConfig, type RepairObject, type RepairRecord, type RepairUnit, type RecoveryPlan } from '@/api/migrationRepair'
import { attemptRunning, canEditRepairDDL, cloneRepairUnits, formatRepairHistory, groupRepairRecords, hasRepairDDL, isTableRepair, mappingError, matchesRepairDisplayFilter, matchingRepairRecords, needsRepairDisplayAttention, repairAttemptKind, repairAttemptStatus, repairCategories, repairDisplayErrors, repairDisplayStatuses, repairExecutionStatus, repairKind, repairStatus, repairVerificationStatus, unitCommitted, type RepairCategory, type RepairDisplayRow, type RepairFilter, type RepairFocusTarget, type RepairSummary } from '@/utils/migrationRepair'
import { getDbTypeLabel } from '@/utils/dbType'
import { batchIneligibleReason, batchBindingKey, buildRepairBatch, createRepairBatchQueue, type RepairBatchItem } from '@/utils/migrationRepairBatch'

const SqlEditor = defineAsyncComponent(() => import('@/components/SqlEditor.vue'))
const props = defineProps<{ jobID: string }>()
const emit = defineEmits<{ 'summary-change': [summary: RepairSummary] }>()
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
const reviewed = ref(false)
const executeConfirmVisible = ref(false)
const executionSnapshot = ref<RepairUnit[]>([])
const executeVersion = ref(0)
const executionKey = ref('')
const bindingVisible = ref(false)
const connections = ref<Connection[]>([])
const bindingForm = reactive({ src_conn_id: undefined as number | undefined, dst_conn_id: undefined as number | undefined, source_database: '', source_schema: '', config: { Content: 'both', PageSize: 1000, LowerCaseNames: false, CharInLength: false, UseNvarchar2: false, Distributed: false, ChangeOwner: false, TargetSchema: '', Objects: [] } as RepairConfig })
const recoveryVisible = ref(false)
const recoveryObject = ref<RepairRecord | null>(null)
const recoveryForm = reactive({ mapping: {} as Record<string, string>, related: {} as Record<string, boolean>, truncate: false, confirmation: '', sourceQuiesced: false, targetQuiesced: false, reviewed: false })
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

const busy = computed(() => job.value?.status === 'running' || !!activeAttempt.value && attemptRunning(activeAttempt.value.status) || submitting.value || batchRunning.value || batchPreparing.value)
const editorDialect = computed(() => job.value?.dst_db_type === 'mysql' ? 'mysql' as const : 'postgres' as const)
const canExecute = computed(() => !busy.value && !!editing.value && canEditRepairDDL(editing.value.object) && editing.value.object.status !== 'unknown' && !mappingError(mappingForm.values) && !!binding.value?.configured && draftUnits.value.some(unit => unit.selected && unit.sql.trim() && !unitCommitted(unit.status) && !unit.destructive) && (!editing.value.object.requires_review || reviewed.value))
const displayRecords = computed(() => groupRepairRecords(records.value))
const errorDetailRecord = computed(() => displayRecords.value.find(record => record.object.id === errorObjectID.value))
const visibleRecords = computed(() => displayRecords.value.filter(record => matchesRepairDisplayFilter(record, filter.value, attempts.value) && record.members.some(({ object }) => `${object.schema || ''} ${object.name} ${object.source_name || ''} ${object.target_name} ${object.table || ''} ${object.source_table || ''} ${object.original_error || ''} ${object.error || ''}`.toLowerCase().includes(keyword.value.trim().toLowerCase()))).map(record => {
  const batchReason = batchIneligibleReason(record.object)
  return { ...record, key: record.object.id, errors: repairDisplayErrors(record), batchReason, disabled: busy.value || loading.value || !!batchReason }
}))
const batchExecutableCount = computed(() => batchItems.value.filter(item => item.status === 'pending').length)
const batchCompletedCount = computed(() => batchItems.value.filter(item => !['pending', 'saving', 'running'].includes(item.status)).length)
const stats = computed(() => {
  const counts: Record<RepairCategory, number> = { repaired: 0, pending: 0, blocked: 0, data: 0 }
  for (const { key } of repairCategories) counts[key] = displayRecords.value.filter(record => matchesRepairDisplayFilter(record, key, attempts.value)).length
  return counts
})
const emptyDescription = computed(() => error.value ? '修复状态未知，请刷新重试' : loading.value ? '修复状态加载中' : keyword.value.trim() ? '没有匹配的对象' : filter.value === 'attention' ? '当前无待处理对象' : repairCategories.find(category => category.key === filter.value)?.empty || '当前无对象')
const summary = computed<RepairSummary>(() => ({ jobID: props.jobID, state: error.value ? 'unknown' : loading.value ? 'loading' : 'ready', repaired: stats.value.repaired, attention: displayRecords.value.filter(needsRepairDisplayAttention).length }))
const sourceConnections = computed(() => connections.value.filter(conn => !job.value?.src_db_type || conn.db_type === job.value.src_db_type))
const targetConnections = computed(() => connections.value.filter(conn => !job.value?.dst_db_type || conn.db_type === job.value.dst_db_type))
const relatedCandidates = computed(() => {
  const object = recoveryObject.value?.object
  return object ? records.value.filter(record => record.object.id !== object.id && !isTableRepair(record.object) && !unitCommitted(record.object.status) && (record.object.table === (object.table || object.name) || record.object.dependencies?.includes(object.id))) : []
})
const historyObjectIDs = computed(() => {
  if (!historyObjectID.value) return null
  const row = displayRecords.value.find(record => record.members.some(member => member.object.id === historyObjectID.value))
  return new Set(row ? row.members.map(member => member.object.id) : [historyObjectID.value])
})
const visibleAttempts = computed(() => attempts.value.filter(attempt => !historyObjectIDs.value || historyObjectIDs.value.has(attempt.object_id)))
const canRecover = computed(() => {
  const preview = recoveryPlan.value?.preview
  return !!preview && !(preview.related || []).some(object => object.requires_review) && !busy.value && !previewing.value && !mappingError(recoveryForm.mapping) && recoveryForm.sourceQuiesced && recoveryForm.targetQuiesced && recoveryForm.reviewed && (!preview.data_required || preview.target_rows === 0 || recoveryForm.truncate && recoveryForm.confirmation === preview.qualified_target)
})
const current = (scope: number, id: string) => !disposed && scope === epoch && id === props.jobID
const errorText = (e: any) => e?.response?.data?.error || e?.message || '请求失败，请重试'
function showError(e: unknown) { error.value = errorText(e); Message.error(error.value) }
function statusColor(status: string) { return unitCommitted(status) ? 'green' : attemptRunning(status) ? 'blue' : status === 'failed' ? 'red' : status === 'not_required' ? 'gray' : 'orange' }
function qualified(object: RepairObject) { return object.schema ? `${object.schema}.${object.target_name}` : object.target_name }
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
      const saved = (await saveRepairDraft(id, objectID, { version: record.version, units: cloneRepairUnits(record.object.units), reviewed: true })).data
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
  const scope = epoch, id = props.jobID, serial = ++refreshSerial
  loading.value = true
  error.value = ''
  try {
    const [data, detail] = await Promise.all([listRepairs(id), getDataMigrationJob(id)])
    if (!current(scope, id) || serial !== refreshSerial) return
    records.value = data.data.objects || []
    binding.value = data.data.binding
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
    else await refresh()
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
  draftUnits.value = cloneRepairUnits(record.object.units)
  if (canEditRepairDDL(record.object) && !draftUnits.value.length && !hasRepairDDL(record.object) && !unitCommitted(record.object.status) && record.object.status !== 'unknown') addUnit()
  mappingForm.values = { ...(record.object.column_mapping || {}) }
  for (const column of record.object.source_columns || record.object.columns || []) {
    if (!mappingForm.values[column.Name]) mappingForm.values[column.Name] = job.value?.lower_case_names ? column.Name.toLowerCase() : column.Name
  }
  mappingForm.source = ''; mappingForm.target = ''
  reviewed.value = false
  executeConfirmVisible.value = false
  editorVisible.value = true
}
function closeEditor() {
  editorSerial++; executeConfirmVisible.value = false; editing.value = null; draftUnits.value = []
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
  draftUnits.value = draftUnits.value.map(unit => !unitCommitted(unit.status) && !unit.destructive && originals.get(unit.id) ? { ...unit, sql: originals.get(unit.id)! } : unit)
}
function useSuggested() {
  if (!editing.value?.object.suggested_ddl) return
  const editable = draftUnits.value.filter(unit => !unitCommitted(unit.status) && !unit.destructive)
  if (editable.length > 1) { Message.warning('多步骤建议需人工放入对应完整执行单元，避免覆盖已成功步骤'); return }
  if (editable.length === 1) { editable[0].sql = editing.value.object.suggested_ddl; editable[0].selected = true }
  else draftUnits.value.push({ id: newKey(), sql: editing.value.object.suggested_ddl, original_sql: '', status: 'pending', selected: true, destructive: false })
}
async function saveDraft(): Promise<boolean> {
  if (batchRunning.value || batchPreparing.value || !editing.value || !canEditRepairDDL(editing.value.object) || saving.value || mappingError(mappingForm.values)) return false
  const scope = epoch, id = props.jobID, objectID = editing.value.object.id, serial = editorSerial, revision = draftRevision
  saving.value = true
  try {
    const saved = (await saveRepairDraft(id, objectID, { version: editing.value.version, units: draftUnits.value, column_mapping: { ...mappingForm.values }, reviewed: reviewed.value })).data
    if (!current(scope, id) || serial !== editorSerial) return false
    replaceRecord(saved); editing.value = saved
    if (draftRevision !== revision) { Message.info('已保存提交时的草稿，当前新修改仍保留，请再次保存'); return false }
    draftUnits.value = cloneRepairUnits(saved.object.units)
    mappingForm.values = { ...(saved.object.column_mapping || {}) }
    Message.success('草稿已保存'); return true
  } catch (e) { if (current(scope, id) && serial === editorSerial) showError(e); return false }
  finally { if (current(scope, id)) saving.value = false }
}
async function prepareExecute() {
  if (!canExecute.value || !editing.value) return
  if (draftUnits.value.some(unit => unit.selected && !unit.sql.trim())) { Message.error('选中的执行单元不能为空'); return }
  if (!await saveDraft() || !editing.value) return
  executionSnapshot.value = draftUnits.value.filter(unit => unit.selected && !unitCommitted(unit.status) && !unit.destructive).map(unit => ({ ...unit }))
  executeVersion.value = editing.value.version
  executionKey.value = newKey()
  executeConfirmVisible.value = true
}
async function confirmExecute() {
  if (batchRunning.value || batchPreparing.value || !editing.value || submitting.value || !executeConfirmVisible.value) return
  const scope = epoch, id = props.jobID, objectID = editing.value.object.id
  submitting.value = true
  try {
    const attempt = (await executeRepair(id, objectID, { version: executeVersion.value, request_id: executionKey.value, confirmed: true })).data
    if (!current(scope, id)) return
    executeConfirmVisible.value = false; editorVisible.value = false; activeAttempt.value = attempt
    await pollAttempt(attempt.id, scope, id)
  } catch (e) { if (current(scope, id)) showError(e) }
  finally { if (current(scope, id)) submitting.value = false }
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
    bindingForm.config = { Content: binding.value?.legacy && !binding.value.configured ? '' : 'both', PageSize: job.value?.page_size || 1000, LowerCaseNames: !!job.value?.lower_case_names, CharInLength: !!job.value?.char_in_length, UseNvarchar2: !!job.value?.use_nvarchar2, Distributed: false, ChangeOwner: !!job.value?.change_owner, TargetSchema: binding.value?.target_schema || job.value?.dst_schema || '', Objects: (job.value?.migrate_objects || '').split(',').map(value => value.trim()).filter(Boolean) }
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
function invalidatePreview() { previewSerial++; recoveryPlan.value = null; recoveryForm.reviewed = false; recoveryKey.value = ''; previewing.value = false }
function openRecovery(record: RepairRecord) {
  if (batchRunning.value || batchPreparing.value || batchPreviewVisible.value) return
  returnToRecovery.value = false
  invalidatePreview(); recoveryObject.value = record
  recoveryForm.mapping = { ...(record.object.column_mapping || {}) }
  recoveryForm.related = Object.fromEntries(relatedCandidates.value.map(item => [item.object.id, false]))
  recoveryForm.truncate = false; recoveryForm.confirmation = ''; recoveryForm.sourceQuiesced = false; recoveryForm.targetQuiesced = false
  recoveryVisible.value = true
  void loadPreview()
}
async function loadPreview() {
  if (batchRunning.value || batchPreparing.value || !recoveryObject.value || previewing.value) return
  const scope = epoch, id = props.jobID, objectID = recoveryObject.value.object.id, serial = ++previewSerial
  previewing.value = true; recoveryPlan.value = null; recoveryForm.reviewed = false
  try {
    const result = (await previewRecovery(id, objectID, { version: recoveryObject.value.version, ...(Object.keys(recoveryForm.mapping).length ? { column_mapping: recoveryForm.mapping } : {}), related_object_ids: Object.entries(recoveryForm.related).filter(([, enabled]) => enabled).map(([objectID]) => objectID) })).data
    if (!current(scope, id) || serial !== previewSerial || !recoveryVisible.value) return
    await refresh()
    if (!current(scope, id) || serial !== previewSerial || !recoveryVisible.value) return
    recoveryObject.value = records.value.find(record => record.object.id === objectID) || recoveryObject.value
    recoveryForm.mapping = { ...result.preview.column_mapping }
    recoveryPlan.value = result; recoveryKey.value = newKey()
    if (!result.preview.data_required || result.preview.target_rows === 0) { recoveryForm.truncate = false; recoveryForm.confirmation = '' }
  } catch (e) { if (current(scope, id) && serial === previewSerial) showError(e) }
  finally { if (current(scope, id) && serial === previewSerial) previewing.value = false }
}
function reviewRelated(objectID: string) {
  const record = records.value.find(item => item.object.id === objectID)
  if (!record) { Message.error('关联对象已变化，请刷新后重新预览'); return }
  returnToRecovery.value = true; recoveryVisible.value = false; invalidatePreview(); openEditor(record)
}
async function confirmRecovery() {
  if (!canRecover.value || !recoveryObject.value || !recoveryPlan.value || submitting.value) return
  const scope = epoch, id = props.jobID, objectID = recoveryObject.value.object.id
  const truncate = recoveryPlan.value.preview.data_required && recoveryPlan.value.preview.target_rows > 0 && recoveryForm.truncate
  submitting.value = true
  try {
    const attempt = (await startRecovery(id, objectID, { token: recoveryPlan.value.token, request_id: recoveryKey.value, confirmed: true, source_quiesced: recoveryForm.sourceQuiesced, target_quiesced: recoveryForm.targetQuiesced, truncate, confirmation_text: truncate ? recoveryForm.confirmation : undefined })).data
    if (!current(scope, id)) return
    recoveryVisible.value = false; activeAttempt.value = attempt; await pollAttempt(attempt.id, scope, id)
  } catch (e) { if (current(scope, id)) showError(e) }
  finally { if (current(scope, id)) submitting.value = false }
}
async function refreshHistory() {
  const scope = epoch, id = props.jobID, serial = ++historySerial
  loadingHistory.value = true
  try { const result = await listRepairAttempts(id); if (current(scope, id) && serial === historySerial) attempts.value = result.data.items || [] }
  catch (e) { if (current(scope, id) && serial === historySerial) showError(e) }
  finally { if (current(scope, id) && serial === historySerial) loadingHistory.value = false }
}
function openHistory(objectID = '') { historyObjectID.value = objectID; historyVisible.value = true; void refreshHistory() }
async function exportHistory() {
  const scope = epoch, id = props.jobID
  const [objects, history] = await Promise.all([listRepairs(id), listRepairAttempts(id)])
  if (!current(scope, id)) throw new Error('任务已切换，请重新导出')
  return formatRepairHistory(objects.data.objects || [], history.data.items || [])
}
defineExpose({ exportHistory, focusObject })
watch(summary, value => emit('summary-change', value), { immediate: true })
watch(errorDetailRecord, value => { if (!value) closeErrorDetails() })
watch(loading, value => { if (!value) void applyPendingFocus() })
watch([filter, keyword], () => { page.value = 1; clearBatchSelection() }, { flush: 'sync' })
watch(() => visibleRecords.value.length, length => { page.value = Math.min(page.value, Math.max(1, Math.ceil(length / 10))) })
watch([draftUnits, () => mappingForm.values], () => { draftRevision++; executeConfirmVisible.value = false }, { deep: true, flush: 'sync' })
watch(() => props.jobID, () => {
  disposeBatch(); stopAttemptPolling()
  epoch++; refreshSerial++; previewSerial++; editorSerial++; historySerial++
  selectedObjectIDs.value = []; batchItems.value = []; batchPreviewVisible.value = false; batchReviewed.value = false
  batchHasRun.value = false; batchStopped.value = false; batchError.value = ''; batchExpectedBinding = ''
  records.value = []; binding.value = null; job.value = null; activeAttempt.value = null; attempts.value = []
  filter.value = 'attention'; keyword.value = ''; page.value = 1; highlightedObjectID.value = ''; pendingFocus.value = null
  closeErrorDetails()
  editorVisible.value = false; executeConfirmVisible.value = false; recoveryVisible.value = false; bindingVisible.value = false; historyVisible.value = false
  loading.value = false; saving.value = false; submitting.value = false; cancelling.value = false; previewing.value = false; loadingHistory.value = false
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
.repair-statuses { display: flex; flex-direction: column; gap: 4px; }
.repair-status-line { display: flex; align-items: center; gap: 4px; }
.repair-status-label { flex-shrink: 0; font-size: 12px; color: var(--color-text-3); }
.repair-status-line :deep(.arco-tag) { height: auto; min-height: 20px; padding: 0 6px; font-size: 12px; line-height: 20px; white-space: normal; overflow-wrap: anywhere; }
.error-text { color: var(--color-danger-6); overflow-wrap: anywhere; white-space: pre-wrap; }
.repair-error-cell { min-width: 0; }
.repair-error-summary { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; color: var(--color-text-2); line-height: 22px; }
.repair-error-phase { color: var(--color-text-3); }
.repair-error-actions { display: flex; align-items: center; gap: 8px; height: 22px; }
.repair-error-actions :deep(.arco-btn) { height: 22px; padding: 0; font-size: 12px; }
.repair-error-object { margin: 0; font-weight: 600; overflow-wrap: anywhere; }
.repair-error-details { display: grid; gap: 12px; margin-top: 16px; }
.repair-error-detail { padding: 14px; border: 1px solid var(--color-border-2); border-radius: 6px; background: var(--color-bg-2); }
.repair-error-detail h4 { margin: 0 0 8px; font-size: 13px; color: var(--color-text-2); }
.repair-error-detail pre { margin: 0; font: 13px/1.7 var(--font-mono, monospace); color: var(--color-text-1); white-space: pre-wrap; overflow-wrap: anywhere; user-select: text; }
.repair-panel :deep(.arco-table-cell) { overflow-wrap: anywhere; }
.repair-table :deep(.arco-table-td) { height: 68px; box-sizing: border-box; }
.repair-table :deep(.arco-table-td .arco-table-cell) { padding: 9px 12px; }
.repair-table :deep(.arco-table-th .arco-table-cell) { padding: 10px 12px; color: var(--color-text-3); font-size: 12px; }
.repair-object-heading { display: flex; align-items: center; gap: 7px; min-width: 0; line-height: 22px; }
.repair-object-heading > .repair-name { flex: 0 1 auto; font-size: 13px; font-weight: 600; }
.repair-kind { flex-shrink: 0; padding: 0 5px; border-radius: 4px; background: var(--color-fill-2); color: var(--color-text-3); font-size: 11px; line-height: 18px; }
.repair-batch-hint { display: inline-flex; align-items: center; justify-content: center; flex-shrink: 0; width: 22px; height: 22px; margin: 0; padding: 0; border: 0; border-radius: 4px; background: transparent; color: var(--color-text-4); font-size: 14px; cursor: help; }
.repair-batch-hint:hover { color: var(--color-text-2); background: var(--color-fill-2); }
.repair-batch-hint:focus-visible { outline: 2px solid rgb(var(--primary-6)); outline-offset: 1px; }
.repair-object-meta { display: flex; align-items: center; gap: 6px; min-width: 0; margin-top: 2px; font-size: 12px; line-height: 20px; color: var(--color-text-3); }
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
.modal-actions { justify-content: flex-end; margin-top: 18px; }
.repair-editor-footer { display: flex; flex-direction: column; gap: 12px; text-align: left; }
.repair-editor-footer .modal-actions { margin-top: 0; }
.editor-mapping { margin-top: 16px; }
.mapping-list, .related-list { width: 100%; display: grid; gap: 8px; }
.mapping-row { display: grid; grid-template-columns: minmax(100px, 1fr) 20px minmax(120px, 1fr); gap: 8px; align-items: center; }
.editable-mapping { grid-template-columns: minmax(100px, 1fr) 20px minmax(120px, 1fr) 50px; }
.mapping-row code { overflow-wrap: anywhere; }
.preview-facts { padding: 12px; background: var(--color-fill-1); }
.preview-facts > div { display: flex; flex-wrap: wrap; gap: 16px; padding: 5px 0; }
.preview-facts dt { min-width: 80px; color: var(--color-text-3); }
.preview-facts dd { margin: 0; overflow-wrap: anywhere; }
.history-attempt { margin: 16px 0; padding-bottom: 12px; border-bottom: 1px solid var(--color-border-2); }
@container (max-width: 767px) {
  .repair-filters { flex-basis: 100%; }
  .repair-search { max-width: none; }
  .batch-toolbar { width: 100%; }
}
</style>
