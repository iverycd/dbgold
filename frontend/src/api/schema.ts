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
