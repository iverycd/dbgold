package target

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"dbgold/datamigrate/source"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestOscarMySQLConfigureResolvesTargetAndCase(t *testing.T) {
	w, mock := oracleAutoWriter(t)
	w.schema = ""
	query := regexp.QuoteMeta(`SELECT VERSION(),@@version_comment,@@lower_case_table_names,DATABASE(),@@character_set_connection,@@max_allowed_packet`)
	columns := []string{"version", "comment", "mode", "db", "charset", "packet"}
	for _, tc := range []struct {
		version string
		mode    int
		key     string
	}{{"5.7.44", 1, "mixed"}, {"8.0.36", 0, "MiXeD"}} {
		mock.ExpectQuery(query).WillReturnRows(sqlmock.NewRows(columns).AddRow(tc.version, "MySQL Community Server", tc.mode, "dst", "utf8mb4", int64(4194304)))
		require.NoError(t, w.ConfigureOscarMigration(context.Background()))
		require.Equal(t, tc.key, w.MySQLTableNameKey("MiXeD"))
		require.Equal(t, "dst", w.schema)
		require.Equal(t, tc.version, w.OscarMySQLVersion())
	}
	mock.ExpectQuery(query).WillReturnRows(sqlmock.NewRows(columns).AddRow("8.4.0", "MySQL Community Server", 0, "dst", "utf8mb4", int64(4194304)))
	require.ErrorContains(t, w.ConfigureOscarMigration(context.Background()), "5.7 / 8.0")
}

func TestOscarMySQLCopyUsesStrictSessionAndRestoresMode(t *testing.T) {
	w, mock := oracleAutoWriter(t)
	w.SetSourceType("oscar")
	mock.ExpectQuery("SELECT @@SESSION.sql_mode").WillReturnRows(sqlmock.NewRows([]string{"mode"}).AddRow("ANSI_QUOTES"))
	mock.ExpectExec(regexp.QuoteMeta("SET SESSION sql_mode=?")).WithArgs("ANSI_QUOTES,STRICT_ALL_TABLES,NO_AUTO_VALUE_ON_ZERO,NO_BACKSLASH_ESCAPES").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectBegin()
	statement := mock.ExpectPrepare(regexp.QuoteMeta("INSERT INTO `dst`.`T` (`Id`, `D`) VALUES (?, ?)"))
	statement.ExpectExec().WithArgs(int64(0), "9007199254740993.1234").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectExec(regexp.QuoteMeta("SET SESSION sql_mode=?")).WithArgs("ANSI_QUOTES").WillReturnResult(sqlmock.NewResult(0, 0))
	require.NoError(t, w.CopyData(context.Background(), "T", []string{"Id", "D"}, []string{"INTEGER", "NUMERIC"}, [][]any{{int64(0), "9007199254740993.1234"}}))
}

func TestOscarMySQLAutoCalibrationUsesLiveCounterAndRows(t *testing.T) {
	w, mock := oracleAutoWriter(t)
	w.SetSourceType("oscar")
	sequence := source.SequenceInfo{TableName: "T", ColumnName: "Id", StartValueText: "10"}
	mock.ExpectQuery("SELECT @@GLOBAL.auto_increment_increment,@@GLOBAL.auto_increment_offset").WillReturnRows(sqlmock.NewRows([]string{"increment", "offset"}).AddRow(1, 1))
	expectAutoColumn(mock, "bigint", "auto_increment")
	expectAutoState(mock, "400", "999")
	mock.ExpectExec(regexp.QuoteMeta("ALTER TABLE `dst`.`T` AUTO_INCREMENT = 1000")).WillReturnResult(sqlmock.NewResult(0, 0))
	expectAutoCounter(mock, "1000")
	require.NoError(t, w.CreateSequence(context.Background(), sequence, ""))
	sequence.StartValueText = "128"
	require.ErrorContains(t, w.ValidateOscarMySQLIdentity(context.Background(), sequence, "tinyint"), "超出")
	require.ErrorContains(t, w.CreateComment(context.Background(), source.CommentInfo{TableName: "T", ColumnName: "Id", Comment: strings.Repeat("字", 1025)}), "1024")
}
