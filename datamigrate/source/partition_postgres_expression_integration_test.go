package source

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Only an explicitly authorized disposable PostgreSQL database may run this.
// Every created object belongs to one fresh schema; the fixture never modifies
// a role's search_path or any existing function/operator/table.
func TestPostgresPartitionExpressionDependencyIntegration(t *testing.T) {
	dsn := os.Getenv("DBGOLD_PARTITION_PG_DSN")
	if os.Getenv("DBGOLD_PARTITION_ALLOW_DDL") != "1" || dsn == "" {
		t.Skip("需要隔离 PostgreSQL 及 DBGOLD_PARTITION_ALLOW_DDL=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer db.Close()
	// One owned connection keeps the fixture's session-local search_path stable.
	db.SetMaxOpenConns(1)
	name := fmt.Sprintf("dbgold_exprdep_%x", time.Now().UnixNano())
	qn := QuotePGIdent(name)
	exec := func(query string) {
		t.Helper()
		_, err := db.ExecContext(ctx, query)
		require.NoError(t, err)
	}
	exec("CREATE SCHEMA " + qn)
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := db.ExecContext(cleanup, "DROP SCHEMA "+qn+" CASCADE")
		require.NoError(t, err)
	}()
	for _, fn := range []string{"abs", "mod", "div", "coalesce", "add", "multiply"} {
		args := "numeric,numeric"
		if fn == "abs" {
			args = "numeric"
		}
		exec("CREATE FUNCTION " + qn + "." + QuotePGIdent(fn) + "(" + args + ") RETURNS numeric LANGUAGE plpgsql IMMUTABLE AS $$BEGIN RETURN 0; END$$")
	}
	exec("CREATE OPERATOR " + qn + ".+ (FUNCTION=" + qn + ".add,LEFTARG=numeric,RIGHTARG=numeric)")
	exec("CREATE OPERATOR " + qn + ".* (FUNCTION=" + qn + ".multiply,LEFTARG=numeric,RIGHTARG=numeric)")
	exec("CREATE DOMAIN " + qn + ".int8 AS pg_catalog.int8 CHECK(VALUE >= 0)")
	exec("SET search_path TO " + qn + ",pg_catalog")
	r := &PostgresReader{db: db, schema: name}
	for _, tc := range []struct {
		table, expression, dependency string
	}{
		{"builtin", "pg_catalog.abs(k::numeric)", ""},
		{"fn_abs", "abs(k::numeric)", ".abs"},
		{"fn_mod", "mod(k::numeric,3::numeric)", ".mod"},
		{"fn_div", "div(k::numeric,3::numeric)", ".div"},
		{"fn_coalesce", `"coalesce"(k::numeric,3::numeric)`, ".coalesce"},
		{"op_add", "(k::numeric + 3::numeric)", ".+"},
		{"op_multiply", "(k::numeric * 3::numeric)", ".*"},
		{"domain_cast", "(k::int8)", ".int8"},
		{"column_domain", "pg_catalog.abs(k::numeric)", ".int8"},
	} {
		t.Run(tc.table, func(t *testing.T) {
			columns := "k integer"
			if tc.table == "column_domain" {
				columns += ",payload " + qn + ".int8"
			}
			exec("CREATE TABLE " + qn + "." + QuotePGIdent(tc.table) + "(" + columns + ") PARTITION BY LIST(" + tc.expression + ")")
			exec("CREATE TABLE " + qn + "." + QuotePGIdent(tc.table+"_p") + " PARTITION OF " + qn + "." + QuotePGIdent(tc.table) + " FOR VALUES IN(0)")
			spec, err := r.PartitionMetadata(ctx, tc.table)
			require.NoError(t, err)
			require.NotNil(t, spec)
			if tc.dependency == "" {
				require.Empty(t, spec.Unsupported)
			} else {
				require.Contains(t, spec.Unsupported, name+tc.dependency)
			}
			if tc.table == "domain_cast" {
				require.Contains(t, strings.Join(strings.Fields(spec.Keys[0].Expression), ""), "::int8", "native deparse hides the domain schema and resembles the builtin alias")
			}
			if tc.dependency == ".int8" {
				require.Contains(t, spec.Unsupported, "表达式或列类型")
			}
		})
	}
}
