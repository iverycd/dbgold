import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import { reactive, ref, watch, nextTick } from 'vue'

// Exercise the actual TypeScript helpers/API without a browser or live server.
function loadTS(path, require = () => { throw new Error('Unexpected import') }) {
  const source = readFileSync(new URL(path, import.meta.url), 'utf8')
  const { outputText } = ts.transpileModule(source, {
    compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
  })
  const context = { exports: {}, require, FormData, Blob }
  vm.runInNewContext(outputText, context, { filename: path })
  return context.exports
}

const { applyTargetMigrationDefaults, defaultBatchOwnerOptions } = loadTS('../src/utils/migrationOptions.ts')

test('all three single-task option shapes default Oscar owner off and allow opt-in', async () => {
  for (const kind of ['data', 'view', 'object']) {
    const target = ref('postgres')
    const state = reactive({ lowerCaseNames: true, changeOwner: true, ...(kind === 'view' ? {} : { distributed: true }) })
    const stop = watch(target, value => applyTargetMigrationDefaults(state, value))
    target.value = 'oscar'
    await nextTick()
    assert.equal(state.changeOwner, false)
    assert.equal(state.lowerCaseNames, false)
    if (kind !== 'view') assert.equal(state.distributed, false)
    state.changeOwner = true // deliberate manual opt-in
    await nextTick()
    assert.equal(state.changeOwner, true)
    target.value = 'dameng'
    await nextTick()
    assert.equal(state.changeOwner, true)
    assert.equal(state.lowerCaseNames, false)
    target.value = 'postgres'
    await nextTick()
    assert.equal(state.lowerCaseNames, true)
    assert.equal(state.changeOwner, true)
    stop()
  }
})

test('mixed batch defaults and multipart payload keep Oscar override independent', () => {
  const defaults = defaultBatchOwnerOptions()
  assert.equal(defaults.change_owner, true)
  assert.equal(defaults.oscar_change_owner, false)
  const posts = []
  const api = { post: (url, form) => posts.push({ url, form }) }
  const { startBatch } = loadTS('../src/api/migration.ts', id => {
    assert.equal(id, './index')
    return { default: api }
  })
  const options = {
    ...defaults, migrate_content: 'both', page_size: 1000, max_parallel: 1,
    intra_table_parallel: 1, lower_case_names: true, char_in_length: false,
    use_nvarchar2: false, distributed: false, strip_view_schemas: '',
  }
  startBatch(new Blob(['fixture']), [], options)
  assert.equal(posts[0].form.get('change_owner'), 'true')
  assert.equal(posts[0].form.get('oscar_change_owner'), 'false')
  startBatch(new Blob(['fixture']), [], { ...options, change_owner: false, oscar_change_owner: true })
  assert.equal(posts[1].form.get('change_owner'), 'false')
  assert.equal(posts[1].form.get('oscar_change_owner'), 'true')
  delete options.oscar_change_owner
  startBatch(new Blob(['fixture']), [], options)
  assert.equal(posts[2].form.has('oscar_change_owner'), false)
})
