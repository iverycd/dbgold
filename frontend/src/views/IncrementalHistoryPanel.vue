<template>
  <div style="display:flex;justify-content:flex-end;margin-bottom:16px"><a-button :loading="loading" @click="load"><template #icon><icon-refresh /></template>刷新</a-button></div>
  <a-table :data="jobs" :loading="loading" row-key="id" :pagination="{pageSize:20}">
    <template #columns>
      <a-table-column title="Job ID" :width="100"><template #cell="{record}"><a-tooltip :content="record.job_id"><code>{{ record.job_id.slice(0,6) }}…</code></a-tooltip></template></a-table-column>
      <a-table-column title="源库" data-index="src_database" :width="150" />
      <a-table-column title="目标 Schema" data-index="target_schema" :width="150" />
      <a-table-column title="模式" :width="120"><template #cell="{record}">{{ record.start_mode === 'full_then_cdc' ? '全量 + 增量' : '仅增量' }}</template></a-table-column>
      <a-table-column title="状态" :width="120"><template #cell="{record}"><a-tag :color="color(record.status)">{{ text(record.status) }}</a-tag></template></a-table-column>
      <a-table-column title="I / U / D / 跳过" :width="180"><template #cell="{record}">{{ record.insert_count }} / {{ record.update_count }} / {{ record.delete_count }} / {{ record.skipped_count }}</template></a-table-column>
      <a-table-column title="最后事件" :width="170"><template #cell="{record}">{{ record.last_event_at ? date(record.last_event_at) : '—' }}</template></a-table-column>
      <a-table-column title="操作" :width="250"><template #cell="{record}"><a-space>
        <a-button size="mini" @click="detail=record;drawer=true">详情</a-button>
        <a-button v-if="running(record.status)" size="mini" @click="pause(record)">暂停</a-button>
        <a-button v-if="resumable(record.status)" size="mini" @click="resume(record)">恢复</a-button>
        <a-button v-if="record.status==='paused_ddl'" size="mini" type="primary" @click="ack(record)">确认 DDL</a-button>
        <a-button v-if="record.status!=='stopped'" size="mini" status="danger" @click="stop(record)">停止</a-button>
      </a-space></template></a-table-column>
    </template>
  </a-table>
  <a-drawer v-model:visible="drawer" title="增量任务详情" :width="720" @close="detail=null">
    <a-descriptions v-if="detail" :column="2" bordered>
      <a-descriptions-item label="Job ID" :span="2">{{ detail.job_id }}</a-descriptions-item>
      <a-descriptions-item label="状态">{{ text(detail.status) }}</a-descriptions-item><a-descriptions-item label="阶段">{{ detail.phase }}</a-descriptions-item>
      <a-descriptions-item label="Binlog 文件">{{ detail.checkpoint_file || '—' }}</a-descriptions-item><a-descriptions-item label="位置">{{ detail.checkpoint_position }}</a-descriptions-item>
      <a-descriptions-item label="GTID" :span="2"><span style="word-break:break-all">{{ detail.checkpoint_gtid || '—' }}</span></a-descriptions-item>
      <a-descriptions-item label="摘要" :span="2">{{ detail.summary || '—' }}</a-descriptions-item>
    </a-descriptions>
    <a-alert v-if="detail?.last_error" type="error" style="margin-top:12px">{{ detail.last_error }}</a-alert>
    <template v-if="detail?.blocking_ddl"><a-divider>待处理 DDL</a-divider><pre class="ddl">{{ detail.blocking_ddl }}</pre></template>
  </a-drawer>
</template>
<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue'
import { Message } from '@arco-design/web-vue'
import { acknowledgeIncrementalDDL, listIncrementalJobs, pauseIncrementalJob, resumeIncrementalJob, stopIncrementalJob, type IncrementalJob } from '@/api/migration'
const jobs=ref<IncrementalJob[]>([]),loading=ref(false),drawer=ref(false),detail=ref<IncrementalJob|null>(null);let timer:number|undefined
async function load(){loading.value=true;try{jobs.value=(await listIncrementalJobs()).data;if(detail.value)detail.value=jobs.value.find(j=>j.job_id===detail.value?.job_id)||detail.value}catch{Message.error('加载增量任务失败')}finally{loading.value=false}}
async function act(fn:(id:string)=>Promise<unknown>,j:IncrementalJob,msg:string){try{await fn(j.job_id);Message.success(msg);await load()}catch(e:any){Message.error(e?.response?.data?.error||'操作失败')}}
const pause=(j:IncrementalJob)=>act(pauseIncrementalJob,j,'正在安全暂停'),resume=(j:IncrementalJob)=>act(resumeIncrementalJob,j,'任务已恢复'),stop=(j:IncrementalJob)=>act(stopIncrementalJob,j,'正在停止'),ack=(j:IncrementalJob)=>act(acknowledgeIncrementalDDL,j,'DDL 已确认')
const labels:Record<string,string>={initializing:'初始化',snapshot:'全量快照',catching_up:'追赶',running:'运行中',reconnecting:'重连中',pausing:'暂停中',paused_manual:'已暂停',paused_restart:'重启后暂停',paused_ddl:'DDL 暂停',stopped:'已停止',failed:'失败'}
const text=(s:string)=>labels[s]||s,color=(s:string)=>s==='running'?'green':s==='failed'?'red':s.startsWith('paused')?'orange':s==='stopped'?'gray':'blue'
const running=(s:string)=>['initializing','snapshot','catching_up','running','reconnecting'].includes(s),resumable=(s:string)=>['paused_manual','paused_restart','failed'].includes(s),date=(s:string)=>new Date(s).toLocaleString('zh-CN')
onMounted(()=>{load();timer=window.setInterval(load,5000)});onUnmounted(()=>{if(timer)clearInterval(timer)})
</script>
<style scoped>.ddl{padding:12px;background:#f2f3f5;border-radius:4px;white-space:pre-wrap}</style>
