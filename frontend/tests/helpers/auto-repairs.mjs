import { readFileSync } from 'node:fs'
import vm from 'node:vm'
import ts from 'typescript'

const source = readFileSync(new URL('../../src/utils/autoRepairs.ts', import.meta.url), 'utf8')
const context = { exports: {} }
vm.runInNewContext(ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } }).outputText, context)
export const autoRepairUtils = context.exports
