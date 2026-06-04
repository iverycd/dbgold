<template>
  <div>
    <div class="page-header">
      <div class="header-left">
        <span class="record-count">共 {{ records.length }} 条记录</span>
      </div>
      <a-button type="outline" :loading="loading" @click="load">
        <template #icon><icon-refresh /></template>
        刷新
      </a-button>
    </div>

    <a-table
      :data="records"
      :loading="loading"
      row-key="id"
      :pagination="{ pageSize: 50, showTotal: true }"
      :bordered="false"
    >
      <template #columns>
        <a-table-column title="#" data-index="id" :width="80" />
        <a-table-column title="账号" data-index="username" :width="160" />
        <a-table-column title="客户端 IP" data-index="client_ip" :width="180" />
        <a-table-column title="结果" :width="100">
          <template #cell="{ record }">
            <a-tag :color="record.success ? 'green' : 'red'">
              {{ record.success ? '成功' : '失败' }}
            </a-tag>
          </template>
        </a-table-column>
        <a-table-column title="登录时间" data-index="created_at">
          <template #cell="{ record }">
            {{ formatDate(record.created_at) }}
          </template>
        </a-table-column>
      </template>
    </a-table>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { Message } from '@arco-design/web-vue'
import { listLoginHistory, type LoginHistory } from '@/api/loginHistory'

const records = ref<LoginHistory[]>([])
const loading = ref(false)

async function load() {
  loading.value = true
  try {
    const res = await listLoginHistory(500)
    records.value = res.data
  } catch {
    Message.error('加载失败')
  } finally {
    loading.value = false
  }
}

function formatDate(s: string) {
  return new Date(s).toLocaleString('zh-CN', { hour12: false })
}

onMounted(load)
</script>

<style scoped>
.page-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 16px;
}
.header-left {
  display: flex;
  align-items: center;
  gap: 12px;
}
.record-count {
  font-size: 13px;
  color: #64748b;
}
</style>
