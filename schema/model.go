package schema

type Schema struct {
	Name   string
	Tables []Table
}

type Table struct {
	Name        string
	Columns     []Column
	Indexes     []Index
	Constraints []Constraint
	ForeignKeys []ForeignKey
}

type Column struct {
	Name          string
	Type          string
	Nullable      bool
	Default       *string
	PrimaryKey    bool
	AutoIncrement bool
	Comment       string
}

type Index struct {
	Name    string
	Columns []string
	Unique  bool
}

type Constraint struct {
	Name string
	Type string // CHECK | UNIQUE | PRIMARY
	Def  string
}

type ForeignKey struct {
	Name       string
	Columns    []string
	RefTable   string
	RefColumns []string
	OnDelete   string
	OnUpdate   string
}

type FullSchema struct {
	Schema
	Views     []View
	Sequences []Sequence
	Triggers  []Trigger
}

type View struct {
	Name string
	Def  string
}

type Sequence struct {
	Name      string
	Start     int64
	Increment int64
	MinValue  int64
	MaxValue  int64
}

type Trigger struct {
	Name   string
	Table  string
	Event  string
	Timing string
	Body   string
}

type SelectedObjects struct {
	Tables      []Table
	Views       []View
	Sequences   []Sequence
	Triggers    []Trigger
	Indexes     []Index
	Constraints []Constraint
	ForeignKeys []ForeignKey
}
