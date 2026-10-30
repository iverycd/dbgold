package source

import "context"

// PartitionSpec is the original, credential-free logical partition topology.
// Methods use the native uppercase names (RANGE, LIST, HASH, KEY). Expressions
// and original SQL are evidence, never executable target SQL without a compiler.
type PartitionSpec struct {
	Method      string             `json:"method"`
	Keys        []PartitionKey     `json:"keys"`
	Partitions  []Partition        `json:"partitions"`
	Automatic   string             `json:"automatic,omitempty"`
	Interval    *PartitionInterval `json:"interval,omitempty"`
	Algorithm   string             `json:"algorithm,omitempty"`
	RowMovement string             `json:"row_movement,omitempty"`
	SourceDDL   string             `json:"source_ddl,omitempty"`
	Unsupported string             `json:"unsupported,omitempty"`
}

type PartitionKey struct {
	Column     string `json:"column,omitempty"`
	Expression string `json:"expression,omitempty"`
	DataType   string `json:"data_type,omitempty"`
	Collation  string `json:"collation,omitempty"`
}

type Partition struct {
	Schema         string             `json:"schema,omitempty"`
	Name           string             `json:"name"`
	Ordinal        int                `json:"ordinal"`
	Lower          []PartitionValue   `json:"lower,omitempty"`
	Upper          []PartitionValue   `json:"upper,omitempty"`
	Values         [][]PartitionValue `json:"values,omitempty"`
	Default        bool               `json:"default,omitempty"`
	LowerInclusive bool               `json:"lower_inclusive,omitempty"`
	UpperInclusive bool               `json:"upper_inclusive,omitempty"`
	HashModulus    int                `json:"hash_modulus,omitempty"`
	HashRemainder  int                `json:"hash_remainder,omitempty"`
	Subpartition   *PartitionSpec     `json:"subpartition,omitempty"`
	IsInterval     bool               `json:"is_interval,omitempty"`
}

// PartitionInterval is a proved native constant, not executable source SQL.
// Transition remains the last initial RANGE upper bound even when automatic
// partitions are sparse or were materialized out of order.
type PartitionInterval struct {
	Unit       string         `json:"unit"` // number, month, second
	Amount     string         `json:"amount"`
	Transition PartitionValue `json:"transition"`
}

type PartitionValue struct {
	// Kind is number, string, date, timestamp, null, minvalue or maxvalue.
	Kind string `json:"kind"`
	Text string `json:"text,omitempty"`
}

// PartitionReader is optional so old readers and saved jobs retain their
// ordinary metadata contract. Orchestration reads this before target mutation.
type PartitionReader interface {
	PartitionMetadata(context.Context, string) (*PartitionSpec, error)
	// PartitionRoots maps qualified physical table names to the selected
	// schema's original logical root name. Partition labels are not tables.
	PartitionRoots(context.Context) (map[string]string, error)
	StreamPartitionRows(context.Context, string, int, func([]string, []string, [][]interface{}) error) error
}

type PartitionCounter interface {
	CountPartitionRows(context.Context, string, *PartitionSpec) (map[string]int64, error)
}

type PartitionIndexReader interface {
	PartitionIndexes(context.Context, string) ([]IndexInfo, error)
}

func ClonePartitionSpec(spec *PartitionSpec) *PartitionSpec {
	if spec == nil {
		return nil
	}
	clone := *spec
	if spec.Interval != nil {
		interval := *spec.Interval
		clone.Interval = &interval
	}
	clone.Keys = append([]PartitionKey(nil), spec.Keys...)
	clone.Partitions = make([]Partition, len(spec.Partitions))
	for i, part := range spec.Partitions {
		clone.Partitions[i] = part
		clone.Partitions[i].Lower = append([]PartitionValue(nil), part.Lower...)
		clone.Partitions[i].Upper = append([]PartitionValue(nil), part.Upper...)
		clone.Partitions[i].Values = make([][]PartitionValue, len(part.Values))
		for j, value := range part.Values {
			clone.Partitions[i].Values[j] = append([]PartitionValue(nil), value...)
		}
		clone.Partitions[i].Subpartition = ClonePartitionSpec(part.Subpartition)
	}
	return &clone
}
