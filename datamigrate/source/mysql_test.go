package source

import (
	"context"
	"regexp"
	"slices"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestMySQLReadPageQuotesPrimaryKeys(t *testing.T) {
	for _, tc := range []struct {
		table string
		keys  []string
		query string
	}{
		{"ceshi123", []string{"23"},
			"SELECT t.* FROM (SELECT `23` FROM `ceshi123` ORDER BY `23` LIMIT 2, 2) temp " +
				"LEFT JOIN `ceshi123` t ON temp.`23` = t.`23`"},
		{"Mix`Table", []string{"Shard`ID", "23"},
			"SELECT t.* FROM (SELECT `Shard``ID`, `23` FROM `Mix``Table` ORDER BY `Shard``ID`, `23` LIMIT 2, 2) temp " +
				"LEFT JOIN `Mix``Table` t ON temp.`Shard``ID` = t.`Shard``ID` AND temp.`23` = t.`23`"},
	} {
		t.Run(tc.table, func(t *testing.T) {
			db, mock := partitionCatalogMock(t)
			mock.ExpectQuery(regexp.QuoteMeta(tc.query)).WillReturnRows(sqlmock.NewRows(tc.keys))
			keys := slices.Clone(tc.keys)
			r := &MySQLReader{db: db}
			columns, _, _, err := r.ReadPage(t.Context(), tc.table, keys, 2, 2)
			require.NoError(t, err)
			require.Equal(t, tc.keys, columns)
			require.Equal(t, tc.keys, keys)
		})
	}
}

func TestMySQLCountRowsQuotesTable(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM `Mix``Table`")).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(5))
	count, err := (&MySQLReader{db: db}).CountRows(t.Context(), "Mix`Table")
	require.NoError(t, err)
	require.EqualValues(t, 5, count)
}

func TestNormalizeMySQLValues(t *testing.T) {
	values := []interface{}{[]byte("text\x00value"), []byte{0, 1}, []byte{2}, int64(3)}
	normalizeMySQLValues(values, []string{"VARCHAR", "BLOB", "BIT", "BIGINT"})
	if values[0] != "textvalue" {
		t.Fatalf("text value=%q", values[0])
	}
	if _, ok := values[1].([]byte); !ok {
		t.Fatal("BLOB must remain []byte")
	}
	if _, ok := values[2].([]byte); !ok {
		t.Fatal("BIT must remain []byte")
	}
}

func TestMySQLTableInfoPreservesTemporalPrecision(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows := sqlmock.NewRows([]string{"COLUMN_NAME", "DATA_TYPE", "CHARACTER_MAXIMUM_LENGTH", "NUMERIC_PRECISION", "NUMERIC_SCALE", "IS_NULLABLE", "COLUMN_DEFAULT", "EXTRA", "DATETIME_PRECISION", "COLUMN_TYPE"}).
		AddRow("OccurredAt", "datetime", nil, nil, nil, "NO", nil, "", 6, "datetime(6)").
		AddRow("UpdatedAt", "timestamp", nil, nil, nil, "YES", "CURRENT_TIMESTAMP", "DEFAULT_GENERATED", 0, "timestamp").
		AddRow("Label", "varchar", 80, nil, nil, "YES", nil, "", nil, "varchar(80)")
	mock.ExpectQuery(`SELECT COLUMN_NAME, DATA_TYPE, CHARACTER_MAXIMUM_LENGTH, NUMERIC_PRECISION, NUMERIC_SCALE, IS_NULLABLE, COLUMN_DEFAULT, EXTRA, DATETIME_PRECISION`).WithArgs("SourceDB", "MixedTable").WillReturnRows(rows).RowsWillBeClosed()
	r := &MySQLReader{db: db, dbName: "SourceDB"}
	got, err := r.GetTableDDLInfo(context.Background(), "MixedTable")
	if err != nil {
		t.Fatal(err)
	}
	if got.TableName != "MixedTable" || len(got.Columns) != 3 || got.Columns[0].Name != "OccurredAt" || got.Columns[0].DateTimePrecision != 6 || got.Columns[1].DateTimePrecision != 0 || got.Columns[2].DateTimePrecision != 0 {
		t.Fatalf("metadata: %+v", got)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLTableInfoPreservesFloatingDeclarations(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows := sqlmock.NewRows([]string{"COLUMN_NAME", "DATA_TYPE", "CHARACTER_MAXIMUM_LENGTH", "NUMERIC_PRECISION", "NUMERIC_SCALE", "IS_NULLABLE", "COLUMN_DEFAULT", "EXTRA", "DATETIME_PRECISION", "COLUMN_TYPE"}).
		AddRow("SingleValue", "float", nil, 12, nil, "YES", nil, "", nil, "float").
		AddRow("DoubleValue", "double", nil, 22, nil, "YES", nil, "", nil, "double").
		AddRow("RoundedSingle", "float", nil, 8, 2, "YES", nil, "", nil, "float(8,2)").
		AddRow("RoundedDouble", "double", nil, 20, 0, "YES", nil, "", nil, "double(20,0)")
	mock.ExpectQuery(`SELECT COLUMN_NAME, DATA_TYPE, CHARACTER_MAXIMUM_LENGTH,.*COLUMN_TYPE`).WithArgs("SourceDB", "MixedTable").WillReturnRows(rows).RowsWillBeClosed()
	r := &MySQLReader{db: db, dbName: "SourceDB"}
	got, err := r.GetTableDDLInfo(context.Background(), "MixedTable")
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"float", "double", "float(8,2)", "double(20,0)"} {
		if got.Columns[i].FormattedType != want {
			t.Fatalf("column %s declaration = %q; want %q", got.Columns[i].Name, got.Columns[i].FormattedType, want)
		}
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
