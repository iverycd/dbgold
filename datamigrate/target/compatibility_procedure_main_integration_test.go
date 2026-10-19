package target

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// These cases share the independently specified seven-table fixture with the
// archive suite, but also seed history directly so restoring does not depend on
// ChangeDataToHistory having moved the expected rows first.
func TestCompatibilityProcedureMainSQLIntegration(t *testing.T) {
	type restoreCase struct {
		name string
		run  func(*testing.T, context.Context, *archiveFixture)
	}
	for _, product := range []struct{ name, variable, driver string }{
		{"postgres", "DBGOLD_TEST_POSTGRES_DSN", "postgres"},
		{"kingbase", "DBGOLD_TEST_KINGBASE_DSN", "postgres"},
		{"highgo", "DBGOLD_TEST_HIGHGO_DSN", "highgo"},
		{"gaussdb", "DBGOLD_TEST_GAUSSDB_DSN", "opengauss"},
		{"vastbase", "DBGOLD_TEST_VASTBASE_DSN", "postgres"},
		{"gbase", "DBGOLD_TEST_GBASE_DSN", "postgres"},
		{"seabox", "DBGOLD_TEST_SEABOX_DSN", "postgres"},
	} {
		t.Run(product.name, func(t *testing.T) {
			dsn := os.Getenv(product.variable)
			if dsn == "" {
				t.Skip(product.variable + " is not set")
			}
			db, err := sql.Open(product.driver, dsn)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			require.NoError(t, db.PingContext(ctx))
			cases := []restoreCase{
				{"scope_fields_and_repeat", checkRestoreScope},
				{"archive_restore_round_trip", checkRestoreRoundTrip},
				{"missing_root_record", checkRestoreMissingRoot},
				{"conflict_rolls_back_every_move", checkRestoreConflict},
				{"dependency_validation", checkRestoreDependencies},
			}
			if product.name == "postgres" {
				cases = append(cases,
					restoreCase{"trigger_skipped_insert_rolls_back", checkRestoreSkippedInsert},
					restoreCase{"caller_transaction_is_rejected", checkRestoreCallerTransaction},
				)
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					schema := newBusinessIntegrationSchema(t, ctx, db)
					// Initialization must not read workflow data or invoke any
					// procedure: none of their business tables exist yet.
					report, err := ensurePGProcedureCompatibility(ctx, db, schema, product.name)
					require.NoError(t, err)
					require.Equal(t, len(compatibilityProcedureSpecs), report.Created)
					require.Len(t, report.Items, len(compatibilityProcedureSpecs))
					report, err = ensurePGProcedureCompatibility(ctx, db, schema, product.name)
					require.NoError(t, err)
					require.Equal(t, len(compatibilityProcedureSpecs), report.Reused)
					for i, spec := range compatibilityProcedureSpecs {
						if spec.writableCTE {
							require.Contains(t, strings.ToLower(report.Items[i].DDL), "commit")
						}
					}
					f := newArchiveFixture(t, ctx, db, schema)
					f.product = product.name
					tc.run(t, ctx, f)
				})
			}
		})
	}
}

func restoreCall(f *archiveFixture) string {
	return "CALL " + qualifiedFunction(f.schema, "changedatatomain") + "($1::varchar)"
}

func seedRestoreHistory(t *testing.T, ctx context.Context, f *archiveFixture, guids ...string) {
	t.Helper()
	f.seed(t, ctx, guids...)
	for _, pair := range archiveFixtureTables {
		for _, guid := range guids {
			businessExec(t, ctx, f.db, "INSERT INTO "+f.table(pair.history, true)+" SELECT * FROM "+f.table(pair.source, false)+" WHERE "+archiveColumn(pair.key, false)+"=$1", guid)
			businessExec(t, ctx, f.db, "DELETE FROM "+f.table(pair.source, false)+" WHERE "+archiveColumn(pair.key, false)+"=$1", guid)
		}
	}
}

func checkRestoreScope(t *testing.T, ctx context.Context, f *archiveFixture) {
	// Include duplicate children and a self-reference. Neither may broaden the
	// requested root plus direct-child scope or restore a workflow twice.
	businessExec(t, ctx, f.db, "INSERT INTO "+f.table("workflow_pvi", false)+" VALUES ('root','root','root-tag'),('c2','root','1'),('c1','root','1'),('c1','root','1'),('grandchild','c1','1'),('unrelated',NULL,'old')")
	guids := []string{"root", "c1", "c2", "grandchild", "unrelated"}
	seedRestoreHistory(t, ctx, f, guids...)
	want := make(map[string][][]sql.NullString)
	for _, pair := range archiveFixtureTables {
		for _, guid := range guids {
			want[pair.source+guid] = f.rows(t, ctx, pair, true, guid)
		}
	}
	conn, err := f.db.Conn(ctx)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(ctx, "SET search_path TO pg_catalog")
	require.NoError(t, err)
	defer conn.ExecContext(ctx, "RESET search_path")
	for repeat := 0; repeat < 2; repeat++ {
		if repeat == 0 {
			_, err = conn.ExecContext(ctx, "CALL "+qualifiedFunction(f.schema, "changedatatomain")+"('root')")
		} else {
			_, err = conn.ExecContext(ctx, restoreCall(f), "root")
		}
		require.NoError(t, err)
		for _, pair := range archiveFixtureTables {
			for _, guid := range []string{"root", "c1", "c2"} {
				require.Equal(t, want[pair.source+guid], f.rows(t, ctx, pair, false, guid), pair.source+guid)
				require.Empty(t, f.rows(t, ctx, pair, true, guid))
			}
			for _, guid := range []string{"grandchild", "unrelated"} {
				require.Empty(t, f.rows(t, ctx, pair, false, guid))
				require.Equal(t, want[pair.source+guid], f.rows(t, ctx, pair, true, guid), pair.source+guid)
			}
		}
	}
	require.Equal(t, "root-tag", f.tag(t, ctx, "root"))
	require.Empty(t, f.tag(t, ctx, "c1"))
	require.Empty(t, f.tag(t, ctx, "c2"))
	require.Equal(t, "1", f.tag(t, ctx, "grandchild"))
	require.Equal(t, "old", f.tag(t, ctx, "unrelated"))
}

func checkRestoreRoundTrip(t *testing.T, ctx context.Context, f *archiveFixture) {
	businessExec(t, ctx, f.db, "INSERT INTO "+f.table("workflow_pvi", false)+" VALUES ('root',NULL,'root-tag'),('child','root','old'),('grandchild','child','grandchild-tag'),('unrelated',NULL,'unrelated-tag')")
	guids := []string{"root", "child", "grandchild", "unrelated"}
	f.seed(t, ctx, guids...)
	want := make(map[string][][]sql.NullString)
	for _, pair := range archiveFixtureTables {
		for _, guid := range guids {
			want[pair.source+guid] = f.rows(t, ctx, pair, false, guid)
		}
	}
	businessExec(t, ctx, f.db, "CALL "+qualifiedFunction(f.schema, "changedatatohistory")+"($1::varchar)", "root")
	for _, pair := range archiveFixtureTables {
		for _, guid := range []string{"root", "child"} {
			require.Empty(t, f.rows(t, ctx, pair, false, guid))
			require.Equal(t, want[pair.source+guid], f.rows(t, ctx, pair, true, guid))
		}
	}
	require.Equal(t, "1", f.tag(t, ctx, "child"))
	businessExec(t, ctx, f.db, restoreCall(f), "root")
	for _, pair := range archiveFixtureTables {
		for _, guid := range guids {
			require.Equal(t, want[pair.source+guid], f.rows(t, ctx, pair, false, guid), pair.source+guid)
			require.Empty(t, f.rows(t, ctx, pair, true, guid))
		}
	}
	require.Equal(t, "root-tag", f.tag(t, ctx, "root"))
	require.Empty(t, f.tag(t, ctx, "child"))
	require.Equal(t, "grandchild-tag", f.tag(t, ctx, "grandchild"))
	require.Equal(t, "unrelated-tag", f.tag(t, ctx, "unrelated"))
}

func checkRestoreMissingRoot(t *testing.T, ctx context.Context, f *archiveFixture) {
	seedRestoreHistory(t, ctx, f, "no-pvi-row")
	businessExec(t, ctx, f.db, restoreCall(f), "no-pvi-row")
	for _, pair := range archiveFixtureTables {
		require.Len(t, f.rows(t, ctx, pair, false, "no-pvi-row"), 1)
		require.Empty(t, f.rows(t, ctx, pair, true, "no-pvi-row"))
	}
}

func checkRestoreConflict(t *testing.T, ctx context.Context, f *archiveFixture) {
	businessExec(t, ctx, f.db, "INSERT INTO "+f.table("workflow_pvi", false)+" VALUES ('root',NULL,'root-tag'),('child','root','1')")
	seedRestoreHistory(t, ctx, f, "root", "child")
	// The root's fifth pair conflicts after all child tables and its tag have
	// changed, proving the failure rolls back the whole CALL.
	conflict := archiveFixtureTables[4]
	businessExec(t, ctx, f.db, "INSERT INTO "+f.table(conflict.source, false)+" SELECT * FROM "+f.table(conflict.history, true)+" WHERE "+archiveColumn(conflict.key, true)+"=$1", "root")
	_, err := f.db.ExecContext(ctx, restoreCall(f), "root")
	require.Error(t, err)
	require.Equal(t, "23505", compatibilitySQLState(err))
	for i, pair := range archiveFixtureTables {
		for _, guid := range []string{"root", "child"} {
			require.Len(t, f.rows(t, ctx, pair, true, guid), 1)
			if i == 4 && guid == "root" {
				require.Equal(t, f.rows(t, ctx, pair, true, guid), f.rows(t, ctx, pair, false, guid))
			} else {
				require.Empty(t, f.rows(t, ctx, pair, false, guid))
			}
		}
	}
	require.Equal(t, "1", f.tag(t, ctx, "child"))
	require.Equal(t, "root-tag", f.tag(t, ctx, "root"))
}

func requireRestoreUnmoved(t *testing.T, ctx context.Context, f *archiveFixture, guids ...string) {
	t.Helper()
	for _, pair := range archiveFixtureTables {
		for _, guid := range guids {
			require.Len(t, f.rows(t, ctx, pair, true, guid), 1, pair.history+guid)
			require.Empty(t, f.rows(t, ctx, pair, false, guid), pair.source+guid)
		}
	}
}

func checkRestoreDependencies(t *testing.T, ctx context.Context, f *archiveFixture) {
	seedRestoreHistory(t, ctx, f, "root")
	for _, value := range []any{nil, ""} {
		_, err := f.db.ExecContext(ctx, restoreCall(f), value)
		require.ErrorContains(t, err, "must not be NULL or empty")
		requireRestoreUnmoved(t, ctx, f, "root")
	}
	last := archiveFixtureTables[6]
	// Missing source columns and ambiguous destination columns must be caught
	// before the first pair is moved, not only when the last pair executes.
	businessExec(t, ctx, f.db, "ALTER TABLE "+f.table(last.history, true)+" RENAME COLUMN fieldvalue TO missing")
	_, err := f.db.ExecContext(ctx, restoreCall(f), "root")
	require.ErrorContains(t, err, "column fieldvalue is missing")
	businessExec(t, ctx, f.db, "ALTER TABLE "+f.table(last.history, true)+" RENAME COLUMN missing TO fieldvalue")
	requireRestoreUnmoved(t, ctx, f, "root")
	businessExec(t, ctx, f.db, "ALTER TABLE "+f.table(last.source, false)+" ADD fieldvalue varchar")
	_, err = f.db.ExecContext(ctx, restoreCall(f), "root")
	require.ErrorContains(t, err, "column fieldvalue is ambiguous")
	businessExec(t, ctx, f.db, "ALTER TABLE "+f.table(last.source, false)+" DROP COLUMN fieldvalue")
	requireRestoreUnmoved(t, ctx, f, "root")
	// A same-named table in another schema must not satisfy a missing table in
	// the captured target schema, even if caller search_path could find it.
	businessExec(t, ctx, f.db, "ALTER TABLE "+f.table(last.source, false)+" RENAME TO restore_missing_context")
	otherSchema := newBusinessIntegrationSchema(t, ctx, f.db)
	businessExec(t, ctx, f.db, "CREATE TABLE "+qualifiedFunction(otherSchema, last.source)+" (fieldvalue varchar)")
	_, err = f.db.ExecContext(ctx, restoreCall(f), "root")
	require.ErrorContains(t, err, "table workflow_context_value is missing")
	businessExec(t, ctx, f.db, "ALTER TABLE "+qualifiedFunction(f.schema, "restore_missing_context")+" RENAME TO "+archiveColumn(last.source, false))
	requireRestoreUnmoved(t, ctx, f, "root")
}

func checkRestoreSkippedInsert(t *testing.T, ctx context.Context, f *archiveFixture) {
	businessExec(t, ctx, f.db, "INSERT INTO "+f.table("workflow_pvi", false)+" VALUES ('root',NULL,'root-tag'),('child','root','1')")
	seedRestoreHistory(t, ctx, f, "root", "child")
	triggerFunction := qualifiedFunction(f.schema, "skip_restore_row")
	businessExec(t, ctx, f.db, "CREATE FUNCTION "+triggerFunction+"() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN IF NEW.\"PROCESSVERSIONINSTANCEGUID\"='root' THEN RETURN NULL; END IF; RETURN NEW; END$$")
	businessExec(t, ctx, f.db, "CREATE TRIGGER skip_restore_row BEFORE INSERT ON "+f.table("workflow_context_value", false)+" FOR EACH ROW EXECUTE FUNCTION "+triggerFunction+"()")
	_, err := f.db.ExecContext(ctx, restoreCall(f), "root")
	require.ErrorContains(t, err, "row count mismatch")
	requireRestoreUnmoved(t, ctx, f, "root", "child")
	require.Equal(t, "1", f.tag(t, ctx, "child"))
	require.Equal(t, "root-tag", f.tag(t, ctx, "root"))
}

func checkRestoreCallerTransaction(t *testing.T, ctx context.Context, f *archiveFixture) {
	seedRestoreHistory(t, ctx, f, "root")
	tx, err := f.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, restoreCall(f), "root")
	require.Error(t, err)
	require.Equal(t, "2D000", compatibilitySQLState(err))
	require.NoError(t, tx.Rollback())
	requireRestoreUnmoved(t, ctx, f, "root")
}
