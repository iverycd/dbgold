package target

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"time"

	"dbgold/datamigrate/damengnative"
)

type nativeCopyDefault struct{ column, expression string }

// Bound DM inserts can evaluate ON NULL defaults even for non-NULL bindings.
// Suspend only source-frozen defaults outside the data transaction, then restore
// them after commit/rollback. A failed restoration remains a failed table copy.
type nativeCopyDefaults struct {
	conn          *sql.Conn
	schema, table string
	defaults      []nativeCopyDefault
}

func suspendNativeCopyDefaults(ctx context.Context, conn *sql.Conn, schema string, table damengnative.Table) (_ *nativeCopyDefaults, retErr error) {
	return prepareNativeCopyDefaults(ctx, conn, schema, table, true)
}

func prepareNativeCopyDefaults(ctx context.Context, conn *sql.Conn, schema string, table damengnative.Table, suspend bool) (_ *nativeCopyDefaults, retErr error) {
	g := &nativeCopyDefaults{conn: conn, schema: schema, table: table.Name}
	for _, column := range table.Columns {
		if !column.Writable || !column.DefaultOnNull {
			continue
		}
		if column.Name == "" || column.DefaultSQL == "" {
			return nil, fmt.Errorf("原生数据复制缺少冻结的 DEFAULT ON NULL 表达式")
		}
		g.defaults = append(g.defaults, nativeCopyDefault{column.Name, column.DefaultSQL})
	}
	if len(g.defaults) == 0 {
		return nil, nil
	}
	ddl, err := historyEmptyReadDDL(ctx, conn, schema, table.Name)
	if err != nil {
		return nil, err
	}
	onNull, err := damengnative.NativeDefaultOnNullColumns(ddl)
	if err != nil {
		return nil, err
	}
	rows, err := conn.QueryContext(ctx, `SELECT COLUMN_NAME,DATA_DEFAULT FROM ALL_TAB_COLUMNS WHERE OWNER=? AND TABLE_NAME=? ORDER BY COLUMN_ID`, schema, table.Name)
	if err != nil {
		return nil, err
	}
	actual := map[string]sql.NullString{}
	for rows.Next() {
		var name string
		var expression sql.NullString
		if err = rows.Scan(&name, &expression); err != nil {
			break
		}
		actual[name] = expression
	}
	iteration := rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	if iteration != nil {
		return nil, iteration
	}
	// Validate the whole frozen set before the first mutation. A missing default
	// is the one recoverable intermediate left by a disconnected prior batch.
	for _, d := range g.defaults {
		value, exists := actual[d.column]
		if !exists || value.Valid && (!onNull[d.column] || !nativeDefinitionsEqual(d.expression, value.String)) || !value.Valid && onNull[d.column] {
			return nil, fmt.Errorf("列 %s 的默认值已偏离冻结原生计划", d.column)
		}
	}
	if !suspend {
		missing := g.defaults[:0]
		for _, d := range g.defaults {
			if !actual[d.column].Valid {
				missing = append(missing, d)
			}
		}
		g.defaults = missing
		if len(missing) == 0 {
			return nil, nil
		}
		return g, nil
	}
	changed := false
	defer func() {
		if retErr != nil && changed {
			retErr = errors.Join(retErr, g.restore(ctx))
		}
	}()
	for _, d := range g.defaults {
		if !actual[d.column].Valid {
			changed = true
			continue
		}
		// An uncertain DDL acknowledgement may still have removed the default.
		changed = true
		if _, err = conn.ExecContext(ctx, "ALTER TABLE "+damengnative.Qualified(schema, table.Name)+" ALTER COLUMN "+damengnative.Quote(d.column)+" DROP DEFAULT"); err != nil {
			return nil, err
		}
	}
	return g, nil
}

func (g *nativeCopyDefaults) restore(ctx context.Context) error {
	if g == nil {
		return nil
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	// Native metadata can retain an unqualified routine/sequence reference.
	// Resolve it under the target owner, then discard this changed session.
	defer func() { _ = g.conn.Raw(func(any) error { return driver.ErrBadConn }) }()
	if _, err := g.conn.ExecContext(cleanup, "SET SCHEMA "+damengnative.Quote(g.schema)); err != nil {
		return fmt.Errorf("恢复冻结 DEFAULT ON NULL 的目标 Schema 失败: %w", err)
	}
	var result error
	for _, d := range g.defaults {
		_, err := g.conn.ExecContext(cleanup, "ALTER TABLE "+damengnative.Qualified(g.schema, g.table)+" ALTER COLUMN "+damengnative.Quote(d.column)+" SET DEFAULT ON NULL "+d.expression)
		result = errors.Join(result, err)
	}
	if result != nil {
		return fmt.Errorf("恢复冻结 DEFAULT ON NULL 失败，需要按整表计划补迁: %w", result)
	}
	ddl, err := historyEmptyReadDDL(cleanup, g.conn, g.schema, g.table)
	if err != nil {
		return err
	}
	onNull, err := damengnative.NativeDefaultOnNullColumns(ddl)
	if err != nil {
		return err
	}
	for _, d := range g.defaults {
		if !onNull[d.column] {
			return fmt.Errorf("列 %s 的 DEFAULT ON NULL 未恢复", d.column)
		}
		var expression sql.NullString
		if err = g.conn.QueryRowContext(cleanup, `SELECT DATA_DEFAULT FROM ALL_TAB_COLUMNS WHERE OWNER=? AND TABLE_NAME=? AND COLUMN_NAME=?`, g.schema, g.table, d.column).Scan(&expression); err != nil {
			return err
		}
		if !expression.Valid || !nativeDefinitionsEqual(expression.String, d.expression) {
			return fmt.Errorf("列 %s 的 DEFAULT ON NULL 表达式未按冻结计划恢复", d.column)
		}
	}
	return nil
}
