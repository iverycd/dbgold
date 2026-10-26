package target

import (
	"context"
	"testing"

	"dbgold/datamigrate/damengnative"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestMaterializedGeneratedMultipleKeysReconcilePartialModificationReadOnly(t *testing.T) {
	ddl := `CREATE TABLE "Dst"."Empty"("Input" INT,"A" INT GENERATED ALWAYS AS ("Input"*"Input") VIRTUAL,"B" INT GENERATED ALWAYS AS (CASE WHEN "Input" IS NULL THEN 100 ELSE "Input" END) VIRTUAL) PARTITION BY LIST("A") SUBPARTITION BY RANGE("B") SUBPARTITION TEMPLATE(SUBPARTITION "Neg" VALUES LESS THAN(-5))(PARTITION "Five" VALUES(5))`
	p := &damengnative.MaterializedEmptyTouch{Table: "Empty", DefinitionSQL: ddl, Columns: []damengnative.MaterializedEmptyColumn{{Name: "Input"}}, GeneratedColumns: []damengnative.MaterializedEmptyGeneratedColumn{{Name: "A", ValueSQL: "CAST('5' AS INT)"}, {Name: "B", ValueSQL: "CAST('-6' AS INT)"}}}
	temporary, original, err := nativeHistoryGeneratedDefinition(p)
	require.NoError(t, err)
	require.Len(t, original, 2)
	for _, name := range []string{"original", "partial", "all", "unknown"} {
		t.Run(name, func(t *testing.T) {
			conn, m := historyEmptyTestConn(t)
			actual := ddl
			var e error
			if name == "partial" || name == "all" {
				actual, e = damengnative.RewriteNativeGeneratedColumnConstant(actual, "A", p.GeneratedColumns[0].ValueSQL)
				require.NoError(t, e)
			}
			if name == "all" {
				actual, e = damengnative.RewriteNativeGeneratedColumnConstant(actual, "B", p.GeneratedColumns[1].ValueSQL)
				require.NoError(t, e)
			}
			if name == "unknown" {
				actual, e = damengnative.RewriteNativeGeneratedColumnConstant(actual, "B", "CAST('999' AS INT)")
				require.NoError(t, e)
			}
			expectEmptyDDL(m, actual)
			if name != "unknown" {
				m.ExpectQuery(`SELECT c.OWNER,c.CONSTRAINT_NAME`).WillReturnRows(sqlmock.NewRows([]string{"owner", "name", "table", "kind", "status", "validated", "column"}))
			}
			e = nativeHistoryGeneratedCurrent(context.Background(), conn, "Dst", p, temporary)
			if name == "unknown" {
				require.ErrorContains(t, e, "已偏离冻结表达式")
			} else {
				require.NoError(t, e)
			}
			require.NoError(t, m.ExpectationsWereMet(), "partial-generation reconciliation cannot mutate")
		})
	}
}

func TestMaterializedGeneratedRecipeCannotIncludeWritableColumnOrUserCall(t *testing.T) {
	p := &damengnative.MaterializedEmptyTouch{DefinitionSQL: `CREATE TABLE "Dst"."Empty"("Input" INT,"Key" INT GENERATED ALWAYS AS ("Input"*"Input") VIRTUAL)`, Columns: []damengnative.MaterializedEmptyColumn{{Name: "Input"}}}
	for _, generated := range [][]damengnative.MaterializedEmptyGeneratedColumn{
		{{Name: "Input", ValueSQL: "CAST('1' AS INT)"}},
		{{Name: "Key", ValueSQL: `"App"."Danger"()`}},
		{{Name: "Key", ValueSQL: "CAST('1' AS INT)"}, {Name: "Key", ValueSQL: "CAST('2' AS INT)"}},
	} {
		p.GeneratedColumns = generated
		_, _, err := nativeHistoryGeneratedDefinition(p)
		require.Error(t, err)
	}
}

func TestMaterializedGeneratedEpochPreflightChecksCrossSchemaPrivileges(t *testing.T) {
	for _, missing := range []string{"ALTER ANY TABLE", "INSERT ANY TABLE", "DELETE ANY TABLE", ""} {
		t.Run(missing, func(t *testing.T) {
			w, m := mockNativeDM(t)
			m.ExpectQuery(`SELECT USER FROM DUAL`).WillReturnRows(sqlmock.NewRows([]string{"user"}).AddRow("Operator"))
			m.ExpectQuery(`SELECT COUNT\(\*\) FROM SYS.SYSOBJECTS s JOIN SYS.SYSOBJECTS u`).WillReturnRows(sqlmock.NewRows([]string{"owner"}).AddRow(0))
			grants := sqlmock.NewRows([]string{"privilege"})
			for _, privilege := range []string{"CREATE ANY MATERIALIZED VIEW", "ALTER ANY TABLE", "INSERT ANY TABLE", "DELETE ANY TABLE"} {
				if privilege != missing {
					grants.AddRow(privilege)
				}
			}
			m.ExpectQuery(`SELECT PRIVILEGE FROM SESSION_PRIVS`).WillReturnRows(grants)
			issues := w.ValidateDamengNativeObjects(context.Background(), []damengnative.Object{{Kind: "materialized_view", Schema: w.schema, Name: "Snapshot", Materialized: &damengnative.MaterializedState{History: &damengnative.MaterializedHistoryPlan{Views: []damengnative.MaterializedHistoryView{{EmptyTouch: &damengnative.MaterializedEmptyTouch{Table: "Base", GeneratedColumns: []damengnative.MaterializedEmptyGeneratedColumn{{Name: "Key", ValueSQL: "CAST('1' AS INT)"}}}}}}}}})
			if missing == "" {
				require.Empty(t, issues)
			} else {
				require.Len(t, issues, 1)
				require.Contains(t, issues[0].Reason, missing)
			}
			require.NoError(t, m.ExpectationsWereMet(), "missing write privileges must be detected read-only")
		})
	}
}
