package datamigrate

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"dbgold/datamigrate/pgoracle"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

func TestPGOracleRunIsolatesFirstAndMiddleTableDDLFailure(t *testing.T) {
	for _, broken := range []string{"AFirst", "MMiddle"} {
		for _, stage := range []string{"DROP", "CREATE"} {
			t.Run(broken+"/"+stage, func(t *testing.T) {
				r, w, m := pgOracleFixture("AFirst", "MMiddle", "ZLast")
				for i := range r.snapshot.Tables {
					r.snapshot.Tables[i].Info.Comment = "table description"
				}
				w.createFailure, w.createFailureStage = strings.ToUpper(broken), stage
				report := m.Run(context.Background())
				require.True(t, report.HasFailures())
				require.Equal(t, 3, report.Tables.Total)
				require.Equal(t, 2, report.Tables.Success)
				require.Equal(t, 1, report.Tables.Failed)
				require.Equal(t, 2, report.Data.Success)
				require.Equal(t, 1, report.Data.Failed)
				require.Zero(t, w.copyAttempts[strings.ToUpper(broken)])
				require.NotContains(t, r.reads, broken)
				require.EqualValues(t, 2, w.counts["ZLAST"])
				require.Equal(t, 2, report.PrimaryKeys.Success)
				require.Equal(t, 1, report.PrimaryKeys.Failed)
				require.Equal(t, 2, report.Comments.Success)
				require.Equal(t, 1, report.Comments.Failed)
				require.Equal(t, "dependency", report.Comments.Items[0].Stage)
				require.NotContains(t, w.objects, "comment:"+strings.ToUpper(broken))
				require.Contains(t, report.Tables.Items[0].DDL, `"APP"."`+strings.ToUpper(broken)+`"`)
				require.Equal(t, "dependency", report.PrimaryKeys.Items[0].Stage)
				for _, table := range []string{"AFirst", "MMiddle", "ZLast"} {
					if table != broken {
						require.Contains(t, r.reads, table)
						require.Equal(t, []string{"ID"}, w.copyColumns[strings.ToUpper(table)])
					}
				}
			})
		}
	}
}

func TestPGOracleRunRetainsCommittedPageAndContinuesLaterTable(t *testing.T) {
	r, w, m := pgOracleFixture("ABroken", "ZHealthy")
	r.snapshot.Tables[0].Info.Comment = "table description"
	r.snapshot.Tables[0].Info.Columns[0].Comment = "column description"
	m.cfg.CaptureRepair = true
	w.copyFailure, w.copyFailurePage = "ABROKEN", 2
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.Equal(t, 2, report.Tables.Success)
	require.Equal(t, 1, report.Data.Failed)
	require.Equal(t, 1, report.Data.Success)
	require.EqualValues(t, 1, w.counts["ABROKEN"])
	require.EqualValues(t, 2, w.counts["ZHEALTHY"])
	require.Contains(t, report.Data.Items[0].Error, "第 2 页")
	require.Contains(t, report.Data.Items[0].Error, "已提交 1 行")
	require.Equal(t, 1, report.PrimaryKeys.Success)
	require.Equal(t, 1, report.PrimaryKeys.Failed)
	require.Len(t, report.RowCounts, 1)
	require.Equal(t, "ZHEALTHY", report.RowCounts[0].Table)
	require.True(t, report.RowCounts[0].Match)
	require.Equal(t, 2, report.Comments.Success)
	require.Contains(t, w.objects, "comment:ABROKEN")
	require.Contains(t, w.objects, "comment:ABROKEN.ID")
	var comments int
	for _, object := range report.RepairManifest.Objects {
		if object.Kind == "comment" {
			comments++
			require.Equal(t, "succeeded", object.Status)
			require.Len(t, object.Units, 1)
			require.Equal(t, "succeeded", object.Units[0].Status)
		}
	}
	require.Equal(t, 2, comments)
}

func TestPGOracleRunTaskErrorsStopFollowingMutation(t *testing.T) {
	_, w, m := pgOracleFixture("AFirst", "ZLast")
	w.createFailure, w.createError = "AFIRST", driver.ErrBadConn
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.Empty(t, w.copied)
	require.Empty(t, w.objects)
	for _, sql := range w.statements {
		require.NotContains(t, sql, `"ZLAST"`)
	}
}

func TestPGOracleRunModesRespectStructureDataAndObjects(t *testing.T) {
	for _, mode := range []string{"schema_only", "data_only", "check_constraints", "views"} {
		t.Run(mode, func(t *testing.T) {
			r, w, m := pgOracleFixture("Orders")
			r.snapshot.Tables[0].Checks = []source.PGObject{{Kind: "check", Table: "Orders", Name: "Positive", Definition: `CHECK ("Id" > 0)`, PG: &source.PGConstraintMetadata{Type: "c", Validated: true}}}
			r.snapshot.Views = []source.ViewInfo{{ViewName: "OrderView", Definition: `SELECT "Id" FROM "MixedSchema"."Orders"`, Columns: []source.ColumnInfo{{Name: "Id", DataType: "int4"}}}}
			switch mode {
			case "schema_only", "data_only":
				m.cfg.Content = mode
			case "check_constraints":
				m.cfg.Objects = []string{mode}
				m.cfg.TableNames = []string{"Orders"}
			case "views":
				m.cfg.ViewNames = []string{"OrderView"}
			}
			report := m.Run(context.Background())
			require.False(t, report.HasFailures(), report)
			switch mode {
			case "schema_only":
				require.Equal(t, 1, report.Tables.Success)
				require.Equal(t, 1, report.Views.Success)
				require.Equal(t, 1, report.Constraints.Success)
				require.Zero(t, report.Data.Total)
				require.Empty(t, r.reads)
			case "data_only":
				require.Zero(t, report.Tables.Total)
				require.Equal(t, 1, report.Data.Success)
				require.Empty(t, w.statements)
				require.Empty(t, w.objects)
				require.Equal(t, []string{"ORDERS"}, w.validated)
			case "check_constraints":
				require.Zero(t, report.Tables.Total)
				require.Zero(t, report.Data.Total)
				require.Equal(t, 1, report.Constraints.Success)
				require.Equal(t, []string{"check:POSITIVE"}, w.objects)
				require.Empty(t, r.reads)
			case "views":
				require.Zero(t, report.Tables.Total)
				require.Zero(t, report.Data.Total)
				require.Equal(t, 1, report.Views.Success)
				require.Equal(t, []string{"view:ORDERVIEW"}, w.objects)
				require.Empty(t, r.reads)
			}
		})
	}
}

func TestPGOracleRunUnsupportedIndexesDoNotBlockTableData(t *testing.T) {
	r, w, m := pgOracleFixture("Parent", "Child")
	for i := range r.snapshot.Tables {
		table := &r.snapshot.Tables[i]
		table.Info.Columns = append(table.Info.Columns, source.ColumnInfo{Name: "Optional", DataType: "int4", IsNullable: true})
		r.ddl[table.Info.TableName] = &table.Info
		r.rows[table.Info.TableName] = [][]any{{int64(1), nil}, {int64(2), int64(3)}}
	}
	r.snapshot.Tables[0].Indexes = append(r.snapshot.Tables[0].Indexes,
		source.IndexInfo{TableName: "Parent", IndexName: "Gin", Method: "gin", Columns: []string{"Id"}},
		source.IndexInfo{TableName: "Parent", IndexName: "NullableUnique", IsUnique: true, Method: "btree", Columns: []string{"Id", "Optional"}, PG: &source.PGIndexMetadata{KeyCount: 2, Valid: true, ConstraintName: "NullableUnique", ConstraintType: "u", Validated: true}},
	)
	r.snapshot.Tables[1].ForeignKeys = []source.FKInfo{{TableName: "Child", ConstraintName: "ChildPair", Columns: []string{"Id", "Optional"}, RefTable: "Parent", RefSchema: "MixedSchema", RefColumns: []string{"Id", "Optional"}, OnUpdate: "NO ACTION", OnDelete: "NO ACTION", PG: &source.PGConstraintMetadata{Match: "s", Validated: true}}}
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.Equal(t, 2, report.Tables.Success)
	require.Equal(t, 2, report.Data.Success)
	require.Equal(t, 2, report.PrimaryKeys.Success)
	require.Equal(t, 2, report.Indexes.Failed)
	require.Equal(t, 1, report.Constraints.Failed)
	require.EqualValues(t, 2, w.counts["PARENT"])
	require.EqualValues(t, 2, w.counts["CHILD"])
	require.NotContains(t, w.objects, "index:GIN")
	require.NotContains(t, w.objects, "index:NULLABLEUNIQUE")
	require.NotContains(t, w.objects, "foreign_key:CHILDPAIR")
}

func TestPGOracleRunPersistenceFailurePreventsFirstWrite(t *testing.T) {
	for _, stage := range []string{"manifest", "object"} {
		t.Run(stage, func(t *testing.T) {
			_, w, m := pgOracleFixture("Orders")
			m.cfg.CaptureRepair = true
			m.cfg.RepairSink = func(RepairManifest) error {
				if stage == "manifest" {
					return errors.New("disk full")
				}
				return nil
			}
			if stage == "object" {
				m.cfg.RepairObjectSink = func(repair.Object) error { return errors.New("object ledger unavailable") }
			}
			report := m.Run(context.Background())
			require.True(t, report.HasFailures())
			require.Empty(t, w.statements)
			require.Empty(t, w.copied)
			require.Empty(t, w.created)
		})
	}
}

func TestPGOracleRunSequenceAndGeneratedTriggerIdentityPersist(t *testing.T) {
	r, w, m := pgOracleFixture("Orders")
	r.snapshot.Tables[0].Info.Columns[0].Identity = "a"
	r.snapshot.Sequences = []source.PGSequence{{Name: "OrderSequence", Schema: "MixedSchema", Increment: 3, Min: 1, Max: 10000, LastValue: 7, IsCalled: true, Cache: 1, Bindings: []source.PGSequenceBinding{{Schema: "MixedSchema", TableName: "Orders", ColumnName: "Id", Identity: "a", Owned: true}}}}
	m.cfg.CaptureRepair = true
	var snapshots []RepairManifest
	m.cfg.RepairSink = func(manifest RepairManifest) error {
		b, err := json.Marshal(manifest)
		require.NoError(t, err)
		var frozen RepairManifest
		require.NoError(t, json.Unmarshal(b, &frozen))
		snapshots = append(snapshots, frozen)
		return nil
	}
	report := m.Run(context.Background())
	require.False(t, report.HasFailures(), report)
	require.Equal(t, 1, report.Sequences.Success)
	require.Equal(t, 1, report.Triggers.Success)
	require.Positive(t, w.sequenceInspections)
	require.NotNil(t, report.RepairManifest)
	require.NotEmpty(t, snapshots)
	var sequence, trigger, table *repair.Object
	for i := range report.RepairManifest.Objects {
		o := &report.RepairManifest.Objects[i]
		switch o.Kind {
		case "sequence":
			sequence = o
		case "trigger":
			trigger = o
		case "table":
			table = o
		}
	}
	require.NotNil(t, sequence)
	require.NotNil(t, trigger)
	require.NotNil(t, table)
	for _, object := range []*repair.Object{sequence, trigger} {
		require.Equal(t, "succeeded", object.Status)
		require.NotNil(t, object.PGOracleSequence)
		require.NotNil(t, object.PGOracleSequence.Identity)
		require.EqualValues(t, 42, object.PGOracleSequence.Identity.ObjectID)
		require.Equal(t, object.PGOracleSequence.Fingerprint(), object.PGOracleSequence.Identity.Fingerprint)
		require.Equal(t, "OrderSequence", object.PGOracleSequence.Source.Name)
		require.Equal(t, "ORDERSEQUENCE", object.PGOracleSequence.Name)
		require.Equal(t, "Orders", object.PGOracleSequence.Bindings[0].SourceTable)
		require.Equal(t, "ORDERS", object.PGOracleSequence.Bindings[0].Table)
	}
	require.Contains(t, trigger.Dependencies, sequence.ID)
	require.Contains(t, trigger.Dependencies, table.ID)
	require.Equal(t, "succeeded", table.DataStatus)
	require.Contains(t, sequence.DDL, "INCREMENT BY 3")
	require.NotContains(t, sequence.DDL, "DROP SEQUENCE")
	require.Contains(t, trigger.DDL, "DBGOLD_PG_ORACLE:")
}

func TestPGOracleRunMissingSnapshotTableRemainsInReport(t *testing.T) {
	r, w, m := pgOracleFixture("Missing", "Healthy")
	r.snapshot.Tables = r.snapshot.Tables[1:]
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.Equal(t, 1, report.Data.Success)
	require.Equal(t, 1, report.Data.Failed)
	require.Equal(t, 2, report.Tables.Total, "unreadable selected table must remain in the table report")
	require.Equal(t, 1, report.Tables.Failed)
	require.Equal(t, "Missing", report.Tables.Items[0].Name)
	require.NotContains(t, r.reads, "Missing")
	require.Zero(t, w.copyAttempts["MISSING"])
}

func TestPGOracleRunGeneratedTriggerFailureVisibleAsMigrationFailure(t *testing.T) {
	r, w, m := pgOracleFixture("Orders")
	r.snapshot.Sequences = []source.PGSequence{{Name: "OrderSequence", Schema: "MixedSchema", Increment: 1, Min: 1, Max: 10000, LastValue: 1, Cache: 1, Bindings: []source.PGSequenceBinding{{Schema: "MixedSchema", TableName: "Orders", ColumnName: "Id", Identity: "a", Owned: true}}}}
	w.objectFailure = "trigger:TRG_ORDERS_ID"
	report := m.Run(context.Background())
	require.Equal(t, 1, report.Tables.Success)
	require.Equal(t, 1, report.Data.Success)
	require.Equal(t, 1, report.Sequences.Success)
	require.Equal(t, 1, report.Triggers.Failed)
	require.True(t, report.HasFailures())
	require.True(t, report.TriggerMigrationEnabled, "generated trigger results must not be rendered as un-migrated source inventory")
	require.Contains(t, report.Triggers.Items[0].DDL, "CREATE OR REPLACE TRIGGER")
}

func TestPGOracleRunRetainedTableProtectsReferencedClosure(t *testing.T) {
	r, w, m := pgOracleFixture("AFailed", "BParent", "CGrandparent", "ZIndependent")
	w.createFailure, w.createFailureStage = "AFAILED", "DROP"
	w.dependencies = []target.MigrationDependency{
		{Schema: "APP", Table: "AFAILED", Name: "FK_AB", RefSchema: "APP", RefTable: "BPARENT"},
		{Schema: "APP", Table: "BPARENT", Name: "FK_BC", RefSchema: "APP", RefTable: "CGRANDPARENT"},
	}
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.Equal(t, 1, report.Tables.Success)
	require.Equal(t, 3, report.Tables.Failed)
	require.EqualValues(t, 2, w.counts["ZINDEPENDENT"])
	for _, name := range []string{"AFailed", "BParent", "CGrandparent"} {
		require.NotContains(t, r.reads, name)
		require.Zero(t, w.copyAttempts[strings.ToUpper(name)])
	}
	for _, sql := range w.statements {
		require.NotContains(t, sql, `"BPARENT"`)
		require.NotContains(t, sql, `"CGRANDPARENT"`)
	}
}

func TestPGOracleRunDataOnlyUsesFrozenSequenceEvidence(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(fmt.Sprint(conflict), func(t *testing.T) {
			r, w, m := pgOracleFixture("Orders", "Independent")
			sequence := source.PGSequence{Name: "OrderSequence", Schema: "MixedSchema", Increment: 1, Min: 1, Max: 10000, LastValue: 1, Cache: 1, Bindings: []source.PGSequenceBinding{{Schema: "MixedSchema", TableName: "Orders", ColumnName: "Id", Identity: "a", Owned: true}}}
			r.snapshot.Sequences = []source.PGSequence{sequence}
			p := m.PreflightPGOracle(context.Background())
			require.True(t, p.Ready, p.Issues)
			var plans []pgoracle.SequencePlan
			for _, plan := range p.sequences {
				plan.Identity = &pgoracle.SequenceIdentity{ObjectID: 42, Fingerprint: plan.Fingerprint()}
				plans = append(plans, *plan)
			}
			require.Len(t, plans, 1)
			if conflict {
				other := plans[0]
				other.Identity = &pgoracle.SequenceIdentity{ObjectID: 43, Fingerprint: other.Fingerprint()}
				plans = append(plans, other)
			}
			m.cfg.Content = "data_only"
			report := m.Run(WithPGOracleRecoveryEvidence(context.Background(), plans))
			require.Zero(t, report.Tables.Total)
			require.EqualValues(t, 2, w.counts["INDEPENDENT"])
			if conflict {
				require.True(t, report.HasFailures())
				require.Equal(t, 1, report.Data.Success)
				require.Equal(t, 1, report.Data.Failed)
				require.Zero(t, w.counts["ORDERS"])
			} else {
				require.False(t, report.HasFailures(), report)
				require.Equal(t, 2, report.Data.Success)
				require.Equal(t, 1, report.Sequences.Success)
				require.Contains(t, w.allowedTriggers["ORDERS"], "TRG_ORDERS_ID")
				require.Contains(t, w.objects, "sequence:ORDERSEQUENCE")
			}
		})
	}
}
