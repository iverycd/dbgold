package source

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func damengFloatCatalogRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"COLUMN_NAME", "DATA_TYPE", "CHAR_LENGTH", "DATA_PRECISION", "DATA_SCALE", "NULLABLE", "DATA_DEFAULT", "IS_IDENTITY"})
}

func TestDamengReaderReadsActualFloatStorage(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	db.SetMaxOpenConns(1)
	reader := &DaMengReader{db: db, schema: `Mixed"Owner`}
	catalog := damengFloatCatalogRows().
		AddRow("Id", "INT", 0, 10, 0, "N", nil, 1).
		AddRow("Single", "FLOAT", 0, 24, 0, "Y", nil, 0).
		AddRow("Wide", "FLOAT", 0, 126, 0, "Y", nil, 0).
		AddRow("Exact", "FLOAT", 0, 126, 0, "Y", nil, 0).
		AddRow(`Real"Exact`, "REAL", 0, 24, 0, "Y", nil, 0).
		AddRow("PlainDouble", "DOUBLE PRECISION", 0, 0, 0, "Y", nil, 0).
		AddRow("Money", "DECIMAL", 0, 20, 4, "Y", nil, 0)
	mock.ExpectQuery("SELECT c.COLUMN_NAME").WithArgs(`Mixed"Owner`, "MixedTable").WillReturnRows(catalog).RowsWillBeClosed()
	query := `SELECT "Single","Wide","Exact","Real""Exact","PlainDouble" FROM "Mixed""Owner"."MixedTable" WHERE 1=0`
	mock.ExpectQuery(regexp.QuoteMeta(query)).WillReturnRows(sqlmock.NewRowsWithColumnDefinition(
		sqlmock.NewColumn("Single").OfType("FLOAT", float32(0)),
		sqlmock.NewColumn("Wide").OfType("FLOAT", float32(0)),
		sqlmock.NewColumn("Exact").OfType("FLOAT", "").WithPrecisionAndScale(38, 8),
		sqlmock.NewColumn(`Real"Exact`).OfType("REAL", "").WithPrecisionAndScale(8, 2),
		sqlmock.NewColumn("PlainDouble").OfType("DOUBLE", float64(0)),
	)).RowsWillBeClosed()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	info, err := reader.GetTableDDLInfo(ctx, "MixedTable")
	require.NoError(t, err)
	require.Equal(t, "MixedTable", info.TableName)
	for i, want := range []string{"", "real", "double", "decimal", "decimal", "double", ""} {
		require.Equal(t, want, info.Columns[i].DamengFloatStorage, info.Columns[i].Name)
	}
	require.Equal(t, `Real"Exact`, info.Columns[4].Name)
	require.Equal(t, "REAL", info.Columns[4].DataType)
	require.EqualValues(t, 126, info.Columns[2].Precision)
	require.Equal(t, "auto_increment", info.Columns[0].Extra)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDamengReaderDoesNotProbeExactNumbers(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery("SELECT c.COLUMN_NAME").WithArgs("Owner", "Rows").WillReturnRows(damengFloatCatalogRows().AddRow("Amount", "NUMBER", 0, 20, 2, "Y", nil, 0)).RowsWillBeClosed()
	info, err := (&DaMengReader{db: db, schema: "Owner"}).GetTableDDLInfo(context.Background(), "Rows")
	require.NoError(t, err)
	require.Empty(t, info.Columns[0].DamengFloatStorage)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDamengReaderRejectsUnknownFloatDescriptors(t *testing.T) {
	for _, scenario := range []string{"query error", "unknown type", "wrong name", "wrong count"} {
		t.Run(scenario, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			mock.ExpectQuery("SELECT c.COLUMN_NAME").WithArgs("Owner", "Rows").WillReturnRows(damengFloatCatalogRows().AddRow("Mixed", "FLOAT", 0, 53, 0, "Y", nil, 0))
			expect := mock.ExpectQuery(regexp.QuoteMeta(`SELECT "Mixed" FROM "Owner"."Rows" WHERE 1=0`))
			switch scenario {
			case "query error":
				expect.WillReturnError(errors.New("descriptor unavailable"))
			case "unknown type":
				expect.WillReturnRows(sqlmock.NewRowsWithColumnDefinition(sqlmock.NewColumn("Mixed").OfType("VARCHAR", "")))
			case "wrong name":
				expect.WillReturnRows(sqlmock.NewRowsWithColumnDefinition(sqlmock.NewColumn("MIXED").OfType("DOUBLE", float64(0))))
			case "wrong count":
				expect.WillReturnRows(sqlmock.NewRowsWithColumnDefinition(sqlmock.NewColumn("Mixed").OfType("DOUBLE", float64(0)), sqlmock.NewColumn("Extra").OfType("DOUBLE", float64(0))))
			}
			_, err = (&DaMengReader{db: db, schema: "Owner"}).GetTableDDLInfo(context.Background(), "Rows")
			require.Error(t, err)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
