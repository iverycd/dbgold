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

type mysqlDamengIndexTable struct{ schema, table string }

type mysqlDamengIndexDefinition struct {
	columns    []string
	descending []bool
	unique     bool
}

type mysqlDamengIndexSnapshot struct {
	tables map[mysqlDamengIndexTable][]mysqlDamengIndexDefinition
	err    error
}

type mysqlDamengIndexCache struct {
	created   map[mysqlDamengIndexTable][]mysqlDamengIndexDefinition
	snapshots map[string]mysqlDamengIndexSnapshot
}

func (w *DaMengWriter) resetMySQLDamengIndexCache() {
	w.mysqlDMIndexesMu.Lock()
	w.mysqlDMIndexes = mysqlDamengIndexCache{}
	w.mysqlDMIndexesMu.Unlock()
}

// Keep confirmed CREATE results independent of the catalog snapshot. A later
// CREATE must remain visible even when the snapshot is older or failed to load.
func (w *DaMengWriter) recordMySQLDamengIndex(idx source.IndexInfo) {
	if len(idx.Columns) == 0 {
		return
	}
	key := mysqlDamengIndexTable{strings.ToUpper(w.schema), strings.ToUpper(idx.TableName)}
	definition := mysqlDamengTargetIndex(idx)
	w.mysqlDMIndexesMu.Lock()
	defer w.mysqlDMIndexesMu.Unlock()
	if w.mysqlDMIndexes.created == nil {
		w.mysqlDMIndexes.created = map[mysqlDamengIndexTable][]mysqlDamengIndexDefinition{}
	}
	w.mysqlDMIndexes.created[key] = append(w.mysqlDMIndexes.created[key], definition)
}

func mysqlDamengTargetIndex(idx source.IndexInfo) mysqlDamengIndexDefinition {
	definition := mysqlDamengIndexDefinition{
		columns:    make([]string, len(idx.Columns)),
		descending: make([]bool, len(idx.Columns)),
		unique:     idx.IsUnique || idx.IsPrimary,
	}
	for i, column := range idx.Columns {
		definition.columns[i] = strings.ToUpper(column)
		// ADD PRIMARY KEY does not emit the source's per-column directions.
		definition.descending[i] = !idx.IsPrimary && i < len(idx.Descending) && idx.Descending[i]
	}
	return definition
}

func mysqlDamengIndexCovered(want mysqlDamengIndexDefinition, candidates []mysqlDamengIndexDefinition) bool {
	for _, candidate := range candidates {
		if (!want.unique || candidate.unique) && slices.Equal(candidate.columns, want.columns) && slices.Equal(candidate.descending, want.descending) {
			return true
		}
	}
	return false
}

// A duplicate column list can be supplied by a primary key or an index with a
// different name. Only non-primary indexes may reuse verified target coverage.
func (w *DaMengWriter) createMySQLDamengIndex(ctx context.Context, idx source.IndexInfo) error {
	for _, statement := range w.dia.IndexStatements(w.schema, idx) {
		if err := repair.Observe(ctx, statement.SQL, "running", nil); err != nil {
			return fmt.Errorf("保存 DDL 执行前状态: %w", err)
		}
		_, err := w.db.ExecContext(ctx, statement.SQL)
		status := "succeeded"
		if err == nil {
			// Persistence can fail after CREATE commits. Retain the actual target
			// evidence before notifying the observer about the successful DDL.
			w.recordMySQLDamengIndex(idx)
		}
		var dmErr *dm.DmError
		if !idx.IsPrimary && errors.As(err, &dmErr) && dmErr.ErrCode == -3236 {
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

// Read each compatibility view once per schema and combine the results in Go.
// Joining or sorting these expanded views in DM can be very expensive.
const mysqlDamengIndexesSQL = `SELECT OWNER,INDEX_NAME,TABLE_OWNER,TABLE_NAME,UNIQUENESS,STATUS,INDEX_TYPE
FROM ALL_INDEXES
WHERE TABLE_OWNER=?`

const mysqlDamengIndexColumnsSQL = `SELECT INDEX_OWNER,INDEX_NAME,TABLE_OWNER,TABLE_NAME,COLUMN_NAME,COLUMN_POSITION,DESCEND
FROM ALL_IND_COLUMNS
WHERE TABLE_OWNER=?`

func (w *DaMengWriter) verifyMySQLDamengIndexCoverage(ctx context.Context, idx source.IndexInfo) error {
	if len(idx.Columns) == 0 {
		return fmt.Errorf("缺少可核验的索引列")
	}
	if idx.Method != "" && !strings.EqualFold(idx.Method, "BTREE") {
		return fmt.Errorf("源索引类型 %s 不能按普通 B 树索引复用", idx.Method)
	}
	key := mysqlDamengIndexTable{strings.ToUpper(w.schema), strings.ToUpper(idx.TableName)}
	want := mysqlDamengTargetIndex(idx)
	w.mysqlDMIndexesMu.Lock()
	defer w.mysqlDMIndexesMu.Unlock()
	if mysqlDamengIndexCovered(want, w.mysqlDMIndexes.created[key]) {
		return nil
	}
	if w.mysqlDMIndexes.snapshots == nil {
		w.mysqlDMIndexes.snapshots = map[string]mysqlDamengIndexSnapshot{}
	}
	snapshot, loaded := w.mysqlDMIndexes.snapshots[key.schema]
	if !loaded {
		// Serialize loads as well as cache publication. Failed loads are also
		// remembered for this run, so thousands of duplicates cannot retry one
		// expensive or unavailable catalog query thousands of times.
		snapshot.tables, snapshot.err = w.loadMySQLDamengIndexes(ctx, key.schema)
		w.mysqlDMIndexes.snapshots[key.schema] = snapshot
	}
	if snapshot.err != nil {
		return snapshot.err
	}
	if mysqlDamengIndexCovered(want, snapshot.tables[key]) {
		return nil
	}
	return fmt.Errorf("未找到同表、键列顺序、排序方向及唯一性兼容的有效 B 树索引")
}

func (w *DaMengWriter) loadMySQLDamengIndexes(ctx context.Context, schema string) (map[mysqlDamengIndexTable][]mysqlDamengIndexDefinition, error) {
	type catalogIndexKey struct{ owner, name, tableOwner, table string }
	type catalogColumn struct {
		name       string
		position   int
		descending bool
	}
	type catalogIndex struct {
		columns []catalogColumn
		unique  bool
		valid   bool
	}
	indexes := map[catalogIndexKey]*catalogIndex{}
	rows, err := w.db.QueryContext(ctx, mysqlDamengIndexesSQL, schema)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var key catalogIndexKey
		var uniqueness, status, kind string
		if err := rows.Scan(&key.owner, &key.name, &key.tableOwner, &key.table, &uniqueness, &status, &kind); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if existing, duplicate := indexes[key]; duplicate {
			existing.valid = false
			continue
		}
		indexes[key] = &catalogIndex{
			unique: uniqueness == "UNIQUE",
			valid: key.tableOwner == schema && status == "VALID" && (kind == "NORMAL" || kind == "CLUSTER") &&
				(uniqueness == "UNIQUE" || uniqueness == "NONUNIQUE"),
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	rows, err = w.db.QueryContext(ctx, mysqlDamengIndexColumnsSQL, schema)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var key catalogIndexKey
		var column, direction sql.NullString
		var position int
		if err := rows.Scan(&key.owner, &key.name, &key.tableOwner, &key.table, &column, &position, &direction); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if index := indexes[key]; index != nil {
			index.valid = index.valid && column.Valid && direction.Valid && (direction.String == "ASC" || direction.String == "DESC")
			index.columns = append(index.columns, catalogColumn{column.String, position, direction.String == "DESC"})
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	// Publish nothing until both result sets have been read completely.
	tables := map[mysqlDamengIndexTable][]mysqlDamengIndexDefinition{}
	for key, index := range indexes {
		if !index.valid || len(index.columns) == 0 {
			continue
		}
		slices.SortFunc(index.columns, func(a, b catalogColumn) int {
			if a.position < b.position {
				return -1
			}
			if a.position > b.position {
				return 1
			}
			return 0
		})
		definition := mysqlDamengIndexDefinition{unique: index.unique}
		for i, column := range index.columns {
			if column.position != i+1 {
				index.valid = false
				break
			}
			definition.columns = append(definition.columns, column.name)
			definition.descending = append(definition.descending, column.descending)
		}
		if index.valid {
			table := mysqlDamengIndexTable{key.tableOwner, key.table}
			tables[table] = append(tables[table], definition)
		}
	}
	return tables, nil
}
