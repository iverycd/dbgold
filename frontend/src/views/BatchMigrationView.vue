<template>
  <div class="batch-migration">
    <h2>批量迁移</h2>

    <!-- ===== 步骤一：上传 + 校验 ===== -->
    <a-card v-if="phase === 'upload'" :bordered="false" class="step-card">
      <a-space direction="vertical" fill style="width: 100%">
        <a-alert type="normal">
          上传 Excel（.xlsx），每行描述一对源库 / 目标库连接信息。系统会校验哪些迁移对受支持，
          不支持的将被自动排除。批次内任务<strong>按顺序逐个执行</strong>。
        </a-alert>

        <a-space>
          <a-upload
            :auto-upload="false"
            accept=".xlsx"
            :limit="1"
            :show-file-list="true"
            @change="handleFileChange"
          >
            <template #upload-button>
              <a-button>
                <template #icon><icon-upload /></template>
                选择 Excel 文件
              </a-button>
            </template>
          </a-upload>

          <a-button @click="handleDownloadTemplate">
            <template #icon><icon-download /></template>
            下载模板
          </a-button>

          <a-button
            type="primary"
            :loading="validating"
            :disabled="!selectedFile"
            @click="handleValidate"
          >
            校验
          </a-button>
        </a-space>

        <!-- 校验结果 -->
        <template v-if="validateResult">
          <a-alert :type="validateResult.supported_count > 0 ? 'success' : 'warning'">
            共 {{ validateResult.rows.length }} 行，受支持
            <strong style="color:#00b42a">{{ validateResult.supported_count }}</strong>
            行，不支持
            <strong style="color:#f53f3f">{{ validateResult.unsupported_count }}</strong>
            行（将被排除）。
          </a-alert>

          <a-table
            :data="validateResult.rows"
            row-key="row_num"
            :pagination="{ pageSize: 20 }"
            size="small"
          >
            <template #columns>
              <a-table-column title="行" data-index="row_num" :width="60" />
              <a-table-column title="源库" :width="200">
                <template #cell="{ record }">
                  <a-tag :color="getDbTypeColor(record.src_db_type)">
                    {{ getDbTypeLabel(record.src_db_type) }}
                  </a-tag>
                  <span class="conn-text">{{ record.src_host }}:{{ record.src_port }}/{{ record.src_database }}</span>
                </template>
              </a-table-column>
              <a-table-column title="目标库" :width="220">
                <template #cell="{ record }">
                  <a-tag :color="getDbTypeColor(record.dst_db_type)">
                    {{ getDbTypeLabel(record.dst_db_type) }}
                  </a-tag>
                  <span class="conn-text">
                    {{ record.dst_host }}:{{ record.dst_port }}/{{ record.dst_database }}
                  </span>
                </template>
              </a-table-column>
              <a-table-column title="目标 Schema" data-index="target_schema" :width="120" />
              <a-table-column title="状态" :width="220">
                <template #cell="{ record }">
                  <a-tag v-if="record.supported" color="green">✓ 支持</a-tag>
                  <a-tooltip v-else :content="record.reason">
                    <a-tag color="red">✕ 不支持</a-tag>
                  </a-tooltip>
                  <span v-if="!record.supported" class="reason-text">{{ record.reason }}</span>
                </template>
              </a-table-column>
            </template>
          </a-table>

          <a-collapse :default-active-key="['opts']" style="margin-top:4px">
            <a-collapse-item key="opts" header="迁移选项（应用到本批全部任务）">
              <a-form :model="options" layout="vertical">
                <a-row :gutter="16">
                  <a-col :span="8">
                    <a-form-item label="迁移内容">
                      <a-radio-group v-model="options.migrate_content">
                        <a-radio value="both">结构+数据</a-radio>
                        <a-radio value="schema_only">仅结构</a-radio>
                        <a-radio value="data_only">仅数据</a-radio>
                      </a-radio-group>
                    </a-form-item>
                  </a-col>
                  <a-col :span="8">
                    <a-form-item label="每批行数 pageSize">
                      <a-input-number v-model="options.page_size" :min="1000" :max="500000" :step="1000" style="width:160px" />
                    </a-form-item>
                  </a-col>
                  <a-col :span="8">
                    <a-form-item label="表间并发 maxParallel">
                      <a-input-number v-model="options.max_parallel" :min="1" :max="50" style="width:160px" />
                    </a-form-item>
                  </a-col>
                  <a-col :span="8">
                    <a-form-item label="表内并发 intraTableParallel">
                      <a-input-number v-model="options.intra_table_parallel" :min="1" :max="20" style="width:160px" />
                    </a-form-item>
                  </a-col>
                  <a-col :span="16">
                    <a-form-item label="视图剥离 schema 前缀（逗号分隔，可选）">
                      <a-input v-model="options.strip_view_schemas" placeholder="如 dbo,public" allow-clear />
                    </a-form-item>
                  </a-col>
                </a-row>
                <a-space wrap>
                  <a-checkbox v-model="options.lower_case_names">对象名转小写</a-checkbox>
                  <a-checkbox v-model="options.change_owner">更改对象 owner 为 Schema 同名角色</a-checkbox>
                  <a-checkbox v-model="options.char_in_length">char 长度单位（CHAR）</a-checkbox>
                  <a-checkbox v-model="options.use_nvarchar2">使用 nvarchar2</a-checkbox>
                  <a-checkbox v-model="options.distributed">分布式模式（DISTRIBUTE BY hash）</a-checkbox>
                </a-space>
              </a-form>
            </a-collapse-item>
          </a-collapse>

          <a-button
            type="primary"
            status="success"
            :loading="starting"
            :disabled="validateResult.supported_count === 0"
            @click="handleStart"
          >
            <template #icon><icon-play-arrow /></template>
            开始批量迁移（{{ validateResult.supported_count }} 个任务）
          </a-button>
        </template>
      </a-space>
    </a-card>

    <!-- ===== 步骤二：执行进度 ===== -->
    <a-card v-else :bordered="false" class="step-card">
      <a-space direction="vertical" fill style="width: 100%">
        <div style="display:flex;justify-content:space-between;align-items:center">
          <a-space>
            <a-tag :color="batchStatusColor">{{ batchStatusText }}</a-tag>
            <span class="conn-text">
              共 {{ jobs.length }} 个任务，完成 {{ finishedCount }} / {{ jobs.length }}
            </span>
          </a-space>
          <a-space>
            <a-button
              v-if="batchStatus === 'running'"
              status="danger"
              size="small"
              @click="handleCancel"
            >
              取消批次
            </a-button>
            <a-button size="small" @click="resetToUpload">返回上传</a-button>
          </a-space>
        </div>

        <a-table :data="jobs" row-key="job_id" :pagination="false" size="small">
          <template #columns>
            <a-table-column title="#" :width="50">
              <template #cell="{ rowIndex }">{{ rowIndex + 1 }}</template>
            </a-table-column>
            <a-table-column title="源库" :width="200">
              <template #cell="{ record }">
                <a-tag :color="getDbTypeColor(record.src_db_type)">
                  {{ getDbTypeLabel(record.src_db_type) }}
                </a-tag>
                <span class="conn-text">{{ record.src_conn?.host }}/{{ record.src_conn?.database }}</span>
              </template>
            </a-table-column>
            <a-table-column title="目标库" :width="200">
              <template #cell="{ record }">
                <a-tag :color="getDbTypeColor(record.dst_db_type)">
                  {{ getDbTypeLabel(record.dst_db_type) }}
                </a-tag>
                <span class="conn-text">{{ record.dst_conn?.host }}/{{ record.dst_conn?.database }}</span>
              </template>
            </a-table-column>
            <a-table-column title="目标 Schema" data-index="dst_schema" :width="110" />
            <a-table-column title="状态" :width="120">
              <template #cell="{ record }">
                <a-tag v-if="record.status === 'running'" color="blue">
                  <icon-loading /> 运行中
                </a-tag>
                <a-tag v-else-if="record.status === 'done'" color="green">✓ 完成</a-tag>
                <a-tag v-else-if="record.status === 'failed'" color="red">✕ 失败</a-tag>
                <a-tag v-else-if="record.status === 'cancelled'" color="gray">已取消</a-tag>
                <a-tag v-else color="gray">{{ record.status }}</a-tag>
              </template>
            </a-table-column>
            <a-table-column title="操作" :width="100">
              <template #cell="{ record }">
                <a-button
                  size="mini"
                  type="text"
                  :disabled="record.status === 'running'"
                  @click="openReport(record.job_id)"
                >
                  查看报告
                </a-button>
              </template>
            </a-table-column>
          </template>
        </a-table>
      </a-space>
    </a-card>

    <!-- 报告抽屉 -->
    <a-drawer
      v-model:visible="reportVisible"
      :width="720"
      title="迁移报告"
      :footer="false"
      @close="reportJobId = ''"
    >
      <MigrationReportPanel v-if="reportJobId" :jobID="reportJobId" />
    </a-drawer>
  </div>
</template>

<script setup lang="ts">
import { ref, reactive, computed, onUnmounted } from 'vue'
import { Message } from '@arco-design/web-vue'
import type { FileItem } from '@arco-design/web-vue'
import { getDbTypeColor, getDbTypeLabel } from '@/utils/dbType'
import {
  validateBatch,
  startBatch,
  listBatchJobs,
  cancelBatch,
  downloadBatchTemplate,
  type BatchValidateResult,
  type BatchOptions,
  type DataMigrationJob,
} from '@/api/migration'
import MigrationReportPanel from './MigrationReportPanel.vue'

const phase = ref<'upload' | 'progress'>('upload')
const selectedFile = ref<File | null>(null)
const validating = ref(false)
const starting = ref(false)
const validateResult = ref<BatchValidateResult | null>(null)

// 整批共用的迁移选项，默认值对齐单任务（MigrationView.vue）
const options = reactive<BatchOptions>({
  migrate_content: 'both',
  page_size: 20000,
  max_parallel: 10,
  intra_table_parallel: 8,
  lower_case_names: true,
  char_in_length: false,
  use_nvarchar2: false,
  distributed: false,
  change_owner: true,
  strip_view_schemas: '',
})

const batchId = ref('')
const batchStatus = ref<'running' | 'done' | 'cancelled'>('running')
const jobs = ref<DataMigrationJob[]>([])
let pollTimer: ReturnType<typeof setInterval> | null = null

const reportVisible = ref(false)
const reportJobId = ref('')

const finishedCount = computed(
  () => jobs.value.filter((j) => j.status !== 'running').length,
)
const batchStatusColor = computed(() =>
  batchStatus.value === 'running' ? 'blue' : batchStatus.value === 'done' ? 'green' : 'gray',
)
const batchStatusText = computed(() =>
  batchStatus.value === 'running' ? '执行中' : batchStatus.value === 'done' ? '已完成' : '已取消',
)

function handleFileChange(_: FileItem[], item: FileItem) {
  selectedFile.value = item.file ?? null
  validateResult.value = null
}

async function handleDownloadTemplate() {
  try {
    await downloadBatchTemplate()
  } catch (e: any) {
    Message.error(e?.message ?? '下载模板失败')
  }
}

async function handleValidate() {
  if (!selectedFile.value) return
  validating.value = true
  try {
    const { data } = await validateBatch(selectedFile.value)
    validateResult.value = data
  } catch (e: any) {
    Message.error(e?.response?.data?.error ?? '校验失败')
  } finally {
    validating.value = false
  }
}

async function handleStart() {
  if (!selectedFile.value || !validateResult.value) return
  starting.value = true
  try {
    // 不支持的行后端会自动排除，这里无需传 exclude_rows
    const { data } = await startBatch(selectedFile.value, [], options)
    batchId.value = data.batch_id
    batchStatus.value = 'running'
    phase.value = 'progress'
    jobs.value = []
    startPolling()
  } catch (e: any) {
    Message.error(e?.response?.data?.error ?? '启动失败')
  } finally {
    starting.value = false
  }
}

function startPolling() {
  stopPolling()
  const poll = async () => {
    try {
      const { data } = await listBatchJobs(batchId.value)
      jobs.value = data
      // 全部子任务结束 且 数量达到预期时停止轮询
      const allDone = data.length > 0 && data.every((j) => j.status !== 'running')
      if (allDone) {
        // 再拉一次批次状态由后端推进；这里简单按子任务推断
        batchStatus.value = data.some((j) => j.status === 'cancelled') ? 'cancelled' : 'done'
        stopPolling()
      }
    } catch {
      // 忽略瞬时错误，下次继续
    }
  }
  poll()
  pollTimer = setInterval(poll, 2000)
}

function stopPolling() {
  if (pollTimer) {
    clearInterval(pollTimer)
    pollTimer = null
  }
}

async function handleCancel() {
  try {
    await cancelBatch(batchId.value)
    batchStatus.value = 'cancelled'
    Message.success('已发送取消信号')
  } catch (e: any) {
    Message.error(e?.response?.data?.error ?? '取消失败')
  }
}

function openReport(jobId: string) {
  reportJobId.value = jobId
  reportVisible.value = true
}

function resetToUpload() {
  stopPolling()
  phase.value = 'upload'
  selectedFile.value = null
  validateResult.value = null
  jobs.value = []
  batchId.value = ''
}

onUnmounted(stopPolling)
</script>

<style scoped>
.batch-migration h2 {
  margin-bottom: 16px;
}
.step-card {
  background: #fff;
  border-radius: 8px;
}
.conn-text {
  color: #86909c;
  font-size: 12px;
  margin-left: 6px;
}
.reason-text {
  color: #f53f3f;
  font-size: 12px;
  margin-left: 6px;
}
</style>
