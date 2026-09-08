package target

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"dbgold/datamigrate/source"
)

type OracleObject struct {
	Kind, Name, Table string
	Columns           []string
}

// PreflightOracle only observes target state. A same-name sequence is reusable
// only when the corresponding trigger proves its table/column provenance.
func (w *OracleWriter) PreflightOracle(ctx context.Context, tables map[string]*source.TableDDLInfo, objects []OracleObject, sequences []source.SequenceInfo, rebuild, dataOnly bool) error {
	validateName := func(name string) error {
		if w.codec != nil {
			c := w.codec.Clone()
			if c.Decode(c.Encode(name)) != name || len(c.Encode(name)) > 30 {
				return fmt.Errorf("对象名 %s 超出 Oracle 目标字符集限制", name)
			}
		}
		return nil
	}
	for name := range tables {
		if err := validateName(name); err != nil {
			return err
		}
		for _, column := range tables[name].Columns {
			if err := validateName(strings.ToUpper(column.Name)); err != nil {
				return fmt.Errorf("表 %s: %w", name, err)
			}
			if rebuild && column.Default != nil {
				if err := w.validateSQLText(*column.Default); err != nil {
					return fmt.Errorf("表 %s 列 %s 默认值: %w", name, column.Name, err)
				}
			}
		}
		if dataOnly {
			cols, err := w.loadColumns(ctx, name)
			if err != nil {
				return err
			}
			for _, c := range tables[name].Columns {
				if _, ok := cols[strings.ToUpper(c.Name)]; !ok {
					return fmt.Errorf("仅数据迁移: 目标表 %s 缺少列 %s", name, strings.ToUpper(c.Name))
				}
			}
		}
	}
	for _, o := range objects {
		if err := validateName(o.Name); err != nil {
			return err
		}
		for _, column := range o.Columns {
			if err := validateName(column); err != nil {
				return fmt.Errorf("对象 %s: %w", o.Name, err)
			}
		}
	}
	if len(objects) == 0 {
		return nil
	}
	// Prevent a DROP-only account from destroying tables before CREATE fails.
	rows, err := w.db.QueryContext(ctx, "SELECT PRIVILEGE FROM SESSION_PRIVS")
	if err != nil {
		return err
	}
	grants := map[string]bool{}
	for rows.Next() {
		var p string
		if err = rows.Scan(&p); err != nil {
			rows.Close()
			return err
		}
		grants[p] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, o := range objects {
		priv := ""
		switch o.Kind {
		case "TABLE", "VIEW", "SEQUENCE", "TRIGGER":
			priv = "CREATE " + o.Kind
		case "INDEX":
			if w.schema != w.username {
				priv = "CREATE INDEX"
			}
		}
		if priv != "" {
			anyPriv := strings.Replace(priv, "CREATE ", "CREATE ANY ", 1)
			if !grants[anyPriv] && (w.schema != w.username || !grants[priv]) {
				return fmt.Errorf("Oracle Schema %s 缺少 %s 权限", w.schema, priv)
			}
		}
	}
	for _, s := range sequences {
		if err = w.inspectSequence(ctx, s); err != nil {
			return err
		}
	}
	for _, o := range objects {
		if o.Kind == "SEQUENCE" || o.Kind == "TRIGGER" {
			continue
		}
		if o.Kind == "CONSTRAINT" {
			var table string
			err = w.db.QueryRowContext(ctx, `SELECT TABLE_NAME FROM ALL_CONSTRAINTS WHERE OWNER=:1 AND CONSTRAINT_NAME=:2`, w.schema, o.Name).Scan(&table)
			if err == nil && !(rebuild && table == o.Table) {
				return fmt.Errorf("Oracle 约束名 %s 已存在于表 %s", o.Name, table)
			}
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			continue
		}
		if o.Kind == "INDEX" {
			var owner, table string
			err = w.db.QueryRowContext(ctx, `SELECT TABLE_OWNER,TABLE_NAME FROM ALL_INDEXES WHERE OWNER=:1 AND INDEX_NAME=:2`, w.schema, o.Name).Scan(&owner, &table)
			if err == nil && !(rebuild && owner == w.schema && table == o.Table) {
				return fmt.Errorf("Oracle 索引名 %s 已存在于 %s.%s", o.Name, owner, table)
			}
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			continue
		}
		var kind string
		err = w.db.QueryRowContext(ctx, `SELECT OBJECT_TYPE FROM ALL_OBJECTS WHERE OWNER=:1 AND OBJECT_NAME=:2 AND OBJECT_TYPE IN ('TABLE','VIEW','SEQUENCE','SYNONYM','MATERIALIZED VIEW','PACKAGE','PROCEDURE','FUNCTION','TYPE') AND ROWNUM=1`, w.schema, o.Name).Scan(&kind)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == nil && (kind != o.Kind || o.Kind == "TABLE" && !rebuild) {
			return fmt.Errorf("Oracle 名称 %s 已被 %s 占用", o.Name, kind)
		}
		if o.Kind == "TABLE" && err == nil && rebuild {
			rows, e := w.db.QueryContext(ctx, `SELECT c.OWNER,c.TABLE_NAME,c.CONSTRAINT_NAME FROM ALL_CONSTRAINTS c JOIN ALL_CONSTRAINTS p ON c.R_OWNER=p.OWNER AND c.R_CONSTRAINT_NAME=p.CONSTRAINT_NAME WHERE p.OWNER=:1 AND p.TABLE_NAME=:2 AND c.CONSTRAINT_TYPE='R'`, w.schema, o.Name)
			if e != nil {
				return e
			}
			for rows.Next() {
				var owner, table, name string
				if e = rows.Scan(&owner, &table, &name); e != nil {
					rows.Close()
					return e
				}
				if _, ok := tables[table]; !ok || owner != w.schema {
					rows.Close()
					return fmt.Errorf("重建 %s 会删除迁移范围外的外键 %s.%s，请先处理依赖", o.Name, owner, name)
				}
			}
			e = rows.Err()
			rows.Close()
			if e != nil {
				return e
			}
		}
	}
	return nil
}

func (w *OracleWriter) inspectSequence(ctx context.Context, s source.SequenceInfo) error {
	seq, trig := w.dia.SequenceName(s), w.dia.TriggerName(s)
	var seqKind string
	err := w.db.QueryRowContext(ctx, `SELECT OBJECT_TYPE FROM ALL_OBJECTS WHERE OWNER=:1 AND OBJECT_NAME=:2 AND OBJECT_TYPE IN ('TABLE','VIEW','SEQUENCE','SYNONYM','MATERIALIZED VIEW','PACKAGE','PROCEDURE','FUNCTION','TYPE') AND ROWNUM=1`, w.schema, seq).Scan(&seqKind)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	seqExists := err == nil
	if seqExists && seqKind != "SEQUENCE" {
		return fmt.Errorf("序列名 %s 被 %s 占用", seq, seqKind)
	}
	var tableOwner, tableName, event, triggerType string
	err = w.db.QueryRowContext(ctx, `SELECT TABLE_OWNER,TABLE_NAME,TRIGGERING_EVENT,TRIGGER_TYPE FROM ALL_TRIGGERS WHERE OWNER=:1 AND TRIGGER_NAME=:2`, w.schema, trig).Scan(&tableOwner, &tableName, &event, &triggerType)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	triggerExists := err == nil
	if !seqExists && !triggerExists {
		return nil
	}
	if !triggerExists || tableOwner != w.schema || tableName != s.TableName || event != "INSERT" || triggerType != "BEFORE EACH ROW" {
		return fmt.Errorf("无法确认同名自增对象 %s/%s 归属当前表，拒绝覆盖", seq, trig)
	}
	rows, err := w.db.QueryContext(ctx, `SELECT TEXT FROM ALL_SOURCE WHERE OWNER=:1 AND NAME=:2 AND TYPE='TRIGGER' ORDER BY LINE`, w.schema, trig)
	if err != nil {
		return err
	}
	defer rows.Close()
	var definition strings.Builder
	for rows.Next() {
		var line string
		if err = rows.Scan(&line); err != nil {
			return err
		}
		definition.WriteString(line)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	marker := "/* " + strings.ReplaceAll(w.dia.SequenceMarker(w.schema, s), "*/", "* /") + " */"
	if !strings.Contains(definition.String(), marker) || !strings.Contains(definition.String(), w.dia.QualifyTable(w.schema, seq)+".NEXTVAL") {
		return fmt.Errorf("自增触发器 %s 缺少工具归属标记，拒绝覆盖", trig)
	}
	w.dia.OwnedSequences[seq] = seqExists
	return nil
}
