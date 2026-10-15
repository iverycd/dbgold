// Package pgschema provides supplementary exclusions for PostgreSQL-compatible
// database schema dropdowns.
package pgschema

import "strings"

// IsHidden reports whether name should be excluded from schema dropdowns in
// addition to each caller's existing filters. Matching is case-sensitive and
// does not change the original schema name.
func IsHidden(name string) bool {
	switch name {
	case "sysaudit", "src_restrict", "sysmac", "anon", "sys_catalog",
		"dbms_sql", "xlog_record_read", "dbms_job", "dbms_scheduler", "dbms_session",
		"dbms_application_info", "dbms_profiler_proctable", "dbms_lock", "dbms_lob", "dbms_rowid", "dbms_xplan",
		"performance_schema", "xmltype", "audit":
		return true
	default:
		return strings.HasPrefix(name, "pg_toast_temp_")
	}
}
