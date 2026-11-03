package target

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestMySQLIndexKeyLimits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		page   int
		large  bool
		format string
		want   int
	}{
		{"4K", 4096, true, "DYNAMIC", 768},
		{"8K", 8192, true, "DYNAMIC", 1536},
		{"16K", 16384, true, "DYNAMIC", 3072},
		{"32K", 32768, true, "DYNAMIC", 3072},
		{"64K", 65536, true, "DYNAMIC", 3072},
		{"compressed", 16384, true, "COMPRESSED", 3072},
		{"compact", 16384, true, " compact ", 767},
		{"redundant", 8192, true, "REDUNDANT", 767},
		{"large prefix off", 16384, false, "DYNAMIC", 767},
		{"small legacy page", 4096, false, "DYNAMIC", 767},
		{"missing page", 0, true, "DYNAMIC", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, (MySQLIndexProfile{PageSize: tc.page, LargePrefix: tc.large}).KeyLimit(tc.format))
		})
	}
}

func expectMySQLIndexPage(mock sqlmock.Sqlmock, page int) {
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT @@innodb_page_size`)).WillReturnRows(sqlmock.NewRows([]string{"page"}).AddRow(page))
}

func TestInspectMySQLIndexProfileReadsOnlyIndexCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows *sqlmock.Rows
		want bool
	}{
		{"removed variable", sqlmock.NewRows([]string{"Variable_name", "Value"}), true},
		{"enabled", sqlmock.NewRows([]string{"Variable_name", "Value"}).AddRow("innodb_large_prefix", "ON"), true},
		{"legacy off", sqlmock.NewRows([]string{"Variable_name", "Value"}).AddRow("innodb_large_prefix", "OFF"), false},
		{"numeric enabled", sqlmock.NewRows([]string{"Variable_name", "Value"}).AddRow("innodb_large_prefix", "1"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, mock := oracleMySQLDependencyMock(t, "app")
			expectMySQLIndexPage(mock, 16384)
			mock.ExpectQuery(regexp.QuoteMeta(`SHOW VARIABLES LIKE 'innodb_large_prefix'`)).WillReturnRows(tc.rows)
			got, err := w.InspectMySQLIndexProfile(context.Background())
			require.NoError(t, err)
			require.Equal(t, MySQLIndexProfile{PageSize: 16384, LargePrefix: tc.want}, got)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestInspectMySQLIndexProfileRejectsIncompleteMetadata(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows *sqlmock.Rows
	}{
		{"unknown value", sqlmock.NewRows([]string{"Variable_name", "Value"}).AddRow("innodb_large_prefix", "perhaps")},
		{"unknown variable", sqlmock.NewRows([]string{"Variable_name", "Value"}).AddRow("other", "ON")},
		{"duplicate", sqlmock.NewRows([]string{"Variable_name", "Value"}).AddRow("innodb_large_prefix", "ON").AddRow("innodb_large_prefix", "ON")},
		{"read error", sqlmock.NewRows([]string{"Variable_name", "Value"}).AddRow("innodb_large_prefix", "ON").RowError(0, errors.New("catalog read interrupted"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, mock := oracleMySQLDependencyMock(t, "app")
			expectMySQLIndexPage(mock, 16384)
			mock.ExpectQuery(regexp.QuoteMeta(`SHOW VARIABLES LIKE 'innodb_large_prefix'`)).WillReturnRows(tc.rows)
			_, err := w.InspectMySQLIndexProfile(context.Background())
			require.Error(t, err)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
	for _, page := range []int{0, -4096, 1234} {
		w, mock := oracleMySQLDependencyMock(t, "app")
		expectMySQLIndexPage(mock, page)
		_, err := w.InspectMySQLIndexProfile(context.Background())
		require.ErrorContains(t, err, "innodb_page_size")
		require.NoError(t, mock.ExpectationsWereMet())
	}
	for _, step := range []string{"page", "prefix"} {
		t.Run(step+" query denied", func(t *testing.T) {
			w, mock := oracleMySQLDependencyMock(t, "app")
			if step == "page" {
				mock.ExpectQuery(regexp.QuoteMeta(`SELECT @@innodb_page_size`)).WillReturnError(errors.New("page denied"))
			} else {
				expectMySQLIndexPage(mock, 16384)
				mock.ExpectQuery(regexp.QuoteMeta(`SHOW VARIABLES LIKE 'innodb_large_prefix'`)).WillReturnError(errors.New("prefix denied"))
			}
			_, err := w.InspectMySQLIndexProfile(context.Background())
			require.ErrorContains(t, err, "denied")
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func mysqlIndexColumnRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"COLUMN_NAME", "COLUMN_TYPE", "CHARACTER_MAXIMUM_LENGTH", "NUMERIC_PRECISION", "NUMERIC_SCALE", "DATETIME_PRECISION", "MAXLEN"})
}

func expectMySQLIndexTable(mock sqlmock.Sqlmock, schema, table string) {
	mock.ExpectQuery(regexp.QuoteMeta(mysqlIndexTableQuery)).WithArgs(schema, table).
		WillReturnRows(sqlmock.NewRows([]string{"ENGINE", "ROW_FORMAT"}).AddRow("InnoDB", "Dynamic"))
}

func TestInspectMySQLIndexTableUsesActualTargetDeclarations(t *testing.T) {
	const schema, table = "Target'`", "Mixed`Table"
	w, mock := oracleMySQLDependencyMock(t, schema)
	expectMySQLIndexTable(mock, schema, table)
	mock.ExpectQuery(regexp.QuoteMeta(mysqlIndexColumnsQuery)).WithArgs(schema, table).WillReturnRows(mysqlIndexColumnRows().
		AddRow("Title", "varchar(1000)", 1000, nil, nil, nil, 4).
		AddRow("Legacy", "varchar(1000)", 1000, nil, nil, nil, 3).
		AddRow("ASCII", "char(20)", 20, nil, nil, nil, 1).
		AddRow("Data", "varbinary(2000)", 2000, nil, nil, nil, nil).
		AddRow("Body", "longtext", 1073741823, nil, nil, nil, 4).
		AddRow("BinaryBody", "longblob", 4294967295, nil, nil, nil, nil).
		AddRow("Amount", "decimal(18,4) unsigned", nil, 18, 4, nil, nil).
		AddRow("Created", "datetime(6)", nil, nil, nil, 6, nil).
		AddRow("ID", "int unsigned", nil, 10, 0, nil, nil))
	got, err := w.InspectMySQLIndexTable(context.Background(), table)
	require.NoError(t, err)
	require.Equal(t, "InnoDB", got.Engine)
	require.Equal(t, "Dynamic", got.RowFormat)
	require.Equal(t, []MySQLIndexColumn{
		{Name: "Title", DataType: "varchar(1000)", Length: 1000, MaxBytes: 4},
		{Name: "Legacy", DataType: "varchar(1000)", Length: 1000, MaxBytes: 3},
		{Name: "ASCII", DataType: "char(20)", Length: 20, MaxBytes: 1},
		{Name: "Data", DataType: "varbinary(2000)", Length: 2000, MaxBytes: 1},
		{Name: "Body", DataType: "longtext", Length: 1073741823, MaxBytes: 4},
		{Name: "BinaryBody", DataType: "longblob", Length: 4294967295, MaxBytes: 1},
		{Name: "Amount", DataType: "decimal(18,4) unsigned", Precision: 18, Scale: 4},
		{Name: "Created", DataType: "datetime(6)", DateTimePrecision: 6},
		{Name: "ID", DataType: "int unsigned", Precision: 10},
	}, got.Columns)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestInspectMySQLIndexTableResolvesDefaultDatabaseReadOnly(t *testing.T) {
	w, mock := oracleMySQLDependencyMock(t, "")
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT DATABASE()`)).WillReturnRows(sqlmock.NewRows([]string{"DATABASE()"}).AddRow("FromDSN"))
	expectMySQLIndexTable(mock, "FromDSN", "T")
	mock.ExpectQuery(regexp.QuoteMeta(mysqlIndexColumnsQuery)).WithArgs("FromDSN", "T").WillReturnRows(mysqlIndexColumnRows().AddRow("ID", "int", nil, 10, 0, nil, nil))
	_, err := w.InspectMySQLIndexTable(context.Background(), "T")
	require.NoError(t, err)
	require.Empty(t, w.schema)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestInspectMySQLIndexTableLeavesOtherEnginesUnchanged(t *testing.T) {
	w, mock := oracleMySQLDependencyMock(t, "app")
	mock.ExpectQuery(regexp.QuoteMeta(mysqlIndexTableQuery)).WithArgs("app", "T").
		WillReturnRows(sqlmock.NewRows([]string{"ENGINE", "ROW_FORMAT"}).AddRow("MyISAM", "Dynamic"))
	got, err := w.InspectMySQLIndexTable(context.Background(), "T")
	require.NoError(t, err)
	require.Equal(t, MySQLIndexTable{Engine: "MyISAM", RowFormat: "Dynamic"}, got)
	require.NoError(t, mock.ExpectationsWereMet(), "other engines require no column catalog queries")
}

func TestInspectMySQLIndexTableLeavesUnknownTypesToExecution(t *testing.T) {
	w, mock := oracleMySQLDependencyMock(t, "app")
	expectMySQLIndexTable(mock, "app", "T")
	mock.ExpectQuery(regexp.QuoteMeta(mysqlIndexColumnsQuery)).WithArgs("app", "T").WillReturnRows(mysqlIndexColumnRows().
		AddRow("Choice", "enum('One','Two')", 3, nil, nil, nil, nil).
		AddRow("Choices", "set('A','B')", 3, nil, nil, nil, nil).
		AddRow("Shape", "geometry", nil, nil, nil, nil, nil))
	got, err := w.InspectMySQLIndexTable(context.Background(), "T")
	require.NoError(t, err)
	require.Len(t, got.Columns, 3)
	require.Equal(t, "enum('One','Two')", got.Columns[0].DataType)
	require.Equal(t, "geometry", got.Columns[2].DataType)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestInspectMySQLIndexTableRejectsIncompleteColumns(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows *sqlmock.Rows
	}{
		{"missing columns", mysqlIndexColumnRows()},
		{"missing name", mysqlIndexColumnRows().AddRow("", "int", nil, 10, 0, nil, nil)},
		{"missing type", mysqlIndexColumnRows().AddRow("ID", "", nil, 10, 0, nil, nil)},
		{"duplicate name", mysqlIndexColumnRows().AddRow("ID", "int", nil, 10, 0, nil, nil).AddRow("id", "int", nil, 10, 0, nil, nil)},
		{"missing charset", mysqlIndexColumnRows().AddRow("Title", "varchar(1000)", 1000, nil, nil, nil, nil)},
		{"missing text length", mysqlIndexColumnRows().AddRow("Title", "varchar(1000)", nil, nil, nil, nil, 4)},
		{"negative length", mysqlIndexColumnRows().AddRow("Title", "varchar(1000)", -1, nil, nil, nil, 4)},
		{"missing binary length", mysqlIndexColumnRows().AddRow("Data", "varbinary(10)", nil, nil, nil, nil, nil)},
		{"missing numeric precision", mysqlIndexColumnRows().AddRow("Amount", "decimal(10,2)", nil, nil, 2, nil, nil)},
		{"invalid scale", mysqlIndexColumnRows().AddRow("Amount", "decimal(10,2)", nil, 10, 11, nil, nil)},
		{"row read failure", mysqlIndexColumnRows().AddRow("ID", "int", nil, 10, 0, nil, nil).RowError(0, errors.New("catalog interrupted"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, mock := oracleMySQLDependencyMock(t, "app")
			expectMySQLIndexTable(mock, "app", "T")
			mock.ExpectQuery(regexp.QuoteMeta(mysqlIndexColumnsQuery)).WithArgs("app", "T").WillReturnRows(tc.rows)
			_, err := w.InspectMySQLIndexTable(context.Background(), "T")
			require.Error(t, err)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestInspectMySQLIndexTableRejectsMissingTableMetadata(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows *sqlmock.Rows
	}{
		{"absent table", sqlmock.NewRows([]string{"ENGINE", "ROW_FORMAT"})},
		{"missing engine", sqlmock.NewRows([]string{"ENGINE", "ROW_FORMAT"}).AddRow(nil, "Dynamic")},
		{"missing format", sqlmock.NewRows([]string{"ENGINE", "ROW_FORMAT"}).AddRow("InnoDB", nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, mock := oracleMySQLDependencyMock(t, "app")
			mock.ExpectQuery(regexp.QuoteMeta(mysqlIndexTableQuery)).WithArgs("app", "T").WillReturnRows(tc.rows)
			_, err := w.InspectMySQLIndexTable(context.Background(), "T")
			require.Error(t, err)
			if tc.name == "absent table" {
				require.ErrorIs(t, err, sql.ErrNoRows)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
	for _, step := range []string{"table", "columns"} {
		t.Run(step+" query denied", func(t *testing.T) {
			w, mock := oracleMySQLDependencyMock(t, "app")
			if step == "table" {
				mock.ExpectQuery(regexp.QuoteMeta(mysqlIndexTableQuery)).WithArgs("app", "T").WillReturnError(errors.New("table denied"))
			} else {
				expectMySQLIndexTable(mock, "app", "T")
				mock.ExpectQuery(regexp.QuoteMeta(mysqlIndexColumnsQuery)).WithArgs("app", "T").WillReturnError(errors.New("columns denied"))
			}
			_, err := w.InspectMySQLIndexTable(context.Background(), "T")
			require.ErrorContains(t, err, "denied")
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
