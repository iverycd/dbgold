package mysqldmtrigger

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"dbgold/datamigrate/source"
	"github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	_ "github.com/pingcap/tidb/pkg/parser/test_driver"
)

type variable struct {
	name, typ string
	kind      valueKind
}
type label struct {
	source, target string
	loop           bool
}
type converter struct {
	info        source.TriggerInfo
	tables      map[string]*source.TableDDLInfo
	opt         Options
	parser      *parser.Parser
	tokens      []token
	pos, serial int
	scopes      []map[string]variable
	labels      []label
	deps        map[string]*source.TableDDLInfo
	mode        mysql.SQLMode
}

func validName(s string) bool {
	return s != "" && utf8.ValidString(s) && len(s) <= 128 && !strings.ContainsRune(s, 0)
}

func savedMode(s string) (mysql.SQLMode, error) {
	allowed := map[string]bool{"": true, "STRICT_TRANS_TABLES": true, "STRICT_ALL_TABLES": true, "NO_ZERO_IN_DATE": true, "NO_ZERO_DATE": true, "ERROR_FOR_DIVISION_BY_ZERO": true, "NO_ENGINE_SUBSTITUTION": true, "ONLY_FULL_GROUP_BY": true, "ANSI_QUOTES": true, "NO_BACKSLASH_ESCAPES": true, "IGNORE_SPACE": true, "NO_AUTO_CREATE_USER": true}
	var modes []string
	for _, m := range strings.Split(strings.ToUpper(s), ",") {
		m = strings.TrimSpace(m)
		if !allowed[m] {
			return 0, fmt.Errorf("SQL_MODE %s 需要人工适配", m)
		}
		modes = append(modes, m)
	}
	return mysql.GetSQLMode(strings.Join(modes, ","))
}

// Convert returns either a complete immutable program or no executable SQL.
func Convert(info source.TriggerInfo, tables map[string]*source.TableDDLInfo, opt Options) (Plan, error) {
	var empty Plan
	if info.MetadataError != "" {
		return empty, fmt.Errorf("%s", info.MetadataError)
	}
	if !validName(info.Name) || !validName(info.TableName) || !validName(info.Schema) || !validName(opt.Schema) {
		return empty, fmt.Errorf("触发器名称、源 schema、基表或目标 schema 缺失或无效")
	}
	if info.SourceDDL == "" {
		return empty, fmt.Errorf("缺少可核对的完整源 CREATE TRIGGER")
	}
	timing, event := strings.ToUpper(info.Timing), strings.ToUpper(info.Event)
	if timing != "BEFORE" && timing != "AFTER" {
		return empty, fmt.Errorf("不支持触发时机 %s", info.Timing)
	}
	if event != "INSERT" && event != "UPDATE" && event != "DELETE" {
		return empty, fmt.Errorf("不支持触发事件 %s", info.Event)
	}
	mode, err := savedMode(info.SQLMode)
	if err != nil {
		return empty, err
	}
	// Copy options before freezing: callers may reuse and mutate their maps.
	b, err := json.Marshal(opt)
	if err != nil {
		return empty, err
	}
	var frozenOptions Options
	if err = json.Unmarshal(b, &frozenOptions); err != nil {
		return empty, err
	}
	opt = frozenOptions
	opt.Schema = strings.ToUpper(opt.Schema)
	opt.Predecessor = strings.ToUpper(opt.Predecessor)
	for name, sqlText := range opt.PredecessorSQLs {
		if name != strings.ToUpper(name) || !validName(name) || !strings.HasPrefix(sqlText, "CREATE TRIGGER "+quote(opt.Schema)+"."+quote(name)+" ") || !strings.Contains(sqlText, ToolMarker) {
			return empty, fmt.Errorf("前驱触发器 %s 的冻结定义身份不一致", name)
		}
	}
	c := &converter{info: info, tables: tables, opt: opt, parser: parser.New(), deps: map[string]*source.TableDDLInfo{}, mode: mode}
	c.parser.SetSQLMode(mode)
	if err = c.validateMappings(); err != nil {
		return empty, err
	}
	if _, err = c.table(info.Schema, info.TableName); err != nil {
		return empty, err
	}
	c.tokens, err = lex(info.Body, mode.HasANSIQuotesMode(), mode.HasNoBackslashEscapesMode())
	if err != nil {
		return empty, err
	}
	if len(c.tokens) == 0 {
		return empty, fmt.Errorf("触发器正文为空")
	}
	c.scopes = []map[string]variable{{}}
	body, err := c.statement()
	if err != nil {
		return empty, c.positionError(err)
	}
	if c.at(";") {
		c.pos++
	}
	if c.pos != len(c.tokens) {
		return empty, c.positionError(fmt.Errorf("触发器包含未处理的后续语句"))
	}
	p := Plan{Version: CurrentVersion, Source: info, Schema: opt.Schema, Table: c.tableName(info.TableName), Name: strings.ToUpper(info.Name), Predecessor: opt.Predecessor, Options: opt}
	if !validName(p.Table) || !validName(p.Name) || p.Predecessor != "" && !validName(p.Predecessor) {
		return empty, fmt.Errorf("目标对象名无效")
	}
	if p.Name == p.Predecessor {
		return empty, fmt.Errorf("触发器不能以自身为前驱")
	}
	ordering := ""
	if p.Predecessor != "" {
		ordering = " FOLLOWS " + quote(p.Schema) + "." + quote(p.Predecessor)
	}
	p.TriggerSQL = "CREATE TRIGGER " + quote(p.Schema) + "." + quote(p.Name) + " " + timing + " " + event + " ON " + quote(p.Schema) + "." + quote(p.Table) + " FOR EACH ROW" + ordering + "\nBEGIN\n  /* " + ToolMarker + " */\n" + body + "END;"
	for name := range c.deps {
		p.Dependencies = append(p.Dependencies, name)
	}
	sort.Strings(p.Dependencies)
	for _, target := range p.Dependencies {
		original := c.deps[target]
		var snapshot source.TableDDLInfo
		b, _ := json.Marshal(original)
		if err = json.Unmarshal(b, &snapshot); err != nil {
			return empty, err
		}
		p.SourceTables = append(p.SourceTables, snapshot)
	}
	return p, nil
}

func (c *converter) positionError(err error) error {
	offset := len(c.info.Body)
	if c.pos < len(c.tokens) {
		offset = c.tokens[c.pos].start
	}
	line := 1 + strings.Count(c.info.Body[:offset], "\n")
	return fmt.Errorf("触发器 %s 第 %d 行（字节 %d）: %w", c.info.Name, line, offset+1, err)
}
func (c *converter) at(s string) bool {
	return c.pos < len(c.tokens) && (c.tokens[c.pos].word == s || c.tokens[c.pos].raw == s)
}
func (c *converter) take(s string) error {
	if !c.at(s) {
		return fmt.Errorf("语法需要 %s", s)
	}
	c.pos++
	return nil
}
func (c *converter) fresh(prefix string) string {
	c.serial++
	return fmt.Sprintf("DBGOLD_%s_%d", prefix, c.serial)
}
func (c *converter) lookup(name string) (variable, bool) {
	for i := len(c.scopes) - 1; i >= 0; i-- {
		if v, ok := c.scopes[i][strings.ToLower(name)]; ok {
			return v, true
		}
	}
	return variable{}, false
}
func (c *converter) parse(s string) (ast.StmtNode, error) {
	n, e := c.parser.ParseOneStmt(s, "utf8mb4", "utf8mb4_bin")
	if e != nil {
		return nil, fmt.Errorf("MySQL SQL 无法安全解析: %w", e)
	}
	return n, nil
}
func (c *converter) tableName(name string) string {
	if mapped := c.opt.TableNames[name]; mapped != "" {
		return strings.ToUpper(mapped)
	}
	return strings.ToUpper(name)
}
func (c *converter) columnName(table, name string) string {
	if cols := c.opt.ColumnNames[table]; cols != nil && cols[name] != "" {
		return strings.ToUpper(cols[name])
	}
	return strings.ToUpper(name)
}
func (c *converter) target(t *source.TableDDLInfo) string {
	return quote(c.opt.Schema) + "." + quote(c.tableName(t.TableName))
}
func (c *converter) validateMappings() error {
	seen := map[string]string{}
	for name, t := range c.tables {
		if t == nil || t.TableName != name {
			return fmt.Errorf("源表快照身份不一致")
		}
		mapped := c.tableName(name)
		if !validName(mapped) {
			return fmt.Errorf("目标表名无效")
		}
		if old := seen[mapped]; old != "" && old != name {
			return fmt.Errorf("源表 %s 和 %s 的目标名称冲突", old, name)
		}
		seen[mapped] = name
		cols := map[string]bool{}
		for _, col := range t.Columns {
			mapped := c.columnName(name, col.Name)
			if !validName(mapped) || cols[mapped] {
				return fmt.Errorf("表 %s 的目标列名无效或冲突", name)
			}
			cols[mapped] = true
		}
	}
	return nil
}
func (c *converter) table(schema, name string) (*source.TableDDLInfo, error) {
	if schema != "" && schema != c.info.Schema {
		return nil, fmt.Errorf("跨源库引用 %s.%s 需要人工适配", schema, name)
	}
	t := c.tables[name]
	if t == nil {
		return nil, fmt.Errorf("依赖表 %s 元数据不可用", name)
	}
	c.deps[c.tableName(name)] = t
	return t, nil
}
func (c *converter) column(t *source.TableDDLInfo, name string) (*source.ColumnInfo, error) {
	if t == nil {
		return nil, fmt.Errorf("列 %s 缺少表上下文", name)
	}
	var col *source.ColumnInfo
	for i := range t.Columns {
		if strings.EqualFold(t.Columns[i].Name, name) {
			if col != nil {
				return nil, fmt.Errorf("列 %s 大小写匹配不唯一", name)
			}
			col = &t.Columns[i]
		}
	}
	if col == nil {
		return nil, fmt.Errorf("表 %s 缺少列 %s", t.TableName, name)
	}
	if col.Generated != "" || strings.Contains(strings.ToUpper(col.Extra), "GENERATED") {
		return nil, fmt.Errorf("生成列 %s 需要人工适配", name)
	}
	return col, nil
}
