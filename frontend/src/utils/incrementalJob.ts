import type { IncrementalJob } from '@/api/migration'

export const incrementalStatusLabels: Record<string, string> = {
  initializing: '初始化',
  snapshot: '全量快照',
  catching_up: '追赶',
  running: '运行中',
  reconnecting: '重连中',
  pausing: '暂停中',
  paused_manual: '已暂停',
  paused_restart: '重启后暂停',
  paused_ddl: 'DDL 暂停',
  paused_row_conflict: '行冲突暂停',
  paused_bootstrap_review: '全量待确认',
  cutting_over: '追赶切换边界',
  validating: '最终校验',
  ready_to_cutover: '可完成切换',
  ready_with_warnings: '带风险待确认',
  cutover_blocked: '切换受阻',
  stopped: '已完成',
  aborted: '已放弃',
  failed: '失败',
}

export const bootstrapStageLabels: Record<string, string> = {
  schema: '建表',
  data: '数据复制',
  row_count: '行数校验',
  cdc_compatibility: 'CDC 兼容性',
  target_missing: '目标表缺失',
  objects: '对象创建',
  validation: '校验',
}

export const incrementalStatusText = (status: string) => incrementalStatusLabels[status] || status
export const bootstrapStageText = (stage: string) => bootstrapStageLabels[stage] || stage

export function incrementalStatusColor(status: string) {
  if (['running', 'ready_to_cutover', 'stopped'].includes(status)) return 'green'
  if (['failed', 'cutover_blocked'].includes(status)) return 'red'
  if (status.startsWith('paused') || status === 'ready_with_warnings') return 'orange'
  if (status === 'aborted') return 'orangered'
  return 'blue'
}

export function pausable(job: IncrementalJob) {
  return ['catching_up', 'running', 'reconnecting'].includes(job.status)
}

export function unsafeBootstrap(job: IncrementalJob) {
  return job.start_mode === 'full_then_cdc' && !job.bootstrap_completed && ['paused_restart', 'failed'].includes(job.status)
}

export function resumable(job: IncrementalJob) {
  return ['paused_manual', 'paused_restart', 'failed', 'paused_row_conflict'].includes(job.status) && job.locator_strategy_version === 1
}

export function preparable(status: string) {
  return ['running', 'catching_up'].includes(status)
}

export function cancelable(status: string) {
  return ['cutting_over', 'ready_to_cutover', 'ready_with_warnings', 'cutover_blocked'].includes(status)
}

export function completable(status: string) {
  return ['ready_to_cutover', 'ready_with_warnings'].includes(status)
}

export function abortable(status: string) {
  return !['validating', 'stopped', 'aborted'].includes(status)
}

export function bootstrapLogPolling(job: IncrementalJob) {
  return ['initializing', 'snapshot', 'paused_bootstrap_review'].includes(job.status)
}

export function positionText(file: string, position: number, gtid: string) {
  const filePosition = file ? `${file}:${position || 0}` : '—'
  return gtid ? `${filePosition} · GTID ${gtid}` : filePosition
}

export function connectionEndpoint(conn: { host: string; port: number }) {
  const host = conn.host.includes(':') && !conn.host.startsWith('[') ? `[${conn.host}]` : conn.host
  return `${host}:${conn.port}`
}

export const formatIncrementalDate = (value?: string) => value ? new Date(value).toLocaleString('zh-CN') : '—'
