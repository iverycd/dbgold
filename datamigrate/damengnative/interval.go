package damengnative

import "fmt"

// IntervalPartitionPlan retains existing automatic partitions omitted by DM's
// TABLE metadata, including empty partitions separated by unused intervals.
// The temporary row recipe is executed only against an empty target table.
type IntervalPartitionPlan struct {
	Column        string                  `json:"column"`
	DefinitionSQL string                  `json:"definition_sql"`
	EmptyTouch    *MaterializedEmptyTouch `json:"empty_touch"`
	Partitions    []IntervalPartition     `json:"partitions"`
}

type IntervalPartition struct {
	Name                   string              `json:"name"`
	HighSQL                string              `json:"high_sql"`
	SeedSQL                string              `json:"seed_sql"`
	Tablespace             string              `json:"tablespace,omitempty"`
	DefinitionSQL          string              `json:"definition_sql,omitempty"`
	DerivedConstraintNames []string            `json:"derived_constraint_names,omitempty"`
	Values                 []IntervalSeedValue `json:"values,omitempty"`
}

type IntervalSeedValue struct {
	Column string `json:"column"`
	SQL    string `json:"sql"`
}

// This scope-bound marker is deliberately not executable ALTER syntax. Only
// the typed interval executor may interpret its authenticated frozen recipe.
func IntervalPartitionSQL(schema, table string, plan *IntervalPartitionPlan) string {
	return "ALTER TABLE " + Qualified(schema, table) + " /* DBGOLD_INTERVAL " + Fingerprint(plan) + " */"
}

func RewriteIntervalPartitions(original *IntervalPartitionPlan, source, target string) (*IntervalPartitionPlan, error) {
	if original == nil || source == "" || target == "" {
		return nil, fmt.Errorf("缺少冻结的间隔分区模式映射")
	}
	plan := *original
	var err error
	plan.DefinitionSQL, _, err = RewriteSchema(original.DefinitionSQL, source, target)
	if err != nil {
		return nil, err
	}
	plan.EmptyTouch, err = RewriteEmptyTouch(original.EmptyTouch, source, target)
	if err != nil {
		return nil, err
	}
	plan.Partitions = append([]IntervalPartition{}, original.Partitions...)
	for i := range plan.Partitions {
		part := &plan.Partitions[i]
		part.DerivedConstraintNames = append([]string{}, original.Partitions[i].DerivedConstraintNames...)
		part.Values = append([]IntervalSeedValue{}, original.Partitions[i].Values...)
		for j := range part.Values {
			part.Values[j].SQL, _, err = RewriteSchema(part.Values[j].SQL, source, target)
			if err != nil {
				return nil, err
			}
		}
		for _, text := range []*string{&part.HighSQL, &part.SeedSQL, &part.DefinitionSQL} {
			*text, _, err = RewriteSchema(*text, source, target)
			if err != nil {
				return nil, err
			}
		}
	}
	return &plan, nil
}

// RewriteEmptyTouch clones the immutable recipe before mapping SQL identifiers.
// Defaults and routing values retain literal content and original case.
func RewriteEmptyTouch(original *MaterializedEmptyTouch, source, target string) (*MaterializedEmptyTouch, error) {
	if original == nil {
		return nil, fmt.Errorf("缺少冻结的临时行恢复定义")
	}
	empty := *original
	var err error
	empty.DefinitionSQL, _, err = RewriteSchema(empty.DefinitionSQL, source, target)
	if err != nil {
		return nil, err
	}
	empty.Columns = append([]MaterializedEmptyColumn{}, original.Columns...)
	for i := range empty.Columns {
		for _, text := range []*string{&empty.Columns[i].DefaultSQL, &empty.Columns[i].ValueSQL} {
			*text, _, err = RewriteSchema(*text, source, target)
			if err != nil {
				return nil, err
			}
		}
	}
	empty.Constraints = append([]MaterializedEmptyConstraint{}, original.Constraints...)
	for i := range empty.Constraints {
		constraint := &empty.Constraints[i]
		constraint.Columns = append([]string{}, constraint.Columns...)
		constraint.DefinitionSQL, _, err = RewriteSchema(constraint.DefinitionSQL, source, target)
		if err != nil {
			return nil, err
		}
	}
	empty.IdentityRestore = append([]string{}, original.IdentityRestore...)
	for i := range empty.IdentityRestore {
		empty.IdentityRestore[i], _, err = RewriteSchema(empty.IdentityRestore[i], source, target)
		if err != nil {
			return nil, err
		}
	}
	return &empty, nil
}
