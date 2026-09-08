// Package pgroutines reads native definitions without rewriting identifiers or
// routine bodies. Callers keep this optional capability outside source.Reader.
package pgroutines

import (
	"database/sql"
	"dbgold/schema"
	"fmt"
	"strings"
)

func Extract(db *sql.DB, namespace string, triggers bool) ([]schema.Routine, error) {
	if db == nil {
		return nil, fmt.Errorf("not connected")
	}
	if namespace == "" {
		namespace = "public"
	}
	var query string
	if triggers {
		query = `SELECT t.tgname,'TRIGGER',pg_get_triggerdef(t.oid) FROM pg_catalog.pg_trigger t JOIN pg_catalog.pg_class c ON c.oid=t.tgrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND NOT t.tgisinternal ORDER BY c.relname,t.tgname`
	} else {
		var modern bool
		if e := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_class c ON c.oid=a.attrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='pg_catalog' AND c.relname='pg_proc' AND a.attname='prokind')`).Scan(&modern); e != nil {
			return nil, e
		}
		kind := `'FUNCTION'`
		filter := `NOT p.proisagg`
		if modern {
			kind = `CASE WHEN p.prokind='p' THEN 'PROCEDURE' ELSE 'FUNCTION' END`
			filter = `p.prokind IN ('f','p')`
		}
		query = `SELECT p.proname||'('||oidvectortypes(p.proargtypes)||')',` + kind + `,pg_get_functiondef(p.oid) FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname=$1 AND ` + filter + ` ORDER BY p.proname,p.oid`
	}
	rows, e := db.Query(query, namespace)
	if e != nil {
		return nil, fmt.Errorf("源库不支持完整原始定义提取或权限不足: %w", e)
	}
	defer rows.Close()
	var out []schema.Routine
	for rows.Next() {
		var r schema.Routine
		var body sql.NullString
		if e = rows.Scan(&r.Name, &r.Type, &body); e != nil {
			return nil, e
		}
		if !body.Valid || !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(body.String)), "CREATE ") {
			return nil, fmt.Errorf("对象 %s 缺少完整 CREATE 定义，停止导出", r.Name)
		}
		r.Body = body.String
		if !strings.HasSuffix(strings.TrimSpace(r.Body), ";") {
			r.Body += ";"
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
