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
}

export interface ConnectionForm {
  name: string
  db_type: string
  host: string
  port: number
  database: string
  username: string
  password: string
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
