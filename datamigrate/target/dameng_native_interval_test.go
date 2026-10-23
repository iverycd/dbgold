package target

import (
	"context"
	"testing"

	"dbgold/datamigrate/damengnative"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestIntervalUnknownRequiresRestoredAttributesBeforePureReadSuccess(t *testing.T) {
	for _, restored := range []bool{true, false} {
		t.Run(map[bool]string{true: "restored", false: "nullable_changed"}[restored], func(t *testing.T) {
			db, m, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			w := &DaMengNativeWriter{DaMengWriter: &DaMengWriter{db: db, schema: "Dst"}}
			ddl := `CREATE TABLE "Dst"."Empty"("V" INT NOT NULL) PARTITION BY RANGE("V") INTERVAL(10)(PARTITION "Initial" VALUES LESS THAN(0))`
			p := &damengnative.IntervalPartitionPlan{Column: "V", DefinitionSQL: ddl, EmptyTouch: &damengnative.MaterializedEmptyTouch{Table: "Empty", DefinitionSQL: ddl, Columns: []damengnative.MaterializedEmptyColumn{{Name: "V", NotNull: true}}}, Partitions: []damengnative.IntervalPartition{{Name: "Initial", HighSQL: "0"}, {Name: "Auto", HighSQL: "10", SeedSQL: "CAST(9 AS INT)"}}}
			unit := damengnative.Unit{SQL: damengnative.IntervalPartitionSQL("Dst", "Empty", p), Phase: "interval_prepare"}
			o := damengnative.Object{Kind: "table", Schema: "Dst", Name: "Empty", Interval: p, Units: []damengnative.Unit{unit}}
			m.ExpectQuery(`SELECT p.PARTITION_NAME,p.HIGH_VALUE`).WithArgs("Dst", "Empty").WillReturnRows(sqlmock.NewRows([]string{"name", "high", "space", "physical"}).AddRow("Initial", "0", "MAIN", "Empty_Initial").AddRow("Auto", "10", "MAIN", "Empty_Auto"))
			actual := ddl
			if !restored {
				actual = `CREATE TABLE "Dst"."Empty"("V" INT) PARTITION BY RANGE("V") INTERVAL(10)(PARTITION "Initial" VALUES LESS THAN(0))`
			}
			expectEmptyDDL(m, actual)
			m.ExpectQuery(`SELECT c.OWNER,c.CONSTRAINT_NAME`).WillReturnRows(sqlmock.NewRows([]string{"owner", "name", "table", "kind", "status", "validated", "column"}))
			if !restored {
				m.ExpectQuery(`SELECT COUNT`).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
			}
			done, retry, err := w.ReconcileDamengNativeIntervalPartitions(context.Background(), o, unit)
			require.NoError(t, err)
			require.Equal(t, restored, done)
			require.Equal(t, !restored, retry)
			require.NoError(t, m.ExpectationsWereMet(), "目录归并本身不能执行DML/DDL")
		})
	}
}

func TestIntervalPhysicalDefinitionsNormalizeOnlyProvenDerivedConstraintNames(t *testing.T) {
	source := `CREATE TABLE "Dst"."T_Initial"("Id" BIGINT IDENTITY(10,3), CONSTRAINT "PK_1_100" PRIMARY KEY("Id"), CONSTRAINT "BusinessCheck" CHECK("Id">0))`
	actual := `CREATE TABLE "Dst"."T_Initial"("Id" BIGINT IDENTITY(16,3), CONSTRAINT "PK_1_200" PRIMARY KEY("Id"), CONSTRAINT "BusinessCheck" CHECK("Id">0))`
	left := nativeIntervalDerivedDefinition(source, []string{"PK_1_100"})
	right := nativeIntervalDerivedDefinition(actual, []string{"PK_1_200"})
	require.True(t, nativeDefinitionsEqual(left, right))
	require.False(t, nativeDefinitionsEqual(left, nativeIntervalDerivedDefinition(actual, nil)), "an unproved target name cannot be discarded")
	require.False(t, nativeDefinitionsEqual(left, nativeIntervalDerivedDefinition(`CREATE TABLE "Dst"."T_Initial"("Id" BIGINT IDENTITY(16,3), CONSTRAINT "PK_1_200" PRIMARY KEY("Id"), CONSTRAINT "Changed" CHECK("Id">0))`, []string{"PK_1_200"})))
	require.False(t, nativeDefinitionsEqual(left, nativeIntervalDerivedDefinition(`CREATE TABLE "Dst"."T_Initial"("Id" BIGINT IDENTITY(16,3), CONSTRAINT "PK_1_200" PRIMARY KEY("Id"), CONSTRAINT "BusinessCheck" CHECK("Id">1))`, []string{"PK_1_200"})))
}

func TestIntervalAffineValuesAreAuthenticatedAndBelongToWritableColumns(t *testing.T) {
	for _, values := range [][]damengnative.IntervalSeedValue{
		{{Column: "Generated", SQL: "CAST(1 AS INT)"}},
		{{Column: "Id", SQL: ""}},
		{{Column: "Id", SQL: "CAST(1 AS INT)"}, {Column: "Id", SQL: "CAST(2 AS INT)"}},
	} {
		o, u := nativeIntervalScopeFixture()
		o.Interval.Partitions[0].Values = values
		u.SQL = damengnative.IntervalPartitionSQL(o.Schema, o.Name, o.Interval)
		o.Units[0] = u
		w, m := mockNativeDM(t)
		_, err := w.intervalPlan(o, u)
		require.ErrorContains(t, err, "冻结可写列")
		require.NoError(t, m.ExpectationsWereMet())
	}
	o, u := nativeIntervalScopeFixture()
	o.Interval.Partitions[0].Values = []damengnative.IntervalSeedValue{{Column: "Id", SQL: "CAST(1 AS INT)"}}
	u.SQL = damengnative.IntervalPartitionSQL(o.Schema, o.Name, o.Interval)
	o.Units[0] = u
	w, m := mockNativeDM(t)
	_, err := w.intervalPlan(o, u)
	require.NoError(t, err)
	o.Interval.Partitions[0].Values[0].SQL = "CAST(2 AS INT)"
	_, err = w.intervalPlan(o, u)
	require.Error(t, err, "持久化后值发生变化也必须使marker失效")
	require.NoError(t, m.ExpectationsWereMet())
}
