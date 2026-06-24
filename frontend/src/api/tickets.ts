import api from './index'

// TicketForm 前台提交工单的表单结构。
// 源库连接信息（host/username/password）与离线文件（src_file_*）二选一。
export interface TicketForm {
  applicant: string
  remark: string
  captcha_id: string
  captcha_code: string
  src_db_type: string
  src_host: string
  src_port: number
  src_database: string
  src_username: string
  src_password: string
  src_file_name?: string
  src_file_path?: string
  src_file_size?: number
  dst_db_type: string
  dst_host: string
  dst_port: number
  dst_database: string
  dst_username: string
  dst_password: string
}

// Ticket 列表项（后端列表接口不返回密码字段）。
export interface Ticket {
  id: number
  applicant: string
  remark: string
  src_db_type: string
  src_host: string
  src_port: number
  src_database: string
  src_username: string
  src_file_name: string
  src_file_path: string
  src_file_size: number
  dst_db_type: string
  dst_host: string
  dst_port: number
  dst_database: string
  dst_username: string
  client_ip: string
  status: string
  admin_note: string
  created_at: string
}

// TicketDetail 详情（含密码，仅 admin 详情接口返回）。
export interface TicketDetail extends Ticket {
  src_password: string
  dst_password: string
}

// UploadResult 源库离线文件上传结果。
export interface UploadResult {
  stored_path: string
  original_name: string
  size: number
}

// TicketInfoForm 管理员修改工单连接基础信息的表单（源库 / 目标库对称）。
// 不含源库离线文件字段（src_file_*）与流转字段（status / admin_note）。
export interface TicketInfoForm {
  applicant: string
  remark: string
  src_db_type: string
  src_host: string
  src_port: number
  src_database: string
  src_username: string
  src_password: string
  dst_db_type: string
  dst_host: string
  dst_port: number
  dst_database: string
  dst_username: string
  dst_password: string
}

// 公开提交（无需登录）
export const submitTicket = (form: TicketForm) =>
  api.post<{ id: number; message: string }>('/tickets', form)

// 获取图形验证码（无需登录）。image 为 base64 PNG data-uri，可直接作为 img src。
export const getCaptcha = () =>
  api.get<{ captcha_id: string; image: string }>('/tickets/captcha')

// uploadTicketFile 上传源库离线文件（.sql / .dmp，最大 50GB）。
// timeout: 0 关闭超时（默认 30s 对大文件远不够）；onProgress 透传 0-100 百分比。
export const uploadTicketFile = (file: File, onProgress?: (percent: number) => void) => {
  const fd = new FormData()
  fd.append('file', file)
  return api.post<UploadResult>('/tickets/upload', fd, {
    timeout: 0,
    onUploadProgress: (e) => {
      if (onProgress && e.total) {
        onProgress(Math.round((e.loaded * 100) / e.total))
      }
    },
  })
}

// 以下为管理员端点
export const listTickets = () => api.get<Ticket[]>('/admin/tickets')

export const getTicket = (id: number) => api.get<TicketDetail>(`/admin/tickets/${id}`)

export const updateTicket = (id: number, body: { status: string; admin_note: string }) =>
  api.put<{ message: string }>(`/admin/tickets/${id}`, body)

// updateTicketInfo 管理员修改工单的连接基础信息。
export const updateTicketInfo = (id: number, body: TicketInfoForm) =>
  api.put<{ message: string }>(`/admin/tickets/${id}/info`, body)

export const deleteTicket = (id: number) =>
  api.delete<{ message: string }>(`/admin/tickets/${id}`)

// createTicketConnections 用工单信息按名复用 / 创建源库 + 目标库连接，返回两个连接 ID。
export const createTicketConnections = (id: number) =>
  api.post<{ src_conn_id: number; dst_conn_id: number }>(`/admin/tickets/${id}/connections`, {})
