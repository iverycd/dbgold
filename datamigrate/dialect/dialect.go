// Package dialect 定义「目标方言」抽象:把生成目标库 SQL 的职责
// 从 migrator / target writer 中剥离出来,集中为纯函数(零 I/O)。
//
// 设计要点:
//   - Dialect 只负责把元数据拼成 SQL 字符串,不连接数据库、不执行。
//   - 它是 DDL 的唯一真相来源:报告展示的 DDL 与 writer 实际执行的 SQL
//     都来自同一组方法,从根本上杜绝两者漂移。
//   - 大小写策略由调用方(migrator 的 objName)通过 NameFunc 注入,
//     Dialect 不自行做 lower()/upper()(遵守项目 CLAUDE.md 约定)。
//   - 新增目标库 = 新增一个实现本接口的文件 + 注册类型映射 + 实现 Writer。
package dialect

import "dbgold/datamigrate/source"

// NameFunc 由调用方注入的标识符大小写规整函数(对应 migrator.objName)。
// Dialect 用它处理表名/列名后再加引号,自身不决定大小写。
type NameFunc func(string) string

// TypeOpt 透传给类型映射的选项。
type TypeOpt struct {
	CharInLength bool
	UseNvarchar2 bool
}

// Statement 一条可执行 SQL。SQL 不含末尾分号(与 writer 单句执行约定一致),
// 但建表这类需要一次提交多句的场景,SQL 内部可含 ";\n" 分隔(与现状一致)。
type Statement struct {
	SQL string
}

// Capabilities 描述目标库能力,供 Migrator 编排时决策,
// 避免在 migrator 里出现 `switch targetDBType` 的硬编码分支。
type Capabilities struct {
	// UsesSequences 为 true 时,自增通过 Phase3 创建序列实现(PostgreSQL);
	// 为 false 时,自增在建表阶段以 IDENTITY 列声明(达梦)。
	UsesSequences bool
	// IdentityInsert 为 true 时,写入含自增列的表前后需要开关 IDENTITY_INSERT(达梦)。
	IdentityInsert bool
	// SupportsDistribute 为 true 时,支持分布列(GaussDB 分布式)。
	SupportsDistribute bool
	// SupportsChangeOwner 为 true 时,支持 ALTER ... OWNER TO(PostgreSQL 系)。
	SupportsChangeOwner bool
}

// Dialect 目标方言接口。一个目标库对应一个 Dialect 实现。
type Dialect interface {
	// Name 返回目标库类型标识,如 "postgres" / "gaussdb" / "seabox" / "dameng"。
	Name() string

	// QuoteIdent 为单个标识符加引号(已由调用方决定好大小写)。
	QuoteIdent(name string) string
	// QualifyTable 返回带 schema 前缀的表名;schema 为空时只返回表名。
	// 传入的 table 已是规整后的名字。
	QualifyTable(schema, table string) string

	// MapType 把源列类型映射为目标库类型字符串;srcType 取自 reader.DBType()。
	MapType(col source.ColumnInfo, srcType string, opt TypeOpt) string

	// CreateTableStatements 生成建表所需语句。name 用于规整表名/列名大小写,
	// srcType 用于默认值脱壳(coldefault)与类型映射。
	CreateTableStatements(schema string, info *source.TableDDLInfo, srcType string, opt TypeOpt, name NameFunc) ([]Statement, error)

	// 以下 PostDDL 方法接收的 *Info 中的名字均已由调用方规整。
	SequenceStatements(schema string, seq source.SequenceInfo) []Statement
	IndexStatements(schema string, idx source.IndexInfo) []Statement
	ForeignKeyStatements(schema string, fk source.FKInfo) []Statement
	ViewStatements(schema string, view source.ViewInfo) []Statement

	// AdjustViewDefinition 把视图定义中的中间形式函数(如 gen_random_uuid())
	// 替换为目标库对应函数。
	AdjustViewDefinition(def string) string

	// Caps 返回目标库能力标志。
	Caps() Capabilities
}

// JoinSQL 把多条语句拼成用于报告展示的单段文本。
func JoinSQL(stmts []Statement) string {
	switch len(stmts) {
	case 0:
		return ""
	case 1:
		return stmts[0].SQL
	}
	out := stmts[0].SQL
	for _, s := range stmts[1:] {
		out += ";\n" + s.SQL
	}
	return out
}
