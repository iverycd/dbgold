package target

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

// This test writes only uniquely named fixtures in an explicitly supplied empty
// DM schema. It never connects to or changes an Oracle source database.
func TestOracleDamengSequenceIntegration(t *testing.T) {
	dsn, schema := os.Getenv("ORACLE_DM_SEQUENCE_TEST_DSN"), os.Getenv("ORACLE_DM_SEQUENCE_TEST_SCHEMA")
	if os.Getenv("ORACLE_DM_SEQUENCE_TEST_ALLOW_DDL") != "1" || dsn == "" || schema == "" {
		t.Skip("requires ORACLE_DM_SEQUENCE_TEST_DSN, ORACLE_DM_SEQUENCE_TEST_SCHEMA and ORACLE_DM_SEQUENCE_TEST_ALLOW_DDL=1 for an isolated empty DM schema")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	w, err := NewDaMeng(dsn, schema, ConnPoolConfig{})
	if err != nil {
		t.Fatal("cannot connect to the configured DM sequence test instance")
	}
	t.Cleanup(func() { _ = w.Close() })
	w.SetSourceType("oracle")
	schema = w.schema
	var count int
	require.NoError(t, w.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM ALL_TABLES WHERE OWNER=?", schema).Scan(&count))
	require.Zero(t, count, "the explicitly configured DM schema must initially have no tables")
	prefix := fmt.Sprintf("ODMS%X", time.Now().UnixNano())
	var tables, sequences []string
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		for _, table := range tables {
			_, _ = w.db.ExecContext(cleanup, "DROP TABLE "+w.dia.QualifyTable(schema, table)+" CASCADE CONSTRAINTS")
		}
		for _, sequence := range sequences {
			_, _ = w.db.ExecContext(cleanup, "DROP SEQUENCE "+w.dia.QualifyTable(schema, sequence))
		}
	})
	fixture := func(t *testing.T, suffix, start, step, min, max string) source.SequenceInfo {
		t.Helper()
		table, sequence, trigger := prefix+suffix+"T", prefix+suffix+"S", prefix+suffix+"TR"
		tables, sequences = append(tables, table), append(sequences, sequence)
		_, err := w.db.ExecContext(ctx, "CREATE TABLE "+w.dia.QualifyTable(schema, table)+` ("ID" NUMBER(19,0) NOT NULL, "TAG" VARCHAR(30))`)
		require.NoError(t, err)
		return source.SequenceInfo{TableName: table, ColumnName: "ID", StartValueText: start,
			Native: &source.NativeSequenceInfo{Name: sequence, IncrementBy: step, MinValue: min, MaxValue: max, CacheSize: "0",
				Bindings: []source.SequenceBinding{{TableName: table, ColumnName: "ID", Kind: "trigger", TriggerName: trigger, OnlyWhenNull: true}},
			},
		}
	}
	insertExplicit := func(t *testing.T, seq source.SequenceInfo, id int64, tag string) {
		t.Helper()
		_, err := w.db.ExecContext(ctx, "INSERT INTO "+w.dia.QualifyTable(schema, seq.TableName)+` ("ID","TAG") VALUES (?,?)`, id, tag)
		require.NoError(t, err)
	}
	insertAuto := func(t *testing.T, seq source.SequenceInfo, tag string) int64 {
		t.Helper()
		_, err := w.db.ExecContext(ctx, "INSERT INTO "+w.dia.QualifyTable(schema, seq.TableName)+` ("TAG") VALUES (?)`, tag)
		require.NoError(t, err)
		var id int64
		require.NoError(t, w.db.QueryRowContext(ctx, "SELECT \"ID\" FROM "+w.dia.QualifyTable(schema, seq.TableName)+` WHERE "TAG"=?`, tag).Scan(&id))
		return id
	}
	for i, max := range []string{"999999999999999999999999999", "123456789012345678901234"} {
		t.Run(fmt.Sprintf("fresh_clamp_%d", len(max)), func(t *testing.T) {
			seq := fixture(t, fmt.Sprintf("F%d", i), "1", "5", "1", max)
			require.NoError(t, w.CreateSequence(ctx, seq, ""))
			require.Equal(t, max, seq.Native.MaxValue, "creating the target must preserve source metadata")
			var targetMax string
			require.NoError(t, w.db.QueryRowContext(ctx, "SELECT MAX_VALUE FROM ALL_SEQUENCES WHERE SEQUENCE_OWNER=? AND SEQUENCE_NAME=?", schema, seq.Native.Name).Scan(&targetMax))
			require.Equal(t, "9223372036854775807", targetMax)
			var reused []repair.Event
			observed := repair.WithObserver(ctx, func(event repair.Event) error {
				reused = append(reused, event)
				return nil
			})
			require.NoError(t, w.CreateSequence(observed, seq, ""))
			require.Len(t, reused, 2, "the second creation should reuse the sequence and its trigger")
			for _, event := range reused {
				require.Equal(t, "reused", event.Status)
			}
			require.Equal(t, int64(1), insertAuto(t, seq, "first"))
			info := &source.TableDDLInfo{TableName: seq.TableName, Columns: []source.ColumnInfo{{Name: "ID", DataType: "NUMBER", IsNullable: false}}}
			require.NoError(t, w.ValidateOracleDamengLoadTriggers(ctx, info), "read the real DM trigger catalog before data-only loading")
			insertExplicit(t, seq, 77, "explicit")
			var explicit int64
			require.NoError(t, w.db.QueryRowContext(ctx, "SELECT \"ID\" FROM "+w.dia.QualifyTable(schema, seq.TableName)+` WHERE "TAG"='explicit'`).Scan(&explicit))
			require.Equal(t, int64(77), explicit, "managed null-only triggers must preserve imported IDs")
			info.Columns[0].IsNullable = true
			require.ErrorContains(t, w.ValidateOracleDamengLoadTriggers(ctx, info), "可为 NULL")
		})
	}
	for _, descending := range []bool{false, true} {
		t.Run(fmt.Sprintf("bare_sequence_phase_descending=%t", descending), func(t *testing.T) {
			start, phase, step, min, max, suffix := "1", "2", "5", "1", "1000", "A"
			seed, sourceFloor, phaseValue, stepValue := int64(42), "80", int64(2), int64(5)
			if descending {
				start, phase, step, min, max, suffix = "-1", "-2", "-5", "-1000", "-1", "D"
				seed, sourceFloor, phaseValue, stepValue = -42, "-80", -2, -5
			}
			seq := fixture(t, suffix, start, step, min, max)
			bare := seq
			bare.StartValueText = phase
			_, err := w.db.ExecContext(ctx, w.dia.SequenceStatements(schema, bare, "")[0].SQL)
			require.NoError(t, err, "create only a compatible bare sequence, leaving its trigger missing")
			insertExplicit(t, seq, seed, "seed")
			require.NoError(t, w.CreateSequence(ctx, seq, ""), "reuse the bare sequence, advance it safely and add the missing trigger")
			first := insertAuto(t, seq, "first")
			require.Equal(t, int64(0), (first-phaseValue)%stepValue)
			if descending {
				require.Less(t, first, seed)
			} else {
				require.Greater(t, first, seed)
			}
			seq.StartValueText = sourceFloor
			require.NoError(t, w.CalibrateOracleDameng(ctx, seq))
			second := insertAuto(t, seq, "second")
			require.Equal(t, int64(0), (second-phaseValue)%stepValue, "calibration must preserve the existing target progression")
			if descending {
				require.LessOrEqual(t, second, int64(-80))
			} else {
				require.GreaterOrEqual(t, second, int64(80))
			}
			require.NoError(t, w.CalibrateOracleDameng(ctx, seq))
			third := insertAuto(t, seq, "third")
			require.Equal(t, second+stepValue, third, "repeated calibration must not rewind an active target sequence")
		})
	}
	t.Run("reviewed_generated_repair", func(t *testing.T) {
		seq := fixture(t, "R", "1", "5", "1", "1000")
		insertExplicit(t, seq, 42, "seed")
		applyPlan := func() {
			statements, err := w.PlanOracleDamengSequence(ctx, seq)
			require.NoError(t, err)
			require.NotEmpty(t, statements)
			object := repair.Object{Kind: "sequence", Schema: schema, Table: seq.TableName, TargetName: seq.Native.Name, Sequence: &seq}
			for i, statement := range statements {
				object.Units = append(object.Units, repair.Unit{ID: fmt.Sprintf("step-%d", i+1), SQL: statement.SQL, OriginalSQL: statement.SQL, Selected: true, Status: "pending"})
			}
			units, err := ExecuteRepair(ctx, w, object, object.Units, func(saved []repair.Unit) error {
				require.Len(t, saved, len(statements))
				return nil
			})
			require.NoError(t, err)
			for i, unit := range units {
				require.Equal(t, statements[i].SQL, unit.SQL, "execute exactly the reviewed generated SQL")
				require.Equal(t, "succeeded", unit.Status)
				require.False(t, unit.Selected)
			}
		}
		applyPlan()
		first := insertAuto(t, seq, "first")
		require.Greater(t, first, int64(42))
		require.Equal(t, int64(0), (first-1)%5)
		insertExplicit(t, seq, 200, "later-import")
		applyPlan()
		second := insertAuto(t, seq, "second")
		require.Greater(t, second, int64(200))
		require.Equal(t, int64(0), (second-1)%5)
		require.NoError(t, w.CalibrateOracleDameng(ctx, seq))
		verification, err := InspectRepair(ctx, w, repair.Object{Kind: "sequence", Schema: schema, TargetName: seq.Native.Name, Sequence: &seq})
		require.NoError(t, err)
		require.Equal(t, "succeeded", strings.TrimSpace(verification.Status))
	})
}
