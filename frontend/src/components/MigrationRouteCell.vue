<template>
  <div class="migration-route-cell">
    <a-tooltip position="top" mini>
      <template #content>{{ endpointTooltip(source) }}</template>
      <div class="route-endpoint">
        <div class="endpoint-main">
          <a-tag v-if="source.dbType" :color="getDbTypeColor(source.dbType)" size="small">
            {{ getDbTypeLabel(source.dbType) }}
          </a-tag>
          <span :class="['endpoint-name', { deleted: !source.name }]">{{ source.name || '连接已删除' }}</span>
        </div>
        <div class="endpoint-sub">{{ source.database || '—' }}</div>
      </div>
    </a-tooltip>

    <icon-arrow-right class="route-arrow" aria-hidden="true" />

    <a-tooltip position="top" mini>
      <template #content>{{ endpointTooltip(destination) }}</template>
      <div class="route-endpoint">
        <div class="endpoint-main">
          <a-tag v-if="destination.dbType" :color="getDbTypeColor(destination.dbType)" size="small">
            {{ getDbTypeLabel(destination.dbType) }}
          </a-tag>
          <span :class="['endpoint-name', { deleted: !destination.name }]">{{ destination.name || '连接已删除' }}</span>
        </div>
        <div class="endpoint-sub">
          {{ destination.database || '—' }}<span v-if="destination.schema" class="schema-text"> / {{ destination.schema }}</span>
        </div>
      </div>
    </a-tooltip>
  </div>
</template>

<script setup lang="ts">
import { getDbTypeColor, getDbTypeLabel } from '@/utils/dbType'

export interface MigrationRouteEndpoint {
  dbType: string
  name?: string
  host?: string
  port?: number
  database?: string
  username?: string
  schema?: string
}

defineProps<{
  source: MigrationRouteEndpoint
  destination: MigrationRouteEndpoint
}>()

function endpointTooltip(endpoint: MigrationRouteEndpoint) {
  const details = [endpoint.name || '连接已删除']
  if (endpoint.host) details.push(`${endpoint.host}:${endpoint.port || '—'}`)
  if (endpoint.database) details.push(`库 ${endpoint.database}`)
  if (endpoint.schema) details.push(`Schema ${endpoint.schema}`)
  if (endpoint.username) details.push(`账号 ${endpoint.username}`)
  return details.join(' · ')
}
</script>

<style scoped>
.migration-route-cell {
  display: grid;
  grid-template-columns: minmax(0, 1fr) 18px minmax(0, 1fr);
  align-items: center;
  gap: 8px;
  min-width: 0;
}
.route-endpoint { min-width: 0; cursor: default; }
.endpoint-main {
  display: flex;
  align-items: center;
  gap: 6px;
  min-width: 0;
}
.endpoint-name {
  overflow: hidden;
  color: var(--fg-primary);
  font-size: 13px;
  font-weight: 500;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.endpoint-name.deleted { color: var(--fg-muted); font-weight: 400; }
.endpoint-sub {
  overflow: hidden;
  margin-top: 3px;
  color: var(--fg-muted);
  font-size: 11px;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.schema-text { color: #165dff; }
.route-arrow { color: var(--border-strong); font-size: 16px; }
</style>
