<template>
  <div class="migration-action-bar" role="region" aria-label="迁移操作">
    <div class="migration-action-status" role="status">
      <span class="migration-status-dot" :class="{ 'is-busy': busy }"></span>
      {{ status }}
    </div>
    <a-space wrap><slot /></a-space>
  </div>
</template>

<script setup lang="ts">
defineProps<{ status: string; busy?: boolean }>()
</script>

<style scoped>
.migration-action-bar {
  position: sticky;
  bottom: 0;
  z-index: 10;
  display: flex;
  align-items: center;
  justify-content: space-between;
  flex-wrap: wrap;
  gap: 12px;
  margin-top: 20px;
  padding: 16px 24px;
  border: 1px solid var(--color-border-2);
  border-radius: 10px;
  background: var(--color-bg-1);
  box-shadow: 0 -4px 16px rgb(15 23 42 / 4%);
}
.migration-action-status { display: flex; align-items: center; gap: 8px; color: var(--color-text-3); font-size: 13px; }
.migration-status-dot { width: 7px; height: 7px; flex-shrink: 0; border-radius: 50%; background: var(--color-text-4); }
.migration-status-dot.is-busy { background: #16a34a; box-shadow: 0 0 0 4px rgb(22 163 74 / 10%); }
@media (max-width: 767px) { .migration-action-bar { padding: 12px 16px; } }
</style>
