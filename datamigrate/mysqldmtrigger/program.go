package mysqldmtrigger

import (
	"fmt"
	"strings"

	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/mysql"
)

func (c *converter) sequence(stops ...string) (string, error) {
	var b strings.Builder
	for c.pos < len(c.tokens) {
		for _, s := range stops {
			if c.at(s) {
				return b.String(), nil
			}
		}
		q, e := c.statement()
		if e != nil {
			return "", e
		}
		b.WriteString(q)
		if e = c.take(";"); e != nil {
			return "", e
		}
	}
	return "", fmt.Errorf("程序块未闭合")
}

// until identifies only program boundaries. SQL leaves are parsed independently;
// CASE expressions cannot accidentally close an IF or SELECT boundary.
func (c *converter) until(stops ...string) (string, error) {
	start := c.pos
	depth, cases := 0, 0
	for c.pos < len(c.tokens) {
		t := c.tokens[c.pos]
		if depth == 0 && cases == 0 {
			for _, s := range stops {
				if t.word == s || t.raw == s {
					if start == c.pos {
						return "", fmt.Errorf("表达式为空")
					}
					return c.info.Body[c.tokens[start].start:c.tokens[c.pos-1].end], nil
				}
			}
		}
		if t.word == "CASE" {
			cases++
		}
		if t.word == "END" && cases > 0 {
			cases--
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
	return "", fmt.Errorf("语句缺少结束符")
}

func (c *converter) statement() (string, error) {
	if c.pos >= len(c.tokens) {
		return "", fmt.Errorf("缺少语句")
	}
	labelName := ""
	if c.pos+1 < len(c.tokens) && c.tokens[c.pos+1].raw == ":" {
		var e error
		labelName, e = identifier(c.tokens[c.pos])
		if e != nil {
			return "", e
		}
		c.pos += 2
	}
	switch {
	case c.at("BEGIN"):
		return c.block(labelName)
	case c.at("WHILE"), c.at("REPEAT"), c.at("LOOP"):
		return c.loop(labelName)
	}
	if labelName != "" {
		return "", fmt.Errorf("标签只能用于 BEGIN 或循环")
	}
	switch {
	case c.at("IF"):
		return c.ifStatement()
	case c.at("CASE"):
		return c.caseStatement()
	case c.at("LEAVE"), c.at("ITERATE"):
		return c.jump()
	case c.at("SET"):
		return c.setStatement()
	case c.at("SIGNAL"):
		return c.signal()
	case c.at("SELECT"):
		return c.selectInto()
	case c.at("INSERT"), c.at("UPDATE"), c.at("DELETE"):
		start := c.pos
		q, e := c.until(";")
		if e != nil && c.pos == len(c.tokens) {
			q = c.info.Body[c.tokens[start].start:]
			e = nil
		}
		if e != nil {
			return "", e
		}
		return c.dml(q)
	default:
		return "", fmt.Errorf("语句 %s 需要人工适配", c.tokens[c.pos].raw)
	}
}

func (c *converter) addLabel(name string, loop bool) (label, error) {
	l := label{source: name, target: c.fresh("LABEL"), loop: loop}
	if name != "" {
		for _, old := range c.labels {
			if strings.EqualFold(old.source, name) {
				return l, fmt.Errorf("重复的活动标签 %s", name)
			}
		}
	}
	c.labels = append(c.labels, l)
	return l, nil
}
func (c *converter) endLabel(name string) error {
	if c.pos < len(c.tokens) && !c.at(";") {
		got, e := identifier(c.tokens[c.pos])
		if e != nil {
			return e
		}
		if name == "" || !strings.EqualFold(name, got) {
			return fmt.Errorf("结束标签 %s 不匹配", got)
		}
		c.pos++
	}
	return nil
}
func (c *converter) block(name string) (string, error) {
	c.pos++
	c.scopes = append(c.scopes, map[string]variable{})
	defer func() { c.scopes = c.scopes[:len(c.scopes)-1] }()
	var decl strings.Builder
	for c.at("DECLARE") {
		q, e := c.declaration()
		if e != nil {
			return "", e
		}
		decl.WriteString(q)
		if e = c.take(";"); e != nil {
			return "", e
		}
	}
	l, e := c.addLabel(name, false)
	if e != nil {
		return "", e
	}
	defer func() { c.labels = c.labels[:len(c.labels)-1] }()
	body, e := c.sequence("END")
	if e != nil {
		return "", e
	}
	c.pos++
	if e = c.endLabel(name); e != nil {
		return "", e
	}
	if name != "" {
		body = "<<" + l.target + ">>\nLOOP\n" + body + "EXIT;\nEND LOOP;\n"
	}
	if decl.Len() > 0 {
		return "DECLARE\n" + decl.String() + "BEGIN\n" + body + "END;\n", nil
	}
	return "BEGIN\n" + body + "END;\n", nil
}

func (c *converter) declaration() (string, error) {
	c.pos++
	var names []string
	for {
		if c.pos >= len(c.tokens) {
			return "", fmt.Errorf("DECLARE 缺少名称")
		}
		n, e := identifier(c.tokens[c.pos])
		if e != nil {
			return "", e
		}
		names = append(names, n)
		c.pos++
		if !c.at(",") {
			break
		}
		c.pos++
	}
	typeStart := c.pos
	_, e := c.until("DEFAULT", ";")
	if e != nil {
		return "", e
	}
	typeEnd := c.pos
	if typeStart == typeEnd {
		return "", fmt.Errorf("DECLARE 缺少标量类型")
	}
	spec := c.info.Body[c.tokens[typeStart].start:c.tokens[typeEnd-1].end]
	typ, kind, e := c.localType(spec)
	if e != nil {
		return "", e
	}
	var value expression
	hasDefault := c.at("DEFAULT")
	if hasDefault {
		c.pos++
		s, e := c.until(";")
		if e != nil {
			return "", e
		}
		value, e = c.expression(s, nil)
		if e != nil {
			return "", e
		}
		if e = assignable(kind, value.kind); e != nil {
			return "", e
		}
	}
	var b strings.Builder
	scope := c.scopes[len(c.scopes)-1]
	for _, n := range names {
		key := strings.ToLower(n)
		if _, ok := scope[key]; ok {
			return "", fmt.Errorf("重复的局部变量 %s", n)
		}
		v := variable{name: c.fresh("VAR"), typ: typ, kind: kind}
		scope[key] = v
		b.WriteString(quote(v.name) + " " + typ)
		if hasDefault {
			b.WriteString(" := " + value.sql)
		}
		b.WriteString(";\n")
	}
	return b.String(), nil
}

func (c *converter) localType(spec string) (string, valueKind, error) {
	n, e := c.parse("CREATE TABLE dbgold_type_probe (v " + spec + ")")
	if e != nil {
		return "", kindUnknown, e
	}
	t, ok := n.(*ast.CreateTableStmt)
	if !ok || len(t.Cols) != 1 || len(t.Cols[0].Options) > 0 || len(t.Constraints) > 0 {
		return "", kindUnknown, fmt.Errorf("局部变量只支持标量类型")
	}
	f := t.Cols[0].Tp
	if mysql.HasUnsignedFlag(f.GetFlag()) || mysql.HasZerofillFlag(f.GetFlag()) {
		return "", kindUnknown, fmt.Errorf("局部变量 UNSIGNED/ZEROFILL 需要人工适配")
	}
	switch f.GetType() {
	case mysql.TypeTiny, mysql.TypeShort, mysql.TypeInt24, mysql.TypeLong, mysql.TypeLonglong:
		typ := map[byte]string{mysql.TypeTiny: "TINYINT", mysql.TypeShort: "SMALLINT", mysql.TypeInt24: "INT", mysql.TypeLong: "INT", mysql.TypeLonglong: "BIGINT"}[f.GetType()]
		return typ, kindNumber, nil
	case mysql.TypeNewDecimal:
		p, s := f.GetFlen(), f.GetDecimal()
		if p < 0 {
			p = 10
		}
		if s < 0 {
			s = 0
		}
		if p > 38 {
			return "", kindUnknown, fmt.Errorf("局部数值精度超过达梦 38 位")
		}
		return fmt.Sprintf("NUMBER(%d,%d)", p, s), kindNumber, nil
	case mysql.TypeFloat, mysql.TypeDouble:
		return "DOUBLE", kindNumber, nil
	case mysql.TypeVarchar, mysql.TypeVarString, mysql.TypeString:
		length := f.GetFlen()
		if length <= 0 || length > 32767 {
			return "", kindUnknown, fmt.Errorf("局部字符变量长度不支持")
		}
		if f.GetCharset() == "binary" {
			return "", kindUnknown, fmt.Errorf("局部二进制变量需要人工适配")
		}
		return fmt.Sprintf("VARCHAR(%d)", length), kindText, nil
	case mysql.TypeDate:
		return "DATE", kindTime, nil
	case mysql.TypeDatetime, mysql.TypeTimestamp:
		precision := f.GetDecimal()
		if precision < 0 {
			precision = 0
		}
		if precision > 6 {
			return "", kindUnknown, fmt.Errorf("局部时间精度不支持")
		}
		return fmt.Sprintf("TIMESTAMP(%d)", precision), kindTime, nil
	default:
		return "", kindUnknown, fmt.Errorf("局部类型 %s 需要人工适配", spec)
	}
}

func (c *converter) ifStatement() (string, error) {
	c.pos++
	prefix := "IF"
	var b strings.Builder
	for {
		s, e := c.until("THEN")
		if e != nil {
			return "", e
		}
		v, e := c.expression(s, nil)
		if e != nil {
			return "", e
		}
		pred, e := c.predicate(v)
		if e != nil {
			return "", e
		}
		c.pos++
		body, e := c.sequence("ELSEIF", "ELSE", "END")
		if e != nil {
			return "", e
		}
		b.WriteString(prefix + " " + pred + " THEN\n" + body)
		if !c.at("ELSEIF") {
			break
		}
		c.pos++
		prefix = "ELSIF"
	}
	if c.at("ELSE") {
		c.pos++
		body, e := c.sequence("END")
		if e != nil {
			return "", e
		}
		b.WriteString("ELSE\n" + body)
	}
	if e := c.take("END"); e != nil {
		return "", e
	}
	if e := c.take("IF"); e != nil {
		return "", e
	}
	b.WriteString("END IF;\n")
	return b.String(), nil
}

func (c *converter) caseStatement() (string, error) {
	c.pos++
	var b strings.Builder
	b.WriteString("CASE")
	simple := !c.at("WHEN")
	var kind valueKind
	if simple {
		s, e := c.until("WHEN")
		if e != nil {
			return "", e
		}
		v, e := c.expression(s, nil)
		if e != nil {
			return "", e
		}
		if v.kind == kindText || v.kind == kindUnknown {
			return "", fmt.Errorf("简单 CASE 的字符排序语义需要人工适配，请改用明确条件")
		}
		kind = v.kind
		b.WriteString(" " + v.sql)
	}
	b.WriteString("\n")
	count := 0
	for c.at("WHEN") {
		count++
		c.pos++
		s, e := c.until("THEN")
		if e != nil {
			return "", e
		}
		v, e := c.expression(s, nil)
		if e != nil {
			return "", e
		}
		when := v.sql
		if simple {
			if e = assignable(kind, v.kind); e != nil {
				return "", e
			}
		} else {
			when, e = c.predicate(v)
			if e != nil {
				return "", e
			}
		}
		c.pos++
		body, e := c.sequence("WHEN", "ELSE", "END")
		if e != nil {
			return "", e
		}
		b.WriteString("WHEN " + when + " THEN\n" + body)
	}
	if count == 0 {
		return "", fmt.Errorf("CASE 缺少 WHEN")
	}
	if c.at("ELSE") {
		c.pos++
		body, e := c.sequence("END")
		if e != nil {
			return "", e
		}
		b.WriteString("ELSE\n" + body)
	}
	if e := c.take("END"); e != nil {
		return "", e
	}
	if e := c.take("CASE"); e != nil {
		return "", e
	}
	b.WriteString("END CASE;\n")
	return b.String(), nil
}

func (c *converter) loop(name string) (string, error) {
	kind := c.tokens[c.pos].word
	c.pos++
	l, e := c.addLabel(name, true)
	if e != nil {
		return "", e
	}
	defer func() { c.labels = c.labels[:len(c.labels)-1] }()
	header := "LOOP"
	if kind == "WHILE" {
		s, e := c.until("DO")
		if e != nil {
			return "", e
		}
		v, e := c.expression(s, nil)
		if e != nil {
			return "", e
		}
		pred, e := c.predicate(v)
		if e != nil {
			return "", e
		}
		header = "WHILE " + pred + " LOOP"
		c.pos++
	}
	stop := "END"
	if kind == "REPEAT" {
		stop = "UNTIL"
	}
	body, e := c.sequence(stop)
	if e != nil {
		return "", e
	}
	if kind == "REPEAT" {
		c.pos++
		s, e := c.until("END")
		if e != nil {
			return "", e
		}
		v, e := c.expression(s, nil)
		if e != nil {
			return "", e
		}
		pred, e := c.predicate(v)
		if e != nil {
			return "", e
		}
		body += "EXIT WHEN " + pred + ";\n"
	}
	if e = c.take("END"); e != nil {
		return "", e
	}
	if e = c.take(kind); e != nil {
		return "", e
	}
	if e = c.endLabel(name); e != nil {
		return "", e
	}
	return "<<" + l.target + ">>\n" + header + "\n" + body + "END LOOP;\n", nil
}
func (c *converter) jump() (string, error) {
	leave := c.at("LEAVE")
	c.pos++
	if c.pos >= len(c.tokens) {
		return "", fmt.Errorf("跳转缺少标签")
	}
	name, e := identifier(c.tokens[c.pos])
	if e != nil {
		return "", e
	}
	c.pos++
	for i := len(c.labels) - 1; i >= 0; i-- {
		l := c.labels[i]
		if strings.EqualFold(l.source, name) {
			if !leave && !l.loop {
				return "", fmt.Errorf("ITERATE 只能指向循环标签")
			}
			op := "CONTINUE"
			if leave {
				op = "EXIT"
			}
			return op + " " + l.target + ";\n", nil
		}
	}
	return "", fmt.Errorf("找不到活动标签 %s", name)
}

func (c *converter) setStatement() (string, error) {
	c.pos++
	var b strings.Builder
	for {
		if c.pos >= len(c.tokens) {
			return "", fmt.Errorf("SET 缺少目标")
		}
		target := ""
		kind := kindUnknown
		n, e := identifier(c.tokens[c.pos])
		if e != nil {
			return "", fmt.Errorf("会话/系统变量或 SET 目标不支持")
		}
		c.pos++
		if strings.EqualFold(n, "NEW") && c.at(".") {
			c.pos++
			if strings.ToUpper(c.info.Timing) != "BEFORE" || strings.ToUpper(c.info.Event) == "DELETE" {
				return "", fmt.Errorf("仅 BEFORE INSERT/UPDATE 可以修改 NEW")
			}
			if c.pos >= len(c.tokens) {
				return "", fmt.Errorf("NEW 缺少列")
			}
			colName, e := identifier(c.tokens[c.pos])
			if e != nil {
				return "", e
			}
			c.pos++
			col, e := c.column(c.tables[c.info.TableName], colName)
			if e != nil {
				return "", e
			}
			if strings.Contains(strings.ToLower(col.Extra), "auto_increment") {
				return "", fmt.Errorf("自增列赋值需要人工适配")
			}
			kind = kindOf(col.DataType)
			target = ":NEW." + quote(c.columnName(c.info.TableName, col.Name))
		} else {
			v, ok := c.lookup(n)
			if !ok {
				return "", fmt.Errorf("SET 目标 %s 不是已声明局部变量", n)
			}
			target = quote(v.name)
			kind = v.kind
		}
		if e = c.take("="); e != nil {
			return "", e
		}
		start := c.pos
		s, e := c.until(",", ";")
		if e != nil && c.pos == len(c.tokens) {
			s = c.info.Body[c.tokens[start].start:]
			e = nil
		}
		if e != nil {
			return "", e
		}
		v, e := c.expression(s, nil)
		if e != nil {
			return "", e
		}
		if e = assignable(kind, v.kind); e != nil {
			return "", e
		}
		b.WriteString(target + " := " + v.sql + ";\n")
		if !c.at(",") {
			break
		}
		c.pos++
	}
	return b.String(), nil
}

func (c *converter) signal() (string, error) {
	c.pos++
	if e := c.take("SQLSTATE"); e != nil {
		return "", e
	}
	if c.at("VALUE") {
		c.pos++
	}
	if c.pos >= len(c.tokens) {
		return "", fmt.Errorf("SIGNAL 缺少 SQLSTATE")
	}
	n, e := c.parse("SELECT " + c.tokens[c.pos].raw)
	if e != nil {
		return "", e
	}
	q, ok := n.(*ast.SelectStmt)
	if !ok || q.Fields == nil || len(q.Fields.Fields) != 1 {
		return "", fmt.Errorf("SIGNAL SQLSTATE 不支持")
	}
	lit, ok := q.Fields.Fields[0].Expr.(ast.ValueExpr)
	if !ok || lit.GetValue() != "45000" {
		return "", fmt.Errorf("仅支持 SIGNAL SQLSTATE '45000'")
	}
	c.pos++
	msg := "'Unhandled user-defined exception'"
	if c.at("SET") {
		c.pos++
		if e = c.take("MESSAGE_TEXT"); e != nil {
			return "", fmt.Errorf("SIGNAL 仅支持 MESSAGE_TEXT")
		}
		if e = c.take("="); e != nil {
			return "", e
		}
		start := c.pos
		s, e := c.until(";")
		if e != nil && c.pos == len(c.tokens) {
			s = c.info.Body[c.tokens[start].start:]
			e = nil
		}
		if e != nil {
			return "", e
		}
		v, e := c.expression(s, nil)
		if e != nil {
			return "", e
		}
		if v.kind != kindText {
			return "", fmt.Errorf("SIGNAL MESSAGE_TEXT 必须是字符表达式")
		}
		msg = v.sql
	}
	return "RAISE_APPLICATION_ERROR(-20000, " + msg + ");\n", nil
}
