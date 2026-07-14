<template>
  <a-form :model="form" layout="vertical" style="margin-top: 12px; max-width: 1120px">
    <a-alert type="warning" style="margin-bottom:16px">
      增量迁移仅支持 MySQL → PostgreSQL。源库必须启用 ROW binlog 和 FULL row image；运行期间的 DDL 会暂停任务，需人工处理后确认恢复。
    </a-alert>
    <a-row :gutter="20">
      <a-col :span="12">
        <a-form-item label="MySQL 源连接">
          <a-select v-model="form.src_conn_id" allow-search @change="loadDatabases">
            <a-option v-for="c in mysqlConnections" :key="c.id" :value="c.id">{{ c.name }} · {{ c.host }}:{{ c.port }}</a-option>
          </a-select>
        </a-form-item>
        <a-form-item label="源数据库">
          <a-select v-model="form.src_database" allow-search><a-option v-for="d in databases" :key="d" :value="d">{{ d }}</a-option></a-select>
        </a-form-item>
      </a-col>
      <a-col :span="12">
        <a-form-item label="PostgreSQL 目标连接">
          <a-select v-model="form.dst_conn_id" allow-search @change="loadSchemas">
            <a-option v-for="c in postgresConnections" :key="c.id" :value="c.id">{{ c.name }} · {{ c.host }}:{{ c.port }}</a-option>
          </a-select>
        </a-form-item>
        <a-form-item label="目标 Schema">
          <a-select v-model="form.target_schema" allow-search><a-option v-for="s in schemas" :key="s" :value="s">{{ s }}</a-option></a-select>
        </a-form-item>
      </a-col>
    </a-row>
    <a-form-item label="启动方式">
      <a-radio-group v-model="form.start_mode">
        <a-radio value="full_then_cdc">全量快照后持续同步</a-radio>
        <a-radio value="incremental_only">从指定位点开始</a-radio>
      </a-radio-group>
    </a-form-item>
    <a-row v-if="form.start_mode === 'incremental_only'" :gutter="16">
      <a-col :span="6"><a-form-item label="位点类型"><a-radio-group v-model="form.position_mode"><a-radio value="gtid">GTID</a-radio><a-radio value="file">文件</a-radio></a-radio-group></a-form-item></a-col>
      <a-col v-if="form.position_mode === 'gtid'" :span="18"><a-form-item label="GTID Set"><a-input v-model="form.start_gtid" placeholder="uuid:1-100" /></a-form-item></a-col>
      <template v-else>
        <a-col :span="10"><a-form-item label="Binlog 文件"><a-input v-model="form.start_file" placeholder="mysql-bin.000001" /></a-form-item></a-col>
        <a-col :span="8"><a-form-item label="位置"><a-input-number v-model="form.start_position" :min="4" style="width:100%" /></a-form-item></a-col>
      </template>
    </a-row>
    <a-row :gutter="16">
      <a-col :span="8"><a-form-item label="表范围"><a-select v-model="form.migrate_mode"><a-option value="all">全部表</a-option><a-option value="include">仅包含</a-option><a-option value="exclude">排除</a-option></a-select></a-form-item></a-col>
      <a-col :span="12"><a-form-item label="表过滤"><a-input v-model="form.table_filter" :disabled="form.migrate_mode === 'all'" placeholder="逗号分隔，支持 *" /></a-form-item></a-col>
      <a-col :span="4"><a-form-item label="名称"><a-checkbox v-model="form.lower_case_names">转小写</a-checkbox></a-form-item></a-col>
    </a-row>
    <a-space style="margin-bottom:16px">
      <a-button :loading="checking" :disabled="!ready" @click="preflight">运行预检</a-button>
      <a-button type="primary" :loading="starting" :disabled="!preflightResult?.ok || !!currentJob" @click="start">启动增量任务</a-button>
    </a-space>
    <a-card v-if="preflightResult" title="预检结果" style="margin-bottom:16px">
      <a-descriptions :column="4" size="small">
        <a-descriptions-item label="log_bin">{{ preflightResult.log_bin ? 'ON' : 'OFF' }}</a-descriptions-item>
        <a-descriptions-item label="format">{{ preflightResult.binlog_format }}</a-descriptions-item>
        <a-descriptions-item label="row image">{{ preflightResult.binlog_row_image }}</a-descriptions-item>
        <a-descriptions-item label="匹配表">{{ preflightResult.tables?.length || 0 }}</a-descriptions-item>
      </a-descriptions>
      <a-alert v-for="e in preflightResult.errors" :key="e" type="error" style="margin-top:8px">{{ e }}</a-alert>
      <a-alert v-for="w in preflightResult.warnings" :key="w" type="warning" style="margin-top:8px">{{ w }}</a-alert>
      <div v-if="preflightResult.no_primary_key_tables?.length" class="hint">无主键表：{{ preflightResult.no_primary_key_tables.join(', ') }}</div>
    </a-card>
    <a-card v-if="currentJob" title="当前增量任务">
      <template #extra><a-tag :color="statusColor(currentJob.status)">{{ statusText(currentJob.status) }}</a-tag></template>
      <a-descriptions :column="3" size="small">
        <a-descriptions-item label="阶段">{{ currentJob.phase }}</a-descriptions-item>
        <a-descriptions-item label="INSERT">{{ currentJob.insert_count }}</a-descriptions-item>
        <a-descriptions-item label="UPDATE">{{ currentJob.update_count }}</a-descriptions-item>
        <a-descriptions-item label="DELETE">{{ currentJob.delete_count }}</a-descriptions-item>
        <a-descriptions-item label="跳过/告警">{{ currentJob.skipped_count }} / {{ currentJob.warning_count }}</a-descriptions-item>
        <a-descriptions-item label="最后事件">{{ currentJob.last_event_at ? formatDate(currentJob.last_event_at) : '—' }}</a-descriptions-item>
        <a-descriptions-item label="Binlog 位点" :span="3">{{ currentJob.checkpoint_file || '—' }}:{{ currentJob.checkpoint_position || 0 }}</a-descriptions-item>
      </a-descriptions>
      <a-alert v-if="currentJob.last_error" type="error" style="margin-top:10px">{{ currentJob.last_error }}</a-alert>
      <div v-if="currentJob.blocking_ddl" style="margin-top:12px">
        <div class="hint">检测到 DDL，请先在目标库人工处理：</div><pre class="ddl">{{ currentJob.blocking_ddl }}</pre>
      </div>
      <a-space style="margin-top:12px">
        <a-button v-if="isRunning" @click="pause">暂停</a-button>
        <a-button v-if="canResume" @click="resume">恢复</a-button>
        <a-button v-if="currentJob.status === 'paused_ddl'" type="primary" @click="ackDDL">确认已处理并恢复</a-button>
        <a-button v-if="currentJob.status !== 'stopped'" status="danger" @click="stop">停止</a-button>
      </a-space>
    </a-card>
  </a-form>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import { Message } from '@arco-design/web-vue'
import { listConnections, listConnectionDatabases, listConnectionSchemas, type Connection } from '@/api/connections'
import { acknowledgeIncrementalDDL, getIncrementalJob, pauseIncrementalJob, preflightIncremental, resumeIncrementalJob, startIncremental, stopIncrementalJob, type IncrementalJob, type IncrementalPreflight, type IncrementalRequest } from '@/api/migration'

const connections=ref<Connection[]>([]),databases=ref<string[]>([]),schemas=ref<string[]>([])
const checking=ref(false),starting=ref(false),preflightResult=ref<IncrementalPreflight|null>(null),currentJob=ref<IncrementalJob|null>(null)
let timer:number|undefined
const form=reactive<IncrementalRequest>({src_conn_id:0,dst_conn_id:0,src_database:'',target_schema:'',start_mode:'full_then_cdc',position_mode:'gtid',start_gtid:'',start_file:'',start_position:4,migrate_mode:'all',table_filter:'',lower_case_names:true})
const mysqlConnections=computed(()=>connections.value.filter(c=>c.db_type==='mysql')),postgresConnections=computed(()=>connections.value.filter(c=>c.db_type==='postgres'))
const ready=computed(()=>!!(form.src_conn_id&&form.dst_conn_id&&form.src_database&&form.target_schema))
const isRunning=computed(()=>['initializing','snapshot','catching_up','running','reconnecting'].includes(currentJob.value?.status||''))
const canResume=computed(()=>['paused_manual','paused_restart','failed'].includes(currentJob.value?.status||''))
watch(form,()=>preflightResult.value=null,{deep:true})
async function loadDatabases(){form.src_database='';if(form.src_conn_id)databases.value=(await listConnectionDatabases(form.src_conn_id)).data||[]}
async function loadSchemas(){form.target_schema='';if(form.dst_conn_id)schemas.value=(await listConnectionSchemas(form.dst_conn_id)).data||[]}
async function preflight(){checking.value=true;try{preflightResult.value=(await preflightIncremental(form)).data;if(preflightResult.value.ok)Message.success('预检通过');else Message.error('预检未通过')}catch(e:any){preflightResult.value=e?.response?.data?.preflight||null;Message.error(e?.response?.data?.error||'预检失败')}finally{checking.value=false}}
async function start(){starting.value=true;try{const r=await startIncremental(form);currentJob.value=(await getIncrementalJob(r.data.job_id)).data;beginPoll();Message.success('增量任务已启动')}catch(e:any){Message.error(e?.response?.data?.error||'启动失败')}finally{starting.value=false}}
async function refresh(){if(currentJob.value)currentJob.value=(await getIncrementalJob(currentJob.value.job_id)).data}
function beginPoll(){if(timer)clearInterval(timer);timer=window.setInterval(()=>refresh().catch(()=>{}),2000)}
async function pause(){await pauseIncrementalJob(currentJob.value!.job_id);await refresh()}
async function resume(){await resumeIncrementalJob(currentJob.value!.job_id);await refresh()}
async function stop(){await stopIncrementalJob(currentJob.value!.job_id);await refresh()}
async function ackDDL(){await acknowledgeIncrementalDDL(currentJob.value!.job_id);await refresh()}
const labels:Record<string,string>={initializing:'初始化',snapshot:'全量快照',catching_up:'追赶',running:'运行中',reconnecting:'重连中',pausing:'暂停中',paused_manual:'已暂停',paused_restart:'重启后暂停',paused_ddl:'DDL 暂停',stopped:'已停止',failed:'失败'}
function statusText(s:string){return labels[s]||s} function statusColor(s:string){return s==='running'?'green':s==='failed'?'red':s.startsWith('paused')?'orange':s==='stopped'?'gray':'blue'}
function formatDate(s:string){return new Date(s).toLocaleString('zh-CN')}
onMounted(async()=>{connections.value=(await listConnections()).data})
onUnmounted(()=>{if(timer)clearInterval(timer)})
</script>
<style scoped>.hint{margin-top:8px;color:var(--color-text-3);font-size:12px}.ddl{padding:10px;background:#f2f3f5;white-space:pre-wrap;border-radius:4px}</style>
