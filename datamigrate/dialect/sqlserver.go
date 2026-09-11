package dialect

import (
	"crypto/sha256"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/typemap"
	"dbgold/datamigrate/valueconv"
	"fmt"
	"math/big"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

type SQLServerDialect struct {
	Tables map[string]*source.TableDDLInfo
}

func NewSQLServer() *SQLServerDialect {
	return &SQLServerDialect{Tables: map[string]*source.TableDDLInfo{}}
}
func (d *SQLServerDialect) Name() string       { return "sqlserver" }
func (d *SQLServerDialect) Caps() Capabilities { return Capabilities{IdentityInsert: true} }
func (d *SQLServerDialect) QuoteIdent(s string) string {
	return "[" + strings.ReplaceAll(s, "]", "]]") + "]"
}
func (d *SQLServerDialect) QualifyTable(s, t string) string {
	if s == "" {
		s = "dbo"
	}
	return d.QuoteIdent(s) + "." + d.QuoteIdent(t)
}
func SQLServerLiteral(s string) string { return "N'" + strings.ReplaceAll(s, "'", "''") + "'" }
func ValidateSQLServerName(s string) error {
	if s == "" || !utf8.ValidString(s) || strings.ContainsRune(s, 0) || len(utf16.Encode([]rune(s))) > 128 {
		return fmt.Errorf("SQL Server 对象名 %q 必须为 1–128 个 UTF-16 单元", s)
	}
	return nil
}
func SQLServerObjectName(parts ...string) string {
	s := strings.Join(parts, "_")
	if ValidateSQLServerName(s) == nil {
		return s
	}
	h := sha256.Sum256([]byte(s))
	r := []rune(s)
	if len(r) > 48 {
		r = r[:48]
	}
	return fmt.Sprintf("%s_%x", string(r), h[:8])
}
func (d *SQLServerDialect) MapType(c source.ColumnInfo, src string, _ TypeOpt) string {
	if src != "mysql" {
		return ""
	}
	t, _ := typemap.SQLServerType(c)
	return t
}
func (d *SQLServerDialect) CreateTableStatements(schema string, info *source.TableDDLInfo, src string, opt TypeOpt, name NameFunc) ([]Statement, error) {
	if src != "mysql" {
		return nil, fmt.Errorf("SQL Server 目标仅支持 MySQL")
	}
	if opt.CharInLength || opt.UseNvarchar2 {
		return nil, fmt.Errorf("SQL Server 不支持 CHAR 长度单位/NVARCHAR2 选项")
	}
	if info == nil || len(info.Columns) == 0 || len(info.Columns) > 1024 {
		return nil, fmt.Errorf("SQL Server 表列数必须在 1–1024 之间")
	}
	if err := ValidateSQLServerName(name(info.TableName)); err != nil {
		return nil, err
	}
	if schema != "" {
		if err := ValidateSQLServerName(schema); err != nil {
			return nil, err
		}
	}
	out := []string{}
	seen := map[string]bool{}
	identities := 0
	for _, c := range info.Columns {
		n := name(c.Name)
		if e := ValidateSQLServerName(n); e != nil {
			return nil, e
		}
		if seen[n] {
			return nil, fmt.Errorf("列名冲突: %s", n)
		}
		seen[n] = true
		t, e := typemap.SQLServerType(c)
		if e != nil {
			return nil, e
		}
		extra := strings.ToLower(c.Extra)
		if c.Generated != "" || strings.Contains(extra, "virtual generated") || strings.Contains(extra, "stored generated") || strings.Contains(extra, "on update") {
			return nil, fmt.Errorf("列 %s 的生成列/自动更新规则无法等价迁移", c.Name)
		}
		s := d.QuoteIdent(n) + " " + t
		if strings.Contains(extra, "auto_increment") {
			identities++
			if identities > 1 {
				return nil, fmt.Errorf("SQL Server 每表只支持一个 IDENTITY")
			}
			s += " IDENTITY(1,1)"
		} else {
			def, e := sqlServerDefault(c)
			if e != nil {
				return nil, e
			}
			s += def
		}
		if c.IsNullable {
			s += " NULL"
		} else {
			s += " NOT NULL"
		}
		out = append(out, s)
	}
	q := d.QualifyTable(schema, name(info.TableName))
	return []Statement{{SQL: "IF OBJECT_ID(" + SQLServerLiteral(q) + ", N'U') IS NOT NULL DROP TABLE " + q}, {SQL: "CREATE TABLE " + q + " (\n  " + strings.Join(out, ",\n  ") + "\n)"}}, nil
}
func sqlServerDefault(c source.ColumnInfo) (string, error) {
	if c.Default == nil {
		return "", nil
	}
	v := *c.Default
	dt := strings.ToLower(c.DataType)
	if (dt == "datetime" || dt == "timestamp") && oracleNowDefault.MatchString(v) {
		return " DEFAULT SYSDATETIME()", nil
	}
	if strings.Contains(strings.ToLower(c.Extra), "default_generated") {
		return "", fmt.Errorf("列 %s: 不支持默认表达式 %q", c.Name, v)
	}
	if strings.ContainsRune(v, 0) {
		return "", fmt.Errorf("列 %s: 不支持 NUL 默认值", c.Name)
	}
	var raw interface{} = v
	if dt == "bit" && len(v) > 3 && (strings.HasPrefix(v, "b'") || strings.HasPrefix(v, "B'")) && strings.HasSuffix(v, "'") {
		n, ok := new(big.Int).SetString(v[2:len(v)-1], 2)
		if !ok {
			return "", fmt.Errorf("无效 BIT 默认值")
		}
		raw = n.String()
	}
	if dt == "binary" || dt == "varbinary" || strings.Contains(dt, "blob") {
		return "", fmt.Errorf("列 %s: 二进制默认值暂不支持", c.Name)
	}
	converted, e := valueconv.ConvertSQLServer(raw, c)
	if e != nil {
		return "", fmt.Errorf("默认值: %w", e)
	}
	if b, ok := converted.(bool); ok {
		if b {
			return " DEFAULT 1", nil
		}
		return " DEFAULT 0", nil
	}
	return " DEFAULT " + SQLServerLiteral(fmt.Sprint(converted)), nil
}
func (d *SQLServerDialect) Column(table, col string) (source.ColumnInfo, bool) {
	if t := d.Tables[table]; t != nil {
		for _, c := range t.Columns {
			if c.Name == col {
				return c, true
			}
		}
	}
	return source.ColumnInfo{}, false
}
func (d *SQLServerDialect) ValidateIndex(i source.IndexInfo) error {
	if i.Unsupported != "" {
		return fmt.Errorf("%s", i.Unsupported)
	}
	if len(i.Columns) == 0 || len(i.Columns) > 16 {
		return fmt.Errorf("SQL Server 2008 R2 索引键必须为 1–16 列")
	}
	if e := ValidateSQLServerName(i.IndexName); e != nil {
		return e
	}
	bytes := int64(0)
	for _, n := range i.Columns {
		c, ok := d.Column(i.TableName, n)
		if !ok {
			return fmt.Errorf("索引列 %s.%s 不存在", i.TableName, n)
		}
		t, e := typemap.SQLServerType(c)
		if e != nil {
			return e
		}
		switch {
		case strings.Contains(t, "MAX"):
			return fmt.Errorf("索引列 %s 的 MAX 类型不能作为索引键", n)
		case strings.HasPrefix(t, "NVARCHAR"):
			bytes += c.Length * 4
		case strings.HasPrefix(t, "VARBINARY"):
			bytes += c.Length
		case strings.HasPrefix(t, "DECIMAL"):
			bytes += 17
		default:
			bytes += 8
		}
		if i.IsPrimary && c.IsNullable {
			return fmt.Errorf("主键列 %s 可空", n)
		}
	}
	if bytes > 900 {
		return fmt.Errorf("索引最大键宽 %d 超过 SQL Server 2008 R2 的 900 字节限制", bytes)
	}
	return nil
}
func (d *SQLServerDialect) IndexStatements(schema string, i source.IndexInfo) []Statement {
	cols := []string{}
	filters := []string{}
	for _, n := range i.Columns {
		cols = append(cols, d.QuoteIdent(n))
		if c, ok := d.Column(i.TableName, n); ok && c.IsNullable {
			filters = append(filters, d.QuoteIdent(n)+" IS NOT NULL")
		}
	}
	q := d.QualifyTable(schema, i.TableName)
	key := strings.Join(cols, ", ")
	if i.IsPrimary {
		return []Statement{{SQL: "ALTER TABLE " + q + " ADD CONSTRAINT " + d.QuoteIdent(SQLServerObjectName(i.TableName, i.IndexName)) + " PRIMARY KEY (" + key + ")"}}
	}
	unique := ""
	where := ""
	if i.IsUnique {
		unique = "UNIQUE "
		if len(filters) > 0 {
			where = " WHERE " + strings.Join(filters, " AND ")
		}
	}
	return []Statement{{SQL: "CREATE " + unique + "NONCLUSTERED INDEX " + d.QuoteIdent(i.IndexName) + " ON " + q + " (" + key + ")" + where}}
}
func (d *SQLServerDialect) ValidateForeignKey(f source.FKInfo) error {
	if f.Unsupported != "" {
		return fmt.Errorf("%s", f.Unsupported)
	}
	if len(f.Columns) == 0 || len(f.Columns) != len(f.RefColumns) {
		return fmt.Errorf("外键列定义不完整")
	}
	for _, a := range []string{f.OnDelete, f.OnUpdate} {
		switch strings.ToUpper(a) {
		case "", "RESTRICT", "NO ACTION", "CASCADE", "SET NULL":
		default:
			return fmt.Errorf("不支持外键动作 %s", a)
		}
	}
	for j, n := range f.Columns {
		c, ok := d.Column(f.TableName, n)
		if !ok {
			return fmt.Errorf("外键列不存在: %s.%s", f.TableName, n)
		}
		r, ok := d.Column(f.RefTable, f.RefColumns[j])
		if !ok {
			return fmt.Errorf("引用列不存在: %s.%s", f.RefTable, f.RefColumns[j])
		}
		ct, _ := typemap.SQLServerType(c)
		rt, _ := typemap.SQLServerType(r)
		if ct != rt {
			return fmt.Errorf("外键列类型不一致: %s / %s", ct, rt)
		}
		if !c.IsNullable && (f.OnDelete == "SET NULL" || f.OnUpdate == "SET NULL") {
			return fmt.Errorf("SET NULL 外键列必须可空")
		}
	}
	return ValidateSQLServerName(f.ConstraintName)
}
func (d *SQLServerDialect) ForeignKeyStatements(schema string, f source.FKInfo) []Statement {
	cols := []string{}
	refs := []string{}
	for _, n := range f.Columns {
		cols = append(cols, d.QuoteIdent(n))
	}
	for _, n := range f.RefColumns {
		refs = append(refs, d.QuoteIdent(n))
	}
	s := "ALTER TABLE " + d.QualifyTable(schema, f.TableName) + " WITH CHECK ADD CONSTRAINT " + d.QuoteIdent(f.ConstraintName) + " FOREIGN KEY (" + strings.Join(cols, ", ") + ") REFERENCES " + d.QualifyTable(schema, f.RefTable) + " (" + strings.Join(refs, ", ") + ")"
	for _, a := range [][2]string{{"DELETE", f.OnDelete}, {"UPDATE", f.OnUpdate}} {
		rule := strings.ToUpper(a[1])
		if rule == "" || rule == "RESTRICT" {
			rule = "NO ACTION"
		}
		s += " ON " + a[0] + " " + rule
	}
	return []Statement{{SQL: s}}
}
func (d *SQLServerDialect) SequenceStatements(schema string, s source.SequenceInfo, _ string) []Statement {
	start := s.StartValueText
	if start == "" {
		start = fmt.Sprint(s.StartValue)
	}
	n, ok := new(big.Int).SetString(start, 10)
	if !ok || n.Sign() < 0 {
		return nil
	}
	q := d.QualifyTable(schema, s.TableName)
	// Fresh empty tables use RESEED itself as the next value. Populated tables
	// increment it; StartValueText avoids overflowing unsigned BIGINT seeds.
	limitSQL := ""
	if c, ok := d.Column(s.TableName, s.ColumnName); ok {
		bits := map[string]uint{"tinyint": 8, "smallint": 16, "mediumint": 24, "int": 32, "integer": 32, "bigint": 64}[strings.ToLower(c.DataType)]
		if bits > 0 {
			if !strings.Contains(strings.ToLower(c.FormattedType), "unsigned") {
				bits--
			}
			limit := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), bits), big.NewInt(1))
			limitSQL = " IF @next > " + limit.String() + " BEGIN RAISERROR (N'IDENTITY next value exceeds source type range',16,1); RETURN; END;"
		}
	}
	sql := "DECLARE @next decimal(38,0) = " + n.String() + ", @max decimal(38,0), @seed decimal(38,0); SELECT @max = MAX(CAST(" + d.QuoteIdent(s.ColumnName) + " AS decimal(38,0))) FROM " + q + "; IF @max IS NOT NULL AND @next <= @max SET @next = @max + 1;" + limitSQL + " SET @seed = CASE WHEN @max IS NULL AND (SELECT last_value FROM sys.identity_columns WHERE object_id=OBJECT_ID(" + SQLServerLiteral(q) + ")) IS NULL THEN @next ELSE @next - 1 END; DBCC CHECKIDENT (" + SQLServerLiteral(q) + ", RESEED, @seed) WITH NO_INFOMSGS"
	return []Statement{{SQL: sql}}
}
func (d *SQLServerDialect) ViewStatements(schema string, v source.ViewInfo) []Statement {
	q := d.QualifyTable(schema, v.ViewName)
	cols := []string{}
	for _, c := range v.Columns {
		cols = append(cols, d.QuoteIdent(c.Name))
	}
	header := q
	if len(cols) > 0 {
		header += " (" + strings.Join(cols, ", ") + ")"
	}
	return []Statement{{SQL: "IF OBJECT_ID(" + SQLServerLiteral(q) + ", N'V') IS NOT NULL DROP VIEW " + q}, {SQL: "CREATE VIEW " + header + " AS\n" + v.Definition}}
}
func (d *SQLServerDialect) AdjustViewDefinition(s string) string { return s }
func (d *SQLServerDialect) CommentStatements(schema string, c source.CommentInfo) []Statement {
	if schema == "" {
		schema = "dbo"
	}
	args := ", @level0type=N'SCHEMA', @level0name=" + SQLServerLiteral(schema) + ", @level1type=N'TABLE', @level1name=" + SQLServerLiteral(c.TableName)
	minor := "0"
	if c.ColumnName != "" {
		args += ", @level2type=N'COLUMN', @level2name=" + SQLServerLiteral(c.ColumnName)
		minor = "COLUMNPROPERTY(OBJECT_ID(" + SQLServerLiteral(d.QualifyTable(schema, c.TableName)) + "), " + SQLServerLiteral(c.ColumnName) + ", 'ColumnId')"
	}
	exists := "SELECT 1 FROM sys.extended_properties WHERE class=1 AND major_id=OBJECT_ID(" + SQLServerLiteral(d.QualifyTable(schema, c.TableName)) + ") AND minor_id=" + minor + " AND name=N'MS_Description'"
	value := " @name=N'MS_Description', @value=" + SQLServerLiteral(c.Comment) + args
	return []Statement{{SQL: "IF EXISTS (" + exists + ") EXEC sys.sp_updateextendedproperty" + value + " ELSE EXEC sys.sp_addextendedproperty" + value}}
}
