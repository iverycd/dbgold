package target

import (
	"context"
	"dbgold/datamigrate/source"
)

// Writer 是目标库抽象接口，新增目标库只需实现此接口
type Writer interface {
	// DBType 返回数据库类型标识，如 "postgres"
	DBType() string
	// CreateTable 在目标库执行建表 DDL（先 DROP IF EXISTS，再 CREATE）
	CreateTable(ctx context.Context, ddl string) error
	// CopyData 使用批量协议写入一批行数据
	CopyData(ctx context.Context, table string, cols []string, rows [][]interface{}) error
	// CreateSequence 创建序列并绑定到列的默认值
	CreateSequence(ctx context.Context, seq source.SequenceInfo) error
	// CreateIndex 创建索引或唯一约束
	CreateIndex(ctx context.Context, idx source.IndexInfo) error
	// CreateForeignKey 创建外键约束
	CreateForeignKey(ctx context.Context, fk source.FKInfo) error
	// CreateView 创建视图
	CreateView(ctx context.Context, view source.ViewInfo) error
	// Close 关闭目标库连接
	Close() error
}
