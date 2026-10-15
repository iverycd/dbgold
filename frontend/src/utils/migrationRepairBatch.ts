import type { RepairAttempt, RepairBinding, RepairList, RepairObject, RepairRecord } from '@/api/migrationRepair'
import { attemptRunning, canEditRepairDDL, cloneRepairUnits, unitCommitted } from '@/utils/migrationRepair'

export type RepairBatchStatus = 'pending' | 'saving' | 'running' | 'succeeded' | 'failed' | 'skipped' | 'stopped' | 'unknown'
export interface RepairBatchItem {
  record: RepairRecord
  requestID: string
  status: RepairBatchStatus
  reason: string
  attemptID?: string
}
export interface RepairBatchDependencies {
  load(): Promise<{ list: RepairList; jobRunning: boolean }>
  save(objectID: string, record: RepairRecord): Promise<RepairRecord>
  execute(objectID: string, version: number, requestID: string): Promise<RepairAttempt>
  getAttempt(id: string): Promise<RepairAttempt>
  onItems(items: RepairBatchItem[]): void
  onAttempt(attempt: RepairAttempt): void
  onError(message: string): void
  pollDelayMs?: number
}

const unknownStatus = (status?: string) => ['unknown', 'unverified', 'needs_verification'].includes(status || '')
const copy = <T>(value: T): T => JSON.parse(JSON.stringify(value))
const label = (object: RepairObject) => `${object.schema ? `${object.schema}.` : ''}${object.target_name || object.name || object.id}`
const hasUnknownResult = (object: RepairObject) => unknownStatus(object.status) || unknownStatus(object.data_status) || (object.units || []).some(unit => unknownStatus(unit.status))

/** The saved units alone define batch execution; suggestions and original DDL are never substituted. */
export function batchIneligibleReason(object: RepairObject): string {
  if (hasUnknownResult(object)) return '存在执行结果未知的记录，请先在目标数据库核对'
  if (object.diagnostic_only) return '仅供诊断的记录不可执行'
  if (unitCommitted(object.status)) return ['table', 'tables'].includes(object.kind) ? '表结构已执行成功，无需重复执行 DDL' : '对象已执行成功，无需再次执行'
  if (attemptRunning(object.status) || (object.units || []).some(unit => attemptRunning(unit.status))) return '对象正在执行'
  if (!canEditRepairDDL(object)) return '缺少可执行的目标对象信息'
  const selected = cloneRepairUnits(object.units).filter(unit => unit.selected)
  if (!selected.length) return '没有已勾选的有效执行单元，请先编辑并保存草稿'
  if (selected.some(unit => !unit.sql?.trim())) return '已勾选的执行单元包含空 SQL，请先编辑并保存草稿'
  return ''
}

export function batchBindingKey(binding: RepairBinding): string {
  return JSON.stringify([binding.src_conn_id, binding.dst_conn_id, binding.source_database, binding.source_schema, binding.target_schema, binding.configured, binding.legacy])
}

/** Stable dependency-first ordering, retaining skipped items so the review explains every selection. */
export function buildRepairBatch(records: RepairRecord[], selectedIDs: string[], newKey: () => string): RepairBatchItem[] {
  const all = new Map(records.map(record => [record.object.id, record]))
  const selected = new Map<string, RepairBatchItem>()
  for (const id of new Set(selectedIDs)) {
    const source = all.get(id)
    const record: RepairRecord = source ? copy(source) : { version: 0, object: { id, name: id, target_name: id, kind: '', schema: '', ddl: '', units: [], dependencies: [], legacy: false, requires_review: true, status: 'unknown' } }
    const reason = source ? batchIneligibleReason(record.object) : '对象已不存在，请刷新后重新选择'
    record.object.units = cloneRepairUnits(record.object.units)
    selected.set(id, { record, requestID: newKey(), status: reason ? 'skipped' : 'pending', reason })
  }
  const ordered: RepairBatchItem[] = [], visiting: string[] = [], visited = new Set<string>(), cycles = new Set<string>()
  function visit(id: string) {
    if (visited.has(id)) return
    const cycleStart = visiting.indexOf(id)
    if (cycleStart >= 0) {
      visiting.slice(cycleStart).forEach(member => cycles.add(member))
      return
    }
    const item = selected.get(id)!
    visiting.push(id)
    for (const dependency of item.record.object.dependencies || []) {
      if (selected.has(dependency) && !unitCommitted(all.get(dependency)?.object.status || '')) visit(dependency)
    }
    visiting.pop()
    visited.add(id)
    ordered.push(item)
  }
  selected.forEach((_, id) => visit(id))
  for (const item of ordered) {
    if (cycles.has(item.record.object.id) && item.status === 'pending') {
      item.status = 'skipped'
      item.reason = '存在依赖环，无法确定执行顺序'
    }
  }
  for (const item of ordered) {
    if (item.status !== 'pending') continue
    for (const id of item.record.object.dependencies || []) {
      const dependency = all.get(id)
      const chosen = selected.get(id)
      if (!dependency) item.reason = `缺少依赖对象：${id}`
      else if (unitCommitted(dependency.object.status)) continue
      else if (!chosen) item.reason = `依赖未成功且未选入本次执行：${label(dependency.object)}`
      else if (chosen.status === 'skipped') item.reason = `依赖不可执行：${label(dependency.object)}（${chosen.reason}）`
      if (item.reason) { item.status = 'skipped'; break }
    }
  }
  return ordered
}

function unitsKey(record: RepairRecord): string {
  return JSON.stringify(cloneRepairUnits(record.object.units).map(unit => [unit.id, unit.sql, unit.selected, unit.destructive]))
}
function targetKey(record: RepairRecord): string {
  const object = record.object
  return JSON.stringify([object.id, object.kind, object.schema, object.target_name, object.table, [...(object.dependencies || [])].sort()])
}
function errorText(error: unknown): string {
  const detail = error as { response?: { data?: { error?: string; message?: string } }; message?: string }
  return detail?.response?.data?.error || detail?.response?.data?.message || detail?.message || '请求失败'
}

/** In-memory queue. stop drains the accepted attempt; dispose detaches all work from a closed report. */
export function createRepairBatchQueue(input: RepairBatchItem[], expectedBindingKey: string, deps: RepairBatchDependencies) {
  const items = copy(input)
  let stopped = false, disposed = false, running: Promise<void> | undefined
  let timer: ReturnType<typeof setTimeout> | undefined
  let releaseDisposed!: () => void
  const disposal = new Promise<void>(resolve => { releaseDisposed = resolve })
  const publish = () => { if (!disposed) deps.onItems(copy(items)) }
  const haltPending = (reason: string) => {
    stopped = true
    for (const item of items) if (item.status === 'pending') { item.status = 'stopped'; item.reason = reason }
    publish()
  }
  // Race every await, including HTTP requests, so closing a report cannot retain its running queue.
  const awaitCurrent = <T>(promise: Promise<T>): Promise<{ value: T } | undefined> => Promise.race([
    promise.then(value => ({ value })), disposal.then(() => undefined),
  ])
  const pause = async () => {
    await awaitCurrent(new Promise<void>(resolve => { timer = setTimeout(() => { timer = undefined; resolve() }, deps.pollDelayMs ?? 2000) }))
  }
  function fatal(item: RepairBatchItem, reason: string, unknown = false) {
    if (disposed) return
    item.status = unknown ? 'unknown' : 'stopped'
    item.reason = reason
    haltPending('批量执行已停止，请核对当前结果后重新选择')
    deps.onError(reason)
  }
  function stopBeforeSubmission(item: RepairBatchItem): boolean {
    if (disposed) return true
    if (!stopped) return false
    item.status = 'stopped'; item.reason = '已停止后续执行'; publish()
    return true
  }
  async function perform() {
    publish()
    for (const item of items) {
      if (disposed || stopped) break
      if (item.status !== 'pending') continue
      let submitted = false
      try {
        item.status = 'saving'; publish()
        const loaded = await awaitCurrent(deps.load())
        if (!loaded || stopBeforeSubmission(item)) return
        const { list, jobRunning } = loaded.value
        if (!list.binding.configured || batchBindingKey(list.binding) !== expectedBindingKey) {
          fatal(item, '连接绑定已变化或尚未配置，已停止后续执行'); return
        }
        if (jobRunning || list.running_attempt_id) {
          fatal(item, '检测到其他迁移或修复正在执行，已停止后续执行'); return
        }
        const current = list.objects.find(record => record.object.id === item.record.object.id)
        if (!current) { item.status = 'skipped'; item.reason = '对象已不存在'; publish(); continue }
        if (current.version !== item.record.version || targetKey(current) !== targetKey(item.record) || unitsKey(current) !== unitsKey(item.record)) {
          fatal(item, '对象草稿版本或内容已变化，请重新预览确认'); return
        }
        if (hasUnknownResult(current.object)) { fatal(item, '对象存在执行结果未知的记录，已停止后续执行', true); return }
        if (attemptRunning(current.object.status) || current.object.units?.some(unit => attemptRunning(unit.status))) {
          fatal(item, '对象已在其他执行中，已停止后续执行'); return
        }
        let reason = batchIneligibleReason(current.object)
        for (const dependencyID of current.object.dependencies || []) {
          if (reason) break
          const dependency = list.objects.find(record => record.object.id === dependencyID)
          if (!dependency) reason = `缺少依赖对象：${dependencyID}`
          else if (!unitCommitted(dependency.object.status)) reason = `依赖尚未执行成功：${label(dependency.object)}`
        }
        if (reason) { item.status = 'skipped'; item.reason = reason; publish(); continue }
        const saved = await awaitCurrent(deps.save(item.record.object.id, copy(item.record)))
        if (!saved || stopBeforeSubmission(item)) return
        if (targetKey(saved.value) !== targetKey(item.record) || unitsKey(saved.value) !== unitsKey(item.record)) {
          fatal(item, '保存返回的目标对象或执行单元与确认内容不一致，已停止执行'); return
        }
        if (batchIneligibleReason(saved.value.object)) { fatal(item, '保存后的对象已不可执行，已停止后续执行'); return }
        // Do not retry a rejected execute request: its server-side acceptance may be unknown.
        submitted = true
        const accepted = await awaitCurrent(deps.execute(item.record.object.id, saved.value.version, item.requestID))
        if (!accepted || disposed) return
        let attempt = accepted.value
        item.attemptID = attempt.id
        while (!disposed) {
          if (!attempt.id || attempt.id !== item.attemptID || attempt.object_id !== item.record.object.id || attempt.kind !== 'ddl') {
            fatal(item, '执行响应与当前对象不一致，请通过执行历史核对结果', true); return
          }
          deps.onAttempt(copy(attempt))
          if (disposed) return
          if (unknownStatus(attempt.status) || attempt.units?.some(unit => unknownStatus(unit.status))) {
            fatal(item, attempt.error || '执行结果未知，已停止后续执行，请在目标数据库核对', true); return
          }
          if (!attemptRunning(attempt.status)) break
          item.status = 'running'; item.reason = ''; publish()
          await pause()
          if (disposed) return
          const polled = await awaitCurrent(deps.getAttempt(attempt.id))
          if (!polled || disposed) return
          attempt = polled.value
        }
        if (disposed) return
        if (unitCommitted(attempt.status)) { item.status = 'succeeded'; item.reason = '' }
        else if (attempt.status === 'failed') { item.status = 'failed'; item.reason = attempt.error || attempt.units?.find(unit => unit.error)?.error || '执行失败' }
        else if (attempt.status === 'cancelled') { item.status = 'stopped'; item.reason = '执行已取消'; haltPending('执行已取消，已停止后续执行') }
        else { fatal(item, `无法确认执行结果（${attempt.status || '空状态'}），已停止后续执行`, true); return }
        publish()
      } catch (error) {
        if (disposed) return
        fatal(item, `${submitted ? '提交或进度请求异常，执行结果需核对' : '准备执行失败'}：${errorText(error)}。不会自动重新提交。`, submitted)
        return
      }
    }
  }
  return {
    run(): Promise<void> { return running ||= perform() },
    stop() { if (!disposed) haltPending('已停止后续执行') },
    dispose() {
      disposed = true; stopped = true
      if (timer !== undefined) clearTimeout(timer)
      timer = undefined
      releaseDisposed()
    },
  }
}
