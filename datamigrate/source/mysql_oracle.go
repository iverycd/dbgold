package source

import (
	"context"
	"database/sql"
	"fmt"
	"math/big"
	"strings"

	mysql "github.com/go-sql-driver/mysql"
)

// NewMySQLForOracle is deliberately separate: parseTime normalizes partial
// zero dates, and the legacy reader removes NUL bytes for PostgreSQL targets.
func NewMySQLForOracle(dsn, database string, pool ConnPoolConfig) (*MySQLReader, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	cfg.ParseTime = false
	r, err := NewMySQL(cfg.FormatDSN(), database, pool)
	if err != nil {
		return nil, err
	}
	r.oracleTarget = true
	return r, nil
}

func (r *MySQLReader) SourceSchema() string { return r.dbName }

func (r *MySQLReader) oracleTableInfo(ctx context.Context, table string) (*TableDDLInfo, error) {
	rows, err := r.queryContext(ctx, `SELECT COLUMN_NAME, DATA_TYPE, CHARACTER_MAXIMUM_LENGTH,
 NUMERIC_PRECISION, NUMERIC_SCALE, IS_NULLABLE, COLUMN_DEFAULT, EXTRA,
 COLUMN_TYPE, DATETIME_PRECISION, GENERATION_EXPRESSION
 FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? ORDER BY ORDINAL_POSITION`, r.dbName, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	info := &TableDDLInfo{TableName: table}
	for rows.Next() {
		var c ColumnInfo
		var length, precision, scale, fsp sql.NullInt64
		var nullable string
		var def, generated sql.NullString
		if err = rows.Scan(&c.Name, &c.DataType, &length, &precision, &scale, &nullable, &def, &c.Extra, &c.FormattedType, &fsp, &generated); err != nil {
			return nil, err
		}
		c.Length, c.Precision, c.Scale, c.DateTimePrecision = length.Int64, precision.Int64, scale.Int64, fsp.Int64
		c.IsNullable, c.Generated = nullable == "YES", generated.String
		if def.Valid {
			v := def.String
			c.Default = &v
		}
		c.Ordinal = len(info.Columns) + 1
		info.Columns = append(info.Columns, c)
	}
	return info, rows.Err()
}

func mysqlOracleIdent(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }

func (r *MySQLReader) readOraclePage(ctx context.Context, table string, pks []string, offset, limit int64) ([]string, []string, [][]interface{}, error) {
	q := "SELECT * FROM " + mysqlOracleIdent(r.dbName) + "." + mysqlOracleIdent(table)
	if len(pks) > 0 {
		keys := make([]string, len(pks))
		for i, k := range pks {
			keys[i] = mysqlOracleIdent(k)
		}
		q += " ORDER BY " + strings.Join(keys, ", ")
	}
	rows, err := r.queryContext(ctx, q+" LIMIT ? OFFSET ?", limit, offset)
	if err != nil {
		return nil, nil, nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, nil, nil, err
	}
	cts, err := rows.ColumnTypes()
	if err != nil {
		return nil, nil, nil, err
	}
	types := make([]string, len(cts))
	for i, ct := range cts {
		types[i] = ct.DatabaseTypeName()
	}
	var out [][]interface{}
	for rows.Next() {
		vals, dest := make([]interface{}, len(cols)), make([]interface{}, len(cols))
		for i := range vals {
			dest[i] = &vals[i]
		}
		if err = rows.Scan(dest...); err != nil {
			return nil, nil, nil, err
		}
		// Preserve bytes, including NUL, partial zero dates and large decimals.
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				vals[i] = append([]byte{}, b...)
			}
		}
		out = append(out, vals)
	}
	return cols, types, out, rows.Err()
}

func (r *MySQLReader) oracleSequences(ctx context.Context) ([]SequenceInfo, error) {
	rows, err := r.queryContext(ctx, `SELECT TABLE_NAME, AUTO_INCREMENT FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND AUTO_INCREMENT IS NOT NULL`, r.dbName)
	if err != nil {
		return nil, err
	}
	starts := map[string]string{}
	for rows.Next() {
		var table, text string
		if err = rows.Scan(&table, &text); err != nil {
			rows.Close()
			return nil, err
		}
		if n, ok := new(big.Int).SetString(text, 10); !ok || n.Sign() < 0 {
			rows.Close()
			return nil, fmt.Errorf("表 %s 自增起始值无效", table)
		}
		starts[table] = text
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = r.queryContext(ctx, `SELECT TABLE_NAME, COLUMN_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND EXTRA LIKE '%auto_increment%' ORDER BY TABLE_NAME, ORDINAL_POSITION`, r.dbName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SequenceInfo
	for rows.Next() {
		var s SequenceInfo
		if err = rows.Scan(&s.TableName, &s.ColumnName); err != nil {
			return nil, err
		}
		s.StartValueText = starts[s.TableName]
		if s.StartValueText == "" {
			s.StartValueText = "1"
		}
		n, _ := new(big.Int).SetString(s.StartValueText, 10)
		if n.IsInt64() {
			s.StartValue = n.Int64()
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *MySQLReader) oracleIndexes(ctx context.Context) ([]IndexInfo, error) {
	rows, err := r.queryContext(ctx, `SELECT TABLE_NAME, INDEX_NAME, COLUMN_NAME, NON_UNIQUE, SUB_PART, INDEX_TYPE, COLLATION
 FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = ? AND INDEX_NAME != 'PRIMARY' ORDER BY TABLE_NAME, INDEX_NAME, SEQ_IN_INDEX`, r.dbName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IndexInfo
	indexes := map[string]int{}
	for rows.Next() {
		var table, name, method string
		var col, coll sql.NullString
		var prefix sql.NullInt64
		var nonUnique int
		if err = rows.Scan(&table, &name, &col, &nonUnique, &prefix, &method, &coll); err != nil {
			return nil, err
		}
		key := table + "\x00" + name
		i, ok := indexes[key]
		if !ok {
			i = len(out)
			indexes[key] = i
			out = append(out, IndexInfo{TableName: table, IndexName: name, IsUnique: nonUnique == 0, Method: method})
		}
		if !col.Valid || prefix.Valid || method != "BTREE" || coll.String == "D" {
			out[i].Unsupported = "Oracle 暂不支持 MySQL 函数、前缀、全文、空间或降序索引的等价转换"
		}
		out[i].Columns = append(out[i].Columns, col.String)
	}
	return out, rows.Err()
}

func (r *MySQLReader) oracleForeignKeys(ctx context.Context) ([]FKInfo, error) {
	rows, err := r.queryContext(ctx, `SELECT k.TABLE_NAME, k.CONSTRAINT_NAME, k.COLUMN_NAME, k.REFERENCED_TABLE_SCHEMA,
 k.REFERENCED_TABLE_NAME, k.REFERENCED_COLUMN_NAME, r.DELETE_RULE, r.UPDATE_RULE
 FROM information_schema.KEY_COLUMN_USAGE k JOIN information_schema.REFERENTIAL_CONSTRAINTS r
 ON r.CONSTRAINT_SCHEMA=k.CONSTRAINT_SCHEMA AND r.CONSTRAINT_NAME=k.CONSTRAINT_NAME AND r.TABLE_NAME=k.TABLE_NAME
 WHERE k.TABLE_SCHEMA=? AND k.REFERENCED_TABLE_NAME IS NOT NULL ORDER BY k.TABLE_NAME,k.CONSTRAINT_NAME,k.ORDINAL_POSITION`, r.dbName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FKInfo
	indexes := map[string]int{}
	for rows.Next() {
		var f FKInfo
		var col, refCol string
		if err = rows.Scan(&f.TableName, &f.ConstraintName, &col, &f.RefSchema, &f.RefTable, &refCol, &f.OnDelete, &f.OnUpdate); err != nil {
			return nil, err
		}
		key := f.TableName + "\x00" + f.ConstraintName
		i, ok := indexes[key]
		if !ok {
			i = len(out)
			indexes[key] = i
			if f.RefSchema != r.dbName {
				f.Unsupported = "Oracle 暂不支持跨 MySQL database 外键"
			}
			out = append(out, f)
		}
		out[i].Columns = append(out[i].Columns, col)
		out[i].RefColumns = append(out[i].RefColumns, refCol)
	}
	return out, rows.Err()
}

func (r *MySQLReader) GetViewsForTarget(ctx context.Context, target string) ([]ViewInfo, error) {
	if target != "oracle" && target != "oscar" {
		return r.GetViews(ctx)
	}
	rows, err := r.queryContext(ctx, `SELECT TABLE_NAME, VIEW_DEFINITION FROM information_schema.VIEWS WHERE TABLE_SCHEMA = ? ORDER BY TABLE_NAME`, r.dbName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ViewInfo
	for rows.Next() {
		var v ViewInfo
		var def sql.NullString
		if err = rows.Scan(&v.ViewName, &def); err != nil {
			return nil, err
		}
		if !def.Valid || target == "oscar" && strings.TrimSpace(def.String) == "" {
			return nil, fmt.Errorf("视图 %s 定义不可读，请检查 SHOW VIEW 权限", v.ViewName)
		}
		v.Definition = def.String
		if target == "oscar" {
			v.Definition = transformMySQLViewForOscar(v.ViewName, v.Definition)
		}
		out = append(out, v)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if target == "oscar" {
		return out, nil
	}
	for i := range out {
		info, err := r.oracleTableInfo(ctx, out[i].ViewName)
		if err != nil {
			return nil, err
		}
		out[i].Columns = info.Columns
	}
	return out, nil
}
