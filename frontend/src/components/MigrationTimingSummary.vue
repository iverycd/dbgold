<template>
  <section class="migration-timing" aria-label="迁移时间概览" :aria-busy="loading" aria-live="polite">
    <div v-if="loading || unavailable" class="timing-state">
      <icon-clock-circle aria-hidden="true" />
      <span>{{ loading ? '正在加载迁移时间…' : '时间信息暂不可用' }}</span>
    </div>
    <dl v-else class="timing-grid">
      <div class="timing-item timing-start">
        <dt><icon-clock-circle aria-hidden="true" />迁移开始时间</dt>
        <dd>{{ timing.startedAt }}</dd>
      </div>
      <div class="timing-item">
        <dt>迁移结束时间</dt>
        <dd>{{ timing.finishedAt }}</dd>
      </div>
      <div class="timing-item">
        <dt>总耗时</dt>
        <dd>{{ timing.duration }}</dd>
      </div>
    </dl>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { migrationTimingSummary, type MigrationTiming } from '@/utils/migrationReport'

const props = defineProps<{
  job?: MigrationTiming | null
  loading?: boolean
  unavailable?: boolean
}>()

const timing = computed(() => migrationTimingSummary(props.job))
</script>

<style scoped>
.migration-timing {
  min-width: 0;
  margin-bottom: 20px;
  border: 1px solid rgb(22 163 74 / 22%);
  border-left: 3px solid #16a34a;
  border-radius: 10px;
  background: linear-gradient(rgb(22 163 74 / 6%), rgb(22 163 74 / 6%)), var(--color-bg-2);
  container: migration-timing / inline-size;
}
.timing-grid {
  display: grid;
  grid-template-columns: minmax(0, 1.2fr) minmax(0, 1.1fr) minmax(0, 1fr);
  gap: 20px;
  margin: 0;
  padding: 20px;
}
.timing-item { min-width: 0; }
.timing-item + .timing-item {
  padding-left: 20px;
  border-left: 1px solid rgb(22 163 74 / 16%);
}
.timing-item dt {
  display: flex;
  align-items: center;
  gap: 7px;
  margin-bottom: 8px;
  color: var(--color-text-2);
  font-size: 13px;
  line-height: 20px;
}
.timing-start dt svg { color: #16a34a; font-size: 17px; }
.timing-item dd {
  margin: 0;
  color: var(--color-text-1);
  font-size: 16px;
  font-weight: 600;
  font-variant-numeric: tabular-nums;
  line-height: 28px;
  overflow-wrap: anywhere;
}
.timing-start dd { font-size: 20px; font-weight: 700; }
.timing-state {
  display: flex;
  align-items: center;
  gap: 10px;
  min-height: 56px;
  padding: 16px 20px;
  color: var(--color-text-2);
  font-size: 14px;
}
.timing-state svg { flex-shrink: 0; color: #16a34a; font-size: 20px; }
@container migration-timing (max-width: 767px) {
  .timing-grid { grid-template-columns: minmax(0, 1fr); gap: 16px; padding: 16px; }
  .timing-item + .timing-item {
    padding-top: 16px;
    padding-left: 0;
    border-top: 1px solid rgb(22 163 74 / 16%);
    border-left: 0;
  }
}
</style>
