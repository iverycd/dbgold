import api from './index'

// ===== 数据迁移 =====

export interface SupportedPair {
  source: string
  target: string
}

export interface StartDataMigrationRequest {
  src_conn_id: number
  dst_conn_id: number
  migrate_mode: 'all' | 'include' | 'exclude'
  table_filter?: string
  migrate_content?: 'both' | 'schema_only' | 'data_only'
  page_size?: number
  max_parallel?: number
  intra_table_parallel?: number
  lower_case_names?: boolean
  char_in_length?: boolean
  use_nvarchar2?: boolean
  distributed?: boolean
  change_owner?: boolean
  src_database?: string
  target_schema?: string
  strip_view_schemas?: string
  src_max_open_conns?: number
  src_max_idle_conns?: number
  src_conn_max_lifetime?: number
  dst_max_open_conns?: number
  dst_max_idle_conns?: number
  dst_conn_max_lifetime?: number
}

export interface ConnSnapshot {
  id: number
  name: string
  host: string
  port: number
  database: string
  username: string
}

export interface DataMigrationJob {
  id: number
  job_id: string
  src_conn_id: number
  dst_conn_id: number
  src_db_type: string
  dst_db_type: string
  migrate_mode: string
  table_filter: string
  page_size: number
  max_parallel: number
  intra_table_parallel?: number
  status: 'running' | 'done' | 'failed' | 'cancelled'
  summary: string
  created_at: string
  finished_at?: string
  dst_schema?: string
  src_conn?: ConnSnapshot
  dst_conn?: ConnSnapshot
}

export const getSupportedPairs = () =>
  api.get<SupportedPair[]>('/migration/data-migrate/supported-pairs')

export const startDataMigration = (data: StartDataMigrationRequest) =>
  api.post<{ job_id: string }>('/migration/data-migrate', data)

export const cancelDataMigration = (jobID: string) =>
  api.post<void>(`/migration/data-migrate/${jobID}/cancel`)

export const listDataMigrationJobs = () =>
  api.get<DataMigrationJob[]>('/migration/data-migrate/jobs')

// createDataMigrateEventSource 创建 SSE 连接，返回 EventSource 实例
// token 通过 query string 传递，因为浏览器 EventSource 不支持自定义 header
export const createDataMigrateEventSource = (jobID: string): EventSource => {
  const token = localStorage.getItem('token') ?? ''
  return new EventSource(`/api/migration/data-migrate/stream?jobID=${jobID}&token=${encodeURIComponent(token)}`)
}

// ===== 迁移报告 =====

export interface ObjectResult {
  name: string
  ddl: string
  error: string
}

export interface CategoryReport {
  total: number
  success: number
  failed: number
  items: ObjectResult[]
}

export interface TableRowCount {
  table: string
  src: number
  dst: number
  match: boolean
}

export interface MigrationReport {
  tables: CategoryReport
  data: CategoryReport
  primaryKeys: CategoryReport
  views: CategoryReport
  indexes: CategoryReport
  constraints: CategoryReport
  sequences: CategoryReport
  comments: CategoryReport
  triggers: CategoryReport
  rowCounts: TableRowCount[]
}

export const getDataMigrationReport = (jobID: string) =>
  api.get<MigrationReport>(`/migration/data-migrate/${jobID}/report`)

// ===== MySQL → PostgreSQL Binlog CDC =====

export interface IncrementalRequest {
  src_conn_id: number
  dst_conn_id: number
  src_database: string
  target_schema: string
  start_mode: 'full_then_cdc' | 'incremental_only'
  position_mode?: 'auto' | 'gtid' | 'file'
  start_gtid?: string
  start_file?: string
  start_position?: number
  server_id?: number
  migrate_mode: 'all' | 'include' | 'exclude'
  table_filter?: string
  lower_case_names?: boolean
  bootstrap_failure_policy?: 'review_and_exclude' | 'fail_all'
  keyless_change_policy?: 'full_row_match'
}

export interface CDCPosition { file: string; position: number; gtid: string }
export interface CDCTableInfo {
  name: string
  engine: string
  columns: string[]
  primary_key_indexes: number[]
  locator_strategy: 'primary_key' | 'unique_key' | 'full_row'
  locator_index?: string
  locator_columns: string[]
  locator_warning?: string
}
export interface IncrementalPreflight {
  ok: boolean
  log_bin: boolean
  binlog_format: string
  binlog_row_image: string
  gtid_mode: string
  binlog_retention_seconds: number | null
  current_position: CDCPosition
  tables: CDCTableInfo[]
  no_primary_key_tables: string[]
  errors: string[]
  warnings: string[]
}

export interface IncrementalJob {
  id: number
  job_id: string
  src_conn_id: number
  dst_conn_id: number
  src_database: string
  target_schema: string
  start_mode: string
  bootstrap_completed: boolean
  bootstrap_failure_policy: string
  keyless_change_policy: string
  locator_strategy_version: number
  primary_locator_count: number
  unique_locator_count: number
  full_row_locator_count: number
  bootstrap_state: string
  pending_file: string
  pending_position: number
  pending_gtid: string
  effective_table_count: number
  excluded_table_count: number
  failed_object_count: number
  failed_ddl_count: number
  bootstrap_manifest_hash: string
  status: string
  phase: string
  summary: string
  last_error: string
  blocking_ddl: string
  conflict_table: string
  conflict_action: string
  conflict_file: string
  conflict_position: number
  conflict_gtid: string
  conflict_error: string
  conflict_before_hash: string
  checkpoint_file: string
  checkpoint_position: number
  checkpoint_gtid: string
  source_head_file: string
  source_head_position: number
  source_head_gtid: string
  caught_up: boolean
  lag_seconds: number
  cutover_file: string
  cutover_position: number
  cutover_gtid: string
  validation_state: string
  validation_json: string
  insert_count: number
  update_count: number
  delete_count: number
  skipped_count: number
  warning_count: number
  log_dropped_count: number
  last_event_at?: string
  created_at: string
  updated_at: string
  finished_at?: string
}

export interface BootstrapIssue {
  table: string
  stage: 'schema' | 'data' | 'row_count' | 'cdc_compatibility'
  error: string
  ddl?: string
}

export interface BootstrapReview {
  state: string
  position: CDCPosition
  effective_tables: string[]
  excluded_tables: BootstrapIssue[]
  manifest_hash: string
  requested_count: number
  warnings: string[]
  failed_objects: BootstrapFailedObject[]
  failure_report_version: number
  locator_strategy_version: number
  locator_strategies: LocatorStrategy[]
}

export interface LocatorStrategy {
  table: string
  strategy: 'primary_key' | 'unique_key' | 'full_row'
  index?: string
  columns: string[]
}

export interface BootstrapFailedObject {
  category: 'table' | 'data' | 'primary_key' | 'view' | 'index' | 'foreign_key' | 'sequence' | 'comment' | 'row_count' | 'cdc_compatibility'
  name: string
  error: string
  ddl?: string
  stage: 'schema' | 'data' | 'objects' | 'validation'
}

export type IncrementalLogLevel = 'info' | 'ddl' | 'data' | 'index' | 'warn' | 'error' | 'done'

export interface IncrementalMigrationLog {
  id: number
  job_id?: string
  phase: string
  level: IncrementalLogLevel
  line: string
  created_at: string
}

export interface IncrementalMigrationLogPage {
  items: IncrementalMigrationLog[]
  oldest_id: number
  newest_id: number
  has_older: boolean
  has_newer: boolean
  log_dropped_count: number
}

export interface IncrementalMigrationLogQuery {
  after_id?: number
  before_id?: number
  limit?: number
}

export const preflightIncremental = (data: IncrementalRequest) =>
  api.post<IncrementalPreflight>('/migration/incremental/preflight', data)
export const startIncremental = (data: IncrementalRequest) =>
  api.post<{ job_id: string; preflight: IncrementalPreflight }>('/migration/incremental/jobs', data)
export const listIncrementalJobs = () => api.get<IncrementalJob[]>('/migration/incremental/jobs')
export const getIncrementalJob = (jobID: string) => api.get<IncrementalJob>(`/migration/incremental/jobs/${jobID}`)
export const getIncrementalMigrationLogs = (jobID: string, params: IncrementalMigrationLogQuery = {}) =>
  api.get<IncrementalMigrationLogPage>(`/migration/incremental/jobs/${jobID}/logs`, { params })
export const getIncrementalBootstrapReview = (jobID: string) =>
  api.get<BootstrapReview>(`/migration/incremental/jobs/${jobID}/bootstrap-review`)
export const downloadIncrementalFailedDDL = async (jobID: string) => {
  const token = localStorage.getItem('token') ?? ''
  const resp = await fetch(`/api/migration/incremental/jobs/${encodeURIComponent(jobID)}/export-failed-ddl`, {
    headers: { Authorization: `Bearer ${token}` },
  })
  if (!resp.ok) {
    let message = '导出修复 SQL 失败'
    try {
      const data = (await resp.json()) as { error?: string }
      if (data.error) message = data.error
    } catch {
      // Keep the stable fallback for non-JSON proxy errors.
    }
    throw new Error(message)
  }
  const disposition = resp.headers.get('Content-Disposition') || ''
  const filenameMatch = disposition.match(/filename="?([^";]+)"?/i)
  const filename = filenameMatch?.[1] || `incremental-${jobID.slice(0, 8)}-failed-ddl.sql`
  const blob = await resp.blob()
  const url = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = filename
  anchor.click()
  URL.revokeObjectURL(url)
}
export const acceptIncrementalBootstrapExclusions = (jobID: string, manifestHash: string) =>
  api.post(`/migration/incremental/jobs/${jobID}/accept-bootstrap-exclusions`, { manifest_hash: manifestHash, acknowledge: true })
export const pauseIncrementalJob = (jobID: string) => api.post(`/migration/incremental/jobs/${jobID}/pause`)
export const resumeIncrementalJob = (jobID: string) => api.post(`/migration/incremental/jobs/${jobID}/resume`)
export const prepareIncrementalCutover = (jobID: string) => api.post(`/migration/incremental/jobs/${jobID}/prepare-cutover`)
export const cancelIncrementalCutover = (jobID: string) => api.post(`/migration/incremental/jobs/${jobID}/cancel-cutover`)
export const stopIncrementalJob = (jobID: string, acknowledgeWarnings = false, acknowledgeExclusions = false) =>
  api.post(`/migration/incremental/jobs/${jobID}/stop`, {
    acknowledge_warnings: acknowledgeWarnings,
    acknowledge_exclusions: acknowledgeExclusions,
  })
export const abortIncrementalJob = (jobID: string) => api.post(`/migration/incremental/jobs/${jobID}/abort`)
export const acknowledgeIncrementalDDL = (jobID: string) => api.post(`/migration/incremental/jobs/${jobID}/ack-ddl`)

// ===== 视图迁移 =====

export interface MigrateViewsRequest {
  src_conn_id: number
  dst_conn_id: number
  view_names: string[]
  src_database?: string
  target_schema?: string
  lower_case_names?: boolean
  change_owner?: boolean
  strip_view_schemas?: string
}

// migrateViews 同步批量迁移选中的视图，返回每个视图的结果
export const migrateViews = (data: MigrateViewsRequest) =>
  api.post<{ results: ObjectResult[] }>('/migration/view-migrate', data, { timeout: 300000 })

// ===== 对象迁移（主键/索引/序列/外键）=====

// 对象迁移支持的对象类型
export type MigrateObjectType = 'primary_keys' | 'indexes' | 'sequences' | 'foreign_keys' | 'comments'

// listConnectionTables 列出源连接指定库下的全部表名
export const listConnectionTables = (connId: number, database?: string) =>
  api.get<string[]>(`/connections/${connId}/tables`, {
    params: database ? { database } : undefined,
  })

export interface StartObjectMigrationRequest {
  src_conn_id: number
  dst_conn_id: number
  migrate_objects: MigrateObjectType[]
  table_names: string[]
  src_database?: string
  target_schema?: string
  lower_case_names?: boolean
  change_owner?: boolean
  distributed?: boolean
  src_max_open_conns?: number
  src_max_idle_conns?: number
  src_conn_max_lifetime?: number
  dst_max_open_conns?: number
  dst_max_idle_conns?: number
  dst_conn_max_lifetime?: number
}

// startObjectMigration 启动仅对象迁移任务，返回 job_id（复用 SSE 日志流与迁移报告）
export const startObjectMigration = (data: StartObjectMigrationRequest) =>
  api.post<{ job_id: string }>('/migration/object-migrate', data)

// ===== 批量迁移（Excel 上传）=====

export interface BatchRow {
  row_num: number
  src_db_type: string
  src_host: string
  src_port: number
  src_database: string
  src_username: string
  dst_db_type: string
  dst_host: string
  dst_port: number
  dst_database: string
  dst_username: string
  target_schema: string
  supported: boolean
  reason: string
}

export interface BatchValidateResult {
  rows: BatchRow[]
  supported_count: number
  unsupported_count: number
}

export interface BatchMigration {
  id: number
  batch_id: string
  file_name: string
  total: number
  status: 'running' | 'done' | 'cancelled'
  created_at: string
  finished_at?: string
}

// validateBatch 上传 Excel 校验每行迁移对是否受支持（不执行）
export const validateBatch = (file: File) => {
  const fd = new FormData()
  fd.append('file', file)
  return api.post<BatchValidateResult>('/migration/batch/validate', fd)
}

// 批量迁移整批共用的迁移选项（对齐单任务默认值）
export interface BatchOptions {
  migrate_content: 'both' | 'schema_only' | 'data_only'
  page_size: number
  max_parallel: number
  intra_table_parallel: number
  lower_case_names: boolean
  char_in_length: boolean
  use_nvarchar2: boolean
  distributed: boolean
  change_owner: boolean
  strip_view_schemas: string
}

// startBatch 确认执行：重新上传 Excel + 被排除行号 + 整批迁移选项，后端串行执行受支持行
export const startBatch = (file: File, excludeRows: number[], opts: BatchOptions) => {
  const fd = new FormData()
  fd.append('file', file)
  if (excludeRows.length) fd.append('exclude_rows', excludeRows.join(','))
  fd.append('migrate_content', opts.migrate_content)
  fd.append('page_size', String(opts.page_size))
  fd.append('max_parallel', String(opts.max_parallel))
  fd.append('intra_table_parallel', String(opts.intra_table_parallel))
  fd.append('lower_case_names', String(opts.lower_case_names))
  fd.append('char_in_length', String(opts.char_in_length))
  fd.append('use_nvarchar2', String(opts.use_nvarchar2))
  fd.append('distributed', String(opts.distributed))
  fd.append('change_owner', String(opts.change_owner))
  fd.append('strip_view_schemas', opts.strip_view_schemas)
  return api.post<{ batch_id: string; total: number }>('/migration/batch/start', fd)
}

export const listBatches = () => api.get<BatchMigration[]>('/migration/batch')

export const listBatchJobs = (batchID: string) =>
  api.get<DataMigrationJob[]>(`/migration/batch/${batchID}/jobs`)

export const cancelBatch = (batchID: string) =>
  api.post<void>(`/migration/batch/${batchID}/cancel`)

// downloadBatchTemplate 下载空模板 xlsx（GET 文件，需带 token，用 fetch + blob）
export const downloadBatchTemplate = async () => {
  const token = localStorage.getItem('token') ?? ''
  const resp = await fetch('/api/migration/batch/template', {
    headers: { Authorization: `Bearer ${token}` },
  })
  if (!resp.ok) throw new Error('下载模板失败')
  const blob = await resp.blob()
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = 'batch_migration_template.xlsx'
  a.click()
  URL.revokeObjectURL(url)
}
