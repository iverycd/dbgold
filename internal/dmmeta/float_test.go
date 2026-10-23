package dmmeta

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestFloatStorageUsesActualDescriptors(t *testing.T) {
	for _, tc := range []struct {
		name, native, want string
		precision          int64
		decimal            bool
		bad                bool
	}{
		{name: "real", native: "REAL", want: "real", precision: 126},
		{name: "double", native: "DOUBLE", want: "double", precision: 24},
		{name: "double alias", native: " DOUBLE   PRECISION ", want: "double"},
		{name: "binary float alias", native: "BINARY_FLOAT", want: "real"},
		{name: "binary double alias", native: "BINARY_DOUBLE", want: "double"},
		{name: "native float24", native: "FLOAT", want: "real", precision: 24},
		{name: "native float25", native: "FLOAT", want: "double", precision: 25},
		{name: "native float126", native: "FLOAT", want: "double", precision: 126},
		{name: "native default float", native: "FLOAT", want: "double"},
		{name: "decimal float", native: "FLOAT", want: "decimal", precision: 24, decimal: true},
		{name: "decimal real", native: "REAL", want: "decimal", precision: 24, decimal: true},
		{name: "decimal double", native: "DOUBLE", want: "decimal", precision: 53, decimal: true},
		{name: "number", native: "NUMBER", want: "decimal"},
		{name: "decimal", native: "DECIMAL", want: "decimal"},
		{name: "invalid precision", native: "FLOAT", precision: 127, bad: true},
		{name: "negative precision", native: "FLOAT", precision: -1, bad: true},
		{name: "unknown descriptor", native: "VARCHAR", bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			// In particular, the FLOAT descriptor claims float32 even for
			// binary64, reproducing DM's unreliable ScanType.
			definition := sqlmock.NewColumn("Original").OfType(tc.native, float32(0))
			if tc.decimal {
				definition.WithPrecisionAndScale(38, 8)
			}
			mock.ExpectQuery("SELECT").WillReturnRows(sqlmock.NewRowsWithColumnDefinition(definition)).RowsWillBeClosed()
			rows, err := db.Query("SELECT Original WHERE 1=0")
			require.NoError(t, err)
			columns, err := rows.ColumnTypes()
			require.NoError(t, err)
			require.NoError(t, rows.Close())
			kind, err := FloatStorage(columns[0], tc.precision)
			if tc.bad {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.want, kind)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
	_, err := FloatStorage(nil, 24)
	require.Error(t, err)
}

func TestIsFloatType(t *testing.T) {
	for _, name := range []string{"float", "FLOAT(24)", " REAL ", "DOUBLE", "double precision"} {
		require.True(t, IsFloatType(name), name)
	}
	for _, name := range []string{"NUMBER", "DECIMAL", "NUMERIC", "VARCHAR", "FLOATING"} {
		require.False(t, IsFloatType(name), name)
	}
}
