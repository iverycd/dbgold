package datamigrate

import (
	"context"
	"errors"
	"testing"

	"dbgold/datamigrate/source"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

type pgPartitionScopeReader struct {
	*pgPlanReader
	all         []string
	roots       map[string]string
	specs       map[string]*source.PartitionSpec
	scoped      []string
	legacyReads int
}

func (r *pgPartitionScopeReader) ListTables(context.Context) ([]string, error) { return r.all, nil }
func (r *pgPartitionScopeReader) PartitionMetadata(_ context.Context, table string) (*source.PartitionSpec, error) {
	return source.ClonePartitionSpec(r.specs[table]), nil
}
func (r *pgPartitionScopeReader) PartitionRoots(context.Context) (map[string]string, error) {
	return r.roots, nil
}
func (r *pgPartitionScopeReader) StreamPartitionRows(context.Context, string, int, func([]string, []string, [][]interface{}) error) error {
	return errors.New("unexpected stream in preflight")
}
func (r *pgPartitionScopeReader) GetPrimaryKeys(context.Context) ([]source.IndexInfo, error) {
	return nil, nil
}
func (r *pgPartitionScopeReader) GetIndexes(context.Context) ([]source.IndexInfo, error) {
	return nil, nil
}
func (r *pgPartitionScopeReader) GetForeignKeys(context.Context) ([]source.FKInfo, error) {
	return nil, nil
}
func (r *pgPartitionScopeReader) ReadPGSnapshot(ctx context.Context) (*source.PGSnapshot, error) {
	r.legacyReads++
	return r.pgPlanReader.ReadPGSnapshot(ctx)
}
func (r *pgPartitionScopeReader) ReadPGSnapshotForTables(_ context.Context, names []string) (*source.PGSnapshot, error) {
	r.scoped = append([]string{}, names...)
	selected := map[string]bool{}
	for _, name := range names {
		selected[name] = true
	}
	snapshot := *r.snapshot
	snapshot.Tables = nil
	for _, table := range r.snapshot.Tables {
		if selected[table.Info.TableName] {
			snapshot.Tables = append(snapshot.Tables, table)
		}
	}
	return &snapshot, r.err
}

func newPGPartitionScope(t *testing.T) (*Migrator, *pgPartitionScopeReader, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, mock.ExpectationsWereMet()); db.Close() })
	good := source.PGTable{Info: source.TableDDLInfo{TableName: "Good", Columns: []source.ColumnInfo{{Name: "Id", DataType: "int4"}}}}
	r := &pgPartitionScopeReader{pgPlanReader: &pgPlanReader{snapshot: &source.PGSnapshot{Profile: source.PGProfile{Schema: "public"}, Tables: []source.PGTable{good}}}, all: []string{"Blocked", "Good", "Physical", "Excluded"}, roots: map[string]string{"Physical": "Blocked"}, specs: map[string]*source.PartitionSpec{"Blocked": {Unsupported: "unproven native partition"}}}
	w := &pgPlanWriter{db: db}
	m := NewMigrator(r, w, &Job{LogCh: make(chan string, 1024)}, Config{Mode: "all", Content: "schema_only", TargetDBType: "mysql", ExcludedObjects: []ObjectRef{tableRef("Excluded")}})
	return m, r, mock
}

func expectPGPartitionScopeTarget(mock sqlmock.Sqlmock, privileges bool) {
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM information_schema.SCHEMATA`).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`SELECT TABLE_SCHEMA,TABLE_NAME,CONSTRAINT_NAME`).WillReturnRows(sqlmock.NewRows([]string{"schema", "table", "constraint", "refschema", "reftable"}))
	if privileges {
		mock.ExpectQuery(`SELECT ENGINE,TABLE_TYPE`).WillReturnRows(sqlmock.NewRows([]string{"engine", "type", "rowformat"}))
		mock.ExpectQuery(`SELECT TABLE_NAME,VIEW_DEFINITION`).WillReturnRows(sqlmock.NewRows([]string{"name", "definition"}))
		mock.ExpectQuery(`SHOW GRANTS`).WillReturnRows(sqlmock.NewRows([]string{"grant"}).AddRow("GRANT ALL PRIVILEGES ON *.* TO 'test'@'localhost'"))
	}
}

func TestPGPartitionSnapshotScopeKeepsHealthyRootsAndExclusionIdentities(t *testing.T) {
	m, r, mock := newPGPartitionScope(t)
	expectPGPartitionScopeTarget(mock, true)
	p := m.PreflightPGMySQL(context.Background())
	require.Equal(t, []string{"Good"}, r.scoped)
	require.Zero(t, r.legacyReads)
	require.False(t, p.Ready)
	require.True(t, p.PartitionPreflight.CanStart)
	require.Len(t, p.tables, 1)
	require.Equal(t, "Good", p.tables[0].Info.TableName)
	for _, issue := range p.Issues {
		require.NotContains(t, issue.Reason, "排除项不存在")
		require.NotEqual(t, "selection", issue.Object.Kind)
	}
}

func TestPGPartitionForeignKeyDependencyDoesNotBlockHealthyTable(t *testing.T) {
	m, r, mock := newPGPartitionScope(t)
	r.snapshot.Tables[0].ForeignKeys = []source.FKInfo{{TableName: "Good", ConstraintName: "ToBlocked", Columns: []string{"Id"}, RefSchema: "public", RefTable: "Blocked", RefColumns: []string{"Id"}, OnDelete: "NO ACTION", OnUpdate: "NO ACTION"}, {TableName: "Good", ConstraintName: "ToExcluded", Columns: []string{"Id"}, RefSchema: "public", RefTable: "Excluded", RefColumns: []string{"Id"}, OnDelete: "NO ACTION", OnUpdate: "NO ACTION"}}
	expectPGPartitionScopeTarget(mock, true)
	p := m.PreflightPGMySQL(context.Background())
	require.True(t, p.PartitionPreflight.CanStart)
	require.Len(t, p.tables, 1)
	require.Empty(t, p.validationFKs)
	warnings := 0
	for _, issue := range p.Issues {
		if issue.Object.Kind == "foreign_key" {
			require.Equal(t, "warning", issue.Severity)
			require.Contains(t, issue.Reason, "本次跳过")
			warnings++
		}
	}
	require.Equal(t, 2, warnings)
	for _, object := range p.Objects {
		require.NotEqual(t, "foreign_key", object.Object.Kind)
	}
}

func TestPGPartitionStrictOtherErrorsStillPreventPartialStart(t *testing.T) {
	m, r, mock := newPGPartitionScope(t)
	r.snapshot.Tables[0].Info.Columns[0].DataType = "_int4"
	expectPGPartitionScopeTarget(mock, false)
	p := m.PreflightPGMySQL(context.Background())
	require.False(t, p.Ready)
	require.False(t, p.PartitionPreflight.CanStart)
}

func TestPGPartitionAllBlockedKeepsSpecificDiagnosis(t *testing.T) {
	m, r, _ := newPGPartitionScope(t)
	m.cfg.Mode = "include"
	m.cfg.Filter = "Blocked"
	p := m.PreflightPGMySQL(context.Background())
	require.False(t, p.Ready)
	require.False(t, p.PartitionPreflight.CanStart)
	require.Nil(t, r.scoped)
	require.Zero(t, r.legacyReads)
	for _, issue := range p.Issues {
		require.NotEqual(t, "selection", issue.Object.Kind)
	}
}

func TestPGPartitionExplicitBlockedNamesDoNotBecomeMissingTableErrors(t *testing.T) {
	m, r, _ := newPGPartitionScope(t)
	m.cfg.TableNames = []string{"Blocked"}
	p := m.PreflightPGMySQL(context.Background())
	require.False(t, p.Ready)
	require.False(t, p.PartitionPreflight.CanStart)
	require.Nil(t, r.scoped)
	for _, issue := range p.Issues {
		require.NotContains(t, issue.Reason, "选择的源表不存在")
		require.NotEqual(t, "selection", issue.Object.Kind)
	}
}
