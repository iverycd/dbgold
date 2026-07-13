package source

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	_ "github.com/sijms/go-ora/v2"
)

// OracleReader 实现 Reader 接口，连接到 Oracle 数据库
type OracleReader struct {
	db      *sql.DB
	owner   string // 大写，对应连接配置的 Database 字段（Oracle 中等同于 schema/用户名）
	version int    // Oracle 主版本号，用于分页语法选择（12+ 支持 OFFSET...FETCH）
}

// NewOracle 创建并连接 Oracle Reader
// dsn 格式：oracle://username:password@host:port/service
// owner 为要迁移的 schema（Oracle 用户名），存储为大写
func NewOracle(dsn, owner string, pool ConnPoolConfig) (*OracleReader, error) {
	db, err := sql.Open("oracle", dsn)
	if err != nil {
		return nil, err
	}
	pool.applyTo(db)
	if err := db.Ping(); err != nil {
		return nil, err
	}

	r := &OracleReader{db: db, owner: strings.ToUpper(owner)}

	// 查询 Oracle 主版本号，决定分页语法
	var verStr string
	if err := db.QueryRow(`SELECT VERSION FROM V$INSTANCE`).Scan(&verStr); err == nil {
		// 版本格式如 "11.2.0.4.0" 或 "19.0.0.0.0"
		parts := strings.SplitN(verStr, ".", 2)
		if len(parts) > 0 {
			r.version, _ = strconv.Atoi(parts[0])
		}
	}
	if r.version == 0 {
		r.version = 12 // 查询失败时默认使用现代语法
	}

	return r, nil
}

func (r *OracleReader) Close() error   { return r.db.Close() }
func (r *OracleReader) DBType() string { return "oracle" }

func (r *OracleReader) ListDatabases(ctx context.Context) ([]string, error) {
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

func (r *OracleReader) ListTables(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT TABLE_NAME FROM ALL_TABLES WHERE OWNER = :1 ORDER BY TABLE_NAME`,
		r.owner)
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

func (r *OracleReader) GetTableDDLInfo(ctx context.Context, table string) (*TableDDLInfo, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT COLUMN_NAME,
		        DATA_TYPE,
		        NVL(CHAR_LENGTH, 0),
		        NVL(DATA_PRECISION, 0),
		        NVL(DATA_SCALE, 0),
		        NULLABLE,
		        DATA_DEFAULT
		 FROM ALL_TAB_COLUMNS
		 WHERE OWNER = :1 AND TABLE_NAME = :2
		 ORDER BY COLUMN_ID`,
		r.owner, table)
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
		if err := rows.Scan(&col.Name, &col.DataType, &charLen, &numPrec, &numScale,
			&nullable, &defaultVal); err != nil {
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
		info.Columns = append(info.Columns, col)
	}
	return info, rows.Err()
}

func (r *OracleReader) GetPrimaryKey(ctx context.Context, table string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT cc.COLUMN_NAME
		 FROM ALL_CONSTRAINTS c
		 JOIN ALL_CONS_COLUMNS cc
		   ON c.OWNER = cc.OWNER AND c.CONSTRAINT_NAME = cc.CONSTRAINT_NAME
		 WHERE c.CONSTRAINT_TYPE = 'P'
		   AND c.OWNER = :1 AND c.TABLE_NAME = :2
		 ORDER BY cc.POSITION`,
		r.owner, table)
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

func (r *OracleReader) GetPrimaryKeys(ctx context.Context) ([]IndexInfo, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT cc.TABLE_NAME, cc.COLUMN_NAME
		 FROM ALL_CONSTRAINTS c
		 JOIN ALL_CONS_COLUMNS cc
		   ON c.OWNER = cc.OWNER AND c.CONSTRAINT_NAME = cc.CONSTRAINT_NAME
		 WHERE c.CONSTRAINT_TYPE = 'P'
		   AND c.OWNER = :1
		 ORDER BY cc.TABLE_NAME, cc.POSITION`,
		r.owner)
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

func (r *OracleReader) ReadPage(ctx context.Context, table string, pkCols []string, offset, limit int64) ([]string, []string, [][]interface{}, error) {
	var query string
	if r.version >= 12 {
		// 12c+ 支持 OFFSET...FETCH NEXT 语法
		if len(pkCols) > 0 {
			pkList := make([]string, len(pkCols))
			for i, col := range pkCols {
				pkList[i] = fmt.Sprintf(`"%s"`, col)
			}
			orderBy := strings.Join(pkList, ", ")
			query = fmt.Sprintf(
				`SELECT * FROM "%s"."%s" ORDER BY %s OFFSET %d ROWS FETCH NEXT %d ROWS ONLY`,
				r.owner, table, orderBy, offset, limit)
		} else {
			query = fmt.Sprintf(
				`SELECT * FROM "%s"."%s" OFFSET %d ROWS FETCH NEXT %d ROWS ONLY`,
				r.owner, table, offset, limit)
		}
	} else {
		// 11g 及以下：双层 ROWNUM 嵌套
		endRow := offset + limit
		if len(pkCols) > 0 {
			pkList := make([]string, len(pkCols))
			for i, col := range pkCols {
				pkList[i] = fmt.Sprintf(`"%s"`, col)
			}
			orderBy := strings.Join(pkList, ", ")
			query = fmt.Sprintf(
				`SELECT * FROM (SELECT A.*, ROWNUM RNcolumn FROM (SELECT * FROM "%s"."%s" ORDER BY %s) A WHERE ROWNUM <= %d) WHERE RNcolumn > %d`,
				r.owner, table, orderBy, endRow, offset)
		} else {
			query = fmt.Sprintf(
				`SELECT * FROM (SELECT A.*, ROWNUM RNcolumn FROM (SELECT * FROM "%s"."%s") A WHERE ROWNUM <= %d) WHERE RNcolumn > %d`,
				r.owner, table, endRow, offset)
		}
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

	// 11g 分页时去掉末尾追加的 RNCOLUMN 伪列
	if r.version < 12 && len(cols) > 0 && strings.ToUpper(cols[len(cols)-1]) == "RNCOLUMN" {
		cols = cols[:len(cols)-1]
		colTypes = colTypes[:len(colTypes)-1]
	}

	colTypeName := make([]string, len(colTypes))
	for i, ct := range colTypes {
		colTypeName[i] = strings.ToUpper(ct.DatabaseTypeName())
	}

	var result [][]interface{}
	for rows.Next() {
		// 11g 模式：scan 全部列（含 RNCOLUMN），再截断
		scanCount := len(colTypes)
		if r.version < 12 {
			scanCount = len(cols) + 1 // 多一个 RNCOLUMN
		}
		vals := make([]interface{}, scanCount)
		ptrs := make([]interface{}, scanCount)
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, nil, nil, err
		}
		// 截断 RNCOLUMN
		vals = vals[:len(cols)]

		for i, v := range vals {
			if v == nil {
				continue
			}
			if i >= len(colTypeName) {
				continue
			}
			dt := colTypeName[i]
			switch dt {
			case "CHAR", "VARCHAR", "VARCHAR2", "NCHAR", "NVARCHAR2", "CLOB", "NCLOB", "LONG":
				if b, ok := v.([]byte); ok {
					vals[i] = strings.ReplaceAll(string(b), "\x00", "")
				} else if s, ok := v.(string); ok {
					vals[i] = strings.ReplaceAll(s, "\x00", "")
				}
				// DATE/TIMESTAMP 保持 time.Time(中立值),由目标 ValueConverter 格式化;
				// BLOB, RAW, LONG RAW: 保持 []byte
			}
		}
		result = append(result, vals)
	}
	return cols, colTypeName, result, rows.Err()
}

func (r *OracleReader) GetSequences(ctx context.Context) ([]SequenceInfo, error) {
	// Oracle 自增通过"触发器 + 序列"模式实现：
	// BEFORE EACH ROW 触发器中调用 SEQ.NEXTVAL 赋值给 :NEW.col
	rows, err := r.db.QueryContext(ctx,
		`SELECT TABLE_NAME, TRIGGER_BODY FROM ALL_TRIGGERS
		 WHERE OWNER = :1 AND UPPER(TRIGGER_TYPE) LIKE '%BEFORE EACH ROW%'`,
		r.owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	reSeq := regexp.MustCompile(`(?i)SELECT\s+(\S+?)\.NEXTVAL\s+INTO\s+:NEW\.`)
	reCol := regexp.MustCompile(`(?i):NEW\.(\w+)`)

	type seqRef struct {
		tableName string
		seqName   string
		colName   string
	}
	var refs []seqRef

	for rows.Next() {
		var tableName, triggerBody string
		if err := rows.Scan(&tableName, &triggerBody); err != nil {
			return nil, err
		}
		body := strings.ToUpper(triggerBody)
		body = strings.ReplaceAll(body, "INTO:", "INTO :")

		matchSeq := reSeq.FindStringSubmatch(body)
		if len(matchSeq) < 2 {
			continue
		}
		seqName := matchSeq[1]

		matchCol := reCol.FindStringSubmatch(body)
		if len(matchCol) < 2 {
			continue
		}
		colName := matchCol[1]

		refs = append(refs, seqRef{
			tableName: tableName,
			seqName:   strings.Trim(seqName, `"`),
			colName:   colName,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var seqs []SequenceInfo
	for _, ref := range refs {
		var lastNumber sql.NullInt64
		_ = r.db.QueryRowContext(ctx,
			`SELECT LAST_NUMBER FROM ALL_SEQUENCES
			 WHERE SEQUENCE_OWNER = :1 AND SEQUENCE_NAME = :2`,
			r.owner, ref.seqName,
		).Scan(&lastNumber)

		startVal := int64(1)
		if lastNumber.Valid && lastNumber.Int64 > 1 {
			startVal = lastNumber.Int64
		}
		seqs = append(seqs, SequenceInfo{
			TableName:  ref.tableName,
			ColumnName: ref.colName,
			StartValue: startVal,
		})
	}
	return seqs, nil
}

func (r *OracleReader) GetIndexes(ctx context.Context) ([]IndexInfo, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT i.TABLE_NAME, i.INDEX_NAME, ic.COLUMN_NAME, i.UNIQUENESS
		 FROM ALL_INDEXES i
		 JOIN ALL_IND_COLUMNS ic
		   ON i.OWNER = ic.INDEX_OWNER AND i.INDEX_NAME = ic.INDEX_NAME
		 WHERE i.OWNER = :1
		   AND NOT EXISTS (
		       SELECT 1 FROM ALL_CONSTRAINTS c
		       WHERE c.OWNER = i.OWNER AND c.INDEX_NAME = i.INDEX_NAME
		         AND c.CONSTRAINT_TYPE = 'P'
		   )
		 ORDER BY i.TABLE_NAME, i.INDEX_NAME, ic.COLUMN_POSITION`,
		r.owner)
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

func (r *OracleReader) GetForeignKeys(ctx context.Context) ([]FKInfo, error) {
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
		   AND c.OWNER = :1
		 ORDER BY c.CONSTRAINT_NAME, cc.POSITION`,
		r.owner)
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

func (r *OracleReader) GetViews(ctx context.Context) ([]ViewInfo, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT lower(VIEW_NAME) VIEW_NAME, TEXT FROM ALL_VIEWS
		 WHERE OWNER = :1
		 ORDER BY VIEW_NAME`,
		r.owner)
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
		v.Definition = transformOracleViewDef(strings.ToLower(v.Definition))
		views = append(views, v)
	}
	return views, rows.Err()
}

// transformOracleViewDef 将 Oracle 视图 SELECT 体转换为 PostgreSQL 兼容语法。
// ALL_VIEWS.TEXT 已是纯 SELECT 语句，无需剥离 CREATE VIEW 头。
func transformOracleViewDef(def string) string {
	def = strings.TrimSpace(def)
	def = strings.TrimRight(def, "; \t\n\r")

	// 去掉表别名双引号（"ALIAS".col → ALIAS.col）
	reQuotedAlias := regexp.MustCompile(`"([A-Za-z_][A-Za-z0-9_]*)"\."`)
	def = reQuotedAlias.ReplaceAllString(def, `$1."`)

	// Oracle 伪列 rowid → ctid::text（PostgreSQL 最接近的行标识符）
	reRowid := regexp.MustCompile(`(?i)\browid\b`)
	def = reRowid.ReplaceAllString(def, "ctid::text")

	// FROM DUAL / , DUAL → 去掉（PostgreSQL 不需要 DUAL 表）
	reDual := regexp.MustCompile(`(?i),?\s*from\s+(?:\w+\.)?dual\b`)
	def = reDual.ReplaceAllString(def, "")

	// NVL(x, y) → COALESCE(x, y)（大小写不敏感）
	reNVL := regexp.MustCompile(`(?i)\bnvl\s*\(`)
	def = reNVL.ReplaceAllString(def, "coalesce(")

	return def
}

func (r *OracleReader) GetTriggerCount(ctx context.Context) (int, error) {
	var count int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ALL_TRIGGERS WHERE OWNER = :1`,
		r.owner,
	).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}

func (r *OracleReader) CountRows(ctx context.Context, table string) (int64, error) {
	var count int64
	err := r.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT COUNT(*) FROM "%s"."%s"`, r.owner, table),
	).Scan(&count)
	return count, err
}

// GetComments 返回所有表注释和列注释信息(原始大小写)。
func (r *OracleReader) GetComments(ctx context.Context) ([]CommentInfo, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT TABLE_NAME, '' AS COLUMN_NAME, COMMENTS
		 FROM ALL_TAB_COMMENTS
		 WHERE OWNER = :1 AND TABLE_TYPE = 'TABLE' AND COMMENTS IS NOT NULL AND COMMENTS <> ''
		 UNION ALL
		 SELECT TABLE_NAME, COLUMN_NAME, COMMENTS
		 FROM ALL_COL_COMMENTS
		 WHERE OWNER = :2 AND COMMENTS IS NOT NULL AND COMMENTS <> ''
		 ORDER BY TABLE_NAME, COLUMN_NAME`, r.owner, r.owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var comments []CommentInfo
	for rows.Next() {
		var cm CommentInfo
		if err := rows.Scan(&cm.TableName, &cm.ColumnName, &cm.Comment); err != nil {
			return nil, err
		}
		comments = append(comments, cm)
	}
	return comments, rows.Err()
}
