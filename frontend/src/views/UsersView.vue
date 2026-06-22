<template>
  <div>
    <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 16px">
      <h2 style="margin: 0">用户管理</h2>
      <a-button type="primary" @click="openCreate">
        <template #icon><icon-plus /></template>
        新建用户
      </a-button>
    </div>

    <a-table :data="users" :loading="loading" row-key="id" :pagination="false">
      <template #columns>
        <a-table-column title="#" data-index="id" :width="80" />
        <a-table-column title="用户名" data-index="username" />
        <a-table-column title="角色" :width="120">
          <template #cell="{ record }">
            <a-tag :color="record.role === 'admin' ? 'orangered' : 'blue'" size="small">
              {{ record.role === 'admin' ? '管理员' : '普通用户' }}
            </a-tag>
          </template>
        </a-table-column>
        <a-table-column title="状态" :width="120">
          <template #cell="{ record }">
            <a-switch
              :model-value="record.enabled"
              :loading="togglingId === record.id"
              :disabled="record.id === auth.user?.id"
              @change="(val) => handleToggle(record, val as boolean)"
            >
              <template #checked>启用</template>
              <template #unchecked>禁用</template>
            </a-switch>
          </template>
        </a-table-column>
        <a-table-column title="创建时间" data-index="created_at">
          <template #cell="{ record }">
            {{ formatDate(record.created_at) }}
          </template>
        </a-table-column>
        <a-table-column title="操作" :width="140">
          <template #cell="{ record }">
            <a-button size="small" @click="openResetPassword(record)">重置密码</a-button>
          </template>
        </a-table-column>
      </template>
    </a-table>

    <a-modal
      v-model:visible="createVisible"
      title="新建用户"
      :mask-closable="false"
      :ok-loading="submitting"
      @before-ok="handleCreate"
      @cancel="createVisible = false"
    >
      <a-form :model="createForm" layout="vertical" ref="createFormRef">
        <a-form-item label="用户名" field="username" :rules="[{ required: true, message: '请输入用户名' }]">
          <a-input v-model="createForm.username" placeholder="用户名" />
        </a-form-item>
        <a-form-item
          label="密码"
          field="password"
          :rules="[{ required: true, message: '请输入密码' }, { minLength: 6, message: '密码至少 6 位' }]"
        >
          <a-input-password v-model="createForm.password" placeholder="至少 6 位" />
        </a-form-item>
        <a-form-item label="角色" field="role" :rules="[{ required: true }]">
          <a-select v-model="createForm.role" placeholder="选择角色">
            <a-option value="user">普通用户</a-option>
            <a-option value="admin">管理员</a-option>
          </a-select>
        </a-form-item>
      </a-form>
    </a-modal>

    <a-modal
      v-model:visible="resetVisible"
      :title="`重置密码 - ${resetTarget?.username ?? ''}`"
      :mask-closable="false"
      :ok-loading="submitting"
      @before-ok="handleReset"
      @cancel="resetVisible = false"
    >
      <a-form :model="resetForm" layout="vertical" ref="resetFormRef">
        <a-form-item
          label="新密码"
          field="password"
          :rules="[{ required: true, message: '请输入新密码' }, { minLength: 6, message: '密码至少 6 位' }]"
        >
          <a-input-password v-model="resetForm.password" placeholder="至少 6 位" />
        </a-form-item>
      </a-form>
    </a-modal>
  </div>
</template>

<script setup lang="ts">
import { ref, reactive, onMounted } from 'vue'
import { Message } from '@arco-design/web-vue'
import { listUsers, createUser, updateUser, type User } from '@/api/users'
import { useAuthStore } from '@/stores/auth'

const auth = useAuthStore()
const users = ref<User[]>([])
const loading = ref(false)
const togglingId = ref<number | null>(null)
const submitting = ref(false)

const createVisible = ref(false)
const createFormRef = ref<{ validate: () => Promise<Record<string, unknown> | undefined> } | null>(null)
const createForm = reactive({ username: '', password: '', role: 'user' })

const resetVisible = ref(false)
const resetTarget = ref<User | null>(null)
const resetFormRef = ref<{ validate: () => Promise<Record<string, unknown> | undefined> } | null>(null)
const resetForm = reactive({ password: '' })

async function load() {
  loading.value = true
  try {
    const res = await listUsers()
    users.value = res.data
  } catch {
    Message.error('加载用户列表失败')
  } finally {
    loading.value = false
  }
}

function openCreate() {
  Object.assign(createForm, { username: '', password: '', role: 'user' })
  createVisible.value = true
}

async function handleCreate(done: (closed: boolean) => void) {
  const invalid = await createFormRef.value?.validate()
  if (invalid) {
    done(false)
    return
  }
  submitting.value = true
  try {
    await createUser(createForm)
    Message.success('创建成功')
    done(true)
    await load()
  } catch (e: any) {
    const detail = e?.response?.data?.error
    Message.error(detail === 'username already exists' ? '用户名已存在' : detail || '创建失败')
    done(false)
  } finally {
    submitting.value = false
  }
}

function openResetPassword(user: User) {
  resetTarget.value = user
  resetForm.password = ''
  resetVisible.value = true
}

async function handleReset(done: (closed: boolean) => void) {
  const invalid = await resetFormRef.value?.validate()
  if (invalid || !resetTarget.value) {
    done(false)
    return
  }
  submitting.value = true
  try {
    await updateUser(resetTarget.value.id, { password: resetForm.password })
    Message.success('密码已重置')
    done(true)
  } catch {
    Message.error('重置失败')
    done(false)
  } finally {
    submitting.value = false
  }
}

async function handleToggle(user: User, enabled: boolean) {
  togglingId.value = user.id
  try {
    await updateUser(user.id, { enabled })
    user.enabled = enabled
    Message.success(enabled ? '已启用' : '已禁用')
  } catch (e: any) {
    const detail = e?.response?.data?.error
    Message.error(detail || '操作失败')
    await load()
  } finally {
    togglingId.value = null
  }
}

function formatDate(s: string) {
  if (!s) return '-'
  return new Date(s).toLocaleString('zh-CN', { hour12: false })
}

onMounted(load)
</script>
