package target

import (
	"context"
	"regexp"
	"testing"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/repair"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestNativeViewExecutesSavedSQLThenVerifiesCompilation(t *testing.T) {
	const schema = "MixedOwner"
	const name = "MyView"
	const query = `CREATE OR REPLACE VIEW "MyView" AS SELECT 1 FROM DUAL UNION ALL SELECT 2 FROM DUAL`
	const stored = `CREATE VIEW "MixedOwner"."MyView" AS SELECT 1 FROM DUAL UNION ALL SELECT 2 FROM DUAL`
	const compile = `ALTER VIEW "MixedOwner"."MyView" COMPILE`
	base := damengnative.Object{ID: "v", Kind: "view", Schema: schema, Name: name}
	o := repair.Object{ID: base.ID, Kind: base.Kind, Schema: schema, TargetName: name, Native: &base, NativeFingerprint: damengnative.Fingerprint(base), Status: "blocked"}
	for _, scenario := range []string{"success", "invalid", "resume", "conflicting view", "materialized view", "unknown same revision"} {
		t.Run(scenario, func(t *testing.T) {
			w, mock := mockNativeDM(t)
			w.db.SetMaxOpenConns(2) // Keep the sqlmock driver registered across discarded DM sessions.
			keepAlive, err := w.db.Conn(context.Background())
			require.NoError(t, err)
			defer keepAlive.Close()
			obj, err := o.WithNativeViewDraft(query, true)
			require.NoError(t, err)
			state := func(kind, status, sql string) {
				rows := sqlmock.NewRows([]string{"KIND", "STATUS"})
				if kind != "" {
					rows.AddRow(kind, status)
				}
				mock.ExpectQuery("SELECT CASE WHEN EXISTS").WithArgs(schema, name, schema, name).WillReturnRows(rows)
				if kind == "VIEW" {
					mock.ExpectQuery("SELECT DBMS_METADATA.GET_DDL").WithArgs(name, schema).WillReturnRows(sqlmock.NewRows([]string{"DDL"}).AddRow(sql))
				}
			}
			exec := func(sql string) {
				mock.ExpectExec(regexp.QuoteMeta(`SET SCHEMA "MixedOwner"`)).WillReturnResult(sqlmock.NewResult(0, 0))
				mock.ExpectExec(regexp.QuoteMeta(sql)).WillReturnResult(sqlmock.NewResult(0, 0))
				mock.ExpectClose()
			}
			switch scenario {
			case "materialized view":
				state("MATERIALIZED VIEW", "VALID", "")
			case "conflicting view":
				state("VIEW", "VALID", `CREATE VIEW "MixedOwner"."MyView" AS SELECT 99 FROM DUAL`)
			case "resume":
				obj.Units[0].Status = "succeeded"
			case "unknown same revision":
				obj.Units[0].Status = "unknown"
				state("VIEW", "INVALID", stored)
			default:
				state("", "", "")
				exec(query)
			}
			if scenario != "conflicting view" && scenario != "materialized view" {
				state("VIEW", "INVALID", stored)
				exec(compile)
				if scenario == "invalid" {
					state("VIEW", "INVALID", stored)
				} else {
					state("VIEW", "VALID", stored)
				}
			}
			out, err := ExecuteRepair(context.Background(), w, obj, obj.Units, nil)
			switch scenario {
			case "materialized view", "conflicting view":
				require.ErrorContains(t, err, "同名")
				require.Equal(t, "failed", out[0].Status)
			case "invalid":
				require.ErrorContains(t, err, "VALID")
				require.Equal(t, "failed", out[1].Status)
			default:
				require.NoError(t, err)
				require.Equal(t, "succeeded", out[0].Status)
				require.Equal(t, "succeeded", out[1].Status)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
