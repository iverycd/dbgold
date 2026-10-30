// Package partition compiles observed logical partition metadata into target DDL.
// It does not connect to databases or execute source SQL. Missing proof is an
// error, never permission to flatten a partitioned table into an ordinary table.
package partition

import (
	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
)

type Profile struct {
	Schema   string          `json:"schema,omitempty"`
	Product  string          `json:"product"`
	Version  string          `json:"version"`
	Mode     string          `json:"mode,omitempty"`
	Features map[string]bool `json:"features,omitempty"`
}

type Options struct {
	SourceType   string
	TargetType   string
	TargetSchema string
	Name         func(string) string
	ColumnName   func(string) string
}

type Mapping struct {
	SourceSchema  string `json:"source_schema,omitempty"`
	SourceName    string `json:"source_name"`
	TargetSchema  string `json:"target_schema,omitempty"`
	TargetName    string `json:"target_name"`
	TargetOrdinal int    `json:"target_ordinal,omitempty"`
}

type Plan struct {
	Source      *source.PartitionSpec    `json:"source"`
	Target      *source.PartitionSpec    `json:"target"`
	Statements  []dialect.Statement      `json:"statements"`
	Mapping     []Mapping                `json:"mapping,omitempty"`
	Fingerprint string                   `json:"fingerprint"`
	Rule        string                   `json:"rule"`
	Interval    *IntervalMaterialization `json:"interval_materialization,omitempty"`
	KeyColumns  []KeyColumnDomain        `json:"key_columns,omitempty"`
}

// KeyColumnDomain freezes target storage facts needed by exact KEY routing.
// Expression casts must not hide changed storage types or NULL acceptance.
type KeyColumnDomain struct {
	Column   string `json:"column"`
	DataType string `json:"data_type"`
	Nullable bool   `json:"nullable"`
}

// Feature names refer to capabilities actually observed on the target. Product
// aliases alone must never enable partition syntax on a PostgreSQL fork.
const (
	FeatureRange        = "native_range"
	FeatureList         = "native_list"
	FeatureHash         = "native_hash"
	FeatureExpressions  = "expression_keys"
	FeatureMultiRange   = "multi_column_range"
	FeatureMultiList    = "multi_column_list"
	FeatureSubpartition = "subpartition"
	FeatureDefaultRange = "default_range"
	FeatureDefaultList  = "default_list"
	FeatureTextRouting  = "exact_text_collation"
	FeatureInterval     = "native_interval"
)

// LeafKey is a display-independent count key shared with source inspectors.
// Names are quoted separately when constructing SQL; this is never SQL text.
func LeafKey(schema, name string) string {
	if schema == "" {
		return name
	}
	return schema + "." + name
}

func LeafNames(spec *source.PartitionSpec) []string {
	var out []string
	if spec == nil {
		return out
	}
	for _, p := range spec.Partitions {
		if p.Subpartition == nil {
			out = append(out, LeafKey(p.Schema, p.Name))
		} else {
			out = append(out, LeafNames(p.Subpartition)...)
		}
	}
	return out
}
