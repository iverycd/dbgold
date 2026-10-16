import api from './index'

export interface Connection {
  id: number
  name: string
  db_type: string
  host: string
  port: number
  database: string
  username: string
  created_at: string
  owner_username?: string
  env: string
}

export interface ConnectionForm {
  name: string
  db_type: string
  host: string
  port: number
  database: string
  username: string
  password: string
  env: string
}

export const listConnections = () => api.get<Connection[]>('/connections')

export const createConnection = (form: ConnectionForm) =>
  api.post<Connection>('/connections', form)

export const updateConnection = (id: number, form: ConnectionForm) =>
  api.put<{ message: string }>(`/connections/${id}`, form)

export const deleteConnection = (id: number) =>
  api.delete<{ message: string }>(`/connections/${id}`)

export const testConnection = (id: number) =>
  api.post<{ message: string }>(`/connections/${id}/test`)

export const listConnectionDatabases = (id: number) =>
  api.get<string[]>(`/connections/${id}/databases`)

// Catalogs are actual databases; /databases retains its source Schema semantics.
export const listConnectionCatalogs = (id: number) =>
  api.get<string[]>(`/connections/${id}/catalogs`)

export const updateConnectionDatabase = (id: number, database: string, expectedDatabase: string) =>
  api.patch<Connection>(`/connections/${id}/database`, {
    database,
    expected_database: expectedDatabase,
  })

export const listConnectionSchemas = (id: number) =>
  api.get<string[]>(`/connections/${id}/schemas`)

export const listConnectionViews = (id: number, database?: string, targetType?: string) =>
  api.get<string[]>(`/connections/${id}/views`, { params: { database, target_type: targetType } })
