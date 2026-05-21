import api from './index'
import type { Schema } from './schema'

export interface ColumnDiff {
  column: { name: string; type: string; nullable: boolean }
  old_column: { name: string; type: string; nullable: boolean }
  type_changed: boolean
  nullable_changed: boolean
  default_changed: boolean
}

export interface TableDiff {
  table_name: string
  added_columns: { name: string; type: string }[]
  dropped_columns: { name: string; type: string }[]
  modified_columns: ColumnDiff[]
  added_indexes: { name: string }[]
  dropped_indexes: { name: string }[]
}

export interface DiffResult {
  AddedTables: { name: string }[]
  DroppedTables: { name: string }[]
  ModifiedTables: TableDiff[]
}

export interface DiffRequest {
  src_connection_id?: number
  src_database?: string
  src_schema?: Schema
  dst_connection_id?: number
  dst_database?: string
  dst_schema?: Schema
}

export const diffSchemas = (req: DiffRequest) => api.post<DiffResult>('/diff', req)
