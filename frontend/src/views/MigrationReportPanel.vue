<template>
  <div class="migration-report-panel">
    <div v-if="loading" style="text-align: center; padding: 24px">
      <a-spin />
    </div>

    <a-alert v-else-if="fetchError" type="error" :content="fetchError" />

    <template v-else-if="report">
      <a-table
        :data="tableRows"
        row-key="key"
        :pagination="false"
        size="small"
      >
        <template #columns>
          <a-table-column title="对象类型" data-index="label" :width="120" />
          <a-table-column title="总数" data-index="total" :width="80" />
          <a-table-column title="成功" :width="80">
            <template #cell="{ record }">
              <span v-if="record.isTrigger">—</span>
              <span v-else>{{ record.success }}</span>
            </template>
          </a-table-column>
          <a-table-column title="失败" :width="80">
            <template #cell="{ record }">
              <span v-if="record.isTrigger">—</span>
              <span v-else :style="record.failed > 0 ? 'color: #f53f3f' : ''">{{ record.failed }}</span>
            </template>
          </a-table-column>
          <a-table-column title="状态">
            <template #cell="{ record }">
              <span v-if="record.isTrigger" style="color: #86909c">
                ⊘ 未迁移（{{ record.total === -1 ? '获取失败' : record.total + ' 个' }}）
              </span>
              <a-tag v-else-if="record.failed > 0" color="orange">⚠ 部分失败</a-tag>
              <a-tag v-else-if="record.total === 0" color="gray">无对象</a-tag>
              <a-tag v-else color="green">✓ 全部成功</a-tag>
            </template>
          </a-table-column>
        </template>

        <template #expand-row="{ record }">
          <div v-if="!record.isTrigger && record.failed > 0" class="failure-list">
            <div
              v-for="item in record.items"
              :key="item.name"
              class="failure-item"
            >
              <div class="failure-name">{{ item.name }}</div>
              <div class="failure-error">失败原因：{{ item.error }}</div>
              <div class="failure-ddl">
                <template v-if="item.ddl">
                  <div class="ddl-header">
                    <span>DDL：</span>
                    <a-button size="mini" type="text" @click="copyDDL(item.ddl)">复制 DDL</a-button>
                  </div>
                  <pre class="ddl-code">{{ item.ddl }}</pre>
                </template>
                <span v-else style="color: #86909c">DDL：—</span>
              </div>
            </div>
          </div>
        </template>
      </a-table>
    </template>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { Message } from '@arco-design/web-vue'
import {
  getDataMigrationReport,
  type MigrationReport,
  type ObjectResult,
} from '@/api/migration'

const props = defineProps<{ jobID: string }>()

const report = ref<MigrationReport | null>(null)
const loading = ref(false)
const fetchError = ref('')

interface ReportRow {
  key: string
  label: string
  total: number
  success: number
  failed: number
  items: ObjectResult[]
  isTrigger: boolean
}

const tableRows = computed<ReportRow[]>(() => {
  if (!report.value) return []
  const r = report.value
  return [
    { key: 'tables', label: '表', ...r.tables, isTrigger: false },
    { key: 'data', label: '数据写入', ...r.data, isTrigger: false },
    { key: 'views', label: '视图', ...r.views, isTrigger: false },
    { key: 'indexes', label: '索引', ...r.indexes, isTrigger: false },
    { key: 'constraints', label: '外键', ...r.constraints, isTrigger: false },
    { key: 'sequences', label: '序列', ...r.sequences, isTrigger: false },
    {
      key: 'triggers',
      label: '触发器',
      total: r.triggers.total,
      success: 0,
      failed: 0,
      items: [],
      isTrigger: true,
    },
  ]
})

async function loadReport() {
  loading.value = true
  fetchError.value = ''
  try {
    const res = await getDataMigrationReport(props.jobID)
    report.value = res.data
  } catch {
    fetchError.value = '暂无报告数据'
  } finally {
    loading.value = false
  }
}

async function copyDDL(ddl: string) {
  try {
    await navigator.clipboard.writeText(ddl)
    Message.success('DDL 已复制')
  } catch {
    Message.error('复制失败')
  }
}

onMounted(loadReport)
</script>

<style scoped>
.migration-report-panel {
  margin-top: 16px;
}
.failure-list {
  padding: 8px 16px;
  background: #f7f8fa;
}
.failure-item {
  padding: 8px 0;
  border-bottom: 1px solid #e5e6eb;
}
.failure-item:last-child {
  border-bottom: none;
}
.failure-name {
  font-weight: 600;
  margin-bottom: 4px;
}
.failure-error {
  color: #f53f3f;
  margin-bottom: 4px;
  font-size: 13px;
}
.ddl-header {
  display: flex;
  justify-content: space-between;
  align-items: center;
  margin-bottom: 4px;
}
.ddl-code {
  background: #1d1d1d;
  color: #d4d4d4;
  padding: 8px 12px;
  border-radius: 4px;
  font-size: 12px;
  font-family: 'Menlo', 'Monaco', 'Courier New', monospace;
  overflow-x: auto;
  white-space: pre;
  margin: 0;
}
</style>
