package damengnative

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestMaterializedEmptyClusterTypedConstants(t *testing.T) {
	for _, tc := range []struct{ kind, sqlType, seed string }{
		{"INT", "INT", "'0'"}, {"DECIMAL", "DECIMAL(38,20)", "'0'"},
		{"VARCHAR", "VARCHAR(80)", "'0'"}, {"VARBINARY", "VARBINARY(80)", "0x00"},
		{"DATE", "DATE", "'2000-01-01'"}, {"TIME", "TIME(6)", "'00:00:00'"},
		{"TIMESTAMP WITH TIME ZONE", "TIMESTAMP(6) WITH TIME ZONE", "'2000-01-01 00:00:00 +00:00'"},
		{"INTERVAL YEAR TO MONTH", "INTERVAL YEAR(4) TO MONTH", "'0-0'"},
		{"INTERVAL DAY TO SECOND", "INTERVAL DAY(4) TO SECOND(6)", "'0 00:00:00'"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			got, err := materializedEmptyClusterValue(Column{Name: "K", DataType: tc.kind, SQLType: tc.sqlType}, nil)
			require.NoError(t, err)
			require.Equal(t, "CAST("+tc.seed+" AS "+tc.sqlType+")", got)
		})
	}
	historical := "SELECT, O'Brien"
	got, err := materializedEmptyClusterValue(Column{Name: "K", DataType: "VARCHAR", SQLType: "VARCHAR(80)"}, &historical)
	require.NoError(t, err)
	require.Equal(t, "CAST('SELECT, O''Brien' AS VARCHAR(80))", got)
}

func TestMaterializedEmptyListBoundaryTuplePreservesLiterals(t *testing.T) {
	for _, tc := range []struct {
		boundary string
		width    int
		want     []string
	}{
		{"5,6", 1, []string{"5"}},
		{"('a,b',10),('c',20)", 2, []string{"'a,b'", "10"}},
		{"(TIMESTAMP '2000-01-01 00:00:00',0xFF)", 2, []string{"TIMESTAMP '2000-01-01 00:00:00'", "0xFF"}},
		{"DATE '2000-01-01'", 1, []string{"DATE '2000-01-01'"}},
	} {
		got, err := materializedEmptyListTuple(tc.boundary, tc.width)
		require.NoError(t, err)
		require.Equal(t, tc.want, got)
	}
}

func TestMaterializedEmptyListFreezesNonKeyRoutingColumn(t *testing.T) {
	db, m, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := nativeMVReader(db)
	r.ctx = context.Background()
	p := &MaterializedEmptyTouch{Table: "Base", DefinitionSQL: `CREATE TABLE "App"."Base"("Id" INT,"K" INT NOT NULL) PARTITION BY LIST("K")(PARTITION "P" VALUES(5,6))`, Columns: []MaterializedEmptyColumn{{Name: "Id"}, {Name: "K", NotNull: true}}}
	table := Table{Name: "Base", Columns: []Column{{Name: "Id", DataType: "INT", SQLType: "INT", Writable: true}, {Name: "K", DataType: "INT", SQLType: "INT", Writable: true}}}
	m.ExpectQuery(`SELECT HIGH_VALUE FROM ALL_TAB_PARTITIONS`).WithArgs("App", "Base").WillReturnRows(sqlmock.NewRows([]string{"HIGH_VALUE"}).AddRow("5,6"))
	require.NoError(t, r.populateMaterializedEmptyCluster(table, p))
	require.Equal(t, "CAST(5 AS INT)", p.Columns[1].ValueSQL)
	require.Empty(t, p.Columns[0].ValueSQL)
	require.NoError(t, m.ExpectationsWereMet())
}
