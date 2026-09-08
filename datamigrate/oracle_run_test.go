package datamigrate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/datamigrate/valueconv"
	"github.com/stretchr/testify/require"
)

type oracleFixtureReader struct{ *oscarFixtureReader }

func (r *oracleFixtureReader) SourceSchema() string { return "Src" }
func (r *oracleFixtureReader) GetViewsForTarget(_ context.Context, kind string) ([]source.ViewInfo, error) {
	if kind != "oracle" {
		return nil, errors.New("wrong target")
	}
	return r.views, nil
}

type oracleRecordingWriter struct {
	recordingOscarWriter
	dia            *dialect.OracleDialect
	inspected      bool
	preflightError error
	failTable      string
}

func (w *oracleRecordingWriter) DBType() string           { return "oracle" }
func (w *oracleRecordingWriter) Dialect() dialect.Dialect { return w.dia }
func (w *oracleRecordingWriter) EffectiveSchema() string  { return "APP" }
func (w *oracleRecordingWriter) CreateTable(_ context.Context, stmts []dialect.Statement) error {
	if w.failTable != "" && strings.Contains(dialect.JoinSQL(stmts), w.failTable) {
		return errors.New("CREATE TABLE failed")
	}
	return w.record(stmts)
}
func (w *oracleRecordingWriter) PreflightOracle(context.Context, map[string]*source.TableDDLInfo, []target.OracleObject, []source.SequenceInfo, bool, bool) error {
	w.inspected = true
	return w.preflightError
}
func (w *oracleRecordingWriter) CreateIndex(_ context.Context, i source.IndexInfo) error {
	if i.Unsupported != "" {
		return errors.New(i.Unsupported)
	}
	return w.record(w.dia.IndexStatements("APP", i))
}
func (w *oracleRecordingWriter) CreateSequence(_ context.Context, s source.SequenceInfo, _ string) error {
	return w.record(w.dia.SequenceStatements("APP", s, ""))
}
func (w *oracleRecordingWriter) CreateForeignKey(_ context.Context, f source.FKInfo) error {
	if e := dialect.ValidateOracleForeignKey(f); e != nil {
		return e
	}
	return w.record(w.dia.ForeignKeyStatements("APP", f))
}
func (w *oracleRecordingWriter) CreateView(_ context.Context, v source.ViewInfo) error {
	return w.record(w.dia.ViewStatements("APP", v))
}
func (w *oracleRecordingWriter) CreateComment(_ context.Context, c source.CommentInfo) error {
	return w.record(w.dia.CommentStatements("APP", c))
}
func (w *oracleRecordingWriter) MigrationWarnings() []valueconv.MigrationWarning {
	return []valueconv.MigrationWarning{{Table: "ORDERS", Column: "ID", Reason: "fixture", Count: 1}}
}
func oracleMigrationFixture() (*oracleFixtureReader, *oracleRecordingWriter, *Migrator) {
	r := &oracleFixtureReader{newOscarFixture()}
	w := &oracleRecordingWriter{dia: dialect.NewOracle()}
	job := &Job{LogCh: make(chan string, 512)}
	m := NewMigrator(r, w, job, Config{TargetDBType: "oracle", PageSize: 10, MaxParallel: 2, Mode: "all", LowerCaseNames: true, ChangeOwner: true, Distributed: true})
	return r, w, m
}
func TestOracleMigrationModes(t *testing.T) {
	for _, mode := range []string{"both", "schema_only", "data_only", "objects"} {
		t.Run(mode, func(t *testing.T) {
			r, w, m := oracleMigrationFixture()
			m.cfg.Content = mode
			if mode == "objects" {
				m.cfg.Content = ""
				m.cfg.Objects = []string{"sequences", "indexes", "primary_keys", "foreign_keys", "comments"}
			}
			report := m.Run(context.Background())
			require.False(t, report.HasFailures(), report)
			require.True(t, w.inspected)
			require.False(t, m.cfg.LowerCaseNames)
			require.False(t, m.cfg.ChangeOwner)
			require.False(t, m.cfg.Distributed)
			require.Equal(t, "APP", m.cfg.TargetSchema)
			if mode == "both" || mode == "data_only" {
				require.ElementsMatch(t, []string{"ORDERS", "ITEMS"}, w.copied)
				require.Equal(t, 2, report.Data.Success)
			} else {
				require.Empty(t, w.copied)
			}
			if mode == "both" || mode == "schema_only" {
				require.Equal(t, 2, report.Tables.Success)
				require.Equal(t, 1, report.Views.Success)
			} else {
				require.Zero(t, report.Tables.Total)
				require.Zero(t, report.Views.Total)
			}
			if mode != "data_only" {
				require.Equal(t, 2, report.PrimaryKeys.Success)
				require.Equal(t, 2, report.Indexes.Success)
				require.Equal(t, 1, report.Sequences.Success)
				require.Equal(t, 1, report.Constraints.Success)
			} else {
				require.Empty(t, w.sql)
			}
			require.Empty(t, w.owners)
			require.Equal(t, 1, len(report.Warnings))
			require.Equal(t, "Orders", r.tables[0])
			require.Equal(t, "id", r.ddl["Orders"].Columns[0].Name)
		})
	}
}
func TestOraclePreflightNeverMutatesOnErrors(t *testing.T) {
	for _, kind := range []string{"case", "long", "precision", "default", "target", "special_index", "foreign_action", "lob_index"} {
		t.Run(kind, func(t *testing.T) {
			r, w, m := oracleMigrationFixture()
			switch kind {
			case "case":
				r.tables = append(r.tables, "ORDERS")
			case "long":
				r.tables = append(r.tables, strings.Repeat("x", 31))
			case "precision":
				r.ddl["Orders"].Columns[0].DataType = "decimal"
				r.ddl["Orders"].Columns[0].Precision = 65
			case "default":
				r.ddl["Orders"].Columns[0].Extra = "on update current_timestamp"
			case "target":
				w.preflightError = errors.New("sequence belongs to another table")
			case "special_index":
				r.indexes[0].Unsupported = "不支持前缀索引"
			case "foreign_action":
				r.foreign[0].OnUpdate = "CASCADE"
			case "lob_index":
				r.ddl["Orders"].Columns[0].DataType = "varchar"
				r.ddl["Orders"].Columns[0].Length = 2000
			}
			report := m.Run(context.Background())
			require.True(t, report.HasFailures())
			require.Empty(t, w.sql)
			require.Empty(t, w.copied)
		})
	}
}
func TestOracleObjectSelectionAndUnsupportedFK(t *testing.T) {
	r, w, m := oracleMigrationFixture()
	m.cfg.Objects = []string{"foreign_keys"}
	m.cfg.TableNames = []string{"Orders"}
	report := m.Run(context.Background())
	require.Zero(t, report.Constraints.Total)
	require.Empty(t, w.sql)
	r, w, m = oracleMigrationFixture()
	m.cfg.Objects = []string{"foreign_keys"}
	m.cfg.TableNames = []string{"Items"}
	r.foreign[0].OnUpdate = "CASCADE"
	report = m.Run(context.Background())
	require.Equal(t, 1, report.Constraints.Failed)
	require.ErrorContains(t, errors.New(report.Constraints.Items[0].Error), "ON UPDATE CASCADE")
	require.Empty(t, w.sql)
}
func TestOracleViewsDependencyOrderAndFailure(t *testing.T) {
	r, w, m := oracleMigrationFixture()
	r.views = []source.ViewInfo{{ViewName: "aChild", Definition: "SELECT id FROM zParent"}, {ViewName: "zParent", Definition: "SELECT id FROM Orders"}}
	results := m.MigrateViews(context.Background(), []string{"aChild", "zParent"})
	require.Len(t, results, 2)
	require.Equal(t, "ZPARENT", results[0].Name)
	require.Empty(t, results[0].Error)
	require.Equal(t, "ACHILD", results[1].Name)
	require.Contains(t, w.sql[1], `"APP"."ACHILD"`)
	r.views = []source.ViewInfo{{ViewName: "aChild", Definition: "SELECT id FROM zParent"}, {ViewName: "zParent", Definition: "SELECT GROUP_CONCAT(id) FROM Orders"}}
	w.sql = nil
	results = m.MigrateViews(context.Background(), []string{"aChild", "zParent"})
	require.Len(t, results, 2)
	require.Contains(t, results[0].Error, "GROUP_CONCAT")
	require.Contains(t, results[1].Error, "依赖视图")
	require.Empty(t, w.sql)
	r.views = []source.ViewInfo{{ViewName: "a", Definition: "SELECT id FROM b"}, {ViewName: "b", Definition: "SELECT id FROM a"}}
	results = m.MigrateViews(context.Background(), []string{"a", "b"})
	require.Len(t, results, 2)
	require.Contains(t, results[0].Error, "循环")
	require.Empty(t, w.sql)
}

func TestOracleFailedDDLAndDependenciesAreReported(t *testing.T) {
	r, w, m := oracleMigrationFixture()
	w.failTable = `"ORDERS"`
	r.ddl["Orders"].Columns = append(r.ddl["Orders"].Columns, source.ColumnInfo{Name: "Choice", DataType: "enum", Length: 10, IsNullable: true})
	report := m.Run(context.Background())
	require.Equal(t, 1, report.Tables.Failed)
	expected, e := w.dia.CreateTableStatements("APP", r.ddl["Orders"], "mysql", dialect.TypeOpt{}, strings.ToUpper)
	require.NoError(t, e)
	require.Equal(t, dialect.JoinSQL(expected), report.Tables.Items[0].DDL)
	require.Contains(t, report.Tables.Items[0].Error, "CREATE TABLE failed")
	require.Equal(t, 1, report.Views.Failed)
	require.Contains(t, report.Views.Items[0].Error, "建表失败")
	require.Equal(t, 1, report.Constraints.Failed)
	for _, warning := range report.Warnings {
		require.NotEqual(t, "Choice", warning.Column)
		require.NotEqual(t, "CHOICE", warning.Column)
	}
}

type oraclePagingReader struct{ *oracleFixtureReader }

func (r *oraclePagingReader) ReadPage(_ context.Context, _ string, _ []string, offset, _ int64) ([]string, []string, [][]interface{}, error) {
	if offset >= 2 {
		return []string{"Id"}, []string{"INT"}, nil, nil
	}
	return []string{"Id"}, []string{"INT"}, [][]interface{}{{offset + 1}}, nil
}
func (r *oraclePagingReader) CountRows(context.Context, string) (int64, error) { return 2, nil }

type oraclePagingWriter struct{ *oracleRecordingWriter }

func (w *oraclePagingWriter) CopyData(_ context.Context, _ string, _, _ []string, rows [][]interface{}) error {
	if rows[0][0] == int64(2) {
		return errors.New("列 ID 页内第 1 行: 无效值")
	}
	return nil
}
func TestOracleConversionErrorsIncludePageAndColumn(t *testing.T) {
	for _, parallel := range []int{1, 2} {
		r, w, m := oracleMigrationFixture()
		m.reader, m.writer = &oraclePagingReader{r}, &oraclePagingWriter{w}
		m.cfg.PageSize, m.cfg.IntraTableParallel = 1, parallel
		ok, err := m.migrateTableData(context.Background(), "Orders")
		require.False(t, ok)
		require.Contains(t, err, "表 Orders 第 2 页")
		require.Contains(t, err, "列 ID 页内第 1 行")
	}
}
