import api from './index'

export interface Column {
  name: string
  type: string
  nullable: boolean
  primary_key: boolean
  auto_increment: boolean
  comment: string
}

export interface Table {
  name: string
  columns: Column[]
  indexes: { name: string; columns: string[]; unique: boolean }[]
  constraints: { name: string; type: string; def: string }[]
  foreign_keys: { name: string; columns: string[]; ref_table: string; ref_columns: string[] }[]
}

export interface Schema {
  name: string
  tables: Table[]
}

export interface FullSchema extends Schema {
  views: { name: string; def: string }[]
  sequences: { name: string; start: number; increment: number }[]
  triggers: { name: string; table: string; event: string; timing: string; body: string }[]
}

export const extractSchema = (connectionId: number, database: string) =>
  api.post<Schema>('/schema/extract', { connection_id: connectionId, database })

export const parseDDLFile = (file: File) => {
  const form = new FormData()
  form.append('file', file)
  return api.post<FullSchema>('/schema/parse', form)
}

// downloadRoutines 导出源库自定义函数/存储过程/触发器原始 DDL 为 .sql 文件。
// 后端返回附件流，失败时返回 JSON 错误，需带 token（用 fetch + blob）。
export const downloadRoutines = async (connectionId: number, database: string) => {
  const token = localStorage.getItem('token') ?? ''
  const params = new URLSearchParams({
    connection_id: String(connectionId),
    database,
  })
  const resp = await fetch(`/api/schema/export-routines?${params.toString()}`, {
    headers: { Authorization: `Bearer ${token}` },
  })
  if (!resp.ok) {
    let msg = '导出失败'
    try {
      const data = (await resp.json()) as { error?: string }
      if (data.error) msg = data.error
    } catch {
      // 非 JSON 响应，用默认提示
    }
    throw new Error(msg)
  }
  const blob = await resp.blob()
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = `${database}_routines_triggers.sql`
  a.click()
  URL.revokeObjectURL(url)
}
