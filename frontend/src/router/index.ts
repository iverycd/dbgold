import { createRouter, createWebHistory } from 'vue-router'
import { useAuthStore } from '@/stores/auth'
import LoginView from '@/views/LoginView.vue'
import ConnectionsView from '@/views/ConnectionsView.vue'
import MigrationView from '@/views/MigrationView.vue'
import BatchMigrationView from '@/views/BatchMigrationView.vue'
import HistoryView from '@/views/HistoryView.vue'
import LoginHistoryView from '@/views/LoginHistoryView.vue'
import UsersView from '@/views/UsersView.vue'
import TicketSubmitView from '@/views/TicketSubmitView.vue'
import TicketsManageView from '@/views/TicketsManageView.vue'

const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/login', component: LoginView, meta: { public: true } },
    { path: '/ticket', component: TicketSubmitView, meta: { public: true } },
    { path: '/', redirect: '/connections' },
    { path: '/connections', component: ConnectionsView },
    { path: '/query', component: () => import('@/views/QueryCenterView.vue') },
    { path: '/migration', component: MigrationView },
    { path: '/batch-migration', component: BatchMigrationView },
    { path: '/history', component: HistoryView },
    { path: '/history/data/:jobId', component: () => import('@/views/DataMigrationJobDetailView.vue') },
    { path: '/history/incremental/:jobId', component: () => import('@/views/IncrementalJobDetailView.vue') },
    { path: '/tickets', component: TicketsManageView },
    { path: '/users', component: UsersView, meta: { adminOnly: true } },
    { path: '/login-history', component: LoginHistoryView, meta: { adminOnly: true } },
  ],
})

router.beforeEach(async (to) => {
  const auth = useAuthStore()
  if (!to.meta.public && !auth.token) {
    return '/login'
  }
  if (to.meta.adminOnly) {
    if (!auth.user) await auth.fetchMe()
    if (auth.user?.role !== 'admin') return '/connections'
  }
})

export default router
