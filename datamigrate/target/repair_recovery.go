package target

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"dbgold/datamigrate/source"
)

// InspectRecovery is a fail-closed read-only preflight. User triggers can write
// beyond this table and cannot be safely replayed as a migration side effect.
func InspectRecovery(ctx context.Context, w Writer, table string) ([]source.ColumnInfo, error) {
	if err := checkRecoveryRisks(ctx, w, table, false); err != nil {
		return nil, err
	}
	return inspectColumns(ctx, w, table)
}

func checkRecoveryRisks(ctx context.Context, w Writer, table string, truncating bool) error {
	a, err := repairAccess(w)
	if err != nil {
		return err
	}
	schema, err := ResolveRepairSchema(ctx, w)
	if err != nil {
		return err
	}
	var tableQ, triggers, fks string
	args := []any{schema, table}
	switch a.family {
	case "postgres":
		tableQ = `SELECT COUNT(*) FROM pg_class t JOIN pg_namespace n ON n.oid=t.relnamespace WHERE n.nspname=$1 AND t.relname=$2 AND t.relkind='r'`
		triggers = `SELECT COUNT(*) FROM pg_trigger tr JOIN pg_class t ON t.oid=tr.tgrelid JOIN pg_namespace n ON n.oid=t.relnamespace WHERE n.nspname=$1 AND t.relname=$2 AND NOT tr.tgisinternal AND tr.tgenabled<>'D'`
		fks = `SELECT COUNT(*) FROM pg_constraint c JOIN pg_class t ON t.oid=c.confrelid JOIN pg_namespace n ON n.oid=t.relnamespace WHERE n.nspname=$1 AND t.relname=$2 AND c.contype='f'`
	case "mysql":
		tableQ = `SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND TABLE_TYPE='BASE TABLE' AND ENGINE='InnoDB'`
		triggers = `SELECT COUNT(*) FROM information_schema.TRIGGERS WHERE EVENT_OBJECT_SCHEMA=? AND EVENT_OBJECT_TABLE=?`
		fks = `SELECT COUNT(*) FROM information_schema.KEY_COLUMN_USAGE WHERE REFERENCED_TABLE_SCHEMA=? AND REFERENCED_TABLE_NAME=?`
	case "sqlserver":
		tableQ = `SELECT COUNT(*) FROM sys.tables t JOIN sys.schemas s ON s.schema_id=t.schema_id WHERE s.name=@p1 AND t.name=@p2 AND t.is_ms_shipped=0`
		triggers = `SELECT COUNT(*) FROM sys.triggers tr JOIN sys.tables t ON t.object_id=tr.parent_id JOIN sys.schemas s ON s.schema_id=t.schema_id WHERE s.name=@p1 AND t.name=@p2 AND tr.is_disabled=0`
		fks = `SELECT COUNT(*) FROM sys.foreign_keys f JOIN sys.tables t ON t.object_id=f.referenced_object_id JOIN sys.schemas s ON s.schema_id=t.schema_id WHERE s.name=@p1 AND t.name=@p2`
	case "oracle", "dameng":
		p1, p2 := ":1", ":2"
		if a.family == "dameng" {
			p1, p2 = "?", "?"
		}
		tableQ = `SELECT COUNT(*) FROM ALL_TABLES WHERE OWNER=` + p1 + ` AND TABLE_NAME=` + p2
		triggers = `SELECT COUNT(*) FROM ALL_TRIGGERS WHERE TABLE_OWNER=` + p1 + ` AND TABLE_NAME=` + p2 + ` AND STATUS='ENABLED'`
		fks = `SELECT COUNT(*) FROM ALL_CONSTRAINTS c JOIN ALL_CONSTRAINTS p ON c.R_OWNER=p.OWNER AND c.R_CONSTRAINT_NAME=p.CONSTRAINT_NAME WHERE p.OWNER=` + p1 + ` AND p.TABLE_NAME=` + p2 + ` AND c.CONSTRAINT_TYPE='R'`
	default:
		return fmt.Errorf("目标 %s 暂不能完整检查触发器和外键，已阻止自动补迁或清空", w.DBType())
	}
	var count int
	if err = a.db.QueryRowContext(ctx, tableQ, args...).Scan(&count); err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("目标 %s.%s 不是可安全补迁的普通表", schema, table)
	}
	if err = a.db.QueryRowContext(ctx, triggers, args...).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		managed := false
		if ow, ok := w.(*OracleWriter); ok {
			managed, err = ow.onlyManagedRecoveryTriggers(ctx, table)
			if err != nil {
				return err
			}
		}
		if dw, ok := w.(*DaMengWriter); ok && dw.srcType == "oracle" {
			managed, err = dw.OnlyManagedOracleDamengTriggers(ctx, table)
			if err != nil {
				return err
			}
		}
		if !managed {
			return fmt.Errorf("目标表 %s.%s 存在启用的自定义触发器，不能自动补迁或清空", schema, table)
		}
	}
	if truncating {
		if err = a.db.QueryRowContext(ctx, fks, args...).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			return fmt.Errorf("目标表 %s.%s 存在入向外键，不能清空；不会级联删除或关闭约束", schema, table)
		}
	}
	return nil
}

// Only this tool's exact, validated identity trigger body is exempt from the
// trigger guard. A marker alone is insufficient: custom DML appended to a marked
// trigger still fails this comparison and cannot run during recovery.
func (w *OracleWriter) onlyManagedRecoveryTriggers(ctx context.Context, table string) (bool, error) {
	rows, e := w.db.QueryContext(ctx, `SELECT TRIGGER_NAME FROM ALL_TRIGGERS WHERE TABLE_OWNER=:1 AND TABLE_NAME=:2 AND STATUS='ENABLED'`, w.schema, table)
	if e != nil {
		return false, e
	}
	var names []string
	for rows.Next() {
		var name string
		if e = rows.Scan(&name); e != nil {
			rows.Close()
			return false, e
		}
		names = append(names, name)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return false, e
	}
	rows, e = w.db.QueryContext(ctx, `SELECT COLUMN_NAME FROM ALL_TAB_COLUMNS WHERE OWNER=:1 AND TABLE_NAME=:2 ORDER BY COLUMN_ID`, w.schema, table)
	if e != nil {
		return false, e
	}
	known := map[string]source.SequenceInfo{}
	for rows.Next() {
		var column string
		if e = rows.Scan(&column); e != nil {
			rows.Close()
			return false, e
		}
		s := source.SequenceInfo{TableName: table, ColumnName: column, StartValue: 1}
		known[w.dia.TriggerName(s)] = s
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return false, e
	}
	for _, name := range names {
		s, ok := known[name]
		if !ok {
			return false, nil
		}
		rows, e = w.db.QueryContext(ctx, `SELECT TEXT FROM ALL_SOURCE WHERE OWNER=:1 AND NAME=:2 AND TYPE='TRIGGER' ORDER BY LINE`, w.schema, name)
		if e != nil {
			return false, e
		}
		var text strings.Builder
		for rows.Next() {
			var line string
			if e = rows.Scan(&line); e != nil {
				rows.Close()
				return false, e
			}
			text.WriteString(line)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return false, e
		}
		definition := strings.ReplaceAll(text.String(), "\r\n", "\n")
		pos := strings.Index(definition, "BEFORE INSERT ON ")
		statements := w.dia.SequenceStatements(w.schema, s, "")
		if pos < 0 || len(statements) != 2 {
			return false, nil
		}
		want := statements[1].SQL
		want = want[strings.Index(want, "BEFORE INSERT ON "):]
		if strings.TrimSpace(definition[pos:]) != strings.TrimSpace(want) {
			return false, nil
		}
		if e = w.validateObject(ctx, name, "TRIGGER"); e != nil {
			return false, e
		}
	}
	return len(names) > 0, nil
}

// TruncateRecovery is only called after the application has persisted the
// user's explicit replace-data choice. It never uses CASCADE or disables FKs.
func TruncateRecovery(ctx context.Context, w Writer, table string) error {
	if err := checkRecoveryRisks(ctx, w, table, true); err != nil {
		return err
	}
	a, err := repairAccess(w)
	if err != nil {
		return err
	}
	schema, err := ResolveRepairSchema(ctx, w)
	if err != nil {
		return err
	}
	_, err = execObserved(ctx, a.db, "TRUNCATE TABLE "+w.Dialect().QualifyTable(schema, table))
	return err
}

// columns are the original source metadata with names mapped to target names.
// Actual target types are loaded independently; SQL Server binds using these
// actual types instead of regenerating the pre-repair CAST from source types.
func PrepareRecovery(ctx context.Context, w Writer, table string, columns []source.ColumnInfo) error {
	actual, err := InspectRecovery(ctx, w, table)
	if err != nil {
		return err
	}
	if dw, ok := w.(*DaMengWriter); ok && dw.srcType == "oracle" {
		if err := dw.ValidateOracleDamengLoadTriggers(ctx, &source.TableDDLInfo{TableName: table, Columns: columns}); err != nil {
			return err
		}
	}
	byName := map[string]source.ColumnInfo{}
	for _, c := range actual {
		byName[c.Name] = c
	}
	seen := map[string]bool{}
	for _, c := range columns {
		if seen[c.Name] {
			return fmt.Errorf("列映射重复: %s", c.Name)
		}
		seen[c.Name] = true
		a, ok := byName[c.Name]
		if !ok {
			return fmt.Errorf("目标缺少映射列 %s.%s", table, c.Name)
		}
		if a.Generated != "" {
			return fmt.Errorf("映射列 %s.%s 是生成列，不能显式复制", table, c.Name)
		}
		if c.IsNullable && !a.IsNullable {
			return fmt.Errorf("目标列 %s.%s 不允许源列中的 NULL", table, c.Name)
		}
	}
	for _, c := range actual {
		if !seen[c.Name] && !c.IsNullable && c.Default == nil && c.Generated == "" && !strings.Contains(c.Extra, "auto_increment") {
			return fmt.Errorf("新增必填目标列 %s.%s 无默认值", table, c.Name)
		}
	}
	if sw, ok := w.(*SQLServerWriter); ok {
		casts := map[string]string{}
		info := slices.Clone(columns)
		for i, c := range info {
			a := byName[c.Name]
			switch strings.ToLower(a.DataType) {
			case "tinyint", "smallint", "int", "bigint", "bit", "decimal", "numeric", "real", "float", "nvarchar", "nchar", "varbinary", "binary", "date", "datetime2", "time":
			default:
				return fmt.Errorf("SQL Server 修复列 %s 类型 %s 尚不支持安全绑定", c.Name, a.DataType)
			}
			casts[c.Name] = a.FormattedType
			info[i].IsNullable = a.IsNullable
			info[i].Extra = a.Extra
		}
		if sw.recoveryCasts == nil {
			sw.recoveryCasts = map[string]map[string]string{}
		}
		sw.recoveryCasts[table] = casts
		sw.dia.Tables[table] = &source.TableDDLInfo{TableName: table, Columns: info}
	}
	invalidateRecoveryCache(w)
	return nil
}
