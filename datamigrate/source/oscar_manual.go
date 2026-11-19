package source

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"dbgold/datamigrate/failure"
	"dbgold/internal/jdbcbridge"
)

type OscarManualObject struct {
	Kind       string
	Name       string
	Definition string
}

// OscarManualObjects inventories source objects that need manual migration.
// A failure to read one catalog does not suppress the other inventories.
func (r *OscarReader) OscarManualObjects(ctx context.Context) ([]OscarManualObject, error) {
	return r.oscarManualObjects(ctx, r.oscarGeneratedSequences)
}

func (r *OscarReader) oscarManualObjects(ctx context.Context, generatedSequences func(context.Context) (map[string]bool, error)) ([]OscarManualObject, error) {
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
			generated, err = generatedSequences(ctx)
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
	return r.boundOscarSequences(ctx, func(ctx context.Context, table string, col jdbcbridge.MigrationColumn) (bool, error) {
		seq, related, err := r.oscarAutoIncrement(ctx, table, col)
		return related && seq.Unsupported == "", err
	})
}

func (r *OscarDamengReader) OscarManualObjects(ctx context.Context) ([]OscarManualObject, error) {
	objects, diagnostics := r.oscarManualObjects(ctx, func(ctx context.Context) (map[string]bool, error) {
		return r.boundOscarSequences(ctx, func(ctx context.Context, table string, col jdbcbridge.MigrationColumn) (bool, error) {
			identity, err := r.damengIdentity(ctx, table, col)
			if failure.IsTaskError(err) {
				return false, err
			}
			return identity != nil, nil
		})
	})
	if failure.IsTaskError(diagnostics) {
		return objects, diagnostics
	}
	for i := range objects {
		definition, err := r.oscarManualDefinition(ctx, objects[i])
		if failure.IsTaskError(err) {
			return objects, errors.Join(diagnostics, err)
		}
		// Some Oscar versions or account grants expose only the object inventory.
		// Keep the catalog identity even when its optional source text is unavailable.
		objects[i].Definition = definition
	}
	return objects, diagnostics
}

func (r *OscarReader) oscarManualDefinition(ctx context.Context, object OscarManualObject) (string, error) {
	if object.Kind == "sequence" {
		rows, err := r.query(ctx, `SELECT SYS_GET_SEQUENCEDEF(c.OID) FROM SYS_CLASS c
JOIN SYS_NAMESPACE n ON n.OID=c.RELNAMESPACE WHERE n.NSPNAME=? AND c.RELNAME=? AND c.RELKIND='S'`, r.schema, object.Name)
		if err != nil {
			return "", err
		}
		return oscarSingleText(rows)
	}
	query := `SELECT TYPE,LINE,TEXT FROM ALL_SOURCE WHERE OWNER=? AND NAME=? AND TYPE IN ('FUNCTION','PROCEDURE','PACKAGE','PACKAGE BODY') ORDER BY TYPE,LINE`
	if object.Kind == "trigger" {
		query = `SELECT TYPE,LINE,TEXT FROM ALL_SOURCE WHERE OWNER=? AND NAME=? AND TYPE='TRIGGER' ORDER BY TYPE,LINE`
	}
	rows, err := r.query(ctx, query, r.schema, object.Name)
	if err != nil {
		return "", err
	}
	var definition strings.Builder
	var kind string
	var line int64
	for _, row := range rows {
		if len(row) != 3 {
			return "", fmt.Errorf("神通对象 %s 的源定义列数不完整", object.Name)
		}
		currKind, kindErr := oscarText(row[0])
		currLine, lineErr := oscarInteger(row[1])
		fragment, textErr := oscarText(row[2])
		if kindErr != nil || lineErr != nil || textErr != nil {
			return "", fmt.Errorf("神通对象 %s 的源定义片段无效", object.Name)
		}
		if currKind != kind {
			if definition.Len() > 0 {
				definition.WriteByte('\n')
			}
			kind, line = currKind, 0
		}
		if currLine != line+1 {
			return "", fmt.Errorf("神通对象 %s 的源定义行号缺失或重复", object.Name)
		}
		definition.WriteString(fragment)
		if !strings.HasSuffix(fragment, "\n") {
			definition.WriteByte('\n')
		}
		line = currLine
	}
	return definition.String(), nil
}

func (r *OscarReader) boundOscarSequences(ctx context.Context, bound func(context.Context, string, jdbcbridge.MigrationColumn) (bool, error)) (map[string]bool, error) {
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
			related, err := bound(ctx, table, col)
			if err != nil {
				return out, errors.Join(diagnostics, err)
			}
			if related {
				out[name] = true
			}
		}
	}
	return out, diagnostics
}
