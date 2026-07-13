// Package coldefault 提供按源库类型剥离列默认值字面量脏数据的纯函数。
// 这些逻辑原先内嵌在 datamigrate/migrator.go,属于「源维度」职责
// (与目标库无关),抽取到独立包以便目标方言(Dialect)层复用。
package coldefault

import "strings"

// Strip 按源库类型剥离默认值字面量中的脏数据,返回逻辑默认值。
// srcType 取自 source.Reader.DBType(),如 "mysql" / "sqlserver" / "oracle" / "dameng"。
// 达梦系统表(ALL_TAB_COLUMNS.DATA_DEFAULT)与 Oracle 兼容,默认值同样以带字面量
// 引号的文本存储,复用 StripOracle 的脱壳逻辑。
func Strip(srcType, raw string) string {
	switch srcType {
	case "sqlserver":
		return StripSQLServer(raw)
	case "oracle", "dameng":
		return StripOracle(raw)
	case "mysql":
		return StripMySQLExpr(raw)
	default:
		return raw
	}
}

// StripOracle 清理 Oracle 默认值中多余的单引号。
// Oracle 在 ALL_TAB_COLUMNS.DATA_DEFAULT 里原样保存 SQL 字面量,如:
//
//	''0''   → 0      (bigint 列的数字默认值)
//	'''abc''' → 'abc' (字符串默认值,外层再包一层引号)
//	NULL    → 保持不变
func StripOracle(def string) string {
	def = strings.TrimSpace(def)
	// 连续两个单引号包裹:''value'' → value(用于数字或裸值)
	if len(def) >= 4 && strings.HasPrefix(def, "''") && strings.HasSuffix(def, "''") {
		inner := def[2 : len(def)-2]
		// 确保内部没有单引号(否则是字符串默认值,不做处理)
		if !strings.Contains(inner, "'") {
			return strings.TrimSpace(inner)
		}
	}
	// 三层单引号:'''value''' → 'value'(字符串默认值)
	if len(def) >= 6 && strings.HasPrefix(def, "'''") && strings.HasSuffix(def, "'''") {
		return def[2 : len(def)-2]
	}
	// 单层单引号:'value' → value(Oracle 直接存储 SQL 字面量,外层格式化会补引号)
	if len(def) >= 2 && strings.HasPrefix(def, "'") && strings.HasSuffix(def, "'") {
		inner := def[1 : len(def)-1]
		return strings.ReplaceAll(inner, "''", "'")
	}
	return def
}

// StripSQLServer 清理 SQL Server 默认值的额外括号与 Unicode 前缀。
// SQL Server 默认值通常带额外括号,如 ((0)) → 0,(getdate()) → getdate(),(N'abc') → abc。
func StripSQLServer(def string) string {
	def = strings.TrimSpace(def)
	// 循环剥离最外层括号(只要整体被括号包围)
	for strings.HasPrefix(def, "(") && strings.HasSuffix(def, ")") {
		inner := def[1 : len(def)-1]
		if isBalancedParens(inner) {
			def = strings.TrimSpace(inner)
		} else {
			break
		}
	}
	// 去掉 N'...' 前缀(SQL Server Unicode 字符串字面量)
	if strings.HasPrefix(def, "N'") && strings.HasSuffix(def, "'") {
		def = def[1:] // 去掉 N,保留 '...'
	}
	return def
}

// StripMySQLExpr 剥离 MySQL 8.0 在 information_schema 中给表达式默认值加的外层括号。
// MySQL 8.0 将 DEFAULT ' ' 存储为 (' '),DEFAULT 0 存储为 (0)。
// 剥括号后若内部是 SQL 字符串字面量(如 ' '),再去掉引号返回裸值,
// 让上层的 DEFAULT '%s' 路径正确拼出 DEFAULT ' '。
func StripMySQLExpr(def string) string {
	def = strings.TrimSpace(def)
	if strings.HasPrefix(def, "(") && strings.HasSuffix(def, ")") {
		inner := strings.TrimSpace(def[1 : len(def)-1])
		if isBalancedParens(inner) {
			def = inner
		}
	}
	// 内部是 SQL 字符串字面量 'value',提取裸值交由上层统一处理
	if len(def) >= 2 && strings.HasPrefix(def, "'") && strings.HasSuffix(def, "'") {
		inner := def[1 : len(def)-1]
		return strings.ReplaceAll(inner, "''", "'")
	}
	return def
}

// isBalancedParens 检查字符串中括号是否平衡
func isBalancedParens(s string) bool {
	depth := 0
	for _, ch := range s {
		switch ch {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}
