// datamigrate/source/postgres.go
package source

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	_ "gitee.com/opengauss/openGauss-connector-go-pq"
	_ "github.com/lib/pq"
)

// PostgresReader 实现 Reader 接口，连接到 PostgreSQL 及其兼容库
// （GaussDB / HighGo / SeaBox / KingBase）。这些库语法与类型均与 PG 兼容，
// 共用同一套元数据查询与 PGToDameng 类型映射，仅底层驱动可能不同。
// PostgreSQL 的迁移单元是「库内的 schema」：连接的 dbname 固定在 DSN 中，
// schema 字段对应本次迁移的 pg schema（类比达梦/Oracle 的 OWNER）。
type PostgresReader struct {
	db     *sql.DB
	schema string // 本次迁移的 pg schema，空则默认 public
}

// NewPostgres 创建并连接 PostgreSQL Reader（使用标准 lib/pq 驱动）。
// dsn 格式：host=... port=... user=... password=... dbname=... sslmode=disable
// schema 为要迁移的 pg schema（如 public）。
func NewPostgres(dsn, schema string, pool ConnPoolConfig) (*PostgresReader, error) {
	return newPostgresWithDriver("postgres", dsn, schema, pool)
}

// NewPostgresCompatible 创建 PG 兼容库 Reader，driverName 指定底层驱动：
// GaussDB 用 "opengauss"，HighGo/SeaBox/KingBase 等复用 "postgres"。
func NewPostgresCompatible(driverName, dsn, schema string, pool ConnPoolConfig) (*PostgresReader, error) {
	if driverName == "" {
		driverName = "postgres"
	}
	return newPostgresWithDriver(driverName, dsn, schema, pool)
}

func newPostgresWithDriver(driverName, dsn, schema string, pool ConnPoolConfig) (*PostgresReader, error) {
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, err
	}
	pool.applyTo(db)
	if err := db.Ping(); err != nil {
		return nil, err
	}
	if schema == "" {
		schema = "public"
	}
	return &PostgresReader{db: db, schema: schema}, nil
}

func (r *PostgresReader) Close() error { return r.db.Close() }

// DBType 统一返回 "postgres"：GaussDB/HighGo/SeaBox/KingBase 与 PG 类型、默认值、
// 值形态完全一致，共用 typemap("postgres","dameng")、coldefault 与 valueconv 分支。
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
	// udt_name 给出 pg 内部短类型名（int4/varchar/numeric/timestamptz/bytea...），
	// 供 typemap 精确匹配；不做 lower()/upper()，列名原样返回。
	rows, err := r.db.QueryContext(ctx,
		`SELECT column_name, udt_name, character_maximum_length,
		        numeric_precision, numeric_scale, is_nullable, column_default, is_identity
		 FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name = $2
		 ORDER BY ordinal_position`, r.schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	info := &TableDDLInfo{TableName: table}
	for rows.Next() {
		var col ColumnInfo
		var nullable, isIdentity string
		var length, precision, scale sql.NullInt64
		var defaultVal sql.NullString
		if err := rows.Scan(&col.Name, &col.DataType, &length, &precision, &scale,
			&nullable, &defaultVal, &isIdentity); err != nil {
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
		// 自增识别：GENERATED ... AS IDENTITY 或 serial（default 为 nextval(...)）。
		// 自增列在达梦以 IDENTITY 声明，不带默认值。
		isAutoInc := strings.EqualFold(isIdentity, "YES") ||
			(defaultVal.Valid && strings.HasPrefix(strings.ToLower(strings.TrimSpace(defaultVal.String)), "nextval("))
		if isAutoInc {
			col.Extra = "auto_increment"
		} else if defaultVal.Valid && strings.TrimSpace(defaultVal.String) != "" {
			s := strings.TrimSpace(defaultVal.String)
			col.Default = &s
		}
		info.Columns = append(info.Columns, col)
	}
	return info, rows.Err()
}

func (r *PostgresReader) GetPrimaryKey(ctx context.Context, table string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT kcu.column_name
		 FROM information_schema.table_constraints tc
		 JOIN information_schema.key_column_usage kcu
		   ON tc.constraint_name = kcu.constraint_name
		  AND tc.table_schema = kcu.table_schema
		 WHERE tc.constraint_type = 'PRIMARY KEY'
		   AND tc.table_schema = $1 AND tc.table_name = $2
		 ORDER BY kcu.ordinal_position`, r.schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pks []string
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			return nil, err
		}
		pks = append(pks, col)
	}
	return pks, rows.Err()
}

func (r *PostgresReader) GetPrimaryKeys(ctx context.Context) ([]IndexInfo, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT kcu.table_name, kcu.column_name
		 FROM information_schema.table_constraints tc
		 JOIN information_schema.key_column_usage kcu
		   ON tc.constraint_name = kcu.constraint_name
		  AND tc.table_schema = kcu.table_schema
		 WHERE tc.constraint_type = 'PRIMARY KEY'
		   AND tc.table_schema = $1
		 ORDER BY kcu.table_name, kcu.ordinal_position`, r.schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	pkMap := map[string]*IndexInfo{}
	var order []string
	for rows.Next() {
		var table, col string
		if err := rows.Scan(&table, &col); err != nil {
			return nil, err
		}
		if _, ok := pkMap[table]; !ok {
			pkMap[table] = &IndexInfo{
				TableName: table,
				IndexName: "PRIMARY",
				IsPrimary: true,
				IsUnique:  true,
			}
			order = append(order, table)
		}
		pkMap[table].Columns = append(pkMap[table].Columns, col)
	}
	result := make([]IndexInfo, 0, len(order))
	for _, k := range order {
		result = append(result, *pkMap[k])
	}
	return result, rows.Err()
}

func (r *PostgresReader) ReadPage(ctx context.Context, table string, pkCols []string, offset, limit int64) ([]string, []string, [][]interface{}, error) {
	var query string
	if len(pkCols) > 0 {
		pkList := make([]string, len(pkCols))
		for i, col := range pkCols {
			pkList[i] = fmt.Sprintf(`"%s"`, col)
		}
		query = fmt.Sprintf(
			`SELECT * FROM "%s"."%s" ORDER BY %s LIMIT %d OFFSET %d`,
			r.schema, table, strings.Join(pkList, ", "), limit, offset)
	} else {
		query = fmt.Sprintf(
			`SELECT * FROM "%s"."%s" LIMIT %d OFFSET %d`,
			r.schema, table, limit, offset)
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
			vals[i] = strings.ReplaceAll(string(b), "\x00", "")
		}
		result = append(result, vals)
	}
	return cols, colTypeName, result, rows.Err()
}

// GetSequences 返回自增列（IDENTITY / serial）信息。
// StartValue 取表内该列的 MAX+1，供达梦 Phase3 RESTART WITH 重置 IDENTITY 种子，
// 避免后续应用插入撞已导入的 id。
func (r *PostgresReader) GetSequences(ctx context.Context) ([]SequenceInfo, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT table_name, column_name
		 FROM information_schema.columns
		 WHERE table_schema = $1
		   AND (is_identity = 'YES' OR lower(column_default) LIKE 'nextval(%')
		 ORDER BY table_name, ordinal_position`, r.schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var seqs []SequenceInfo
	for rows.Next() {
		var s SequenceInfo
		if err := rows.Scan(&s.TableName, &s.ColumnName); err != nil {
			return nil, err
		}
		s.StartValue = 1
		seqs = append(seqs, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 逐个自增列取 MAX+1 作为重置种子（自增列数量通常很少）。
	for i := range seqs {
		var startVal sql.NullInt64
		q := fmt.Sprintf(`SELECT COALESCE(MAX("%s"),0)+1 FROM "%s"."%s"`,
			seqs[i].ColumnName, r.schema, seqs[i].TableName)
		if err := r.db.QueryRowContext(ctx, q).Scan(&startVal); err == nil && startVal.Valid {
			seqs[i].StartValue = startVal.Int64
		}
	}
	return seqs, nil
}

// GetIndexes 返回非主键索引（含唯一索引），跳过表达式索引（attnum=0）。
func (r *PostgresReader) GetIndexes(ctx context.Context) ([]IndexInfo, error) {
	// 用 generate_subscripts 展开 indkey 并保序，避免 WITH ORDINALITY（PG 9.4+），
	// 以兼容 openGauss/GaussDB（PG 9.2 血统）。跳过表达式索引（indkey 元素为 0）。
	rows, err := r.db.QueryContext(ctx,
		`SELECT t.relname AS table_name, i.relname AS index_name,
		        a.attname AS column_name, ix.indisunique
		 FROM pg_index ix
		 JOIN pg_class i ON i.oid = ix.indexrelid
		 JOIN pg_class t ON t.oid = ix.indrelid
		 JOIN pg_namespace n ON n.oid = t.relnamespace
		 JOIN generate_subscripts(ix.indkey, 1) AS ss(k) ON true
		 JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = ix.indkey[ss.k]
		 WHERE n.nspname = $1 AND t.relkind = 'r'
		   AND ix.indisprimary = false AND ix.indkey[ss.k] <> 0
		 ORDER BY t.relname, i.relname, ss.k`, r.schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	idxMap := map[string]*IndexInfo{}
	var order []string
	for rows.Next() {
		var tableName, idxName, colName string
		var isUnique bool
		if err := rows.Scan(&tableName, &idxName, &colName, &isUnique); err != nil {
			return nil, err
		}
		key := tableName + "." + idxName
		if _, ok := idxMap[key]; !ok {
			idxMap[key] = &IndexInfo{
				TableName: tableName,
				IndexName: idxName,
				IsUnique:  isUnique,
				IsPrimary: false,
			}
			order = append(order, key)
		}
		idxMap[key].Columns = append(idxMap[key].Columns, colName)
	}
	result := make([]IndexInfo, 0, len(order))
	for _, k := range order {
		result = append(result, *idxMap[k])
	}
	return result, rows.Err()
}

func (r *PostgresReader) GetForeignKeys(ctx context.Context) ([]FKInfo, error) {
	// 用 pg_constraint 的 conkey/confkey 数组按位置精确配对本列与引用列。
	// 不能用 information_schema.constraint_column_usage：它不保证引用列顺序，
	// 且与 key_column_usage join 时对多列外键会产生 N×N 笛卡尔积（列重复）。
	// 用 generate_subscripts 展开数组并保序，兼容 openGauss/GaussDB（PG 9.2，无 WITH ORDINALITY）。
	rows, err := r.db.QueryContext(ctx,
		`SELECT c.conname AS constraint_name,
		        t.relname  AS table_name,
		        att.attname AS column_name,
		        rt.relname AS ref_table,
		        ratt.attname AS ref_column,
		        CASE c.confdeltype WHEN 'a' THEN 'NO ACTION' WHEN 'r' THEN 'RESTRICT'
		             WHEN 'c' THEN 'CASCADE' WHEN 'n' THEN 'SET NULL' WHEN 'd' THEN 'SET DEFAULT'
		             ELSE 'NO ACTION' END AS delete_rule,
		        CASE c.confupdtype WHEN 'a' THEN 'NO ACTION' WHEN 'r' THEN 'RESTRICT'
		             WHEN 'c' THEN 'CASCADE' WHEN 'n' THEN 'SET NULL' WHEN 'd' THEN 'SET DEFAULT'
		             ELSE 'NO ACTION' END AS update_rule
		 FROM pg_constraint c
		 JOIN pg_class t ON t.oid = c.conrelid
		 JOIN pg_namespace n ON n.oid = t.relnamespace
		 JOIN pg_class rt ON rt.oid = c.confrelid
		 JOIN generate_subscripts(c.conkey, 1) AS ss(k) ON true
		 JOIN pg_attribute att  ON att.attrelid = c.conrelid  AND att.attnum = c.conkey[ss.k]
		 JOIN pg_attribute ratt ON ratt.attrelid = c.confrelid AND ratt.attnum = c.confkey[ss.k]
		 WHERE c.contype = 'f' AND n.nspname = $1
		 ORDER BY t.relname, c.conname, ss.k`, r.schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	fkMap := map[string]*FKInfo{}
	var order []string
	for rows.Next() {
		var constraintName, tableName, col, refTable, refCol, onDelete, onUpdate string
		if err := rows.Scan(&constraintName, &tableName, &col, &refTable, &refCol, &onDelete, &onUpdate); err != nil {
			return nil, err
		}
		key := tableName + "." + constraintName
		if _, ok := fkMap[key]; !ok {
			fkMap[key] = &FKInfo{
				TableName:      tableName,
				ConstraintName: constraintName,
				RefTable:       refTable,
				OnDelete:       onDelete,
				OnUpdate:       onUpdate,
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

// GetViews 返回视图。view_definition 已是 SELECT 主体（不含 CREATE 头），
// 与达梦 ViewStatements 的期望一致；跨库语法差异导致的失败由 migrator 按对象记录。
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
		fmt.Sprintf(`SELECT COUNT(*) FROM "%s"."%s"`, r.schema, table),
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
