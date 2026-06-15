package typemap

import "dbgold/datamigrate/source"

// Mapper 把单列的源类型映射为目标库类型字符串。
// charInLength=true 时 char/varchar 长度单位使用 CHAR;
// useNvarchar2=true 时 varchar/char 转为 nvarchar2(优先级更高)。
type Mapper func(col source.ColumnInfo, charInLength, useNvarchar2 bool) string

// registry 以 "源_目标"(如 "mysql_postgres" / "mysql_dameng")为键,
// 保存每个迁移组合的类型映射函数。各 *_*.go 文件在 init() 中自注册。
var registry = map[string]Mapper{}

// Register 注册一个「源库 → 目标库」类型映射。重复注册会覆盖。
func Register(srcType, dstType string, m Mapper) {
	registry[srcType+"_"+dstType] = m
}

// Get 返回指定「源库 → 目标库」组合的类型映射函数。
// 第二个返回值表示该组合是否已注册。
func Get(srcType, dstType string) (Mapper, bool) {
	m, ok := registry[srcType+"_"+dstType]
	return m, ok
}
