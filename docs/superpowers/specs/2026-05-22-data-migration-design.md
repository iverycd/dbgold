# MySQL 全库在线迁移到 PostgreSQL 设计文档

**日期：** 2026-05-22  
**状态：** 待实施

---

## 背景与目标

dbgold 目前支持 Schema 层面的 DDL 迁移（对比两库结构差异并生成 SQL），但不迁移实际数据。本需求在现有迁移页面基础上新增「数据迁移」能力，参照 gomysql2pg 项目的成熟实现，支持 MySQL 全库数据一键迁移到 PostgreSQL。

**目标：**
- 一键完成全流程：建表结构 → 迁移数据 → 创建序列/索引/外键/视图
- 实时日志流（SSE）展示迁移进度
- 可扩展架构：将来支持其他数据库组合（Oracle→MySQL、MSSQL→PG 等）
- 迁移类型：全量快照，源库不停机，不要求增量同步

---

## 整体架构

### 新增模块

```
dbgold/
├── datamigrate/
│   ├── migrator.go          # 迁移器核心：生命周期管理、阶段调度
│   ├── schema.go            # DDL 迁移：建表、序列、索引、外键、视图
│   ├── data.go              # 数据迁移：分页读取源库 + COPY 写入目标库
│   ├── logger.go            # 迁移日志：写入 SSE channel
│   ├── registry.go          # 内存 JobRegistry：jobID → Job
│   ├── source/
│   │   ├── interface.go     # 源库接口定义
│   │   └── mysql.go         # MySQL 实现
│   ├── target/
│   │   ├── interface.go     # 目标库接口定义
│   │   └── postgres.go      # PostgreSQL 实现
│   └── typemap/
│       └── mysql_pg.go      # MySQL → PostgreSQL 类型映射
```

### 扩展现有模块

- `api/handler/migration.go` — 新增数据迁移相关 handler
- `api/router.go` — 注册新 API 端点
- `store/db.go` — 新增 `DataMigrationJob` 模型
- `store/migration.go` — 新增 DataMigrationJob CRUD
- `frontend/src/views/MigrationView.vue` — 新增「数据迁移」Tab
- `frontend/src/api/migration.ts` — 新增数据迁移 API 调用

---

## 源库/目标库接口

### source.Reader

```go
type Reader interface {
    // 获取所有表名
    ListTables(ctx context.Context) ([]string, error)
    // 获取表的列定义（用于 DDL 生成）
    GetTableDDLInfo(ctx context.Context, table string) (*TableDDLInfo, error)
    // 分页读取数据，返回列名和行数据
    ReadPage(ctx context.Context, table string, pkCol string, offset, limit int) ([]string, [][]interface{}, error)
    // 获取主键列名（无主键返回空串）
    GetPrimaryKey(ctx context.Context, table string) (string, error)
    // 获取序列信息（AUTO_INCREMENT 列）
    GetSequences(ctx context.Context) ([]SequenceInfo, error)
    // 获取索引信息
    GetIndexes(ctx context.Context) ([]IndexInfo, error)
    // 获取外键信息
    GetForeignKeys(ctx context.Context) ([]FKInfo, error)
    // 获取视图信息
    GetViews(ctx context.Context) ([]ViewInfo, error)
    // 获取 DBType 标识（用于 typemap 选择）
    DBType() string
}
```

### target.Writer

```go
type Writer interface {
    // 执行建表 DDL
    CreateTable(ctx context.Context, ddl string) error
    // 使用 COPY 协议批量写入数据
    CopyData(ctx context.Context, table string, cols []string, rows [][]interface{}) error
    // 创建序列
    CreateSequence(ctx context.Context, seq SequenceInfo) error
    // 创建索引/约束
    CreateIndex(ctx context.Context, idx IndexInfo) error
    // 创建外键
    CreateForeignKey(ctx context.Context, fk FKInfo) error
    // 创建视图
    CreateView(ctx context.Context, view ViewInfo) error
    // 获取 DBType 标识
    DBType() string
}
```

---

## 类型映射（MySQL → PostgreSQL）

| MySQL 类型 | PostgreSQL 类型 | 备注 |
|-----------|----------------|------|
| tinyint, smallint, mediumint, int | int | |
| bigint | bigint | |
| float, double | double precision | |
| decimal(p,s) | decimal(p,s) | |
| char(n) | char(n) | |
| varchar(n) | varchar(n) | |
| tinytext, text, mediumtext, longtext | text | |
| datetime, timestamp | timestamp | |
| date | date | |
| time | time | |
| tinyblob, blob, mediumblob, longblob | bytea | |
| binary, varbinary | bytea | |
| json | jsonb | |
| bit | bit | |
| enum | varchar(255) | |
| set | text | |
| year | int | |

---

## 并发模型

### 迁移阶段

```
Migrator.Run(ctx)
 ├── Phase 1: CreateTables（串行）
 │    └── 逐表生成并执行 CREATE TABLE
 ├── Phase 2: MigrateData（并发）
 │    ├── semaphore（maxParallel）控制并发数
 │    ├── goroutine per table
 │    │    ├── 有主键：分页查询（每页 pageSize 行）
 │    │    └── 无主键：全表一次性读取
 │    └── 每页数据用 PostgreSQL COPY 协议写入
 └── Phase 3: Post-DDL（串行）
      ├── CreateSequences
      ├── CreateIndexes
      ├── CreateForeignKeys
      └── CreateViews
```

### 取消机制

`context.WithCancel` 贯穿整个迁移链路。前端关闭 SSE 连接或调用 cancel API 时触发取消，所有 goroutine 通过 ctx.Done() 感知退出。

---

## 日志格式

每条 SSE 消息为一行纯文本，格式：`[前缀]  消息内容`

| 前缀 | 含义 | 前端颜色 |
|------|------|---------|
| `[INFO]` | 任务级别信息 | 灰色 |
| `[DDL]` | 建表/建视图等 DDL | 默认 |
| `[DATA]` | 数据分页迁移进度 | 默认 |
| `[INDEX]` | 索引/序列/外键创建 | 默认 |
| `[WARN]` | 警告（非致命错误） | 橙色 |
| `[ERROR]` | 错误（某张表/某操作失败） | 红色 |
| `[DONE]` | 迁移完成摘要 | 绿色 |

示例输出：
```
[INFO]  开始迁移任务，共 42 张表，pageSize=10000，maxParallel=5
[DDL]   创建表 users ... OK
[DDL]   创建表 orders ... OK
[DATA]  迁移 users: 第 1 页 (10000 行) ... OK
[DATA]  迁移 users: 第 2 页 (3821 行) ... OK
[DATA]  迁移 orders: 第 1 页 (10000 行) ... OK
[INDEX] 创建序列 seq_users_id ... OK
[INDEX] 创建索引 idx_orders_user_id ... OK
[ERROR] 创建外键 fk_orders_user_id 失败: relation "users" does not exist
[DONE]  迁移完成：成功 41 张，失败 1 张，耗时 3m24s
```

---

## 数据库模型

```go
type DataMigrationJob struct {
    ID          uint       `gorm:"primaryKey"`
    JobID       string     `gorm:"uniqueIndex"`   // UUID
    SrcConnID   uint                               // 源库连接 ID
    DstConnID   uint                               // 目标库连接 ID
    SrcDBType   string                             // mysql / oracle / sqlserver
    DstDBType   string                             // postgres / mysql
    MigrateMode string                             // all / exclude / include
    TableFilter string                             // 逗号分隔表名或通配符（exclude/include 模式）
    PageSize    int
    MaxParallel int
    Status      string     // running / done / failed / cancelled
    Summary     string     // 完成摘要
    CreatedAt   time.Time
    FinishedAt  *time.Time
}
```

---

## API 端点

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/api/migration/data-migrate/supported-pairs` | 查询后端支持的迁移组合 |
| `POST` | `/api/migration/data-migrate` | 创建并启动迁移任务，返回 `{ jobID }` |
| `GET` | `/api/migration/data-migrate/stream?jobID=xxx` | SSE 实时日志流 |
| `POST` | `/api/migration/data-migrate/:jobID/cancel` | 取消运行中任务 |
| `GET` | `/api/migration/data-migrate/jobs` | 历史任务列表 |

### POST /api/migration/data-migrate 请求体

```json
{
  "srcConnID": 1,
  "dstConnID": 2,
  "migrateMode": "exclude",
  "tableFilter": "*_log,tmp_*",
  "pageSize": 10000,
  "maxParallel": 5
}
```

### GET /api/migration/data-migrate/supported-pairs 响应

```json
[
  { "source": "mysql", "target": "postgres" }
]
```

---

## 前端设计

### MigrationView 新增 Tab

现有「DDL 迁移」Tab 保持不变，新增「数据迁移」Tab。

### 数据迁移 Tab 布局

```
┌─────────────────────────────────────────────────────┐
│  源库                    →       目标库              │
│  [下拉：仅显示 MySQL 连接]   [下拉：仅显示 PG 连接] │
│  （选择后自动验证组合是否支持）                      │
├─────────────────────────────────────────────────────┤
│  ⚠ 当前不支持 Oracle → PostgreSQL 的数据迁移        │  ← 不支持时显示
├─────────────────────────────────────────────────────┤
│  迁移范围                                            │
│  ● 全库迁移                                          │
│  ○ 排除指定表  [输入框，逗号分隔，支持 * 通配符]     │
│  ○ 仅迁移指定表 [输入框，逗号分隔]                   │
├─────────────────────────────────────────────────────┤
│  ▼ 高级设置                                          │
│    每页行数 pageSize      [10000]                    │
│    最大并发数 maxParallel  [5   ]                    │
├─────────────────────────────────────────────────────┤
│                    [开始迁移]                         │
├─────────────────────────────────────────────────────┤
│  迁移日志                              [复制日志]    │
│ ┌───────────────────────────────────────────────┐   │
│ │ [INFO]  开始迁移任务，共 42 张表              │   │
│ │ [DDL]   创建表 users ... OK                   │   │
│ │ [DATA]  迁移 users: 第1页 (10000行) ... OK    │   │
│ │ [ERROR] 创建外键 fk_orders ... 失败           │   │
│ │ [DONE]  完成，成功41张，失败1张，耗时3m24s   │   │
│ └───────────────────────────────────────────────┘   │
│                    [停止迁移]                         │
└─────────────────────────────────────────────────────┘
```

### 交互细节

- 源库下拉仅显示 MySQL 类型连接，目标库下拉仅显示 PostgreSQL 类型连接（根据 supported-pairs 动态过滤）
- 选择连接后自动检查 supported-pairs，不支持的组合禁用「开始迁移」并显示红色提示
- 迁移中：「开始迁移」禁用，显示「停止迁移」按钮，日志区自动滚动跟随
- 日志颜色：`[ERROR]` 红色、`[WARN]` 橙色、`[DONE]` 绿色、其余默认色
- 迁移完成/失败/取消后：「停止迁移」隐藏，显示「重新迁移」按钮
- 提供「复制日志」按钮，将日志区全文复制到剪贴板

---

## 迁移范围过滤规则

| 模式 | 说明 | 示例 |
|------|------|------|
| `all` | 迁移全部表，tableFilter 忽略 | — |
| `exclude` | 排除匹配的表，迁移其余所有表 | `*_log,tmp_*,audit` |
| `include` | 仅迁移匹配的表 | `users,orders,products` |

通配符 `*` 匹配任意字符串，匹配逻辑在后端执行。

---

## 错误处理策略

- 单张表 DDL 创建失败：记录 `[ERROR]` 日志，继续处理其他表（不中止整个任务）
- 单页数据写入失败：记录 `[ERROR]` 日志，跳过该页，继续处理其他分页和表
- 整个任务 ctx 取消：所有 goroutine 退出，任务状态置为 `cancelled`
- 任务完成时在 `[DONE]` 日志中汇总成功/失败表数

---

## 本次实现范围

- `source/mysql.go` — MySQL 源库实现
- `target/postgres.go` — PostgreSQL 目标库实现（使用 `pq.CopyIn`）
- `typemap/mysql_pg.go` — MySQL → PostgreSQL 类型映射
- 接口文件留好扩展位，其他组合（Oracle、MSSQL 等）后续按需实现

---

## 不在本次范围内

- 增量同步 / CDC（binlog 监听）
- 存储过程、自定义函数迁移
- 用户权限迁移
- 其他数据库组合（Oracle→PG、MSSQL→MySQL 等）
