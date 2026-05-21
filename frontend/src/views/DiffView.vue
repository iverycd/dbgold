<template>
  <div>
    <h2>Schema 对比</h2>
    <a-row :gutter="24" style="margin-bottom: 16px">
      <a-col :span="11">
        <a-card title="源 Schema">
          <connection-select v-model:connection-id="src.connId" v-model:database="src.dbName" />
        </a-card>
      </a-col>
      <a-col :span="2" style="display:flex;align-items:center;justify-content:center">
        <icon-swap style="font-size: 24px; color: #165dff" />
      </a-col>
      <a-col :span="11">
        <a-card title="目标 Schema">
          <connection-select v-model:connection-id="dst.connId" v-model:database="dst.dbName" />
        </a-card>
      </a-col>
    </a-row>

    <a-button
      type="primary"
      :loading="loading"
      :disabled="!canDiff"
      @click="handleDiff"
      style="margin-bottom: 16px"
    >
      开始对比
    </a-button>

    <template v-if="result">
      <a-alert v-if="isEmpty" type="success" content="两个 Schema 完全一致，无差异" style="margin-bottom: 16px" />
      <template v-else>
        <a-card v-if="result.AddedTables?.length" title="新增表" style="margin-bottom: 12px">
          <a-space wrap>
            <a-tag v-for="t in result.AddedTables" :key="t.name" color="green">+ {{ t.name }}</a-tag>
          </a-space>
        </a-card>
        <a-card v-if="result.DroppedTables?.length" title="删除表" style="margin-bottom: 12px">
          <a-space wrap>
            <a-tag v-for="t in result.DroppedTables" :key="t.name" color="red">- {{ t.name }}</a-tag>
          </a-space>
        </a-card>
        <a-card v-if="result.ModifiedTables?.length" title="修改表" style="margin-bottom: 12px">
          <a-collapse>
            <a-collapse-item v-for="td in result.ModifiedTables" :key="td.table_name" :header="td.table_name">
              <div v-if="td.added_columns?.length">
                <p style="color: #00b42a; font-weight: bold">新增列</p>
                <a-tag v-for="c in td.added_columns" :key="c.name" color="green" style="margin: 2px">
                  {{ c.name }} {{ c.type }}
                </a-tag>
              </div>
              <div v-if="td.dropped_columns?.length" style="margin-top: 8px">
                <p style="color: #f53f3f; font-weight: bold">删除列</p>
                <a-tag v-for="c in td.dropped_columns" :key="c.name" color="red" style="margin: 2px">
                  {{ c.name }}
                </a-tag>
              </div>
              <div v-if="td.modified_columns?.length" style="margin-top: 8px">
                <p style="color: #ff7d00; font-weight: bold">修改列</p>
                <div v-for="c in td.modified_columns" :key="c.column.name" style="margin: 4px 0">
                  <a-tag color="orange">{{ c.column.name }}: {{ c.old_column.type }} → {{ c.column.type }}</a-tag>
                </div>
              </div>
            </a-collapse-item>
          </a-collapse>
        </a-card>
      </template>
    </template>
  </div>
</template>

<script setup lang="ts">
import { ref, reactive, computed } from 'vue'
import { Message } from '@arco-design/web-vue'
import ConnectionSelect from '@/components/ConnectionSelect.vue'
import { diffSchemas, type DiffResult } from '@/api/diff'

const src = reactive({ connId: undefined as number | undefined, dbName: '' })
const dst = reactive({ connId: undefined as number | undefined, dbName: '' })
const loading = ref(false)
const result = ref<DiffResult | null>(null)

const canDiff = computed(() =>
  !!(src.connId && src.dbName && dst.connId && dst.dbName)
)

const isEmpty = computed(() =>
  !result.value?.AddedTables?.length &&
  !result.value?.DroppedTables?.length &&
  !result.value?.ModifiedTables?.length
)

async function handleDiff() {
  if (!src.connId || !dst.connId) return
  loading.value = true
  try {
    const res = await diffSchemas({
      src_connection_id: src.connId,
      src_database: src.dbName,
      dst_connection_id: dst.connId,
      dst_database: dst.dbName,
    })
    result.value = res.data
  } catch {
    Message.error('对比失败')
  } finally {
    loading.value = false
  }
}
</script>
