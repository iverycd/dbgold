package damengnative

import "fmt"

// RewriteMaterializedHistory maps identifiers in a frozen recipe without
// changing source timestamps, object identities, literals or source buffers.
func RewriteMaterializedHistory(original *MaterializedHistoryPlan, source, target string) (*MaterializedHistoryPlan, error) {
	if original == nil || original.Schema != source || source == "" || target == "" {
		return nil, fmt.Errorf("物化历史恢复计划不属于已冻结源模式")
	}
	plan := *original
	plan.Schema = target
	plan.Views = append([]MaterializedHistoryView{}, original.Views...)
	plan.Logs = append([]MaterializedHistoryLog{}, original.Logs...)
	plan.RowIDMaps = append([]MaterializedRowIDMap{}, original.RowIDMaps...)
	columns := func(input []Column) ([]Column, error) {
		out := append([]Column{}, input...)
		for i := range out {
			var err error
			out[i].SQLType, _, err = RewriteSchema(out[i].SQLType, source, target)
			if err != nil {
				return nil, err
			}
			if out[i].TypeSchema == source {
				out[i].TypeSchema = target
			}
		}
		return out, nil
	}
	buffer := func(input MaterializedExchangeBuffer) (MaterializedExchangeBuffer, error) {
		out := input
		var err error
		out.CreateSQL, _, err = RewriteSchema(input.CreateSQL, source, target)
		if err != nil {
			return out, err
		}
		out.Indexes = append([]string{}, input.Indexes...)
		for i := range out.Indexes {
			out.Indexes[i], _, err = RewriteSchema(out.Indexes[i], source, target)
			if err != nil {
				return out, err
			}
		}
		return out, nil
	}
	for i := range plan.Views {
		view := &plan.Views[i]
		var err error
		if view.EmptyTouch != nil {
			empty := *view.EmptyTouch
			empty.DefinitionSQL, _, err = RewriteSchema(empty.DefinitionSQL, source, target)
			if err != nil {
				return nil, err
			}
			empty.Columns = append([]MaterializedEmptyColumn{}, empty.Columns...)
			for j := range empty.Columns {
				empty.Columns[j].DefaultSQL, _, err = RewriteSchema(empty.Columns[j].DefaultSQL, source, target)
				if err != nil {
					return nil, err
				}
				empty.Columns[j].ValueSQL, _, err = RewriteSchema(empty.Columns[j].ValueSQL, source, target)
				if err != nil {
					return nil, err
				}
			}
			empty.Constraints = append([]MaterializedEmptyConstraint{}, empty.Constraints...)
			for j := range empty.Constraints {
				empty.Constraints[j].Columns = append([]string{}, empty.Constraints[j].Columns...)
				empty.Constraints[j].DefinitionSQL, _, err = RewriteSchema(empty.Constraints[j].DefinitionSQL, source, target)
				if err != nil {
					return nil, err
				}
			}
			empty.IdentityRestore = append([]string{}, empty.IdentityRestore...)
			for j := range empty.IdentityRestore {
				empty.IdentityRestore[j], _, err = RewriteSchema(empty.IdentityRestore[j], source, target)
				if err != nil {
					return nil, err
				}
			}
			view.EmptyTouch = &empty
		}
		if view.Redefinition != nil {
			redefinition := *view.Redefinition
			redefinition.CreateSQL, _, err = RewriteSchema(redefinition.CreateSQL, source, target)
			if err != nil {
				return nil, err
			}
			view.Redefinition = &redefinition
		}
		view.Query, _, err = RewriteSchema(view.Query, source, target)
		if err != nil {
			return nil, err
		}
		view.TouchSQL = append([]string{}, view.TouchSQL...)
		for j := range view.TouchSQL {
			view.TouchSQL[j], _, err = RewriteSchema(view.TouchSQL[j], source, target)
			if err != nil {
				return nil, err
			}
		}
		view.Columns, err = columns(view.Columns)
		if err != nil {
			return nil, err
		}
		view.RowIDColumns = append([]int{}, view.RowIDColumns...)
		view.RowIDTables = append([]string{}, view.RowIDTables...)
		view.Buffer, err = buffer(view.Buffer)
		if err != nil {
			return nil, err
		}
		view.Buffers = append([]MaterializedExchangeBuffer{}, view.Buffers...)
		for j := range view.Buffers {
			view.Buffers[j], err = buffer(view.Buffers[j])
			if err != nil {
				return nil, err
			}
		}
	}
	for i := range plan.Logs {
		log := &plan.Logs[i]
		var err error
		log.Columns, err = columns(log.Columns)
		if err != nil {
			return nil, err
		}
		log.Buffer, err = buffer(log.Buffer)
		if err != nil {
			return nil, err
		}
	}
	return &plan, nil
}
