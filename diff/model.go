package diff

import "dbgold/schema"

type Result struct {
	AddedTables    []schema.Table
	DroppedTables  []schema.Table
	ModifiedTables []TableDiff
}

type TableDiff struct {
	TableName          string
	AddedColumns       []schema.Column
	DroppedColumns     []schema.Column
	ModifiedColumns    []ColumnDiff
	AddedIndexes       []schema.Index
	DroppedIndexes     []schema.Index
	AddedConstraints   []schema.Constraint
	DroppedConstraints []schema.Constraint
	AddedForeignKeys   []schema.ForeignKey
	DroppedForeignKeys []schema.ForeignKey
}

type ColumnDiff struct {
	Column               schema.Column
	OldColumn            schema.Column
	TypeChanged          bool
	NullableChanged      bool
	DefaultChanged       bool
	AutoIncrementChanged bool
}
