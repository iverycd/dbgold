<template>
  <a-card title="PG → MySQL 兼容预检" style="margin: 16px 0">
    <a-alert v-if="!readonly" type="info" style="margin-bottom: 12px">
      预检将只读检查全部选中对象和数据。请在预检、迁移和校验期间停止源库写入及结构变更。
      启动时后端会重新检查；存在阻止项时不会修改目标表。
    </a-alert>
    <a-space v-if="!readonly" style="margin-bottom: 12px">
      <a-button :loading="busy" :disabled="disabled" @click="$emit('check')">{{ result ? '重新预检' : '运行预检' }}</a-button>
      <a-button v-if="result" @click="download">导出预检与 DDL</a-button>
    </a-space>
    <a-space v-if="excluded.length" wrap style="margin-bottom: 12px">
      <span>已明确排除：</span>
      <a-tag v-for="(item, index) in excluded" :key="index" :closable="!readonly && !busy" color="orange" @close="$emit('restore', item)">
        {{ objectLabel(item) }}
      </a-tag>
    </a-space>
    <template v-if="result">
      <a-alert :type="result.ready ? 'success' : 'error'" style="margin-bottom: 12px">
        {{ result.ready ? '预检通过' : '预检未通过：请处理阻止项' }} · 已检查 {{ result.rows_checked }} 行
      </a-alert>
      <p>源库：{{ result.source.product }} · {{ result.source.version }} · {{ result.source.mode || '默认兼容模式' }}</p>
      <p>目标：MySQL {{ result.target.version }} / {{ result.target.database }} · {{ result.target.target_collation }}</p>
      <p>{{ result.source.verification }}；{{ result.target.verification }}</p>
      <a-table :data="result.issues" :pagination="{ pageSize: 10 }" size="small">
        <template #columns>
          <a-table-column title="级别" :width="80">
            <template #cell="{ record }"><a-tag :color="record.severity === 'error' ? 'red' : 'orange'">{{ record.severity === 'error' ? '阻止' : '提示' }}</a-tag></template>
          </a-table-column>
          <a-table-column title="对象"><template #cell="{ record }">{{ objectLabel(record.object) }}</template></a-table-column>
          <a-table-column title="原因" data-index="reason" />
          <a-table-column title="建议" data-index="suggestion" />
          <a-table-column v-if="!readonly" title="处理" :width="140">
            <template #cell="{ record }">
              <a-button v-if="record.excludable" type="text" status="danger" :disabled="busy || disabled" @click="$emit('exclude', record.object)">明确排除此对象</a-button>
            </template>
          </a-table-column>
        </template>
      </a-table>
      <a-collapse v-if="result.objects.length" style="margin-top: 12px">
        <a-collapse-item header="对象映射与计划 DDL" key="ddl">
          <div v-for="(object, index) in result.objects" :key="index" style="margin-bottom: 12px">
            <strong>{{ objectLabel(object.object) }} → {{ object.target }}</strong>
            <pre style="white-space: pre-wrap; overflow-wrap: anywhere; max-height: 320px; overflow: auto">{{ object.statements.map(statement => statement.SQL + ';').join('\n') }}</pre>
          </div>
        </a-collapse-item>
      </a-collapse>
    </template>
  </a-card>
</template>
<script setup lang="ts">
import type { MigrationObjectRef, PGMySQLPreflight } from '@/api/migration'
const props = withDefaults(defineProps<{ result?: PGMySQLPreflight | null; excluded?: MigrationObjectRef[]; busy?: boolean; disabled?: boolean; readonly?: boolean }>(), { excluded: () => [] })
defineEmits<{ check: []; exclude: [item: MigrationObjectRef]; restore: [item: MigrationObjectRef] }>()
function objectLabel(item: MigrationObjectRef) { return `${item.kind}: ${item.table ? item.table + '.' : ''}${item.name}` }
function download() {
  const url = URL.createObjectURL(new Blob([JSON.stringify(props.result, null, 2)], { type: 'application/json;charset=utf-8' }))
  const a = document.createElement('a'); a.href = url; a.download = 'pg-mysql-preflight.json'; a.click(); URL.revokeObjectURL(url)
}
</script>
