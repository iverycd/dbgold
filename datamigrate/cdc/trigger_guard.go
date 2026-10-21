package cdc

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

const targetBusinessTriggersSQL = `SELECT t.relname,g.tgname FROM pg_trigger g JOIN pg_class t ON t.oid=g.tgrelid JOIN pg_namespace n ON n.oid=t.relnamespace WHERE n.nspname=$1 AND NOT g.tgisinternal AND g.tgenabled<>'D' ORDER BY t.relname,g.tgname`

// ROW binlog already includes source trigger effects. Target business triggers
// would apply those effects a second time, so they cannot be used during CDC.
type triggerMetadataReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func checkTargetBusinessTriggers(ctx context.Context, db triggerMetadataReader, cfg Config, tables []TableInfo) error {
	selected := map[string]bool{}
	for _, t := range tables {
		selected[targetTableName(cfg, t.Name)] = true
	}
	rows, err := db.QueryContext(ctx, targetBusinessTriggersSQL, cfg.TargetSchema)
	if err != nil {
		return fmt.Errorf("检查 CDC 目标触发器失败: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var table, name string
		if err := rows.Scan(&table, &name); err != nil {
			return err
		}
		if selected[table] {
			return fmt.Errorf("CDC 目标表 %s.%s 存在启用业务触发器 %s，源端副作用已包含在 ROW binlog，必须先停用目标触发器", cfg.TargetSchema, table, name)
		}
	}
	return rows.Err()
}

// Hold each table's ordinary write lock while checking its triggers and
// applying the transaction. CREATE/ENABLE TRIGGER takes a conflicting table
// lock, so a trigger cannot appear between this check and the replicated DML.
func (a *PostgresApplier) checkTransactionTriggers(ctx context.Context, tx *sql.Tx, changes []Change) error {
	tables := []TableInfo{}
	seen := map[string]bool{}
	for _, ch := range changes {
		if ch.Table != nil && !seen[ch.Table.Name] {
			seen[ch.Table.Name] = true
			tables = append(tables, *ch.Table)
		}
	}
	if len(tables) == 0 {
		return nil
	}
	quoted := make([]string, 0, len(tables))
	for _, t := range tables {
		quoted = append(quoted, a.qualified(t.Name))
	}
	sort.Strings(quoted)
	if _, err := tx.ExecContext(ctx, "LOCK TABLE "+strings.Join(quoted, ",")+" IN ROW EXCLUSIVE MODE"); err != nil {
		return fmt.Errorf("锁定 CDC 目标表: %w", err)
	}
	return checkTargetBusinessTriggers(ctx, tx, Config{TargetSchema: a.schema, LowerCaseNames: a.lower}, tables)
}
