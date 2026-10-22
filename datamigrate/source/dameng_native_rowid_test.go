package source

import (
	"context"
	"regexp"
	"testing"

	"dbgold/datamigrate/damengnative"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestDamengNativeCaptureRowIDUsesTheContinuousDataCursor(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := &DaMengNativeReader{&DaMengReader{db: db, schema: "MixedOwner"}}
	table := damengnative.Table{Name: "Keyless", Schema: "MixedOwner", CaptureRowID: true, RowIDMapping: "RowMap", Columns: []damengnative.Column{{Name: "Value", DataType: "VARCHAR", Writable: true}, {Name: "Generated", DataType: "INT"}}}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT "Value" AS "Value", ROWIDTOCHAR(ROWID) AS "__DBGOLD_SOURCE_ROWID" FROM "MixedOwner"."Keyless"`)).
		WillReturnRows(sqlmock.NewRows([]string{"Value", "__DBGOLD_SOURCE_ROWID"}).AddRow("same\x00", "AAAAAAAAAAAAAAAAAB").AddRow("same\x00", "AAAAAAAAAAAAAAAAAC"))
	scan, err := r.OpenDamengNativeScan(context.Background(), table)
	require.NoError(t, err)
	defer scan.Close()
	for _, rid := range []string{"AAAAAAAAAAAAAAAAAB", "AAAAAAAAAAAAAAAAAC"} {
		require.True(t, scan.Next())
		values, err := scan.Values()
		require.NoError(t, err)
		require.Equal(t, []any{"same\x00", rid}, values)
	}
	require.False(t, scan.Next())
	require.NoError(t, scan.Err())
	require.Len(t, table.Columns, 2, "the transport address must not become a schema column")
	require.NoError(t, mock.ExpectationsWereMet())
}
