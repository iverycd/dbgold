import api from './index'

export interface RepairUnit {
  id: string
  sql: string
  original_sql: string
  status: string
  error?: string
  selected: boolean
  destructive: boolean
}
export interface RepairColumn {
  Name: string
  DataType: string
  FormattedType?: string
  IsNullable: boolean
  Default?: string | null
  Extra?: string
  Generated?: string
  Identity?: string
}
export interface RepairObject {
  id: string
  kind: string
  name: string
  table?: string
  source_name?: string
  source_table?: string
  target_name: string
  schema: string
  ddl: string
  suggested_ddl?: string
  error?: string
  original_error?: string
  status: string
  original_status?: string
  units: RepairUnit[]
  dependencies: string[]
  column_mapping?: Record<string, string>
  columns?: RepairColumn[]
  source_columns?: RepairColumn[]
  foreign_key?: { TableName: string; Columns: string[]; RefSchema: string; RefTable: string; RefColumns: string[] }
  legacy: boolean
  requires_review: boolean
  data_status?: string
}
export interface RepairRecord { object: RepairObject; version: number }
export interface RepairBinding {
  src_conn_id: number
  dst_conn_id: number
  source_database: string
  source_schema: string
  target_schema: string
  configured: boolean
  legacy: boolean
}
export interface RepairList { objects: RepairRecord[]; binding: RepairBinding; running_attempt_id?: string }
export interface RecoveryResult { status: string; committed_rows: number; source_rows: number; target_rows: number; error?: string }
export interface RepairAttempt {
  id: string
  job_id: string
  object_id: string
  kind: 'ddl' | 'recovery'
  status: string
  units: RepairUnit[]
  result?: RecoveryResult
  verification?: { status: string; message: string }
  error?: string
  created_at: string
  finished_at?: string
}
export interface RecoveryPreview {
  object_id: string
  source_table: string
  target_table: string
  qualified_target: string
  source_rows: number
  target_rows: number
  column_mapping: Record<string, string>
  source_columns: RepairColumn[]
  target_columns: RepairColumn[]
  related: RepairObject[]
  data_required: boolean
  fingerprint: string
  warnings: string[]
}
export interface RecoveryPlan { token: string; preview: RecoveryPreview }
export interface RepairConfig {
  Content: string
  PageSize: number
  LowerCaseNames: boolean
  CharInLength: boolean
  UseNvarchar2: boolean
  Distributed: boolean
  ChangeOwner: boolean
  TargetSchema: string
  Objects: string[]
}
export interface BindingRequest {
  src_conn_id: number
  dst_conn_id: number
  source_database?: string
  source_schema?: string
  config?: RepairConfig
}
const base = (job: string) => `/migration/data-migrate/${encodeURIComponent(job)}`
const objectURL = (job: string, object: string) => `${base(job)}/repairs/${encodeURIComponent(object)}`
export const listRepairs = (job: string) => api.get<RepairList>(`${base(job)}/repairs`)
export const bindRepairConnections = (job: string, data: BindingRequest) => api.put(`${base(job)}/repairs/binding`, data)
export const saveRepairDraft = (job: string, object: string, data: { version: number; units: RepairUnit[]; column_mapping?: Record<string, string>; reviewed?: boolean }) => api.put<RepairRecord>(objectURL(job, object), data)
export const executeRepair = (job: string, object: string, data: { version: number; request_id: string; confirmed: true }) => api.post<RepairAttempt>(`${objectURL(job, object)}/execute`, data)
export const verifyRepair = (job: string, object: string, version: number) => api.post<RepairRecord & { verification: { status: string; message: string } }>(`${objectURL(job, object)}/verify`, { version })
export const listRepairAttempts = (job: string) => api.get<{ items: RepairAttempt[] }>(`${base(job)}/repair-attempts`)
export const getRepairAttempt = (job: string, attempt: string) => api.get<RepairAttempt>(`${base(job)}/repair-attempts/${encodeURIComponent(attempt)}`)
export const cancelRepairAttempt = (job: string, attempt: string) => api.post(`${base(job)}/repair-attempts/${encodeURIComponent(attempt)}/cancel`)
export const previewRecovery = (job: string, object: string, data: { version: number; column_mapping?: Record<string, string>; related_object_ids?: string[] }) => api.post<RecoveryPlan>(`${objectURL(job, object)}/recovery-preview`, data)
export const startRecovery = (job: string, object: string, data: { token: string; request_id: string; confirmed: true; source_quiesced: boolean; target_quiesced: boolean; truncate: boolean; confirmation_text?: string }) => api.post<RepairAttempt>(`${objectURL(job, object)}/recover`, data)
