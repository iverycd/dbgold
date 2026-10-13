<template>
  <div class="migration-workspace">
    <header class="migration-page-heading">
      <div>
        <h2>数据库迁移</h2>
        <p>配置迁移范围，检查兼容性，追踪执行结果。</p>
      </div>
      <span class="migration-page-label"><icon-swap /> 数据迁移工作台</span>
    </header>
    <a-tabs v-model:active-key="activeTab" class="migration-mode-tabs" :destroy-on-hide="false" :lazy-load="false">
      <a-tab-pane key="data-migrate" title="单次迁移">
        <a-form :model="dataMigrate" layout="vertical" class="migration-form">
          <!-- 源库 / 目标库选择 -->
          <div class="migration-connections">
            <div class="migration-connection-side">
              <a-card class="conn-card" :body-style="{ padding: '24px' }">
                <div class="conn-card-header">
                  <a-tag color="orange" size="small">源库</a-tag>
                  <span class="conn-card-title">从哪里迁移</span>
                </div>
                <a-select
                  v-model="srcEnvFilter"
                  placeholder="按环境筛选"
                  style="width: 100%; margin-top: 10px"
                  allow-clear
                >
                  <template #prefix><span class="migration-select-label">环境</span></template>
                  <a-option v-for="e in envHistory" :key="e" :value="e" :label="e" />
                </a-select>
                <a-select
                  v-model="dataMigrate.srcConnId"
                  placeholder="选择源库连接"
                  style="width: 100%; margin-top: 10px"
                  allow-search
                  @change="(val) => { checkPairSupport(); loadSrcDatabases(val as number) }"
                >
                  <template #prefix><span class="migration-select-label">连接</span></template>
                  <a-option v-for="c in srcConnections" :key="c.id" :value="c.id" :label="c.name">
                    <a-tag :color="getDbTypeColor(c.db_type)" size="small" style="margin-right:6px">{{ getDbTypeLabel(c.db_type) }}</a-tag>{{ c.name }}
                  </a-option>
                </a-select>
                <div v-if="selectedSrc" class="conn-meta">
                  <span class="conn-meta-item"><span class="conn-meta-label">地址</span>{{ selectedSrc.host }}:{{ selectedSrc.port }}</span>
                  <span v-if="selectedSrc.database" class="conn-meta-item"><span class="conn-meta-label">数据库</span>{{ selectedSrc.database }}</span>
                  <span class="conn-meta-item"><span class="conn-meta-label">账号</span>{{ selectedSrc.username }}</span>
                </div>
                <a-select
                  v-if="dataMigrate.srcDatabases.length > 0"
                  v-model="dataMigrate.srcDatabase"
                  placeholder="选择要迁移的数据库"
                  style="width: 100%; margin-top: 10px"
                  allow-search
                >
                  <template #prefix><span class="migration-select-label">源数据库</span></template>
                  <a-option v-for="db in dataMigrate.srcDatabases" :key="db" :value="db" :label="db" />
                </a-select>
                <a-tooltip
                  v-if="dataMigrate.srcDatabase && canExportRoutines"
                  content="导出源库自定义函数/存储过程/触发器原始 DDL 为 .sql 文件。跨厂商语法不兼容，导出内容未做转换，需手动适配目标库。"
                >
                  <a-button
                    type="outline"
                    size="small"
                    long
                    style="margin-top: 10px"
                    :loading="exportingRoutines"
                    @click="handleExportRoutines"
                  >
                    <template #icon><icon-download /></template>
                    导出函数/存储过程/触发器
                  </a-button>
                </a-tooltip>
              </a-card>
            </div>
            <div class="migration-connection-arrow">
              <icon-arrow-right style="font-size: 28px; color: #165dff" />
            </div>
            <div class="migration-connection-side">
              <a-card class="conn-card" :body-style="{ padding: '24px' }">
                <div class="conn-card-header">
                  <a-tag color="blue" size="small">目标库</a-tag>
                  <span class="conn-card-title">迁移到哪里</span>
                </div>
                <a-select
                  v-model="dstEnvFilter"
                  placeholder="按环境筛选"
                  style="width: 100%; margin-top: 10px"
                  allow-clear
                >
                  <template #prefix><span class="migration-select-label">环境</span></template>
                  <a-option v-for="e in envHistory" :key="e" :value="e" :label="e" />
                </a-select>
                <a-select
                  v-model="dataMigrate.dstConnId"
                  placeholder="选择目标库连接"
                  style="width: 100%; margin-top: 10px"
                  allow-search
                  @change="(val) => { checkPairSupport(); loadDstSchemas(val as number) }"
                >
                  <template #prefix><span class="migration-select-label">连接</span></template>
                  <a-option v-for="c in pgConnections" :key="c.id" :value="c.id" :label="c.name">
                    <a-tag :color="getDbTypeColor(c.db_type)" size="small" style="margin-right:6px">{{ getDbTypeLabel(c.db_type) }}</a-tag>{{ c.name }}
                  </a-option>
                </a-select>
                <div v-if="selectedDst" class="conn-meta">
                  <span class="conn-meta-item"><span class="conn-meta-label">地址</span>{{ selectedDst.host }}:{{ selectedDst.port }}</span>
                  <span class="conn-meta-item"><span class="conn-meta-label">数据库</span>{{ targetDatabaseName(selectedDst, dataMigrate.dstSchema) }}</span>
                  <span class="conn-meta-item"><span class="conn-meta-label">账号</span>{{ selectedDst.username }}</span>
                </div>
                <a-select
                  v-if="dataMigrate.dstConnId"
                  v-model="dataMigrate.dstSchema"
                  :placeholder="selectedDst?.db_type === 'oracle' ? '留空使用当前登录用户' : '请选择目标 Schema'"
                  :allow-clear="selectedDst?.db_type === 'oracle'"
                  style="width: 100%; margin-top: 10px"
                  allow-search
                >
                  <template #prefix><span class="migration-select-label">目标 Schema</span></template>
                  <a-option v-for="s in dataMigrate.dstSchemas" :key="s" :value="s" :label="s" />
                </a-select>
                <div v-if="dataMigrate.dstConnId" class="schema-permission-tip">
                  <icon-info-circle style="flex-shrink:0" />
                  <span v-if="selectedDst?.db_type === 'oracle'">Schema 为已有 Oracle 用户，留空使用当前登录用户；连接中的数据库为 Service Name。</span>
                  <span v-else-if="selectedDst?.db_type === 'sqlserver'">目标数据库取自连接配置，Schema 默认 dbo。</span>
                  <span v-else>请确保目标 Schema 拥有创建对象的权限，否则请在目标数据库中自行处理模式权限后再执行迁移。</span>
                </div>
              </a-card>
            </div>
          </div>

          <!-- 不支持提示 -->
          <a-alert v-if="selectedSrc?.db_type === 'sqlserver' && selectedDst?.db_type === 'dameng'" type="info" style="margin-bottom: 12px">
              仅迁移 dbo 对象，达梦对象名统一大写。不兼容类型、精度和对象会在写入前报错；不自动降级。迁移期间请暂停源表写入。
            </a-alert>
            <a-alert
            v-if="dataMigrate.unsupportedMsg"
            type="error"
            style="margin-bottom: 16px"
          >
            {{ dataMigrate.unsupportedMsg }}
          </a-alert>

          <section class="migration-surface migration-settings"><div class="migration-section-heading"><h3>迁移设置</h3><span>选择范围与执行选项</span></div>
          <!-- 迁移范围 -->
          <a-form-item label="迁移范围" style="margin-bottom: 16px">
            <a-radio-group v-model="dataMigrate.mode">
              <a-radio value="all">全库迁移</a-radio>
              <a-radio value="exclude">排除指定表</a-radio>
              <a-radio value="include">仅迁移指定表</a-radio>
            </a-radio-group>
            <template v-if="dataMigrate.mode !== 'all'">
              <a-input
                v-model="dataMigrate.filter"
                placeholder="逗号分隔表名，支持 * 通配符，如：*_log,tmp_*"
                style="margin-top: 8px; max-width: 400px"
                @input="validateTableFilter"
              />
              <div v-if="tableFilterError" style="color: rgb(var(--danger-6)); font-size: 12px; margin-top: 4px">
                {{ tableFilterError }}
              </div>
            </template>
          </a-form-item>

          <!-- 迁移内容 -->
          <a-form-item label="迁移内容" style="margin-bottom: 16px">
            <a-radio-group v-model="dataMigrate.content">
              <a-radio value="both">表结构 + 数据行</a-radio>
              <a-radio value="schema_only">仅创建表结构</a-radio>
              <a-radio value="data_only">仅迁移数据行</a-radio>
            </a-radio-group>
          </a-form-item>

          <!-- 高级设置 -->
          <a-collapse class="migration-advanced">
            <a-collapse-item key="advanced" header="高级设置">
              <a-row :gutter="24" class="migration-advanced-grid">
                <!-- 左列：迁移参数与选项 -->
                <a-col :xs="24" :lg="12">
                  <div class="adv-section-title">迁移性能</div>
                  <a-row :gutter="16">
                    <a-col :span="8">
                      <a-form-item label="每页行数">
                        <a-input-number v-model="dataMigrate.pageSize" :min="1000" :max="500000" :step="1000" style="width: 100%" />
                      </a-form-item>
                    </a-col>
                    <a-col :span="8">
                      <a-form-item label="最大并发数">
                        <a-input-number v-model="dataMigrate.maxParallel" :min="1" :max="50" style="width: 100%" />
                      </a-form-item>
                    </a-col>
                    <a-col :span="8">
                      <a-form-item label="表内分页并发数">
                        <a-input-number v-model="dataMigrate.intraTableParallel" :min="1" :max="20" style="width: 100%" />
                      </a-form-item>
                    </a-col>
                  </a-row>

                  <div class="adv-section-title">对象选项</div>
                  <a-row :gutter="16">
                    <a-col v-if="selectedDst?.db_type !== 'dameng' && selectedDst?.db_type !== 'oscar' && selectedDst?.db_type !== 'oracle'" :span="12">
                      <a-checkbox v-model="dataMigrate.lowerCaseNames" class="adv-check">对象名转小写</a-checkbox>
                    </a-col>
                    <a-col v-if="selectedDst?.db_type === 'oscar' || selectedDst?.db_type === 'oracle'" :span="12">
                      <span class="adv-check">{{ selectedDst?.db_type === 'oracle' ? 'Oracle' : 'Oscar' }} 对象名统一转大写</span>
                    </a-col>
                    <a-col :span="12">
                      <a-checkbox v-model="dataMigrate.changeOwner" :disabled="selectedDst?.db_type === 'oracle' || selectedDst?.db_type === 'sqlserver'" class="adv-check">更改对象 owner 为 Schema 同名角色</a-checkbox>
                    </a-col>
                    <a-col :span="12">
                      <a-checkbox v-model="dataMigrate.charInLength" :disabled="selectedDst?.db_type === 'oracle' || selectedDst?.db_type === 'sqlserver'" class="adv-check">char 长度单位（CHAR）</a-checkbox>
                    </a-col>
                    <a-col :span="12">
                      <a-checkbox :disabled="selectedDst?.db_type === 'sqlserver'" v-model="dataMigrate.useNvarchar2" class="adv-check">使用 nvarchar2</a-checkbox>
                    </a-col>
                    <a-col :span="12">
                      <a-checkbox v-model="dataMigrate.distributed" :disabled="selectedDst?.db_type === 'oscar' || selectedDst?.db_type === 'oracle' || selectedDst?.db_type === 'sqlserver'" class="adv-check">分布式模式（DISTRIBUTE BY hash）</a-checkbox>
                    </a-col>
                  </a-row>

                  <div class="adv-section-title">视图剥离模式名</div>
                  <a-input
                    v-model="dataMigrate.stripViewSchemas"
                    placeholder="逗号分隔，如 financeplatform_3.0, otherdb"
                    allow-clear
                  />
                  <div class="adv-hint">迁移视图时，从视图定义中去除这些模式名前缀（忽略大小写）。用于跨库引用导致目标库找不到 schema 的场景。</div>
                </a-col>

                <!-- 右列：连接池配置 -->
                <a-col :xs="24" :lg="12">
                  <div class="adv-section-title">连接池配置 <span class="adv-section-note">0 表示使用默认值</span></div>
                  <div class="adv-subtitle">源库连接池</div>
                  <a-row :gutter="16">
                    <a-col :span="8">
                      <a-form-item label="最大连接数">
                        <a-input-number v-model="dataMigrate.srcMaxOpenConns" :min="0" :max="500" placeholder="默认 50" style="width: 100%" />
                      </a-form-item>
                    </a-col>
                    <a-col :span="8">
                      <a-form-item label="最大空闲连接数">
                        <a-input-number v-model="dataMigrate.srcMaxIdleConns" :min="0" :max="500" placeholder="默认 25" style="width: 100%" />
                      </a-form-item>
                    </a-col>
                    <a-col :span="8">
                      <a-form-item label="连接生命周期（秒）">
                        <a-input-number v-model="dataMigrate.srcConnMaxLifetime" :min="0" placeholder="默认 3600" style="width: 100%" />
                      </a-form-item>
                    </a-col>
                  </a-row>
                  <div class="adv-subtitle">目标库连接池</div>
                  <a-row :gutter="16">
                    <a-col :span="8">
                      <a-form-item label="最大连接数" style="margin-bottom: 0">
                        <a-input-number v-model="dataMigrate.dstMaxOpenConns" :min="0" :max="500" placeholder="默认 50" style="width: 100%" />
                      </a-form-item>
                    </a-col>
                    <a-col :span="8">
                      <a-form-item label="最大空闲连接数" style="margin-bottom: 0">
                        <a-input-number v-model="dataMigrate.dstMaxIdleConns" :min="0" :max="500" placeholder="默认 25" style="width: 100%" />
                      </a-form-item>
                    </a-col>
                    <a-col :span="8">
                      <a-form-item label="连接生命周期（秒）" style="margin-bottom: 0">
                        <a-input-number v-model="dataMigrate.dstConnMaxLifetime" :min="0" placeholder="默认 3600" style="width: 100%" />
                      </a-form-item>
                    </a-col>
                  </a-row>
                </a-col>
              </a-row>
            </a-collapse-item>
          </a-collapse>

          </section>

          <PGMySQLPreflightPanel v-if="isPGMySQLSelection('data')"
            :result="pgTaskSummaries.data ? (pgTaskSummaries.data.preflight ?? pgTaskSummaries.data.preview) : pgChecks.data"
            :compact="!!pgTaskSummaries.data" :summary-source="pgTaskSummaries.data?.preflight ? 'report' : 'page'"
            :excluded="pgExclusions.data" :busy="pgBusy.data" :disabled="dataMigrate.running"
            @view-report="showPGTaskResult('data', 'report')" @view-logs="showPGTaskResult('data', 'logs')"
            @check="runPGCheck('data')" @exclude="excludePGObject('data', $event)" @restore="restorePGObject('data', $event)" />

          <section ref="dataResultsSection" tabindex="-1" v-if="dataMigrate.running || dataMigrate.logs.length || dataMigrate.currentJobId" class="migration-surface migration-results">
            <div class="migration-section-heading"><h3>执行结果</h3><span>{{ dataMigrate.running ? '迁移进行中' : '查看日志与迁移报告' }}</span></div>
            <a-tabs v-model:active-key="dataMigrateResultTab" :destroy-on-hide="false" :lazy-load="false">
              <a-tab-pane key="logs" title="执行日志">
                <a-empty v-if="!dataMigrate.logs.length" description="等待迁移日志…" />
                <!-- 日志区 -->
                <div v-show="dataMigrate.logs.length">
                  <a-space style="margin-bottom: 8px">
                    <span style="font-weight:500">迁移日志</span>
                    <a-button size="mini" @click="copyLogs">复制日志</a-button>
                  </a-space>
                  <div ref="logContainer" class="migration-log-container">
                    <div
                      v-for="(line, i) in dataMigrate.logs"
                      :key="i"
                      :class="getLogClass(line)"
                      class="log-line"
                    >{{ line }}</div>
                  </div>
                </div>
              </a-tab-pane>
              <a-tab-pane v-if="dataMigrate.finished && dataMigrate.currentJobId" key="report" title="迁移报告">
                <!-- 迁移报告 -->
                <div v-if="dataMigrate.finished && dataMigrate.currentJobId" style="margin-top: 16px">
                  <!-- 连接信息摘要条 -->
                  <div v-if="selectedSrc && selectedDst" class="report-conn-bar">
                    <div class="report-conn-side">
                      <a-tag :color="getDbTypeColor(selectedSrc.db_type)" size="small" class="report-conn-type-tag">
                        {{ getDbTypeLabel(selectedSrc.db_type) }}
                      </a-tag>
                      <span class="report-conn-name">{{ selectedSrc.name }}</span>
                      <span class="report-conn-detail">
                        <span class="conn-meta-label">地址</span>{{ selectedSrc.host }}:{{ selectedSrc.port }}
                      </span>
                      <span v-if="dataMigrate.srcDatabase" class="report-conn-detail">
                        <span class="conn-meta-label">库</span>{{ dataMigrate.srcDatabase }}
                      </span>
                    </div>
                    <icon-arrow-right class="report-conn-arrow" />
                    <div class="report-conn-side">
                      <a-tag :color="getDbTypeColor(selectedDst.db_type)" size="small" class="report-conn-type-tag">
                        {{ getDbTypeLabel(selectedDst.db_type) }}
                      </a-tag>
                      <span class="report-conn-name">{{ selectedDst.name }}</span>
                      <span class="report-conn-detail">
                        <span class="conn-meta-label">地址</span>{{ selectedDst.host }}:{{ selectedDst.port }}
                      </span>
                      <span class="report-conn-detail">
                        <span class="conn-meta-label">库</span>{{ targetDatabaseName(selectedDst, dataMigrate.dstSchema) }}
                      </span>
                      <span v-if="dataMigrate.dstSchema" class="report-conn-detail">
                        <span class="conn-meta-label">Schema</span>{{ dataMigrate.dstSchema }}
                      </span>
                    </div>
                  </div>
                  <MigrationReportPanel ref="dataReportPanel" :key="dataMigrate.currentJobId" :jobID="dataMigrate.currentJobId" @loaded="(jobID, report) => onPGReportLoaded('data', jobID, report)" />
                </div>
              </a-tab-pane>
            </a-tabs>
          </section>
          <!-- 操作按钮 -->
          <MigrationActionBar :status="dataMigrate.running ? '正在迁移，请关注执行日志' : dataMigrate.finished ? '执行已结束，请查看日志与报告' : '配置完成后可开始迁移'" :busy="dataMigrate.running">
            <a-button
              type="primary"
              :disabled="!canStartMigration"
              :loading="dataMigrate.running"
              @click="requestDataMigration"
            >开始迁移</a-button>
            <a-button
              v-if="dataMigrate.running"
              status="danger"
              @click="cancelDataMigration"
            >停止迁移</a-button>
            <a-button
              v-if="dataMigrate.finished"
              @click="resetDataMigration"
            >重新迁移</a-button>
          </MigrationActionBar>

        </a-form>
      </a-tab-pane>

      <a-tab-pane key="incremental" title="增量迁移">
        <IncrementalMigrationPanel />
      </a-tab-pane>

      <a-tab-pane key="view-migrate" title="视图迁移">
        <a-form :model="viewMigrate" layout="vertical" class="migration-form">
          <!-- 源库 / 目标库选择 -->
          <div class="migration-connections">
            <div class="migration-connection-side">
              <a-card class="conn-card" :body-style="{ padding: '24px' }">
                <div class="conn-card-header">
                  <a-tag color="orange" size="small">源库</a-tag>
                  <span class="conn-card-title">从哪里迁移</span>
                </div>
                <a-select
                  v-model="srcEnvFilter"
                  placeholder="按环境筛选"
                  style="width: 100%; margin-top: 10px"
                  allow-clear
                >
                  <template #prefix><span class="migration-select-label">环境</span></template>
                  <a-option v-for="e in envHistory" :key="e" :value="e" :label="e" />
                </a-select>
                <a-select
                  v-model="viewMigrate.srcConnId"
                  placeholder="选择源库连接"
                  style="width: 100%; margin-top: 10px"
                  allow-search
                  @change="(val) => { vmCheckPairSupport(); vmLoadSrcDatabases(val as number) }"
                >
                  <template #prefix><span class="migration-select-label">连接</span></template>
                  <a-option v-for="c in srcConnections" :key="c.id" :value="c.id" :label="c.name">
                    <a-tag :color="getDbTypeColor(c.db_type)" size="small" style="margin-right:6px">{{ getDbTypeLabel(c.db_type) }}</a-tag>{{ c.name }}
                  </a-option>
                </a-select>
                <div v-if="vmSelectedSrc" class="conn-meta">
                  <span class="conn-meta-item"><span class="conn-meta-label">地址</span>{{ vmSelectedSrc.host }}:{{ vmSelectedSrc.port }}</span>
                  <span v-if="vmSelectedSrc.database" class="conn-meta-item"><span class="conn-meta-label">数据库</span>{{ vmSelectedSrc.database }}</span>
                  <span class="conn-meta-item"><span class="conn-meta-label">账号</span>{{ vmSelectedSrc.username }}</span>
                </div>
                <a-select
                  v-if="viewMigrate.srcDatabases.length > 0"
                  v-model="viewMigrate.srcDatabase"
                  placeholder="选择数据库"
                  style="width: 100%; margin-top: 10px"
                  allow-search
                  @change="vmLoadViews"
                >
                  <template #prefix><span class="migration-select-label">源数据库</span></template>
                  <a-option v-for="db in viewMigrate.srcDatabases" :key="db" :value="db" :label="db" />
                </a-select>
              </a-card>
            </div>
            <div class="migration-connection-arrow">
              <icon-arrow-right style="font-size: 28px; color: #165dff" />
            </div>
            <div class="migration-connection-side">
              <a-card class="conn-card" :body-style="{ padding: '24px' }">
                <div class="conn-card-header">
                  <a-tag color="blue" size="small">目标库</a-tag>
                  <span class="conn-card-title">迁移到哪里</span>
                </div>
                <a-select
                  v-model="dstEnvFilter"
                  placeholder="按环境筛选"
                  style="width: 100%; margin-top: 10px"
                  allow-clear
                >
                  <template #prefix><span class="migration-select-label">环境</span></template>
                  <a-option v-for="e in envHistory" :key="e" :value="e" :label="e" />
                </a-select>
                <a-select
                  v-model="viewMigrate.dstConnId"
                  placeholder="选择目标库连接"
                  style="width: 100%; margin-top: 10px"
                  allow-search
                  @change="(val) => { vmCheckPairSupport(); vmLoadDstSchemas(val as number) }"
                >
                  <template #prefix><span class="migration-select-label">连接</span></template>
                  <a-option v-for="c in pgConnections" :key="c.id" :value="c.id" :label="c.name">
                    <a-tag :color="getDbTypeColor(c.db_type)" size="small" style="margin-right:6px">{{ getDbTypeLabel(c.db_type) }}</a-tag>{{ c.name }}
                  </a-option>
                </a-select>
                <div v-if="vmSelectedDst" class="conn-meta">
                  <span class="conn-meta-item"><span class="conn-meta-label">地址</span>{{ vmSelectedDst.host }}:{{ vmSelectedDst.port }}</span>
                  <span class="conn-meta-item"><span class="conn-meta-label">数据库</span>{{ targetDatabaseName(vmSelectedDst, viewMigrate.dstSchema) }}</span>
                  <span class="conn-meta-item"><span class="conn-meta-label">账号</span>{{ vmSelectedDst.username }}</span>
                </div>
                <a-select
                  v-if="viewMigrate.dstConnId"
                  v-model="viewMigrate.dstSchema"
                  :placeholder="vmSelectedDst?.db_type === 'oracle' ? '留空使用当前登录用户' : '请选择目标 Schema'"
                  :allow-clear="vmSelectedDst?.db_type === 'oracle'"
                  style="width: 100%; margin-top: 10px"
                  allow-search
                >
                  <template #prefix><span class="migration-select-label">目标 Schema</span></template>
                  <a-option v-for="s in viewMigrate.dstSchemas" :key="s" :value="s" :label="s" />
                </a-select>
                <div v-if="viewMigrate.dstConnId" class="schema-permission-tip">
                  <icon-info-circle style="flex-shrink:0" />
                  <span v-if="vmSelectedDst?.db_type === 'oracle'">Schema 为已有 Oracle 用户，留空使用当前登录用户；连接中的数据库为 Service Name。</span>
                  <span v-else-if="vmSelectedDst?.db_type === 'sqlserver'">目标数据库取自连接配置，Schema 默认 dbo。</span>
                  <span v-else>请确保目标 Schema 拥有创建对象的权限，否则请在目标数据库中自行处理模式权限后再执行迁移。</span>
                </div>
              </a-card>
            </div>
          </div>

          <!-- 不支持提示 -->
          <a-alert v-if="vmSelectedSrc?.db_type === 'sqlserver' && vmSelectedDst?.db_type === 'dameng'" type="info" style="margin-bottom: 12px">
              仅迁移 dbo 对象，达梦对象名统一大写。不兼容类型、精度和对象会在写入前报错；不自动降级。迁移期间请暂停源表写入。
            </a-alert>
            <a-alert
            v-if="viewMigrate.unsupportedMsg"
            type="error"
            style="margin-bottom: 16px"
          >
            {{ viewMigrate.unsupportedMsg }}
          </a-alert>

          <section class="migration-surface migration-settings"><div class="migration-section-heading"><h3>迁移设置</h3><span>选择范围与执行选项</span></div>
          <!-- 视图选择 -->
          <a-spin :loading="viewMigrate.loadingViews" style="display:block">
            <div v-if="viewMigrate.views.length > 0" style="margin-bottom: 16px">
              <a-space style="margin-bottom: 8px" wrap>
                <a-input
                  v-model="viewMigrate.search"
                  placeholder="搜索视图名"
                  allow-clear
                  style="width: 240px"
                >
                  <template #prefix><icon-search /></template>
                </a-input>
                <a-button size="small" @click="vmSelectAllFiltered">全选当前结果</a-button>
                <a-button size="small" @click="vmClearSelection">清空选择</a-button>
                <span style="font-size: 12px; color: var(--color-text-3)">
                  已选 {{ viewMigrate.selected.length }} / 共 {{ viewMigrate.views.length }}
                </span>
              </a-space>
              <a-table
                :data="vmFilteredRows"
                row-key="name"
                :pagination="{ pageSize: 15, showTotal: true }"
                :scroll="{ y: 360 }"
                size="small"
                :row-selection="{ type: 'checkbox', showCheckedAll: true }"
                v-model:selected-keys="viewMigrate.selected"
              >
                <template #columns>
                  <a-table-column title="视图名" data-index="name" />
                </template>
              </a-table>
            </div>
            <a-empty v-else-if="viewMigrate.srcConnId && !viewMigrate.loadingViews" description="该连接下没有视图，或所选数据库不含视图" />
          </a-spin>

          <!-- 高级设置 -->
          <a-collapse class="migration-advanced">
            <a-collapse-item key="advanced" header="高级设置">
              <div class="adv-section-title">对象选项</div>
              <a-row :gutter="16">
                <a-col v-if="vmSelectedDst?.db_type !== 'dameng' && vmSelectedDst?.db_type !== 'oscar' && vmSelectedDst?.db_type !== 'oracle'" :span="12">
                  <a-checkbox v-model="viewMigrate.lowerCaseNames" class="adv-check">对象名转小写</a-checkbox>
                </a-col>
                <a-col v-if="vmSelectedDst?.db_type === 'oscar' || vmSelectedDst?.db_type === 'oracle'" :span="12">
                  <span class="adv-check">{{ vmSelectedDst?.db_type === 'oracle' ? 'Oracle' : 'Oscar' }} 对象名统一转大写</span>
                </a-col>
                <a-col :span="12">
                  <a-checkbox v-model="viewMigrate.changeOwner" :disabled="vmSelectedDst?.db_type === 'oracle' || vmSelectedDst?.db_type === 'sqlserver'" class="adv-check">更改对象 owner 为 Schema 同名角色</a-checkbox>
                </a-col>
              </a-row>
              <div class="adv-section-title">视图剥离模式名</div>
              <a-input
                v-model="viewMigrate.stripViewSchemas"
                placeholder="逗号分隔，如 financeplatform_3.0, otherdb"
                allow-clear
              />
              <div class="adv-hint">从视图定义中去除这些模式名前缀（忽略大小写）。用于跨库引用导致目标库找不到 schema 的场景。</div>
            </a-collapse-item>
          </a-collapse>

          </section>

          <PGMySQLPreflightPanel v-if="isPGMySQLSelection('views')" :result="pgChecks.views" :excluded="pgExclusions.views" :busy="pgBusy.views"
            @check="runPGCheck('views')" @exclude="excludePGObject('views', $event)" @restore="restorePGObject('views', $event)" />

          <!-- 结果 -->
          <div v-if="viewMigrate.results.length > 0" class="migration-surface migration-results">
            <div class="migration-section-heading"><h3>迁移结果</h3></div>
            <a-space style="margin-bottom: 8px">
              <a-tag color="green">成功 {{ vmSuccessCount }}</a-tag>
              <a-tag :color="vmFailCount > 0 ? 'red' : 'gray'">失败 {{ vmFailCount }}</a-tag>
            </a-space>
            <a-table
              :data="viewMigrate.results"
              row-key="name"
              :pagination="{ pageSize: 15, showTotal: true }" :scroll="{ x: 700 }"
              size="small"
              :row-class="record => record.error ? '' : 'migration-row-no-details'"
              :expandable="{ icon: (_: unknown, record: any) => record.error ? undefined : null }"
            >
              <template #columns>
                <a-table-column title="视图名" data-index="name" :width="280" />
                <a-table-column title="结果" :width="100">
                  <template #cell="{ record }">
                    <span :style="record.error ? 'color: rgb(var(--danger-6))' : 'color: rgb(var(--success-6))'">
                      {{ record.error ? '失败' : '成功' }}
                    </span>
                  </template>
                </a-table-column>
                <a-table-column title="错误摘要">
                  <template #cell="{ record }">
                    <span style="color: rgb(var(--danger-6)); font-size: 12px">{{ record.error }}</span>
                  </template>
                </a-table-column>
              </template>
              <template #expand-row="{ record }">
                <div class="view-result-detail">
                  <div v-if="record.error" class="view-result-error">失败原因：{{ record.error }}</div>
                  <pre v-if="record.ddl" class="view-result-ddl">{{ record.ddl }}</pre>
                </div>
              </template>
            </a-table>
          </div>
        <!-- 操作按钮 -->
          <MigrationActionBar :status="viewMigrate.running ? '正在迁移视图' : viewMigrate.results.length ? '执行已结束，请查看结果' : '配置完成后可开始迁移'" :busy="viewMigrate.running">
            <a-button
              type="primary"
              :disabled="!vmCanMigrate"
              :loading="viewMigrate.running"
              @click="handleMigrateViews"
            >开始迁移视图</a-button>
          </MigrationActionBar>

        </a-form>
      </a-tab-pane>

      <a-tab-pane key="object-migrate" title="对象迁移">
        <a-form :model="objMigrate" layout="vertical" class="migration-form">
          <!-- 源库 / 目标库选择 -->
          <div class="migration-connections">
            <div class="migration-connection-side">
              <a-card class="conn-card" :body-style="{ padding: '24px' }">
                <div class="conn-card-header">
                  <a-tag color="orange" size="small">源库</a-tag>
                  <span class="conn-card-title">从哪里迁移</span>
                </div>
                <a-select
                  v-model="srcEnvFilter"
                  placeholder="按环境筛选"
                  style="width: 100%; margin-top: 10px"
                  allow-clear
                >
                  <template #prefix><span class="migration-select-label">环境</span></template>
                  <a-option v-for="e in envHistory" :key="e" :value="e" :label="e" />
                </a-select>
                <a-select
                  v-model="objMigrate.srcConnId"
                  placeholder="选择源库连接"
                  style="width: 100%; margin-top: 10px"
                  allow-search
                  @change="(val) => { omCheckPairSupport(); omLoadSrcDatabases(val as number) }"
                >
                  <template #prefix><span class="migration-select-label">连接</span></template>
                  <a-option v-for="c in srcConnections" :key="c.id" :value="c.id" :label="c.name">
                    <a-tag :color="getDbTypeColor(c.db_type)" size="small" style="margin-right:6px">{{ getDbTypeLabel(c.db_type) }}</a-tag>{{ c.name }}
                  </a-option>
                </a-select>
                <div v-if="omSelectedSrc" class="conn-meta">
                  <span class="conn-meta-item"><span class="conn-meta-label">地址</span>{{ omSelectedSrc.host }}:{{ omSelectedSrc.port }}</span>
                  <span v-if="omSelectedSrc.database" class="conn-meta-item"><span class="conn-meta-label">数据库</span>{{ omSelectedSrc.database }}</span>
                  <span class="conn-meta-item"><span class="conn-meta-label">账号</span>{{ omSelectedSrc.username }}</span>
                </div>
                <a-select
                  v-if="objMigrate.srcDatabases.length > 0"
                  v-model="objMigrate.srcDatabase"
                  placeholder="选择数据库"
                  style="width: 100%; margin-top: 10px"
                  allow-search
                  @change="omLoadTables"
                >
                  <template #prefix><span class="migration-select-label">源数据库</span></template>
                  <a-option v-for="db in objMigrate.srcDatabases" :key="db" :value="db" :label="db" />
                </a-select>
              </a-card>
            </div>
            <div class="migration-connection-arrow">
              <icon-arrow-right style="font-size: 28px; color: #165dff" />
            </div>
            <div class="migration-connection-side">
              <a-card class="conn-card" :body-style="{ padding: '24px' }">
                <div class="conn-card-header">
                  <a-tag color="blue" size="small">目标库</a-tag>
                  <span class="conn-card-title">迁移到哪里</span>
                </div>
                <a-select
                  v-model="dstEnvFilter"
                  placeholder="按环境筛选"
                  style="width: 100%; margin-top: 10px"
                  allow-clear
                >
                  <template #prefix><span class="migration-select-label">环境</span></template>
                  <a-option v-for="e in envHistory" :key="e" :value="e" :label="e" />
                </a-select>
                <a-select
                  v-model="objMigrate.dstConnId"
                  placeholder="选择目标库连接"
                  style="width: 100%; margin-top: 10px"
                  allow-search
                  @change="(val) => { omCheckPairSupport(); omLoadDstSchemas(val as number) }"
                >
                  <template #prefix><span class="migration-select-label">连接</span></template>
                  <a-option v-for="c in pgConnections" :key="c.id" :value="c.id" :label="c.name">
                    <a-tag :color="getDbTypeColor(c.db_type)" size="small" style="margin-right:6px">{{ getDbTypeLabel(c.db_type) }}</a-tag>{{ c.name }}
                  </a-option>
                </a-select>
                <div v-if="omSelectedDst" class="conn-meta">
                  <span class="conn-meta-item"><span class="conn-meta-label">地址</span>{{ omSelectedDst.host }}:{{ omSelectedDst.port }}</span>
                  <span class="conn-meta-item"><span class="conn-meta-label">数据库</span>{{ targetDatabaseName(omSelectedDst, objMigrate.dstSchema) }}</span>
                  <span class="conn-meta-item"><span class="conn-meta-label">账号</span>{{ omSelectedDst.username }}</span>
                </div>
                <a-select
                  v-if="objMigrate.dstConnId"
                  v-model="objMigrate.dstSchema"
                  :placeholder="omSelectedDst?.db_type === 'oracle' ? '留空使用当前登录用户' : '请选择目标 Schema'"
                  :allow-clear="omSelectedDst?.db_type === 'oracle'"
                  style="width: 100%; margin-top: 10px"
                  allow-search
                >
                  <template #prefix><span class="migration-select-label">目标 Schema</span></template>
                  <a-option v-for="s in objMigrate.dstSchemas" :key="s" :value="s" :label="s" />
                </a-select>
                <div v-if="objMigrate.dstConnId" class="schema-permission-tip">
                  <icon-info-circle style="flex-shrink:0" />
                  对象迁移仅向目标库已存在的表补建对象，请确保目标表与引用表已迁移完成。
                </div>
              </a-card>
            </div>
          </div>

          <!-- 不支持提示 -->
          <a-alert v-if="omSelectedSrc?.db_type === 'sqlserver' && omSelectedDst?.db_type === 'dameng'" type="info" style="margin-bottom: 12px">
              仅迁移 dbo 对象，达梦对象名统一大写。不兼容类型、精度和对象会在写入前报错；不自动降级。迁移期间请暂停源表写入。
            </a-alert>
            <a-alert
            v-if="objMigrate.unsupportedMsg"
            type="error"
            style="margin-bottom: 16px"
          >
            {{ objMigrate.unsupportedMsg }}
          </a-alert>

          <section class="migration-surface migration-settings"><div class="migration-section-heading"><h3>迁移设置</h3><span>选择范围与执行选项</span></div>
          <!-- 对象类型选择 -->
          <a-form-item label="迁移对象类型" style="margin-bottom: 16px">
            <a-checkbox-group v-model="objMigrate.objects" class="migration-object-options">
              <a-checkbox v-for="opt in OBJECT_TYPE_OPTIONS" :key="opt.value" :value="opt.value">{{ opt.label }}</a-checkbox>
            </a-checkbox-group>
          </a-form-item>

          <!-- 表选择 -->
          <a-spin :loading="objMigrate.loadingTables" style="display:block">
            <div v-if="objMigrate.tables.length > 0" style="margin-bottom: 16px">
              <a-space style="margin-bottom: 8px" wrap>
                <a-input
                  v-model="objMigrate.search"
                  placeholder="搜索表名"
                  allow-clear
                  style="width: 240px"
                >
                  <template #prefix><icon-search /></template>
                </a-input>
                <a-button size="small" @click="omSelectAllFiltered">全选当前结果</a-button>
                <a-button size="small" @click="omClearSelection">清空选择</a-button>
                <span style="font-size: 12px; color: var(--color-text-3)">
                  已选 {{ objMigrate.selected.length }} / 共 {{ objMigrate.tables.length }}
                </span>
              </a-space>
              <a-table
                :data="omFilteredRows"
                row-key="name"
                :pagination="{ pageSize: 15, showTotal: true }"
                :scroll="{ y: 360 }"
                size="small"
                :row-selection="{ type: 'checkbox', showCheckedAll: true }"
                v-model:selected-keys="objMigrate.selected"
              >
                <template #columns>
                  <a-table-column title="表名" data-index="name" />
                </template>
              </a-table>
            </div>
            <a-empty v-else-if="objMigrate.srcConnId && !objMigrate.loadingTables" description="该连接下没有表，或所选数据库不含表" />
          </a-spin>

          <!-- 手动输入表名 -->
          <a-form-item label="手动追加表名（可选）" style="max-width: 560px; margin-bottom: 16px">
            <a-input
              v-model="objMigrate.manualTables"
              placeholder="逗号分隔表名，与上方勾选合并去重"
              allow-clear
            />
            <template #extra>
              表名需与源库原始大小写一致；将与表格勾选的表合并后一并迁移。
            </template>
          </a-form-item>

          <!-- 高级设置 -->
          <a-collapse class="migration-advanced">
            <a-collapse-item key="advanced" header="高级设置">
              <div class="adv-section-title">对象选项</div>
              <a-row :gutter="16">
                <a-col v-if="omSelectedDst?.db_type !== 'dameng' && omSelectedDst?.db_type !== 'oscar' && omSelectedDst?.db_type !== 'oracle'" :span="12">
                  <a-checkbox v-model="objMigrate.lowerCaseNames" class="adv-check">对象名转小写</a-checkbox>
                </a-col>
                <a-col v-if="omSelectedDst?.db_type === 'oscar' || omSelectedDst?.db_type === 'oracle'" :span="12">
                  <span class="adv-check">{{ omSelectedDst?.db_type === 'oracle' ? 'Oracle' : 'Oscar' }} 对象名统一转大写</span>
                </a-col>
                <a-col :span="12">
                  <a-checkbox v-model="objMigrate.changeOwner" :disabled="omSelectedDst?.db_type === 'oracle' || omSelectedDst?.db_type === 'sqlserver'" class="adv-check">更改对象 owner 为 Schema 同名角色</a-checkbox>
                </a-col>
                <a-col :span="12">
                  <a-checkbox v-model="objMigrate.distributed" :disabled="omSelectedDst?.db_type === 'oscar' || omSelectedDst?.db_type === 'oracle' || omSelectedDst?.db_type === 'sqlserver'" class="adv-check">分布式库（建主键前设置分布列）</a-checkbox>
                </a-col>
              </a-row>
            </a-collapse-item>
          </a-collapse>

          </section>

          <PGMySQLPreflightPanel v-if="isPGMySQLSelection('objects')"
            :result="pgTaskSummaries.objects ? (pgTaskSummaries.objects.preflight ?? pgTaskSummaries.objects.preview) : pgChecks.objects"
            :compact="!!pgTaskSummaries.objects" :summary-source="pgTaskSummaries.objects?.preflight ? 'report' : 'page'"
            :excluded="pgExclusions.objects" :busy="pgBusy.objects" :disabled="objMigrate.running"
            @view-report="showPGTaskResult('objects', 'report')" @view-logs="showPGTaskResult('objects', 'logs')"
            @check="runPGCheck('objects')" @exclude="excludePGObject('objects', $event)" @restore="restorePGObject('objects', $event)" />

          <section ref="objectsResultsSection" tabindex="-1" v-if="objMigrate.running || objMigrate.logs.length || objMigrate.currentJobId" class="migration-surface migration-results">
            <div class="migration-section-heading"><h3>执行结果</h3><span>{{ objMigrate.running ? '迁移进行中' : '查看日志与迁移报告' }}</span></div>
            <a-tabs v-model:active-key="objMigrateResultTab" :destroy-on-hide="false" :lazy-load="false">
              <a-tab-pane key="logs" title="执行日志">
                <a-empty v-if="!objMigrate.logs.length" description="等待迁移日志…" />
                <!-- 日志区 -->
                <div v-show="objMigrate.logs.length">
                  <a-space style="margin-bottom: 8px">
                    <span style="font-weight:500">迁移日志</span>
                    <a-button size="mini" @click="copyObjLogs">复制日志</a-button>
                  </a-space>
                  <div ref="objLogContainer" class="migration-log-container">
                    <div
                      v-for="(line, i) in objMigrate.logs"
                      :key="i"
                      :class="getLogClass(line)"
                      class="log-line"
                    >{{ line }}</div>
                  </div>
                </div>
              </a-tab-pane>
              <a-tab-pane v-if="objMigrate.finished && objMigrate.currentJobId" key="report" title="迁移报告">
                <!-- 迁移报告 -->
                <div v-if="objMigrate.finished && objMigrate.currentJobId" style="margin-top: 16px">
                  <MigrationReportPanel ref="objectReportPanel" :key="objMigrate.currentJobId" :jobID="objMigrate.currentJobId" @loaded="(jobID, report) => onPGReportLoaded('objects', jobID, report)" />
                </div>
              </a-tab-pane>
            </a-tabs>
          </section>
          <!-- 操作按钮 -->
          <MigrationActionBar :status="objMigrate.running ? '正在迁移，请关注执行日志' : objMigrate.finished ? '执行已结束，请查看日志与报告' : '配置完成后可开始迁移'" :busy="objMigrate.running">
            <a-button
              type="primary"
              :disabled="!omCanMigrate"
              :loading="objMigrate.running"
              @click="startObjectMigration"
            >开始迁移对象</a-button>
            <a-button
              v-if="objMigrate.running"
              status="danger"
              @click="cancelObjectMigration"
            >停止迁移</a-button>
            <a-button
              v-if="objMigrate.finished"
              @click="resetObjectMigration"
            >重新迁移</a-button>
          </MigrationActionBar>

        </a-form>
      </a-tab-pane>

    </a-tabs>

    <a-modal
      v-model:visible="dataMigrationConfirmVisible"
      title="确认开始迁移"
      modal-class="migration-confirm-modal"
      width="min(560px, calc(100vw - 32px))"
      :mask-closable="false"
      :closable="true"
      :esc-to-close="true"
      ok-text="确认并开始"
      cancel-text="取消"
      :ok-button-props="{ status: dataMigrate.content === 'data_only' ? 'normal' : 'danger' }"
      @ok="confirmDataMigration"
    >
      <dl class="migration-confirm-summary">
        <div><dt>目标连接</dt><dd>{{ selectedDst?.name }} <span class="migration-confirm-address">{{ selectedDst?.host }}:{{ selectedDst?.port }}</span></dd></div>
        <div><dt>目标数据库</dt><dd>{{ selectedDst ? targetDatabaseName(selectedDst, dataMigrate.dstSchema) : '未选择' }}</dd></div>
        <div v-if="selectedDst?.db_type !== 'mysql'"><dt>目标 Schema</dt><dd>{{ dataMigrate.dstSchema || '默认 Schema' }}</dd></div>
        <div><dt>迁移内容</dt><dd>{{ dataMigrationContentLabel }}</dd></div>
        <div><dt>迁移范围</dt><dd>{{ dataMigrationScopeLabel }}</dd></div>
      </dl>
      <a-alert :type="dataMigrate.content === 'data_only' ? 'info' : 'warning'">
        <template v-if="dataMigrate.content === 'data_only'">
          本次迁移将向目标表写入数据，请确认目标表结构和数据符合迁移要求。
        </template>
        <template v-else>
          本次迁移范围内的目标同名表将<strong>被删除重建</strong>，<strong>现有数据会丢失</strong>，请确认已完成备份。
        </template>
      </a-alert>
    </a-modal>
  </div>
</template>

<script setup lang="ts">
import { ref, reactive, computed, watch, nextTick, onMounted, onUnmounted } from 'vue'
import { onBeforeRouteLeave, useRoute } from 'vue-router'
import { Message, Modal } from '@arco-design/web-vue'
import { getDbTypeColor, getDbTypeLabel } from '@/utils/dbType'
import { applyTargetMigrationDefaults } from '@/utils/migrationOptions'
import { copyText } from '@/utils/clipboard'
import MigrationActionBar from '@/components/MigrationActionBar.vue'
import MigrationReportPanel from './MigrationReportPanel.vue'
import PGMySQLPreflightPanel from './PGMySQLPreflightPanel.vue'
import IncrementalMigrationPanel from './IncrementalMigrationPanel.vue'
import { listConnections, listConnectionDatabases, listConnectionSchemas, listConnectionViews, type Connection } from '@/api/connections'
import { downloadRoutines } from '@/api/schema'
import {
  getSupportedPairs,
  preflightDataMigration,
  type PGMySQLPreflight,
  type PGMySQLPreflightRequest,
  type MigrationObjectRef,
  type MigrationReport,
  startDataMigration as apiStartMigration,
  cancelDataMigration as apiCancelMigration,
  createDataMigrateEventSource,
  migrateViews as apiMigrateViews,
  listConnectionTables,
  startObjectMigration as apiStartObjectMigration,
  type MigrateObjectType,
  type SupportedPair,
  type ObjectResult,
} from '@/api/migration'

const activeTab = ref('data-migrate')

const route = useRoute()

// ===== 数据迁移 Tab =====
const connections = ref<Connection[]>([])
const supportedPairs = ref<SupportedPair[]>([])
const logContainer = ref<HTMLElement | null>(null)
let currentEventSource: EventSource | null = null

const dataMigrate = reactive({
  srcConnId: undefined as number | undefined,
  dstConnId: undefined as number | undefined,
  srcDatabase: '',
  srcDatabases: [] as string[],
  dstSchema: '',
  dstSchemas: [] as string[],
  mode: 'all' as 'all' | 'include' | 'exclude',
  filter: '',
  content: 'both' as 'both' | 'schema_only' | 'data_only',
  pageSize: 20000,
  maxParallel: 10,
  intraTableParallel: 8,
  lowerCaseNames: true,
  charInLength: false,
  useNvarchar2: false,
  distributed: false,
  changeOwner: false,
  stripViewSchemas: '',
  srcMaxOpenConns: 50,
  srcMaxIdleConns: 25,
  srcConnMaxLifetime: 3600,
  dstMaxOpenConns: 50,
  dstMaxIdleConns: 25,
  dstConnMaxLifetime: 3600,
  running: false,
  finished: false,
  logs: [] as string[],
  unsupportedMsg: '',
  currentJobId: '',
})

const tableFilterError = ref('')
const dataMigrationConfirmVisible = ref(false)
const dataMigrationStarting = ref(false)
const dataMigrationContentLabel = computed(() => ({
  both: '表结构 + 数据行',
  schema_only: '仅创建表结构',
  data_only: '仅迁移数据行',
}[dataMigrate.content]))
const dataMigrationScopeLabel = computed(() => {
  if (dataMigrate.mode === 'all') return '全库迁移'
  const label = dataMigrate.mode === 'include' ? '仅迁移指定表' : '排除指定表'
  return `${label}：${dataMigrate.filter || '未指定表名'}`
})

function validateTableFilter(): boolean {
  if (!dataMigrate.filter || dataMigrate.mode === 'all') {
    tableFilterError.value = ''
    return true
  }
  const parts = dataMigrate.filter.split(',')
  for (const part of parts) {
    const trimmed = part.trim()
    if (trimmed && !/^[a-zA-Z0-9_*%\-]+$/.test(trimmed)) {
      tableFilterError.value = '表名只能包含字母、数字、下划线和通配符 *，分隔符只能使用英文逗号'
      return false
    }
  }
  tableFilterError.value = ''
  return true
}

// 按环境筛选（源库 / 目标库各自独立，三个 Tab 共用，与连接管理页逻辑一致）
const srcEnvFilter = ref<string | undefined>(undefined)
const dstEnvFilter = ref<string | undefined>(undefined)
// envHistory：从已有连接里去重取出用过的环境值，供筛选下拉使用
const envHistory = computed(() => {
  const set = new Set(connections.value.map((c) => c.env).filter((v): v is string => !!v))
  return Array.from(set)
})

const srcConnections = computed(() =>
  connections.value.filter(
    (c) =>
      (c.db_type === 'mysql' || c.db_type === 'sqlserver' || c.db_type === 'dameng' || c.db_type === 'oracle' || c.db_type === 'postgres' || c.db_type === 'gaussdb' || c.db_type === 'highgo' || c.db_type === 'vastbase' || c.db_type === 'gbase' || c.db_type === 'seabox' || c.db_type === 'kingbase') &&
      (!srcEnvFilter.value || c.env === srcEnvFilter.value)
  )
)
const pgConnections = computed(() =>
  connections.value.filter(
    (c) =>
      (c.db_type === 'postgres' || c.db_type === 'gaussdb' || c.db_type === 'seabox' || c.db_type === 'dameng' || c.db_type === 'highgo' || c.db_type === 'vastbase' || c.db_type === 'gbase' || c.db_type === 'kingbase' || c.db_type === 'mysql' || c.db_type === 'oscar' || c.db_type === 'oracle' || c.db_type === 'sqlserver') &&
      (!dstEnvFilter.value || c.env === dstEnvFilter.value)
  )
)
const selectedSrc = computed(() =>
  connections.value.find((c) => c.id === dataMigrate.srcConnId)
)
// 支持导出函数/存储过程/触发器的源库类型（与后端可选导出接口实现保持一致）
const ROUTINE_EXPORT_TYPES = ['mysql', 'oracle', 'sqlserver', 'dameng', 'postgres', 'gaussdb', 'highgo', 'vastbase', 'gbase', 'seabox', 'kingbase']
const canExportRoutines = computed(() =>
  !!selectedSrc.value && ROUTINE_EXPORT_TYPES.includes(selectedSrc.value.db_type)
)
const exportingRoutines = ref(false)
const handleExportRoutines = async () => {
  if (!dataMigrate.srcConnId || !dataMigrate.srcDatabase) return
  exportingRoutines.value = true
  try {
    await downloadRoutines(dataMigrate.srcConnId, dataMigrate.srcDatabase)
    Message.success('导出成功')
  } catch (e: any) {
    Message.error(`导出失败: ${e?.message ?? e}`)
  } finally {
    exportingRoutines.value = false
  }
}
// MySQL 的目标 Schema 就是实际写入的数据库，可能与连接默认库不同。
function targetDatabaseName(connection: Connection, targetSchema: string): string {
  if (connection.db_type === 'mysql') {
    return targetSchema || connection.database || '未选择'
  }
  return connection.database || '未配置'
}

const selectedDst = computed(() =>
  connections.value.find((c) => c.id === dataMigrate.dstConnId)
)

watch(() => dataMigrate.dstConnId, (newId) => {
  const dst = connections.value.find((c) => c.id === newId)
  applyTargetMigrationDefaults(dataMigrate, dst?.db_type)
  dataMigrate.charInLength = dst?.db_type === 'dameng' || dst?.db_type === 'oracle'
})

const canStartMigration = computed(() =>
  dataMigrate.srcConnId !== undefined &&
  dataMigrate.dstConnId !== undefined &&
  dataMigrate.srcDatabase !== '' &&
  !dataMigrate.unsupportedMsg &&
  !dataMigrate.running &&
  !dataMigrationConfirmVisible.value &&
  !dataMigrationStarting.value
)

function checkPairSupport() {
  if (!dataMigrate.srcConnId || !dataMigrate.dstConnId) {
    dataMigrate.unsupportedMsg = ''
    return
  }
  const src = connections.value.find((c) => c.id === dataMigrate.srcConnId)
  const dst = connections.value.find((c) => c.id === dataMigrate.dstConnId)
  if (!src || !dst) return
  const supported = supportedPairs.value.some(
    (p) => p.source === src.db_type && p.target === dst.db_type
  )
  dataMigrate.unsupportedMsg = supported
    ? ''
    : `当前不支持 ${src.db_type} → ${dst.db_type} 的数据迁移`
}

async function loadSrcDatabases(connId: number) {
  dataMigrate.srcDatabase = ''
  dataMigrate.srcDatabases = []
  try {
    const res = await listConnectionDatabases(connId)
    dataMigrate.srcDatabases = res.data ?? []
  } catch (e: any) {
    // 400 表示该类型不支持列库,静默忽略;其他错误(如密码错误)提示用户
    if (e?.response?.status !== 400) {
      Message.error(`获取源数据库列表失败: ${e?.response?.data?.error ?? e?.message ?? '连接失败,请检查连接配置'}`)
    }
  }
}

async function loadDstSchemas(connId: number) {
  dataMigrate.dstSchema = ''
  dataMigrate.dstSchemas = []
  const dst = connections.value.find((c) => c.id === connId)
  if (!dst || (dst.db_type !== 'postgres' && dst.db_type !== 'gaussdb' && dst.db_type !== 'seabox' && dst.db_type !== 'dameng' && dst.db_type !== 'highgo' && dst.db_type !== 'vastbase' && dst.db_type !== 'gbase' && dst.db_type !== 'kingbase' && dst.db_type !== 'mysql' && dst.db_type !== 'oscar' && dst.db_type !== 'oracle' && dst.db_type !== 'sqlserver')) return
  try {
    const res = await listConnectionSchemas(connId)
    dataMigrate.dstSchemas = res.data ?? []
    if (dst.db_type === 'sqlserver' && dataMigrate.dstSchemas.includes('dbo')) dataMigrate.dstSchema = 'dbo'
  } catch (e: any) {
    Message.error(`获取目标 Schema 失败: ${e?.response?.data?.error ?? e?.message ?? '连接失败,请检查连接配置'}`)
  }
}

function getLogClass(line: string): string {
  if (line.includes('[ERROR]')) return 'log-error'
  if (line.includes('[WARN]')) return 'log-warn'
  if (line.includes('[DONE]')) return 'log-done'
  return ''
}

function requestDataMigration() {
  if (!canStartMigration.value) return
  if (!validateTableFilter()) return
  if (dataMigrate.dstConnId && !dataMigrate.dstSchema && selectedDst.value?.db_type !== 'oracle') {
    Message.error('请选择目标 Schema')
    return
  }
  dataMigrationConfirmVisible.value = true
}

async function confirmDataMigration() {
  if (!dataMigrationConfirmVisible.value || dataMigrationStarting.value || dataMigrate.running) return
  dataMigrationConfirmVisible.value = false
  dataMigrationStarting.value = true
  try {
    await startDataMigration()
  } finally {
    dataMigrationStarting.value = false
  }
}

async function startDataMigration() {
  if (!validateTableFilter()) return
  if (dataMigrate.dstConnId && !dataMigrate.dstSchema && selectedDst.value?.db_type !== 'oracle') {
    Message.error('请选择目标 Schema')
    return
  }
  dataMigrate.running = true
  dataMigrate.finished = false
  dataMigrate.logs = []
  dataMigrate.currentJobId = ''
  pgSubmissions.data = null
  try {
    if (isPGMySQLSelection('data') && !(await ensurePGCheck('data'))?.ready) { dataMigrate.running = false; return }
    const submitted = preparePGSubmission('data')
    const res = await apiStartMigration(dataMigrationRequest())
    dataMigrate.currentJobId = res.data.job_id
    savePGSubmission('data', res.data.job_id, submitted)
    connectSSE(res.data.job_id)
  } catch (e: any) {
    dataMigrate.logs.push(`[ERROR] 启动失败: ${e?.response?.data?.error ?? e?.message ?? e}`)
    dataMigrate.running = false
    dataMigrate.finished = true
  }
}

function connectSSE(jobID: string) {
  currentEventSource = createDataMigrateEventSource(jobID)
  currentEventSource.addEventListener('message', (e) => {
    if (e.data === '[STREAM_END]') {
      dataMigrate.running = false
      dataMigrate.finished = true
      currentEventSource?.close()
      currentEventSource = null
      return
    }
    dataMigrate.logs.push(e.data)
    nextTick(() => {
      if (logContainer.value) {
        logContainer.value.scrollTop = logContainer.value.scrollHeight
      }
    })
  })
  currentEventSource.onerror = () => {
    dataMigrate.logs.push('[ERROR] 日志流连接中断，请查看历史任务获取详情')
    dataMigrate.running = false
    dataMigrate.finished = true
    currentEventSource?.close()
    currentEventSource = null
  }
}

async function cancelDataMigration() {
  if (!dataMigrate.currentJobId) return
  try {
    await apiCancelMigration(dataMigrate.currentJobId)
  } catch {
    // 取消失败时 SSE 自然会断开
  }
}

function resetDataMigration() {
  dataMigrate.running = false
  dataMigrate.finished = false
  dataMigrate.logs = []
  dataMigrate.currentJobId = ''
  clearPGCheck('data', true)
}

function copyLogs() {
  copyText(dataMigrate.logs.join('\n')).catch(() => Message.error('复制失败'))
}

function handleBeforeUnload(e: BeforeUnloadEvent) {
  if (dataMigrate.running) {
    e.preventDefault()
    e.returnValue = ''
  }
}

onBeforeRouteLeave(() => {
  if (!dataMigrate.running) return true
  return new Promise<boolean>((resolve) => {
    Modal.confirm({
      title: '迁移正在进行中',
      content: '离开页面后迁移将继续在后台运行，但您将无法在此页面查看进度。确定要离开吗？',
      okText: '确定离开',
      cancelText: '留在此页',
      maskClosable: false,
      onOk: () => resolve(true),
      onCancel: () => resolve(false),
    })
  })
})

onMounted(async () => {
  window.addEventListener('beforeunload', handleBeforeUnload)
  const [connsRes, pairsRes] = await Promise.all([
    listConnections(),
    getSupportedPairs(),
  ])
  connections.value = connsRes.data
  supportedPairs.value = pairsRes.data

  // 从工单「一键迁移」跳转而来时，按 query 预选源/目标连接并触发与手动选择一致的联动。
  const srcQ = Number(route.query.src)
  const dstQ = Number(route.query.dst)
  const srcDbQ = typeof route.query.srcdb === 'string' ? route.query.srcdb : ''
  if (srcQ && connections.value.some((c) => c.id === srcQ)) {
    dataMigrate.srcConnId = srcQ
    checkPairSupport()
    // 等源库数据库列表加载完成后，再按工单携带的库名预选（目标 schema 不预选）。
    await loadSrcDatabases(srcQ)
    if (srcDbQ) {
      // 精确匹配 → 忽略大小写匹配 → 兜底直接塞入并选中，确保工单库名一定带入。
      const exact = dataMigrate.srcDatabases.find((db) => db === srcDbQ)
      const ci = exact ?? dataMigrate.srcDatabases.find((db) => db.toLowerCase() === srcDbQ.toLowerCase())
      if (ci) {
        dataMigrate.srcDatabase = ci
      } else {
        dataMigrate.srcDatabases = [srcDbQ, ...dataMigrate.srcDatabases]
        dataMigrate.srcDatabase = srcDbQ
      }
    }
  }
  if (dstQ && connections.value.some((c) => c.id === dstQ)) {
    dataMigrate.dstConnId = dstQ
    checkPairSupport()
    loadDstSchemas(dstQ)
  }
})

onUnmounted(() => {
  window.removeEventListener('beforeunload', handleBeforeUnload)
  currentEventSource?.close()
  currentEventSource = null
  objEventSource?.close()
  objEventSource = null
})

// ===== 视图迁移 Tab =====
const viewMigrate = reactive({
  srcConnId: undefined as number | undefined,
  dstConnId: undefined as number | undefined,
  srcDatabase: '',
  srcDatabases: [] as string[],
  dstSchema: '',
  dstSchemas: [] as string[],
  views: [] as string[],
  selected: [] as string[],
  search: '',
  lowerCaseNames: true,
  changeOwner: false,
  stripViewSchemas: '',
  loadingViews: false,
  running: false,
  unsupportedMsg: '',
  results: [] as ObjectResult[],
})

const vmSelectedSrc = computed(() => connections.value.find((c) => c.id === viewMigrate.srcConnId))
const vmSelectedDst = computed(() => connections.value.find((c) => c.id === viewMigrate.dstConnId))

const vmFilteredRows = computed(() => {
  const kw = viewMigrate.search.trim().toLowerCase()
  const list = kw ? viewMigrate.views.filter((v) => v.toLowerCase().includes(kw)) : viewMigrate.views
  return list.map((name) => ({ name }))
})

const vmSuccessCount = computed(() => viewMigrate.results.filter((r) => !r.error).length)
const vmFailCount = computed(() => viewMigrate.results.filter((r) => r.error).length)

const vmCanMigrate = computed(() =>
  viewMigrate.srcConnId !== undefined &&
  viewMigrate.dstConnId !== undefined &&
  viewMigrate.selected.length > 0 &&
  !viewMigrate.unsupportedMsg &&
  !viewMigrate.running
)

watch(() => viewMigrate.dstConnId, (newId) => {
  const dst = connections.value.find((c) => c.id === newId)
  applyTargetMigrationDefaults(viewMigrate, dst?.db_type)
  if (vmSelectedSrc.value?.db_type === 'sqlserver') void vmLoadViews()
})

function vmCheckPairSupport() {
  if (!viewMigrate.srcConnId || !viewMigrate.dstConnId) {
    viewMigrate.unsupportedMsg = ''
    return
  }
  const src = connections.value.find((c) => c.id === viewMigrate.srcConnId)
  const dst = connections.value.find((c) => c.id === viewMigrate.dstConnId)
  if (!src || !dst) return
  const supported = supportedPairs.value.some((p) => p.source === src.db_type && p.target === dst.db_type)
  viewMigrate.unsupportedMsg = supported ? '' : `当前不支持 ${src.db_type} → ${dst.db_type} 的视图迁移`
}

async function vmLoadSrcDatabases(connId: number) {
  viewMigrate.srcDatabase = ''
  viewMigrate.srcDatabases = []
  viewMigrate.views = []
  viewMigrate.selected = []
  try {
    const res = await listConnectionDatabases(connId)
    viewMigrate.srcDatabases = res.data ?? []
  } catch (e: any) {
    // 400 表示该类型不支持列库,静默忽略;其他错误(如密码错误)提示用户
    if (e?.response?.status !== 400) {
      Message.error(`获取源数据库列表失败: ${e?.response?.data?.error ?? e?.message ?? '连接失败,请检查连接配置'}`)
    }
  }
  // 无可选库列表时（如 oracle），直接按连接默认库加载视图
  if (viewMigrate.srcDatabases.length === 0) await vmLoadViews()
}

async function vmLoadDstSchemas(connId: number) {
  viewMigrate.dstSchema = ''
  viewMigrate.dstSchemas = []
  const dst = connections.value.find((c) => c.id === connId)
  if (!dst || (dst.db_type !== 'postgres' && dst.db_type !== 'gaussdb' && dst.db_type !== 'seabox' && dst.db_type !== 'dameng' && dst.db_type !== 'highgo' && dst.db_type !== 'vastbase' && dst.db_type !== 'gbase' && dst.db_type !== 'kingbase' && dst.db_type !== 'mysql' && dst.db_type !== 'oscar' && dst.db_type !== 'oracle' && dst.db_type !== 'sqlserver')) return
  try {
    const res = await listConnectionSchemas(connId)
    viewMigrate.dstSchemas = res.data ?? []
    if (dst.db_type === 'sqlserver' && viewMigrate.dstSchemas.includes('dbo')) viewMigrate.dstSchema = 'dbo'
  } catch (e: any) {
    Message.error(`获取目标 Schema 失败: ${e?.response?.data?.error ?? e?.message ?? '连接失败,请检查连接配置'}`)
  }
}

let vmLoadViewsRequest = 0
async function vmLoadViews() {
  const request = ++vmLoadViewsRequest
  if (!viewMigrate.srcConnId) return
  viewMigrate.loadingViews = true
  viewMigrate.views = []
  viewMigrate.selected = []
  viewMigrate.results = []
  try {
    const res = await listConnectionViews(viewMigrate.srcConnId, viewMigrate.srcDatabase || undefined, vmSelectedDst.value?.db_type)
    if (request !== vmLoadViewsRequest) return
    viewMigrate.views = res.data ?? []
  } catch (e: any) {
    if (request !== vmLoadViewsRequest) return
    Message.error(`加载视图失败: ${e?.response?.data?.error ?? e?.message ?? e}`)
  } finally {
    if (request === vmLoadViewsRequest) viewMigrate.loadingViews = false
  }
}

function vmSelectAllFiltered() {
  const set = new Set(viewMigrate.selected)
  for (const row of vmFilteredRows.value) set.add(row.name)
  viewMigrate.selected = Array.from(set)
}

function vmClearSelection() {
  viewMigrate.selected = []
}

async function handleMigrateViews() {
  if (viewMigrate.dstConnId && !viewMigrate.dstSchema && vmSelectedDst.value?.db_type !== 'oracle') {
    Message.error('请选择目标 Schema')
    return
  }
  viewMigrate.running = true
  viewMigrate.results = []
  try {
    if (isPGMySQLSelection('views') && !(await ensurePGCheck('views'))?.ready) return
    const res = await apiMigrateViews({
      excluded_objects: pgExclusions.views,
      src_conn_id: viewMigrate.srcConnId!,
      dst_conn_id: viewMigrate.dstConnId!,
      view_names: viewMigrate.selected,
      src_database: viewMigrate.srcDatabase || undefined,
      target_schema: viewMigrate.dstSchema || undefined,
      lower_case_names: viewMigrate.lowerCaseNames,
      change_owner: viewMigrate.changeOwner,
      strip_view_schemas: viewMigrate.stripViewSchemas || undefined,
    })
    viewMigrate.results = res.data.results ?? []
    const fail = viewMigrate.results.filter((r) => r.error).length
    if (fail > 0) Message.warning(`迁移完成，${fail} 个视图失败`)
    else Message.success(`成功迁移 ${viewMigrate.results.length} 个视图`)
  } catch (e: any) {
    Message.error(`迁移失败: ${e?.response?.data?.error ?? e?.message ?? e}`)
  } finally {
    viewMigrate.running = false
  }
}

// ===== 对象迁移 Tab（主键/索引/序列/外键/注释）=====
const OBJECT_TYPE_OPTIONS: { label: string; value: MigrateObjectType }[] = [
  { label: '主键', value: 'primary_keys' },
  { label: '索引', value: 'indexes' },
  { label: '序列（自增列）', value: 'sequences' },
  { label: '外键', value: 'foreign_keys' },
  { label: '注释', value: 'comments' },
]

const objMigrate = reactive({
  srcConnId: undefined as number | undefined,
  dstConnId: undefined as number | undefined,
  srcDatabase: '',
  srcDatabases: [] as string[],
  dstSchema: '',
  dstSchemas: [] as string[],
  tables: [] as string[],          // 源库全部表名
  selected: [] as string[],        // 表格勾选的表
  manualTables: '',                // 手动输入的逗号分隔表名
  search: '',
  objects: [] as MigrateObjectType[],
  lowerCaseNames: true,
  changeOwner: false,
  distributed: false,
  loadingTables: false,
  running: false,
  finished: false,
  unsupportedMsg: '',
  logs: [] as string[],
  currentJobId: '',
})

let objEventSource: EventSource | null = null
const objLogContainer = ref<HTMLElement | null>(null)

const omSelectedSrc = computed(() => connections.value.find((c) => c.id === objMigrate.srcConnId))
const omSelectedDst = computed(() => connections.value.find((c) => c.id === objMigrate.dstConnId))

const omFilteredRows = computed(() => {
  const kw = objMigrate.search.trim().toLowerCase()
  const list = kw ? objMigrate.tables.filter((t) => t.toLowerCase().includes(kw)) : objMigrate.tables
  return list.map((name) => ({ name }))
})

// 合并表格勾选与手动输入,去重
const omFinalTables = computed(() => {
  const set = new Set<string>(objMigrate.selected)
  for (const t of objMigrate.manualTables.split(',')) {
    const v = t.trim()
    if (v) set.add(v)
  }
  return Array.from(set)
})

const omCanMigrate = computed(() =>
  objMigrate.srcConnId !== undefined &&
  objMigrate.dstConnId !== undefined &&
  omFinalTables.value.length > 0 &&
  objMigrate.objects.length > 0 &&
  !objMigrate.unsupportedMsg &&
  !objMigrate.running
)

watch(() => objMigrate.dstConnId, (newId) => {
  const dst = connections.value.find((c) => c.id === newId)
  applyTargetMigrationDefaults(objMigrate, dst?.db_type)
  if (omSelectedSrc.value?.db_type === 'sqlserver') void omLoadTables()
})

function omCheckPairSupport() {
  if (!objMigrate.srcConnId || !objMigrate.dstConnId) {
    objMigrate.unsupportedMsg = ''
    return
  }
  const src = connections.value.find((c) => c.id === objMigrate.srcConnId)
  const dst = connections.value.find((c) => c.id === objMigrate.dstConnId)
  if (!src || !dst) return
  const supported = supportedPairs.value.some((p) => p.source === src.db_type && p.target === dst.db_type)
  objMigrate.unsupportedMsg = supported ? '' : `当前不支持 ${src.db_type} → ${dst.db_type} 的对象迁移`
}

async function omLoadSrcDatabases(connId: number) {
  objMigrate.srcDatabase = ''
  objMigrate.srcDatabases = []
  objMigrate.tables = []
  objMigrate.selected = []
  try {
    const res = await listConnectionDatabases(connId)
    objMigrate.srcDatabases = res.data ?? []
  } catch (e: any) {
    // 400 表示该类型不支持列库,静默忽略;其他错误(如密码错误)提示用户
    if (e?.response?.status !== 400) {
      Message.error(`获取源数据库列表失败: ${e?.response?.data?.error ?? e?.message ?? '连接失败,请检查连接配置'}`)
    }
  }
  if (objMigrate.srcDatabases.length === 0) await omLoadTables()
}

async function omLoadDstSchemas(connId: number) {
  objMigrate.dstSchema = ''
  objMigrate.dstSchemas = []
  const dst = connections.value.find((c) => c.id === connId)
  if (!dst || (dst.db_type !== 'postgres' && dst.db_type !== 'gaussdb' && dst.db_type !== 'seabox' && dst.db_type !== 'dameng' && dst.db_type !== 'highgo' && dst.db_type !== 'vastbase' && dst.db_type !== 'gbase' && dst.db_type !== 'kingbase' && dst.db_type !== 'mysql' && dst.db_type !== 'oscar' && dst.db_type !== 'oracle' && dst.db_type !== 'sqlserver')) return
  try {
    const res = await listConnectionSchemas(connId)
    objMigrate.dstSchemas = res.data ?? []
    if (dst.db_type === 'sqlserver' && objMigrate.dstSchemas.includes('dbo')) objMigrate.dstSchema = 'dbo'
  } catch (e: any) {
    Message.error(`获取目标 Schema 失败: ${e?.response?.data?.error ?? e?.message ?? '连接失败,请检查连接配置'}`)
  }
}

let omLoadTablesRequest = 0
async function omLoadTables() {
  const request = ++omLoadTablesRequest
  if (!objMigrate.srcConnId) return
  objMigrate.loadingTables = true
  objMigrate.tables = []
  objMigrate.selected = []
  try {
    const res = await listConnectionTables(objMigrate.srcConnId, objMigrate.srcDatabase || undefined, omSelectedDst.value?.db_type)
    if (request !== omLoadTablesRequest) return
    objMigrate.tables = res.data ?? []
  } catch (e: any) {
    if (request !== omLoadTablesRequest) return
    Message.error(`加载表失败: ${e?.response?.data?.error ?? e?.message ?? e}`)
  } finally {
    if (request === omLoadTablesRequest) objMigrate.loadingTables = false
  }
}

function omSelectAllFiltered() {
  const set = new Set(objMigrate.selected)
  for (const row of omFilteredRows.value) set.add(row.name)
  objMigrate.selected = Array.from(set)
}

function omClearSelection() {
  objMigrate.selected = []
}

async function startObjectMigration() {
  if (objMigrate.dstConnId && !objMigrate.dstSchema && omSelectedDst.value?.db_type !== 'oracle') {
    Message.error('请选择目标 Schema')
    return
  }
  if (omFinalTables.value.length === 0) {
    Message.error('请至少选择一张表')
    return
  }
  if (objMigrate.objects.length === 0) {
    Message.error('请至少选择一种对象类型')
    return
  }
  objMigrate.running = true
  objMigrate.finished = false
  objMigrate.currentJobId = ''
  pgSubmissions.objects = null
  objMigrate.logs = []
  try {
    if (isPGMySQLSelection('objects') && !(await ensurePGCheck('objects'))?.ready) { objMigrate.running = false; return }
    const submitted = preparePGSubmission('objects')
    const res = await apiStartObjectMigration({
      excluded_objects: pgExclusions.objects,
      src_conn_id: objMigrate.srcConnId!,
      dst_conn_id: objMigrate.dstConnId!,
      migrate_objects: objMigrate.objects,
      table_names: omFinalTables.value,
      src_database: objMigrate.srcDatabase || undefined,
      target_schema: objMigrate.dstSchema || undefined,
      lower_case_names: objMigrate.lowerCaseNames,
      change_owner: objMigrate.changeOwner,
      distributed: objMigrate.distributed,
    })
    objMigrate.currentJobId = res.data.job_id
    savePGSubmission('objects', res.data.job_id, submitted)
    omConnectSSE(res.data.job_id)
  } catch (e: any) {
    objMigrate.logs.push(`[ERROR] 启动失败: ${e?.response?.data?.error ?? e?.message ?? e}`)
    objMigrate.running = false
    objMigrate.finished = true
  }
}

function omConnectSSE(jobID: string) {
  objEventSource = createDataMigrateEventSource(jobID)
  objEventSource.addEventListener('message', (e) => {
    if (e.data === '[STREAM_END]') {
      objMigrate.running = false
      objMigrate.finished = true
      objEventSource?.close()
      objEventSource = null
      return
    }
    objMigrate.logs.push(e.data)
    nextTick(() => {
      if (objLogContainer.value) {
        objLogContainer.value.scrollTop = objLogContainer.value.scrollHeight
      }
    })
  })
  objEventSource.onerror = () => {
    objMigrate.logs.push('[ERROR] 日志流连接中断，请查看历史任务获取详情')
    objMigrate.running = false
    objMigrate.finished = true
    objEventSource?.close()
    objEventSource = null
  }
}

async function cancelObjectMigration() {
  if (!objMigrate.currentJobId) return
  try {
    await apiCancelMigration(objMigrate.currentJobId)
  } catch {
    // 取消失败时 SSE 自然会断开
  }
}

function resetObjectMigration() {
  objMigrate.running = false
  objMigrate.finished = false
  objMigrate.logs = []
  objMigrate.currentJobId = ''
  clearPGCheck('objects', true)
}

function copyObjLogs() {
  copyText(objMigrate.logs.join('\n')).catch(() => Message.error('复制失败'))
}


// Preview state is scoped to the three existing migration workflows.
type PGCheckKind = 'data' | 'views' | 'objects'
const pgChecks = reactive<Record<PGCheckKind, PGMySQLPreflight | null>>({ data: null, views: null, objects: null })
const pgExclusions = reactive<Record<PGCheckKind, MigrationObjectRef[]>>({ data: [], views: [], objects: [] })
const pgBusy = reactive<Record<PGCheckKind, boolean>>({ data: false, views: false, objects: false })
const pgAbort = new Map<PGCheckKind, AbortController>()
const pgCheckRequests = new Map<PGCheckKind, string>()
// Bind compact summaries to the submitted configuration and the actual job.
// A later configuration edit permanently restores the editable preview.
type PGTaskKind = 'data' | 'objects'
interface PGSubmission {
  jobID: string
  requestKey: string
  preview: PGMySQLPreflight
  preflight: PGMySQLPreflight | null
}
const pgSubmissions = reactive<Record<PGTaskKind, PGSubmission | null>>({ data: null, objects: null })
const dataReportPanel = ref<InstanceType<typeof MigrationReportPanel> | null>(null)
const objectReportPanel = ref<InstanceType<typeof MigrationReportPanel> | null>(null)
const dataResultsSection = ref<HTMLElement | null>(null)
const objectsResultsSection = ref<HTMLElement | null>(null)
function matchingPGSubmission(kind: PGTaskKind) {
  const state = kind === 'data' ? dataMigrate : objMigrate
  const submitted = pgSubmissions[kind]
  return submitted && submitted.jobID === state.currentJobId && submitted.requestKey === JSON.stringify(pgRequest(kind)) ? submitted : null
}
const pgTaskSummaries = computed(() => ({ data: matchingPGSubmission('data'), objects: matchingPGSubmission('objects') }))
function preparePGSubmission(kind: PGTaskKind): Omit<PGSubmission, 'jobID'> | null {
  const preview = pgChecks[kind]
  const requestKey = JSON.stringify(pgRequest(kind))
  return isPGMySQLSelection(kind) && preview?.ready && pgCheckRequests.get(kind) === requestKey
    ? { requestKey, preview, preflight: null } : null
}
function savePGSubmission(kind: PGTaskKind, jobID: string, submitted: Omit<PGSubmission, 'jobID'> | null) {
  // A configuration change while the start request was in flight invalidates its preview.
  if (submitted && submitted.requestKey === JSON.stringify(pgRequest(kind)) && pgCheckRequests.get(kind) === submitted.requestKey) {
    pgSubmissions[kind] = { ...submitted, jobID }
  }
}
function onPGReportLoaded(kind: PGTaskKind, jobID: string, report: MigrationReport) {
  const state = kind === 'data' ? dataMigrate : objMigrate
  if (state.currentJobId !== jobID) return
  const submitted = matchingPGSubmission(kind)
  if (submitted) submitted.preflight = report.preflight ?? null
  const tab = kind === 'data' ? dataMigrateResultTab : objMigrateResultTab
  tab.value = 'report'
}
async function showPGTaskResult(kind: PGTaskKind, destination: 'logs' | 'report') {
  const state = kind === 'data' ? dataMigrate : objMigrate
  const jobID = state.currentJobId
  if (!jobID) return
  const tab = kind === 'data' ? dataMigrateResultTab : objMigrateResultTab
  tab.value = destination
  await nextTick()
  if (state.currentJobId !== jobID) return
  if (destination === 'report') {
    await (kind === 'data' ? dataReportPanel : objectReportPanel).value?.showPreflight()
  } else {
    const section = (kind === 'data' ? dataResultsSection : objectsResultsSection).value
    section?.focus({ preventScroll: true })
    section?.scrollIntoView({ behavior: 'smooth', block: 'start' })
  }
}

function isPGMySQLSelection(kind: PGCheckKind) {
  const state = kind === 'data' ? dataMigrate : kind === 'views' ? viewMigrate : objMigrate
  const src = connections.value.find(c => c.id === state.srcConnId)
  const dst = connections.value.find(c => c.id === state.dstConnId)
  return !!src && ['postgres', 'gaussdb', 'highgo', 'vastbase', 'gbase', 'seabox', 'kingbase'].includes(src.db_type) && dst?.db_type === 'mysql'
}
function dataMigrationRequest(): PGMySQLPreflightRequest {
return {
      src_conn_id: dataMigrate.srcConnId!,
      dst_conn_id: dataMigrate.dstConnId!,
      migrate_mode: dataMigrate.mode,
      table_filter: dataMigrate.filter,
      migrate_content: dataMigrate.content,
      page_size: dataMigrate.pageSize,
      max_parallel: dataMigrate.maxParallel,
      intra_table_parallel: dataMigrate.intraTableParallel,
      lower_case_names: dataMigrate.lowerCaseNames,
      char_in_length: dataMigrate.charInLength,
      use_nvarchar2: dataMigrate.useNvarchar2,
      distributed: dataMigrate.distributed,
      change_owner: dataMigrate.changeOwner,
      src_database: dataMigrate.srcDatabase,
      target_schema: dataMigrate.dstSchema || undefined,
      strip_view_schemas: dataMigrate.stripViewSchemas || undefined,
      src_max_open_conns: dataMigrate.srcMaxOpenConns || undefined,
      src_max_idle_conns: dataMigrate.srcMaxIdleConns || undefined,
      src_conn_max_lifetime: dataMigrate.srcConnMaxLifetime || undefined,
      dst_max_open_conns: dataMigrate.dstMaxOpenConns || undefined,
      dst_max_idle_conns: dataMigrate.dstMaxIdleConns || undefined,
      dst_conn_max_lifetime: dataMigrate.dstConnMaxLifetime || undefined,
      excluded_objects: pgExclusions.data,
  }
}
function pgRequest(kind: PGCheckKind): PGMySQLPreflightRequest {
  if (kind === 'data') return dataMigrationRequest()
  const state = kind === 'views' ? viewMigrate : objMigrate
  return {
    src_conn_id: state.srcConnId!, dst_conn_id: state.dstConnId!, migrate_mode: 'all',
    src_database: state.srcDatabase || undefined, target_schema: state.dstSchema || undefined,
    lower_case_names: state.lowerCaseNames, change_owner: state.changeOwner,
    excluded_objects: pgExclusions[kind],
    ...(kind === 'views' ? { view_names: viewMigrate.selected, strip_view_schemas: viewMigrate.stripViewSchemas || undefined } : { migrate_objects: objMigrate.objects, table_names: omFinalTables.value, distributed: objMigrate.distributed }),
  }
}
async function ensurePGCheck(kind: PGCheckKind) {
  // Flush configuration watchers before considering a previous successful check.
  // The backend still builds and validates a fresh plan before executing migration.
  await nextTick()
  if (pgChecks[kind]?.ready && pgCheckRequests.get(kind) === JSON.stringify(pgRequest(kind))) return pgChecks[kind]
  return runPGCheck(kind)
}
function clearPGCheck(kind: PGCheckKind, clearExclusions = false) {
  pgAbort.get(kind)?.abort()
  pgAbort.delete(kind)
  pgBusy[kind] = false
  pgChecks[kind] = null
  pgCheckRequests.delete(kind)
  if (kind !== 'views') pgSubmissions[kind] = null
  if (clearExclusions) pgExclusions[kind] = []
}
async function runPGCheck(kind: PGCheckKind) {
  clearPGCheck(kind)
  const request = pgRequest(kind)
  const requestKey = JSON.stringify(request)
  const controller = new AbortController(); pgAbort.set(kind, controller); pgBusy[kind] = true
  try {
    const result = await preflightDataMigration(request, controller.signal)
    if (controller.signal.aborted || requestKey !== JSON.stringify(pgRequest(kind))) return
    pgChecks[kind] = result.data
    pgCheckRequests.set(kind, requestKey)
    return result.data
  } catch (e: any) {
    if (!controller.signal.aborted) Message.error(`预检失败：${e?.response?.data?.error ?? e.message}`)
  } finally {
    if (pgAbort.get(kind) === controller) { pgBusy[kind] = false; pgAbort.delete(kind) }
  }
}
function objectKey(o: MigrationObjectRef) { return JSON.stringify([o.kind, o.table ?? '', o.name]) }
function excludePGObject(kind: PGCheckKind, object: MigrationObjectRef) {
  if (!pgExclusions[kind].some(o => objectKey(o) === objectKey(object))) pgExclusions[kind].push(object)
  pgChecks[kind] = null
}
function restorePGObject(kind: PGCheckKind, object: MigrationObjectRef) {
  pgExclusions[kind] = pgExclusions[kind].filter(o => objectKey(o) !== objectKey(object)); pgChecks[kind] = null
}
for (const kind of ['data', 'views', 'objects'] as PGCheckKind[]) {
  watch(() => JSON.stringify(pgRequest(kind)), () => {
    clearPGCheck(kind)
  })
  watch(() => { const req = pgRequest(kind); return [req.src_conn_id, req.dst_conn_id, req.src_database, req.target_schema].join('|') }, () => { pgExclusions[kind] = [] })
}
onUnmounted(() => { for (const controller of pgAbort.values()) controller.abort() })

const dataMigrateResultTab = ref('logs')
const objMigrateResultTab = ref('logs')
watch(() => dataMigrate.currentJobId, () => { dataMigrateResultTab.value = 'logs' })
watch(() => objMigrate.currentJobId, () => { objMigrateResultTab.value = 'logs' })

</script>

<style scoped>
/* 全局主题覆盖了所有 primary 按钮，在此保留删除重建的危险状态。 */
:global(.migration-confirm-modal .arco-btn-primary.arco-btn-status-danger) {
  background: rgb(var(--danger-6)) !important;
  border-color: rgb(var(--danger-6)) !important;
  box-shadow: none !important;
}
:global(.migration-confirm-modal .arco-btn-primary.arco-btn-status-danger:hover) {
  background: rgb(var(--danger-5)) !important;
  border-color: rgb(var(--danger-5)) !important;
}
:global(.migration-confirm-modal .arco-btn-primary.arco-btn-status-danger:active) {
  background: rgb(var(--danger-7)) !important;
  border-color: rgb(var(--danger-7)) !important;
}
.migration-confirm-summary {
  display: grid;
  gap: 12px;
  margin: 0 0 20px;
  font-size: 13px;
  line-height: 1.6;
}
.migration-confirm-summary > div {
  display: grid;
  grid-template-columns: 88px minmax(0, 1fr);
  gap: 12px;
}
.migration-confirm-summary dt {
  color: var(--color-text-3);
  white-space: nowrap;
}
.migration-confirm-summary dd {
  margin: 0;
  color: var(--color-text-1);
  overflow-wrap: anywhere;
}
.migration-confirm-address {
  display: block;
  color: var(--color-text-3);
  font-size: 12px;
}
/* 高级设置 —— 轻量分组标题，替代厚重的 a-divider */
.adv-section-title {
  font-size: 13px;
  font-weight: 600;
  color: var(--color-text-1);
  margin-bottom: 10px;
}
.adv-section-title:not(:first-child) {
  margin-top: 16px;
}
.adv-section-note {
  font-weight: 400;
  font-size: 12px;
  color: var(--color-text-3);
  margin-left: 6px;
}
.adv-subtitle {
  font-size: 12px;
  color: var(--color-text-3);
  margin-bottom: 8px;
}
.adv-check {
  margin-bottom: 8px;
}
.adv-hint {
  font-size: 12px;
  color: var(--color-text-3);
  line-height: 1.5;
  margin-top: 4px;
}
.migration-log-container {
  background: #1a1a1a;
  color: #d4d4d4;
  font-family: 'Menlo', 'Monaco', 'Courier New', monospace;
  font-size: 12px;
  padding: 12px;
  border-radius: 4px;
  height: 400px;
  overflow-y: auto;
}
.log-line {
  line-height: 1.6;
  white-space: pre-wrap;
  word-break: break-all;
}
.log-error { color: #f47174; }
.log-warn  { color: #e5c07b; }
.log-done  { color: #98c379; }
.conn-card {
  border: 1px solid var(--color-border-2);
  border-radius: 6px;
  height: 100%;
}
.conn-card-header {
  display: flex;
  align-items: center;
  gap: 8px;
}
.conn-card-title {
  font-size: 14px;
  font-weight: 600;
  color: var(--color-text-1);
}
.conn-meta {
  display: flex;
  flex-wrap: wrap;
  gap: 6px 12px;
  margin-top: 8px;
}
.conn-meta-item {
  font-size: 12px;
  color: var(--color-text-3);
}
.conn-meta-label {
  color: var(--color-text-4);
  margin-right: 3px;
}
.schema-permission-tip {
  display: flex;
  align-items: flex-start;
  gap: 5px;
  margin-top: 10px;
  padding: 7px 10px;
  background: #fffbe6;
  border: 1px solid #ffe58f;
  border-radius: 4px;
  font-size: 12px;
  color: #7d5a00;
  line-height: 1.5;
}
.report-conn-bar {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 10px 16px;
  margin-bottom: 12px;
  background: var(--color-fill-2);
  border: 1px solid var(--color-border-2);
  border-radius: 6px;
  font-size: 12px;
  flex-wrap: wrap;
}
.report-conn-side {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
  flex: 1;
  min-width: 0;
}
.report-conn-type-tag {
  flex-shrink: 0;
}
.report-conn-name {
  font-size: 13px;
  font-weight: 500;
  color: var(--color-text-1);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
  max-width: 180px;
}
.report-conn-detail {
  color: var(--color-text-3);
  white-space: nowrap;
}
.report-conn-arrow {
  font-size: 18px;
  color: #165dff;
  flex-shrink: 0;
}
.view-result-detail {
  padding: 8px 12px;
}
.view-result-error {
  color: rgb(var(--danger-6));
  font-size: 12px;
  margin-bottom: 6px;
}
.view-result-ddl {
  background: var(--color-fill-2);
  border: 1px solid var(--color-border-2);
  border-radius: 4px;
  padding: 8px 10px;
  font-family: 'Menlo', 'Monaco', 'Courier New', monospace;
  font-size: 12px;
  white-space: pre-wrap;
  word-break: break-all;
  margin: 0;
}
</style>

<style src="@/styles/migration-workspace.css"></style>
