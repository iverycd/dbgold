import api from './index'

export interface QueryObject {
  name: string
  type: 'table' | 'view'
}

export interface QueryColumn {
  name: string
  data_type: string
  nullable: boolean
  primary_key: boolean
}

export interface ResultColumn {
  name: string
  type: string
}

export interface QueryRowsResult {
  kind: 'rows'
  columns: ResultColumn[]
  rows: unknown[][]
  row_count: number
  truncated: boolean
  duration_ms: number
  audit_id: number
}

export interface QueryCommandResult {
  kind: 'command'
  affected_rows: number
  duration_ms: number
  audit_id: number
}

export type QueryResult = QueryRowsResult | QueryCommandResult

export interface ExecuteQueryPayload {
  connection_id: number
  namespace: string
  sql: string
  confirmed?: boolean
  confirmation_text?: string
}

export interface ConfirmationRequired {
  code: 'confirmation_required'
  error: string
  risk_level: 'write' | 'dangerous'
  confirmation_mode: 'click' | 'type_connection_name'
  statement_type: string
  connection_name: string
}

export interface QueryAudit {
  id: number
  owner_id: number
  username?: string
  connection_id: number
  connection_name: string
  db_type: string
  namespace: string
  sql: string
  statement_type: string
  risk_level: string
  confirmed: boolean
  status: 'success' | 'failed'
  duration_ms: number
  row_count: number
  affected_rows: number
  truncated: boolean
  error?: string
  client_ip: string
  created_at: string
}

export const listQueryNamespaces = (connectionId: number) =>
  api.get<string[]>(`/query/connections/${connectionId}/namespaces`)

export const listQueryObjects = (connectionId: number, namespace: string) =>
  api.get<QueryObject[]>(`/query/connections/${connectionId}/objects`, { params: { namespace } })

export const listQueryColumns = (connectionId: number, namespace: string, object: string) =>
  api.get<QueryColumn[]>(`/query/connections/${connectionId}/columns`, {
    params: { namespace, object },
  })

export const executeQuery = (payload: ExecuteQueryPayload) =>
  api.post<QueryResult>('/query/execute', payload, { timeout: 35_000 })

export const listQueryHistory = (params: {
  scope?: 'mine' | 'all'
  connection_id?: number
  status?: string
  before_id?: number
  limit?: number
} = {}) => api.get<QueryAudit[]>('/query/history', { params })
