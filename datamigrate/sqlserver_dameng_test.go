package datamigrate

import (
	"context"
	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
	"testing"
)

type dmFixtureReader struct {
	*mockReader
	indexes []source.IndexInfo
	views   []source.ViewInfo
	fks     []source.FKInfo
}

func (r *dmFixtureReader) DBType() string { return "sqlserver" }
func (r *dmFixtureReader) GetIndexes(context.Context) ([]source.IndexInfo, error) {
	return r.indexes, nil
}
func (r *dmFixtureReader) GetViews(context.Context) ([]source.ViewInfo, error)     { return r.views, nil }
func (r *dmFixtureReader) GetForeignKeys(context.Context) ([]source.FKInfo, error) { return r.fks, nil }
func (r *dmFixtureReader) IdentityBounds(context.Context, string, string) (*int64, *int64, error) {
	return nil, nil, nil
}

type dmFixtureWriter struct {
	*mockWriter
	views   []source.ViewInfo
	indexes []source.IndexInfo
	fks     []source.FKInfo
}

func (w *dmFixtureWriter) DBType() string           { return "dameng" }
func (w *dmFixtureWriter) Dialect() dialect.Dialect { return dialect.NewDaMeng() }
func (w *dmFixtureWriter) CreateView(_ context.Context, v source.ViewInfo) error {
	w.views = append(w.views, v)
	return nil
}
func (w *dmFixtureWriter) CreateIndex(_ context.Context, v source.IndexInfo) error {
	w.indexes = append(w.indexes, v)
	return nil
}
func (w *dmFixtureWriter) CreateForeignKey(_ context.Context, v source.FKInfo) error {
	w.fks = append(w.fks, v)
	return nil
}
func dmFixture() (*dmFixtureReader, *dmFixtureWriter) {
	return &dmFixtureReader{mockReader: &mockReader{tables: []string{"Mixed"}, ddl: map[string]*source.TableDDLInfo{"Mixed": {TableName: "Mixed", Columns: []source.ColumnInfo{{Name: "Id", DataType: "int"}}}}}}, &dmFixtureWriter{mockWriter: &mockWriter{}}
}
func dmMigrator(r source.Reader, w *dmFixtureWriter) *Migrator {
	return NewMigrator(r, w, &Job{LogCh: make(chan string, 1024)}, Config{PageSize: 10, MaxParallel: 1, TargetDBType: "dameng", TargetSchema: "APP", Content: "schema_only"})
}
func TestSQLServerDamengPreflightNoWrites(t *testing.T) {
	for _, kind := range []string{"precision", "default", "collision", "index", "view", "foreign key"} {
		t.Run(kind, func(t *testing.T) {
			r, w := dmFixture()
			switch kind {
			case "precision":
				r.ddl["Mixed"].Columns[0] = source.ColumnInfo{Name: "At", DataType: "datetime2", DateTimePrecision: 7}
			case "default":
				s := "newid()"
				r.ddl["Mixed"].Columns[0].Default = &s
			case "collision":
				r.tables = append(r.tables, "MIXED")
				r.ddl["MIXED"] = r.ddl["Mixed"]
			case "index":
				r.indexes = []source.IndexInfo{{TableName: "Mixed", IndexName: "I", Unsupported: "filtered"}}
			case "view":
				r.views = []source.ViewInfo{{ViewName: "V", Definition: "CREATE VIEW dbo.V AS SELECT TOP 1 Id FROM dbo.Mixed"}}
			case "foreign key":
				r.fks = []source.FKInfo{{TableName: "Mixed", ConstraintName: "FK", RefTable: "Other"}}
			}
			m := dmMigrator(r, w)
			report := m.Run(context.Background())
			require.Greater(t, report.Tables.Failed+report.Views.Failed+report.Indexes.Failed+report.Constraints.Failed, 0)
			require.Empty(t, w.created)
			require.Empty(t, w.copied)
			require.Empty(t, w.views)
		})
	}
}
func TestSQLServerDamengSelectedObjectsAndNames(t *testing.T) {
	r, w := dmFixture()
	r.tables = append(r.tables, "Other")
	r.ddl["Other"] = &source.TableDDLInfo{TableName: "Other", Columns: []source.ColumnInfo{{Name: "Id", DataType: "int"}}}
	r.indexes = []source.IndexInfo{{TableName: "Mixed", IndexName: "idx", Columns: []string{"Id"}}, {TableName: "Other", IndexName: "bad", Unsupported: "unsupported"}}
	m := dmMigrator(r, w)
	m.cfg.Objects = []string{"indexes"}
	m.cfg.TableNames = []string{"Mixed"}
	report := m.Run(context.Background())
	require.Equal(t, 0, report.Indexes.Failed)
	require.Len(t, w.indexes, 1)
	require.Equal(t, "MIXED", w.indexes[0].TableName)
	require.Equal(t, "ID", w.indexes[0].Columns[0])
	require.Empty(t, w.created)
}
func TestSQLServerDamengViewsOrderedAndAtomicPreflight(t *testing.T) {
	r, w := dmFixture()
	r.views = []source.ViewInfo{{ViewName: "A", Definition: "CREATE VIEW dbo.A AS SELECT Id FROM dbo.Z"}, {ViewName: "Z", Definition: "CREATE VIEW dbo.Z AS SELECT Id, N'AbC' AS Txt FROM dbo.Mixed"}}
	m := dmMigrator(r, w)
	report := m.Run(context.Background())
	require.Equal(t, 0, report.Views.Failed)
	require.Len(t, w.views, 2)
	require.Equal(t, "Z", w.views[0].ViewName)
	require.Contains(t, w.views[0].Definition, "'AbC'")
	r, w = dmFixture()
	r.views = []source.ViewInfo{{ViewName: "Good", Definition: "CREATE VIEW dbo.Good AS SELECT Id FROM dbo.Mixed"}, {ViewName: "Bad", Definition: "CREATE VIEW dbo.Bad AS SELECT TOP 1 Id FROM dbo.Mixed"}}
	m = dmMigrator(r, w)
	results := m.MigrateViews(context.Background(), []string{"Good", "Bad"})
	require.NotEmpty(t, results[0].Error)
	require.Empty(t, w.views)
}
func TestSQLServerDamengExecutionErrorsAreReported(t *testing.T) {
	r, w := dmFixture()
	r.sequences = []source.SequenceInfo{{TableName: "Mixed", ColumnName: "Id", IdentityInfo: &source.IdentityInfo{Seed: 1, Increment: 1}}}
	r.ddl["Mixed"].Columns[0].Extra = "auto_increment"
	r.ddl["Mixed"].Columns[0].IdentityInfo = r.sequences[0].IdentityInfo
	w.createSeqFail = true
	report := dmMigrator(r, w).Run(context.Background())
	require.Equal(t, 1, report.Sequences.Failed)
	require.NotEmpty(t, report.Sequences.Items[0].Error)
	require.Contains(t, report.Sequences.Items[0].DDL, "DROP IDENTITY")
}

func TestSQLServerDamengDataOnlyContinuesIdentity(t *testing.T) {
	r, w := dmFixture()
	id := &source.IdentityInfo{Seed: 10, Increment: 5}
	r.ddl["Mixed"].Columns[0].Extra = "auto_increment"
	r.ddl["Mixed"].Columns[0].IdentityInfo = id
	r.sequences = []source.SequenceInfo{{TableName: "Mixed", ColumnName: "Id", IdentityInfo: id}}
	m := dmMigrator(r, w)
	m.cfg.Content = "data_only"
	report := m.Run(context.Background())
	require.Zero(t, report.Tables.Failed+report.Data.Failed+report.Sequences.Failed)
	require.Empty(t, w.created)
	require.Len(t, w.sequenceOwners, 1)
}
