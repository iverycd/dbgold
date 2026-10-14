import { EditorSelection, Transaction, type EditorState, type TransactionSpec } from '@codemirror/state'
import { isolateHistory } from '@codemirror/commands'
import { formatDialect, mysql, plsql, postgresql, sql, transactsql } from 'sql-formatter'

const dialects = {
  mysql,
  postgres: postgresql,
  gaussdb: postgresql,
  highgo: postgresql,
  vastbase: postgresql,
  gbase: postgresql,
  seabox: postgresql,
  kingbase: postgresql,
  sqlserver: transactsql,
  oracle: plsql,
  dameng: plsql,
  oscar: sql,
}

export function createSQLFormatTransaction(state: EditorState, dbType: string): TransactionSpec | null {
  const selection = state.selection.main
  const from = selection.empty ? 0 : selection.from
  const to = selection.empty ? state.doc.length : selection.to
  const text = state.sliceDoc(from, to)
  if (!text.trim()) return null

  const formatted = formatDialect(text, {
    dialect: dialects[dbType as keyof typeof dialects] ?? sql,
    tabWidth: 2,
    useTabs: false,
    keywordCase: 'preserve',
    dataTypeCase: 'preserve',
    functionCase: 'preserve',
    identifierCase: 'preserve',
  })
  if (formatted === text) return null

  return {
    changes: { from, to, insert: formatted },
    selection: selection.empty
      ? EditorSelection.cursor(Math.min(selection.head, formatted.length))
      : EditorSelection.range(from, from + formatted.length),
    annotations: [Transaction.userEvent.of('input.format'), isolateHistory.of('full')],
    scrollIntoView: true,
  }
}
