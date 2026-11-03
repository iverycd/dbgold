package datamigrate

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

// Unrelated native integration fixtures use random business table names. Give
// them existing sentinel functions so missing application tables do not prevent
// their event restoration. The production installer must leave these definitions
// alone. Actual creation/compilation is covered by the target integration tests.
// Call only after the fixture's opt-in and empty-schema checks.
func newNativeIntegrationWriter(t *testing.T, dsn, schema string, pool target.ConnPoolConfig) (*target.DaMengNativeWriter, error) {
	t.Helper()
	w, err := target.NewDaMengNative(dsn, schema, pool)
	if err != nil {
		return nil, err
	}
	provisionDamengFixtureFunctions(t, dsn, w.EffectiveSchema())
	return w, nil
}

func provisionDamengFixtureFunctions(t *testing.T, dsn, schema string) {
	t.Helper()
	db, err := sql.Open("dm", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, item := range damengFunctionFixtureReport(schema).Items {
		name := item.Name
		kind := strings.ToUpper(item.Kind)
		var count int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_OBJECTS WHERE OWNER=? AND OBJECT_NAME=? AND OBJECT_TYPE=?`, schema, name, kind).Scan(&count))
		if count != 0 {
			continue
		}
		qualified := damengnative.Qualified(schema, name)
		ddl := "CREATE FUNCTION " + qualified + " RETURN INT AS BEGIN RETURN 197; END;"
		if item.Kind == "procedure" {
			ddl = "CREATE PROCEDURE " + qualified + "(marker OUT INT) AS BEGIN marker := 197; END;"
		}
		_, err := db.ExecContext(ctx, ddl)
		require.NoError(t, err)
		t.Cleanup(func() {
			clean, stop := context.WithTimeout(context.Background(), 15*time.Second)
			defer stop()
			var sentinel int
			var err error
			if item.Kind == "procedure" {
				_, err = db.ExecContext(clean, "CALL "+qualified+"(?)", sql.Out{Dest: &sentinel})
			} else {
				err = db.QueryRowContext(clean, "SELECT "+qualified+"() FROM DUAL").Scan(&sentinel)
			}
			if err != nil {
				t.Errorf("existing function %s was changed: %v", qualified, err)
			} else if sentinel != 197 {
				t.Errorf("existing function %s returned %d, want 197", qualified, sentinel)
			}
			_, err = db.ExecContext(clean, "DROP "+kind+" "+qualified)
			if err != nil {
				t.Errorf("cleanup fixture function %s: %v", qualified, err)
			}
		})
	}
}
