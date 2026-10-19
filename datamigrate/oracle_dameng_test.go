package datamigrate

import (
	"context"
	"fmt"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

type oracleDMReader struct {
	*dmFixtureReader
	prepared []source.SequenceInfo
}

func (r *oracleDMReader) DBType() string          { return "oracle" }
func (r *oracleDMReader) MigrationSchema() string { return "SRC" }
func (r *oracleDMReader) PrepareNativeSequence(_ context.Context, seq source.SequenceInfo) (source.SequenceInfo, error) {
	r.prepared = append(r.prepared, seq)
	seq.StartValueText = "42"
	return seq, nil
}
func (r *oracleDMReader) ReadPage(_ context.Context, table string, _ []string, offset, _ int64) ([]string, []string, [][]interface{}, error) {
	var cols, types []string
	for _, c := range r.ddl[table].Columns {
		cols = append(cols, c.Name)
		types = append(types, c.DataType)
	}
	if offset > 0 {
		return cols, types, nil, nil
	}
	return cols, types, [][]interface{}{{"41", "AbC\x00中文"}}, nil
}

type oracleDMWriter struct {
	*dmFixtureWriter
	columns                    []string
	sequences, calibrated      []source.SequenceInfo
	existingChecks             int
	existingErr, capabilityErr error
}

func (w *oracleDMWriter) ValidateOracleDamengExisting(context.Context, *source.TableDDLInfo, dialect.TypeOpt) error {
	w.existingChecks++
	return w.existingErr
}
func (w *oracleDMWriter) ValidateOracleDamengCapabilities(context.Context, []*source.TableDDLInfo) error {
	return w.capabilityErr
}
func (w *oracleDMWriter) ValidateDamengColumns(context.Context, *source.TableDDLInfo) error {
	return nil
}
func (w *oracleDMWriter) CopyData(ctx context.Context, table string, cols, types []string, rows [][]interface{}) error {
	w.columns = append([]string(nil), cols...)
	return w.mockWriter.CopyData(ctx, table, cols, types, rows)
}
func (w *oracleDMWriter) CreateSequence(_ context.Context, seq source.SequenceInfo, _ string) error {
	if seq.Native != nil {
		if err := dialect.ValidateOracleDamengSequence(seq); err != nil {
			return err
		}
	}
	w.sequences = append(w.sequences, seq)
	return nil
}
func (w *oracleDMWriter) CalibrateOracleDameng(_ context.Context, seq source.SequenceInfo) error {
	w.calibrated = append(w.calibrated, seq)
	return nil
}

func oracleDMFixture(content string) (*oracleDMReader, *oracleDMWriter, *Migrator) {
	r, w := dmFixture()
	r.ddl["Mixed"].Columns = []source.ColumnInfo{{Name: "RowId", DataType: "NUMBER", Precision: 18}, {Name: "Txt", DataType: "VARCHAR2", Length: 100, DataLength: 400, CharUsed: "C", IsNullable: true}}
	r.views = []source.ViewInfo{{ViewName: "V", Definition: `SELECT "RowId", NVL("Txt", 'AbC') AS "Txt" FROM "SRC"."Mixed"`}}
	rr, ww := &oracleDMReader{dmFixtureReader: r}, &oracleDMWriter{dmFixtureWriter: w}
	m := NewMigrator(rr, ww, &Job{LogCh: make(chan string, 4096)}, Config{Content: content, TargetSchema: "APP", PageSize: 10, MaxParallel: 1, LowerCaseNames: true, ChangeOwner: true, Distributed: true})
	return rr, ww, m
}

func TestOracleDamengTablePreflightBeforeAnyWrite(t *testing.T) {
	for _, kind := range []string{"type", "default", "identity", "name", "capacity", "capability"} {
		t.Run(kind, func(t *testing.T) {
			r, w, m := oracleDMFixture("schema_only")
			r.tables = append(r.tables, "Later")
			r.ddl["Later"] = &source.TableDDLInfo{TableName: "Later", Columns: []source.ColumnInfo{{Name: "X", DataType: "NUMBER", Precision: 10}}}
			switch kind {
			case "type":
				r.ddl["Later"].Columns[0].DataType = "XMLTYPE"
			case "default":
				s := "SEQ.NEXTVAL"
				r.ddl["Later"].Columns[0].Default = &s
			case "identity":
				r.ddl["Later"].Columns[0].Identity = "ALWAYS"
			case "name":
				r.tables = append(r.tables, "MIXED")
			case "capacity":
				m.cfg.Content = "data_only"
				w.existingErr = fmt.Errorf("目标容量不足")
			case "capability":
				w.capabilityErr = fmt.Errorf("IFUN_DATETIME_MODE 需要为1")
			}
			report := m.Run(context.Background())
			require.Positive(t, report.Tables.Failed)
			require.Empty(t, w.created)
			require.Empty(t, w.copied)
			require.Empty(t, w.views)
			require.Empty(t, w.indexes)
		})
	}
}

func TestOracleDamengModesAndRawViewMapping(t *testing.T) {
	for _, mode := range []string{"both", "schema_only", "data_only"} {
		t.Run(mode, func(t *testing.T) {
			_, w, m := oracleDMFixture(mode)
			report := m.Run(context.Background())
			require.False(t, report.HasFailures(), "%+v", report)
			require.False(t, m.cfg.LowerCaseNames || m.cfg.ChangeOwner || m.cfg.Distributed)
			if mode == "data_only" {
				require.Empty(t, w.created)
				require.Empty(t, w.views)
				require.Equal(t, 1, w.existingChecks)
			} else {
				require.Len(t, w.created, 1)
				require.Contains(t, w.created[0], `"ROW_ID" NUMBER(18,0)`)
				require.Len(t, w.views, 1)
				require.Contains(t, w.views[0].Definition, `"ROW_ID"`)
				require.Contains(t, w.views[0].Definition, `'AbC'`)
				require.NotContains(t, w.views[0].Definition, "ctid")
			}
			if mode == "schema_only" {
				require.Empty(t, w.copied)
			} else {
				require.Equal(t, []string{"MIXED"}, w.copied)
				require.Equal(t, []string{"ROW_ID", "TXT"}, w.columns)
			}
		})
	}
}

func TestOracleDamengObjectErrorsRemainCategoryFailures(t *testing.T) {
	r, w, m := oracleDMFixture("schema_only")
	r.indexes = []source.IndexInfo{{TableName: "Mixed", IndexName: "Bad", Unsupported: "函数索引暂不支持"}}
	report := m.Run(context.Background())
	require.Len(t, w.created, 1)
	require.Equal(t, 1, report.Indexes.Failed)
	require.Contains(t, report.Indexes.Items[0].Error, "函数索引")
}

func TestOracleDamengSharedSequenceScopeAndRepair(t *testing.T) {
	r, w, m := oracleDMFixture("schema_only")
	r.sequences = []source.SequenceInfo{{TableName: "Unselected", ColumnName: "ID", StartValueText: "10", Native: &source.NativeSequenceInfo{Name: "SharedSeq", IncrementBy: "1", MinValue: "1", MaxValue: "9223372036854775807", CacheSize: "0", Bindings: []source.SequenceBinding{{TableName: "Unselected", ColumnName: "ID", Kind: "trigger", TriggerName: "TRG_OTHER", OnlyWhenNull: true}, {TableName: "Mixed", ColumnName: "RowId", Kind: "trigger", TriggerName: "TRG_MIXED", OnlyWhenNull: true}}}}}
	var manifest RepairManifest
	m.cfg.CaptureRepair = true
	m.cfg.RepairSink = func(v RepairManifest) error { manifest = v; return nil }
	report := m.Run(context.Background())
	require.False(t, report.HasFailures(), "%+v", report)
	require.Len(t, w.sequences, 1)
	require.Equal(t, "SHAREDSEQ", w.sequences[0].Native.Name)
	require.Equal(t, "42", w.sequences[0].StartValueText)
	require.Len(t, w.sequences[0].Native.Bindings, 1)
	require.Equal(t, "ROW_ID", w.sequences[0].Native.Bindings[0].ColumnName)
	for _, prepared := range r.prepared {
		require.Len(t, prepared.Native.Bindings, 1)
		require.Equal(t, "Mixed", prepared.Native.Bindings[0].TableName)
	}
	found := false
	for _, o := range manifest.Objects {
		if o.Kind == "sequence" {
			found = true
			require.Equal(t, "succeeded", o.Status)
			require.Contains(t, o.Dependencies, repairID("table", "MIXED", "MIXED"))
		}
	}
	require.True(t, found)
}

func TestOracleDamengViewsOnlyAndMissingDependencies(t *testing.T) {
	r, w, m := oracleDMFixture("both")
	r.views = append(r.views, source.ViewInfo{ViewName: "V2", Definition: `SELECT "RowId" FROM "SRC"."V"`})
	out := m.MigrateViews(context.Background(), []string{"V2", "V"})
	require.Len(t, out, 2)
	for _, v := range out {
		require.Empty(t, v.Error, v)
	}
	require.Equal(t, "V", w.views[0].ViewName)
	require.Equal(t, "V2", w.views[1].ViewName)
	require.Empty(t, w.created)
	require.Empty(t, w.copied)
	r.views = []source.ViewInfo{{ViewName: "BAD", Definition: `SELECT ID FROM MISSING`}}
	out = m.MigrateViews(context.Background(), []string{"BAD"})
	require.Len(t, out, 1)
	require.NotEmpty(t, out[0].Error)
	require.Len(t, w.views, 2)
}

func TestOracleDamengRecoveryMapsSharedNativeBindings(t *testing.T) {
	r, w, m := oracleDMFixture("data_only")
	r.sequences = []source.SequenceInfo{{TableName: "Mixed", ColumnName: "RowId", Native: &source.NativeSequenceInfo{Name: "Seq", Bindings: []source.SequenceBinding{{TableName: "Mixed", ColumnName: "RowId", Kind: "trigger", TriggerName: "TRG", OnlyWhenNull: true}, {TableName: "Other", ColumnName: "ID"}}}}}
	values, err := RecoverySequences(context.Background(), r, w, m.cfg, repair.RecoveryPreview{SourceTable: "Mixed", TargetTable: "MIXED", ColumnMapping: map[string]string{"RowId": "ROW_ID"}})
	require.NoError(t, err)
	require.Len(t, values, 1)
	require.Len(t, values[0].Native.Bindings, 1)
	require.Equal(t, "ROW_ID", values[0].Native.Bindings[0].ColumnName)
	require.Equal(t, "COL", recoveryName(Config{LowerCaseNames: true}, "dameng", "oracle", "Col"))
}

func TestOracleDamengViewHeadersAndDependencies(t *testing.T) {
	r, w, m := oracleDMFixture("both")
	r.views[0].Columns = []source.ColumnInfo{{Name: "HeaderId"}, {Name: "HeaderTxt"}}
	r.views = append(r.views, source.ViewInfo{ViewName: "ChildView", Definition: `SELECT "HeaderId" FROM "SRC"."V"`})
	out := m.MigrateViews(context.Background(), []string{"ChildView", "V"})
	require.Len(t, out, 2)
	for _, v := range out {
		require.Empty(t, v.Error, v)
	}
	require.Equal(t, "HEADERID", w.views[0].Columns[0].Name)
	require.Contains(t, out[0].DDL, `("HEADERID", "HEADERTXT")`)
	require.Contains(t, w.views[1].Definition, `"HEADERID"`)
	require.Equal(t, "HeaderId", r.views[0].Columns[0].Name, "source metadata must not be changed")
}

func TestOracleDamengViewCompatibilityAndNamespace(t *testing.T) {
	for _, kind := range []string{"compatibility", "table collision"} {
		t.Run(kind, func(t *testing.T) {
			r, w, m := oracleDMFixture("both")
			if kind == "compatibility" {
				w.capabilityErr = fmt.Errorf("Oracle兼容模式未开启")
			} else {
				r.views[0].ViewName = "mixed"
			}
			out := m.MigrateViews(context.Background(), []string{r.views[0].ViewName})
			require.Len(t, out, 1)
			require.NotEmpty(t, out[0].Error)
			require.Empty(t, w.views)
			require.Empty(t, w.created)
		})
	}
}

func TestOracleDamengObjectsIgnoreUnselectedUnsupportedFK(t *testing.T) {
	r, w, m := oracleDMFixture("schema_only")
	r.fks = []source.FKInfo{{TableName: "Other", ConstraintName: "Unsupported", Unsupported: "跨owner不支持"}}
	m.cfg.Objects = []string{"foreign_keys"}
	m.cfg.TableNames = []string{"Mixed"}
	report := m.Run(context.Background())
	require.Zero(t, report.Constraints.Total)
	require.False(t, report.HasFailures())
	require.Empty(t, w.fks)
}

func TestOracleDamengRecoveryRechecksCapacityBeforeTruncate(t *testing.T) {
	r, w, m := oracleDMFixture("both")
	o := repair.Object{ID: "table-id", Kind: "table", SourceTable: "Mixed", Table: "MIXED", TargetName: "MIXED", SourceColumns: r.ddl["Mixed"].Columns, ColumnMapping: map[string]string{"RowId": "ROW_ID", "Txt": "TXT"}, DataStatus: "pending"}
	truncates := 0
	ops := recoveryOps{schema: func(context.Context, target.Writer) (string, error) { return "APP", nil }, inspect: func(context.Context, target.Writer, string) ([]source.ColumnInfo, error) {
		return []source.ColumnInfo{{Name: "ROW_ID", DataType: "NUMBER"}, {Name: "TXT", DataType: "VARCHAR2", IsNullable: true}}, nil
	}, truncate: func(context.Context, target.Writer, string) error { truncates++; return nil }}
	p, err := previewRecovery(context.Background(), r, w, m.cfg, o, nil, nil, ops)
	require.NoError(t, err)
	w.existingErr = fmt.Errorf("目标DATE会丢失时分秒")
	_, err = runRecovery(context.Background(), r, w, m.cfg, p, true, nil, ops)
	require.ErrorContains(t, err, "目标DATE")
	require.Zero(t, truncates)
	require.Empty(t, w.copied)
}

func TestOracleDamengRepairSharedSequenceKeepsOtherBinding(t *testing.T) {
	_, w, m := oracleDMFixture("both")
	v := oracleDMNativeSequenceForRepair()
	o := repair.Object{Kind: "sequence", Table: "T", Schema: "APP", TargetName: "S", Sequence: &v, ColumnMapping: map[string]string{"Orig": "ID"}}
	table := repair.Object{Kind: "table", Table: "U", ColumnMapping: map[string]string{"Other": "OLD"}}
	got := regenerateRepairCandidate(w, m.cfg, table, o, map[string]string{"Other": "NEW"})
	require.Equal(t, "ID", got.Sequence.Native.Bindings[0].ColumnName)
	require.Equal(t, "NEW", got.Sequence.Native.Bindings[1].ColumnName)
	require.Equal(t, o.ColumnMapping, got.ColumnMapping)
	require.Contains(t, got.SuggestedDDL, `:NEW."NEW"`)
	again := regenerateRepairCandidate(w, m.cfg, table, got, map[string]string{"Other": "NEW"})
	require.Equal(t, got, again)
	third := regenerateRepairCandidate(w, m.cfg, table, again, map[string]string{"Other": "LAST"})
	require.Equal(t, "LAST", third.Sequence.Native.Bindings[1].ColumnName)
	require.Equal(t, "OLD", o.Sequence.Native.Bindings[1].ColumnName, "original manifest metadata is immutable")
}

func oracleDMNativeSequenceForRepair() source.SequenceInfo {
	return source.SequenceInfo{TableName: "T", ColumnName: "ID", StartValueText: "1", Native: &source.NativeSequenceInfo{Name: "S", IncrementBy: "1", MinValue: "1", MaxValue: "1000", CacheSize: "0", Bindings: []source.SequenceBinding{{TableName: "T", ColumnName: "ID", SourceColumnName: "Orig", Kind: "trigger", TriggerName: "TRG_T", OnlyWhenNull: true}, {TableName: "U", ColumnName: "OLD", SourceColumnName: "Other", Kind: "trigger", TriggerName: "TRG_U", OnlyWhenNull: true}}}}
}
