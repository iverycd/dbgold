package datamigrate

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

type oscarFixtureReader struct {
	mockReader
	srcType          string
	indexes, primary []source.IndexInfo
	sequences        []source.SequenceInfo
	foreign          []source.FKInfo
	comments         []source.CommentInfo
	views            []source.ViewInfo
}

func (r *oscarFixtureReader) DBType() string { return r.srcType }
func (r *oscarFixtureReader) GetIndexes(context.Context) ([]source.IndexInfo, error) {
	return r.indexes, nil
}
func (r *oscarFixtureReader) GetPrimaryKeys(context.Context) ([]source.IndexInfo, error) {
	return r.primary, nil
}
func (r *oscarFixtureReader) GetSequences(context.Context) ([]source.SequenceInfo, error) {
	return r.sequences, nil
}
func (r *oscarFixtureReader) GetForeignKeys(context.Context) ([]source.FKInfo, error) {
	return r.foreign, nil
}
func (r *oscarFixtureReader) GetComments(context.Context) ([]source.CommentInfo, error) {
	return r.comments, nil
}
func (r *oscarFixtureReader) GetViews(context.Context) ([]source.ViewInfo, error) {
	return r.views, nil
}

type recordingOscarWriter struct {
	mockWriter
	mu                                  sync.Mutex
	sql, copiedColumns, counted, owners []string
	failIndex                           bool
}

func (w *recordingOscarWriter) DBType() string           { return "oscar" }
func (w *recordingOscarWriter) Dialect() dialect.Dialect { return dialect.NewOscar() }
func (w *recordingOscarWriter) record(stmts []dialect.Statement) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sql = append(w.sql, dialect.JoinSQL(stmts))
	return nil
}
func (w *recordingOscarWriter) CreateTable(_ context.Context, stmts []dialect.Statement) error {
	return w.record(stmts)
}
func (w *recordingOscarWriter) CopyData(_ context.Context, table string, columns, _ []string, _ [][]interface{}) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.copied = append(w.copied, table)
	w.copiedColumns = append(w.copiedColumns, columns...)
	return nil
}
func (w *recordingOscarWriter) CountRows(_ context.Context, table string) (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.counted = append(w.counted, table)
	return 0, nil
}
func (w *recordingOscarWriter) CreateIndex(_ context.Context, idx source.IndexInfo) error {
	_ = w.record(w.Dialect().IndexStatements("MixedSchema", idx))
	if w.failIndex {
		return fmt.Errorf("oscar JDBC SQLSTATE 42S11: index already exists")
	}
	return nil
}
func (w *recordingOscarWriter) CreateSequence(_ context.Context, seq source.SequenceInfo, owner string) error {
	return w.record(w.Dialect().SequenceStatements("MixedSchema", seq, owner))
}
func (w *recordingOscarWriter) CreateForeignKey(_ context.Context, fk source.FKInfo) error {
	return w.record(w.Dialect().ForeignKeyStatements("MixedSchema", fk))
}
func (w *recordingOscarWriter) CreateComment(_ context.Context, cm source.CommentInfo) error {
	return w.record(w.Dialect().CommentStatements("MixedSchema", cm))
}
func (w *recordingOscarWriter) CreateView(_ context.Context, v source.ViewInfo) error {
	return w.record(w.Dialect().ViewStatements("MixedSchema", v))
}
func (w *recordingOscarWriter) ChangeOwner(_ context.Context, kind, name, owner string) error {
	w.owners = append(w.owners, kind+" "+name+" -> "+owner)
	return nil
}

func newOscarFixture() *oscarFixtureReader {
	r := &oscarFixtureReader{srcType: "mysql"}
	r.tables = []string{"Orders", "Items"}
	r.ddl = make(map[string]*source.TableDDLInfo)
	r.rows = make(map[string][][]interface{})
	for _, table := range r.tables {
		r.ddl[table] = &source.TableDDLInfo{TableName: table, Columns: []source.ColumnInfo{{Name: "id", DataType: "int", IsNullable: true}}}
		r.rows[table] = [][]interface{}{{int64(1)}}
		r.indexes = append(r.indexes, source.IndexInfo{TableName: table, IndexName: "idx_Id", Columns: []string{"id"}, IsUnique: table == "Items"})
		r.primary = append(r.primary, source.IndexInfo{TableName: table, IndexName: "PRIMARY", Columns: []string{"id"}, IsPrimary: true})
	}
	r.sequences = []source.SequenceInfo{{TableName: "Orders", ColumnName: "id", StartValue: 2}}
	r.foreign = []source.FKInfo{{TableName: "Items", ConstraintName: "fk_Order", Columns: []string{"id"}, RefTable: "Orders", RefColumns: []string{"id"}}}
	r.comments = []source.CommentInfo{{TableName: "Orders", ColumnName: "id", Comment: "Keep中文MiXeD"}}
	r.views = []source.ViewInfo{{ViewName: "vOrders", Definition: `SELECT id, 'Keep中文MiXeD' AS label FROM Orders`}}
	return r
}

func TestOscarMigratorUppercaseAcrossAllPhases(t *testing.T) {
	for _, sourceType := range []string{"mysql", "sqlserver", "oracle", "dameng"} {
		for _, owner := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/owner=%v", sourceType, owner), func(t *testing.T) {
				r, w := newOscarFixture(), &recordingOscarWriter{}
				r.srcType = sourceType
				m, _ := newTestMigrator(r, w)
				m.cfg.TargetDBType, m.cfg.TargetSchema = "oscar", "MixedSchema"
				m.cfg.ChangeOwner, m.cfg.LowerCaseNames = owner, true
				report := m.Run(context.Background())
				require.Zero(t, report.Tables.Failed)
				require.Equal(t, 2, report.Tables.Success)
				require.Equal(t, 2, report.Data.Success)
				require.Equal(t, 2, report.Indexes.Success)
				require.Equal(t, 2, report.PrimaryKeys.Success)
				require.Equal(t, 1, report.Views.Success)
				require.Equal(t, 1, report.Sequences.Success)
				require.Equal(t, 1, report.Constraints.Success)
				require.Equal(t, 1, report.Comments.Success)
				require.ElementsMatch(t, []string{"ORDERS", "ITEMS"}, w.copied)
				require.ElementsMatch(t, []string{"ORDERS", "ITEMS"}, w.counted)
				require.ElementsMatch(t, []string{"ID", "ID"}, w.copiedColumns)
				ddl := strings.Join(w.sql, "\n")
				for _, part := range []string{`"MixedSchema"."ORDERS"`, `"ITEMS_IDX_ID"`, `"ORDERS_IDX_ID"`, `"PK_ORDERS"`, `"SEQ_ORDERS_ID"`, `"FK_ORDER"`, `"VORDERS"`, `'Keep中文MiXeD'`} {
					require.Contains(t, ddl, part)
				}
				require.NotContains(t, ddl, `"MixedSchema"."Orders"`)
				require.NotContains(t, ddl, `"ORDERS_ORDERS_IDX_ID"`)
				if owner {
					require.Contains(t, w.owners, "SEQUENCE SEQ_ORDERS_ID -> MixedSchema")
					require.Contains(t, w.owners, "VIEW VORDERS -> MixedSchema")
				} else {
					require.Empty(t, w.owners)
				}
				// Metadata is not mutated; all source lookups still use original case.
				require.Equal(t, "Orders", r.indexes[0].TableName)
				require.Equal(t, "id", r.indexes[0].Columns[0])
			})
		}
	}
}

func TestOscarNamingConflictsAbortBeforeTargetWrites(t *testing.T) {
	for _, conflict := range []string{"table", "column", "index", "sequence", "view", "foreign key"} {
		t.Run(conflict, func(t *testing.T) {
			r := newOscarFixture()
			switch conflict {
			case "table":
				r.tables = append(r.tables, "ORDERS")
			case "column":
				r.ddl["Orders"].Columns = append(r.ddl["Orders"].Columns, source.ColumnInfo{Name: "ID", DataType: "int"})
			case "index":
				r.indexes = append(r.indexes, source.IndexInfo{TableName: "Orders", IndexName: "IDX_ID", Columns: []string{"id"}})
			case "sequence":
				r.sequences = append(r.sequences, r.sequences[0])
			case "view":
				r.views = append(r.views, source.ViewInfo{ViewName: "VORDERS"})
			case "foreign key":
				r.foreign = append(r.foreign, source.FKInfo{TableName: "Items", ConstraintName: "FK_ORDER"})
			}
			w := &recordingOscarWriter{}
			m, _ := newTestMigrator(r, w)
			m.cfg.TargetDBType = "oscar"
			report := m.Run(context.Background())
			var failures []ObjectResult
			for _, category := range []CategoryReport{report.Tables, report.Indexes, report.Sequences, report.Views, report.Constraints} {
				failures = append(failures, category.Items...)
			}
			require.NotEmpty(t, failures)
			require.Contains(t, failures[0].Error, "对象名冲突")
			require.Empty(t, w.sql)
			require.Empty(t, w.copied)
		})
	}
}

func TestOscarViewConflictWithoutTablesIsReported(t *testing.T) {
	r, w := newOscarFixture(), &recordingOscarWriter{}
	r.tables = nil
	r.views = []source.ViewInfo{{ViewName: "Foo"}, {ViewName: "FOO"}}
	m, _ := newTestMigrator(r, w)
	m.cfg.TargetDBType = "oscar"
	report := m.Run(context.Background())
	require.Equal(t, 1, report.Views.Failed)
	require.Equal(t, "FOO", report.Views.Items[0].Name)
	require.Empty(t, w.sql)
}

type failingOscarMetadataReader struct{ *oscarFixtureReader }

func (r failingOscarMetadataReader) GetViews(context.Context) ([]source.ViewInfo, error) {
	return nil, fmt.Errorf("source view metadata unavailable")
}

func TestOscarPreflightFailsClosedAndDetectsCrossObjectCollisions(t *testing.T) {
	t.Run("metadata unavailable", func(t *testing.T) {
		w := &recordingOscarWriter{}
		m, _ := newTestMigrator(failingOscarMetadataReader{newOscarFixture()}, w)
		m.cfg.TargetDBType = "oscar"
		report := m.Run(context.Background())
		require.Equal(t, 1, report.Tables.Failed)
		require.Contains(t, report.Tables.Items[0].Error, "source view metadata unavailable")
		require.Empty(t, w.sql)
	})
	t.Run("sequence would replace table", func(t *testing.T) {
		r, w := newOscarFixture(), &recordingOscarWriter{}
		r.tables = append(r.tables, "SEQ_ORDERS_ID")
		r.ddl["SEQ_ORDERS_ID"] = &source.TableDDLInfo{TableName: "SEQ_ORDERS_ID"}
		m, _ := newTestMigrator(r, w)
		m.cfg.TargetDBType = "oscar"
		report := m.Run(context.Background())
		require.Equal(t, 1, report.Sequences.Failed)
		require.Contains(t, report.Sequences.Items[0].Error, "table SEQ_ORDERS_ID")
		require.Empty(t, w.sql)
	})
}

func TestOscarDataOnlyUsesUppercaseWithoutDDL(t *testing.T) {
	r, w := newOscarFixture(), &recordingOscarWriter{}
	m, _ := newTestMigrator(r, w)
	m.cfg.TargetDBType, m.cfg.Content = "oscar", "data_only"
	report := m.Run(context.Background())
	require.Equal(t, 2, report.Data.Success)
	require.ElementsMatch(t, []string{"ORDERS", "ITEMS"}, w.copied)
	require.Empty(t, w.sql)
}

func TestOscarObjectOnlySelectionAndIndexReport(t *testing.T) {
	r, w := newOscarFixture(), &recordingOscarWriter{failIndex: true}
	m, _ := newTestMigrator(r, w)
	m.cfg.TargetDBType, m.cfg.TargetSchema = "oscar", "MixedSchema"
	m.cfg.Objects, m.cfg.TableNames = []string{"indexes"}, []string{"Orders"}
	report := m.Run(context.Background())
	require.Equal(t, 1, report.Indexes.Failed)
	require.Equal(t, "ORDERS_IDX_ID", report.Indexes.Items[0].Name)
	require.Equal(t, w.sql[0], report.Indexes.Items[0].DDL)
	require.Contains(t, report.Indexes.Items[0].Error, "42S11")
	require.Empty(t, w.copied)
}

func TestOscarViewOnlyConflictsAndOriginalSelection(t *testing.T) {
	r, w := newOscarFixture(), &recordingOscarWriter{}
	m, _ := newTestMigrator(r, w)
	m.cfg.TargetDBType = "oscar"
	m.cfg.TargetSchema = "MixedSchema"
	result := m.MigrateViews(context.Background(), []string{"vOrders"})
	require.Len(t, result, 1)
	require.Empty(t, result[0].Error)
	require.Equal(t, "VORDERS", result[0].Name)
	require.Equal(t, w.sql[0], result[0].DDL)
	w.sql = nil
	r.views = append(r.views, source.ViewInfo{ViewName: "VORDERS"})
	result = m.MigrateViews(context.Background(), []string{"vOrders", "VORDERS"})
	require.Len(t, result, 2)
	require.NotEmpty(t, result[0].Error)
	require.Empty(t, w.sql)
}
