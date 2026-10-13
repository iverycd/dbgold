package datamigrate

import (
	"context"
	"sync"

	"dbgold/datamigrate/source"
)

// sourceTableDDLCache belongs to one Run and contains only original source
// metadata. Target mappings and dedicated reader snapshots remain independent.
type sourceTableDDLCache struct {
	mu     sync.Mutex
	tables map[string]*source.TableDDLInfo
}

func (m *Migrator) resetSourceTableDDLCache() {
	m.sourceDDLCache.mu.Lock()
	defer m.sourceDDLCache.mu.Unlock()
	m.sourceDDLCache.tables = make(map[string]*source.TableDDLInfo)
}

func (m *Migrator) clearSourceTableDDLCache() {
	m.sourceDDLCache.mu.Lock()
	defer m.sourceDDLCache.mu.Unlock()
	m.sourceDDLCache.tables = nil
}

// sourceTableDDLInfo shares successful source reads between repair preparation
// and ordinary table DDL generation. Both phases are serial; holding the lock
// during a read also prevents duplicate loads from concurrent callers.
func (m *Migrator) sourceTableDDLInfo(ctx context.Context, table string) (*source.TableDDLInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.sourceDDLCache.mu.Lock()
	defer m.sourceDDLCache.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if info, ok := m.sourceDDLCache.tables[table]; ok {
		return cloneSourceTableDDLInfo(info), nil
	}
	info, err := m.reader.GetTableDDLInfo(ctx, table)
	if err != nil {
		return cloneSourceTableDDLInfo(info), err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if info == nil {
		return nil, nil
	}
	if m.sourceDDLCache.tables != nil {
		m.sourceDDLCache.tables[table] = cloneSourceTableDDLInfo(info)
	}
	return cloneSourceTableDDLInfo(info), nil
}

func cloneSourceTableDDLInfo(info *source.TableDDLInfo) *source.TableDDLInfo {
	if info == nil {
		return nil
	}
	copyInfo := *info
	if info.Columns == nil {
		return &copyInfo
	}
	copyInfo.Columns = make([]source.ColumnInfo, len(info.Columns))
	copy(copyInfo.Columns, info.Columns)
	for i := range copyInfo.Columns {
		column := &copyInfo.Columns[i]
		if column.Default != nil {
			value := *column.Default
			column.Default = &value
		}
		if column.IdentityInfo != nil {
			identity := *column.IdentityInfo
			if identity.Last != nil {
				last := *identity.Last
				identity.Last = &last
			}
			column.IdentityInfo = &identity
		}
	}
	return &copyInfo
}
