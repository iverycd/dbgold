package datamigrate

import (
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
)

type oracleDamengTriggerRecord struct {
	object    repair.Object
	canonical string
}

// BuildOracleDamengTriggerAdaptation derives original migration outcomes only
// from the frozen report. It never inspects live objects or the repair ledger.
func BuildOracleDamengTriggerAdaptation(report MigrationReport) TriggerAdaptationSummary {
	manifest := report.RepairManifest
	if manifest == nil || manifest.Version != 1 || manifest.Objects == nil || report.Sequences.TotalUnknown {
		return TriggerAdaptationSummary{}
	}
	records := map[string][]oracleDamengTriggerRecord{}
	dia := dialect.NewDaMeng()
	sequenceInventory := false
	for _, object := range manifest.Objects {
		kind := strings.ToLower(strings.TrimSpace(object.Kind))
		if kind != "sequence" && kind != "sequences" {
			continue
		}
		if object.Sequence == nil || object.Sequence.Native == nil || len(object.Sequence.Native.Bindings) == 0 {
			return TriggerAdaptationSummary{}
		}
		sequenceInventory = true
		for _, binding := range object.Sequence.Native.Bindings {
			if binding.Kind == "default" && binding.TriggerName == "" {
				continue // An unsupported NEXTVAL column default is not a trigger.
			}
			if binding.Kind != "trigger" && binding.Kind != "identity" && binding.TriggerName == "" {
				return TriggerAdaptationSummary{} // A legacy binding lost its kind/identity.
			}
			schema := object.Schema
			if schema == "" {
				schema = manifest.Config.TargetSchema
			}
			key := dia.QualifyTable(schema, dialect.OracleDamengTriggerName(*object.Sequence, binding))
			records[key] = append(records[key], oracleDamengTriggerRecord{
				object:    object,
				canonical: oracleDamengRecordedTriggerSQL(schema, *object.Sequence, binding),
			})
		}
	}
	if !sequenceInventory && report.Sequences.Total > 0 {
		return TriggerAdaptationSummary{}
	}
	summary := TriggerAdaptationSummary{Recorded: true, Total: len(records)}
	for _, claims := range records {
		status := "unknown"
		// A target name shared by multiple binding claims is ambiguous even if
		// one of their SQL units succeeded. Count the target identity only once.
		if len(claims) == 1 {
			status = oracleDamengRecordedTriggerStatus(claims[0])
		}
		switch status {
		case "succeeded":
			summary.Success++
		case "failed":
			summary.Failed++
		case "blocked":
			summary.Blocked++
		case "pending":
			summary.Pending++
		default:
			summary.Unknown++
		}
	}
	return summary
}

func oracleDamengRecordedTriggerSQL(schema string, original source.SequenceInfo, binding source.SequenceBinding) string {
	if schema == "" {
		return ""
	}
	seq, native := original, *original.Native
	// A stale parent error must not hide an exact successful child unit. Keep
	// real numeric metadata and render just this binding, without mutating it.
	seq.Unsupported, seq.FailureStage = "", ""
	native.Bindings = []source.SequenceBinding{binding}
	seq.Native = &native
	statements := dialect.NewDaMeng().SequenceStatements(schema, seq, "")
	if len(statements) != 2 {
		return ""
	}
	return statements[1].SQL
}

func oracleDamengRecordedTriggerStatus(record oracleDamengTriggerRecord) string {
	matched := -1
	if record.canonical != "" {
		for i, unit := range record.object.Units {
			if unit.SQL == record.canonical {
				if matched >= 0 {
					return "unknown"
				}
				matched = i
			}
		}
	}
	blocked := oracleDamengRecordedTriggerBlocked(record.object)
	if matched >= 0 {
		unit := record.object.Units[matched]
		switch unit.Status {
		case "succeeded", "reused":
			if unit.Error == "" {
				return "succeeded"
			}
		case "failed", "rolled_back":
			return "failed"
		case "blocked":
			return "blocked"
		case "pending":
			if blocked {
				return "blocked"
			}
			return "pending"
		}
		return "unknown"
	}
	for _, unit := range record.object.Units {
		// Edited or legacy executed SQL cannot be attributed to a binding from
		// its name alone. In particular, never call an uncertain CREATE blocked.
		if record.canonical != "" && unit.OriginalSQL == record.canonical && unit.SQL != record.canonical {
			return "unknown"
		}
		if unit.Status != "pending" && unit.Status != "blocked" {
			return "unknown"
		}
	}
	if oracleDamengRecordedTriggerPreventsExecution(record.object) {
		return "blocked"
	}
	return "unknown"
}

func oracleDamengRecordedTriggerBlocked(object repair.Object) bool {
	return oracleDamengRecordedTriggerPreventsExecution(object) || object.Status == "failed"
}

func oracleDamengRecordedTriggerPreventsExecution(object repair.Object) bool {
	if object.Sequence.Unsupported != "" {
		return true
	}
	switch object.Sequence.FailureStage {
	case "metadata", "preflight", "unsupported", "dependency":
		return true
	}
	return object.Status == "blocked"
}
