package target

import (
	"context"
	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func testSQLServerNativeSequence() source.SequenceInfo {
	return source.SequenceInfo{StartValueText: "4", Native: &source.NativeSequenceInfo{
		Name: "Shared]", Schema: "source", IncrementBy: "2", MinValue: "-50", MaxValue: "1000",
		Bindings: []source.SequenceBinding{{Kind: "default", TableName: "T", ColumnName: "Id"}},
	}}
}

func TestSQLServerNativeSequenceCreationRollsBackWithoutDefault(t *testing.T) {
	w, mock := sqlServerMock(t)
	s := testSQLServerNativeSequence()
	mock.ExpectExec("SET ANSI_NULLS").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectBegin()
	mock.ExpectQuery("FROM sys.sequences").WithArgs("[dbo].[Shared]]]", dialect.SQLServerSequenceMarker).WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery("FROM sys.columns c").WithArgs("[dbo].[T]", "Id", int64(0)).WillReturnRows(sqlmock.NewRows([]string{"type", "scale", "identity", "computed", "default", "proven"}).AddRow("bigint", 0, false, false, nil, false))
	mock.ExpectExec("CREATE SEQUENCE").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("sp_addextendedproperty").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("NEXT VALUE FOR").WillReturnError(context.DeadlineExceeded)
	mock.ExpectRollback()
	mock.ExpectClose()
	require.ErrorIs(t, w.CreateSequence(context.Background(), s, ""), context.DeadlineExceeded)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSQLServerNativeSequenceCalibratesOnlyWithVerifiedBinding(t *testing.T) {
	for _, direct := range []bool{true, false} {
		t.Run(map[bool]string{true: "valid", false: "wrong binding"}[direct], func(t *testing.T) {
			w, mock := sqlServerMock(t)
			s := testSQLServerNativeSequence()
			mock.ExpectExec("SET ANSI_NULLS").WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectBegin()
			mock.ExpectQuery("FROM sys.sequences").WithArgs("[dbo].[Shared]]]", dialect.SQLServerSequenceMarker).WillReturnRows(sqlmock.NewRows([]string{"id", "step", "min", "max", "current", "initial", "cycle", "exhausted", "cached", "cache", "marker"}).AddRow(7, "2", "-50", "1000", "8", "4", false, false, false, nil, nil))
			mock.ExpectQuery("FROM sys.columns c").WithArgs("[dbo].[T]", "Id", int64(7)).WillReturnRows(sqlmock.NewRows([]string{"type", "scale", "identity", "computed", "default", "proven"}).AddRow("bigint", 0, false, false, "(NEXT VALUE FOR [dbo].[Shared]]])", direct))
			if direct {
				mock.ExpectQuery(regexp.QuoteMeta("SELECT CONVERT(varchar(50),MAX([Id])) FROM [dbo].[T] WITH (TABLOCKX,HOLDLOCK)")).WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow("13"))
				mock.ExpectQuery("FROM sys.sql_expression_dependencies dep").WithArgs(int64(7)).WillReturnRows(sqlmock.NewRows([]string{"schema", "table", "col"}).AddRow("dbo", "T", "Id"))
				mock.ExpectExec("sys.sp_sequence_get_range").WithArgs("[dbo].[Shared]]]", int64(2)).WillReturnResult(sqlmock.NewResult(0, 0))
				mock.ExpectCommit()
			} else {
				mock.ExpectRollback()
			}
			mock.ExpectClose()
			err := CalibrateRecovery(context.Background(), w, "", []source.SequenceInfo{s})
			if direct {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "其他默认值")
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestSQLServerSequenceRepairRejectsEditedLedgerSQL(t *testing.T) {
	s := testSQLServerNativeSequence()
	obj := repair.Object{Kind: "sequence", Schema: "dbo", TargetName: s.Native.Name, Sequence: &s}
	for _, stmt := range dialect.NewSQLServer().SequenceStatements(obj.Schema, s, "") {
		obj.Units = append(obj.Units, repair.Unit{ID: stmt.SQL, SQL: stmt.SQL, OriginalSQL: stmt.SQL, Selected: true})
	}
	require.True(t, IsSQLServerSequenceRepair(obj, "sqlserver"))
	require.NoError(t, ValidateSQLServerSequenceRepair(obj, obj.Units, "sqlserver"))
	modified := append([]repair.Unit(nil), obj.Units...)
	modified[0].SQL = "DROP TABLE [dbo].[T]"
	require.ErrorContains(t, ValidateSQLServerSequenceRepair(obj, modified, "sqlserver"), "不支持编辑")
}
