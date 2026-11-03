package datamigrate

import (
	"context"
	"errors"
	"io"
	"slices"
	"testing"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/datamigrate/validation"
	"github.com/stretchr/testify/require"
)

func damengFunctionFixtureReport(schema string) *target.CompatibilityFunctionsReport {
	r := &target.CompatibilityFunctionsReport{Total: 9, Success: 9, Reused: 9}
	for _, name := range []string{"GETCHILDOULIST", "GETITEMTEXT", "GETOUNAMEBYUSERGUID", "GETQUESTIONTEXT", "GETUSERNAMEBYUSERGUID", "SELECTALLUSERCOUNT", "FUN_GETPY"} {
		r.Items = append(r.Items, target.CompatibilityFunctionResult{Name: name, Schema: schema, Kind: "function", Status: "reused", DefinitionWarning: "已存在，跳过，未校验定义"})
	}
	for _, name := range []string{"EPOINT_PROC_ALTER", "GETCURRENTNUMBER"} {
		r.Items = append(r.Items, target.CompatibilityFunctionResult{Name: name, Schema: schema, Kind: "procedure", Status: "reused", DefinitionWarning: "已存在，跳过，未校验定义"})
	}
	return r
}

// Unrelated native fixtures model an already provisioned destination. Record
// the call so their event-order tests also detect SQL after event activation.
func (w *nativeTestWriter) EnsureMySQLDamengBusinessFunctions(context.Context, map[string]string) (*target.CompatibilityFunctionsReport, error) {
	w.lifecycle = append(w.lifecycle, "business_functions")
	return damengFunctionFixtureReport(w.EffectiveSchema()), nil
}

type functionSourceReader struct {
	*mockReader
	kind string
}

func (r *functionSourceReader) DBType() string { return r.kind }

type functionRunWriter struct {
	*oracleDMWriter
	calls     int
	onInstall func(map[string]string)
}

func (w *functionRunWriter) EnsureMySQLDamengBusinessFunctions(_ context.Context, blocked map[string]string) (*target.CompatibilityFunctionsReport, error) {
	w.calls++
	if w.onInstall != nil {
		w.onInstall(blocked)
	}
	return damengFunctionFixtureReport("APP"), nil
}

func TestDamengBusinessFunctionsAllCrossDatabaseSources(t *testing.T) {
	for _, kind := range []string{"mysql", "oracle", "sqlserver", "postgres", "gaussdb", "highgo", "vastbase", "gbase", "seabox", "kingbase"} {
		for _, content := range []string{"", "both", "schema_only"} {
			t.Run(kind+"/"+content, func(t *testing.T) {
				var m *Migrator
				var w *functionRunWriter
				if kind == "oracle" {
					_, base, migrator := oracleDMFixture(content)
					m, w = migrator, &functionRunWriter{oracleDMWriter: base}
					m.writer = w
				} else {
					r, base := dmFixture()
					var reader source.Reader = r
					if kind != "sqlserver" {
						reader = &functionSourceReader{mockReader: r.mockReader, kind: kind}
					}
					w = &functionRunWriter{oracleDMWriter: &oracleDMWriter{dmFixtureWriter: base}}
					m, _ = newTestMigrator(reader, w)
					m.cfg.Content, m.cfg.TargetSchema = content, "APP"
				}
				w.onInstall = func(map[string]string) { require.NotEmpty(t, w.created, "install follows table DDL") }
				report := m.Run(context.Background())
				require.Equal(t, 1, w.calls)
				require.Equal(t, 9, report.CompatibilityFunctions.Reused)
				require.False(t, report.HasFailures(), "%+v", report)
			})
		}
	}
}

type nativeFunctionWriter struct {
	*nativeTestWriter
	err       error
	onInstall func(map[string]string)
}

func (w *nativeFunctionWriter) EnsureMySQLDamengBusinessFunctions(ctx context.Context, blocked map[string]string) (*target.CompatibilityFunctionsReport, error) {
	if w.onInstall != nil {
		w.onInstall(blocked)
	}
	r, _ := w.nativeTestWriter.EnsureMySQLDamengBusinessFunctions(ctx, blocked)
	if w.err != nil {
		r.Failed, r.Reused, r.Success = 1, 8, 8
		for i := range r.Items {
			if r.Items[i].Name == "EPOINT_PROC_ALTER" {
				r.Items[i].Status, r.Items[i].Error = "failed", w.err.Error()
			}
		}
	}
	return r, w.err
}

func TestDamengBusinessFunctionsNativeBeforeEvents(t *testing.T) {
	for _, failure := range []error{nil, errors.New("function compile failed"), io.EOF} {
		m, r, base := nativeTestMigrator(Config{Mode: "all", CaptureRepair: true})
		addNativeTestEvent(r)
		w := &nativeFunctionWriter{nativeTestWriter: base, err: failure}
		m.writer = w
		report := m.Run(context.Background())
		require.Equal(t, 1, countFunctionCalls(w.lifecycle))
		require.Less(t, slices.Index(w.lifecycle, "verify"), slices.Index(w.lifecycle, "business_functions"))
		require.Len(t, report.NativeSchema.Inventory, 2, "business routines do not alter the source inventory")
		require.Equal(t, 9, report.CompatibilityFunctions.Total)
		if failure == nil {
			require.False(t, report.HasFailures(), "%+v", report)
			require.Less(t, slices.Index(w.lifecycle, "business_functions"), slices.Index(w.lifecycle, "final_session"))
			require.Equal(t, damengnative.EventTriggerStateSQL("Dst", "Hook", true), w.lifecycle[len(w.lifecycle)-1])
		} else {
			require.True(t, report.HasFailures())
			require.Equal(t, 1, report.Tables.Success, "completed table work is retained")
			require.NotContains(t, w.lifecycle, "final_session")
		}
	}
}

func countFunctionCalls(lifecycle []string) int {
	n := 0
	for _, item := range lifecycle {
		if item == "business_functions" {
			n++
		}
	}
	return n
}

func TestDamengBusinessFunctionsNativeFailureSkipsAndResets(t *testing.T) {
	for _, stage := range []string{"preflight", "cancel", "persistence", "connection", "cdc"} {
		t.Run(stage, func(t *testing.T) {
			m, r, w := nativeTestMigrator(Config{Mode: "all", CaptureRepair: true})
			first := m.Run(context.Background())
			require.NotNil(t, first.CompatibilityFunctions)
			require.True(t, m.mainMigrationFinished)
			w.lifecycle = nil
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch stage {
			case "preflight":
				r.snapshot.Issues = []damengnative.Issue{{Severity: "error", Reason: "metadata denied"}}
			case "cancel":
				cancel()
			case "persistence":
				m.cfg.RepairSink = func(RepairManifest) error { return errors.New("disk full") }
			case "connection":
				w.executeErrors = map[string]error{`CREATE TABLE "Dst"."Mixed" ("Value" VARCHAR(100))`: io.EOF}
			case "cdc":
				m.cfg.MigrationPurpose = "cdc"
			}
			report := m.Run(ctx)
			require.NotContains(t, w.lifecycle, "business_functions")
			require.Nil(t, report.CompatibilityFunctions)
			require.Nil(t, m.CompatibilityFunctions(), "old report must not leak from the previous run")
		})
	}
}

func TestDamengBusinessFunctionsNativeBlockedNamesAreExact(t *testing.T) {
	m, _, base := nativeTestMigrator(Config{Mode: "all"})
	w := &nativeFunctionWriter{nativeTestWriter: base}
	m.writer, m.mainMigrationFinished = w, true
	m.blockMigrationTable("source_alias", "create", errors.New("mixed-case table failed"))
	m.blockMigrationTable("FRAME_USER", "data", errors.New("uppercase table failed"))
	report := newMigrationReport()
	report.NativeSchema = &NativeSchemaReport{Preflight: &damengnative.Preflight{Snapshot: &damengnative.Snapshot{Tables: []damengnative.Table{
		{Name: "Frame_Ou", SourceName: "source_alias"}, {Name: "FRAME_USER"},
	}}}}
	w.onInstall = func(blocked map[string]string) {
		require.Equal(t, "mixed-case table failed", blocked["Frame_Ou"])
		require.Equal(t, "uppercase table failed", blocked["FRAME_USER"])
		require.NotContains(t, blocked, "FRAME_OU", "distinct uppercase dependency must remain independent")
		require.NotContains(t, blocked, "source_alias")
	}
	m.finishDamengBusinessFunctions(context.Background(), &report)
	require.Equal(t, 1, countFunctionCalls(w.lifecycle))
}

func TestDamengBusinessFunctionsNativeModes(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want int
	}{
		{"default", Config{Mode: "all"}, 1},
		{"both", Config{Mode: "all", Content: "both"}, 1},
		// Native migration currently requires a whole schema and its data;
		// the function installer must not bypass that existing preflight.
		{"schema_preflight_rejected", Config{Mode: "all", Content: "schema_only"}, 0},
		{"data", Config{Mode: "all", Content: "data_only"}, 0},
		{"objects", Config{Mode: "all", Objects: []string{"functions"}}, 0},
		{"views", Config{Mode: "all", ViewNames: []string{"V"}}, 0},
		{"cdc", Config{Mode: "all", MigrationPurpose: "cdc"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _, w := nativeTestMigrator(tc.cfg)
			report := m.Run(context.Background())
			require.Equal(t, tc.want, countFunctionCalls(w.lifecycle))
			if tc.want == 0 {
				require.Nil(t, report.CompatibilityFunctions)
			}
		})
	}
}

func TestDamengBusinessFunctionsNativeContinuesAfterTableFailure(t *testing.T) {
	m, _, base := nativeTestMigrator(Config{Mode: "all", CaptureRepair: true})
	base.executeErrors = map[string]error{`CREATE TABLE "Dst"."Mixed" ("Value" VARCHAR(100))`: errors.New("create denied")}
	w := &nativeFunctionWriter{nativeTestWriter: base, onInstall: func(blocked map[string]string) {
		require.Contains(t, blocked, "Mixed")
		require.NotContains(t, blocked, "MIXED")
	}}
	m.writer = w
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.Equal(t, 1, countFunctionCalls(w.lifecycle))
	require.Equal(t, 9, report.CompatibilityFunctions.Reused)
}

type nativeFunctionValidationReader struct {
	*nativeTestReader
	*pathValidationScanner
}

type nativeFunctionValidationWriter struct {
	*nativeFunctionWriter
	*pathValidationScanner
}

func TestDamengBusinessFunctionsNativeAfterContentValidation(t *testing.T) {
	m, r, base := nativeTestMigrator(Config{Mode: "all", CaptureRepair: true, ValidateData: true})
	addNativeTestEvent(r)
	r.rows = [][]any{{int64(1)}, {int64(2)}}
	src := pathScanner([]string{"Mixed"}, "Value", r.rows)
	dst := pathScanner([]string{"Mixed"}, "Value", r.rows)
	w := &nativeFunctionWriter{nativeTestWriter: base, onInstall: func(map[string]string) {
		require.Contains(t, src.scanned, "Mixed", "source content has been validated")
		require.Contains(t, dst.scanned, "Mixed", "target content has been validated")
		require.NotContains(t, base.lifecycle, "final_session")
	}}
	m.reader = &nativeFunctionValidationReader{r, src}
	m.writer = &nativeFunctionValidationWriter{w, dst}
	report := m.Run(context.Background())
	require.Equal(t, 1, countFunctionCalls(base.lifecycle))
	require.Equal(t, validation.StatusPassed, report.DataValidation.Status)
	require.False(t, report.HasFailures(), "%+v", report)
	require.Equal(t, damengnative.EventTriggerStateSQL("Dst", "Hook", true), base.lifecycle[len(base.lifecycle)-1])
}

func TestDamengBusinessRoutinesFollowNativeCopies(t *testing.T) {
	m, r, base := nativeTestMigrator(Config{Mode: "all", CaptureRepair: true})
	r.snapshot.Objects = append(r.snapshot.Objects, damengnative.Object{
		ID: "source-function", Kind: "function", Schema: "Src", Name: "FUN_GETPY",
		Units: []damengnative.Unit{{SQL: `CREATE FUNCTION "Src"."FUN_GETPY" RETURN INT AS BEGIN RETURN 197; END;`, Phase: "after_data"}},
	})
	r.snapshot.Objects = append(r.snapshot.Objects, damengnative.Object{
		ID: "source-procedure", Kind: "procedure", Schema: "Src", Name: "EPOINT_PROC_ALTER",
		Units: []damengnative.Unit{{SQL: `CREATE PROCEDURE "Src"."EPOINT_PROC_ALTER" AS BEGIN NULL; END;`, Phase: "after_data"}},
	})
	m.writer = &nativeFunctionWriter{nativeTestWriter: base, onInstall: func(map[string]string) {
		require.Contains(t, base.statements, `CREATE FUNCTION "Dst"."FUN_GETPY" RETURN INT AS BEGIN RETURN 197; END;`)
		require.Contains(t, base.statements, `CREATE PROCEDURE "Dst"."EPOINT_PROC_ALTER" AS BEGIN NULL; END;`)
	}}
	report := m.Run(context.Background())
	require.False(t, report.HasFailures(), "%+v", report)
	require.Equal(t, 1, countFunctionCalls(base.lifecycle))
	require.Equal(t, 9, report.CompatibilityFunctions.Reused)
	require.Len(t, report.NativeSchema.Inventory, 3)
	var functions, procedures int
	for _, category := range report.NativeSchema.Categories {
		if category.Kind == "function" {
			functions += category.Success
		}
		if category.Kind == "procedure" {
			procedures += category.Success
		}
	}
	require.Equal(t, 1, functions, "native function copies retain their own statistics")
	require.Equal(t, 1, procedures, "native procedure copies retain their own statistics")
}
