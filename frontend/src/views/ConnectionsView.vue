<template>
  <div>
    <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 16px">
      <h2 style="margin: 0">连接管理</h2>
      <a-button type="primary" @click="openCreate">
        <template #icon><icon-plus /></template>
        新建连接
      </a-button>
    </div>

    <a-table :data="connections" :loading="loading" row-key="id" :pagination="false">
      <template #columns>
        <a-table-column title="名称" data-index="name" />
        <a-table-column title="类型" data-index="db_type" :width="100">
          <template #cell="{ record }">
            <a-tag :color="getDbTypeColor(record.db_type)" size="small">{{ getDbTypeLabel(record.db_type) }}</a-tag>
          </template>
        </a-table-column>
        <a-table-column title="主机" data-index="host" />
        <a-table-column title="端口" data-index="port" :width="80" />
        <a-table-column title="数据库" data-index="database" />
        <a-table-column v-if="isAdmin" title="所属用户" data-index="owner_username" :width="120">
          <template #cell="{ record }">
            <a-tag size="small" color="arcoblue">{{ record.owner_username || '-' }}</a-tag>
          </template>
        </a-table-column>
        <a-table-column title="操作" :width="200">
          <template #cell="{ record }">
            <a-space>
              <a-button size="small" @click="openEdit(record)">编辑</a-button>
              <a-button size="small" status="success" :loading="testingId === record.id" @click="handleTest(record)">测试</a-button>
              <a-popconfirm content="确认删除此连接？" @ok="handleDelete(record.id)">
                <a-button size="small" status="danger">删除</a-button>
              </a-popconfirm>
            </a-space>
          </template>
        </a-table-column>
      </template>
    </a-table>

    <a-modal
      v-model:visible="modalVisible"
      :title="editingId ? '编辑连接' : '新建连接'"
      :mask-closable="false"
      @before-ok="handleSubmit"
      @cancel="modalVisible = false"
      :ok-loading="submitting"
    >
      <a-form :model="form" layout="vertical" ref="formRef">
        <a-form-item label="名称" field="name" :rules="[{ required: true, message: '请输入名称' }]">
          <a-input v-model="form.name" placeholder="连接名称" />
        </a-form-item>
        <a-form-item label="数据库类型" field="db_type" :rules="[{ required: true }]">
          <a-select v-model="form.db_type" placeholder="选择数据库类型">
            <a-option value="mysql">MySQL</a-option>
            <a-option value="postgres">PostgreSQL</a-option>
            <a-option value="oracle">Oracle</a-option>
            <a-option value="sqlserver">SQL Server</a-option>
            <a-option value="gaussdb">GaussDB</a-option>
            <a-option value="dameng">DaMeng（达梦）</a-option>
            <a-option value="seabox">SeaBox</a-option>
            <a-option value="highgo">HighGo（瀚高）</a-option>
          </a-select>
        </a-form-item>
        <a-row :gutter="12">
          <a-col :span="16">
            <a-form-item label="主机" field="host" :rules="[{ required: true }]">
              <a-input v-model="form.host" placeholder="localhost" />
            </a-form-item>
          </a-col>
          <a-col :span="8">
            <a-form-item label="端口" field="port" :rules="[{ required: true }]">
              <a-input-number v-model="form.port" :min="1" :max="65535" style="width: 100%" />
            </a-form-item>
          </a-col>
        </a-row>
        <a-form-item label="数据库名" field="database" :rules="[{ required: form.db_type !== 'mysql' && form.db_type !== 'dameng', message: '请输入数据库名' }]">
          <a-input v-model="form.database" placeholder="数据库名" />
        </a-form-item>
        <a-form-item label="用户名" field="username" :rules="[{ required: true }]">
          <a-input v-model="form.username" placeholder="用户名" />
        </a-form-item>
        <a-form-item label="密码" field="password" :rules="editingId ? [] : [{ required: true }]">
          <a-input-password v-model="form.password" :placeholder="editingId ? '留空不修改' : '密码'" />
        </a-form-item>
      </a-form>
    </a-modal>
  </div>
</template>

<script setup lang="ts">
import { ref, reactive, onMounted, watch, computed } from 'vue'
import { Message } from '@arco-design/web-vue'
import {
  listConnections,
  createConnection,
  updateConnection,
  deleteConnection,
  testConnection,
  type Connection,
} from '@/api/connections'
import { getDbTypeColor, getDbTypeLabel } from '@/utils/dbType'
import { useAuthStore } from '@/stores/auth'

const auth = useAuthStore()
const isAdmin = computed(() => auth.user?.role === 'admin')
const connections = ref<Connection[]>([])
const loading = ref(false)
const testingId = ref<number | null>(null)
const modalVisible = ref(false)
const submitting = ref(false)
const editingId = ref<number | null>(null)
const formRef = ref<{ validate: () => Promise<Record<string, unknown> | undefined> } | null>(null)

const defaultForm = () => ({
  name: '',
  db_type: 'mysql',
  host: 'localhost',
  port: 3306,
  database: '',
  username: '',
  password: '',
})

const form = reactive(defaultForm())

const defaultPortMap: Record<string, number> = {
  mysql: 3306,
  postgres: 5432,
  oracle: 1521,
  sqlserver: 1433,
  gaussdb: 5432,
  dameng: 5236,
  seabox: 5432,
  highgo: 5866,
}

watch(() => form.db_type, (newType) => {
  if (editingId.value === null) {
    form.port = defaultPortMap[newType] ?? 3306
  }
})

async function loadConnections() {
  loading.value = true
  try {
    const res = await listConnections()
    connections.value = res.data
  } catch {
    Message.error('加载连接列表失败')
  } finally {
    loading.value = false
  }
}

function openCreate() {
  editingId.value = null
  Object.assign(form, defaultForm())
  modalVisible.value = true
}

function openEdit(conn: Connection) {
  editingId.value = conn.id
  Object.assign(form, {
    name: conn.name,
    db_type: conn.db_type,
    host: conn.host,
    port: conn.port,
    database: conn.database,
    username: conn.username,
    password: '',
  })
  modalVisible.value = true
}

async function handleSubmit(done: (closed: boolean) => void) {
  const valid = await formRef.value?.validate()
  if (valid) {
    done(false)
    return
  }
  submitting.value = true
  try {
    if (editingId.value) {
      await updateConnection(editingId.value, form)
      Message.success('更新成功')
    } else {
      await createConnection(form)
      Message.success('创建成功')
    }
    done(true)
    await loadConnections()
  } catch {
    Message.error('操作失败')
    done(false)
  } finally {
    submitting.value = false
  }
}

async function handleTest(conn: Connection) {
  testingId.value = conn.id
  try {
    await testConnection(conn.id)
    Message.success('连接成功')
  } catch (e: any) {
    const detail = e?.response?.data?.error
    Message.error(detail ? `连接失败：${detail}` : '连接失败')
  } finally {
    testingId.value = null
  }
}

async function handleDelete(id: number) {
  try {
    await deleteConnection(id)
    Message.success('删除成功')
    await loadConnections()
  } catch {
    Message.error('删除失败')
  }
}

onMounted(loadConnections)
</script>