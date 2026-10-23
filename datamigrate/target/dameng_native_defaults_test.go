package target

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"dbgold/datamigrate/damengnative"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

const copyDefaultDDL = `CREATE TABLE "MixedOwner"."T"("V" INT DEFAULT ON NULL "MixedOwner"."Seq".NEXTVAL NOT NULL)`

func nativeDefaultTable() damengnative.Table {
	return damengnative.Table{Schema: "MixedOwner", Name: "T", Columns: []damengnative.Column{{Name: "V", DataType: "INT", SQLType: "INT", Writable: true, DefaultOnNull: true, DefaultSQL: `"MixedOwner"."Seq".NEXTVAL`}}}
}

func expectCopyDefaultState(m sqlmock.Sqlmock, ddl string, expression any) {
	m.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL`).WithArgs("T", "MixedOwner").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(ddl))
	m.ExpectQuery(`SELECT COLUMN_NAME,DATA_DEFAULT`).WithArgs("MixedOwner", "T").WillReturnRows(sqlmock.NewRows([]string{"column", "default"}).AddRow("V", expression))
}

func expectCopyDefaultRestored(m sqlmock.Sqlmock) {
	m.ExpectExec(regexp.QuoteMeta(`SET SCHEMA "MixedOwner"`)).WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectExec(regexp.QuoteMeta(`ALTER TABLE "MixedOwner"."T" ALTER COLUMN "V" SET DEFAULT ON NULL "MixedOwner"."Seq".NEXTVAL`)).WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL`).WithArgs("T", "MixedOwner").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(copyDefaultDDL))
	m.ExpectQuery(`SELECT DATA_DEFAULT`).WithArgs("MixedOwner", "T", "V").WillReturnRows(sqlmock.NewRows([]string{"default"}).AddRow(`"MixedOwner"."Seq".NEXTVAL`))
	m.ExpectClose()
}

func TestNativeCopySuspendsDefaultOutsideTransactionAndRestoresAfterCommitOrRollback(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "rollback"}[fail], func(t *testing.T) {
			w, m := mockNativeDM(t)
			expectCopyDefaultState(m, copyDefaultDDL, `"MixedOwner"."Seq".NEXTVAL`)
			m.ExpectExec(`ALTER TABLE .* DROP DEFAULT`).WillReturnResult(sqlmock.NewResult(0, 0))
			m.ExpectBegin()
			exec := m.ExpectPrepare(regexp.QuoteMeta(`INSERT INTO "MixedOwner"."T" ("V") VALUES (?)`)).ExpectExec().WithArgs(int64(10))
			if fail {
				exec.WillReturnError(errors.New("insert failed"))
				m.ExpectRollback()
				m.ExpectExec(`^ROLLBACK$`).WillReturnResult(sqlmock.NewResult(0, 0))
			} else {
				exec.WillReturnResult(sqlmock.NewResult(0, 1))
				m.ExpectCommit()
			}
			expectCopyDefaultRestored(m)
			err := w.CopyDamengNativeBatch(context.Background(), nativeDefaultTable(), [][]any{{int64(10)}})
			if fail {
				require.ErrorContains(t, err, "insert failed")
			} else {
				require.NoError(t, err)
			}
			require.NoError(t, m.ExpectationsWereMet(), "DDL must never commit the open data transaction")
		})
	}
}

func TestNativeCopyRequiresConfirmedSessionRollbackBeforeRestoringDDL(t *testing.T) {
	for _, failure := range []string{"commit_lost", "transaction_rollback_failed", "session_rollback_failed"} {
		t.Run(failure, func(t *testing.T) {
			w, m := mockNativeDM(t)
			expectCopyDefaultState(m, copyDefaultDDL, `"MixedOwner"."Seq".NEXTVAL`)
			m.ExpectExec(`ALTER TABLE .* DROP DEFAULT`).WillReturnResult(sqlmock.NewResult(0, 0))
			m.ExpectBegin()
			write := m.ExpectPrepare(regexp.QuoteMeta(`INSERT INTO "MixedOwner"."T" ("V") VALUES (?)`)).ExpectExec().WithArgs(int64(10))
			if failure == "commit_lost" {
				write.WillReturnResult(sqlmock.NewResult(0, 1))
				m.ExpectCommit().WillReturnError(errors.New("commit acknowledgement lost"))
			} else {
				write.WillReturnError(errors.New("insert failed"))
				m.ExpectRollback().WillReturnError(errors.New("transaction rollback failed"))
			}
			rollback := m.ExpectExec(`^ROLLBACK$`)
			if failure == "session_rollback_failed" {
				rollback.WillReturnError(errors.New("session unreachable"))
				// No restoration DDL may execute on the uncertain transaction.
				m.ExpectClose()
			} else {
				rollback.WillReturnResult(sqlmock.NewResult(0, 0))
				expectCopyDefaultRestored(m)
			}
			err := w.CopyDamengNativeBatch(context.Background(), nativeDefaultTable(), [][]any{{int64(10)}})
			require.Error(t, err)
			if failure == "session_rollback_failed" {
				require.ErrorContains(t, err, "默认值需随整表补迁恢复")
			}
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestNativeCopyRejectsChangedDefaultBeforeWrites(t *testing.T) {
	w, m := mockNativeDM(t)
	expectCopyDefaultState(m, copyDefaultDDL, `"MixedOwner"."Other".NEXTVAL`)
	err := w.CopyDamengNativeBatch(context.Background(), nativeDefaultTable(), [][]any{{int64(10)}})
	require.ErrorContains(t, err, "偏离冻结原生计划")
	require.NoError(t, m.ExpectationsWereMet())
}

func TestNativeCopyResumesMissingDefaultFromInterruptedBatch(t *testing.T) {
	w, m := mockNativeDM(t)
	expectCopyDefaultState(m, `CREATE TABLE "MixedOwner"."T"("V" INT NOT NULL)`, nil)
	m.ExpectBegin()
	m.ExpectPrepare(regexp.QuoteMeta(`INSERT INTO "MixedOwner"."T" ("V") VALUES (?)`)).ExpectExec().WithArgs(int64(10)).WillReturnResult(sqlmock.NewResult(0, 1))
	m.ExpectCommit()
	expectCopyDefaultRestored(m)
	require.NoError(t, w.CopyDamengNativeBatch(context.Background(), nativeDefaultTable(), [][]any{{int64(10)}}))
	require.NoError(t, m.ExpectationsWereMet())
}

func TestNativeCopyDefaultCleanupUsesIndependentContextAndReportsFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancelled_context", true: "cleanup_failure"}[fail], func(t *testing.T) {
			w, m := mockNativeDM(t)
			conn, err := w.db.Conn(context.Background())
			require.NoError(t, err)
			defer conn.Close()
			g := &nativeCopyDefaults{conn: conn, schema: w.schema, table: "T", defaults: []nativeCopyDefault{{"V", `"MixedOwner"."Seq".NEXTVAL`}}}
			if fail {
				m.ExpectExec(regexp.QuoteMeta(`SET SCHEMA "MixedOwner"`)).WillReturnResult(sqlmock.NewResult(0, 0))
				m.ExpectExec(`ALTER TABLE .* SET DEFAULT ON NULL`).WillReturnError(errors.New("connection lost"))
				m.ExpectClose()
			} else {
				expectCopyDefaultRestored(m)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			err = g.restore(ctx)
			if fail {
				require.ErrorContains(t, err, "按整表计划补迁")
			} else {
				require.NoError(t, err)
			}
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestNativeCopyDefaultPreflightRequiresCrossSchemaAlterPrivilege(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		w, m := mockNativeDM(t)
		m.ExpectQuery(`SELECT USER FROM DUAL`).WillReturnRows(sqlmock.NewRows([]string{"user"}).AddRow("Operator"))
		m.ExpectQuery(`SELECT COUNT\(\*\) FROM SYS.SYSOBJECTS s JOIN SYS.SYSOBJECTS u`).WithArgs(w.schema, "Operator").WillReturnRows(sqlmock.NewRows([]string{"owner"}).AddRow(0))
		grants := sqlmock.NewRows([]string{"privilege"}).AddRow("CREATE ANY TABLE")
		if allowed {
			grants.AddRow("ALTER ANY TABLE")
		}
		m.ExpectQuery(`SELECT PRIVILEGE FROM SESSION_PRIVS`).WillReturnRows(grants)
		issues := w.ValidateDamengNativeObjects(context.Background(), []damengnative.Object{{Kind: "table", Name: "T", Schema: w.schema, Units: []damengnative.Unit{{SQL: copyDefaultDDL, Phase: "before_data"}}}})
		if allowed {
			require.Empty(t, issues)
		} else {
			require.Len(t, issues, 1)
			require.Contains(t, issues[0].Reason, "DEFAULT ON NULL")
		}
		require.NoError(t, m.ExpectationsWereMet(), "permission validation must remain read-only")
	}
}
