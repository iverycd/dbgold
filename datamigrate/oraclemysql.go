package datamigrate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/datamigrate/valueconv"
)

// These optional capabilities keep other writers and the public Writer contract
// independent of the Oracle -> MySQL replacement workflow.
type oracleMySQLTarget interface {
	OracleMySQLSchema(context.Context) (string, error)
	OracleMySQLVersion(context.Context) (dialect.MySQLVersion, error)
	OracleMySQLLowerCaseTableNames(context.Context) (int, error)
	OracleMySQLForeignKeys(context.Context) ([]target.MySQLForeignKey, error)
	ExecuteOracleMySQLDependency(context.Context, string) error
}

// OracleMySQLDependencyResult describes physical target cleanup separately from
// source foreign keys. RestoreSQL is reference material, never a generic retry.
type OracleMySQLDependencyResult struct {
	ForeignKey    target.MySQLForeignKey `json:"foreign_key"`
	DropStatus    string                 `json:"drop_status"`
	RestoreStatus string                 `json:"restore_status"`
	Error         string                 `json:"error,omitempty"`
}

type oracleMySQLRunState struct {
	plans          map[string]*dialect.OracleMySQLTablePlan
	planErrors     map[string]error
	statements     map[string][]dialect.Statement
	selected       map[string]bool
	blocked        map[string]error
	postEligible   map[string]bool
	keyFailed      map[string]bool // Target identifiers.
	createdKeys    map[string]map[string]bool
	touched        map[string]bool // Conservative: attempted DROP counts as touched.
	inventoryRead  bool
	inventoryError error
	primaryKeys    []source.IndexInfo
	indexes        []source.IndexInfo
	foreignKeys    []source.FKInfo
	version        dialect.MySQLVersion
	versionRead    bool
	versionError   error
	dependencies   []OracleMySQLDependencyResult
	targetSnapshot []target.MySQLForeignKey
	lowerCaseNames int
}

func (m *Migrator) oracleMySQLSchemaMode() bool {
	return m.reader.DBType() == "oracle" && m.writer.DBType() == "mysql" &&
		m.cfg.Content != "data_only" && !m.cfg.objectMode() && len(m.cfg.ViewNames) == 0
}

func (m *Migrator) oracleMySQLState() *oracleMySQLRunState {
	if m.oracleMySQL == nil {
		m.oracleMySQL = &oracleMySQLRunState{
			plans: map[string]*dialect.OracleMySQLTablePlan{}, planErrors: map[string]error{},
			statements: map[string][]dialect.Statement{}, selected: map[string]bool{},
			blocked: map[string]error{}, postEligible: map[string]bool{},
			keyFailed: map[string]bool{}, touched: map[string]bool{},
			createdKeys: map[string]map[string]bool{},
		}
	}
	return m.oracleMySQL
}

func oracleMySQLTargetKey(name string, caseMode int) string {
	if caseMode != 0 {
		return strings.ToLower(name)
	}
	return name
}

func (m *Migrator) oracleMySQLTargetKey(name string) string {
	return oracleMySQLTargetKey(name, m.oracleMySQL.lowerCaseNames)
}

func (m *Migrator) readOracleMySQLInventory(ctx context.Context) error {
	s := m.oracleMySQLState()
	if !s.inventoryRead {
		s.inventoryRead = true
		var pe, ie, fe error
		s.primaryKeys, pe = m.reader.GetPrimaryKeys(ctx)
		s.indexes, ie = m.readSourceIndexes(ctx)
		s.foreignKeys, fe = m.reader.GetForeignKeys(ctx)
		if err := errors.Join(pe, ie, fe); err != nil {
			s.inventoryError = fmt.Errorf("读取 Oracle 主键、索引或外键依赖: %w", err)
		}
	}
	return s.inventoryError
}

// Target-specific normalization must also happen before table planning, which
// protects the real indexed columns from automatic type promotion.
func (m *Migrator) readSourceIndexes(ctx context.Context) ([]source.IndexInfo, error) {
	if m.reader.DBType() == "oracle" && m.writer.DBType() == "mysql" {
		if reader, ok := m.reader.(interface {
			GetIndexesForMySQL(context.Context) ([]source.IndexInfo, error)
		}); ok {
			return reader.GetIndexesForMySQL(ctx)
		}
	}
	indexes, err := m.reader.GetIndexes(ctx)
	if err == nil && m.reader.DBType() == "mysql" && source.IsPGFamily(m.writer.DBType()) {
		indexes = m.nameMySQLPGIndexes(indexes)
	}
	return indexes, err
}

// Share a version probe between table and index planning without enabling the
// table replacement workflow for object-only jobs.
func (m *Migrator) oracleMySQLTargetVersion(ctx context.Context) (dialect.MySQLVersion, error) {
	if s := m.oracleMySQL; s != nil && s.versionRead {
		return s.version, s.versionError
	}
	var version dialect.MySQLVersion
	var err error
	if s := m.oracleMySQLIndexes; m.oracleMySQL == nil && s != nil && s.versionRead {
		version, err = s.version, s.versionError
	} else if writer, ok := m.writer.(interface {
		OracleMySQLVersion(context.Context) (dialect.MySQLVersion, error)
	}); ok {
		version, err = writer.OracleMySQLVersion(ctx)
	} else {
		err = fmt.Errorf("目标 Writer 缺少 MySQL 版本探测能力")
	}
	if s := m.oracleMySQL; s != nil {
		s.version, s.versionError, s.versionRead = version, err, true
	}
	if s := m.oracleMySQLIndexes; s != nil {
		s.version, s.versionError, s.versionRead = version, err, true
	}
	return version, err
}

func (m *Migrator) sourcePrimaryKeys(ctx context.Context) ([]source.IndexInfo, error) {
	if m.oracleMySQL != nil {
		err := m.readOracleMySQLInventory(ctx)
		return m.oracleMySQLSelectedIndexes(m.oracleMySQL.primaryKeys), err
	}
	return m.reader.GetPrimaryKeys(ctx)
}
func (m *Migrator) sourceIndexes(ctx context.Context) ([]source.IndexInfo, error) {
	if m.reader.DBType() == "oracle" && m.writer.DBType() == "mysql" && m.oracleMySQLIndexes != nil && m.oracleMySQLIndexes.loaded {
		return cloneOracleMySQLIndexes(m.oracleMySQLIndexes.indexes), m.oracleMySQLIndexes.err
	}
	var indexes []source.IndexInfo
	var err error
	if m.oracleMySQL != nil {
		err = m.readOracleMySQLInventory(ctx)
		indexes = m.oracleMySQLSelectedIndexes(m.oracleMySQL.indexes)
	} else {
		indexes, err = m.readSourceIndexes(ctx)
	}
	if m.reader.DBType() != "oracle" || m.writer.DBType() != "mysql" {
		return indexes, err
	}
	return m.prepareOracleMySQLIndexes(ctx, indexes, err)
}

func (m *Migrator) oracleMySQLSelectedIndexes(indexes []source.IndexInfo) []source.IndexInfo {
	return slices.DeleteFunc(slices.Clone(indexes), func(index source.IndexInfo) bool {
		for _, excluded := range m.cfg.ExcludedObjects {
			kind := "index"
			if index.IsPrimary {
				kind = "primary_key"
			}
			if (excluded.Kind == kind || excluded.Kind == kind+"s") &&
				(excluded.Name == index.IndexName || excluded.Name == dialect.IndexName(m.writer.Dialect(), index)) &&
				(excluded.Table == "" || excluded.Table == index.TableName) {
				return true
			}
		}
		return false
	})
}

func (m *Migrator) oracleMySQLReferenceKeyCreated(fk source.FKInfo) bool {
	columns := make([]string, len(fk.RefColumns))
	for i, column := range fk.RefColumns {
		columns[i] = m.targetColumn(fk.RefTable, column)
	}
	return m.oracleMySQL.createdKeys[m.oracleMySQLTargetKey(m.objName(fk.RefTable))][repair.Hash(columns)]
}
func (m *Migrator) sourceForeignKeys(ctx context.Context) ([]source.FKInfo, error) {
	if m.oracleMySQL != nil {
		err := m.readOracleMySQLInventory(ctx)
		return m.oracleMySQL.foreignKeys, err
	}
	return m.reader.GetForeignKeys(ctx)
}

func (m *Migrator) oracleMySQLSourceSchema() string {
	if r, ok := m.reader.(interface{ MigrationSchema() string }); ok {
		return r.MigrationSchema()
	}
	return ""
}

func (m *Migrator) oracleMySQLForeignKeyInScope(fk source.FKInfo) bool {
	s := m.oracleMySQL
	if s == nil || !s.selected[fk.TableName] || !s.selected[fk.RefTable] {
		return false
	}
	if fk.RefSchema != "" && fk.RefSchema != m.oracleMySQLSourceSchema() {
		return false
	}
	for _, excluded := range m.cfg.ExcludedObjects {
		if (excluded.Kind == "foreign_key" || excluded.Kind == "foreign_keys") && excluded.Name == fk.ConstraintName && (excluded.Table == "" || excluded.Table == fk.TableName) {
			return false
		}
	}
	return true
}

func (m *Migrator) planOracleMySQLTable(ctx context.Context, table string, info *source.TableDDLInfo) (*dialect.OracleMySQLTablePlan, error) {
	s := m.oracleMySQLState()
	if p, ok := s.plans[table]; ok {
		return p, s.planErrors[table]
	}
	if err := m.readOracleMySQLInventory(ctx); err != nil {
		return nil, err
	}
	if info == nil || len(info.Columns) == 0 {
		return nil, fmt.Errorf("表 %s 缺少列元数据", table)
	}
	protected, known := map[string]bool{}, map[string]bool{}
	for _, c := range info.Columns {
		known[c.Name] = true
	}
	opaque := false
	// Oracle incoming FKs necessarily reference a PK/unique key. Protect every
	// source key, including keys outside the selected post-DDL object subset.
	for _, idx := range append(slices.Clone(s.primaryKeys), s.indexes...) {
		if idx.TableName != table {
			continue
		}
		if idx.Unsupported != "" || len(idx.Columns) == 0 {
			opaque = true
		}
		for _, c := range idx.Columns {
			protected[c] = true
			if !known[c] { // Oracle expression indexes expose hidden SYS_NC columns.
				opaque = true
			}
		}
	}
	for _, fk := range s.foreignKeys {
		if fk.TableName == table {
			for _, c := range fk.Columns {
				protected[c] = true
			}
		}
		if fk.RefTable == table && (fk.RefSchema == "" || fk.RefSchema == m.oracleMySQLSourceSchema()) {
			for _, c := range fk.RefColumns {
				protected[c] = true
			}
		}
	}
	keys, expression := partitionKeyColumns(info.Partition)
	opaque = opaque || expression
	for _, c := range keys {
		protected[c] = true
	}
	opt := dialect.OracleMySQLOptions{ProtectedColumns: protected, BlockPromotion: opaque, Version: s.version}
	p, err := dialect.NewMySQL().PlanOracleMySQLTable(m.cfg.TargetSchema, info, opt, m.objName)
	if errors.Is(err, dialect.ErrOracleMySQLVersionRequired) {
		opt.Version, err = m.oracleMySQLTargetVersion(ctx)
		if err == nil {
			p, err = dialect.NewMySQL().PlanOracleMySQLTable(m.cfg.TargetSchema, info, opt, m.objName)
		}
	}
	s.plans[table], s.planErrors[table] = p, err
	return p, err
}

// All ordinary/partition base DDL uses this entry point. The source cache is
// never overwritten by the target plan's mapped types or normalized defaults.
func (m *Migrator) baseCreateTableStatements(ctx context.Context, table string, info *source.TableDDLInfo, opt dialect.TypeOpt) ([]dialect.Statement, error) {
	var err error
	opt, err = m.mysqlPGSpatialTypeOptions(ctx, info, opt)
	if err != nil {
		return nil, err
	}
	if m.oracleMySQLSchemaMode() {
		p, err := m.planOracleMySQLTable(ctx, table, info)
		if err != nil {
			return nil, err
		}
		return slices.Clone(p.Statements), nil
	}
	return m.writer.Dialect().CreateTableStatements(m.cfg.TargetSchema, info, m.reader.DBType(), opt, m.objName)
}

func (m *Migrator) oracleMySQLTables(ctx context.Context) ([]string, []string, error) {
	var all []string
	var err error
	if s := m.partitionState; s != nil {
		if s.preflight.FatalError != "" {
			return nil, nil, fmt.Errorf("%s", s.preflight.FatalError)
		}
		if _, ok := partitionSourceReader(m.reader).(source.PartitionReader); ok {
			all = slices.Clone(s.all)
		}
	}
	if all == nil {
		all, err = m.reader.ListTables(ctx)
		if err != nil {
			return nil, nil, err
		}
	}
	tables := FilterTables(slices.Clone(all), m.cfg.Mode, m.cfg.Filter)
	for _, excluded := range m.cfg.ExcludedObjects {
		if excluded.Kind == "table" {
			tables = slices.DeleteFunc(tables, func(t string) bool { return t == excluded.Name })
		}
	}
	return tables, all, nil
}

// A blocked/retained child protects its parent, transitively. Names are matched
// exactly against the target names generated by the task's naming policy.
func oracleMySQLExecutableTables(schema string, names map[string]string, blocked map[string]error, fks []target.MySQLForeignKey, caseMode int) map[string]bool {
	key := func(s string) string { return oracleMySQLTargetKey(s, caseMode) }
	ready := map[string]bool{}
	for targetTable, original := range names {
		ready[targetTable] = blocked[original] == nil
	}
	for changed := true; changed; {
		changed = false
		for _, fk := range fks {
			if key(fk.RefSchema) != key(schema) || !ready[key(fk.RefTable)] {
				continue
			}
			if key(fk.Schema) != key(schema) || !ready[key(fk.Table)] {
				ready[key(fk.RefTable)] = false
				blocked[names[key(fk.RefTable)]] = fmt.Errorf("目标表 %s.%s 被重建范围外的 %s.%s 外键 %s 引用，未执行 DROP", schema, fk.RefTable, fk.Schema, fk.Table, fk.ConstraintName)
				changed = true
			}
		}
	}
	return ready
}

func relevantOracleMySQLForeignKeys(schema string, ready map[string]bool, fks []target.MySQLForeignKey, caseMode int) []target.MySQLForeignKey {
	key := func(s string) string { return oracleMySQLTargetKey(s, caseMode) }
	result := []target.MySQLForeignKey{}
	for _, fk := range fks {
		if key(fk.Schema) == key(schema) && ready[key(fk.Table)] || key(fk.RefSchema) == key(schema) && ready[key(fk.RefTable)] {
			result = append(result, fk)
		}
	}
	slices.SortFunc(result, func(a, b target.MySQLForeignKey) int {
		return strings.Compare(a.Schema+"\x00"+a.Table+"\x00"+a.ConstraintName, b.Schema+"\x00"+b.Table+"\x00"+b.ConstraintName)
	})
	return result
}

func (m *Migrator) runOracleMySQL(ctx context.Context) (report MigrationReport) {
	report = newMigrationReport()
	if count, err := m.reader.GetTriggerCount(ctx); err != nil {
		report.Triggers.Total = -1
	} else {
		report.Triggers.Total = count
	}
	s := m.oracleMySQLState()
	defer func() {
		if err := ctx.Err(); err != nil {
			m.markOracleMySQLUnrestored()
			if !report.HasFailures() {
				report.Constraints.Total++
				report.Constraints.Failed++
				report.Constraints.Items = append(report.Constraints.Items, ObjectResult{Name: "任务取消", Stage: "task", Error: err.Error()})
			}
		}
		report.TargetDependencies = slices.Clone(s.dependencies)
	}()
	fatal := func(stage string, err error) {
		report.Tables.Total++
		report.Tables.Failed++
		report.Tables.Items = append(report.Tables.Items, ObjectResult{Name: stage, Stage: "preflight", Error: err.Error()})
		m.log.Errorf("%s，未继续执行: %v", stage, err)
	}
	target, ok := m.writer.(oracleMySQLTarget)
	if !ok {
		fatal("目标依赖预检", fmt.Errorf("目标 Writer 缺少 Oracle → MySQL 覆盖重建能力"))
		return
	}
	schema, err := target.OracleMySQLSchema(ctx)
	if err != nil {
		fatal("目标 database", err)
		return
	}
	if m.cfg.TargetSchema != "" && m.cfg.TargetSchema != schema {
		fatal("目标 database", fmt.Errorf("配置 database 与实际目标 database 不一致"))
		return
	}
	m.cfg.TargetSchema = schema
	s.lowerCaseNames, err = target.OracleMySQLLowerCaseTableNames(ctx)
	if err != nil {
		fatal("目标表名大小写规则", err)
		return
	}
	m.log.Info("=== Oracle → MySQL 表结构与外键预检 ===")
	m.preparePartitions(ctx)
	tables, all, err := m.oracleMySQLTables(ctx)
	if err != nil {
		// finishPartitions owns its fatal errors and category counts.
		if m.partitionState == nil || m.partitionState.preflight.FatalError == "" {
			fatal("源表列表", err)
		}
		return
	}
	m.initializeOracleMySQLAuto(ctx, tables)
	// Retain bounded source metadata prewarming, but do it before freezing target
	// plans. Partition compilation may already own a frozen cache entry.
	var pending []string
	for _, table := range tables {
		if m.partitionState != nil && m.partitionState.blocked[table] {
			continue
		}
		m.sourceDDLCache.mu.Lock()
		_, cached := m.sourceDDLCache.tables[table]
		m.sourceDDLCache.mu.Unlock()
		if !cached {
			pending = append(pending, table)
		}
	}
	prewarmed, err := m.prewarmSourceTableDDL(ctx, pending, newRepairMetadataProgress(m.log, time.Now))
	if err != nil {
		fatal("源表结构预读", err)
		return
	}
	activeTables, names := []string{}, map[string]string{}
	for _, table := range tables {
		s.selected[table] = true
		if m.partitionState != nil && m.partitionState.blocked[table] {
			s.blocked[table] = fmt.Errorf("分区预检未通过")
			continue
		}
		activeTables = append(activeTables, table)
		name := m.oracleMySQLTargetKey(m.objName(table))
		if e := validateMySQLObjectName(m.objName(table), true); e != nil {
			s.blocked[table] = e
		}
		if previous, exists := names[name]; exists && previous != table {
			s.blocked[table], s.blocked[previous] = fmt.Errorf("目标表名称与 %s 冲突", previous), fmt.Errorf("目标表名称与 %s 冲突", table)
		}
		names[name] = table
		if result, exists := prewarmed[table]; exists && result.err != nil {
			s.blocked[table] = result.err
			continue
		}
		stmts, _, e := m.buildCreateTableDDL(ctx, table)
		if e != nil && m.stopMigrationTask(ctx, e) {
			fatal("源表结构预检", e)
			return
		}
		if e == nil {
			if info, e2 := m.sourceTableDDLInfo(ctx, table); e2 == nil && info != nil {
				for _, col := range info.Columns {
					if nameErr := validateMySQLObjectName(m.objName(col.Name), false); nameErr != nil {
						e = nameErr
						break
					}
				}
			}
		}
		if e != nil {
			s.blocked[table] = e
		} else {
			s.statements[table] = stmts
		}
	}
	m.validateOracleMySQLAutoForeignKeys(ctx)
	report.Tables.Total = len(activeTables)
	if len(activeTables) > 0 {
		s.targetSnapshot, err = target.OracleMySQLForeignKeys(ctx)
		if err != nil {
			for _, table := range activeTables {
				s.blocked[table] = fmt.Errorf("目标外键依赖预检: %w", err)
			}
		}
	}
	ready := oracleMySQLExecutableTables(schema, names, s.blocked, s.targetSnapshot, s.lowerCaseNames)
	for _, table := range activeTables {
		if e := s.blocked[table]; e != nil {
			m.blockMigrationTable(table, "preflight", e)
			report.Tables.Failed++
			report.Tables.Items = append(report.Tables.Items, ObjectResult{Name: table, Stage: "preflight", Error: e.Error()})
			m.log.Errorf("表 %s 预检未通过，未执行 DROP: %v", table, e)
		}
	}
	for _, fk := range s.targetSnapshot {
		if m.oracleMySQLTargetKey(fk.Schema) == m.oracleMySQLTargetKey(schema) && m.oracleMySQLTargetKey(fk.RefSchema) == m.oracleMySQLTargetKey(schema) && ready[m.oracleMySQLTargetKey(fk.Table)] && ready[m.oracleMySQLTargetKey(fk.RefTable)] {
			s.dependencies = append(s.dependencies, OracleMySQLDependencyResult{ForeignKey: fk, DropStatus: "pending", RestoreStatus: "not_required"})
		}
	}
	for _, table := range activeTables {
		if p := s.plans[table]; p != nil {
			for _, warning := range p.Warnings {
				report.Warnings = append(report.Warnings, valueconv.MigrationWarning{Table: m.objName(table), Column: m.objName(warning.Column), Reason: warning.Reason, Count: 1})
				m.log.Warnf("表 %s 列 %s: %s", table, warning.Column, warning.Reason)
			}
		}
	}
	if m.cfg.CaptureRepair {
		m.beginRepair(ctx)
		if m.repairs.sinkErr != nil {
			fatal("修复记录持久化", m.repairs.sinkErr)
			return
		}
		if e := m.registerOracleMySQLDependencies(); e != nil {
			fatal("目标外键清理计划持久化", e)
			return
		}
	}
	if ctx.Err() != nil || !slices.ContainsFunc(activeTables, func(t string) bool { return ready[m.oracleMySQLTargetKey(m.objName(t))] }) {
		return
	}
	fresh, err := target.OracleMySQLForeignKeys(ctx)
	if err != nil || repair.Hash(relevantOracleMySQLForeignKeys(schema, ready, fresh, s.lowerCaseNames)) != repair.Hash(relevantOracleMySQLForeignKeys(schema, ready, s.targetSnapshot, s.lowerCaseNames)) {
		if err == nil {
			err = fmt.Errorf("目标外键依赖已偏离冻结计划，请重新创建任务")
		}
		fatal("执行前目标外键复核", err)
		return
	}
	for _, table := range activeTables {
		if ready[m.oracleMySQLTargetKey(m.objName(table))] {
			if e := m.recheckPartitionSource(ctx, table); e != nil {
				fatal("执行前分区复核", e)
				return
			}
		}
	}
	validationTables := slices.DeleteFunc(slices.Clone(activeTables), func(table string) bool {
		return !ready[m.oracleMySQLTargetKey(m.objName(table))]
	})
	m.freezeDataValidation(ctx, validationTables)
	m.logPreparationComplete(ctx)
	for i := range s.dependencies {
		if err := m.executeOracleMySQLDependency(ctx, target, i, false); err != nil {
			report.Constraints.Total++
			report.Constraints.Failed++
			report.Constraints.Items = append(report.Constraints.Items, ObjectResult{Name: s.dependencies[i].ForeignKey.ConstraintName, Stage: "drop_dependency", DDL: s.dependencies[i].ForeignKey.DropSQL, Error: err.Error()})
			m.restoreOracleMySQLDependencies(target)
			return
		}
	}
	m.log.Info("=== Phase 1: 创建表结构 ===")
	for _, table := range activeTables {
		if !ready[m.oracleMySQLTargetKey(m.objName(table))] {
			continue
		}
		if err := ctx.Err(); err != nil {
			m.restoreOracleMySQLDependencies(target)
			return
		}
		s.touched[m.oracleMySQLTargetKey(m.objName(table))] = true
		stmts := s.statements[table]
		if err := m.repairTable(ctx, table, stmts); err != nil {
			report.Tables.Failed++
			report.Tables.Items = append(report.Tables.Items, ObjectResult{Name: table, Stage: "execute", DDL: dialect.JoinSQL(stmts), Error: err.Error()})
			m.blockMigrationTable(table, "execute", err)
			s.blocked[table] = err
			m.log.Errorf("表 %s DROP/CREATE 失败，跳过该表继续迁移: %v；SQL: %s", table, err, dialect.JoinSQL(stmts))
			if m.stopMigrationTask(ctx, err) {
				m.restoreOracleMySQLDependencies(target)
				return
			}
			continue
		}
		report.Tables.Success++
		s.postEligible[table] = true
		m.log.DDLf("创建表 %s ... OK", table)
		if p := s.plans[table]; p != nil {
			for _, column := range p.PromotedColumns {
				sourceType := ""
				if info, err := m.sourceTableDDLInfo(ctx, table); err == nil && info != nil {
					for _, col := range info.Columns {
						if col.Name == column {
							sourceType = source.FormatColumnType(col)
							break
						}
					}
				}
				report.AutoRepairs = append(report.AutoRepairs, AutoRepair{SourceType: sourceType, Type: "oracle_mysql_longtext", Table: table, TargetTable: m.objName(table), Source: column, Target: m.objName(column) + " LONGTEXT", Reason: "整表声明行宽超过 MySQL 65535 字节上限", Status: "applied"})
			}
		}
	}
	if m.cfg.Content != "schema_only" {
		m.copyOracleMySQLTables(ctx, activeTables, &report)
	}
	if ctx.Err() != nil {
		m.markOracleMySQLUnrestored()
		return
	}
	m.log.Info("=== Phase 3: 创建主键、索引和外键 ===")
	m.createPostDDL(ctx, &report, tables, all)
	if ctx.Err() != nil {
		return
	}
	m.finalizeMySQLDependencies(target)
	m.logMigrationCompletion("迁移完成：成功 %d 张，失败 %d 张", report.Tables.Success, report.Tables.Failed+report.Data.Failed)
	return
}

func (m *Migrator) copyOracleMySQLTables(ctx context.Context, tables []string, report *MigrationReport) {
	s := m.oracleMySQL
	parallel := max(1, m.cfg.MaxParallel)
	if m.cfg.PageSize <= 0 {
		m.cfg.PageSize = 2000
	}
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	var mu sync.Mutex
	failed := map[string]string{}
	for _, table := range tables {
		if !s.postEligible[table] {
			continue
		}
		report.Data.Total++
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			mu.Lock()
			failed[table] = ctx.Err().Error()
			mu.Unlock()
			continue
		}
		wg.Add(1)
		go func(table string) {
			defer wg.Done()
			defer func() { <-sem }()
			ok, message := m.migrateTableData(ctx, table)
			mu.Lock()
			defer mu.Unlock()
			if !ok {
				failed[table] = message
			}
		}(table)
	}
	wg.Wait()
	for _, table := range tables {
		if !s.postEligible[table] {
			continue
		}
		if message, bad := failed[table]; bad {
			s.postEligible[table] = false
			report.Data.Failed++
			report.Data.Items = append(report.Data.Items, ObjectResult{Name: table, Stage: "execute", Error: message})
			continue
		}
		report.Data.Success++
		m.markDataValidationCopied(table)
		src, se := m.reader.CountRows(ctx, table)
		if m.stopMigrationTask(ctx, se) {
			return
		}
		dst, de := m.writer.CountRows(ctx, m.objName(table))
		if m.stopMigrationTask(ctx, de) {
			return
		}
		rc := TableRowCount{Table: m.objName(table), Src: src, Dst: dst, Match: se == nil && de == nil && src == dst}
		report.RowCounts = append(report.RowCounts, rc)
		if !rc.Match {
			s.postEligible[table] = false
			report.Data.Success--
			report.Data.Failed++
			report.Data.Items = append(report.Data.Items, ObjectResult{Name: table, Stage: "validation", Error: fmt.Sprintf("行数校验失败，源=%d 目标=%d；源错误=%v 目标错误=%v", src, dst, se, de)})
		}
	}
}

func oracleMySQLDependencyID(fk target.MySQLForeignKey) string {
	return repairID("target_dependency", fk.Table, fk.ConstraintName)
}

func (m *Migrator) registerOracleMySQLDependencies() error {
	if m.repairs == nil {
		return nil
	}
	r := m.repairs
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, dep := range m.oracleMySQL.dependencies {
		fk := dep.ForeignKey
		id := oracleMySQLDependencyID(fk)
		r.appendObject(repair.Object{ID: id, Kind: "target_dependency", Schema: fk.Schema, Table: fk.Table,
			Name: fk.ConstraintName, TargetName: fk.ConstraintName, DDL: fk.DropSQL, SourceDDL: fk.RestoreSQL,
			DiagnosticOnly: true, Status: "pending", Dependencies: []string{},
			Units: []repair.Unit{
				{ID: "drop", SQL: fk.DropSQL, OriginalSQL: fk.DropSQL, Status: "pending", Destructive: true},
				{ID: "restore", SQL: fk.RestoreSQL, OriginalSQL: fk.RestoreSQL, Status: "not_required"},
			}})
		for i := range r.objects {
			o := &r.objects[i]
			if o.Kind == "table" && (m.oracleMySQLTargetKey(o.Table) == m.oracleMySQLTargetKey(fk.Table) || m.oracleMySQLTargetKey(o.Table) == m.oracleMySQLTargetKey(fk.RefTable)) {
				o.Dependencies = append(o.Dependencies, id)
			}
		}
	}
	return m.flushRepairsLocked()
}

func (m *Migrator) saveOracleMySQLDependency(index int) error {
	if m.repairs == nil {
		return nil
	}
	r := m.repairs
	r.mu.Lock()
	defer r.mu.Unlock()
	dep := m.oracleMySQL.dependencies[index]
	position := r.objectPosition(oracleMySQLDependencyID(dep.ForeignKey))
	if position < 0 {
		return fmt.Errorf("目标外键清理缺少已保存执行计划")
	}
	o := &r.objects[position]
	o.Units[0].Status, o.Units[1].Status = dep.DropStatus, dep.RestoreStatus
	for i := range o.Units {
		o.Units[i].Error = ""
		if o.Units[i].Status == "failed" || o.Units[i].Status == "unknown" || o.Units[i].Status == "blocked" {
			o.Units[i].Error = dep.Error
		}
	}
	o.Error = dep.Error
	o.Status = dep.DropStatus
	if dep.RestoreStatus != "not_required" {
		o.Status = dep.RestoreStatus
	}
	if dep.Error != "" && o.Status == "succeeded" {
		o.Status = "failed"
	}
	return m.flushRepairObjectLocked(*o)
}

func (m *Migrator) executeOracleMySQLDependency(ctx context.Context, w oracleMySQLTarget, index int, restore bool) error {
	dep := &m.oracleMySQL.dependencies[index]
	query, stage := dep.ForeignKey.DropSQL, "移除目标旧外键"
	status := &dep.DropStatus
	if restore {
		query, stage, status = dep.ForeignKey.RestoreSQL, "恢复目标旧外键", &dep.RestoreStatus
	}
	*status = "running"
	if err := m.saveOracleMySQLDependency(index); err != nil {
		*status = "blocked"
		dep.Error = fmt.Sprintf("%s前保存执行记录失败，未执行 SQL: %v", stage, err)
		return err
	}
	observed := false
	ctx = repair.WithObserver(ctx, func(event repair.Event) error {
		if event.SQL != query {
			return fmt.Errorf("目标外键清理执行 SQL 与冻结计划不一致")
		}
		observed = true
		*status = event.Status
		if event.Error != "" {
			dep.Error = event.Error
		}
		return m.saveOracleMySQLDependency(index)
	})
	err := w.ExecuteOracleMySQLDependency(ctx, query)
	if !observed || *status == "running" {
		*status = "succeeded"
		if err != nil {
			*status = "unknown"
		}
	}
	if err != nil {
		dep.Error = fmt.Sprintf("%s失败，SQL: %s；%v", stage, query, err)
		m.log.Errorf("%s", dep.Error)
	} else if restore {
		dep.Error = ""
	}
	return errors.Join(err, m.saveOracleMySQLDependency(index))
}

func (m *Migrator) markOracleMySQLUnrestored() {
	for i := range m.oracleMySQL.dependencies {
		dep := &m.oracleMySQL.dependencies[i]
		if dep.DropStatus == "succeeded" && dep.RestoreStatus == "not_required" {
			dep.RestoreStatus = "blocked"
			dep.Error = "任务未完成，旧约束未恢复；请核对目标当前结构和记录中的恢复 SQL"
			_ = m.saveOracleMySQLDependency(i) // sinkErr remains visible to finishRepair.
		}
	}
}

func (m *Migrator) restoreOracleMySQLDependencies(w oracleMySQLTarget) {
	// Restoration must remain bounded and work even after task cancellation.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for i := len(m.oracleMySQL.dependencies) - 1; i >= 0; i-- {
		dep := &m.oracleMySQL.dependencies[i]
		if dep.DropStatus != "succeeded" {
			continue
		}
		if m.oracleMySQL.touched[m.oracleMySQLTargetKey(dep.ForeignKey.Table)] || m.oracleMySQL.touched[m.oracleMySQLTargetKey(dep.ForeignKey.RefTable)] {
			dep.RestoreStatus = "blocked"
			dep.Error = "约束端点已尝试重建，未向变更后的表恢复旧约束；请核对记录中的恢复 SQL"
			_ = m.saveOracleMySQLDependency(i)
			continue
		}
		_ = m.executeOracleMySQLDependency(ctx, w, i, true)
	}
}

// Healthy replacement endpoints intentionally use source constraints, never the
// old target definition. Failed/touched endpoints require manual review.
func (m *Migrator) finalizeMySQLDependencies(w oracleMySQLTarget) {
	s := m.oracleMySQL
	healthy := map[string]bool{}
	for table, ok := range s.postEligible {
		if ok {
			healthy[m.oracleMySQLTargetKey(m.objName(table))] = true
		}
	}
	for i := range s.dependencies {
		dep := &s.dependencies[i]
		if dep.DropStatus != "succeeded" || healthy[m.oracleMySQLTargetKey(dep.ForeignKey.Table)] && healthy[m.oracleMySQLTargetKey(dep.ForeignKey.RefTable)] {
			continue
		}
		if s.touched[m.oracleMySQLTargetKey(dep.ForeignKey.Table)] || s.touched[m.oracleMySQLTargetKey(dep.ForeignKey.RefTable)] {
			dep.RestoreStatus = "blocked"
			dep.Error = "关联表迁移失败且已尝试重建，未恢复旧约束；请核对记录中的恢复 SQL"
			_ = m.saveOracleMySQLDependency(i)
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_ = m.executeOracleMySQLDependency(ctx, w, i, true)
			cancel()
		}
	}
}
