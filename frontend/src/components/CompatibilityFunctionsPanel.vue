<template>
  <section class="compatibility-functions">
    <h4>{{ kind === 'procedure' ? '兼容存储过程' : '兼容函数' }}</h4>
    <a-space wrap>
      <span>总数 {{ report.total }}</span><span>成功 {{ report.success }}</span>
      <span>失败 {{ report.failed }}</span><span>新建 {{ report.created }}</span><span>复用 {{ report.reused }}</span>
    </a-space>
    <article v-for="item in report.items" :key="`${item.schema}.${item.signature || item.name}`">
      <div class="function-heading">
        <strong>{{ item.schema }}.{{ item.signature || `${item.name}()` }}</strong>
        <a-tag :color="item.status === 'failed' ? 'red' : item.status === 'reused' ? 'blue' : 'green'">{{ functionStatus[item.status] }}</a-tag>
        <a-button v-if="item.ddl" size="mini" type="text" @click="copyDDL(item.ddl)">复制 DDL</a-button>
      </div>
      <div v-if="item.generator">底层函数：{{ item.generator }}</div>
      <a-alert v-if="item.error" type="error">{{ item.error }}</a-alert>
      <details v-if="item.ddl" class="sql-details">
        <summary>查看 DDL</summary>
        <pre>{{ item.ddl }}</pre>
      </details>
      <p v-else>{{ item.definition_warning || (kind === 'procedure' ? '尚未生成存储过程 DDL' : '尚未生成函数 DDL') }}</p>
    </article>
    <article v-for="extension in report.extensions || []" :key="extension.name">
      <strong>扩展 {{ extension.schema }}.{{ extension.name }}</strong>：{{ extensionStatus[extension.status] }}
      <a-button v-if="extension.sql" size="mini" type="text" @click="copyDDL(extension.sql)">复制 SQL</a-button>
      <details v-if="extension.sql" class="sql-details">
        <summary>查看 SQL</summary>
        <pre>{{ extension.sql }}</pre>
      </details>
      <a-alert v-if="extension.error" type="error">{{ extension.error }}</a-alert>
    </article>
  </section>
</template>

<script setup lang="ts">
import { Message } from '@arco-design/web-vue'
import type { CompatibilityFunctionsReport } from '@/api/migration'
import { copyText } from '@/utils/clipboard'
defineProps<{ report: CompatibilityFunctionsReport; kind?: 'function' | 'procedure' }>()
const functionStatus = { created: '新建', reused: '复用', failed: '失败' }
const extensionStatus = { installed: '已安装', reused: '已复用', failed: '失败', rolled_back: '已回滚', unknown: '结果待核验' }
async function copyDDL(ddl: string) {
  try { await copyText(ddl); Message.success('DDL 已复制') }
  catch { Message.error('复制失败') }
}
</script>

<style scoped>
.compatibility-functions { padding: 12px 16px; overflow-wrap: anywhere; }
h4 { margin: 0 0 12px; }
article { padding: 12px 0; border-bottom: 1px solid var(--color-border-2); }
.function-heading { display: flex; flex-wrap: wrap; gap: 8px; align-items: center; margin-bottom: 8px; }
.sql-details { margin-top: 8px; }
.sql-details summary { cursor: pointer; color: var(--color-text-2); }
pre { white-space: pre-wrap; overflow-wrap: anywhere; padding: 12px; background: var(--color-fill-2); max-height: 320px; overflow: auto; }
</style>
