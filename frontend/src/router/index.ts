import { createRouter, createWebHistory } from 'vue-router'
import { useAuthStore } from '@/stores/auth'
import LoginView from '@/views/LoginView.vue'
import ConnectionsView from '@/views/ConnectionsView.vue'
import SchemaView from '@/views/SchemaView.vue'
import DiffView from '@/views/DiffView.vue'
import MigrationView from '@/views/MigrationView.vue'
import HistoryView from '@/views/HistoryView.vue'

const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/login', component: LoginView, meta: { public: true } },
    { path: '/', redirect: '/connections' },
    { path: '/connections', component: ConnectionsView },
    { path: '/schema', component: SchemaView },
    { path: '/diff', component: DiffView },
    { path: '/migration', component: MigrationView },
    { path: '/history', component: HistoryView },
  ],
})

router.beforeEach((to) => {
  const auth = useAuthStore()
  if (!to.meta.public && !auth.token) {
    return '/login'
  }
})

export default router
