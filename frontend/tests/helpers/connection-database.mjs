import { readFileSync } from 'node:fs'
import vm from 'node:vm'
import ts from 'typescript'
import * as Vue from 'vue'

function load(file, require) {
  const source = readFileSync(new URL(`../../src/${file}.ts`, import.meta.url), 'utf8')
  const context = { exports: {}, require }
  vm.runInNewContext(ts.transpileModule(source, {
    compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
  }).outputText, context)
  return context.exports
}

export const connectionDatabaseUtils = load('utils/connectionDatabase', () => { throw new Error('Unexpected import') })

export function loadConnectionDatabaseEditors(api = {}, vue = {}) {
  return load('composables/connectionDatabaseEditors', id => {
    if (id === 'vue') return { ...Vue, ...vue }
    if (id === '@/api/connections') return {
      listConnectionCatalogs: async () => ({ data: [] }),
      updateConnectionDatabase: async () => { throw new Error('Mock updateConnectionDatabase explicitly when saving') },
      ...api,
    }
    if (id === '@/utils/connectionDatabase') return connectionDatabaseUtils
    throw new Error(`Unexpected import ${id}`)
  })
}

export const connectionDatabaseEditors = loadConnectionDatabaseEditors()
