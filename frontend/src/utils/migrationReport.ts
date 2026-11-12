import type { DataValidationValue, DataValidationReport, CategoryReport, CompatibilityFunctionsReport, MigrationReport, ObjectResult, TriggerAdaptationReport } from '@/api/migration'

// ready means every object passed. can_start also allows a healthy subset, and
// an explicit top-level refusal always overrides a nested partition result.
export function preflightCanStart(result?: { can_start?: boolean; partition_preflight?: { can_start?: boolean }; ready?: boolean } | null): boolean {
  return result?.can_start ?? result?.partition_preflight?.can_start ?? result?.ready ?? false
}

export function preflightStatusLabel(result?: { can_start?: boolean; partition_preflight?: { can_start?: boolean }; ready?: boolean } | null): string {
  return !result ? '待预检' : !preflightCanStart(result) ? '预检未通过' : result.ready ? '预检通过' : '部分可执行'
}

// Isolate the original MySQL KEY clause for this failed index. Split only at
// table-body commas, preserving nested expressions and quoted text verbatim.
export function sourceIndexDefinition(ddl: string, name: string): string {
  if (!/^\s*CREATE\s+TABLE\b/i.test(ddl) || !name) return ''
  const clauses: string[] = []
  let quote = '', depth = 0, start = -1
  for (let i = 0; i < ddl.length; i++) {
    const char = ddl[i]
    if (quote) {
      if (char === '\\') { i++; continue }
      if (char === quote) {
        if (ddl[i + 1] === quote) i++
        else quote = ''
      }
      continue
    }
    if (char === '`' || char === "'" || char === '"') { quote = char; continue }
    if (char === '(') {
      if (depth++ === 0) start = i + 1
    } else if (char === ')') {
      if (--depth === 0) { clauses.push(ddl.slice(start, i).trim()); break }
    } else if (char === ',' && depth === 1) {
      clauses.push(ddl.slice(start, i).trim())
      start = i + 1
    }
  }
  const indexes = clauses.flatMap(clause => {
    const match = clause.match(/^(?:(?:UNIQUE|FULLTEXT|SPATIAL)\s+)?(?:KEY|INDEX)\s+(?:`((?:``|[^`])+)`|([^\s(]+))\s*(?:USING\s+\w+\s*)?\(/i)
    return match ? [{ name: match[1]?.replace(/``/g, '`') ?? match[2], clause }] : []
  })
  const exact = indexes.find(index => index.name === name)
  if (exact) return exact.clause
  // Legacy reports retained only the normalized target name. Avoid guessing
  // when more than one source index could match it.
  const matches = indexes.filter(index => index.name.toLowerCase() === name.toLowerCase())
  return matches.length === 1 ? matches[0].clause : ''
}

export function failureStage(item: ObjectResult): ObjectResult['stage'] {
  if (item.stage) return item.stage
  // Older reports used a synthetic object for a category-wide read failure.
  if (item.name === '元数据读取' && !item.ddl?.trim()) return 'metadata'
  return undefined
}

export function describeFailure(item: ObjectResult, category?: string) {
  const stage = failureStage(item)
  const stageLabel = stage === 'metadata' ? '元数据读取失败'
    : stage === 'preflight' ? '迁移预检失败'
    : stage === 'dependency' ? '依赖对象失败，已跳过'
    : stage === 'task' ? '任务级错误，迁移已终止'
    : stage === 'unsupported' ? '不支持自动转换'
    : stage === 'execute' ? 'DDL 执行失败'
    : category === 'data' ? '数据迁移失败' : '失败阶段未记录'
  const emptyDDLMessage = stage === 'metadata' ? '元数据读取失败，尚未生成目标 DDL。'
    : stage === 'preflight' ? '该对象预检未通过，尚未执行该对象的目标写入。'
    : stage === 'dependency' ? '该对象依赖的表或对象失败，未执行该对象的目标写入。'
    : stage === 'task' ? '任务已终止，后续对象未执行；此前已成功提交的对象和分页予以保留。'
    : stage === 'unsupported' ? '该对象暂不支持自动转换，未生成目标 DDL。'
    : stage === 'execute' ? 'DDL 执行失败，但报告未记录目标 DDL。' : '未记录 DDL。'
  const sourceKind = category === 'triggers' ? '触发器' : category === 'views' ? '视图' : '表'
  return { stageLabel, emptyDDLMessage, sourceDDLLabel: `源端${sourceKind}定义（仅供参考，需适配目标库）`, sourceDDLCopyLabel: `复制源端${sourceKind}定义`, showTargetDDL: category !== 'data' || !!item.ddl?.trim() }
}

export function categoryPresentation(category: CategoryReport) {
  const metadataFailed = category.items?.some(item => failureStage(item) === 'metadata') ?? false
  // Older reports represented a category-wide read failure with a synthetic
  // item. A named object's metadata failure still has known object counts.
  const legacyMetadataFailure = category.total_unknown === undefined && (category.items?.some(item =>
    item.name === '元数据读取' && failureStage(item) === 'metadata' && !item.ddl?.trim()
      && !item.source_name && !item.target_name && !item.table && !item.target_table,
  ) ?? false)
  const totalUnknown = category.total_unknown === true || legacyMetadataFailure
  return {
    totalLabel: totalUnknown ? '未知' : String(category.total),
    failedLabel: totalUnknown ? '未知' : String(category.failed),
    statusLabel: totalUnknown ? (metadataFailed ? '元数据读取失败' : '对象总数未知')
      : category.failed > 0 ? '部分失败' : category.total === 0 ? '无对象' : '全部成功',
    statusColor: metadataFailed || category.failed > 0 ? 'orange'
      : totalUnknown || category.total === 0 ? 'gray' : 'green',
  }
}

export function compatibilityReportGroups(report?: CompatibilityFunctionsReport) {
  if (!report) return []
  const items = report.items ?? []
  const procedureItems = items.filter(item => item.kind === 'procedure')
  const created = procedureItems.filter(item => item.status === 'created').length
  const reused = procedureItems.filter(item => item.status === 'reused').length
  const procedures: CompatibilityFunctionsReport = {
    total: procedureItems.length, success: created + reused,
    failed: procedureItems.filter(item => item.status === 'failed').length,
    created, reused, items: procedureItems,
  }
  // Historical reports may have aggregate counts without complete item details.
  // Subtract only explicitly recorded procedures to retain those function counts.
  const functions: CompatibilityFunctionsReport = {
    total: report.total - procedures.total,
    success: report.success - procedures.success,
    failed: report.failed - procedures.failed,
    created: report.created - procedures.created,
    reused: report.reused - procedures.reused,
    items: items.filter(item => item.kind !== 'procedure'),
    extensions: report.extensions,
  }
  const groups = [{ key: 'compatibility_functions', label: '兼容函数', kind: 'function' as const, report: functions }]
  return procedureItems.length
    ? [...groups, { key: 'compatibility_procedures', label: '兼容存储过程', kind: 'procedure' as const, report: procedures }]
    : groups
}

function triggerAdaptationPresentation(report: TriggerAdaptationReport) {
  if (!report.recorded) return { totalLabel: '未记录', failedLabel: '—', statusLabel: '关联触发器结果未记录', statusColor: 'gray' }
  const complete = report.success === report.total && !report.failed && !report.blocked && !report.pending && !report.unknown
  const status: string[] = []
  if (report.success > 0) status.push('部分适配')
  if (report.failed > 0) status.push(`失败${report.failed}个`)
  if (report.blocked > 0) status.push(`阻塞${report.blocked}个`)
  if (report.pending > 0) status.push(`待执行${report.pending}个`)
  if (report.unknown > 0) status.push(`结果未知${report.unknown}个`)
  return {
    totalLabel: String(report.total), failedLabel: String(report.failed),
    statusLabel: report.total === 0 ? '无关联触发器' : complete ? (report.mode === 'mysql_auto_increment' ? '已转换为 AUTO_INCREMENT，起点已校准' : '全部适配') : status.join('，') || '尚未适配',
    statusColor: report.total === 0 ? 'gray' : complete ? 'green' : 'orange',
  }
}

function triggerMigrationPresentation(category: CategoryReport) {
  const presentation = categoryPresentation(category)
  if (category.total_unknown || presentation.totalLabel === '未知') return presentation
  const pending = Math.max(0, category.total - category.success - category.failed)
  if (!pending) return presentation
  return {
    ...presentation,
    statusLabel: `${category.failed ? '部分失败' : category.success ? '部分完成' : '尚未执行'}，待执行${pending}个`,
    statusColor: category.failed || category.success ? 'orange' : 'gray',
  }
}

export function reportCategories(report: MigrationReport) {
  const adaptation = report.trigger_adaptation
  const categories = [
    ['tables', '表'], ['data', '数据写入'], ['primaryKeys', '主键'], ['views', '视图'],
    ['indexes', '索引'], ['constraints', '外键'], ['sequences', '序列'],
    ['comments', '注释'], ['triggers', '触发器'],
  ] as const
  const sources = [
    ...categories.map(([key, label]) => ({
      key, label: key === 'triggers' && adaptation ? '源端触发器' : key === 'constraints' && report.native_schema ? '约束' : label,
      category: report[key] ?? { total: 0, success: 0, failed: 0, items: [] },
      compatibilityReport: undefined as CompatibilityFunctionsReport | undefined,
      compatibilityKind: undefined as 'function' | 'procedure' | undefined,
    })),
    ...(report.native_schema?.categories ?? []).map(category => ({
      key: `native:${category.kind}`, label: category.label, category,
      compatibilityReport: undefined as CompatibilityFunctionsReport | undefined,
      compatibilityKind: undefined as 'function' | 'procedure' | undefined,
    })),
    ...(adaptation ? [{
      key: 'trigger_adaptation', label: adaptation.mode === 'mysql_auto_increment' ? '自增列形式的触发器' : '序列关联触发器',
      category: { total: adaptation.total, success: adaptation.success, failed: adaptation.failed, items: [] as ObjectResult[] },
      compatibilityReport: undefined as CompatibilityFunctionsReport | undefined,
      compatibilityKind: undefined as 'function' | 'procedure' | undefined,
    }] : []),
    ...compatibilityReportGroups(report.compatibility_functions).map(group => ({
      key: group.key, label: group.label, category: group.report,
      compatibilityReport: group.report, compatibilityKind: group.kind,
    })),
  ]
  return sources.map(({ key, label, category, compatibilityReport, compatibilityKind }) => {
    const isTrigger = key === 'triggers' && !report.trigger_migration_enabled
    const isTriggerAdaptation = key === 'trigger_adaptation'
    const sourceInventoryOnly = isTrigger && !!adaptation
    const presentation = isTriggerAdaptation && adaptation ? triggerAdaptationPresentation(adaptation)
      : key === 'triggers' && report.trigger_migration_enabled ? triggerMigrationPresentation(category) : categoryPresentation(category)
    const isCompatibility = !!compatibilityReport
    return {
      autoIncrementItems: isTriggerAdaptation && adaptation?.mode === 'mysql_auto_increment' ? adaptation.items ?? [] : [],
      ...category, ...presentation, key, label, isTrigger, isTriggerAdaptation, isCompatibility,
      isInformational: isTrigger || isTriggerAdaptation, sourceInventoryOnly,
      totalLabel: sourceInventoryOnly && category.total < 0 ? '获取失败' : presentation.totalLabel,
      successLabel: isTrigger || isTriggerAdaptation && !adaptation?.recorded ? '—' : String(category.success),
      failedLabel: sourceInventoryOnly ? '—' : presentation.failedLabel,
      statusLabel: sourceInventoryOnly ? (adaptation?.mode === 'mysql_auto_increment' ? '源端数量；自增转换见下方，普通触发器未迁移' : '仅统计源端数量') : presentation.statusLabel,
      compatibilityReport, compatibilityKind,
      created: compatibilityReport?.created ?? 0,
      reused: compatibilityReport?.reused ?? 0,
      success: isTrigger ? 0 : category.success,
      failed: isTrigger ? 0 : category.failed,
      items: isTrigger ? [] : (category.items ?? []).map((item, index) => ({
        ...item, ...describeFailure(item, key),
        repairKind: key.startsWith('native:') ? key.slice(7) : key,
        nameLabel: key === 'indexes' && failureStage(item) !== 'metadata' ? '索引名称：' : '',
        targetNameLabel: failureStage(item) === 'unsupported' ? '目标索引名称（计划）：' : '目标索引名称：',
        targetTableLabel: item.target_table ? [item.target_schema, item.target_table].filter(Boolean).join('.') : '',
        sourceIndexDDL: key === 'indexes' ? sourceIndexDefinition(item.source_ddl ?? '', item.source_name || item.name) : '',
        key: JSON.stringify([key, item.table ?? '', item.name, item.stage ?? '', index]),
      })),
    }
  })
}

export function migrationOverviewRows(rows: ReturnType<typeof reportCategories>) {
  if (!rows.some(row => row.compatibilityKind === 'procedure')) return rows

  // Keep both compatibility rows together on the first page of the overview.
  const afterComments = rows.findIndex(row => row.key === 'comments') + 1
  const remainingRows = rows.slice(afterComments)
  return [
    ...rows.slice(0, afterComments),
    ...remainingRows.filter(row => row.isCompatibility),
    ...remainingRows.filter(row => !row.isCompatibility),
  ]
}

type OriginalFailureEntry = { key: string; category: string } & (
  { name: string; item: ObjectResult; stageLabel: string } | { missing: number }
)

export function originalFailureDetails(categories: ReturnType<typeof reportCategories>) {
  const entries: OriginalFailureEntry[] = []
  for (const row of categories) {
    if (row.isInformational || row.failed <= 0) continue
    const items = row.compatibilityReport
      ? row.compatibilityReport.items.filter(item => item.status === 'failed').map(item => ({
        item, name: [item.schema, item.signature || item.name].filter(Boolean).join('.'), stageLabel: '兼容对象初始化',
      }))
      : row.items.map(item => ({ item, name: item.name || item.target_name || item.source_name, stageLabel: item.stageLabel }))
    for (const [index, detail] of items.entries()) {
      entries.push({ ...detail, name: detail.name || '对象名未记录', key: `${row.key}:${index}`, category: row.label })
    }
    if (row.failed > items.length) {
      entries.push({ key: `${row.key}:missing`, category: row.label, missing: row.failed - items.length })
    }
  }
  return entries
}

function appendReportDDL(lines: string[], label: string, ddl: string) {
  lines.push(label)
  for (const line of ddl.split('\n')) lines.push(`      ${line}`)
}

export function formatReportCategories(report: MigrationReport): string[] {
  const categories = reportCategories(report)
  const lines = ['=== 迁移概览 ===']
  for (const row of categories) {
    if (row.isTrigger) {
      lines.push(`${row.label.padEnd(6)} 总计 ${row.total === -1 ? '获取失败' : row.totalLabel}  （${row.sourceInventoryOnly ? row.statusLabel : '未迁移'}）`)
    } else {
      lines.push(`${row.label.padEnd(6)} 总计 ${row.totalLabel}  成功 ${row.successLabel}  失败 ${row.failedLabel}  状态：${row.statusLabel}`)
    }
  }
  if (report.trigger_adaptation?.mode === 'mysql_auto_increment' && report.trigger_adaptation.items?.length) {
    lines.push('', '=== 自增列形式的触发器明细 ===')
    for (const item of report.trigger_adaptation.items) {
      lines.push(`  ${item.trigger}：${item.sequence_owner}.${item.sequence} → ${item.target_table}.${item.target_column} AUTO_INCREMENT`)
      lines.push(`    源列：${item.table}.${item.column}；转换：${autoIncrementStatus(item.conversion_status)}；校准：${autoIncrementStatus(item.calibration_status)}；起点：${item.start ?? '未记录'}`)
      if (item.shared_sequence) lines.push('    共享序列：转换后各表独立自增，新 ID 不再保证跨表唯一')
      if (item.error) lines.push(`    原因：${item.error}`)
    }
  }
  for (const group of compatibilityReportGroups(report.compatibility_functions)) {
    const compatibility = group.report
    const routineLabel = group.kind === 'procedure' ? '存储过程' : '函数'
    lines.push('', `=== ${group.label}明细 ===`, `新建 ${compatibility.created}，复用 ${compatibility.reused}`)
    for (const item of compatibility.items) {
      const status = item.status === 'reused' && item.definition_warning === '已存在，跳过，未校验定义'
        ? '已存在，跳过，未校验定义'
        : ({ created: '新建', reused: '复用', failed: '失败' })[item.status] || item.status
      lines.push(`  ${item.schema}.${item.signature || item.name}：${status}`)
      if (item.generator) lines.push(`    底层函数：${item.generator}`)
      if (item.error) lines.push(`    失败原因：${item.error}`)
      if (item.ddl) appendReportDDL(lines, `    ${routineLabel} DDL：`, item.ddl)
      else if (!item.definition_warning) lines.push(`    尚未生成${routineLabel} DDL`)
      if (item.definition_warning) lines.push(`    ${item.definition_warning}`)
    }
    for (const extension of compatibility.extensions ?? []) {
      lines.push(`  扩展 ${extension.schema}.${extension.name}：${({ installed: '已安装', reused: '已复用', failed: '失败', rolled_back: '已回滚', unknown: '结果待核验' })[extension.status]}`)
      if (extension.sql) lines.push(`    SQL：${extension.sql}`)
      if (extension.error) lines.push(`    原因：${extension.error}`)
    }
  }
  lines.push('')
  const failedCategories = categories.filter(row => !row.isInformational && !row.isCompatibility && row.failed > 0 && row.items.length > 0)
  if (!failedCategories.length) return lines
  lines.push('=== 失败项详情 ===')
  for (const row of failedCategories) {
    lines.push('', `【${row.label}】`)
    for (const item of row.items) {
      lines.push(`  ● ${item.nameLabel}${item.name}`)
      if (row.key === 'indexes') {
        if (item.source_name) lines.push(`    源索引名称：${item.source_name}`)
        if (item.target_name) lines.push(`    ${item.targetNameLabel}${item.target_name}`)
        if (item.targetTableLabel) lines.push(`    目标表：${item.targetTableLabel}`)
      }
      if (item.table) lines.push(`    所属表：${item.table}`)
      lines.push(`    失败阶段：${item.stageLabel}`, `    失败原因：${item.error}`)
      if (item.ddl?.trim()) {
        appendReportDDL(lines, '    目标 DDL：', item.ddl)
      } else if (item.showTargetDDL) {
        lines.push(`    目标 DDL：${item.emptyDDLMessage}`)
      }
      if (item.source_ddl?.trim()) {
        if (item.sourceIndexDDL) appendReportDDL(lines, '    源端失败索引定义（仅供参考，需适配目标库）：', item.sourceIndexDDL)
        appendReportDDL(lines, `    ${item.sourceDDLLabel}：`, item.source_ddl)
      }
      if (item.suggestion) lines.push(`    建议：${item.suggestion}`)
      for (const rename of item.column_renames ?? []) {
        lines.push(`    建议列名：${rename.source} → ${rename.target}`)
      }
      if (item.suggested_ddl) {
        appendReportDDL(lines, '    建议 DDL（手动创建）：', item.suggested_ddl)
      }
    }
  }
  return [...lines, '']
}

export function autoIncrementStatus(status: string): string {
  return ({ converted: '已转换为 AUTO_INCREMENT', reused: '已验证现有自增列', succeeded: '已校准', failed: '失败', blocked: '已阻止', pending: '待执行', disabled: '源触发器已禁用，未转换', not_applicable: '不适用' } as Record<string, string>)[status] ?? '未记录'
}

// A missing validation block means no content check was recorded, never a pass.
export function dataValidationStatus(status?: string): { label: string; color: string } {
  const states: Record<string, { label: string; color: string }> = {
    passed: { label: '全量校验通过', color: 'green' },
    mismatch: { label: '内容不一致', color: 'orange' },
    error: { label: '校验失败', color: 'red' },
    unsupported: { label: '无法校验', color: 'orange' },
    cancelled: { label: '校验已取消', color: 'gray' },
    incomplete: { label: '校验未完成', color: 'orange' },
  }
  return states[status || ''] || { label: '未完成', color: 'orange' }
}

export function dataValidationNeedsAttention(report?: DataValidationReport, enabled = false): boolean {
  if (!report) return enabled
  return report.status !== 'passed' || report.tables.some(table => table.status !== 'passed')
}

export function dataValidationLabel(report?: DataValidationReport, enabled = false): string {
  if (!report) return enabled ? '未完成' : '未启用'
  if (report.status === 'passed' && report.tables.some(table => table.status !== 'passed')) return '校验未全部通过'
  return dataValidationStatus(report.status).label
}

export function formatDataValidationValue(value?: DataValidationValue): string {
  if (!value) return '不存在'
  if (value.null) return 'NULL'
  if (value.sha256) return `二进制（${value.length ?? 0} 字节，SHA256 ${value.sha256}）`
  return `${JSON.stringify(value.text)}${value.truncated ? `（已截断，原长度 ${value.length ?? '未知'}）` : ''}`
}

export function formatDataValidation(report?: DataValidationReport, enabled = false): string[] {
  const lines = ['', '=== 数据内容校验 ===', dataValidationLabel(report, enabled)]
  if (!report) {
    lines.push(enabled ? '任务启用了内容校验，但未记录完整结果。' : '本任务未记录数据内容校验结果，行数相同不代表内容一致。')
    return lines
  }
  for (const table of report.tables) {
    lines.push(`${table.source_table} → ${table.target_table}：${dataValidationStatus(table.status).label}；源 ${table.source_rows} / 目标 ${table.target_rows}，缺失 ${table.missing_rows}，多余 ${table.extra_rows}，字段变化 ${table.changed_rows}，重复主键 ${table.duplicate_keys}`)
    if (table.error) lines.push(`  原因：${table.error}`)
    for (const sample of table.samples || []) {
      const type = ({ missing: '目标缺失', extra: '目标多余', changed: '字段变化', duplicate_key: '重复主键' } as Record<string, string>)[sample.type] || sample.type
      if (sample.key) lines.push(`  主键：${Object.entries(sample.key).map(([column, value]) => `${column}=${formatDataValidationValue(value)}`).join('，')}`)
      for (const field of sample.fields || []) lines.push(`  ${field.column}：源 ${formatDataValidationValue(field.source)} / 目标 ${formatDataValidationValue(field.target)}`)
      lines.push(`  ${type}：主键指纹 ${sample.key_hash || '—'}；行指纹 ${sample.row_hash || '—'}${sample.columns?.length ? `；字段 ${sample.columns.join('、')}` : ''}`)
    }
  }
  return lines
}

export function nativeSchemaNeedsAttention(report?: MigrationReport | null): boolean {
  return report?.native_schema?.preflight?.ready === false || !!report?.native_schema?.verification?.some(issue => issue.severity !== 'warning')
}

export interface MigrationTiming {
  created_at?: string | null
  finished_at?: string | null
  status?: string
}

function migrationDate(value?: string | null): Date | null {
  const text = value?.trim()
  if (!text || /^000[01]-01-01(?:T|\s|$)/.test(text)) return null
  const date = new Date(text)
  return Number.isFinite(date.getTime()) ? date : null
}

function formatMigrationDate(date: Date | null): string {
  if (!date) return '未记录'
  const pad = (value: number) => String(value).padStart(2, '0')
  return `${String(date.getFullYear()).padStart(4, '0')}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`
}

export function migrationTimingSummary(job?: MigrationTiming | null): { startedAt: string; finishedAt: string; duration: string } {
  const start = migrationDate(job?.created_at)
  const end = migrationDate(job?.finished_at)
  const running = job?.status === 'running' && !end
  let duration = running ? '进行中' : '—'
  if (start && end && end.getTime() >= start.getTime()) {
    const seconds = Math.floor((end.getTime() - start.getTime()) / 1000)
    const days = Math.floor(seconds / 86400)
    const hours = Math.floor(seconds % 86400 / 3600)
    const minutes = Math.floor(seconds % 3600 / 60)
    const parts: string[] = []
    if (days) parts.push(`${days} 天`)
    if (days || hours) parts.push(`${hours} 小时`)
    if (days || hours || minutes) parts.push(`${minutes} 分`)
    parts.push(`${seconds % 60} 秒`)
    duration = parts.join(' ')
  }
  return {
    startedAt: formatMigrationDate(start),
    finishedAt: running ? '尚未结束' : formatMigrationDate(end),
    duration,
  }
}

export function formatMigrationTiming(job?: MigrationTiming | null, unavailable = false): string[] {
  if (unavailable) return ['迁移开始时间：时间信息暂不可用', '迁移结束时间：时间信息暂不可用', '总耗时：—']
  const timing = migrationTimingSummary(job)
  return [`迁移开始时间：${timing.startedAt}`, `迁移结束时间：${timing.finishedAt}`, `总耗时：${timing.duration}`]
}
