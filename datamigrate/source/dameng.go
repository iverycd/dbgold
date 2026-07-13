package source

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"

	_ "gitee.com/chunanyong/dm"
)

// DaMengReader 实现 Reader 接口，连接到达梦数据库
type DaMengReader struct {
	db     *sql.DB
	schema string // 对应达梦的 OWNER/SCHEMA，来自连接配置的 Database 字段
}

// NewDaMeng 创建并连接达梦 Reader
// dsn 格式：dm://username:password@host:port
// schema 为要迁移的 OWNER（达梦用 schema 隔离数据，等同于用户名）
func NewDaMeng(dsn, schema string, pool ConnPoolConfig) (*DaMengReader, error) {
	db, err := sql.Open("dm", dsn)
	if err != nil {
		return nil, err
	}
	pool.applyTo(db)
	if err := db.Ping(); err != nil {
		return nil, err
	}
	return &DaMengReader{db: db, schema: strings.ToUpper(schema)}, nil
}

func (r *DaMengReader) Close() error   { return r.db.Close() }
func (r *DaMengReader) DBType() string { return "dameng" }

func (r *DaMengReader) ListDatabases(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT DISTINCT OWNER FROM ALL_TABLES ORDER BY OWNER`)
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

func (r *DaMengReader) ListTables(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT TABLE_NAME FROM ALL_TABLES WHERE OWNER = ? ORDER BY TABLE_NAME`,
		r.schema)
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

func (r *DaMengReader) GetTableDDLInfo(ctx context.Context, table string) (*TableDDLInfo, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT c.COLUMN_NAME,
		        c.DATA_TYPE,
		        NVL(c.CHAR_LENGTH, 0),
		        NVL(c.DATA_PRECISION, 0),
		        NVL(c.DATA_SCALE, 0),
		        c.NULLABLE,
		        c.DATA_DEFAULT,
		        CASE WHEN sc.INFO2 & 0x01 = 0x01 THEN 1 ELSE 0 END AS IS_IDENTITY
		 FROM ALL_TAB_COLUMNS c
		 JOIN ALL_OBJECTS o ON o.object_name = c.TABLE_NAME AND o.owner = c.OWNER
		 JOIN SYS.SYSCOLUMNS sc ON sc.id = o.object_id AND sc.name = c.COLUMN_NAME
		 WHERE c.OWNER = ? AND c.TABLE_NAME = ?
		 ORDER BY c.COLUMN_ID`,
		r.schema, table)
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
		var isIdentity int
		if err := rows.Scan(&col.Name, &col.DataType, &charLen, &numPrec, &numScale,
			&nullable, &defaultVal, &isIdentity); err != nil {
			return nil, err
		}
		col.Length = charLen
		col.Precision = numPrec
		col.Scale = numScale
		col.IsNullable = strings.EqualFold(nullable, "Y")
		if defaultVal.Valid && strings.TrimSpace(defaultVal.String) != "" {
			s := strings.TrimSpace(defaultVal.String)
			col.Default = &s
		}
		if isIdentity == 1 {
			col.Extra = "auto_increment"
		}
		info.Columns = append(info.Columns, col)
	}
	return info, rows.Err()
}

func (r *DaMengReader) GetPrimaryKey(ctx context.Context, table string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT cc.COLUMN_NAME
		 FROM ALL_CONSTRAINTS c
		 JOIN ALL_CONS_COLUMNS cc
		   ON c.OWNER = cc.OWNER AND c.CONSTRAINT_NAME = cc.CONSTRAINT_NAME
		 WHERE c.CONSTRAINT_TYPE = 'P'
		   AND c.OWNER = ? AND c.TABLE_NAME = ?
		 ORDER BY cc.POSITION`,
		r.schema, table)
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

func (r *DaMengReader) GetPrimaryKeys(ctx context.Context) ([]IndexInfo, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT cc.TABLE_NAME, cc.COLUMN_NAME
		 FROM ALL_CONSTRAINTS c
		 JOIN ALL_CONS_COLUMNS cc
		   ON c.OWNER = cc.OWNER AND c.CONSTRAINT_NAME = cc.CONSTRAINT_NAME
		 WHERE c.CONSTRAINT_TYPE = 'P'
		   AND c.OWNER = ?
		 ORDER BY cc.TABLE_NAME, cc.POSITION`,
		r.schema)
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

func (r *DaMengReader) ReadPage(ctx context.Context, table string, pkCols []string, offset, limit int64) ([]string, []string, [][]interface{}, error) {
	var query string
	if len(pkCols) > 0 {
		pkList := make([]string, len(pkCols))
		for i, col := range pkCols {
			pkList[i] = fmt.Sprintf(`"%s"`, col)
		}
		orderBy := strings.Join(pkList, ", ")
		query = fmt.Sprintf(
			`SELECT * FROM "%s"."%s" ORDER BY %s OFFSET %d ROWS FETCH NEXT %d ROWS ONLY`,
			r.schema, table, orderBy, offset, limit)
	} else {
		query = fmt.Sprintf(
			`SELECT * FROM "%s"."%s" OFFSET %d ROWS FETCH NEXT %d ROWS ONLY`,
			r.schema, table, offset, limit)
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
			case "CHAR", "VARCHAR", "VARCHAR2", "CLOB", "TEXT", "LONGVARCHAR":
				if b, ok := v.([]byte); ok {
					vals[i] = strings.ReplaceAll(string(b), "\x00", "")
				} else if s, ok := v.(string); ok {
					vals[i] = strings.ReplaceAll(s, "\x00", "")
				}
				// DATE/DATETIME/TIMESTAMP/TIME 保持 time.Time(中立值),
				// 由目标 ValueConverter 格式化;BLOB/BINARY/VARBINARY 保持 []byte
			}
		}
		result = append(result, vals)
	}
	return cols, colTypeName, result, rows.Err()
}

func (r *DaMengReader) GetSequences(ctx context.Context) ([]SequenceInfo, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT b.table_name, a.name AS col_name,
		        (ident_incr(b.owner||'.'||b.table_name) + IDENT_CURRENT(b.owner||'.'||b.table_name)) AS seq_next_val
		 FROM SYS.SYSCOLUMNS a, all_tables b, all_objects c
		 WHERE a.INFO2 & 0x01 = 0x01
		   AND a.id = c.object_id
		   AND c.object_name = b.table_name
		   AND b.owner = ?`,
		r.schema)
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

func (r *DaMengReader) GetIndexes(ctx context.Context) ([]IndexInfo, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT i.TABLE_NAME, i.INDEX_NAME, ic.COLUMN_NAME, i.UNIQUENESS
		 FROM ALL_INDEXES i
		 JOIN ALL_IND_COLUMNS ic
		   ON i.OWNER = ic.INDEX_OWNER AND i.INDEX_NAME = ic.INDEX_NAME
		 WHERE i.OWNER = ?
		   AND NOT EXISTS (
		       SELECT 1 FROM ALL_CONSTRAINTS c
		       WHERE c.OWNER = i.OWNER AND c.INDEX_NAME = i.INDEX_NAME
		         AND c.CONSTRAINT_TYPE = 'P'
		   )
		 ORDER BY i.TABLE_NAME, i.INDEX_NAME, ic.COLUMN_POSITION`,
		r.schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	idxMap := map[string]*IndexInfo{}
	var order []string
	for rows.Next() {
		var tableName, idxName, colName, uniqueness string
		if err := rows.Scan(&tableName, &idxName, &colName, &uniqueness); err != nil {
			return nil, err
		}
		key := tableName + "." + idxName
		if _, ok := idxMap[key]; !ok {
			idxMap[key] = &IndexInfo{
				TableName: tableName,
				IndexName: idxName,
				IsUnique:  strings.EqualFold(uniqueness, "UNIQUE"),
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

func (r *DaMengReader) GetForeignKeys(ctx context.Context) ([]FKInfo, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT c.CONSTRAINT_NAME,
		        c.TABLE_NAME,
		        cc.COLUMN_NAME,
		        rc.TABLE_NAME AS REF_TABLE,
		        rcc.COLUMN_NAME AS REF_COLUMN,
		        c.DELETE_RULE,
		        'NO ACTION' AS UPDATE_RULE
		 FROM ALL_CONSTRAINTS c
		 JOIN ALL_CONS_COLUMNS cc
		   ON c.OWNER = cc.OWNER AND c.CONSTRAINT_NAME = cc.CONSTRAINT_NAME
		 JOIN ALL_CONSTRAINTS rc
		   ON c.R_OWNER = rc.OWNER AND c.R_CONSTRAINT_NAME = rc.CONSTRAINT_NAME
		 JOIN ALL_CONS_COLUMNS rcc
		   ON rc.OWNER = rcc.OWNER AND rc.CONSTRAINT_NAME = rcc.CONSTRAINT_NAME
		      AND cc.POSITION = rcc.POSITION
		 WHERE c.CONSTRAINT_TYPE = 'R'
		   AND c.OWNER = ?
		 ORDER BY c.CONSTRAINT_NAME, cc.POSITION`,
		r.schema)
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

func (r *DaMengReader) GetViews(ctx context.Context) ([]ViewInfo, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT VIEW_NAME,
		        lower(DBMS_METADATA.GET_DDL('VIEW', UPPER(VIEW_NAME), UPPER(?))) AS view_ddl
		 FROM ALL_VIEWS
		 WHERE OWNER = UPPER(?)
		 ORDER BY VIEW_NAME`,
		r.schema, r.schema)
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
		v.Definition = transformDaMengViewDef(v.Definition)
		views = append(views, v)
	}
	return views, rows.Err()
}

// transformDaMengViewDef 将达梦视图定义转换为 PostgreSQL 兼容语法：
// 1. 剥离 CREATE [OR REPLACE] VIEW ... AS 头部，只保留 SELECT 体
// 2. 去掉表别名上的双引号（"A".col → A.col），避免 PG 大小写敏感问题
// 3. 函数名转换：ifnull → coalesce，nvl → coalesce
func transformDaMengViewDef(def string) string {
	// 剥离 CREATE [OR REPLACE] VIEW <name> AS
	// 视图名可能带 schema 前缀，如 "admin"."view_name"，用非贪婪 .*? 匹配
	reHeader := regexp.MustCompile(`(?is)^\s*create\s+(or\s+replace\s+)?view\s+.*?\bas\b\s+`)
	def = reHeader.ReplaceAllString(def, "")
	def = strings.TrimSpace(def)
	// 去掉结尾分号
	def = strings.TrimRight(def, "; \t\n\r")

	// "ALIAS".col → ALIAS.col（别名带双引号时 PG 区分大小写，去掉引号让 PG 折叠为小写）
	reQuotedAlias := regexp.MustCompile(`"([A-Za-z_][A-Za-z0-9_]*)"\."`)
	def = reQuotedAlias.ReplaceAllString(def, `$1."`)

	// ifnull(x, y) → coalesce(x, y)
	def = strings.ReplaceAll(def, "ifnull(", "coalesce(")
	// isnull(x, y) → coalesce(x, y)
	def = strings.ReplaceAll(def, "isnull(", "coalesce(")
	// nvl(x, y) → coalesce(x, y)
	def = strings.ReplaceAll(def, "nvl(", "coalesce(")

	return def
}

func (r *DaMengReader) GetTriggerCount(ctx context.Context) (int, error) {
	var count int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ALL_TRIGGERS WHERE OWNER = ?`,
		r.schema,
	).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}

func (r *DaMengReader) CountRows(ctx context.Context, table string) (int64, error) {
	var count int64
	err := r.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT COUNT(*) FROM "%s"."%s"`, r.schema, table),
	).Scan(&count)
	return count, err
}

// GetComments 返回所有表注释和列注释信息(原始大小写)。
func (r *DaMengReader) GetComments(ctx context.Context) ([]CommentInfo, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT TABLE_NAME, '' AS COLUMN_NAME, COMMENTS
		 FROM ALL_TAB_COMMENTS
		 WHERE OWNER = ? AND TABLE_TYPE = 'TABLE' AND COMMENTS IS NOT NULL
		 UNION ALL
		 SELECT TABLE_NAME, COLUMN_NAME, COMMENTS
		 FROM ALL_COL_COMMENTS
		 WHERE OWNER = ? AND COMMENTS IS NOT NULL
		 ORDER BY TABLE_NAME, COLUMN_NAME`, r.schema, r.schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var comments []CommentInfo
	for rows.Next() {
		var cm CommentInfo
		// 达梦兼容 Oracle 语法，'' 字面量同样会被当作 NULL（表注释分支用 '' AS COLUMN_NAME 占位），
		// 直接 Scan 进 string 会报 "converting NULL to string is unsupported"，用 NullString 兜底。
		var colName sql.NullString
		if err := rows.Scan(&cm.TableName, &colName, &cm.Comment); err != nil {
			return nil, err
		}
		cm.ColumnName = colName.String
		comments = append(comments, cm)
	}
	return comments, rows.Err()
}
