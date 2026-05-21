<template>
  <div>
    <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 16px">
      <h2 style="margin: 0">迁移历史</h2>
      <a-button @click="loadHistory" :loading="loading">
        <template #icon><icon-refresh /></template>
        刷新
      </a-button>
    </div>

    <a-table
      :data="history"
      :loading="loading"
      row-key="id"
      :pagination="{ pageSize: 20 }"
    >
      <template #columns>
        <a-table-column title="ID" data-index="id" :width="60" />
        <a-table-column title="类型" data-index="type" :width="90">
          <template #cell="{ record }">
            <a-tag :color="typeColor(record.type)">{{ record.type }}</a-tag>
          </template>
        </a-table-column>
        <a-table-column title="源" :width="180">
          <template #cell="{ record }">
            <span v-if="record.src_database">{{ record.src_database }}</span>
            <span v-else style="color: #c9cdd4">—</span>
          </template>
        </a-table-column>
        <a-table-column title="目标" :width="180">
          <template #cell="{ record }">
            <span>{{ record.dst_database }}</span>
          </template>
        </a-table-column>
        <a-table-column title="状态" data-index="status" :width="80">
          <template #cell="{ record }">
            <a-tag :color="record.status === 'success' ? 'green' : 'red'">{{ record.status }}</a-tag>
          </template>
        </a-table-column>
        <a-table-column title="时间" data-index="created_at" :width="170">
          <template #cell="{ record }">
            {{ formatDate(record.created_at) }}
          </template>
        </a-table-column>
        <a-table-column title="操作" :width="80">
          <template #cell="{ record }">
            <a-button size="small" @click="viewDetail(record)">查看</a-button>
          </template>
        </a-table-column>
      </template>
    </a-table>

    <a-drawer
      v-model:visible="drawerVisible"
      title="迁移 SQL 详情"
      :width="600"
    >
      <sql-preview :sqls="detailSqls" />
    </a-drawer>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { Message } from '@arco-design/web-vue'
import SqlPreview from '@/components/SqlPreview.vue'
import { listMigrations, type MigrationHistory } from '@/api/migration'

const history = ref<MigrationHistory[]>([])
const loading = ref(false)
const drawerVisible = ref(false)
const detailSqls = ref<string[]>([])

async function loadHistory() {
  loading.value = true
  try {
    const res = await listMigrations()
    history.value = res.data
  } catch {
    Message.error('加载失败')
  } finally {
    loading.value = false
  }
}

function viewDetail(record: MigrationHistory) {
  try {
    const parsed: unknown = JSON.parse(record.sql_statements)
    detailSqls.value = Array.isArray(parsed) ? (parsed as string[]) : []
  } catch {
    detailSqls.value = []
  }
  drawerVisible.value = true
}

function typeColor(type: string) {
  return { diff: 'blue', full: 'purple', selective: 'orange' }[type] ?? 'gray'
}

function formatDate(dateStr: string) {
  return new Date(dateStr).toLocaleString('zh-CN')
}

onMounted(loadHistory)
</script>
