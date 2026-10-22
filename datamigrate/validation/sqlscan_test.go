package validation

import (
	"context"
	"database/sql/driver"
	"io"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func expectMySQLDescribe(mock sqlmock.Sqlmock, columns [][]driver.Value) {
	rows := sqlmock.NewRows([]string{"COLUMN_NAME", "DATA_TYPE", "precision", "scale", "fsp", "length"})
	for _, c := range columns {
		rows.AddRow(c...)
	}
	mock.ExpectQuery("SELECT COLUMN_NAME, DATA_TYPE").WithArgs("MiXeD", "Rows").WillReturnRows(rows)
	mock.ExpectQuery("SELECT COLUMN_NAME FROM information_schema.KEY_COLUMN_USAGE").WithArgs("MiXeD", "Rows").WillReturnRows(sqlmock.NewRows([]string{"COLUMN_NAME"}).AddRow("Id"))
}
func TestMySQLValidationScanPreservesOriginalValues(t *testing.T) {
	db, mock, e := sqlmock.New()
	require.NoError(t, e)
	defer db.Close()
	cols := [][]driver.Value{{"Id", "bigint", 20, 0, 0, 0}, {"At", "datetime", 0, 0, 6, 0}, {"Money", "decimal", 38, 10, 0, 0}, {"Text", "varchar", 0, 0, 0, 40}, {"Bits", "bit", 9, 0, 0, 0}}
	expectMySQLDescribe(mock, cols)
	s := NewSQLScanner(db, "mysql", "MiXeD")
	meta, e := s.DescribeValidationTable(context.Background(), "Rows")
	require.NoError(t, e)
	require.Equal(t, []string{"Id"}, meta.PrimaryKey)
	expectMySQLDescribe(mock, cols)
	mock.ExpectQuery("SELECT CAST.*LPAD.*FROM `MiXeD`.`Rows`").WillReturnRows(sqlmock.NewRows([]string{"Id", "At", "Money", "Text", "Bits"}).AddRow("18446744073709551615", "2024-00-12 11:12:13.000001", "12345678901234567890.1234567890", []byte("x\x00y "), "000000101"))
	stream, e := s.OpenValidationScan(context.Background(), "Rows", meta.Columns)
	require.NoError(t, e)
	row, e := stream.Next(context.Background())
	require.NoError(t, e)
	require.Equal(t, "2024-00-12 11:12:13.000001", row[1])
	require.Equal(t, []byte("x\x00y "), row[3])
	require.Equal(t, "000000101", row[4])
	_, e = stream.Next(context.Background())
	require.ErrorIs(t, e, io.EOF)
	require.NoError(t, stream.Close())
	require.NoError(t, mock.ExpectationsWereMet())
}
func TestValidationColumnClassificationsAreExplicit(t *testing.T) {
	cases := []struct {
		family, native string
		kind           Kind
		bits           int
	}{{"oracle", "DATE", Timestamp, 0}, {"oracle", "BINARY_FLOAT", Float, 32}, {"oracle", "SDO_GEOMETRY", Unsupported, 0}, {"postgres", "_int4", Unsupported, 0}, {"postgres", "timestamp with time zone", TimestampTZ, 0}, {"sqlserver", "timestamp", Binary, 0}, {"sqlserver", "bit", Bool, 0}, {"mysql", "float", Float, 32}, {"mysql", "double", Float, 64}, {"oscar", "DECIMAL", Number, 0}}
	for _, tt := range cases {
		t.Run(tt.family+tt.native, func(t *testing.T) {
			c := Column{DatabaseType: tt.native}
			ClassifyColumn(tt.family, &c)
			require.Equal(t, tt.kind, c.Kind)
			require.Equal(t, tt.bits, c.FloatBits)
		})
	}
}
func TestValidationSQLServerRejectsAmbiguousSchemas(t *testing.T) {
	db, mock, e := sqlmock.New()
	require.NoError(t, e)
	defer db.Close()
	mock.ExpectQuery("SELECT s.name FROM sys.tables").WithArgs("Same").WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("dbo").AddRow("other"))
	_, e = NewSQLScanner(db, "sqlserver", "").DescribeValidationTable(context.Background(), "Same")
	require.ErrorContains(t, e, "2 SQL Server schemas")
	require.NoError(t, mock.ExpectationsWereMet())
}
func TestValidationProjectionDoesNotCleanCharactersOrRoundNumbers(t *testing.T) {
	q, e := ValidationProjection("oracle", Column{Name: `Mi"xed`, DatabaseType: "NUMBER", Kind: Number})
	require.NoError(t, e)
	require.Contains(t, q, `"Mi""xed"`)
	require.Contains(t, q, "TM9")
	require.NotContains(t, strings.ToUpper(q), "ROUND")
	q, e = ValidationProjection("mysql", Column{Name: "Date", DatabaseType: "datetime", Kind: Timestamp})
	require.NoError(t, e)
	require.Equal(t, "CAST(`Date` AS CHAR)", q)
	_, e = ValidationProjection("postgres", Column{Name: "Shape", Kind: Unsupported})
	require.ErrorContains(t, e, "no lossless")
}

func TestSQLServerUnicodeProjectionReadsRawStoredUnits(t *testing.T) {
	for _, native := range []string{"nchar", "nvarchar", "ntext"} {
		c := Column{Name: "Original", DatabaseType: native, Kind: Text}
		q, err := ValidationProjection("sqlserver", c)
		require.NoError(t, err)
		expected := "CONVERT(varbinary(max),[Original])"
		if native == "ntext" {
			expected = "CONVERT(varbinary(max),CAST([Original] AS nvarchar(max)))"
		}
		require.Equal(t, expected, q)
	}
}
func sqlServerValidationTestStream(t *testing.T, native string, value driver.Value) *sqlValidationStream {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	mock.ExpectQuery("SELECT original").WillReturnRows(sqlmock.NewRows([]string{"Original"}).AddRow(value))
	conn, err := db.Conn(context.Background())
	require.NoError(t, err)
	rows, err := conn.QueryContext(context.Background(), "SELECT original")
	require.NoError(t, err)
	stream := &sqlValidationStream{rows: rows, conn: conn, cleanup: func() {}, columns: 1, family: "sqlserver", nativeColumns: []Column{{Name: "Original", Kind: Text, DatabaseType: native}}}
	t.Cleanup(func() { stream.Close(); require.NoError(t, mock.ExpectationsWereMet()) })
	return stream
}
func TestSQLServerUnicodeRawRowPreservesEmojiNULTrailingSpace(t *testing.T) {
	// A, U+1F600, NUL, a genuine U+FFFD, and a trailing space.
	raw := []byte{0x41, 0, 0x3d, 0xd8, 0, 0xde, 0, 0, 0xfd, 0xff, 0x20, 0}
	for _, native := range []string{"nchar", "nvarchar", "ntext"} {
		t.Run(native, func(t *testing.T) {
			s := sqlServerValidationTestStream(t, native, raw)
			row, err := s.Next(context.Background())
			require.NoError(t, err)
			require.Equal(t, "A😀\x00� ", row[0])
		})
	}
}
func TestSQLServerUnicodeRawRowRejectsLossyInputs(t *testing.T) {
	for _, raw := range [][]byte{{0x41}, {0, 0xd8}, {0, 0xdc}, {0, 0xd8, 0x41, 0}, {0, 0xdc, 0, 0xd8}} {
		s := sqlServerValidationTestStream(t, "nvarchar", raw)
		_, err := s.Next(context.Background())
		require.Error(t, err)
		require.Contains(t, err.Error(), "UTF-16LE")
	}
	s := sqlServerValidationTestStream(t, "nvarchar", nil)
	row, err := s.Next(context.Background())
	require.NoError(t, err)
	require.Nil(t, row[0])
}
func TestSQLServerNonUnicodeReplacementCannotProveLosslessRead(t *testing.T) {
	for _, native := range []string{"char", "varchar", "text"} {
		t.Run(native, func(t *testing.T) {
			s := sqlServerValidationTestStream(t, native, "original� ")
			_, err := s.Next(context.Background())
			require.ErrorContains(t, err, "驱动可能已替换原始字节")
		})
	}
	s := sqlServerValidationTestStream(t, "varchar", "original\x00 ")
	row, err := s.Next(context.Background())
	require.NoError(t, err)
	require.Equal(t, "original\x00 ", row[0])
}
