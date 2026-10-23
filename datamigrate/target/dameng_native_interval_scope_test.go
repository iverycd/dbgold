package target

import (
	"context"
	"testing"

	"dbgold/datamigrate/damengnative"
	"github.com/stretchr/testify/require"
)

func nativeIntervalScopeFixture() (damengnative.Object, damengnative.Unit) {
	const schema = "MixedOwner"
	const table = `Exact"Table`
	definition := "CREATE TABLE " + damengnative.Qualified(schema, table) + `("Id" INT) PARTITION BY RANGE("Id") INTERVAL(10)(PARTITION "Initial" VALUES LESS THAN(0))`
	p := &damengnative.IntervalPartitionPlan{
		Column: "Id", DefinitionSQL: definition,
		EmptyTouch: &damengnative.MaterializedEmptyTouch{Table: table, DefinitionSQL: definition, Columns: []damengnative.MaterializedEmptyColumn{{Name: "Id", ValueSQL: "0"}}},
		Partitions: []damengnative.IntervalPartition{{Name: "ExactPartition", HighSQL: "10", SeedSQL: "9", Tablespace: "MAIN"}},
	}
	u := damengnative.Unit{Phase: "interval_prepare", SQL: damengnative.IntervalPartitionSQL(schema, table, p)}
	return damengnative.Object{ID: "table", Kind: "table", Name: table, Schema: schema, Interval: p, Units: []damengnative.Unit{u}}, u
}

func TestDamengNativeIntervalInvalidScopeAndHashRejectBeforeConnection(t *testing.T) {
	for _, variant := range []string{
		"wrong_schema", "schema_case_changed", "other_object", "not_table", "missing_plan", "missing_key", "missing_empty_touch", "empty_touch_other_table", "no_partitions", "wrong_phase", "changed_marker", "changed_recipe", "unit_not_frozen", "frozen_unit_wrong_phase", "duplicate_partition", "unnamed_partition", "missing_boundary",
	} {
		t.Run(variant, func(t *testing.T) {
			object, unit := nativeIntervalScopeFixture()
			switch variant {
			case "wrong_schema":
				object.Schema = "Other"
			case "schema_case_changed":
				object.Schema = "mixedowner"
			case "other_object":
				object.Name = "OtherTable"
			case "not_table":
				object.Kind = "view"
			case "missing_plan":
				object.Interval = nil
			case "missing_key":
				object.Interval.Column = ""
			case "missing_empty_touch":
				object.Interval.EmptyTouch = nil
			case "empty_touch_other_table":
				object.Interval.EmptyTouch.Table = "OtherTable"
			case "no_partitions":
				object.Interval.Partitions = nil
			case "wrong_phase":
				unit.Phase = "after_data"
			case "changed_marker":
				unit.SQL += "; DROP TABLE \"MixedOwner\".\"Other\""
			case "changed_recipe":
				object.Interval.Partitions[0].SeedSQL = "8"
			case "unit_not_frozen":
				object.Units = nil
			case "frozen_unit_wrong_phase":
				object.Units[0].Phase = "after_data"
			case "duplicate_partition":
				object.Interval.Partitions = append(object.Interval.Partitions, object.Interval.Partitions[0])
			case "unnamed_partition":
				object.Interval.Partitions[0].Name = ""
			case "missing_boundary":
				object.Interval.Partitions[0].HighSQL = ""
			}
			// For malformed structural plans, authenticate their exact marker
			// too: rejection must come from the structural identity checks, not
			// merely from detecting an old hash after a field was changed.
			switch variant {
			case "missing_key", "missing_empty_touch", "empty_touch_other_table", "no_partitions", "duplicate_partition", "unnamed_partition", "missing_boundary":
				unit.SQL = damengnative.IntervalPartitionSQL("MixedOwner", object.Name, object.Interval)
				object.Units[0] = unit
			}
			for _, operation := range []string{"restore", "reconcile"} {
				t.Run(operation, func(t *testing.T) {
					w, mock := mockNativeDM(t)
					// If either entry point acquires a connection first, database/sql
					// returns context.Canceled. Scope rejection must happen even
					// before that acquisition, with no driver query or execution.
					ctx, cancel := context.WithCancel(context.Background())
					cancel()
					var err error
					if operation == "restore" {
						err = w.RestoreDamengNativeIntervalPartitions(ctx, object, unit)
					} else {
						var done, retry bool
						done, retry, err = w.ReconcileDamengNativeIntervalPartitions(ctx, object, unit)
						require.False(t, done)
						require.False(t, retry)
					}
					require.Error(t, err)
					require.NotErrorIs(t, err, context.Canceled)
					require.Contains(t, err.Error(), "间隔分区")
					require.NoError(t, mock.ExpectationsWereMet(), "invalid frozen plans must not issue target SQL")
				})
			}
		})
	}
}

func TestDamengNativeIntervalScopeAcceptsExactQuotedFrozenIdentity(t *testing.T) {
	w, mock := mockNativeDM(t)
	object, unit := nativeIntervalScopeFixture()
	plan, err := w.intervalPlan(object, unit)
	require.NoError(t, err)
	require.Same(t, object.Interval, plan)
	require.Contains(t, unit.SQL, `"MixedOwner"."Exact""Table"`)
	require.Equal(t, "ExactPartition", plan.Partitions[0].Name)
	require.NoError(t, mock.ExpectationsWereMet(), "validating identity itself is read-only and connection-free")
}
