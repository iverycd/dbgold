package datamigrate

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/pgoracle"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/sqlsyntax"
	"dbgold/datamigrate/typemap"
)

func oracleDeferred(deferrable, initially bool) string {
	if !deferrable {
		return " NOT DEFERRABLE"
	}
	if initially {
		return " DEFERRABLE INITIALLY DEFERRED"
	}
	return " DEFERRABLE INITIALLY IMMEDIATE"
}
func oracleValidated(valid bool) string {
	if valid {
		return " ENABLE VALIDATE"
	}
	return " ENABLE NOVALIDATE"
}

func (m *Migrator) pgOracleIndexSQL(t source.PGTable, i source.IndexInfo, d *dialect.OracleDialect) (string, string, error) {
	name := m.objName(i.IndexName)
	if i.PG != nil && i.PG.ConstraintName != "" {
		name = m.objName(i.PG.ConstraintName)
	}
	if i.Unsupported != "" {
		return name, "", fmt.Errorf("%s", i.Unsupported)
	}
	if len(i.Columns) == 0 || i.Method != "" && !strings.EqualFold(i.Method, "btree") {
		return name, "", fmt.Errorf("仅支持普通 BTREE 列索引")
	}
	if g := i.PG; g != nil {
		if !g.Valid || g.Expressions != "" || g.Predicate != "" || g.KeyCount != len(i.Columns) || g.NullsNotDistinct || g.NonDefaultOpClass || g.NonDefaultCollation {
			return name, "", fmt.Errorf("索引包含无效状态、表达式、部分条件、INCLUDE、NULLS NOT DISTINCT 或未适配排序/操作符类")
		}
	}
	cols := make([]string, len(i.Columns))
	nullable := false
	for j, c := range i.Columns {
		var found *source.ColumnInfo
		for k := range t.Info.Columns {
			if t.Info.Columns[k].Name == c {
				found = &t.Info.Columns[k]
				break
			}
		}
		if found == nil {
			return name, "", fmt.Errorf("索引列 %s 元数据缺失", c)
		}
		mapped, e := typemap.PGToOracleChecked(*found, m.cfg.UseNvarchar2, d.CharBytes, d.NCharBytes)
		if e != nil {
			return name, "", e
		}
		if strings.Contains(mapped, "LOB") {
			return name, "", fmt.Errorf("Oracle 普通索引不支持 LOB 列 %s", c)
		}
		nullable = nullable || found.IsNullable
		cols[j] = d.QuoteIdent(m.objName(c))
	}
	if i.IsUnique && len(cols) > 1 && nullable {
		return name, "", fmt.Errorf("含可空列的复合 UNIQUE 在 PG 与 Oracle 中 NULL 规则不同，不自动创建")
	}
	if i.IsUnique && strings.EqualFold(i.Locality, "local") && t.Info.Partition != nil {
		keys, expression := partitionKeyColumns(t.Info.Partition)
		if expression {
			return name, "", fmt.Errorf("LOCAL 唯一索引与表达式分区的等价规则尚未适配")
		}
		for _, key := range keys {
			if !slices.Contains(i.Columns, key) {
				return name, "", fmt.Errorf("LOCAL 唯一索引未包含分区键 %s，不能改为更严格的全局唯一约束", key)
			}
		}
	}
	constraint := i.IsPrimary || i.PG != nil && i.PG.ConstraintType == "u"
	if constraint {
		kind := "UNIQUE"
		if i.IsPrimary {
			kind = "PRIMARY KEY"
		}
		flags := ""
		if g := i.PG; g != nil {
			flags = oracleDeferred(g.Deferrable, g.InitiallyDeferred) + oracleValidated(g.Validated)
		}
		return name, "ALTER TABLE " + d.QualifyTable(m.cfg.TargetSchema, m.objName(t.Info.TableName)) + " ADD CONSTRAINT " + d.QuoteIdent(name) + " " + kind + " (" + strings.Join(cols, ", ") + ")" + flags, nil
	}
	for j := range cols {
		if j < len(i.Descending) && i.Descending[j] {
			cols[j] += " DESC"
		}
	}
	u := ""
	if i.IsUnique {
		u = "UNIQUE "
	}
	return name, "CREATE " + u + "INDEX " + d.QualifyTable(m.cfg.TargetSchema, name) + " ON " + d.QualifyTable(m.cfg.TargetSchema, m.objName(t.Info.TableName)) + " (" + strings.Join(cols, ", ") + ")", nil
}

func (m *Migrator) planPGOracleTableObjects(p *PGOraclePreflight, t source.PGTable, d *dialect.OracleDialect) {
	for _, i := range t.Indexes {
		ref := idxRef(i)
		p.indexes[ref] = i
		if !m.wantPGOracle(p, ref.Kind) || p.excluded(ref) {
			continue
		}
		name, ddl, e := m.pgOracleIndexSQL(t, i, d)
		p.sourceDDL[ref] = i.Definition
		ns := "INDEX"
		if i.IsPrimary || i.PG != nil && i.PG.ConstraintType == "u" {
			ns = "CONSTRAINT"
		}
		p.register(ref, name, ns)
		p.dependencies[ref] = []ObjectRef{tableRef(t.Info.TableName)}
		obj := PlannedObject{Object: ref, Target: name, Stage: "index", Statements: []dialect.Statement{}}
		if e == nil {
			obj.Statements = append(obj.Statements, dialect.Statement{SQL: ddl})
		} else {
			p.add(ref, e)
		}
		p.Objects = append(p.Objects, obj)
	}
	for _, f := range t.ForeignKeys {
		p.foreignKeys[ObjectRef{Kind: "foreign_key", Table: t.Info.TableName, Name: f.ConstraintName}] = f
	}
	if m.wantPGOracle(p, "check") {
		checks := append([]source.PGObject{}, t.Checks...)
		for _, u := range t.Unsupported {
			if u.Kind == "check" || u.Kind == "constraint" {
				checks = append(checks, u)
			}
		}
		for _, c := range checks {
			ref := ObjectRef{Kind: "check", Table: t.Info.TableName, Name: c.Name}
			if p.excluded(ref) {
				continue
			}
			p.sourceDDL[ref] = c.Definition
			p.register(ref, m.objName(c.Name), "CONSTRAINT")
			p.dependencies[ref] = []ObjectRef{tableRef(t.Info.TableName)}
			var sql string
			var e error
			if c.Reason != "" {
				e = fmt.Errorf("%s", c.Reason)
			} else if c.PG != nil && (c.PG.Type != "c" || c.PG.NoInherit) {
				e = fmt.Errorf("排除约束或 NO INHERIT CHECK 不自动转换")
			} else {
				var expression string
				expression, e = pgOracleCheckBody(c.Definition)
				if e == nil {
					expression, e = pgoracle.CheckExpression(expression, t.Info.Columns, m.objName)
				}
				if e == nil {
					flags := " ENABLE VALIDATE"
					if c.PG != nil {
						flags = oracleValidated(c.PG.Validated)
					}
					sql = "ALTER TABLE " + d.QualifyTable(m.cfg.TargetSchema, m.objName(t.Info.TableName)) + " ADD CONSTRAINT " + d.QuoteIdent(m.objName(c.Name)) + " CHECK (" + expression + ")" + flags
				}
			}
			obj := PlannedObject{Object: ref, Target: m.objName(c.Name), Stage: "check", Statements: []dialect.Statement{}}
			if e != nil {
				p.add(ref, e)
			} else {
				obj.Statements = append(obj.Statements, dialect.Statement{SQL: sql})
			}
			p.Objects = append(p.Objects, obj)
		}
	}
	if m.wantPGOracle(p, "comment") {
		comments := []source.CommentInfo{}
		if t.Info.Comment != "" {
			comments = append(comments, source.CommentInfo{TableName: t.Info.TableName, Comment: t.Info.Comment})
		}
		for _, c := range t.Info.Columns {
			if c.Comment != "" {
				comments = append(comments, source.CommentInfo{TableName: t.Info.TableName, ColumnName: c.Name, Comment: c.Comment})
			}
		}
		for _, c := range comments {
			name := t.Info.TableName
			if c.ColumnName != "" {
				name += "." + c.ColumnName
			}
			ref := ObjectRef{Kind: "comment", Table: t.Info.TableName, Name: name}
			if p.excluded(ref) {
				continue
			}
			p.dependencies[ref] = []ObjectRef{tableRef(t.Info.TableName)}
			c.TableName, c.ColumnName = m.objName(c.TableName), m.objName(c.ColumnName)
			p.Objects = append(p.Objects, PlannedObject{Object: ref, Target: m.objName(name), Stage: "comment", Statements: d.CommentStatements(m.cfg.TargetSchema, c)})
		}
	}
}

func pgOracleCheckBody(def string) (string, error) {
	ts, e := sqlsyntax.Lex(def)
	if e != nil {
		return "", e
	}
	if len(ts) < 3 || !ts[0].Is("CHECK") || !ts[1].Is("(") {
		return "", fmt.Errorf("CHECK 原始定义不完整")
	}
	depth := 1
	var body []string
	for i := 2; i < len(ts); i++ {
		t := ts[i]
		if t.Is("(") {
			depth++
		}
		if t.Is(")") {
			depth--
		}
		if depth == 0 {
			for _, tail := range ts[i+1:] {
				if !tail.Is("NOT") && !tail.Is("VALID") && !tail.Is("NO") && !tail.Is("INHERIT") {
					return "", fmt.Errorf("CHECK 包含未支持的尾部定义")
				}
			}
			return strings.Join(body, " "), nil
		}
		body = append(body, t.Text)
	}
	return "", fmt.Errorf("CHECK 括号不完整")
}

func (m *Migrator) planPGOracleForeignKeys(p *PGOraclePreflight, d *dialect.OracleDialect) {
	if !m.wantPGOracle(p, "foreign_key") {
		return
	}
	refs := []ObjectRef{}
	for ref := range p.foreignKeys {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Table+refs[i].Name < refs[j].Table+refs[j].Name })
	for _, ref := range refs {
		if p.excluded(ref) {
			continue
		}
		f := p.foreignKeys[ref]
		p.register(ref, m.objName(f.ConstraintName), "CONSTRAINT")
		p.dependencies[ref] = []ObjectRef{tableRef(f.TableName), tableRef(f.RefTable)}
		if f.PG != nil {
			p.sourceDDL[ref] = f.PG.Definition
		}
		e := dialect.ValidateOracleForeignKey(f)
		if e == nil && f.RefSchema != "" && f.RefSchema != p.Source.Schema {
			e = fmt.Errorf("跨 Schema 外键未映射")
		}
		if e == nil && f.PG != nil && f.PG.Match != "" && f.PG.Match != "s" {
			e = fmt.Errorf("仅支持 MATCH SIMPLE 外键")
		}
		if e == nil && f.PG != nil && f.PG.Deferrable && (strings.EqualFold(f.OnUpdate, "RESTRICT") || strings.EqualFold(f.OnDelete, "RESTRICT")) {
			e = fmt.Errorf("延期外键的 RESTRICT 动作不能改为可延期的 NO ACTION")
		}
		found := false
		for ir, idx := range p.indexes {
			if idx.TableName != f.RefTable || !slices.Equal(idx.Columns, f.RefColumns) || !idx.IsPrimary && (idx.PG == nil || idx.PG.ConstraintType != "u") {
				continue
			}
			found = true
			p.dependencies[ref] = append(p.dependencies[ref], ir)
			for _, t := range p.tables {
				if t.Info.TableName == idx.TableName {
					_, _, ie := m.pgOracleIndexSQL(t, idx, d)
					if ie != nil && e == nil {
						e = fmt.Errorf("引用键无法等价迁移: %w", ie)
					}
				}
			}
			break
		}
		if !found && e == nil {
			e = fmt.Errorf("缺少迁移范围内可引用的主键或唯一约束")
		}
		obj := PlannedObject{Object: ref, Target: m.objName(f.ConstraintName), Stage: "foreign_key", Statements: []dialect.Statement{}}
		if e == nil {
			cols := mapNames(f.Columns, func(s string) string { return d.QuoteIdent(m.objName(s)) })
			references := mapNames(f.RefColumns, func(s string) string { return d.QuoteIdent(m.objName(s)) })
			sql := "ALTER TABLE " + d.QualifyTable(m.cfg.TargetSchema, m.objName(f.TableName)) + " ADD CONSTRAINT " + d.QuoteIdent(obj.Target) + " FOREIGN KEY (" + strings.Join(cols, ", ") + ") REFERENCES " + d.QualifyTable(m.cfg.TargetSchema, m.objName(f.RefTable)) + " (" + strings.Join(references, ", ") + ")"
			if action := strings.ToUpper(f.OnDelete); action == "CASCADE" || action == "SET NULL" {
				sql += " ON DELETE " + action
			}
			if f.PG != nil {
				sql += oracleDeferred(f.PG.Deferrable, f.PG.InitiallyDeferred) + oracleValidated(f.PG.Validated)
			}
			obj.Statements = append(obj.Statements, dialect.Statement{SQL: sql})
		} else {
			p.add(ref, e)
		}
		p.Objects = append(p.Objects, obj)
	}
}

func (m *Migrator) planPGOracleSequences(p *PGOraclePreflight, sequences []source.PGSequence) {
	if !m.wantPGOracle(p, "sequence") && !p.dataOnly {
		return
	}
	selected := map[string]bool{}
	for _, t := range p.tables {
		selected[t.Info.TableName] = true
	}
	for _, s := range sequences {
		ref := ObjectRef{Kind: "sequence", Name: s.Name}
		if p.excluded(ref) {
			continue
		}
		plan := &pgoracle.SequencePlan{Source: s, Schema: m.cfg.TargetSchema, Name: m.objName(s.Name), Bindings: []pgoracle.Binding{}}
		if s.Schema != p.Source.Schema {
			p.add(ref, fmt.Errorf("序列 %s.%s 来自未映射的源 Schema", s.Schema, s.Name))
		}
		for _, b := range s.Bindings {
			if b.Schema != p.Source.Schema || !selected[b.TableName] || b.Identity == "" && b.Default == "" {
				continue
			}
			if b.Identity == "" && !dialect.PGOracleDirectSequenceDefault(b.Default) {
				p.add(ref, fmt.Errorf("序列绑定 %s.%s 不是直接 nextval 默认值", b.TableName, b.ColumnName))
				continue
			}
			plan.Bindings = append(plan.Bindings, pgoracle.Binding{SourceTable: b.TableName, SourceColumn: b.ColumnName, Table: m.objName(b.TableName), Column: m.objName(b.ColumnName), Trigger: dialect.OracleObjectName("TRG", b.TableName, b.ColumnName)})
			p.dependencies[ref] = append(p.dependencies[ref], tableRef(b.TableName))
		}
		if len(s.Bindings) > 0 && len(plan.Bindings) == 0 {
			continue
		}
		p.sequences[ref] = plan
		if p.dataOnly {
			continue
		}
		p.register(ref, plan.Name, "RELATION")
		ss, e := plan.SequenceStatements()
		if s.Unsupported != "" {
			e = fmt.Errorf("%s", s.Unsupported)
		}
		p.sourceDDL[ref] = fmt.Sprintf("SEQUENCE %s.%s INCREMENT %d MINVALUE %d MAXVALUE %d CACHE %d CYCLE %t; last_value=%d is_called=%t", s.Schema, s.Name, s.Increment, s.Min, s.Max, s.Cache, s.Cycle, s.LastValue, s.IsCalled)
		if e != nil {
			p.add(ref, e)
		}
		p.Objects = append(p.Objects, PlannedObject{Object: ref, Target: plan.Name, Stage: "sequence", Statements: ss})
		p.warn(ref, "按源持久分配位置接续序列，源会话缓存不迁移，允许编号间隙")
		for _, b := range plan.Bindings {
			tr := ObjectRef{Kind: "trigger", Table: b.SourceTable, Name: b.Trigger}
			p.register(tr, b.Trigger, "TRIGGER")
			p.dependencies[tr] = []ObjectRef{ref, tableRef(b.SourceTable)}
			p.Objects = append(p.Objects, PlannedObject{Object: tr, Target: b.Trigger, Stage: "trigger", Statements: []dialect.Statement{{SQL: plan.TriggerSQL(b)}}})
			p.warn(tr, "自动编号使用序列与 INSERT 触发器：显式 NULL 也会取号，identity ALWAYS 不限制显式赋值")
		}
	}
}

func (m *Migrator) planPGOracleViews(p *PGOraclePreflight, views []source.ViewInfo, relations map[string][]source.ColumnInfo) {
	d := m.writer.Dialect()
	viewNames := map[string]bool{}
	for _, v := range views {
		viewNames[v.ViewName] = true
	}
	for _, v := range views {
		ref := ObjectRef{Kind: "view", Name: v.ViewName}
		if p.excluded(ref) || p.viewOnly && !slices.Contains(m.cfg.ViewNames, v.ViewName) {
			continue
		}
		p.sourceDDL[ref] = v.Definition
		p.register(ref, m.objName(v.ViewName), "RELATION")
		def, deps, e := pgoracle.TranslateView(v, p.Source.Schema, m.cfg.TargetSchema, relations, m.objName)
		for _, dep := range deps {
			kind := "table"
			if viewNames[dep] {
				kind = "view"
			}
			p.dependencies[ref] = append(p.dependencies[ref], ObjectRef{Kind: kind, Name: dep})
		}
		obj := PlannedObject{Object: ref, Target: m.objName(v.ViewName), Stage: "view", Statements: []dialect.Statement{}}
		if e != nil {
			p.add(ref, e)
		} else {
			v.ViewName = obj.Target
			v.Definition = def
			v.Columns = slices.Clone(v.Columns)
			for i := range v.Columns {
				v.Columns[i].Name = m.objName(v.Columns[i].Name)
			}
			obj.Statements = d.ViewStatements(m.cfg.TargetSchema, v)
		}
		p.Objects = append(p.Objects, obj)
	}
	for _, name := range m.cfg.ViewNames {
		if !viewNames[name] {
			ref := ObjectRef{Kind: "view", Name: name}
			p.add(ref, fmt.Errorf("源视图不存在或不可见"))
			p.Objects = append(p.Objects, PlannedObject{Object: ref, Target: m.objName(name), Stage: "view", Statements: []dialect.Statement{}})
		}
	}
	// A stable topological order also gives cycles explicit object failures.
	var pending, ordered []PlannedObject
	for _, o := range p.Objects {
		if o.Object.Kind == "view" {
			pending = append(pending, o)
		} else {
			ordered = append(ordered, o)
		}
	}
	done := map[ObjectRef]bool{}
	for len(pending) > 0 {
		next := []PlannedObject{}
		progress := false
		for _, o := range pending {
			ready := true
			for _, dep := range p.dependencies[o.Object] {
				if dep.Kind == "view" {
					for _, candidate := range pending {
						if candidate.Object == dep && !done[dep] {
							ready = false
						}
					}
				}
			}
			if ready {
				ordered = append(ordered, o)
				done[o.Object] = true
				progress = true
			} else {
				next = append(next, o)
			}
		}
		if !progress {
			for _, o := range next {
				p.add(o.Object, fmt.Errorf("视图依赖循环"))
				ordered = append(ordered, o)
			}
			break
		}
		pending = next
	}
	p.Objects = ordered
}
