import type { MigrationDataIssue, MigrationReport, TableRowCount } from '@/api/migration'
import type { RepairDisplayRow } from './migrationRepair'

type IssueKind = MigrationDataIssue['kind'] | 'unknown'
export const issueLabels: Record<IssueKind, string> = {
  copy_error: '数据复制失败',
  row_count_mismatch: '行数不一致',
  row_count_error: '行数统计失败',
  content_validation: '数据内容有差异',
  verification_error: '数据校验未通过',
  unknown: '数据待核对',
}

type TableIssue = Omit<MigrationDataIssue, 'kind' | 'message'> & { kind: IssueKind; message: string; recorded?: boolean }
export interface MigrationTableEvidence {
  issues: { kind: IssueKind; message: string }[]
  rowCount?: TableRowCount
  pureMismatch: boolean
  originalMismatchErrors?: string[]
}

export interface MigrationDataIssueIndex {
  counts: Map<string, TableRowCount[]>
  issues: TableIssue[]
  byName: Map<string, TableIssue[]>
  ambiguousNames: Set<string>
  originalMismatchErrors: Map<string, string[]>
  incompleteErrors: boolean
}

function append<T>(map: Map<string, T[]>, key: string, value: T) {
  const list = map.get(key)
  if (list) list.push(value)
  else map.set(key, [value])
}

function tableNames(source?: string, target?: string) {
  return [...new Set([source, target].filter((name): name is string => !!name))]
}

function addIssue(index: MigrationDataIssueIndex, issue: TableIssue) {
  const existing = index.byName.get(issue.table) || []
  if (existing.some(item => item.table === issue.table && item.target_table === issue.target_table && item.kind === issue.kind && item.message === issue.message)) return
  index.issues.push(issue)
  for (const name of tableNames(issue.table, issue.target_table)) append(index.byName, name, issue)
}

export function indexMigrationDataIssues(report?: MigrationReport | null, repairRows: RepairDisplayRow[] = []): MigrationDataIssueIndex {
  const index: MigrationDataIssueIndex = { counts: new Map(), issues: [], byName: new Map(), ambiguousNames: new Set(), originalMismatchErrors: new Map(), incompleteErrors: false }
  if (!report) return index
  for (const count of report.rowCounts || []) append(index.counts, count.table, count)
  for (const issue of report.data_issues || []) {
    addIssue(index, { ...issue, message: issue.message || '', recorded: true })
  }
  const classifiedNames = new Set(index.byName.keys())

  // Older data category entries include both copy and verification failures.
  // A failed page is copy evidence, while an unqualified error remains available verbatim.
  const failures = report.data?.items || []
  index.incompleteErrors = (report.data?.failed || 0) > failures.filter(item => !!item.name).length
  for (const item of failures) {
    const source = item.source_name || item.table || item.name
    const target = item.target_table || item.target_name
    if (!source) continue
    const names = tableNames(source, target)
    if (item.failed_page && item.failed_page > 0) {
      if (!names.some(name => index.byName.get(name)?.some(issue => issue.kind === 'copy_error'))) {
        addIssue(index, { table: source, target_table: target, kind: 'copy_error', message: item.error || '' })
      }
    } else if (!names.some(name => classifiedNames.has(name))) {
      addIssue(index, { table: source, target_table: target, kind: 'unknown', message: item.error || '' })
    } else if (item.error && names.some(name => index.byName.get(name)?.some(issue => issue.kind === 'row_count_mismatch'))) {
      // Some writers include a committed-row prefix in the category error, even for a pure count mismatch.
      for (const name of names) append(index.originalMismatchErrors, name, item.error)
    }
  }
  // New reports identify which content checks ran. Older reports retain their per-table results.
  if (!report.data_issues) {
    for (const table of report.dataValidation?.tables || []) {
      if (table.status === 'passed') continue
      addIssue(index, { table: table.source_table, target_table: table.target_table, kind: table.status === 'mismatch' ? 'content_validation' : 'verification_error', message: table.error || table.note || '' })
    }
    for (const table of report.partition_migration?.tables || []) {
      if (['failed', 'blocked', 'incomplete'].includes(table.status)) {
        addIssue(index, { table: table.source_table, target_table: table.target_table, kind: 'verification_error', message: table.error || '分区校验未通过' })
      }
    }
  }

  // A row-count record has no schema. Never attach a shared name to two repair targets.
  const seen = new Set<string>()
  for (const { object } of repairRows) {
    if (!['table', 'tables', 'data'].includes(object.kind)) continue
    for (const name of tableNames(object.source_table || object.source_name || object.name, object.table || object.target_name)) {
      if (seen.has(name)) index.ambiguousNames.add(name)
      seen.add(name)
    }
  }
  return index
}

const numberFormat = new Intl.NumberFormat('zh-CN')
export function formatRowCountDetail(row: TableRowCount): string {
  const difference = row.dst - row.src
  const delta = difference ? `，${difference < 0 ? '少' : '多'} ${numberFormat.format(Math.abs(difference))} 行` : ''
  return `源 ${numberFormat.format(row.src)} 行，目标 ${numberFormat.format(row.dst)} 行${delta}`
}

interface CountPresentation {
  label: string
  color: 'green' | 'orange' | 'red' | 'gray'
  detail: string
  comparable: boolean
  mismatch: boolean
}

function presentCount(row: TableRowCount, issues: TableIssue[]): CountPresentation {
  const error = issues.find(issue => issue.kind === 'row_count_error')
  const confirmed = issues.some(issue => issue.kind === 'row_count_mismatch')
  // A partition can fail its own count even when its parent table has valid totals.
  // A confirmed append delta can be negative. Older reports also used -1 for an unread count.
  const valid = Number.isSafeInteger(row.src) && row.src >= 0 && Number.isSafeInteger(row.dst) && (confirmed || row.dst >= 0)
  if (valid && row.match && row.src === row.dst) {
    return error
      ? { label: '整表行数一致，另有统计失败', color: 'red', detail: error.message, comparable: true, mismatch: false }
      : { label: '一致', color: 'green', detail: '', comparable: true, mismatch: false }
  }
  if (error && !confirmed) return { label: issueLabels.row_count_error, color: 'red', detail: error.message, comparable: false, mismatch: false }
  const verification = issues.find(issue => issue.kind === 'verification_error')
  if (valid && !row.match && row.src === row.dst && verification?.recorded) {
    return { label: '两端行数一致，数据校验未通过', color: 'red', detail: verification.message, comparable: true, mismatch: false }
  }
  const ambiguousZero = (row.src === 0 || row.dst === 0) && issues.some(issue => issue.kind === 'copy_error' || issue.kind === 'unknown')
  if (valid && !row.match && row.src !== row.dst && (confirmed || !ambiguousZero)) {
    if (error) return { label: '行数不一致，另有统计失败', color: 'red', detail: error.message, comparable: true, mismatch: true }
    return { label: issueLabels.row_count_mismatch, color: 'orange', detail: formatRowCountDetail(row), comparable: true, mismatch: true }
  }
  const other = issues.find(issue => issue.kind === 'verification_error' || issue.kind === 'copy_error')
  return { label: other ? issueLabels[other.kind] : '行数待核对', color: other ? 'red' : 'gray', detail: other?.message || '', comparable: false, mismatch: false }
}

export function rowCountPresentation(index: MigrationDataIssueIndex, row: TableRowCount): CountPresentation {
  return presentCount(row, index.byName.get(row.table) || [])
}

export function rowCountEntries(report?: MigrationReport | null, index = indexMigrationDataIssues(report)) {
  const entries: (CountPresentation & { table: string; row?: TableRowCount })[] = (report?.rowCounts || []).map(row => ({
    table: row.table, row, ...rowCountPresentation(index, row),
  }))
  // A count may fail before any numeric result is saved.
  const presentNames = new Set(index.counts.keys())
  for (const issue of index.issues) {
    if (issue.kind !== 'row_count_error' || tableNames(issue.table, issue.target_table).some(name => presentNames.has(name))) continue
    const table = issue.target_table || issue.table
    entries.push({ table, label: issueLabels.row_count_error, color: 'red', detail: issue.message, comparable: false, mismatch: false })
    presentNames.add(table)
  }
  return entries
}

export function issuesForRepairRow(index: MigrationDataIssueIndex, row: RepairDisplayRow): MigrationTableEvidence | undefined {
  const object = row.object
  if (!['table', 'tables', 'data'].includes(object.kind)) return
  const source = object.source_table || object.source_name || object.name
  const target = object.table || object.target_name
  const names = tableNames(source, target)
  if (names.some(name => index.ambiguousNames.has(name))) return
  const counts = names.flatMap(name => index.counts.get(name) || [])
  const rowCount = counts.length === 1 ? counts[0] : undefined
  const recorded = [...new Set(names.flatMap(name => index.byName.get(name) || []))].filter(issue =>
    !issue.target_table || ((!source || source === issue.table) && (!target || target === issue.target_table)))
  const issues: MigrationTableEvidence['issues'] = recorded.map(({ kind, message }) => ({ kind, message }))
  const result = rowCount && presentCount(rowCount, recorded)
  if (result?.mismatch && !issues.some(item => item.kind === 'row_count_mismatch')) issues.push({ kind: 'row_count_mismatch', message: result.detail })
  if (!rowCount && !issues.length) return
  const hasUnexplainedError = !recorded.length && row.members.some(({ object }) => !!(object.original_error || object.error))
  const inferred = !recorded.some(issue => issue.kind === 'row_count_mismatch')
  if ((result?.mismatch && inferred && (index.incompleteErrors || hasUnexplainedError)) || (!issues.length && rowCount && !rowCount.match)) {
    issues.push({ kind: 'unknown', message: '' })
  }
  const originalMismatchErrors = names.flatMap(name => index.originalMismatchErrors.get(name) || [])
  return { issues, rowCount, pureMismatch: !!result?.mismatch && issues.every(issue => issue.kind === 'row_count_mismatch'), originalMismatchErrors }
}

export function summarizeRowCounts(report?: MigrationReport | null, index = indexMigrationDataIssues(report)) {
  let checked = 0
  let mismatched = 0
  let fewer = 0
  let more = 0
  let unknown = 0
  let verificationErrors = 0
  for (const row of report?.rowCounts || []) {
    const result = rowCountPresentation(index, row)
    if (!result.comparable) {
      if (result.label !== issueLabels.row_count_error) unknown++
      continue
    }
    checked++
    if (!row.match && !result.mismatch) verificationErrors++
    if (!result.mismatch) continue
    mismatched++
    if (row.src > row.dst) fewer += row.src - row.dst
    else more += row.dst - row.src
  }
  const countErrors = new Set(index.issues.filter(issue => issue.kind === 'row_count_error').map(issue => issue.table)).size
  const parts = [`已核对 ${numberFormat.format(checked)} 张表`]
  if (mismatched) parts.push(`其中 ${numberFormat.format(mismatched)} 张行数不一致`)
  if (fewer) parts.push(`校验时目标端合计少 ${numberFormat.format(fewer)} 行`)
  if (more) parts.push(`${fewer ? '另有表合计多' : '校验时目标端合计多'} ${numberFormat.format(more)} 行`)
  if (countErrors) parts.push(`${numberFormat.format(countErrors)} 张表行数统计失败`)
  if (verificationErrors) parts.push(`${numberFormat.format(verificationErrors)} 张表两端行数一致，但数据校验未通过`)
  if (unknown) parts.push(`${numberFormat.format(unknown)} 张表行数待核对`)
  return { checked, mismatched, fewer, more, countErrors, unknown, text: parts.join('，') + '。' }
}

export function reportDataIssueLines(report?: MigrationReport | null, index = indexMigrationDataIssues(report)): string[] {
  const lines = new Set<string>()
  for (const issue of index.issues) {
    const counts = tableNames(issue.table, issue.target_table).flatMap(name => index.counts.get(name) || [])
    const detail = issue.kind === 'row_count_mismatch' && counts.length === 1 && rowCountPresentation(index, counts[0]!).mismatch
      ? formatRowCountDetail(counts[0]!) : issue.message
    lines.add(`${issue.table} · ${issueLabels[issue.kind]}${detail ? ' · ' + detail : ''}`)
  }
  for (const row of report?.rowCounts || []) {
    if (row.match || index.byName.get(row.table)?.some(issue => issue.kind === 'row_count_mismatch' || issue.kind === 'row_count_error')) continue
    const result = rowCountPresentation(index, row)
    lines.add(`${row.table} · ${result.label}${result.detail ? ' · ' + result.detail : ''}`)
  }
  return [...lines]
}
