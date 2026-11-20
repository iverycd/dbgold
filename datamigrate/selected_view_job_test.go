package datamigrate

import (
	"context"
	"errors"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

type selectedViewWriter struct {
	mockWriter
	names []string
}

func (w *selectedViewWriter) CreateView(_ context.Context, v source.ViewInfo) error {
	w.names = append(w.names, v.ViewName)
	if v.ViewName == "Broken" {
		return errors.New("target rejected view")
	}
	return nil
}

func TestSelectedViewJobRecordsOnlySelectedViewsBeforeExecution(t *testing.T) {
	reader := newOscarFixture()
	reader.views = []source.ViewInfo{
		{ViewName: "Ignored", Definition: "SELECT 1 AS id"},
		{ViewName: "Broken", Definition: "SELECT 2 AS id"},
		{ViewName: "Working", Definition: "SELECT 3 AS id"},
	}
	writer := &selectedViewWriter{}
	m, _ := newTestMigrator(reader, writer)
	m.cfg.TargetDBType, m.cfg.TargetSchema, m.cfg.CaptureRepair = "postgres", "dst", true
	m.cfg.ViewNames = []string{"Working", "Broken"}
	var checkpoints int
	m.cfg.RepairSink = func(manifest RepairManifest) error {
		checkpoints++
		require.Equal(t, m.cfg.ViewNames, manifest.Config.ViewNames)
		require.Len(t, manifest.Objects, 2)
		if checkpoints == 1 {
			require.Empty(t, writer.names)
			for _, object := range manifest.Objects {
				require.Equal(t, "view", object.Kind)
				require.Len(t, object.Units, 1)
				require.Equal(t, "pending", object.Status)
				require.Equal(t, object.Units[0].SQL, object.DDL)
			}
		}
		return nil
	}
	var progress []string
	m.cfg.RepairObjectSink = func(o repair.Object) error {
		progress = append(progress, o.TargetName)
		return nil
	}
	report, results := m.RunSelectedViews(context.Background())
	require.Equal(t, 2, checkpoints)
	require.ElementsMatch(t, m.cfg.ViewNames, writer.names)
	require.Empty(t, writer.created)
	require.Empty(t, writer.copied)
	require.Contains(t, progress, "Working")
	require.Contains(t, progress, "Broken")
	require.Len(t, results, 2)
	require.Equal(t, 1, report.Views.Success)
	require.Equal(t, 1, report.Views.Failed)
	for _, o := range report.RepairManifest.Objects {
		require.Equal(t, o.Status, o.OriginalStatus)
		if o.SourceName == "Broken" {
			require.Contains(t, o.OriginalError, "target rejected view")
		}
	}
}

func TestSelectedViewJobRequiresInitialCheckpoint(t *testing.T) {
	reader := newOscarFixture()
	writer := &selectedViewWriter{}
	m, _ := newTestMigrator(reader, writer)
	m.cfg.CaptureRepair = true
	m.cfg.ViewNames = []string{reader.views[0].ViewName}
	m.cfg.RepairSink = func(RepairManifest) error { return errors.New("cannot save checkpoint") }
	report, results := m.RunSelectedViews(context.Background())
	require.True(t, report.HasFailures())
	require.Len(t, results, 1)
	require.Contains(t, results[0].Error, "cannot save checkpoint")
	require.Empty(t, writer.names)
	require.Empty(t, writer.created)
}

func TestSelectedViewJobUsesFreshMetadataAndDependenciesWhenReused(t *testing.T) {
	reader, writer := newOscarFixture(), &selectedViewWriter{}
	reader.views = []source.ViewInfo{{ViewName: "SameName", Unsupported: "original source error"}}
	m, _ := newTestMigrator(reader, writer)
	m.cfg.TargetDBType, m.cfg.TargetSchema = "postgres", "dst"
	m.cfg.ViewNames, m.cfg.CaptureRepair = []string{"SameName"}, true
	ctx := context.Background()
	first, _ := m.RunSelectedViews(ctx)
	require.Equal(t, 1, first.Views.Failed)
	require.Len(t, first.RepairManifest.Objects, 1)

	for _, tc := range []struct {
		sql     string
		capture bool
	}{{"SELECT 2 AS id", true}, {"SELECT 3 AS id", false}} {
		reader.views = []source.ViewInfo{{ViewName: "SameName", Definition: tc.sql}}
		m.cfg.CaptureRepair = tc.capture
		got, results := m.RunSelectedViews(ctx)
		require.Len(t, results, 1)
		require.Equal(t, 1, got.Views.Success, results)
		require.Contains(t, results[0].DDL, tc.sql)
		if tc.capture {
			require.Equal(t, tc.sql, got.RepairManifest.Objects[0].SourceDDL)
		} else {
			require.Nil(t, got.RepairManifest)
			require.Nil(t, m.repairs)
		}
	}
	require.Equal(t, []string{"SameName", "SameName"}, writer.names)
}

type countedOscarViewReader struct {
	*oscarFixtureReader
	reads int
}

func (r *countedOscarViewReader) GetViews(ctx context.Context) ([]source.ViewInfo, error) {
	r.reads++
	return r.oscarFixtureReader.GetViews(ctx)
}

type oscarSelectedMySQLWriter struct {
	selectedViewWriter
	oscarMySQLWriter
}

func (*oscarSelectedMySQLWriter) DBType() string                                { return "mysql" }
func (*oscarSelectedMySQLWriter) Dialect() dialect.Dialect                      { return dialect.NewMySQL() }
func (*oscarSelectedMySQLWriter) ConfigureOscarMigration(context.Context) error { return nil }
func (*oscarSelectedMySQLWriter) MySQLTableNameKey(name string) string          { return name }

func TestSelectedViewJobOscarMySQLReadsOneSnapshotPerRun(t *testing.T) {
	reader := &countedOscarViewReader{oscarFixtureReader: newOscarFixture()}
	reader.srcType = "oscar"
	writer := &oscarSelectedMySQLWriter{}
	m, _ := newTestMigrator(reader, writer)
	m.cfg.ViewNames, m.cfg.CaptureRepair = []string{"vOrders"}, true
	m.cfg.TargetSchema = "dst"
	for i, alias := range []string{"first_id", "second_id"} {
		definition := "SELECT id AS " + alias + " FROM Orders"
		reader.views = []source.ViewInfo{{ViewName: "vOrders", Definition: definition}}
		report, results := m.RunSelectedViews(context.Background())
		require.Equal(t, i+1, reader.reads)
		require.Len(t, results, 1)
		require.Equal(t, 1, report.Views.Success, results)
		require.Contains(t, results[0].DDL, alias)
		require.Equal(t, results[0].DDL, report.RepairManifest.Objects[0].DDL)
		require.Equal(t, definition, report.RepairManifest.Objects[0].SourceDDL)
	}
	require.Equal(t, []string{"vOrders", "vOrders"}, writer.names)
}
