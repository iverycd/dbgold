package damengnative

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestNativeGeneratedConstantRequiresExactNativeRoundTrip(t *testing.T) {
	for _, equal := range []int{0, 1} {
		db, m, err := sqlmock.New()
		require.NoError(t, err)
		r := nativeMVReader(db)
		m.ExpectQuery(`SELECT CAST\("DBGOLD_GENERATED_KEY" AS VARCHAR\(32767\)\) FROM`).WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow("29"))
		m.ExpectQuery(`SELECT CASE WHEN CAST\('29' AS INT\)=CAST\(30-1 AS INT\) THEN 1 ELSE 0 END FROM DUAL`).WillReturnRows(sqlmock.NewRows([]string{"equal"}).AddRow(equal))
		literal, err := r.freezeNativeGeneratedConstant(Column{DataType: "INT", SQLType: "INT"}, "CAST(30-1 AS INT)")
		if equal == 1 {
			require.NoError(t, err)
			require.Equal(t, "CAST('29' AS INT)", literal)
		} else {
			require.ErrorContains(t, err, "不能无损重建")
		}
		require.NoError(t, m.ExpectationsWereMet())
		db.Close()
	}
}
