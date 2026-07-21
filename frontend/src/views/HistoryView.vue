<template>
  <div>
    <a-tabs v-model:active-key="activeTab" @change="handleTabChange">
      <!-- ===== 单次迁移任务 ===== -->
      <a-tab-pane key="data" title="单次迁移">
        <div style="display: flex; justify-content: flex-end; margin-bottom: 16px">
          <a-button @click="loadDataJobs" :loading="dataJobsLoading">
            <template #icon><icon-refresh /></template>
            刷新
          </a-button>
        </div>

        <a-table
          :data="dataJobs"
          :loading="dataJobsLoading"
          row-key="id"
          :pagination="{ pageSize: 20 }"
        >
          <template #columns>
            <a-table-column title="Job ID" :width="100">
              <template #cell="{ record }">
                <a-tooltip :content="record.job_id" mini>
                  <span style="font-family: monospace; font-size: 12px; cursor: default">
                    {{ record.job_id.slice(0, 6) }}…
                  </span>
                </a-tooltip>
              </template>
            </a-table-column>
            <a-table-column title="源库" :width="220">
              <template #cell="{ record }">
                <div class="history-conn-cell">
                  <a-tag :color="getDbTypeColor(record.src_db_type)" size="small">{{ getDbTypeLabel(record.src_db_type) }}</a-tag>
                  <a-tooltip v-if="record.src_conn" :content="record.src_conn.name" mini>
                    <span class="conn-name">{{ record.src_conn.name }}</span>
                  </a-tooltip>
                  <span v-else class="conn-deleted">已删除</span>
                </div>
                <div v-if="record.src_conn" class="conn-detail">
                  <span class="conn-label">库</span>
                  <a-tooltip :content="record.src_conn.database" mini>
                    <span class="conn-detail-val">{{ record.src_conn.database }}</span>
                  </a-tooltip>
                  <span class="conn-detail-sep">·</span>
                  <span class="conn-label">账号</span>
                  <span class="conn-detail-val">{{ record.src_conn.username }}</span>
                </div>
              </template>
            </a-table-column>
            <a-table-column title="目标库" :width="220">
              <template #cell="{ record }">
                <div class="history-conn-cell">
                  <a-tag :color="getDbTypeColor(record.dst_db_type)" size="small">{{ getDbTypeLabel(record.dst_db_type) }}</a-tag>
                  <a-tooltip v-if="record.dst_conn" :content="record.dst_conn.name" mini>
                    <span class="conn-name">{{ record.dst_conn.name }}</span>
                  </a-tooltip>
                  <span v-else class="conn-deleted">已删除</span>
                </div>
                <div v-if="record.dst_conn" class="conn-detail">
                  <span class="conn-label">库</span>
                  <a-tooltip :content="record.dst_conn.database" mini>
                    <span class="conn-detail-val">{{ record.dst_conn.database }}</span>
                  </a-tooltip>
                  <span class="conn-detail-sep">·</span>
                  <span class="conn-label">账号</span>
                  <span class="conn-detail-val">{{ record.dst_conn.username }}</span>
                  <template v-if="record.dst_schema">
                    <span class="conn-detail-sep">·</span>
                    <span class="conn-label">Schema</span>
                    <span class="conn-detail-val schema-val">{{ record.dst_schema }}</span>
                  </template>
                </div>
              </template>
            </a-table-column>
            <a-table-column title="迁移模式" :width="90">
              <template #cell="{ record }">
                <a-tag>{{ record.migrate_mode }}</a-tag>
              </template>
            </a-table-column>
            <a-table-column title="状态" :width="100">
              <template #cell="{ record }">
                <a-tag :color="dataJobStatusColor(record.status)">{{ record.status }}</a-tag>
              </template>
            </a-table-column>
            <a-table-column title="开始时间" :width="160">
              <template #cell="{ record }">
                {{ formatDate(record.created_at) }}
              </template>
            </a-table-column>
            <a-table-column title="结束时间" :width="160">
              <template #cell="{ record }">
                <span v-if="record.finished_at">{{ formatDate(record.finished_at) }}</span>
                <span v-else style="color: #86909c">—</span>
              </template>
            </a-table-column>
            <a-table-column title="操作" :width="100">
              <template #cell="{ record }">
                <a-button
                  size="small"
                  @click="viewReport(record)"
                >
                  详情
                </a-button>
              </template>
            </a-table-column>
          </template>
        </a-table>

      </a-tab-pane>

      <a-tab-pane key="incremental" title="增量迁移">
        <IncrementalHistoryPanel />
      </a-tab-pane>
    </a-tabs>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { Message } from '@arco-design/web-vue'
import IncrementalHistoryPanel from './IncrementalHistoryPanel.vue'
import { listDataMigrationJobs, type DataMigrationJob } from '@/api/migration'
import { getDbTypeColor, getDbTypeLabel } from '@/utils/dbType'

const dataJobs = ref<DataMigrationJob[]>([])
const dataJobsLoading = ref(false)
const route = useRoute()
const router = useRouter()
const activeTab = ref(route.query.tab === 'incremental' ? 'incremental' : 'data')

watch(() => route.query.tab, (tab) => {
  activeTab.value = tab === 'incremental' ? 'incremental' : 'data'
})

function handleTabChange(key: string | number) {
  const tab = String(key)
  router.replace({ path: '/history', query: tab === 'incremental' ? { tab: 'incremental' } : {} })
}

async function loadDataJobs() {
  dataJobsLoading.value = true
  try {
    const res = await listDataMigrationJobs()
    dataJobs.value = res.data
  } catch {
    Message.error('加载失败')
  } finally {
    dataJobsLoading.value = false
  }
}

function viewReport(record: DataMigrationJob) {
  router.push(`/history/data/${encodeURIComponent(record.job_id)}`)
}

function dataJobStatusColor(status: string) {
  return { done: 'green', failed: 'red', running: 'blue', cancelled: 'gray' }[status] ?? 'gray'
}

function formatDate(dateStr: string) {
  return new Date(dateStr).toLocaleString('zh-CN')
}

onMounted(() => {
  loadDataJobs()
})
</script>

<style scoped>
.history-conn-cell {
  display: flex;
  align-items: center;
  gap: 6px;
  min-width: 0;
}
.conn-name {
  font-size: 13px;
  font-weight: 500;
  color: var(--fg-primary);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  max-width: 130px;
  cursor: default;
}
.conn-deleted {
  font-size: 12px;
  color: #86909c;
}
.conn-detail {
  display: flex;
  align-items: center;
  gap: 3px;
  margin-top: 3px;
  font-size: 11px;
  color: var(--fg-muted);
  min-width: 0;
}
.conn-label {
  color: var(--fg-muted);
  flex-shrink: 0;
}
.conn-detail-val {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  max-width: 72px;
  cursor: default;
}
.schema-val {
  color: #165dff;
  max-width: 80px;
}
.conn-detail-sep {
  color: var(--border-strong);
  flex-shrink: 0;
}
.report-conn-info {
  padding: 12px 0 16px;
  border-bottom: 1px solid var(--color-border-2);
  margin-bottom: 4px;
}
.report-conn-row {
  display: flex;
  align-items: baseline;
  gap: 6px;
  margin-bottom: 6px;
  font-size: 13px;
}
.report-conn-label {
  font-weight: 600;
  min-width: 36px;
  color: var(--color-text-2);
}
.report-conn-sub {
  margin-left: 4px;
  font-size: 12px;
  color: var(--color-text-3);
}
.report-schema-badge {
  display: inline-block;
  margin-left: 6px;
  padding: 1px 8px;
  background: #e8f3ff;
  color: #165dff;
  border-radius: 4px;
  font-size: 12px;
  font-weight: 600;
  border: 1px solid #bedaff;
}
</style>
