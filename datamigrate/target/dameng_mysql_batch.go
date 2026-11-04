package target

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"

	"dbgold/datamigrate/valueconv"
)

const (
	mysqlDMBatchRows  = 1000
	mysqlDMBatchBytes = 4 << 20
	// v1.8.23 switches LOB values above 2048 bytes to off-row binding.
	mysqlDMInlineBytes = 2048
)

func (w *DaMengWriter) PoolStats() sql.DBStats { return w.db.Stats() }

// BatchWriteStats counts attempted MySQL array executions and scalar row executions.
func (w *DaMengWriter) BatchWriteStats() (bulkExecs, scalarRows int64) {
	return w.mysqlDMBulkExecs.Load(), w.mysqlDMScalarRows.Load()
}

// CopyMySQLBatchData is opt-in for ordinary MySQL table copies. Each page is
// one transaction, even when a page contains several array and scalar writes.
func (w *DaMengWriter) CopyMySQLBatchData(ctx context.Context, table string, cols, colTypes []string, rows [][]any) error {
	if w.srcType != "mysql" {
		return fmt.Errorf("达梦数组批写仅支持 MySQL 源库")
	}
	if len(rows) > 0 && (len(cols) == 0 || len(colTypes) != len(cols)) {
		return fmt.Errorf("表 %s 缺少源列类型", table)
	}
	return w.copyData(ctx, table, cols, colTypes, rows, true)
}

// Both writer modes use the same MySQL value converter and date expressions.
func (w *DaMengWriter) mysqlInsertRow(table string, cols, types []string, row []any) ([]any, error) {
	if len(row) != len(cols) {
		return nil, fmt.Errorf("表 %s 行列数不匹配", table)
	}
	if len(types) != len(cols) {
		return nil, fmt.Errorf("表 %s 缺少源列类型", table)
	}
	out := w.convertRow(row, types)
	for i, dt := range types {
		switch strings.ToUpper(dt) {
		case "DATE", "DATETIME", "TIMESTAMP":
			v, err := valueconv.MySQLDamengDate(row[i], dt)
			if err != nil {
				return nil, fmt.Errorf("%s.%s: %w", table, cols[i], err)
			}
			out[i] = v
		}
	}
	return out, nil
}

// The array path does not call the driver's scalar value converter per cell.
// Unknown shapes, large values and uint64 outside int64 keep scalar semantics.
// The first row determines the binds for a whole array. A leading empty/null
// value can leave an expression untyped for the following rows.
func mysqlDMRowShape(row []any) (size int, canBatch, canStart bool) {
	size, canBatch, canStart = 16*len(row), true, true
	for _, v := range row {
		switch x := v.(type) {
		case nil:
			canStart = false
		case string:
			size += len(x)
			canBatch = canBatch && len(x) <= mysqlDMInlineBytes
			canStart = canStart && x != ""
		case []byte:
			size += len(x)
			canBatch = canBatch && len(x) <= mysqlDMInlineBytes
			canStart = canStart && len(x) > 0
		case uint64:
			canBatch = canBatch && x <= math.MaxInt64
		case int64, float32, float64:
			// Native MySQL numeric values are accepted directly by the driver.
		default:
			canBatch = false
		}
	}
	return size, canBatch, canStart
}

func (w *DaMengWriter) writeMySQLBatches(ctx context.Context, tx *sql.Tx, insert, table string, cols, types []string, rows [][]any) error {
	var stmt *sql.Stmt
	var scalarRows int64
	defer func() {
		if scalarRows > 0 {
			w.mysqlDMScalarRows.Add(scalarRows)
		}
		if stmt != nil {
			_ = stmt.Close()
		}
	}()
	writeScalar := func(row []any, index int) error {
		if stmt == nil {
			var err error
			stmt, err = tx.PrepareContext(ctx, insert)
			if err != nil {
				return err
			}
		}
		scalarRows++
		if _, err := stmt.ExecContext(ctx, row...); err != nil {
			return fmt.Errorf("表 %s 页内第 %d 行: %w", table, index+1, err)
		}
		return nil
	}

	batch := make([][]any, 0, min(len(rows), mysqlDMBatchRows))
	start, bytes := 0, 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if len(batch) == 1 {
			if err := writeScalar(batch[0], start); err != nil {
				return err
			}
		} else {
			// A single [][]any argument is the DM connection's array-binding
			// protocol. sql.Stmt would enforce one argument per placeholder.
			w.mysqlDMBulkExecs.Add(1)
			result, err := tx.ExecContext(ctx, insert, batch)
			if err != nil {
				return fmt.Errorf("表 %s 页内第 %d–%d 行: %w", table, start+1, start+len(batch), err)
			}
			n, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("表 %s 页内第 %d–%d 行读取影响行数: %w", table, start+1, start+len(batch), err)
			}
			if n != int64(len(batch)) {
				return fmt.Errorf("表 %s 页内第 %d–%d 行批写仅影响 %d 行，预期 %d 行", table, start+1, start+len(batch), n, len(batch))
			}
		}
		clear(batch)
		batch = batch[:0]
		bytes = 0
		return nil
	}
	for i, row := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		out, err := w.mysqlInsertRow(table, cols, types, row)
		if err != nil {
			return fmt.Errorf("页内第 %d 行: %w", i+1, err)
		}
		size, eligible, starter := mysqlDMRowShape(out)
		if !eligible || len(batch) == mysqlDMBatchRows || bytes+size > mysqlDMBatchBytes {
			if err = flush(); err != nil {
				return err
			}
		}
		if !eligible || size > mysqlDMBatchBytes || (len(batch) == 0 && !starter) {
			if err = writeScalar(out, i); err != nil {
				return err
			}
			continue
		}
		if len(batch) == 0 {
			start = i
		}
		batch = append(batch, out)
		bytes += size
	}
	return flush()
}
