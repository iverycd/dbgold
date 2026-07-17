<template>
  <section class="incremental-log-panel">
    <div class="log-toolbar">
      <div class="log-title">
        <span>全量迁移日志</span>
        <a-tag size="small">{{ logs.length }} 条</a-tag>
      </div>
      <a-space wrap>
        <a-button size="mini" :disabled="loadingOlder || !hasOlder" :loading="loadingOlder" @click="loadOlder">
          加载更早
        </a-button>
        <a-button size="mini" :loading="refreshing" @click="refreshLatest">
          <template #icon><icon-refresh /></template>
          刷新
        </a-button>
        <a-button size="mini" :disabled="logs.length === 0" @click="copyLogs">复制日志</a-button>
        <a-checkbox v-model="followLatest">跟随最新</a-checkbox>
        <a-button size="mini" type="text" @click="expanded = !expanded">
          {{ expanded ? '收起' : '展开' }}
        </a-button>
      </a-space>
    </div>

    <a-alert v-if="droppedCount > 0" type="warning" class="log-alert">
      日志通道拥塞或持久化失败，已有 {{ droppedCount }} 条日志未能保存；数据迁移本身不会因此中止。
    </a-alert>
    <a-alert v-if="loadError" type="error" class="log-alert">{{ loadError }}</a-alert>

    <div v-show="expanded" ref="logContainer" class="migration-log-container" @scroll="handleScroll">
      <div v-if="initialLoading" class="log-placeholder">正在加载日志…</div>
      <div v-else-if="logs.length === 0" class="log-placeholder">暂无全量迁移日志</div>
      <div
        v-for="item in logs"
        :key="item.id"
        class="log-line"
        :class="`log-${normalizedLevel(item)}`"
      >{{ item.line }}</div>
    </div>
  </section>
</template>

<script setup lang="ts">
import { nextTick, onUnmounted, ref, watch } from 'vue'
import { Message } from '@arco-design/web-vue'
import {
  getIncrementalMigrationLogs,
  type IncrementalLogLevel,
  type IncrementalMigrationLog,
  type IncrementalMigrationLogPage,
} from '@/api/migration'
import { copyText } from '@/utils/clipboard'

const props = withDefaults(defineProps<{
  jobID: string
  polling?: boolean
  refreshToken?: number
}>(), {
  polling: true,
  refreshToken: 0,
})

const PAGE_SIZE = 500
const MAX_BROWSER_LOGS = 5000
const POLL_INTERVAL = 2000
const BOTTOM_THRESHOLD = 48
const MAX_DRAIN_PAGES_PER_TURN = 20
const MAX_DRAIN_RETRIES = 3

const logs = ref<IncrementalMigrationLog[]>([])
const logContainer = ref<HTMLElement | null>(null)
const expanded = ref(true)
const followLatest = ref(true)
const initialLoading = ref(false)
const loadingOlder = ref(false)
const loadingNew = ref(false)
const refreshing = ref(false)
const hasOlder = ref(false)
const droppedCount = ref(0)
const loadError = ref('')
const oldestSeenID = ref(0)
const newestSeenID = ref(0)
const browsingOlder = ref(false)
const nearBottom = ref(true)
let pollTimer: number | undefined
let requestGeneration = 0
let pendingNewRequest = false
let pendingDrain = false

function normalizedLevel(item: IncrementalMigrationLog): IncrementalLogLevel {
  if (['info', 'ddl', 'data', 'index', 'warn', 'error', 'done'].includes(item.level)) return item.level
  if (item.line.includes('[ERROR]')) return 'error'
  if (item.line.includes('[WARN]')) return 'warn'
  if (item.line.includes('[DONE]')) return 'done'
  if (item.line.includes('[DDL]')) return 'ddl'
  if (item.line.includes('[DATA]')) return 'data'
  if (item.line.includes('[INDEX]')) return 'index'
  return 'info'
}

function sortAndDeduplicate(items: IncrementalMigrationLog[]) {
  const byID = new Map<number, IncrementalMigrationLog>()
  for (const item of items) byID.set(item.id, item)
  return [...byID.values()].sort((a, b) => a.id - b.id)
}

function applyPageMetadata(page: IncrementalMigrationLogPage) {
  droppedCount.value = page.log_dropped_count || 0
  loadError.value = ''
}

async function scrollToBottom() {
  await nextTick()
  if (logContainer.value) logContainer.value.scrollTop = logContainer.value.scrollHeight
  nearBottom.value = true
}

async function loadTail(silent = false): Promise<boolean> {
  const generation = requestGeneration
  if (!silent) initialLoading.value = true
  try {
    const page = (await getIncrementalMigrationLogs(props.jobID, { limit: PAGE_SIZE })).data
    if (generation !== requestGeneration) return false
    logs.value = sortAndDeduplicate(page.items || [])
    oldestSeenID.value = page.oldest_id || logs.value[0]?.id || 0
    newestSeenID.value = page.newest_id || logs.value[logs.value.length - 1]?.id || 0
    hasOlder.value = !!page.has_older
    browsingOlder.value = false
    applyPageMetadata(page)
    await scrollToBottom()
    return true
  } catch (error: any) {
    if (generation !== requestGeneration) return false
    loadError.value = error?.response?.data?.error || '全量迁移日志加载失败'
    return false
  } finally {
    if (generation === requestGeneration) initialLoading.value = false
  }
}

async function loadNew(silent = true, drain = false) {
  if (!props.jobID) return
  if (loadingNew.value) {
    pendingNewRequest = true
    pendingDrain = pendingDrain || drain
    return
  }
  loadingNew.value = true
  const runGeneration = requestGeneration
  let keepDraining = drain
  let continueLater = false
  let retryCount = 0
  try {
    let pageCount = 0
    do {
      if (runGeneration !== requestGeneration) break
      pendingNewRequest = false
      const hasNewer = await doLoadNew(silent)
      pageCount++
      keepDraining = keepDraining || pendingDrain
      pendingDrain = false
      if (hasNewer === null && keepDraining && retryCount < MAX_DRAIN_RETRIES) {
        retryCount++
        await new Promise(resolve => window.setTimeout(resolve, 250 * (2 ** (retryCount - 1))))
        pendingNewRequest = true
      } else if (hasNewer === null && keepDraining) {
        loadError.value = '全量迁移日志增量拉取失败，请稍后手工刷新'
      } else if (hasNewer !== null) {
        retryCount = 0
      }
      if (keepDraining && hasNewer === true) {
        if (pageCount < MAX_DRAIN_PAGES_PER_TURN) pendingNewRequest = true
        else continueLater = true
      }
    } while (pendingNewRequest)
  } finally {
    loadingNew.value = false
    if (continueLater && runGeneration === requestGeneration && props.jobID && followLatest.value) {
      window.setTimeout(() => {
        if (runGeneration === requestGeneration && followLatest.value) void loadNew(true, true)
      }, 0)
    }
  }
}

async function doLoadNew(silent = true): Promise<boolean | null> {
  if (!newestSeenID.value) {
    return await loadTail(silent) ? false : null
  }
  const generation = requestGeneration
  const shouldStick = followLatest.value && nearBottom.value
  try {
    const page = (await getIncrementalMigrationLogs(props.jobID, {
      after_id: newestSeenID.value,
      limit: PAGE_SIZE,
    })).data
    if (generation !== requestGeneration) return false
    const incoming = page.items || []
    if (incoming.length) {
      logs.value = sortAndDeduplicate([...logs.value, ...incoming])
      newestSeenID.value = page.newest_id || incoming[incoming.length - 1]?.id || newestSeenID.value
      if (logs.value.length > MAX_BROWSER_LOGS) {
        logs.value = logs.value.slice(logs.value.length - MAX_BROWSER_LOGS)
        oldestSeenID.value = logs.value[0]?.id || oldestSeenID.value
        hasOlder.value = true
      }
    }
    applyPageMetadata(page)
    if (shouldStick) await scrollToBottom()
    return !!page.has_newer
  } catch (error: any) {
    if (!silent && generation === requestGeneration) {
      loadError.value = error?.response?.data?.error || '刷新全量迁移日志失败'
    }
    return null
  }
}

async function loadOlder() {
  if (!oldestSeenID.value || loadingOlder.value || !hasOlder.value) return
  requestGeneration++
  pendingNewRequest = false
  pendingDrain = false
  loadingOlder.value = true
  const generation = requestGeneration
  const element = logContainer.value
  const previousHeight = element?.scrollHeight || 0
  followLatest.value = false
  browsingOlder.value = true
  try {
    const page = (await getIncrementalMigrationLogs(props.jobID, {
      before_id: oldestSeenID.value,
      limit: PAGE_SIZE,
    })).data
    if (generation !== requestGeneration) return false
    const incoming = page.items || []
    if (incoming.length) {
      logs.value = sortAndDeduplicate([...incoming, ...logs.value])
      oldestSeenID.value = page.oldest_id || incoming[0]?.id || oldestSeenID.value
      if (logs.value.length > MAX_BROWSER_LOGS) logs.value = logs.value.slice(0, MAX_BROWSER_LOGS)
    }
    hasOlder.value = !!page.has_older
    applyPageMetadata(page)
    await nextTick()
    if (element) element.scrollTop += element.scrollHeight - previousHeight
  } catch (error: any) {
    if (generation === requestGeneration) {
      loadError.value = error?.response?.data?.error || '更早日志加载失败'
    }
  } finally {
    loadingOlder.value = false
  }
}

async function refreshLatest() {
  refreshing.value = true
  try {
    if (browsingOlder.value) {
      requestGeneration++
      pendingNewRequest = false
      pendingDrain = false
      await loadTail(true)
    } else await loadNew(false, true)
  } finally {
    refreshing.value = false
  }
}

function handleScroll() {
  const element = logContainer.value
  if (!element) return
  nearBottom.value = element.scrollHeight - element.scrollTop - element.clientHeight <= BOTTOM_THRESHOLD
}

async function copyLogs() {
  try {
    await copyText(logs.value.map(item => item.line).join('\n'))
    Message.success('日志已复制')
  } catch {
    Message.error('复制日志失败')
  }
}

function stopPolling() {
  if (pollTimer) clearInterval(pollTimer)
  pollTimer = undefined
}

function startPolling() {
  stopPolling()
  if (!props.polling) return
  pollTimer = window.setInterval(() => {
    if (followLatest.value && !initialLoading.value && !refreshing.value) void loadNew(true, true)
  }, POLL_INTERVAL)
}

function reset() {
  requestGeneration++
  logs.value = []
  oldestSeenID.value = 0
  newestSeenID.value = 0
  hasOlder.value = false
  droppedCount.value = 0
  loadError.value = ''
  followLatest.value = true
  browsingOlder.value = false
  nearBottom.value = true
  pendingNewRequest = false
  pendingDrain = false
}

watch(() => props.jobID, async (jobID) => {
  stopPolling()
  reset()
  if (!jobID) return
  await loadTail()
  startPolling()
}, { immediate: true })

watch(() => props.polling, (enabled) => {
  startPolling()
  // 状态切换到 CDC 时再取一次，避免遗漏“全量完成并开始追赶”的最后一批日志。
  if (!enabled && props.jobID && followLatest.value) void loadNew(true, true)
})
watch(() => props.refreshToken, () => {
  if (props.jobID && followLatest.value) void loadNew(false, true)
})
watch(followLatest, (enabled) => {
  if (enabled && browsingOlder.value) {
    requestGeneration++
    pendingNewRequest = false
    pendingDrain = false
    void loadTail(true)
  }
})
watch(expanded, (enabled) => {
  if (enabled && followLatest.value) void scrollToBottom()
})

onUnmounted(() => {
  requestGeneration++
  stopPolling()
})
</script>

<style scoped>
.incremental-log-panel {
  margin-top: 16px;
  padding: 12px;
  border: 1px solid var(--color-border-2);
  border-radius: 6px;
}
.log-toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  margin-bottom: 8px;
}
.log-title {
  display: flex;
  align-items: center;
  gap: 8px;
  font-weight: 500;
}
.log-alert { margin-bottom: 8px; }
.migration-log-container {
  height: 400px;
  overflow-y: auto;
  padding: 12px;
  color: #d4d4d4;
  background: #1a1a1a;
  border-radius: 4px;
  font-family: Menlo, Monaco, 'Courier New', monospace;
  font-size: 12px;
}
.log-line {
  min-height: 19px;
  line-height: 1.6;
  white-space: pre-wrap;
  word-break: break-all;
}
.log-placeholder { color: #8c8c8c; line-height: 2; }
.log-info { color: #d4d4d4; }
.log-ddl { color: #61afef; }
.log-data { color: #c678dd; }
.log-index { color: #56b6c2; }
.log-warn { color: #e5c07b; }
.log-error { color: #f47174; }
.log-done { color: #98c379; }

@media (max-width: 800px) {
  .log-toolbar { align-items: flex-start; flex-direction: column; }
}
</style>
