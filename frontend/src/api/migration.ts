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
