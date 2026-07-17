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

// Routine 表示一个需要原样导出的数据库代码对象。
// Body 为源库原始 DDL（含 CREATE 头及该方言的终止符/DELIMITER），
// 不做任何跨库语法转换——各厂商 PL/SQL、T-SQL 语法不兼容，
// 导出后由用户手动适配目标库。
type Routine struct {
	Name string
	Type string // PROCEDURE | FUNCTION | PACKAGE | PACKAGE BODY | TRIGGER
	Body string
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
