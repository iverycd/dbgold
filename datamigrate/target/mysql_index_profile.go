package target

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// MySQLIndexProfile contains only the target capabilities needed to size an
// InnoDB index. Inspecting it does not impose session modes or version policy.
type MySQLIndexProfile struct {
	PageSize    int
	LargePrefix bool
}

// KeyLimit is the total InnoDB key budget in bytes, including composite keys.
// Pages larger than 16 KiB do not increase the 3072-byte MySQL key limit.
func (p MySQLIndexProfile) KeyLimit(rowFormat string) int {
	if p.PageSize <= 0 {
		return 0
	}
	limit := 3072
	if p.PageSize < 16384 {
		limit = p.PageSize * 3 / 16
	}
	format := strings.ToUpper(strings.TrimSpace(rowFormat))
	if !p.LargePrefix || format == "COMPACT" || format == "REDUNDANT" {
		limit = min(limit, 767)
	}
	return limit
}

// MySQLIndexColumn describes the actual target declaration. DataType retains
// COLUMN_TYPE (including length/precision), rather than a source type mapping.
// Length is in characters for text and in bytes for binary string columns.
type MySQLIndexColumn struct {
	Name              string
	DataType          string
	Length            int64
	Precision         int64
	Scale             int64
	DateTimePrecision int64
	MaxBytes          int
}

type MySQLIndexTable struct {
	Engine    string
	RowFormat string
	Columns   []MySQLIndexColumn
}

// InspectMySQLIndexProfile deliberately avoids the broader PG preflight, which
// also enforces version certification and SQL mode requirements.
func (w *MySQLWriter) InspectMySQLIndexProfile(ctx context.Context) (MySQLIndexProfile, error) {
	p := MySQLIndexProfile{LargePrefix: true}
	if err := w.db.QueryRowContext(ctx, `SELECT @@innodb_page_size`).Scan(&p.PageSize); err != nil {
		return p, fmt.Errorf("读取目标 MySQL 索引页大小: %w", err)
	}
	switch p.PageSize {
	case 4096, 8192, 16384, 32768, 65536:
	default:
		return p, fmt.Errorf("目标 MySQL innodb_page_size=%d 无效", p.PageSize)
	}
	// The variable was removed in MySQL 8.0, where large prefixes are always
	// enabled. SHOW safely returns no rows when it is absent.
	rows, err := w.db.QueryContext(ctx, `SHOW VARIABLES LIKE 'innodb_large_prefix'`)
	if err != nil {
		return p, fmt.Errorf("读取目标 MySQL 索引前缀设置: %w", err)
	}
	defer rows.Close()
	seen := false
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return p, fmt.Errorf("读取目标 MySQL 索引前缀设置: %w", err)
		}
		if seen || !strings.EqualFold(name, "innodb_large_prefix") {
			return p, fmt.Errorf("目标 MySQL 索引前缀设置元数据不完整")
		}
		seen = true
		switch strings.ToUpper(strings.TrimSpace(value)) {
		case "ON", "1":
			p.LargePrefix = true
		case "OFF", "0":
			p.LargePrefix = false
		default:
			return p, fmt.Errorf("目标 MySQL innodb_large_prefix=%q 无效", value)
		}
	}
	if err := rows.Err(); err != nil {
		return p, fmt.Errorf("读取目标 MySQL 索引前缀设置: %w", err)
	}
	return p, nil
}

const mysqlIndexTableQuery = `SELECT ENGINE,ROW_FORMAT
 FROM information_schema.TABLES
 WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND TABLE_TYPE='BASE TABLE'`

const mysqlIndexColumnsQuery = `SELECT c.COLUMN_NAME,c.COLUMN_TYPE,c.CHARACTER_MAXIMUM_LENGTH,
 c.NUMERIC_PRECISION,c.NUMERIC_SCALE,c.DATETIME_PRECISION,s.MAXLEN
 FROM information_schema.COLUMNS c
 LEFT JOIN information_schema.CHARACTER_SETS s ON s.CHARACTER_SET_NAME=c.CHARACTER_SET_NAME
 WHERE c.TABLE_SCHEMA=? AND c.TABLE_NAME=?
 ORDER BY c.ORDINAL_POSITION`

// InspectMySQLIndexTable reads existing target metadata for an object-only
// migration. All identifiers remain query arguments, including unusual names.
func (w *MySQLWriter) InspectMySQLIndexTable(ctx context.Context, table string) (MySQLIndexTable, error) {
	result := MySQLIndexTable{}
	if table == "" {
		return result, fmt.Errorf("读取目标 MySQL 索引列缺少表名")
	}
	schema, err := w.OracleMySQLSchema(ctx)
	if err != nil {
		return result, err
	}
	var engine, rowFormat sql.NullString
	if err := w.db.QueryRowContext(ctx, mysqlIndexTableQuery, schema, table).Scan(&engine, &rowFormat); err != nil {
		return result, fmt.Errorf("读取目标 MySQL 表 %s.%s 的索引配置: %w", schema, table, err)
	}
	if !engine.Valid || strings.TrimSpace(engine.String) == "" || !rowFormat.Valid || strings.TrimSpace(rowFormat.String) == "" {
		return result, fmt.Errorf("目标 MySQL 表 %s.%s 的引擎或行格式元数据不完整", schema, table)
	}
	result.Engine, result.RowFormat = engine.String, rowFormat.String
	// Prefix planning is limited to InnoDB. Preserve the existing execution
	// path for other engines without requiring unrelated column metadata.
	if !strings.EqualFold(result.Engine, "InnoDB") {
		return result, nil
	}
	rows, err := w.db.QueryContext(ctx, mysqlIndexColumnsQuery, schema, table)
	if err != nil {
		return result, fmt.Errorf("读取目标 MySQL 表 %s.%s 的索引列: %w", schema, table, err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var c MySQLIndexColumn
		var length, precision, scale, datetime, maxBytes sql.NullInt64
		if err := rows.Scan(&c.Name, &c.DataType, &length, &precision, &scale, &datetime, &maxBytes); err != nil {
			return result, fmt.Errorf("读取目标 MySQL 表 %s.%s 的索引列: %w", schema, table, err)
		}
		key := strings.ToLower(c.Name)
		if c.Name == "" || strings.TrimSpace(c.DataType) == "" || seen[key] {
			return result, fmt.Errorf("目标 MySQL 表 %s.%s 的索引列名或类型元数据不完整", schema, table)
		}
		seen[key] = true
		c.Length, c.Precision, c.Scale, c.DateTimePrecision = length.Int64, precision.Int64, scale.Int64, datetime.Int64
		base := strings.ToLower(strings.TrimSpace(c.DataType))
		if end := strings.IndexAny(base, "( "); end >= 0 {
			base = base[:end]
		}
		switch base {
		case "char", "varchar", "tinytext", "text", "mediumtext", "longtext":
			if !length.Valid || length.Int64 < 0 || !maxBytes.Valid || maxBytes.Int64 < 1 || maxBytes.Int64 > 16 {
				return result, fmt.Errorf("目标 MySQL 表 %s.%s 列 %s 的字符长度或字符集元数据不完整", schema, table, c.Name)
			}
			c.MaxBytes = int(maxBytes.Int64)
		case "binary", "varbinary", "tinyblob", "blob", "mediumblob", "longblob":
			if !length.Valid || length.Int64 < 0 {
				return result, fmt.Errorf("目标 MySQL 表 %s.%s 列 %s 的二进制长度元数据不完整", schema, table, c.Name)
			}
			c.MaxBytes = 1
		case "decimal", "numeric":
			if !precision.Valid || precision.Int64 < 1 || !scale.Valid || scale.Int64 < 0 || scale.Int64 > precision.Int64 {
				return result, fmt.Errorf("目标 MySQL 表 %s.%s 列 %s 的数值精度元数据不完整", schema, table, c.Name)
			}
		}
		result.Columns = append(result.Columns, c)
	}
	if err := rows.Err(); err != nil {
		return result, fmt.Errorf("读取目标 MySQL 表 %s.%s 的索引列: %w", schema, table, err)
	}
	if len(result.Columns) == 0 {
		return result, fmt.Errorf("目标 MySQL 表 %s.%s 缺少索引列元数据", schema, table)
	}
	return result, nil
}
