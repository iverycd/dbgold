import type { CategoryReport, MigrationReport, ObjectResult } from '@/api/migration'

const sourceDDLLabel = '源端表定义（仅供参考，需适配目标库）'

export function failureStage(item: ObjectResult): ObjectResult['stage'] {
  if (item.stage) return item.stage
  // Older reports used a synthetic object for a category-wide read failure.
  if (item.name === '元数据读取' && !item.ddl?.trim()) return 'metadata'
  return undefined
}

export function describeFailure(item: ObjectResult) {
  const stage = failureStage(item)
  const stageLabel = stage === 'metadata' ? '元数据读取失败'
    : stage === 'unsupported' ? '不支持自动转换'
    : stage === 'execute' ? 'DDL 执行失败' : '失败阶段未记录'
  const emptyDDLMessage = stage === 'metadata' ? '元数据读取失败，尚未生成目标 DDL。'
    : stage === 'unsupported' ? '该对象暂不支持自动转换，未生成目标 DDL。'
    : stage === 'execute' ? 'DDL 执行失败，但报告未记录目标 DDL。' : '未记录 DDL。'
  return { stageLabel, emptyDDLMessage, sourceDDLLabel }
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

export function reportCategories(report: MigrationReport) {
  const categories = [
    ['tables', '表'], ['data', '数据写入'], ['primaryKeys', '主键'], ['views', '视图'],
    ['indexes', '索引'], ['constraints', '外键'], ['sequences', '序列'],
    ['comments', '注释'], ['triggers', '触发器'],
  ] as const
  return categories.map(([key, label]) => {
    const category = report[key]
    const isTrigger = key === 'triggers'
    return {
      ...category, ...categoryPresentation(category), key, label, isTrigger,
      success: isTrigger ? 0 : category.success,
      failed: isTrigger ? 0 : category.failed,
      items: isTrigger ? [] : (category.items ?? []).map((item, index) => ({
        ...item, ...describeFailure(item),
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
      lines.push(`${row.label.padEnd(6)}总计 ${row.total === -1 ? '获取失败' : row.totalLabel}  （未迁移）`)
    } else {
      lines.push(`${row.label.padEnd(6)}总计 ${row.totalLabel}  成功 ${row.success}  失败 ${row.failedLabel}  状态：${row.statusLabel}`)
    }
  }
  lines.push('')
  const failedCategories = categories.filter(row => row.failed > 0 && row.items.length > 0)
  if (!failedCategories.length) return lines
  lines.push('=== 失败项详情 ===')
  for (const row of failedCategories) {
    lines.push('', `【${row.label}】`)
    for (const item of row.items) {
      lines.push(`  ● ${item.name}`)
      if (item.table) lines.push(`    所属表：${item.table}`)
      lines.push(`    失败阶段：${item.stageLabel}`, `    失败原因：${item.error}`)
      if (item.ddl?.trim()) {
        lines.push('    目标 DDL：', ...item.ddl.split('\n').map(line => `      ${line}`))
      } else {
        lines.push(`    目标 DDL：${item.emptyDDLMessage}`)
      }
      if (item.source_ddl?.trim()) {
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
