package datamigrate

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
)

func (m *Migrator) isMySQLGaussDB() bool {
	return m.reader.DBType() == "mysql" && m.writer.DBType() == "gaussdb" && m.cfg.MigrationPurpose != "cdc"
}

func (m *Migrator) gaussDBPrimaryKeys(ctx context.Context) ([]source.IndexInfo, error) {
	if !m.gaussKeysRead {
		m.gaussKeys, m.gaussKeysErr = m.reader.GetPrimaryKeys(ctx)
		m.gaussKeysRead = true
	}
	return m.gaussKeys, m.gaussKeysErr
}

func (m *Migrator) gaussDBTableOptions(ctx context.Context, info *source.TableDDLInfo, opt dialect.TypeOpt) (dialect.TypeOpt, error) {
	if !m.isMySQLGaussDB() || !m.cfg.Distributed {
		return opt, nil
	}
	keys, err := m.gaussDBPrimaryKeys(ctx)
	if err != nil {
		return opt, fmt.Errorf("规划分布键时读取主键失败: %w", err)
	}
	indexes, err := m.sourceIndexes(ctx)
	if err != nil {
		return opt, fmt.Errorf("规划分布键时读取唯一索引失败: %w", err)
	}
	indexes = slices.Clone(indexes)
	slices.SortStableFunc(indexes, func(a, b source.IndexInfo) int { return strings.Compare(a.IndexName, b.IndexName) })
	var candidates []string
	for _, key := range keys {
		if key.TableName == info.TableName {
			candidates = append(candidates, key.Columns...)
		}
	}
	for _, key := range indexes {
		if key.TableName == info.TableName && key.IsUnique && key.Unsupported == "" && len(key.MySQLPrefixLengths) == 0 && key.MySQLExpression == "" {
			candidates = append(candidates, key.Columns...)
		}
	}
	columns := map[string]source.ColumnInfo{}
	for _, col := range info.Columns {
		columns[col.Name] = col
		candidates = append(candidates, col.Name)
	}
	for _, name := range candidates {
		col, ok := columns[name]
		if ok && col.Generated == "" && target.GaussDBHashType(m.writer.Dialect().MapType(col, m.reader.DBType(), opt)) {
			opt.GaussDBDistribution = []string{m.targetColumn(info.TableName, name)}
			if m.gaussTableKeys == nil {
				m.gaussTableKeys = map[string][]string{}
			}
			m.gaussTableKeys[info.TableName] = slices.Clone(opt.GaussDBDistribution)
			return opt, nil
		}
	}
	return opt, fmt.Errorf("表 %s 没有已适配的 HASH 分布列，未执行 DROP/CREATE", info.TableName)
}

func (m *Migrator) planGaussDBIndex(ctx context.Context, idx source.IndexInfo) source.IndexInfo {
	if !m.isMySQLGaussDB() || !idx.IsPrimary && !idx.IsUnique || idx.Unsupported != "" {
		return idx
	}
	// A full migration's ledger is prepared before CREATE. Never bind its index
	// plan to a pre-existing table which will be dropped. captureRepairCall saves
	// the actual plan before issuing the post-DDL; interrupted jobs can refresh it.
	if m.gaussPreparing && !m.cfg.objectMode() && m.cfg.Content != "data_only" {
		return idx
	}
	planner, ok := m.writer.(target.GaussDBIndexPlanner)
	if !ok {
		idx.Unsupported = "目标 Writer 缺少 GaussDB 分布索引检查能力"
		return idx
	}
	planned, err := planner.PlanGaussDBIndex(ctx, idx)
	if err != nil {
		planned.Unsupported = "GaussDB 分布索引检查失败: " + err.Error()
	}
	return planned
}
