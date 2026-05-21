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
