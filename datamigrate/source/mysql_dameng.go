package source

import (
	"context"
	"database/sql"
	"strings"

	mysql "github.com/go-sql-driver/mysql"
)

// NewMySQLForDameng preserves raw date values and reads complete index key
// metadata without changing other targets' MySQL readers.
func NewMySQLForDameng(dsn, database string, pool ConnPoolConfig) (*MySQLReader, error) {
	dsn, err := mysqlDamengDSN(dsn)
	if err != nil {
		return nil, err
	}
	r, err := NewMySQL(dsn, database, pool)
	if err != nil {
		return nil, err
	}
	r.damengTarget = true
	return r, nil
}

func mysqlDamengDSN(dsn string) (string, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return "", err
	}
	cfg.ParseTime = false
	return cfg.FormatDSN(), nil
}

// damengIndexes keeps directions so index equivalence checks cannot confuse
// different key definitions. Unsupported indexes never expose partial keys.
func (r *MySQLReader) damengIndexes(ctx context.Context) ([]IndexInfo, error) {
	rows, err := r.queryContext(ctx, `SELECT TABLE_NAME, INDEX_NAME, COLUMN_NAME, NON_UNIQUE, SUB_PART, INDEX_TYPE, COLLATION
 FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = ? AND INDEX_NAME != 'PRIMARY'
 ORDER BY TABLE_NAME, INDEX_NAME, SEQ_IN_INDEX`, r.dbName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type indexKey struct{ table, name string }
	indexMap := map[indexKey]int{}
	var indexes []IndexInfo
	for rows.Next() {
		var table, name, method string
		var column, collation sql.NullString
		var prefix sql.NullInt64
		var nonUnique int
		if err := rows.Scan(&table, &name, &column, &nonUnique, &prefix, &method, &collation); err != nil {
			return nil, err
		}
		key := indexKey{table: table, name: name}
		i, ok := indexMap[key]
		if !ok {
			i = len(indexes)
			indexMap[key] = i
			indexes = append(indexes, IndexInfo{TableName: table, IndexName: name, IsUnique: nonUnique == 0, Method: method})
		}
		idx := &indexes[i]
		var reason string
		switch {
		case !column.Valid:
			reason = "函数/表达式索引"
		case prefix.Valid:
			reason = "前缀索引"
		case strings.EqualFold(method, "FULLTEXT"):
			// FULLTEXT keys have no B-tree ordering; COLLATION is normally NULL.
		case !strings.EqualFold(method, "BTREE"):
			reason = "空间或其他非 BTREE 索引"
		case !strings.EqualFold(collation.String, "A") && !strings.EqualFold(collation.String, "D"):
			reason = "排序方向未知的索引"
		}
		if reason != "" {
			if idx.Unsupported == "" {
				idx.Unsupported = "暂不支持 MySQL " + reason + "的等价转换，请根据源端定义手工适配"
			}
			idx.Columns, idx.Descending = nil, nil
		} else if idx.Unsupported == "" {
			idx.Columns = append(idx.Columns, column.String)
			if !strings.EqualFold(method, "FULLTEXT") {
				idx.Descending = append(idx.Descending, strings.EqualFold(collation.String, "D"))
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Validate only after collecting all keys, so a composite FULLTEXT index
	// cannot accidentally be migrated as a usable single-column definition.
	for i := range indexes {
		idx := &indexes[i]
		if strings.EqualFold(idx.Method, "FULLTEXT") && idx.Unsupported == "" && len(idx.Columns) > 1 {
			idx.Unsupported = "暂不支持 MySQL 多列全文索引的自动转换，达梦 CONTEXT INDEX 仅支持单个文本列，请根据源端定义手工适配"
			idx.Columns, idx.Descending = nil, nil
		}
	}
	return indexes, nil
}
