<template>
  <div>
    <h2>迁移 SQL 生成</h2>
    <a-tabs v-model:active-key="activeTab">
      <a-tab-pane key="data-migrate" title="数据迁移">
        <a-form :model="dataMigrate" layout="vertical" style="margin-top: 12px">
          <!-- 源库 / 目标库选择 -->
          <a-row :gutter="20" align="stretch" style="margin-bottom: 16px">
            <a-col :span="11">
              <a-card class="conn-card" :body-style="{ padding: '16px' }">
                <div class="conn-card-header">
                  <a-tag color="orange" size="small">源库</a-tag>
                  <span class="conn-card-title">源库</span>
                </div>
                <a-select
                  v-model="dataMigrate.srcConnId"
                  placeholder="选择源库连接"
                  style="width: 100%; margin-top: 10px"
                  @change="(val) => { checkPairSupport(); loadSrcDatabases(val as number) }"
                >
                  <a-option v-for="c in srcConnections" :key="c.id" :value="c.id" :label="c.name">
                    <a-tag :color="getDbTypeColor(c.db_type)" size="small" style="margin-right:6px">{{ getDbTypeLabel(c.db_type) }}</a-tag>{{ c.name }}
                  </a-option>
                </a-select>
                <div v-if="selectedSrc" class="conn-meta">
                  <span class="conn-meta-item"><span class="conn-meta-label">地址</span>{{ selectedSrc.host }}:{{ selectedSrc.port }}</span>
                  <span class="conn-meta-item"><span class="conn-meta-label">账号</span>{{ selectedSrc.username }}</span>
                </div>
                <a-select
                  v-if="dataMigrate.srcDatabases.length > 0"
                  v-model="dataMigrate.srcDatabase"
                  placeholder="选择要迁移的数据库"
                  style="width: 100%; margin-top: 10px"
                  allow-search
                >
                  <a-option v-for="db in dataMigrate.srcDatabases" :key="db" :value="db" :label="db" />
                </a-select>
              </a-card>
            </a-col>
            <a-col :span="2" style="display:flex;align-items:center;justify-content:center">
              <icon-arrow-right style="font-size: 28px; color: #165dff" />
            </a-col>
            <a-col :span="11">
              <a-card class="conn-card" :body-style="{ padding: '16px' }">
                <div class="conn-card-header">
                  <a-tag color="blue" size="small">目标库</a-tag>
                  <span class="conn-card-title">目标库</span>
                </div>
                <a-select
                  v-model="dataMigrate.dstConnId"
                  placeholder="选择目标库连接"
                  style="width: 100%; margin-top: 10px"
                  @change="(val) => { checkPairSupport(); loadDstSchemas(val as number) }"
                >
                  <a-option v-for="c in pgConnections" :key="c.id" :value="c.id" :label="c.name">
                    <a-tag :color="getDbTypeColor(c.db_type)" size="small" style="margin-right:6px">{{ getDbTypeLabel(c.db_type) }}</a-tag>{{ c.name }}
                  </a-option>
                </a-select>
                <div v-if="selectedDst" class="conn-meta">
                  <span class="conn-meta-item"><span class="conn-meta-label">地址</span>{{ selectedDst.host }}:{{ selectedDst.port }}</span>
                  <span class="conn-meta-item"><span class="conn-meta-label">数据库</span>{{ selectedDst.database }}</span>
                  <span class="conn-meta-item"><span class="conn-meta-label">账号</span>{{ selectedDst.username }}</span>
                </div>
                <a-select
                  v-if="dataMigrate.dstConnId"
                  v-model="dataMigrate.dstSchema"
                  placeholder="请选择目标 Schema"
                  style="width: 100%; margin-top: 10px"
                  allow-search
                >
                  <a-option v-for="s in dataMigrate.dstSchemas" :key="s" :value="s" :label="s" />
                </a-select>
                <div v-if="dataMigrate.dstConnId" class="schema-permission-tip">
                  <icon-info-circle style="flex-shrink:0" />
                  请确保目标 Schema 拥有创建对象的权限，否则请在目标数据库中自行处理模式权限后再执行迁移。
                </div>
              </a-card>
            </a-col>
          </a-row>

          <!-- 不支持提示 -->
          <a-alert
            v-if="dataMigrate.unsupportedMsg"
            type="error"
            style="margin-bottom: 16px"
          >
            {{ dataMigrate.unsupportedMsg }}
          </a-alert>

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
          <a-collapse :default-active-key="['advanced']" style="margin-bottom: 16px; max-width: 560px">
            <a-collapse-item key="advanced" header="高级设置">
              <a-row :gutter="16">
                <a-col :span="12">
                  <a-form-item label="每页行数 (pageSize)">
                    <a-input-number v-model="dataMigrate.pageSize" :min="1000" :max="500000" :step="1000" style="width: 140px" />
                  </a-form-item>
                </a-col>
                <a-col :span="12">
                  <a-form-item label="最大并发数 (maxParallel)">
                    <a-input-number v-model="dataMigrate.maxParallel" :min="1" :max="50" style="width: 140px" />
                  </a-form-item>
                </a-col>
                <a-col :span="12">
                  <a-form-item label="表内分页并发数">
                    <a-input-number v-model="dataMigrate.intraTableParallel" :min="1" :max="20" style="width: 140px" />
                  </a-form-item>
                </a-col>
                <a-col v-if="selectedDst?.db_type !== 'dameng'" :span="12">
                  <a-form-item style="margin-bottom: 4px">
                    <a-checkbox v-model="dataMigrate.lowerCaseNames">对象名转小写</a-checkbox>
                  </a-form-item>
                </a-col>
                <a-col :span="12">
                  <a-form-item style="margin-bottom: 4px">
                    <a-checkbox v-model="dataMigrate.charInLength">char 长度单位（CHAR）</a-checkbox>
                  </a-form-item>
                </a-col>
                <a-col :span="12">
                  <a-form-item style="margin-bottom: 4px">
                    <a-checkbox v-model="dataMigrate.useNvarchar2">使用 nvarchar2</a-checkbox>
                  </a-form-item>
                </a-col>
                <a-col :span="12">
                  <a-form-item style="margin-bottom: 4px">
                    <a-checkbox v-model="dataMigrate.distributed">分布式模式（DISTRIBUTE BY hash）</a-checkbox>
                  </a-form-item>
                </a-col>
                <a-col :span="12">
                  <a-form-item style="margin-bottom: 4px">
                    <a-checkbox v-model="dataMigrate.changeOwner">更改对象 owner 为 Schema 同名角色</a-checkbox>
                  </a-form-item>
                </a-col>
                <a-col :span="24">
                  <a-form-item label="视图剥离模式名">
                    <a-input
                      v-model="dataMigrate.stripViewSchemas"
                      placeholder="逗号分隔，如 financeplatform_3.0, otherdb"
                      allow-clear
                    />
                    <template #extra>
                      迁移视图时，从视图定义中去除这些模式名前缀（忽略大小写）。用于跨库引用导致目标库找不到 schema 的场景。
                    </template>
                  </a-form-item>
                </a-col>
              </a-row>
              <a-divider style="margin: 12px 0 8px">连接池配置（0 表示使用默认值）</a-divider>
              <a-row :gutter="16">
                <a-col :span="24" style="margin-bottom: 8px">
                  <span style="font-size: 12px; color: var(--color-text-3)">源库连接池</span>
                </a-col>
                <a-col :span="8">
                  <a-form-item label="最大连接数">
                    <a-input-number v-model="dataMigrate.srcMaxOpenConns" :min="0" :max="500" placeholder="默认 50" style="width: 120px" />
                  </a-form-item>
                </a-col>
                <a-col :span="8">
                  <a-form-item label="最大空闲连接数">
                    <a-input-number v-model="dataMigrate.srcMaxIdleConns" :min="0" :max="500" placeholder="默认 25" style="width: 120px" />
                  </a-form-item>
                </a-col>
                <a-col :span="8">
                  <a-form-item label="连接生命周期（秒）">
                    <a-input-number v-model="dataMigrate.srcConnMaxLifetime" :min="0" placeholder="默认 3600" style="width: 120px" />
                  </a-form-item>
                </a-col>
                <a-col :span="24" style="margin-bottom: 8px">
                  <span style="font-size: 12px; color: var(--color-text-3)">目标库连接池</span>
                </a-col>
                <a-col :span="8">
                  <a-form-item label="最大连接数">
                    <a-input-number v-model="dataMigrate.dstMaxOpenConns" :min="0" :max="500" placeholder="默认 50" style="width: 120px" />
                  </a-form-item>
                </a-col>
                <a-col :span="8">
                  <a-form-item label="最大空闲连接数">
                    <a-input-number v-model="dataMigrate.dstMaxIdleConns" :min="0" :max="500" placeholder="默认 25" style="width: 120px" />
                  </a-form-item>
                </a-col>
                <a-col :span="8">
                  <a-form-item label="连接生命周期（秒）">
                    <a-input-number v-model="dataMigrate.dstConnMaxLifetime" :min="0" placeholder="默认 3600" style="width: 120px" />
                  </a-form-item>
                </a-col>
              </a-row>
            </a-collapse-item>
          </a-collapse>

          <!-- 目标表删除重建警告 -->
          <a-alert type="warning" style="margin-bottom: 16px">
            <template #title>注意：迁移前目标表将被删除重建</template>
            迁移开始时会对目标库中同名表执行 <strong>DROP TABLE IF EXISTS ... CASCADE</strong>，再重新建表并导入数据。目标表中的现有数据将被清空，请确认目标库中无需保留的数据已备份。
          </a-alert>

          <!-- 操作按钮 -->
          <a-space style="margin-bottom: 16px">
            <a-button
              type="primary"
              :disabled="!canStartMigration"
              :loading="dataMigrate.running"
              @click="startDataMigration"
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
          </a-space>

          <!-- 日志区 -->
          <div v-if="dataMigrate.logs.length > 0">
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

          <!-- 迁移报告 -->
          <div v-if="dataMigrate.finished && dataMigrate.currentJobId" style="margin-top: 16px">
            <a-divider>迁移报告</a-divider>

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
                  <span class="conn-meta-label">库</span>{{ selectedDst.database }}
                </span>
                <span v-if="dataMigrate.dstSchema" class="report-conn-detail">
                  <span class="conn-meta-label">Schema</span>{{ dataMigrate.dstSchema }}
                </span>
              </div>
            </div>

            <MigrationReportPanel :jobID="dataMigrate.currentJobId" />
          </div>
        </a-form>
      </a-tab-pane>

      <a-tab-pane key="view-migrate" title="视图迁移">
        <a-form :model="viewMigrate" layout="vertical" style="margin-top: 12px">
          <!-- 源库 / 目标库选择 -->
          <a-row :gutter="20" align="stretch" style="margin-bottom: 16px">
            <a-col :span="11">
              <a-card class="conn-card" :body-style="{ padding: '16px' }">
                <div class="conn-card-header">
                  <a-tag color="orange" size="small">源库</a-tag>
                  <span class="conn-card-title">源库</span>
                </div>
                <a-select
                  v-model="viewMigrate.srcConnId"
                  placeholder="选择源库连接"
                  style="width: 100%; margin-top: 10px"
                  @change="(val) => { vmCheckPairSupport(); vmLoadSrcDatabases(val as number) }"
                >
                  <a-option v-for="c in srcConnections" :key="c.id" :value="c.id" :label="c.name">
                    <a-tag :color="getDbTypeColor(c.db_type)" size="small" style="margin-right:6px">{{ getDbTypeLabel(c.db_type) }}</a-tag>{{ c.name }}
                  </a-option>
                </a-select>
                <div v-if="vmSelectedSrc" class="conn-meta">
                  <span class="conn-meta-item"><span class="conn-meta-label">地址</span>{{ vmSelectedSrc.host }}:{{ vmSelectedSrc.port }}</span>
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
                  <a-option v-for="db in viewMigrate.srcDatabases" :key="db" :value="db" :label="db" />
                </a-select>
              </a-card>
            </a-col>
            <a-col :span="2" style="display:flex;align-items:center;justify-content:center">
              <icon-arrow-right style="font-size: 28px; color: #165dff" />
            </a-col>
            <a-col :span="11">
              <a-card class="conn-card" :body-style="{ padding: '16px' }">
                <div class="conn-card-header">
                  <a-tag color="blue" size="small">目标库</a-tag>
                  <span class="conn-card-title">目标库</span>
                </div>
                <a-select
                  v-model="viewMigrate.dstConnId"
                  placeholder="选择目标库连接"
                  style="width: 100%; margin-top: 10px"
                  @change="(val) => { vmCheckPairSupport(); vmLoadDstSchemas(val as number) }"
                >
                  <a-option v-for="c in pgConnections" :key="c.id" :value="c.id" :label="c.name">
                    <a-tag :color="getDbTypeColor(c.db_type)" size="small" style="margin-right:6px">{{ getDbTypeLabel(c.db_type) }}</a-tag>{{ c.name }}
                  </a-option>
                </a-select>
                <div v-if="vmSelectedDst" class="conn-meta">
                  <span class="conn-meta-item"><span class="conn-meta-label">地址</span>{{ vmSelectedDst.host }}:{{ vmSelectedDst.port }}</span>
                  <span class="conn-meta-item"><span class="conn-meta-label">数据库</span>{{ vmSelectedDst.database }}</span>
                  <span class="conn-meta-item"><span class="conn-meta-label">账号</span>{{ vmSelectedDst.username }}</span>
                </div>
                <a-select
                  v-if="viewMigrate.dstConnId"
                  v-model="viewMigrate.dstSchema"
                  placeholder="请选择目标 Schema"
                  style="width: 100%; margin-top: 10px"
                  allow-search
                >
                  <a-option v-for="s in viewMigrate.dstSchemas" :key="s" :value="s" :label="s" />
                </a-select>
                <div v-if="viewMigrate.dstConnId" class="schema-permission-tip">
                  <icon-info-circle style="flex-shrink:0" />
                  请确保目标 Schema 拥有创建对象的权限，否则请在目标数据库中自行处理模式权限后再执行迁移。
                </div>
              </a-card>
            </a-col>
          </a-row>

          <!-- 不支持提示 -->
          <a-alert
            v-if="viewMigrate.unsupportedMsg"
            type="error"
            style="margin-bottom: 16px"
          >
            {{ viewMigrate.unsupportedMsg }}
          </a-alert>

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
          <a-collapse style="margin-bottom: 16px; max-width: 560px">
            <a-collapse-item key="advanced" header="高级设置">
              <a-row :gutter="16">
                <a-col v-if="vmSelectedDst?.db_type !== 'dameng'" :span="12">
                  <a-form-item style="margin-bottom: 4px">
                    <a-checkbox v-model="viewMigrate.lowerCaseNames">对象名转小写</a-checkbox>
                  </a-form-item>
                </a-col>
                <a-col :span="12">
                  <a-form-item style="margin-bottom: 4px">
                    <a-checkbox v-model="viewMigrate.changeOwner">更改对象 owner 为 Schema 同名角色</a-checkbox>
                  </a-form-item>
                </a-col>
                <a-col :span="24">
                  <a-form-item label="视图剥离模式名">
                    <a-input
                      v-model="viewMigrate.stripViewSchemas"
                      placeholder="逗号分隔，如 financeplatform_3.0, otherdb"
                      allow-clear
                    />
                    <template #extra>
                      从视图定义中去除这些模式名前缀（忽略大小写）。用于跨库引用导致目标库找不到 schema 的场景。
                    </template>
                  </a-form-item>
                </a-col>
              </a-row>
            </a-collapse-item>
          </a-collapse>

          <!-- 操作按钮 -->
          <a-space style="margin-bottom: 16px">
            <a-button
              type="primary"
              :disabled="!vmCanMigrate"
              :loading="viewMigrate.running"
              @click="handleMigrateViews"
            >开始迁移视图</a-button>
          </a-space>

          <!-- 结果 -->
          <div v-if="viewMigrate.results.length > 0">
            <a-divider>迁移结果</a-divider>
            <a-space style="margin-bottom: 8px">
              <a-tag color="green">成功 {{ vmSuccessCount }}</a-tag>
              <a-tag :color="vmFailCount > 0 ? 'red' : 'gray'">失败 {{ vmFailCount }}</a-tag>
            </a-space>
            <a-table
              :data="viewMigrate.results"
              row-key="name"
              :pagination="false"
              size="small"
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
        </a-form>
      </a-tab-pane>

      <a-tab-pane key="object-migrate" title="对象迁移">
        <a-form :model="objMigrate" layout="vertical" style="margin-top: 12px">
          <!-- 源库 / 目标库选择 -->
          <a-row :gutter="20" align="stretch" style="margin-bottom: 16px">
            <a-col :span="11">
              <a-card class="conn-card" :body-style="{ padding: '16px' }">
                <div class="conn-card-header">
                  <a-tag color="orange" size="small">源库</a-tag>
                  <span class="conn-card-title">源库</span>
                </div>
                <a-select
                  v-model="objMigrate.srcConnId"
                  placeholder="选择源库连接"
                  style="width: 100%; margin-top: 10px"
                  @change="(val) => { omCheckPairSupport(); omLoadSrcDatabases(val as number) }"
                >
                  <a-option v-for="c in srcConnections" :key="c.id" :value="c.id" :label="c.name">
                    <a-tag :color="getDbTypeColor(c.db_type)" size="small" style="margin-right:6px">{{ getDbTypeLabel(c.db_type) }}</a-tag>{{ c.name }}
                  </a-option>
                </a-select>
                <div v-if="omSelectedSrc" class="conn-meta">
                  <span class="conn-meta-item"><span class="conn-meta-label">地址</span>{{ omSelectedSrc.host }}:{{ omSelectedSrc.port }}</span>
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
                  <a-option v-for="db in objMigrate.srcDatabases" :key="db" :value="db" :label="db" />
                </a-select>
              </a-card>
            </a-col>
            <a-col :span="2" style="display:flex;align-items:center;justify-content:center">
              <icon-arrow-right style="font-size: 28px; color: #165dff" />
            </a-col>
            <a-col :span="11">
              <a-card class="conn-card" :body-style="{ padding: '16px' }">
                <div class="conn-card-header">
                  <a-tag color="blue" size="small">目标库</a-tag>
                  <span class="conn-card-title">目标库</span>
                </div>
                <a-select
                  v-model="objMigrate.dstConnId"
                  placeholder="选择目标库连接"
                  style="width: 100%; margin-top: 10px"
                  @change="(val) => { omCheckPairSupport(); omLoadDstSchemas(val as number) }"
                >
                  <a-option v-for="c in pgConnections" :key="c.id" :value="c.id" :label="c.name">
                    <a-tag :color="getDbTypeColor(c.db_type)" size="small" style="margin-right:6px">{{ getDbTypeLabel(c.db_type) }}</a-tag>{{ c.name }}
                  </a-option>
                </a-select>
                <div v-if="omSelectedDst" class="conn-meta">
                  <span class="conn-meta-item"><span class="conn-meta-label">地址</span>{{ omSelectedDst.host }}:{{ omSelectedDst.port }}</span>
                  <span class="conn-meta-item"><span class="conn-meta-label">数据库</span>{{ omSelectedDst.database }}</span>
                  <span class="conn-meta-item"><span class="conn-meta-label">账号</span>{{ omSelectedDst.username }}</span>
                </div>
                <a-select
                  v-if="objMigrate.dstConnId"
                  v-model="objMigrate.dstSchema"
                  placeholder="请选择目标 Schema"
                  style="width: 100%; margin-top: 10px"
                  allow-search
                >
                  <a-option v-for="s in objMigrate.dstSchemas" :key="s" :value="s" :label="s" />
                </a-select>
                <div v-if="objMigrate.dstConnId" class="schema-permission-tip">
                  <icon-info-circle style="flex-shrink:0" />
                  对象迁移仅向目标库已存在的表补建对象，请确保目标表与引用表已迁移完成。
                </div>
              </a-card>
            </a-col>
          </a-row>

          <!-- 不支持提示 -->
          <a-alert
            v-if="objMigrate.unsupportedMsg"
            type="error"
            style="margin-bottom: 16px"
          >
            {{ objMigrate.unsupportedMsg }}
          </a-alert>

          <!-- 对象类型选择 -->
          <a-form-item label="迁移对象类型" style="margin-bottom: 16px">
            <a-checkbox-group v-model="objMigrate.objects">
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
          <a-collapse style="margin-bottom: 16px; max-width: 560px">
            <a-collapse-item key="advanced" header="高级设置">
              <a-row :gutter="16">
                <a-col v-if="omSelectedDst?.db_type !== 'dameng'" :span="12">
                  <a-form-item style="margin-bottom: 4px">
                    <a-checkbox v-model="objMigrate.lowerCaseNames">对象名转小写</a-checkbox>
                  </a-form-item>
                </a-col>
                <a-col :span="12">
                  <a-form-item style="margin-bottom: 4px">
                    <a-checkbox v-model="objMigrate.changeOwner">更改对象 owner 为 Schema 同名角色</a-checkbox>
                  </a-form-item>
                </a-col>
                <a-col :span="12">
                  <a-form-item style="margin-bottom: 4px">
                    <a-checkbox v-model="objMigrate.distributed">分布式库（建主键前设置分布列）</a-checkbox>
                  </a-form-item>
                </a-col>
              </a-row>
            </a-collapse-item>
          </a-collapse>

          <!-- 操作按钮 -->
          <a-space style="margin-bottom: 16px">
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
          </a-space>

          <!-- 日志区 -->
          <div v-if="objMigrate.logs.length > 0">
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

          <!-- 迁移报告 -->
          <div v-if="objMigrate.finished && objMigrate.currentJobId" style="margin-top: 16px">
            <a-divider>迁移报告</a-divider>
            <MigrationReportPanel :jobID="objMigrate.currentJobId" />
          </div>
        </a-form>
      </a-tab-pane>

      <a-tab-pane key="diff" title="Diff 迁移">
        <a-space direction="vertical" fill style="width: 100%; margin-top: 12px">
          <a-row :gutter="24">
            <a-col :span="11">
              <a-card title="源">
                <connection-select v-model:connection-id="diffSrc.connId" v-model:database="diffSrc.dbName" />
              </a-card>
            </a-col>
            <a-col :span="2" style="display:flex;align-items:center;justify-content:center">
              <icon-arrow-right style="font-size: 24px; color: #165dff" />
            </a-col>
            <a-col :span="11">
              <a-card title="目标">
                <connection-select v-model:connection-id="diffDst.connId" v-model:database="diffDst.dbName" />
              </a-card>
            </a-col>
          </a-row>
          <a-checkbox v-model="schemaMigrateLowerCase">对象名转小写</a-checkbox>
          <a-button
            type="primary"
            :loading="diffLoading"
            :disabled="!(diffSrc.connId && diffSrc.dbName && diffDst.connId && diffDst.dbName)"
            @click="handleDiffMigration"
          >
            生成迁移 SQL
          </a-button>
          <sql-preview :sqls="diffSqls" />
        </a-space>
      </a-tab-pane>

      <a-tab-pane key="full" title="全量迁移">
        <a-space direction="vertical" fill style="width: 100%; margin-top: 12px">
          <a-card title="目标数据库（将为此库生成完整建表 SQL）">
            <connection-select v-model:connection-id="fullDst.connId" v-model:database="fullDst.dbName" />
          </a-card>
          <a-checkbox v-model="schemaMigrateLowerCase">对象名转小写</a-checkbox>
          <a-button
            type="primary"
            :loading="fullLoading"
            :disabled="!(fullDst.connId && fullDst.dbName)"
            @click="handleFullMigration"
          >
            生成全量 SQL
          </a-button>
          <sql-preview :sqls="fullSqls" />
        </a-space>
      </a-tab-pane>
    </a-tabs>
  </div>
</template>

<script setup lang="ts">
import { ref, reactive, computed, watch, nextTick, onMounted, onUnmounted } from 'vue'
import { onBeforeRouteLeave, useRoute } from 'vue-router'
import { Message, Modal } from '@arco-design/web-vue'
import ConnectionSelect from '@/components/ConnectionSelect.vue'
import SqlPreview from '@/components/SqlPreview.vue'
import { getDbTypeColor, getDbTypeLabel } from '@/utils/dbType'
import MigrationReportPanel from './MigrationReportPanel.vue'
import { runDiffMigration, runFullMigration } from '@/api/migration'
import { listConnections, listConnectionDatabases, listConnectionSchemas, listConnectionViews, type Connection } from '@/api/connections'
import {
  getSupportedPairs,
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

const diffSrc = reactive({ connId: undefined as number | undefined, dbName: '' })
const diffDst = reactive({ connId: undefined as number | undefined, dbName: '' })
const diffLoading = ref(false)
const diffSqls = ref<string[]>([])
const schemaMigrateLowerCase = ref(true)

const fullDst = reactive({ connId: undefined as number | undefined, dbName: '' })
const fullLoading = ref(false)
const fullSqls = ref<string[]>([])

async function handleDiffMigration() {
  if (!diffSrc.connId || !diffSrc.dbName || !diffDst.connId || !diffDst.dbName) return
  diffLoading.value = true
  diffSqls.value = []
  try {
    const res = await runDiffMigration({
      src_connection_id: diffSrc.connId,
      src_database: diffSrc.dbName,
      dst_connection_id: diffDst.connId,
      dst_database: diffDst.dbName,
      lower_case_names: schemaMigrateLowerCase.value,
    })
    diffSqls.value = res.data.sql_statements
    Message.success(`已生成 ${diffSqls.value.length} 条 SQL`)
  } catch {
    Message.error('生成失败')
  } finally {
    diffLoading.value = false
  }
}

async function handleFullMigration() {
  if (!fullDst.connId || !fullDst.dbName) return
  fullLoading.value = true
  fullSqls.value = []
  try {
    const res = await runFullMigration({
      dst_connection_id: fullDst.connId,
      dst_database: fullDst.dbName,
      lower_case_names: schemaMigrateLowerCase.value,
    })
    fullSqls.value = res.data.sql_statements
    Message.success(`已生成 ${fullSqls.value.length} 条 SQL`)
  } catch {
    Message.error('生成失败')
  } finally {
    fullLoading.value = false
  }
}

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
  changeOwner: true,
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

const srcConnections = computed(() =>
  connections.value.filter(
    (c) => c.db_type === 'mysql' || c.db_type === 'sqlserver' || c.db_type === 'dameng' || c.db_type === 'oracle'
  )
)
const pgConnections = computed(() =>
  connections.value.filter((c) => c.db_type === 'postgres' || c.db_type === 'gaussdb' || c.db_type === 'seabox' || c.db_type === 'dameng' || c.db_type === 'highgo' || c.db_type === 'mysql')
)
const selectedSrc = computed(() =>
  connections.value.find((c) => c.id === dataMigrate.srcConnId)
)
const selectedDst = computed(() =>
  connections.value.find((c) => c.id === dataMigrate.dstConnId)
)

watch(() => dataMigrate.dstConnId, (newId) => {
  const dst = connections.value.find((c) => c.id === newId)
  if (dst?.db_type === 'dameng') {
    dataMigrate.lowerCaseNames = false
    dataMigrate.charInLength = true
  } else {
    dataMigrate.lowerCaseNames = true
    dataMigrate.charInLength = false
  }
})

const canStartMigration = computed(() =>
  dataMigrate.srcConnId !== undefined &&
  dataMigrate.dstConnId !== undefined &&
  dataMigrate.srcDatabase !== '' &&
  !dataMigrate.unsupportedMsg &&
  !dataMigrate.running
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
  if (!dst || (dst.db_type !== 'postgres' && dst.db_type !== 'gaussdb' && dst.db_type !== 'seabox' && dst.db_type !== 'dameng' && dst.db_type !== 'highgo' && dst.db_type !== 'mysql')) return
  try {
    const res = await listConnectionSchemas(connId)
    dataMigrate.dstSchemas = res.data ?? []
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

async function startDataMigration() {
  if (!validateTableFilter()) return
  if (dataMigrate.dstConnId && !dataMigrate.dstSchema) {
    Message.error('请选择目标 Schema')
    return
  }
  dataMigrate.running = true
  dataMigrate.finished = false
  dataMigrate.logs = []
  try {
    const res = await apiStartMigration({
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
    })
    dataMigrate.currentJobId = res.data.job_id
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
}

function copyLogs() {
  navigator.clipboard.writeText(dataMigrate.logs.join('\n'))
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
  changeOwner: true,
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
  viewMigrate.lowerCaseNames = dst?.db_type !== 'dameng'
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
  if (!dst || (dst.db_type !== 'postgres' && dst.db_type !== 'gaussdb' && dst.db_type !== 'seabox' && dst.db_type !== 'dameng' && dst.db_type !== 'highgo' && dst.db_type !== 'mysql')) return
  try {
    const res = await listConnectionSchemas(connId)
    viewMigrate.dstSchemas = res.data ?? []
  } catch (e: any) {
    Message.error(`获取目标 Schema 失败: ${e?.response?.data?.error ?? e?.message ?? '连接失败,请检查连接配置'}`)
  }
}

async function vmLoadViews() {
  if (!viewMigrate.srcConnId) return
  viewMigrate.loadingViews = true
  viewMigrate.views = []
  viewMigrate.selected = []
  viewMigrate.results = []
  try {
    const res = await listConnectionViews(viewMigrate.srcConnId, viewMigrate.srcDatabase || undefined)
    viewMigrate.views = res.data ?? []
  } catch (e: any) {
    Message.error(`加载视图失败: ${e?.response?.data?.error ?? e?.message ?? e}`)
  } finally {
    viewMigrate.loadingViews = false
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
  if (viewMigrate.dstConnId && !viewMigrate.dstSchema) {
    Message.error('请选择目标 Schema')
    return
  }
  viewMigrate.running = true
  viewMigrate.results = []
  try {
    const res = await apiMigrateViews({
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
  changeOwner: true,
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
  objMigrate.lowerCaseNames = dst?.db_type !== 'dameng'
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
  if (!dst || (dst.db_type !== 'postgres' && dst.db_type !== 'gaussdb' && dst.db_type !== 'seabox' && dst.db_type !== 'dameng' && dst.db_type !== 'highgo' && dst.db_type !== 'mysql')) return
  try {
    const res = await listConnectionSchemas(connId)
    objMigrate.dstSchemas = res.data ?? []
  } catch (e: any) {
    Message.error(`获取目标 Schema 失败: ${e?.response?.data?.error ?? e?.message ?? '连接失败,请检查连接配置'}`)
  }
}

async function omLoadTables() {
  if (!objMigrate.srcConnId) return
  objMigrate.loadingTables = true
  objMigrate.tables = []
  objMigrate.selected = []
  try {
    const res = await listConnectionTables(objMigrate.srcConnId, objMigrate.srcDatabase || undefined)
    objMigrate.tables = res.data ?? []
  } catch (e: any) {
    Message.error(`加载表失败: ${e?.response?.data?.error ?? e?.message ?? e}`)
  } finally {
    objMigrate.loadingTables = false
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
  if (objMigrate.dstConnId && !objMigrate.dstSchema) {
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
  objMigrate.logs = []
  try {
    const res = await apiStartObjectMigration({
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
}

function copyObjLogs() {
  navigator.clipboard.writeText(objMigrate.logs.join('\n'))
}

</script>

<style scoped>
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
