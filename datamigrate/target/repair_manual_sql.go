package target

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"slices"

	"dbgold/datamigrate/repair"
)

func disposableRepairConnection(ctx context.Context, db *sql.DB) (*sql.Conn, func(), error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	close := func() {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		_ = conn.Close()
	}
	return conn, close, nil
}

func repairSchemaSQL(family string, w Writer, schema string) string {
	switch family {
	case "postgres":
		return "SET search_path TO " + w.Dialect().QuoteIdent(schema)
	case "dameng":
		return "SET SCHEMA " + w.Dialect().QuoteIdent(schema)
	case "oracle":
		return "ALTER SESSION SET CURRENT_SCHEMA = " + w.Dialect().QuoteIdent(schema)
	case "mysql":
		return "USE " + w.Dialect().QuoteIdent(schema)
	}
	return ""
}

// Each selected editor document is one driver call on the bound session.
// Generated object plans can refer to completely different SQL and objects.
func executeManualRepairSQL(ctx context.Context, w Writer, units []repair.Unit, onStep func([]repair.Unit) error) ([]repair.Unit, error) {
	out := slices.Clone(units)
	selected, err := selectedRepairSteps(out)
	if err != nil {
		return out, err
	}
	if oscar, ok := w.(*OscarWriter); ok {
		execute := func(i int) error { return oscar.bridge.Exec(ctx, oscar.session, out[i].SQL) }
		return out, runRepairSteps(ctx, out, selected, nil, execute, onStep)
	}
	a, err := repairAccess(w)
	if err != nil {
		return out, err
	}
	schema, err := ResolveRepairSchema(ctx, w)
	if err != nil {
		return out, err
	}
	conn, close, err := disposableRepairConnection(ctx, a.db)
	if err != nil {
		return out, err
	}
	defer close()
	if useSchema := repairSchemaSQL(a.family, w, schema); useSchema != "" {
		if _, err = conn.ExecContext(ctx, useSchema); err != nil {
			return out, err
		}
	}
	err = runRepairSteps(ctx, out, selected, nil, func(i int) error {
		_, err := conn.ExecContext(ctx, out[i].SQL)
		return err
	}, onStep)
	invalidateRecoveryCache(w)
	return out, err
}
