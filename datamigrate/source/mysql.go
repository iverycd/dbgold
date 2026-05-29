// datamigrate/source/mysql.go
package source

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"

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
	// 连接池配置：迁移并发读取需要足够的连接复用，避免每次查询重建连接
	db.SetMaxOpenConns(50)
	db.SetMaxIdleConns(25)
	db.SetConnMaxLifetime(time.Hour)
	if err := db.Ping(); err != nil {
		return nil, err
	}
	return &MySQLReader{db: db, dbName: dbName}, nil
}

func (r *MySQLReader) Close() error   { return r.db.Close() }
func (r *MySQLReader) DBType() string { return "mysql" }

func (r *MySQLReader) ListDatabases(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT SCHEMA_NAME FROM information_schema.SCHEMATA
		 WHERE SCHEMA_NAME NOT IN ('information_schema','mysql','performance_schema','sys')
		 ORDER BY SCHEMA_NAME`)
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

func (r *MySQLReader) GetPrimaryKey(ctx context.Context, table string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT COLUMN_NAME FROM information_schema.KEY_COLUMN_USAGE
		 WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND CONSTRAINT_NAME = 'PRIMARY'
		 ORDER BY ORDINAL_POSITION`, r.dbName, table)
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

func (r *MySQLReader) GetPrimaryKeys(ctx context.Context) ([]IndexInfo, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT TABLE_NAME, COLUMN_NAME
		 FROM information_schema.KEY_COLUMN_USAGE
		 WHERE TABLE_SCHEMA = ? AND CONSTRAINT_NAME = 'PRIMARY'
		 ORDER BY TABLE_NAME, ORDINAL_POSITION`, r.dbName)
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

func (r *MySQLReader) ReadPage(ctx context.Context, table string, pkCols []string, offset, limit int64) ([]string, [][]interface{}, error) {
	var query string
	if len(pkCols) > 0 {
		pkList := strings.Join(pkCols, ", ")
		joinConds := make([]string, len(pkCols))
		for i, col := range pkCols {
			joinConds[i] = fmt.Sprintf("temp.%s = t.%s", col, col)
		}
		query = fmt.Sprintf(
			`SELECT t.* FROM (SELECT %s FROM %s ORDER BY %s LIMIT %d, %d) temp LEFT JOIN %s t ON %s`,
			pkList, table, pkList, offset, limit, table, strings.Join(joinConds, " AND "))
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
	colTypes, err := rows.ColumnTypes()
	if err != nil {
		return nil, nil, err
	}
	// 预计算每列的类型名（大写），用于后续按类型分支处理
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
			return nil, nil, err
		}
		for i, v := range vals {
			b, ok := v.([]byte)
			if !ok {
				continue
			}
			dt := colTypeName[i]
			switch {
			case strings.Contains(dt, "BLOB") || strings.Contains(dt, "BINARY"):
				// 二进制列保持 []byte，pq 正确写入 bytea
			case dt == "BIT":
				// MySQL BIT 转 16 进制后截掉首字符，得到 "0"/"1"，符合 PG bit(1) 格式
				vals[i] = hex.EncodeToString(b)[1:]
			case dt == "GEOMETRY":
				// GIS 类型：16 进制字符串，去掉 golang 多出的前 8 个 0
				vals[i] = hex.EncodeToString(b)[8:]
			default:
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
	// 在 MySQL 端做基础清理：去反引号、去 schema 前缀、去 convert/using utf8mb4
	rows, err := r.db.QueryContext(ctx,
		`SELECT table_name,
		        replace(replace(replace(replace(VIEW_DEFINITION,
		            '`+"`"+`',''),
		            concat(table_schema,'.'),''),
		            'convert(',''),
		            'using utf8mb4)','') AS view_def
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
		v.Definition = transformViewDef(v.Definition)
		if v.ViewName == "view_portal_myitem" {
			v.Definition = regexp.MustCompile(`(?i)\(portal_item\.DISABLED\s*=\s*0\)`).
				ReplaceAllString(v.Definition, "(portal_item.DISABLED = '0')")
		}
		if v.ViewName == "view_cns_jxkh_rule_item" {
			v.Definition = regexp.MustCompile(`(?i)\(b\.ISUSED\s*=\s*1\)`).
				ReplaceAllString(v.Definition, "(b.ISUSED = '1')")
		}
		views = append(views, v)
	}
	return views, rows.Err()
}

// transformViewDef 将 MySQL 视图定义改写为 PostgreSQL 兼容语法
func transformViewDef(def string) string {
	// 1. 处理 MySQL FROM 子句中的嵌套括号和隐式 CROSS JOIN 语法
	def = rewriteFromParen(def)
	// ifnull → coalesce
	def = regexp.MustCompile(`(?i)\bifnull\s*\(`).ReplaceAllString(def, "coalesce(")
	// isnull(x) → (x IS NULL)
	def = regexp.MustCompile(`(?i)\bisnull\s*\(([^)]+)\)`).ReplaceAllString(def, "($1 IS NULL)")
	// group_concat(x separator 'sep') → string_agg(x, 'sep')
	def = regexp.MustCompile(`(?i)\bgroup_concat\s*\((.+?)\s+separator\s+('[^']*')\s*\)`).ReplaceAllString(def, "string_agg($1, $2)")
	def = regexp.MustCompile(`(?i)\bgroup_concat\s*\(`).ReplaceAllString(def, "string_agg(")
	// date_format → to_char，替换常见格式符
	def = regexp.MustCompile(`(?i)\bdate_format\s*\(`).ReplaceAllString(def, "to_char(")
	def = strings.NewReplacer("%Y", "YYYY", "%m", "MM", "%d", "DD", "%H", "HH24", "%i", "MI", "%s", "SS").Replace(def)
	// if(cond, a, b) → CASE WHEN cond THEN a ELSE b END
	def = regexp.MustCompile(`(?i)\bif\s*\(([^,]+),\s*([^,]+),\s*([^)]+)\)`).ReplaceAllString(def, "CASE WHEN $1 THEN $2 ELSE $3 END")
	// 去掉 CHARSET 子句
	def = regexp.MustCompile(`(?i)\s+charset\s+\w+`).ReplaceAllString(def, "")
	return def
}

func isWordChar(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
}

func stripOuterParens(s string) string {
	s = strings.TrimSpace(s)
	if len(s) == 0 || s[0] != '(' {
		return s
	}
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				if i == len(s)-1 {
					return strings.TrimSpace(s[1 : len(s)-1])
				}
				return s
			}
		}
	}
	return s
}

func fullyStripOuterParens(s string) string {
	for {
		stripped := stripOuterParens(s)
		if stripped == s {
			return s
		}
		s = stripped
	}
}

type joinPart struct {
	raw      string
	joinType string
}

func splitTopLevelJoin(s string) []joinPart {
	upper := strings.ToUpper(s)
	depth := 0
	start := 0
	var parts []joinPart
	pendingJoinType := ""
	i := 0
	for i < len(s) {
		ch := s[i]
		if ch == '(' {
			depth++
			i++
			continue
		}
		if ch == ')' {
			depth--
			i++
			continue
		}
		if depth == 0 && i+4 <= len(upper) && upper[i:i+4] == "JOIN" {
			if i > 0 && isWordChar(s[i-1]) {
				i++
				continue
			}
			if i+4 < len(s) && isWordChar(s[i+4]) {
				i++
				continue
			}
			seg := strings.TrimSpace(s[start:i])
			jt := ""
			for _, q := range []string{"LEFT OUTER", "RIGHT OUTER", "FULL OUTER", "LEFT", "RIGHT", "INNER", "CROSS", "FULL"} {
				segUpper := strings.ToUpper(seg)
				if strings.HasSuffix(segUpper, " "+q) || segUpper == q {
					seg = strings.TrimSpace(seg[:len(seg)-len(q)])
					jt = q
					break
				}
			}
			parts = append(parts, joinPart{raw: seg, joinType: pendingJoinType})
			pendingJoinType = jt
			i += 4
			start = i
			continue
		}
		i++
	}
	parts = append(parts, joinPart{raw: strings.TrimSpace(s[start:]), joinType: pendingJoinType})
	return parts
}

func findTopLevelKeyword(s, kw string) int {
	upper := strings.ToUpper(s)
	kwUpper := strings.ToUpper(kw)
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
		}
		if depth == 0 && i+len(kw) <= len(s) && upper[i:i+len(kwUpper)] == kwUpper {
			if i > 0 && isWordChar(s[i-1]) {
				continue
			}
			if i+len(kw) < len(s) && isWordChar(s[i+len(kw)]) {
				continue
			}
			return i
		}
	}
	return -1
}

func buildFromClause(parts []joinPart) string {
	if len(parts) == 0 {
		return ""
	}
	var resolveSegment func(seg string) string
	resolveSegment = func(seg string) string {
		seg = strings.TrimSpace(seg)
		stripped := stripOuterParens(seg)
		if stripped == seg {
			return seg
		}
		subParts := splitTopLevelJoin(stripped)
		return buildFromClause(subParts)
	}
	result := resolveSegment(parts[0].raw)
	for _, p := range parts[1:] {
		raw := strings.TrimSpace(p.raw)
		jt := p.joinType
		if jt == "" {
			jt = "INNER"
		}
		strippedRaw := stripOuterParens(raw)
		if strippedRaw != raw {
			sub := resolveSegment(raw)
			result += " " + sub
			continue
		}
		onIdx := findTopLevelKeyword(raw, "ON")
		usingIdx := findTopLevelKeyword(raw, "USING")
		switch {
		case onIdx >= 0:
			tableRef := strings.TrimSpace(raw[:onIdx])
			onExpr := strings.TrimSpace(raw[onIdx+2:])
			result += " " + jt + " JOIN " + tableRef + " ON " + onExpr
		case usingIdx >= 0:
			tableRef := strings.TrimSpace(raw[:usingIdx])
			usingExpr := strings.TrimSpace(raw[usingIdx+5:])
			result += " " + jt + " JOIN " + tableRef + " USING " + usingExpr
		default:
			result += " CROSS JOIN " + resolveSegment(raw)
		}
	}
	return result
}

func rewriteFromParen(def string) string {
	reFindFrom := regexp.MustCompile(`(?i)\bFROM\s*\(`)
	reStartsWithSelect := regexp.MustCompile(`(?i)^SELECT\b`)
	pos := 0
	for pos < len(def) {
		loc := reFindFrom.FindStringIndex(def[pos:])
		if loc == nil {
			return def
		}
		absFromStart := pos + loc[0]
		openIdx := pos + loc[1] - 1
		depth := 0
		closeIdx := -1
		for i := openIdx; i < len(def); i++ {
			switch def[i] {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					closeIdx = i
				}
			}
			if closeIdx >= 0 {
				break
			}
		}
		if closeIdx < 0 {
			return def
		}
		inner := def[openIdx+1 : closeIdx]
		trimmed := strings.TrimSpace(fullyStripOuterParens(inner))
		if reStartsWithSelect.MatchString(trimmed) {
			pos = closeIdx + 1
			continue
		}
		parts := splitTopLevelJoin(trimmed)
		rewritten := buildFromClause(parts)
		before := def[:absFromStart]
		after := def[closeIdx+1:]
		def = before + "FROM " + rewritten + after
		pos = absFromStart
	}
	return def
}

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

// CountRows 返回指定表的行数
func (r *MySQLReader) CountRows(ctx context.Context, table string) (int64, error) {
	var count int64
	err := r.db.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM `%s`", table)).Scan(&count)
	return count, err
}
