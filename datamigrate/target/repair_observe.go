package target

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
)

type sqlExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// Running is durable before issuing SQL. A persistence error after execution
// is returned to the caller; the durable running record remains reconcilable.
func execObserved(ctx context.Context, ex sqlExecutor, query string, args ...any) (sql.Result, error) {
	return execObservedAllow(ctx, ex, query, nil, args...)
}

// ignore only applies to a database execution error, never to a failed durable
// observation. This preserves existing absent-table DROP behavior.
func execObservedAllow(ctx context.Context, ex sqlExecutor, query string, ignore func(error) bool, args ...any) (sql.Result, error) {
	if e := repair.Observe(ctx, query, "running", nil); e != nil {
		return nil, fmt.Errorf("保存 DDL 执行前状态: %w", e)
	}
	r, e := ex.ExecContext(ctx, query, args...)
	if e != nil && ignore != nil && ignore(e) {
		e = nil
	}
	status := "succeeded"
	if e != nil {
		status = repairErrorStatus(e)
	}
	pe := repair.Observe(ctx, query, status, e)
	if pe != nil {
		return r, errors.Join(e, fmt.Errorf("保存 DDL 执行结果失败: %w", pe))
	}
	return r, e
}

func execOscarObserved(ctx context.Context, w *OscarWriter, query string) error {
	if e := repair.Observe(ctx, query, "running", nil); e != nil {
		return fmt.Errorf("保存 DDL 执行前状态: %w", e)
	}
	e := w.bridge.Exec(ctx, w.session, query)
	status := "succeeded"
	if e != nil {
		status = repairErrorStatus(e)
	}
	return errors.Join(e, repair.Observe(ctx, query, status, e))
}

// The transaction owns these statements. Success events are emitted only after
// commit; successful Exec calls before a rollback are not reported as applied.
func execObservedTransaction(ctx context.Context, tx *sql.Tx, stmts []dialect.Statement) error {
	var executed []dialect.Statement
	rollback := func(cause error) error {
		e := tx.Rollback()
		status := "rolled_back"
		if e != nil {
			status = "unknown"
		}
		for _, s := range executed {
			cause = errors.Join(cause, repair.Observe(ctx, s.SQL, status, e))
		}
		return cause
	}
	for _, s := range stmts {
		if e := repair.Observe(ctx, s.SQL, "running", nil); e != nil {
			return rollback(fmt.Errorf("保存 DDL 执行前状态: %w", e))
		}
		if _, e := tx.ExecContext(ctx, s.SQL); e != nil {
			pe := repair.Observe(ctx, s.SQL, repairErrorStatus(e), e)
			return rollback(errors.Join(e, pe))
		}
		executed = append(executed, s)
	}
	err := tx.Commit()
	status := "succeeded"
	if err != nil {
		status = "unknown"
	}
	result := err
	for _, s := range executed {
		result = errors.Join(result, repair.Observe(ctx, s.SQL, status, err))
	}
	return result
}
