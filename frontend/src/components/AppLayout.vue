<template>
  <a-layout style="height: 100vh">
    <a-layout-sider
      :width="200"
      style="background: #fff"
      breakpoint="lg"
      :collapsed-width="0"
    >
      <div class="logo">
        <span>DBGold</span>
      </div>
      <a-menu
        :selected-keys="[currentRoute]"
        :default-open-keys="['schema']"
        mode="inline"
        @menu-item-click="navigate"
      >
        <a-menu-item key="/connections">
          <template #icon><icon-link /></template>
          连接管理
        </a-menu-item>
        <a-menu-item key="/schema">
          <template #icon><icon-storage /></template>
          Schema 提取
        </a-menu-item>
        <a-menu-item key="/diff">
          <template #icon><icon-swap /></template>
          Schema 对比
        </a-menu-item>
        <a-menu-item key="/migration">
          <template #icon><icon-thunderbolt /></template>
          迁移生成
        </a-menu-item>
        <a-menu-item key="/history">
          <template #icon><icon-history /></template>
          迁移历史
        </a-menu-item>
      </a-menu>
    </a-layout-sider>

    <a-layout>
      <a-layout-header style="background: #fff; padding: 0 24px; display: flex; align-items: center; justify-content: flex-end; border-bottom: 1px solid #f0f0f0">
        <a-dropdown>
          <a-avatar :size="32" style="cursor: pointer; background: #165dff">
            {{ userInitial }}
          </a-avatar>
          <template #content>
            <a-doption @click="handleLogout">退出登录</a-doption>
          </template>
        </a-dropdown>
      </a-layout-header>
      <a-layout-content style="padding: 24px; overflow: auto">
        <slot />
      </a-layout-content>
    </a-layout>
  </a-layout>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useRouter, useRoute } from 'vue-router'
import { useAuthStore } from '@/stores/auth'

const router = useRouter()
const route = useRoute()
const auth = useAuthStore()

const currentRoute = computed(() => route.path)
const userInitial = computed(() => auth.user?.username?.[0]?.toUpperCase() ?? 'U')

function navigate(key: string) {
  router.push(key)
}

function handleLogout() {
  auth.logout()
  router.push('/login')
}
</script>

<style scoped>
.logo {
  height: 64px;
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 18px;
  font-weight: bold;
  color: #165dff;
  border-bottom: 1px solid #f0f0f0;
}
</style>
