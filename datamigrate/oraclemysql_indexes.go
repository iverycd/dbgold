package datamigrate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/datamigrate/valueconv"
	mysql "github.com/go-sql-driver/mysql"
)

// Index planning is independent of table replacement: object-only jobs reuse
// existing tables and must not enable the replacement/FK cleanup workflow.
type oracleMySQLIndexTarget interface {
	InspectMySQLIndexProfile(context.Context) (target.MySQLIndexProfile, error)
	InspectMySQLIndexTable(context.Context, string) (target.MySQLIndexTable, error)
}

type oracleMySQLIndexIdentity struct{ table, name string }
type oracleMySQLIndexTableResult struct {
	table target.MySQLIndexTable
	err   error
}
type oracleMySQLIndexRunState struct {
	loaded       bool
	indexes      []source.IndexInfo
	err          error
	profileRead  bool
	profile      target.MySQLIndexProfile
	profileErr   error
	versionRead  bool
	version      dialect.MySQLVersion
	versionError error
	tables       map[string]oracleMySQLIndexTableResult
	warnings     map[oracleMySQLIndexIdentity]string
	reported     map[oracleMySQLIndexIdentity]bool
}

func cloneOracleMySQLIndexes(indexes []source.IndexInfo) []source.IndexInfo {
	copy := slices.Clone(indexes)
	for i := range copy {
		copy[i].Columns = slices.Clone(copy[i].Columns)
		copy[i].PrefixLengths = slices.Clone(copy[i].PrefixLengths)
		copy[i].Descending = slices.Clone(copy[i].Descending)
		copy[i].Attnums = slices.Clone(copy[i].Attnums)
	}
	return copy
}

func (m *Migrator) oracleMySQLIndexSelected(index source.IndexInfo) bool {
	if len(m.cfg.ViewNames) > 0 || m.cfg.Content == "data_only" || m.cfg.objectMode() && !slices.Contains(m.cfg.Objects, "indexes") {
		return false
	}
	if m.cfg.objectMode() && len(m.cfg.TableNames) > 0 {
		if !slices.Contains(m.cfg.TableNames, index.TableName) {
			return false
		}
	} else if len(FilterTables([]string{index.TableName}, m.cfg.Mode, m.cfg.Filter)) == 0 {
		return false
	}
	for _, excluded := range m.cfg.ExcludedObjects {
		if excluded.Kind == "table" && excluded.Name == index.TableName {
			return false
		}
		if (excluded.Kind == "index" || excluded.Kind == "indexes") &&
			(excluded.Name == index.IndexName || excluded.Name == m.objName(index.IndexName)) &&
			(excluded.Table == "" || excluded.Table == index.TableName) {
			return false
		}
	}
	return true
}

func (m *Migrator) prepareOracleMySQLIndexes(ctx context.Context, indexes []source.IndexInfo, inventoryErr error) ([]source.IndexInfo, error) {
	s := &oracleMySQLIndexRunState{
		loaded: true, err: inventoryErr,
		tables: map[string]oracleMySQLIndexTableResult{}, warnings: map[oracleMySQLIndexIdentity]string{},
		reported: map[oracleMySQLIndexIdentity]bool{},
	}
	m.oracleMySQLIndexes = s
	if inventoryErr != nil {
		return nil, inventoryErr
	}
	for _, index := range cloneOracleMySQLIndexes(indexes) {
		if m.oracleMySQLIndexSelected(index) {
			s.indexes = append(s.indexes, index)
		}
	}
	for i, index := range s.indexes {
		if index.IsPrimary || index.Unsupported != "" || index.IsUnique && !slices.Contains(index.Descending, true) {
			continue
		}
		planned, warning, err := m.prepareOracleMySQLIndex(ctx, index)
		if err != nil {
			// Keep failures local. Unrelated indexes, PKs, and tables can proceed.
			index.Unsupported = fmt.Sprintf("索引 %s 规划失败: %v", index.IndexName, err)
			s.indexes[i] = index
			continue
		}
		s.indexes[i] = planned
		if warning != "" {
			s.warnings[oracleMySQLIndexIdentity{index.TableName, index.IndexName}] = warning
		}
	}
	return cloneOracleMySQLIndexes(s.indexes), nil
}

func (m *Migrator) oracleMySQLIndexColumns(ctx context.Context, index source.IndexInfo) (target.MySQLIndexTable, error) {
	if m.oracleMySQLSchemaMode() {
		info, err := m.sourceTableDDLInfo(ctx, index.TableName)
		if err != nil {
			return target.MySQLIndexTable{}, err
		}
		plan, err := m.planOracleMySQLTable(ctx, index.TableName, info)
		if err != nil {
			return target.MySQLIndexTable{}, err
		}
		table := target.MySQLIndexTable{Engine: "InnoDB", RowFormat: "DYNAMIC"}
		for _, c := range plan.Columns {
			table.Columns = append(table.Columns, target.MySQLIndexColumn{
				Name: c.Name, DataType: c.DataType, Length: c.Length,
				Precision: c.Precision, Scale: c.Scale, DateTimePrecision: c.DateTimePrecision, MaxBytes: 4,
			})
		}
		return table, nil
	}
	s := m.oracleMySQLIndexes
	if result, exists := s.tables[index.TableName]; exists {
		return result.table, result.err
	}
	inspector, ok := m.writer.(oracleMySQLIndexTarget)
	if !ok {
		return target.MySQLIndexTable{}, fmt.Errorf("目标 Writer 缺少 MySQL 索引元数据探测能力")
	}
	table, err := inspector.InspectMySQLIndexTable(ctx, m.objName(index.TableName))
	if err == nil && strings.EqualFold(table.Engine, "InnoDB") {
		// Existing target columns use target names. Translate only the returned
		// metadata, never the source inventory used for scope filtering.
		columns := make([]target.MySQLIndexColumn, 0, len(table.Columns))
		info, sourceErr := m.sourceTableDDLInfo(ctx, index.TableName)
		if sourceErr != nil {
			err = sourceErr
		} else if info == nil {
			err = fmt.Errorf("表 %s 缺少源列元数据", index.TableName)
		} else {
			for _, original := range info.Columns {
				for _, actual := range table.Columns {
					if strings.EqualFold(actual.Name, m.targetColumn(index.TableName, original.Name)) {
						actual.Name = original.Name
						columns = append(columns, actual)
						break
					}
				}
			}
			table.Columns = columns
		}
	}
	s.tables[index.TableName] = oracleMySQLIndexTableResult{table, err}
	return table, err
}

func (m *Migrator) prepareOracleMySQLIndex(ctx context.Context, index source.IndexInfo) (source.IndexInfo, string, error) {
	table, err := m.oracleMySQLIndexColumns(ctx, index)
	if err != nil {
		return index, "", err
	}
	var warnings []string
	if slices.Contains(index.Descending, true) {
		if !strings.EqualFold(table.Engine, "InnoDB") {
			return index, "", fmt.Errorf("目标 MySQL 引擎 %s 不支持保留降序索引，请人工适配", table.Engine)
		}
		version, err := m.oracleMySQLTargetVersion(ctx)
		if err != nil {
			return index, "", fmt.Errorf("读取降序索引目标版本: %w", err)
		}
		if !version.AtLeast(8, 0, 0) {
			index.Descending = nil
			warnings = append(warnings, fmt.Sprintf("索引 %s：目标 MySQL %d.%d.%d 不支持物理降序，将按真实列创建升序索引；降序未保留，查询性能可能不同", index.IndexName, version.Major, version.Minor, version.Patch))
		}
	}
	if index.IsPrimary || index.IsUnique {
		return index, strings.Join(warnings, "；"), nil
	}
	if !strings.EqualFold(table.Engine, "InnoDB") {
		return index, "", nil
	}
	// A full key that fits even the legacy 767-byte limit needs no server
	// capability query. This also leaves unknown types to existing execution.
	_, warning, err := planOracleMySQLIndex(index, table.Columns, 767)
	if err != nil || warning == "" {
		return index, strings.Join(warnings, "；"), err
	}
	s := m.oracleMySQLIndexes
	if !s.profileRead {
		s.profileRead = true
		if inspector, ok := m.writer.(oracleMySQLIndexTarget); ok {
			s.profile, s.profileErr = inspector.InspectMySQLIndexProfile(ctx)
		} else {
			s.profileErr = fmt.Errorf("目标 Writer 缺少 MySQL 索引长度探测能力")
		}
	}
	if s.profileErr != nil {
		return index, "", s.profileErr
	}
	planned, warning, err := planOracleMySQLIndex(index, table.Columns, s.profile.KeyLimit(table.RowFormat))
	if warning != "" {
		warnings = append(warnings, warning)
	}
	return planned, strings.Join(warnings, "；"), err
}

func (m *Migrator) recordOracleMySQLIndexWarning(report *MigrationReport, index source.IndexInfo) {
	s := m.oracleMySQLIndexes
	if s == nil {
		return
	}
	id := oracleMySQLIndexIdentity{index.TableName, index.IndexName}
	if reason := s.warnings[id]; reason != "" && !s.reported[id] {
		s.reported[id] = true
		report.Warnings = append(report.Warnings, valueconv.MigrationWarning{
			Table: m.objName(index.TableName), Column: strings.Join(index.Columns, ", "), Reason: reason, Count: 1,
		})
		m.log.Warnf("表 %s: %s", index.TableName, reason)
	}
}

func (m *Migrator) describeOracleMySQLIndexError(index source.IndexInfo, err error) error {
	if err == nil || m.reader.DBType() != "oracle" || m.writer.DBType() != "mysql" || !index.IsPrimary && !index.IsUnique {
		return err
	}
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) && mysqlErr.Number == 1071 {
		return fmt.Errorf("%w；主键或唯一索引保留完整字段，不能自动改为前缀索引，以免改变唯一性约束，请人工调整目标定义", err)
	}
	return err
}
