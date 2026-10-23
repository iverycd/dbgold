package validation

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func damengValidationCatalogRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"COLUMN_NAME", "DATA_TYPE", "precision", "scale", "fsp", "length"})
}

func TestDamengValidationUsesActualFloatStorage(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	db.SetMaxOpenConns(1)
	catalog := damengValidationCatalogRows().
		AddRow("Id", "INTEGER", 10, 0, 0, 0).
		AddRow("Single", "REAL", 24, 0, 0, 0).
		AddRow("NativeSmall", "FLOAT", 24, 0, 0, 0).
		AddRow("NativeLarge", "FLOAT", 126, 0, 0, 0).
		AddRow("Wide", "DOUBLE PRECISION", 53, 0, 0, 0).
		AddRow(`Mode"Real`, "REAL", 24, 0, 0, 0).
		AddRow("ModeFloat", "FLOAT", 126, 0, 0, 0).
		AddRow("Amount", "DECIMAL", 38, 8, 0, 0)
	mock.ExpectQuery("SELECT COLUMN_NAME, DATA_TYPE").WithArgs(`Mixed"Owner`, "Rows").WillReturnRows(catalog).RowsWillBeClosed()
	query := `SELECT "Single","NativeSmall","NativeLarge","Wide","Mode""Real","ModeFloat" FROM "Mixed""Owner"."Rows" WHERE 1=0`
	mock.ExpectQuery(regexp.QuoteMeta(query)).WillReturnRows(sqlmock.NewRowsWithColumnDefinition(
		sqlmock.NewColumn("Single").OfType("REAL", float32(0)),
		sqlmock.NewColumn("NativeSmall").OfType("FLOAT", float32(0)),
		sqlmock.NewColumn("NativeLarge").OfType("FLOAT", float32(0)),
		sqlmock.NewColumn("Wide").OfType("DOUBLE", float64(0)),
		sqlmock.NewColumn(`Mode"Real`).OfType("REAL", "").WithPrecisionAndScale(8, 2),
		sqlmock.NewColumn("ModeFloat").OfType("FLOAT", "").WithPrecisionAndScale(38, 8),
	)).RowsWillBeClosed()
	mock.ExpectQuery("SELECT c.COLUMN_NAME FROM ALL_CONS_COLUMNS").WithArgs(`Mixed"Owner`, "Rows").WillReturnRows(sqlmock.NewRows([]string{"COLUMN_NAME"}).AddRow("Id")).RowsWillBeClosed()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	meta, err := NewSQLScanner(db, "dameng", `Mixed"Owner`).DescribeValidationTable(ctx, "Rows")
	require.NoError(t, err)
	require.Equal(t, []string{"Id"}, meta.PrimaryKey)
	for i, want := range []int{0, 32, 32, 64, 64, 0, 0, 0} {
		require.Equal(t, want, meta.Columns[i].FloatBits, meta.Columns[i].Name)
		kind := Number
		if want != 0 {
			kind = Float
		}
		require.Equal(t, kind, meta.Columns[i].Kind, meta.Columns[i].Name)
	}
	projection, err := ValidationProjection("dameng", meta.Columns[5])
	require.NoError(t, err)
	require.Equal(t, `CAST("Mode""Real" AS VARCHAR(256))`, projection)
	projection, err = ValidationProjection("dameng", meta.Columns[3])
	require.NoError(t, err)
	require.Equal(t, `"NativeLarge"`, projection)
	// NUMBER_MODE exceptions do not permit the validator to claim that a
	// rounded decimal value is the same as the original binary float.
	exact, err := canonicalNumber("0.1", meta.Columns[5])
	require.NoError(t, err)
	binary, err := canonicalNumber(float32(0.1), meta.Columns[1])
	require.NoError(t, err)
	require.NotEqual(t, exact, binary)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDamengValidationDoesNotProbeExactNumbers(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery("SELECT COLUMN_NAME, DATA_TYPE").WithArgs("Owner", "Rows").WillReturnRows(damengValidationCatalogRows().AddRow("Amount", "NUMBER", 20, 2, 0, 0))
	mock.ExpectQuery("SELECT c.COLUMN_NAME FROM ALL_CONS_COLUMNS").WithArgs("Owner", "Rows").WillReturnRows(sqlmock.NewRows([]string{"COLUMN_NAME"}))
	meta, err := NewSQLScanner(db, "dameng", "Owner").DescribeValidationTable(context.Background(), "Rows")
	require.NoError(t, err)
	require.Equal(t, Number, meta.Columns[0].Kind)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDamengValidationRejectsUnknownFloatDescriptors(t *testing.T) {
	for _, scenario := range []string{"query error", "unknown type", "wrong name", "wrong count"} {
		t.Run(scenario, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			mock.ExpectQuery("SELECT COLUMN_NAME, DATA_TYPE").WithArgs("Owner", "Rows").WillReturnRows(damengValidationCatalogRows().AddRow("Mixed", "REAL", 24, 0, 0, 0))
			expect := mock.ExpectQuery(regexp.QuoteMeta(`SELECT "Mixed" FROM "Owner"."Rows" WHERE 1=0`))
			switch scenario {
			case "query error":
				expect.WillReturnError(errors.New("descriptor unavailable"))
			case "unknown type":
				expect.WillReturnRows(sqlmock.NewRowsWithColumnDefinition(sqlmock.NewColumn("Mixed").OfType("VARCHAR", "")))
			case "wrong name":
				expect.WillReturnRows(sqlmock.NewRowsWithColumnDefinition(sqlmock.NewColumn("MIXED").OfType("REAL", float32(0))))
			case "wrong count":
				expect.WillReturnRows(sqlmock.NewRowsWithColumnDefinition(sqlmock.NewColumn("Mixed").OfType("REAL", float32(0)), sqlmock.NewColumn("Extra").OfType("REAL", float32(0))))
			}
			_, err = NewSQLScanner(db, "dameng", "Owner").DescribeValidationTable(context.Background(), "Rows")
			require.Error(t, err)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestFloatColumnClassificationRequiresRelevantPrecision(t *testing.T) {
	for _, tc := range []struct {
		family, native string
		precision      int64
		wantKind       Kind
		wantBits       int
	}{
		{"sqlserver", "float", 0, Float, 64},
		{"sqlserver", "float", 1, Float, 32},
		{"sqlserver", "float", 24, Float, 32},
		{"sqlserver", "float", 25, Float, 64},
		{"sqlserver", "float", 53, Float, 64},
		{"dameng", "REAL", 24, Unsupported, 0},
		{"dameng", "FLOAT", 24, Unsupported, 0},
		{"dameng", "DOUBLE", 53, Unsupported, 0},
		{"dameng", "DOUBLE PRECISION", 53, Unsupported, 0},
		{"oracle", "FLOAT", 126, Number, 0},
		{"oracle", "REAL", 63, Number, 0},
		{"oracle", "DOUBLE PRECISION", 126, Number, 0},
		{"oracle", "BINARY_FLOAT", 0, Float, 32},
		{"oracle", "BINARY_DOUBLE", 0, Float, 64},
	} {
		c := Column{DatabaseType: tc.native, Precision: tc.precision}
		ClassifyColumn(tc.family, &c)
		require.Equal(t, tc.wantKind, c.Kind, "%+v", tc)
		require.Equal(t, tc.wantBits, c.FloatBits, "%+v", tc)
	}
}
