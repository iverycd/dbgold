package datamigrate

import (
	"context"
	"database/sql/driver"
	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"fmt"
	"github.com/stretchr/testify/require"
	"slices"
	"strings"
	"sync"
	"testing"
)

type sqlServerFixtureReader struct {
	*oscarFixtureReader
	mu    sync.Mutex
	reads map[string]int
}

func (r *sqlServerFixtureReader) ReadPage(ctx context.Context, t string, pk []string, off, limit int64) ([]string, []string, [][]interface{}, error) {
	r.mu.Lock()
	r.reads[t]++
	r.mu.Unlock()
	return r.oscarFixtureReader.ReadPage(ctx, t, pk, off, limit)
}
func (r *sqlServerFixtureReader) SourceSchema() string { return "Src" }

type sqlServerRecordingWriter struct {
	recordingOscarWriter
	dia                  *dialect.SQLServerDialect
	existingErr, copyErr string
	blocked              string
	validated            int
	nameCheck            func([]string) error
	createErrors         map[string]error
	createAttempts       []string
}

func (w *sqlServerRecordingWriter) Dialect() dialect.Dialect { return w.dia }
func (w *sqlServerRecordingWriter) DBType() string           { return "sqlserver" }
func (w *sqlServerRecordingWriter) CheckNames(_ context.Context, names []string) error {
	if w.nameCheck != nil {
		return w.nameCheck(names)
	}
	return nil
}
func (w *sqlServerRecordingWriter) CreateTable(ctx context.Context, statements []dialect.Statement) error {
	ddl := dialect.JoinSQL(statements)
	w.createAttempts = append(w.createAttempts, ddl)
	for name, err := range w.createErrors {
		if strings.Contains(ddl, "CREATE TABLE [dbo].["+name+"]") {
			return err
		}
	}
	return w.recordingOscarWriter.CreateTable(ctx, statements)
}
func (w *sqlServerRecordingWriter) ValidateExisting(context.Context, *source.TableDDLInfo) error {
	w.validated++
	if w.existingErr != "" {
		return fmt.Errorf("%s", w.existingErr)
	}
	return nil
}
func (w *sqlServerRecordingWriter) RebuildDependencies(_ context.Context, tables map[string]bool) ([]dialect.Statement, error) {
	if tables[w.blocked] {
		return nil, &target.SQLServerDependencyError{Table: w.blocked, Reason: "范围外依赖"}
	}
	return nil, nil
}
func (w *sqlServerRecordingWriter) DropDependencies(context.Context, []dialect.Statement) error {
	return nil
}
func (w *sqlServerRecordingWriter) ObjectExists(context.Context, string) (bool, error) {
	return true, nil
}
func (w *sqlServerRecordingWriter) CopyData(ctx context.Context, t string, c, ct []string, rows [][]interface{}) error {
	if t == w.copyErr {
		return fmt.Errorf("bad data")
	}
	return w.recordingOscarWriter.CopyData(ctx, t, c, ct, rows)
}
func (w *sqlServerRecordingWriter) CreateIndex(_ context.Context, i source.IndexInfo) error {
	if e := w.dia.ValidateIndex(i); e != nil {
		return e
	}
	return w.record(w.dia.IndexStatements("dbo", i))
}
func (w *sqlServerRecordingWriter) CreateSequence(_ context.Context, s source.SequenceInfo, _ string) error {
	return w.record(w.dia.SequenceStatements("dbo", s, ""))
}
func (w *sqlServerRecordingWriter) CreateForeignKey(_ context.Context, f source.FKInfo) error {
	if e := w.dia.ValidateForeignKey(f); e != nil {
		return e
	}
	return w.record(w.dia.ForeignKeyStatements("dbo", f))
}
func (w *sqlServerRecordingWriter) CreateView(_ context.Context, v source.ViewInfo) error {
	return w.record(w.dia.ViewStatements("dbo", v))
}
func (w *sqlServerRecordingWriter) CreateComment(_ context.Context, c source.CommentInfo) error {
	return w.record(w.dia.CommentStatements("dbo", c))
}
func sqlServerFixture() (*sqlServerFixtureReader, *sqlServerRecordingWriter, *Migrator) {
	r := &sqlServerFixtureReader{oscarFixtureReader: newOscarFixture(), reads: map[string]int{}}
	for _, i := range r.ddl {
		i.Columns[0].IsNullable = false
	}
	r.ddl["Orders"].Columns[0].Extra = "auto_increment"
	w := &sqlServerRecordingWriter{dia: dialect.NewSQLServer()}
	m := NewMigrator(r, w, &Job{LogCh: make(chan string, 512)}, Config{TargetDBType: "sqlserver", Mode: "all", PageSize: 1, MaxParallel: 2})
	return r, w, m
}
func TestSQLServerMigrationModes(t *testing.T) {
	for _, mode := range []string{"both", "schema_only", "data_only", "objects"} {
		t.Run(mode, func(t *testing.T) {
			_, w, m := sqlServerFixture()
			m.cfg.Content = mode
			if mode == "objects" {
				m.cfg.Objects = []string{"primary_keys", "indexes", "comments"}
			}
			r := m.Run(context.Background())
			require.False(t, r.HasFailures(), "%+v", r)
			sql := strings.Join(w.sql, "\n")
			if mode == "both" || mode == "schema_only" {
				require.Contains(t, sql, "CREATE TABLE")
				require.Equal(t, 1, r.Views.Success)
			} else {
				require.NotContains(t, sql, "CREATE TABLE")
				require.Equal(t, 2, w.validated)
			}
			if mode == "both" || mode == "data_only" {
				require.Equal(t, 2, r.Data.Success)
			} else {
				require.Empty(t, w.copied)
			}
			if mode == "data_only" {
				require.NotContains(t, sql, "CREATE NONCLUSTERED INDEX")
				require.Contains(t, sql, "DBCC CHECKIDENT")
			}
		})
	}
}
func TestSQLServerFailureStopsOnlyItsTable(t *testing.T) {
	r, w, m := sqlServerFixture()
	w.copyErr = "Orders"
	report := m.Run(context.Background())
	require.Equal(t, 1, report.Data.Failed)
	require.Equal(t, 1, report.Data.Success)
	require.Equal(t, 1, r.reads["Orders"])
	require.Equal(t, 1, report.Views.Failed)
	require.Contains(t, report.Data.Items[0].Error, "之前提交的数据可能保留")
}
func TestSQLServerUnsupportedTablePreserved(t *testing.T) {
	r, w, m := sqlServerFixture()
	r.ddl["Orders"].Columns[0].DataType = "json"
	report := m.Run(context.Background())
	require.Equal(t, 1, report.Tables.Failed)
	require.Equal(t, 1, report.Tables.Success)
	require.NotContains(t, strings.Join(w.sql, "\n"), "DROP TABLE [dbo].[Orders]")
	require.Contains(t, report.Tables.Items[0].Error, "json")
	require.True(t, m.migrationTableBlocked("Orders"))
	require.False(t, m.migrationTableBlocked("Items"))
}
func TestSQLServerExternalDependencyDoesNotStopIndependentTable(t *testing.T) {
	_, w, m := sqlServerFixture()
	w.blocked = "Orders"
	report := m.Run(context.Background())
	require.Equal(t, 1, report.Tables.Failed)
	require.Equal(t, 1, report.Tables.Success)
	require.NotContains(t, strings.Join(w.sql, "\n"), "DROP TABLE [dbo].[Orders]")
}
func TestSQLServerTargetCollationConflictsBlockBothSourcesOnly(t *testing.T) {
	r, w, m := sqlServerFixture()
	r.tables = append(r.tables, "ORDERS")
	w.nameCheck = func(names []string) error {
		if slices.Contains(names, "Orders") && slices.Contains(names, "ORDERS") {
			return &target.SQLServerNameError{Names: []string{"Orders", "ORDERS"}}
		}
		return nil
	}
	report := m.Run(context.Background())
	require.Equal(t, 2, report.Tables.Failed)
	require.Equal(t, 1, report.Tables.Success)
	require.True(t, m.migrationTableBlocked("Orders"))
	require.True(t, m.migrationTableBlocked("ORDERS"))
	require.False(t, m.migrationTableBlocked("Items"))
	sql := strings.Join(w.sql, "\n")
	require.Contains(t, sql, "CREATE TABLE [dbo].[Items]")
	require.NotContains(t, sql, "DROP TABLE [dbo].[Orders]")
	require.NotContains(t, sql, "DROP TABLE [dbo].[ORDERS]")
	require.Equal(t, 1, report.Data.Success)
	for _, item := range report.Tables.Items {
		require.Equal(t, "preflight", item.Stage)
	}
}

func TestSQLServerUnknownNameInspectionFailureStopsBeforeWrites(t *testing.T) {
	_, w, m := sqlServerFixture()
	w.nameCheck = func([]string) error { return fmt.Errorf("target collation unavailable") }
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.Empty(t, w.sql)
	require.Empty(t, w.copied)
}

func TestSQLServerColumnNameInspectionFailureKeepsUnknownScopeGlobal(t *testing.T) {
	_, w, m := sqlServerFixture()
	w.nameCheck = func(names []string) error {
		if slices.Contains(names, "Items") || slices.Contains(names, "Orders") {
			return nil
		}
		return fmt.Errorf("target collation unavailable during column check")
	}
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.Empty(t, w.sql)
	require.Empty(t, w.copied)
}

func TestSQLServerCreateFailureScopeControlsLaterTables(t *testing.T) {
	for _, task := range []bool{false, true} {
		t.Run(fmt.Sprint(task), func(t *testing.T) {
			_, w, m := sqlServerFixture()
			cause := error(fmt.Errorf("invalid table DDL"))
			if task {
				cause = driver.ErrBadConn
			}
			w.createErrors = map[string]error{"Items": cause}
			report := m.Run(context.Background())
			require.True(t, report.HasFailures())
			if task {
				require.Len(t, w.createAttempts, 1)
				require.Empty(t, w.copied)
				require.Zero(t, report.Tables.Success)
			} else {
				require.Len(t, w.createAttempts, 2)
				require.Equal(t, 1, report.Tables.Success)
				require.Equal(t, 1, report.Tables.Failed)
				require.True(t, m.migrationTableBlocked("Items"))
				require.False(t, m.migrationTableBlocked("Orders"))
				require.Equal(t, []string{"Orders"}, w.copied)
			}
		})
	}
}

func TestSQLServerViewDependencyOrderingAndCycles(t *testing.T) {
	r, w, m := sqlServerFixture()
	r.views = []source.ViewInfo{{ViewName: "A", Definition: "SELECT * FROM Z"}, {ViewName: "Z", Definition: "SELECT * FROM Orders"}}
	out := m.MigrateViews(context.Background(), []string{"A", "Z"})
	require.Len(t, out, 2)
	for _, v := range out {
		require.Empty(t, v.Error)
	}
	require.Contains(t, w.sql[0], "CREATE VIEW [dbo].[Z]")
	require.Contains(t, w.sql[1], "CREATE VIEW [dbo].[A]")
	w.sql = nil
	r.views[1].Definition = "SELECT * FROM A"
	out = m.MigrateViews(context.Background(), []string{"A", "Z"})
	require.Len(t, out, 2)
	for _, v := range out {
		require.Contains(t, v.Error, "循环依赖")
	}
	require.Empty(t, w.sql)
}
func TestSQLServerBackendRejectsUnsupportedOptions(t *testing.T) {
	_, w, m := sqlServerFixture()
	m.cfg.UseNvarchar2 = true
	r := m.Run(context.Background())
	require.True(t, r.HasFailures())
	require.Empty(t, w.sql)
}

func TestSQLServerAppendDependencyOrder(t *testing.T) {
	order, blocked := sqlServerDataOrder([]string{"Child", "Parent", "Other"}, []source.FKInfo{{TableName: "Child", RefTable: "Parent"}})
	require.Empty(t, blocked)
	require.Less(t, slices.Index(order, "Parent"), slices.Index(order, "Child"))
	order, blocked = sqlServerDataOrder([]string{"A", "B", "Other"}, []source.FKInfo{{TableName: "A", RefTable: "B"}, {TableName: "B", RefTable: "A"}})
	require.Equal(t, []string{"Other"}, order)
	require.Equal(t, []string{"A", "B"}, blocked)
}

func TestSQLServerViewGlobalFailureIsNeverSuccess(t *testing.T) {
	_, w, m := sqlServerFixture()
	m.cfg.ChangeOwner = true
	out := m.MigrateViews(context.Background(), []string{"vOrders"})
	require.Len(t, out, 1)
	require.NotEmpty(t, out[0].Error)
	require.Empty(t, w.sql)
}
