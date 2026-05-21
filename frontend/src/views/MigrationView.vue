<template>
  <div>
    <h2>迁移 SQL 生成</h2>
    <a-tabs v-model:active-key="activeTab">
      <a-tab-pane key="diff" title="Diff 迁移">
        <a-space direction="vertical" fill style="width: 100%; margin-top: 12px">
          <a-row :gutter="24">
            <a-col :span="11">
              <a-card title="源">
                <connection-select v-model:connection-id="diffSrc.connId" v-model:database="diffSrc.dbName" />
              </a-card>
            </a-col>
            <a-col :span="2" style="display:flex;align-items:center;justify-content:center">
              <icon-arrow-right style="font-size: 24px; color: #165dff" />
            </a-col>
            <a-col :span="11">
              <a-card title="目标">
                <connection-select v-model:connection-id="diffDst.connId" v-model:database="diffDst.dbName" />
              </a-card>
            </a-col>
          </a-row>
          <a-button
            type="primary"
            :loading="diffLoading"
            :disabled="!(diffSrc.connId && diffSrc.dbName && diffDst.connId && diffDst.dbName)"
            @click="handleDiffMigration"
          >
            生成迁移 SQL
          </a-button>
          <sql-preview :sqls="diffSqls" />
        </a-space>
      </a-tab-pane>

      <a-tab-pane key="full" title="全量迁移">
        <a-space direction="vertical" fill style="width: 100%; margin-top: 12px">
          <a-card title="目标数据库（将为此库生成完整建表 SQL）">
            <connection-select v-model:connection-id="fullDst.connId" v-model:database="fullDst.dbName" />
          </a-card>
          <a-button
            type="primary"
            :loading="fullLoading"
            :disabled="!(fullDst.connId && fullDst.dbName)"
            @click="handleFullMigration"
          >
            生成全量 SQL
          </a-button>
          <sql-preview :sqls="fullSqls" />
        </a-space>
      </a-tab-pane>
    </a-tabs>
  </div>
</template>

<script setup lang="ts">
import { ref, reactive } from 'vue'
import { Message } from '@arco-design/web-vue'
import ConnectionSelect from '@/components/ConnectionSelect.vue'
import SqlPreview from '@/components/SqlPreview.vue'
import { runDiffMigration, runFullMigration } from '@/api/migration'

const activeTab = ref('diff')

const diffSrc = reactive({ connId: undefined as number | undefined, dbName: '' })
const diffDst = reactive({ connId: undefined as number | undefined, dbName: '' })
const diffLoading = ref(false)
const diffSqls = ref<string[]>([])

const fullDst = reactive({ connId: undefined as number | undefined, dbName: '' })
const fullLoading = ref(false)
const fullSqls = ref<string[]>([])

async function handleDiffMigration() {
  if (!diffSrc.connId || !diffDst.connId) return
  diffLoading.value = true
  diffSqls.value = []
  try {
    const res = await runDiffMigration({
      src_connection_id: diffSrc.connId,
      src_database: diffSrc.dbName,
      dst_connection_id: diffDst.connId,
      dst_database: diffDst.dbName,
    })
    diffSqls.value = res.data.sql_statements
    Message.success(`已生成 ${diffSqls.value.length} 条 SQL`)
  } catch {
    Message.error('生成失败')
  } finally {
    diffLoading.value = false
  }
}

async function handleFullMigration() {
  if (!fullDst.connId || !fullDst.dbName) return
  fullLoading.value = true
  fullSqls.value = []
  try {
    const res = await runFullMigration({
      dst_connection_id: fullDst.connId,
      dst_database: fullDst.dbName,
    })
    fullSqls.value = res.data.sql_statements
    Message.success(`已生成 ${fullSqls.value.length} 条 SQL`)
  } catch {
    Message.error('生成失败')
  } finally {
    fullLoading.value = false
  }
}
</script>
