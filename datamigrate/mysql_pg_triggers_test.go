package datamigrate

import (
	"context"
	"errors"
	"testing"

	"dbgold/datamigrate/mysqltrigger"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

type triggerTestReader struct {
	mockReader
	triggers   []source.TriggerInfo
	triggerErr error
	reads      int
}

func (r *triggerTestReader) GetTriggers(context.Context) ([]source.TriggerInfo, error) {
	r.reads++
	return r.triggers, r.triggerErr
}

// Existing migration mocks have no business triggers; new guards remain exercised
// separately through triggerTestWriter and the target package's catalog tests.
func (w *mockWriter) CheckMySQLPGLoadTriggers(context.Context, []string) error { return nil }

type triggerTestWriter struct {
	mockWriter
	plans         []mysqltrigger.Plan
	loadError     error
	executeError  error
	beforeExecute func()
}

type triggerCountMismatchWriter struct{ *triggerTestWriter }

func (w *triggerCountMismatchWriter) CountRows(context.Context, string) (int64, error) { return 1, nil }

func (w *triggerTestWriter) CheckMySQLPGLoadTriggers(context.Context, []string) error {
	return w.loadError
}
func (w *triggerTestWriter) CreateMySQLPGTrigger(ctx context.Context, p mysqltrigger.Plan, owner string) error {
	if w.beforeExecute != nil {
		w.beforeExecute()
	}
	w.plans = append(w.plans, p)
	for _, sql := range p.Statements() {
		if err := repair.Observe(ctx, sql, "running", nil); err != nil {
			return err
		}
	}
	if w.executeError != nil {
		for _, sql := range p.Statements() {
			_ = repair.Observe(ctx, sql, "rolled_back", w.executeError)
		}
		return w.executeError
	}
	for _, sql := range p.Statements() {
		if err := repair.Observe(ctx, sql, "succeeded", nil); err != nil {
			return err
		}
	}
	return nil
}

func triggerFixture(t *testing.T) (*Migrator, *triggerTestReader, *triggerTestWriter) {
	t.Helper()
	r := &triggerTestReader{mockReader: mockReader{
		tables: []string{"OrderData"},
		ddl: map[string]*source.TableDDLInfo{"OrderData": {TableName: "OrderData", Columns: []source.ColumnInfo{
			{Name: "Id", DataType: "int"}, {Name: "Amount", DataType: "decimal", Precision: 12, Scale: 2, IsNullable: true},
		}}}, rows: map[string][][]interface{}{"OrderData": {{1, 2}}}, triggerCount: 9,
	}, triggers: []source.TriggerInfo{{Schema: "App", TableName: "OrderData", Name: "FillAmount", Timing: "BEFORE", Event: "INSERT", Body: "SET NEW.Amount = IFNULL(NEW.Amount, 0)", SourceDDL: "CREATE TRIGGER FillAmount BEFORE INSERT ON OrderData FOR EACH ROW SET NEW.Amount = IFNULL(NEW.Amount, 0)", ActionOrder: 1}}}
	w := &triggerTestWriter{}
	m, _ := newTestMigrator(r, w)
	m.cfg.TargetSchema, m.cfg.MigrateTriggers = "dst", true
	return m, r, w
}

func TestMySQLPGTriggerOptInAndPostDataExecution(t *testing.T) {
	m, r, w := triggerFixture(t)
	w.beforeExecute = func() { require.NotEmpty(t, w.copied, "historical rows must load before trigger installation") }
	m.cfg.CaptureRepair = true
	var inventory bool
	m.cfg.RepairSink = func(manifest RepairManifest) error {
		for _, o := range manifest.Objects {
			if o.Kind == "trigger" && o.Status == "pending" {
				inventory = true
			}
		}
		return nil
	}
	report := m.Run(context.Background())
	require.Equal(t, 1, r.reads)
	require.True(t, inventory)
	require.True(t, report.TriggerMigrationEnabled)
	require.Equal(t, 1, report.Triggers.Total)
	require.Equal(t, 1, report.Triggers.Success)
	require.Zero(t, report.Triggers.Failed)
	require.Len(t, w.plans, 1)
	require.NotEmpty(t, w.plans[0].Tables)
	require.NotEmpty(t, w.plans[0].SourceTables)
	var recorded *repair.Object
	for i := range report.RepairManifest.Objects {
		if report.RepairManifest.Objects[i].Kind == "trigger" {
			recorded = &report.RepairManifest.Objects[i]
		}
	}
	require.NotNil(t, recorded)
	require.Equal(t, "succeeded", recorded.Status)
	require.NotNil(t, recorded.MySQLPGTrigger)
	require.Len(t, recorded.Units, 2)
	for _, u := range recorded.Units {
		require.Equal(t, "succeeded", u.Status)
	}
}

func TestMySQLPGTriggerDisabledRetainsInventory(t *testing.T) {
	m, r, w := triggerFixture(t)
	m.cfg.MigrateTriggers = false
	report := m.Run(context.Background())
	require.False(t, report.TriggerMigrationEnabled)
	require.Equal(t, 9, report.Triggers.Total)
	require.Zero(t, r.reads)
	require.Empty(t, w.plans)
}

func TestMySQLPGTriggerSubsetFailuresContinue(t *testing.T) {
	m, r, w := triggerFixture(t)
	bad := r.triggers[0]
	bad.Name, bad.Event, bad.Body = "Unsupported", "UPDATE", "CALL source_proc()"
	r.triggers = append(r.triggers, bad)
	report := m.Run(context.Background())
	require.Equal(t, 2, report.Triggers.Total)
	require.Equal(t, 1, report.Triggers.Success)
	require.Equal(t, 1, report.Triggers.Failed)
	require.Len(t, w.plans, 1)
	require.Equal(t, "unsupported", report.Triggers.Items[0].Stage)
	require.NotEmpty(t, report.Triggers.Items[0].SourceDDL)
	require.True(t, report.HasFailures())
}

func TestMySQLPGTriggerSameEventGroupNeverInstalled(t *testing.T) {
	m, r, w := triggerFixture(t)
	second := r.triggers[0]
	second.Name = "Another"
	r.triggers = append(r.triggers, second)
	report := m.Run(context.Background())
	require.Equal(t, 2, report.Triggers.Failed)
	require.Empty(t, w.plans)
}

func TestMySQLPGTriggerFailedTableBlocksInstallation(t *testing.T) {
	for _, stage := range []string{"schema", "data"} {
		t.Run(stage, func(t *testing.T) {
			m, _, w := triggerFixture(t)
			w.createTableFail = stage == "schema"
			w.copyDataFail = stage == "data"
			report := m.Run(context.Background())
			require.Empty(t, w.plans)
			require.Equal(t, 1, report.Triggers.Failed)
			require.Equal(t, "dependency", report.Triggers.Items[0].Stage)
		})
	}
}

func TestMySQLPGTriggerRowCountMismatchBlocksInstallation(t *testing.T) {
	m, _, w := triggerFixture(t)
	m.writer = &triggerCountMismatchWriter{w}
	report := m.Run(context.Background())
	require.Empty(t, w.plans)
	require.Equal(t, 1, report.Triggers.Failed)
	require.Equal(t, "dependency", report.Triggers.Items[0].Stage)
}

func TestMySQLPGTriggerObjectAndSchemaOnlyModes(t *testing.T) {
	for _, objects := range []bool{false, true} {
		t.Run(map[bool]string{false: "schema_only", true: "objects"}[objects], func(t *testing.T) {
			m, _, w := triggerFixture(t)
			m.cfg.Content = "schema_only"
			if objects {
				m.cfg.MigrateTriggers = false
				m.cfg.Objects = []string{"triggers"}
				m.cfg.TableNames = []string{"OrderData"}
			}
			report := m.Run(context.Background())
			require.True(t, report.TriggerMigrationEnabled)
			require.Equal(t, 1, report.Triggers.Success)
			require.Empty(t, w.copied)
			if objects {
				require.Empty(t, w.created)
			}
		})
	}
}

func TestMySQLPGTriggerMetadataAndExecutionFailures(t *testing.T) {
	t.Run("metadata", func(t *testing.T) {
		m, r, w := triggerFixture(t)
		r.triggerErr = errors.New("metadata denied")
		report := m.Run(context.Background())
		require.True(t, report.Triggers.TotalUnknown)
		require.Equal(t, "metadata", report.Triggers.Items[0].Stage)
		require.Empty(t, w.plans)
	})
	t.Run("rollback", func(t *testing.T) {
		m, _, w := triggerFixture(t)
		m.cfg.CaptureRepair = true
		w.executeError = errors.New("trigger create denied")
		report := m.Run(context.Background())
		require.Equal(t, "execute", report.Triggers.Items[0].Stage)
		require.NotEmpty(t, report.Triggers.Items[0].DDL)
		for _, o := range report.RepairManifest.Objects {
			if o.Kind == "trigger" {
				require.Equal(t, "failed", o.Status)
				for _, u := range o.Units {
					require.Equal(t, "rolled_back", u.Status)
				}
			}
		}
	})
}

func TestMySQLPGDataOnlyRejectsActiveTriggersBeforeCopy(t *testing.T) {
	m, r, w := triggerFixture(t)
	m.cfg.Content = "data_only"
	w.loadError = errors.New("active trigger on OrderData")
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.Equal(t, "preflight", report.Data.Items[0].Stage)
	require.Empty(t, w.copied)
	require.Empty(t, w.plans)
	require.Zero(t, r.reads)
}

func TestLegacyTriggerReportOnlyBecomesRepairableWhenEnabled(t *testing.T) {
	report := newMigrationReport()
	report.Triggers = CategoryReport{Total: 1, Failed: 1, Items: []ObjectResult{{Name: "Tr", Error: "unsupported", Stage: "unsupported", SourceDDL: "original"}}}
	require.Empty(t, LegacyRepairObjects(report, "dst"))
	report.TriggerMigrationEnabled = true
	objects := LegacyRepairObjects(report, "dst")
	require.Len(t, objects, 1)
	require.Equal(t, "trigger", objects[0].Kind)
	require.True(t, objects[0].DiagnosticOnly)
}

func TestMySQLPGTriggerExternalDependencyIsNotMissingLedgerWork(t *testing.T) {
	m, r, _ := triggerFixture(t)
	r.tables = append(r.tables, "Audit")
	r.ddl["Audit"] = &source.TableDDLInfo{TableName: "Audit", Columns: []source.ColumnInfo{{Name: "Id", DataType: "int"}}}
	r.triggers[0].Body = "INSERT INTO Audit (Id) VALUES (NEW.Id)"
	r.triggers[0].Timing = "AFTER"
	m.cfg.Objects = []string{"triggers"}
	m.cfg.TableNames = []string{"OrderData"}
	m.cfg.CaptureRepair = true
	report := m.Run(context.Background())
	require.Equal(t, 1, report.Triggers.Success)
	ids := map[string]bool{}
	for _, o := range report.RepairManifest.Objects {
		ids[o.ID] = true
	}
	for _, o := range report.RepairManifest.Objects {
		if o.Kind == "trigger" {
			require.Len(t, o.MySQLPGTrigger.Dependencies, 2)
			for _, dep := range o.Dependencies {
				require.True(t, ids[dep], "existing external target is verified by plan, not absent ledger work")
			}
		}
	}
}

func TestMySQLPGTriggerRecoveryReviewAndFrozenMapping(t *testing.T) {
	m, _, w := triggerFixture(t)
	m.cfg.CaptureRepair = true
	w.copyDataFail = true
	report := m.Run(context.Background())
	var table, trigger repair.Object
	for _, o := range report.RepairManifest.Objects {
		if o.Kind == "trigger" {
			trigger = o
		}
		if o.Kind == "table" {
			table = o
		}
	}
	require.NotNil(t, trigger.MySQLPGTrigger)
	require.True(t, trigger.RequiresReview)
	unchanged := regenerateRepairCandidate(w, m.cfg, table, trigger, map[string]string{"Id": "Id", "Amount": "Amount"})
	require.False(t, unchanged.RequiresReview, "whole immutable plan is reviewed at execution confirmation")
	require.False(t, unchanged.DiagnosticOnly)
	changed := regenerateRepairCandidate(w, m.cfg, table, trigger, map[string]string{"Id": "Id", "Amount": "Other"})
	require.True(t, changed.DiagnosticOnly)
	require.Equal(t, "blocked", changed.Status)
	require.Contains(t, changed.Error, "映射已改变")
}
