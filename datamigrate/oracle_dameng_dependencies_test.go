package datamigrate

import (
	"context"
	"database/sql/driver"
	"fmt"
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

type oracleDMDependencyReader struct {
	*oracleDMReader
	primary  []source.IndexInfo
	comments []source.CommentInfo
}

func (r *oracleDMDependencyReader) GetPrimaryKeys(context.Context) ([]source.IndexInfo, error) {
	return r.primary, nil
}
func (r *oracleDMDependencyReader) GetComments(context.Context) ([]source.CommentInfo, error) {
	return r.comments, nil
}

type oracleDMDependencyWriter struct {
	*oracleDMWriter
	capability map[string]error
	defaults   map[string]error
	comments   []source.CommentInfo
	probes     []string
}

func (w *oracleDMDependencyWriter) PrepareOracleDameng(_ context.Context, infos []*source.TableDDLInfo, _ dialect.TypeOpt) error {
	for _, info := range infos {
		w.probes = append(w.probes, info.TableName)
		if err := w.capability[info.TableName]; err != nil {
			return err
		}
	}
	return nil
}
func (w *oracleDMDependencyWriter) ValidateOracleDamengDefaults(_ context.Context, infos []*source.TableDDLInfo, _ dialect.TypeOpt) error {
	for _, info := range infos {
		if err := w.defaults[info.TableName]; err != nil {
			return err
		}
	}
	return nil
}
func (w *oracleDMDependencyWriter) CreateComment(_ context.Context, value source.CommentInfo) error {
	w.comments = append(w.comments, value)
	return nil
}

func oracleDMDependencyFixture(t *testing.T) (*oracleDMDependencyReader, *oracleDMDependencyWriter, *Migrator) {
	t.Helper()
	r, w, m := oracleDMFixture("schema_only")
	r.tables = append(r.tables, "Bad")
	r.ddl["Bad"] = &source.TableDDLInfo{TableName: "Bad", Columns: []source.ColumnInfo{{Name: "Id", DataType: "NUMBER", Precision: 12}}}
	rr := &oracleDMDependencyReader{oracleDMReader: r}
	ww := &oracleDMDependencyWriter{oracleDMWriter: w, capability: map[string]error{}, defaults: map[string]error{}}
	m.reader, m.writer = rr, ww
	return rr, ww, m
}

func TestOracleDamengManifestCapabilityAndDefaultErrorsAreTableScoped(t *testing.T) {
	for _, phase := range []string{"capability", "default"} {
		t.Run(phase, func(t *testing.T) {
			_, w, m := oracleDMDependencyFixture(t)
			if phase == "capability" {
				w.capability["Bad"] = fmt.Errorf("Bad.TS 精度不支持")
			} else {
				w.defaults["Bad"] = fmt.Errorf("Bad.Id 默认值不支持")
			}
			m.beginRepair(context.Background())
			require.NoError(t, m.repairs.sinkErr)
			seen := map[string]repair.Object{}
			for _, object := range m.repairs.objects {
				if object.Kind == "table" {
					seen[object.SourceTable] = object
				}
			}
			require.Empty(t, seen["Mixed"].Error)
			require.NotEmpty(t, seen["Mixed"].Units)
			require.Contains(t, seen["Mixed"].DDL, `CREATE TABLE "APP"."MIXED"`)
			require.Contains(t, seen["Bad"].Error, "Bad")
			require.Empty(t, seen["Bad"].Units)
		})
	}
}

func TestOracleDamengFailedTableBlocksOnlyDependentObjectsAndKeepsSharedRepairBindings(t *testing.T) {
	r, w, m := oracleDMDependencyFixture(t)
	w.capability["Bad"] = fmt.Errorf("Bad.TS 精度不支持")
	r.primary = []source.IndexInfo{
		{TableName: "Mixed", IndexName: "PK_Mixed", Columns: []string{"RowId"}, IsPrimary: true, IsUnique: true},
		{TableName: "Bad", IndexName: "PK_Bad", Columns: []string{"Id"}, IsPrimary: true, IsUnique: true},
	}
	r.indexes = []source.IndexInfo{{TableName: "Mixed", IndexName: "I_Mixed", Columns: []string{"Txt"}}, {TableName: "Bad", IndexName: "I_Bad", Columns: []string{"Id"}}}
	r.comments = []source.CommentInfo{{TableName: "Mixed", ColumnName: "Txt", Comment: "healthy"}, {TableName: "Bad", Comment: "blocked"}}
	r.fks = []source.FKInfo{
		{TableName: "Mixed", ConstraintName: "FK_BadParent", Columns: []string{"RowId"}, RefTable: "Bad", RefColumns: []string{"Id"}},
		{TableName: "Bad", ConstraintName: "FK_BadChild", Columns: []string{"Id"}, RefTable: "Mixed", RefColumns: []string{"RowId"}},
		{TableName: "Mixed", ConstraintName: "FK_Healthy", Columns: []string{"RowId"}, RefTable: "Mixed", RefColumns: []string{"RowId"}},
	}
	r.views = append(r.views,
		source.ViewInfo{ViewName: "BadView", Definition: `SELECT "Id" FROM "SRC"."Bad"`},
		source.ViewInfo{ViewName: "BadChildView", Definition: `SELECT "Id" FROM "SRC"."BadView"`})
	r.sequences = []source.SequenceInfo{{TableName: "Bad", ColumnName: "Id", StartValueText: "1", Native: &source.NativeSequenceInfo{Name: "Shared", IncrementBy: "1", MinValue: "1", MaxValue: "1000", CacheSize: "0", Bindings: []source.SequenceBinding{
		{TableName: "Bad", ColumnName: "Id", Kind: "trigger", TriggerName: "BadTrigger", OnlyWhenNull: true},
		{TableName: "Mixed", ColumnName: "RowId", Kind: "trigger", TriggerName: "GoodTrigger", OnlyWhenNull: true},
	}}}}
	m.cfg.CaptureRepair = true
	var manifest RepairManifest
	m.cfg.RepairSink = func(value RepairManifest) error { manifest = value; return nil }
	report := m.Run(context.Background())
	require.Equal(t, 1, report.Tables.Success)
	require.Equal(t, 1, report.Tables.Failed)
	require.Len(t, w.created, 1)
	require.Len(t, w.indexes, 2, "healthy PK and index must still execute")
	for _, index := range w.indexes {
		require.Equal(t, "MIXED", index.TableName)
	}
	require.Equal(t, 1, report.PrimaryKeys.Success)
	require.Equal(t, 1, report.PrimaryKeys.Failed)
	require.Equal(t, 1, report.Indexes.Success)
	require.Equal(t, 1, report.Indexes.Failed)
	require.Len(t, w.comments, 1)
	require.Equal(t, 1, report.Comments.Success)
	require.Equal(t, 1, report.Comments.Failed)
	require.Len(t, w.fks, 1)
	require.Equal(t, "FK_HEALTHY", w.fks[0].ConstraintName)
	require.Equal(t, 2, report.Constraints.Failed)
	require.Equal(t, 1, report.Views.Success)
	require.Equal(t, 2, report.Views.Failed)
	for _, view := range report.Views.Items {
		require.Equal(t, "dependency", view.Stage)
	}
	require.Len(t, w.views, 1)
	require.Len(t, w.sequences, 1)
	require.Equal(t, "BAD", w.sequences[0].TableName, "repair identity must not change with active binding")
	require.Len(t, w.sequences[0].Native.Bindings, 1)
	require.Equal(t, "MIXED", w.sequences[0].Native.Bindings[0].TableName)
	for _, sequence := range r.prepared {
		require.Len(t, sequence.Native.Bindings, 1)
		require.Equal(t, "Mixed", sequence.Native.Bindings[0].TableName)
	}
	require.Equal(t, 1, report.Sequences.Success)
	require.Equal(t, 1, report.Sequences.Failed)
	require.Equal(t, "dependency", report.Sequences.Items[0].Stage)
	sequenceObjects := 0
	for _, object := range manifest.Objects {
		if object.Kind != "sequence" {
			continue
		}
		sequenceObjects++
		require.Equal(t, "blocked", object.Status)
		require.False(t, object.DiagnosticOnly)
		require.NotNil(t, object.Sequence)
		require.Len(t, object.Sequence.Native.Bindings, 2, "blocked binding metadata must remain recoverable")
		blockedUnit, healthyUnit := false, false
		for _, unit := range object.Units {
			if strings.Contains(unit.SQL, `"BADTRIGGER"`) {
				blockedUnit = unit.Status == "blocked" && unit.Selected
			}
			if strings.Contains(unit.SQL, `"GOODTRIGGER"`) {
				healthyUnit = unit.Status == "succeeded"
			}
		}
		require.True(t, blockedUnit, "missing trigger must stay pending in the repair inventory")
		require.True(t, healthyUnit)
	}
	require.Equal(t, 1, sequenceObjects, "a shared source sequence has one stable repair object")
}

func TestOracleDamengCalibrationSkipsOnlyFailedBindings(t *testing.T) {
	r, w, m := oracleDMDependencyFixture(t)
	r.sequences = []source.SequenceInfo{{TableName: "Bad", ColumnName: "Id", StartValueText: "1", Native: &source.NativeSequenceInfo{Name: "Shared", IncrementBy: "1", MinValue: "1", MaxValue: "1000", CacheSize: "0", Bindings: []source.SequenceBinding{{TableName: "Bad", ColumnName: "Id", Kind: "trigger"}, {TableName: "Mixed", ColumnName: "RowId", Kind: "trigger"}}}}}
	m.dmBlocked = map[string]bool{"Bad": true}
	report := newMigrationReport()
	m.calibrateOracleDameng(context.Background(), &report, []string{"Bad", "Mixed"})
	require.Len(t, w.calibrated, 1)
	require.Len(t, w.calibrated[0].Native.Bindings, 1)
	require.Equal(t, "MIXED", w.calibrated[0].Native.Bindings[0].TableName)
	require.Equal(t, "BAD", w.calibrated[0].TableName)
	require.Equal(t, 1, report.Sequences.Success)
	require.Equal(t, 1, report.Sequences.Failed)
}

func TestOracleDamengStandaloneViewsStopOnceOnConnectionFailure(t *testing.T) {
	r, w, m := oracleDMDependencyFixture(t)
	r.views = []source.ViewInfo{
		{ViewName: "First", Definition: `SELECT "Txt" FROM "SRC"."Mixed"`},
		{ViewName: "Disconnected", Definition: `SELECT "Txt" FROM "SRC"."Mixed"`},
		{ViewName: "Never", Definition: `SELECT "Txt" FROM "SRC"."Mixed"`},
	}
	w.capability["Disconnected"] = driver.ErrBadConn
	results := m.MigrateViews(context.Background(), []string{"First", "Disconnected", "Never"})
	require.Len(t, results, 2)
	require.Empty(t, results[0].Error)
	require.Equal(t, "FIRST", results[0].Name)
	require.Equal(t, "task", results[1].Stage)
	require.ErrorContains(t, fmt.Errorf("%s", results[1].Error), "bad connection")
	require.Len(t, w.views, 1)
	require.NotContains(t, w.probes, "Never")
}
