import { createRouter, createWebHistory } from 'vue-router'
import { useAuthStore } from '@/stores/auth'
import LoginView from '@/views/LoginView.vue'
import ConnectionsView from '@/views/ConnectionsView.vue'
import SchemaView from '@/views/SchemaView.vue'
import DiffView from '@/views/DiffView.vue'
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
    { path: '/schema', component: SchemaView },
    { path: '/diff', component: DiffView },
    { path: '/migration', component: MigrationView },
    { path: '/batch-migration', component: BatchMigrationView },
    { path: '/history', component: HistoryView },
    { path: '/tickets', component: TicketsManageView, meta: { adminOnly: true } },
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
