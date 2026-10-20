package source

import (
	"context"
	"database/sql"
	"fmt"
	"math/big"
	"regexp"
	"strings"

	"dbgold/datamigrate/failure"
)

const oracleDMIdent = `(?:"(?:[^"]|"")+"|[\pL_][\pL\pN_$#]*)`

var oracleDMSequenceRef = regexp.MustCompile(`(?is)^\s*(` + oracleDMIdent + `)(?:\s*\.\s*(` + oracleDMIdent + `))?\s*\.\s*NEXTVAL\s*$`)
var oracleDMNextval = regexp.MustCompile(`(?i)\bNEXTVAL\b`)
var oracleDMLiteral = regexp.MustCompile(`(?is)^N?'(?:[^']|'')*'$`)
var oracleDMNewColumn = regexp.MustCompile(`(?is)^\s*:NEW\s*\.\s*(` + oracleDMIdent + `)\s*$`)
var oracleDMNullGuard = regexp.MustCompile(`(?is)^\s*:?(?:NEW)\s*\.\s*(` + oracleDMIdent + `)\s+IS\s+NULL\s*$`)
var oracleDMSelectInto = regexp.MustCompile(`(?is)^SELECT\s+(.+?)\s+INTO\s*(.+?)\s+FROM\s+DUAL\s*;\s*$`)
var oracleDMAssignment = regexp.MustCompile(`(?is)^(.+?)\s*:=\s*(.+?)\s*;\s*$`)
var oracleDMBody = regexp.MustCompile(`(?is)^\s*BEGIN\s+(.+)\s+END\s*;?\s*$`)
var oracleDMIf = regexp.MustCompile(`(?is)^IF\s+(.+?)\s+THEN\s+(.+)\s+END\s+IF\s*;\s*$`)

func oracleDMIdentifier(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, `"`) {
		return strings.ReplaceAll(s[1:len(s)-1], `""`, `"`)
	}
	// Unquoted Oracle references resolve to uppercase; quoted references retain
	// their spelling. Dictionary result identifiers themselves are never changed.
	return strings.ToUpper(s)
}

func oracleDMReference(s, owner string) (string, string, bool) {
	m := oracleDMSequenceRef.FindStringSubmatch(s)
	if m == nil {
		return "", "", false
	}
	if m[2] == "" {
		return owner, oracleDMIdentifier(m[1]), true
	}
	return oracleDMIdentifier(m[1]), oracleDMIdentifier(m[2]), true
}

// Remove comments without rewriting string literals or quoted identifiers.
func oracleDMWithoutComments(s string) (string, error) {
	var out strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '\'' || s[i] == '"' {
			quote, start := s[i], i
			i++
			closed := false
			for i < len(s) {
				if s[i] == quote {
					i++
					if i < len(s) && s[i] == quote {
						i++
						continue
					}
					closed = true
					break
				}
				i++
			}
			if !closed {
				return "", fmt.Errorf("Oracle SQL 引号未闭合")
			}
			out.WriteString(s[start:i])
		} else if strings.HasPrefix(s[i:], "--") {
			for i < len(s) && s[i] != '\n' {
				i++
			}
			out.WriteByte(' ')
		} else if strings.HasPrefix(s[i:], "/*") {
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return "", fmt.Errorf("Oracle SQL 注释未闭合")
			}
			i += end + 4
			out.WriteByte(' ')
		} else {
			out.WriteByte(s[i])
			i++
		}
	}
	return strings.TrimSpace(out.String()), nil
}

func oracleDamengGeneration(generation, onNull string) string {
	generation = strings.ToUpper(strings.TrimSpace(generation))
	if generation == "BY DEFAULT" && onNull == "YES" {
		return "BY DEFAULT ON NULL"
	}
	return generation
}

// parseOracleDMTrigger accepts exactly one sequence assignment, with an
// optional NULL guard. No source procedural body is copied to the target.
func parseOracleDMTrigger(body, when, owner string) (SequenceBinding, string, string, error) {
	fail := func() (SequenceBinding, string, string, error) {
		return SequenceBinding{}, "", "", fmt.Errorf("仅支持单一 NEXTVAL 赋值及可选的 NEW 列 IS NULL 条件")
	}
	clean, err := oracleDMWithoutComments(body)
	if err != nil {
		return SequenceBinding{}, "", "", err
	}
	m := oracleDMBody.FindStringSubmatch(clean)
	if m == nil {
		return fail()
	}
	assignment, guard := strings.TrimSpace(m[1]), ""
	if f := oracleDMIf.FindStringSubmatch(assignment); f != nil {
		guard, assignment = strings.TrimSpace(f[1]), strings.TrimSpace(f[2])
	}
	var ref, column string
	if m := oracleDMSelectInto.FindStringSubmatch(assignment); m != nil {
		ref, column = m[1], m[2]
	} else if m := oracleDMAssignment.FindStringSubmatch(assignment); m != nil {
		column, ref = m[1], m[2]
	} else {
		return fail()
	}
	c := oracleDMNewColumn.FindStringSubmatch(column)
	seqOwner, seqName, ok := oracleDMReference(ref, owner)
	if c == nil || !ok {
		return fail()
	}
	b := SequenceBinding{ColumnName: oracleDMIdentifier(c[1]), Kind: "trigger"}
	for _, condition := range []string{guard, when} {
		condition = strings.TrimSpace(condition)
		if condition == "" {
			continue
		}
		if strings.HasPrefix(condition, "(") && strings.HasSuffix(condition, ")") {
			condition = strings.TrimSpace(condition[1 : len(condition)-1])
		}
		g := oracleDMNullGuard.FindStringSubmatch(condition)
		if g == nil || oracleDMIdentifier(g[1]) != b.ColumnName {
			return fail()
		}
		b.OnlyWhenNull = true
	}
	return b, seqOwner, seqName, nil
}

func (r *OracleDamengReader) GetSequences(ctx context.Context) ([]SequenceInfo, error) {
	type reference struct {
		owner, name string
		binding     SequenceBinding
		unsupported string
	}
	refs := []reference{}
	identities := map[string]bool{}
	if r.version >= 12 {
		rows, err := r.db.QueryContext(ctx, `SELECT i.TABLE_NAME,i.COLUMN_NAME,i.GENERATION_TYPE,i.SEQUENCE_NAME,c.DEFAULT_ON_NULL
 FROM ALL_TAB_IDENTITY_COLS i JOIN ALL_TAB_COLUMNS c
 ON c.OWNER=i.OWNER AND c.TABLE_NAME=i.TABLE_NAME AND c.COLUMN_NAME=i.COLUMN_NAME
 WHERE i.OWNER=:1 ORDER BY i.TABLE_NAME,i.COLUMN_NAME`, r.owner)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var table, column, generation, seq, onNull string
			if err = rows.Scan(&table, &column, &generation, &seq, &onNull); err != nil {
				rows.Close()
				return nil, err
			}
			identities[table+"\x00"+column] = true
			ref := reference{owner: r.owner, name: seq, binding: SequenceBinding{TableName: table, ColumnName: column, Kind: "identity", OnlyWhenNull: true}}
			if oracleDamengGeneration(generation, onNull) != "BY DEFAULT ON NULL" {
				ref.unsupported = fmt.Sprintf("Oracle identity 模式 %s 暂不支持等价迁移", oracleDamengGeneration(generation, onNull))
			}
			refs = append(refs, ref)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	// DATA_DEFAULT is LONG: filter on DEFAULT_LENGTH, not the LONG expression.
	rows, err := r.db.QueryContext(ctx, `SELECT TABLE_NAME,COLUMN_NAME,DATA_DEFAULT FROM ALL_TAB_COLUMNS WHERE OWNER=:1 AND DEFAULT_LENGTH>0 ORDER BY TABLE_NAME,COLUMN_ID`, r.owner)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var table, column string
		var def sql.NullString
		if err = rows.Scan(&table, &column, &def); err != nil {
			rows.Close()
			return nil, err
		}
		if identities[table+"\x00"+column] || !def.Valid {
			continue
		}
		text, e := oracleDMWithoutComments(def.String)
		if e != nil {
			continue
		}
		owner, seq, direct := oracleDMReference(text, r.owner)
		// A string literal containing NEXTVAL is an ordinary literal default.
		if oracleDMLiteral.MatchString(text) || !oracleDMNextval.MatchString(text) {
			continue
		}
		ref := reference{owner: owner, name: seq, binding: SequenceBinding{TableName: table, ColumnName: column, Kind: "default"}}
		ref.unsupported = "Oracle NEXTVAL 默认值暂不支持自动迁移，请手动适配"
		if !direct {
			ref.unsupported = "复杂序列默认表达式暂不支持自动迁移"
		}
		refs = append(refs, ref)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = r.db.QueryContext(ctx, `SELECT TABLE_NAME,TRIGGER_NAME,TRIGGER_TYPE,TRIGGERING_EVENT,TRIGGER_BODY,WHEN_CLAUSE
 FROM ALL_TRIGGERS WHERE OWNER=:1 AND TABLE_OWNER=OWNER AND TABLE_NAME IS NOT NULL AND STATUS='ENABLED' ORDER BY TABLE_NAME,TRIGGER_NAME`, r.owner)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var trigger, kind, event string
		var table, body, when sql.NullString
		if err = rows.Scan(&table, &trigger, &kind, &event, &body, &when); err != nil {
			rows.Close()
			return nil, err
		}
		if !table.Valid || table.String == "" {
			continue
		}
		clean, e := oracleDMWithoutComments(body.String)
		if e == nil && !oracleDMNextval.MatchString(clean) {
			continue
		}
		binding, owner, seq, e := parseOracleDMTrigger(body.String, when.String, r.owner)
		binding.TableName, binding.TriggerName = table.String, trigger
		ref := reference{owner: owner, name: seq, binding: binding}
		if e != nil || kind != "BEFORE EACH ROW" || strings.TrimSpace(event) != "INSERT" {
			ref.unsupported = fmt.Sprintf("触发器 %s 不是受支持的简单 BEFORE INSERT 自增触发器", trigger)
		}
		refs = append(refs, ref)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := []SequenceInfo{}
	position := map[string]int{}
	uses := map[string]int{}
	for _, ref := range refs {
		if ref.binding.ColumnName != "" {
			uses[ref.binding.TableName+"\x00"+ref.binding.ColumnName]++
		}
	}
	for _, ref := range refs {
		if uses[ref.binding.TableName+"\x00"+ref.binding.ColumnName] > 1 {
			ref.unsupported = "同一列关联多个序列生成方式，无法可靠保留触发顺序"
		}
		if ref.owner != r.owner && ref.unsupported == "" {
			ref.unsupported = "跨 schema 序列引用暂不支持迁移"
		}
		if ref.unsupported != "" {
			out = append(out, SequenceInfo{TableName: ref.binding.TableName, ColumnName: ref.binding.ColumnName, Unsupported: ref.unsupported,
				Native: &NativeSequenceInfo{Name: ref.name, Bindings: []SequenceBinding{ref.binding}}})
			continue
		}
		key := ref.owner + "\x00" + ref.name
		if pos, ok := position[key]; ok {
			out[pos].Native.Bindings = append(out[pos].Native.Bindings, ref.binding)
			continue
		}
		s := SequenceInfo{TableName: ref.binding.TableName, ColumnName: ref.binding.ColumnName, Native: &NativeSequenceInfo{Name: ref.name, Bindings: []SequenceBinding{ref.binding}}}
		var cycle, order string
		e := r.db.QueryRowContext(ctx, `SELECT TO_CHAR(LAST_NUMBER,'TM9','NLS_NUMERIC_CHARACTERS=''.,'''),
 TO_CHAR(INCREMENT_BY,'TM9','NLS_NUMERIC_CHARACTERS=''.,'''),TO_CHAR(MIN_VALUE,'TM9','NLS_NUMERIC_CHARACTERS=''.,'''),
 TO_CHAR(MAX_VALUE,'TM9','NLS_NUMERIC_CHARACTERS=''.,'''),TO_CHAR(CACHE_SIZE,'TM9','NLS_NUMERIC_CHARACTERS=''.,'''),CYCLE_FLAG,ORDER_FLAG
 FROM ALL_SEQUENCES WHERE SEQUENCE_OWNER=:1 AND SEQUENCE_NAME=:2`, ref.owner, ref.name).Scan(&s.StartValueText, &s.Native.IncrementBy, &s.Native.MinValue, &s.Native.MaxValue, &s.Native.CacheSize, &cycle, &order)
		if e != nil {
			if failure.IsTaskError(e) {
				return nil, fmt.Errorf("读取序列 %s 元数据失败: %w", ref.name, e)
			}
			s.Unsupported = fmt.Sprintf("读取序列 %s 元数据失败: %v", ref.name, e)
		} else {
			s.Native.Cycle, s.Native.Ordered = cycle == "Y", order == "Y"
		}
		position[key] = len(out)
		out = append(out, s)
	}
	return out, nil
}

// FilterNativeSequences is shared by migration execution and durable repair
// inventory. It never mutates the reader's metadata or another selected scope.
func FilterNativeSequences(values []SequenceInfo, selected map[string]bool) []SequenceInfo {
	out := []SequenceInfo{}
	for _, original := range values {
		s := original
		if s.Native == nil {
			if selected[s.TableName] {
				out = append(out, s)
			}
			continue
		}
		native := *s.Native
		native.Bindings = nil
		for _, b := range s.Native.Bindings {
			if selected[b.TableName] {
				native.Bindings = append(native.Bindings, b)
			}
		}
		if len(native.Bindings) == 0 {
			continue
		}
		s.Native = &native
		s.TableName, s.ColumnName = native.Bindings[0].TableName, native.Bindings[0].ColumnName
		out = append(out, s)
	}
	return out
}

// PrepareNativeSequence is called after table scoping. LAST_NUMBER is a cache
// watermark rather than the next cached value: conservative gaps are expected.
// Neither this method nor discovery consumes or changes the source sequence.
func (r *OracleDamengReader) PrepareNativeSequence(ctx context.Context, s SequenceInfo) (SequenceInfo, error) {
	if s.Native == nil || s.Unsupported != "" {
		return s, nil
	}
	parse := func(text string) (*big.Int, error) {
		n, ok := new(big.Int).SetString(text, 10)
		if !ok {
			return nil, fmt.Errorf("无效序列整数 %q", text)
		}
		return n, nil
	}
	next, err := parse(s.StartValueText)
	if err != nil {
		return s, err
	}
	step, err := parse(s.Native.IncrementBy)
	if err != nil || step.Sign() == 0 {
		return s, fmt.Errorf("无效序列步长 %q", s.Native.IncrementBy)
	}
	if s.Native.Cycle {
		return s, fmt.Errorf("循环序列无法保证迁移后自增唯一性")
	}
	for _, b := range s.Native.Bindings {
		agg := "MAX"
		if step.Sign() < 0 {
			agg = "MIN"
		}
		var extreme sql.NullString
		query := "SELECT TO_CHAR(" + agg + "(" + quoteOracleDameng(b.ColumnName) + "),'TM9','NLS_NUMERIC_CHARACTERS=''.,''') FROM " + r.tableName(b.TableName)
		if err = r.db.QueryRowContext(ctx, query).Scan(&extreme); err != nil {
			return s, err
		}
		if !extreme.Valid {
			continue
		}
		bound, e := parse(extreme.String)
		if e != nil {
			return s, e
		}
		if step.Sign() > 0 && next.Cmp(bound) <= 0 || step.Sign() < 0 && next.Cmp(bound) >= 0 {
			distance := new(big.Int).Sub(bound, next)
			distance.Quo(distance, step)
			distance.Add(distance, big.NewInt(1))
			next.Add(next, new(big.Int).Mul(distance, step))
		}
	}
	lo, e := parse(s.Native.MinValue)
	if e != nil {
		return s, e
	}
	hi, e := parse(s.Native.MaxValue)
	if e != nil {
		return s, e
	}
	if next.Cmp(lo) < 0 || next.Cmp(hi) > 0 {
		return s, fmt.Errorf("序列 %s 续值 %s 超出源序列范围", s.Native.Name, next)
	}
	s.StartValueText = next.String()
	if next.IsInt64() {
		s.StartValue = next.Int64()
	}
	return s, nil
}
