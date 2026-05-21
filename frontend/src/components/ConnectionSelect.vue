<template>
  <a-row :gutter="12">
    <a-col :span="14">
      <a-select
        :model-value="connectionId"
        placeholder="选择连接"
        allow-search
        @change="(val) => emit('update:connectionId', val as number)"
        :loading="loading"
      >
        <a-option v-for="c in connections" :key="c.id" :value="c.id">
          {{ c.name }} ({{ c.db_type }})
        </a-option>
      </a-select>
    </a-col>
    <a-col :span="10">
      <a-input
        :model-value="database"
        placeholder="数据库名"
        @input="(val: string) => emit('update:database', val)"
        allow-clear
      />
    </a-col>
  </a-row>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { listConnections, type Connection } from '@/api/connections'

defineProps<{
  connectionId: number | undefined
  database: string
}>()

const emit = defineEmits<{
  'update:connectionId': [value: number]
  'update:database': [value: string]
}>()

const connections = ref<Connection[]>([])
const loading = ref(false)

onMounted(async () => {
  loading.value = true
  try {
    const res = await listConnections()
    connections.value = res.data
  } finally {
    loading.value = false
  }
})
</script>
