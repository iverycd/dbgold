<template>
  <div class="login-container">
    <a-card class="login-card" title="DBGold 数据库迁移工具">
      <a-form :model="form" @submit="handleSubmit" layout="vertical">
        <a-form-item label="用户名" field="username" :rules="[{ required: true }]">
          <a-input v-model="form.username" placeholder="请输入用户名" allow-clear />
        </a-form-item>
        <a-form-item label="密码" field="password" :rules="[{ required: true }]">
          <a-input-password v-model="form.password" placeholder="请输入密码" />
        </a-form-item>
        <a-form-item>
          <a-button
            type="primary"
            html-type="submit"
            long
            :loading="loading"
          >
            登录
          </a-button>
        </a-form-item>
      </a-form>
    </a-card>
  </div>
</template>

<script setup lang="ts">
import { reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { Message } from '@arco-design/web-vue'
import { useAuthStore } from '@/stores/auth'

const router = useRouter()
const auth = useAuthStore()
const loading = ref(false)
const form = reactive({ username: '', password: '' })

async function handleSubmit() {
  loading.value = true
  try {
    await auth.login(form.username, form.password)
    router.push('/connections')
  } catch {
    Message.error('用户名或密码错误')
  } finally {
    loading.value = false
  }
}
</script>

<style scoped>
.login-container {
  display: flex;
  align-items: center;
  justify-content: center;
  min-height: 100vh;
  background: #f0f2f5;
}
.login-card {
  width: 380px;
}
</style>
