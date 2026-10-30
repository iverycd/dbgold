package partition

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/sqlsyntax"
)

// Evidence is pinned to official mysql-8.0.36, InnoDB:
// sql/partitioning/partition_handler.cc ph_calculate_key_hash_value;
// sql/field.h Field_num::charset (my_charset_numeric = my_charset_latin1);
// strings/ctype-simple.cc my_hash_sort_simple; strings/ctype-bin.cc my_hash_sort_bin;
// strings/ctype-latin1.cc sort_order_latin1; sql/handler.h low_byte_first.
// https://github.com/mysql/mysql-server/tree/mysql-8.0.36
//
// ALGORITHM 1 hashes packed little-endian bytes. ALGORITHM 2 removes trailing
// 0x20 bytes from each numeric field and applies the fixed latin1 weights below.
// Each byte updates H ^= (((H&63)+S)*byte)+(H<<8), S+=3, starting H=1,S=4.
// NULL instead updates H ^= (H<<1)|1 and leaves S unchanged. Only H's final low
// 32 bits are returned. Masking H after every step is equivalent: addition,
// xor and left-shift cannot carry high bits into lower bits, and H&63 depends
// only on those low bits. All generated bigint intermediates stay below 2^41.
// This is the source algorithm expressed using immutable native PG builtins;
// it never uses a target vendor hash, business helper columns, or a UDF.
const mysqlKeyLatin1WeightsHex = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f404142434445464748494a4b4c4d4e4f505152535455565758595a5b5c5d5e5f604142434445464748494a4b4c4d4e4f505152535455565758595a7b7c7d7e7f808182838485868788898a8b8c8d8e8f909192939495969798999a9b9c9d9e9fa0a1a2a3a4a5a6a7a8a9aaabacadaeafb0b1b2b3b4b5b6b7b8b9babbbcbdbebf414141415c5b5c434545454549494949444e4f4f4f4f5dd7d85555555959dedf414141415c5b5c434545454549494949444e4f4f4f4f5df7d85555555959deff"

type mysqlPGKeyInteger struct {
	column           string
	width            int
	nullable         bool
	minimum, maximum int64
}

func (c *compiler) mysqlKeyAsPGList(dst, src *source.PartitionSpec, depth int, mapping *[]Mapping) error {
	if c.opt.SourceType != "mysql" || c.opt.TargetType != "postgres" || !c.profile.Features["postgres_declarative"] || !c.profile.Features[FeatureExpressions] || !c.profile.Features[FeatureList] {
		return fmt.Errorf("KEY 精确字节散列转换目前只验证 MySQL → PostgreSQL 原生表达式 LIST")
	}
	m := method(src.Method)
	if m != "KEY" && m != "LINEAR KEY" {
		return fmt.Errorf("不是 MySQL KEY 原生方法")
	}
	algorithm := 0
	switch src.Algorithm {
	case "mysql-key-innodb-1:8.0.36":
		algorithm = 1
	case "mysql-key-innodb-2:8.0.36":
		algorithm = 2
	default:
		return fmt.Errorf("跨库 KEY 字节散列目前只验证官方 MySQL 8.0.36 InnoDB ALGORITHM 1/2，当前 %s", src.Algorithm)
	}
	if len(src.Keys) == 0 {
		return fmt.Errorf("KEY 列清单为空")
	}
	var keys []mysqlPGKeyInteger
	widthTotal := 0
	lastColumnPosition := -1
	for _, key := range src.Keys {
		col, exists := c.column(key.Column)
		if !exists || key.Expression != "" || col.Generated != "" || strings.Contains(strings.ToLower(col.Extra), "generated") {
			return fmt.Errorf("跨库 KEY 仅接受已冻结的原生直接整数列")
		}
		typ, err := canonicalMySQLKeyType(key.DataType)
		if err != nil {
			return err
		}
		decl := col.FormattedType
		if decl == "" {
			decl = col.DataType
		}
		actual, err := canonicalMySQLKeyType(decl)
		if err != nil || actual != typ || key.Collation != "" {
			return fmt.Errorf("KEY 列 %s 的整数存储身份不完整或不一致", key.Column)
		}
		width := map[string]int{"tinyint": 1, "smallint": 2, "mediumint": 3, "int": 4}[strings.Fields(typ)[0]]
		if width == 0 {
			return fmt.Errorf("跨库 KEY 列 %s 的存储类型 %s 尚未验证；当前精确表达式支持总计最多4字节的整数键", key.Column, typ)
		}
		if width == 4 && strings.Contains(typ, "unsigned") {
			return fmt.Errorf("unsigned INT KEY 的目标列全值域映射尚未实现，不能用当前数据替代证明")
		}
		position := -1
		for i, column := range c.info.Columns {
			if column.Name == key.Column {
				position = i
				break
			}
		}
		if position <= lastColumnPosition {
			return fmt.Errorf("KEY 字段清单没有保持实际物理列顺序")
		}
		lastColumnPosition = position
		widthTotal += width
		if widthTotal > 4 {
			return fmt.Errorf("跨库 KEY 原生表达式展开目前限制总整数存储宽度4字节，避免不可控DDL/求值成本")
		}
		minimum, maximum := -(int64(1) << (8*width - 1)), (int64(1)<<(8*width-1))-1
		if strings.Contains(typ, "unsigned") {
			minimum, maximum = 0, (int64(1)<<(8*width))-1
		}
		keys = append(keys, mysqlPGKeyInteger{column: c.quote(c.opt.ColumnName(key.Column)), width: width, nullable: col.IsNullable, minimum: minimum, maximum: maximum})
		domain := KeyColumnDomain{Column: c.opt.ColumnName(key.Column), DataType: "integer", Nullable: col.IsNullable}
		found := false
		for _, existing := range c.keyColumns {
			if existing.Column == domain.Column {
				if existing != domain {
					return fmt.Errorf("KEY 列 %s 在分区层级中的目标类型/可空域不一致", domain.Column)
				}
				found = true
				break
			}
		}
		if !found {
			c.keyColumns = append(c.keyColumns, domain)
		}
	}
	sort.Slice(c.keyColumns, func(i, j int) bool { return c.keyColumns[i].Column < c.keyColumns[j].Column })
	n := len(src.Partitions)
	if n < 1 || n > 8192 {
		return fmt.Errorf("KEY 分区清单超出1..8192验证范围")
	}
	modulus := n
	if m == "LINEAR KEY" {
		modulus = 1
		for modulus < n {
			modulus <<= 1
		}
	}
	expression := mysqlKeyPGHashExpression(keys, algorithm)
	if m == "LINEAR KEY" {
		expression = mysqlKeyPGCall("int8and", expression, mysqlKeyPGInt(int64(modulus-1)))
	} else {
		expression = mysqlKeyPGCall("int8mod", expression, mysqlKeyPGInt(int64(modulus)))
	}
	// The existing PG mapper widens tiny/small/medium integers to int. Guard
	// the original domain inside the native routing expression itself. -1 has
	// no LIST leaf, so out-of-source-domain writes are rejected by PostgreSQL.
	// This guard is part of the frozen expression and is therefore verified
	// during append/repair, unlike an uninspected external CHECK constraint.
	for _, key := range keys {
		value := "CAST(" + key.column + " AS bigint)"
		expression = "CASE WHEN " + mysqlKeyPGCall("int8lt", value, mysqlKeyPGInt(key.minimum)) + " THEN " + mysqlKeyPGInt(-1) + " WHEN " + mysqlKeyPGCall("int8gt", value, mysqlKeyPGInt(key.maximum)) + " THEN " + mysqlKeyPGInt(-1) + " ELSE " + expression + " END"
	}
	// PostgreSQL's partition grammar requires parentheses around a CASE key.
	expression = "(" + expression + ")"
	if len(expression) > 512*1024 {
		return fmt.Errorf("KEY 原生表达式超过已验证512KiB安全展开上限")
	}
	normal := source.ClonePartitionSpec(src)
	normal.Method, normal.Algorithm = "LIST", ""
	// PostgreSQL LIST accepts one key. The final expression includes every
	// physical source field; this temporary key only reuses topology validation.
	normal.Keys = []source.PartitionKey{src.Keys[0]}
	for i := range normal.Partitions {
		p := &normal.Partitions[i]
		if p.HashModulus != n || p.HashRemainder != i || p.Default {
			return fmt.Errorf("KEY 目录分区数、顺序或桶号不完整")
		}
		p.HashModulus, p.HashRemainder = 0, 0
		p.Lower, p.Upper, p.Values = nil, nil, nil
		p.LowerInclusive, p.UpperInclusive = false, false
	}
	for residue := 0; residue < modulus; residue++ {
		bucket := residue
		if bucket >= n {
			bucket &= modulus/2 - 1
		}
		normal.Partitions[bucket].Values = append(normal.Partitions[bucket].Values, []source.PartitionValue{{Kind: "number", Text: strconv.Itoa(residue)}})
	}
	*dst = *source.ClonePartitionSpec(normal)
	if err := c.prepare(dst, normal, depth, mapping); err != nil {
		return err
	}
	dst.Keys = []source.PartitionKey{{Expression: expression, DataType: "bigint"}}
	c.rules = append(c.rules, "mysql-innodb-key-bytes-to-native-list")
	return nil
}

// The expression uses bigint casts, which could otherwise hide an unsafe base
// mapping (numeric rounds on cast, while smallint narrows the domain). Validate
// the exact generated column type and NULL domain before any DDL is executed.
func (c *compiler) validateMySQLKeyPGDDL(base string) error {
	if len(c.keyColumns) == 0 {
		return nil
	}
	tokens, err := sqlsyntax.Lex(base)
	if err != nil {
		return fmt.Errorf("KEY 基础DDL无法严格解析: %w", err)
	}
	start := -1
	for i, t := range tokens {
		if t.Is("(") {
			start = i
			break
		}
	}
	if start < 0 {
		return fmt.Errorf("KEY 基础DDL没有列定义")
	}
	groups := [][]sqlsyntax.Token{}
	depth, from, end := 1, start+1, -1
	for i := start + 1; i < len(tokens); i++ {
		if tokens[i].Is("(") {
			depth++
		}
		if tokens[i].Is(")") {
			depth--
			if depth == 0 {
				groups = append(groups, tokens[from:i])
				end = i
				break
			}
		}
		if depth == 1 && tokens[i].Is(",") {
			groups = append(groups, tokens[from:i])
			from = i + 1
		}
	}
	if end < 0 {
		return fmt.Errorf("KEY 基础DDL列定义没有闭合")
	}
	seen := map[string]bool{}
	for _, group := range groups {
		if len(group) < 2 {
			continue
		}
		name := group[0].Value
		if group[0].Kind != 'q' && group[0].Kind != 'i' {
			continue
		}
		var domain *KeyColumnDomain
		for i := range c.keyColumns {
			if c.keyColumns[i].Column == name {
				domain = &c.keyColumns[i]
				break
			}
		}
		if domain == nil {
			continue
		}
		if seen[name] {
			return fmt.Errorf("KEY 基础DDL键列 %s 重复", name)
		}
		seen[name] = true
		position := 1
		if !group[position].Is("int") && !group[position].Is("integer") && !group[position].Is("int4") {
			return fmt.Errorf("KEY 基础DDL列 %s 必须保持 PostgreSQL integer 类型", name)
		}
		position++
		if position < len(group) && !(group[position].Is("NOT") || group[position].Is("NULL") || group[position].Is("DEFAULT") || group[position].Is("CONSTRAINT") || group[position].Is("CHECK") || group[position].Is("PRIMARY") || group[position].Is("UNIQUE")) {
			return fmt.Errorf("KEY 基础DDL列 %s 包含未验证类型修饰或生成规则", name)
		}
		notNull := false
		depth = 0
		for i := position; i < len(group); i++ {
			if group[i].Is("(") {
				depth++
			}
			if group[i].Is(")") {
				depth--
			}
			if depth == 0 && i+1 < len(group) && group[i].Is("NOT") && group[i+1].Is("NULL") {
				notNull = true
			}
			if depth == 0 && i+1 < len(group) && group[i].Is("PRIMARY") && group[i+1].Is("KEY") {
				notNull = true
			}
		}
		if notNull == domain.Nullable {
			return fmt.Errorf("KEY 基础DDL列 %s 的可空域与源列不一致", name)
		}
	}
	for _, domain := range c.keyColumns {
		if !seen[domain.Column] {
			return fmt.Errorf("KEY 基础DDL缺少列 %s", domain.Column)
		}
	}
	return nil
}

func mysqlKeyPGInt(n int64) string { return "CAST('" + strconv.FormatInt(n, 10) + "' AS bigint)" }
func mysqlKeyPGCall(name string, args ...string) string {
	return "pg_catalog." + name + "(" + strings.Join(args, ",") + ")"
}
func mysqlKeyPGHashStep(h, sequence, weight string) string {
	term := mysqlKeyPGCall("int8mul", mysqlKeyPGCall("int8pl", mysqlKeyPGCall("int8and", h, mysqlKeyPGInt(63)), sequence), weight)
	return mysqlKeyPGCall("int8and", mysqlKeyPGCall("int8xor", h, mysqlKeyPGCall("int8pl", term, mysqlKeyPGCall("int8shl", h, "8"))), mysqlKeyPGInt(4294967295))
}
func mysqlKeyPGHashNull(h string) string {
	return mysqlKeyPGCall("int8and", mysqlKeyPGCall("int8xor", h, mysqlKeyPGCall("int8or", mysqlKeyPGCall("int8shl", h, "1"), mysqlKeyPGInt(1))), mysqlKeyPGInt(4294967295))
}

func mysqlKeyPGHashExpression(keys []mysqlPGKeyInteger, algorithm int) string {
	h, sequence := mysqlKeyPGInt(1), mysqlKeyPGInt(4)
	for _, key := range keys {
		previous := h
		value := "CAST(" + key.column + " AS bigint)"
		prefixes := []string{h}
		for i := 0; i < key.width; i++ {
			b := mysqlKeyPGCall("int8and", mysqlKeyPGCall("int8shr", value, strconv.Itoa(8*i)), mysqlKeyPGInt(255))
			if algorithm == 2 {
				b = "CAST(" + mysqlKeyPGCall("get_byte", mysqlKeyPGCall("decode", "'"+mysqlKeyLatin1WeightsHex+"'", "'hex'"), "CAST("+b+" AS integer)") + " AS bigint)"
			}
			s := sequence
			if i > 0 {
				s = mysqlKeyPGCall("int8pl", s, mysqlKeyPGInt(int64(3*i)))
			}
			h = mysqlKeyPGHashStep(h, s, b)
			prefixes = append(prefixes, h)
		}
		usedBytes := mysqlKeyPGInt(int64(key.width))
		if algorithm == 2 {
			var branches, lengthBranches []string
			for used := 0; used < key.width; used++ {
				var spaces, mask int64
				for j := used; j < key.width; j++ {
					spaces = (spaces << 8) | 32
					mask = (mask << 8) | 255
				}
				tail := mysqlKeyPGCall("int8and", mysqlKeyPGCall("int8shr", value, strconv.Itoa(8*used)), mysqlKeyPGInt(mask))
				condition := mysqlKeyPGCall("int8eq", tail, mysqlKeyPGInt(spaces))
				branches = append(branches, "WHEN "+condition+" THEN "+prefixes[used])
				lengthBranches = append(lengthBranches, "WHEN "+condition+" THEN "+mysqlKeyPGInt(int64(used)))
			}
			h = "CASE " + strings.Join(branches, " ") + " ELSE " + h + " END"
			usedBytes = "CASE " + strings.Join(lengthBranches, " ") + " ELSE " + usedBytes + " END"
		}
		if key.nullable {
			h = "CASE WHEN " + key.column + " IS NULL THEN " + mysqlKeyPGHashNull(previous) + " ELSE " + h + " END"
			usedBytes = "CASE WHEN " + key.column + " IS NULL THEN " + mysqlKeyPGInt(0) + " ELSE " + usedBytes + " END"
		}
		sequence = mysqlKeyPGCall("int8pl", sequence, mysqlKeyPGCall("int8mul", mysqlKeyPGInt(3), usedBytes))
	}
	return h
}
