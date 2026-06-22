import api from './index'
import type { User } from './auth'

export type { User }

export interface CreateUserForm {
  username: string
  password: string
  role: string
}

export const listUsers = () => api.get<User[]>('/admin/users')

export const createUser = (form: CreateUserForm) =>
  api.post<User>('/admin/users', form)

export const updateUser = (id: number, body: { enabled?: boolean; password?: string }) =>
  api.put<{ message: string }>(`/admin/users/${id}`, body)
