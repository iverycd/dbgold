package target

import (
	"context"
	"fmt"
	"strings"

	"dbgold/datamigrate/damengnative"
)

func (w *DaMengNativeWriter) CheckDamengNativeReference(ctx context.Context, ref damengnative.Reference) error {
	var query string
	var args []any
	switch damengnative.NormalizeKind(ref.Kind) {
	case "tablespace":
		query, args = `SELECT COUNT(*) FROM DBA_TABLESPACES WHERE TABLESPACE_NAME=?`, []any{ref.Name}
	case "user", "grantee":
		query, args = `SELECT COUNT(*) FROM ALL_USERS WHERE USERNAME=?`, []any{ref.Name}
	case "role":
		query, args = `SELECT COUNT(*) FROM DBA_ROLES WHERE ROLE=?`, []any{ref.Name}
	case "schema":
		query, args = `SELECT COUNT(*) FROM SYS.SYSOBJECTS WHERE TYPE$='SCH' AND NAME=?`, []any{ref.Schema}
	default:
		if ref.Schema == "" || ref.Name == "" {
			return fmt.Errorf("外部依赖缺少完整 schema/对象名")
		}
		query, args = `SELECT COUNT(*) FROM ALL_OBJECTS WHERE OWNER=? AND OBJECT_NAME=?`, []any{ref.Schema, ref.Name}
	}
	var count int
	if err := w.db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return fmt.Errorf("核对外部依赖 %s.%s 失败: %w", ref.Schema, ref.Name, err)
	}
	if count == 0 {
		return fmt.Errorf("外部依赖 %s.%s 不存在或当前账号不可见", ref.Schema, ref.Name)
	}
	return nil
}

// This is a read-only permission check. It deliberately runs before any DROP;
// a connection with only destructive privileges must not clear the schema.
func (w *DaMengNativeWriter) ValidateDamengNativeObjects(ctx context.Context, objects []damengnative.Object) []damengnative.Issue {
	issues := []damengnative.Issue{}
	add := func(o damengnative.Object, reason string) {
		issues = append(issues, damengnative.Issue{ObjectID: o.ID, Kind: o.Kind, Name: o.Name, Severity: "error", Reason: reason})
	}
	var username string
	if err := w.db.QueryRowContext(ctx, `SELECT USER FROM DUAL`).Scan(&username); err != nil {
		return []damengnative.Issue{{Severity: "error", Reason: "无法读取目标执行用户: " + err.Error()}}
	}
	rows, err := w.db.QueryContext(ctx, `SELECT PRIVILEGE FROM SESSION_PRIVS`)
	if err != nil {
		return []damengnative.Issue{{Severity: "error", Reason: "无法确认目标创建权限，未执行目标 DDL: " + err.Error()}}
	}
	grants := map[string]bool{}
	for rows.Next() {
		var p string
		if err = rows.Scan(&p); err != nil {
			break
		}
		grants[strings.ToUpper(strings.TrimSpace(p))] = true
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return []damengnative.Issue{{Severity: "error", Reason: "读取目标创建权限失败: " + err.Error()}}
	}
	for _, o := range objects {
		if o.Managed {
			continue
		}
		if o.Materialized != nil && o.Materialized.History != nil {
			if err := w.validateHistoryRedefinition(ctx, o.Materialized.History.Views, objects); err != nil {
				add(o, err.Error())
			}
		}
		kind := damengnative.NormalizeKind(o.Kind)
		priv := ""
		switch kind {
		case "table", "view", "sequence", "trigger", "synonym", "domain", "context":
			priv = "CREATE " + strings.ToUpper(kind)
		case "materialized_view", "materialized_view_log":
			priv = "CREATE MATERIALIZED VIEW"
		case "function", "procedure", "package", "package_body", "jclass":
			priv = "CREATE PROCEDURE"
		case "type", "type_body", "class", "class_body":
			priv = "CREATE TYPE"
		case "index", "context_index":
			if username != w.schema {
				priv = "CREATE INDEX"
			}
		case "db_link":
			priv = "CREATE LINK"
		}
		if priv != "" && username != "SYSDBA" && username != "SYS" {
			anyPriv := strings.Replace(priv, "CREATE ", "CREATE ANY ", 1)
			if !grants[anyPriv] && (username != w.schema || !grants[priv]) {
				add(o, "目标账号缺少 "+priv+" 权限")
			}
		}
		for _, u := range o.Units {
			if err := validateNativeObjectUnit(w.schema, o, u); err != nil {
				add(o, err.Error())
				continue
			}
			if u.Phase == "event_create" && username != "SYSDBA" && username != "SYS" {
				scope, _, scopeErr := damengnative.EventTriggerScope(u.SQL, o.Schema)
				if scopeErr != nil {
					add(o, scopeErr.Error())
				} else if scope == "database" && !grants["CREATE ANY TRIGGER"] {
					add(o, "数据库级事件触发器需要 CREATE ANY TRIGGER 权限")
				}
				if username != w.schema && !grants["ALTER ANY TRIGGER"] {
					add(o, "跨 Schema 禁用与恢复事件触发器需要 ALTER ANY TRIGGER 权限")
				}
			}
			tokens, err := nativeSQLTokens(u.SQL)
			if err != nil {
				add(o, err.Error())
				continue
			}
			if len(tokens) > 0 && strings.EqualFold(tokens[0], "DROP") {
				for _, reason := range w.nativeExternalDropDependencies(ctx, o) {
					add(o, reason)
				}
			}
			if username != w.schema && username != "SYSDBA" && username != "SYS" && len(tokens) > 0 && (strings.EqualFold(tokens[0], "DROP") || u.Phase == "drop") {
				priv := nativeDropPrivilege(kind, tokens[0])
				if priv == "" {
					add(o, "无法证明目标账号具备跨 Schema 删除该对象的权限")
				} else if !grants[priv] {
					add(o, "目标账号缺少 "+priv+" 权限")
				}
			}
			if (kind == "grant" || kind == "object_grant") && username != w.schema && username != "SYSDBA" && username != "SYS" && !grants["GRANT ANY OBJECT PRIVILEGE"] {
				add(o, "目标账号缺少 GRANT ANY OBJECT PRIVILEGE 权限")
			}
			for _, name := range nativeTablespaces(tokens) {
				if err := w.CheckDamengNativeReference(ctx, damengnative.Reference{Kind: "tablespace", Name: name}); err != nil {
					add(o, err.Error())
				}
			}
			if kind == "grant" || kind == "object_grant" {
				for i, token := range tokens {
					if (strings.EqualFold(token, "TO") || strings.EqualFold(token, "FROM")) && i+1 < len(tokens) {
						name := nativeTokenName(tokens[i+1])
						if name == "PUBLIC" {
							continue
						}
						var count int
						if err := w.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT USERNAME AS NAME FROM ALL_USERS UNION ALL SELECT ROLE AS NAME FROM DBA_ROLES) WHERE NAME=?`, name).Scan(&count); err != nil {
							add(o, "无法核对授权主体: "+err.Error())
						} else if count == 0 {
							add(o, "目标授权主体不存在: "+name)
						}
					}
				}
			}
		}
	}
	return issues
}

func nativeDropPrivilege(kind, verb string) string {
	if strings.EqualFold(verb, "ALTER") {
		return "ALTER ANY TABLE"
	}
	switch kind {
	case "table", "view", "sequence", "trigger", "synonym", "domain", "index":
		return "DROP ANY " + strings.ToUpper(kind)
	case "context_index":
		return "DROP ANY INDEX"
	case "materialized_view", "materialized_view_log":
		return "DROP ANY MATERIALIZED VIEW"
	case "procedure", "function", "package", "package_body", "jclass":
		return "DROP ANY PROCEDURE"
	case "class", "class_body", "type", "type_body":
		return "DROP ANY TYPE"
	case "db_link":
		return "DROP ANY LINK"
	}
	return ""
}

func (w *DaMengNativeWriter) nativeExternalDropDependencies(ctx context.Context, object damengnative.Object) []string {
	var reasons []string
	if damengnative.NormalizeKind(object.Kind) == "table" {
		rows, err := w.db.QueryContext(ctx, `SELECT c.OWNER,c.TABLE_NAME,c.CONSTRAINT_NAME FROM ALL_CONSTRAINTS c JOIN ALL_CONSTRAINTS p ON c.R_OWNER=p.OWNER AND c.R_CONSTRAINT_NAME=p.CONSTRAINT_NAME WHERE p.OWNER=? AND p.TABLE_NAME=? AND c.CONSTRAINT_TYPE='R' AND c.OWNER<>? ORDER BY c.OWNER,c.TABLE_NAME,c.CONSTRAINT_NAME`, w.schema, object.Name, w.schema)
		if err != nil {
			return []string{"无法完整检查覆盖表的外部入向外键: " + err.Error()}
		}
		for rows.Next() {
			var owner, table, name string
			if err = rows.Scan(&owner, &table, &name); err != nil {
				break
			}
			reasons = append(reasons, fmt.Sprintf("外部对象 %s.%s 的外键 %s 引用待覆盖表，禁止级联删除", owner, table, name))
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
		if err != nil {
			reasons = append(reasons, "读取覆盖外键依赖失败: "+err.Error())
			return reasons
		}
	}
	rows, err := w.db.QueryContext(ctx, `SELECT DISTINCT s.NAME,dep.NAME FROM SYS.SYSDEPENDENCIES d JOIN SYS.SYSOBJECTS referenced_object ON referenced_object.ID=d.REFED_ID JOIN SYS.SYSOBJECTS rs ON rs.ID=referenced_object.SCHID JOIN SYS.SYSOBJECTS dep ON dep.ID=d.ID JOIN SYS.SYSOBJECTS s ON s.ID=dep.SCHID WHERE rs.NAME=? AND referenced_object.NAME=? AND s.NAME<>? ORDER BY s.NAME,dep.NAME`, w.schema, object.Name, w.schema)
	if err != nil {
		return append(reasons, "无法完整检查覆盖对象的外部依赖: "+err.Error())
	}
	for rows.Next() {
		var owner, name string
		if err = rows.Scan(&owner, &name); err != nil {
			break
		}
		reasons = append(reasons, fmt.Sprintf("外部对象 %s.%s 依赖待覆盖对象，禁止使其失效", owner, name))
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		reasons = append(reasons, "读取覆盖对象依赖失败: "+err.Error())
	}
	return reasons
}

func nativeTablespaces(tokens []string) []string {
	var names []string
	seen := map[string]bool{}
	add := func(token string) {
		name := nativeTokenName(token)
		if name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	for i, token := range tokens {
		if strings.EqualFold(token, "TABLESPACE") && i+1 < len(tokens) {
			add(tokens[i+1])
		}
		if !strings.EqualFold(token, "STORAGE") || i+1 >= len(tokens) || tokens[i+1] != "(" {
			continue
		}
		depth := 0
		for j := i + 1; j < len(tokens); j++ {
			if tokens[j] == "(" {
				depth++
			}
			if tokens[j] == ")" {
				depth--
				if depth == 0 {
					break
				}
			}
			if strings.EqualFold(tokens[j], "ON") && j+1 < len(tokens) {
				add(tokens[j+1])
			}
		}
	}
	return names
}
