package damengnative

import (
	"database/sql"
	"fmt"
	"math/big"
	"sort"
	"strings"
)

func (r *catalogReader) getDDL(kind, name string) (string, error) {
	if kind == "MATERIALIZED_VIEW_LOG" {
		name = r.materializedLogDDLName(name)
	}
	var ddl sql.NullString
	err := r.db.QueryRowContext(r.ctx, `SELECT DBMS_METADATA.GET_DDL(?,?,?)`, kind, name, r.schema).Scan(&ddl)
	if err != nil {
		return "", err
	}
	if !ddl.Valid || strings.TrimSpace(ddl.String) == "" {
		return "", fmt.Errorf("原生 DDL 为空")
	}
	return StripClientDelimiter(ddl.String), nil
}
func (r *catalogReader) extractDefinitions() {
	// No session transform is used here: TABLE definitions retain storage,
	// cluster keys, domain references, generated columns and exact defaults.
	materialized := map[int]bool{}
	// Establish generated storage ownership before touching any internal table
	// or timer definition, regardless of their object-ID allocation order.
	for i, o := range r.snapshot.Objects {
		if o.Kind != "materialized_view" || o.Managed {
			continue
		}
		materialized[i] = true
		_, _, r.snapshot.Objects[i].DropSQL, r.snapshot.Objects[i].CompileSQL = ddlOptions(o)
		ddl, err := r.getDDL("MATERIALIZED_VIEW", o.Name)
		if err != nil {
			r.issue(&r.snapshot.Objects[i], "读取物化视图 DDL 失败: "+err.Error())
			continue
		}
		if err = r.materializedSnapshot(i, ddl); err != nil {
			r.issue(&r.snapshot.Objects[i], err.Error())
		}
	}
	for i := 0; i < len(r.snapshot.Objects); i++ {
		if materialized[i] {
			continue
		}
		o := &r.snapshot.Objects[i]
		if o.Managed || len(o.Units) > 0 {
			continue
		}
		if o.Kind == "table" && reservedNativeTableName(o.Name) {
			r.issue(o, "系统保留承载表缺少可确认的父对象，不能作为普通用户表回放")
			continue
		}
		kind, phase, drop, compile := ddlOptions(*o)
		if kind == "" {
			r.issue(o, "原生目录对象类型暂不能安全回放: "+o.Kind)
			continue
		}
		o.DropSQL, o.CompileSQL = drop, compile
		if o.Kind == "constraint" { // Table-native constraints are recreated with the
			// table, including clustered PKs not supported by GET_DDL(CONSTRAINT).
			o.Managed = true
			continue
		}
		ddl, err := r.getDDL(kind, o.Name)
		if err != nil {
			if o.Kind == "db_link" {
				r.issue(o, "读取 DBLink 原生定义失败（服务端详情已脱敏）")
			} else {
				r.issue(o, "读取原生 DDL 失败: "+err.Error())
			}
			continue
		}
		if o.Kind == "db_link" {
			unit, e := FreezeLinkUnit(ddl, phase)
			if e != nil {
				r.issue(o, e.Error())
			} else {
				o.Units = append(o.Units, unit)
			}
			continue
		}
		if _, err = nativeTokens(ddl); err != nil {
			r.issue(o, err.Error())
			continue
		}
		if o.Kind == "table" {
			originalDDL := ddl
			ddl, foundations, delayed, err := r.freezeTableKeyAndValidationPlan(o.Name, ddl)
			if err != nil {
				r.issue(o, err.Error())
				continue
			}
			prepared := append(append([]Unit{}, foundations...), delayed...)
			ddl = r.disableConstraintDefinitions(o.Name, ddl, prepared...)
			r.checkExistingIndexReferences(o, ddl)
			base, deferred, err := deferForeignKeys(ddl, r.schema, o.Name)
			if err != nil {
				r.issue(o, err.Error())
				continue
			}
			base = r.stripGeneratedConstraintNames(o.Name, base)
			o.Units = append(o.Units, Unit{SQL: base, Phase: phase})
			for _, unit := range foundations {
				unit.SQL = r.stripGeneratedConstraintNames(o.Name, unit.SQL)
				o.Units = append(o.Units, unit)
			}
			for _, unit := range delayed {
				unit.SQL = r.stripGeneratedConstraintNames(o.Name, unit.SQL)
				o.Units = append(o.Units, unit)
			}
			deferred, err = r.freezeTableForeignKeys(o.Name, len(deferred))
			if err != nil {
				r.issue(o, err.Error())
				continue
			}
			for _, unit := range deferred {
				unit = r.stripGeneratedConstraintNames(o.Name, unit)
				o.Units = append(o.Units, Unit{SQL: unit, Phase: "after_data"})
			}
			r.manageTableForeignKeys(o.Name)
			r.restoreConstraintStates(o, originalDDL)
		} else if o.Kind == "sequence" {
			ddl, err = r.sequenceDDL(o, ddl)
			if err != nil {
				r.issue(o, err.Error())
				continue
			}
			o.Units = append(o.Units, Unit{SQL: ddl, Phase: phase})
		} else if o.Kind == "materialized_view" {
			if err = r.materializedSnapshot(i, ddl); err != nil {
				r.issue(&r.snapshot.Objects[i], err.Error())
			}
		} else {
			if o.Kind == "trigger" && (o.EventTrigger || nativeTriggerIsEvent(ddl)) {
				o.EventTrigger = true
				if scopeKind, scope, scopeErr := nativeTriggerTarget(ddl, r.schema); scopeErr != nil {
					r.issue(o, scopeErr.Error())
					continue
				} else if scopeKind == "schema" && scope != r.schema {
					o.External = append(o.External, Reference{Kind: "schema", Schema: scope, Name: scope})
				}
				ddl, err = DisabledEventTriggerDDL(ddl)
				if err != nil {
					r.issue(o, err.Error())
					continue
				}
				phase = "event_create"
			}
			o.Units = append(o.Units, Unit{SQL: ddl, Phase: phase})
		}
		if o.Kind == "trigger" {
			var status string
			if err = r.db.QueryRowContext(r.ctx, `SELECT STATUS FROM ALL_TRIGGERS WHERE OWNER=? AND TRIGGER_NAME=?`, r.schema, o.Name).Scan(&status); err != nil {
				r.issue(o, "触发器启用状态不可读取: "+err.Error())
			} else {
				switch strings.ToUpper(strings.TrimSpace(status)) {
				case "DISABLED", "DISABLE", "N", "0":
					o.Status = "DISABLED"
					if !o.EventTrigger {
						o.Units = append(o.Units, Unit{SQL: "ALTER TRIGGER " + Qualified(r.schema, o.Name) + " DISABLE", Phase: "restore"})
					}
				case "ENABLED", "ENABLE", "Y", "1":
					o.Status = "ENABLED"
				default:
					r.issue(o, "触发器启用状态不能识别: "+status)
				}
				if o.EventTrigger && (o.Status == "ENABLED" || o.Status == "DISABLED") {
					o.Units = append(o.Units, Unit{SQL: EventTriggerStateSQL(r.schema, o.Name, o.Status == "ENABLED"), Phase: "event_activate"})
				}
			}
		}
	}
}

func reservedNativeTableName(name string) bool {
	upper := strings.ToUpper(name)
	for _, prefix := range []string{"BM$_", "BMJ$_", "MDRT$_", "MLOG$_", "MTAB$_", "MVIEW$_", "MTRG$_", "STAT$_", "BIN$_"} {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	for _, suffix := range []string{"$ALOG", "$AUX", "$DAUX", "$RAUX", "$ROT", "$UAUX"} {
		if strings.HasSuffix(upper, suffix) {
			return true
		}
	}
	return false
}

func ddlOptions(o Object) (kind, phase, drop, compile string) {
	q := Qualified(o.Schema, o.Name)
	phase = "after_data"
	switch o.Kind {
	case "table":
		return "TABLE", "before_data", "DROP TABLE " + q + " CASCADE CONSTRAINTS", ""
	case "sequence":
		return "SEQUENCE", "before_data", "DROP SEQUENCE " + q, ""
	case "domain":
		return "DOMAIN", "before_data", "DROP DOMAIN " + q, ""
	case "type":
		return "TYPE", "before_data", "DROP TYPE " + q, "ALTER TYPE " + q + " COMPILE"
	case "class":
		return "CLASS_HEAD", "before_data", "DROP CLASS " + q, "ALTER CLASS " + q + " COMPILE"
	case "class_body":
		return "CLASS_BODY", "after_data", "DROP CLASS BODY " + q, "ALTER CLASS " + q + " COMPILE"
	case "package":
		return "PKG_SPEC", "before_data", "DROP PACKAGE " + q, "ALTER PACKAGE " + q + " COMPILE"
	case "package_body":
		return "PKG_BODY", "after_data", "DROP PACKAGE BODY " + q, "ALTER PACKAGE " + q + " COMPILE"
	case "function":
		return "FUNCTION", phase, "DROP FUNCTION " + q, "ALTER FUNCTION " + q + " COMPILE"
	case "procedure":
		return "PROCEDURE", phase, "DROP PROCEDURE " + q, "ALTER PROCEDURE " + q + " COMPILE"
	case "view":
		return "VIEW", phase, "DROP VIEW " + q, "ALTER VIEW " + q + " COMPILE"
	case "trigger":
		return "TRIGGER", phase, "DROP TRIGGER " + q, ""
	case "synonym":
		return "SYNONYM", phase, "DROP SYNONYM " + q, ""
	case "db_link":
		return "DB_LINK", "before_data", "DROP LINK " + q, ""
	case "index":
		return "INDEX", phase, "DROP INDEX " + q, ""
	case "context_index":
		return "CONTEXT_INDEX", phase, "DROP CONTEXT INDEX " + q, ""
	case "constraint":
		return "CONSTRAINT", phase, "ALTER TABLE " + Qualified(o.Schema, o.Table) + " DROP CONSTRAINT " + Quote(o.Name), ""
	case "materialized_view":
		return "MATERIALIZED_VIEW", phase, "DROP MATERIALIZED VIEW " + q, ""
	case "materialized_view_log":
		return "MATERIALIZED_VIEW_LOG", phase, "DROP MATERIALIZED VIEW LOG ON " + Qualified(o.Schema, o.Table), ""
	case "context":
		return "CONTEXT", "before_data", "DROP CONTEXT " + Quote(o.Name), ""
	case "jclass":
		return "JCLASS", "before_data", "", ""
	default:
		return "", "", "", ""
	}
}

func (r *catalogReader) manageTableForeignKeys(table string) {
	for i := range r.snapshot.Objects {
		o := &r.snapshot.Objects[i]
		if o.Kind == "constraint" && o.Table == table && r.constraints[o.Name] == "R" {
			o.Managed = true
		}
	}
}

func (r *catalogReader) stripGeneratedConstraintNames(table, ddl string) string {
	names := map[string]bool{}
	for _, o := range r.snapshot.Objects {
		if o.Kind == "constraint" && o.Table == table && o.Generated {
			names[o.Name] = true
		}
	}
	ts, err := nativeTokens(ddl)
	if err != nil {
		return ddl
	}
	var out strings.Builder
	last := 0
	for i := 0; i+1 < len(ts); i++ {
		if ts[i].kind == 'i' && strings.EqualFold(ts[i].value, "CONSTRAINT") && names[ts[i+1].value] {
			out.WriteString(ddl[last:ts[i].start])
			last = ts[i+1].end
			i++
		}
	}
	out.WriteString(ddl[last:])
	return out.String()
}

func (r *catalogReader) restoreConstraintStates(table *Object, ddl string) {
	// A constraint can refer to a separately named, user-created index. Such
	// an index is not regenerated merely because the key columns match.
	for _, idx := range r.snapshot.Objects {
		if idx.Kind != "index" || idx.Table != table.Name || !idx.Managed || idx.Generated {
			continue
		}
		definitions := ddl
		for _, unit := range table.Units {
			definitions += "\n" + unit.SQL
		}
		tokens, _ := nativeTokens(definitions)
		found := false
		for _, t := range tokens {
			if t.kind == 'q' && t.value == idx.Name || t.kind == 'i' && strings.ToUpper(t.value) == idx.Name {
				found = true
			}
		}
		if !found {
			r.issue(&idx, "约束使用的用户命名索引未包含在原生建表定义中，无法证明重建后仍保留该索引名称")
		}
	}
	for _, c := range r.snapshot.Objects {
		if c.Kind != "constraint" || c.Table != table.Name || c.Status != "DISABLED" {
			continue
		}
		if c.ConstraintType == "R" {
			continue
		} // FKs carry their complete frozen state atomically in their ADD unit.
		if r.hasFrozenConstraintUnit(c, table.Units) {
			continue
		}
		if c.Generated {
			ts, _ := nativeTokens(ddl)
			preserved := false
			for i := 0; i+1 < len(ts); i++ {
				if ts[i].kind == 'i' && strings.EqualFold(ts[i].value, "CONSTRAINT") && ts[i+1].value == c.Name {
					depth := 0
					for _, t := range ts[i+2:] {
						if t.kind == 'p' && t.value == "(" {
							depth++
						}
						if t.kind == 'p' && t.value == ")" {
							depth--
							if depth < 0 {
								break
							}
						}
						if depth == 0 && t.kind == 'p' && t.value == "," {
							break
						}
						if depth == 0 && t.kind == 'i' && strings.EqualFold(t.value, "DISABLE") {
							preserved = true
						}
					}
				}
			}
			if !preserved {
				r.issue(&c, "系统生成名称的禁用约束未包含可确认的 DISABLE 状态，不能安全恢复其状态")
			}
			continue
		}
		phase := "before_data"
		if c.ConstraintType == "R" {
			continue
		} // FKs are disabled atomically in ADD.
		table.Units = append(table.Units, Unit{SQL: "ALTER TABLE " + Qualified(r.schema, table.Name) + " DISABLE CONSTRAINT " + Quote(c.Name), Phase: phase})
	}
}

func (r *catalogReader) disableConstraintDefinitions(table, ddl string, prepared ...Unit) string {
	states := map[string]Object{}
	for _, o := range r.snapshot.Objects {
		if o.Kind == "constraint" && o.Table == table && o.Status == "DISABLED" && o.ConstraintType != "R" && !r.hasFrozenConstraintUnit(o, prepared) {
			states[o.Name] = o
		}
	}
	if len(states) == 0 {
		return ddl
	}
	ts, err := nativeTokens(ddl)
	if err != nil {
		return ddl
	}
	type edit struct {
		start, end int
		text       string
	}
	var edits []edit
	found := map[string]bool{}
	for i := 0; i+1 < len(ts); i++ {
		if ts[i].kind != 'i' || !strings.EqualFold(ts[i].value, "CONSTRAINT") {
			continue
		}
		name := ts[i+1].value
		if ts[i+1].kind == 'i' {
			name = strings.ToUpper(name)
		}
		if _, ok := states[name]; !ok {
			continue
		}
		found[name] = true
		depth := 0
		end := ts[i+1].end
		hasState := false
		for j := i + 2; j < len(ts); j++ {
			t := ts[j]
			if t.kind == 'p' && t.value == "(" {
				depth++
			}
			if t.kind == 'p' && t.value == ")" {
				depth--
				if depth < 0 {
					break
				}
			}
			if depth == 0 && t.kind == 'p' && t.value == "," {
				break
			}
			if depth == 0 && t.kind == 'i' && (strings.EqualFold(t.value, "ENABLE") || strings.EqualFold(t.value, "DISABLE")) {
				hasState = true
				if strings.EqualFold(t.value, "ENABLE") {
					edits = append(edits, edit{t.start, t.end, "DISABLE"})
				}
			}
			end = t.end
		}
		if !hasState {
			edits = append(edits, edit{end, end, " DISABLE"})
		}
	}
	for name, o := range states {
		if !found[name] {
			r.issue(&o, "禁用约束未在原生定义中携带可确认的名称，无法保证从创建起保持禁用状态")
		}
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	for _, e := range edits {
		ddl = ddl[:e.start] + e.text + ddl[e.end:]
	}
	return ddl
}

func (r *catalogReader) checkExistingIndexReferences(table *Object, ddl string) {
	ts, err := nativeTokens(ddl)
	if err != nil {
		return
	}
	storage := map[string]bool{"STORAGE": true, "TABLESPACE": true, "PCTFREE": true, "INITRANS": true, "MAXTRANS": true, "PARALLEL": true, "NOPARALLEL": true, "LOGGING": true, "NOLOGGING": true, "LOCAL": true, "GLOBAL": true, "ONLINE": true, "REVERSE": true, "COMPRESS": true, "NOCOMPRESS": true, "ENABLE": true, "DISABLE": true, "VALIDATE": true, "NOVALIDATE": true, "CREATE": true}
	for i := 0; i+2 < len(ts); i++ {
		if ts[i].kind != 'i' || !strings.EqualFold(ts[i].value, "USING") || !strings.EqualFold(ts[i+1].value, "INDEX") {
			continue
		}
		t := ts[i+2]
		if t.kind == 'p' && t.value == "(" || t.kind != 'q' && t.kind != 'i' || t.kind == 'i' && storage[strings.ToUpper(t.value)] {
			continue
		}
		name := t.value
		owner := r.schema
		if t.kind == 'i' {
			name = strings.ToUpper(name)
		}
		if i+4 < len(ts) && ts[i+3].kind == 'p' && ts[i+3].value == "." {
			owner, name = name, ts[i+4].value
			if ts[i+4].kind == 'i' {
				name = strings.ToUpper(name)
			}
		}
		r.issue(table, "原生约束 USING INDEX 引用了既有索引 "+Qualified(owner, name)+"，当前无法在保留聚集/约束语义的同时先建立该索引；已在覆盖前阻止")
	}
}

// deferForeignKeys changes only the top-level FK clauses in a native CREATE
// TABLE. It never parses or rewrites column defaults, CHECK expressions or SQL
// inside quoted strings. An unfamiliar inline shape blocks before any writes.
func deferForeignKeys(ddl, schema, table string) (string, []string, error) {
	tokens, err := nativeTokens(ddl)
	if err != nil {
		return "", nil, err
	}
	open, close := -1, -1
	depth := 0
	for i, t := range tokens {
		if t.kind == 'p' && t.value == "(" {
			if open < 0 {
				open = i
			}
			depth++
		} else if t.kind == 'p' && t.value == ")" {
			depth--
			if depth == 0 && open >= 0 {
				close = i
				break
			}
		}
	}
	if open < 0 || close < 0 {
		return "", nil, fmt.Errorf("表 %s 的原生 DDL 缺少完整列定义", table)
	}
	var pieces []string
	start := tokens[open].end
	depth = 0
	for _, t := range tokens[open+1 : close] {
		if t.kind != 'p' {
			continue
		}
		switch t.value {
		case "(":
			depth++
		case ")":
			depth--
		case ",":
			if depth == 0 {
				pieces = append(pieces, ddl[start:t.start])
				start = t.end
			}
		}
	}
	pieces = append(pieces, ddl[start:tokens[close].start])
	var keep, deferred []string
	for _, piece := range pieces {
		ts, e := nativeTokens(piece)
		if e != nil {
			return "", nil, e
		}
		ref, fk := -1, -1
		depth = 0
		for i, t := range ts {
			if t.kind == 'p' && t.value == "(" {
				depth++
			}
			if t.kind == 'p' && t.value == ")" {
				depth--
			}
			if depth == 0 && t.kind == 'i' {
				if strings.EqualFold(t.value, "REFERENCES") {
					ref = i
				}
				if strings.EqualFold(t.value, "FOREIGN") {
					fk = i
				}
			}
		}
		if ref < 0 {
			keep = append(keep, piece)
			continue
		}
		if fk >= 0 {
			deferred = append(deferred, "ALTER TABLE "+Qualified(schema, table)+" ADD "+strings.TrimSpace(piece))
			continue
		}
		if len(ts) < 2 || ts[0].kind != 'q' && ts[0].kind != 'i' {
			return "", nil, fmt.Errorf("表 %s 的行内外键不能安全拆分", table)
		}
		cut := ts[ref].start
		if ref >= 2 && strings.EqualFold(ts[ref-2].value, "CONSTRAINT") {
			cut = ts[ref-2].start
		}
		clause := strings.TrimSpace(piece[cut:])
		name := ""
		if strings.HasPrefix(strings.ToUpper(clause), "CONSTRAINT ") {
			tail, e := nativeTokens(clause)
			if e != nil || len(tail) < 3 {
				return "", nil, fmt.Errorf("行内外键名称不可读")
			}
			name = clause[:tail[2].start]
			clause = clause[tail[2].start:]
		}
		keep = append(keep, piece[:cut])
		columnName := ts[0].value
		if ts[0].kind == 'i' {
			columnName = strings.ToUpper(columnName)
		}
		deferred = append(deferred, "ALTER TABLE "+Qualified(schema, table)+" ADD "+name+"FOREIGN KEY ("+Quote(columnName)+") "+clause)
	}
	return ddl[:tokens[open].end] + strings.Join(keep, ",") + ddl[tokens[close].start:], deferred, nil
}

func (r *catalogReader) sequenceDDL(o *Object, ddl string) (string, error) {
	var last, minimum, maximum sql.NullString
	if err := r.db.QueryRowContext(r.ctx, `SELECT CAST(LAST_NUMBER AS VARCHAR(80)),CAST(MIN_VALUE AS VARCHAR(80)),CAST(MAX_VALUE AS VARCHAR(80)) FROM ALL_SEQUENCES WHERE SEQUENCE_OWNER=? AND SEQUENCE_NAME=?`, r.schema, o.Name).Scan(&last, &minimum, &maximum); err != nil {
		return "", fmt.Errorf("序列续号边界不可读取: %w", err)
	}
	value, ok := new(big.Int).SetString(last.String, 10)
	if !ok {
		return "", fmt.Errorf("序列续号边界非精确整数")
	}
	lo, ok1 := new(big.Int).SetString(minimum.String, 10)
	hi, ok2 := new(big.Int).SetString(maximum.String, 10)
	if !ok1 || !ok2 || value.Cmp(lo) < 0 || value.Cmp(hi) > 0 {
		return "", fmt.Errorf("序列已耗尽或 LAST_NUMBER 不在源序列边界内，不能将其伪装为可继续取号")
	}
	tokens, err := nativeTokens(ddl)
	if err != nil {
		return "", err
	}
	for i := 0; i+2 < len(tokens); i++ {
		if strings.EqualFold(tokens[i].value, "START") && strings.EqualFold(tokens[i+1].value, "WITH") {
			j := i + 2
			end := tokens[j].end
			if tokens[j].value == "-" || tokens[j].value == "+" {
				j++
				if j >= len(tokens) {
					return "", fmt.Errorf("序列 START WITH 不完整")
				}
				end = tokens[j].end
			}
			for j+1 < len(tokens) && tokens[j+1].start == end && tokens[j+1].kind == 'p' && tokens[j+1].value >= "0" && tokens[j+1].value <= "9" {
				j++
				end = tokens[j].end
			}
			r.snapshot.Issues = append(r.snapshot.Issues, Issue{ObjectID: o.ID, Kind: o.Kind, Name: o.Name, Severity: "warning", Reason: "序列以源 LAST_NUMBER 缓存边界续号；不读取 NEXTVAL，不消费源序列"})
			return ddl[:tokens[i+2].start] + value.String() + ddl[end:], nil
		}
	}
	return "", fmt.Errorf("序列原生 DDL 缺少 START WITH，不能安全冻结续号状态")
}

func (r *catalogReader) readCommentsAndGrants() error {
	// ALL_COL_COMMENTS can silently omit columns in a separately named Schema
	// owned by the current user on DM8. Read the native, exact-Schema catalogs;
	// SYSTABLECOMMENTS also includes sequence comments absent from ALL_TAB_COMMENTS.
	rows, err := r.db.QueryContext(r.ctx, `SELECT TVNAME,CAST(NULL AS VARCHAR(128)),COMMENT$ FROM SYS.SYSTABLECOMMENTS WHERE SCHNAME=? AND COMMENT$ IS NOT NULL UNION ALL SELECT TVNAME,COLNAME,COMMENT$ FROM SYS.SYSCOLUMNCOMMENTS WHERE SCHNAME=? AND COMMENT$ IS NOT NULL`, r.schema, r.schema)
	if err != nil {
		return fmt.Errorf("注释清单不可读取: %w", err)
	}
	for rows.Next() {
		var table, text string
		var col sql.NullString
		if err = rows.Scan(&table, &col, &text); err != nil {
			rows.Close()
			return err
		}
		parent := r.namedObject(table)
		if parent == nil || parent.Managed {
			continue
		}
		kind, qn := "TABLE", Qualified(r.schema, table)
		if parent.Kind == "sequence" {
			kind = "SEQUENCE"
		} else if parent.Kind == "view" {
			kind = "VIEW"
		}
		name := table
		if col.Valid && col.String != "" {
			kind, qn, name = "COLUMN", qn+"."+Quote(col.String), table+"."+Quote(col.String)
		}
		o := Object{Kind: "comment", Name: name, Schema: r.schema, Table: table, ParentID: parent.ID, Dependencies: []string{parent.ID}}
		if r.definitions {
			o.Units = []Unit{{SQL: "COMMENT ON " + kind + " " + qn + " IS '" + strings.ReplaceAll(text, "'", "''") + "'", Phase: "after_data"}}
		}
		r.add(0, o)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	rows, err = r.db.QueryContext(r.ctx, `SELECT g.OBJID,COUNT(*) FROM SYS.SYSGRANTS g JOIN SYS.SYSOBJECTS o ON o.ID=g.OBJID JOIN SYS.SYSOBJECTS s ON s.ID=o.SCHID WHERE s.NAME=? GROUP BY g.OBJID ORDER BY g.OBJID`, r.schema)
	if err != nil {
		return fmt.Errorf("对象/列授权清单不可读取: %w", err)
	}
	var granted []int64
	for rows.Next() {
		var id, count int64
		if err = rows.Scan(&id, &count); err != nil {
			rows.Close()
			return err
		}
		if count > 0 {
			granted = append(granted, id)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range granted {
		parent := r.base(id)
		if parent == nil {
			return fmt.Errorf("授权对象 %d 未包含在完整对象清单中", id)
		}
		o := Object{Kind: "object_grant", Name: parent.Name, Schema: r.schema, Table: parent.Table, ParentID: parent.ID, Dependencies: []string{parent.ID}}
		i := r.add(0, o)
		if !r.definitions {
			continue
		}
		var ddl sql.NullString
		err = r.db.QueryRowContext(r.ctx, `SELECT DBMS_METADATA.GET_DEPENDENT_DDL('OBJECT_GRANT',?,?)`, parent.Name, r.schema).Scan(&ddl)
		if err != nil || !ddl.Valid {
			r.issue(&r.snapshot.Objects[i], fmt.Sprintf("对象授权 DDL 不可提取: %v", err))
			continue
		}
		units, err := splitSimpleDDL(ddl.String)
		if err != nil {
			r.issue(&r.snapshot.Objects[i], "对象授权 DDL 不可安全分句: "+err.Error())
			continue
		}
		for _, unit := range units {
			// Some DM8 builds emit unqualified, unquoted mixed-case object names
			// here. The catalog OBJID proves the identity; bind that one reference
			// to its exact stored name before mapping the selected Schema.
			unit, err = bindNativeGrantObject(unit, r.schema, parent.Name)
			if err != nil {
				r.issue(&r.snapshot.Objects[i], err.Error())
				break
			}
			r.snapshot.Objects[i].Units = append(r.snapshot.Objects[i].Units, Unit{SQL: unit, Phase: "after_data"})
		}
	}
	return nil
}

func bindNativeGrantObject(ddl, schema, name string) (string, error) {
	tokens, err := nativeTokens(ddl)
	if err != nil {
		return "", err
	}
	for i, token := range tokens {
		if token.kind != 'i' || !strings.EqualFold(token.value, "ON") || i+1 >= len(tokens) {
			continue
		}
		start := i + 1
		if tokens[start].kind == 'i' {
			switch strings.ToUpper(tokens[start].value) {
			case "TABLE", "VIEW", "PROCEDURE", "FUNCTION", "PACKAGE", "TYPE", "CLASS", "SEQUENCE":
				start++
			}
		}
		if start >= len(tokens) {
			break
		}
		end := start
		if end+2 < len(tokens) && tokens[end+1].kind == 'p' && tokens[end+1].value == "." {
			if !strings.EqualFold(tokens[start].value, schema) {
				return "", fmt.Errorf("原生授权对象 Schema 与目录归属不一致")
			}
			end += 2
		}
		if (tokens[end].kind != 'i' && tokens[end].kind != 'q') || !strings.EqualFold(tokens[end].value, name) {
			return "", fmt.Errorf("原生授权对象名与目录标识不一致")
		}
		var prefix strings.Builder
		last, depth := 0, 0
		// Native metadata also omits quotes around mixed-case column grants.
		// Only the identifier lists before ON are column names; principals and
		// privilege keywords retain the native spelling.
		for _, column := range tokens[:i] {
			if column.kind == 'p' && column.value == "(" {
				depth++
			} else if column.kind == 'p' && column.value == ")" {
				depth--
			} else if depth > 0 && (column.kind == 'i' || column.kind == 'q') {
				prefix.WriteString(ddl[last:column.start])
				prefix.WriteString(Quote(column.value))
				last = column.end
			}
		}
		prefix.WriteString(ddl[last:tokens[start].start])
		return prefix.String() + Qualified(schema, name) + ddl[tokens[end].end:], nil
	}
	return "", fmt.Errorf("原生授权缺少可确认的对象引用")
}
func (r *catalogReader) namedObject(name string) *Object {
	for i := range r.snapshot.Objects {
		o := &r.snapshot.Objects[i]
		if o.Name == name && (o.Kind == "table" || o.Kind == "table_partition" || o.Kind == "view" || o.Kind == "materialized_view" || o.Kind == "sequence") {
			return o
		}
	}
	return nil
}
func splitSimpleDDL(ddl string) ([]string, error) {
	tokens, err := nativeTokens(ddl)
	if err != nil {
		return nil, err
	}
	var units []string
	start, depth := 0, 0
	for i, t := range tokens {
		if t.kind == 'p' && t.value == "(" {
			depth++
		} else if t.kind == 'p' && t.value == ")" {
			depth--
		}
		// DM GET_DEPENDENT_DDL can join independent grants with CRLF and no
		// semicolon. GRANT OPTION belongs to one statement; other top-level
		// GRANT heads begin the next (never split procedural SQL here).
		if depth == 0 && t.kind == 'i' && strings.EqualFold(t.value, "GRANT") && i > 0 && (i+1 >= len(tokens) || !strings.EqualFold(tokens[i+1].value, "OPTION")) {
			if s := strings.TrimSpace(ddl[start:t.start]); s != "" {
				units = append(units, s)
			}
			start = t.start
		}
		if depth == 0 && t.kind == 'p' && t.value == ";" {
			s := strings.TrimSpace(ddl[start:t.start])
			if s != "" {
				units = append(units, s)
			}
			start = t.end
		}
	}
	if s := strings.TrimSpace(ddl[start:]); s != "" {
		units = append(units, s)
	}
	if len(units) == 0 {
		return nil, fmt.Errorf("DDL 为空")
	}
	for _, u := range units {
		ts, e := nativeTokens(u)
		if e != nil || len(ts) == 0 || !strings.EqualFold(ts[0].value, "GRANT") {
			return nil, fmt.Errorf("只允许原生 GRANT 授权语句")
		}
	}
	return units, nil
}
