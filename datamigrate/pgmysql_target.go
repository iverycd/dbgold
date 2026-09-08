package datamigrate

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/sqlsyntax"
	"dbgold/datamigrate/typemap"
)

type mysqlColumn struct {
	name, typ, nullable, extra string
	collation, def, generated  sql.NullString
}

var integerDisplayWidth = regexp.MustCompile(`(?i)^(tinyint|smallint|mediumint|int|bigint)\(\d+\)`)

func normalizedMySQLType(s string) string {
	s = strings.ToLower(s)
	s = integerDisplayWidth.ReplaceAllString(s, "$1")
	return strings.ReplaceAll(s, "(0)", "")
}
func (m *Migrator) inspectPGMySQLTarget(ctx context.Context, p *PGMySQLPreflight, db *sql.DB, existing bool) error {
	var exists int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME=?`, p.Target.Database).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return fmt.Errorf("目标 database 不存在")
	}
	targets := map[string]string{}
	for _, t := range p.tables {
		targets[m.objName(t.Info.TableName)] = t.Info.TableName
	}
	// Existing foreign keys may prevent DROP or reference rows outside this plan.
	rows, err := db.QueryContext(ctx, `SELECT TABLE_SCHEMA,TABLE_NAME,CONSTRAINT_NAME,REFERENCED_TABLE_SCHEMA,REFERENCED_TABLE_NAME FROM information_schema.KEY_COLUMN_USAGE WHERE REFERENCED_TABLE_SCHEMA=? GROUP BY TABLE_SCHEMA,TABLE_NAME,CONSTRAINT_NAME,REFERENCED_TABLE_SCHEMA,REFERENCED_TABLE_NAME`, p.Target.Database)
	if err != nil {
		return err
	}
	for rows.Next() {
		var sch, t, c, rs, rt string
		if err = rows.Scan(&sch, &t, &c, &rs, &rt); err != nil {
			rows.Close()
			return err
		}
		if _, hit := targets[rt]; !hit {
			continue
		}
		if !existing {
			if _, internal := targets[t]; !internal || sch != p.Target.Database {
				p.add(ObjectRef{Kind: "foreign_key", Table: t, Name: c}, "目标表存在迁移范围外的入向外键，不能删除", false)
			} else {
				sql := "ALTER TABLE " + sqlsyntax.QuoteIdent(sch) + "." + sqlsyntax.QuoteIdent(t) + " DROP FOREIGN KEY " + sqlsyntax.QuoteIdent(c)
				p.Objects = append(p.Objects, PlannedObject{ObjectRef{"foreign_key", t, c}, c, "drop_dependency", []dialect.Statement{{SQL: sql}}})
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, t := range p.tables {
		name := m.objName(t.Info.TableName)
		ref := tableRef(t.Info.TableName)
		var engine sql.NullString
		var kind string
		var rowFormat sql.NullString
		e := db.QueryRowContext(ctx, `SELECT ENGINE,TABLE_TYPE,ROW_FORMAT FROM information_schema.TABLES WHERE TABLE_SCHEMA=? AND TABLE_NAME=?`, p.Target.Database, name).Scan(&engine, &kind, &rowFormat)
		if !existing {
			if e != nil && e != sql.ErrNoRows {
				return e
			}
			if e == nil && kind != "BASE TABLE" {
				p.add(ref, "目标同名对象不是普通表，不能覆盖", false)
			}
			continue
		}
		if e == sql.ErrNoRows {
			p.add(ref, "目标表不存在", false)
			continue
		}
		if e != nil {
			return e
		}
		if engine.String != "InnoDB" || kind != "BASE TABLE" {
			p.add(ref, "已有目标必须是 InnoDB 普通表", false)
			continue
		}
		if !strings.EqualFold(rowFormat.String, "Dynamic") {
			p.add(ref, "已有目标行格式必须是 DYNAMIC，才能按计划验证行宽和索引限制", false)
		}
		cols, e := loadMySQLColumns(ctx, db, p.Target.Database, name)
		if e != nil {
			return e
		}
		if len(cols) != len(t.Info.Columns) {
			p.add(ref, "目标列数量与源表不一致", false)
			continue
		}
		for i, c := range t.Info.Columns {
			actual := cols[i]
			typ, e := typemap.PGToMySQLChecked(c)
			if e != nil {
				p.add(ref, e.Error(), false)
				continue
			}
			if actual.name != m.objName(c.Name) || normalizedMySQLType(actual.typ) != normalizedMySQLType(typ) || strings.EqualFold(actual.nullable, "YES") != c.IsNullable {
				p.add(ref, "目标列 "+actual.name+" 的名称、类型、顺序或可空性不匹配", false)
			}
			if actual.generated.Valid && actual.generated.String != "" {
				p.add(ref, "目标生成列不允许显式复制", false)
			}
			if actual.collation.Valid && actual.collation.String != p.Target.TargetCollation {
				p.add(ref, "目标列 "+actual.name+" 排序规则必须与预检映射一致："+p.Target.TargetCollation, false)
			}
			if strings.Contains(actual.extra, "auto_increment") != (c.Extra == "auto_increment") {
				p.add(ref, "目标自增属性不匹配", false)
			}
		}
		// Data-only writes into an empty schema created in a separate schema-only
		// run. Appending to populated tables needs a separate conflict policy.
		if m.cfg.Content == "data_only" {
			var count int64
			e = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+sqlsyntax.QuoteIdent(p.Target.Database)+`.`+sqlsyntax.QuoteIdent(name)).Scan(&count)
			if e != nil {
				return e
			}
			if count != 0 {
				p.add(ref, "data_only 首版要求空目标表，不能隐式追加或覆盖已有行", false)
			}
		}
		if e = m.checkExistingPGObjects(ctx, p, db, t); e != nil {
			return e
		}
		if m.cfg.objectMode() && m.wantPGObject("comment") {
			var shown, ddl string
			e = db.QueryRowContext(ctx, `SHOW CREATE TABLE `+sqlsyntax.QuoteIdent(p.Target.Database)+`.`+sqlsyntax.QuoteIdent(name)).Scan(&shown, &ddl)
			if e != nil {
				return e
			}
			for _, c := range t.Info.Columns {
				if c.Comment == "" {
					continue
				}
				definition, e := mysqlColumnDefinition(ddl, m.objName(c.Name))
				if e != nil {
					p.add(ref, e.Error(), false)
					continue
				}
				definition, e = replaceMySQLColumnComment(definition, c.Comment)
				if e != nil {
					p.add(ref, e.Error(), false)
					continue
				}
				p.Objects = append(p.Objects, PlannedObject{ObjectRef{"comment", t.Info.TableName, c.Name}, c.Name, "comment", []dialect.Statement{{SQL: "ALTER TABLE " + sqlsyntax.QuoteIdent(p.Target.Database) + "." + sqlsyntax.QuoteIdent(name) + " MODIFY COLUMN " + definition}}})
			}
			if t.Info.Comment != "" {
				p.Objects = append(p.Objects, PlannedObject{ObjectRef{"comment", t.Info.TableName, t.Info.TableName}, name, "comment", []dialect.Statement{{SQL: "ALTER TABLE " + sqlsyntax.QuoteIdent(p.Target.Database) + "." + sqlsyntax.QuoteIdent(name) + " COMMENT=" + sqlsyntax.QuoteString(t.Info.Comment)}}})
			}
		}
	}
	if m.cfg.Content != "data_only" {
		if e := m.inspectPGTargetNamesAndViews(ctx, p, db, targets, existing); e != nil {
			return e
		}
	}
	// Existing triggers can mutate copied rows even when the source has none.
	if existing {
		rows, err = db.QueryContext(ctx, `SELECT EVENT_OBJECT_TABLE,TRIGGER_NAME FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA=?`, p.Target.Database)
		if err != nil {
			return err
		}
		for rows.Next() {
			var t, n string
			if err = rows.Scan(&t, &n); err != nil {
				rows.Close()
				return err
			}
			if _, hit := targets[t]; hit && m.cfg.Content == "data_only" {
				p.add(ObjectRef{"trigger", t, n}, "已有目标触发器可能改变复制数据", false)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
func loadMySQLColumns(ctx context.Context, db *sql.DB, schema, table string) ([]mysqlColumn, error) {
	rows, e := db.QueryContext(ctx, `SELECT COLUMN_NAME,COLUMN_TYPE,IS_NULLABLE,EXTRA,COLLATION_NAME,COLUMN_DEFAULT,GENERATION_EXPRESSION FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? ORDER BY ORDINAL_POSITION`, schema, table)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []mysqlColumn
	for rows.Next() {
		var c mysqlColumn
		if e = rows.Scan(&c.name, &c.typ, &c.nullable, &c.extra, &c.collation, &c.def, &c.generated); e != nil {
			return nil, e
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
func (m *Migrator) checkExistingPGObjects(ctx context.Context, p *PGMySQLPreflight, db *sql.DB, t source.PGTable) error {
	rows, e := db.QueryContext(ctx, `SELECT INDEX_NAME,COLUMN_NAME,NON_UNIQUE,SUB_PART,INDEX_TYPE FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? ORDER BY INDEX_NAME,SEQ_IN_INDEX`, p.Target.Database, m.objName(t.Info.TableName))
	if e != nil {
		return e
	}
	type idx struct {
		cols   []string
		unique bool
	}
	indexes := map[string]*idx{}
	for rows.Next() {
		var n, method string
		var col sql.NullString
		var non int
		var prefix sql.NullInt64
		if e = rows.Scan(&n, &col, &non, &prefix, &method); e != nil {
			rows.Close()
			return e
		}
		if !col.Valid || prefix.Valid || method != "BTREE" {
			p.add(tableRef(t.Info.TableName), "已有目标包含函数/前缀/特殊索引，不能验证写入语义", false)
		}
		if indexes[n] == nil {
			indexes[n] = &idx{unique: non == 0}
		}
		indexes[n].cols = append(indexes[n].cols, col.String)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	if m.cfg.Content == "data_only" {
		for n, i := range indexes {
			if !i.unique {
				continue
			}
			matched := false
			for _, src := range t.Indexes {
				if src.IsUnique && strings.Join(mapNames(src.Columns, m.objName), "\x00") == strings.Join(i.cols, "\x00") {
					matched = true
				}
			}
			if !matched {
				p.add(ObjectRef{"index", t.Info.TableName, n}, "目标存在源预检未覆盖的唯一约束", false)
			}
		}
	}
	if m.cfg.Content == "data_only" {
		for _, src := range t.Indexes {
			if !src.IsUnique {
				continue
			}
			matched := false
			for _, idx := range indexes {
				if idx.unique && strings.Join(mapNames(src.Columns, m.objName), "\x00") == strings.Join(idx.cols, "\x00") {
					matched = true
				}
			}
			if !matched {
				p.add(idxRef(src), "目标缺少源表的主键/唯一约束", false)
			}
		}
	}
	for _, obj := range p.Objects {
		if obj.Stage == "index" && obj.Object.Table == t.Info.TableName {
			name := obj.Target
			if obj.Object.Kind == "primary_key" {
				name = "PRIMARY"
			}
			if _, exists := indexes[name]; exists {
				p.add(obj.Object, "目标索引已存在，请排除该对象或先调整目标", true)
			}
		}
	}
	if m.cfg.Content == "data_only" {
		return m.compareExistingPGConstraints(ctx, p, db, t)
	}

	return nil
}

// SHOW CREATE is parsed by delimiters, preserving every attribute. Backslash
// escapes are decoded to SQL-standard quotes because execution uses
// NO_BACKSLASH_ESCAPES; unsupported escape forms fail closed.
func mysqlColumnDefinition(ddl, column string) (string, error) {
	prefix := sqlsyntax.QuoteIdent(column)
	start := strings.Index(ddl, "(")
	if start < 0 {
		return "", fmt.Errorf("无法解析 SHOW CREATE TABLE")
	}
	start++
	depth := 0
	quote := byte(0)
	part := start
	for i := start; i < len(ddl); i++ {
		c := ddl[i]
		if quote != 0 {
			if c == '\\' && quote == '\'' {
				i++
				continue
			}
			if c == quote {
				if i+1 < len(ddl) && ddl[i+1] == quote {
					i++
				} else {
					quote = 0
				}
			}
			continue
		}
		if c == '\'' || c == '`' || c == '"' {
			quote = c
			continue
		}
		if c == '(' {
			depth++
		}
		if c == ')' {
			if depth == 0 {
				segment := strings.TrimSpace(ddl[part:i])
				if strings.HasPrefix(segment, prefix+" ") {
					return normalizeMySQLStrings(segment)
				}
				break
			}
			depth--
		}
		if c == ',' && depth == 0 {
			segment := strings.TrimSpace(ddl[part:i])
			if strings.HasPrefix(segment, prefix+" ") {
				return normalizeMySQLStrings(segment)
			}
			part = i + 1
		}
	}
	return "", fmt.Errorf("SHOW CREATE 未找到列 %s", column)
}
func normalizeMySQLStrings(s string) (string, error) {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\'' {
			out.WriteByte(s[i])
			continue
		}
		out.WriteByte('\'')
		i++
		closed := false
		for i < len(s) {
			c := s[i]
			if c == '\'' {
				out.WriteByte(c)
				if i+1 < len(s) && s[i+1] == '\'' {
					out.WriteByte('\'')
					i += 2
					continue
				}
				closed = true
				break
			}
			if c == '\\' {
				i++
				if i >= len(s) {
					return "", fmt.Errorf("SHOW CREATE 转义不完整")
				}
				switch s[i] {
				case '0':
					c = 0
				case 'n':
					c = '\n'
				case 'r':
					c = '\r'
				case 't':
					c = '\t'
				case 'b':
					c = '\b'
				case 'Z':
					c = 26
				case '\\', '\'', '"':
					c = s[i]
				default:
					return "", fmt.Errorf("SHOW CREATE 含不支持的转义")
				}
			}
			if c == '\'' {
				out.WriteString("''")
			} else {
				out.WriteByte(c)
			}
			i++
		}
		if !closed {
			return "", fmt.Errorf("SHOW CREATE 字符串未闭合")
		}
	}
	return out.String(), nil
}
func replaceMySQLColumnComment(def, comment string) (string, error) {
	quote := byte(0)
	for i := 0; i < len(def); i++ {
		c := def[i]
		if quote != 0 {
			if c == quote {
				if i+1 < len(def) && def[i+1] == quote {
					i++
				} else {
					quote = 0
				}
			}
			continue
		}
		if c == '\'' || c == '`' || c == '"' {
			quote = c
			continue
		}
		if strings.HasPrefix(strings.ToUpper(def[i:]), " COMMENT ") {
			start := i
			j := i + 9
			for j < len(def) && def[j] == ' ' {
				j++
			}
			if j >= len(def) || def[j] != '\'' {
				return "", fmt.Errorf("无效 COMMENT 定义")
			}
			j++
			for j < len(def) {
				if def[j] == '\'' {
					if j+1 < len(def) && def[j+1] == '\'' {
						j += 2
						continue
					}
					j++
					return def[:start] + " COMMENT " + sqlsyntax.QuoteString(comment) + def[j:], nil
				}
				j++
			}
			return "", fmt.Errorf("未闭合 COMMENT")
		}
	}
	return def + " COMMENT " + sqlsyntax.QuoteString(comment), nil
}

func (m *Migrator) compareExistingPGConstraints(ctx context.Context, p *PGMySQLPreflight, db *sql.DB, t source.PGTable) error {
	seenChecks := map[string]bool{}
	if p.Target.Parsed.Checks() {
		rows, e := db.QueryContext(ctx, `SELECT tc.CONSTRAINT_NAME,cc.CHECK_CLAUSE,tc.ENFORCED FROM information_schema.TABLE_CONSTRAINTS tc JOIN information_schema.CHECK_CONSTRAINTS cc ON cc.CONSTRAINT_SCHEMA=tc.CONSTRAINT_SCHEMA AND cc.CONSTRAINT_NAME=tc.CONSTRAINT_NAME WHERE tc.TABLE_SCHEMA=? AND tc.TABLE_NAME=? AND tc.CONSTRAINT_TYPE='CHECK'`, p.Target.Database, m.objName(t.Info.TableName))
		if e != nil {
			return e
		}
		for rows.Next() {
			var n, expr, enforced string
			if e = rows.Scan(&n, &expr, &enforced); e != nil {
				rows.Close()
				return e
			}
			seenChecks[n] = true
			actual, e := canonicalMySQLCheck(expr)
			if e != nil {
				rows.Close()
				return e
			}
			matched := false
			for _, c := range t.Checks {
				expected, e := translatePGCheck(c.Definition, t.Info, m.objName)
				if e == nil && m.objName(c.Name) == n && expected == actual && enforced == "YES" {
					matched = true
				}
			}
			if !matched {
				p.add(ObjectRef{"check", t.Info.TableName, n}, "已有目标 CHECK 与源表达式/执行状态不匹配", false)
			}
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
	}
	for _, c := range t.Checks {
		if !seenChecks[m.objName(c.Name)] {
			p.add(pgRef(c), "目标缺少源表 CHECK；请创建约束或明确排除后重新预检", true)
		}
	}
	rows, e := db.QueryContext(ctx, `SELECT k.CONSTRAINT_NAME,k.COLUMN_NAME,k.REFERENCED_TABLE_SCHEMA,k.REFERENCED_TABLE_NAME,k.REFERENCED_COLUMN_NAME,r.DELETE_RULE,r.UPDATE_RULE FROM information_schema.KEY_COLUMN_USAGE k JOIN information_schema.REFERENTIAL_CONSTRAINTS r ON r.CONSTRAINT_SCHEMA=k.CONSTRAINT_SCHEMA AND r.CONSTRAINT_NAME=k.CONSTRAINT_NAME AND r.TABLE_NAME=k.TABLE_NAME WHERE k.TABLE_SCHEMA=? AND k.TABLE_NAME=? AND k.REFERENCED_TABLE_NAME IS NOT NULL ORDER BY k.CONSTRAINT_NAME,k.ORDINAL_POSITION`, p.Target.Database, m.objName(t.Info.TableName))
	if e != nil {
		return e
	}
	actual := map[string]*source.FKInfo{}
	for rows.Next() {
		var n, c, rs, rt, rc, del, upd string
		if e = rows.Scan(&n, &c, &rs, &rt, &rc, &del, &upd); e != nil {
			rows.Close()
			return e
		}
		if actual[n] == nil {
			actual[n] = &source.FKInfo{RefSchema: rs, RefTable: rt, OnDelete: del, OnUpdate: upd}
		}
		actual[n].Columns = append(actual[n].Columns, c)
		actual[n].RefColumns = append(actual[n].RefColumns, rc)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	normalize := func(s string) string {
		if s == "NO ACTION" {
			return "RESTRICT"
		}
		return s
	}
	for _, f := range p.validationFKs {
		if f.TableName == t.Info.TableName && actual[m.objName(f.ConstraintName)] == nil {
			p.add(ObjectRef{"foreign_key", f.TableName, f.ConstraintName}, "目标缺少源表外键；请创建约束或明确排除后重新预检", true)
		}
	}
	for n, a := range actual {
		matched := false
		for _, f := range p.validationFKs {
			if f.TableName == t.Info.TableName && m.objName(f.ConstraintName) == n && a.RefSchema == p.Target.Database && m.objName(f.RefTable) == a.RefTable && strings.Join(mapNames(f.Columns, m.objName), "\x00") == strings.Join(a.Columns, "\x00") && strings.Join(mapNames(f.RefColumns, m.objName), "\x00") == strings.Join(a.RefColumns, "\x00") && normalize(a.OnDelete) == normalize(f.OnDelete) && normalize(a.OnUpdate) == normalize(f.OnUpdate) {
				matched = true
			}
		}
		if !matched {
			p.add(ObjectRef{"foreign_key", t.Info.TableName, n}, "已有目标外键与源列、引用关系或动作不匹配", false)
		}
	}
	return nil
}
func canonicalMySQLCheck(raw string) (string, error) {
	ts, e := sqlsyntax.Lex(raw)
	if e != nil {
		return "", e
	}
	expr := sqlsyntax.Expr{Tokens: ts, Resolve: func(parts []string) (string, error) {
		if len(parts) != 1 {
			return "", fmt.Errorf("不支持的 CHECK 列引用")
		}
		return sqlsyntax.QuoteIdent(parts[0]), nil
	}}
	return expr.Parse()
}

func (m *Migrator) inspectPGTargetNamesAndViews(ctx context.Context, p *PGMySQLPreflight, db *sql.DB, targets map[string]string, existing bool) error {
	planned := map[string]ObjectRef{}
	views := map[string]bool{}
	for _, o := range p.Objects {
		if o.Stage == "check" || o.Stage == "foreign_key" {
			planned[strings.ToLower(o.Target)] = o.Object
		}
		if o.Stage == "view" {
			views[o.Target] = true
		}
	}
	if len(planned) > 0 {
		rows, e := db.QueryContext(ctx, `SELECT TABLE_NAME,CONSTRAINT_NAME FROM information_schema.TABLE_CONSTRAINTS WHERE CONSTRAINT_SCHEMA=? AND CONSTRAINT_TYPE IN ('FOREIGN KEY','CHECK')`, p.Target.Database)
		if e != nil {
			return e
		}
		for rows.Next() {
			var table, name string
			if e = rows.Scan(&table, &name); e != nil {
				rows.Close()
				return e
			}
			if ref, hit := planned[strings.ToLower(name)]; hit {
				_, rebuilt := targets[table]
				if existing || !rebuilt {
					p.add(ref, "目标约束名已存在，不能覆盖其他表或重复创建", true)
				}
			}
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
	}
	for name := range views {
		var kind string
		e := db.QueryRowContext(ctx, `SELECT TABLE_TYPE FROM information_schema.TABLES WHERE TABLE_SCHEMA=? AND TABLE_NAME=?`, p.Target.Database, name).Scan(&kind)
		if e != nil && e != sql.ErrNoRows {
			return e
		}
		if e == nil && kind != "VIEW" {
			p.add(ObjectRef{Kind: "view", Name: name}, "目标同名对象不是视图，不能覆盖", false)
		}
	}
	if !existing && len(targets) > 0 {
		rows, e := db.QueryContext(ctx, `SELECT TABLE_NAME,VIEW_DEFINITION FROM information_schema.VIEWS WHERE TABLE_SCHEMA=?`, p.Target.Database)
		if e != nil {
			return e
		}
		for rows.Next() {
			var name string
			var def sql.NullString
			if e = rows.Scan(&name, &def); e != nil {
				rows.Close()
				return e
			}
			if views[name] {
				continue
			}
			tokens, err := sqlsyntax.Lex(def.String)
			dependent := !def.Valid || err != nil
			for i, t := range tokens {
				if i+2 < len(tokens) && t.Value == p.Target.Database && tokens[i+1].Is(".") {
					if _, ok := targets[tokens[i+2].Value]; ok {
						dependent = true
					}
				}
			}
			if dependent {
				p.add(ObjectRef{Kind: "view", Name: name}, "目标存在未纳入重建计划的依赖视图（或定义无法验证），请先处理其依赖", false)
			}
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
	}
	return nil
}

// Preflight checks privileges before DROP; CREATE/DROP are separate grants and
// a user with only DROP must never erase a table before discovering the gap.
// Unresolved role-only/table-only grants fail closed with an actionable error.
func (m *Migrator) checkPGMySQLPrivileges(ctx context.Context, p *PGMySQLPreflight, db *sql.DB) error {
	required := map[string]bool{"SELECT": true}
	if !m.cfg.objectMode() && len(m.cfg.ViewNames) == 0 && m.cfg.Content != "schema_only" {
		required["INSERT"] = true
	}
	for _, o := range p.Objects {
		switch o.Stage {
		case "table":
			required["CREATE"] = true
			required["DROP"] = true
		case "index":
			required["INDEX"] = true
			required["ALTER"] = true
		case "foreign_key":
			required["REFERENCES"] = true
			required["ALTER"] = true
		case "check", "sequence", "comment", "drop_dependency":
			required["ALTER"] = true
		case "view":
			required["CREATE VIEW"] = true
			required["SHOW VIEW"] = true
			required["DROP"] = true
		}
	}
	rows, e := db.QueryContext(ctx, `SHOW GRANTS`)
	if e != nil {
		return e
	}
	defer rows.Close()
	granted := map[string]bool{}
	for rows.Next() {
		var grant string
		if e = rows.Scan(&grant); e != nil {
			return e
		}
		privs, scope, ok := parseMySQLGrant(grant)
		if !ok {
			continue
		}
		if scope != "*.*" && scope != sqlsyntax.QuoteIdent(p.Target.Database)+".*" {
			continue
		}
		for _, priv := range strings.Split(privs, ",") {
			priv = strings.TrimSpace(priv)
			if priv == "ALL PRIVILEGES" {
				for p := range required {
					granted[p] = true
				}
			} else {
				granted[priv] = true
			}
		}
	}
	if e = rows.Err(); e != nil {
		return e
	}
	var missing []string
	for priv := range required {
		if !granted[priv] {
			missing = append(missing, priv)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		return fmt.Errorf("无法确认目标 database 级权限 %s；请授予直接权限（仅表级或角色继承权限首版不作推测）", strings.Join(missing, ", "))
	}
	return nil
}
func parseMySQLGrant(grant string) (string, string, bool) {
	if !strings.HasPrefix(grant, "GRANT ") {
		return "", "", false
	}
	on := strings.Index(grant, " ON ")
	if on < 0 {
		return "", "", false
	}
	scopeStart := on + 4
	quote := byte(0)
	for i := scopeStart; i < len(grant); i++ {
		c := grant[i]
		if quote != 0 {
			if c == quote {
				if i+1 < len(grant) && grant[i+1] == quote {
					i++
				} else {
					quote = 0
				}
			}
			continue
		}
		if c == '`' || c == '\'' {
			quote = c
			continue
		}
		if strings.HasPrefix(grant[i:], " TO ") {
			return strings.ToUpper(grant[6:on]), grant[scopeStart:i], true
		}
	}
	return "", "", false
}
