import api from './index'

export interface LoginResponse {
  token: string
  user: { id: number; username: string; role: string }
}

export interface User {
  id: number
  username: string
  role: string
  enabled: boolean
  created_at: string
}

export const login = (username: string, password: string) =>
  api.post<LoginResponse>('/auth/login', { username, password })

export const getMe = () => api.get<User>('/auth/me')

export const changePassword = (oldPassword: string, newPassword: string) =>
  api.put('/auth/password', { old_password: oldPassword, new_password: newPassword })
