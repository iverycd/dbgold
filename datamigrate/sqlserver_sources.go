package datamigrate

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/datamigrate/typemap"
	"dbgold/datamigrate/valueconv"
)

type sqlServerSourcePlan struct {
	versionError  error
	sequenceError error
	sequences     []sqlServerSequenceEntry
	bindings      map[[2]string][]int
	tables        map[string]sqlServerTablePlan
}

type sqlServerSequenceEntry struct {
	source.SequenceInfo
	identity bool
}

type sqlServerTablePlan struct {
	info     *source.TableDDLInfo
	warnings []valueconv.MigrationWarning
	err      error
}

func isOraclePGSQLServerSource(src string) bool {
	return src == "oracle" || src == "dameng" || source.IsPGFamily(src)
}

func (m *Migrator) isOraclePGSQLServer() bool {
	return m.isSQLServer() && isOraclePGSQLServerSource(m.reader.DBType())
}

func (m *Migrator) sqlServerSourceSchema() string {
	if scoped, ok := m.reader.(interface{ MigrationSchema() string }); ok {
		return scoped.MigrationSchema()
	}
	return ""
}

func (m *Migrator) prepareSQLServerSource(ctx context.Context) *sqlServerSourcePlan {
	if m.sqlServerSource != nil {
		return m.sqlServerSource
	}
	p := &sqlServerSourcePlan{
		bindings: map[[2]string][]int{},
		tables:   map[string]sqlServerTablePlan{},
	}
	m.sqlServerSource = p
	major, minor := 10, 50
	if inspector, ok := m.writer.(interface {
		SQLServerVersion(context.Context) (int, int, error)
	}); ok {
		major, minor, p.versionError = inspector.SQLServerVersion(ctx)
		if p.versionError == nil && (major < 10 || major == 10 && minor < 50) {
			p.versionError = fmt.Errorf("SQL Server 目标版本 %d.%d 低于 2008 R2", major, minor)
		}
	}
	if p.versionError != nil {
		return p
	}
	if len(m.cfg.ViewNames) > 0 {
		return p
	}
	var values []source.SequenceInfo
	if r, ok := m.reader.(interface {
		SQLServerSequences(context.Context, []string) ([]source.SequenceInfo, error)
	}); ok {
		values, p.sequenceError = r.SQLServerSequences(ctx, nil)
	} else {
		values, p.sequenceError = m.reader.GetSequences(ctx)
	}
	schema := m.sqlServerSourceSchema()
	for _, value := range values {
		entry := sqlServerSequenceEntry{SequenceInfo: value}
		native := value.Native
		if native != nil {
			// Work from the complete binding inventory. A shared sequence
			// must not be mistaken for a column-owned generator after filtering.
			identity := len(native.Bindings) == 1 && native.Bindings[0].Kind == "identity"
			if major < 11 && len(native.Bindings) == 1 {
				identity = identity || native.Bindings[0].Kind == "serial" || native.Bindings[0].Kind == "default"
			}
			entry.identity = identity
			if entry.Unsupported == "" {
				entry.Unsupported = sqlServerSequenceIssue(entry, schema, major)
			}
			for _, b := range native.Bindings {
				if b.Kind != "ownership" && (b.Schema == "" || b.Schema == schema) {
					key := [2]string{b.TableName, b.ColumnName}
					p.bindings[key] = append(p.bindings[key], len(p.sequences))
				}
			}
		}
		p.sequences = append(p.sequences, entry)
	}
	return p
}

func sqlServerSequenceIssue(entry sqlServerSequenceEntry, schema string, major int) string {
	s := entry.Native
	if s.Schema != "" && s.Schema != schema {
		return "序列位于源 Schema 之外，需要人工适配"
	}
	if _, ok := new(big.Int).SetString(entry.StartValueText, 10); !ok {
		return "无法确认源序列的安全续号位置"
	}
	if s.Ordered || s.Cycle {
		return "有序或循环序列需要人工适配"
	}
	for _, b := range s.Bindings {
		if b.Schema != "" && b.Schema != schema {
			return "序列绑定跨源 Schema，需要人工适配"
		}
		switch b.Kind {
		case "identity", "serial", "default", "ownership":
		default:
			return fmt.Sprintf("列 %s.%s 的序列绑定方式 %s 需要人工适配", b.TableName, b.ColumnName, b.Kind)
		}
		if b.OnlyWhenNull || strings.Contains(strings.ToUpper(b.Identity), "ON NULL") {
			return fmt.Sprintf("列 %s.%s 的显式 NULL 发号语义需要人工适配", b.TableName, b.ColumnName)
		}
	}
	if entry.identity {
		if s.IncrementBy != "1" && s.Bindings[0].Identity != "dameng_identity" {
			return "当前 IDENTITY 转换只支持步长为 1 的生成器"
		}
		return ""
	}
	if major < 11 {
		return "SQL Server 2008 R2 无法创建独立或共享序列，需要人工恢复序列及列默认值"
	}
	return ""
}

func (m *Migrator) sqlServerTable(ctx context.Context, raw *source.TableDDLInfo) (*source.TableDDLInfo, error) {
	if !m.isOraclePGSQLServer() || raw == nil {
		return raw, nil
	}
	p := m.prepareSQLServerSource(ctx)
	if p.versionError != nil {
		return nil, p.versionError
	}
	if isMigrationTaskError(p.sequenceError) {
		return nil, p.sequenceError
	}
	if prior, ok := p.tables[raw.TableName]; ok {
		info := cloneSourceTableDDLInfo(prior.info)
		if info != nil {
			info.Partition = source.ClonePartitionSpec(raw.Partition)
		}
		return info, prior.err
	}
	planned := sqlServerTablePlan{info: cloneSourceTableDDLInfo(raw)}
	defer func() { p.tables[raw.TableName] = planned }()
	var identities []*source.ColumnInfo
	for i := range planned.info.Columns {
		c := &planned.info.Columns[i]
		if c.DefaultOnNull && m.cfg.Content != "data_only" && !m.cfg.objectMode() {
			planned.err = fmt.Errorf("列 %s 的 DEFAULT ON NULL 需要人工适配", c.Name)
			return nil, planned.err
		}
		wasAuto := strings.Contains(c.Extra, "auto_increment") || c.Identity != "" && c.Identity != "0"
		c.Extra = strings.TrimSpace(strings.ReplaceAll(c.Extra, "auto_increment", ""))
		bindings := p.bindings[[2]string{raw.TableName, c.Name}]
		if len(bindings) == 0 {
			if wasAuto {
				planned.err = fmt.Errorf("列 %s 的源自增绑定无法确认", c.Name)
				return nil, planned.err
			}
			continue
		}
		c.SQLServerDeferredDefault = true
		if len(bindings) > 1 {
			for _, index := range bindings {
				p.sequences[index].Unsupported = fmt.Sprintf("列 %s.%s 依赖多个序列，需要人工适配", raw.TableName, c.Name)
			}
			continue
		}
		entry := &p.sequences[bindings[0]]
		if !entry.identity || entry.Unsupported != "" {
			continue
		}
		if c.IsNullable {
			entry.Unsupported = fmt.Sprintf("可空列 %s.%s 无法转换为 IDENTITY", raw.TableName, c.Name)
			continue
		}
		if typemap.IsSQLServerUnboundedDecimal(*c, m.reader.DBType()) {
			c.SQLServerPrecision, c.SQLServerScale = 38, 0
		}
		mapped, err := typemap.SQLServerTypeForSource(*c, m.reader.DBType())
		_, maximum := dialect.SQLServerIdentityBounds(mapped)
		maxIdentity, representable := new(big.Int).SetString(maximum, 10)
		if err != nil || !representable {
			entry.Unsupported = fmt.Sprintf("列 %s.%s 的类型无法转换为 IDENTITY", raw.TableName, c.Name)
			c.SQLServerPrecision, c.SQLServerScale = 0, 0
			continue
		}
		sourceMax, ok := new(big.Int).SetString(entry.Native.MaxValue, 10)
		if !ok || sourceMax.Cmp(maxIdentity) < 0 {
			entry.Unsupported = fmt.Sprintf("列 %s.%s 的序列上限无法由 IDENTITY 保留", raw.TableName, c.Name)
			c.SQLServerPrecision, c.SQLServerScale = 0, 0
			continue
		}
		if _, err = valueconv.ConvertSQLServerForSource(entry.StartValueText, *c, m.reader.DBType()); err != nil {
			entry.Unsupported = fmt.Sprintf("列 %s.%s 的续号位置无法表示: %v", raw.TableName, c.Name, err)
			c.SQLServerPrecision, c.SQLServerScale = 0, 0
			continue
		}
		identities = append(identities, c)
	}
	for _, c := range identities {
		entry := &p.sequences[p.bindings[[2]string{raw.TableName, c.Name}][0]]
		if len(identities) > 1 {
			entry.Unsupported = "SQL Server 每表仅支持一个 IDENTITY，需要人工恢复发号"
			c.SQLServerPrecision, c.SQLServerScale = 0, 0
			continue
		}
		c.Extra = strings.TrimSpace(c.Extra + " auto_increment")
		c.SQLServerIdentitySeed = entry.StartValueText
		c.SQLServerIdentityIncrement = entry.Native.IncrementBy
		planned.warnings = append(planned.warnings, valueconv.MigrationWarning{Table: raw.TableName, Column: c.Name, Reason: "自增已映射为 IDENTITY，应用显式写值时需在该会话开启 IDENTITY_INSERT", Count: 1})
	}
	warnings, err := planSQLServerDecimals(ctx, m.reader, m.writer, raw.TableName, m.objName(raw.TableName), m.objName, planned.info.Columns, m.cfg.Content == "data_only" || m.cfg.objectMode(), nil)
	planned.warnings = append(planned.warnings, warnings...)
	planned.err = err
	return cloneSourceTableDDLInfo(planned.info), err
}

func (m *Migrator) sqlServerPlannedSequences(ctx context.Context, selected func(string) bool) []source.SequenceInfo {
	var out []source.SequenceInfo
	schema := m.sqlServerSourceSchema()
	for _, entry := range m.prepareSQLServerSource(ctx).sequences {
		v := entry.SequenceInfo
		if v.Native == nil {
			if selected(v.TableName) {
				v.TableName, v.ColumnName = m.objName(v.TableName), m.objName(v.ColumnName)
				out = append(out, v)
			}
			continue
		}
		native := *v.Native
		native.SourceName = native.Name
		native.Name, native.Bindings = m.objName(native.Name), nil
		v.TableName, v.ColumnName = "", ""
		for _, b := range v.Native.Bindings {
			if b.Schema != "" && b.Schema != schema || !selected(b.TableName) {
				continue
			}
			b.SourceColumnName = b.ColumnName
			b.TableName, b.ColumnName = m.objName(b.TableName), m.objName(b.ColumnName)
			native.Bindings = append(native.Bindings, b)
		}
		if len(native.Bindings) == 0 {
			if len(v.Native.Bindings) != 0 || m.cfg.Content == "data_only" || m.cfg.Mode != "" && m.cfg.Mode != "all" || len(m.cfg.TableNames) > 0 {
				continue
			}
		} else if len(native.Bindings) < len(v.Native.Bindings) && v.Unsupported == "" {
			v.Unsupported = "共享序列还有未选中的绑定，需要纳入全部关联表后迁移"
		}
		v.Native = &native
		v.SQLServerCalibrationOnly = m.cfg.Content == "data_only"
		if entry.identity && v.Unsupported == "" && len(native.Bindings) == 1 {
			v.TableName, v.ColumnName = native.Bindings[0].TableName, native.Bindings[0].ColumnName
			v.Native = nil
		}
		if m.sqlServerObjectExcluded("sequence", entry.TableName, native.SourceName) {
			continue
		}
		out = append(out, v)
	}
	return out
}

func (m *Migrator) checkSQLServerSequenceNames(ctx context.Context, values []source.SequenceInfo) error {
	inspector, ok := m.writer.(sqlServerInspector)
	if !ok {
		return nil
	}
	var names []string
	for _, v := range values {
		if v.Native != nil && v.Unsupported == "" {
			names = append(names, v.Native.Name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	err := inspector.CheckNames(ctx, names)
	if err == nil {
		return nil
	}
	var issue *target.SQLServerNameError
	if !errors.As(err, &issue) {
		return err
	}
	for i := range values {
		v := &values[i]
		if v.Native != nil && v.Unsupported == "" && slices.Contains(issue.Names, v.Native.Name) {
			v.Unsupported = err.Error()
		}
	}
	return nil
}

func (m *Migrator) sqlServerSequenceObjectName(v source.SequenceInfo) string {
	if m.isOraclePGSQLServer() && v.Unsupported != "" && v.Native != nil && v.Native.SourceName != "" {
		return v.Native.SourceName
	}
	return dialect.SequenceName(m.writer.Dialect(), v)
}

func (m *Migrator) scopeSQLServerForeignKey(f source.FKInfo) source.FKInfo {
	if m.isOraclePGSQLServer() && f.RefSchema != "" && f.RefSchema != m.sqlServerSourceSchema() {
		f.Unsupported = fmt.Sprintf("外键引用源 Schema %s 之外的表，需要人工适配", f.RefSchema)
	}
	return f
}

func (m *Migrator) reportSQLServerTriggers(ctx context.Context, category *CategoryReport, tables []string) {
	reader, ok := m.reader.(source.TriggerReader)
	if !ok {
		return
	}
	triggers, err := reader.GetTriggers(ctx)
	if err != nil {
		if !m.stopMigrationTask(ctx, err) {
			recordMetadataFailure(category, err)
		}
		return
	}
	for _, t := range triggers {
		if !slices.Contains(tables, t.TableName) || slices.Contains(m.cfg.ExcludedObjects, ObjectRef{Kind: "trigger", Table: t.TableName, Name: t.Name}) {
			continue
		}
		reason := "触发器需要在 SQL Server 中手工适配"
		if t.MetadataError != "" {
			reason += "；读取源定义失败: " + t.MetadataError
		}
		sqlServerResult(category, t.Name, "", fmt.Errorf("%s", reason))
		item := &category.Items[len(category.Items)-1]
		item.Table, item.SourceName, item.Stage, item.SourceDDL = t.TableName, t.Name, "unsupported", t.SourceDDL
		if item.SourceDDL == "" {
			item.SourceDDL = t.Body
		}
	}
}

func (m *Migrator) sqlServerUnsupportedConstraints(ctx context.Context) ([]source.CheckConstraintInfo, error) {
	reader, ok := m.reader.(interface {
		SQLServerUnsupportedConstraints(context.Context) ([]source.CheckConstraintInfo, error)
	})
	if !ok {
		return nil, nil
	}
	return reader.SQLServerUnsupportedConstraints(ctx)
}

func (m *Migrator) reportSQLServerUnsupportedConstraints(ctx context.Context, category *CategoryReport, eligible func(string) bool) {
	checks, err := m.sqlServerUnsupportedConstraints(ctx)
	if err != nil {
		if !m.stopMigrationTask(ctx, err) {
			recordMetadataFailure(category, err)
		}
		return
	}
	for _, check := range checks {
		if !eligible(check.TableName) || m.sqlServerObjectExcluded("check", check.TableName, check.Name) {
			continue
		}
		sqlServerResult(category, check.Name, "", fmt.Errorf("%s", check.Unsupported))
		item := &category.Items[len(category.Items)-1]
		item.Table, item.SourceName, item.Stage, item.SourceDDL = check.TableName, check.Name, "unsupported", check.Definition
	}
}

func (m *Migrator) sqlServerObjectExcluded(kind, table, name string) bool {
	return m.isOraclePGSQLServer() && slices.ContainsFunc(m.cfg.ExcludedObjects, func(ref ObjectRef) bool {
		return ref.Kind == kind && ref.Name == name && (ref.Table == "" || ref.Table == table)
	})
}

func (m *Migrator) runSQLServerSequences(ctx context.Context, report *MigrationReport, values []source.SequenceInfo) {
	d := m.writer.Dialect()
	for _, v := range values {
		if ctx.Err() != nil {
			return
		}
		var dep string
		if m.migrationTargetTableError(v.TableName) != nil {
			dep = v.TableName
		}
		if v.Native != nil {
			for _, b := range v.Native.Bindings {
				if m.migrationTargetTableError(b.TableName) != nil {
					dep = b.TableName
					break
				}
			}
		}
		var ss []dialect.Statement
		if v.Unsupported == "" {
			ss = d.SequenceStatements(m.cfg.TargetSchema, v, "")
		}
		name := m.sqlServerSequenceObjectName(v)
		var stage string
		var err error
		switch {
		case v.Unsupported != "":
			stage = "unsupported"
			err = m.repairSequence(ctx, v, "")
		case dep != "":
			stage = "dependency"
			err = m.captureSequenceCall(ctx, v, ss, func(context.Context) error {
				return sqlServerDDLNotExecuted{fmt.Errorf("依赖表 %s 迁移失败，未创建或校准序列", dep)}
			})
		case m.cfg.Content == "data_only":
			stage = "execute"
			err = m.captureSequenceCall(ctx, v, ss, func(c context.Context) error {
				return target.CalibrateRecovery(c, m.writer, v.TableName, []source.SequenceInfo{v})
			})
		default:
			stage = "execute"
			err = m.repairSequence(ctx, v, "")
		}
		if m.stopMigrationTask(ctx, err) {
			return
		}
		sqlServerResult(&report.Sequences, name, dialect.JoinSQL(ss), err)
		if err != nil {
			item := &report.Sequences.Items[len(report.Sequences.Items)-1]
			item.Stage, item.Table = stage, v.TableName
		}
	}
}

func sqlServerRecoverySequences(ctx context.Context, r source.Reader, w target.Writer, cfg Config, preview repair.RecoveryPreview) ([]source.SequenceInfo, error) {
	cfg.TargetDBType = "sqlserver"
	cfg.Content = "data_only"
	m := &Migrator{reader: r, writer: w, cfg: cfg}
	p := m.prepareSQLServerSource(ctx)
	if p.versionError != nil {
		return nil, p.versionError
	}
	if p.sequenceError != nil {
		return nil, p.sequenceError
	}
	values := m.sqlServerPlannedSequences(ctx, func(string) bool { return true })
	targetName := m.objName(preview.SourceTable)
	var out []source.SequenceInfo
	for _, s := range values {
		if s.Unsupported != "" || !nativeSequenceDependsOn(s, targetName) {
			continue
		}
		if s.Native == nil {
			for _, c := range preview.SourceColumns {
				if m.objName(c.Name) == s.ColumnName && c.SQLServerIdentitySeed != "" {
					s.TableName, s.ColumnName = preview.TargetTable, preview.ColumnMapping[c.Name]
					out = append(out, s)
					break
				}
			}
			continue
		}
		native := *s.Native
		native.Bindings = slices.Clone(native.Bindings)
		for i := range native.Bindings {
			b := &native.Bindings[i]
			if b.TableName == targetName {
				b.TableName, b.ColumnName = preview.TargetTable, preview.ColumnMapping[b.SourceColumnName]
				if b.ColumnName == "" {
					return nil, fmt.Errorf("序列 %s 的源列 %s 缺少冻结映射", native.Name, b.SourceColumnName)
				}
			}
		}
		s.Native = &native
		out = append(out, s)
	}
	return out, nil
}

func planSQLServerDecimals(ctx context.Context, r source.Reader, w target.Writer, sourceTable, targetTable string, columnName func(string) string, columns []source.ColumnInfo, existing bool, frozen []source.ColumnInfo) ([]valueconv.MigrationWarning, error) {
	type observed struct {
		integer, fraction int
		found             bool
	}
	ranges := map[string]observed{}
	var names []string
	collect := func(name string, v interface{}) error {
		if v == nil {
			return nil
		}
		integer, fraction, err := valueconv.SQLServerDecimalDigits(v)
		if err != nil {
			return fmt.Errorf("表 %s 的列 %s: %w", sourceTable, name, err)
		}
		o := ranges[name]
		o.found = true
		o.integer, o.fraction = max(o.integer, integer), max(o.fraction, fraction)
		ranges[name] = o
		return nil
	}
	for _, c := range columns {
		if !typemap.IsSQLServerUnboundedDecimal(c, r.DBType()) {
			continue
		}
		names = append(names, c.Name)
		if existing || c.SQLServerDeferredDefault {
			continue
		}
		v, present, err := dialect.SQLServerDefaultDecimalLiteral(c, r.DBType())
		if err != nil {
			return nil, err
		}
		if present {
			if err = collect(c.Name, v); err != nil {
				return nil, err
			}
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	reader, ok := r.(interface {
		SQLServerDecimalProfile(context.Context, string, []string, func(string, interface{}) error) error
	})
	if !ok {
		return nil, fmt.Errorf("源表 %s 无法按精确数值读取未声明精度的列", sourceTable)
	}
	if err := reader.SQLServerDecimalProfile(ctx, sourceTable, names, collect); err != nil {
		return nil, err
	}
	var warnings []valueconv.MigrationWarning
	for i := range columns {
		c := &columns[i]
		if !slices.Contains(names, c.Name) {
			continue
		}
		o := ranges[c.Name]
		precision, scale := c.SQLServerPrecision, c.SQLServerScale
		if existing {
			inspector, ok := w.(interface {
				SQLServerExistingDecimal(context.Context, string, string) (int64, int64, error)
			})
			if !ok {
				return nil, fmt.Errorf("目标表 %s 无法确认现有 DECIMAL 精度", targetTable)
			}
			var err error
			precision, scale, err = inspector.SQLServerExistingDecimal(ctx, targetTable, columnName(c.Name))
			if err != nil {
				return nil, err
			}
			if i < len(frozen) && frozen[i].SQLServerPrecision != 0 && (precision != frozen[i].SQLServerPrecision || scale != frozen[i].SQLServerScale) {
				return nil, fmt.Errorf("目标列 %s.%s 的 DECIMAL 精度已改变", targetTable, columnName(c.Name))
			}
		} else if precision == 0 {
			precision, scale = 38, int64(o.fraction)
			if !o.found {
				scale = 18
			}
		}
		if precision < 1 || precision > 38 || scale < 0 || scale > precision || int64(o.integer) > precision-scale || int64(o.fraction) > scale {
			return nil, fmt.Errorf("表 %s 列 %s 需要 %d 位整数和 %d 位小数，无法无损写入 DECIMAL(%d,%d)", sourceTable, c.Name, o.integer, o.fraction, precision, scale)
		}
		c.SQLServerPrecision, c.SQLServerScale = precision, scale
		warnings = append(warnings, valueconv.MigrationWarning{Table: sourceTable, Column: c.Name, Reason: fmt.Sprintf("未声明精度的源数值使用 DECIMAL(%d,%d)，后续最多 %d 位整数和 %d 位小数", precision, scale, precision-scale, scale), Count: 1})
	}
	return warnings, nil
}
