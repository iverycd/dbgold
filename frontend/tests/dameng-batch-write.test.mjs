import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import * as Vue from 'vue'
import { parse } from '@vue/compiler-sfc'

function execute(source, require) {
  const context = { exports: {}, require, FormData, Blob, setInterval: () => 1, clearInterval() {} }
  const js = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } })
  vm.runInNewContext(js.outputText, context)
  return context.exports
}

test('Excel applies the selected DM writer only when supported MySQL to DM data rows exist', async () => {
  const forms = []
  const api = execute(readFileSync(new URL('../src/api/migration.ts', import.meta.url), 'utf8'), () => ({
    default: { post: async (_url, form) => { forms.push(form); return { data: { batch_id: 'test' } } } },
  }))
  const { descriptor } = parse(readFileSync(new URL('../src/views/BatchMigrationView.vue', import.meta.url), 'utf8'))
  const scope = Vue.effectScope()
  const page = scope.run(() => execute(`${descriptor.scriptSetup.content}
    export { validateResult, selectedFile, options, canSelectDamengWriteStrategy, handleFileChange, handleStart };`, id => {
    if (id === 'vue') return { ...Vue, onUnmounted() {} }
    if (id === '@/api/migration') return { startBatch: api.startBatch, listBatchJobs: async () => ({ data: [] }) }
    if (id === '@/utils/migrationOptions') return { defaultBatchOwnerOptions: () => ({ change_owner: true }) }
    if (id === '@arco-design/web-vue') return { Message: { error: text => assert.fail(text) } }
    return {}
  }))
  const row = (src_db_type, dst_db_type, supported = true) => ({ src_db_type, dst_db_type, supported })
  const input = rows => { page.validateResult.value = { rows, supported_count: rows.filter(r => r.supported).length } }
  async function expectForm(strategy) {
    const before = forms.length
    await page.handleStart()
    assert.equal(forms.length, before + 1)
    assert.equal(forms.at(-1).get('dameng_write_strategy'), strategy)
  }

  try {
    page.selectedFile.value = new Blob(['excel'])
    input([row('mysql', 'dameng', false), row('dameng', 'dameng'), row('mysql', 'gaussdb')])
    assert.equal(page.canSelectDamengWriteStrategy.value, false)
    await expectForm(null)

    input([row('mysql', 'dameng'), row('mysql', 'postgres')])
    assert.equal(page.canSelectDamengWriteStrategy.value, true)
    await expectForm('batch')
    page.options.dameng_write_strategy = 'row'
    page.options.migrate_content = 'data_only'
    await expectForm('row')
    page.options.migrate_content = 'schema_only'
    assert.equal(page.canSelectDamengWriteStrategy.value, false)
    await expectForm(null)
    page.options.migrate_content = 'both'
    await expectForm('batch')

    page.options.dameng_write_strategy = 'row'
    page.handleFileChange([], { file: new Blob(['new excel']) })
    assert.equal(page.canSelectDamengWriteStrategy.value, false)
    input([row('oracle', 'dameng')])
    await expectForm(null)
    input([row('mysql', 'dameng')])
    await expectForm('batch')
  } finally { scope.stop() }
})
