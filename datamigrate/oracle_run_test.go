package datamigrate

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"slices"
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
	preflight      func(map[string]*source.TableDDLInfo, []target.OracleObject) error
	ddlFailure     string
	ddlCause       error
	ddlAttempts    []string
	indexCause     error
	viewCause      error
}

func (w *oracleRecordingWriter) DBType() string           { return "oracle" }
func (w *oracleRecordingWriter) Dialect() dialect.Dialect { return w.dia }
func (w *oracleRecordingWriter) EffectiveSchema() string  { return "APP" }
func (w *oracleRecordingWriter) CreateTable(_ context.Context, stmts []dialect.Statement) error {
	if w.failTable != "" && strings.Contains(dialect.JoinSQL(stmts), w.failTable) {
		return errors.New("CREATE TABLE failed")
	}
	for _, statement := range stmts {
		w.ddlAttempts = append(w.ddlAttempts, statement.SQL)
		if w.ddlFailure != "" && strings.Contains(statement.SQL, w.ddlFailure) {
			if w.ddlCause != nil {
				return w.ddlCause
			}
			return errors.New("table statement rejected")
		}
	}
	return w.record(stmts)
}
func (w *oracleRecordingWriter) PreflightOracle(_ context.Context, tables map[string]*source.TableDDLInfo, objects []target.OracleObject, _ []source.SequenceInfo, _, _ bool) error {
	w.inspected = true
	if w.preflight != nil {
		return w.preflight(tables, objects)
	}
	return w.preflightError
}
func (w *oracleRecordingWriter) CreateIndex(_ context.Context, i source.IndexInfo) error {
	if w.indexCause != nil {
		return w.indexCause
	}
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
	if w.viewCause != nil {
		return w.viewCause
	}
	return w.record(w.dia.ViewStatements("APP", v))
}

type oracleRetainedDependencyWriter struct {
	*oracleRecordingWriter
	snapshot target.MigrationDependencies
	err      error
}

func (w *oracleRetainedDependencyWriter) InspectMigrationDependencies(context.Context) (target.MigrationDependencies, error) {
	return w.snapshot, w.err
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
func TestOraclePreflightIsolatesLocalFailuresBeforeTargetWrites(t *testing.T) {
	for _, kind := range []string{"case", "long", "precision", "default", "special_index", "foreign_action", "lob_index", "missing_info"} {
		t.Run(kind, func(t *testing.T) {
			r, w, m := oracleMigrationFixture()
			blocked := []string{"Orders"}
			switch kind {
			case "case":
				r.tables = append(r.tables, "ORDERS")
				blocked = []string{"Orders", "ORDERS"}
			case "long":
				r.tables = append(r.tables, strings.Repeat("x", 31))
				blocked = []string{strings.Repeat("x", 31)}
			case "precision":
				r.ddl["Orders"].Columns[0].DataType = "decimal"
				r.ddl["Orders"].Columns[0].Precision = 65
			case "default":
				r.ddl["Orders"].Columns[0].Extra = "on update current_timestamp"
			case "special_index":
				r.indexes[0].Unsupported = "不支持前缀索引"
			case "foreign_action":
				r.foreign[0].OnUpdate = "CASCADE"
				blocked = []string{"Items"}
			case "lob_index":
				r.ddl["Orders"].Columns[0].DataType = "varchar"
				r.ddl["Orders"].Columns[0].Length = 2000
			case "missing_info":
				r.ddl["Orders"] = nil
			}
			report := m.Run(context.Background())
			require.True(t, report.HasFailures())
			require.Equal(t, len(r.tables), report.Tables.Total)
			require.Equal(t, len(blocked), report.Tables.Failed)
			healthy := []string{}
			for _, table := range r.tables {
				if slices.Contains(blocked, table) {
					require.True(t, m.migrationTableBlocked(table))
					require.NotContains(t, strings.Join(w.ddlAttempts, "\n"), `"APP"."`+strings.ToUpper(table)+`"`)
				} else {
					healthy = append(healthy, strings.ToUpper(table))
				}
			}
			require.ElementsMatch(t, healthy, w.copied)
			require.Equal(t, len(healthy), report.Tables.Success)
			for _, item := range report.Tables.Items {
				require.NotEmpty(t, item.SourceName)
				require.Equal(t, item.SourceName, item.Table)
			}
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

func TestOracleOpaquePreflightFailureStillStopsBeforeWrites(t *testing.T) {
	_, w, m := oracleMigrationFixture()
	w.preflightError = errors.New("target dependency inventory unavailable")
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.Empty(t, w.sql)
	require.Empty(t, w.copied)
}

func TestOracleTargetPreflightRechecksRetainedDependencyClosure(t *testing.T) {
	r, w, m := oracleMigrationFixture()
	r.tables = append(r.tables, "Independent")
	r.ddl["Independent"] = &source.TableDDLInfo{TableName: "Independent", Columns: []source.ColumnInfo{{Name: "id", DataType: "int"}}}
	r.rows["Independent"] = [][]interface{}{{int64(1)}}
	var batches [][]string
	w.preflight = func(tables map[string]*source.TableDDLInfo, _ []target.OracleObject) error {
		batch := []string{}
		for table := range tables {
			batch = append(batch, table)
		}
		batches = append(batches, batch)
		if tables["ORDERS"] != nil {
			return &target.OracleTablePreflightError{Table: "ORDERS", Err: errors.New("retained source-independent target object")}
		}
		if tables["ITEMS"] != nil {
			return &target.OracleTablePreflightError{Table: "ITEMS", Err: errors.New("retained Orders target table references Items")}
		}
		return nil
	}
	report := m.Run(context.Background())
	require.Equal(t, 2, report.Tables.Failed)
	require.Equal(t, 1, report.Tables.Success)
	require.Len(t, batches, 3)
	require.ElementsMatch(t, []string{"ORDERS", "ITEMS", "INDEPENDENT"}, batches[0])
	require.ElementsMatch(t, []string{"ITEMS", "INDEPENDENT"}, batches[1])
	require.Equal(t, []string{"INDEPENDENT"}, batches[2])
	require.Equal(t, []string{"INDEPENDENT"}, w.copied)
}

func TestOracleFirstAndMiddleDDLFailuresPreserveHealthyDataAndObjects(t *testing.T) {
	for _, capture := range []bool{false, true} {
		for _, table := range []string{"A", "B"} {
			for _, statement := range []string{"DROP", "CREATE"} {
				t.Run(fmt.Sprintf("%s/%s/capture=%v", table, statement, capture), func(t *testing.T) {
					r, w, m := oracleMigrationFixture()
					r.tables = []string{"A", "B", "C"}
					r.primary, r.indexes, r.sequences, r.comments, r.foreign = nil, nil, nil, nil, nil
					for _, name := range r.tables {
						r.ddl[name] = &source.TableDDLInfo{TableName: name, Columns: []source.ColumnInfo{{Name: "id", DataType: "int"}}}
						r.rows[name] = [][]interface{}{{int64(1)}}
						r.primary = append(r.primary, source.IndexInfo{TableName: name, IndexName: "PRIMARY", Columns: []string{"id"}, IsPrimary: true})
						r.indexes = append(r.indexes, source.IndexInfo{TableName: name, IndexName: "ix", Columns: []string{"id"}})
						r.comments = append(r.comments, source.CommentInfo{TableName: name, ColumnName: "id", Comment: "source comment"})
					}
					r.foreign = []source.FKInfo{{TableName: "C", RefTable: table, ConstraintName: "fk", Columns: []string{"id"}, RefColumns: []string{"id"}}}
					r.views = []source.ViewInfo{{ViewName: "Dependent", Definition: "SELECT id FROM " + table}, {ViewName: "HealthyView", Definition: "SELECT id FROM C"}}
					w.ddlFailure = statement + ` TABLE "APP"."` + table + `"`
					m.cfg.CaptureRepair = capture
					report := m.Run(context.Background())
					require.Equal(t, 3, report.Tables.Total)
					require.Equal(t, 2, report.Tables.Success)
					require.Equal(t, 1, report.Tables.Failed)
					healthy := slices.DeleteFunc(slices.Clone(r.tables), func(name string) bool { return name == table })
					require.ElementsMatch(t, healthy, w.copied)
					require.Equal(t, 2, report.Data.Success)
					require.Len(t, report.RowCounts, 2)
					require.Equal(t, 2, report.PrimaryKeys.Success)
					require.Equal(t, 1, report.PrimaryKeys.Failed)
					require.Equal(t, 2, report.Indexes.Success)
					require.Equal(t, 1, report.Indexes.Failed)
					require.Equal(t, 2, report.Comments.Success)
					require.Equal(t, 1, report.Comments.Failed)
					require.Equal(t, 1, report.Constraints.Failed)
					require.Equal(t, "dependency", report.Constraints.Items[0].Stage)
					require.Equal(t, 1, report.Views.Success)
					require.Equal(t, 1, report.Views.Failed)
					require.Equal(t, "dependency", report.Views.Items[0].Stage)
					require.Equal(t, table, report.Tables.Items[0].SourceName)
					if capture {
						require.NotNil(t, report.RepairManifest)
						for _, object := range report.RepairManifest.Objects {
							if object.Kind == "table" && object.SourceTable == table {
								require.NotEqual(t, "succeeded", object.Status)
								require.NotEqual(t, "succeeded", object.DataStatus)
							}
						}
					}
				})
			}
		}
	}
}

func TestOracleViewLocalValidationAndTargetConflictsDoNotStopTables(t *testing.T) {
	for _, kind := range []string{"columns", "view_collision", "table_collision", "sequence_collision", "target"} {
		t.Run(kind, func(t *testing.T) {
			r, w, m := oracleMigrationFixture()
			r.views = []source.ViewInfo{{ViewName: "BadView", Definition: "SELECT id FROM Items"}, {ViewName: "HealthyView", Definition: "SELECT id FROM Items"}}
			wantTables, wantFailures := 2, 1
			switch kind {
			case "columns":
				r.views[0].Columns = []source.ColumnInfo{{Name: "Id"}, {Name: "ID"}}
			case "view_collision":
				r.views = append(r.views, source.ViewInfo{ViewName: "BADVIEW", Definition: "SELECT id FROM Items"})
				wantFailures = 2
			case "table_collision":
				r.views[0].ViewName = "orders"
				wantTables = 1
			case "sequence_collision":
				r.views[0].ViewName = "SEQ_ORDERS_ID"
				wantTables = 1
			case "target":
				w.preflight = func(_ map[string]*source.TableDDLInfo, objects []target.OracleObject) error {
					for _, object := range objects {
						if object.Kind == "VIEW" && object.Name == "BADVIEW" {
							return &target.OracleObjectPreflightError{Kind: "VIEW", Name: "BADVIEW", Err: errors.New("target table owns this view name")}
						}
					}
					return nil
				}
			}
			report := m.Run(context.Background())
			require.Equal(t, wantTables, report.Tables.Success)
			require.Equal(t, 1, report.Views.Success)
			require.Equal(t, wantFailures, report.Views.Failed)
			require.Contains(t, strings.Join(w.sql, "\n"), `CREATE OR REPLACE VIEW "APP"."HEALTHYVIEW"`)
			for _, item := range report.Views.Items {
				require.NotEmpty(t, item.SourceName)
				require.Equal(t, strings.ToUpper(item.SourceName), item.TargetName)
			}
		})
	}
}

func TestOracleStandaloneViewsPreserveIndependentSelections(t *testing.T) {
	r, w, m := oracleMigrationFixture()
	r.views = []source.ViewInfo{{ViewName: "Bad", Definition: "SELECT id FROM Orders", Columns: []source.ColumnInfo{{Name: "Id"}, {Name: "ID"}}}, {ViewName: "Good", Definition: "SELECT id FROM Orders"}}
	results := m.MigrateViews(context.Background(), []string{"Bad", "Good", "Missing"})
	require.Len(t, results, 3)
	for _, result := range results {
		if result.SourceName == "Good" {
			require.Empty(t, result.Error)
		} else {
			require.NotEmpty(t, result.Error)
		}
	}
	require.Len(t, w.sql, 1)
	require.Contains(t, w.sql[0], `"GOOD"`)
}

func TestOracleStandaloneViewsStopOnTaskFailureWithoutRunContext(t *testing.T) {
	r, w, m := oracleMigrationFixture()
	r.views = []source.ViewInfo{{ViewName: "A", Definition: "SELECT id FROM Orders"}, {ViewName: "B", Definition: "SELECT id FROM Orders"}}
	w.viewCause = driver.ErrBadConn
	results := m.MigrateViews(context.Background(), []string{"A", "B"})
	require.Len(t, results, 1)
	require.NotEmpty(t, results[0].Error)
	require.Empty(t, w.sql)
}

func TestOracleRuntimeRetainedDependenciesProtectPendingParentsOnly(t *testing.T) {
	for _, parent := range []string{"ITEMS", "ZLATER", "unreadable"} {
		t.Run(parent, func(t *testing.T) {
			r, w, m := oracleMigrationFixture()
			r.tables = append(r.tables, "ZLater")
			r.ddl["ZLater"] = &source.TableDDLInfo{TableName: "ZLater", Columns: []source.ColumnInfo{{Name: "id", DataType: "int"}}}
			r.rows["ZLater"] = [][]interface{}{{int64(1)}}
			w.ddlFailure = `DROP TABLE "APP"."ORDERS"`
			safe := &oracleRetainedDependencyWriter{oracleRecordingWriter: w, snapshot: target.MigrationDependencies{Schema: "APP"}}
			if parent == "unreadable" {
				safe.err = errors.New("target catalog unavailable")
			} else {
				safe.snapshot.Relations = []target.MigrationDependency{{Schema: "APP", Table: "ORDERS", RefSchema: "APP", RefTable: parent, Name: "OLD_FK"}}
			}
			m.writer = safe
			report := m.Run(context.Background())
			require.True(t, report.HasFailures())
			switch parent {
			case "ITEMS":
				// ITEMS was already rebuilt before ORDERS failed.
				require.False(t, m.migrationTableBlocked("Items"))
				require.Equal(t, 2, report.Tables.Success)
				require.ElementsMatch(t, []string{"ITEMS", "ZLATER"}, w.copied)
			case "ZLATER":
				require.True(t, m.migrationTableBlocked("ZLater"))
				require.Equal(t, 1, report.Tables.Success)
				require.Equal(t, []string{"ITEMS"}, w.copied)
				require.NotContains(t, strings.Join(w.ddlAttempts, "\n"), `"APP"."ZLATER"`)
			case "unreadable":
				require.Empty(t, w.copied)
				require.NotContains(t, strings.Join(w.ddlAttempts, "\n"), `"APP"."ZLATER"`)
			}
		})
	}
}

func TestOracleTaskFailuresStopFurtherDDLAndPostObjects(t *testing.T) {
	for _, stage := range []string{"preflight", "create", "post", "persistence"} {
		t.Run(stage, func(t *testing.T) {
			_, w, m := oracleMigrationFixture()
			switch stage {
			case "preflight":
				w.preflightError = driver.ErrBadConn
			case "create":
				w.ddlFailure, w.ddlCause = `CREATE TABLE "APP"."ITEMS"`, driver.ErrBadConn
			case "post":
				w.indexCause = driver.ErrBadConn
			case "persistence":
				m.cfg.CaptureRepair = true
				m.cfg.RepairSink = func(RepairManifest) error {
					if len(w.ddlAttempts) > 0 {
						return errors.New("disk full")
					}
					return nil
				}
			}
			report := m.Run(context.Background())
			require.True(t, report.HasFailures())
			if stage == "preflight" {
				require.Empty(t, w.ddlAttempts)
			} else if stage == "create" || stage == "persistence" {
				require.Len(t, w.ddlAttempts, 2)
				require.Empty(t, w.copied)
			}
			require.Zero(t, report.Sequences.Success)
			require.Zero(t, report.Views.Success)
		})
	}
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
