package datamigrate

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/partition"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
)

// PartitionPreflight is additive: Ready means every selected table passed;
// CanStart permits independent tables to run after table-local failures.
type PartitionPreflight struct {
	Ready      bool                    `json:"ready"`
	CanStart   bool                    `json:"can_start"`
	SourceType string                  `json:"source_type"`
	TargetType string                  `json:"target_type"`
	Issues     []CompatibilityIssue    `json:"issues"`
	Tables     []*PartitionTableResult `json:"tables"`
	FatalError string                  `json:"fatal_error,omitempty"`
}

type PartitionTableResult struct {
	schemaExecuted bool
	dataExecuted   bool
	SourceTable    string                `json:"source_table"`
	TargetTable    string                `json:"target_table"`
	Status         string                `json:"status"`
	Error          string                `json:"error,omitempty"`
	Source         *source.PartitionSpec `json:"source,omitempty"`
	Plan           *partition.Plan       `json:"plan,omitempty"`
	SourceRows     map[string]int64      `json:"source_rows,omitempty"`
	TargetRows     map[string]int64      `json:"target_rows,omitempty"`
	Baseline       map[string]int64      `json:"baseline,omitempty"`
}

type partitionRunState struct {
	mu            sync.Mutex
	preflight     *PartitionPreflight
	all           []string
	roots         map[string]string
	tables        map[string]*PartitionTableResult
	blocked       map[string]bool
	profile       partition.Profile
	hasPartitions bool
	pgSnapshot    *source.PGSnapshot
	mySQLProfile  target.MySQLProfile
	pgBaseLoaded  bool
	pgBaseError   error
}

func partitionSourceProduct(r source.Reader) string {
	if profiled, ok := r.(interface{ PGProfile() source.PGProfile }); ok {
		if name := profiled.PGProfile().Product; name != "" {
			return name
		}
	}
	return r.DBType()
}

// Metadata snapshots intentionally expose only the established Reader contract.
// Optional partition I/O must reach the original reader while metadata used by
// the migration continues to come from the validated snapshot.
func partitionSourceReader(r source.Reader) source.Reader {
	for {
		snapshot, ok := r.(*sqlServerDamengSnapshot)
		if !ok {
			return r
		}
		r = snapshot.Reader
	}
}

// PreflightPartitions performs metadata and target catalog reads only. Run
// builds its own plan; no client-supplied SQL or readiness is trusted.
func (m *Migrator) PreflightPartitions(ctx context.Context) *PartitionPreflight {
	m.oracleMySQL = nil
	m.mysqlPGSpatial = mysqlPGSpatialState{readOnly: true}
	m.resetSourceTableDDLCache()
	defer m.clearSourceTableDDLCache()
	m.partitionState = nil
	m.dmPlans, m.dmUsable, m.dmBlocked = nil, nil, nil
	return m.preparePartitions(ctx)
}

func (m *Migrator) preparePartitions(ctx context.Context) *PartitionPreflight {
	if m.partitionState != nil {
		return m.partitionState.preflight
	}
	r, enabled := partitionSourceReader(m.reader).(source.PartitionReader)
	targetType := m.cfg.TargetDBType
	if enabled {
		targetType = m.writer.DBType()
	}
	p := &PartitionPreflight{Ready: true, SourceType: partitionSourceProduct(m.reader), TargetType: targetType, Issues: []CompatibilityIssue{}, Tables: []*PartitionTableResult{}}
	s := &partitionRunState{preflight: p, roots: map[string]string{}, tables: map[string]*PartitionTableResult{}, blocked: map[string]bool{}}
	m.partitionState = s
	if !enabled {
		p.CanStart = true
		return p
	} // Legacy/custom readers do not acquire new Writer method requirements.
	if m.isDamengNative() {
		p.CanStart = true
		return p
	}
	fatal := func(err error) *PartitionPreflight {
		p.Ready = false
		p.CanStart = false
		p.FatalError = err.Error()
		return p
	}
	all, err := m.reader.ListTables(ctx)
	if err != nil {
		return fatal(fmt.Errorf("读取分区迁移表清单: %w", err))
	}
	s.roots, err = r.PartitionRoots(ctx)
	if err != nil {
		return fatal(fmt.Errorf("读取分区父子关系: %w", err))
	}
	for _, table := range all {
		if root := s.roots[table]; root == "" || root == table {
			s.all = append(s.all, table)
		}
	}
	selected := FilterTables(slices.Clone(s.all), m.cfg.Mode, m.cfg.Filter)
	if m.cfg.Mode == "" {
		selected = slices.Clone(s.all)
	}
	if m.cfg.objectMode() && len(m.cfg.TableNames) > 0 {
		selected = slices.Clone(m.cfg.TableNames)
	}
	for _, excluded := range m.cfg.ExcludedObjects {
		if excluded.Kind == "table" {
			selected = slices.DeleteFunc(selected, func(t string) bool { return t == excluded.Name })
		}
	}
	// Explicit physical children never silently broaden the user's selection.
	requested := slices.Clone(m.cfg.TableNames)
	if m.cfg.Mode == "include" {
		requested = append(requested, strings.FieldsFunc(m.cfg.Filter, func(r rune) bool { return r == ',' || r == '，' || r == ';' || r == '\n' })...)
	}
	for _, name := range requested {
		name = strings.TrimSpace(name)
		if root := s.roots[name]; root != "" && root != name {
			entry := &PartitionTableResult{SourceTable: name, TargetTable: m.objName(name)}
			s.tables[name] = entry
			p.Tables = append(p.Tables, entry)
			m.blockPartitionTable(entry, fmt.Errorf("%s 是物理分区，请选择逻辑根表 %s", name, root))
		}
	}
	for _, table := range selected {
		if err := ctx.Err(); err != nil {
			return fatal(err)
		}
		if s.tables[table] != nil {
			continue
		}
		entry := &PartitionTableResult{SourceTable: table, TargetTable: m.objName(table), Status: "ordinary"}
		s.tables[table] = entry
		p.Tables = append(p.Tables, entry)
		spec, e := r.PartitionMetadata(ctx, table)
		if e != nil {
			m.blockPartitionTable(entry, fmt.Errorf("分区元数据读取失败: %w", e))
			continue
		}
		if spec == nil {
			continue
		}
		s.hasPartitions = true
		entry.Source = source.ClonePartitionSpec(spec)
		entry.Status = "ready"
		if m.cfg.MigrationPurpose == "cdc" {
			m.blockPartitionTable(entry, fmt.Errorf("本期分区保留能力不支持 CDC 首次全量导入及增量路由"))
			continue
		}
		if spec.Unsupported != "" {
			m.blockPartitionTable(entry, fmt.Errorf("%s", spec.Unsupported))
			continue
		}
		info, e := m.sourceTableDDLInfo(ctx, table)
		if e != nil || info == nil {
			if e == nil {
				e = fmt.Errorf("没有列元数据")
			}
			m.blockPartitionTable(entry, e)
			continue
		}
		info.Partition = source.ClonePartitionSpec(spec)
		m.sourceDDLCache.mu.Lock()
		if m.sourceDDLCache.tables == nil {
			m.sourceDDLCache.tables = make(map[string]*source.TableDDLInfo)
		}
		m.sourceDDLCache.tables[table] = cloneSourceTableDDLInfo(info)
		m.sourceDDLCache.mu.Unlock()
	}
	if s.hasPartitions {
		inspector, available := m.writer.(target.PartitionInspector)
		if !available {
			for _, t := range p.Tables {
				if t.Source != nil && !s.blocked[t.SourceTable] {
					m.blockPartitionTable(t, fmt.Errorf("目标 %s 的分区目录适配尚未实现，未执行该表", m.writer.DBType()))
				}
			}
		} else {
			s.profile, err = inspector.InspectPartitionProfile(ctx)
			if err == nil && m.cfg.TargetSchema == "" {
				m.cfg.TargetSchema = s.profile.Schema
			}
			for _, t := range p.Tables {
				if t.Source == nil || s.blocked[t.SourceTable] {
					continue
				}
				if err != nil {
					m.blockPartitionTable(t, fmt.Errorf("目标分区能力读取失败: %w", err))
					continue
				}
				if e := m.compilePartitionTable(ctx, t, inspector); e != nil {
					m.blockPartitionTable(t, e)
				}
			}
		}
		m.checkPartitionConstraints(ctx)
		m.checkPartitionPlanNames()
	}
	p.Ready = p.FatalError == "" && len(s.blocked) == 0
	p.CanStart = false
	for _, t := range p.Tables {
		if !s.blocked[t.SourceTable] {
			p.CanStart = true
			break
		}
	}
	if len(p.Tables) == 0 && len(m.cfg.ViewNames) > 0 {
		p.CanStart = true
	}
	return p
}

func (m *Migrator) blockPartitionTable(t *PartitionTableResult, err error) {
	s := m.partitionState
	if s.blocked[t.SourceTable] {
		return
	}
	s.blocked[t.SourceTable] = true
	t.Status = "blocked"
	t.Error = err.Error()
	s.preflight.Ready = false
	s.preflight.Issues = append(s.preflight.Issues, CompatibilityIssue{Object: tableRef(t.SourceTable), Severity: "error", Reason: err.Error(), Suggestion: "调整源定义或明确排除此表后重试；本次其他表继续执行", Excludable: true})
}

func (m *Migrator) compilePartitionTable(ctx context.Context, t *PartitionTableResult, inspector target.PartitionInspector) error {
	info, err := m.sourceTableDDLInfo(ctx, t.SourceTable)
	if err != nil {
		return err
	}
	baseInfo := info
	if m.writer.DBType() == "dameng" {
		plan, e := m.planDamengTable(ctx, t.SourceTable)
		if e != nil {
			return e
		}
		baseInfo = plan.target
	}
	var base []dialect.Statement
	if m.isPGMySQL() {
		base, err = m.pgPartitionBaseStatements(ctx, t.SourceTable)
	} else {
		base, err = m.baseCreateTableStatements(ctx, t.SourceTable, baseInfo, dialect.TypeOpt{CharInLength: m.cfg.CharInLength, UseNvarchar2: m.cfg.UseNvarchar2})
	}
	if err != nil {
		return err
	}
	t.Plan, err = partition.Compile(info, base, m.partitionState.profile, partition.Options{SourceType: partitionSourceProduct(m.reader), TargetType: m.writer.DBType(), TargetSchema: m.cfg.TargetSchema, Name: m.objName, ColumnName: func(c string) string { return m.targetColumn(t.SourceTable, c) }})
	if err != nil {
		return err
	}
	if m.cfg.Distributed {
		return fmt.Errorf("分区与分布列联合路由尚未完成等价适配，未执行该表")
	}
	if m.cfg.Content == "data_only" || m.cfg.objectMode() {
		actual, e := inspector.InspectPartitionTable(ctx, t.TargetTable)
		if e != nil {
			return e
		}
		if e = partition.Equivalent(t.Plan.Target, actual); e != nil {
			return fmt.Errorf("已有目标分区结构不兼容: %w", e)
		}
		if e = target.VerifyPartitionKeyColumns(ctx, m.writer, t.TargetTable, t.Plan); e != nil {
			return e
		}
		t.Baseline, e = inspector.CountPartitionRows(ctx, t.TargetTable, t.Plan.Target)
		if e != nil {
			return e
		}
		for _, mapping := range t.Plan.Mapping {
			count, found := partitionRowCount(t.Baseline, mapping.TargetSchema, mapping.TargetName, mapping.TargetOrdinal)
			if !found || count < 0 {
				return fmt.Errorf("目标分区 %s 的追加基线缺失或无效", mapping.TargetName)
			}
		}
	} else if checker, ok := m.writer.(interface {
		ValidatePartitionNames(context.Context, string, *source.PartitionSpec) error
	}); ok {
		if err = checker.ValidatePartitionNames(ctx, t.TargetTable, t.Plan.Target); err != nil {
			return err
		}
	}
	return nil
}

// PG sources have a separate, version-aware MySQL DDL path. Generic dialect
// CreateTableStatements deliberately rejects them. This read-only preliminary
// plan must use the same table metadata; the full PG preflight still validates
// defaults, indexes, constraints, target names and copy conversions afterward.
func (m *Migrator) pgPartitionBaseStatements(ctx context.Context, table string) ([]dialect.Statement, error) {
	s := m.partitionState
	if !s.pgBaseLoaded {
		s.pgBaseLoaded = true
		r, sourceOK := m.reader.(pgSnapshotReader)
		w, targetOK := m.writer.(mysqlInspector)
		if !sourceOK || !targetOK {
			s.pgBaseError = fmt.Errorf("PG 分区迁往 MySQL 需要完整源元数据及目标版本探测")
		} else {
			s.mySQLProfile, s.pgBaseError = w.InspectMySQL(ctx)
			if s.pgBaseError == nil {
				if scoped, ok := r.(pgScopedSnapshotReader); ok {
					s.pgSnapshot, s.pgBaseError = scoped.ReadPGSnapshotForTables(ctx, m.pgMySQLSnapshotTables())
				} else {
					s.pgSnapshot, s.pgBaseError = r.ReadPGSnapshot(ctx)
				}
			}
		}
	}
	if s.pgBaseError != nil {
		return nil, s.pgBaseError
	}
	if s.pgSnapshot == nil {
		return nil, fmt.Errorf("PG 分区源元数据快照为空")
	}
	if s.mySQLProfile.Database != m.cfg.TargetSchema {
		return nil, fmt.Errorf("目标分区 Schema 与 MySQL 版本探测的数据库不一致")
	}
	for _, original := range s.pgSnapshot.Tables {
		if original.Info.TableName != table {
			continue
		}
		t := original
		t.Info.Columns = slices.Clone(original.Info.Columns)
		if m.cfg.Content != "data_only" && !m.cfg.objectMode() {
			preview := &PGMySQLPreflight{Target: s.mySQLProfile, Source: s.pgSnapshot.Profile, snapshot: s.pgSnapshot}
			m.fitPGMySQLRow(preview, &t)
			for _, issue := range preview.Issues {
				if issue.Severity == "error" {
					return nil, fmt.Errorf("%s", issue.Reason)
				}
			}
		}
		stmts, _, err := dialect.NewMySQL().PGTableStatements(m.cfg.TargetSchema, t, s.mySQLProfile.Parsed, m.objName)
		return stmts, err
	}
	return nil, fmt.Errorf("PG 元数据快照缺少所选分区根表 %s", table)
}

func partitionKeyColumns(spec *source.PartitionSpec) ([]string, bool) {
	var names []string
	expression := false
	if spec == nil {
		return names, false
	}
	for _, k := range spec.Keys {
		if k.Column == "" || k.Expression != "" {
			expression = true
		}
		if k.Column != "" && !slices.Contains(names, k.Column) {
			names = append(names, k.Column)
		}
	}
	for _, p := range spec.Partitions {
		cols, expr := partitionKeyColumns(p.Subpartition)
		expression = expression || expr
		for _, c := range cols {
			if !slices.Contains(names, c) {
				names = append(names, c)
			}
		}
	}
	return names, expression
}

func (m *Migrator) checkPartitionConstraints(ctx context.Context) {
	s := m.partitionState
	pks, pe := m.reader.GetPrimaryKeys(ctx)
	indexes, ie := m.reader.GetIndexes(ctx)
	fks, fe := m.reader.GetForeignKeys(ctx)
	for _, t := range s.preflight.Tables {
		if t.Plan == nil || s.blocked[t.SourceTable] {
			continue
		}
		if err := verifyPartitionKeySourceChecks(ctx, m.reader, t.SourceTable, t.Plan); err != nil {
			m.blockPartitionTable(t, err)
			continue
		}
		if pe != nil || ie != nil || fe != nil {
			m.blockPartitionTable(t, fmt.Errorf("无法证明分区约束完整: 主键=%v，索引=%v，外键=%v", pe, ie, fe))
			continue
		}
		all := append(slices.Clone(pks), indexes...)
		if pr, ok := partitionSourceReader(m.reader).(interface {
			PartitionIndexes(context.Context, string) ([]source.IndexInfo, error)
		}); ok {
			extra, e := pr.PartitionIndexes(ctx, t.SourceTable)
			if e != nil {
				m.blockPartitionTable(t, e)
				continue
			}
			all = append(all, extra...)
		}
		keys, expression := partitionKeyColumns(t.Source)
		needsKeys := source.IsPGFamily(m.writer.DBType()) || m.writer.DBType() == "mysql"
		for _, idx := range all {
			if idx.TableName != t.SourceTable || (!idx.IsPrimary && !idx.IsUnique) {
				continue
			}
			if idx.Unsupported != "" {
				m.blockPartitionTable(t, fmt.Errorf("唯一约束 %s 无法等价迁移: %s", idx.IndexName, idx.Unsupported))
				break
			}
			if strings.EqualFold(idx.Locality, "local") {
				if expression {
					m.blockPartitionTable(t, fmt.Errorf("LOCAL 唯一索引 %s 的表达式分区归属尚不能证明等价", idx.IndexName))
					break
				}
				for _, key := range keys {
					if !slices.Contains(idx.Columns, key) {
						m.blockPartitionTable(t, fmt.Errorf("LOCAL 唯一索引 %s 不能转换为更严格的全局唯一约束", idx.IndexName))
						break
					}
				}
			}
			if needsKeys {
				_, targetExpr := partitionKeyColumns(t.Plan.Target)
				if expression || targetExpr {
					m.blockPartitionTable(t, fmt.Errorf("唯一约束 %s 与表达式分区键无法保持目标全局唯一性", idx.IndexName))
					break
				}
				for _, key := range keys {
					if !slices.Contains(idx.Columns, key) {
						m.blockPartitionTable(t, fmt.Errorf("唯一约束 %s 未包含分区键 %s，不能通过追加键列改变唯一性", idx.IndexName, key))
						break
					}
				}
			}
		}
		if m.writer.DBType() == "mysql" {
			for _, fk := range fks {
				if fk.TableName == t.SourceTable || fk.RefTable == t.SourceTable {
					m.blockPartitionTable(t, fmt.Errorf("MySQL 分区 InnoDB 无法保留外键 %s 的引用语义", fk.ConstraintName))
					break
				}
			}
		}
	}
}

func (m *Migrator) checkPartitionPlanNames() {
	s := m.partitionState
	reserved := map[string]string{}
	for _, name := range s.all {
		reserved[m.objName(name)] = name
	}
	for _, t := range s.preflight.Tables {
		if t.Plan == nil || s.blocked[t.SourceTable] {
			continue
		}
		for _, mapping := range t.Plan.Mapping {
			if mapping.TargetName == "" {
				continue
			}
			key := mapping.TargetName
			// PG partitions are relations; other native names are table-scoped.
			if !s.profile.Features["postgres_declarative"] {
				key = t.TargetTable + "\x00" + key
			}
			if owner, ok := reserved[key]; ok {
				m.blockPartitionTable(t, fmt.Errorf("分区名称 %s 与 %s 冲突", mapping.TargetName, owner))
				if other := s.tables[owner]; other != nil {
					m.blockPartitionTable(other, fmt.Errorf("表名与 %s 的分区冲突", t.SourceTable))
				}
			} else {
				reserved[key] = t.SourceTable
			}
		}
	}
}

func (m *Migrator) migrationTables(ctx context.Context) ([]string, error) {
	if s := m.partitionState; s != nil {
		if s.preflight.FatalError != "" {
			return nil, fmt.Errorf("%s", s.preflight.FatalError)
		}
		if _, ok := partitionSourceReader(m.reader).(source.PartitionReader); ok {
			return slices.DeleteFunc(slices.Clone(s.all), func(t string) bool {
				if s.blocked[t] {
					return true
				}
				for _, excluded := range m.cfg.ExcludedObjects {
					if excluded.Kind == "table" && excluded.Name == t {
						return true
					}
				}
				return false
			}), nil
		}
	}
	return m.reader.ListTables(ctx)
}

func (m *Migrator) partitionStatements(table string, base []dialect.Statement) ([]dialect.Statement, error) {
	if s := m.partitionState; s != nil {
		if t := s.tables[table]; t != nil && t.Plan != nil {
			if s.blocked[table] {
				return nil, fmt.Errorf("%s", t.Error)
			}
			info, e := m.sourceTableDDLInfo(context.Background(), table)
			if e != nil {
				return nil, e
			}
			p, e := partition.Compile(info, base, s.profile, partition.Options{SourceType: partitionSourceProduct(m.reader), TargetType: m.writer.DBType(), TargetSchema: m.cfg.TargetSchema, Name: m.objName, ColumnName: func(c string) string { return m.targetColumn(table, c) }})
			if e != nil {
				return nil, e
			}
			t.Plan = p
			return p.Statements, nil
		}
	}
	return base, nil
}

func (m *Migrator) recheckPartitionSource(ctx context.Context, table string) error {
	if s := m.partitionState; s != nil {
		if t := s.tables[table]; t != nil && t.Source != nil {
			reader, ok := partitionSourceReader(m.reader).(source.PartitionReader)
			if !ok {
				return fmt.Errorf("源库缺少分区元数据复核能力")
			}
			fresh, e := reader.PartitionMetadata(ctx, table)
			if e != nil {
				return e
			}
			if repair.Hash(fresh) != repair.Hash(t.Source) {
				return fmt.Errorf("源表 %s 的分区定义已偏离冻结计划", table)
			}
			if err := verifyPartitionKeySourceChecks(ctx, m.reader, table, t.Plan); err != nil {
				return err
			}
			if partitionNeedsColumnProof(t.Source) {
				// Interval and exact KEY expression proofs depend on the full
				// key domain and nullability, beyond the partition catalog.
				m.sourceDDLCache.mu.Lock()
				frozen := cloneSourceTableDDLInfo(m.sourceDDLCache.tables[table])
				m.sourceDDLCache.mu.Unlock()
				if frozen == nil || len(frozen.Columns) == 0 {
					return fmt.Errorf("分区源列取值域缺少冻结快照")
				}
				current, err := partitionSourceReader(m.reader).GetTableDDLInfo(ctx, table)
				if err != nil {
					return fmt.Errorf("复核分区源列取值域: %w", err)
				}
				if current == nil || !recoverySourceColumnsMatch(partitionSourceProduct(m.reader), frozen.Columns, current.Columns) {
					return fmt.Errorf("分区源表 %s 的列类型、精度或可空性已偏离冻结计划", table)
				}
			}
		}
	}
	return nil
}

func partitionNeedsColumnProof(spec *source.PartitionSpec) bool {
	if spec == nil {
		return false
	}
	if spec.Interval != nil || strings.Contains(strings.ToUpper(spec.Method), "KEY") {
		return true
	}
	for _, p := range spec.Partitions {
		if partitionNeedsColumnProof(p.Subpartition) {
			return true
		}
	}
	return false
}

func verifyPartitionKeySourceChecks(ctx context.Context, r source.Reader, table string, plan *partition.Plan) error {
	if plan == nil || len(plan.KeyColumns) == 0 {
		return nil
	}
	reader, ok := partitionSourceReader(r).(interface {
		PartitionHasChecks(context.Context, string) (bool, error)
	})
	if !ok {
		return fmt.Errorf("跨库 KEY 缺少源 CHECK 约束完整性核验能力")
	}
	hasChecks, err := reader.PartitionHasChecks(ctx, table)
	if err != nil {
		return fmt.Errorf("读取跨库 KEY 源 CHECK 约束: %w", err)
	}
	if hasChecks {
		return fmt.Errorf("跨库 KEY 表含尚未完整等价迁移的 CHECK 约束，不能删除约束或扩大原值域")
	}
	return nil
}

func (m *Migrator) migratePartitionData(ctx context.Context, table string) (bool, string) {
	s := m.partitionState
	t := s.tables[table]
	if err := m.partitionCopyReady(table); err != nil {
		return false, err.Error()
	}
	if e := m.recheckPartitionSource(ctx, table); e != nil {
		return false, e.Error()
	}
	inspector := m.writer.(target.PartitionInspector)
	actual, e := inspector.InspectPartitionTable(ctx, t.TargetTable)
	if e == nil {
		e = partition.Equivalent(t.Plan.Target, actual)
	}
	if e == nil {
		e = target.VerifyPartitionKeyColumns(ctx, m.writer, t.TargetTable, t.Plan)
	}
	if e != nil {
		return false, fmt.Sprintf("写入前分区核对失败: %v", e)
	}
	batch := m.cfg.PageSize
	if batch <= 0 {
		batch = 20000
	}
	reader, ok := partitionSourceReader(m.reader).(source.PartitionReader)
	if !ok {
		return false, "源库缺少分区固定游标读取能力"
	}
	e = reader.StreamPartitionRows(ctx, table, batch, func(cols, types []string, rows [][]interface{}) error {
		mapped := make([]string, len(cols))
		for i, c := range cols {
			mapped[i] = m.targetColumn(table, c)
		}
		return m.writer.CopyData(ctx, t.TargetTable, mapped, types, rows)
	})
	if e != nil {
		return false, e.Error()
	}
	t.dataExecuted = true
	return true, ""
}

func partitionRowCount(rows map[string]int64, schema, name string, ordinal int) (int64, bool) {
	keys := []string{schema + "." + name, name}
	if ordinal > 0 {
		keys = append(keys, fmt.Sprint(ordinal))
	}
	for _, key := range keys {
		if n, ok := rows[key]; ok {
			return n, true
		}
	}
	return 0, false
}

func (m *Migrator) finishPartitions(ctx context.Context, report *MigrationReport) {
	s := m.partitionState
	if s == nil || m.isDamengNative() {
		return
	}
	if !s.hasPartitions && len(s.blocked) == 0 && s.preflight.FatalError == "" {
		return
	}
	report.PartitionMigration = s.preflight
	if s.preflight.FatalError != "" {
		report.Tables.Total++
		report.Tables.Failed++
		report.Tables.Items = append(report.Tables.Items, ObjectResult{Name: "分区预检", Stage: "preflight", Error: s.preflight.FatalError})
		return
	}
	for _, t := range s.preflight.Tables {
		if s.blocked[t.SourceTable] {
			report.Tables.Total++
			report.Tables.Failed++
			report.Tables.Items = append(report.Tables.Items, ObjectResult{Name: t.SourceTable, Stage: "preflight", Error: t.Error})
			continue
		}
		if t.Plan == nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			t.Status = "incomplete"
			t.Error = err.Error()
			continue
		}
		// A previous target with matching partitions/counts is not evidence that
		// this run's CREATE or copy succeeded. Preserve the actual execution error.
		executionError := ""
		for _, category := range []CategoryReport{report.Tables, report.Data} {
			for _, item := range category.Items {
				if (item.Name == t.SourceTable || item.Name == t.TargetTable) && item.Error != "" {
					executionError = item.Error
					break
				}
			}
			if executionError != "" {
				break
			}
		}
		if executionError != "" {
			t.Status = "failed"
			t.Error = executionError
			continue
		}
		if !m.cfg.objectMode() && ((m.cfg.Content != "data_only" && !t.schemaExecuted) || (m.cfg.Content != "schema_only" && !t.dataExecuted)) {
			t.Status = "incomplete"
			t.Error = "本次表结构或数据执行未完成，不能以已有目标结构判为迁移成功"
			report.Tables.Failed++
			report.Tables.Items = append(report.Tables.Items, ObjectResult{Name: t.SourceTable, Stage: "execution", Error: t.Error})
			continue
		}
		inspector := m.writer.(target.PartitionInspector)
		e := m.recheckPartitionSource(ctx, t.SourceTable)
		if e == nil {
			var actual *source.PartitionSpec
			actual, e = inspector.InspectPartitionTable(ctx, t.TargetTable)
			if e == nil {
				e = partition.Equivalent(t.Plan.Target, actual)
			}
			if e == nil {
				e = target.VerifyPartitionKeyColumns(ctx, m.writer, t.TargetTable, t.Plan)
			}
		}
		if e == nil && m.cfg.Content != "schema_only" && !m.cfg.objectMode() {
			counter, ok := partitionSourceReader(m.reader).(interface {
				CountPartitionRows(context.Context, string, *source.PartitionSpec) (map[string]int64, error)
			})
			if !ok {
				e = fmt.Errorf("源库缺少逐分区计数能力")
			} else {
				t.SourceRows, e = counter.CountPartitionRows(ctx, t.SourceTable, t.Source)
			}
			if e == nil {
				t.TargetRows, e = inspector.CountPartitionRows(ctx, t.TargetTable, t.Plan.Target)
			}
			if e == nil {
				for _, mapping := range t.Plan.Mapping {
					a, aok := partitionRowCount(t.SourceRows, mapping.SourceSchema, mapping.SourceName, 0)
					b, bok := partitionRowCount(t.TargetRows, mapping.TargetSchema, mapping.TargetName, mapping.TargetOrdinal)
					base, _ := partitionRowCount(t.Baseline, mapping.TargetSchema, mapping.TargetName, mapping.TargetOrdinal)
					if !aok || !bok || a != b-base {
						e = fmt.Errorf("分区 %s 行数不一致或缺少目录证据: 源=%d 目标增量=%d", mapping.SourceName, a, b-base)
						break
					}
				}
			}
		}
		if e == nil {
			e = ctx.Err()
		}
		if e != nil {
			t.Status = "failed"
			if ctx.Err() != nil {
				t.Status = "incomplete"
			}
			t.Error = e.Error()
			report.Tables.Failed++
			report.Tables.Items = append(report.Tables.Items, ObjectResult{Name: t.SourceTable, Stage: "verification", Error: e.Error()})
		} else {
			t.Status = "succeeded"
			if m.cfg.Content != "schema_only" && !m.cfg.objectMode() {
				// Existing data_only tables append rows. The ordinary row-count
				// phase sees total target rows, while this proof compares only the
				// committed increment against the source's mapped physical leaves.
				var sourceTotal, targetIncrement int64
				for _, mapping := range t.Plan.Mapping {
					a, _ := partitionRowCount(t.SourceRows, mapping.SourceSchema, mapping.SourceName, 0)
					b, _ := partitionRowCount(t.TargetRows, mapping.TargetSchema, mapping.TargetName, mapping.TargetOrdinal)
					baseline, _ := partitionRowCount(t.Baseline, mapping.TargetSchema, mapping.TargetName, mapping.TargetOrdinal)
					sourceTotal += a
					targetIncrement += b - baseline
				}
				count := TableRowCount{Table: t.TargetTable, Src: sourceTotal, Dst: targetIncrement, Match: sourceTotal == targetIncrement}
				found := false
				for i := range report.RowCounts {
					if report.RowCounts[i].Table == t.TargetTable || report.RowCounts[i].Table == t.SourceTable {
						report.RowCounts[i] = count
						found = true
					}
				}
				if !found {
					report.RowCounts = append(report.RowCounts, count)
				}
			}
		}
	}
}
