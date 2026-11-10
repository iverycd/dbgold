package datamigrate

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
)

// Capability preparation belongs to one migration. In particular, an ordinary
// table must never install an extension merely because its target is PG-like.
type mysqlPGSpatialState struct {
	once         sync.Once
	readOnly     bool
	capabilities *target.PGSpatialCapabilities
	err          error
}

type mysqlPGSpatialWriter interface {
	PrepareMySQLPGSpatial(context.Context) (*target.PGSpatialCapabilities, error)
	InspectMySQLPGSpatial(context.Context) (*target.PGSpatialCapabilities, error)
	CheckMySQLPGSpatialTable(context.Context, string, *source.TableDDLInfo, *target.PGSpatialCapabilities) error
}

func (m *Migrator) isMySQLPGSpatial() bool {
	return m.reader.DBType() == "mysql" && source.IsPGFamily(m.writer.DBType())
}

func hasMySQLSpatialColumns(info *source.TableDDLInfo) bool {
	if info == nil {
		return false
	}
	for _, col := range info.Columns {
		if _, ok := source.MySQLSpatialSubtype(col.DataType); ok {
			return true
		}
	}
	return false
}

func (m *Migrator) prepareMySQLPGSpatial(ctx context.Context) (*target.PGSpatialCapabilities, error) {
	m.mysqlPGSpatial.once.Do(func() {
		w, ok := m.writer.(mysqlPGSpatialWriter)
		if !ok {
			m.mysqlPGSpatial.err = fmt.Errorf("目标 %s 未实现空间能力检查", m.writer.DBType())
			return
		}
		if m.mysqlPGSpatial.readOnly {
			m.mysqlPGSpatial.capabilities, m.mysqlPGSpatial.err = w.InspectMySQLPGSpatial(ctx)
		} else {
			m.mysqlPGSpatial.capabilities, m.mysqlPGSpatial.err = w.PrepareMySQLPGSpatial(ctx)
		}
		if m.mysqlPGSpatial.err == nil && (m.mysqlPGSpatial.capabilities == nil || m.mysqlPGSpatial.capabilities.Schema == "") {
			m.mysqlPGSpatial.err = fmt.Errorf("目标空间能力检查未返回 geometry 所属 schema")
		}
		if m.log != nil {
			if m.mysqlPGSpatial.err != nil {
				m.log.Errorf("空间扩展准备失败: %v", m.mysqlPGSpatial.err)
			} else {
				m.log.Infof("空间能力准备完成: geometry schema=%s", m.mysqlPGSpatial.capabilities.Schema)
			}
		}
	})
	return m.mysqlPGSpatial.capabilities, m.mysqlPGSpatial.err
}

func (m *Migrator) mysqlPGSpatialTypeOptions(ctx context.Context, info *source.TableDDLInfo, opt dialect.TypeOpt) (dialect.TypeOpt, error) {
	if !m.isMySQLPGSpatial() || !hasMySQLSpatialColumns(info) {
		return opt, nil
	}
	for _, col := range info.Columns {
		if err := dialect.ValidateMySQLSpatialColumn(col); err != nil {
			return opt, fmt.Errorf("表 %s 列 %s: %w", info.TableName, col.Name, err)
		}
	}
	cap, err := m.prepareMySQLPGSpatial(ctx)
	if err != nil {
		return opt, fmt.Errorf("表 %s 空间类型预检: %w", info.TableName, err)
	}
	opt.SpatialSchema = cap.Schema
	return opt, nil
}

func (m *Migrator) mappedMySQLSpatialTable(info *source.TableDDLInfo) *source.TableDDLInfo {
	mapped := cloneSourceTableDDLInfo(info)
	mapped.TableName = m.objName(info.TableName)
	for i := range mapped.Columns {
		mapped.Columns[i].Name = m.targetColumn(info.TableName, mapped.Columns[i].Name)
	}
	return mapped
}

func (m *Migrator) checkMySQLPGSpatialExisting(ctx context.Context, info *source.TableDDLInfo) error {
	if !m.isMySQLPGSpatial() || !hasMySQLSpatialColumns(info) {
		return nil
	}
	cap, err := m.prepareMySQLPGSpatial(ctx)
	if err != nil {
		return err
	}
	mapped := m.mappedMySQLSpatialTable(info)
	return m.writer.(mysqlPGSpatialWriter).CheckMySQLPGSpatialTable(ctx, mapped.TableName, mapped, cap)
}

func (m *Migrator) preflightMySQLPGSpatialData(ctx context.Context, tables []string, report *MigrationReport, failed map[string]bool) bool {
	if len(tables) == 0 || !m.isMySQLPGSpatial() {
		return true
	}
	for _, table := range tables {
		if failed[table] || m.migrationTableBlocked(table) {
			continue
		}
		info, err := m.sourceTableDDLInfo(ctx, table)
		if err == nil {
			err = m.checkMySQLPGSpatialExisting(ctx, info)
		}
		if err == nil {
			continue
		}
		if m.stopMigrationTask(ctx, err) {
			return false
		}
		failed[table] = true
		m.blockMigrationTable(table, "preflight", err)
		report.Data.Failed++
		report.Data.Items = append(report.Data.Items, ObjectResult{Name: table, Stage: "preflight", Error: err.Error()})
		m.log.Errorf("空间数据迁移预检失败 [%s]: %v", table, err)
	}
	return true
}

// Keep source metadata intact. Only selected MySQL -> PG indexes lose the
// Reader's explicit unsupported marker, after all required checks succeed.
func (m *Migrator) planMySQLPGSpatialIndex(ctx context.Context, idx source.IndexInfo) source.IndexInfo {
	if !m.isMySQLPGSpatial() || !source.IsMySQLSpatialIndex(idx.Method) {
		return idx
	}
	if idx.Unsupported != "" && idx.Unsupported != source.MySQLSpatialIndexUnsupported {
		return idx
	}
	fail := func(err error) source.IndexInfo {
		idx.Unsupported = fmt.Sprintf("空间索引 %s.%s: %v", idx.TableName, idx.IndexName, err)
		m.stopMigrationTask(ctx, err)
		return idx
	}
	if len(idx.Columns) != 1 || idx.IsUnique || idx.IsPrimary {
		return fail(fmt.Errorf("仅支持非唯一的单列 SPATIAL 索引"))
	}
	info, err := m.sourceTableDDLInfo(ctx, idx.TableName)
	if err != nil {
		return fail(err)
	}
	if info == nil {
		return fail(fmt.Errorf("缺少源表列元数据"))
	}
	found := false
	for _, c := range info.Columns {
		if c.Name == idx.Columns[0] {
			_, found = source.MySQLSpatialSubtype(c.DataType)
		}
	}
	if !found {
		return fail(fmt.Errorf("索引列 %s 不是已支持的空间类型", idx.Columns[0]))
	}
	cap, err := m.prepareMySQLPGSpatial(ctx)
	if err != nil {
		return fail(err)
	}
	if cap.GISTError != "" || cap.GISTOpClass == "" {
		return fail(fmt.Errorf("目标 GiST 空间索引不可用: %s", cap.GISTError))
	}
	if m.cfg.objectMode() {
		indexedInfo := &source.TableDDLInfo{TableName: info.TableName}
		for _, col := range info.Columns {
			if col.Name == idx.Columns[0] {
				indexedInfo.Columns = append(indexedInfo.Columns, col)
			}
		}
		if err = m.checkMySQLPGSpatialExisting(ctx, indexedInfo); err != nil {
			return fail(err)
		}
	}
	idx.Method, idx.Unsupported = "gist", ""
	idx.PGSpatialSchema, idx.PGSpatialOpClass = cap.GISTSchema, cap.GISTOpClass
	idx.Columns = slices.Clone(idx.Columns)
	return idx
}

// Recovery previews are read-only: enabling an extension belongs to a new
// migration/execution, never to the preview of a destructive reload.
func inspectMySQLPGSpatialRecovery(ctx context.Context, r source.Reader, w target.Writer, info *source.TableDDLInfo, table string, mapping map[string]string) error {
	if r.DBType() != "mysql" || !source.IsPGFamily(w.DBType()) || !hasMySQLSpatialColumns(info) {
		return nil
	}
	checker, ok := w.(mysqlPGSpatialWriter)
	if !ok {
		return fmt.Errorf("目标 %s 未实现空间能力检查", w.DBType())
	}
	cap, err := checker.InspectMySQLPGSpatial(ctx)
	if err != nil {
		return fmt.Errorf("补迁空间能力检查: %w", err)
	}
	mapped := cloneSourceTableDDLInfo(info)
	mapped.TableName = table
	for i := range mapped.Columns {
		mapped.Columns[i].Name = mapping[mapped.Columns[i].Name]
	}
	return checker.CheckMySQLPGSpatialTable(ctx, table, mapped, cap)
}
