package datamigrate

import (
	"context"
	"errors"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
)

type preparedViewCandidate struct {
	view source.ViewInfo
	err  error
}

// The repair inventory and automatic creation share the prepared definition,
// including a preparation failure that left only a source definition for editing.
func (m *Migrator) preparedMigrationView(ctx context.Context, v source.ViewInfo) (source.ViewInfo, error) {
	if m.repairs != nil {
		if candidate, ok := m.repairs.preparedViews[v.ViewName]; ok {
			return candidate.view, candidate.err
		}
	}
	if strings.TrimSpace(v.Definition) == "" || v.Unsupported != "" {
		reason := v.Unsupported
		if reason == "" {
			reason = "源视图定义为空"
		}
		return v, errors.New(reason)
	}
	return m.prepareView(ctx, v)
}

func (m *Migrator) viewRepairCandidate(ctx context.Context, v source.ViewInfo) (repair.Object, []dialect.Statement) {
	name := m.objName(v.ViewName)
	o := repair.Object{ID: repairID("view", "", name), Kind: "view", Name: name, SourceName: v.ViewName, TargetName: name, Schema: m.cfg.TargetSchema, SourceDDL: v.Definition}
	prepared, err := m.preparedMigrationView(ctx, v)
	if m.repairs != nil {
		if m.repairs.preparedViews == nil {
			m.repairs.preparedViews = make(map[string]preparedViewCandidate)
		}
		m.repairs.preparedViews[v.ViewName] = preparedViewCandidate{prepared, err}
	}
	if err != nil {
		o.Error = err.Error()
		var issue *sqlServerPGViewError
		if errors.As(err, &issue) && issue.suggested != "" {
			o.SuggestedDDL = m.sqlServerViewCandidate(name, issue.suggested)
		}
		return o, nil
	}
	return o, m.writer.Dialect().ViewStatements(m.cfg.TargetSchema, prepared)
}
