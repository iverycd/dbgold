import { readFileSync } from 'node:fs'
import vm from 'node:vm'
import ts from 'typescript'
import * as Vue from 'vue'
import { parse } from '@vue/compiler-sfc'
import { compile } from '@vue/compiler-dom'

const { descriptor } = parse(readFileSync(new URL('../../src/components/CompatibilityFunctionsPanel.vue', import.meta.url), 'utf8'))
const script = ts.transpileModule(`${descriptor.scriptSetup.content}\nexport { copyDDL, functionStatus, extensionStatus };`, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } }).outputText
const code = compile(descriptor.template.content, { mode: 'function', prefixIdentifiers: true }).code
const render = vm.runInNewContext(`(function() { ${code} })()`, { Vue })
export function createCompatibilityPanel(copied = []) {
  return {
    props: ['report'], render,
    setup(props) {
      const context = { exports: {}, defineProps: () => props, require: id => {
        if (id === '@/utils/clipboard') return { copyText: async text => copied.push(text) }
        if (id === '@arco-design/web-vue') return { Message: { success() {}, error(message) { throw Error(message) } } }
        throw Error(`Unexpected import ${id}`)
      } }
      vm.runInNewContext(script, context)
      return context.exports
    },
  }
}
