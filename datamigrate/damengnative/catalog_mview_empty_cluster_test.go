package damengnative

import (
	"context"
	"errors"
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
	m.ExpectQuery(`SELECT CASE WHEN CAST\(5 AS INT\) IS NOT NULL THEN .* PARTITION FOR\(CAST\(5 AS INT\)\)`).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	require.NoError(t, r.populateMaterializedEmptyCluster(table, p))
	require.Equal(t, "CAST(5 AS INT)", p.Columns[1].ValueSQL)
	require.Empty(t, p.Columns[0].ValueSQL)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestMaterializedEmptyPartitionLevelsRetainEachNestedRoutingKey(t *testing.T) {
	ddl := `CREATE TABLE "App"."Base"("A" INT,"B" INT,"C" INT) PARTITION BY RANGE("A") SUBPARTITION BY LIST("B") SUBPARTITION TEMPLATE(SUBPARTITION "BT" VALUES(5,7)) SUBPARTITION BY LIST("C") SUBPARTITION TEMPLATE(SUBPARTITION "CT" VALUES(11,13)) (PARTITION "P" VALUES LESS THAN(-5)(SUBPARTITION "B" VALUES(17)(SUBPARTITION "C" VALUES(19))))`
	levels, err := materializedEmptyPartitionLevels(ddl)
	require.NoError(t, err)
	require.Equal(t, []materializedEmptyPartitionLevel{{kind: "RANGE", keys: []string{"A"}, bounds: [][]string{{"-5"}}}, {kind: "LIST", keys: []string{"B"}, bounds: [][]string{{"5"}, {"7"}, {"17"}}}, {kind: "LIST", keys: []string{"C"}, bounds: [][]string{{"11"}, {"13"}, {"19"}}}}, levels)
}

func TestMaterializedEmptyPartitionRouteIsReadOnlyAndRejectsUnroutableZero(t *testing.T) {
	db, m, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := nativeMVReader(db)
	p := &MaterializedEmptyTouch{Table: "Base", DefinitionSQL: `CREATE TABLE "App"."Base"("A" INT,"B" INT) PARTITION BY RANGE("A") SUBPARTITION BY LIST("B") SUBPARTITION TEMPLATE(SUBPARTITION "Five" VALUES(5))(PARTITION "Only" VALUES LESS THAN(-5))`, Columns: []MaterializedEmptyColumn{{Name: "A", ValueSQL: "CAST('0' AS INT)"}, {Name: "B"}}}
	columns := []Column{{Name: "A", DataType: "INT", SQLType: "INT", Writable: true}, {Name: "B", DataType: "INT", SQLType: "INT", Writable: true}}
	m.ExpectQuery(`SELECT CASE WHEN .* THEN \(SELECT COUNT\(\*\) FROM "App"\."Base" SUBPARTITION FOR\(CAST\(CAST\(-5 AS INT\)-1 AS INT\),CAST\(5 AS INT\)\)\) ELSE -1 END FROM DUAL`).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	require.NoError(t, r.populateMaterializedEmptyCluster(Table{Name: "Base", Columns: columns}, p))
	require.Equal(t, "CAST(CAST(-5 AS INT)-1 AS INT)", p.Columns[0].ValueSQL)
	require.Equal(t, "CAST(5 AS INT)", p.Columns[1].ValueSQL)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestMaterializedEmptyPartitionBoundsCannotCallUserFunctions(t *testing.T) {
	for _, s := range []string{`danger()`, `"App"."danger"()`, `CAST(danger() AS INT)`, `1;CALL danger()`, `SELECT danger() FROM DUAL`, `App.danger()`, `sequence.NEXTVAL`, `danger`, `CURRENT_TIMESTAMP`, `1+danger`, `TO_DATE`} {
		require.False(t, materializedEmptyPartitionConstant(s), s)
	}
	for _, s := range []string{`-5`, `MAXVALUE`, `'a,b'`, `TO_DATE('2000-01-01','YYYY-MM-DD')`, `CAST(0.01 AS DECIMAL(38,20))`, `TIMESTAMP '2000-01-01 00:00:00'`, `N'Unicode'`, `-1.7E+308`, `0xFF`} {
		require.True(t, materializedEmptyPartitionConstant(s), s)
	}
}

func TestMaterializedEmptyPartitionCandidateFallbackAndSourceDrift(t *testing.T) {
	for _, count := range []int{0, 1} {
		db, m, err := sqlmock.New()
		require.NoError(t, err)
		r := nativeMVReader(db)
		p := &MaterializedEmptyTouch{Table: "Base", DefinitionSQL: `CREATE TABLE "App"."Base"("A" INT) PARTITION BY RANGE("A")(PARTITION "Only" VALUES EQU OR LESS THAN(-2147483648))`, Columns: []MaterializedEmptyColumn{{Name: "A"}}}
		m.ExpectQuery(`SELECT CASE WHEN .*`).WillReturnError(errors.New("numeric overflow"))
		m.ExpectQuery(`SELECT CASE WHEN .*`).WillReturnError(errors.New("partition does not exist"))
		m.ExpectQuery(`SELECT CASE WHEN CAST\(-2147483648 AS INT\) IS NOT NULL.*`).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(count))
		err = r.populateMaterializedEmptyCluster(Table{Name: "Base", Columns: []Column{{Name: "A", DataType: "INT", SQLType: "INT", Writable: true}}}, p)
		if count == 0 {
			require.NoError(t, err)
			require.Equal(t, "CAST(-2147483648 AS INT)", p.Columns[0].ValueSQL)
		} else {
			require.ErrorContains(t, err, "已有数据")
			require.Empty(t, p.Columns[0].ValueSQL)
		}
		require.NoError(t, m.ExpectationsWereMet())
		db.Close()
	}
}

func TestMaterializedEmptyPartitionCancellationDoesNotProbe(t *testing.T) {
	db, m, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := nativeMVReader(db)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.ctx = ctx
	p := &MaterializedEmptyTouch{DefinitionSQL: `CREATE TABLE "App"."Base"("A" INT) PARTITION BY HASH("A") PARTITIONS 2`}
	err = r.populateMaterializedEmptyCluster(Table{Columns: []Column{{Name: "A", DataType: "INT", SQLType: "INT", Writable: true}}}, p)
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestMaterializedEmptyGeneratedPartitionKeyRetainsNullEpochWithoutEvaluation(t *testing.T) {
	db, m, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := nativeMVReader(db)
	p := &MaterializedEmptyTouch{Table: "Base", DefinitionSQL: `CREATE TABLE "App"."Base"("Id" INT,"A" INT,"B" INT GENERATED ALWAYS AS(danger("Id")) VIRTUAL) PARTITION BY LIST("A") SUBPARTITION BY RANGE("B") SUBPARTITION TEMPLATE(SUBPARTITION "Neg" VALUES LESS THAN(-5))(PARTITION "Five" VALUES(5))`, Columns: []MaterializedEmptyColumn{{Name: "Id"}, {Name: "A"}}}
	require.NoError(t, r.populateMaterializedEmptyCluster(Table{Name: "Base", Columns: []Column{{Name: "Id", Writable: true, DataType: "INT", SQLType: "INT"}, {Name: "A", Writable: true, DataType: "INT", SQLType: "INT"}, {Name: "B", Writable: false, DataType: "INT", SQLType: "INT"}}}, p))
	require.Empty(t, p.Columns[0].ValueSQL)
	require.Equal(t, "CAST(5 AS INT)", p.Columns[1].ValueSQL)
	require.NoError(t, m.ExpectationsWereMet(), "生成表达式不能在源预检求值")
}
