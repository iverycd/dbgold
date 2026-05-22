# 数据迁移报告 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在 MySQL→PostgreSQL 数据迁移完成后生成结构化迁移报告，展示各类对象（表、数据、视图、索引、外键、序列、触发器）的迁移成功/失败统计，失败对象可行内展开查看原因和 DDL，报告同时支持迁移后原地查看和历史记录回查。

**Architecture:** Migrator.Run() 在内存中收集 MigrationReport，Run() 结束时返回 report，handler 序列化为 JSON 存入 DataMigrationReport 数据库表；前端通过 GET /api/migration/data-migrate/:jobID/report 获取报告，用新增的 MigrationReportPanel.vue 组件渲染，该组件被嵌入 MigrationView（迁移完成后自动展示）和 HistoryView（历史任务的「查看报告」按钮）。

**Tech Stack:** Go 1.25 / GORM / SQLite / gin / Vue 3 / TypeScript / @arco-design/web-vue

---

## 文件结构

| 文件 | 操作 | 职责 |
|------|------|------|
| `datamigrate/report.go` | 新建 | MigrationReport 类型定义 + DDL builder 函数 |
| `datamigrate/report_test.go` | 新建 | DDL builder 单元测试 |
| `datamigrate/source/interface.go` | 修改 | Reader 接口新增 GetTriggerCount |
| `datamigrate/source/mysql.go` | 修改 | MySQLReader 实现 GetTriggerCount |
| `datamigrate/migrator.go` | 修改 | Run() 收集并返回 MigrationReport |
| `datamigrate/migrator_test.go` | 修改 | 更新 mock + 新增报告收集测试 |
| `store/migration_report.go` | 新建 | DataMigrationReport 模型 + CRUD |
| `store/db.go` | 修改 | AutoMigrate 新增 DataMigrationReport |
| `api/handler/datamigration.go` | 修改 | 持久化报告 + GetDataMigrationReport handler |
| `api/router.go` | 修改 | 注册报告 API 路由 |
| `frontend/src/api/migration.ts` | 修改 | 新增接口类型 + getDataMigrationReport 函数 |
| `frontend/src/views/MigrationReportPanel.vue` | 新建 | 报告展示组件（表格 + 行内展开） |
| `frontend/src/views/MigrationView.vue` | 修改 | 迁移完成后嵌入 MigrationReportPanel |
| `frontend/src/views/HistoryView.vue` | 修改 | 数据迁移历史 tab + 「查看报告」按钮 |

---

## Task 1: 定义 MigrationReport 类型和 DDL builder 函数

**Files:**
- Create: `datamigrate/report.go`
- Create: `datamigrate/report_test.go`

- [ ] **Step 1: 写失败测试**

```go
// datamigrate/report_test.go
package datamigrate

import (
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/assert"
)

func TestSequenceDDL(t *testing.T) {
	seq := source.SequenceInfo{TableName: "users", ColumnName: "id", StartValue: 1}
	ddl := SequenceDDL(seq)
	assert.Contains(t, ddl, `CREATE SEQUENCE IF NOT EXISTS "seq_users_id" START 1`)
	assert.Contains(t, ddl, `ALTER TABLE "users" ALTER COLUMN "id" SET DEFAULT nextval`)
}

func TestIndexDDL_Unique(t *testing.T) {
	idx := source.IndexInfo{TableName: "users", IndexName: "idx_users_email", Columns: []string{"email"}, IsUnique: true}
	ddl := IndexDDL(idx)
	assert.Equal(t, `CREATE UNIQUE INDEX "idx_users_email" ON "users" ("email")`, ddl)
}

func TestIndexDDL_Primary(t *testing.T) {
	idx := source.IndexInfo{TableName: "users", IndexName: "PRIMARY", Columns: []string{"id"}, IsPrimary: true}
	ddl := IndexDDL(idx)
	assert.Equal(t, `ALTER TABLE "users" ADD PRIMARY KEY ("id")`, ddl)
}

func TestIndexDDL_Regular(t *testing.T) {
	idx := source.IndexInfo{TableName: "orders", IndexName: "idx_orders_user", Columns: []string{"user_id"}, IsUnique: false}
	ddl := IndexDDL(idx)
	assert.Equal(t, `CREATE INDEX "idx_orders_user" ON "orders" ("user_id")`, ddl)
}

func TestFKDDL_WithOnDelete(t *testing.T) {
	fk := source.FKInfo{
		TableName: "orders", ConstraintName: "fk_orders_user",
		Columns: []string{"user_id"}, RefTable: "users", RefColumns: []string{"id"},
		OnDelete: "CASCADE", OnUpdate: "",
	}
	ddl := FKDDL(fk)
	assert.Contains(t, ddl, `ADD CONSTRAINT "fk_orders_user"`)
	assert.Contains(t, ddl, `REFERENCES "users" ("id")`)
	assert.Contains(t, ddl, `ON DELETE CASCADE`)
	assert.NotContains(t, ddl, `ON UPDATE`)
}

func TestFKDDL_WithOnUpdate(t *testing.T) {
	fk := source.FKInfo{
		TableName: "orders", ConstraintName: "fk_orders_product",
		Columns: []string{"product_id"}, RefTable: "products", RefColumns: []string{"id"},
		OnDelete: "", OnUpdate: "RESTRICT",
	}
	ddl := FKDDL(fk)
	assert.Contains(t, ddl, `ON UPDATE RESTRICT`)
	assert.NotContains(t, ddl, `ON DELETE`)
}

func TestNewMigrationReport_ItemsNotNil(t *testing.T) {
	r := newMigrationReport()
	assert.NotNil(t, r.Tables.Items)
	assert.NotNil(t, r.Data.Items)
	assert.NotNil(t, r.Views.Items)
	assert.NotNil(t, r.Indexes.Items)
	assert.NotNil(t, r.Constraints.Items)
	assert.NotNil(t, r.Sequences.Items)
	assert.NotNil(t, r.Triggers.Items)
}
```

- [ ] **Step 2: 运行测试确认失败**

```bash
cd /Users/kay/Documents/GoProj/dbgold
/Users/kay/sdk/go1.25.5/bin/go test ./datamigrate/... -run "TestSequenceDDL|TestIndexDDL|TestFKDDL|TestNewMigrationReport" -v
```

Expected: FAIL（SequenceDDL、IndexDDL、FKDDL、newMigrationReport 未定义）

- [ ] **Step 3: 实现 report.go**

```go
// datamigrate/report.go
package datamigrate

import (
	"fmt"
	"strings"

	"dbgold/datamigrate/source"
)

// ObjectResult 单个失败对象的详情
type ObjectResult struct {
	Name  string `json:"name"`
	DDL   string `json:"ddl"`
	Error string `json:"error"`
}

// CategoryReport 一类对象的迁移统计
type CategoryReport struct {
	Total   int            `json:"total"`
	Success int            `json:"success"`
	Failed  int            `json:"failed"`
	Items   []ObjectResult `json:"items"`
}

// MigrationReport 完整迁移报告
type MigrationReport struct {
	Tables      CategoryReport `json:"tables"`
	Data        CategoryReport `json:"data"`
	Views       CategoryReport `json:"views"`
	Indexes     CategoryReport `json:"indexes"`
	Constraints CategoryReport `json:"constraints"`
	Sequences   CategoryReport `json:"sequences"`
	Triggers    CategoryReport `json:"triggers"`
}

func newCategoryReport() CategoryReport {
	return CategoryReport{Items: []ObjectResult{}}
}

func newMigrationReport() MigrationReport {
	return MigrationReport{
		Tables:      newCategoryReport(),
		Data:        newCategoryReport(),
		Views:       newCategoryReport(),
		Indexes:     newCategoryReport(),
		Constraints: newCategoryReport(),
		Sequences:   newCategoryReport(),
		Triggers:    newCategoryReport(),
	}
}

func quoteColumns(cols []string) string {
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = fmt.Sprintf(`"%s"`, c)
	}
	return strings.Join(quoted, ", ")
}

// SequenceDDL 从 SequenceInfo 重建 PostgreSQL 序列 DDL
func SequenceDDL(seq source.SequenceInfo) string {
	seqName := fmt.Sprintf("seq_%s_%s", seq.TableName, seq.ColumnName)
	return fmt.Sprintf(
		"CREATE SEQUENCE IF NOT EXISTS \"%s\" START %d;\nALTER TABLE \"%s\" ALTER COLUMN \"%s\" SET DEFAULT nextval('\"%s\"')",
		seqName, seq.StartValue, seq.TableName, seq.ColumnName, seqName,
	)
}

// IndexDDL 从 IndexInfo 重建 PostgreSQL 索引/主键 DDL
func IndexDDL(idx source.IndexInfo) string {
	cols := quoteColumns(idx.Columns)
	if idx.IsPrimary {
		return fmt.Sprintf(`ALTER TABLE "%s" ADD PRIMARY KEY (%s)`, idx.TableName, cols)
	}
	unique := ""
	if idx.IsUnique {
		unique = "UNIQUE "
	}
	return fmt.Sprintf(`CREATE %sINDEX "%s" ON "%s" (%s)`, unique, idx.IndexName, idx.TableName, cols)
}

// FKDDL 从 FKInfo 重建 PostgreSQL 外键 DDL
func FKDDL(fk source.FKInfo) string {
	cols := quoteColumns(fk.Columns)
	refCols := quoteColumns(fk.RefColumns)
	s := fmt.Sprintf(`ALTER TABLE "%s" ADD CONSTRAINT "%s" FOREIGN KEY (%s) REFERENCES "%s" (%s)`,
		fk.TableName, fk.ConstraintName, cols, fk.RefTable, refCols)
	if fk.OnDelete != "" {
		s += " ON DELETE " + fk.OnDelete
	}
	if fk.OnUpdate != "" {
		s += " ON UPDATE " + fk.OnUpdate
	}
	return s
}
```

- [ ] **Step 4: 运行测试确认通过**

```bash
/Users/kay/sdk/go1.25.5/bin/go test ./datamigrate/... -run "TestSequenceDDL|TestIndexDDL|TestFKDDL|TestNewMigrationReport" -v
```

Expected: PASS（7 tests）

- [ ] **Step 5: 提交**

```bash
git add datamigrate/report.go datamigrate/report_test.go
git commit -m "feat: add MigrationReport types and DDL builder functions"
```

---

## Task 2: 在 source.Reader 接口中新增 GetTriggerCount，并在 MySQLReader 实现

**Files:**
- Modify: `datamigrate/source/interface.go`
- Modify: `datamigrate/source/mysql.go`
- Modify: `datamigrate/migrator_test.go` （更新 mock）

- [ ] **Step 1: 在 interface.go 的 Reader 接口末尾追加方法**

在 `datamigrate/source/interface.go` 的 `Reader` interface 最后一个方法 `GetViews` 后追加：

```go
	// GetTriggerCount 返回源库触发器总数，失败时返回 0 和 error
	GetTriggerCount(ctx context.Context) (int, error)
```

完整 Reader interface 末尾应为：

```go
	// GetViews 返回所有视图信息
	GetViews(ctx context.Context) ([]ViewInfo, error)
	// GetTriggerCount 返回源库触发器总数，失败时返回 0 和 error
	GetTriggerCount(ctx context.Context) (int, error)
}
```

- [ ] **Step 2: 在 mysql.go 末尾追加 GetTriggerCount 实现**

在 `datamigrate/source/mysql.go` 文件末尾追加：

```go
// GetTriggerCount 查询 information_schema.TRIGGERS 返回触发器总数
func (r *MySQLReader) GetTriggerCount(ctx context.Context) (int, error) {
	var count int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = ?`,
		r.dbName,
	).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}
```

- [ ] **Step 3: 更新 migrator_test.go 中的 mockReader**

在 `datamigrate/migrator_test.go` 的 mockReader 方法列表末尾（GetViews 之后）追加：

```go
func (m *mockReader) GetTriggerCount(_ context.Context) (int, error) { return 2, nil }
```

- [ ] **Step 4: 确认编译通过**

```bash
/Users/kay/sdk/go1.25.5/bin/go build ./datamigrate/...
```

Expected: 无错误（interface 已满足）

- [ ] **Step 5: 运行现有测试确认无回归**

```bash
/Users/kay/sdk/go1.25.5/bin/go test ./datamigrate/... -v
```

Expected: PASS（所有现有测试）

- [ ] **Step 6: 提交**

```bash
git add datamigrate/source/interface.go datamigrate/source/mysql.go datamigrate/migrator_test.go
git commit -m "feat: add GetTriggerCount to source.Reader interface and MySQLReader"
```

---

## Task 3: 新增 DataMigrationReport store 模型

**Files:**
- Create: `store/migration_report.go`
- Modify: `store/db.go`

- [ ] **Step 1: 创建 store/migration_report.go**

```go
// store/migration_report.go
package store

import "time"

type DataMigrationReport struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	JobID      string    `gorm:"uniqueIndex;not null" json:"job_id"`
	ReportJSON string    `gorm:"type:text" json:"report_json"`
	CreatedAt  time.Time `json:"created_at"`
}

func CreateDataMigrationReport(r *DataMigrationReport) error {
	return DB.Create(r).Error
}

func GetDataMigrationReport(jobID string) (*DataMigrationReport, error) {
	var r DataMigrationReport
	if err := DB.Where("job_id = ?", jobID).First(&r).Error; err != nil {
		return nil, err
	}
	return &r, nil
}
```

- [ ] **Step 2: 修改 store/db.go 的 AutoMigrate 调用**

将：
```go
	if err := DB.AutoMigrate(&User{}, &Connection{}, &MigrationHistory{}, &DataMigrationJob{}); err != nil {
```

改为：
```go
	if err := DB.AutoMigrate(&User{}, &Connection{}, &MigrationHistory{}, &DataMigrationJob{}, &DataMigrationReport{}); err != nil {
```

- [ ] **Step 3: 确认编译通过**

```bash
/Users/kay/sdk/go1.25.5/bin/go build ./store/...
```

Expected: 无错误

- [ ] **Step 4: 提交**

```bash
git add store/migration_report.go store/db.go
git commit -m "feat: add DataMigrationReport store model and CRUD"
```

---

## Task 4: 重构 Migrator.Run() 收集并返回 MigrationReport

**Files:**
- Modify: `datamigrate/migrator.go`
- Modify: `datamigrate/migrator_test.go`

- [ ] **Step 1: 在 migrator_test.go 中更新 mockWriter 和现有测试，新增报告验证测试**

将 `datamigrate/migrator_test.go` 的全部内容替换为：

```go
// datamigrate/migrator_test.go
package datamigrate

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/assert"
)

// mockReader 实现 source.Reader 接口，用于测试
type mockReader struct {
	tables       []string
	ddl          map[string]*source.TableDDLInfo
	pk           map[string]string
	rows         map[string][][]interface{}
	triggerCount int
}

func (m *mockReader) DBType() string { return "mysql" }
func (m *mockReader) Close() error   { return nil }
func (m *mockReader) ListTables(_ context.Context) ([]string, error) { return m.tables, nil }
func (m *mockReader) GetTableDDLInfo(_ context.Context, t string) (*source.TableDDLInfo, error) {
	return m.ddl[t], nil
}
func (m *mockReader) GetPrimaryKey(_ context.Context, t string) (string, error) {
	return m.pk[t], nil
}
func (m *mockReader) ReadPage(_ context.Context, t, _ string, _, _ int64) ([]string, [][]interface{}, error) {
	rows := m.rows[t]
	if len(rows) == 0 {
		return []string{"id"}, nil, nil
	}
	return []string{"id"}, rows, nil
}
func (m *mockReader) GetSequences(_ context.Context) ([]source.SequenceInfo, error) { return nil, nil }
func (m *mockReader) GetIndexes(_ context.Context) ([]source.IndexInfo, error)      { return nil, nil }
func (m *mockReader) GetForeignKeys(_ context.Context) ([]source.FKInfo, error)     { return nil, nil }
func (m *mockReader) GetViews(_ context.Context) ([]source.ViewInfo, error)         { return nil, nil }
func (m *mockReader) GetTriggerCount(_ context.Context) (int, error)                { return m.triggerCount, nil }

// mockWriter 实现 target.Writer 接口，用于测试
type mockWriter struct {
	created         []string
	copied          []string
	createTableFail bool // 若 true，所有 CreateTable 调用返回错误
	copyDataFail    bool // 若 true，所有 CopyData 调用返回错误
}

func (m *mockWriter) DBType() string { return "postgres" }
func (m *mockWriter) Close() error   { return nil }
func (m *mockWriter) CreateTable(_ context.Context, ddl string) error {
	if m.createTableFail {
		return fmt.Errorf("create table failed")
	}
	m.created = append(m.created, ddl)
	return nil
}
func (m *mockWriter) CopyData(_ context.Context, table string, _ []string, _ [][]interface{}) error {
	if m.copyDataFail {
		return fmt.Errorf("copy data failed")
	}
	m.copied = append(m.copied, table)
	return nil
}
func (m *mockWriter) CreateSequence(_ context.Context, _ source.SequenceInfo) error { return nil }
func (m *mockWriter) CreateIndex(_ context.Context, _ source.IndexInfo) error       { return nil }
func (m *mockWriter) CreateForeignKey(_ context.Context, _ source.FKInfo) error     { return nil }
func (m *mockWriter) CreateView(_ context.Context, _ source.ViewInfo) error         { return nil }

func newTestMigrator(reader source.Reader, writer target.Writer) (*Migrator, *Job) {
	_, cancel := context.WithCancel(context.Background())
	job := &Job{
		LogCh:  make(chan string, 512),
		Cancel: cancel,
	}
	cfg := Config{PageSize: 10, MaxParallel: 2, Mode: "all", Filter: ""}
	return NewMigrator(reader, writer, job, cfg), job
}

func drainLogs(job *Job) []string {
	close(job.LogCh)
	var logs []string
	for l := range job.LogCh {
		logs = append(logs, l)
	}
	return logs
}

func TestMigratorRun_AllTables(t *testing.T) {
	reader := &mockReader{
		tables: []string{"users"},
		ddl: map[string]*source.TableDDLInfo{
			"users": {TableName: "users", Columns: []source.ColumnInfo{
				{Name: "id", DataType: "int", IsNullable: false},
				{Name: "name", DataType: "varchar", Length: 100, IsNullable: true},
			}},
		},
		pk:           map[string]string{"users": "id"},
		rows:         map[string][][]interface{}{"users": {{1, "Alice"}, {2, "Bob"}}},
		triggerCount: 3,
	}
	writer := &mockWriter{}
	m, job := newTestMigrator(reader, writer)

	report := m.Run(context.Background())
	logs := drainLogs(job)

	assert.Contains(t, writer.copied, "users")
	assert.Equal(t, 1, report.Tables.Total)
	assert.Equal(t, 1, report.Tables.Success)
	assert.Equal(t, 0, report.Tables.Failed)
	assert.Equal(t, 1, report.Data.Success)
	assert.Equal(t, 0, report.Data.Failed)
	assert.Equal(t, 3, report.Triggers.Total)

	hasDone := false
	for _, l := range logs {
		if strings.Contains(l, "[DONE]") {
			hasDone = true
		}
	}
	assert.True(t, hasDone, "should emit [DONE] log")
}

func TestMigratorRun_TableCreationFailed(t *testing.T) {
	reader := &mockReader{
		tables: []string{"users"},
		ddl: map[string]*source.TableDDLInfo{
			"users": {TableName: "users", Columns: []source.ColumnInfo{
				{Name: "id", DataType: "int", IsNullable: false},
			}},
		},
		pk: map[string]string{},
	}
	writer := &mockWriter{createTableFail: true}
	m, job := newTestMigrator(reader, writer)

	report := m.Run(context.Background())
	drainLogs(job)

	assert.Equal(t, 1, report.Tables.Failed)
	assert.Equal(t, 0, report.Tables.Success)
	assert.Equal(t, 1, len(report.Tables.Items))
	assert.Equal(t, "users", report.Tables.Items[0].Name)
	assert.NotEmpty(t, report.Tables.Items[0].Error)
	// 建表失败的表不参与数据迁移
	assert.Equal(t, 0, report.Data.Total)
}

func TestMigratorRun_DataWriteFailed(t *testing.T) {
	reader := &mockReader{
		tables: []string{"users"},
		ddl: map[string]*source.TableDDLInfo{
			"users": {TableName: "users", Columns: []source.ColumnInfo{
				{Name: "id", DataType: "int", IsNullable: false},
			}},
		},
		pk:   map[string]string{"users": "id"},
		rows: map[string][][]interface{}{"users": {{1}, {2}}},
	}
	writer := &mockWriter{copyDataFail: true}
	m, job := newTestMigrator(reader, writer)

	report := m.Run(context.Background())
	drainLogs(job)

	assert.Equal(t, 0, report.Tables.Failed)
	assert.Equal(t, 1, report.Data.Failed)
	assert.Equal(t, 1, len(report.Data.Items))
	assert.Equal(t, "users", report.Data.Items[0].Name)
	assert.NotEmpty(t, report.Data.Items[0].Error)
	assert.Empty(t, report.Data.Items[0].DDL)
}

func TestMigratorRun_ContextCancelled(t *testing.T) {
	reader := &mockReader{tables: []string{"users"}, ddl: map[string]*source.TableDDLInfo{
		"users": {TableName: "users"},
	}, pk: map[string]string{}}
	writer := &mockWriter{}
	m, job := newTestMigrator(reader, writer)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	m.Run(ctx)
	logs := drainLogs(job)

	hasCancelled := false
	for _, l := range logs {
		if strings.Contains(l, "取消") || strings.Contains(l, "cancelled") {
			hasCancelled = true
		}
	}
	assert.True(t, hasCancelled)
}
```

- [ ] **Step 2: 运行测试确认失败（Run 返回值类型不匹配）**

```bash
/Users/kay/sdk/go1.25.5/bin/go test ./datamigrate/... -run "TestMigratorRun" -v 2>&1 | head -30
```

Expected: 编译错误（Run() 没有返回值）

- [ ] **Step 3: 重写 datamigrate/migrator.go**

将 `datamigrate/migrator.go` 全部内容替换为：

```go
// datamigrate/migrator.go
package datamigrate

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/datamigrate/typemap"
)

// Config 迁移任务配置
type Config struct {
	PageSize    int
	MaxParallel int
	Mode        string
	Filter      string
}

// Migrator 串联三阶段迁移：DDL → 数据 → Post-DDL
type Migrator struct {
	reader source.Reader
	writer target.Writer
	job    *Job
	cfg    Config
	log    *Logger
}

// NewMigrator 创建 Migrator
func NewMigrator(reader source.Reader, writer target.Writer, job *Job, cfg Config) *Migrator {
	return &Migrator{reader: reader, writer: writer, job: job, cfg: cfg, log: NewLogger(job.LogCh)}
}

// Run 执行完整的三阶段迁移，返回 MigrationReport；结束时不关闭 job.LogCh（由调用方关闭）
func (m *Migrator) Run(ctx context.Context) MigrationReport {
	report := newMigrationReport()
	start := time.Now()

	// 查询触发器总数（失败时 Total=-1，前端展示"获取失败"）
	if count, err := m.reader.GetTriggerCount(ctx); err != nil {
		report.Triggers.Total = -1
	} else {
		report.Triggers.Total = count
	}

	if err := ctx.Err(); err != nil {
		m.log.Warn("任务已取消")
		return report
	}

	allTables, err := m.reader.ListTables(ctx)
	if err != nil {
		m.log.Errorf("获取表列表失败: %v", err)
		return report
	}
	tables := FilterTables(allTables, m.cfg.Mode, m.cfg.Filter)
	m.log.Infof("开始迁移任务，共 %d 张表，pageSize=%d，maxParallel=%d",
		len(tables), m.cfg.PageSize, m.cfg.MaxParallel)

	report.Tables.Total = len(tables)

	// Phase 1: 建表 DDL（串行）
	m.log.Info("=== Phase 1: 创建表结构 ===")
	tablesFailed := map[string]bool{}
	for _, table := range tables {
		if ctx.Err() != nil {
			m.log.Warn("任务已取消")
			return report
		}
		ddl, err := m.buildCreateTableDDL(ctx, table)
		if err != nil {
			m.log.Errorf("生成建表 DDL 失败 [%s]: %v", table, err)
			tablesFailed[table] = true
			report.Tables.Failed++
			report.Tables.Items = append(report.Tables.Items, ObjectResult{Name: table, DDL: "", Error: err.Error()})
			continue
		}
		if err := m.writer.CreateTable(ctx, ddl); err != nil {
			m.log.Errorf("创建表失败 [%s]: %v", table, err)
			tablesFailed[table] = true
			report.Tables.Failed++
			report.Tables.Items = append(report.Tables.Items, ObjectResult{Name: table, DDL: ddl, Error: err.Error()})
			continue
		}
		m.log.DDLf("创建表 %s ... OK", table)
		report.Tables.Success++
	}

	// Phase 2: 迁移数据（并发）
	m.log.Info("=== Phase 2: 迁移数据 ===")
	report.Data.Total = len(tables) - len(tablesFailed)
	sem := make(chan struct{}, m.cfg.MaxParallel)
	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, table := range tables {
		if tablesFailed[table] {
			continue
		}
		if ctx.Err() != nil {
			m.log.Warn("任务已取消")
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(tbl string) {
			defer wg.Done()
			defer func() { <-sem }()
			ok, firstErr := m.migrateTableData(ctx, tbl)
			mu.Lock()
			if ok {
				report.Data.Success++
			} else {
				report.Data.Failed++
				report.Data.Items = append(report.Data.Items, ObjectResult{Name: tbl, DDL: "", Error: firstErr})
			}
			mu.Unlock()
		}(table)
	}
	wg.Wait()

	// Phase 3: Post-DDL（串行）
	m.log.Info("=== Phase 3: 创建序列、索引、外键、视图 ===")
	m.createPostDDL(ctx, &report)

	elapsed := time.Since(start).Round(time.Second)
	m.log.Donef("迁移完成：成功 %d 张，失败 %d 张，耗时 %s",
		report.Data.Success, report.Tables.Failed+report.Data.Failed, elapsed)
	return report
}

// buildCreateTableDDL 根据源库列信息生成目标库建表 DDL
func (m *Migrator) buildCreateTableDDL(ctx context.Context, table string) (string, error) {
	info, err := m.reader.GetTableDDLInfo(ctx, table)
	if err != nil {
		return "", err
	}
	var cols []string
	for _, col := range info.Columns {
		pgType := typemap.MySQLToPG(col)
		colDef := fmt.Sprintf(`"%s" %s`, col.Name, pgType)
		if !col.IsNullable {
			colDef += " NOT NULL"
		}
		if col.Default != nil && col.Extra != "auto_increment" {
			escapedDefault := strings.ReplaceAll(*col.Default, "'", "''")
			colDef += fmt.Sprintf(" DEFAULT '%s'", escapedDefault)
		}
		cols = append(cols, "  "+colDef)
	}
	ddl := fmt.Sprintf("DROP TABLE IF EXISTS \"%s\";\nCREATE TABLE \"%s\" (\n%s\n);",
		table, table, strings.Join(cols, ",\n"))
	return ddl, nil
}

// migrateTableData 迁移单张表的数据，返回（是否成功，首次错误信息）
func (m *Migrator) migrateTableData(ctx context.Context, table string) (bool, string) {
	pk, err := m.reader.GetPrimaryKey(ctx, table)
	if err != nil {
		m.log.Errorf("获取主键失败 [%s]: %v", table, err)
		return false, err.Error()
	}
	var offset int64
	pageNum := 0
	firstErr := ""
	hasError := false
	for {
		if ctx.Err() != nil {
			if firstErr == "" {
				firstErr = "任务已取消"
			}
			return false, firstErr
		}
		cols, rows, err := m.reader.ReadPage(ctx, table, pk, offset, int64(m.cfg.PageSize))
		if err != nil {
			m.log.Errorf("读取数据失败 [%s] 第 %d 页: %v", table, pageNum+1, err)
			return false, err.Error()
		}
		if len(rows) == 0 {
			break
		}
		if err := m.writer.CopyData(ctx, table, cols, rows); err != nil {
			m.log.Errorf("写入数据失败 [%s] 第 %d 页: %v", table, pageNum+1, err)
			if !hasError {
				firstErr = err.Error()
				hasError = true
			}
			offset += int64(m.cfg.PageSize)
			continue
		}
		pageNum++
		m.log.Dataf("迁移 %s: 第 %d 页 (%d 行) ... OK", table, pageNum, len(rows))
		if len(rows) < m.cfg.PageSize {
			break
		}
		offset += int64(m.cfg.PageSize)
	}
	if hasError {
		return false, firstErr
	}
	return true, ""
}

// createPostDDL 串行创建序列、索引、外键、视图，并填充 report
func (m *Migrator) createPostDDL(ctx context.Context, report *MigrationReport) {
	seqs, err := m.reader.GetSequences(ctx)
	if err != nil {
		m.log.Errorf("获取序列信息失败: %v", err)
	} else {
		report.Sequences.Total = len(seqs)
		for _, seq := range seqs {
			if ctx.Err() != nil {
				return
			}
			ddl := SequenceDDL(seq)
			if err := m.writer.CreateSequence(ctx, seq); err != nil {
				m.log.Errorf("创建序列失败 [%s.%s]: %v", seq.TableName, seq.ColumnName, err)
				report.Sequences.Failed++
				report.Sequences.Items = append(report.Sequences.Items, ObjectResult{
					Name:  fmt.Sprintf("%s.%s", seq.TableName, seq.ColumnName),
					DDL:   ddl,
					Error: err.Error(),
				})
			} else {
				m.log.Indexf("创建序列 seq_%s_%s ... OK", seq.TableName, seq.ColumnName)
				report.Sequences.Success++
			}
		}
	}

	indexes, err := m.reader.GetIndexes(ctx)
	if err != nil {
		m.log.Errorf("获取索引信息失败: %v", err)
	} else {
		report.Indexes.Total = len(indexes)
		for _, idx := range indexes {
			if ctx.Err() != nil {
				return
			}
			ddl := IndexDDL(idx)
			if err := m.writer.CreateIndex(ctx, idx); err != nil {
				m.log.Errorf("创建索引失败 [%s]: %v", idx.IndexName, err)
				report.Indexes.Failed++
				report.Indexes.Items = append(report.Indexes.Items, ObjectResult{
					Name:  idx.IndexName,
					DDL:   ddl,
					Error: err.Error(),
				})
			} else {
				m.log.Indexf("创建索引 %s ... OK", idx.IndexName)
				report.Indexes.Success++
			}
		}
	}

	fks, err := m.reader.GetForeignKeys(ctx)
	if err != nil {
		m.log.Errorf("获取外键信息失败: %v", err)
	} else {
		report.Constraints.Total = len(fks)
		for _, fk := range fks {
			if ctx.Err() != nil {
				return
			}
			ddl := FKDDL(fk)
			if err := m.writer.CreateForeignKey(ctx, fk); err != nil {
				m.log.Errorf("创建外键失败 [%s]: %v", fk.ConstraintName, err)
				report.Constraints.Failed++
				report.Constraints.Items = append(report.Constraints.Items, ObjectResult{
					Name:  fk.ConstraintName,
					DDL:   ddl,
					Error: err.Error(),
				})
			} else {
				m.log.Indexf("创建外键 %s ... OK", fk.ConstraintName)
				report.Constraints.Success++
			}
		}
	}

	views, err := m.reader.GetViews(ctx)
	if err != nil {
		m.log.Errorf("获取视图信息失败: %v", err)
	} else {
		report.Views.Total = len(views)
		for _, v := range views {
			if ctx.Err() != nil {
				return
			}
			if err := m.writer.CreateView(ctx, v); err != nil {
				m.log.Errorf("创建视图失败 [%s]: %v", v.ViewName, err)
				report.Views.Failed++
				report.Views.Items = append(report.Views.Items, ObjectResult{
					Name:  v.ViewName,
					DDL:   v.Definition,
					Error: err.Error(),
				})
			} else {
				m.log.DDLf("创建视图 %s ... OK", v.ViewName)
				report.Views.Success++
			}
		}
	}
}
```

- [ ] **Step 4: 运行所有 datamigrate 测试**

```bash
/Users/kay/sdk/go1.25.5/bin/go test ./datamigrate/... -v
```

Expected: PASS（所有测试，包括 TestMigratorRun_TableCreationFailed 和 TestMigratorRun_DataWriteFailed）

- [ ] **Step 5: 提交**

```bash
git add datamigrate/migrator.go datamigrate/migrator_test.go
git commit -m "feat: refactor Migrator.Run to collect and return MigrationReport"
```

---

## Task 5: 在 handler 中持久化报告并新增 GetDataMigrationReport

**Files:**
- Modify: `api/handler/datamigration.go`

- [ ] **Step 1: 在 datamigration.go 的 import 块中追加 "encoding/json"**

将现有 import 块：

```go
import (
	"context"
	"fmt"
	"net/http"
	"time"

	"dbgold/datamigrate"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/middleware"
	"dbgold/store"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)
```

替换为：

```go
import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"dbgold/datamigrate"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/middleware"
	"dbgold/store"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)
```

- [ ] **Step 2: 在 goroutine 内的 m.Run(ctx) 调用处收集报告并持久化**

将：

```go
		m := datamigrate.NewMigrator(reader, writer, job, cfg)
		m.Run(ctx)

		status := "done"
		if ctx.Err() != nil {
			status = "cancelled"
		}
		updateJobStatus(dbJob, status, "")
```

替换为：

```go
		m := datamigrate.NewMigrator(reader, writer, job, cfg)
		report := m.Run(ctx)

		if reportJSON, err := json.Marshal(report); err == nil {
			_ = store.CreateDataMigrationReport(&store.DataMigrationReport{
				JobID:      jobID,
				ReportJSON: string(reportJSON),
			})
		}

		status := "done"
		if ctx.Err() != nil {
			status = "cancelled"
		}
		updateJobStatus(dbJob, status, "")
```

- [ ] **Step 3: 在文件末尾追加 GetDataMigrationReport handler**

```go
// GetDataMigrationReport 返回指定任务的迁移报告 JSON
func GetDataMigrationReport(c *gin.Context) {
	jobID := c.Param("jobID")
	r, err := store.GetDataMigrationReport(jobID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "报告不存在"})
		return
	}
	c.Data(http.StatusOK, "application/json", []byte(r.ReportJSON))
}
```

- [ ] **Step 4: 编译确认**

```bash
/Users/kay/sdk/go1.25.5/bin/go build ./api/...
```

Expected: 无错误

- [ ] **Step 5: 提交**

```bash
git add api/handler/datamigration.go
git commit -m "feat: persist migration report and add GetDataMigrationReport handler"
```

---

## Task 6: 注册报告 API 路由

**Files:**
- Modify: `api/router.go`

- [ ] **Step 1: 在 authed group 的数据迁移路由块中追加报告路由**

在 `api/router.go` 中，找到：

```go
		authed.GET("/migration/data-migrate/jobs", handler.ListDataMigrationJobs)
```

在其后追加：

```go
		authed.GET("/migration/data-migrate/:jobID/report", handler.GetDataMigrationReport)
```

- [ ] **Step 2: 编译确认**

```bash
/Users/kay/sdk/go1.25.5/bin/go build ./...
```

Expected: 无错误

- [ ] **Step 3: 运行全部 Go 测试**

```bash
/Users/kay/sdk/go1.25.5/bin/go test ./... -v 2>&1 | tail -20
```

Expected: 所有测试 PASS

- [ ] **Step 4: 提交**

```bash
git add api/router.go
git commit -m "feat: register GET /migration/data-migrate/:jobID/report route"
```

---

## Task 7: 前端 API 客户端新增类型和函数

**Files:**
- Modify: `frontend/src/api/migration.ts`

- [ ] **Step 1: 在 migration.ts 的 createDataMigrateEventSource 之后追加**

```typescript
// ===== 迁移报告 =====

export interface ObjectResult {
  name: string
  ddl: string
  error: string
}

export interface CategoryReport {
  total: number
  success: number
  failed: number
  items: ObjectResult[]
}

export interface MigrationReport {
  tables: CategoryReport
  data: CategoryReport
  views: CategoryReport
  indexes: CategoryReport
  constraints: CategoryReport
  sequences: CategoryReport
  triggers: CategoryReport
}

export const getDataMigrationReport = (jobID: string) =>
  api.get<MigrationReport>(`/migration/data-migrate/${jobID}/report`)
```

- [ ] **Step 2: 编译前端确认类型无误**

```bash
cd /Users/kay/Documents/GoProj/dbgold/frontend
npm run type-check 2>&1 | tail -10
```

Expected: 无类型错误

- [ ] **Step 3: 提交**

```bash
cd /Users/kay/Documents/GoProj/dbgold
git add frontend/src/api/migration.ts
git commit -m "feat: add MigrationReport TypeScript interfaces and getDataMigrationReport function"
```

---

## Task 8: 创建 MigrationReportPanel.vue 组件

**Files:**
- Create: `frontend/src/views/MigrationReportPanel.vue`

- [ ] **Step 1: 创建组件文件**

```vue
<!-- frontend/src/views/MigrationReportPanel.vue -->
<template>
  <div class="migration-report-panel">
    <div v-if="loading" style="text-align: center; padding: 24px">
      <a-spin />
    </div>

    <a-alert v-else-if="fetchError" type="error" :content="fetchError" />

    <template v-else-if="report">
      <a-table
        :data="tableRows"
        row-key="key"
        :pagination="false"
        size="small"
        :expandable="{ rowExpandable: (record: ReportRow) => record.failed > 0 }"
      >
        <template #columns>
          <a-table-column title="对象类型" data-index="label" :width="120" />
          <a-table-column title="总数" data-index="total" :width="80" />
          <a-table-column title="成功" :width="80">
            <template #cell="{ record }: { record: ReportRow }">
              <span v-if="record.isTrigger">—</span>
              <span v-else>{{ record.success }}</span>
            </template>
          </a-table-column>
          <a-table-column title="失败" :width="80">
            <template #cell="{ record }: { record: ReportRow }">
              <span v-if="record.isTrigger">—</span>
              <span v-else style="color: red">{{ record.failed > 0 ? record.failed : record.failed }}</span>
            </template>
          </a-table-column>
          <a-table-column title="状态">
            <template #cell="{ record }: { record: ReportRow }">
              <span v-if="record.isTrigger" style="color: #86909c">
                ⊘ 未迁移（{{ record.total === -1 ? '获取失败' : record.total + ' 个' }}）
              </span>
              <a-tag v-else-if="record.failed > 0" color="orange">⚠ 部分失败</a-tag>
              <a-tag v-else-if="record.total === 0" color="gray">无对象</a-tag>
              <a-tag v-else color="green">✓ 全部成功</a-tag>
            </template>
          </a-table-column>
        </template>

        <template #expand-row="{ record }: { record: ReportRow }">
          <div class="failure-list">
            <div
              v-for="item in record.items"
              :key="item.name"
              class="failure-item"
            >
              <div class="failure-name">{{ item.name }}</div>
              <div class="failure-error">失败原因：{{ item.error }}</div>
              <div class="failure-ddl">
                <template v-if="item.ddl">
                  <div class="ddl-header">
                    <span>DDL：</span>
                    <a-button size="mini" type="text" @click="copyDDL(item.ddl)">复制 DDL</a-button>
                  </div>
                  <pre class="ddl-code">{{ item.ddl }}</pre>
                </template>
                <span v-else style="color: #86909c">DDL：—</span>
              </div>
            </div>
          </div>
        </template>
      </a-table>
    </template>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { Message } from '@arco-design/web-vue'
import {
  getDataMigrationReport,
  type MigrationReport,
  type ObjectResult,
} from '@/api/migration'

const props = defineProps<{ jobID: string }>()

const report = ref<MigrationReport | null>(null)
const loading = ref(false)
const fetchError = ref('')

interface ReportRow {
  key: string
  label: string
  total: number
  success: number
  failed: number
  items: ObjectResult[]
  isTrigger: boolean
}

const tableRows = computed<ReportRow[]>(() => {
  if (!report.value) return []
  const r = report.value
  return [
    { key: 'tables', label: '表', ...r.tables, isTrigger: false },
    { key: 'data', label: '数据写入', ...r.data, isTrigger: false },
    { key: 'views', label: '视图', ...r.views, isTrigger: false },
    { key: 'indexes', label: '索引', ...r.indexes, isTrigger: false },
    { key: 'constraints', label: '外键', ...r.constraints, isTrigger: false },
    { key: 'sequences', label: '序列', ...r.sequences, isTrigger: false },
    {
      key: 'triggers',
      label: '触发器',
      total: r.triggers.total,
      success: 0,
      failed: 0,
      items: [],
      isTrigger: true,
    },
  ]
})

async function loadReport() {
  loading.value = true
  fetchError.value = ''
  try {
    const res = await getDataMigrationReport(props.jobID)
    report.value = res.data
  } catch {
    fetchError.value = '暂无报告数据'
  } finally {
    loading.value = false
  }
}

async function copyDDL(ddl: string) {
  try {
    await navigator.clipboard.writeText(ddl)
    Message.success('DDL 已复制')
  } catch {
    Message.error('复制失败')
  }
}

onMounted(loadReport)
</script>

<style scoped>
.migration-report-panel {
  margin-top: 16px;
}
.failure-list {
  padding: 8px 16px;
  background: #f7f8fa;
}
.failure-item {
  padding: 8px 0;
  border-bottom: 1px solid #e5e6eb;
}
.failure-item:last-child {
  border-bottom: none;
}
.failure-name {
  font-weight: 600;
  margin-bottom: 4px;
}
.failure-error {
  color: #f53f3f;
  margin-bottom: 4px;
  font-size: 13px;
}
.ddl-header {
  display: flex;
  justify-content: space-between;
  align-items: center;
  margin-bottom: 4px;
}
.ddl-code {
  background: #1d1d1d;
  color: #d4d4d4;
  padding: 8px 12px;
  border-radius: 4px;
  font-size: 12px;
  font-family: 'Menlo', 'Monaco', 'Courier New', monospace;
  overflow-x: auto;
  white-space: pre;
  margin: 0;
}
</style>
```

- [ ] **Step 2: 构建前端检查编译无误**

```bash
cd /Users/kay/Documents/GoProj/dbgold/frontend
npm run build 2>&1 | tail -15
```

Expected: 构建成功，无 TypeScript 错误

- [ ] **Step 3: 提交**

```bash
cd /Users/kay/Documents/GoProj/dbgold
git add frontend/src/views/MigrationReportPanel.vue
git commit -m "feat: add MigrationReportPanel component with expandable failure details"
```

---

## Task 9: 在 MigrationView.vue 中嵌入报告面板

**Files:**
- Modify: `frontend/src/views/MigrationView.vue`

- [ ] **Step 1: 在 MigrationView.vue script 中导入 MigrationReportPanel**

找到 script setup 的 import 块，追加：

```typescript
import MigrationReportPanel from './MigrationReportPanel.vue'
```

- [ ] **Step 2: 在日志区 div 之后追加报告面板**

找到模板中日志区的结束位置：

```html
          <div v-if="dataMigrate.logs.length > 0">
```

在该 div 的**闭合标签**（`</div>`）之后追加：

```html
          <div v-if="dataMigrate.finished && dataMigrate.currentJobId" style="margin-top: 16px">
            <a-divider>迁移报告</a-divider>
            <MigrationReportPanel :job-id="dataMigrate.currentJobId" />
          </div>
```

注意：需要找到日志区 div 的确切闭合位置。日志区 div 从 `v-if="dataMigrate.logs.length > 0"` 开始，结束于其对应的 `</div>`，报告面板紧接在该 div 之后。

- [ ] **Step 3: 构建前端确认**

```bash
cd /Users/kay/Documents/GoProj/dbgold/frontend
npm run build 2>&1 | tail -10
```

Expected: 构建成功

- [ ] **Step 4: 提交**

```bash
cd /Users/kay/Documents/GoProj/dbgold
git add frontend/src/views/MigrationView.vue
git commit -m "feat: embed MigrationReportPanel below log area in MigrationView"
```

---

## Task 10: 在 HistoryView.vue 新增数据迁移历史 tab 和「查看报告」功能

**Files:**
- Modify: `frontend/src/views/HistoryView.vue`

- [ ] **Step 1: 将 HistoryView.vue 全部内容替换**

```vue
<!-- frontend/src/views/HistoryView.vue -->
<template>
  <div>
    <a-tabs default-active-key="ddl">
      <!-- ===== DDL 迁移历史 ===== -->
      <a-tab-pane key="ddl" title="DDL 迁移">
        <div style="display: flex; justify-content: flex-end; margin-bottom: 16px">
          <a-button @click="loadHistory" :loading="loading">
            <template #icon><icon-refresh /></template>
            刷新
          </a-button>
        </div>

        <a-table
          :data="history"
          :loading="loading"
          row-key="id"
          :pagination="{ pageSize: 20 }"
        >
          <template #columns>
            <a-table-column title="ID" data-index="id" :width="60" />
            <a-table-column title="类型" data-index="type" :width="90">
              <template #cell="{ record }">
                <a-tag :color="typeColor(record.type)">{{ record.type }}</a-tag>
              </template>
            </a-table-column>
            <a-table-column title="源" :width="180">
              <template #cell="{ record }">
                <span v-if="record.src_database">{{ record.src_database }}</span>
                <span v-else style="color: #c9cdd4">—</span>
              </template>
            </a-table-column>
            <a-table-column title="目标" :width="180">
              <template #cell="{ record }">
                <span>{{ record.dst_database }}</span>
              </template>
            </a-table-column>
            <a-table-column title="状态" data-index="status" :width="80">
              <template #cell="{ record }">
                <a-tag :color="record.status === 'success' ? 'green' : 'red'">{{ record.status }}</a-tag>
              </template>
            </a-table-column>
            <a-table-column title="时间" data-index="created_at" :width="170">
              <template #cell="{ record }">
                {{ formatDate(record.created_at) }}
              </template>
            </a-table-column>
            <a-table-column title="操作" :width="80">
              <template #cell="{ record }">
                <a-button size="small" @click="viewDDLDetail(record)">查看</a-button>
              </template>
            </a-table-column>
          </template>
        </a-table>

        <a-drawer
          v-model:visible="ddlDrawerVisible"
          title="迁移 SQL 详情"
          :width="600"
        >
          <sql-preview :sqls="detailSqls" />
        </a-drawer>
      </a-tab-pane>

      <!-- ===== 数据迁移历史 ===== -->
      <a-tab-pane key="data" title="数据迁移">
        <div style="display: flex; justify-content: flex-end; margin-bottom: 16px">
          <a-button @click="loadDataJobs" :loading="dataJobsLoading">
            <template #icon><icon-refresh /></template>
            刷新
          </a-button>
        </div>

        <a-table
          :data="dataJobs"
          :loading="dataJobsLoading"
          row-key="id"
          :pagination="{ pageSize: 20 }"
        >
          <template #columns>
            <a-table-column title="Job ID" :width="120">
              <template #cell="{ record }">
                <span style="font-family: monospace; font-size: 12px">
                  {{ record.job_id.slice(0, 8) }}...
                </span>
              </template>
            </a-table-column>
            <a-table-column title="源库类型" data-index="src_db_type" :width="90" />
            <a-table-column title="目标库类型" data-index="dst_db_type" :width="100" />
            <a-table-column title="迁移模式" :width="90">
              <template #cell="{ record }">
                <a-tag>{{ record.migrate_mode }}</a-tag>
              </template>
            </a-table-column>
            <a-table-column title="状态" :width="100">
              <template #cell="{ record }">
                <a-tag :color="dataJobStatusColor(record.status)">{{ record.status }}</a-tag>
              </template>
            </a-table-column>
            <a-table-column title="开始时间" :width="160">
              <template #cell="{ record }">
                {{ formatDate(record.created_at) }}
              </template>
            </a-table-column>
            <a-table-column title="结束时间" :width="160">
              <template #cell="{ record }">
                <span v-if="record.finished_at">{{ formatDate(record.finished_at) }}</span>
                <span v-else style="color: #86909c">—</span>
              </template>
            </a-table-column>
            <a-table-column title="操作" :width="100">
              <template #cell="{ record }">
                <a-button
                  v-if="record.status === 'done' || record.status === 'failed'"
                  size="small"
                  @click="viewReport(record)"
                >
                  查看报告
                </a-button>
                <span v-else style="color: #86909c">—</span>
              </template>
            </a-table-column>
          </template>
        </a-table>

        <a-drawer
          v-model:visible="reportDrawerVisible"
          title="迁移报告"
          :width="800"
        >
          <MigrationReportPanel v-if="reportJobId" :job-id="reportJobId" />
        </a-drawer>
      </a-tab-pane>
    </a-tabs>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { Message } from '@arco-design/web-vue'
import SqlPreview from '@/components/SqlPreview.vue'
import MigrationReportPanel from './MigrationReportPanel.vue'
import { listMigrations, listDataMigrationJobs, type MigrationHistory, type DataMigrationJob } from '@/api/migration'

const history = ref<MigrationHistory[]>([])
const loading = ref(false)
const ddlDrawerVisible = ref(false)
const detailSqls = ref<string[]>([])

const dataJobs = ref<DataMigrationJob[]>([])
const dataJobsLoading = ref(false)
const reportDrawerVisible = ref(false)
const reportJobId = ref('')

async function loadHistory() {
  loading.value = true
  try {
    const res = await listMigrations()
    history.value = res.data
  } catch {
    Message.error('加载失败')
  } finally {
    loading.value = false
  }
}

async function loadDataJobs() {
  dataJobsLoading.value = true
  try {
    const res = await listDataMigrationJobs()
    dataJobs.value = res.data
  } catch {
    Message.error('加载失败')
  } finally {
    dataJobsLoading.value = false
  }
}

function viewDDLDetail(record: MigrationHistory) {
  try {
    const parsed: unknown = JSON.parse(record.sql_statements)
    detailSqls.value = Array.isArray(parsed) ? (parsed as string[]) : []
  } catch {
    detailSqls.value = []
  }
  ddlDrawerVisible.value = true
}

function viewReport(record: DataMigrationJob) {
  reportJobId.value = record.job_id
  reportDrawerVisible.value = true
}

function typeColor(type: string) {
  return { diff: 'blue', full: 'purple', selective: 'orange' }[type] ?? 'gray'
}

function dataJobStatusColor(status: string) {
  return { done: 'green', failed: 'red', running: 'blue', cancelled: 'gray' }[status] ?? 'gray'
}

function formatDate(dateStr: string) {
  return new Date(dateStr).toLocaleString('zh-CN')
}

onMounted(() => {
  loadHistory()
  loadDataJobs()
})
</script>
```

- [ ] **Step 2: 构建前端确认无错误**

```bash
cd /Users/kay/Documents/GoProj/dbgold/frontend
npm run build 2>&1 | tail -15
```

Expected: 构建成功，bundle 大小正常

- [ ] **Step 3: 运行全部 Go 测试确认无回归**

```bash
cd /Users/kay/Documents/GoProj/dbgold
/Users/kay/sdk/go1.25.5/bin/go test ./... 2>&1 | tail -20
```

Expected: 所有测试 PASS

- [ ] **Step 4: 提交**

```bash
git add frontend/src/views/HistoryView.vue
git commit -m "feat: add data migration history tab with report viewer to HistoryView"
```

---

## 完成验收

运行完所有任务后，验证：

```bash
# 全部 Go 测试通过
/Users/kay/sdk/go1.25.5/bin/go test ./... -v 2>&1 | grep -E "^(ok|FAIL|---)"

# 前端构建成功
cd frontend && npm run build 2>&1 | tail -5
```

预期输出：
- 所有 Go package 显示 `ok`
- 前端 `dist/index.html` 生成成功，无 TypeScript 错误
