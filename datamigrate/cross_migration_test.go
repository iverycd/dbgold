package datamigrate

import (
	"context"
	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"errors"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func crossFixture(src string) *oracleFixtureReader {
	r := &oracleFixtureReader{newOscarFixture()}
	r.srcType = src
	r.tables = []string{"A", "B", "C"}
	r.ddl = map[string]*source.TableDDLInfo{}
	for _, name := range r.tables {
		r.rows[name] = [][]interface{}{{int64(1)}}
		r.ddl[name] = &source.TableDDLInfo{TableName: name, Columns: []source.ColumnInfo{{Name: "Id", DataType: "int"}}}
	}
	r.primary = nil
	r.indexes = nil
	r.sequences = nil
	r.foreign = nil
	r.comments = nil
	r.views = nil
	return r
}
func TestCrossOracleTableFailuresContinue(t *testing.T) {
	for _, src := range []string{"dameng", "sqlserver"} {
		for _, table := range []string{"A", "B"} {
			for _, phase := range []string{"generate", "drop", "create"} {
				t.Run(src+table+phase, func(t *testing.T) {
					r := crossFixture(src)
					w := &oracleRecordingWriter{dia: dialect.NewOracle()}
					if phase == "generate" {
						r.ddl[table].Columns[0].DataType = "unsupported_type"
					} else {
						statement := "DROP TABLE"
						if phase == "create" {
							statement = "CREATE TABLE"
						}
						w.ddlFailure = statement + ` "APP"."` + table + `"`
					}
					m := NewMigrator(r, w, &Job{LogCh: make(chan string, 512)}, Config{TargetDBType: "oracle", TargetSchema: "APP", Mode: "all", Content: "both", PageSize: 10, MaxParallel: 1})
					report := m.Run(context.Background())
					require.True(t, report.HasFailures(), report)
					require.Contains(t, w.copied, "C")
					require.NotContains(t, w.copied, table)
					require.Equal(t, 2, report.Tables.Success)
					require.Equal(t, 1, report.Tables.Failed)
				})
			}
		}
	}
}
func TestCrossDamengSQLServerTableFailuresContinue(t *testing.T) {
	for _, table := range []string{"A", "B"} {
		for _, phase := range []string{"generate", "DROP", "CREATE"} {
			t.Run(table+phase, func(t *testing.T) {
				r := crossFixture("dameng")
				w := &crossSQLServerFaultWriter{sqlServerRecordingWriter: &sqlServerRecordingWriter{dia: dialect.NewSQLServer()}}
				if phase == "generate" {
					r.ddl[table].Columns[0].DataType = "unsupported_type"
				} else {
					w.failStatement = phase + " TABLE [dbo].[" + table + "]"
				}
				m := NewMigrator(r, w, &Job{LogCh: make(chan string, 512)}, Config{TargetDBType: "sqlserver", TargetSchema: "dbo", Mode: "all", Content: "both", PageSize: 10, MaxParallel: 1})
				report := m.Run(context.Background())
				require.True(t, report.HasFailures(), report)
				require.Contains(t, w.copied, "C")
				require.NotContains(t, w.copied, table)
				require.Equal(t, 2, report.Tables.Success)
				require.Equal(t, 1, report.Tables.Failed)
			})
		}
	}
}

type crossSQLServerFaultWriter struct {
	*sqlServerRecordingWriter
	failStatement string
}

func (w *crossSQLServerFaultWriter) CreateTable(ctx context.Context, statements []dialect.Statement) error {
	for _, statement := range statements {
		if w.failStatement != "" && strings.Contains(statement.SQL, w.failStatement) {
			return errors.New("table statement rejected")
		}
	}
	return w.sqlServerRecordingWriter.CreateTable(ctx, statements)
}
func TestCrossOraclePreflightReadOnlyAndExcluded(t *testing.T) {
	r := crossFixture("sqlserver")
	r.ddl["A"].Columns[0].DataType = "unsupported_type"
	w := &oracleRecordingWriter{dia: dialect.NewOracle()}
	m := NewMigrator(r, w, &Job{LogCh: make(chan string, 512)}, Config{TargetDBType: "oracle", TargetSchema: "APP", Mode: "all", Content: "both"})
	p := m.PreflightCrossMigration(context.Background())
	require.False(t, p.Ready)
	require.True(t, p.CanStart)
	require.Empty(t, w.sql)
	require.Empty(t, w.copied)
	require.NotEmpty(t, p.Issues)
	m.cfg.ExcludedObjects = []ObjectRef{{Kind: "table", Name: "A"}}
	p = m.PreflightCrossMigration(context.Background())
	require.True(t, p.Ready)
	for _, o := range p.Objects {
		require.NotEqual(t, "A", o.Object.Name)
	}
}
func TestCrossOracleSequencePlanPreservesProgression(t *testing.T) {
	r := crossFixture("sqlserver")
	w := &oracleRecordingWriter{dia: dialect.NewOracle()}
	m := NewMigrator(r, w, &Job{}, Config{TargetDBType: "oracle", TargetSchema: "APP"})
	s := source.SequenceInfo{StartValueText: "-20", Native: &source.NativeSequenceInfo{Name: "Seq", Schema: "dbo", IncrementBy: "-5", MinValue: "-99999", MaxValue: "100", CacheSize: "0", Bindings: []source.SequenceBinding{{Schema: "dbo", TableName: "A", ColumnName: "Id", Kind: "identity"}}}}
	s = m.crossOracleSequence(s, map[string]bool{"A": true})
	require.Empty(t, s.Unsupported)
	sql := dialect.JoinSQL(w.dia.SequenceStatements("APP", s, ""))
	require.Contains(t, sql, "MIN(")
	require.Contains(t, sql, "INCREMENT BY -5")
	require.NotContains(t, sql, "DROP SEQUENCE")
	require.True(t, strings.Contains(sql, "FLOOR(ABS(x-n)"))
	s.Native.OracleObjectID = 123
	ctx := WithCrossOracleEvidence(context.Background(), []source.SequenceInfo{s})
	copy := s
	n := *s.Native
	n.OracleObjectID = 0
	copy.Native = &n
	copy = bindCrossOracleEvidence(ctx, copy)
	require.EqualValues(t, 123, copy.Native.OracleObjectID)
}

func TestCrossOracleObjectFailuresKeepTableAndIndependentObjects(t *testing.T) {
	r := crossFixture("sqlserver")
	r.ddl["A"].Columns[0].IsNullable = true
	r.indexes = []source.IndexInfo{{TableName: "A", IndexName: "UniqueNullable", Columns: []string{"Id"}, IsUnique: true}, {TableName: "C", IndexName: "HealthyIdx", Columns: []string{"Id"}}}
	w := &oracleRecordingWriter{dia: dialect.NewOracle()}
	m := NewMigrator(r, w, &Job{LogCh: make(chan string, 512)}, Config{TargetDBType: "oracle", TargetSchema: "APP", Mode: "all", Content: "both", PageSize: 10, MaxParallel: 1})
	report := m.Run(context.Background())
	require.Equal(t, 3, report.Tables.Success)
	require.Equal(t, 1, report.Indexes.Success)
	require.Equal(t, 1, report.Indexes.Failed)
	require.Contains(t, report.Indexes.Items[0].Error, "NULL")
	require.Contains(t, w.copied, "A")
}
func TestCrossOracleFailedTableSkipsDependentObjects(t *testing.T) {
	r := crossFixture("dameng")
	r.indexes = []source.IndexInfo{{TableName: "A", IndexName: "Bad", Columns: []string{"Id"}}, {TableName: "C", IndexName: "Good", Columns: []string{"Id"}}}
	r.foreign = []source.FKInfo{{TableName: "C", ConstraintName: "DependsA", Columns: []string{"Id"}, RefTable: "A", RefColumns: []string{"Id"}}}
	w := &oracleRecordingWriter{dia: dialect.NewOracle(), ddlFailure: `CREATE TABLE "APP"."A"`}
	report := NewMigrator(r, w, &Job{LogCh: make(chan string, 512)}, Config{TargetDBType: "oracle", TargetSchema: "APP", Mode: "all", Content: "both", PageSize: 10, MaxParallel: 1}).Run(context.Background())
	require.Equal(t, 1, report.Indexes.Success)
	require.Equal(t, "dependency", report.Indexes.Items[0].Stage)
	require.Equal(t, "dependency", report.Constraints.Items[0].Stage)
	require.Contains(t, w.copied, "C")
	require.NotContains(t, w.copied, "A")
}
func TestCrossOracleViewsOnlyAndObjectOnlyDoNotRebuildTables(t *testing.T) {
	for _, mode := range []string{"views", "objects"} {
		t.Run(mode, func(t *testing.T) {
			r := crossFixture("sqlserver")
			r.views = []source.ViewInfo{{ViewName: "MixedView", Definition: `SELECT [Id] FROM [Src].[C]`}}
			r.indexes = []source.IndexInfo{{TableName: "C", IndexName: "MixedIndex", Columns: []string{"Id"}}}
			w := &oracleRecordingWriter{dia: dialect.NewOracle()}
			cfg := Config{TargetDBType: "oracle", TargetSchema: "APP", Mode: "all", Content: "schema_only", CaptureRepair: true}
			if mode == "views" {
				cfg.ViewNames = []string{"MixedView"}
			} else {
				cfg.Objects = []string{"indexes"}
				cfg.TableNames = []string{"C"}
			}
			report := NewMigrator(r, w, &Job{LogCh: make(chan string, 512)}, cfg).Run(context.Background())
			require.False(t, report.HasFailures(), report)
			require.Empty(t, w.ddlAttempts)
			require.Empty(t, w.copied)
			if mode == "views" {
				require.Equal(t, 1, report.Views.Success)
			} else {
				require.Equal(t, 1, report.Indexes.Success)
			}
		})
	}
}

type crossAppendWriter struct {
	*oracleRecordingWriter
	counts map[string]int64
}

func (w *crossAppendWriter) CountRows(_ context.Context, t string) (int64, error) {
	return w.counts[t], nil
}
func (w *crossAppendWriter) CopyData(ctx context.Context, t string, cols, types []string, rows [][]any) error {
	w.counts[t] += int64(len(rows))
	return w.recordingOscarWriter.CopyData(ctx, t, cols, types, rows)
}
func TestCrossOracleDataOnlyCountsAppendedRows(t *testing.T) {
	r := &crossCountReader{crossFixture("sqlserver")}
	w := &crossAppendWriter{oracleRecordingWriter: &oracleRecordingWriter{dia: dialect.NewOracle()}, counts: map[string]int64{"A": 5, "B": 9, "C": 12}}
	report := NewMigrator(r, w, &Job{LogCh: make(chan string, 512)}, Config{TargetDBType: "oracle", TargetSchema: "APP", Mode: "all", Content: "data_only", PageSize: 10, MaxParallel: 1}).Run(context.Background())
	require.False(t, report.HasFailures(), report)
	require.Empty(t, w.ddlAttempts)
	require.Len(t, report.RowCounts, 3)
	for _, v := range report.RowCounts {
		require.True(t, v.Match)
		require.EqualValues(t, 1, v.Dst)
	}
}

func TestCrossOracleDataOnlyBlocksUnsafeGeneratorConsumers(t *testing.T) {
	r := crossFixture("dameng")
	r.sequences = []source.SequenceInfo{{StartValueText: "10", Unsupported: "未知自增状态", Native: &source.NativeSequenceInfo{Schema: "Src", Name: "Auto", IncrementBy: "5", MinValue: "1", MaxValue: "1000", CacheSize: "0", Bindings: []source.SequenceBinding{{Schema: "Src", TableName: "A", ColumnName: "Id", Kind: "identity"}}}}}
	w := &oracleRecordingWriter{dia: dialect.NewOracle()}
	report := NewMigrator(r, w, &Job{LogCh: make(chan string, 512)}, Config{TargetDBType: "oracle", TargetSchema: "APP", Mode: "all", Content: "data_only", PageSize: 10, MaxParallel: 1}).Run(context.Background())
	require.True(t, report.HasFailures())
	require.NotContains(t, w.copied, "A")
	require.Contains(t, w.copied, "C")
	require.Empty(t, w.ddlAttempts)
}

type crossCountReader struct{ *oracleFixtureReader }

func (r *crossCountReader) CountRows(_ context.Context, t string) (int64, error) {
	return int64(len(r.rows[t])), nil
}
