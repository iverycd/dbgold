package target

import (
	"context"
	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"errors"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"regexp"
	"testing"
)

func sqlServerMock(t *testing.T) (*SQLServerWriter, sqlmock.Sqlmock) {
	t.Helper()
	db, m, e := sqlmock.New()
	require.NoError(t, e)
	w := &SQLServerWriter{db: db, schema: "dbo", dia: dialect.NewSQLServer()}
	w.dia.Tables["T"] = &source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{{Name: "Id", DataType: "int", Extra: "auto_increment"}, {Name: "Name", DataType: "varchar", Length: 20, IsNullable: true}}}
	t.Cleanup(func() { _ = db.Close() })
	return w, m
}
func TestSQLServerCopyBatchLimit(t *testing.T) {
	w, m := sqlServerMock(t)
	m.ExpectExec("SET ANSI_NULLS").WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectBegin()
	m.ExpectExec(regexp.QuoteMeta("SET IDENTITY_INSERT [dbo].[T] ON")).WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectExec(`INSERT INTO .*@p2000`).WillReturnResult(sqlmock.NewResult(0, 1000))
	m.ExpectExec(`INSERT INTO .*CAST\(@p2 AS NVARCHAR\(40\)\)\)$`).WithArgs(int64(1000), "n").WillReturnResult(sqlmock.NewResult(0, 1))
	m.ExpectExec(regexp.QuoteMeta("SET IDENTITY_INSERT [dbo].[T] OFF")).WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectCommit()
	m.ExpectExec(regexp.QuoteMeta("SET IDENTITY_INSERT [dbo].[T] OFF")).WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectClose()
	rows := make([][]interface{}, 1001)
	for i := range rows {
		rows[i] = []interface{}{i, "n"}
	}
	require.NoError(t, w.CopyData(context.Background(), "T", []string{"Id", "Name"}, nil, rows))
	require.NoError(t, m.ExpectationsWereMet())
}
func TestSQLServerCopyRollbackAndCleanup(t *testing.T) {
	for _, badValue := range []bool{false, true} {
		t.Run(map[bool]string{false: "server error", true: "conversion error"}[badValue], func(t *testing.T) {
			w, m := sqlServerMock(t)
			m.ExpectExec("SET ANSI_NULLS").WillReturnResult(sqlmock.NewResult(0, 0))
			m.ExpectBegin()
			m.ExpectExec("SET IDENTITY_INSERT .* ON").WillReturnResult(sqlmock.NewResult(0, 0))
			if !badValue {
				m.ExpectExec("INSERT INTO").WillReturnError(errors.New("write failed"))
			}
			m.ExpectRollback()
			m.ExpectExec("SET IDENTITY_INSERT .* OFF").WillReturnError(errors.New("cleanup failed"))
			m.ExpectClose()
			id := "1"
			if badValue {
				id = "99999999999999999999"
			}
			require.Error(t, w.CopyData(context.Background(), "T", []string{"Id", "Name"}, nil, [][]interface{}{{id, "n"}}))
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}
func TestSQLServerCancelledCopy(t *testing.T) {
	w, m := sqlServerMock(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, w.CopyData(ctx, "T", []string{"Id", "Name"}, nil, [][]interface{}{{1, "n"}}), context.Canceled)
	require.NoError(t, m.ExpectationsWereMet())
}
func TestSQLServerValidateExisting(t *testing.T) {
	for _, bad := range []bool{false, true} {
		w, m := sqlServerMock(t)
		length := 80
		if bad {
			length = 40
		}
		m.ExpectQuery("SELECT c.name,t.name").WithArgs("[dbo].[T]").WillReturnRows(sqlmock.NewRows([]string{"name", "type", "length", "p", "s", "null", "identity", "computed"}).AddRow("Id", "int", 4, 10, 0, false, true, false).AddRow("Name", "nvarchar", length, 0, 0, true, false, false))
		e := w.ValidateExisting(context.Background(), w.dia.Tables["T"])
		if bad {
			require.Error(t, e)
		} else {
			require.NoError(t, e)
		}
		require.NoError(t, m.ExpectationsWereMet())
	}
}
func TestSQLServerDependenciesAreBounded(t *testing.T) {
	w, m := sqlServerMock(t)
	m.ExpectQuery("SELECT ps.name").WillReturnRows(sqlmock.NewRows([]string{"ps", "p", "fk", "rs", "r"}).AddRow("dbo", "Outside", "FK", "dbo", "T"))
	ss, e := w.RebuildDependencies(context.Background(), map[string]bool{"T": true})
	require.Nil(t, ss)
	var dep *SQLServerDependencyError
	require.ErrorAs(t, e, &dep)
	require.Equal(t, "T", dep.Table)
	require.NoError(t, m.ExpectationsWereMet())
}
func TestSQLServerSeedErrorsReported(t *testing.T) {
	w, m := sqlServerMock(t)
	m.ExpectExec("SET ANSI_NULLS").WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectBegin()
	m.ExpectExec("DECLARE @next").WillReturnError(errors.New("reseed denied"))
	m.ExpectRollback()
	m.ExpectClose()
	require.ErrorContains(t, w.CreateSequence(context.Background(), source.SequenceInfo{TableName: "T", ColumnName: "Id", StartValue: 8}, ""), "reseed denied")
	require.NoError(t, m.ExpectationsWereMet())
}
func TestSQLServerDDLTransaction(t *testing.T) {
	w, m := sqlServerMock(t)
	m.ExpectExec("SET ANSI_NULLS").WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectBegin()
	m.ExpectExec("DROP TABLE").WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectExec("CREATE TABLE").WillReturnError(errors.New("invalid DDL"))
	m.ExpectRollback()
	m.ExpectClose()
	require.Error(t, w.CreateTable(context.Background(), []dialect.Statement{{SQL: "DROP TABLE [dbo].[T]"}, {SQL: "CREATE TABLE [dbo].[T] ()"}}))
	require.NoError(t, m.ExpectationsWereMet())
}
