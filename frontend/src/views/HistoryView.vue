<template>
  <div>
    <a-tabs default-active-key="ddl">
      <!-- ===== DDL 迁移历史 ===== -->
      <a-tab-pane key="ddl" title="DDL 迁移">
        <div style="display: flex; justify-content: flex-end; margin-bottom: 16px">
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
                <a-button size="small" @click="viewDDLDetail(record)">查看</a-button>
              </template>
            </a-table-column>
          </template>
        </a-table>

        <a-drawer
          v-model:visible="ddlDrawerVisible"
          title="迁移 SQL 详情"
          :width="600"
        >
          <sql-preview :sqls="detailSqls" />
        </a-drawer>
      </a-tab-pane>

      <!-- ===== 数据迁移历史 ===== -->
      <a-tab-pane key="data" title="数据迁移">
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
            <a-table-column title="Job ID" :width="120">
              <template #cell="{ record }">
                <span style="font-family: monospace; font-size: 12px">
                  {{ record.job_id.slice(0, 8) }}...
                </span>
              </template>
            </a-table-column>
            <a-table-column title="源库" :width="180">
              <template #cell="{ record }">
                <div>
                  <a-tag :color="getDbTypeColor(record.src_db_type)" size="small" style="margin-right: 4px">{{ getDbTypeLabel(record.src_db_type) }}</a-tag>
                  <span v-if="record.src_conn">{{ record.src_conn.name }}</span>
                  <span v-else style="color: #86909c">已删除</span>
                </div>
                <div v-if="record.src_conn" class="conn-detail">
                  <span class="conn-label">数据库：</span>{{ record.src_conn.database }}
                  <a-divider direction="vertical" />
                  <span class="conn-label">账号：</span>{{ record.src_conn.username }}
                </div>
              </template>
            </a-table-column>
            <a-table-column title="目标库" :width="180">
              <template #cell="{ record }">
                <div>
                  <a-tag :color="getDbTypeColor(record.dst_db_type)" size="small" style="margin-right: 4px">{{ getDbTypeLabel(record.dst_db_type) }}</a-tag>
                  <span v-if="record.dst_conn">{{ record.dst_conn.name }}</span>
                  <span v-else style="color: #86909c">已删除</span>
                </div>
                <div v-if="record.dst_conn" class="conn-detail">
                  <span class="conn-label">数据库：</span>{{ record.dst_conn.database }}
                  <a-divider direction="vertical" />
                  <span class="conn-label">账号：</span>{{ record.dst_conn.username }}
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
                  v-if="record.status === 'done' || record.status === 'failed'"
                  size="small"
                  @click="viewReport(record)"
                >
                  查看报告
                </a-button>
                <span v-else style="color: #86909c">—</span>
              </template>
            </a-table-column>
          </template>
        </a-table>

        <a-drawer
          v-model:visible="reportDrawerVisible"
          title="迁移报告"
          :width="800"
          @close="reportJobId = ''"
        >
          <div v-if="reportJob" class="report-conn-info">
            <div class="report-conn-row">
              <span class="report-conn-label">源库</span>
              <a-tag :color="getDbTypeColor(reportJob.src_db_type)" size="small">{{ getDbTypeLabel(reportJob.src_db_type) }}</a-tag>
              <template v-if="reportJob.src_conn">
                <span>{{ reportJob.src_conn.name }} · {{ reportJob.src_conn.host }}:{{ reportJob.src_conn.port }}</span>
                <span class="report-conn-sub">数据库：{{ reportJob.src_conn.database }}&nbsp;&nbsp;账号：{{ reportJob.src_conn.username }}</span>
              </template>
              <span v-else style="color: #86909c">已删除</span>
            </div>
            <div class="report-conn-row">
              <span class="report-conn-label">目标库</span>
              <a-tag :color="getDbTypeColor(reportJob.dst_db_type)" size="small">{{ getDbTypeLabel(reportJob.dst_db_type) }}</a-tag>
              <template v-if="reportJob.dst_conn">
                <span>{{ reportJob.dst_conn.name }} · {{ reportJob.dst_conn.host }}:{{ reportJob.dst_conn.port }}</span>
                <span class="report-conn-sub">数据库：{{ reportJob.dst_conn.database }}&nbsp;&nbsp;账号：{{ reportJob.dst_conn.username }}</span>
                <span v-if="reportJob.dst_schema" class="report-schema-badge">Schema：{{ reportJob.dst_schema }}</span>
              </template>
              <span v-else style="color: #86909c">已删除</span>
            </div>
          </div>
          <MigrationReportPanel v-if="reportJobId" :jobID="reportJobId" />
        </a-drawer>
      </a-tab-pane>
    </a-tabs>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { Message } from '@arco-design/web-vue'
import SqlPreview from '@/components/SqlPreview.vue'
import MigrationReportPanel from './MigrationReportPanel.vue'
import { listMigrations, listDataMigrationJobs, type MigrationHistory, type DataMigrationJob } from '@/api/migration'
import { getDbTypeColor, getDbTypeLabel } from '@/utils/dbType'

const history = ref<MigrationHistory[]>([])
const loading = ref(false)
const ddlDrawerVisible = ref(false)
const detailSqls = ref<string[]>([])

const dataJobs = ref<DataMigrationJob[]>([])
const dataJobsLoading = ref(false)
const reportDrawerVisible = ref(false)
const reportJobId = ref('')
const reportJob = ref<DataMigrationJob | null>(null)

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

function viewDDLDetail(record: MigrationHistory) {
  try {
    const parsed: unknown = JSON.parse(record.sql_statements)
    detailSqls.value = Array.isArray(parsed) ? (parsed as string[]) : []
  } catch {
    detailSqls.value = []
  }
  ddlDrawerVisible.value = true
}

function viewReport(record: DataMigrationJob) {
  reportJobId.value = record.job_id
  reportJob.value = record
  reportDrawerVisible.value = true
}

function typeColor(type: string) {
  return { diff: 'blue', full: 'purple', selective: 'orange' }[type] ?? 'gray'
}

function dataJobStatusColor(status: string) {
  return { done: 'green', failed: 'red', running: 'blue', cancelled: 'gray' }[status] ?? 'gray'
}

function formatDate(dateStr: string) {
  return new Date(dateStr).toLocaleString('zh-CN')
}

onMounted(() => {
  loadHistory()
  loadDataJobs()
})
</script>

<style scoped>
.conn-detail {
  margin-top: 2px;
  font-size: 12px;
  color: var(--color-text-3);
}
.conn-label {
  color: var(--color-text-4);
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
