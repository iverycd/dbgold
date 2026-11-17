package source

import (
	"context"
	"errors"
	"fmt"

	"dbgold/datamigrate/failure"
)

type OscarManualObject struct {
	Kind string
	Name string
}

// OscarManualObjects inventories source objects that need manual migration.
// A failure to read one catalog does not suppress the other inventories.
func (r *OscarReader) OscarManualObjects(ctx context.Context) ([]OscarManualObject, error) {
	catalogs := []struct {
		kind, query string
	}{
		{"trigger", `SELECT DISTINCT TRIGGER_NAME FROM ALL_TRIGGERS WHERE OWNER=? ORDER BY TRIGGER_NAME`},
		{"routine", `SELECT DISTINCT p.PRONAME FROM INFO_SCHEM.V_SYS_PROC p
JOIN INFO_SCHEM.V_SYS_NAMESPACE n ON n.OID=p.PRONAMESPACE
WHERE n.NSPNAME=? ORDER BY p.PRONAME`},
		{"sequence", `SELECT c.RELNAME FROM SYS_CLASS c JOIN SYS_NAMESPACE n ON n.OID=c.RELNAMESPACE
WHERE n.NSPNAME=? AND c.RELKIND='S' ORDER BY c.RELNAME`},
	}
	var objects []OscarManualObject
	var diagnostics error
	for _, catalog := range catalogs {
		rows, err := r.query(ctx, catalog.query, r.schema)
		if err != nil {
			diagnostics = errors.Join(diagnostics, fmt.Errorf("读取神通 %s 对象目录: %w", catalog.kind, err))
			if failure.IsTaskError(err) {
				return objects, diagnostics
			}
			continue
		}
		var generated map[string]bool
		if catalog.kind == "sequence" && len(rows) > 0 {
			generated, err = r.oscarGeneratedSequences(ctx)
			if err != nil {
				diagnostics = errors.Join(diagnostics, fmt.Errorf("核验神通序列与自增列的绑定: %w", err))
			}
		}
		for _, row := range rows {
			name, err := oscarSingleText([][]any{row})
			if err != nil {
				diagnostics = errors.Join(diagnostics, fmt.Errorf("读取神通 %s 对象名: %w", catalog.kind, err))
				continue
			}
			if !generated[name] {
				objects = append(objects, OscarManualObject{Kind: catalog.kind, Name: name})
			}
		}
	}
	return objects, diagnostics
}

func (r *OscarReader) oscarGeneratedSequences(ctx context.Context) (map[string]bool, error) {
	tables, err := r.ListTables(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool)
	var diagnostics error
	for _, table := range tables {
		cols, err := r.columns(ctx, table)
		if err != nil {
			diagnostics = errors.Join(diagnostics, err)
			if failure.IsTaskError(err) {
				return out, diagnostics
			}
			continue
		}
		for _, col := range cols {
			if col.Default == nil {
				continue
			}
			owner, name := oscarSequenceDefault(*col.Default)
			if name == "" || owner != "" && owner != r.schema {
				continue
			}
			seq, related, err := r.oscarAutoIncrement(ctx, table, col)
			if err != nil {
				return out, errors.Join(diagnostics, err)
			}
			if related && seq.Unsupported == "" {
				out[name] = true
			}
		}
	}
	return out, diagnostics
}
