import type { RepairAttempt, RepairObject, RepairRecord, RepairUnit } from '@/api/migrationRepair'
import type { ObjectResult } from '@/api/migration'

export interface RepairSummary {
  jobID: string
  state: 'loading' | 'ready' | 'unknown'
  repaired: number
  attention: number
}
export type RepairFocusTarget = Pick<ObjectResult, 'name' | 'source_name' | 'target_name' | 'table' | 'target_table' | 'target_schema'> & { kind: string }

export function needsRepairAttention(object: RepairObject): boolean {
  return !unitCommitted(object.status) || !!(object.data_status && object.data_status !== 'not_required' && !unitCommitted(object.data_status))
}

export const repairCategories = [
  { key: 'repaired', label: '修复执行成功', empty: '当前无修复执行成功的对象' },
  { key: 'pending', label: '待修复', empty: '当前无待修复对象' },
  { key: 'blocked', label: '依赖阻塞', empty: '当前无依赖阻塞对象' },
  { key: 'data', label: '待补数据', empty: '当前无待补数据对象' },
] as const
export type RepairCategory = typeof repairCategories[number]['key']
export type RepairFilter = 'attention' | 'all' | RepairCategory

export function matchesRepairFilter(object: RepairObject, filter: RepairFilter, attempts: RepairAttempt[] = []): boolean {
  switch (filter) {
    case 'all': return true
    case 'attention': return needsRepairAttention(object)
    case 'repaired':
      return unitCommitted(object.status) && object.original_status !== 'succeeded' && !!(
        object.original_status || object.original_error || object.error ||
        object.units?.some(unit => unitCommitted(unit.status)) ||
        attempts.some(attempt => attempt.object_id === object.id && ['ddl', 'recovery'].includes(attempt.kind) && unitCommitted(attempt.status))
      )
    case 'pending': return !unitCommitted(object.status) && object.status !== 'blocked'
    case 'blocked': return object.status === 'blocked'
    case 'data': return !object.diagnostic_only && isTableRepair(object) && !!object.data_status && object.data_status !== 'not_required' && !unitCommitted(object.data_status)
  }
}

function canonicalRepairKind(kind: string): string {
  return ({ tables: 'table', data: 'table', primary_keys: 'primary_key', primaryKeys: 'primary_key', indexes: 'index', unique: 'index', foreign_keys: 'foreign_key', constraints: 'foreign_key', sequences: 'sequence', views: 'view', comments: 'comment' } as Record<string, string>)[kind] || kind
}

// Match recorded identifiers exactly; case folding can select a different object.
export function matchingRepairRecords(records: RepairRecord[], target: RepairFocusTarget): RepairRecord[] {
  const names = [target.name, target.source_name, target.target_name].filter(Boolean)
  return records.filter(({ object }) => {
    if (canonicalRepairKind(object.kind) !== canonicalRepairKind(target.kind)) return false
    if (target.table && ![object.table, object.source_table].includes(target.table)) return false
    if (target.target_table && object.table !== target.target_table) return false
    if (target.target_schema && object.schema !== target.target_schema) return false
    if (target.source_name && ![object.source_name, object.name].includes(target.source_name)) return false
    if (target.target_name && ![object.target_name, object.name].includes(target.target_name)) return false
    return names.some(name => [object.name, object.source_name, object.target_name].includes(name))
  })
}

export function repairStatus(value: string) {
  return ({ unverified: '执行结果未知', draft: '草稿待执行', not_required: '无需导入', succeeded: '执行成功', repaired: '执行成功', success: '成功', failed: '待修复', pending: '未执行', blocked: '依赖阻塞', running: '执行中', queued: '等待执行', cancelled: '已取消', unknown: '执行结果未知', needs_verification: '执行结果未知', needs_data: '待补数据', not_started: '未执行', skipped: '未执行' } as Record<string, string>)[value] || value || '未执行'
}
export function repairDataStatus(value: string): string {
  if (unitCommitted(value)) return '导入成功'
  return ({ failed: '导入失败，待接续', pending: '待导入', not_started: '待导入', skipped: '待导入', needs_data: '待导入', draft: '待导入', running: '导入中', queued: '等待导入', cancelling: '取消中', cancelled: '已取消', unknown: '导入结果未知', unverified: '导入结果未知', needs_verification: '导入结果未知', blocked: '依赖阻塞', not_required: '无需导入' } as Record<string, string>)[value] || value || '待导入'
}
export function repairObjectStatuses(object: RepairObject): { label: string; status: string; text: string }[] {
  if (object.kind === 'data') {
    const status = object.data_status || object.status
    return [{ label: '数据', status, text: repairDataStatus(status) }]
  }
  const statuses = [{ label: ['table', 'tables'].includes(object.kind) ? '表结构' : '', status: object.status, text: repairStatus(object.status) }]
  if (object.data_status) statuses.push({ label: '数据', status: object.data_status, text: repairDataStatus(object.data_status) })
  return statuses
}
export function repairKind(value: string) {
  return ({ table: '表', tables: '表', data: '数据', primary_key: '主键', primary_keys: '主键', primaryKeys: '主键', index: '索引', indexes: '索引', unique: '唯一约束', foreign_key: '外键', foreign_keys: '外键', constraints: '外键', sequence: '序列 / 自增', sequences: '序列 / 自增', view: '视图', views: '视图', comment: '表 / 列注释', comments: '表 / 列注释', owner: 'Owner', distribution: '分布属性' } as Record<string, string>)[value] || value
}
export const attemptRunning = (status: string) => ['running', 'queued', 'cancelling'].includes(status)
export const unitCommitted = (status: string) => ['succeeded', 'success', 'repaired'].includes(status)
export const isTableRepair = (object: RepairObject) => ['table', 'tables', 'data'].includes(object.kind)
export const hasRepairDDL = (object: RepairObject) => !!(object.ddl || object.units?.length || object.suggested_ddl)
export function canEditRepairDDL(object: RepairObject): boolean {
  if (object.diagnostic_only || !object.schema?.trim() || !object.target_name?.trim()) return false
  if (['table', 'tables', 'view', 'views', 'sequence', 'sequences', 'owner', 'distribution'].includes(object.kind)) return true
  return ['index', 'indexes', 'primary_key', 'primary_keys', 'primaryKeys', 'unique', 'foreign_key', 'foreign_keys', 'constraints', 'comment', 'comments'].includes(object.kind) && !!(object.table || object.ddl || object.units?.length)
}
export function repairExecutionStatus(status: string): string {
  return unitCommitted(status) ? '执行成功' : status === 'failed' ? '执行失败' : repairStatus(status)
}
export function repairVerificationStatus(status: string): string {
  return status === 'succeeded' ? '核验通过' : status === 'failed' ? '核验未通过' : '待人工核验'
}
export function repairAttemptKind(kind: string) {
  return ({ ddl: 'DDL 修复', recovery: '接续迁移', verification: '历史自动核验（旧版本）', manual_verification: '历史人工核验（旧版本）' } as Record<string, string>)[kind] || kind
}
export function repairAttemptStatus(attempt: RepairAttempt) {
  if (attempt.kind === 'manual_verification') return '人工核验通过'
  if (attempt.kind === 'verification') return repairVerificationStatus(attempt.verification?.status || attempt.status)
  return repairExecutionStatus(attempt.status)
}
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
    const statuses = repairObjectStatuses(object).map(({ label, text }) => `${label ? `${label}：` : ''}${text}`).join('；')
    lines.push(`【${repairKind(object.kind)}】${object.schema ? `${object.schema}.` : ''}${object.target_name}：${statuses}；草稿版本 ${version}`)
    if (object.original_error ?? object.error) lines.push(`原始错误：${object.original_error ?? object.error}`)
    if (object.original_error !== undefined && object.error && object.original_error !== object.error) lines.push(`当前错误：${object.error}`)
    if (object.ddl) lines.push(`原始 DDL：\n${object.ddl}`)
    if (object.source_ddl) lines.push(`源端定义（仅供参考）：\n${object.source_ddl}`)
    for (const [src, dst] of Object.entries(object.column_mapping || {})) lines.push(`列映射：${src} → ${dst}`)
    for (const unit of object.units || []) lines.push(`草稿步骤 ${unit.id}（${repairStatus(unit.status)}）：\n${unit.sql}`)
  }
  lines.push('', '=== 执行历史（提交时的 SQL 快照） ===')
  if (!attempts.length) lines.push('尚未执行修复')
  for (const attempt of attempts) {
    lines.push(`${attempt.created_at} ${attempt.id} 对象 ${attempt.object_id} ${repairAttemptKind(attempt.kind)}：${repairAttemptStatus(attempt)}`)
    if (attempt.kind === 'manual_verification') lines.push(`确认人：用户 #${attempt.owner_id}；确认时间：${attempt.finished_at || attempt.created_at}；核验依据：${attempt.verification?.message || ''}`)
    for (const unit of attempt.units || []) lines.push(`步骤 ${unit.id}（${repairExecutionStatus(unit.status)}）：\n${unit.sql}${unit.error ? `\n错误：${unit.error}` : ''}`)
    if (attempt.error) lines.push(`错误：${attempt.error}`)
    if (attempt.verification) lines.push(`历史核验（旧版本，${repairVerificationStatus(attempt.verification.status)}）：${attempt.verification.message}`)
    if (attempt.result) lines.push(`已提交 ${attempt.result.committed_rows} 行；源 ${attempt.result.source_rows} 行 / 目标 ${attempt.result.target_rows} 行；${attempt.result.error || ''}`)
  }
  return lines
}
