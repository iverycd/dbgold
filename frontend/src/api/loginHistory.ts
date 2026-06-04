import api from './index'

export interface LoginHistory {
  id: number
  username: string
  client_ip: string
  success: boolean
  created_at: string
}

export const listLoginHistory = (limit = 500) =>
  api.get<LoginHistory[]>('/admin/login-history', { params: { limit } })
