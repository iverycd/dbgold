package damengnative

import (
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestNativeIntervalDefinitionKeepsInitialBoundaryAndPureConstructors(t *testing.T) {
	ddl := `CREATE TABLE "App"."Base"("PARTITION" VARCHAR(80) DEFAULT 'INTERVAL(danger())',"A" DATE) PARTITION BY RANGE("A") INTERVAL(NUMTOYMINTERVAL (1,'MONTH'))(PARTITION "Before" VALUES LESS THAN(DATE '2010-01-01'),PARTITION "Start" VALUES EQU OR LESS THAN(DATE '2020-01-01'))`
	d, err := nativeIntervalDefinition(ddl)
	require.NoError(t, err)
	require.Equal(t, "A", d.column)
	require.Equal(t, "NUMTOYMINTERVAL (1,'MONTH')", d.interval)
	require.Equal(t, "DATE '2020-01-01'", d.last)
	require.True(t, d.inclusive)
	require.Equal(t, map[string]bool{"Before": true, "Start": true}, d.initial)
	d, err = nativeIntervalDefinition(`CREATE TABLE "App"."Base"("A" INT DEFAULT 1) PARTITION BY RANGE("A")(PARTITION "P" VALUES LESS THAN(5))`)
	require.NoError(t, err)
	require.Nil(t, d)
	_, err = nativeIntervalDefinition(`CREATE TABLE "App"."Base"("A" INT) PARTITION BY RANGE("A") INTERVAL(danger())(PARTITION "P" VALUES LESS THAN(5))`)
	require.ErrorContains(t, err, "原生常量")
}

func TestIntervalSeedUsesNativeReadOnlyExistingPartitionProof(t *testing.T) {
	db, m, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := nativeMVReader(db)
	m.ExpectQuery(`SELECT CASE WHEN .* THEN \(SELECT COUNT\(\*\) FROM "App"\."Base" PARTITION FOR\(CAST\(CAST\(1 AS INT\)-1 AS INT\)\)\) ELSE -1 END FROM DUAL`).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))
	seed, err := r.freezeIntervalSeed("Base", Column{DataType: "INT", SQLType: "INT"}, intervalDefinition{interval: "1", last: "-5", inclusive: true}, "1")
	require.NoError(t, err)
	require.Equal(t, "CAST(CAST(1 AS INT)-1 AS INT)", seed)
	require.NoError(t, m.ExpectationsWereMet())
}
