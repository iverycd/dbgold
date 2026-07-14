<template>
  <div>
    <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 16px">
      <h2 style="margin: 0">工单管理</h2>
      <a-button @click="loadTickets">
        <template #icon><icon-refresh /></template>
        刷新
      </a-button>
    </div>

    <a-table :data="tickets" :loading="loading" row-key="id" :pagination="{ pageSize: 20 }">
      <template #columns>
        <a-table-column title="ID" data-index="id" :width="70" />
        <a-table-column title="申请人" data-index="applicant" :width="120">
          <template #cell="{ record }">{{ record.applicant || '-' }}</template>
        </a-table-column>
        <a-table-column title="源库" :width="240">
          <template #cell="{ record }">
            <a-tag :color="getDbTypeColor(record.src_db_type)" size="small">{{ getDbTypeLabel(record.src_db_type) }}</a-tag>
            <span v-if="record.src_file_path" class="conn-text">
              <icon-file /> {{ record.src_file_name }}（{{ formatBytes(record.src_file_size) }}）
            </span>
            <span v-else class="conn-text">{{ connText(record.src_host, record.src_port, record.src_database) }}</span>
          </template>
        </a-table-column>
        <a-table-column title="目标库" :width="220">
          <template #cell="{ record }">
            <a-tag :color="getDbTypeColor(record.dst_db_type)" size="small">{{ getDbTypeLabel(record.dst_db_type) }}</a-tag>
            <span class="conn-text">{{ connText(record.dst_host, record.dst_port, record.dst_database) }}</span>
          </template>
        </a-table-column>
        <a-table-column title="状态" :width="100">
          <template #cell="{ record }">
            <a-tag :color="statusColor(record.status)" size="small">{{ statusLabel(record.status) }}</a-tag>
          </template>
        </a-table-column>
        <a-table-column title="提交时间" :width="170">
          <template #cell="{ record }">{{ formatTime(record.created_at) }}</template>
        </a-table-column>
        <a-table-column title="操作" :width="200">
          <template #cell="{ record }">
            <a-space>
              <a-button size="small" @click="openDetail(record.id)">详情</a-button>
              <a-button size="small" type="primary" @click="openProcess(record)">处理</a-button>
              <a-popconfirm v-if="isAdmin" content="确认删除此工单？" @ok="handleDelete(record.id)">
                <a-button size="small" status="danger">删除</a-button>
              </a-popconfirm>
            </a-space>
          </template>
        </a-table-column>
      </template>
    </a-table>

    <!-- 详情抽屉 -->
    <a-drawer
      v-model:visible="detailVisible"
      title="工单详情"
      :width="480"
      :footer="false"
      @close="closeDetail"
    >
      <a-spin :loading="detailLoading" style="width: 100%">
        <template v-if="detail">
          <!-- 顶部操作：只读态显示编辑，编辑态显示保存 / 取消 -->
          <div class="detail-actions">
            <template v-if="!editing">
              <a-space>
                <a-tooltip
                  v-if="detail.src_file_path"
                  content="源库为离线文件，无法自动创建连接，请手动处理"
                >
                  <a-button type="primary" size="small" disabled>
                    <template #icon><icon-swap /></template>
                    一键迁移
                  </a-button>
                </a-tooltip>
                <a-button v-else type="primary" size="small" :loading="migrating" @click="handleMigrate">
                  <template #icon><icon-swap /></template>
                  一键迁移
                </a-button>
                <a-button size="small" @click="startEdit">
                  <template #icon><icon-edit /></template>
                  编辑
                </a-button>
              </a-space>
            </template>
            <template v-else>
              <a-space>
                <a-button type="primary" size="small" :loading="saving" @click="handleSave">保存</a-button>
                <a-button size="small" @click="cancelEdit">取消</a-button>
              </a-space>
            </template>
          </div>

          <!-- ===== 只读展示 ===== -->
          <template v-if="!editing">
            <a-descriptions :column="1" bordered size="medium" title="基本信息">
              <a-descriptions-item label="申请人">{{ detail.applicant || '-' }}</a-descriptions-item>
              <a-descriptions-item label="需求说明">{{ detail.remark || '-' }}</a-descriptions-item>
              <a-descriptions-item label="状态">
                <a-tag :color="statusColor(detail.status)" size="small">{{ statusLabel(detail.status) }}</a-tag>
              </a-descriptions-item>
              <a-descriptions-item label="处理备注">{{ detail.admin_note || '-' }}</a-descriptions-item>
              <a-descriptions-item label="提交 IP">{{ detail.client_ip || '-' }}</a-descriptions-item>
              <a-descriptions-item label="提交时间">{{ formatTime(detail.created_at) }}</a-descriptions-item>
            </a-descriptions>

            <a-descriptions :column="1" bordered size="medium" title="源数据库" style="margin-top: 16px">
              <a-descriptions-item label="类型">{{ getDbTypeLabel(detail.src_db_type) }}</a-descriptions-item>
              <template v-if="detail.src_file_path">
                <a-descriptions-item label="提供方式">离线文件</a-descriptions-item>
                <a-descriptions-item label="文件名">{{ detail.src_file_name }}</a-descriptions-item>
                <a-descriptions-item label="文件大小">{{ formatBytes(detail.src_file_size) }}</a-descriptions-item>
                <a-descriptions-item label="落盘路径">
                  <span class="path-text">{{ detail.src_file_path }}</span>
                </a-descriptions-item>
              </template>
              <template v-else>
                <a-descriptions-item label="主机:端口">{{ detail.src_host }}:{{ detail.src_port }}</a-descriptions-item>
                <a-descriptions-item label="数据库名">{{ detail.src_database || '-' }}</a-descriptions-item>
                <a-descriptions-item label="用户名">{{ detail.src_username }}</a-descriptions-item>
                <a-descriptions-item label="密码">
                  <span>{{ showSrcPwd ? (detail.src_password || '-') : '••••••••' }}</span>
                  <a-link style="margin-left: 8px" @click="showSrcPwd = !showSrcPwd">{{ showSrcPwd ? '隐藏' : '显示' }}</a-link>
                </a-descriptions-item>
              </template>
            </a-descriptions>

            <a-descriptions :column="1" bordered size="medium" title="目标数据库" style="margin-top: 16px">
              <a-descriptions-item label="类型">{{ getDbTypeLabel(detail.dst_db_type) }}</a-descriptions-item>
              <a-descriptions-item label="主机:端口">{{ detail.dst_host }}:{{ detail.dst_port }}</a-descriptions-item>
              <a-descriptions-item label="数据库名">{{ detail.dst_database || '-' }}</a-descriptions-item>
              <a-descriptions-item label="用户名">{{ detail.dst_username }}</a-descriptions-item>
              <a-descriptions-item label="密码">
                <span>{{ showDstPwd ? (detail.dst_password || '-') : '••••••••' }}</span>
                <a-link style="margin-left: 8px" @click="showDstPwd = !showDstPwd">{{ showDstPwd ? '隐藏' : '显示' }}</a-link>
              </a-descriptions-item>
            </a-descriptions>
          </template>

          <!-- ===== 编辑表单 ===== -->
          <a-form v-else :model="editForm" layout="vertical">
            <div class="form-section-title">基本信息</div>
            <a-form-item label="申请人">
              <a-input v-model="editForm.applicant" placeholder="申请人（联系方式，选填）" />
            </a-form-item>
            <a-form-item label="需求说明">
              <a-textarea v-model="editForm.remark" placeholder="需求说明（选填）" :auto-size="{ minRows: 2, maxRows: 4 }" />
            </a-form-item>

            <div class="form-section-title">源数据库</div>
            <template v-if="detail.src_file_path">
              <div class="offline-hint">源库为离线文件（{{ detail.src_file_name }}），连接信息不可编辑。</div>
            </template>
            <template v-else>
              <a-form-item label="类型">
                <a-select v-model="editForm.src_db_type">
                  <a-option v-for="opt in dbTypeOptions" :key="opt.value" :value="opt.value">{{ opt.label }}</a-option>
                </a-select>
              </a-form-item>
              <a-form-item label="主机">
                <a-input v-model="editForm.src_host" placeholder="主机地址" />
              </a-form-item>
              <a-form-item label="端口">
                <a-input-number v-model="editForm.src_port" :min="0" :max="65535" placeholder="端口" style="width: 100%" />
              </a-form-item>
              <a-form-item label="数据库名">
                <a-input v-model="editForm.src_database" placeholder="数据库名（选填）" />
              </a-form-item>
              <a-form-item label="用户名">
                <a-input v-model="editForm.src_username" placeholder="用户名" />
              </a-form-item>
              <a-form-item label="密码">
                <a-input-password v-model="editForm.src_password" placeholder="密码" />
              </a-form-item>
            </template>

            <div class="form-section-title">目标数据库</div>
            <a-form-item label="类型">
              <a-select v-model="editForm.dst_db_type">
                <a-option v-for="opt in dbTypeOptions" :key="opt.value" :value="opt.value">{{ opt.label }}</a-option>
              </a-select>
            </a-form-item>
            <a-form-item label="主机">
              <a-input v-model="editForm.dst_host" placeholder="主机地址" />
            </a-form-item>
            <a-form-item label="端口">
              <a-input-number v-model="editForm.dst_port" :min="0" :max="65535" placeholder="端口" style="width: 100%" />
            </a-form-item>
            <a-form-item label="数据库名">
              <a-input v-model="editForm.dst_database" placeholder="数据库名（选填）" />
            </a-form-item>
            <a-form-item label="用户名">
              <a-input v-model="editForm.dst_username" placeholder="用户名" />
            </a-form-item>
            <a-form-item label="密码">
              <a-input-password v-model="editForm.dst_password" placeholder="密码" />
            </a-form-item>
          </a-form>
        </template>
      </a-spin>
    </a-drawer>

    <!-- 处理弹窗 -->
    <a-modal
      v-model:visible="processVisible"
      title="处理工单"
      :mask-closable="false"
      @before-ok="handleProcess"
      @cancel="processVisible = false"
      :ok-loading="processing"
    >
      <a-form :model="processForm" layout="vertical">
        <a-form-item label="状态">
          <a-select v-model="processForm.status">
            <a-option value="pending">待处理</a-option>
            <a-option value="processed">已处理</a-option>
            <a-option value="rejected">已驳回</a-option>
          </a-select>
        </a-form-item>
        <a-form-item label="处理备注">
          <a-textarea v-model="processForm.admin_note" placeholder="处理结果说明（选填）" :auto-size="{ minRows: 2, maxRows: 4 }" />
        </a-form-item>
      </a-form>
    </a-modal>
  </div>
</template>

<script setup lang="ts">
import { ref, reactive, computed, onMounted } from 'vue'
import { useRouter } from 'vue-router'
import { Message } from '@arco-design/web-vue'
import {
  listTickets,
  getTicket,
  updateTicket,
  updateTicketInfo,
  createTicketConnections,
  deleteTicket,
  type Ticket,
  type TicketDetail,
  type TicketInfoForm,
} from '@/api/tickets'
import { getDbTypeColor, getDbTypeLabel } from '@/utils/dbType'
import { useAuthStore } from '@/stores/auth'

const router = useRouter()

// 删除工单仅限 admin;普通用户只读 + 处理。
const auth = useAuthStore()
const isAdmin = computed(() => auth.user?.role === 'admin')

// 数据库类型下拉选项，取值与连接管理（ConnectionsView）保持一致。
const dbTypeOptions = [
  { value: 'mysql', label: 'MySQL' },
  { value: 'postgres', label: 'PostgreSQL' },
  { value: 'oracle', label: 'Oracle' },
  { value: 'sqlserver', label: 'SQL Server' },
  { value: 'gaussdb', label: 'GaussDB' },
  { value: 'dameng', label: 'DaMeng（达梦）' },
  { value: 'seabox', label: 'SeaBox' },
  { value: 'highgo', label: 'HighGo（瀚高）' },
  { value: 'kingbase', label: 'Kingbase（人大金仓）' },
]

const tickets = ref<Ticket[]>([])
const loading = ref(false)

const detailVisible = ref(false)
const detailLoading = ref(false)
const detail = ref<TicketDetail | null>(null)
const showSrcPwd = ref(false)
const showDstPwd = ref(false)

// 详情抽屉内的编辑态
const editing = ref(false)
const saving = ref(false)
const migrating = ref(false)
const editForm = reactive<TicketInfoForm>({
  applicant: '',
  remark: '',
  src_db_type: '',
  src_host: '',
  src_port: 0,
  src_database: '',
  src_username: '',
  src_password: '',
  dst_db_type: '',
  dst_host: '',
  dst_port: 0,
  dst_database: '',
  dst_username: '',
  dst_password: '',
})

const processVisible = ref(false)
const processing = ref(false)
const processId = ref<number | null>(null)
const processForm = reactive({ status: 'pending', admin_note: '' })

function connText(host: string, port: number, database: string) {
  return ` ${host}:${port}${database ? '/' + database : ''}`
}

function statusLabel(s: string) {
  return { pending: '待处理', processed: '已处理', rejected: '已驳回' }[s] ?? s
}
function statusColor(s: string) {
  return { pending: 'orange', processed: 'green', rejected: 'red' }[s] ?? 'gray'
}
function formatTime(t: string) {
  return t ? new Date(t).toLocaleString('zh-CN') : '-'
}
function formatBytes(bytes: number): string {
  if (!bytes || bytes <= 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.floor(Math.log(bytes) / Math.log(1024))
  return `${(bytes / Math.pow(1024, i)).toFixed(i === 0 ? 0 : 2)} ${units[i]}`
}

async function loadTickets() {
  loading.value = true
  try {
    const res = await listTickets()
    tickets.value = res.data
  } catch {
    Message.error('加载工单列表失败')
  } finally {
    loading.value = false
  }
}

async function openDetail(id: number) {
  detailVisible.value = true
  detailLoading.value = true
  detail.value = null
  showSrcPwd.value = false
  showDstPwd.value = false
  editing.value = false
  try {
    const res = await getTicket(id)
    detail.value = res.data
  } catch {
    Message.error('加载工单详情失败')
  } finally {
    detailLoading.value = false
  }
}

// closeDetail 抽屉关闭时清空状态，避免下次打开残留编辑态 / 旧数据。
function closeDetail() {
  detail.value = null
  editing.value = false
}

// startEdit 进入编辑态：从详情拷贝当前值到表单（密码用已返回的明文预填）。
function startEdit() {
  if (!detail.value) return
  const d = detail.value
  editForm.applicant = d.applicant
  editForm.remark = d.remark
  editForm.src_db_type = d.src_db_type
  editForm.src_host = d.src_host
  editForm.src_port = d.src_port
  editForm.src_database = d.src_database
  editForm.src_username = d.src_username
  editForm.src_password = d.src_password || ''
  editForm.dst_db_type = d.dst_db_type
  editForm.dst_host = d.dst_host
  editForm.dst_port = d.dst_port
  editForm.dst_database = d.dst_database
  editForm.dst_username = d.dst_username
  editForm.dst_password = d.dst_password || ''
  editing.value = true
}

function cancelEdit() {
  editing.value = false
}

// handleMigrate 一键迁移：用工单信息建源/目标连接，跳转到迁移页并预选。
async function handleMigrate() {
  if (!detail.value) return
  migrating.value = true
  try {
    const res = await createTicketConnections(detail.value.id)
    router.push({
      path: '/migration',
      query: {
        src: res.data.src_conn_id,
        dst: res.data.dst_conn_id,
        srcdb: detail.value.src_database || undefined,
      },
    })
  } catch (e: any) {
    Message.error(e?.response?.data?.error ?? '创建连接失败')
  } finally {
    migrating.value = false
  }
}

// handleSave 保存连接基础信息，成功后退出编辑态、刷新详情与列表。
async function handleSave() {
  if (!detail.value) return
  saving.value = true
  try {
    await updateTicketInfo(detail.value.id, { ...editForm })
    Message.success('保存成功')
    editing.value = false
    const res = await getTicket(detail.value.id)
    detail.value = res.data
    await loadTickets()
  } catch {
    Message.error('保存失败')
  } finally {
    saving.value = false
  }
}

function openProcess(record: Ticket) {
  processId.value = record.id
  processForm.status = record.status
  processForm.admin_note = record.admin_note || ''
  processVisible.value = true
}

async function handleProcess(done: (closed: boolean) => void) {
  if (processId.value === null) {
    done(true)
    return
  }
  processing.value = true
  try {
    await updateTicket(processId.value, { status: processForm.status, admin_note: processForm.admin_note })
    Message.success('处理成功')
    done(true)
    await loadTickets()
  } catch {
    Message.error('处理失败')
    done(false)
  } finally {
    processing.value = false
  }
}

async function handleDelete(id: number) {
  try {
    await deleteTicket(id)
    Message.success('删除成功')
    await loadTickets()
  } catch {
    Message.error('删除失败')
  }
}

onMounted(loadTickets)
</script>

<style scoped>
.conn-text {
  margin-left: 4px;
  font-size: 12px;
  color: #64748b;
}
.path-text {
  font-size: 12px;
  color: #64748b;
  word-break: break-all;
}
.detail-actions {
  display: flex;
  justify-content: flex-end;
  margin-bottom: 16px;
}
.form-section-title {
  font-weight: 600;
  font-size: 14px;
  margin: 8px 0 12px;
  color: #1d2129;
}
.offline-hint {
  font-size: 13px;
  color: #86909c;
  margin-bottom: 12px;
}
</style>
