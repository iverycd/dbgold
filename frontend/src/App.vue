<template>
  <app-layout v-if="isAuthenticated">
    <router-view />
  </app-layout>
  <router-view v-else />
</template>

<script setup lang="ts">
import { computed, onMounted } from 'vue'
import { useRoute } from 'vue-router'
import { useAuthStore } from '@/stores/auth'
import AppLayout from '@/components/AppLayout.vue'

const auth = useAuthStore()
const route = useRoute()

const isAuthenticated = computed(() => !!auth.token && route.path !== '/login')

onMounted(() => {
  if (auth.token) {
    auth.fetchMe()
  }
})
</script>
