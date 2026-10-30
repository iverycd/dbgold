package partition

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/sqlsyntax"
)

// Compile only decorates the exact base DDL that will be executed. It never
// regenerates columns, silently changes a key or evaluates a source expression.
func Compile(info *source.TableDDLInfo, base []dialect.Statement, profile Profile, opt Options) (*Plan, error) {
	if info == nil {
		return nil, fmt.Errorf("分区编译缺少表元数据")
	}
	if opt.Name == nil {
		opt.Name = func(s string) string { return s }
	}
	if opt.ColumnName == nil {
		opt.ColumnName = opt.Name
	}
	p := &Plan{Source: source.ClonePartitionSpec(info.Partition), Statements: append([]dialect.Statement(nil), base...), Rule: "ordinary-table"}
	if info.Partition == nil {
		var err error
		p.Fingerprint, err = planFingerprint(p)
		return p, err
	}
	if profile.Product != opt.TargetType || profile.Version == "" {
		return nil, fmt.Errorf("目标 %s 的产品/版本分区能力尚未读取/验证", opt.TargetType)
	}
	if len(base) == 0 {
		return nil, fmt.Errorf("分区表 %s 缺少基础建表语句", info.TableName)
	}
	p.Target = source.ClonePartitionSpec(info.Partition)
	c := compiler{info: info, profile: profile, opt: opt, names: map[string]bool{}, leafKeys: map[string]bool{}}
	if err := c.prepare(p.Target, info.Partition, 0, &p.Mapping); err != nil {
		return nil, fmt.Errorf("表 %s 的分区迁移: %w", info.TableName, err)
	}
	p.KeyColumns = append([]KeyColumnDomain(nil), c.keyColumns...)
	var intervalErr error
	p.Interval, intervalErr = c.freezeIntervalMaterialization(p.Target)
	if intervalErr != nil {
		return nil, intervalErr
	}
	create := -1
	for i, s := range p.Statements {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(s.SQL)), "CREATE TABLE ") {
			if create >= 0 {
				return nil, fmt.Errorf("分区表基础 DDL 含多个 CREATE TABLE")
			}
			create = i
		}
	}
	if create < 0 {
		return nil, fmt.Errorf("分区表缺少可装饰的 CREATE TABLE")
	}
	if err := c.validateMySQLKeyPGDDL(p.Statements[create].SQL); err != nil {
		return nil, err
	}
	var keyDDLErr error
	p.Statements[create].SQL, keyDDLErr = c.nativeMySQLKeyDDL(p.Statements[create].SQL, p.Target)
	if keyDDLErr != nil {
		return nil, keyDDLErr
	}
	root := opt.Name(info.TableName)
	if err := c.validateName(root); err != nil {
		return nil, err
	}
	var err error
	switch {
	case profile.Features["postgres_declarative"]:
		p.Statements[create].SQL, err = c.pgRoot(p.Statements[create].SQL, p.Target)
		if err == nil {
			var children []dialect.Statement
			children, err = c.pgChildren(opt.TargetSchema, root, p.Target)
			p.Statements = append(p.Statements, children...)
		}
		p.Rule = "native-declarative-partitions"
	case profile.Features["inline_oracle"] || profile.Features["inline_mysql"] || profile.Features["inline_gauss"]:
		var clause string
		clause, err = c.inline(p.Target, 0)
		if err == nil {
			p.Statements[create].SQL = strings.TrimSuffix(strings.TrimSpace(p.Statements[create].SQL), ";") + "\n" + clause
		}
		p.Rule = "native-inline-partitions"
	case profile.Features["native_sqlserver"]:
		p.Statements, err = c.sqlServer(p.Statements, create, p.Target, root)
		p.Rule = "native-range-ordinal-map"
	default:
		return nil, fmt.Errorf("%s %s 的原生分区 DDL 适配尚未实现/验证", profile.Product, profile.Version)
	}
	if err != nil {
		return nil, err
	}
	if len(c.rules) > 0 {
		p.Rule += ":" + strings.Join(c.rules, ",")
	}
	if p.Interval != nil {
		p.Statements = append(p.Statements, dialect.Statement{SQL: IntervalMaterializationSQL(p)})
	}
	p.Fingerprint, err = planFingerprint(p)
	return p, err
}

type compiler struct {
	info       *source.TableDDLInfo
	profile    Profile
	opt        Options
	names      map[string]bool
	leafKeys   map[string]bool
	rules      []string
	keyColumns []KeyColumnDomain
}

func method(s string) string { return strings.ToUpper(strings.Join(strings.Fields(s), " ")) }
func baseMethod(s string) string {
	b := strings.TrimSuffix(method(s), " COLUMNS")
	if b == "LINEAR HASH" {
		return "HASH"
	}
	if b == "LINEAR KEY" {
		return "KEY"
	}
	return b
}

func (c *compiler) prepare(dst, src *source.PartitionSpec, depth int, mapping *[]Mapping) error {
	if depth > 7 {
		return fmt.Errorf("超过八级的分区拓扑尚未实现/验证")
	}
	if src.Unsupported != "" {
		return fmt.Errorf("源分区元数据尚未实现/验证: %s", src.Unsupported)
	}
	m := method(src.Method)
	if m == "KEY" || m == "LINEAR KEY" {
		return c.prepareMySQLKey(dst, src, depth, mapping)
	}
	b := baseMethod(m)
	if b == "RANGE" && c.opt.SourceType == "sqlserver" && !c.profile.Features["native_sqlserver"] {
		normal, err := c.sqlServerRangeLeft(src)
		if err != nil {
			return err
		}
		if normal != nil {
			*dst = *source.ClonePartitionSpec(normal)
			if err := c.prepare(dst, normal, depth, mapping); err != nil {
				return err
			}
			c.rules = append(c.rules, "sqlserver-integer-range-left-to-half-open")
			return nil
		}
	}
	if m == "LINEAR HASH" && c.opt.SourceType != "mysql" {
		return fmt.Errorf("%s 的 LINEAR HASH 原生算法尚未实现/验证", c.opt.SourceType)
	}
	if b == "HASH" && c.opt.SourceType == "mysql" && c.opt.TargetType != "mysql" && c.profile.Features["postgres_declarative"] {
		return c.mysqlHashAsList(dst, src, depth, mapping)
	}
	if b != "RANGE" && b != "LIST" && b != "HASH" {
		return fmt.Errorf("%s 的精确路由适配尚未实现/验证，禁止替换为目标原生 HASH 或普通表", m)
	}
	if !c.profile.Features[map[string]string{"RANGE": FeatureRange, "LIST": FeatureList, "HASH": FeatureHash}[b]] {
		return fmt.Errorf("%s %s 的 %s 分区能力尚未实现/验证", c.profile.Product, c.profile.Version, b)
	}
	if len(src.Keys) == 0 || len(src.Partitions) == 0 {
		return fmt.Errorf("分区键或完整分区清单为空")
	}
	if len(src.Keys) > 1 && !c.profile.Features[map[string]string{"RANGE": FeatureMultiRange, "LIST": FeatureMultiList, "HASH": "multi_column_hash"}[b]] {
		return fmt.Errorf("目标 %s 的多列 %s 分区等价适配尚未实现/验证", c.opt.TargetType, b)
	}
	if err := c.prepareInterval(dst, src, depth); err != nil {
		return err
	}
	if depth == 0 && src.RowMovement != "" {
		if src.RowMovement != "enabled" && src.RowMovement != "disabled" {
			return fmt.Errorf("源分区键更新路由规则不可确认")
		}
		if !c.profile.Features["row_movement_clause"] {
			targetMovement := "disabled"
			if c.profile.Features["row_movement"] {
				targetMovement = "enabled"
			}
			if targetMovement != src.RowMovement {
				return fmt.Errorf("源跨分区 UPDATE 行移动为 %s，目标原生行为为 %s，不能等价保留", src.RowMovement, targetMovement)
			}
		}
	}
	dst.Method = b
	if m == "LINEAR HASH" && c.opt.SourceType == "mysql" && c.opt.TargetType == "mysql" {
		dst.Method = m
	}
	if c.profile.Features["inline_gauss"] {
		dst.Algorithm = "pg_partition:" + c.opt.TargetType + ":" + strings.ToLower(b)
	}
	if c.profile.Features["inline_mysql"] && (strings.HasSuffix(m, " COLUMNS") || b == "RANGE" || b == "LIST") {
		// COLUMNS is required for lexicographic tuples and direct date/string keys.
		if len(src.Keys) > 1 || src.Keys[0].Column != "" {
			dst.Method = b + " COLUMNS"
		}
	}
	if b == "HASH" {
		if c.opt.SourceType != c.opt.TargetType {
			return fmt.Errorf("跨产品 HASH 算法不保证逐行分区归属相同；%s → %s 的精确路由转换尚未实现/验证", c.opt.SourceType, c.opt.TargetType)
		}
		if src.Algorithm == "" {
			return fmt.Errorf("源 HASH 算法没有完整身份，无法证明逐行路由等价")
		}
		if c.opt.SourceType == "mysql" {
			expected := "mysql-mod"
			if m == "LINEAR HASH" {
				expected = "mysql-linear-hash"
			}
			if src.Algorithm != expected {
				return fmt.Errorf("MySQL %s 算法 %q 与原生方法不一致或尚未实现/验证", m, src.Algorithm)
			}
		}
	}
	for i, k := range src.Keys {
		if k.Column != "" {
			if _, ok := c.column(k.Column); !ok {
				return fmt.Errorf("分区键列 %q 不在冻结表结构中", k.Column)
			}
			dst.Keys[i].Column = c.opt.ColumnName(k.Column)
			dst.Keys[i].Expression = ""
			if err := c.validateName(dst.Keys[i].Column); err != nil {
				return err
			}
		} else {
			if !c.profile.Features[FeatureExpressions] {
				return fmt.Errorf("目标原生表达式分区尚未实现/验证")
			}
			expr, err := c.expression(k.Expression)
			if err != nil {
				return err
			}
			dst.Keys[i].Expression = expr
		}
		if c.opt.SourceType != c.opt.TargetType && c.textKey(k) && !c.profile.Features[FeatureTextRouting] {
			return fmt.Errorf("字符分区键 %q 的排序、大小写和尾空格路由等价性尚未验证", k.Column)
		}
		if b == "RANGE" && c.nullable(k) && !c.sameNullRange() {
			return fmt.Errorf("可空 RANGE 分区键 %q 的 NULL 分区归属在源目标间不同；精确等价转换尚未实现/验证", k.Column)
		}
	}
	for i, p := range src.Partitions {
		d := &dst.Partitions[i]
		d.Name = c.opt.Name(p.Name)
		d.Schema = c.opt.TargetSchema
		if c.opt.TargetType == "sqlserver" {
			d.Name = strconv.Itoa(i + 1)
			d.Ordinal = i + 1
		}
		if d.Name == "" && c.opt.SourceType == "sqlserver" {
			d.Name = c.opt.Name(c.info.TableName + "_p" + strconv.Itoa(i+1))
		}
		if err := c.validateName(d.Name); err != nil {
			return err
		}
		nameKey := d.Name
		if c.profile.Features["postgres_declarative"] {
			nameKey = LeafKey(d.Schema, d.Name)
			if d.Name == c.opt.Name(c.info.TableName) {
				return fmt.Errorf("分区名 %q 与目标父表名冲突", d.Name)
			}
		}
		if c.names[nameKey] {
			return fmt.Errorf("分区名按现有大小写策略转换后冲突: %s", nameKey)
		}
		c.names[nameKey] = true
		if p.Default && !c.profile.Features[map[string]string{"RANGE": FeatureDefaultRange, "LIST": FeatureDefaultList}[b]] {
			return fmt.Errorf("目标 %s 的 %s DEFAULT 分区不能原生等价表达", c.opt.TargetType, b)
		}
		if err := validateValues(p, b, len(src.Keys)); err != nil {
			return fmt.Errorf("分区 %s: %w", p.Name, err)
		}
		if b == "RANGE" && !c.profile.Features["postgres_declarative"] && !c.profile.Features["native_sqlserver"] {
			if !p.Default && !p.LowerInclusive {
				return fmt.Errorf("分区 %s 的开下界不能原生等价表达为目标连续上界分区", p.Name)
			}
			if !p.Default && p.UpperInclusive {
				return fmt.Errorf("分区 %s 的闭上界到目标连续开上界转换尚未实现/验证", p.Name)
			}
			if i == 0 && !allKind(p.Lower, "minvalue") {
				return fmt.Errorf("目标连续 RANGE 无法保留源起始空洞，禁止扩大有效值域")
			}
			if i > 0 && !(src.Interval != nil && p.IsInterval) && !sameValues(src.Partitions[i-1].Upper, p.Lower) {
				return fmt.Errorf("目标连续 RANGE 无法保留分区 %s 前的空洞", p.Name)
			}
		}
		if p.Subpartition != nil {
			if !c.profile.Features[FeatureSubpartition] {
				return fmt.Errorf("目标 %s 的子分区适配尚未实现/验证", c.opt.TargetType)
			}
			if !c.profile.Features["postgres_declarative"] && depth > 0 {
				return fmt.Errorf("目标原生分区仅支持两级，不能压平源分区层级")
			}
			if err := c.prepare(d.Subpartition, p.Subpartition, depth+1, mapping); err != nil {
				return err
			}
		} else {
			key := LeafKey(d.Schema, d.Name)
			if c.leafKeys[key] {
				return fmt.Errorf("分区完整名称映射不唯一: %s", key)
			}
			c.leafKeys[key] = true
			ordinal := 0
			if c.opt.TargetType == "sqlserver" {
				ordinal = d.Ordinal
			}
			*mapping = append(*mapping, Mapping{SourceSchema: p.Schema, SourceName: p.Name, TargetSchema: d.Schema, TargetName: d.Name, TargetOrdinal: ordinal})
		}
	}
	if c.opt.TargetType == "oscar" && b == "LIST" {
		// Oscar requires DEFAULT last. LIST order does not change membership;
		// freeze the actual target order so native catalog verification agrees.
		defaults := 0
		for _, p := range dst.Partitions {
			if p.Default {
				defaults++
			}
		}
		if defaults > 1 {
			return fmt.Errorf("LIST 分区包含多个 DEFAULT，不能保持唯一原生路由")
		}
		sort.SliceStable(dst.Partitions, func(i, j int) bool { return !dst.Partitions[i].Default && dst.Partitions[j].Default })
		for i := range dst.Partitions {
			dst.Partitions[i].Ordinal = i + 1
		}
	}
	return nil
}

func (c *compiler) column(name string) (source.ColumnInfo, bool) {
	for _, v := range c.info.Columns {
		if v.Name == name {
			return v, true
		}
	}
	return source.ColumnInfo{}, false
}
func (c *compiler) nullable(k source.PartitionKey) bool {
	if k.Column == "" {
		if n, err := c.integerExpression(k.Expression); err == nil {
			return n.nullable
		}
		return true
	}
	v, ok := c.column(k.Column)
	return !ok || v.IsNullable
}
func (c *compiler) textKey(k source.PartitionKey) bool {
	dt := k.DataType
	if col, ok := c.column(k.Column); ok {
		dt = col.DataType
	}
	dt = strings.ToLower(dt)
	return strings.Contains(dt, "char") || strings.Contains(dt, "text") || strings.Contains(dt, "enum") || strings.Contains(dt, "set")
}
func (c *compiler) sameNullRange() bool {
	if c.opt.SourceType == c.opt.TargetType {
		return true
	}
	if c.opt.SourceType == "sqlserver" && c.opt.TargetType == "mysql" {
		// Both route NULL to the first leaf. SQL Server's exceptional explicit
		// NULL boundary remains blocked separately by validateValues.
		return true
	}
	if source.IsPGFamily(c.opt.SourceType) && c.profile.Features["postgres_declarative"] {
		return true
	}
	if (c.opt.SourceType == "oracle" || c.opt.SourceType == "dameng") && (c.opt.TargetType == "oracle" || c.opt.TargetType == "dameng" || c.opt.TargetType == "oscar") {
		return true
	}
	return false
}

func (c *compiler) validateName(name string) error {
	limit := 128
	switch c.opt.TargetType {
	case "mysql":
		limit = 64
	case "postgres", "gaussdb", "highgo", "vastbase", "gbase", "seabox", "kingbase":
		limit = 63
	case "oracle":
		limit = 30
	}
	if name == "" || !utf8.ValidString(name) || strings.ContainsAny(name, "\x00\r\n") || len(name) > limit {
		return fmt.Errorf("分区名 %q 不符合目标 %s 标识符限制（最多 %d 字节），禁止截断或自动改名", name, c.opt.TargetType, limit)
	}
	return nil
}

func validateValues(p source.Partition, b string, n int) error {
	if p.Default {
		return nil
	}
	if b == "RANGE" {
		if len(p.Lower) != n || len(p.Upper) != n {
			return fmt.Errorf("RANGE 分区完整上下边界数量与分区键不一致")
		}
		for _, vs := range [][]source.PartitionValue{p.Lower, p.Upper} {
			for _, v := range vs {
				if strings.EqualFold(v.Kind, "null") {
					return fmt.Errorf("RANGE NULL 边界不能自动转换")
				}
				if _, e := canonicalValue(v); e != nil {
					return e
				}
			}
		}
	}
	if b == "LIST" {
		if len(p.Values) == 0 {
			return fmt.Errorf("LIST 值列表为空")
		}
		for _, vs := range p.Values {
			if len(vs) != n {
				return fmt.Errorf("LIST 元组宽度与分区键不一致")
			}
			for _, v := range vs {
				if _, e := canonicalValue(v); e != nil {
					return e
				}
			}
		}
	}
	if b == "HASH" && (p.HashModulus <= 0 || p.HashRemainder < 0 || p.HashRemainder >= p.HashModulus) {
		return fmt.Errorf("HASH modulus/remainder 无效")
	}
	return nil
}

func allKind(values []source.PartitionValue, kind string) bool {
	if len(values) == 0 {
		return false
	}
	for _, v := range values {
		if !strings.EqualFold(v.Kind, kind) {
			return false
		}
	}
	return true
}
func sameValues(a, b []source.PartitionValue) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, e := canonicalValue(a[i])
		y, f := canonicalValue(b[i])
		if e != nil || f != nil || x != y {
			return false
		}
	}
	return true
}

func (c *compiler) expression(raw string) (string, error) {
	n, integerErr := c.integerExpression(raw)
	if integerErr == nil {
		return "(" + n.sql + ")", nil
	}
	ts, err := sqlsyntax.Lex(raw)
	if err != nil {
		return "", fmt.Errorf("分区表达式不能安全解析: %w", err)
	}
	for len(ts) > 2 && ts[0].Text == "(" && ts[len(ts)-1].Text == ")" {
		ts = ts[1 : len(ts)-1]
	}
	if len(ts) == 1 && (ts[0].Kind == 'q' || ts[0].Kind == 'i') {
		name := ts[0].Value
		if _, ok := c.column(name); ok {
			return c.quote(c.opt.ColumnName(name)), nil
		}
	}
	if c.opt.SourceType == "mysql" && len(ts) == 4 && ts[0].Kind == 'i' && ts[1].Text == "(" && (ts[2].Kind == 'q' || ts[2].Kind == 'i') && ts[3].Text == ")" {
		fn := strings.ToUpper(ts[0].Value)
		col, ok := c.column(ts[2].Value)
		if !ok {
			return "", fmt.Errorf("分区表达式引用了未知列")
		}
		if fn == "YEAR" || fn == "MONTH" || fn == "DAY" || fn == "DAYOFMONTH" {
			if strings.ToLower(col.DataType) != "date" && strings.ToLower(col.DataType) != "datetime" {
				return "", fmt.Errorf("%s 分区表达式的日期域/时区等价性尚未实现/验证", fn)
			}
			if fn == "DAYOFMONTH" {
				fn = "DAY"
			}
			if c.opt.TargetType == "mysql" {
				return fn + "(" + c.quote(c.opt.ColumnName(col.Name)) + ")", nil
			}
			if c.profile.Features["postgres_declarative"] {
				return "(EXTRACT(" + fn + " FROM " + c.quote(c.opt.ColumnName(col.Name)) + "))", nil
			}
		}
	}
	return "", fmt.Errorf("分区表达式 %q 不能严格等价转换: %v；禁止原样执行源 SQL", raw, integerErr)
}

func (c *compiler) quote(s string) string {
	switch c.opt.TargetType {
	case "mysql":
		return "`" + strings.ReplaceAll(s, "`", "``") + "`"
	case "sqlserver":
		return "[" + strings.ReplaceAll(s, "]", "]]") + "]"
	default:
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
}
func (c *compiler) qualified(schema, name string) string {
	if schema == "" {
		return c.quote(name)
	}
	return c.quote(schema) + "." + c.quote(name)
}
func (c *compiler) keys(s *source.PartitionSpec) string {
	out := make([]string, len(s.Keys))
	for i, k := range s.Keys {
		if k.Column != "" {
			out[i] = c.quote(k.Column)
		} else {
			out[i] = k.Expression
		}
	}
	return strings.Join(out, ",")
}

var sqlNumber = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)

func (c *compiler) value(v source.PartitionValue) (string, error) {
	canonical, err := canonicalValue(v)
	if err != nil {
		return "", err
	}
	v = canonical
	switch v.Kind {
	case "number":
		if !sqlNumber.MatchString(v.Text) {
			return "", fmt.Errorf("无效数字边界")
		}
		return v.Text, nil
	case "null":
		return "NULL", nil
	case "minvalue":
		return "MINVALUE", nil
	case "maxvalue":
		return "MAXVALUE", nil
	case "string", "date", "timestamp":
		if strings.ContainsRune(v.Text, '\x00') {
			return "", fmt.Errorf("分区字面量含 NUL，适配尚未实现/验证")
		}
		if c.opt.TargetType == "mysql" && strings.Contains(v.Text, `\`) {
			return "", fmt.Errorf("MySQL 含反斜杠分区边界依赖会话 SQL_MODE，适配尚未验证")
		}
		literal := "'" + strings.ReplaceAll(v.Text, "'", "''") + "'"
		if v.Kind == "date" && c.opt.TargetType != "mysql" && c.opt.TargetType != "sqlserver" {
			return "DATE " + literal, nil
		}
		if v.Kind == "timestamp" && c.opt.TargetType != "mysql" && c.opt.TargetType != "sqlserver" {
			return "TIMESTAMP " + literal, nil
		}
		if c.opt.TargetType == "sqlserver" && v.Kind == "string" {
			return "N" + literal, nil
		}
		return literal, nil
	}
	return "", fmt.Errorf("分区字面量尚未实现/验证")
}
func (c *compiler) tuple(vs []source.PartitionValue) (string, error) {
	out := make([]string, len(vs))
	for i, v := range vs {
		s, e := c.value(v)
		if e != nil {
			return "", e
		}
		out[i] = s
	}
	return strings.Join(out, ","), nil
}
