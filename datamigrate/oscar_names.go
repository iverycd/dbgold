package datamigrate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
)

func (m *Migrator) isOscar() bool {
	return m.cfg.TargetDBType == "oscar"
}

func (m *Migrator) readViews(ctx context.Context) ([]source.ViewInfo, error) {
	if m.isOscar() || m.isOracleDameng() {
		if reader, ok := m.reader.(interface {
			GetViewsForTarget(context.Context, string) ([]source.ViewInfo, error)
		}); ok {
			return reader.GetViewsForTarget(ctx, m.writer.DBType())
		}
	}
	return m.reader.GetViews(ctx)
}

// Oscar relations share a schema namespace. Check the complete set before any
// DROP TABLE / DROP SEQUENCE, including generated names (not only source names).
// Do not truncate long names here: the server must report its actual limit.
type oscarNames map[string]string

type oscarNameConflict struct{ target, previous, origin string }

func (e *oscarNameConflict) Error() string {
	return fmt.Sprintf("Oscar 对象名冲突: %s 与 %s 均映射为 %q", e.previous, e.origin, e.target)
}

func (n oscarNames) add(target, origin string) error {
	if previous, exists := n[target]; exists {
		return &oscarNameConflict{target: target, previous: previous, origin: origin}
	}
	n[target] = origin
	return nil
}

func recordOscarPreflightError(report *MigrationReport, err error) {
	category, name := &report.Tables, "Oscar 对象命名预检"
	var conflict *oscarNameConflict
	if errors.As(err, &conflict) {
		name = conflict.target
		switch {
		case strings.HasPrefix(conflict.origin, "view "):
			category = &report.Views
		case strings.HasPrefix(conflict.origin, "index "):
			category = &report.Indexes
		case strings.HasPrefix(conflict.origin, "primary key "):
			category = &report.PrimaryKeys
		case strings.HasPrefix(conflict.origin, "sequence "):
			category = &report.Sequences
		case strings.HasPrefix(conflict.origin, "foreign key "):
			category = &report.Constraints
		}
	}
	category.Total = max(category.Total, 1)
	category.Failed++
	category.Items = append(category.Items, ObjectResult{Name: name, Error: err.Error()})
}

func (m *Migrator) preflightOscarNames(ctx context.Context, tables, allTables []string) error {
	relations := make(oscarNames)
	selected := make(map[string]bool, len(tables))
	origins := map[string]ObjectRef{}
	block := func(ref ObjectRef, err error) {
		if ref.Kind == "table" {
			m.blockMigrationTable(ref.Name, "preflight", err)
		} else if ref.Table != "" {
			m.blockMigrationTable(ref.Table, "preflight", err)
		} else {
			m.blockMigrationObject(ref, err)
		}
	}
	register := func(name, origin string, ref ObjectRef) {
		origins[origin] = ref
		if err := relations.add(name, origin); err != nil {
			block(ref, err)
			var conflict *oscarNameConflict
			if errors.As(err, &conflict) {
				block(origins[conflict.previous], err)
			}
		}
	}
	for _, table := range tables {
		selected[table] = true
		register(m.objName(table), "table "+table, tableRef(table))
	}
	for _, table := range tables {
		if err := ctx.Err(); err != nil {
			return err
		}
		if m.cfg.objectMode() {
			continue
		}
		info, err := m.sourceTableDDLInfo(ctx, table)
		if err != nil {
			if m.stopMigrationTask(ctx, err) {
				return err
			}
			m.blockMigrationTable(table, "preflight", fmt.Errorf("检查表 %s 的列名: %w", table, err))
			continue
		}
		if info == nil {
			m.blockMigrationTable(table, "preflight", fmt.Errorf("检查表 %s 的列名: 缺少表元数据", table))
			continue
		}
		columns := make(oscarNames)
		for _, col := range info.Columns {
			if err := columns.add(m.objName(col.Name), table+"."+col.Name); err != nil {
				m.blockMigrationTable(table, "preflight", err)
			}
		}
	}
	if m.cfg.Content == "data_only" && !m.cfg.objectMode() {
		return nil
	}
	want := func(kind string) bool { return !m.cfg.objectMode() || slices.Contains(m.cfg.Objects, kind) }
	d := m.writer.Dialect()
	// Fail closed if metadata cannot be inspected: skipping a preflight read
	// could allow CREATE OR REPLACE to overwrite a case-colliding object.
	if want("primary_keys") {
		pks, err := m.reader.GetPrimaryKeys(ctx)
		if err != nil {
			return fmt.Errorf("Oscar 主键命名预检无法读取源元数据: %w", err)
		}
		for _, pk := range pks {
			if !selected[pk.TableName] {
				continue
			}
			owner := pk.TableName
			origin := "primary key " + pk.TableName + "." + pk.IndexName
			pk.TableName, pk.IndexName = m.objName(pk.TableName), m.objName(pk.IndexName)
			pk.IsPrimary = true
			register(dialect.IndexName(d, pk), origin, ObjectRef{Kind: "primary_key", Table: owner, Name: pk.IndexName})
		}
	}
	if want("indexes") {
		indexes, err := m.reader.GetIndexes(ctx)
		if err != nil {
			return fmt.Errorf("Oscar 索引命名预检无法读取源元数据: %w", err)
		}
		for _, idx := range indexes {
			if !selected[idx.TableName] {
				continue
			}
			owner := idx.TableName
			origin := "index " + idx.TableName + "." + idx.IndexName
			idx.TableName, idx.IndexName = m.objName(idx.TableName), m.objName(idx.IndexName)
			register(dialect.IndexName(d, idx), origin, ObjectRef{Kind: "index", Table: owner, Name: idx.IndexName})
		}
	}
	if want("sequences") {
		seqs, err := m.reader.GetSequences(ctx)
		if err != nil {
			return fmt.Errorf("Oscar 序列命名预检无法读取源元数据: %w", err)
		}
		for _, seq := range seqs {
			if !selected[seq.TableName] {
				continue
			}
			owner := seq.TableName
			origin := "sequence " + seq.TableName + "." + seq.ColumnName
			seq.TableName, seq.ColumnName = m.objName(seq.TableName), m.objName(seq.ColumnName)
			register(dialect.SequenceName(d, seq), origin, ObjectRef{Kind: "sequence", Table: owner, Name: seq.ColumnName})
		}
	}
	if want("foreign_keys") && (m.cfg.objectMode() || m.cfg.Mode != "include") {
		excluded := make(map[string]bool)
		if m.cfg.Mode == "exclude" {
			for _, t := range allTables {
				excluded[t] = !selected[t]
			}
		}
		constraints := make(map[string]oscarNames)
		fks, err := m.reader.GetForeignKeys(ctx)
		if err != nil {
			return fmt.Errorf("Oscar 外键命名预检无法读取源元数据: %w", err)
		}
		for _, fk := range fks {
			if excluded[fk.TableName] || excluded[fk.RefTable] {
				continue
			}
			table := m.objName(fk.TableName)
			if constraints[table] == nil {
				constraints[table] = make(oscarNames)
			}
			if err := constraints[table].add(m.objName(fk.ConstraintName), "foreign key "+fk.TableName+"."+fk.ConstraintName); err != nil {
				m.blockMigrationTable(fk.TableName, "preflight", err)
			}
		}
	}
	if !m.cfg.objectMode() && m.cfg.Mode != "include" {
		views, err := m.readViews(ctx)
		if err != nil {
			return fmt.Errorf("Oscar 视图命名预检无法读取源元数据: %w", err)
		}
		for _, view := range views {
			register(m.objName(view.ViewName), "view "+view.ViewName, ObjectRef{Kind: "view", Name: view.ViewName})
		}
	}
	return ctx.Err()
}
