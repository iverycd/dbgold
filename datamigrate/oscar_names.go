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
	if m.isOscar() {
		if reader, ok := m.reader.(interface {
			GetViewsForTarget(context.Context, string) ([]source.ViewInfo, error)
		}); ok {
			return reader.GetViewsForTarget(ctx, "oscar")
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
	for _, table := range tables {
		selected[table] = true
		if err := relations.add(m.objName(table), "table "+table); err != nil {
			return err
		}
	}
	for _, table := range tables {
		if err := ctx.Err(); err != nil {
			return err
		}
		if m.cfg.objectMode() {
			continue
		}
		info, err := m.reader.GetTableDDLInfo(ctx, table)
		if err != nil {
			return fmt.Errorf("检查表 %s 的列名: %w", table, err)
		}
		if info == nil {
			return fmt.Errorf("检查表 %s 的列名: 缺少表元数据", table)
		}
		columns := make(oscarNames)
		for _, col := range info.Columns {
			if err := columns.add(m.objName(col.Name), table+"."+col.Name); err != nil {
				return err
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
			origin := "primary key " + pk.TableName + "." + pk.IndexName
			pk.TableName, pk.IndexName = m.objName(pk.TableName), m.objName(pk.IndexName)
			pk.IsPrimary = true
			if err := relations.add(dialect.IndexName(d, pk), origin); err != nil {
				return err
			}
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
			origin := "index " + idx.TableName + "." + idx.IndexName
			idx.TableName, idx.IndexName = m.objName(idx.TableName), m.objName(idx.IndexName)
			if err := relations.add(dialect.IndexName(d, idx), origin); err != nil {
				return err
			}
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
			origin := "sequence " + seq.TableName + "." + seq.ColumnName
			seq.TableName, seq.ColumnName = m.objName(seq.TableName), m.objName(seq.ColumnName)
			if err := relations.add(dialect.SequenceName(d, seq), origin); err != nil {
				return err
			}
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
				return err
			}
		}
	}
	if !m.cfg.objectMode() && m.cfg.Mode != "include" {
		views, err := m.readViews(ctx)
		if err != nil {
			return fmt.Errorf("Oscar 视图命名预检无法读取源元数据: %w", err)
		}
		for _, view := range views {
			if err := relations.add(m.objName(view.ViewName), "view "+view.ViewName); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}
