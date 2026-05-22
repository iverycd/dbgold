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

func (r *MySQLReader) Close() error   { return r.db.Close() }
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

func (r *MySQLReader) ReadPage(ctx context.Context, table, pkCol string, offset, limit int64) ([]string, [][]interface{}, error) {
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
