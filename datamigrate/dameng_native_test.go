package datamigrate

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

type nativeTestReader struct {
	source.Reader
	snapshot *damengnative.Snapshot
	rows     [][]any
	scans    int
	mu       sync.Mutex
}

func (r *nativeTestReader) DBType() string { return "dameng" }
func (r *nativeTestReader) SnapshotDamengNative(context.Context) (*damengnative.Snapshot, error) {
	return r.snapshot, nil
}
func (r *nativeTestReader) OpenDamengNativeScan(_ context.Context, _ damengnative.Table) (damengnative.Scan, error) {
	r.mu.Lock()
	r.scans++
	r.mu.Unlock()
	return &nativeTestScan{rows: r.rows, pos: -1}, nil
}

type nativeTestScan struct {
	rows [][]any
	pos  int
}

func (s *nativeTestScan) Next() bool             { s.pos++; return s.pos < len(s.rows) }
func (s *nativeTestScan) Values() ([]any, error) { return append([]any{}, s.rows[s.pos]...), nil }
func (s *nativeTestScan) Err() error             { return nil }
func (s *nativeTestScan) Close() error           { return nil }

type nativeTestWriter struct {
	target.Writer
	snapshot         *damengnative.Snapshot
	statements       []string
	copied           [][]any
	fail             map[string]int
	copyErr          error
	disable, restore []damengnative.Unit
	batchSizes       []int
	mu               sync.Mutex
}

func (w *nativeTestWriter) DBType() string          { return "dameng" }
func (w *nativeTestWriter) EffectiveSchema() string { return w.snapshot.Profile.Schema }
func (w *nativeTestWriter) InspectDamengNative(context.Context) (*damengnative.Snapshot, error) {
	return w.snapshot, nil
}
func (w *nativeTestWriter) ExecuteDamengNative(ctx context.Context, sql string) error {
	if err := repair.Observe(ctx, sql, "running", nil); err != nil {
		return err
	}
	w.mu.Lock()
	w.statements = append(w.statements, sql)
	var e error
	if w.fail[sql] > 0 {
		w.fail[sql]--
		e = errors.New("dependent object unavailable")
	}
	if strings.HasPrefix(sql, "DROP TABLE") || strings.HasPrefix(sql, "TRUNCATE TABLE") {
		w.copied = nil
	}
	w.mu.Unlock()
	status := "succeeded"
	if e != nil {
		status = "failed"
	}
	return errors.Join(e, repair.Observe(ctx, sql, status, e))
}
func (w *nativeTestWriter) CopyDamengNativeBatch(_ context.Context, t damengnative.Table, rows [][]any) error {
	if t.Schema != w.EffectiveSchema() {
		return errors.New("incorrect target schema")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.copyErr != nil {
		return w.copyErr
	}
	w.batchSizes = append(w.batchSizes, len(rows))
	for _, row := range rows {
		w.copied = append(w.copied, append([]any{}, row...))
	}
	return nil
}
func (w *nativeTestWriter) NativeRecoveryState(context.Context, string) ([]damengnative.Unit, []damengnative.Unit, error) {
	return w.disable, w.restore, nil
}
func (w *nativeTestWriter) VerifyDamengNative(context.Context, *damengnative.Snapshot) ([]damengnative.Issue, error) {
	return nil, nil
}
func (w *nativeTestWriter) CountRows(context.Context, string) (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return int64(len(w.copied)), nil
}

func nativeTestProfile(schema, instance string) damengnative.Profile {
	return damengnative.Profile{Version: "DM8 V8.1.4", Instance: instance, Schema: schema, Settings: map[string]string{"COMPATIBLE_MODE": "2", "CASE_SENSITIVE": "1", "CHARSET": "1", "PAGE_SIZE": "16"}}
}
func nativeTestMigrator(cfg Config) (*Migrator, *nativeTestReader, *nativeTestWriter) {
	obj := damengnative.Object{ID: "source-table", Kind: "table", Schema: "Src", Name: "Mixed", Units: []damengnative.Unit{{SQL: `CREATE TABLE "Src"."Mixed" ("Value" VARCHAR(100))`, Phase: "before_data"}}, DropSQL: `DROP TABLE "Src"."Mixed"`}
	r := &nativeTestReader{snapshot: &damengnative.Snapshot{Profile: nativeTestProfile("Src", "source"), Objects: []damengnative.Object{obj}, Tables: []damengnative.Table{{ObjectID: obj.ID, Schema: "Src", Name: "Mixed", Rows: 2, Columns: []damengnative.Column{{Name: "Value", DataType: "VARCHAR", SQLType: "VARCHAR(100)", Writable: true}}}}}, rows: [][]any{{"a\x00中"}, {"Mixed CASE"}}}
	w := &nativeTestWriter{snapshot: &damengnative.Snapshot{Profile: nativeTestProfile("Dst", "target")}, fail: map[string]int{}}
	return NewMigrator(r, w, &Job{LogCh: make(chan string, 1000)}, cfg), r, w
}

func TestDamengNativeRunPreservesNamesDataAndFrozenUnits(t *testing.T) {
	m, r, w := nativeTestMigrator(Config{CaptureRepair: true, Mode: "all", Content: "both", LowerCaseNames: true})
	var saved RepairManifest
	m.cfg.RepairSink = func(manifest RepairManifest) error { saved = manifest; return nil }
	report := m.Run(context.Background())
	require.False(t, report.HasFailures(), "%+v", report)
	require.Equal(t, 1, r.scans)
	require.Equal(t, [][]any{{"a\x00中"}, {"Mixed CASE"}}, w.copied)
	require.Equal(t, []string{`CREATE TABLE "Dst"."Mixed" ("Value" VARCHAR(100))`}, w.statements)
	require.False(t, saved.Config.LowerCaseNames)
	require.Len(t, saved.Objects, 1)
	require.True(t, saved.Objects[0].TrustedNative())
	require.Equal(t, "succeeded", saved.Objects[0].Status)
	require.Equal(t, "succeeded", saved.Objects[0].DataStatus)
	require.NotNil(t, saved.Objects[0].NativeTable)
}

func TestDamengNativePreflightFailuresNeverMutate(t *testing.T) {
	for _, name := range []string{"conflict", "unknown", "same_schema", "version", "settings", "partial", "metadata"} {
		t.Run(name, func(t *testing.T) {
			m, r, w := nativeTestMigrator(Config{Mode: "all"})
			switch name {
			case "conflict":
				w.snapshot.Objects = []damengnative.Object{{ID: "existing", Kind: "table", Schema: "Dst", Name: "Mixed", DropSQL: `DROP TABLE "Dst"."Mixed"`}}
			case "unknown":
				r.snapshot.Objects = append(r.snapshot.Objects, damengnative.Object{ID: "unknown", Kind: "unsupported", Name: "u", Schema: "Src"})
			case "same_schema":
				w.snapshot.Profile = r.snapshot.Profile
			case "version":
				w.snapshot.Profile.Version = "DM8 other"
			case "settings":
				delete(w.snapshot.Profile.Settings, "COMPATIBLE_MODE")
			case "partial":
				m.cfg.Mode = "include"
			case "metadata":
				r.snapshot.Issues = []damengnative.Issue{{Severity: "error", Reason: "permission denied"}}
			}
			report := m.Run(context.Background())
			require.True(t, report.HasFailures())
			require.Empty(t, w.statements)
			require.Empty(t, w.copied)
			require.Zero(t, r.scans)
		})
	}
}

func TestDamengNativeOverwriteDropsOnlyConflictsAndPersistsBeforeDrop(t *testing.T) {
	m, _, w := nativeTestMigrator(Config{Mode: "all", CaptureRepair: true, OverwriteExisting: true})
	w.snapshot.Objects = []damengnative.Object{{ID: "existing", Kind: "table", Name: "Mixed", Schema: "Dst", DropSQL: `DROP TABLE "Dst"."Mixed"`}, {ID: "untouched", Kind: "table", Name: "Keep", Schema: "Dst", DropSQL: `DROP TABLE "Dst"."Keep"`}}
	savedBeforeWrite := false
	m.cfg.RepairSink = func(manifest RepairManifest) error {
		if len(w.statements) == 0 {
			savedBeforeWrite = true
			require.Len(t, manifest.Objects, 2)
		}
		return nil
	}
	report := m.Run(context.Background())
	require.False(t, report.HasFailures())
	require.True(t, savedBeforeWrite)
	require.Equal(t, `DROP TABLE "Dst"."Mixed"`, w.statements[0])
	for _, sql := range w.statements {
		require.NotContains(t, sql, `"Keep"`)
	}
}

func TestDamengNativeCannotInvalidateTargetOnlyDependants(t *testing.T) {
	m, _, w := nativeTestMigrator(Config{OverwriteExisting: true})
	w.snapshot.Objects = []damengnative.Object{{ID: "existing", Kind: "table", Name: "Mixed", Schema: "Dst", DropSQL: `DROP TABLE "Dst"."Mixed"`}, {ID: "outside", Kind: "view", Name: "KeepView", Schema: "Dst", Dependencies: []string{"existing"}}}
	p := m.PreflightDamengNative(context.Background())
	require.False(t, p.Ready)
	require.Empty(t, w.statements)
}

func TestDamengNativeCheckpointFailureStopsBeforeAnyDDL(t *testing.T) {
	m, r, w := nativeTestMigrator(Config{CaptureRepair: true})
	m.cfg.RepairSink = func(RepairManifest) error { return errors.New("disk full") }
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.Empty(t, w.statements)
	require.Zero(t, r.scans)
}

func TestDamengNativeFailedCycleTerminatesAndAffectsReport(t *testing.T) {
	m, r, w := nativeTestMigrator(Config{CaptureRepair: true})
	sql := `CREATE FUNCTION "Dst"."f" RETURN INT AS BEGIN RETURN missing(); END;`
	r.snapshot.Objects = append(r.snapshot.Objects, damengnative.Object{ID: "function", Kind: "function", Schema: "Src", Name: "f", Units: []damengnative.Unit{{SQL: strings.ReplaceAll(sql, `"Dst"`, `"Src"`), Phase: "after_data"}}})
	w.fail[sql] = 10
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.Len(t, report.NativeSchema.Categories, 1)
	require.Equal(t, 1, report.NativeSchema.Categories[0].Failed)
	require.Equal(t, 1, w.fail[sql]+len(w.statements)-10)
}

func TestDamengNativeCancelledBeforeMutation(t *testing.T) {
	m, r, w := nativeTestMigrator(Config{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report := m.Run(ctx)
	require.True(t, report.HasFailures())
	require.Empty(t, w.statements)
	require.Zero(t, r.scans)
}

func TestTrustedNativeRejectsChangedUnitsOrPlan(t *testing.T) {
	o := damengnative.Object{Kind: "procedure", Schema: "Dst", Name: "P", Units: []damengnative.Unit{{SQL: `CREATE PROCEDURE "Dst"."P" AS BEGIN NULL; END;`}}}
	record := repair.Object{Schema: o.Schema, TargetName: o.Name, Kind: o.Kind, Native: &o, NativeFingerprint: damengnative.Fingerprint(o), Units: []repair.Unit{{SQL: o.Units[0].SQL, OriginalSQL: o.Units[0].SQL}}}
	require.True(t, record.TrustedNative())
	require.False(t, record.CanEditDDL())
	record.Units[0].SQL += " DROP TABLE x"
	require.False(t, record.CanExecuteDDL())
}

func nativeRecoveryFixture(t *testing.T, failCopy bool) (*Migrator, *nativeTestReader, *nativeTestWriter, repair.Object) {
	m, r, w := nativeTestMigrator(Config{CaptureRepair: true, TargetSchema: "Dst"})
	if failCopy {
		w.copyErr = errors.New("write interrupted")
	} else {
		sql := `ALTER TABLE "Src"."Mixed" ADD CONSTRAINT "C" CHECK ("Value" IS NOT NULL)`
		r.snapshot.Objects[0].Units = append(r.snapshot.Objects[0].Units, damengnative.Unit{SQL: sql, Phase: "after_data"})
		w.fail[strings.ReplaceAll(sql, `"Src"`, `"Dst"`)] = 1
	}
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.NotNil(t, report.RepairManifest)
	require.Len(t, report.RepairManifest.Objects, 1)
	w.snapshot.Tables = append([]damengnative.Table{}, r.snapshot.Tables...)
	w.snapshot.Tables[0].Schema = "Dst"
	w.copyErr = nil
	return m, r, w, report.RepairManifest.Objects[0]
}

func TestDamengNativeRecoveryRunsPendingTableUnitsWithoutRecopy(t *testing.T) {
	m, r, w, o := nativeRecoveryFixture(t, false)
	p, err := PreviewRecovery(context.Background(), r, w, m.cfg, o, nil, nil)
	require.NoError(t, err)
	require.False(t, p.DataRequired)
	scans := r.scans
	res, err := RunRecovery(context.Background(), r, w, m.cfg, p, false, nil)
	require.NoError(t, err)
	require.Equal(t, "succeeded", res.Status)
	require.Equal(t, scans, r.scans)
	require.Equal(t, `ALTER TABLE "Dst"."Mixed" ADD CONSTRAINT "C" CHECK ("Value" IS NOT NULL)`, w.statements[len(w.statements)-1])
}

func TestDamengNativeRecoveryRequiresExplicitTruncateAndUsesNativeData(t *testing.T) {
	m, r, w, o := nativeRecoveryFixture(t, true)
	w.copied = [][]any{{"partial"}}
	w.disable = []damengnative.Unit{{SQL: `ALTER TRIGGER "Dst"."Tr" DISABLE`}}
	w.restore = []damengnative.Unit{{SQL: `ALTER TRIGGER "Dst"."Tr" ENABLE`}}
	p, err := PreviewRecovery(context.Background(), r, w, m.cfg, o, nil, nil)
	require.NoError(t, err)
	require.True(t, p.DataRequired)
	before := len(w.statements)
	_, err = RunRecovery(context.Background(), r, w, m.cfg, p, false, nil)
	require.ErrorContains(t, err, "单独确认清空")
	require.Len(t, w.statements, before)
	res, err := RunRecovery(context.Background(), r, w, m.cfg, p, true, nil)
	require.NoError(t, err)
	require.Equal(t, "succeeded", res.DataStatus)
	require.Equal(t, r.rows, w.copied)
	require.Equal(t, []string{`ALTER TRIGGER "Dst"."Tr" DISABLE`, `TRUNCATE TABLE "Dst"."Mixed"`, `ALTER TRIGGER "Dst"."Tr" ENABLE`}, w.statements[before:])
}

func TestDamengNativeRecoveryRechecksProfileAndFrozenSourceColumns(t *testing.T) {
	m, r, w, o := nativeRecoveryFixture(t, true)
	w.snapshot.Profile.Settings["COMPATIBLE_MODE"] = "0"
	_, err := PreviewRecovery(context.Background(), r, w, m.cfg, o, nil, nil)
	require.ErrorContains(t, err, "语义参数")
}

func TestDamengNativeInventoryStatusesAndManagedObjects(t *testing.T) {
	m, r, _ := nativeTestMigrator(Config{})
	r.snapshot.Objects = append(r.snapshot.Objects, damengnative.Object{ID: "pk", Kind: "constraint", Name: "PK", Schema: "Src", Managed: true, ParentID: "source-table"})
	report := m.Run(context.Background())
	require.False(t, report.HasFailures())
	require.Equal(t, 1, report.Constraints.Success)
	for _, o := range report.NativeSchema.Inventory {
		require.Equal(t, "succeeded", o.Status)
	}
}

func TestNativeFoundationPromotesFunctionDependencyButNeverTriggers(t *testing.T) {
	objects := []damengnative.Object{{ID: "t", Kind: "table", Dependencies: []string{"f", "tr"}, Units: []damengnative.Unit{{SQL: "table", Phase: "before_data"}}}, {ID: "f", Kind: "function", Units: []damengnative.Unit{{SQL: "function", Phase: "after_data"}}}, {ID: "tr", Kind: "trigger", Units: []damengnative.Unit{{SQL: "trigger", Phase: "after_data"}}}}
	got := nativePromoteFoundationDependencies(objects)
	require.Equal(t, "before_data", got[1].Units[0].Phase)
	require.Equal(t, "after_data", got[2].Units[0].Phase)
}

func TestDamengNativeSameLogicalDatabaseOnDifferentNodesIsBlocked(t *testing.T) {
	m, r, w := nativeTestMigrator(Config{OverwriteExisting: true})
	r.snapshot.Profile.Database = "same-magic"
	w.snapshot.Profile.Database = "same-magic"
	w.snapshot.Profile.Schema = r.snapshot.Profile.Schema
	p := m.PreflightDamengNative(context.Background())
	require.False(t, p.Ready)
	require.Empty(t, w.statements)
}

func TestDamengNativeInvalidUDTValueIsBlockedBeforeOverwrite(t *testing.T) {
	m, r, w := nativeTestMigrator(Config{OverwriteExisting: true})
	r.snapshot.Tables[0].Columns[0] = damengnative.Column{Name: "Value", DataType: "CLASS123", SQLType: `"Src"."T"`, TypeSchema: "Src", TypeName: "T", Writable: true}
	w.snapshot.Objects = []damengnative.Object{{ID: "existing", Kind: "table", Schema: "Dst", Name: "Mixed", DropSQL: `DROP TABLE "Dst"."Mixed"`}}
	p := m.PreflightDamengNative(context.Background())
	require.False(t, p.Ready)
	require.Empty(t, w.statements)
	require.Empty(t, w.copied)
}

func TestDamengNativeFlushesOnByteBudget(t *testing.T) {
	m, r, w := nativeTestMigrator(Config{PageSize: 2000})
	r.rows = [][]any{{strings.Repeat("a", 5<<20)}, {strings.Repeat("中", 2<<20)}}
	r.snapshot.Tables[0].Columns[0].DataType = "CLOB"
	r.snapshot.Tables[0].Columns[0].SQLType = "CLOB"
	report := m.Run(context.Background())
	require.False(t, report.HasFailures())
	require.Equal(t, []int{1, 1}, w.batchSizes)
}

func TestDamengNativeInstallsTriggersAfterIdentityAndCompilation(t *testing.T) {
	m, r, w := nativeTestMigrator(Config{CaptureRepair: true})
	r.snapshot.Tables[0].Restore = []damengnative.Unit{{SQL: `ALTER TABLE "Src"."Mixed" DROP IDENTITY`, Phase: "restore"}, {SQL: `ALTER TABLE "Src"."Mixed" ADD COLUMN "Id" IDENTITY(8, 2)`, Phase: "restore"}}
	r.snapshot.Objects = append(r.snapshot.Objects, damengnative.Object{ID: "f", Schema: "Src", Kind: "function", Name: "f", Units: []damengnative.Unit{{SQL: `CREATE FUNCTION "Src"."f" RETURN INT AS BEGIN RETURN 1; END;`, Phase: "after_data"}}, CompileSQL: `ALTER FUNCTION "Src"."f" COMPILE`}, damengnative.Object{ID: "tr", Schema: "Src", Kind: "trigger", Name: "tr", Table: "Mixed", Units: []damengnative.Unit{{SQL: `CREATE TRIGGER "Src"."tr" BEFORE INSERT ON "Src"."Mixed" BEGIN NULL; END;`, Phase: "after_data"}, {SQL: `ALTER TRIGGER "Src"."tr" DISABLE`, Phase: "restore"}}})
	report := m.Run(context.Background())
	require.False(t, report.HasFailures())
	create, identity, compile := -1, -1, -1
	for i, sql := range w.statements {
		if strings.HasPrefix(sql, "CREATE TRIGGER") {
			create = i
		}
		if strings.Contains(sql, "ADD COLUMN") {
			identity = i
		}
		if strings.HasPrefix(sql, "ALTER FUNCTION") {
			compile = i
		}
	}
	require.Greater(t, create, identity)
	require.Greater(t, create, compile)
	require.Equal(t, `ALTER TRIGGER "Dst"."tr" DISABLE`, w.statements[len(w.statements)-1])
}
