package source

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"

	_ "github.com/microsoft/go-mssqldb"
)

// SQLServerReader 实现 Reader 接口，连接到 SQL Server 数据库
type SQLServerReader struct {
	db     *sql.DB
	dbName string
}

// NewSQLServer 创建并连接 SQL Server Reader
// dsn 格式：sqlserver://user:password@host:port?database=dbname
func NewSQLServer(dsn, dbName string, pool ConnPoolConfig) (*SQLServerReader, error) {
	db, err := sql.Open("sqlserver", dsn)
	if err != nil {
		return nil, err
	}
	pool.applyTo(db)
	if err := db.Ping(); err != nil {
		return nil, err
	}
	return &SQLServerReader{db: db, dbName: dbName}, nil
}

func (r *SQLServerReader) Close() error   { return r.db.Close() }
func (r *SQLServerReader) DBType() string { return "sqlserver" }

func (r *SQLServerReader) ListDatabases(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT name FROM sys.databases
		 WHERE name NOT IN ('master', 'tempdb', 'model', 'msdb')
		 ORDER BY name`)
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

func (r *SQLServerReader) ListTables(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT t.name FROM sys.tables t
		 INNER JOIN sys.schemas s ON t.schema_id = s.schema_id
		 WHERE t.type = 'U'
		 ORDER BY t.name`)
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

func (r *SQLServerReader) GetTableDDLInfo(ctx context.Context, table string) (*TableDDLInfo, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT c.COLUMN_NAME, c.DATA_TYPE,
		        ISNULL(c.CHARACTER_MAXIMUM_LENGTH, 0),
		        ISNULL(c.NUMERIC_PRECISION, 0),
		        ISNULL(c.NUMERIC_SCALE, 0),
		        c.IS_NULLABLE,
		        c.COLUMN_DEFAULT,
		        sc.is_identity
		 FROM INFORMATION_SCHEMA.COLUMNS c
		 JOIN sys.tables st ON c.TABLE_NAME = st.name
		 JOIN sys.columns sc ON st.object_id = sc.object_id AND c.COLUMN_NAME = sc.name
		 WHERE c.TABLE_CATALOG = DB_NAME() AND c.TABLE_NAME = @p1
		 ORDER BY c.ORDINAL_POSITION`,
		table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	info := &TableDDLInfo{TableName: table}
	for rows.Next() {
		var col ColumnInfo
		var nullable string
		var charLen, numPrec, numScale int64
		var defaultVal sql.NullString
		var isIdentity bool
		if err := rows.Scan(&col.Name, &col.DataType, &charLen, &numPrec, &numScale,
			&nullable, &defaultVal, &isIdentity); err != nil {
			return nil, err
		}
		// CHARACTER_MAXIMUM_LENGTH 为 -1 表示 MAX 类型（varchar(max) 等）
		col.Length = charLen
		col.Precision = numPrec
		col.Scale = numScale
		col.IsNullable = strings.EqualFold(nullable, "YES")
		if defaultVal.Valid {
			col.Default = &defaultVal.String
		}
		if isIdentity {
			col.Extra = "auto_increment"
		}
		info.Columns = append(info.Columns, col)
	}
	return info, rows.Err()
}

func (r *SQLServerReader) GetPrimaryKey(ctx context.Context, table string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT kcu.COLUMN_NAME
		 FROM INFORMATION_SCHEMA.KEY_COLUMN_USAGE kcu
		 JOIN INFORMATION_SCHEMA.TABLE_CONSTRAINTS tc
		   ON kcu.CONSTRAINT_NAME = tc.CONSTRAINT_NAME
		  AND kcu.TABLE_SCHEMA = tc.TABLE_SCHEMA
		 WHERE tc.CONSTRAINT_TYPE = 'PRIMARY KEY'
		   AND kcu.TABLE_CATALOG = DB_NAME()
		   AND kcu.TABLE_NAME = @p1
		 ORDER BY kcu.ORDINAL_POSITION`,
		table)
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

func (r *SQLServerReader) GetPrimaryKeys(ctx context.Context) ([]IndexInfo, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT kcu.TABLE_NAME, kcu.COLUMN_NAME
		 FROM INFORMATION_SCHEMA.KEY_COLUMN_USAGE kcu
		 JOIN INFORMATION_SCHEMA.TABLE_CONSTRAINTS tc
		   ON kcu.CONSTRAINT_NAME = tc.CONSTRAINT_NAME
		  AND kcu.TABLE_SCHEMA = tc.TABLE_SCHEMA
		 WHERE tc.CONSTRAINT_TYPE = 'PRIMARY KEY'
		   AND kcu.TABLE_CATALOG = DB_NAME()
		 ORDER BY kcu.TABLE_NAME, kcu.ORDINAL_POSITION`)
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

func (r *SQLServerReader) ReadPage(ctx context.Context, table string, pkCols []string, offset, limit int64) ([]string, []string, [][]interface{}, error) {
	var query string
	if len(pkCols) > 0 {
		pkList := strings.Join(pkCols, ", ")
		joinConds := make([]string, len(pkCols))
		for i, col := range pkCols {
			joinConds[i] = fmt.Sprintf("temp.[%s] = t.[%s]", col, col)
		}
		query = fmt.Sprintf(
			`SELECT t.* FROM (SELECT %s FROM [%s] ORDER BY %s OFFSET %d ROWS FETCH NEXT %d ROWS ONLY) temp LEFT JOIN [%s] t ON %s`,
			pkList, table, pkList, offset, limit, table, strings.Join(joinConds, " AND "))
	} else {
		query = fmt.Sprintf(
			`SELECT * FROM [%s] ORDER BY (SELECT NULL) OFFSET %d ROWS FETCH NEXT %d ROWS ONLY`,
			table, offset, limit)
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
			if v == nil {
				continue
			}
			dt := colTypeName[i]
			switch dt {
			case "BIT":
				// 驱动返回 bool,归一为 int64(0/1) —— 通用转换,与目标库无关
				switch b := v.(type) {
				case bool:
					if b {
						vals[i] = int64(1)
					} else {
						vals[i] = int64(0)
					}
				}
			case "VARCHAR", "NVARCHAR", "CHAR", "NCHAR", "TEXT", "NTEXT":
				// 清理非法 Unicode 空字符(通用清洗)
				if b, ok := v.([]byte); ok {
					vals[i] = strings.ReplaceAll(string(b), "\x00", "")
				} else if s, ok := v.(string); ok {
					vals[i] = strings.ReplaceAll(s, "\x00", "")
				}
				// UNIQUEIDENTIFIER / DATETIME / TIME / MONEY / XML 保持中立值,
				// 由目标 ValueConverter 落地;VARBINARY/BINARY/IMAGE/TIMESTAMP 保持 []byte
			}
		}
		result = append(result, vals)
	}
	return cols, colTypeName, result, rows.Err()
}

func (r *SQLServerReader) GetSequences(ctx context.Context) ([]SequenceInfo, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT t.name AS table_name,
		        c.name AS column_name,
		        CAST(ic.seed_value AS BIGINT) AS auto_increment
		 FROM sys.tables t
		 JOIN sys.columns c ON t.object_id = c.object_id
		 JOIN sys.identity_columns ic ON t.object_id = ic.object_id AND c.column_id = ic.column_id
		 WHERE t.is_ms_shipped = 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var seqs []SequenceInfo
	for rows.Next() {
		var s SequenceInfo
		var startVal sql.NullInt64
		if err := rows.Scan(&s.TableName, &s.ColumnName, &startVal); err != nil {
			return nil, err
		}
		if startVal.Valid {
			s.StartValue = startVal.Int64
		} else {
			s.StartValue = 1
		}
		seqs = append(seqs, s)
	}
	return seqs, rows.Err()
}

func (r *SQLServerReader) GetIndexes(ctx context.Context) ([]IndexInfo, error) {
	rows, err := r.db.QueryContext(ctx,
		`WITH indexcolumns AS (
		     SELECT ixc.object_id, ixc.index_id,
		         STUFF((SELECT ', ' + c2.name
		                FROM sys.index_columns ixc2
		                JOIN sys.columns c2 ON ixc2.object_id = c2.object_id AND ixc2.column_id = c2.column_id
		                WHERE ixc2.object_id = ixc.object_id AND ixc2.index_id = ixc.index_id
		                ORDER BY ixc2.key_ordinal
		                FOR XML PATH(''), TYPE).value('.', 'nvarchar(max)'), 1, 2, '') AS column_list
		     FROM sys.index_columns ixc
		     GROUP BY ixc.object_id, ixc.index_id
		 )
		 SELECT t.name, i.name, ic.column_list, i.is_unique, i.is_primary_key
		 FROM sys.tables t
		 JOIN sys.indexes i ON t.object_id = i.object_id
		 JOIN indexcolumns ic ON t.object_id = ic.object_id AND i.index_id = ic.index_id
		 WHERE t.is_ms_shipped = 0 AND i.type > 0 AND i.is_primary_key = 0
		 ORDER BY t.name, i.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var indexes []IndexInfo
	for rows.Next() {
		var tableName, idxName, colList string
		var isUnique, isPrimary bool
		if err := rows.Scan(&tableName, &idxName, &colList, &isUnique, &isPrimary); err != nil {
			return nil, err
		}
		cols := strings.Split(colList, ", ")
		indexes = append(indexes, IndexInfo{
			TableName: tableName,
			IndexName: idxName,
			Columns:   cols,
			IsUnique:  isUnique,
			IsPrimary: isPrimary,
		})
	}
	return indexes, rows.Err()
}

func (r *SQLServerReader) GetForeignKeys(ctx context.Context) ([]FKInfo, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT fk.name AS constraint_name,
		        tp.name AS table_name,
		        cp.name AS column_name,
		        tr.name AS ref_table,
		        cr.name AS ref_column,
		        fk.delete_referential_action_desc,
		        fk.update_referential_action_desc
		 FROM sys.foreign_keys fk
		 JOIN sys.foreign_key_columns fkc ON fk.object_id = fkc.constraint_object_id
		 JOIN sys.tables tp ON fkc.parent_object_id = tp.object_id
		 JOIN sys.columns cp ON fkc.parent_object_id = cp.object_id AND fkc.parent_column_id = cp.column_id
		 JOIN sys.tables tr ON fkc.referenced_object_id = tr.object_id
		 JOIN sys.columns cr ON fkc.referenced_object_id = cr.object_id AND fkc.referenced_column_id = cr.column_id
		 ORDER BY fk.name, fkc.constraint_column_id`)
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
		// SQL Server 返回 "NO_ACTION"，PostgreSQL 需要 "NO ACTION"
		onDelete = strings.ReplaceAll(onDelete, "_", " ")
		onUpdate = strings.ReplaceAll(onUpdate, "_", " ")
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

func (r *SQLServerReader) GetViews(ctx context.Context) ([]ViewInfo, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT v.name,
		        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(
		            LOWER(m.definition),
		            'dbo.', ''), '  ', ' '),
		            'create view', 'create or replace view'),
		            '[dbo].', ''),
		            '[', ''),
		            ']', '') AS view_def
		 FROM sys.views v
		 JOIN sys.sql_modules m ON v.object_id = m.object_id
		 WHERE m.definition IS NOT NULL`)
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
		v.Definition = transformSQLServerViewDef(v.Definition)
		views = append(views, v)
	}
	return views, rows.Err()
}

// transformSQLServerViewDef 将 SQL Server 视图定义转换为 PostgreSQL 兼容语法，
// 并剥离头部的 CREATE [OR REPLACE] VIEW name AS，只保留 SELECT 体。
func transformSQLServerViewDef(def string) string {
	// 剥离 "create [or replace] view <name> [with ...] as"，大小写不敏感。
	// 视图名与 as 之间可能夹着 WITH SCHEMABINDING、WITH ENCRYPTION 等子句，
	// 用非贪婪 .*? 跨越这些可选内容，但限定在单行/多行均可（(?s) dotall）。
	reHeader := regexp.MustCompile(`(?is)^\s*create\s+(or\s+replace\s+)?view\s+\S+\s+.*?\bas\b\s+`)
	def = reHeader.ReplaceAllString(def, "")
	def = strings.TrimSpace(def)
	// 防御：万一 WITH SCHEMABINDING 残留在 SELECT 体开头
	def = regexp.MustCompile(`(?i)^with\s+\w[\w,\s]*\n`).ReplaceAllString(def, "")

	// TOP (n) PERCENT / TOP n PERCENT → 移除（SQL Server 视图用此语法配合 ORDER BY，PG 不支持）
	def = regexp.MustCompile(`(?i)\bTOP\s*\(?\s*\d+\s*\)?\s*PERCENT\s*`).ReplaceAllString(def, "")
	// TOP (n) 不带 PERCENT → 移除（视图里的 TOP n 在 PG 里无对应语法）
	def = regexp.MustCompile(`(?i)\bTOP\s*\(?\s*\d+\s*\)?\s*`).ReplaceAllString(def, "")
	// 移除视图定义最外层的 ORDER BY（PG 视图不允许裸 ORDER BY，SQL Server 配合 TOP 100 PERCENT 使用）
	def = stripTopLevelOrderBy(def)

	// CAST(x AS nvarchar) / CAST(x AS nvarchar(n)) / CAST(x AS varchar(n)) → CAST(x AS varchar) / CAST(x AS text)
	// nvarchar/nchar/ntext → varchar/char/text（PG 无 n 前缀类型）
	def = regexp.MustCompile(`(?i)\bcast\s*\((.+?)\s+as\s+nvarchar\s*\(\s*\d+\s*\)\s*\)`).ReplaceAllStringFunc(def, func(m string) string {
		return regexp.MustCompile(`(?i)\bas\s+nvarchar\s*\(\s*\d+\s*\)`).ReplaceAllString(m, "AS varchar")
	})
	def = regexp.MustCompile(`(?i)\bcast\s*\((.+?)\s+as\s+nvarchar\s*\)`).ReplaceAllStringFunc(def, func(m string) string {
		return regexp.MustCompile(`(?i)\bas\s+nvarchar\b`).ReplaceAllString(m, "AS varchar")
	})
	def = regexp.MustCompile(`(?i)\bcast\s*\((.+?)\s+as\s+nchar\s*\(\s*\d+\s*\)\s*\)`).ReplaceAllStringFunc(def, func(m string) string {
		return regexp.MustCompile(`(?i)\bas\s+nchar\s*\(\s*\d+\s*\)`).ReplaceAllString(m, "AS char")
	})
	def = regexp.MustCompile(`(?i)\bcast\s*\((.+?)\s+as\s+ntext\s*\)`).ReplaceAllStringFunc(def, func(m string) string {
		return regexp.MustCompile(`(?i)\bas\s+ntext\b`).ReplaceAllString(m, "AS text")
	})

	// SQL Server 允许单引号列别名 AS '名称'，PG 要求双引号标识符 AS "名称"
	def = regexp.MustCompile(`(?i)\bAS\s+'([^']+)'`).ReplaceAllString(def, `AS "$1"`)

	// + 字符串拼接 → ||
	def = strings.ReplaceAll(def, " + ", " || ")
	// len(x) → length(x)
	def = replaceFuncName(def, "len(", "length(")
	// isnull(x, y) → coalesce(x, y)
	def = replaceFuncName(def, "isnull(", "coalesce(")
	// getdate() → current_timestamp
	def = strings.ReplaceAll(def, "getdate()", "current_timestamp")
	return def
}

func replaceFuncName(s, old, new string) string {
	return strings.ReplaceAll(s, old, new)
}

func (r *SQLServerReader) GetTriggerCount(ctx context.Context) (int, error) {
	var count int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sys.triggers WHERE is_ms_shipped = 0`,
	).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}

func (r *SQLServerReader) CountRows(ctx context.Context, table string) (int64, error) {
	var count int64
	err := r.db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT COUNT(*) FROM [%s]", table),
	).Scan(&count)
	return count, err
}

// stripTopLevelOrderBy 移除 SELECT 语句最外层的 ORDER BY 子句（不影响子查询内部的 ORDER BY）。
// SQL Server 视图常用 "TOP 100 PERCENT ... ORDER BY" 的写法，移除 TOP 100 PERCENT 后
// 剩余的顶层 ORDER BY 在 PostgreSQL 视图中不合法，需要一并去除。
func stripTopLevelOrderBy(def string) string {
	upper := strings.ToUpper(def)
	depth := 0
	orderByPos := -1
	for i := 0; i < len(upper); i++ {
		switch upper[i] {
		case '(':
			depth++
		case ')':
			depth--
		}
		// 只匹配顶层（depth == 0）的 ORDER BY
		if depth == 0 && i+8 <= len(upper) && upper[i:i+8] == "ORDER BY" {
			// 确认是关键字边界，不是列名/别名的一部分
			if i > 0 && (upper[i-1] == ' ' || upper[i-1] == '\n' || upper[i-1] == '\t' || upper[i-1] == ')') {
				orderByPos = i
				// 不 break：取最后一个顶层 ORDER BY
			}
		}
	}
	if orderByPos < 0 {
		return def
	}
	return strings.TrimRight(def[:orderByPos], " \t\n\r")
}
