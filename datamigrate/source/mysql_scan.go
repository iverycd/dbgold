package source

import (
	"context"
	"database/sql"
	"fmt"
	"math/big"
	"strings"
)

// MySQLScanRange selects an inclusive interval on a single integer primary key.
// Decimal strings preserve both ends of the BIGINT UNSIGNED domain.
type MySQLScanRange struct {
	Key   string
	Lower string
	Upper string
}

// MySQLScanRanges returns no ranges when the table should use a single stream.
func (r *MySQLReader) MySQLScanRanges(ctx context.Context, table string, parts int) ([]MySQLScanRange, error) {
	if parts <= 1 || r.snapshotTx != nil {
		return nil, nil
	}
	// Count catalog entries, not table rows. A composite key with just one
	// integer component must not be mistaken for a single-column integer key.
	const primaryKey = `SELECT MAX(k.COLUMN_NAME), MAX(c.DATA_TYPE)
FROM information_schema.KEY_COLUMN_USAGE k
JOIN information_schema.COLUMNS c ON c.TABLE_SCHEMA=k.TABLE_SCHEMA AND c.TABLE_NAME=k.TABLE_NAME AND c.COLUMN_NAME=k.COLUMN_NAME
WHERE k.TABLE_SCHEMA=? AND k.TABLE_NAME=? AND k.CONSTRAINT_NAME='PRIMARY'
HAVING COUNT(*)=1`
	var key, dataType string
	err := r.queryRowContext(ctx, primaryKey, r.dbName, table).Scan(&key, &dataType)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(dataType) {
	case "tinyint", "smallint", "mediumint", "int", "bigint":
	default:
		return nil, nil
	}
	quotedKey := mysqlOracleIdent(key)
	bounds := "SELECT MIN(" + quotedKey + "), MAX(" + quotedKey + ") FROM " + mysqlOracleIdent(r.dbName) + "." + mysqlOracleIdent(table)
	var first, last sql.NullString
	if err := r.queryRowContext(ctx, bounds).Scan(&first, &last); err != nil {
		return nil, err
	}
	if !first.Valid && !last.Valid {
		return nil, nil
	}
	low, okLow := new(big.Int).SetString(first.String, 10)
	high, okHigh := new(big.Int).SetString(last.String, 10)
	if !okLow || !okHigh || low.Cmp(high) > 0 {
		return nil, fmt.Errorf("MySQL 整数主键 %s.%s 的范围无效", table, key)
	}
	one := big.NewInt(1)
	width := new(big.Int).Sub(high, low)
	width.Add(width, one)
	// ceil(width/parts) caps the number of ranges even when keys have gaps.
	width.Add(width, big.NewInt(int64(parts-1)))
	width.Quo(width, big.NewInt(int64(parts)))
	var spans []MySQLScanRange
	for low.Cmp(high) <= 0 {
		end := new(big.Int).Add(low, width)
		end.Sub(end, one)
		if end.Cmp(high) > 0 {
			end.Set(high)
		}
		spans = append(spans, MySQLScanRange{Key: key, Lower: low.String(), Upper: end.String()})
		low.Add(end, one)
	}
	return spans, nil
}

// StreamMySQLTable reads an ordinary table (or one range) from a single result
// set. It deliberately uses the ordinary reader's target-specific conversions.
func (r *MySQLReader) StreamMySQLTable(ctx context.Context, table string, span *MySQLScanRange, batchSize int, consume func([]string, []string, [][]interface{}) error) error {
	if batchSize <= 0 || consume == nil {
		return fmt.Errorf("MySQL 流式读取需要正数批次大小和回调")
	}
	query := "SELECT * FROM " + mysqlOracleIdent(r.dbName) + "." + mysqlOracleIdent(table)
	if span != nil {
		low, okLow := new(big.Int).SetString(span.Lower, 10)
		high, okHigh := new(big.Int).SetString(span.Upper, 10)
		if span.Key == "" || !okLow || !okHigh || low.Cmp(high) > 0 {
			return fmt.Errorf("MySQL 表 %s 的读取范围无效", table)
		}
		// Exact numeric literals avoid MySQL comparing large integers with
		// string parameters as doubles and losing precision above 2^53.
		query += " WHERE " + mysqlOracleIdent(span.Key) + " BETWEEN " + low.String() + " AND " + high.String()
	}
	normalize := normalizeMySQLValues
	var rows *sql.Rows
	var err error
	if r.oracleTarget {
		// readOraclePage uses a parameterized SELECT. Preserve its binary
		// protocol representations of unsigned integers and temporal values.
		prepare := r.db.PrepareContext
		if r.snapshotTx != nil {
			prepare = r.snapshotTx.PrepareContext
		}
		stmt, e := prepare(ctx, query)
		if e != nil {
			return e
		}
		defer stmt.Close()
		rows, err = stmt.QueryContext(ctx)
		normalize = nil
	} else {
		rows, err = r.queryContext(ctx, query)
	}
	if err != nil {
		return err
	}
	defer rows.Close()
	format := streamRowFormat{preserveTypeCase: r.oracleTarget, emptyNilBytes: r.oracleTarget}
	return streamSQLBatches(ctx, rows, batchSize, nil, format, normalize, consume)
}
