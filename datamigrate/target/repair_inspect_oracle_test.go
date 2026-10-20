package target

import (
	"context"
	"testing"

	"dbgold/datamigrate/dialect"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestInspectOracleColumnsUsesVirtualColumnDictionary(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	w := newOracleWriter(db, "MixedOwner")
	// Match the complete dictionary contract: a prefix-only expectation would
	// accept the old invalid VIRTUAL_COLUMN FROM ALL_TAB_COLUMNS query.
	const query = `SELECT c.COLUMN_NAME,c.DATA_TYPE,COALESCE(c.CHAR_LENGTH,c.DATA_LENGTH),COALESCE(c.DATA_PRECISION,0),COALESCE(c.DATA_SCALE,0),CASE WHEN c.NULLABLE='Y' THEN 'YES' ELSE 'NO' END,c.DATA_DEFAULT,'',c.COLUMN_ID,COALESCE(c.DATA_SCALE,0),CASE WHEN vc.VIRTUAL_COLUMN='YES' THEN 'generated' ELSE '' END
 FROM ALL_TAB_COLUMNS c
 JOIN ALL_TAB_COLS vc ON vc.OWNER=c.OWNER AND vc.TABLE_NAME=c.TABLE_NAME AND vc.COLUMN_NAME=c.COLUMN_NAME
 WHERE c.OWNER=:1 AND c.TABLE_NAME=:2 ORDER BY c.COLUMN_ID`
	mock.ExpectQuery(query).WithArgs("MixedOwner", "MixedTable").WillReturnRows(
		sqlmock.NewRows([]string{"name", "type", "length", "precision", "scale", "nullable", "default", "extra", "ordinal", "datetime_precision", "generated"}).
			AddRow("MixedText", "VARCHAR2", 40, 0, 0, "YES", "'KeepCase'", "", 1, 0, "").
			AddRow("CalculatedValue", "NUMBER", 0, 12, 2, "YES", "1+1", "", 2, 2, "generated")).RowsWillBeClosed()
	cols, err := inspectColumns(context.Background(), w, "MixedTable")
	require.NoError(t, err)
	require.Len(t, cols, 2)
	require.Equal(t, "MixedText", cols[0].Name)
	require.Equal(t, "VARCHAR2(40)", cols[0].FormattedType)
	require.Equal(t, "'KeepCase'", *cols[0].Default)
	require.Empty(t, cols[0].Generated)
	require.Equal(t, 1, cols[0].Ordinal)
	require.Equal(t, "CalculatedValue", cols[1].Name)
	require.Equal(t, "generated", cols[1].Generated)
	require.Equal(t, 2, cols[1].Ordinal)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestInspectDamengColumnsKeepsExistingDictionaryQuery(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	w := &DaMengWriter{db: db, schema: "APP", dia: dialect.NewDaMeng()}
	const query = `SELECT COLUMN_NAME,DATA_TYPE,COALESCE(CHAR_LENGTH,DATA_LENGTH),COALESCE(DATA_PRECISION,0),COALESCE(DATA_SCALE,0),CASE WHEN NULLABLE='Y' THEN 'YES' ELSE 'NO' END,DATA_DEFAULT,'',COLUMN_ID,COALESCE(DATA_SCALE,0),CASE WHEN VIRTUAL_COLUMN='YES' THEN 'generated' ELSE '' END FROM ALL_TAB_COLUMNS WHERE OWNER=? AND TABLE_NAME=? ORDER BY COLUMN_ID`
	mock.ExpectQuery(query).WithArgs("APP", "T").WillReturnRows(sqlmock.NewRows([]string{"name", "type", "length", "precision", "scale", "nullable", "default", "extra", "ordinal", "datetime_precision", "generated"}).AddRow("ID", "NUMBER", 0, 10, 0, "NO", nil, "", 1, 0, "")).RowsWillBeClosed()
	cols, err := inspectColumns(context.Background(), w, "T")
	require.NoError(t, err)
	require.Len(t, cols, 1)
	require.Equal(t, "ID", cols[0].Name)
	require.Equal(t, "NUMBER(10,0)", cols[0].FormattedType)
	require.False(t, cols[0].IsNullable)
	require.NoError(t, mock.ExpectationsWereMet())
}
