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

// The primary record stays untouched so commands retain its ID and draft version.
export interface RepairDisplayRow extends RepairRecord {
  members: RepairRecord[]
}

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
  return ({ tables: 'table', data: 'table', primary_keys: 'primary_key', primaryKeys: 'primary_key', indexes: 'index', unique: 'index', foreign_keys: 'foreign_key', constraints: 'foreign_key', sequences: 'sequence', views: 'view', comments: 'comment', triggers: 'trigger' } as Record<string, string>)[kind] || kind
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
  return ({ table: '表', tables: '表', data: '数据', primary_key: '主键', primary_keys: '主键', primaryKeys: '主键', index: '索引', indexes: '索引', unique: '唯一约束', foreign_key: '外键', foreign_keys: '外键', constraints: '外键', sequence: '序列 / 自增', sequences: '序列 / 自增', view: '视图', views: '视图', comment: '表 / 列注释', comments: '表 / 列注释', trigger: '触发器', triggers: '触发器', function: '函数', procedure: '存储过程', package: '包', package_body: '包体', type: '自定义类型', type_body: '类型体', synonym: '同义词', materialized_view: '物化视图', grant: '对象授权', check: '检查约束', owner: 'Owner', distribution: '分布属性' } as Record<string, string>)[value] || value
}
export const attemptRunning = (status: string) => ['running', 'queued', 'cancelling'].includes(status)
export const unitCommitted = (status: string) => ['succeeded', 'success', 'repaired'].includes(status)
export const isTableRepair = (object: RepairObject) => ['table', 'tables', 'data'].includes(object.kind)
export const hasRepairDDL = (object: RepairObject) => !!(object.ddl || object.units?.length || object.suggested_ddl)
export const isMySQLPGTrigger = (object: RepairObject) => object.kind === 'trigger' && !!object.mysql_pg_trigger
export const canRegenerateTriggerPlan = (object: RepairObject): boolean => object.kind === 'trigger' && !object.native && !!object.diagnostic_only && !object.mysql_pg_trigger && !!object.source_name && !!object.source_table && !!object.source_ddl?.trim() && !object.ddl?.trim() && !object.suggested_ddl?.trim() && !object.units?.length && ['failed', 'pending', 'blocked', 'cancelled'].includes(object.status)
export function canRetryMySQLPGTrigger(object: RepairObject): boolean {
  if (!isMySQLPGTrigger(object) || object.diagnostic_only || !object.schema || !object.target_name || !['failed', 'pending', 'blocked', 'cancelled', 'rolled_back'].includes(object.status)) return false
  return object.units?.length === 2 && object.units.every(unit => !!unit.original_sql?.trim() && unit.sql === unit.original_sql && !unit.destructive && !['succeeded', 'success', 'repaired', 'unknown', 'unverified', 'needs_verification', 'running', 'queued'].includes(unit.status))
}
export function canRetryNative(object: RepairObject): boolean {
  return !!object.native && !!object.native_fingerprint && !object.native.managed && !object.diagnostic_only && object.native.schema === object.schema && object.native.name === object.target_name && object.units?.length > 0 && object.units.length === object.native.units.length && !['unknown', 'running', 'succeeded'].includes(object.status) && object.units.every((unit, index) => !!unit.original_sql && unit.sql === unit.original_sql && unit.sql === object.native!.units[index]?.sql && !['unknown', 'running'].includes(unit.status))
}
export const canExecuteRepairDDL = (object: RepairObject) => canEditRepairDDL(object) || canRetryMySQLPGTrigger(object) || canRetryNative(object)

export function canEditRepairDDL(object: RepairObject): boolean {
  if (object.native || object.diagnostic_only || !object.schema?.trim() || !object.target_name?.trim()) return false
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
export function cloneRepairExecutionUnits(object: RepairObject): RepairUnit[] {
  if (object.native) return (object.units || []).map(unit => ({ ...unit, selected: !unitCommitted(unit.status) && !unit.destructive }))
  const units = cloneRepairUnits(object.units)
  if (canRetryMySQLPGTrigger(object)) units.forEach(unit => { unit.selected = true })
  return units
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

function repairTableIdentity(object: RepairObject, source = false): { name: string; valid: boolean } {
  const names = (source ? [object.source_table, object.source_name] : [object.table, object.target_name]).filter((name): name is string => !!name)
  return { name: names[0] || '', valid: new Set(names).size <= 1 }
}

function compatibleRepairTable(table: RepairObject, data: RepairObject): boolean {
  if (table.schema !== data.schema) return false
  const tableTarget = repairTableIdentity(table)
  const dataTarget = repairTableIdentity(data)
  const tableSource = repairTableIdentity(table, true)
  const dataSource = repairTableIdentity(data, true)
  if (![tableTarget, dataTarget, tableSource, dataSource].every(identity => identity.valid)) return false
  if (tableTarget.name && dataTarget.name && tableTarget.name !== dataTarget.name) return false
  return !(tableSource.name && dataSource.name && tableSource.name !== dataSource.name)
}

export function groupRepairRecords(records: RepairRecord[]): RepairDisplayRow[] {
  const recordsByID = new Map<string, RepairRecord>()
  for (const record of records) {
    if (!recordsByID.has(record.object.id)) recordsByID.set(record.object.id, record)
  }
  const uniqueRecords = [...recordsByID.values()]
  const tableRecords = uniqueRecords.filter(({ object }) => ['table', 'tables'].includes(object.kind))
  const groups = new Map<RepairRecord, RepairDisplayRow>()
  const memberships = new Map<RepairRecord, RepairDisplayRow>()

  for (const data of uniqueRecords) {
    if (data.object.kind !== 'data' || data.object.diagnostic_only) continue
    const dependencies = new Set(data.object.dependencies || [])
    const tableDependencies = tableRecords.filter(({ object }) => dependencies.has(object.id))
    let table: RepairRecord | undefined
    if (dependencies.size) {
      // Explicit dependencies must resolve; missing or contradictory IDs cannot fall back to a name.
      if (tableDependencies.length !== 1 || [...dependencies].some(id => !recordsByID.has(id))) continue
      const candidate = tableDependencies[0]!
      if (!candidate.object.diagnostic_only && compatibleRepairTable(candidate.object, data.object)) table = candidate
    } else {
      const identity = repairTableIdentity(data.object)
      if (!identity.valid || !identity.name) continue
      const candidates = tableRecords.filter(({ object }) => !object.diagnostic_only && object.schema === data.object.schema && repairTableIdentity(object).name === identity.name)
      if (candidates.length === 1 && compatibleRepairTable(candidates[0]!.object, data.object)) table = candidates[0]
    }
    if (!table) continue
    let group = groups.get(table)
    if (!group) {
      group = { ...table, members: [table] }
      groups.set(table, group)
      memberships.set(table, group)
    }
    group.members.push(data)
    memberships.set(data, group)
  }

  for (const group of groups.values()) {
    const sourceNames = group.members.map(({ object }) => repairTableIdentity(object, true).name).filter(Boolean)
    if (new Set(sourceNames).size <= 1) continue
    // Missing table metadata must not let the first data record claim a shared target.
    for (const member of group.members) memberships.delete(member)
  }

  const emitted = new Set<RepairDisplayRow>()
  const rows: RepairDisplayRow[] = []
  for (const record of uniqueRecords) {
    const row = memberships.get(record) || { ...record, members: [record] }
    if (emitted.has(row)) continue
    emitted.add(row)
    rows.push(row)
  }
  return rows
}

export function repairDisplayErrors(row: RepairDisplayRow): { label: '结构' | '数据' | ''; text: string }[] {
  const errors: { label: '结构' | '数据' | ''; text: string }[] = []
  for (const { object } of row.members) {
    const text = object.original_error ?? object.error
    if (!text) continue
    const label = object.kind === 'data' ? '数据' : ['table', 'tables'].includes(object.kind) ? '结构' : ''
    if (!errors.some(error => error.label === label && error.text === text)) errors.push({ label, text })
  }
  return errors
}

function normalizedRepairDataStatus(status: string): string {
  if (unitCommitted(status)) return 'succeeded'
  if (['unknown', 'unverified', 'needs_verification'].includes(status)) return 'unknown'
  return ['pending', 'not_started', 'skipped', 'needs_data', 'draft'].includes(status) ? 'pending' : status
}

function repairDisplayDataStatus(row: RepairDisplayRow): { status: string; text: string; conflict: boolean } | undefined {
  const statuses = row.members.map(({ object }) => object.kind === 'data' ? object.data_status || object.status : object.data_status).filter((status): status is string => !!status)
  if (!statuses.length) return undefined
  if (new Set(statuses.map(normalizedRepairDataStatus)).size > 1) return { status: 'needs_verification', text: '数据状态待核对', conflict: true }
  return { status: statuses[0]!, text: repairDataStatus(statuses[0]!), conflict: false }
}

export function repairDisplayStatuses(row: RepairDisplayRow): { label: string; status: string; text: string }[] {
  if (row.members.length === 1) return repairObjectStatuses(row.object)
  const statuses = [{ label: '表结构', status: row.object.status, text: repairStatus(row.object.status) }]
  const data = repairDisplayDataStatus(row)
  if (data) statuses.push({ label: '数据', status: data.status, text: data.text })
  return statuses
}

export function needsRepairDisplayAttention(row: RepairDisplayRow): boolean {
  return row.members.some(({ object }) => needsRepairAttention(object)) || repairDisplayDataStatus(row)?.conflict === true
}

export function matchesRepairDisplayFilter(row: RepairDisplayRow, filter: RepairFilter, attempts: RepairAttempt[] = []): boolean {
  if (filter === 'attention') return needsRepairDisplayAttention(row)
  if (filter === 'data' && row.members.length > 1 && repairDisplayDataStatus(row)?.conflict === true) return true
  return row.members.some(({ object }) => matchesRepairFilter(object, filter, attempts))
}
