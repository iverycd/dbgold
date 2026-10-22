package source

import (
	"context"
	"testing"

	"dbgold/datamigrate/damengnative"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestDaMengNativeStreamPreservesTextBinaryAndExactDecimal(t *testing.T) {
	db, m, e := sqlmock.New()
	require.NoError(t, e)
	defer db.Close()
	r := &DaMengNativeReader{&DaMengReader{db: db, schema: "MixedOwner"}}
	table := damengnative.Table{Name: "MixedTable", Schema: "MixedOwner", Columns: []damengnative.Column{{Name: "MixedText", DataType: "VARCHAR", Writable: true}, {Name: "Exact", DataType: "DECIMAL", Writable: true}, {Name: "Bytes", DataType: "BLOB", Writable: true}, {Name: "Generated", DataType: "INT", Writable: false}}}
	m.ExpectQuery(`SELECT "MixedText" AS "MixedText", CAST\("Exact" AS VARCHAR\(256\)\) AS "Exact", "Bytes" AS "Bytes" FROM "MixedOwner"."MixedTable"`).WillReturnRows(sqlmock.NewRows([]string{"text", "decimal", "binary"}).AddRow("Keep\x00MiXeD", "-1234567890123456789012345678901234.1234", []byte{0, 1, 0, 255}))
	scan, e := r.OpenDamengNativeScan(context.Background(), table)
	require.NoError(t, e)
	defer scan.Close()
	require.True(t, scan.Next())
	values, e := scan.Values()
	require.NoError(t, e)
	require.Equal(t, "Keep\x00MiXeD", values[0])
	require.Equal(t, "-1234567890123456789012345678901234.1234", values[1])
	require.Equal(t, []byte{0, 1, 0, 255}, values[2])
	require.False(t, scan.Next())
	require.NoError(t, scan.Err())
	require.NoError(t, m.ExpectationsWereMet())
}

func TestDaMengNativeMVScanUsesOriginalSnapshotStorage(t *testing.T) {
	db, m, e := sqlmock.New()
	require.NoError(t, e)
	defer db.Close()
	r := &DaMengNativeReader{&DaMengReader{db: db, schema: "App"}}
	m.ExpectQuery(`SELECT "Id" AS "Id" FROM "App"."MTAB\$_Mv"`).WillReturnRows(sqlmock.NewRows([]string{"Id"}).AddRow(1))
	scan, e := r.OpenDamengNativeScan(context.Background(), damengnative.Table{Name: "DBGOLD_MV_hash", SourceName: "MTAB$_Mv", Schema: "App", Columns: []damengnative.Column{{Name: "Id", DataType: "INT", Writable: true}}})
	require.NoError(t, e)
	require.True(t, scan.Next())
	require.NoError(t, scan.Close())
	require.NoError(t, m.ExpectationsWereMet())
}

func TestDaMengNativeLegacyPageDoesNotCleanNUL(t *testing.T) {
	db, m, e := sqlmock.New()
	require.NoError(t, e)
	defer db.Close()
	r := &DaMengNativeReader{&DaMengReader{db: db, schema: "MixedOwner"}}
	m.ExpectQuery(`SELECT \* FROM "MixedOwner"."MixedTable" ORDER BY "MixedId" OFFSET 0 ROWS FETCH NEXT 10 ROWS ONLY`).WillReturnRows(sqlmock.NewRowsWithColumnDefinition(sqlmock.NewColumn("MixedId").OfType("VARCHAR", "")).AddRow("Ab\x00C"))
	cols, _, rows, e := r.ReadPage(context.Background(), "MixedTable", []string{"MixedId"}, 0, 10)
	require.NoError(t, e)
	require.Equal(t, []string{"MixedId"}, cols)
	require.Equal(t, "Ab\x00C", rows[0][0])
	require.Equal(t, "MixedOwner", r.MigrationSchema())
	require.NoError(t, m.ExpectationsWereMet())
}
