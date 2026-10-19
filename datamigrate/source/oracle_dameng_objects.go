package source

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

func (r *OracleDamengReader) GetViews(ctx context.Context) ([]ViewInfo, error) {
	return r.GetViewsForTarget(ctx, "dameng")
}

// The Oracle SELECT body is the source of truth. In particular, the inherited
// reader's PostgreSQL rewrites alter DUAL/ROWID and lowercase string literals.
func (r *OracleDamengReader) GetViewsForTarget(ctx context.Context, targetType string) ([]ViewInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if targetType != "dameng" {
		return r.OracleReader.GetViewsForTarget(ctx, targetType)
	}
	rows, err := r.db.QueryContext(ctx, `SELECT VIEW_NAME,TEXT FROM ALL_VIEWS WHERE OWNER=:1 ORDER BY VIEW_NAME`, r.owner)
	if err != nil {
		return nil, err
	}
	var out []ViewInfo
	for rows.Next() {
		var view ViewInfo
		if err = rows.Scan(&view.ViewName, &view.Definition); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, view)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	// ALL_VIEWS.TEXT contains only SELECT, so an explicit CREATE VIEW (column,
	// ...) header cannot be reconstructed from projection aliases. Release the
	// first result set before querying columns, including with a one-slot pool.
	for i := range out {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		columns, err := r.db.QueryContext(ctx, `SELECT COLUMN_NAME,DATA_TYPE,NVL(DATA_SCALE,0) FROM ALL_TAB_COLUMNS WHERE OWNER=:1 AND TABLE_NAME=:2 ORDER BY COLUMN_ID`, r.owner, out[i].ViewName)
		if err != nil {
			return nil, err
		}
		for columns.Next() {
			var column ColumnInfo
			if err = columns.Scan(&column.Name, &column.DataType, &column.Scale); err != nil {
				columns.Close()
				return nil, err
			}
			if strings.HasPrefix(strings.ToUpper(column.DataType), "TIMESTAMP") {
				column.DateTimePrecision = column.Scale
			}
			out[i].Columns = append(out[i].Columns, column)
		}
		err = columns.Err()
		columns.Close()
		if err != nil {
			return nil, err
		}
		if len(out[i].Columns) == 0 {
			return nil, fmt.Errorf("视图 %s 的列元数据不可见", out[i].ViewName)
		}
	}
	return out, nil
}

func (r *OracleDamengReader) GetIndexes(ctx context.Context) ([]IndexInfo, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT i.TABLE_NAME, i.INDEX_NAME, ic.COLUMN_NAME,
 i.UNIQUENESS, ic.DESCEND, i.INDEX_TYPE, i.STATUS
 FROM ALL_INDEXES i JOIN ALL_IND_COLUMNS ic
 ON i.OWNER=ic.INDEX_OWNER AND i.INDEX_NAME=ic.INDEX_NAME
 WHERE i.OWNER=:1 AND NOT EXISTS (SELECT 1 FROM ALL_CONSTRAINTS c
 WHERE c.OWNER=i.OWNER AND c.INDEX_NAME=i.INDEX_NAME AND c.CONSTRAINT_TYPE='P')
 ORDER BY i.TABLE_NAME,i.INDEX_NAME,ic.COLUMN_POSITION`, r.owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []IndexInfo{}
	positions := map[string]int{}
	for rows.Next() {
		var table, name, unique, direction, kind, status string
		var column sql.NullString
		if err = rows.Scan(&table, &name, &column, &unique, &direction, &kind, &status); err != nil {
			return nil, err
		}
		key := table + "\x00" + name
		pos, ok := positions[key]
		if !ok {
			pos = len(out)
			positions[key] = pos
			out = append(out, IndexInfo{TableName: table, IndexName: name, IsUnique: unique == "UNIQUE", Method: kind})
		}
		idx := &out[pos]
		idx.Columns = append(idx.Columns, column.String)
		idx.Descending = append(idx.Descending, direction == "DESC")
		if kind != "NORMAL" || !column.Valid || strings.HasPrefix(column.String, "SYS_NC") {
			idx.Unsupported = fmt.Sprintf("Oracle 索引类型 %s 或表达式键暂不支持迁移", kind)
		}
		if status != "VALID" && status != "N/A" {
			idx.Unsupported = fmt.Sprintf("Oracle 索引状态 %s 暂不支持迁移", status)
		}
	}
	return out, rows.Err()
}

func (r *OracleDamengReader) GetForeignKeys(ctx context.Context) ([]FKInfo, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT c.CONSTRAINT_NAME,c.TABLE_NAME,cc.COLUMN_NAME,
 rc.TABLE_NAME,rcc.COLUMN_NAME,c.DELETE_RULE,c.R_OWNER,c.STATUS,c.VALIDATED,c.DEFERRABLE,c.DEFERRED
 FROM ALL_CONSTRAINTS c JOIN ALL_CONS_COLUMNS cc
 ON c.OWNER=cc.OWNER AND c.CONSTRAINT_NAME=cc.CONSTRAINT_NAME
 JOIN ALL_CONSTRAINTS rc ON c.R_OWNER=rc.OWNER AND c.R_CONSTRAINT_NAME=rc.CONSTRAINT_NAME
 JOIN ALL_CONS_COLUMNS rcc ON rc.OWNER=rcc.OWNER AND rc.CONSTRAINT_NAME=rcc.CONSTRAINT_NAME AND cc.POSITION=rcc.POSITION
 WHERE c.CONSTRAINT_TYPE='R' AND c.OWNER=:1 ORDER BY c.TABLE_NAME,c.CONSTRAINT_NAME,cc.POSITION`, r.owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FKInfo{}
	positions := map[string]int{}
	for rows.Next() {
		var name, table, column, refTable, refColumn, deletion, owner, status, validated, deferrable, deferred string
		if err = rows.Scan(&name, &table, &column, &refTable, &refColumn, &deletion, &owner, &status, &validated, &deferrable, &deferred); err != nil {
			return nil, err
		}
		key := table + "\x00" + name
		pos, ok := positions[key]
		if !ok {
			pos = len(out)
			positions[key] = pos
			out = append(out, FKInfo{TableName: table, ConstraintName: name, RefTable: refTable, RefSchema: owner, OnDelete: deletion, OnUpdate: "NO ACTION"})
		}
		f := &out[pos]
		f.Columns = append(f.Columns, column)
		f.RefColumns = append(f.RefColumns, refColumn)
		if owner != r.owner {
			f.Unsupported = "跨 schema 外键引用暂不支持迁移"
		} else if status != "ENABLED" || validated != "VALIDATED" || deferrable != "NOT DEFERRABLE" || deferred != "IMMEDIATE" {
			f.Unsupported = "禁用、未验证或可延迟外键暂不支持等价迁移"
		}
	}
	return out, rows.Err()
}
