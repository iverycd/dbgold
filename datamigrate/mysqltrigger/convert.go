package mysqltrigger

import (
	"crypto/sha256"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"dbgold/datamigrate/source"
	"github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	_ "github.com/pingcap/tidb/pkg/parser/test_driver"
)

type converter struct {
	info                  source.TriggerInfo
	tables                map[string]*source.TableDDLInfo
	opt                   Options
	parser                *parser.Parser
	tokens                []token
	pos                   int
	deps                  map[string]*source.TableDDLInfo
	version               int
	characterColumns      map[string]source.TriggerColumnCharacter
	characterProfiles     map[string]source.TriggerCollationProfile
	numericColumns        map[string]source.TriggerNumericColumn
	requiresTextSemantics bool
}

func quote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// Convert never executes SQL. Unknown syntax produces an adaptation error and
// no executable plan, including syntax hidden inside otherwise valid DML.
func Convert(info source.TriggerInfo, tables map[string]*source.TableDDLInfo, opt Options) (Plan, error) {
	return convertVersion(info, tables, opt, CurrentVersion)
}

// Version one remains the byte-for-byte generator for plans saved before v2.
// New syntax and collation rules must only be added behind the v2 branch.
func convertVersion(info source.TriggerInfo, tables map[string]*source.TableDDLInfo, opt Options, version int) (Plan, error) {
	var empty Plan
	if info.MetadataError != "" {
		return empty, fmt.Errorf("%s", info.MetadataError)
	}
	if info.Name == "" || info.TableName == "" || info.Schema == "" || opt.Schema == "" {
		return empty, fmt.Errorf("缺少触发器名称、源 schema、基表或目标 schema")
	}
	if !validIdent(info.Name) || !validIdent(opt.Schema) {
		return empty, fmt.Errorf("触发器或 schema 名称无效或超过 63 字节")
	}
	info.Timing = strings.ToUpper(info.Timing)
	info.Event = strings.ToUpper(info.Event)
	if info.Timing != "BEFORE" && info.Timing != "AFTER" {
		return empty, fmt.Errorf("不支持触发时机 %s", info.Timing)
	}
	if info.Event != "INSERT" && info.Event != "UPDATE" && info.Event != "DELETE" {
		return empty, fmt.Errorf("不支持触发事件 %s", info.Event)
	}
	mode, err := savedMode(info.SQLMode)
	if err != nil {
		return empty, err
	}
	if opt.Name == nil {
		opt.Name = func(s string) string { return s }
	}
	if err = checkNames(tables, opt.Name); err != nil {
		return empty, err
	}
	c := &converter{info: info, tables: tables, opt: opt, parser: parser.New(), deps: map[string]*source.TableDDLInfo{}, version: version,
		characterColumns: map[string]source.TriggerColumnCharacter{}, characterProfiles: map[string]source.TriggerCollationProfile{}, numericColumns: map[string]source.TriggerNumericColumn{}}
	c.parser.SetSQLMode(mode)
	if _, err = c.table(info.Schema, info.TableName); err != nil {
		return empty, err
	}
	c.tokens, err = lex(info.Body, mode.HasANSIQuotesMode(), mode.HasNoBackslashEscapesMode())
	if err != nil {
		return empty, err
	}
	for _, tok := range c.tokens {
		if tok.raw == "{" || tok.raw == "}" {
			return empty, fmt.Errorf("ODBC 花括号语法需要人工适配")
		}
	}
	if len(c.tokens) == 0 {
		return empty, fmt.Errorf("触发器正文为空")
	}
	body, err := c.statement()
	if err != nil {
		return empty, err
	}
	if c.at(";") {
		c.pos++
	}
	if c.pos != len(c.tokens) {
		return empty, fmt.Errorf("触发器正文包含未支持的后续语句")
	}
	p := Plan{Version: version, Source: info, Schema: opt.Schema, Table: opt.Name(info.TableName), Name: opt.Name(info.Name)}
	if !validIdent(p.Table) || !validIdent(p.Name) {
		return empty, fmt.Errorf("转换后对象名无效或超过 63 字节")
	}
	sum := sha256.Sum256([]byte(info.Schema + "\x00" + info.TableName + "\x00" + info.Name + "\x00" + p.Schema + "\x00" + p.Table + "\x00" + p.Name))
	p.FunctionName = fmt.Sprintf("dbgold_trg_%x", sum[:16])
	ret := "NEW"
	if info.Event == "DELETE" {
		ret = "OLD"
	}
	marker := ToolMarker
	if version == 1 {
		marker = legacyToolMarker
	}
	p.FunctionBody = "\n-- " + marker + "\nBEGIN\n" + body + "RETURN " + ret + ";\nEND;\n"
	p.FunctionSQL = functionSQL(p)
	p.TriggerSQL = triggerSQL(p)
	for name := range c.deps {
		p.Dependencies = append(p.Dependencies, name)
	}
	sort.Strings(p.Dependencies)
	for _, name := range p.Dependencies {
		t := *c.deps[name]
		t.Columns = append([]source.ColumnInfo(nil), t.Columns...)
		p.SourceTables = append(p.SourceTables, t)
	}
	p.LowerCaseNames = opt.Name("DbGoldMixed") == "dbgoldmixed"
	p.CharacterMetadata = c.frozenCharacterMetadata()
	p.RequiresTextSemantics = c.requiresTextSemantics
	return p, nil
}

func validIdent(s string) bool { return s != "" && len(s) <= 63 && !strings.ContainsRune(s, 0) }
func functionSQL(p Plan) string {
	delim := "$dbgold$"
	for i := 1; strings.Contains(p.FunctionBody, delim); i++ {
		delim = fmt.Sprintf("$dbgold_%d$", i)
	}
	return "CREATE FUNCTION " + quote(p.Schema) + "." + quote(p.FunctionName) + "() RETURNS trigger LANGUAGE plpgsql SECURITY INVOKER AS " + delim + p.FunctionBody + delim + ";"
}
func triggerSQL(p Plan) string {
	return "CREATE TRIGGER " + quote(p.Name) + " " + p.Source.Timing + " " + p.Source.Event + " ON " + quote(p.Schema) + "." + quote(p.Table) + " FOR EACH ROW EXECUTE PROCEDURE " + quote(p.Schema) + "." + quote(p.FunctionName) + "();"
}

// ValidatePlan recreates the restricted translation from frozen source metadata.
// It does not treat user-supplied SQL or a marker alone as proof of provenance.
func ValidatePlan(p Plan) error {
	tables := map[string]*source.TableDDLInfo{}
	for i := range p.SourceTables {
		t := &p.SourceTables[i]
		if _, ok := tables[t.TableName]; ok {
			return fmt.Errorf("重复的源表快照")
		}
		tables[t.TableName] = t
	}
	version := p.Version
	if version == 0 {
		version = 1
	}
	if version != 1 && version != CurrentVersion {
		return fmt.Errorf("不支持触发器计划版本 %d", p.Version)
	}
	if version == 1 && p.CharacterMetadata != nil {
		return fmt.Errorf("旧版触发器计划不支持字符比较配置")
	}
	op := Options{Schema: p.Schema, CharacterMetadata: p.CharacterMetadata}
	if p.LowerCaseNames {
		op.Name = strings.ToLower
	}
	regenerated, err := convertVersion(p.Source, tables, op, version)
	if err != nil {
		return err
	}
	if p.Table != regenerated.Table || p.Name != regenerated.Name || p.FunctionName != regenerated.FunctionName || p.FunctionSQL != regenerated.FunctionSQL || p.TriggerSQL != regenerated.TriggerSQL || p.FunctionBody != regenerated.FunctionBody || !reflect.DeepEqual(p.Dependencies, regenerated.Dependencies) || !reflect.DeepEqual(p.CharacterMetadata, regenerated.CharacterMetadata) || p.RequiresTextSemantics != regenerated.RequiresTextSemantics {
		return fmt.Errorf("触发器计划与冻结的源定义不一致")
	}
	return nil
}

func savedMode(s string) (mysql.SQLMode, error) {
	allowed := map[string]bool{"": true, "STRICT_TRANS_TABLES": true, "STRICT_ALL_TABLES": true, "NO_ZERO_IN_DATE": true, "NO_ZERO_DATE": true, "ERROR_FOR_DIVISION_BY_ZERO": true, "NO_ENGINE_SUBSTITUTION": true, "ONLY_FULL_GROUP_BY": true, "ANSI_QUOTES": true, "NO_BACKSLASH_ESCAPES": true, "IGNORE_SPACE": true, "NO_AUTO_CREATE_USER": true}
	var parts []string
	for _, m := range strings.Split(strings.ToUpper(s), ",") {
		m = strings.TrimSpace(m)
		if !allowed[m] {
			return 0, fmt.Errorf("SQL_MODE %s 需要人工适配", m)
		}
		parts = append(parts, m)
	}
	return mysql.GetSQLMode(strings.Join(parts, ","))
}

func checkNames(tables map[string]*source.TableDDLInfo, name func(string) string) error {
	seen := map[string]string{}
	for key, t := range tables {
		if t == nil || key != t.TableName {
			return fmt.Errorf("源表快照名称不一致")
		}
		mapped := name(t.TableName)
		if old, ok := seen[mapped]; ok && old != key {
			return fmt.Errorf("源表 %s 与 %s 转换后名称冲突", old, key)
		}
		seen[mapped] = key
		cols := map[string]bool{}
		for _, col := range t.Columns {
			mapped = name(col.Name)
			if cols[mapped] {
				return fmt.Errorf("表 %s 的列名转换后冲突", key)
			}
			cols[mapped] = true
		}
	}
	return nil
}

func (c *converter) at(s string) bool {
	return c.pos < len(c.tokens) && (c.tokens[c.pos].word == s || c.tokens[c.pos].raw == s)
}
func (c *converter) take(s string) error {
	if !c.at(s) {
		return fmt.Errorf("触发器语法需要 %s", s)
	}
	c.pos++
	return nil
}
func (c *converter) sequence(stops ...string) (string, error) {
	var out strings.Builder
	for c.pos < len(c.tokens) {
		stop := false
		for _, s := range stops {
			if c.at(s) {
				stop = true
			}
		}
		if stop {
			return out.String(), nil
		}
		s, err := c.statement()
		if err != nil {
			return "", err
		}
		out.WriteString(s)
		if err = c.take(";"); err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("触发器块未闭合")
}
func (c *converter) statement() (string, error) {
	if c.at("BEGIN") {
		c.pos++
		body, err := c.sequence("END")
		if err != nil {
			return "", err
		}
		if err = c.take("END"); err != nil {
			return "", err
		}
		return body, nil
	}
	if c.at("IF") {
		return c.ifStatement()
	}
	start := c.pos
	depth := 0
	for c.pos < len(c.tokens) {
		t := c.tokens[c.pos]
		if t.raw == ";" && depth == 0 {
			break
		}
		if t.raw == "(" {
			depth++
		}
		if t.raw == ")" {
			depth--
			if depth < 0 {
				return "", fmt.Errorf("括号未匹配")
			}
		}
		c.pos++
	}
	if depth != 0 || start == c.pos {
		return "", fmt.Errorf("触发器语句为空或括号未匹配")
	}
	tokens := c.tokens[start:c.pos]
	if tokens[0].word == "SET" {
		return c.setNew(tokens)
	}
	if tokens[0].word != "INSERT" && tokens[0].word != "UPDATE" && tokens[0].word != "DELETE" {
		return "", fmt.Errorf("语句 %s 需要人工适配", tokens[0].raw)
	}
	return c.dml(c.info.Body[tokens[0].start:tokens[len(tokens)-1].end])
}
func (c *converter) ifStatement() (string, error) {
	c.pos++
	var out strings.Builder
	prefix := "IF"
	for {
		start := c.pos
		depth := 0
		for c.pos < len(c.tokens) {
			if depth == 0 && c.at("THEN") {
				break
			}
			if c.at("(") {
				depth++
			}
			if c.at(")") {
				depth--
			}
			c.pos++
		}
		if start == c.pos || c.pos == len(c.tokens) {
			return "", fmt.Errorf("IF 缺少条件或 THEN")
		}
		e, err := c.expression(c.info.Body[c.tokens[start].start:c.tokens[c.pos-1].end], nil)
		if err != nil {
			return "", err
		}
		if e.kind != kindBool {
			return "", fmt.Errorf("IF 条件必须是明确的布尔表达式")
		}
		c.pos++
		out.WriteString(prefix + " " + e.sql + " THEN\n")
		body, err := c.sequence("ELSEIF", "ELSE", "END")
		if err != nil {
			return "", err
		}
		out.WriteString(body)
		if c.at("ELSEIF") {
			c.pos++
			prefix = "ELSIF"
			continue
		}
		break
	}
	if c.at("ELSE") {
		c.pos++
		body, err := c.sequence("END")
		if err != nil {
			return "", err
		}
		out.WriteString("ELSE\n" + body)
	}
	if err := c.take("END"); err != nil {
		return "", err
	}
	if err := c.take("IF"); err != nil {
		return "", err
	}
	out.WriteString("END IF;\n")
	return out.String(), nil
}
func (c *converter) setNew(ts []token) (string, error) {
	if len(ts) < 6 || ts[1].word != "NEW" || ts[2].raw != "." || ts[4].raw != "=" {
		return "", fmt.Errorf("仅支持 SET NEW.列 = 表达式")
	}
	if c.info.Timing != "BEFORE" || c.info.Event == "DELETE" {
		return "", fmt.Errorf("仅 BEFORE INSERT/UPDATE 可修改 NEW")
	}
	name, err := identifier(ts[3])
	if err != nil {
		return "", err
	}
	col, err := c.column(c.tables[c.info.TableName], name)
	if err != nil {
		return "", err
	}
	if strings.Contains(strings.ToLower(col.Extra), "auto_increment") || generated(col) {
		return "", fmt.Errorf("自增列或生成列赋值需要人工适配")
	}
	e, err := c.expression(c.info.Body[ts[5].start:ts[len(ts)-1].end], nil)
	if err != nil {
		return "", err
	}
	if e, err = assignColumn(col, e); err != nil {
		return "", err
	}
	return "NEW." + quote(c.opt.Name(col.Name)) + " := " + e.sql + ";\n", nil
}

func (c *converter) table(schema, name string) (*source.TableDDLInfo, error) {
	if schema != "" && schema != c.info.Schema {
		return nil, fmt.Errorf("跨库引用 %s.%s 需要人工适配", schema, name)
	}
	t := c.tables[name]
	if t == nil {
		return nil, fmt.Errorf("依赖表 %s 未纳入本次迁移或元数据不可用", name)
	}
	mapped := c.opt.Name(t.TableName)
	if !validIdent(mapped) {
		return nil, fmt.Errorf("目标表名无效或超过 63 字节")
	}
	if old := c.deps[mapped]; old != nil && old.TableName != name {
		return nil, fmt.Errorf("目标表名大小写转换后冲突")
	}
	c.deps[mapped] = t
	return t, nil
}
func (c *converter) column(t *source.TableDDLInfo, name string) (*source.ColumnInfo, error) {
	if t == nil {
		return nil, fmt.Errorf("列 %s 缺少表上下文", name)
	}
	var found *source.ColumnInfo
	for i := range t.Columns {
		if strings.EqualFold(t.Columns[i].Name, name) {
			if found != nil {
				return nil, fmt.Errorf("列名 %s 大小写匹配不唯一", name)
			}
			found = &t.Columns[i]
		}
	}
	if found == nil {
		return nil, fmt.Errorf("表 %s 缺少列 %s", t.TableName, name)
	}
	if !validIdent(c.opt.Name(found.Name)) {
		return nil, fmt.Errorf("目标列名无效或超过 63 字节")
	}
	if generated(found) {
		return nil, fmt.Errorf("生成列 %s 需要人工适配", found.Name)
	}
	return found, nil
}

func generated(col *source.ColumnInfo) bool {
	extra := strings.ToUpper(col.Extra)
	return col.Generated != "" || strings.Contains(extra, "VIRTUAL GENERATED") || strings.Contains(extra, "STORED GENERATED")
}

func (c *converter) parse(s string) (ast.StmtNode, error) {
	n, err := c.parser.ParseOneStmt(s, "utf8mb4", "utf8mb4_bin")
	if err != nil {
		return nil, fmt.Errorf("MySQL 语句无法安全解析: %w", err)
	}
	return n, nil
}
