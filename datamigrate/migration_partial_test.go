package datamigrate

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

type partialMigrationReader struct {
	*oscarFixtureReader
	metadataError error
}

func (r *partialMigrationReader) GetTableDDLInfo(ctx context.Context, table string) (*source.TableDDLInfo, error) {
	if table == "Broken" && r.metadataError != nil {
		return nil, r.metadataError
	}
	return r.oscarFixtureReader.GetTableDDLInfo(ctx, table)
}

func (r *partialMigrationReader) GetViewRelationColumns(ctx context.Context, schema, relation string) (*source.TableDDLInfo, error) {
	if schema != "" && !strings.EqualFold(schema, "dbo") {
		return nil, fmt.Errorf("fixture relation %s.%s does not exist", schema, relation)
	}
	for name := range r.ddl {
		if strings.EqualFold(name, relation) {
			// Keep the same metadata failure injection as ordinary table reads.
			return r.GetTableDDLInfo(ctx, name)
		}
	}
	for _, view := range r.views {
		if strings.EqualFold(view.ViewName, relation) {
			return &source.TableDDLInfo{TableName: view.ViewName, Columns: []source.ColumnInfo{{Name: "Id", DataType: "int"}}}, nil
		}
	}
	return nil, fmt.Errorf("fixture relation %s.%s does not exist", schema, relation)
}

type partialMigrationDialect struct {
	dialect.Dialect
	fail bool
}

func (d partialMigrationDialect) CreateTableStatements(schema string, info *source.TableDDLInfo, src string, opt dialect.TypeOpt, name dialect.NameFunc) ([]dialect.Statement, error) {
	if d.fail && info.TableName == "Broken" {
		return nil, errors.New("column type cannot be mapped")
	}
	return d.Dialect.CreateTableStatements(schema, info, src, opt, name)
}

type partialMigrationWriter struct {
	mockWriter
	kind, failStage string
	failure         error
	indexes         []source.IndexInfo
	fks             []source.FKInfo
	comments        []source.CommentInfo
	views           []source.ViewInfo
	sequences       []source.SequenceInfo
	attempts        []string
}

func (w *partialMigrationWriter) DBType() string { return w.kind }
func (w *partialMigrationWriter) Dialect() dialect.Dialect {
	return partialMigrationDialect{Dialect: dialect.NewPostgres(w.kind), fail: w.failStage == "generate"}
}
func (w *partialMigrationWriter) CreateTable(_ context.Context, statements []dialect.Statement) error {
	for _, statement := range statements {
		w.attempts = append(w.attempts, statement.SQL)
		if strings.Contains(statement.SQL, `"broken"`) &&
			(w.failStage == "drop" && strings.HasPrefix(statement.SQL, "DROP ") || w.failStage == "create" && strings.HasPrefix(statement.SQL, "CREATE ")) {
			return w.failure
		}
	}
	w.created = append(w.created, dialect.JoinSQL(statements))
	return nil
}
func (w *partialMigrationWriter) CreateIndex(_ context.Context, index source.IndexInfo) error {
	w.indexes = append(w.indexes, index)
	return nil
}
func (w *partialMigrationWriter) CreateForeignKey(_ context.Context, fk source.FKInfo) error {
	w.fks = append(w.fks, fk)
	return nil
}
func (w *partialMigrationWriter) CreateComment(_ context.Context, comment source.CommentInfo) error {
	w.comments = append(w.comments, comment)
	return nil
}
func (w *partialMigrationWriter) CreateView(_ context.Context, view source.ViewInfo) error {
	w.views = append(w.views, view)
	return nil
}
func (w *partialMigrationWriter) CreateSequence(_ context.Context, seq source.SequenceInfo, _ string) error {
	w.sequences = append(w.sequences, seq)
	return nil
}

func partialMigrationFixture(sourceType, targetType, failure string, first bool) (*partialMigrationReader, *partialMigrationWriter, *Migrator) {
	r := &partialMigrationReader{oscarFixtureReader: &oscarFixtureReader{srcType: sourceType}}
	r.tables = []string{"A", "Broken", "Z"}
	if first {
		r.tables = []string{"Broken", "A", "Z"}
	}
	r.ddl, r.pk, r.rows = map[string]*source.TableDDLInfo{}, map[string][]string{}, map[string][][]interface{}{}
	for _, table := range r.tables {
		r.ddl[table] = &source.TableDDLInfo{TableName: table, Columns: []source.ColumnInfo{{Name: "Id", DataType: "int"}}}
		r.pk[table], r.rows[table] = []string{"Id"}, [][]interface{}{{1}}
		r.primary = append(r.primary, source.IndexInfo{TableName: table, IndexName: "PK_" + table, IsPrimary: true, Columns: []string{"Id"}})
		r.indexes = append(r.indexes, source.IndexInfo{TableName: table, IndexName: "IDX_" + table, Columns: []string{"Id"}})
		r.comments = append(r.comments, source.CommentInfo{TableName: table, Comment: "original"})
		r.sequences = append(r.sequences, source.SequenceInfo{TableName: table, ColumnName: "Id", StartValue: 2})
	}
	r.foreign = []source.FKInfo{
		{TableName: "Z", RefTable: "A", ConstraintName: "healthy_fk", Columns: []string{"Id"}, RefColumns: []string{"Id"}},
		{TableName: "Z", RefTable: "Broken", ConstraintName: "blocked_fk", Columns: []string{"Id"}, RefColumns: []string{"Id"}},
	}
	r.views = []source.ViewInfo{
		{ViewName: "HealthyView", Definition: `SELECT "Id" FROM "Z"`},
		{ViewName: "FailedView", Definition: `SELECT "Id" FROM "Broken"`},
		{ViewName: "ChildView", Definition: `SELECT "Id" FROM "FailedView"`},
	}
	w := &partialMigrationWriter{kind: targetType, failStage: failure, failure: errors.New("table SQL is invalid")}
	if failure == "metadata" {
		r.metadataError = errors.New("table metadata is not visible")
	}
	m, _ := newTestMigrator(r, w)
	m.cfg.LowerCaseNames, m.cfg.TargetDBType, m.cfg.Content, m.cfg.MaxParallel = true, targetType, "both", 1
	return r, w, m
}

func TestGenericMigrationContinuesAcrossSourceAndPGTargetFamilies(t *testing.T) {
	for _, src := range []string{"mysql", "oracle", "sqlserver", "dameng", "postgres"} {
		for _, dst := range []string{"postgres", "gaussdb", "vastbase", "highgo", "kingbase", "seabox"} {
			for _, stage := range []string{"metadata", "generate", "drop", "create"} {
				t.Run(src+"/"+dst+"/"+stage, func(t *testing.T) {
					_, w, m := partialMigrationFixture(src, dst, stage, stage == "drop")
					report := m.Run(context.Background())
					require.Equal(t, 3, report.Tables.Total)
					require.Equal(t, 2, report.Tables.Success)
					require.Equal(t, 1, report.Tables.Failed)
					require.Equal(t, 2, report.Data.Success)
					require.ElementsMatch(t, []string{"a", "z"}, w.copied)
					require.Equal(t, 2, report.PrimaryKeys.Success)
					require.Equal(t, 1, report.PrimaryKeys.Failed)
					require.Equal(t, 2, report.Indexes.Success)
					require.Equal(t, 1, report.Indexes.Failed)
					require.Len(t, w.sequences, 2)
					require.Len(t, w.comments, 2)
					require.Len(t, w.fks, 1)
					require.Equal(t, "healthy_fk", w.fks[0].ConstraintName)
					require.Len(t, w.views, 1)
					require.Equal(t, "healthyview", w.views[0].ViewName)
					for _, category := range []CategoryReport{report.PrimaryKeys, report.Indexes, report.Sequences, report.Comments, report.Constraints, report.Views} {
						require.Equal(t, category.Total, category.Success+category.Failed, fmt.Sprintf("%+v", category))
						for _, item := range category.Items {
							require.Equal(t, "dependency", item.Stage)
						}
					}
					for _, count := range report.RowCounts {
						require.NotEqual(t, "broken", count.Table)
					}
				})
			}
		}
	}
}

func TestGenericMigrationTaskFailureStopsLaterTables(t *testing.T) {
	for _, cause := range []error{driver.ErrBadConn, migrationTaskFailure{errors.New("checkpoint failed")}} {
		t.Run(cause.Error(), func(t *testing.T) {
			_, w, m := partialMigrationFixture("mysql", "postgres", "create", true)
			w.failure = cause
			report := m.Run(context.Background())
			require.Empty(t, w.created)
			require.Empty(t, w.copied)
			require.Empty(t, w.indexes)
			require.True(t, report.HasFailures())
			for _, sql := range w.attempts {
				require.NotContains(t, sql, `"z"`)
				require.NotContains(t, sql, `"a"`)
			}
		})
	}
}

func TestGenericMigrationFailureLedgerRecordsBlockedDataAndDependencies(t *testing.T) {
	_, _, m := partialMigrationFixture("mysql", "postgres", "create", false)
	m.cfg.CaptureRepair = true
	report := m.Run(context.Background())
	require.NotNil(t, report.RepairManifest)
	var table *repair.Object
	for i := range report.RepairManifest.Objects {
		o := &report.RepairManifest.Objects[i]
		if o.Kind == "table" && o.SourceName == "Broken" {
			table = o
		}
		if o.Table == "broken" && o.Kind != "table" && o.Status != "failed" && o.Status != "unknown" {
			require.Equal(t, "blocked", o.Status)
		}
	}
	require.NotNil(t, table)
	require.Equal(t, "blocked", table.DataStatus)
	for _, o := range report.RepairManifest.Objects {
		if o.Kind == "table" && o.SourceName == "Z" {
			require.Equal(t, "succeeded", o.Status)
			require.Equal(t, "succeeded", o.DataStatus)
		}
	}
}

func TestGenericMigrationAllTablesFailedStillProcessesIndependentObjects(t *testing.T) {
	r, w, m := partialMigrationFixture("mysql", "postgres", "create", true)
	r.tables = []string{"Broken"}
	r.views = []source.ViewInfo{
		{ViewName: "Independent", Definition: `SELECT 1 AS value`},
		{ViewName: "Dependent", Definition: `SELECT "Id" FROM "Broken"`},
	}
	report := m.Run(context.Background())
	require.Equal(t, 1, report.Tables.Failed)
	require.Zero(t, report.Tables.Success)
	require.Empty(t, w.copied)
	require.Empty(t, w.indexes)
	require.Equal(t, 1, report.Indexes.Failed)
	require.Equal(t, 1, report.Views.Success)
	require.Equal(t, 1, report.Views.Failed)
	require.Len(t, w.views, 1)
	require.Equal(t, "independent", w.views[0].ViewName)
	require.Equal(t, "dependency", report.Views.Items[0].Stage)
}

func TestGenericPostDDLRespectsBlockedPartitionDependencies(t *testing.T) {
	r, w, m := partialMigrationFixture("mysql", "postgres", "", false)
	bad := &PartitionTableResult{SourceTable: "Broken", TargetTable: "broken", Error: "unsupported partition", Status: "blocked"}
	m.partitionState = &partitionRunState{tables: map[string]*PartitionTableResult{"Broken": bad}, blocked: map[string]bool{"Broken": true}, preflight: &PartitionPreflight{Tables: []*PartitionTableResult{bad}}}
	report := newMigrationReport()
	m.createPostDDL(context.Background(), &report, []string{"A", "Z"}, r.tables)
	require.Len(t, w.views, 1)
	require.Equal(t, "healthyview", w.views[0].ViewName)
	require.Equal(t, 1, report.PrimaryKeys.Failed)
	require.Equal(t, 1, report.Indexes.Failed)
	require.Equal(t, 2, report.Views.Failed)
	require.Equal(t, 1, report.Constraints.Failed)
	require.False(t, m.migrationTableBlocked("Broken"), "partition accounting stays with its preflight")
}
