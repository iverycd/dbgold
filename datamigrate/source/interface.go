package source

import "context"

// ColumnInfo 表示一列的元数据，用于 DDL 生成
// IdentityInfo preserves SQL Server identity state independently of target names.
type IdentityInfo struct {
	Seed      int64
	Increment int64
	Last      *int64
}

type ColumnInfo struct {
	// PGMySQLLongText is a private migration-plan override. Readers leave it false;
	// the original VARCHAR length remains authoritative for conversion and CHECK.
	PGMySQLLongText   bool `json:"-"`
	IdentityInfo      *IdentityInfo
	Name              string
	DataType          string // 原始数据库类型（如 "varchar"、"int"）
	Length            int64
	DataLength        int64  // Original byte capacity, independent of character length.
	CharUsed          string // Oracle character length semantics: B or C.
	Precision         int64
	Scale             int64
	IsNullable        bool
	Default           *string
	Extra             string // 如 "auto_increment"
	Ordinal           int
	TypeKind          string
	TypeSchema        string
	FormattedType     string
	DateTimePrecision int64
	Identity          string
	Generated         string
	Comment           string
}

// TableDDLInfo 包含建表所需的完整元数据
type TableDDLInfo struct {
	TableName string
	Columns   []ColumnInfo
	Comment   string
}

// SequenceInfo 表示一个自增序列（来自 AUTO_INCREMENT 列）
type SequenceInfo struct {
	Native       *NativeSequenceInfo `json:"Native,omitempty"`
	Unsupported  string              // Metadata that cannot be represented by the selected target.
	FailureStage string              `json:"FailureStage,omitempty"` // Optional provenance for object-level failures.
	IdentityInfo *IdentityInfo
	TableName    string
	ColumnName   string
	StartValue   int64
	// StartValueText preserves MySQL unsigned AUTO_INCREMENT values beyond int64.
	// Existing targets continue using StartValue; Oracle uses the exact text.
	StartValueText string
}

// NativeSequenceInfo retains a named Oracle sequence and its table-scoped uses.
// Decimal text preserves sequence values before target-specific range checks.
type NativeSequenceInfo struct {
	Name, IncrementBy, MinValue, MaxValue, CacheSize string
	Cycle, Ordered                                   bool
	Bindings                                         []SequenceBinding
}

type SequenceBinding struct {
	// TriggerBody is the untouched source definition for diagnostics only.
	// It must never be executed as target SQL.
	TriggerBody string `json:",omitempty"`
	// SourceColumnName is retained by the migrator when a target column is
	// renamed, so a shared sequence can be regenerated for any bound table.
	SourceColumnName                         string `json:",omitempty"`
	TableName, ColumnName, Kind, TriggerName string
	OnlyWhenNull                             bool
}

// IndexInfo 表示一个索引或唯一约束
type IndexInfo struct {
	Descending  []bool // Optional direction per key column; omitted means ascending.
	TableName   string
	IndexName   string
	Columns     []string
	IsUnique    bool
	IsPrimary   bool
	Attnums     []int
	Method      string
	Definition  string
	Unsupported string
}

// FKInfo 表示一个外键约束
type FKInfo struct {
	TableName      string
	ConstraintName string
	Columns        []string
	RefTable       string
	RefColumns     []string
	OnDelete       string
	OnUpdate       string
	RefSchema      string
	Unsupported    string
}

// ViewInfo 表示一个视图
type ViewInfo struct {
	Columns    []ColumnInfo
	ViewName   string
	Definition string // 已转换为目标库语法的 SQL
}

// CommentInfo 表示一条表注释或列注释
type CommentInfo struct {
	TableName  string
	ColumnName string // 为空表示表注释,非空表示列注释
	Comment    string
}

// Reader 是源库抽象接口，新增源库只需实现此接口
type Reader interface {
	// DBType 返回数据库类型标识，如 "mysql"
	DBType() string
	// Close 关闭源库连接，释放资源
	Close() error
	// ListTables 返回过滤后的表名列表
	ListTables(ctx context.Context) ([]string, error)
	// GetTableDDLInfo 返回指定表的列定义
	GetTableDDLInfo(ctx context.Context, table string) (*TableDDLInfo, error)
	// GetPrimaryKey 返回主键列名列表，无主键返回空切片
	GetPrimaryKey(ctx context.Context, table string) ([]string, error)
	// GetPrimaryKeys 返回所有表的主键信息（IsPrimary=true），用于数据写入完毕后统一创建主键
	GetPrimaryKeys(ctx context.Context) ([]IndexInfo, error)
	// ReadPage 分页读取数据：pkCols 为主键列名列表，空则用 LIMIT/OFFSET。
	// colTypes 为每列的 DatabaseTypeName（大写），供目标侧 ValueConverter 落地中立值使用。
	ReadPage(ctx context.Context, table string, pkCols []string, offset, limit int64) (cols []string, colTypes []string, rows [][]interface{}, err error)
	// GetSequences 返回所有 AUTO_INCREMENT 列信息
	GetSequences(ctx context.Context) ([]SequenceInfo, error)
	// GetIndexes 返回所有索引信息（不含主键）
	GetIndexes(ctx context.Context) ([]IndexInfo, error)
	// GetForeignKeys 返回所有外键信息
	GetForeignKeys(ctx context.Context) ([]FKInfo, error)
	// GetViews 返回所有视图信息
	GetViews(ctx context.Context) ([]ViewInfo, error)
	// GetComments 返回所有表注释和列注释信息(原始大小写)
	GetComments(ctx context.Context) ([]CommentInfo, error)
	// GetTriggerCount 返回源库触发器总数，失败时返回 0 和 error
	GetTriggerCount(ctx context.Context) (int, error)
	// CountRows 返回指定表的行数
	CountRows(ctx context.Context, table string) (int64, error)
	// ListDatabases 返回该连接下所有可迁移的数据库/schema 名称列表（排除系统库）
	ListDatabases(ctx context.Context) ([]string, error)
}
