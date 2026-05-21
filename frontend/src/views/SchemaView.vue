<template>
  <div>
    <h2>Schema 提取</h2>
    <a-tabs default-active-key="db">
      <a-tab-pane key="db" title="从数据库提取">
        <a-space direction="vertical" fill style="width: 100%">
          <connection-select v-model:connection-id="connId" v-model:database="dbName" />
          <a-button type="primary" :loading="loading" @click="handleExtract" :disabled="!connId || !dbName">
            提取 Schema
          </a-button>
          <schema-table v-if="schema" :schema="schema" />
        </a-space>
      </a-tab-pane>

      <a-tab-pane key="ddl" title="上传 DDL 文件">
        <a-space direction="vertical" fill style="width: 100%">
          <a-upload
            :auto-upload="false"
            accept=".sql,.ddl,.txt"
            :limit="1"
            @change="handleFileChange"
          >
            <template #upload-button>
              <a-button>
                <template #icon><icon-upload /></template>
                选择 SQL 文件
              </a-button>
            </template>
          </a-upload>
          <a-button type="primary" :loading="loading" @click="handleParse" :disabled="!selectedFile">
            解析 DDL
          </a-button>
          <schema-table v-if="schema" :schema="schema" />
        </a-space>
      </a-tab-pane>
    </a-tabs>
  </div>
</template>

<script setup lang="ts">
import { ref, defineComponent, h } from 'vue'
import { Message } from '@arco-design/web-vue'
import type { FileItem } from '@arco-design/web-vue'
import ConnectionSelect from '@/components/ConnectionSelect.vue'
import { extractSchema, parseDDLFile, type Schema } from '@/api/schema'

const SchemaTable = defineComponent({
  props: { schema: Object as () => Schema },
  setup(props) {
    return () => h('div', [
      h('p', { style: 'color: #86909c; margin-bottom: 8px' }, `共 ${props.schema?.tables?.length ?? 0} 张表`),
      ...(props.schema?.tables ?? []).map(t =>
        h('div', { style: 'margin-bottom: 8px' }, [
          h('a-tag', { color: 'blue', style: 'margin-bottom: 4px' }, t.name),
          h('a-table', {
            data: t.columns ?? [],
            pagination: false,
            size: 'small',
            style: 'margin-bottom: 8px',
          }, {
            columns: () => [
              h('a-table-column', { title: '列名', dataIndex: 'name' }),
              h('a-table-column', { title: '类型', dataIndex: 'type' }),
              h('a-table-column', { title: '可空', dataIndex: 'nullable', width: 60, cell: ({ record }: { record: { nullable: boolean } }) => record.nullable ? '是' : '否' }),
            ]
          })
        ])
      )
    ])
  }
})

const connId = ref<number | undefined>()
const dbName = ref('')
const loading = ref(false)
const schema = ref<Schema | null>(null)
const selectedFile = ref<File | null>(null)

async function handleExtract() {
  if (!connId.value || !dbName.value) return
  loading.value = true
  try {
    const res = await extractSchema(connId.value, dbName.value)
    schema.value = res.data
    Message.success(`已提取 ${res.data.tables?.length ?? 0} 张表`)
  } catch {
    Message.error('提取失败，请检查连接和库名')
  } finally {
    loading.value = false
  }
}

function handleFileChange(fileList: FileItem[]) {
  selectedFile.value = fileList[0]?.file ?? null
}

async function handleParse() {
  if (!selectedFile.value) return
  loading.value = true
  try {
    const res = await parseDDLFile(selectedFile.value)
    schema.value = res.data
    Message.success(`已解析 ${res.data.tables?.length ?? 0} 张表`)
  } catch {
    Message.error('解析失败，请检查文件格式')
  } finally {
    loading.value = false
  }
}
</script>
