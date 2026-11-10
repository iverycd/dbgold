package datamigrate

import (
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/valueconv"
)

type mysqlPGIndexCatalog struct {
	indexes []source.IndexInfo
	err     error
}

func (m *Migrator) readMySQLPGIndexes(ctx context.Context) ([]source.IndexInfo, error) {
	if m.mysqlPGIndexCatalog == nil {
		read := m.reader.GetIndexes
		if r, ok := m.reader.(interface {
			GetIndexesForPG(context.Context) ([]source.IndexInfo, error)
		}); ok && m.cfg.MigrationPurpose != "cdc" {
			read = r.GetIndexesForPG
		}
		indexes, err := read(ctx)
		m.mysqlPGIndexCatalog = &mysqlPGIndexCatalog{indexes: indexes, err: err}
	}
	catalog := m.mysqlPGIndexCatalog
	if catalog.err != nil {
		return nil, catalog.err
	}
	return m.nameMySQLPGIndexes(catalog.indexes), nil
}

func (m *Migrator) targetPostIndex(ctx context.Context, idx source.IndexInfo) source.IndexInfo {
	idx = m.planMySQLPGSpatialIndex(ctx, idx)
	idx = m.planMySQLPGUniqueConstraint(ctx, idx)
	table := idx.TableName
	idx.TableName = m.objName(table)
	idx.IndexName = m.objName(idx.IndexName)
	idx.Columns = slices.Clone(idx.Columns)
	for i, col := range idx.Columns {
		idx.Columns[i] = m.targetColumn(table, col)
	}
	return idx
}

// Plan against the whole source index catalog, before table selection. An
// indexes-only retry for one table must choose the same name as a full run.
func (m *Migrator) nameMySQLPGIndexes(indexes []source.IndexInfo) []source.IndexInfo {
	counts := map[string]int{}
	for _, idx := range indexes {
		counts[m.objName(idx.IndexName)]++
	}
	out := slices.Clone(indexes)
	for i, idx := range out {
		name := m.objName(idx.IndexName)
		if counts[name] == 1 && len(name) <= 63 {
			continue
		}
		table := m.objName(idx.TableName)
		base := table + "_" + name
		// Reserve original names too. Do not allow server-side identifier
		// truncation (including truncation in the middle of a UTF-8 rune).
		candidate := base
		for salt := 0; len(candidate) > 63 || counts[candidate] > 0; salt++ {
			digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d", table, name, salt)))
			head := base[:min(len(base), 50)]
			for !utf8.ValidString(head) {
				head = head[:len(head)-1]
			}
			candidate = fmt.Sprintf("%s_%x", head, digest[:6])
		}
		out[i].TargetIndexName = candidate
		counts[candidate] = 1
	}
	return out
}

// Record only when processing the actual object, never while preparing its
// repair manifest or checking partition constraints.
func (m *Migrator) recordMySQLPrefixWarning(report *MigrationReport, idx source.IndexInfo) {
	if m.writer.DBType() == "mysql" || idx.Unsupported != "" || len(idx.MySQLPrefixLengths) == 0 {
		return
	}
	var parts []string
	for i, column := range idx.Columns {
		if i < len(idx.MySQLPrefixLengths) && idx.MySQLPrefixLengths[i] > 0 {
			parts = append(parts, fmt.Sprintf("%s(%d)", column, idx.MySQLPrefixLengths[i]))
		}
	}
	reason := fmt.Sprintf("索引 %s 的前缀列 %s 按整列创建", idx.IndexName, strings.Join(parts, ", "))
	if idx.IsUnique {
		reason += "，唯一性按完整键值判断"
	}
	report.Warnings = append(report.Warnings, valueconv.MigrationWarning{
		Table: idx.TableName, Column: strings.Join(idx.Columns, ", "), Reason: reason, Count: 1,
	})
	m.log.Warnf("表 %s: %s", idx.TableName, reason)
}
