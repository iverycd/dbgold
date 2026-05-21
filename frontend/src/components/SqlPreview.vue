<template>
  <div v-if="sqls.length > 0">
    <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 8px">
      <span style="color: #86909c">共 {{ sqls.length }} 条 SQL 语句</span>
      <a-button size="small" @click="copyAll">
        <template #icon><icon-copy /></template>
        复制全部
      </a-button>
    </div>
    <a-list :data="sqls" :max-height="400" size="small">
      <template #item="{ item, index }">
        <a-list-item>
          <a-space>
            <a-tag>{{ index + 1 }}</a-tag>
            <code style="font-family: monospace; word-break: break-all">{{ item }}</code>
          </a-space>
        </a-list-item>
      </template>
    </a-list>
  </div>
  <a-empty v-else description="暂无 SQL 语句" />
</template>

<script setup lang="ts">
import { Message } from '@arco-design/web-vue'

const props = defineProps<{ sqls: string[] }>()

function copyAll() {
  navigator.clipboard.writeText(props.sqls.join(';\n')).then(() => {
    Message.success('已复制到剪贴板')
  })
}
</script>
