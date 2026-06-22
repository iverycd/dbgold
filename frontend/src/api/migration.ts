import api from './index'
import type { Schema } from './schema'

export interface MigrationHistory {
  id: number
  type: string
  src_conn_id: number
  src_database: string
  dst_conn_id: number
  dst_database: string
  sql_statements: string
  status: string
  error_message: string | null
  created_at: string
}

export interface MigrationResult {
  id: number
  sql_statements: string[]
}

export interface DiffMigrationRequest {
  src_connection_id?: number
  src_database?: string
  src_schema?: Schema
  dst_connection_id?: number
  dst_database?: string
  dst_schema?: Schema
  db_type?: string
  lower_case_names?: boolean
}

export interface FullMigrationRequest {
  src_connection_id?: number
  src_database?: string
  dst_connection_id: number
  dst_database: string
  lower_case_names?: boolean
}

export const runDiffMigration = (req: DiffMigrationRequest) =>
  api.post<MigrationResult>('/migration/diff', req)

export const runFullMigration = (req: FullMigrationRequest) =>
  api.post<MigrationResult>('/migration/full', req)

export const listMigrations = () => api.get<MigrationHistory[]>('/migration')

export const getMigration = (id: number) => api.get<MigrationHistory>(`/migration/${id}`)

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
  triggers: CategoryReport
  rowCounts: TableRowCount[]
}

export const getDataMigrationReport = (jobID: string) =>
  api.get<MigrationReport>(`/migration/data-migrate/${jobID}/report`)

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
export type MigrateObjectType = 'primary_keys' | 'indexes' | 'sequences' | 'foreign_keys'

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

