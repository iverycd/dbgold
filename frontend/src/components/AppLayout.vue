<template>
  <a-layout class="app-layout">
    <a-layout-sider class="sider" :width="220" breakpoint="lg" :collapsed-width="0">
      <div class="logo">
        <span class="logo-title">DBGold</span>
        <span class="logo-subtitle">基础设施研发部</span>
      </div>

      <a-menu
        :selected-keys="[currentRoute]"
        mode="inline"
        class="nav-menu"
        @menu-item-click="navigate"
      >
        <a-menu-item key="/connections">
          <template #icon><icon-link /></template>
          连接管理
        </a-menu-item>
        <a-menu-item key="/migration">
          <template #icon><icon-thunderbolt /></template>
          迁移生成
        </a-menu-item>
        <a-menu-item key="/batch-migration">
          <template #icon><icon-import /></template>
          批量迁移
        </a-menu-item>
        <a-menu-item key="/history">
          <template #icon><icon-history /></template>
          迁移历史
        </a-menu-item>
        <a-menu-item v-if="auth.user?.role === 'admin'" key="/users">
          <template #icon><icon-user-group /></template>
          用户管理
        </a-menu-item>
        <a-menu-item key="/tickets">
          <template #icon><icon-file /></template>
          工单管理
        </a-menu-item>
        <a-menu-item v-if="auth.user?.role === 'admin'" key="/login-history">
          <template #icon><icon-user /></template>
          登录历史
        </a-menu-item>
      </a-menu>

      <a-tooltip v-if="versionInfo" position="right">
        <template #content>
          commit: {{ versionInfo.git_commit }}<br />
          构建: {{ buildTimeLocal }}
        </template>
        <div class="sider-version">{{ versionInfo.version }}</div>
      </a-tooltip>
    </a-layout-sider>

    <a-layout>
      <a-layout-header class="app-header">
        <span class="page-title">{{ pageTitle }}</span>

        <div class="header-right">
          <div class="header-status">
            <span class="status-dot" aria-hidden="true"></span>
            <span class="status-label">在线</span>
          </div>

          <div class="header-divider" aria-hidden="true"></div>

          <div class="header-user">
            <a-avatar :size="28" class="user-avatar-sm">{{ userInitial }}</a-avatar>
            <span class="header-username">{{ auth.user?.username }}</span>
            <button
              class="logout-btn"
              aria-label="退出登录"
              title="退出登录"
              @click="handleLogout"
            >
              <icon-export />
            </button>
          </div>
        </div>
      </a-layout-header>

      <a-layout-content class="main-content">
        <slot />
      </a-layout-content>
    </a-layout>
  </a-layout>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRouter, useRoute } from 'vue-router'
import { useAuthStore } from '@/stores/auth'
import { getVersion, type VersionInfo } from '@/api/system'

const router = useRouter()
const route = useRoute()
const auth = useAuthStore()

const versionInfo = ref<VersionInfo | null>(null)
onMounted(async () => {
  try {
    const { data } = await getVersion()
    versionInfo.value = data
  } catch {
    versionInfo.value = { version: 'dev', git_commit: 'unknown', build_time: 'unknown' }
  }
})

// 后端 build_time 是 UTC（ISO，带 Z），这里转成北京时间展示
const buildTimeLocal = computed(() => {
  const raw = versionInfo.value?.build_time
  if (!raw || raw === 'unknown') return raw ?? ''
  const d = new Date(raw)
  if (isNaN(d.getTime())) return raw
  return d.toLocaleString('zh-CN', {
    timeZone: 'Asia/Shanghai',
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
  })
})

const PAGE_TITLES: Record<string, string> = {
  '/connections': '连接管理',
  '/migration': '迁移生成',
  '/batch-migration': '批量迁移',
  '/history': '迁移历史',
  '/users': '用户管理',
  '/tickets': '工单管理',
  '/login-history': '登录历史',
}

const currentRoute = computed(() => route.path)
const userInitial = computed(() => auth.user?.username?.[0]?.toUpperCase() ?? 'U')
const pageTitle = computed(() => PAGE_TITLES[route.path] ?? 'DBGold')

function navigate(key: string) {
  router.push(key)
}

function handleLogout() {
  auth.logout()
  router.push('/login')
}
</script>

<style scoped>
.app-layout {
  height: 100vh;
  background: #F1F5F9;
}

/* ─── Sider（深色保留）─── */
.sider {
  background: #1E293B !important;
  border-right: none !important;
  box-shadow: 2px 0 8px rgba(15, 23, 42, 0.12);
}

/* sider 内容区设为纵向 flex，使版本块贴底 */
.sider :deep(.arco-layout-sider-children) {
  display: flex;
  flex-direction: column;
  height: 100%;
}

.logo {
  height: 92px;
  display: flex;
  align-items: center;
  justify-content: center;
  flex-direction: column;
  gap: 3px;
  border-bottom: 1px solid rgba(255, 255, 255, 0.06);
  flex-shrink: 0;
}
.logo-title {
  font-family: 'Fira Code', monospace;
  font-size: 26px;
  font-weight: 700;
  letter-spacing: 2px;
  background: linear-gradient(90deg,
    #f59e0b, #ef4444, #ec4899,
    #8b5cf6, #3b82f6, #06b6d4, #22C55E
  );
  -webkit-background-clip: text;
  -webkit-text-fill-color: transparent;
  background-clip: text;
}
.logo-subtitle {
  font-family: 'Inter', sans-serif;
  font-size: 13px;
  font-weight: 500;
  color: rgba(203, 213, 225, 0.82);
  letter-spacing: 1px;
}

.nav-menu {
  flex: 1;
  padding: 8px 0;
  background: transparent !important;
  border: none !important;
  overflow-y: auto;
}

.sider-version {
  flex-shrink: 0;
  padding: 12px 0;
  text-align: center;
  font-family: 'Fira Code', monospace;
  font-size: 12px;
  color: rgba(203, 213, 225, 0.6);
  border-top: 1px solid rgba(255, 255, 255, 0.06);
  cursor: default;
}

/* ─── Header（浅色）─── */
.app-header {
  background: #FFFFFF !important;
  border-bottom: 1px solid #E2E8F0 !important;
  box-shadow: 0 1px 3px rgba(15, 23, 42, 0.06) !important;
  padding: 0 24px !important;
  display: flex !important;
  align-items: center !important;
  justify-content: space-between !important;
  height: 64px !important;
  flex-shrink: 0;
}
.page-title {
  font-family: 'Inter', sans-serif;
  font-size: 16px;
  font-weight: 600;
  color: #0F172A;
  letter-spacing: -0.1px;
}

/* ─── Header 右侧区域 ─── */
.header-right {
  display: flex;
  align-items: center;
  gap: 16px;
}
.header-divider {
  width: 1px;
  height: 20px;
  background: #E2E8F0;
  flex-shrink: 0;
}
.header-status {
  display: flex;
  align-items: center;
  gap: 6px;
}
.status-dot {
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: #22C55E;
  box-shadow: 0 0 5px rgba(34, 197, 94, 0.6);
  animation: pulse 2.5s ease-in-out infinite;
}
@keyframes pulse {
  0%, 100% { opacity: 1; }
  50% { opacity: 0.5; }
}
.status-label {
  font-family: 'Inter', sans-serif;
  font-size: 12px;
  color: #64748B;
}

/* ─── 用户信息 ─── */
.header-user {
  display: flex;
  align-items: center;
  gap: 8px;
}
.user-avatar-sm {
  background: #22C55E !important;
  color: #FFFFFF !important;
  font-weight: 700 !important;
  font-size: 12px !important;
  flex-shrink: 0;
}
.header-username {
  font-family: 'Inter', sans-serif;
  font-size: 13px;
  font-weight: 500;
  color: #334155;
  max-width: 120px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.logout-btn {
  background: none;
  border: none;
  color: #94A3B8;
  cursor: pointer;
  padding: 6px;
  border-radius: 6px;
  display: flex;
  align-items: center;
  justify-content: center;
  transition: color 150ms ease, background 150ms ease;
  flex-shrink: 0;
}
.logout-btn:hover {
  color: #DC2626;
  background: rgba(220, 38, 38, 0.08);
}

/* ─── Content（银灰底）─── */
.main-content {
  padding: 24px;
  overflow: auto;
  background: #F1F5F9 !important;
}

@media (prefers-reduced-motion: reduce) {
  .status-dot { animation: none; }
}
</style>
