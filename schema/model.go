package schema

// Routine 表示一个需要原样导出的数据库代码对象。
// Body 为源库原始 DDL（含 CREATE 头及该方言的终止符/DELIMITER），
// 不做任何跨库语法转换——各厂商 PL/SQL、T-SQL 语法不兼容，
// 导出后由用户手动适配目标库。
type Routine struct {
	Name string
	Type string // PROCEDURE | FUNCTION | PACKAGE | PACKAGE BODY | TRIGGER
	Body string
}
