<template>
  <div>
    <h2>Schema 提取</h2>
    <a-tabs default-active-key="db">
      <a-tab-pane key="db" title="从数据库提取">
        <a-space direction="vertical" fill style="width: 100%">
          <connection-select v-model:connection-id="connId" v-model:database="dbName" />
          <a-button type="primary" :loading="dbLoading" @click="handleExtract" :disabled="!connId || !dbName">
            提取 Schema
          </a-button>
          <template v-if="dbSchema">
            <p style="color: #86909c; margin-bottom: 8px">共 {{ dbSchema.tables?.length ?? 0 }} 张表</p>
            <div v-for="t in dbSchema.tables" :key="t.name" style="margin-bottom: 16px">
              <a-tag color="blue" style="margin-bottom: 4px">{{ t.name }}</a-tag>
              <a-table :data="t.columns ?? []" :pagination="false" size="small">
                <template #columns>
                  <a-table-column title="列名" data-index="name" />
                  <a-table-column title="类型" data-index="type" />
                  <a-table-column title="可空" :width="60">
                    <template #cell="{ record }">{{ record.nullable ? '是' : '否' }}</template>
                  </a-table-column>
                </template>
              </a-table>
            </div>
          </template>
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
          <a-button type="primary" :loading="ddlLoading" @click="handleParse" :disabled="!selectedFile">
            解析 DDL
          </a-button>
          <template v-if="ddlSchema">
            <p style="color: #86909c; margin-bottom: 8px">共 {{ ddlSchema.tables?.length ?? 0 }} 张表</p>
            <div v-for="t in ddlSchema.tables" :key="t.name" style="margin-bottom: 16px">
              <a-tag color="blue" style="margin-bottom: 4px">{{ t.name }}</a-tag>
              <a-table :data="t.columns ?? []" :pagination="false" size="small">
                <template #columns>
                  <a-table-column title="列名" data-index="name" />
                  <a-table-column title="类型" data-index="type" />
                  <a-table-column title="可空" :width="60">
                    <template #cell="{ record }">{{ record.nullable ? '是' : '否' }}</template>
                  </a-table-column>
                </template>
              </a-table>
            </div>
          </template>
        </a-space>
      </a-tab-pane>
    </a-tabs>
  </div>
</template>

<script setup lang="ts">
import { ref } from 'vue'
import { Message } from '@arco-design/web-vue'
import type { FileItem } from '@arco-design/web-vue'
import ConnectionSelect from '@/components/ConnectionSelect.vue'
import { extractSchema, parseDDLFile, type Schema } from '@/api/schema'

const connId = ref<number | undefined>()
const dbName = ref('')
const dbLoading = ref(false)
const dbSchema = ref<Schema | null>(null)

const ddlLoading = ref(false)
const ddlSchema = ref<Schema | null>(null)
const selectedFile = ref<File | null>(null)

async function handleExtract() {
  if (!connId.value || !dbName.value) return
  dbLoading.value = true
  try {
    const res = await extractSchema(connId.value, dbName.value)
    dbSchema.value = res.data
    Message.success(`已提取 ${res.data.tables?.length ?? 0} 张表`)
  } catch {
    Message.error('提取失败，请检查连接和库名')
  } finally {
    dbLoading.value = false
  }
}

function handleFileChange(fileList: FileItem[]) {
  selectedFile.value = fileList[0]?.file ?? null
}

async function handleParse() {
  if (!selectedFile.value) return
  ddlLoading.value = true
  try {
    const res = await parseDDLFile(selectedFile.value)
    ddlSchema.value = res.data
    Message.success(`已解析 ${res.data.tables?.length ?? 0} 张表`)
  } catch {
    Message.error('解析失败，请检查文件格式')
  } finally {
    ddlLoading.value = false
  }
}
</script>
