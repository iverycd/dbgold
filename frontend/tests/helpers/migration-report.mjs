import { readFileSync } from 'node:fs'
import vm from 'node:vm'
import ts from 'typescript'

function loadUtility(name, require) {
  const source = readFileSync(new URL(`../../src/utils/${name}.ts`, import.meta.url), 'utf8')
  const context = { exports: {}, require }
  vm.runInNewContext(ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } }).outputText, context)
  return context.exports
}
export const migrationDataIssueUtils = loadUtility('migrationDataIssues')
export const migrationReportUtils = loadUtility('migrationReport', id => {
  if (id === './migrationDataIssues') return migrationDataIssueUtils
  throw new Error(`Unexpected report import ${id}`)
})
