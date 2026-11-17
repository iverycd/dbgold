package target

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
)

func executePGManualTriggerRepair(ctx context.Context, w Writer, o repair.Object, units []repair.Unit, onStep func([]repair.Unit) error) ([]repair.Unit, error) {
	out := slices.Clone(units)
	if !source.IsPGFamily(w.DBType()) || !o.TrustedManualTrigger() || o.RequiresReview || !pgManualTriggerRetryable(o.Status) || len(out) != len(o.Units) {
		return out, fmt.Errorf("手工触发器计划与原目标不一致或当前状态不可执行")
	}
	for i, u := range out {
		saved := o.Units[i]
		if u.ID != saved.ID || u.SQL != saved.SQL || u.OriginalSQL != saved.OriginalSQL ||
			u.Destructive || !u.Selected || !pgManualTriggerRetryable(u.Status) || !pgManualTriggerRetryable(saved.Status) {
			return out, fmt.Errorf("手工触发器须执行同一已保存版本中的两个未提交步骤")
		}
	}

	def, err := repair.ParsePGManualTrigger(o.ManualTrigger.FunctionSQL, o.ManualTrigger.SQL)
	if err != nil {
		return out, err
	}
	a, err := repairAccess(w)
	if err != nil {
		return out, err
	}
	schema, err := ResolveRepairSchema(ctx, w)
	if err != nil {
		return out, err
	}
	if schema != o.Schema {
		return out, fmt.Errorf("目标 schema 与手工触发器计划不一致")
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()

	tableOID, err := lockPGManualTriggerTables(ctx, tx, o)
	if err != nil {
		return out, err
	}
	persist := func() error {
		if onStep != nil {
			return onStep(slices.Clone(out))
		}
		return nil
	}
	rollback := func(failed int, cause error) ([]repair.Unit, error) {
		rollbackErr := tx.Rollback()
		for i := range out {
			switch {
			case i != failed && out[i].Status != "running":
				continue
			case rollbackErr != nil && out[i].Status == "running":
				out[i].Status, out[i].Error = "unknown", rollbackErr.Error()
			case i == failed:
				out[i].Status, out[i].Error = "failed", cause.Error()
			default:
				out[i].Status, out[i].Error = "rolled_back", ""
			}
		}
		return out, errors.Join(cause, rollbackErr, persist())
	}

	functionOID, _, err := readPGManualTriggerFunction(ctx, tx, def.Schema, def.FunctionName)
	if err != nil {
		return out, fmt.Errorf("核对目标触发器函数: %w", err)
	}
	if functionOID != 0 {
		return rollback(0, fmt.Errorf("目标已存在同名零参数函数 %s，请核对目标库", qualifiedTriggerIdent(def.Schema, def.FunctionName)))
	}
	var triggerExists bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM pg_catalog.pg_trigger WHERE tgrelid=$1 AND tgname::text=$2
	)`, tableOID, def.TriggerName).Scan(&triggerExists)
	if err != nil {
		return out, fmt.Errorf("核对目标触发器: %w", err)
	}
	if triggerExists {
		return rollback(1, fmt.Errorf("目标表已存在同名触发器 %s，请核对目标库", quoteTriggerIdent(def.TriggerName)))
	}

	for i, label := range []string{"创建触发器函数", "创建触发器"} {
		before := out[i]
		out[i].Status, out[i].Error = "running", ""
		if err = persist(); err != nil {
			out[i] = before
			return rollback(-1, fmt.Errorf("保存执行前状态: %w", err))
		}
		if err = execPreparedPGTriggerSQL(ctx, tx, out[i].SQL); err != nil {
			return rollback(i, fmt.Errorf("%s: %w", label, err))
		}
		if i == 0 {
			var valid bool
			functionOID, valid, err = readPGManualTriggerFunction(ctx, tx, def.Schema, def.FunctionName)
			if err != nil {
				return rollback(i, fmt.Errorf("核对新建触发器函数: %w", err))
			}
			if functionOID == 0 || !valid {
				return rollback(i, fmt.Errorf("目标库未建立预期的 PL/pgSQL 触发器函数"))
			}
		}
	}

	var bound bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM pg_catalog.pg_trigger
		WHERE tgrelid=$1 AND tgname::text=$2 AND tgfoid=$3 AND NOT tgisinternal AND tgenabled<>'D'
	)`, tableOID, def.TriggerName, functionOID).Scan(&bound)
	if err != nil {
		return rollback(1, fmt.Errorf("核对新建触发器绑定: %w", err))
	}
	if !bound {
		return rollback(1, fmt.Errorf("目标触发器未绑定到本次创建的函数"))
	}
	if owner := o.ManualTrigger.Owner; owner != "" {
		_, err = tx.ExecContext(ctx, "ALTER FUNCTION "+qualifiedTriggerIdent(def.Schema, def.FunctionName)+"() OWNER TO "+quoteTriggerIdent(owner))
		if err != nil {
			return rollback(0, fmt.Errorf("设置触发器函数所有者: %w", err))
		}
	}
	unknown := func(cause error) ([]repair.Unit, error) {
		for i := range out {
			out[i].Status, out[i].Error = "unknown", cause.Error()
		}
		return out, errors.Join(cause, persist())
	}
	if err = tx.Commit(); err != nil {
		return unknown(fmt.Errorf("触发器事务提交结果无法确认，请核对目标库: %w", err))
	}
	for i := range out {
		out[i].Status, out[i].Error, out[i].Selected = "succeeded", "", false
	}
	if err = persist(); err != nil {
		return unknown(fmt.Errorf("触发器事务已提交，执行结果保存失败，请核对目标库: %w", err))
	}
	return out, nil
}

func pgManualTriggerRetryable(status string) bool {
	switch status {
	case "pending", "draft", "failed", "blocked", "cancelled", "rolled_back":
		return true
	default:
		return false
	}
}

func lockPGManualTriggerTables(ctx context.Context, tx *sql.Tx, o repair.Object) (int64, error) {
	tables := []string{o.Table}
	if original := o.ManualTrigger.Original.MySQLPGTrigger; original != nil {
		tables = append(tables, original.Dependencies...)
	}
	slices.Sort(tables)
	var tableOID int64
	for _, name := range slices.Compact(tables) {
		if _, err := tx.ExecContext(ctx, "LOCK TABLE "+qualifiedTriggerIdent(o.Schema, name)+" IN SHARE ROW EXCLUSIVE MODE"); err != nil {
			return 0, fmt.Errorf("锁定触发器依赖表 %s: %w", quoteTriggerIdent(name), err)
		}
		var oid int64
		err := tx.QueryRowContext(ctx, `SELECT c.oid FROM pg_catalog.pg_class c
			JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
			WHERE n.nspname::text=$1 AND c.relname::text=$2 AND c.relkind IN ('r','p')`, o.Schema, name).Scan(&oid)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("触发器依赖 %s 不是可见的目标表", qualifiedTriggerIdent(o.Schema, name))
		}
		if err != nil {
			return 0, fmt.Errorf("核对触发器依赖表 %s: %w", quoteTriggerIdent(name), err)
		}
		if name == o.Table {
			tableOID = oid
		}
	}
	return tableOID, nil
}

func readPGManualTriggerFunction(ctx context.Context, tx *sql.Tx, schema, name string) (int64, bool, error) {
	var oid int64
	var valid bool
	err := tx.QueryRowContext(ctx, `SELECT p.oid,
			p.prorettype='pg_catalog.trigger'::pg_catalog.regtype AND l.lanname='plpgsql'
			AND NOT p.proretset AND NOT p.prosecdef
		FROM pg_catalog.pg_proc p
		JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
		JOIN pg_catalog.pg_language l ON l.oid=p.prolang
		WHERE n.nspname::text=$1 AND p.proname::text=$2 AND p.pronargs=0`, schema, name).Scan(&oid, &valid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return oid, valid, err
}

func execPreparedPGTriggerSQL(ctx context.Context, tx *sql.Tx, sqlText string) error {
	// The extended protocol rejects multiple top-level commands. Keep the
	// authored function body intact and prepare each unit on the transaction.
	stmt, err := tx.PrepareContext(ctx, sqlText)
	if err != nil {
		return err
	}
	_, err = stmt.ExecContext(ctx)
	return errors.Join(err, stmt.Close())
}
