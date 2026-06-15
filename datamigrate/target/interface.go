package target

import (
	"context"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
)

// Writer 是目标库抽象接口，新增目标库只需实现此接口
type Writer interface {
	// DBType 返回数据库类型标识，如 "postgres"
	DBType() string
	// Dialect 返回该目标库的方言(SQL 生成),供 Migrator 生成报告 DDL 并保证与执行同源
	Dialect() dialect.Dialect
	// CreateTable 在目标库执行建表 DDL（先 DROP IF EXISTS，再 CREATE）
	CreateTable(ctx context.Context, ddl string) error
	// CopyData 使用批量协议写入一批行数据。
	// colTypes 为每列的 DatabaseTypeName（大写），Writer 用它经 ValueConverter
	// 把 Reader 输出的中立值落地成目标驱动能接受的形态。
	CopyData(ctx context.Context, table string, cols []string, colTypes []string, rows [][]interface{}) error
	// CreateSequence 创建序列并绑定到列的默认值
	CreateSequence(ctx context.Context, seq source.SequenceInfo) error
	// CreateIndex 创建索引或唯一约束
	CreateIndex(ctx context.Context, idx source.IndexInfo) error
	// CreateForeignKey 创建外键约束
	CreateForeignKey(ctx context.Context, fk source.FKInfo) error
	// CreateView 创建视图
	CreateView(ctx context.Context, view source.ViewInfo) error
	// CountRows 返回指定表的行数
	CountRows(ctx context.Context, table string) (int64, error)
	// AlterDistribute 在分布式数据库中将表的分布列设置为指定列（非分布式实现返回 nil）
	AlterDistribute(ctx context.Context, table string, cols []string) error
	// SchemaExists 检查指定 schema 是否存在于目标库
	SchemaExists(ctx context.Context, schema string) (bool, error)
	// ChangeOwner 将对象 owner 改为指定角色
	// objType: "TABLE" | "VIEW" | "SEQUENCE"；name 不含 schema 前缀
	ChangeOwner(ctx context.Context, objType, name, owner string) error
	// Close 关闭目标库连接
	Close() error
}
