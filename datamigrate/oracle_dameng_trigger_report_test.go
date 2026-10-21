package datamigrate

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func triggerReportSequence(name string, count int) source.SequenceInfo {
	seq := source.SequenceInfo{StartValueText: "1", Native: &source.NativeSequenceInfo{Name: name, IncrementBy: "1", MinValue: "1", MaxValue: "9223372036854775807", CacheSize: "0"}}
	for i := 0; i < count; i++ {
		seq.Native.Bindings = append(seq.Native.Bindings, source.SequenceBinding{TableName: fmt.Sprintf("T_%s_%d", name, i), ColumnName: "ID", Kind: "trigger", TriggerName: fmt.Sprintf("TRG_%s_%d", name, i), OnlyWhenNull: true})
	}
	return seq
}

func triggerReportObject(seq source.SequenceInfo, statuses []string, parentStatus string) repair.Object {
	object := repair.Object{Kind: "sequence", Schema: "APP", Sequence: &seq, Status: parentStatus}
	statements := dialect.NewDaMeng().SequenceStatements(object.Schema, seq, "")
	for i, status := range statuses {
		statement := statements[i+1]
		object.Units = append(object.Units, repair.Unit{ID: fmt.Sprintf("step-%d", i), SQL: statement.SQL, OriginalSQL: statement.SQL, Status: status})
	}
	return object
}

func triggerReport(objects ...repair.Object) MigrationReport {
	return MigrationReport{RepairManifest: &RepairManifest{Version: 1, Objects: objects}}
}

func TestOracleDamengTriggerReportCurrent58Success4MetadataBlocked(t *testing.T) {
	seq := triggerReportSequence("HEALTHY", 58)
	statuses := make([]string, 58)
	for i := range statuses {
		statuses[i] = "succeeded"
	}
	healthy := triggerReportObject(seq, statuses, "succeeded")
	missing := triggerReportSequence("MISSING", 4)
	missing.Unsupported, missing.FailureStage = "source sequence not visible", "metadata"
	missing.Native.IncrementBy, missing.Native.MinValue, missing.Native.MaxValue, missing.Native.CacheSize = "", "", "", ""
	report := triggerReport(healthy, repair.Object{Kind: "sequence", Schema: "APP", Sequence: &missing, Status: "failed"})
	report.Triggers.Total = 77
	report.Sequences.Total, report.Sequences.Success, report.Sequences.Failed = 34, 30, 4
	before, err := json.Marshal(report)
	require.NoError(t, err)
	summary := BuildOracleDamengTriggerAdaptation(report)
	require.Equal(t, TriggerAdaptationSummary{Recorded: true, Total: 62, Success: 58, Blocked: 4}, summary)
	after, err := json.Marshal(report)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after), "derived reporting must not mutate frozen categories or manifest")
	require.Equal(t, 77, report.Triggers.Total)
	require.Zero(t, report.Triggers.Failed)
	require.Equal(t, 4, report.Sequences.Failed)
}

func TestOracleDamengTriggerReportPreservesPartialSharedSequenceOutcomes(t *testing.T) {
	seq := triggerReportSequence("SHARED", 4)
	object := triggerReportObject(seq, []string{"succeeded", "failed", "pending", "unknown"}, "failed")
	summary := BuildOracleDamengTriggerAdaptation(triggerReport(object))
	require.Equal(t, TriggerAdaptationSummary{Recorded: true, Total: 4, Success: 1, Failed: 1, Blocked: 1, Unknown: 1}, summary)
}

func TestOracleDamengTriggerReportUsesRealNamesAndIdentityHash(t *testing.T) {
	seq := triggerReportSequence("SeqMiXeD", 2)
	seq.Native.Bindings[0].TableName = strings.Repeat("表", 30)
	seq.Native.Bindings[0].ColumnName = strings.Repeat("C", 50)
	seq.Native.Bindings[0].Kind, seq.Native.Bindings[0].TriggerName = "identity", ""
	seq.Native.Bindings[1].TriggerName = "QuotedMiXeDTrigger"
	seq.Native.Bindings[1].TableName = "MappedTable"
	seq.Native.Bindings[1].ColumnName = "MappedColumn"
	seq.Native.Bindings[1].SourceColumnName = "OriginalColumn"
	name := dialect.OracleDamengTriggerName(seq, seq.Native.Bindings[0])
	require.True(t, strings.HasPrefix(name, "DBGOLD_TRG_"))
	require.Less(t, len(name), 128)
	object := triggerReportObject(seq, []string{"succeeded", "succeeded"}, "succeeded")
	require.Contains(t, object.Units[0].SQL, `"`+name+`"`)
	require.Equal(t, TriggerAdaptationSummary{Recorded: true, Total: 2, Success: 2}, BuildOracleDamengTriggerAdaptation(triggerReport(object)))
}

func TestOracleDamengTriggerReportDoesNotInferSuccessFromParent(t *testing.T) {
	for _, tc := range []struct {
		name, unitStatus, parent string
		change                   func(*repair.Object)
		want                     TriggerAdaptationSummary
	}{
		{name: "missing old unit", parent: "succeeded", want: TriggerAdaptationSummary{Recorded: true, Total: 1, Unknown: 1}},
		{name: "missing failed parent history", parent: "failed", want: TriggerAdaptationSummary{Recorded: true, Total: 1, Unknown: 1}},
		{name: "pending", unitStatus: "pending", parent: "pending", want: TriggerAdaptationSummary{Recorded: true, Total: 1, Pending: 1}},
		{name: "pending after terminal parent", unitStatus: "pending", parent: "failed", want: TriggerAdaptationSummary{Recorded: true, Total: 1, Blocked: 1}},
		{name: "pending after user cancellation", unitStatus: "pending", parent: "cancelled", want: TriggerAdaptationSummary{Recorded: true, Total: 1, Pending: 1}},
		{name: "pending after alternate cancellation", unitStatus: "pending", parent: "canceled", want: TriggerAdaptationSummary{Recorded: true, Total: 1, Pending: 1}},
		{name: "verified reuse", unitStatus: "reused", parent: "succeeded", want: TriggerAdaptationSummary{Recorded: true, Total: 1, Success: 1}},
		{name: "unknown execution", unitStatus: "unknown", parent: "failed", want: TriggerAdaptationSummary{Recorded: true, Total: 1, Unknown: 1}},
		{name: "running execution", unitStatus: "running", parent: "failed", want: TriggerAdaptationSummary{Recorded: true, Total: 1, Unknown: 1}},
		{name: "empty old status", unitStatus: "empty", parent: "succeeded", change: func(o *repair.Object) { o.Units[0].Status = "" }, want: TriggerAdaptationSummary{Recorded: true, Total: 1, Unknown: 1}},
		{name: "success with conflicting error", unitStatus: "succeeded", parent: "succeeded", change: func(o *repair.Object) { o.Units[0].Error = "inconsistent saved result" }, want: TriggerAdaptationSummary{Recorded: true, Total: 1, Unknown: 1}},
		{name: "blocked unit", unitStatus: "blocked", parent: "blocked", want: TriggerAdaptationSummary{Recorded: true, Total: 1, Blocked: 1}},
		{name: "rolled back", unitStatus: "rolled_back", parent: "failed", want: TriggerAdaptationSummary{Recorded: true, Total: 1, Failed: 1}},
		{name: "duplicate canonical units", unitStatus: "succeeded", parent: "succeeded", change: func(o *repair.Object) { o.Units = append(o.Units, o.Units[0]) }, want: TriggerAdaptationSummary{Recorded: true, Total: 1, Unknown: 1}},
		{name: "stale parent unsupported", unitStatus: "succeeded", parent: "failed", change: func(o *repair.Object) { o.Sequence.Unsupported = "stale parent error" }, want: TriggerAdaptationSummary{Recorded: true, Total: 1, Success: 1}},
		{name: "manual SQL only matches original", unitStatus: "succeeded", parent: "failed", change: func(o *repair.Object) { o.Sequence.Unsupported = "stale error"; o.Units[0].SQL += "\n-- edited" }, want: TriggerAdaptationSummary{Recorded: true, Total: 1, Unknown: 1}},
		{name: "edited pending unit", unitStatus: "pending", parent: "failed", change: func(o *repair.Object) { o.Sequence.Unsupported = "stale error"; o.Units[0].SQL += "\n-- edited" }, want: TriggerAdaptationSummary{Recorded: true, Total: 1, Unknown: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var statuses []string
			if tc.unitStatus != "" {
				statuses = []string{tc.unitStatus}
			}
			object := triggerReportObject(triggerReportSequence("S", 1), statuses, tc.parent)
			if tc.change != nil {
				tc.change(&object)
			}
			require.Equal(t, tc.want, BuildOracleDamengTriggerAdaptation(triggerReport(object)))
		})
	}
}

func TestOracleDamengTriggerReportInvalidPreflightNoDDLIsBlocked(t *testing.T) {
	seq := triggerReportSequence("S", 1)
	seq.Native.IncrementBy = "0"
	seq.Unsupported, seq.FailureStage = "invalid sequence increment", "preflight"
	object := repair.Object{Kind: "sequence", Schema: "APP", Sequence: &seq, Status: "failed"}
	require.Equal(t, TriggerAdaptationSummary{Recorded: true, Total: 1, Blocked: 1}, BuildOracleDamengTriggerAdaptation(triggerReport(object)))
	for _, status := range []string{"succeeded", "running", "unknown"} {
		object.Units = []repair.Unit{{SQL: `CREATE TRIGGER "APP"."TRG_S_0" unexpected saved text`, Status: status}}
		require.Equal(t, TriggerAdaptationSummary{Recorded: true, Total: 1, Unknown: 1}, BuildOracleDamengTriggerAdaptation(triggerReport(object)))
	}
}

func TestOracleDamengTriggerReportTargetNameConflictIsCountedOnceUnknown(t *testing.T) {
	first, second := triggerReportSequence("FIRST", 1), triggerReportSequence("SECOND", 1)
	first.Native.Bindings[0].TriggerName, second.Native.Bindings[0].TriggerName = "Collision", "COLLISION"
	report := triggerReport(triggerReportObject(first, []string{"succeeded"}, "succeeded"), triggerReportObject(second, []string{"succeeded"}, "succeeded"))
	require.Equal(t, TriggerAdaptationSummary{Recorded: true, Total: 1, Unknown: 1}, BuildOracleDamengTriggerAdaptation(report))
	// Schema aliases follow the same target normalization as generated SQL.
	report.RepairManifest.Objects[1].Schema = "app"
	require.Equal(t, TriggerAdaptationSummary{Recorded: true, Total: 1, Unknown: 1}, BuildOracleDamengTriggerAdaptation(report))
	report.RepairManifest.Objects[1].Schema = "DIFFERENT_APP"
	for i := range report.RepairManifest.Objects[1].Units {
		unit := &report.RepairManifest.Objects[1].Units[i]
		unit.SQL = strings.ReplaceAll(unit.SQL, `"APP".`, `"DIFFERENT_APP".`)
		unit.OriginalSQL = unit.SQL
	}
	require.Equal(t, TriggerAdaptationSummary{Recorded: true, Total: 2, Success: 2}, BuildOracleDamengTriggerAdaptation(report))
}

func TestOracleDamengTriggerReportInventoryReliability(t *testing.T) {
	for _, tc := range []struct {
		name   string
		report MigrationReport
	}{
		{"missing manifest", MigrationReport{}},
		{"unknown manifest version", MigrationReport{RepairManifest: &RepairManifest{Version: 2, Objects: []repair.Object{}}}},
		{"missing object inventory", MigrationReport{RepairManifest: &RepairManifest{Version: 1}}},
		{"missing native metadata", triggerReport(repair.Object{Kind: "sequence", Sequence: &source.SequenceInfo{}})},
		{"missing binding inventory", triggerReport(repair.Object{Kind: "sequence", Sequence: &source.SequenceInfo{Native: &source.NativeSequenceInfo{Name: "S"}}})},
		{"global sequence metadata failure", MigrationReport{RepairManifest: &RepairManifest{Version: 1, Objects: []repair.Object{}}, Sequences: CategoryReport{TotalUnknown: true}}},
		{"legacy nonzero category with missing inventory", MigrationReport{RepairManifest: &RepairManifest{Version: 1, Objects: []repair.Object{}}, Sequences: CategoryReport{Total: 3}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, TriggerAdaptationSummary{}, BuildOracleDamengTriggerAdaptation(tc.report))
		})
	}
	require.Equal(t, TriggerAdaptationSummary{Recorded: true}, BuildOracleDamengTriggerAdaptation(MigrationReport{RepairManifest: &RepairManifest{Version: 1, Objects: []repair.Object{}}}))
	report := triggerReport(triggerReportObject(triggerReportSequence("S", 1), nil, "pending"))
	report.Sequences.TotalUnknown = true
	require.Equal(t, TriggerAdaptationSummary{}, BuildOracleDamengTriggerAdaptation(report), "partially saved objects cannot prove a complete inventory after a global metadata failure")
}

func TestOracleDamengTriggerReportExcludesDefaultsAndDoesNotChangeHasFailures(t *testing.T) {
	seq := triggerReportSequence("S", 1)
	seq.Native.Bindings[0].Kind, seq.Native.Bindings[0].TriggerName = "default", ""
	report := triggerReport(repair.Object{Kind: "sequence", Schema: "APP", Sequence: &seq})
	require.Equal(t, TriggerAdaptationSummary{Recorded: true}, BuildOracleDamengTriggerAdaptation(report))
	report.TriggerAdaptation = &TriggerAdaptationSummary{Recorded: true, Total: 5, Failed: 5}
	require.False(t, report.HasFailures(), "informational trigger outcomes must not duplicate sequence failures")
}
