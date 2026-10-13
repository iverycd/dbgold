import type { RepairAttempt, RepairObject, RepairRecord, RepairUnit } from '@/api/migrationRepair'

export function repairStatus(value: string) {
  return ({ unverified: '已执行待核验', draft: '草稿待执行', not_required: '无需导入', succeeded: '已修复', repaired: '已修复', success: '成功', failed: '待修复', pending: '未执行', blocked: '依赖阻塞', running: '执行中', queued: '等待执行', cancelled: '已取消', unknown: '待核验', needs_verification: '待核验', needs_data: '待补数据', not_started: '未执行', skipped: '未执行' } as Record<string, string>)[value] || value || '未执行'
}
export function repairKind(value: string) {
  return ({ table: '表', tables: '表', data: '数据', primary_key: '主键', primary_keys: '主键', primaryKeys: '主键', index: '索引', indexes: '索引', unique: '唯一约束', foreign_key: '外键', foreign_keys: '外键', constraints: '外键', sequence: '序列 / 自增', sequences: '序列 / 自增', view: '视图', views: '视图', comment: '表 / 列注释', comments: '表 / 列注释', owner: 'Owner', distribution: '分布属性' } as Record<string, string>)[value] || value
}
export const attemptRunning = (status: string) => ['running', 'queued', 'cancelling'].includes(status)
export const unitCommitted = (status: string) => ['succeeded', 'success', 'repaired'].includes(status)
export const isTableRepair = (object: RepairObject) => ['table', 'tables', 'data'].includes(object.kind)
export function cloneRepairUnits(units: RepairUnit[] | null | undefined): RepairUnit[] {
  return (units || []).map(unit => ({ ...unit, selected: unitCommitted(unit.status) || unit.destructive ? false : !!unit.selected }))
}
export function mappingError(mapping: Record<string, string>): string {
  const targets = Object.values(mapping)
  if (targets.some(target => !target.trim())) return '每个源列都需要指定目标列'
  if (new Set(targets).size !== targets.length) return '多个源列不能映射到同一个目标列'
  return ''
}
export function formatRepairHistory(records: RepairRecord[], attempts: RepairAttempt[]): string[] {
  const lines = ['','=== 对象修复记录（原始迁移结果保留） ===']
  if (!records.length) lines.push('无对象修复记录')
  for (const { object, version } of records) {
    lines.push(`【${repairKind(object.kind)}】${object.schema ? `${object.schema}.` : ''}${object.target_name}：${(object.original_status === 'succeeded' && unitCommitted(object.status) ? '成功' : repairStatus(object.status))}；草稿版本 ${version}`)
    if (object.original_error ?? object.error) lines.push(`原始错误：${object.original_error ?? object.error}`)
    if (object.original_error !== undefined && object.error && object.original_error !== object.error) lines.push(`当前错误：${object.error}`)
    if (object.ddl) lines.push(`原始 DDL：\n${object.ddl}`)
    if (object.data_status) lines.push(`数据状态：${repairStatus(object.data_status)}`)
    for (const [src, dst] of Object.entries(object.column_mapping || {})) lines.push(`列映射：${src} → ${dst}`)
    for (const unit of object.units || []) lines.push(`草稿步骤 ${unit.id}（${repairStatus(unit.status)}）：\n${unit.sql}`)
  }
  lines.push('', '=== 执行历史（提交时的 SQL 快照） ===')
  if (!attempts.length) lines.push('尚未执行修复')
  for (const attempt of attempts) {
    lines.push(`${attempt.created_at} ${attempt.id} 对象 ${attempt.object_id} ${attempt.kind === 'recovery' ? '接续迁移' : 'DDL 修复'}：${repairStatus(attempt.status)}`)
    for (const unit of attempt.units || []) lines.push(`步骤 ${unit.id}（${repairStatus(unit.status)}）：\n${unit.sql}${unit.error ? `\n错误：${unit.error}` : ''}`)
    if (attempt.error) lines.push(`错误：${attempt.error}`)
    if (attempt.verification) lines.push(`对象核验（${repairStatus(attempt.verification.status)}）：${attempt.verification.message}`)
    if (attempt.result) lines.push(`已提交 ${attempt.result.committed_rows} 行；源 ${attempt.result.source_rows} 行 / 目标 ${attempt.result.target_rows} 行；${attempt.result.error || ''}`)
  }
  return lines
}
