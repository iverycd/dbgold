package datamigrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

func TestRepairManifestTracksActualPartialExecutionAndPreservesReport(t *testing.T) {
	r := &mockReader{tables: []string{"MixedCase"}, ddl: map[string]*source.TableDDLInfo{"MixedCase": {TableName: "MixedCase", Columns: []source.ColumnInfo{{Name: "ID", DataType: "int"}}}}}
	m, _ := newTestMigrator(r, &mockWriter{})
	m.cfg.CaptureRepair = true
	m.cfg.TargetSchema = "dst"
	m.cfg.LowerCaseNames = true
	var snapshots []RepairManifest
	m.cfg.RepairSink = func(v RepairManifest) error {
		b, e := json.Marshal(v)
		require.NoError(t, e)
		var copy RepairManifest
		require.NoError(t, json.Unmarshal(b, &copy))
		snapshots = append(snapshots, copy)
		return nil
	}
	m.beginRepair(context.Background())
	ss := []dialect.Statement{{SQL: `DROP TABLE "dst"."mixedcase"`}, {SQL: `CREATE TABLE "dst"."mixedcase" (id invalid)`}}
	err := m.captureRepairCall(context.Background(), repair.Object{Kind: "table", Name: "MixedCase", Table: "mixedcase", TargetName: "mixedcase"}, ss, func(ctx context.Context) error {
		require.NoError(t, repair.Observe(ctx, ss[0].SQL, "running", nil))
		require.NoError(t, repair.Observe(ctx, ss[0].SQL, "succeeded", nil))
		require.NoError(t, repair.Observe(ctx, ss[1].SQL, "running", nil))
		e := errors.New("unknown type")
		require.NoError(t, repair.Observe(ctx, ss[1].SQL, "failed", e))
		return e
	})
	require.Error(t, err)
	report := newMigrationReport()
	report.Tables = CategoryReport{Total: 1, Failed: 1, Items: []ObjectResult{{Name: "MixedCase", DDL: dialect.JoinSQL(ss), Error: err.Error()}}}
	m.finishRepair(context.Background(), &report)
	require.Equal(t, 1, report.Tables.Failed)
	require.Equal(t, dialect.JoinSQL(ss), report.Tables.Items[0].DDL)
	require.NotNil(t, report.RepairManifest)
	o := report.RepairManifest.Objects[0]
	require.Equal(t, "MixedCase", o.SourceName)
	require.Equal(t, "mixedcase", o.TargetName)
	require.Equal(t, "failed", o.OriginalStatus)
	require.Equal(t, "succeeded", o.Units[0].Status)
	require.False(t, o.Units[0].Selected)
	require.True(t, o.Units[0].Destructive)
	require.Equal(t, "failed", o.Units[1].Status)
	require.True(t, o.Units[1].Selected)
	require.GreaterOrEqual(t, len(snapshots), 6)
}
func TestRepairSinkFailurePreventsMigrationMutation(t *testing.T) {
	r := &mockReader{tables: []string{"T"}, ddl: map[string]*source.TableDDLInfo{"T": {TableName: "T", Columns: []source.ColumnInfo{{Name: "id", DataType: "int"}}}}}
	w := &mockWriter{}
	m, _ := newTestMigrator(r, w)
	m.cfg.CaptureRepair = true
	m.cfg.RepairSink = func(RepairManifest) error { return errors.New("disk full") }
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.Empty(t, w.created)
	require.Empty(t, w.copied)
}

func TestRepairManifestRepeatedSQLRetainsEachCommitAndRollback(t *testing.T) {
	for _, failSecond := range []bool{false, true} {
		t.Run(fmt.Sprint(failSecond), func(t *testing.T) {
			m, _ := newTestMigrator(&mockReader{}, &mockWriter{})
			m.beginRepair(context.Background())
			ss := []dialect.Statement{{SQL: "ALTER TABLE t ADD a INT"}, {SQL: "ALTER TABLE t ADD a INT"}}
			err := m.captureRepairCall(context.Background(), repair.Object{Kind: "table", Table: "t", TargetName: "t"}, ss, func(ctx context.Context) error {
				for _, s := range ss {
					require.NoError(t, repair.Observe(ctx, s.SQL, "running", nil))
				}
				if failSecond {
					e := errors.New("duplicate column")
					require.NoError(t, repair.Observe(ctx, ss[1].SQL, "failed", e))
					require.NoError(t, repair.Observe(ctx, ss[0].SQL, "rolled_back", nil))
					return e
				}
				for _, s := range ss {
					require.NoError(t, repair.Observe(ctx, s.SQL, "succeeded", nil))
				}
				return nil
			})
			units := m.repairs.objects[0].Units
			require.Len(t, units, 2)
			if failSecond {
				require.Error(t, err)
				require.Equal(t, "rolled_back", units[0].Status)
				require.Equal(t, "failed", units[1].Status)
			} else {
				require.NoError(t, err)
				for _, u := range units {
					require.Equal(t, "succeeded", u.Status)
				}
			}
		})
	}
}

type repairDependencyReader struct{ *mockReader }

func (r *repairDependencyReader) GetPrimaryKeys(context.Context) ([]source.IndexInfo, error) {
	return []source.IndexInfo{{TableName: "Parent", IndexName: "pk", Columns: []string{"ID"}, IsPrimary: true}}, nil
}
func (r *repairDependencyReader) GetForeignKeys(context.Context) ([]source.FKInfo, error) {
	return []source.FKInfo{{TableName: "Child", ConstraintName: "fk", Columns: []string{"ID"}, RefTable: "Parent", RefColumns: []string{"ID"}}}, nil
}
func TestRepairManifestRecordsUnreachedOwnerAndReferencedKey(t *testing.T) {
	r := &repairDependencyReader{&mockReader{tables: []string{"Parent", "Child"}, ddl: map[string]*source.TableDDLInfo{
		"Parent": {Columns: []source.ColumnInfo{{Name: "ID", DataType: "int"}}},
		"Child":  {Columns: []source.ColumnInfo{{Name: "ID", DataType: "int"}}},
	}}}
	m, _ := newTestMigrator(r, &mockWriter{})
	m.cfg.ChangeOwner, m.cfg.TargetSchema = true, "dst"
	m.beginRepair(context.Background())
	seen := map[string]repair.Object{}
	for _, o := range m.repairs.objects {
		seen[o.Kind+":"+o.TargetName] = o
	}
	owner := seen["owner:TABLE:Parent"]
	require.NotEmpty(t, owner.ID)
	require.Equal(t, []string{seen["table:Parent"].ID}, owner.Dependencies)
	require.Contains(t, seen["foreign_key:fk"].Dependencies, seen["primary_key:pk"].ID)
	report := newMigrationReport()
	m.finishRepair(context.Background(), &report)
	for _, o := range report.RepairManifest.Objects {
		if o.Kind == "owner" {
			require.Equal(t, "blocked", o.Status)
		}
	}
}
func TestLegacyRepairNeverSplitsProceduralBodiesOrReplaysDrop(t *testing.T) {
	report := newMigrationReport()
	report.Tables.Items = []ObjectResult{{Name: "T", DDL: "DROP TABLE T; CREATE TABLE T(id INT)", Error: "failed"}, {Name: "U", DDL: "CREATE TABLE U(id INT);", Error: "failed"}}
	report.Sequences.Items = []ObjectResult{{Name: "S", DDL: "DECLARE v NUMBER; BEGIN v := 1; END;", Error: "failed"}}
	objects := LegacyRepairObjects(report, "dst")
	require.Len(t, objects, 3)
	require.Empty(t, objects[0].Units)
	require.Len(t, objects[1].Units, 1)
	require.Equal(t, "failed", objects[1].Units[0].Status)
	require.Empty(t, objects[2].Units)
	require.True(t, objects[2].RequiresReview)
	require.Equal(t, objects, LegacyRepairObjects(report, "dst"))
	require.NotEqual(t, repairID("index", "a", "same"), repairID("index", "b", "same"))
}
func TestRecoveryPreviewAllowsRepairedTypes(t *testing.T) {
	tests := []struct {
		name   string
		source source.ColumnInfo
		target source.ColumnInfo
	}{
		{"decimal to bigint", source.ColumnInfo{DataType: "decimal", Precision: 20, Scale: 2}, source.ColumnInfo{DataType: "bigint"}},
		{"different family", source.ColumnInfo{DataType: "int"}, source.ColumnInfo{DataType: "text"}},
		{"shorter text", source.ColumnInfo{DataType: "varchar", Length: 100}, source.ColumnInfo{DataType: "varchar", Length: 20}},
		{"lower numeric precision", source.ColumnInfo{DataType: "decimal", Precision: 20, Scale: 4}, source.ColumnInfo{DataType: "numeric", Precision: 12, Scale: 4}},
		{"lower numeric scale", source.ColumnInfo{DataType: "decimal", Precision: 20, Scale: 4}, source.ColumnInfo{DataType: "numeric", Precision: 20, Scale: 2}},
		{"smaller integer", source.ColumnInfo{DataType: "bigint"}, source.ColumnInfo{DataType: "integer"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, w, cfg, o, ops, truncated := recoveryFixture(t)
			tt.source.Name, tt.target.Name = "ID", "renamed"
			r.ddl[o.SourceTable].Columns = []source.ColumnInfo{tt.source}
			o.SourceColumns = r.ddl[o.SourceTable].Columns
			ops.inspect = func(context.Context, target.Writer, string) ([]source.ColumnInfo, error) {
				return []source.ColumnInfo{tt.target}, nil
			}
			p, e := previewRecovery(context.Background(), r, w, cfg, o, nil, nil, ops)
			require.NoError(t, e)
			require.Equal(t, []source.ColumnInfo{tt.source}, p.SourceColumns)
			require.Equal(t, []source.ColumnInfo{tt.target}, p.TargetColumns)
			require.Zero(t, *truncated)
			require.Zero(t, w.writes)
		})
	}
}

func TestRecoveryPreviewPreservesColumnGuards(t *testing.T) {
	tests := []struct {
		name    string
		source  []source.ColumnInfo
		target  []source.ColumnInfo
		mapping map[string]string
		err     string
	}{
		{name: "missing target column", target: []source.ColumnInfo{{Name: "other", DataType: "int"}}, err: "目标列不存在"},
		{name: "duplicate mapping", source: []source.ColumnInfo{{Name: "ID", DataType: "int"}, {Name: "Other", DataType: "int"}}, mapping: map[string]string{"ID": "renamed", "Other": "renamed"}, err: "多个源列映射到目标列"},
		{name: "generated target column", target: []source.ColumnInfo{{Name: "renamed", DataType: "int", Generated: "computed"}}, err: "为生成列"},
		{name: "nullable source", source: []source.ColumnInfo{{Name: "ID", DataType: "int", IsNullable: true}}, err: "收紧了可空性"},
		{name: "unmapped required target column", target: []source.ColumnInfo{{Name: "renamed", DataType: "int"}, {Name: "required", DataType: "int"}}, err: "未映射的目标必填列"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, w, cfg, o, ops, truncated := recoveryFixture(t)
			if tt.source != nil {
				r.ddl[o.SourceTable].Columns = tt.source
				o.SourceColumns = tt.source
			}
			if tt.target != nil {
				ops.inspect = func(context.Context, target.Writer, string) ([]source.ColumnInfo, error) {
					return tt.target, nil
				}
			}
			if tt.mapping != nil {
				o.ColumnMapping = tt.mapping
			}
			_, e := previewRecovery(context.Background(), r, w, cfg, o, nil, nil, ops)
			require.ErrorContains(t, e, tt.err)
			require.Zero(t, *truncated)
			require.Zero(t, w.writes)
		})
	}
}
func TestRecoveryMappingDoesNotReplaceHandwrittenSQL(t *testing.T) {
	w := &mockWriter{}
	table := repair.Object{Kind: "table", Table: "T", ColumnMapping: map[string]string{"Old": "old"}}
	idx := repair.Object{Kind: "index", Table: "T", Schema: "dst", TargetName: "ix", ColumnMapping: map[string]string{"Old": "old"}, Index: &source.IndexInfo{TableName: "T", IndexName: "ix", Columns: []string{"old"}}, Units: []repair.Unit{{ID: "x", OriginalSQL: "generated", SQL: "my custom SQL"}}}
	got := regenerateRepairCandidate(w, Config{}, table, idx, map[string]string{"Old": "new"})
	require.True(t, got.RequiresReview)
	require.Equal(t, "my custom SQL", got.Units[0].SQL)
	require.Contains(t, got.SuggestedDDL, `"new"`)
	require.Equal(t, []string{"old"}, idx.Index.Columns)
	view := repair.Object{Kind: "view", DDL: "select old from T", Units: []repair.Unit{{SQL: "select old from T"}}}
	got = regenerateRepairCandidate(w, Config{}, table, view, map[string]string{"Old": "new"})
	require.True(t, got.RequiresReview)
	require.Equal(t, view.Units, got.Units)
}
func TestRecoveryMappingUsesReferencedTableMappingForForeignKey(t *testing.T) {
	parent := repair.Object{Kind: "table", Table: "parent", ColumnMapping: map[string]string{"Key": "parent_old"}}
	fk := repair.Object{Kind: "foreign_key", Table: "child", ColumnMapping: map[string]string{"Key": "child_key"}, RefColumnMapping: map[string]string{"Key": "parent_old"}, ForeignKey: &source.FKInfo{TableName: "child", Columns: []string{"child_key"}, RefTable: "parent", RefColumns: []string{"parent_old"}}}
	got := regenerateRepairCandidate(&mockWriter{}, Config{}, parent, fk, map[string]string{"Key": "parent_new"})
	require.Equal(t, []string{"child_key"}, got.ForeignKey.Columns)
	require.Equal(t, []string{"parent_new"}, got.ForeignKey.RefColumns)
	require.Equal(t, fk.ColumnMapping, got.ColumnMapping)
	require.Equal(t, map[string]string{"Key": "parent_new"}, got.RefColumnMapping)
	require.Contains(t, got.SuggestedDDL, `"parent_new"`)
	again := regenerateRepairCandidate(&mockWriter{}, Config{}, parent, got, map[string]string{"Key": "parent_new"})
	require.Equal(t, got, again)
}

type recoveryTestReader struct {
	*mockReader
	all         [][]any
	readOffsets []int64
	failOffset  int64
}

func (r *recoveryTestReader) CountRows(context.Context, string) (int64, error) {
	return int64(len(r.all)), nil
}
func (r *recoveryTestReader) ReadPage(_ context.Context, _ string, _ []string, offset, limit int64) ([]string, []string, [][]any, error) {
	r.readOffsets = append(r.readOffsets, offset)
	if offset == r.failOffset {
		return nil, nil, nil, fmt.Errorf("read failed")
	}
	end := min(int64(len(r.all)), offset+limit)
	if offset > end {
		offset = end
	}
	return []string{"ID"}, []string{"INT"}, r.all[offset:end], nil
}

type recoveryTestWriter struct {
	*mockWriter
	count     int64
	writes    int
	failWrite int
	columns   []string
}

func (w *recoveryTestWriter) CountRows(context.Context, string) (int64, error) { return w.count, nil }
func (w *recoveryTestWriter) CopyData(_ context.Context, _ string, cols, _ []string, rows [][]any) error {
	w.writes++
	if w.writes == w.failWrite {
		return errors.New("batch failed")
	}
	w.columns = slices.Clone(cols)
	w.count += int64(len(rows))
	return nil
}
func recoveryFixture(t *testing.T) (*recoveryTestReader, *recoveryTestWriter, Config, repair.Object, recoveryOps, *int) {
	t.Helper()
	cols := []source.ColumnInfo{{Name: "ID", DataType: "int", IsNullable: false}}
	r := &recoveryTestReader{mockReader: &mockReader{ddl: map[string]*source.TableDDLInfo{"SourceT": {TableName: "SourceT", Columns: cols}}}, all: [][]any{{1}, {2}, {3}, {4}, {5}}, failOffset: -1}
	w := &recoveryTestWriter{mockWriter: &mockWriter{}}
	cfg := Config{Content: "both", PageSize: 2, TargetSchema: "dst"}
	o := repair.Object{ID: "table-id", Kind: "table", SourceTable: "SourceT", Table: "TargetT", TargetName: "TargetT", SourceColumns: cols, ColumnMapping: map[string]string{"ID": "renamed"}, DataStatus: "pending"}
	truncated := new(int)
	ops := recoveryOps{schema: func(context.Context, target.Writer) (string, error) { return "dst", nil }, inspect: func(context.Context, target.Writer, string) ([]source.ColumnInfo, error) {
		return []source.ColumnInfo{{Name: "renamed", DataType: "integer"}}, nil
	}, prepare: func(context.Context, target.Writer, string, []source.ColumnInfo) error { return nil }, truncate: func(context.Context, target.Writer, string) error { *truncated++; w.count = 0; return nil }, calibrate: func(context.Context, target.Writer, string, []source.SequenceInfo) error { return nil }}
	return r, w, cfg, o, ops, truncated
}
func TestRecoveryEmptyTableFullReloadAndColumnMapping(t *testing.T) {
	r, w, cfg, o, ops, truncated := recoveryFixture(t)
	p, e := previewRecovery(context.Background(), r, w, cfg, o, nil, nil, ops)
	require.NoError(t, e)
	res, e := runRecovery(context.Background(), r, w, cfg, p, false, nil, ops)
	require.NoError(t, e)
	require.Equal(t, "succeeded", res.DataStatus)
	require.Equal(t, int64(5), res.CommittedRows)
	require.Equal(t, 0, *truncated)
	require.Equal(t, []string{"renamed"}, w.columns)
}
func TestRecoveryPartialFailureStopsAndCannotAppend(t *testing.T) {
	r, w, cfg, o, ops, _ := recoveryFixture(t)
	w.failWrite = 2
	p, e := previewRecovery(context.Background(), r, w, cfg, o, nil, nil, ops)
	require.NoError(t, e)
	res, e := runRecovery(context.Background(), r, w, cfg, p, false, nil, ops)
	require.Error(t, e)
	require.Equal(t, int64(2), res.CommittedRows)
	require.Equal(t, 2, w.writes)
	require.Equal(t, []int64{0, 2}, r.readOffsets)
	p, e = previewRecovery(context.Background(), r, w, cfg, o, nil, nil, ops)
	require.NoError(t, e)
	_, e = runRecovery(context.Background(), r, w, cfg, p, false, nil, ops)
	require.ErrorContains(t, e, "目标表非空")
	require.Equal(t, 2, w.writes)
	w.failWrite = 0
	p, e = previewRecovery(context.Background(), r, w, cfg, o, nil, nil, ops)
	require.NoError(t, e)
	res, e = runRecovery(context.Background(), r, w, cfg, p, true, nil, ops)
	require.NoError(t, e)
	require.Equal(t, int64(5), res.TargetRows)
}
func TestRecoveryValidatesBeforeTruncatingAndBindsPreview(t *testing.T) {
	r, w, cfg, o, ops, truncated := recoveryFixture(t)
	w.count = 3
	p, e := previewRecovery(context.Background(), r, w, cfg, o, nil, nil, ops)
	require.NoError(t, e)
	r.failOffset = 0
	_, e = runRecovery(context.Background(), r, w, cfg, p, true, nil, ops)
	require.Error(t, e)
	require.Zero(t, *truncated)
	require.Equal(t, int64(3), w.count)
	r.failOffset = -1
	w.count = 4
	_, e = runRecovery(context.Background(), r, w, cfg, p, true, nil, ops)
	require.ErrorContains(t, e, "已变化")
	require.Zero(t, *truncated)
	w.count = 3
	_, e = runRecovery(context.Background(), r, w, cfg, p, true, func(repair.RecoveryResult) error { return errors.New("disk full") }, ops)
	require.Error(t, e)
	require.Zero(t, *truncated)
}

func TestRecoveryRejectsTargetTypeChangedAfterPreview(t *testing.T) {
	r, w, cfg, o, ops, truncated := recoveryFixture(t)
	w.count = 3
	actual := source.ColumnInfo{Name: "renamed", DataType: "integer"}
	ops.inspect = func(context.Context, target.Writer, string) ([]source.ColumnInfo, error) {
		return []source.ColumnInfo{actual}, nil
	}
	p, e := previewRecovery(context.Background(), r, w, cfg, o, nil, nil, ops)
	require.NoError(t, e)
	actual.DataType = "bigint"
	res, e := runRecovery(context.Background(), r, w, cfg, p, true, nil, ops)
	require.ErrorContains(t, e, "源／目标结构或行数已变化")
	require.Zero(t, res.CommittedRows)
	require.Zero(t, *truncated)
	require.Zero(t, w.writes)
	require.Equal(t, int64(3), w.count)
	require.Empty(t, r.readOffsets)
}

func TestRecoverySchemaOnlyNeverCopiesOrTruncates(t *testing.T) {
	r, w, cfg, o, ops, truncated := recoveryFixture(t)
	cfg.Content = "schema_only"
	w.count = 3
	p, e := previewRecovery(context.Background(), r, w, cfg, o, nil, nil, ops)
	require.NoError(t, e)
	require.False(t, p.DataRequired)
	_, e = runRecovery(context.Background(), r, w, cfg, p, true, nil, ops)
	require.NoError(t, e)
	require.Zero(t, *truncated)
	require.Zero(t, w.writes)
}
func TestRecoveryDefersSelectedSequenceUntilItIsCreated(t *testing.T) {
	r, w, cfg, o, ops, _ := recoveryFixture(t)
	r.sequences = []source.SequenceInfo{{TableName: o.SourceTable, ColumnName: "Original"}}
	// Use the fixture's actual source mapping rather than a synthetic column.
	for col := range o.ColumnMapping {
		r.sequences[0].ColumnName = col
	}
	related := []repair.Object{{Kind: "sequence", Table: o.Table, Status: "pending", Sequence: &source.SequenceInfo{TableName: o.Table, ColumnName: "renamed"}}}
	ops.calibrate = func(_ context.Context, _ target.Writer, _ string, sequences []source.SequenceInfo) error {
		require.Empty(t, sequences)
		return nil
	}
	p, e := previewRecovery(context.Background(), r, w, cfg, o, related, nil, ops)
	require.NoError(t, e)
	res, e := runRecovery(context.Background(), r, w, cfg, p, false, nil, ops)
	require.NoError(t, e)
	require.Equal(t, "succeeded", res.DataStatus)
}
func TestRecoverySequenceFailureDoesNotUndoDataSuccess(t *testing.T) {
	r, w, cfg, o, ops, _ := recoveryFixture(t)
	ops.calibrate = func(context.Context, target.Writer, string, []source.SequenceInfo) error {
		return errors.New("calibration failed")
	}
	p, e := previewRecovery(context.Background(), r, w, cfg, o, nil, nil, ops)
	require.NoError(t, e)
	res, e := runRecovery(context.Background(), r, w, cfg, p, false, nil, ops)
	require.Error(t, e)
	require.Equal(t, "succeeded", res.DataStatus)
	require.Equal(t, int64(5), w.count)
}
