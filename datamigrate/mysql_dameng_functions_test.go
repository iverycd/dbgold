package datamigrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

// General fixtures model an already provisioned destination. Dedicated tests
// below and in target exercise installation, missing dependencies and failures.
func (w *mockWriter) EnsureMySQLDamengBusinessFunctions(context.Context, map[string]string) (*target.CompatibilityFunctionsReport, error) {
	return &target.CompatibilityFunctionsReport{Total: 7, Success: 7, Reused: 7}, nil
}

type dmFunctionWriter struct {
	*mysqlDMTriggerWriter
	called  int
	blocked map[string]string
	install func()
	err     error
}

func (w *dmFunctionWriter) EnsureMySQLDamengBusinessFunctions(_ context.Context, blocked map[string]string) (*target.CompatibilityFunctionsReport, error) {
	w.called++
	w.blocked = blocked
	if w.install != nil {
		w.install()
	}
	r := &target.CompatibilityFunctionsReport{Total: 1, Created: 1, Success: 1, Items: []target.CompatibilityFunctionResult{{Name: "GETITEMTEXT", Status: "created", DDL: "CREATE FUNCTION ..."}}}
	if w.err != nil {
		r.Created, r.Success, r.Failed = 0, 0, 1
		r.Items[0].Status, r.Items[0].Error = "failed", w.err.Error()
	}
	return r, w.err
}

func TestMySQLDamengBusinessFunctionsRunAtEnd(t *testing.T) {
	m, _, base := mysqlDMTriggerFixture(t)
	w := &dmFunctionWriter{mysqlDMTriggerWriter: base}
	m.writer = w
	w.install = func() {
		require.NotEmpty(t, w.created)
		require.NotEmpty(t, w.copied)
		require.Len(t, w.plans, 1, "functions follow triggers")
	}
	m.cfg.CaptureRepair = true
	report := m.Run(context.Background())
	require.Equal(t, 1, w.called)
	require.NotNil(t, report.RepairManifest)
	require.Equal(t, 1, report.CompatibilityFunctions.Created)
	require.False(t, report.HasFailures(), "%+v", report)
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	var saved MigrationReport
	require.NoError(t, json.Unmarshal(encoded, &saved))
	require.Equal(t, report.CompatibilityFunctions, saved.CompatibilityFunctions)
}

func TestMySQLDamengBusinessFunctionsScope(t *testing.T) {
	for _, tc := range []struct {
		name, content, purpose string
		objects, views         []string
		want                   bool
	}{
		{name: "default", want: true},
		{name: "both", content: "both", want: true},
		{name: "schema", content: "schema_only", want: true},
		{name: "data", content: "data_only"},
		{name: "cdc", purpose: "cdc"},
		{name: "objects", objects: []string{"indexes"}},
		{name: "views", views: []string{"v"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _, base := mysqlDMTriggerFixture(t)
			w := &dmFunctionWriter{mysqlDMTriggerWriter: base}
			m.writer = w
			m.cfg.Content, m.cfg.MigrationPurpose, m.cfg.Objects, m.cfg.ViewNames = tc.content, tc.purpose, tc.objects, tc.views
			m.cfg.MigrateTriggers = false
			m.Run(context.Background())
			require.Equal(t, tc.want, w.called == 1)
		})
	}
	m, _ := newTestMigrator(&mockReader{}, &mockWriter{})
	require.False(t, m.damengBusinessFunctionsRequested(), "PG does not install DM routines")
	r, w := dmFixture()
	require.True(t, dmMigrator(r, w).damengBusinessFunctionsRequested(), "SQL Server also installs Dameng routines")
}

func TestMySQLDamengBusinessFunctionFailuresPreserveMigration(t *testing.T) {
	for _, task := range []bool{false, true} {
		m, _, base := mysqlDMTriggerFixture(t)
		w := &dmFunctionWriter{mysqlDMTriggerWriter: base, err: errors.New("compile failed")}
		if task {
			w.err = io.EOF
		}
		m.writer = w
		report := m.Run(context.Background())
		require.True(t, report.HasFailures())
		require.Positive(t, report.Tables.Success)
		require.Positive(t, report.Data.Success)
		require.Equal(t, 1, report.CompatibilityFunctions.Failed)
		if task {
			require.Contains(t, report.Tables.Items[len(report.Tables.Items)-1].Error, "迁移任务已终止")
		}
	}
}

func TestMySQLDamengBusinessFunctionsSkipEarlyFailureAndCancellation(t *testing.T) {
	for _, stage := range []string{"cancel", "persistence", "trigger connection", "early return"} {
		t.Run(stage, func(t *testing.T) {
			m, _, base := mysqlDMTriggerFixture(t)
			w := &dmFunctionWriter{mysqlDMTriggerWriter: base}
			m.writer = w
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch stage {
			case "cancel":
				cancel()
			case "persistence":
				m.cfg.CaptureRepair = true
				m.cfg.RepairSink = func(RepairManifest) error { return errors.New("disk full") }
			case "trigger connection":
				base.beforeInstall = cancel
			case "early return":
				m.mainMigrationFinished = false
				r := newMigrationReport()
				m.finishDamengBusinessFunctions(ctx, &r)
				require.Zero(t, w.called)
				return
			}
			m.Run(ctx)
			require.Zero(t, w.called)
		})
	}
}

func TestMySQLDamengBusinessFunctionsCarryBlockedTables(t *testing.T) {
	m, _, base := mysqlDMTriggerFixture(t)
	w := &dmFunctionWriter{mysqlDMTriggerWriter: base}
	m.writer = w
	m.mainMigrationFinished = true
	m.blockMigrationTable("Frame_Ou", "create", errors.New("create table failed"))
	report := newMigrationReport()
	m.finishDamengBusinessFunctions(context.Background(), &report)
	require.Equal(t, "create table failed", w.blocked["FRAME_OU"])
}

// Existing real-DB fixtures migrate unrelated randomly named tables. The six
// fixed application routines may therefore fail on missing business tables,
// but no other installation or migration error is accepted by these tests.
func requireMySQLDMFixtureReport(t *testing.T, report MigrationReport, functionsExpected bool) {
	t.Helper()
	if functionsExpected {
		require.NotNil(t, report.CompatibilityFunctions)
		functions := report.CompatibilityFunctions
		require.Equal(t, 7, functions.Total)
		require.Len(t, functions.Items, 7)
		require.Equal(t, functions.Failed > 0, report.HasFailures(), "%+v", report)
		for _, item := range functions.Items {
			if item.Status == "failed" {
				require.Contains(t, item.Error, "函数依赖", "%+v", item)
				require.Contains(t, item.Error, "不存在或不可见", "%+v", item)
			}
		}
	} else {
		require.Nil(t, report.CompatibilityFunctions)
	}
	report.CompatibilityFunctions = nil // Check the original fixture's categories independently.
	require.False(t, report.HasFailures(), "%+v", report)
}

// FUN_GETPY can be installed even by unrelated table fixtures. Remove only
// routines this run actually created; never remove a skipped existing routine.
func cleanupMySQLDMFixtureFunctions(t *testing.T, db *sql.DB, report MigrationReport) {
	t.Helper()
	if report.CompatibilityFunctions == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, item := range report.CompatibilityFunctions.Items {
		if item.Status == "created" {
			_, err := db.ExecContext(ctx, "DROP FUNCTION "+dialect.NewDaMeng().QualifyTable(item.Schema, item.Name))
			if err != nil {
				t.Errorf("cleanup created function %s.%s: %v", item.Schema, item.Name, err)
			}
		}
	}
}
