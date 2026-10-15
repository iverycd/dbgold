// datamigrate/source/postgres.go
package source

import (
	"context"
	"database/sql"
	"dbgold/internal/pgschema"
	"fmt"
	"strings"
	"time"

	_ "dbgold/internal/highgopq"
	_ "gitee.com/opengauss/openGauss-connector-go-pq"
	_ "github.com/lib/pq"
)

func (r *PostgresReader) MigrationSchema() string { return r.schema }

// PostgresReader 实现 Reader 接口，连接到 PostgreSQL 及其兼容库
// （GaussDB / HighGo / Vastbase / GBase / SeaBox / KingBase）。具体目录能力需探测；
// 使用家族类型映射不代表各产品的对象语义完全相同。
// PostgreSQL 的迁移单元是「库内的 schema」：连接的 dbname 固定在 DSN 中，
// schema 字段对应本次迁移的 pg schema（类比达梦/Oracle 的 OWNER）。
type PostgresReader struct {
	db      *sql.DB
	schema  string // 本次迁移的 pg schema，空则默认 public
	profile PGProfile
}

// NewPostgres 创建并连接 PostgreSQL Reader（使用标准 lib/pq 驱动）。
// dsn 格式：host=... port=... user=... password=... dbname=... sslmode=disable
// schema 为要迁移的 pg schema（如 public）。
func NewPostgres(dsn, schema string, pool ConnPoolConfig) (*PostgresReader, error) {
	return NewPostgresProduct("postgres", "postgres", dsn, schema, pool)
}

// NewPostgresCompatible 创建 PG 兼容库 Reader，driverName 指定底层驱动：
// GaussDB 用 "opengauss"，HighGo 用 "highgo"，其余兼容库复用 "postgres"。
func NewPostgresCompatible(driverName, dsn, schema string, pool ConnPoolConfig) (*PostgresReader, error) {
	if driverName == "" {
		driverName = "postgres"
	}
	return NewPostgresProduct(driverName, driverName, dsn, schema, pool)
}

func NewPostgresProduct(product, driverName, dsn, schema string, pool ConnPoolConfig) (*PostgresReader, error) {
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, err
	}
	pool.applyTo(db)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	if schema == "" {
		schema = "public"
	}
	r := &PostgresReader{db: db, schema: schema}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := r.detectProfile(ctx, product); err != nil {
		db.Close()
		return nil, err
	}
	return r, nil
}

func (r *PostgresReader) Close() error { return r.db.Close() }

// DBType 保留家族映射键以兼容原有目标；实际产品、版本与模式由 PGProfile 返回。
func (r *PostgresReader) DBType() string { return "postgres" }

// ListDatabases 返回该连接（dbname 固定）下所有可迁移的 schema，排除系统 schema。
func (r *PostgresReader) ListDatabases(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT schema_name FROM information_schema.schemata
		 WHERE schema_name NOT LIKE 'pg_%' AND schema_name <> 'information_schema'
		 ORDER BY schema_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var dbs []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if pgschema.IsHidden(name) {
			continue
		}
		dbs = append(dbs, name)
	}
	return dbs, rows.Err()
}

func (r *PostgresReader) ListTables(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT table_name FROM information_schema.tables
		 WHERE table_schema = $1 AND table_type = 'BASE TABLE'
		 ORDER BY table_name`, r.schema)
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

func (r *PostgresReader) GetTableDDLInfo(ctx context.Context, table string) (*TableDDLInfo, error) {
	return r.pgColumns(ctx, table)
}

func (r *PostgresReader) GetPrimaryKey(ctx context.Context, table string) ([]string, error) {
	indexes, err := r.pgIndexes(ctx)
	if err != nil {
		return nil, err
	}
	for _, idx := range indexes {
		if idx.TableName == table && idx.IsPrimary {
			return idx.Columns, nil
		}
	}
	return nil, nil
}

func (r *PostgresReader) GetPrimaryKeys(ctx context.Context) ([]IndexInfo, error) {
	indexes, err := r.pgIndexes(ctx)
	if err != nil {
		return nil, err
	}
	var out []IndexInfo
	for _, idx := range indexes {
		if idx.IsPrimary {
			out = append(out, idx)
		}
	}
	return out, nil
}

func (r *PostgresReader) ReadPage(ctx context.Context, table string, pkCols []string, offset, limit int64) ([]string, []string, [][]interface{}, error) {
	var query string
	if len(pkCols) > 0 {
		pkList := make([]string, len(pkCols))
		for i, col := range pkCols {
			pkList[i] = QuotePGIdent(col)
		}
		query = fmt.Sprintf(
			`SELECT * FROM %s.%s ORDER BY %s LIMIT %d OFFSET %d`,
			QuotePGIdent(r.schema), QuotePGIdent(table), strings.Join(pkList, ", "), limit, offset)
	} else {
		query = fmt.Sprintf(
			`SELECT * FROM %s.%s ORDER BY ctid LIMIT %d OFFSET %d`,
			QuotePGIdent(r.schema), QuotePGIdent(table), limit, offset)
	}
	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, nil, nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, nil, nil, err
	}
	colTypes, err := rows.ColumnTypes()
	if err != nil {
		return nil, nil, nil, err
	}
	colTypeName := make([]string, len(colTypes))
	for i, ct := range colTypes {
		colTypeName[i] = strings.ToUpper(ct.DatabaseTypeName())
	}
	var result [][]interface{}
	for rows.Next() {
		vals := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, nil, nil, err
		}
		for i, v := range vals {
			b, ok := v.([]byte)
			if !ok {
				continue
			}
			// BYTEA 二进制列保持 []byte（中立值，落地为达梦 BLOB）；
			// 其余以 []byte 返回的类型（numeric/text/json/uuid 等）转字符串并去 \x00，
			// 交由 dm 驱动参数化写入。
			if colTypeName[i] == "BYTEA" {
				continue
			}
			vals[i] = string(b)
		}
		result = append(result, vals)
	}
	return cols, colTypeName, result, rows.Err()
}

// GetSequences 返回自增列（IDENTITY / serial）信息。
// StartValue 取表内该列的 MAX+1，供达梦 Phase3 RESTART WITH 重置 IDENTITY 种子，
// 避免后续应用插入撞已导入的 id。
func (r *PostgresReader) GetSequences(ctx context.Context) ([]SequenceInfo, error) {
	tables, err := r.ListTables(ctx)
	if err != nil {
		return nil, err
	}
	snap := &PGSnapshot{}
	for _, name := range tables {
		snap.Tables = append(snap.Tables, PGTable{Info: TableDDLInfo{TableName: name}})
	}
	if err = r.loadPGSequences(ctx, snap); err != nil {
		return nil, err
	}
	var result []SequenceInfo
	for _, t := range snap.Tables {
		for _, seq := range t.Sequences {
			if seq.Shared || seq.Cycle || seq.Increment != 1 {
				return nil, fmt.Errorf("序列 %s 无法映射为普通自增", seq.Name)
			}
			result = append(result, seq.SequenceInfo)
		}
	}
	return result, nil
}

func (r *PostgresReader) GetIndexes(ctx context.Context) ([]IndexInfo, error) {
	indexes, err := r.pgIndexes(ctx)
	if err != nil {
		return nil, err
	}
	var out []IndexInfo
	for _, idx := range indexes {
		if !idx.IsPrimary {
			if idx.Unsupported != "" {
				return nil, fmt.Errorf("索引 %s: %s", idx.IndexName, idx.Unsupported)
			}
			out = append(out, idx)
		}
	}
	return out, nil
}

func (r *PostgresReader) GetForeignKeys(ctx context.Context) ([]FKInfo, error) {
	return r.pgForeignKeys(ctx)
}

func (r *PostgresReader) GetViews(ctx context.Context) ([]ViewInfo, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT table_name, view_definition
		 FROM information_schema.views
		 WHERE table_schema = $1
		 ORDER BY table_name`, r.schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var views []ViewInfo
	for rows.Next() {
		var v ViewInfo
		var def sql.NullString
		if err := rows.Scan(&v.ViewName, &def); err != nil {
			return nil, err
		}
		if !def.Valid || strings.TrimSpace(def.String) == "" {
			return nil, fmt.Errorf("视图 %s 定义不可读取", v.ViewName)
		}
		v.Definition = strings.TrimRight(strings.TrimSpace(def.String), "; \t\n\r")
		views = append(views, v)
	}
	return views, rows.Err()
}

func (r *PostgresReader) GetTriggerCount(ctx context.Context) (int, error) {
	var count int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.triggers WHERE trigger_schema = $1`,
		r.schema,
	).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}

func (r *PostgresReader) CountRows(ctx context.Context, table string) (int64, error) {
	var count int64
	err := r.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT COUNT(*) FROM %s.%s`, QuotePGIdent(r.schema), QuotePGIdent(table)),
	).Scan(&count)
	return count, err
}

// GetComments 返回表注释和列注释（原始大小写，不做 lower()/upper()）。
// ColumnName 为空表示表注释，非空表示列注释。
func (r *PostgresReader) GetComments(ctx context.Context) ([]CommentInfo, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT c.relname AS table_name, '' AS column_name, obj_description(c.oid, 'pg_class') AS comment
		 FROM pg_class c
		 JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = $1 AND c.relkind = 'r'
		   AND obj_description(c.oid, 'pg_class') IS NOT NULL
		 UNION ALL
		 SELECT c.relname, a.attname, col_description(c.oid, a.attnum)
		 FROM pg_class c
		 JOIN pg_namespace n ON n.oid = c.relnamespace
		 JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
		 WHERE n.nspname = $1 AND c.relkind = 'r'
		   AND col_description(c.oid, a.attnum) IS NOT NULL
		 ORDER BY table_name, column_name`, r.schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var comments []CommentInfo
	for rows.Next() {
		var cm CommentInfo
		var colName sql.NullString
		if err := rows.Scan(&cm.TableName, &colName, &cm.Comment); err != nil {
			return nil, err
		}
		cm.ColumnName = colName.String
		comments = append(comments, cm)
	}
	return comments, rows.Err()
}
