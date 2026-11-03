import type { AutoRepair } from '@/api/migration'

export function groupAutoRepairs(repairs: AutoRepair[] = []) {
  const groups = new Map<string, { key: string; table: string; target_table: string; items: AutoRepair[] }>()
  for (const repair of repairs) {
    const key = JSON.stringify([repair.table, repair.target_table])
    if (!groups.has(key)) groups.set(key, { key, table: repair.table, target_table: repair.target_table, items: [] })
    groups.get(key)!.items.push(repair)
  }
  return [...groups.values()]
}

export function autoRepairStatus(status: AutoRepair['status']) {
  return status === 'reused' ? '复用已有映射' : '本次已修复'
}

export function autoRepairType(type: AutoRepair['type']) {
  if (type === 'column_rename') return '列名修复'
  if (type === 'oracle_mysql_longtext') return '类型调整'
  if (type === 'oracle_mysql_comment_truncate') return '注释截断'
  return '字段调整'
}

export function autoRepairSourceType(item: AutoRepair) {
  return item.type === 'oracle_mysql_comment_truncate' ? '不适用' : item.source_type || '未记录'
}

export function formatAutoRepairs(repairs: AutoRepair[] = []): string[] {
  if (!repairs.length) return []
  const groups = groupAutoRepairs(repairs)
  const lines = ['', `=== 自动修复（${groups.length} 张表，${repairs.length} 列）===`]
  for (const group of groups) {
    lines.push(`表 ${group.table} → ${group.target_table}`)
    for (const item of group.items) {
      const commentAdjustment = item.type === 'oracle_mysql_comment_truncate'
      const sourceType = commentAdjustment ? '' : item.source_type
      const typeNote = commentAdjustment ? `；${autoRepairType(item.type)}` : ''
      const sourceTypeNote = sourceType ? '' : `；源列类型${autoRepairSourceType(item)}`
      lines.push(`  ${item.source}${sourceType ? ` ${sourceType}` : ''} → ${item.target}${typeNote}；${item.reason}；${autoRepairStatus(item.status)}${sourceTypeNote}`)
    }
  }
  return [...lines, '']
}
