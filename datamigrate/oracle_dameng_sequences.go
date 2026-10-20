package datamigrate

import (
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
)

// Target metadata is copied before adaptation. Log each original boundary once
// even when repair inventory preparation and execution both render the object.
func (m *Migrator) logOracleDamengSequenceBounds(original, adapted source.SequenceInfo) {
	if m.log == nil || original.Native == nil || adapted.Native == nil {
		return
	}
	for _, bound := range []struct{ field, source, target string }{
		{"MINVALUE", original.Native.MinValue, adapted.Native.MinValue},
		{"MAXVALUE", original.Native.MaxValue, adapted.Native.MaxValue},
	} {
		if bound.source == bound.target {
			continue
		}
		key := adapted.Native.Name + "\x00" + bound.field + "\x00" + bound.source + "\x00" + bound.target
		if m.dmSequenceBoundsLogged == nil {
			m.dmSequenceBoundsLogged = map[string]bool{}
		}
		if !m.dmSequenceBoundsLogged[key] {
			m.log.Warnf("序列 %s 的 Oracle %s 已适配达梦边界：%s → %s", adapted.Native.Name, bound.field, bound.source, bound.target)
			m.dmSequenceBoundsLogged[key] = true
		}
	}
}

func (m *Migrator) sequenceFailure(original, adapted source.SequenceInfo, ddl string, err error) ObjectResult {
	name := dialect.SequenceName(m.writer.Dialect(), adapted)
	result := objectResultForError(name, ddl, err)
	if !m.isOracleDameng() {
		return result
	}
	result.Stage = "execute"
	if target.IsOracleDamengSequencePreflight(err) {
		result.Stage = "preflight"
	}
	if adapted.Unsupported != "" {
		result.Stage = adapted.FailureStage
		if result.Stage == "" {
			result.Stage = "unsupported"
		}
	}
	result.Table, result.TargetTable = original.TableName, adapted.TableName
	result.TargetName, result.TargetSchema = name, m.cfg.TargetSchema
	if original.Native != nil {
		result.SourceName = original.Native.Name
		var definitions []string
		for _, binding := range original.Native.Bindings {
			if binding.TriggerBody != "" {
				definitions = append(definitions, "-- 源触发器 "+binding.TriggerName+"\n"+binding.TriggerBody)
			}
		}
		result.SourceDDL = strings.Join(definitions, "\n\n")
	}
	return result
}
