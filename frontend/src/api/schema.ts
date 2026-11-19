import api from './index'

export interface SchemaComparePair {
  source: string
  target: string
}

interface SchemaCompareEndpoint {
  connection_id: number
  namespace: string
}

export interface SchemaCompareRequest {
  source: SchemaCompareEndpoint
  target: SchemaCompareEndpoint
  tables?: string[]
  preserve_case?: boolean
  char_in_length?: boolean
  use_nvarchar2?: boolean
}

interface SchemaChangeIdentity {
  id: string
  table: string
  object?: string
  kind: string
}

export interface SchemaChange extends SchemaChangeIdentity {
  status: 'applicable' | 'manual'
  reason?: string
  source?: unknown
  expected?: unknown
  target?: unknown
  sql?: string[]
  depends_on?: string[]
  order?: number
  execution_order?: number
}

export interface SchemaComparePlan {
  plan_id: string
  expires_at: string
  changes: SchemaChange[] | null
}

export interface SchemaApplyResult extends SchemaChangeIdentity {
  status: 'applied' | 'failed' | 'skipped' | 'unknown'
  error?: string
}

export const getSchemaComparePairs = () => api.get<SchemaComparePair[]>('/schema/compare/supported-pairs')

export const compareSchema = (data: SchemaCompareRequest) =>
  api.post<SchemaComparePlan>('/schema/compare', data, { timeout: 100_000 })

export const applySchemaChanges = (plan_id: string, change_ids: string[]) =>
  api.post<{ results: SchemaApplyResult[] }>('/schema/compare/apply', { plan_id, change_ids }, { timeout: 190_000 })

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
