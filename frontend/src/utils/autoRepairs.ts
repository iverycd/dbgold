import type { AutoRepair } from '@/api/migration'

export function groupAutoRepairs(repairs: AutoRepair[] = []) {
  const groups = new Map<string, { table: string; target_table: string; items: AutoRepair[] }>()
  for (const repair of repairs) {
    const key = JSON.stringify([repair.table, repair.target_table])
    if (!groups.has(key)) groups.set(key, { table: repair.table, target_table: repair.target_table, items: [] })
    groups.get(key)!.items.push(repair)
  }
  return [...groups.values()]
}

export function autoRepairStatus(status: AutoRepair['status']) {
  return status === 'reused' ? '复用已有映射' : '本次已修复'
}

export function formatAutoRepairs(repairs: AutoRepair[] = []): string[] {
  if (!repairs.length) return []
  const groups = groupAutoRepairs(repairs)
  const lines = ['', `=== 自动修复（${groups.length} 张表，${repairs.length} 列）===`]
  for (const group of groups) {
    lines.push(`表 ${group.table} → ${group.target_table}`)
    for (const item of group.items) lines.push(`  ${item.source} → ${item.target}；${item.reason}；${autoRepairStatus(item.status)}`)
  }
  return [...lines, '']
}
