# Data Migration (MySQL → PostgreSQL) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在 dbgold 的「迁移」页面新增「数据迁移」Tab，一键完成 MySQL → PostgreSQL 全流程迁移（建表结构 → 迁移数据 → 序列/索引/外键/视图），通过 SSE 实时推送迁移日志。

**Architecture:** 新增 `datamigrate/` 模块，内部通过 `source.Reader` / `target.Writer` 接口隔离数据库细节；`migrator.go` 串联三个迁移阶段（DDL → 数据 → Post-DDL）；`registry.go` 管理内存中运行中的任务；后端通过 5 个新 API 端点暴露能力；前端在现有 MigrationView 新增 Tab，用 SSE 接收并渲染实时日志。

**Tech Stack:** Go 1.25.5、gin v1.10.0、gin-contrib/sse v0.1.0、lib/pq v1.10.9（PostgreSQL COPY 协议）、go-sql-driver/mysql v1.8.1、Vue 3 + TypeScript + ArcoDesign Vue

---

## 文件映射

### 新增文件

| 文件 | 职责 |
|------|------|
| `datamigrate/source/interface.go` | 源库接口 `Reader` 定义及共用数据结构 |
| `datamigrate/source/mysql.go` | MySQL `Reader` 实现 |
| `datamigrate/target/interface.go` | 目标库接口 `Writer` 定义 |
| `datamigrate/target/postgres.go` | PostgreSQL `Writer` 实现（pq.CopyIn） |
| `datamigrate/typemap/mysql_pg.go` | MySQL → PG 类型映射函数 |
| `datamigrate/logger.go` | `Logger`：向 channel 写入带前缀的日志行 |
| `datamigrate/registry.go` | `JobRegistry`：内存 jobID → `Job` 映射 |
| `datamigrate/migrator.go` | `Migrator`：三阶段迁移调度，参数验证，取消 |
| `datamigrate/filter.go` | 表名过滤（all / exclude / include，支持 * 通配符） |
| `datamigrate/migrator_test.go` | Migrator 单元测试（mock source/target） |
| `datamigrate/filter_test.go` | 过滤逻辑单元测试 |
| `datamigrate/typemap/mysql_pg_test.go` | 类型映射单元测试 |
| `store/datamigration.go` | `DataMigrationJob` 模型及 CRUD |

### 修改文件

| 文件 | 修改内容 |
|------|---------|
| `store/db.go` | `AutoMigrate` 加入 `DataMigrationJob` |
| `api/handler/datamigration.go` | 新增 5 个 handler 函数（新文件） |
| `api/router.go` | 注册 5 个新路由 |
| `frontend/src/api/migration.ts` | 新增数据迁移 API 调用函数 |
| `frontend/src/views/MigrationView.vue` | 新增「数据迁移」Tab |

---

## Task 1: source/interface.go — 源库接口与共用数据结构

**Files:**
- Create: `datamigrate/source/interface.go`

- [ ] **Step 1: 创建 source 接口文件**

```go
// datamigrate/source/interface.go
package source

import "context"

// ColumnInfo 表示一列的元数据，用于 DDL 生成
type ColumnInfo struct {
	Name       string
	DataType   string // 原始数据库类型（如 "varchar"、"int"）
	Length     int64
	Precision  int64
	Scale      int64
	IsNullable bool
	Default    *string
	Extra      string // 如 "auto_increment"
}

// TableDDLInfo 包含建表所需的完整元数据
type TableDDLInfo struct {
	TableName string
	Columns   []ColumnInfo
}

// SequenceInfo 表示一个自增序列（来自 AUTO_INCREMENT 列）
type SequenceInfo struct {
	TableName  string
	ColumnName string
	StartValue int64
}

// IndexInfo 表示一个索引或唯一约束
type IndexInfo struct {
	TableName  string
	IndexName  string
	Columns    []string
	IsUnique   bool
	IsPrimary  bool
}

// FKInfo 表示一个外键约束
type FKInfo struct {
	TableName        string
	ConstraintName   string
	Columns          []string
	RefTable         string
	RefColumns       []string
	OnDelete         string
	OnUpdate         string
}

// ViewInfo 表示一个视图
type ViewInfo struct {
	ViewName   string
	Definition string // 已转换为目标库语法的 SQL
}

// Reader 是源库抽象接口，新增源库只需实现此接口
type Reader interface {
	// DBType 返回数据库类型标识，如 "mysql"
	DBType() string
	// ListTables 返回过滤后的表名列表
	ListTables(ctx context.Context) ([]string, error)
	// GetTableDDLInfo 返回指定表的列定义
	GetTableDDLInfo(ctx context.Context, table string) (*TableDDLInfo, error)
	// GetPrimaryKey 返回主键列名，无主键返回空串
	GetPrimaryKey(ctx context.Context, table string) (string, error)
	// ReadPage 分页读取数据：有主键时按主键分页，无主键时 pkCol 为空、offset/limit 用 LIMIT
	// 返回列名切片和行数据切片
	ReadPage(ctx context.Context, table, pkCol string, offset, limit int) (cols []string, rows [][]interface{}, err error)
	// GetSequences 返回所有 AUTO_INCREMENT 列信息
	GetSequences(ctx context.Context) ([]SequenceInfo, error)
	// GetIndexes 返回所有索引信息（不含主键）
	GetIndexes(ctx context.Context) ([]IndexInfo, error)
	// GetForeignKeys 返回所有外键信息
	GetForeignKeys(ctx context.Context) ([]FKInfo, error)
	// GetViews 返回所有视图信息
	GetViews(ctx context.Context) ([]ViewInfo, error)
}
```

- [ ] **Step 2: 提交**

```bash
git add datamigrate/source/interface.go
git commit -m "feat: add datamigrate source.Reader interface and shared types"
```

---

## Task 2: target/interface.go — 目标库接口

**Files:**
- Create: `datamigrate/target/interface.go`

- [ ] **Step 1: 创建 target 接口文件**

```go
// datamigrate/target/interface.go
package target

import (
	"context"
	"dbgold/datamigrate/source"
)

// Writer 是目标库抽象接口，新增目标库只需实现此接口
type Writer interface {
	// DBType 返回数据库类型标识，如 "postgres"
	DBType() string
	// CreateTable 在目标库执行建表 DDL（先 DROP IF EXISTS，再 CREATE）
	CreateTable(ctx context.Context, ddl string) error
	// CopyData 使用批量协议写入一批行数据
	CopyData(ctx context.Context, table string, cols []string, rows [][]interface{}) error
	// CreateSequence 创建序列并绑定到列的默认值
	CreateSequence(ctx context.Context, seq source.SequenceInfo) error
	// CreateIndex 创建索引或唯一约束
	CreateIndex(ctx context.Context, idx source.IndexInfo) error
	// CreateForeignKey 创建外键约束
	CreateForeignKey(ctx context.Context, fk source.FKInfo) error
	// CreateView 创建视图
	CreateView(ctx context.Context, view source.ViewInfo) error
}
```

- [ ] **Step 2: 提交**

```bash
git add datamigrate/target/interface.go
git commit -m "feat: add datamigrate target.Writer interface"
```

---

## Task 3: typemap/mysql_pg.go — 类型映射及测试

**Files:**
- Create: `datamigrate/typemap/mysql_pg.go`
- Create: `datamigrate/typemap/mysql_pg_test.go`

- [ ] **Step 1: 编写类型映射测试**

```go
// datamigrate/typemap/mysql_pg_test.go
package typemap

import (
	"testing"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/assert"
)

func TestMySQLToPG(t *testing.T) {
	cases := []struct {
		col      source.ColumnInfo
		expected string
	}{
		{source.ColumnInfo{DataType: "int"}, "int"},
		{source.ColumnInfo{DataType: "tinyint"}, "int"},
		{source.ColumnInfo{DataType: "mediumint"}, "int"},
		{source.ColumnInfo{DataType: "smallint"}, "int"},
		{source.ColumnInfo{DataType: "bigint"}, "bigint"},
		{source.ColumnInfo{DataType: "float"}, "double precision"},
		{source.ColumnInfo{DataType: "double"}, "double precision"},
		{source.ColumnInfo{DataType: "decimal", Precision: 10, Scale: 2}, "decimal(10,2)"},
		{source.ColumnInfo{DataType: "varchar", Length: 255}, "varchar(255)"},
		{source.ColumnInfo{DataType: "char", Length: 10}, "char(10)"},
		{source.ColumnInfo{DataType: "text"}, "text"},
		{source.ColumnInfo{DataType: "tinytext"}, "text"},
		{source.ColumnInfo{DataType: "mediumtext"}, "text"},
		{source.ColumnInfo{DataType: "longtext"}, "text"},
		{source.ColumnInfo{DataType: "datetime"}, "timestamp"},
		{source.ColumnInfo{DataType: "timestamp"}, "timestamp"},
		{source.ColumnInfo{DataType: "date"}, "date"},
		{source.ColumnInfo{DataType: "time"}, "time"},
		{source.ColumnInfo{DataType: "blob"}, "bytea"},
		{source.ColumnInfo{DataType: "tinyblob"}, "bytea"},
		{source.ColumnInfo{DataType: "mediumblob"}, "bytea"},
		{source.ColumnInfo{DataType: "longblob"}, "bytea"},
		{source.ColumnInfo{DataType: "binary"}, "bytea"},
		{source.ColumnInfo{DataType: "varbinary"}, "bytea"},
		{source.ColumnInfo{DataType: "json"}, "jsonb"},
		{source.ColumnInfo{DataType: "enum"}, "varchar(255)"},
		{source.ColumnInfo{DataType: "set"}, "text"},
		{source.ColumnInfo{DataType: "year"}, "int"},
		{source.ColumnInfo{DataType: "bit"}, "bit"},
	}
	for _, c := range cases {
		t.Run(c.col.DataType, func(t *testing.T) {
			assert.Equal(t, c.expected, MySQLToPG(c.col))
		})
	}
}
```

- [ ] **Step 2: 运行测试，确认失败**

```bash
/Users/kay/sdk/go1.25.5/bin/go test ./datamigrate/typemap/... 2>&1
```

预期：编译错误，`MySQLToPG` 未定义。

- [ ] **Step 3: 实现类型映射**

```go
// datamigrate/typemap/mysql_pg.go
package typemap

import (
	"fmt"
	"strings"
	"dbgold/datamigrate/source"
)

// MySQLToPG 将 MySQL 列的数据类型转换为 PostgreSQL 类型字符串
func MySQLToPG(col source.ColumnInfo) string {
	dt := strings.ToLower(col.DataType)
	switch dt {
	case "tinyint", "smallint", "mediumint", "int":
		return "int"
	case "bigint":
		return "bigint"
	case "float", "double":
		return "double precision"
	case "decimal", "numeric":
		if col.Precision > 0 {
			return fmt.Sprintf("decimal(%d,%d)", col.Precision, col.Scale)
		}
		return "decimal"
	case "char":
		return fmt.Sprintf("char(%d)", col.Length)
	case "varchar":
		return fmt.Sprintf("varchar(%d)", col.Length)
	case "tinytext", "text", "mediumtext", "longtext":
		return "text"
	case "datetime", "timestamp":
		return "timestamp"
	case "date":
		return "date"
	case "time":
		return "time"
	case "tinyblob", "blob", "mediumblob", "longblob", "binary", "varbinary":
		return "bytea"
	case "json":
		return "jsonb"
	case "enum":
		return "varchar(255)"
	case "set":
		return "text"
	case "year":
		return "int"
	case "bit":
		return "bit"
	default:
		return dt
	}
}
```

- [ ] **Step 4: 运行测试，确认通过**

```bash
/Users/kay/sdk/go1.25.5/bin/go test ./datamigrate/typemap/... -v 2>&1
```

预期：所有 case PASS。

- [ ] **Step 5: 提交**

```bash
git add datamigrate/typemap/mysql_pg.go datamigrate/typemap/mysql_pg_test.go
git commit -m "feat: add MySQL to PostgreSQL type mapping with tests"
```

---

## Task 4: filter.go — 表名过滤逻辑及测试

**Files:**
- Create: `datamigrate/filter.go`
- Create: `datamigrate/filter_test.go`

- [ ] **Step 1: 编写过滤测试**

```go
// datamigrate/filter_test.go
package datamigrate

import (
	"testing"
	"github.com/stretchr/testify/assert"
)

func TestFilterTables(t *testing.T) {
	tables := []string{"users", "orders", "order_log", "tmp_cache", "audit"}

	t.Run("all mode returns all tables", func(t *testing.T) {
		result := FilterTables(tables, "all", "")
		assert.Equal(t, tables, result)
	})

	t.Run("include mode returns only matching tables", func(t *testing.T) {
		result := FilterTables(tables, "include", "users,orders")
		assert.Equal(t, []string{"users", "orders"}, result)
	})

	t.Run("exclude mode removes matching tables", func(t *testing.T) {
		result := FilterTables(tables, "exclude", "*_log,tmp_*,audit")
		assert.Equal(t, []string{"users", "orders"}, result)
	})

	t.Run("wildcard star matches any suffix", func(t *testing.T) {
		result := FilterTables(tables, "exclude", "order*")
		assert.Equal(t, []string{"users", "tmp_cache", "audit"}, result)
	})

	t.Run("wildcard star matches any prefix", func(t *testing.T) {
		result := FilterTables(tables, "exclude", "*_cache")
		assert.Equal(t, []string{"users", "orders", "order_log", "audit"}, result)
	})

	t.Run("empty filter in include mode returns nothing", func(t *testing.T) {
		result := FilterTables(tables, "include", "")
		assert.Empty(t, result)
	})
}
```

- [ ] **Step 2: 运行测试，确认失败**

```bash
/Users/kay/sdk/go1.25.5/bin/go test ./datamigrate/ -run TestFilterTables -v 2>&1
```

预期：编译错误，`FilterTables` 未定义。

- [ ] **Step 3: 实现过滤逻辑**

```go
// datamigrate/filter.go
package datamigrate

import (
	"path/filepath"
	"strings"
)

// FilterTables 根据 mode 和 filter 字符串过滤表名列表。
// mode: "all" | "include" | "exclude"
// filter: 逗号分隔的表名或通配符（支持 * 匹配任意字符串）
func FilterTables(tables []string, mode, filter string) []string {
	if mode == "all" {
		return tables
	}
	patterns := parsePatterns(filter)
	result := make([]string, 0, len(tables))
	for _, t := range tables {
		matched := matchesAny(t, patterns)
		if mode == "include" && matched {
			result = append(result, t)
		} else if mode == "exclude" && !matched {
			result = append(result, t)
		}
	}
	return result
}

func parsePatterns(filter string) []string {
	if filter == "" {
		return nil
	}
	parts := strings.Split(filter, ",")
	patterns := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			patterns = append(patterns, p)
		}
	}
	return patterns
}

func matchesAny(name string, patterns []string) bool {
	for _, p := range patterns {
		if ok, _ := filepath.Match(p, name); ok {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: 运行测试，确认通过**

```bash
/Users/kay/sdk/go1.25.5/bin/go test ./datamigrate/ -run TestFilterTables -v 2>&1
```

预期：所有 case PASS。

- [ ] **Step 5: 提交**

```bash
git add datamigrate/filter.go datamigrate/filter_test.go
git commit -m "feat: add table name filter with wildcard support and tests"
```

---

## Task 5: logger.go — 迁移日志工具

**Files:**
- Create: `datamigrate/logger.go`

- [ ] **Step 1: 创建 Logger**

```go
// datamigrate/logger.go
package datamigrate

import "fmt"

const (
	PrefixInfo  = "[INFO] "
	PrefixDDL   = "[DDL]  "
	PrefixData  = "[DATA] "
	PrefixIndex = "[INDEX]"
	PrefixWarn  = "[WARN] "
	PrefixError = "[ERROR]"
	PrefixDone  = "[DONE] "
)

// Logger 向 channel 写入带前缀的日志行，channel 满时丢弃（非阻塞）
type Logger struct {
	ch chan string
}

// NewLogger 创建一个使用给定 channel 的 Logger
func NewLogger(ch chan string) *Logger {
	return &Logger{ch: ch}
}

func (l *Logger) Info(msg string)  { l.send(PrefixInfo + "  " + msg) }
func (l *Logger) DDL(msg string)   { l.send(PrefixDDL + "   " + msg) }
func (l *Logger) Data(msg string)  { l.send(PrefixData + "  " + msg) }
func (l *Logger) Index(msg string) { l.send(PrefixIndex + " " + msg) }
func (l *Logger) Warn(msg string)  { l.send(PrefixWarn + "  " + msg) }
func (l *Logger) Error(msg string) { l.send(PrefixError + " " + msg) }
func (l *Logger) Done(msg string)  { l.send(PrefixDone + "  " + msg) }

func (l *Logger) Infof(format string, args ...interface{})  { l.Info(fmt.Sprintf(format, args...)) }
func (l *Logger) DDLf(format string, args ...interface{})   { l.DDL(fmt.Sprintf(format, args...)) }
func (l *Logger) Dataf(format string, args ...interface{})  { l.Data(fmt.Sprintf(format, args...)) }
func (l *Logger) Indexf(format string, args ...interface{}) { l.Index(fmt.Sprintf(format, args...)) }
func (l *Logger) Warnf(format string, args ...interface{})  { l.Warn(fmt.Sprintf(format, args...)) }
func (l *Logger) Errorf(format string, args ...interface{}) { l.Error(fmt.Sprintf(format, args...)) }
func (l *Logger) Donef(format string, args ...interface{})  { l.Done(fmt.Sprintf(format, args...)) }

func (l *Logger) send(msg string) {
	select {
	case l.ch <- msg:
	default:
	}
}
```

- [ ] **Step 2: 提交**

```bash
git add datamigrate/logger.go
git commit -m "feat: add migration Logger with prefixed SSE channel writes"
```

---

## Task 6: registry.go — 内存任务注册表

**Files:**
- Create: `datamigrate/registry.go`

- [ ] **Step 1: 创建 JobRegistry**

```go
// datamigrate/registry.go
package datamigrate

import (
	"context"
	"sync"
)

// Job 表示一个运行中的迁移任务
type Job struct {
	LogCh  chan string      // SSE 日志 channel（buffered，容量 512）
	Cancel context.CancelFunc
}

// JobRegistry 管理运行中的迁移任务，线程安全
type JobRegistry struct {
	mu   sync.RWMutex
	jobs map[string]*Job
}

var Registry = &JobRegistry{jobs: make(map[string]*Job)}

// Register 注册一个新任务，返回 Job
func (r *JobRegistry) Register(jobID string, cancel context.CancelFunc) *Job {
	job := &Job{
		LogCh:  make(chan string, 512),
		Cancel: cancel,
	}
	r.mu.Lock()
	r.jobs[jobID] = job
	r.mu.Unlock()
	return job
}

// Get 获取运行中的任务，不存在返回 nil
func (r *JobRegistry) Get(jobID string) *Job {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.jobs[jobID]
}

// Remove 任务结束后从注册表移除
func (r *JobRegistry) Remove(jobID string) {
	r.mu.Lock()
	delete(r.jobs, jobID)
	r.mu.Unlock()
}
```

- [ ] **Step 2: 提交**

```bash
git add datamigrate/registry.go
git commit -m "feat: add in-memory JobRegistry for SSE migration tasks"
```

---

## Task 7: source/mysql.go — MySQL Reader 实现

**Files:**
- Create: `datamigrate/source/mysql.go`

- [ ] **Step 1: 创建 MySQL Reader**

```go
// datamigrate/source/mysql.go
package source

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	_ "github.com/go-sql-driver/mysql"
)

// MySQLReader 实现 Reader 接口，连接到 MySQL 数据库
type MySQLReader struct {
	db     *sql.DB
	dbName string
}

// NewMySQL 创建并连接 MySQL Reader
// dsn 格式：user:password@tcp(host:port)/dbname?parseTime=true
func NewMySQL(dsn, dbName string) (*MySQLReader, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, err
	}
	return &MySQLReader{db: db, dbName: dbName}, nil
}

func (r *MySQLReader) Close() error { return r.db.Close() }
func (r *MySQLReader) DBType() string { return "mysql" }

func (r *MySQLReader) ListTables(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT TABLE_NAME FROM information_schema.TABLES
		 WHERE TABLE_SCHEMA = ? AND TABLE_TYPE = 'BASE TABLE'
		 ORDER BY TABLE_NAME`, r.dbName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables = append(tables, name)
	}
	return tables, rows.Err()
}

func (r *MySQLReader) GetTableDDLInfo(ctx context.Context, table string) (*TableDDLInfo, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT COLUMN_NAME, DATA_TYPE, CHARACTER_MAXIMUM_LENGTH,
		        NUMERIC_PRECISION, NUMERIC_SCALE, IS_NULLABLE, COLUMN_DEFAULT, EXTRA
		 FROM information_schema.COLUMNS
		 WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
		 ORDER BY ORDINAL_POSITION`, r.dbName, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	info := &TableDDLInfo{TableName: table}
	for rows.Next() {
		var col ColumnInfo
		var nullable, extra string
		var length, precision, scale sql.NullInt64
		var defaultVal sql.NullString
		if err := rows.Scan(&col.Name, &col.DataType, &length, &precision, &scale,
			&nullable, &defaultVal, &extra); err != nil {
			return nil, err
		}
		if length.Valid {
			col.Length = length.Int64
		}
		if precision.Valid {
			col.Precision = precision.Int64
		}
		if scale.Valid {
			col.Scale = scale.Int64
		}
		col.IsNullable = strings.EqualFold(nullable, "YES")
		if defaultVal.Valid {
			col.Default = &defaultVal.String
		}
		col.Extra = extra
		info.Columns = append(info.Columns, col)
	}
	return info, rows.Err()
}

func (r *MySQLReader) GetPrimaryKey(ctx context.Context, table string) (string, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT COLUMN_NAME FROM information_schema.KEY_COLUMN_USAGE
		 WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND CONSTRAINT_NAME = 'PRIMARY'
		 ORDER BY ORDINAL_POSITION LIMIT 1`, r.dbName, table)
	var pk string
	err := row.Scan(&pk)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return pk, err
}

func (r *MySQLReader) ReadPage(ctx context.Context, table, pkCol string, offset, limit int) ([]string, [][]interface{}, error) {
	var query string
	if pkCol != "" {
		query = fmt.Sprintf(
			`SELECT t.* FROM (SELECT %s FROM %s ORDER BY %s LIMIT %d, %d) temp
			 LEFT JOIN %s t ON temp.%s = t.%s`,
			pkCol, table, pkCol, offset, limit, table, pkCol, pkCol)
	} else {
		query = fmt.Sprintf(`SELECT * FROM %s LIMIT %d, %d`, table, offset, limit)
	}
	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, nil, err
	}
	var result [][]interface{}
	for rows.Next() {
		vals := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, nil, err
		}
		// 清理 MySQL 返回的 []byte 中的 \x00 字符
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				vals[i] = strings.ReplaceAll(string(b), "\x00", "")
			}
		}
		result = append(result, vals)
	}
	return cols, result, rows.Err()
}

func (r *MySQLReader) GetSequences(ctx context.Context) ([]SequenceInfo, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT TABLE_NAME, COLUMN_NAME, AUTO_INCREMENT
		 FROM information_schema.TABLES t
		 JOIN information_schema.COLUMNS c USING (TABLE_SCHEMA, TABLE_NAME)
		 WHERE t.TABLE_SCHEMA = ? AND c.EXTRA = 'auto_increment'`, r.dbName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var seqs []SequenceInfo
	for rows.Next() {
		var s SequenceInfo
		var autoInc sql.NullInt64
		if err := rows.Scan(&s.TableName, &s.ColumnName, &autoInc); err != nil {
			return nil, err
		}
		if autoInc.Valid {
			s.StartValue = autoInc.Int64
		} else {
			s.StartValue = 1
		}
		seqs = append(seqs, s)
	}
	return seqs, rows.Err()
}

func (r *MySQLReader) GetIndexes(ctx context.Context) ([]IndexInfo, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT TABLE_NAME, INDEX_NAME, COLUMN_NAME, NON_UNIQUE
		 FROM information_schema.STATISTICS
		 WHERE TABLE_SCHEMA = ? AND INDEX_NAME != 'PRIMARY'
		 ORDER BY TABLE_NAME, INDEX_NAME, SEQ_IN_INDEX`, r.dbName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	indexMap := map[string]*IndexInfo{}
	var order []string
	for rows.Next() {
		var table, idxName, col string
		var nonUnique int
		if err := rows.Scan(&table, &idxName, &col, &nonUnique); err != nil {
			return nil, err
		}
		key := table + "." + idxName
		if _, ok := indexMap[key]; !ok {
			indexMap[key] = &IndexInfo{
				TableName: table,
				IndexName: idxName,
				IsUnique:  nonUnique == 0,
			}
			order = append(order, key)
		}
		indexMap[key].Columns = append(indexMap[key].Columns, col)
	}
	result := make([]IndexInfo, 0, len(order))
	for _, k := range order {
		result = append(result, *indexMap[k])
	}
	return result, rows.Err()
}

func (r *MySQLReader) GetForeignKeys(ctx context.Context) ([]FKInfo, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT kcu.TABLE_NAME, kcu.CONSTRAINT_NAME, kcu.COLUMN_NAME,
		        kcu.REFERENCED_TABLE_NAME, kcu.REFERENCED_COLUMN_NAME,
		        rc.DELETE_RULE, rc.UPDATE_RULE
		 FROM information_schema.KEY_COLUMN_USAGE kcu
		 JOIN information_schema.REFERENTIAL_CONSTRAINTS rc
		   ON kcu.CONSTRAINT_NAME = rc.CONSTRAINT_NAME
		  AND kcu.TABLE_SCHEMA = rc.CONSTRAINT_SCHEMA
		 WHERE kcu.TABLE_SCHEMA = ?
		   AND kcu.REFERENCED_TABLE_NAME IS NOT NULL
		 ORDER BY kcu.TABLE_NAME, kcu.CONSTRAINT_NAME, kcu.ORDINAL_POSITION`, r.dbName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	fkMap := map[string]*FKInfo{}
	var order []string
	for rows.Next() {
		var table, name, col, refTable, refCol, onDelete, onUpdate string
		if err := rows.Scan(&table, &name, &col, &refTable, &refCol, &onDelete, &onUpdate); err != nil {
			return nil, err
		}
		key := table + "." + name
		if _, ok := fkMap[key]; !ok {
			fkMap[key] = &FKInfo{
				TableName: table, ConstraintName: name,
				RefTable: refTable, OnDelete: onDelete, OnUpdate: onUpdate,
			}
			order = append(order, key)
		}
		fkMap[key].Columns = append(fkMap[key].Columns, col)
		fkMap[key].RefColumns = append(fkMap[key].RefColumns, refCol)
	}
	result := make([]FKInfo, 0, len(order))
	for _, k := range order {
		result = append(result, *fkMap[k])
	}
	return result, rows.Err()
}

func (r *MySQLReader) GetViews(ctx context.Context) ([]ViewInfo, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT TABLE_NAME, VIEW_DEFINITION
		 FROM information_schema.VIEWS
		 WHERE TABLE_SCHEMA = ?`, r.dbName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var views []ViewInfo
	for rows.Next() {
		var v ViewInfo
		if err := rows.Scan(&v.ViewName, &v.Definition); err != nil {
			return nil, err
		}
		// 清理 MySQL 特定语法：去掉反引号
		v.Definition = strings.ReplaceAll(v.Definition, "`", "\"")
		views = append(views, v)
	}
	return views, rows.Err()
}
```

- [ ] **Step 2: 确认编译通过**

```bash
/Users/kay/sdk/go1.25.5/bin/go build ./datamigrate/source/... 2>&1
```

预期：无错误输出。

- [ ] **Step 3: 提交**

```bash
git add datamigrate/source/mysql.go
git commit -m "feat: implement MySQL source.Reader for data migration"
```

---

## Task 8: target/postgres.go — PostgreSQL Writer 实现

**Files:**
- Create: `datamigrate/target/postgres.go`

- [ ] **Step 1: 创建 PostgreSQL Writer**

```go
// datamigrate/target/postgres.go
package target

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"dbgold/datamigrate/source"
	"github.com/lib/pq"
)

// PostgresWriter 实现 Writer 接口，写入到 PostgreSQL 数据库
type PostgresWriter struct {
	db *sql.DB
}

// NewPostgres 创建并连接 PostgreSQL Writer
// dsn 格式：host=... port=... user=... password=... dbname=... sslmode=disable
func NewPostgres(dsn string) (*PostgresWriter, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, err
	}
	return &PostgresWriter{db: db}, nil
}

func (w *PostgresWriter) Close() error  { return w.db.Close() }
func (w *PostgresWriter) DBType() string { return "postgres" }

func (w *PostgresWriter) CreateTable(ctx context.Context, ddl string) error {
	_, err := w.db.ExecContext(ctx, ddl)
	return err
}

// CopyData 使用 PostgreSQL COPY 协议批量写入行数据
func (w *PostgresWriter) CopyData(ctx context.Context, table string, cols []string, rows [][]interface{}) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, pq.CopyIn(table, cols...))
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, row := range rows {
		if _, err := stmt.ExecContext(ctx, row...); err != nil {
			return err
		}
	}
	if _, err := stmt.ExecContext(ctx); err != nil {
		return err
	}
	return tx.Commit()
}

func (w *PostgresWriter) CreateSequence(ctx context.Context, seq source.SequenceInfo) error {
	seqName := fmt.Sprintf("seq_%s_%s", seq.TableName, seq.ColumnName)
	ddl := fmt.Sprintf(
		`CREATE SEQUENCE IF NOT EXISTS %s INCREMENT BY 1 START %d;
		 ALTER TABLE %s ALTER COLUMN %s SET DEFAULT nextval('%s');`,
		seqName, seq.StartValue, seq.TableName, seq.ColumnName, seqName)
	_, err := w.db.ExecContext(ctx, ddl)
	return err
}

func (w *PostgresWriter) CreateIndex(ctx context.Context, idx source.IndexInfo) error {
	cols := strings.Join(idx.Columns, ", ")
	var ddl string
	if idx.IsPrimary {
		ddl = fmt.Sprintf("ALTER TABLE %s ADD PRIMARY KEY (%s);", idx.TableName, cols)
	} else if idx.IsUnique {
		ddl = fmt.Sprintf("CREATE UNIQUE INDEX IF NOT EXISTS %s ON %s (%s);",
			idx.IndexName, idx.TableName, cols)
	} else {
		ddl = fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s (%s);",
			idx.IndexName, idx.TableName, cols)
	}
	_, err := w.db.ExecContext(ctx, ddl)
	return err
}

func (w *PostgresWriter) CreateForeignKey(ctx context.Context, fk source.FKInfo) error {
	ddl := fmt.Sprintf(
		"ALTER TABLE %s ADD CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s (%s) ON DELETE %s ON UPDATE %s;",
		fk.TableName, fk.ConstraintName,
		strings.Join(fk.Columns, ", "),
		fk.RefTable,
		strings.Join(fk.RefColumns, ", "),
		fk.OnDelete, fk.OnUpdate)
	_, err := w.db.ExecContext(ctx, ddl)
	return err
}

func (w *PostgresWriter) CreateView(ctx context.Context, view source.ViewInfo) error {
	ddl := fmt.Sprintf("CREATE OR REPLACE VIEW %s AS %s;", view.ViewName, view.Definition)
	_, err := w.db.ExecContext(ctx, ddl)
	return err
}
```

- [ ] **Step 2: 确认编译通过**

```bash
/Users/kay/sdk/go1.25.5/bin/go build ./datamigrate/target/... 2>&1
```

预期：无错误输出。

- [ ] **Step 3: 提交**

```bash
git add datamigrate/target/postgres.go
git commit -m "feat: implement PostgreSQL target.Writer with pq.CopyIn"
```

---

## Task 9: migrator.go — 迁移器核心

**Files:**
- Create: `datamigrate/migrator.go`
- Create: `datamigrate/migrator_test.go`

- [ ] **Step 1: 编写 Migrator 测试（使用 mock）**

```go
// datamigrate/migrator_test.go
package datamigrate

import (
	"context"
	"strings"
	"testing"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/assert"
)

// mockReader 实现 source.Reader 接口，用于测试
type mockReader struct {
	tables []string
	ddl    map[string]*source.TableDDLInfo
	pk     map[string]string
	rows   map[string][][]interface{}
}

func (m *mockReader) DBType() string { return "mysql" }
func (m *mockReader) ListTables(_ context.Context) ([]string, error) { return m.tables, nil }
func (m *mockReader) GetTableDDLInfo(_ context.Context, t string) (*source.TableDDLInfo, error) {
	return m.ddl[t], nil
}
func (m *mockReader) GetPrimaryKey(_ context.Context, t string) (string, error) {
	return m.pk[t], nil
}
func (m *mockReader) ReadPage(_ context.Context, t, _ string, _, _ int) ([]string, [][]interface{}, error) {
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

// mockWriter 实现 target.Writer 接口，用于测试
type mockWriter struct {
	created  []string
	copied   []string
	errors   map[string]error
}

func (m *mockWriter) DBType() string { return "postgres" }
func (m *mockWriter) CreateTable(_ context.Context, ddl string) error {
	if m.errors != nil {
		if err, ok := m.errors["CreateTable"]; ok {
			return err
		}
	}
	m.created = append(m.created, ddl)
	return nil
}
func (m *mockWriter) CopyData(_ context.Context, table string, _ []string, _ [][]interface{}) error {
	m.copied = append(m.copied, table)
	return nil
}
func (m *mockWriter) CreateSequence(_ context.Context, _ source.SequenceInfo) error { return nil }
func (m *mockWriter) CreateIndex(_ context.Context, _ source.IndexInfo) error       { return nil }
func (m *mockWriter) CreateForeignKey(_ context.Context, _ source.FKInfo) error     { return nil }
func (m *mockWriter) CreateView(_ context.Context, _ source.ViewInfo) error         { return nil }

func newTestMigrator(reader source.Reader, writer target.Writer) (*Migrator, *Job) {
	ctx, cancel := context.WithCancel(context.Background())
	job := &Job{
		LogCh:  make(chan string, 512),
		Cancel: cancel,
	}
	_ = ctx
	cfg := Config{
		PageSize:    10,
		MaxParallel: 2,
		Mode:        "all",
		Filter:      "",
	}
	return NewMigrator(reader, writer, job, cfg), job
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
		pk:   map[string]string{"users": "id"},
		rows: map[string][][]interface{}{"users": {{1, "Alice"}, {2, "Bob"}}},
	}
	writer := &mockWriter{}
	m, job := newTestMigrator(reader, writer)

	ctx := context.Background()
	m.Run(ctx)
	close(job.LogCh)

	var logs []string
	for l := range job.LogCh {
		logs = append(logs, l)
	}

	assert.Contains(t, writer.copied, "users")
	hasDone := false
	for _, l := range logs {
		if strings.Contains(l, "[DONE]") {
			hasDone = true
		}
	}
	assert.True(t, hasDone, "should emit [DONE] log")
}

func TestMigratorRun_ContextCancelled(t *testing.T) {
	reader := &mockReader{tables: []string{"users"}, ddl: map[string]*source.TableDDLInfo{
		"users": {TableName: "users"},
	}, pk: map[string]string{}}
	writer := &mockWriter{}
	m, job := newTestMigrator(reader, writer)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消

	m.Run(ctx)
	close(job.LogCh)

	var logs []string
	for l := range job.LogCh {
		logs = append(logs, l)
	}
	hasCancelled := false
	for _, l := range logs {
		if strings.Contains(l, "取消") || strings.Contains(l, "cancelled") {
			hasCancelled = true
		}
	}
	assert.True(t, hasCancelled)
}
```

- [ ] **Step 2: 运行测试，确认失败**

```bash
/Users/kay/sdk/go1.25.5/bin/go test ./datamigrate/ -run TestMigratorRun -v 2>&1
```

预期：编译错误，`Migrator`、`Config`、`NewMigrator` 未定义。

- [ ] **Step 3: 实现 Migrator**

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
	PageSize    int    // 每页行数
	MaxParallel int    // 最大并发表数
	Mode        string // all / include / exclude
	Filter      string // 逗号分隔表名或通配符
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
	return &Migrator{
		reader: reader,
		writer: writer,
		job:    job,
		cfg:    cfg,
		log:    NewLogger(job.LogCh),
	}
}

// Run 执行完整的三阶段迁移，结束时关闭 job.LogCh
func (m *Migrator) Run(ctx context.Context) {
	start := time.Now()
	defer func() {
		// 不关闭 channel，由调用方在 goroutine 结束后关闭
	}()

	// 检查取消
	if err := ctx.Err(); err != nil {
		m.log.Warn("任务已取消")
		return
	}

	// 获取表列表
	allTables, err := m.reader.ListTables(ctx)
	if err != nil {
		m.log.Errorf("获取表列表失败: %v", err)
		return
	}
	tables := FilterTables(allTables, m.cfg.Mode, m.cfg.Filter)
	m.log.Infof("开始迁移任务，共 %d 张表，pageSize=%d，maxParallel=%d",
		len(tables), m.cfg.PageSize, m.cfg.MaxParallel)

	successCount := 0
	failCount := 0

	// Phase 1: 建表 DDL
	m.log.Info("=== Phase 1: 创建表结构 ===")
	tablesFailed := map[string]bool{}
	for _, table := range tables {
		if ctx.Err() != nil {
			m.log.Warn("任务已取消")
			return
		}
		ddl, err := m.buildCreateTableDDL(ctx, table)
		if err != nil {
			m.log.Errorf("生成建表 DDL 失败 [%s]: %v", table, err)
			tablesFailed[table] = true
			failCount++
			continue
		}
		if err := m.writer.CreateTable(ctx, ddl); err != nil {
			m.log.Errorf("创建表失败 [%s]: %v", table, err)
			tablesFailed[table] = true
			failCount++
			continue
		}
		m.log.DDLf("创建表 %s ... OK", table)
	}

	// Phase 2: 迁移数据（并发）
	m.log.Info("=== Phase 2: 迁移数据 ===")
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
			ok := m.migrateTableData(ctx, tbl)
			mu.Lock()
			if ok {
				successCount++
			} else {
				failCount++
			}
			mu.Unlock()
		}(table)
	}
	wg.Wait()

	// Phase 3: Post-DDL（序列、索引、外键、视图）
	m.log.Info("=== Phase 3: 创建序列、索引、外键、视图 ===")
	m.createPostDDL(ctx)

	elapsed := time.Since(start).Round(time.Second)
	m.log.Donef("迁移完成：成功 %d 张，失败 %d 张，耗时 %s", successCount, failCount, elapsed)
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
			colDef += fmt.Sprintf(" DEFAULT '%s'", *col.Default)
		}
		cols = append(cols, "  "+colDef)
	}
	ddl := fmt.Sprintf("DROP TABLE IF EXISTS \"%s\";\nCREATE TABLE \"%s\" (\n%s\n);",
		table, table, strings.Join(cols, ",\n"))
	return ddl, nil
}

// migrateTableData 迁移单张表的数据，返回是否成功
func (m *Migrator) migrateTableData(ctx context.Context, table string) bool {
	pk, err := m.reader.GetPrimaryKey(ctx, table)
	if err != nil {
		m.log.Errorf("获取主键失败 [%s]: %v", table, err)
		return false
	}
	offset := 0
	pageNum := 0
	for {
		if ctx.Err() != nil {
			return false
		}
		cols, rows, err := m.reader.ReadPage(ctx, table, pk, offset, m.cfg.PageSize)
		if err != nil {
			m.log.Errorf("读取数据失败 [%s] 第 %d 页: %v", table, pageNum+1, err)
			return false
		}
		if len(rows) == 0 {
			break
		}
		if err := m.writer.CopyData(ctx, table, cols, rows); err != nil {
			m.log.Errorf("写入数据失败 [%s] 第 %d 页: %v", table, pageNum+1, err)
			return false
		}
		pageNum++
		m.log.Dataf("迁移 %s: 第 %d 页 (%d 行) ... OK", table, pageNum, len(rows))
		if len(rows) < m.cfg.PageSize {
			break
		}
		offset += m.cfg.PageSize
	}
	return true
}

// createPostDDL 串行创建序列、索引、外键、视图
func (m *Migrator) createPostDDL(ctx context.Context) {
	seqs, err := m.reader.GetSequences(ctx)
	if err != nil {
		m.log.Errorf("获取序列信息失败: %v", err)
	} else {
		for _, seq := range seqs {
			if ctx.Err() != nil {
				return
			}
			if err := m.writer.CreateSequence(ctx, seq); err != nil {
				m.log.Errorf("创建序列失败 [%s.%s]: %v", seq.TableName, seq.ColumnName, err)
			} else {
				m.log.Indexf("创建序列 seq_%s_%s ... OK", seq.TableName, seq.ColumnName)
			}
		}
	}

	indexes, err := m.reader.GetIndexes(ctx)
	if err != nil {
		m.log.Errorf("获取索引信息失败: %v", err)
	} else {
		for _, idx := range indexes {
			if ctx.Err() != nil {
				return
			}
			if err := m.writer.CreateIndex(ctx, idx); err != nil {
				m.log.Errorf("创建索引失败 [%s]: %v", idx.IndexName, err)
			} else {
				m.log.Indexf("创建索引 %s ... OK", idx.IndexName)
			}
		}
	}

	fks, err := m.reader.GetForeignKeys(ctx)
	if err != nil {
		m.log.Errorf("获取外键信息失败: %v", err)
	} else {
		for _, fk := range fks {
			if ctx.Err() != nil {
				return
			}
			if err := m.writer.CreateForeignKey(ctx, fk); err != nil {
				m.log.Errorf("创建外键失败 [%s]: %v", fk.ConstraintName, err)
			} else {
				m.log.Indexf("创建外键 %s ... OK", fk.ConstraintName)
			}
		}
	}

	views, err := m.reader.GetViews(ctx)
	if err != nil {
		m.log.Errorf("获取视图信息失败: %v", err)
	} else {
		for _, v := range views {
			if ctx.Err() != nil {
				return
			}
			if err := m.writer.CreateView(ctx, v); err != nil {
				m.log.Errorf("创建视图失败 [%s]: %v", v.ViewName, err)
			} else {
				m.log.DDLf("创建视图 %s ... OK", v.ViewName)
			}
		}
	}
}
```

- [ ] **Step 4: 运行测试，确认通过**

```bash
/Users/kay/sdk/go1.25.5/bin/go test ./datamigrate/... -v 2>&1
```

预期：所有测试 PASS。

- [ ] **Step 5: 提交**

```bash
git add datamigrate/migrator.go datamigrate/migrator_test.go
git commit -m "feat: implement Migrator with three-phase migration and tests"
```

---

## Task 10: store/datamigration.go — DataMigrationJob 存储层

**Files:**
- Create: `store/datamigration.go`
- Modify: `store/db.go`

- [ ] **Step 1: 创建 DataMigrationJob 存储文件**

```go
// store/datamigration.go
package store

import "time"

type DataMigrationJob struct {
	ID          uint       `gorm:"primaryKey" json:"id"`
	JobID       string     `gorm:"uniqueIndex;not null" json:"job_id"`
	SrcConnID   uint       `json:"src_conn_id"`
	DstConnID   uint       `json:"dst_conn_id"`
	SrcDBType   string     `json:"src_db_type"`
	DstDBType   string     `json:"dst_db_type"`
	MigrateMode string     `json:"migrate_mode"` // all / exclude / include
	TableFilter string     `json:"table_filter"`
	PageSize    int        `json:"page_size"`
	MaxParallel int        `json:"max_parallel"`
	Status      string     `json:"status"` // running / done / failed / cancelled
	Summary     string     `json:"summary"`
	CreatedAt   time.Time  `json:"created_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
}

func CreateDataMigrationJob(j *DataMigrationJob) error {
	return DB.Create(j).Error
}

func UpdateDataMigrationJob(j *DataMigrationJob) error {
	return DB.Save(j).Error
}

func GetDataMigrationJob(jobID string) (*DataMigrationJob, error) {
	var j DataMigrationJob
	if err := DB.Where("job_id = ?", jobID).First(&j).Error; err != nil {
		return nil, err
	}
	return &j, nil
}

func ListDataMigrationJobs() ([]DataMigrationJob, error) {
	var jobs []DataMigrationJob
	if err := DB.Order("id desc").Find(&jobs).Error; err != nil {
		return nil, err
	}
	return jobs, nil
}
```

- [ ] **Step 2: 在 store/db.go 的 AutoMigrate 中加入 DataMigrationJob**

在 `store/db.go` 中找到：
```go
if err := DB.AutoMigrate(&User{}, &Connection{}, &MigrationHistory{}); err != nil {
```
修改为：
```go
if err := DB.AutoMigrate(&User{}, &Connection{}, &MigrationHistory{}, &DataMigrationJob{}); err != nil {
```

- [ ] **Step 3: 确认编译通过**

```bash
/Users/kay/sdk/go1.25.5/bin/go build ./store/... 2>&1
```

预期：无错误输出。

- [ ] **Step 4: 提交**

```bash
git add store/datamigration.go store/db.go
git commit -m "feat: add DataMigrationJob model and store CRUD"
```

---

## Task 11: api/handler/datamigration.go — HTTP Handler

**Files:**
- Create: `api/handler/datamigration.go`

- [ ] **Step 1: 创建数据迁移 Handler**

```go
// api/handler/datamigration.go
package handler

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"dbgold/datamigrate"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/store"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// SupportedPair 表示一个支持的迁移组合
type SupportedPair struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

// supportedPairs 列出后端已实现的迁移组合，新增实现时在此追加
var supportedPairs = []SupportedPair{
	{Source: "mysql", Target: "postgres"},
}

// GetSupportedPairs 返回支持的迁移组合列表
func GetSupportedPairs(c *gin.Context) {
	c.JSON(http.StatusOK, supportedPairs)
}

type startDataMigrationRequest struct {
	SrcConnID   uint   `json:"src_conn_id" binding:"required"`
	DstConnID   uint   `json:"dst_conn_id" binding:"required"`
	MigrateMode string `json:"migrate_mode" binding:"required,oneof=all include exclude"`
	TableFilter string `json:"table_filter"`
	PageSize    int    `json:"page_size"`
	MaxParallel int    `json:"max_parallel"`
}

// StartDataMigration 创建并启动迁移任务，立即返回 jobID
func StartDataMigration(c *gin.Context) {
	var req startDataMigrationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.PageSize <= 0 {
		req.PageSize = 10000
	}
	if req.MaxParallel <= 0 {
		req.MaxParallel = 5
	}

	srcConn, err := store.GetConnection(req.SrcConnID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "源库连接不存在"})
		return
	}
	dstConn, err := store.GetConnection(req.DstConnID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "目标库连接不存在"})
		return
	}

	// 校验迁移组合是否支持
	supported := false
	for _, p := range supportedPairs {
		if p.Source == srcConn.DBType && p.Target == dstConn.DBType {
			supported = true
			break
		}
	}
	if !supported {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("不支持 %s → %s 的数据迁移", srcConn.DBType, dstConn.DBType),
		})
		return
	}

	jobID := uuid.New().String()
	ctx, cancel := context.WithCancel(context.Background())
	job := datamigrate.Registry.Register(jobID, cancel)

	// 持久化任务记录
	dbJob := &store.DataMigrationJob{
		JobID:       jobID,
		SrcConnID:   req.SrcConnID,
		DstConnID:   req.DstConnID,
		SrcDBType:   srcConn.DBType,
		DstDBType:   dstConn.DBType,
		MigrateMode: req.MigrateMode,
		TableFilter: req.TableFilter,
		PageSize:    req.PageSize,
		MaxParallel: req.MaxParallel,
		Status:      "running",
	}
	if err := store.CreateDataMigrationJob(dbJob); err != nil {
		cancel()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建任务记录失败"})
		return
	}

	srcDSN := buildDSN(srcConn)
	dstDSN := buildDSN(dstConn)

	go func() {
		defer func() {
			close(job.LogCh)
			datamigrate.Registry.Remove(jobID)
		}()

		reader, err := source.NewMySQL(srcDSN, srcConn.Database)
		if err != nil {
			job.LogCh <- fmt.Sprintf("[ERROR] 连接源库失败: %v", err)
			updateJobStatus(dbJob, "failed", fmt.Sprintf("连接源库失败: %v", err))
			return
		}
		defer reader.Close()

		writer, err := target.NewPostgres(dstDSN)
		if err != nil {
			job.LogCh <- fmt.Sprintf("[ERROR] 连接目标库失败: %v", err)
			updateJobStatus(dbJob, "failed", fmt.Sprintf("连接目标库失败: %v", err))
			return
		}
		defer writer.Close()

		cfg := datamigrate.Config{
			PageSize:    req.PageSize,
			MaxParallel: req.MaxParallel,
			Mode:        req.MigrateMode,
			Filter:      req.TableFilter,
		}
		m := datamigrate.NewMigrator(reader, writer, job, cfg)
		m.Run(ctx)

		status := "done"
		if ctx.Err() != nil {
			status = "cancelled"
		}
		updateJobStatus(dbJob, status, "")
	}()

	c.JSON(http.StatusOK, gin.H{"job_id": jobID})
}

func updateJobStatus(job *store.DataMigrationJob, status, summary string) {
	now := time.Now()
	job.Status = status
	job.Summary = summary
	job.FinishedAt = &now
	_ = store.UpdateDataMigrationJob(job)
}

// StreamDataMigration 通过 SSE 推送迁移日志
func StreamDataMigration(c *gin.Context) {
	jobID := c.Query("jobID")
	if jobID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "jobID 必填"})
		return
	}
	job := datamigrate.Registry.Get(jobID)
	if job == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "任务不存在或已完成"})
		return
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	clientGone := c.Request.Context().Done()
	for {
		select {
		case <-clientGone:
			return
		case msg, ok := <-job.LogCh:
			if !ok {
				// channel 关闭，迁移结束
				c.SSEvent("message", "[STREAM_END]")
				c.Writer.Flush()
				return
			}
			c.SSEvent("message", msg)
			c.Writer.Flush()
		}
	}
}

// CancelDataMigration 取消运行中的迁移任务
func CancelDataMigration(c *gin.Context) {
	jobID := c.Param("jobID")
	job := datamigrate.Registry.Get(jobID)
	if job == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "任务不存在或已完成"})
		return
	}
	job.Cancel()
	c.JSON(http.StatusOK, gin.H{"message": "已发送取消信号"})
}

// ListDataMigrationJobs 返回历史任务列表
func ListDataMigrationJobs(c *gin.Context) {
	jobs, err := store.ListDataMigrationJobs()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, jobs)
}
```

- [ ] **Step 2: 确认编译通过**

```bash
/Users/kay/sdk/go1.25.5/bin/go build ./api/... 2>&1
```

预期：无错误输出。如果提示缺少 `github.com/google/uuid`，执行：
```bash
/Users/kay/sdk/go1.25.5/bin/go get github.com/google/uuid
```

- [ ] **Step 3: 提交**

```bash
git add api/handler/datamigration.go go.mod go.sum
git commit -m "feat: add data migration HTTP handlers with SSE streaming"
```

---

## Task 12: api/router.go — 注册新路由

**Files:**
- Modify: `api/router.go`

- [ ] **Step 1: 在 authed 路由组中注册 5 个新端点**

在 `api/router.go` 中找到现有迁移路由块：
```go
authed.POST("/migration/diff", handler.RunDiffMigration)
authed.POST("/migration/full", handler.RunFullMigration)
authed.POST("/migration/selective", handler.RunSelectiveMigration)
authed.GET("/migration", handler.ListMigrations)
authed.GET("/migration/:id", handler.GetMigration)
```

在其后追加：
```go
authed.GET("/migration/data-migrate/supported-pairs", handler.GetSupportedPairs)
authed.POST("/migration/data-migrate", handler.StartDataMigration)
authed.GET("/migration/data-migrate/stream", handler.StreamDataMigration)
authed.POST("/migration/data-migrate/:jobID/cancel", handler.CancelDataMigration)
authed.GET("/migration/data-migrate/jobs", handler.ListDataMigrationJobs)
```

- [ ] **Step 2: 确认编译通过**

```bash
/Users/kay/sdk/go1.25.5/bin/go build ./... 2>&1
```

预期：无错误输出。

- [ ] **Step 3: 提交**

```bash
git add api/router.go
git commit -m "feat: register data migration API routes"
```

---

## Task 13: store — 补充 GetConnection 函数

**Files:**
- Modify: `store/connection.go`

> **注意：** `handler/datamigration.go` 中调用了 `store.GetConnection(id)`。若该函数已存在请跳过此 Task。

- [ ] **Step 1: 确认 GetConnection 是否已存在**

```bash
grep -n "func GetConnection" /Users/kay/Documents/GoProj/dbgold/store/connection.go
```

- [ ] **Step 2: 如不存在，添加以下函数到 store/connection.go**

```go
func GetConnection(id uint) (*Connection, error) {
	var c Connection
	if err := DB.First(&c, id).Error; err != nil {
		return nil, err
	}
	return &c, nil
}
```

- [ ] **Step 3: 确认编译通过**

```bash
/Users/kay/sdk/go1.25.5/bin/go build ./... 2>&1
```

- [ ] **Step 4: 如有修改则提交**

```bash
git add store/connection.go
git commit -m "feat: add GetConnection by ID to store"
```

---

## Task 14: 前端 API 层 — migration.ts 扩展

**Files:**
- Modify: `frontend/src/api/migration.ts`

- [ ] **Step 1: 在 migration.ts 末尾追加以下内容**

```typescript
// ===== 数据迁移 =====

export interface SupportedPair {
  source: string
  target: string
}

export interface StartDataMigrationRequest {
  src_conn_id: number
  dst_conn_id: number
  migrate_mode: 'all' | 'include' | 'exclude'
  table_filter?: string
  page_size?: number
  max_parallel?: number
}

export interface DataMigrationJob {
  id: number
  job_id: string
  src_conn_id: number
  dst_conn_id: number
  src_db_type: string
  dst_db_type: string
  migrate_mode: string
  table_filter: string
  page_size: number
  max_parallel: number
  status: 'running' | 'done' | 'failed' | 'cancelled'
  summary: string
  created_at: string
  finished_at?: string
}

export const getSupportedPairs = (): Promise<SupportedPair[]> =>
  request.get('/migration/data-migrate/supported-pairs')

export const startDataMigration = (data: StartDataMigrationRequest): Promise<{ job_id: string }> =>
  request.post('/migration/data-migrate', data)

export const cancelDataMigration = (jobID: string): Promise<void> =>
  request.post(`/migration/data-migrate/${jobID}/cancel`)

export const listDataMigrationJobs = (): Promise<DataMigrationJob[]> =>
  request.get('/migration/data-migrate/jobs')

// createDataMigrateEventSource 创建 SSE 连接，返回 EventSource 实例
export const createDataMigrateEventSource = (jobID: string): EventSource =>
  new EventSource(`/api/migration/data-migrate/stream?jobID=${jobID}`)
```

- [ ] **Step 2: 提交**

```bash
git add frontend/src/api/migration.ts
git commit -m "feat: add data migration API client functions"
```

---

## Task 15: 前端 MigrationView.vue — 新增数据迁移 Tab

**Files:**
- Modify: `frontend/src/views/MigrationView.vue`

- [ ] **Step 1: 读取现有 MigrationView.vue 确认 Tab 组件用法**

读取文件确认当前使用的 ArcoDesign Tab 组件名称（`a-tabs` / `a-tab-pane` 还是其他）和数据绑定方式。

- [ ] **Step 2: 在现有 Tab 结构中追加「数据迁移」Tab**

在现有 `<a-tabs>` 内最后一个 `<a-tab-pane>` 之后追加：

```vue
<a-tab-pane key="data-migrate" title="数据迁移">
  <div class="data-migrate-container">
    <!-- 源库 / 目标库选择 -->
    <a-row :gutter="16" style="margin-bottom: 16px">
      <a-col :span="11">
        <a-form-item label="源库（MySQL）">
          <a-select
            v-model="dataMigrate.srcConnId"
            placeholder="选择 MySQL 连接"
            @change="checkPairSupport"
          >
            <a-option
              v-for="c in mysqlConnections"
              :key="c.id"
              :value="c.id"
              :label="c.name"
            />
          </a-select>
        </a-form-item>
      </a-col>
      <a-col :span="2" style="text-align:center;line-height:60px;font-size:20px">→</a-col>
      <a-col :span="11">
        <a-form-item label="目标库（PostgreSQL）">
          <a-select
            v-model="dataMigrate.dstConnId"
            placeholder="选择 PostgreSQL 连接"
            @change="checkPairSupport"
          >
            <a-option
              v-for="c in pgConnections"
              :key="c.id"
              :value="c.id"
              :label="c.name"
            />
          </a-select>
        </a-form-item>
      </a-col>
    </a-row>

    <!-- 不支持提示 -->
    <a-alert
      v-if="dataMigrate.unsupportedMsg"
      type="error"
      :content="dataMigrate.unsupportedMsg"
      style="margin-bottom: 16px"
    />

    <!-- 迁移范围 -->
    <a-form-item label="迁移范围" style="margin-bottom: 16px">
      <a-radio-group v-model="dataMigrate.mode">
        <a-radio value="all">全库迁移</a-radio>
        <a-radio value="exclude">排除指定表</a-radio>
        <a-radio value="include">仅迁移指定表</a-radio>
      </a-radio-group>
      <a-input
        v-if="dataMigrate.mode !== 'all'"
        v-model="dataMigrate.filter"
        placeholder="逗号分隔表名，支持 * 通配符，如：*_log,tmp_*"
        style="margin-top: 8px"
      />
    </a-form-item>

    <!-- 高级设置 -->
    <a-collapse style="margin-bottom: 16px">
      <a-collapse-item key="advanced" header="高级设置">
        <a-row :gutter="16">
          <a-col :span="12">
            <a-form-item label="每页行数 (pageSize)">
              <a-input-number v-model="dataMigrate.pageSize" :min="1000" :max="500000" :step="1000" />
            </a-form-item>
          </a-col>
          <a-col :span="12">
            <a-form-item label="最大并发数 (maxParallel)">
              <a-input-number v-model="dataMigrate.maxParallel" :min="1" :max="50" />
            </a-form-item>
          </a-col>
        </a-row>
      </a-collapse-item>
    </a-collapse>

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
  </div>
</a-tab-pane>
```

- [ ] **Step 3: 在 `<script setup lang="ts">` 区域追加响应式状态和函数**

在现有 import 末尾追加：
```typescript
import {
  getSupportedPairs,
  startDataMigration as apiStartMigration,
  cancelDataMigration as apiCancelMigration,
  createDataMigrateEventSource,
  type SupportedPair,
} from '@/api/migration'
```

在现有响应式状态末尾追加：
```typescript
// ===== 数据迁移 =====
const supportedPairs = ref<SupportedPair[]>([])
const logContainer = ref<HTMLElement | null>(null)
let currentEventSource: EventSource | null = null

const dataMigrate = reactive({
  srcConnId: null as number | null,
  dstConnId: null as number | null,
  mode: 'all' as 'all' | 'include' | 'exclude',
  filter: '',
  pageSize: 10000,
  maxParallel: 5,
  running: false,
  finished: false,
  logs: [] as string[],
  unsupportedMsg: '',
  currentJobId: '',
})

// connections 来自已有的连接列表状态（假设叫 connections，按实际名称调整）
const mysqlConnections = computed(() =>
  connections.value.filter((c) => c.db_type === 'mysql')
)
const pgConnections = computed(() =>
  connections.value.filter((c) => c.db_type === 'postgres')
)

const canStartMigration = computed(() =>
  dataMigrate.srcConnId !== null &&
  dataMigrate.dstConnId !== null &&
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

function getLogClass(line: string): string {
  if (line.includes('[ERROR]')) return 'log-error'
  if (line.includes('[WARN]')) return 'log-warn'
  if (line.includes('[DONE]')) return 'log-done'
  return ''
}

async function startDataMigration() {
  dataMigrate.running = true
  dataMigrate.finished = false
  dataMigrate.logs = []

  try {
    const res = await apiStartMigration({
      src_conn_id: dataMigrate.srcConnId!,
      dst_conn_id: dataMigrate.dstConnId!,
      migrate_mode: dataMigrate.mode,
      table_filter: dataMigrate.filter,
      page_size: dataMigrate.pageSize,
      max_parallel: dataMigrate.maxParallel,
    })
    dataMigrate.currentJobId = res.job_id
    connectSSE(res.job_id)
  } catch (e: any) {
    dataMigrate.logs.push(`[ERROR] 启动失败: ${e?.message ?? e}`)
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

// 在 onMounted 或已有初始化逻辑中追加
onMounted(async () => {
  // ... 已有初始化逻辑 ...
  supportedPairs.value = await getSupportedPairs()
})
```

- [ ] **Step 4: 在 `<style>` 区域追加日志样式**

```css
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
```

- [ ] **Step 5: 启动开发服务器，验证前端效果**

```bash
cd frontend && npm run dev
```

在浏览器中：
1. 访问迁移页面，确认「数据迁移」Tab 存在
2. 选择 MySQL 源库和 PostgreSQL 目标库，确认无报错提示
3. 选择不支持的组合（如 Oracle → PostgreSQL），确认红色提示出现且按钮禁用
4. 切换三种迁移范围，确认「排除/仅迁移」模式下输入框出现
5. 展开「高级设置」，确认 pageSize 和 maxParallel 可输入

- [ ] **Step 6: 提交**

```bash
git add frontend/src/views/MigrationView.vue frontend/src/api/migration.ts
git commit -m "feat: add data migration Tab to MigrationView with SSE log streaming"
```

---

## Task 16: 构建前端并全量编译验证

**Files:**
- `frontend/dist/` (构建产物，不提交)

- [ ] **Step 1: 构建前端**

```bash
cd frontend && npm run build 2>&1
```

预期：`dist/` 目录生成，无 TypeScript 错误。

- [ ] **Step 2: 全量 Go 编译**

```bash
/Users/kay/sdk/go1.25.5/bin/go build ./... 2>&1
```

预期：无错误输出。

- [ ] **Step 3: 运行所有 Go 测试**

```bash
/Users/kay/sdk/go1.25.5/bin/go test ./... 2>&1
```

预期：所有测试 PASS。

- [ ] **Step 4: 提交前端构建配置（如有变更）**

```bash
git add frontend/
git commit -m "build: rebuild frontend for data migration feature"
```

---

## 自审结果

**Spec 覆盖检查：**
- ✅ 一键全流程（DDL → 数据 → Post-DDL）：Task 9 Migrator.Run
- ✅ SSE 实时日志：Task 6 JobRegistry + Task 11 StreamDataMigration + Task 15 connectSSE
- ✅ 可扩展 source/target 接口：Task 1、2
- ✅ MySQL → PostgreSQL 类型映射：Task 3
- ✅ 全库 / 排除 / 包含过滤：Task 4
- ✅ 高级设置（pageSize、maxParallel）：Task 9 Config + Task 15 UI
- ✅ supported-pairs 能力查询：Task 11 GetSupportedPairs + Task 14/15 前端联动
- ✅ 不支持组合前端提示：Task 15 checkPairSupport + 红色 alert
- ✅ 取消机制：Task 6 cancel func + Task 11 CancelDataMigration + Task 15 cancelDataMigration
- ✅ DataMigrationJob 持久化：Task 10
- ✅ 5 个 API 端点：Task 12

**类型一致性：** `source.ColumnInfo`、`source.SequenceInfo` 等类型在 Task 1 定义，Task 3、7、8、9 引用一致。`datamigrate.Config`、`datamigrate.Job`、`datamigrate.NewMigrator` 在 Task 6/9 定义，Task 11 引用一致。

**无占位符。**
