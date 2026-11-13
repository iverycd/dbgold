package cdc

import (
	"context"
	"database/sql"
	"errors"
)

const damengStatementCacheLimit = 256

type damengDMLExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// Cache belongs to ONE source transaction/physical target session. Session
// controls and checkpoint SQL deliberately bypass it.
type damengStatements struct {
	tx         *sql.Tx
	statements map[string]*sql.Stmt
}

func newDamengStatements(tx *sql.Tx) *damengStatements {
	return &damengStatements{tx: tx, statements: make(map[string]*sql.Stmt)}
}
func (c *damengStatements) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	stmt := c.statements[query]
	if stmt == nil {
		if len(c.statements) >= damengStatementCacheLimit {
			return c.tx.ExecContext(ctx, query, args...)
		}
		var err error
		stmt, err = c.tx.PrepareContext(ctx, query)
		if err != nil {
			return nil, err
		}
		c.statements[query] = stmt
	}
	return stmt.ExecContext(ctx, args...)
}
func (c *damengStatements) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	// This rare existence-only path (all columns are keys/identities) stays on
	// Tx to preserve database/sql's deferred Scan errors without synthetic Rows.
	return c.tx.QueryRowContext(ctx, query, args...)
}
func (c *damengStatements) Close() error {
	var err error
	for query, stmt := range c.statements {
		err = errors.Join(err, stmt.Close())
		delete(c.statements, query)
	}
	return err
}
