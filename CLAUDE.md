# CLAUDE.md — 开发注意事项

## Arco Design Vue：`a-table` 展开行

### 问题描述

`MigrationReportPanel.vue` 曾出现报告中有失败对象但无法点开查看详情的问题。

### 根本原因

修复"展开行内容不该对所有行显示"时，错误地把 `:expandable` prop 整个移除了。移除后每行都没有展开箭头，用户无法展开任何行。

### 正确做法

Arco Design **Vue** 2.58.0 的 `TableExpandable` 接口**没有** `rowExpandable` 字段（这是 React 版本的 API）。用 `icon` 回调返回 `null` 来隐藏不可展开行的箭头：

```vue
<a-table
  :expandable="{ icon: (_, record) => record.failed > 0 && record.items?.length > 0 ? undefined : null }"
>
  <template #expand-row="{ record }">
    <!-- 展开内容，icon 返回 null 的行不会显示箭头 -->
  </template>
</a-table>
```

- **不要**移除 `:expandable` prop 来"修复"展开内容不该显示的问题。
- **不要**用 `rowExpandable`——Vue 版本不支持，会导致 TS 编译报错。
- **要**用 `icon` 回调：返回 `undefined` 显示默认箭头，返回 `null` 隐藏箭头。

---

## Arco Design Vue：`a-form` 必填 prop

`<a-form>` 必须传入 `:model`，否则控制台报 `Missing required prop: "model"` 警告。

```vue
<!-- 正确 -->
<a-form :model="formData" layout="vertical">
```

在没有独立 form 数据对象时，也可以传入包含表单字段的 reactive 对象（如 `dataMigrate`）。

---

## Arco Design Vue：图标组件需单独注册

`<icon-plus />`、`<icon-refresh />` 等图标组件来自独立插件包，需在 `main.ts` 中额外注册：

```ts
import ArcoVueIcon from '@arco-design/web-vue/es/icon'
app.use(ArcoVueIcon)
```

仅注册 `ArcoVue` 主包不会自动包含图标组件。

---

## a-form-item 需在 a-form 内使用

`<a-form-item>` 依赖父级 `<a-form>` 注入上下文。在没有父级 `<a-form>` 的情况下使用会触发：

```
[Vue warn]: toRefs() expects a reactive object but received a plain one.
```

将包含 `<a-form-item>` 的容器从 `<div>` 改为 `<a-form :model="...">` 即可解决。

---

## Vue：`v-if` 控制组件销毁与重建

### 问题描述

在 `HistoryView.vue` 中，`MigrationReportPanel` 用 `v-if="reportJobId"` 控制显示。用户第一次打开某条记录的报告后，关闭 drawer 时只是隐藏了 drawer，但 `reportJobId` 仍有值，组件没有被销毁。再打开另一条记录时，`reportJobId` 更新为新值，但组件已处于挂载状态，`onMounted` 不会重新触发，`loadReport` 不会重新执行，始终显示第一次加载的报告内容。

### 根本原因

`v-if` 只在绑定值从 falsy 变为 truthy 时挂载组件、触发 `onMounted`。如果值从一个 truthy 变为另一个 truthy（如 job_id A → job_id B），组件不会重新挂载，生命周期钩子不会重新执行。

### 正确做法

在 drawer 关闭时清空控制 `v-if` 的变量，确保下次打开时组件从零重建：

```vue
<a-drawer v-model:visible="drawerVisible" @close="reportJobId = ''">
  <SomeComponent v-if="reportJobId" :jobID="reportJobId" />
</a-drawer>
```

- 关闭 drawer → `reportJobId = ''` → 组件销毁
- 打开新记录 → `reportJobId = 新值` → 组件挂载 → `onMounted` 触发 → 加载正确数据

---

## Arco Design Vue：`a-modal` 禁止点击遮罩关闭

所有 `<a-modal>` 必须加 `:mask-closable="false"`，防止用户误触空白区域关闭弹窗导致表单内容丢失。

```vue
<a-modal
  v-model:visible="modalVisible"
  :mask-closable="false"
  @ok="handleSubmit"
  @cancel="modalVisible = false"
>
```

只能通过"确定"、"取消"按钮或 ESC 键关闭弹窗。

---

## Arco Design Vue：`a-alert` 文字用默认插槽，没有 `content` prop

### 问题描述

`MigrationView.vue` 的不支持提示（oracle → dameng 等组合）只显示了红色叉号图标，文字区域空白。

### 根本原因

Arco Design **Vue** 的 `a-alert` **没有 `content` prop**——文字内容必须通过**默认插槽**传入。写成 `:content="msg"` 时绑定被静默忽略，只渲染了 `type` 对应的图标，文字不显示。（`content` 是 React/其它库的 API。）

### 正确做法

```vue
<!-- 错误：content 绑定被忽略，只剩图标 -->
<a-alert v-if="msg" type="error" :content="msg" />

<!-- 正确：文字走默认插槽 -->
<a-alert v-if="msg" type="error">
  {{ msg }}
</a-alert>
```

- `title` 是 prop（粗体标题），但正文内容只能用插槽。
- 排查"alert 只有图标没文字"时，第一时间检查是不是用了 `:content`。

---

## datamigrate：Reader 方法必须返回原始大小写

### 规则

`datamigrate/source/` 下所有 Reader 实现（`mysql.go`、`sqlserver.go` 等）的每个方法，**禁止在 SQL 里对对象名做 `lower()` / `upper()` 转换**，必须原样返回数据库中存储的大小写。

### 原因

`createPostDDL`（`migrator.go`）用 `tableSet` 过滤本次迁移范围内的对象：

```go
tableSet := map[string]bool{}
for _, t := range tables { tableSet[t] = true }   // 来自 ListTables，原始大小写

// 过滤主键 / 序列 / 索引时：
if tableSet[pk.TableName] { ... }   // 若 pk.TableName 是 lower()，永远匹配不上
```

`tables` 来自 `ListTables`，保持原始大小写。如果其他方法（`GetSequences`、`GetIndexes` 等）在 SQL 里做了 `lower()`，返回的表名就和 `tableSet` 的 key 对不上，导致该类型的所有对象被全部过滤掉、静默丢失。

### 正确做法

大小写转换统一由 `migrator.go` 里的 `m.objName()` 在写目标库前处理：

```go
// Reader 返回原始大小写：
seqCopy.TableName = m.objName(seq.TableName)   // 这里才转
seqCopy.ColumnName = m.objName(seq.ColumnName)
```

- **不要**在 `SELECT lower(t.name)` 里转
- **要**让 Reader 返回原始值，由 migrator 统一决定是否转小写（由用户配置的 `LowerCaseNames` 控制）

### 新增 Reader 的检查清单

新增任何源库的 Reader 时，检查以下方法的 SQL，确认无 `lower()` / `upper()`：

- `ListTables`
- `GetTableDDLInfo`（列名）
- `GetPrimaryKey` / `GetPrimaryKeys`
- `GetSequences`
- `GetIndexes`
- `GetForeignKeys`（表名、引用表名、约束名）
- `GetViews`（视图名）
- `GetComments`（表名、列名）— 表注释/列注释,SQL 里同样禁止 `lower()/upper()`

---

## 新增目标数据库类型的完整检查清单

每次新增一个目标库类型（如 `highgo`），需要修改以下所有位置，缺一不可：

### 后端

1. **`datamigrate/target/<dbtype>.go`** — 新建 Writer 实现，实现 `Writer` 接口
   - PostgreSQL 兼容库：复用 `lib/pq` 驱动（`sql.Open("postgres", dsn)`）和 `dialect.NewPostgres("<dbtype>")`
   - GaussDB 系：使用 `opengauss` 驱动

2. **`driver/registry.go`** — `NewDriver` switch 里加 `case "<dbtype>"`
   - PostgreSQL 兼容库返回 `postgres.New()`

3. **`datamigrate/typemap/*_pg.go`** — 所有源库的类型映射文件都要在 `init()` 里补注册新目标库：
   - `mysql_pg.go`、`oracle_pg.go`、`sqlserver_pg.go`、`dameng_pg.go` 各加一行 `Register("<src>", "<dbtype>", <Mapper>)`
   - 漏掉此步会导致类型转换静默失效，源库类型（如 `datetime`）原样写入 DDL，目标库执行时报 `type "xxx" does not exist`

4. **`api/handler/connection.go`** — 四处都要改：
   - `connectionRequest.DBType` 的 `oneof` 标签加新类型
   - `updateConnectionRequest.DBType` 的 `oneof` 标签加新类型
   - `buildDSN` switch 加 `case "<dbtype>"` 返回对应 DSN 格式（PostgreSQL 兼容库格式：`host=%s port=%d user=%s password=%s dbname=%s sslmode=disable`）
   - schema 列表函数里两处：
     - `conn.DBType != "postgres" && ...` 白名单判断加新类型（否则返回"不支持"错误）
     - `driverName` 判断：默认 `"postgres"`，只有 `gaussdb` 用 `"opengauss"`；新增 PostgreSQL 兼容库不需要改这里

4. **`api/handler/datamigration.go`** — 两处：
   - `supportedPairs` 追加支持的迁移组合（每个源库 → 新目标库）
   - writer 初始化 switch 加 `case "<dbtype>"`

### 前端

5. **`frontend/src/utils/dbType.ts`** — `DB_TYPE_CONFIG` 加颜色和显示名称

6. **`frontend/src/views/ConnectionsView.vue`** — 两处：
   - 数据库类型下拉 `<a-option value="<dbtype>">` 加选项
   - `defaultPortMap` 加默认端口

7. **`frontend/src/views/MigrationView.vue`** — 两处：
   - `pgConnections` computed 的过滤条件加 `|| c.db_type === '<dbtype>'`
   - `loadDstSchemas` 里的 db_type 白名单判断加新类型

### 验证

```bash
/Users/kay/sdk/go1.25.5/bin/go build ./...
```

编译通过后，前端测试：新建连接选新类型、迁移任务目标库能选中新类型。

### 常见漏改导致的报错

| 报错信息 | 漏改的位置 |
|---------|-----------|
| `Key: 'connectionRequest.DBType' Error:Field validation for 'DBType' failed on the 'oneof' tag` | `connection.go` 两个结构体的 `oneof` 标签 |
| `unsupported db type: <dbtype>` | `driver/registry.go` 的 `NewDriver` switch |
| 连接测试报"不支持列出 xxx 类型的 schema" | `connection.go` schema 列表函数的白名单判断 |
| 迁移任务启动报"目标库类型不支持" | `datamigration.go` 的 `supportedPairs` 或 writer switch |
| 建表报 `type "xxx" does not exist` | `datamigrate/typemap/*_pg.go` 各文件漏加 `Register("...", "<dbtype>", ...)` |

### Writer 实现模板

- **PostgreSQL 兼容库**（highgo、seabox 等）：以 `datamigrate/target/postgres.go` 为模板，`sql.Open("postgres", dsn)`，`dialect.NewPostgres("<dbtype>")`
- **OpenGauss 兼容库**（gaussdb 等）：以 `datamigrate/target/gaussdb.go` 为模板，`sql.Open("opengauss", dsn)`
