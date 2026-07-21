<template>
  <section class="route-surface" aria-label="迁移链路">
    <div class="connection-block">
      <span class="connection-role">源端</span>
      <div class="connection-title">
        <a-tag v-if="srcDbType" :color="getDbTypeColor(srcDbType)" size="small">{{ getDbTypeLabel(srcDbType) }}</a-tag>
        <strong :title="srcConn?.name || '连接已删除'">{{ srcConn?.name || '连接已删除' }}</strong>
      </div>
      <ConnectionLines :connection="srcConn" :database="srcDatabase" />
    </div>

    <div class="route-direction" aria-hidden="true">
      <span class="route-line"></span>
      <icon-arrow-right />
      <span class="route-caption">{{ modeLabel }}</span>
    </div>

    <div class="connection-block">
      <span class="connection-role">目标端</span>
      <div class="connection-title">
        <a-tag v-if="dstDbType" :color="getDbTypeColor(dstDbType)" size="small">{{ getDbTypeLabel(dstDbType) }}</a-tag>
        <strong :title="dstConn?.name || '连接已删除'">{{ dstConn?.name || '连接已删除' }}</strong>
      </div>
      <ConnectionLines :connection="dstConn" :database="dstDatabase" :schema="dstSchema" />
    </div>
  </section>
</template>

<script setup lang="ts">
import { defineComponent, h, type PropType } from 'vue'
import { Message } from '@arco-design/web-vue'
import { getDbTypeColor, getDbTypeLabel } from '@/utils/dbType'
import { copyText } from '@/utils/clipboard'
import { connectionEndpoint } from '@/utils/incrementalJob'

export interface MigrationConnectionSnapshot {
  id: number
  name: string
  host: string
  port: number
  database: string
  username: string
}

defineProps<{
  srcDbType?: string
  dstDbType?: string
  srcConn?: MigrationConnectionSnapshot | null
  dstConn?: MigrationConnectionSnapshot | null
  srcDatabase?: string
  dstDatabase?: string
  dstSchema?: string
  modeLabel: string
}>()

async function copy(value: string, label: string) {
  try {
    await copyText(value)
    Message.success(`${label}已复制`)
  } catch {
    Message.error('复制失败')
  }
}

const ConnectionLines = defineComponent({
  name: 'ConnectionLines',
  props: {
    connection: { type: Object as PropType<MigrationConnectionSnapshot | null | undefined>, default: null },
    database: { type: String, default: '' },
    schema: { type: String, default: '' },
  },
  setup(props) {
    const copyButton = (value: string, label: string) => h('button', {
      class: 'inline-copy',
      type: 'button',
      title: `复制${label}`,
      'aria-label': `复制${label}`,
      onClick: () => copy(value, label),
    }, '复制')
    const line = (label: string, value: string) => h('div', [
      h('span', label),
      h('code', value || '—'),
      value ? copyButton(value, label) : null,
    ])
    return () => h('div', { class: 'connection-lines' }, [
      props.connection ? line('地址', connectionEndpoint(props.connection)) : null,
      line('数据库', props.database),
      props.connection ? line('账号', props.connection.username) : null,
      props.schema ? line('Schema', props.schema) : null,
    ])
  },
})
</script>

<style scoped>
.route-surface { display: grid; grid-template-columns: minmax(0, 1fr) 150px minmax(0, 1fr); align-items: stretch; border: 1px solid var(--border); border-radius: var(--radius-md); background: var(--bg-surface); box-shadow: var(--shadow-sm); }
.route-direction { display: flex; position: relative; align-items: center; justify-content: center; gap: 6px; color: var(--accent-indigo); }
.route-line { position: absolute; right: 0; bottom: 50%; left: 0; height: 1px; background: var(--border); }
.route-direction :deep(svg) { z-index: 1; padding: 5px; box-sizing: content-box; border: 1px solid var(--border); border-radius: 50%; background: white; }
.route-caption { position: absolute; top: calc(50% + 20px); padding: 0 7px; background: white; color: var(--fg-muted); font-size: 11px; white-space: nowrap; }
.connection-block { min-width: 0; padding: 18px 20px; }
.connection-role { display: block; margin-bottom: 8px; color: var(--fg-muted); font-size: 11px; font-weight: 700; letter-spacing: .08em; }
.connection-title { display: flex; align-items: center; gap: 8px; min-width: 0; margin-bottom: 12px; }
.connection-title strong { overflow: hidden; font-size: 16px; text-overflow: ellipsis; white-space: nowrap; }
:deep(.connection-lines) { display: grid; gap: 6px; }
:deep(.connection-lines > div) { display: grid; grid-template-columns: 60px minmax(0, 1fr) auto; align-items: start; gap: 8px; min-width: 0; }
:deep(.connection-lines span) { color: var(--fg-muted); font-size: 12px; }
:deep(.connection-lines code) { min-width: 0; color: var(--fg-secondary); font-family: var(--font-mono); font-size: 12px; overflow-wrap: anywhere; }
:deep(.inline-copy) { padding: 0; border: 0; background: none; color: var(--accent-indigo); font-size: 11px; cursor: pointer; }

@media (max-width: 1100px) {
  .route-surface { grid-template-columns: 1fr; }
  .route-direction { min-height: 58px; }
  .route-direction .route-line { top: 0; right: auto; bottom: 0; left: 50%; width: 1px; height: auto; }
  .route-direction :deep(svg) { transform: rotate(90deg); }
  .route-caption { display: none; }
}
</style>
