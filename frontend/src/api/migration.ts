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
}

export interface FullMigrationRequest {
  src_connection_id?: number
  src_database?: string
  dst_connection_id: number
  dst_database: string
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
  page_size?: number
  max_parallel?: number
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
  status: 'running' | 'done' | 'failed' | 'cancelled'
  summary: string
  created_at: string
  finished_at?: string
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

export interface MigrationReport {
  tables: CategoryReport
  data: CategoryReport
  views: CategoryReport
  indexes: CategoryReport
  constraints: CategoryReport
  sequences: CategoryReport
  triggers: CategoryReport
}

export const getDataMigrationReport = (jobID: string) =>
  api.get<MigrationReport>(`/migration/data-migrate/${jobID}/report`)
