package partition

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/sqlsyntax"
)

// KEY is MySQL's field-storage hash, not an arithmetic HASH expression. Only
// the identical InnoDB implementation and explicit algorithm are accepted.
func (c *compiler) prepareMySQLKey(dst, src *source.PartitionSpec, depth int, mapping *[]Mapping) error {
	if c.opt.SourceType == "mysql" && c.opt.TargetType == "postgres" {
		return c.mysqlKeyAsPGList(dst, src, depth, mapping)
	}
	if c.opt.SourceType != "mysql" || c.opt.TargetType != "mysql" {
		return fmt.Errorf("跨产品 KEY/LINEAR KEY 使用厂商内部字段散列，目标原生 HASH 不保证相同路由；尚无可证明的原生等价转换")
	}
	if !c.profile.Features["inline_mysql"] || !c.profile.Features["mysql_key_innodb"] {
		return fmt.Errorf("目标官方 MySQL InnoDB KEY 算法身份尚未验证")
	}
	targetVersion, err := dialect.ParseMySQLVersion(c.profile.Version)
	if err != nil {
		return err
	}
	if _, err := mysqlKeyMethodSQL(src); err != nil {
		return err
	}
	_, sourceVersion, _ := strings.Cut(src.Algorithm, ":")
	if sourceVersion != fmt.Sprintf("%d.%d.%d", targetVersion.Major, targetVersion.Minor, targetVersion.Patch) {
		return fmt.Errorf("KEY 存储散列/排序权重仅验证同一正式版本，源 %s，目标 %s；跨版本不能仅凭算法编号或排序规则名称认定等价", sourceVersion, c.profile.Version)
	}
	for _, key := range src.Keys {
		col, ok := c.column(key.Column)
		if !ok || key.Expression != "" {
			return fmt.Errorf("KEY 分区必须使用已冻结的原生直接列")
		}
		if col.Generated != "" || strings.Contains(strings.ToLower(col.Extra), "generated") {
			return fmt.Errorf("KEY 生成列 %s 的原生生成规则尚未完整保留", key.Column)
		}
		typ, err := canonicalMySQLKeyType(key.DataType)
		if err != nil {
			return fmt.Errorf("KEY 列 %s: %w", key.Column, err)
		}
		decl := col.FormattedType
		if decl == "" {
			decl = col.DataType
		}
		columnType, err := canonicalMySQLKeyType(decl)
		if err != nil || columnType != typ {
			return fmt.Errorf("KEY 列 %s 的完整存储类型在表结构与分区目录中不一致", key.Column)
		}
		if _, err := mysqlKeyColumnSQL(key, c.profile.Version); err != nil {
			return err
		}
	}
	// Reuse the topology/name/row-movement checks with an internal HASH-shaped
	// copy. This never changes the executed algorithm: restore KEY immediately.
	normal := source.ClonePartitionSpec(src)
	normal.Method, normal.Algorithm = "HASH", "mysql-mod"
	*dst = *source.ClonePartitionSpec(normal)
	oldFeatures := c.profile.Features
	c.profile.Features = make(map[string]bool, len(oldFeatures)+1)
	for name, value := range oldFeatures {
		c.profile.Features[name] = value
	}
	c.profile.Features["multi_column_hash"] = true
	err = c.prepare(dst, normal, depth, mapping)
	c.profile.Features = oldFeatures
	if err != nil {
		return err
	}
	dst.Method, dst.Algorithm = method(src.Method), src.Algorithm
	c.rules = append(c.rules, "mysql-innodb-native-key-algorithm")
	return nil
}

// Used for every inline prefix (including nested prefix matching).
func mysqlKeyMethodSQL(s *source.PartitionSpec) (string, error) {
	m := method(s.Method)
	if m != "KEY" && m != "LINEAR KEY" {
		return m, nil
	}
	identity, version, ok := strings.Cut(s.Algorithm, ":")
	if _, err := dialect.ParseMySQLVersion(version); !ok || err != nil {
		return "", fmt.Errorf("MySQL KEY 算法未包含可核验版本身份")
	}
	switch identity {
	case "mysql-key-innodb-1":
		return m + " ALGORITHM = 1", nil
	case "mysql-key-innodb-2":
		return m + " ALGORITHM = 2", nil
	default:
		return "", fmt.Errorf("MySQL %s 算法 %q 未冻结为 InnoDB ALGORITHM 1/2", m, s.Algorithm)
	}
}

var mysqlKeyIntegerType = regexp.MustCompile(`^(tinyint|smallint|mediumint|int|bigint)(\([0-9]+\))?( unsigned)?( zerofill)?$`)
var mysqlKeySizedType = regexp.MustCompile(`^(char|varchar|binary|varbinary|bit)\(([0-9]+)\)$`)
var mysqlKeyDecimalType = regexp.MustCompile(`^decimal\(([0-9]+),([0-9]+)\)( unsigned)?( zerofill)?$`)
var mysqlKeyCollation = regexp.MustCompile(`^[a-z][a-z0-9]*_[a-z0-9_]+$`)

// Integer display widths do not change KEY bytes; signedness, storage width,
// decimal scale, string length and collation do. Keep this identity in the
// canonical partition signature, including append and post-DDL checks.
func canonicalMySQLKeyType(raw string) (string, error) {
	raw = strings.ToLower(strings.Join(strings.Fields(raw), " "))
	if m := mysqlKeyIntegerType.FindStringSubmatch(raw); m != nil {
		if m[2] != "" {
			n, err := strconv.Atoi(strings.Trim(m[2], "()"))
			if err != nil || n < 0 || n > 255 {
				return "", fmt.Errorf("KEY 整数显示宽度不合法")
			}
		}
		if m[4] != "" && m[3] == "" {
			m[3] = " unsigned"
		}
		return m[1] + m[3] + m[4], nil
	}
	if m := mysqlKeySizedType.FindStringSubmatch(raw); m != nil {
		n, err := strconv.Atoi(m[2])
		if err != nil || n < 0 || n > 65535 || m[1] == "bit" && (n < 1 || n > 64) {
			return "", fmt.Errorf("KEY 类型长度不合法")
		}
		return m[1] + "(" + strconv.Itoa(n) + ")", nil
	}
	if m := mysqlKeyDecimalType.FindStringSubmatch(raw); m != nil {
		p, ep := strconv.Atoi(m[1])
		s, es := strconv.Atoi(m[2])
		if ep != nil || es != nil || p < 1 || p > 65 || s < 0 || s > p || s > 30 {
			return "", fmt.Errorf("KEY DECIMAL 精度不合法")
		}
		if m[4] != "" && m[3] == "" {
			m[3] = " unsigned"
		}
		return fmt.Sprintf("decimal(%d,%d)%s%s", p, s, m[3], m[4]), nil
	}
	// Date/time legacy packed formats and enum/set encodings require additional
	// native storage evidence. Do not let the generic mapper silently change it.
	return "", fmt.Errorf("原生 KEY 存储类型 %q 尚未验证；目前支持整数、DECIMAL、BIT、CHAR/VARCHAR 和 BINARY/VARBINARY", raw)
}

func mysqlKeyColumnSQL(key source.PartitionKey, version string) (string, error) {
	typ, err := canonicalMySQLKeyType(key.DataType)
	if err != nil {
		return "", err
	}
	// Retain display attributes in DDL even though integer display width is not
	// part of routing identity. All text has passed the exact type grammar above.
	decl := strings.ToLower(strings.Join(strings.Fields(key.DataType), " "))
	isText := strings.HasPrefix(typ, "char(") || strings.HasPrefix(typ, "varchar(")
	if !isText {
		if key.Collation != "" {
			return "", fmt.Errorf("非字符 KEY 列 %s 出现无法解释的 collation", key.Column)
		}
		return decl, nil
	}
	if !mysqlKeyCollation.MatchString(key.Collation) {
		return "", fmt.Errorf("字符 KEY 列 %s 缺少完整原生排序规则", key.Column)
	}
	if strings.HasPrefix(version, "5.7.") && (strings.Contains(key.Collation, "_0900_") || strings.HasPrefix(key.Collation, "utf8mb3_")) {
		return "", fmt.Errorf("MySQL 5.7 不能原生声明 KEY 排序规则 %s", key.Collation)
	}
	charset := strings.SplitN(key.Collation, "_", 2)[0]
	return decl + " CHARACTER SET " + charset + " COLLATE " + key.Collation, nil
}

// Decorate only proven KEY columns in the generated base DDL. SourceDDL is
// never replayed. No auxiliary columns or hashing functions are introduced.
func (c *compiler) nativeMySQLKeyDDL(base string, target *source.PartitionSpec) (string, error) {
	keys := map[string]source.PartitionKey{}
	var levels []*source.PartitionSpec
	var collect func(*source.PartitionSpec) error
	collect = func(s *source.PartitionSpec) error {
		if s == nil {
			return nil
		}
		if method(s.Method) == "KEY" || method(s.Method) == "LINEAR KEY" {
			levels = append(levels, s)
			for _, key := range s.Keys {
				if old, ok := keys[key.Column]; ok && (old.DataType != key.DataType || old.Collation != key.Collation) {
					return fmt.Errorf("KEY 列存储定义在分区层级中不一致")
				}
				keys[key.Column] = key
			}
		}
		for _, p := range s.Partitions {
			if err := collect(p.Subpartition); err != nil {
				return err
			}
		}
		return nil
	}
	if err := collect(target); err != nil {
		return "", err
	}
	if len(keys) == 0 {
		return base, nil
	}
	tokens, err := sqlsyntax.Lex(base)
	if err != nil {
		return "", fmt.Errorf("KEY 基础 DDL 无法安全解析: %w", err)
	}
	start := -1
	for i, t := range tokens {
		if t.Is("(") {
			start = i
			break
		}
	}
	if start < 0 {
		return "", fmt.Errorf("KEY 基础 DDL 没有列定义")
	}
	var groups [][]sqlsyntax.Token
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
		return "", fmt.Errorf("KEY 基础 DDL 列定义未闭合")
	}
	render := func(ts []sqlsyntax.Token) string {
		a := make([]string, len(ts))
		for i, t := range ts {
			a[i] = t.Text
		}
		return strings.Join(a, " ")
	}
	var definitions []string
	var physicalOrder []string
	for _, g := range groups {
		if len(g) < 2 {
			return "", fmt.Errorf("KEY 基础 DDL 包含空列定义")
		}
		name := g[0].Text
		if g[0].Kind == 'q' {
			name = g[0].Value
		}
		key, exists := keys[name]
		if !exists {
			definitions = append(definitions, render(g))
			continue
		}
		decl, e := mysqlKeyColumnSQL(key, c.profile.Version)
		if e != nil {
			return "", e
		}
		// Generated base DDL emits one type name and optional numeric parameters.
		// Preserve NULL/default/identity clauses but reject ambiguous type syntax.
		j := 2
		if j < len(g) && g[j].Is("(") {
			j++
			for j < len(g) && !g[j].Is(")") {
				if g[j].Kind != 'n' && !g[j].Is(",") {
					return "", fmt.Errorf("KEY 基础列类型包含非数字参数")
				}
				j++
			}
			j++
		}
		for j < len(g) && (g[j].Is("unsigned") || g[j].Is("zerofill")) {
			j++
		}
		if j > len(g) || j < len(g) && !(g[j].Is("NOT") || g[j].Is("NULL") || g[j].Is("DEFAULT") || g[j].Is("AUTO_INCREMENT") || g[j].Is("COMMENT")) {
			return "", fmt.Errorf("KEY 基础列类型包含未验证修饰项")
		}
		definitions = append(definitions, g[0].Text+" "+decl+" "+render(g[j:]))
		physicalOrder = append(physicalOrder, name)
		delete(keys, name)
	}
	if len(keys) > 0 {
		return "", fmt.Errorf("KEY 基础 DDL 缺少分区键列")
	}
	for _, level := range levels {
		included := map[string]bool{}
		for _, key := range level.Keys {
			included[key.Column] = true
		}
		position := 0
		for _, name := range physicalOrder {
			if !included[name] {
				continue
			}
			if level.Keys[position].Column != name {
				return "", fmt.Errorf("KEY 基础 DDL 的物理列顺序改变了原生散列顺序")
			}
			position++
		}
	}
	return render(tokens[:start+1]) + "\n  " + strings.Join(definitions, ",\n  ") + "\n" + render(tokens[end:]), nil
}
