# 数据迁移报告设计文档

**日期：** 2026-05-22  
**状态：** 待实施  
**依赖：** 2026-05-22-data-migration-design.md（已实施）

---

## 背景与目标

MySQL → PostgreSQL 数据迁移完成后，用户需要一份结构化报告，了解各类数据库对象的迁移结果：成功了多少、失败了多少，以及每个失败对象的原因和 DDL。报告既在迁移完成后原地展示，也可通过历史记录页面回查。

---

## 整体架构

**方案：Migrator 内存收集，结束时一次性持久化 JSON**

迁移过程中 `Migrator` 在内存维护 `MigrationReport` 结构，各阶段完成后填充对应字段，`Run()` 结束前将报告作为返回值交给 handler，handler 序列化后存入 `DataMigrationReport` 数据库表。前端通过新增 API 接口获取报告。

不采用日志解析方案（脆弱、无法持久化）和实时逐条写库方案（对全量快照场景过度设计）。

---

## 数据模型

### Go 结构体（datamigrate/report.go）

```go
// ObjectResult 单个失败对象的详情
type ObjectResult struct {
    Name  string `json:"name"`  // 对象名（表名、索引名等）
    DDL   string `json:"ddl"`   // 执行的 DDL，数据写入失败时为空串
    Error string `json:"error"` // 失败原因
}

// CategoryReport 一类对象的迁移统计
type CategoryReport struct {
    Total   int            `json:"total"`
    Success int            `json:"success"`
    Failed  int            `json:"failed"`
    Items   []ObjectResult `json:"items"` // 仅含失败对象，成功对象不存储
}

// MigrationReport 完整迁移报告
type MigrationReport struct {
    Tables      CategoryReport `json:"tables"`
    Data        CategoryReport `json:"data"`        // 数据写入统计（按表）
    Views       CategoryReport `json:"views"`
    Indexes     CategoryReport `json:"indexes"`
    Constraints CategoryReport `json:"constraints"` // 外键
    Sequences   CategoryReport `json:"sequences"`
    Triggers    CategoryReport `json:"triggers"`    // 仅填 Total，Success/Failed 均为 0
}
```

字段说明：
- `Triggers.Total` = 源库触发器数量，`Success`/`Failed` 均为 0，前端展示"未迁移"
- `Data.Items` 中 `DDL` 字段为空串（数据迁移无 DDL），`Error` 为首次页写入失败的原因
- 迁移中途取消/失败时，已收集的部分报告仍然持久化

### 数据库表（store/migration_report.go）

```go
type DataMigrationReport struct {
    ID         uint      `gorm:"primaryKey"`
    JobID      string    `gorm:"uniqueIndex;not null"` // 关联 DataMigrationJob.JobID
    ReportJSON string    `gorm:"type:text"`            // 序列化的 MigrationReport JSON
    CreatedAt  time.Time
}
```

---

## 后端改动

### source.Reader 接口新增方法

```go
// GetTriggerCount 返回源库触发器总数
GetTriggerCount(ctx context.Context) (int, error)
```

`MySQLReader` 实现：查询 `information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = ?`

### Migrator 收集逻辑

`Run(ctx context.Context) MigrationReport`（返回值由 `void` 改为 `MigrationReport`）

各阶段填充规则：

| 阶段 | 填充字段 | DDL 来源 |
|------|---------|---------|
| 开始前 | `Triggers.Total` | `reader.GetTriggerCount()` |
| Phase 1 建表 | `Tables` | `buildCreateTableDDL()` 返回值 |
| Phase 2 数据写入 | `Data` | 无 DDL，Error = 首次页写入错误 |
| Phase 3 序列 | `Sequences` | 从 `SequenceInfo` 重建 DDL 字符串 |
| Phase 3 索引 | `Indexes` | 从 `IndexInfo` 重建 DDL 字符串 |
| Phase 3 外键 | `Constraints` | 从 `FKInfo` 重建 DDL 字符串 |
| Phase 3 视图 | `Views` | `ViewInfo.Definition` |

Phase 2 中，同一张表若出现多页写入失败，`Data.Items` 只记录一条（首次错误），`Data.Failed` 仍只计数一次。

### handler 改动（api/handler/datamigration.go）

`StartDataMigration` 的迁移 goroutine：

```go
report := migrator.Run(ctx)
reportJSON, _ := json.Marshal(report)
store.CreateDataMigrationReport(&store.DataMigrationReport{
    JobID:      jobID,
    ReportJSON: string(reportJSON),
})
```

无论迁移成功、失败还是取消，均持久化报告。

### 新增 API 端点

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/api/migration/data-migrate/:jobID/report` | 返回指定任务的完整报告 |

响应体：

```json
{
  "tables":      { "total": 42, "success": 41, "failed": 1, "items": [...] },
  "data":        { "total": 42, "success": 40, "failed": 2, "items": [...] },
  "views":       { "total": 3,  "success": 3,  "failed": 0, "items": [] },
  "indexes":     { "total": 18, "success": 17, "failed": 1, "items": [...] },
  "constraints": { "total": 5,  "success": 5,  "failed": 0, "items": [] },
  "sequences":   { "total": 8,  "success": 8,  "failed": 0, "items": [] },
  "triggers":    { "total": 2,  "success": 0,  "failed": 0, "items": [] }
}
```

报告不存在时返回 404。

---

## 前端设计

### 新增组件

`frontend/src/views/MigrationReportPanel.vue`

Props：`jobID: string`

行为：挂载时调用 `getDataMigrationReport(jobID)`，加载报告数据并渲染。

### 报告表格布局

| 对象类型 | 总数 | 成功 | 失败 | 状态 |
|---------|------|------|------|------|
| 表 | 42 | 41 | 1 | ⚠ 部分失败（行可展开） |
| 数据写入 | 42 | 40 | 2 | ⚠ 部分失败（行可展开） |
| 视图 | 3 | 3 | 0 | ✓ 全部成功 |
| 索引 | 18 | 17 | 1 | ⚠ 部分失败（行可展开） |
| 外键 | 5 | 5 | 0 | ✓ 全部成功 |
| 序列 | 8 | 8 | 0 | ✓ 全部成功 |
| 触发器 | 2 | — | — | ⊘ 未迁移 |

### 行内展开（失败详情）

失败数 > 0 的行点击后行内展开，列出所有失败对象：

```
▼ 表（1 个失败）
  ┌──────────────────────────────────────────┐
  │ orders                                   │
  │ 失败原因：column "status" type error     │
  │ DDL:                          [复制 DDL] │
  │   DROP TABLE IF EXISTS "orders";         │
  │   CREATE TABLE "orders" (                │
  │     "id" int NOT NULL,                   │
  │     ...                                  │
  │   );                                     │
  └──────────────────────────────────────────┘
```

- DDL 用等宽字体 `<pre>` 代码块展示
- 每个失败对象提供「复制 DDL」按钮
- 数据写入失败行展开后 DDL 显示"—"

### 触发器行

触发器行的"成功"和"失败"列显示"—"，状态列显示"⊘ 未迁移（N 个）"。

### 嵌入「数据迁移」Tab

`MigrationView.vue` 在 SSE 连接关闭（`[DONE]` 或任务结束）后，在日志区下方渲染 `<MigrationReportPanel :jobID="dataMigrate.currentJobId" />`。

### 历史记录回查

`HistoryView.vue` 中，状态为 `done` 或 `failed` 的任务行新增「查看报告」按钮，点击后在抽屉内渲染 `<MigrationReportPanel :jobID="job.job_id" />`。

---

## API 客户端（frontend/src/api/migration.ts）

新增：

```typescript
export function getDataMigrationReport(jobID: string) {
  return api.get(`/migration/data-migrate/${jobID}/report`)
}
```

---

## 错误处理

- 报告接口返回 404：前端展示"暂无报告数据"
- `GetTriggerCount` 失败：`Triggers.Total` 设为 -1，前端展示"获取失败"
- 报告 JSON 序列化失败（极少见）：记录错误日志，不影响任务状态

---

## 不在本次范围内

- 触发器的实际迁移
- 报告导出（PDF/CSV）
- 报告对比（源库 vs 目标库对象数量差异验证）
