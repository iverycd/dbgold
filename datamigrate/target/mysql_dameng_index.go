package target

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"gitee.com/chunanyong/dm"
)

// A duplicate column list can be supplied by a primary key or an index with a
// different name. Reuse only after checking the actual target definition.
func (w *DaMengWriter) createMySQLDamengIndex(ctx context.Context, idx source.IndexInfo) error {
	for _, statement := range w.dia.IndexStatements(w.schema, idx) {
		if err := repair.Observe(ctx, statement.SQL, "running", nil); err != nil {
			return fmt.Errorf("保存 DDL 执行前状态: %w", err)
		}
		_, err := w.db.ExecContext(ctx, statement.SQL)
		status := "succeeded"
		var dmErr *dm.DmError
		if errors.As(err, &dmErr) && dmErr.ErrCode == -3236 {
			if verifyErr := w.verifyMySQLDamengIndexCoverage(ctx, idx); verifyErr != nil {
				err = errors.Join(err, fmt.Errorf("核验已有达梦索引失败: %w", verifyErr))
			} else {
				err, status = nil, "reused"
			}
		}
		if err != nil {
			status = repairErrorStatus(err)
		}
		if persistErr := repair.Observe(ctx, statement.SQL, status, err); persistErr != nil {
			return errors.Join(err, fmt.Errorf("保存 DDL 执行结果失败: %w", persistErr))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

const mysqlDamengIndexCoverageSQL = `SELECT i.OWNER,i.INDEX_NAME,i.UNIQUENESS,i.STATUS,i.INDEX_TYPE,ic.COLUMN_NAME,ic.COLUMN_POSITION,ic.DESCEND
FROM ALL_INDEXES i
JOIN ALL_IND_COLUMNS ic ON ic.INDEX_OWNER=i.OWNER AND ic.INDEX_NAME=i.INDEX_NAME
WHERE i.TABLE_OWNER=? AND i.TABLE_NAME=?
ORDER BY i.OWNER,i.INDEX_NAME,ic.COLUMN_POSITION`

func (w *DaMengWriter) verifyMySQLDamengIndexCoverage(ctx context.Context, idx source.IndexInfo) error {
	if len(idx.Columns) == 0 {
		return fmt.Errorf("缺少可核验的索引列")
	}
	if idx.Method != "" && !strings.EqualFold(idx.Method, "BTREE") {
		return fmt.Errorf("源索引类型 %s 不能按普通 B 树索引复用", idx.Method)
	}
	rows, err := w.db.QueryContext(ctx, mysqlDamengIndexCoverageSQL, strings.ToUpper(w.schema), strings.ToUpper(idx.TableName))
	if err != nil {
		return err
	}
	defer rows.Close()
	type indexKey struct{ owner, name string }
	type candidate struct {
		columns    []string
		descending []bool
		uniqueness string
		valid      bool
	}
	indexes := map[indexKey]*candidate{}
	for rows.Next() {
		var owner, name, uniqueness, status, kind string
		var column, direction sql.NullString
		var position int
		if err := rows.Scan(&owner, &name, &uniqueness, &status, &kind, &column, &position, &direction); err != nil {
			return err
		}
		key := indexKey{owner, name}
		index, ok := indexes[key]
		if !ok {
			index = &candidate{uniqueness: uniqueness, valid: true}
			indexes[key] = index
		}
		index.valid = index.valid && status == "VALID" && (kind == "NORMAL" || kind == "CLUSTER") &&
			(uniqueness == "UNIQUE" || uniqueness == "NONUNIQUE") && uniqueness == index.uniqueness &&
			column.Valid && direction.Valid && (direction.String == "ASC" || direction.String == "DESC") && position == len(index.columns)+1
		index.columns = append(index.columns, column.String)
		index.descending = append(index.descending, direction.String == "DESC")
	}
	if err := rows.Err(); err != nil {
		return err
	}
	columns := make([]string, len(idx.Columns))
	descending := make([]bool, len(idx.Columns))
	for i, name := range idx.Columns {
		columns[i] = strings.ToUpper(name)
		descending[i] = i < len(idx.Descending) && idx.Descending[i]
	}
	for _, index := range indexes {
		if index.valid && (!idx.IsUnique || index.uniqueness == "UNIQUE") &&
			slices.Equal(index.columns, columns) && slices.Equal(index.descending, descending) {
			return nil
		}
	}
	return fmt.Errorf("未找到同表、键列顺序、排序方向及唯一性兼容的有效 B 树索引")
}
