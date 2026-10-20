import type { CategoryReport, CompatibilityFunctionsReport, MigrationReport, ObjectResult } from '@/api/migration'

const sourceDDLLabel = '源端表定义（仅供参考，需适配目标库）'

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
  return { stageLabel, emptyDDLMessage, sourceDDLLabel, showTargetDDL: category !== 'data' || !!item.ddl?.trim() }
}

export function categoryPresentation(category: CategoryReport) {
  const metadataFailed = category.items?.some(item => failureStage(item) === 'metadata') ?? false
  const totalUnknown = category.total_unknown === true || metadataFailed
  return {
    totalLabel: totalUnknown ? '未知' : String(category.total),
    failedLabel: totalUnknown ? '未知' : String(category.failed),
    statusLabel: metadataFailed ? '元数据读取失败' : category.failed > 0 ? '部分失败'
      : totalUnknown ? '对象总数未知' : category.total === 0 ? '无对象' : '全部成功',
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

export function reportCategories(report: MigrationReport) {
  const categories = [
    ['tables', '表'], ['data', '数据写入'], ['primaryKeys', '主键'], ['views', '视图'],
    ['indexes', '索引'], ['constraints', '外键'], ['sequences', '序列'],
    ['comments', '注释'], ['triggers', '触发器'],
  ] as const
  const sources = [
    ...categories.map(([key, label]) => ({
      key, label, category: report[key] ?? { total: 0, success: 0, failed: 0, items: [] },
      compatibilityReport: undefined as CompatibilityFunctionsReport | undefined,
      compatibilityKind: undefined as 'function' | 'procedure' | undefined,
    })),
    ...compatibilityReportGroups(report.compatibility_functions).map(group => ({
      key: group.key, label: group.label, category: group.report,
      compatibilityReport: group.report, compatibilityKind: group.kind,
    })),
  ]
  return sources.map(({ key, label, category, compatibilityReport, compatibilityKind }) => {
    const isTrigger = key === 'triggers'
    const isCompatibility = !!compatibilityReport
    return {
      ...category, ...categoryPresentation(category), key, label, isTrigger, isCompatibility,
      compatibilityReport, compatibilityKind,
      created: compatibilityReport?.created ?? 0,
      reused: compatibilityReport?.reused ?? 0,
      success: isTrigger ? 0 : category.success,
      failed: isTrigger ? 0 : category.failed,
      items: isTrigger ? [] : (category.items ?? []).map((item, index) => ({
        ...item, ...describeFailure(item, key),
        nameLabel: key === 'indexes' && failureStage(item) !== 'metadata' ? '索引名称：' : '',
        targetNameLabel: failureStage(item) === 'unsupported' ? '目标索引名称（计划）：' : '目标索引名称：',
        targetTableLabel: item.target_table ? [item.target_schema, item.target_table].filter(Boolean).join('.') : '',
        sourceIndexDDL: key === 'indexes' ? sourceIndexDefinition(item.source_ddl ?? '', item.source_name || item.name) : '',
        key: JSON.stringify([key, item.table ?? '', item.name, item.stage ?? '', index]),
      })),
    }
  })
}

export function formatReportCategories(report: MigrationReport): string[] {
  const categories = reportCategories(report)
  const lines = ['=== 迁移概览 ===']
  for (const row of categories) {
    if (row.isTrigger) {
      lines.push(`${row.label.padEnd(6)} 总计 ${row.total === -1 ? '获取失败' : row.totalLabel}  （未迁移）`)
    } else {
      lines.push(`${row.label.padEnd(6)} 总计 ${row.totalLabel}  成功 ${row.success}  失败 ${row.failedLabel}  状态：${row.statusLabel}`)
    }
  }
  for (const group of compatibilityReportGroups(report.compatibility_functions)) {
    const compatibility = group.report
    const routineLabel = group.kind === 'procedure' ? '存储过程' : '函数'
    lines.push('', `=== ${group.label}明细 ===`, `新建 ${compatibility.created}，复用 ${compatibility.reused}`)
    for (const item of compatibility.items) {
      const status = ({ created: '新建', reused: '复用', failed: '失败' })[item.status] || item.status
      lines.push(`  ${item.schema}.${item.signature || item.name}：${status}`)
      if (item.generator) lines.push(`    底层函数：${item.generator}`)
      if (item.error) lines.push(`    失败原因：${item.error}`)
      if (item.ddl) lines.push(`    ${routineLabel} DDL：`, ...item.ddl.split('\n').map(line => `      ${line}`))
      else lines.push(`    ${item.definition_warning || `尚未生成${routineLabel} DDL`}`)
    }
    for (const extension of compatibility.extensions ?? []) {
      lines.push(`  扩展 ${extension.schema}.${extension.name}：${({ installed: '已安装', reused: '已复用', failed: '失败', rolled_back: '已回滚', unknown: '结果待核验' })[extension.status]}`)
      if (extension.sql) lines.push(`    SQL：${extension.sql}`)
      if (extension.error) lines.push(`    原因：${extension.error}`)
    }
  }
  lines.push('')
  const failedCategories = categories.filter(row => !row.isCompatibility && row.failed > 0 && row.items.length > 0)
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
        lines.push('    目标 DDL：', ...item.ddl.split('\n').map(line => `      ${line}`))
      } else if (item.showTargetDDL) {
        lines.push(`    目标 DDL：${item.emptyDDLMessage}`)
      }
      if (item.source_ddl?.trim()) {
        if (item.sourceIndexDDL) lines.push('    源端失败索引定义（仅供参考，需适配目标库）：', ...item.sourceIndexDDL.split('\n').map(line => `      ${line}`))
        lines.push(`    ${item.sourceDDLLabel}：`, ...item.source_ddl.split('\n').map(line => `      ${line}`))
      }
      if (item.suggestion) lines.push(`    建议：${item.suggestion}`)
      for (const rename of item.column_renames ?? []) {
        lines.push(`    建议列名：${rename.source} → ${rename.target}`)
      }
      if (item.suggested_ddl) {
        lines.push('    建议 DDL（手动创建）：', ...item.suggested_ddl.split('\n').map(line => `      ${line}`))
      }
    }
  }
  return [...lines, '']
}
