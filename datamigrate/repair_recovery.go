package datamigrate

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
)

// Dependencies are explicit so recovery ordering and its destructive boundary can
// be tested without a database. Production always uses target's dialect adapters.
type recoveryOps struct {
	schema    func(context.Context, target.Writer) (string, error)
	inspect   func(context.Context, target.Writer, string) ([]source.ColumnInfo, error)
	prepare   func(context.Context, target.Writer, string, []source.ColumnInfo) error
	truncate  func(context.Context, target.Writer, string) error
	calibrate func(context.Context, target.Writer, string, []source.SequenceInfo) error
}

func defaultRecoveryOps() recoveryOps {
	return recoveryOps{target.ResolveRepairSchema, target.InspectRecovery, target.PrepareRecovery, target.TruncateRecovery, target.CalibrateRecovery}
}

// PreviewRecovery is read-only. It validates everything needed before a destructive
// empty-and-reload operation and binds the exact metadata/counts to its fingerprint.
func PreviewRecovery(ctx context.Context, r source.Reader, w target.Writer, cfg Config, obj repair.Object, related []repair.Object, mapping map[string]string) (repair.RecoveryPreview, error) {
	return previewRecovery(ctx, r, w, cfg, obj, related, mapping, defaultRecoveryOps())
}
func previewRecovery(ctx context.Context, r source.Reader, w target.Writer, cfg Config, obj repair.Object, related []repair.Object, mapping map[string]string, ops recoveryOps) (repair.RecoveryPreview, error) {
	p := repair.RecoveryPreview{ObjectID: obj.ID, SourceTable: obj.SourceTable, TargetTable: obj.Table, Warnings: []string{"补迁使用当前源数据；执行期间源表和目标表必须停止写入。"}, Related: []repair.Object{}}
	if obj.Kind != "table" && obj.Kind != "data" {
		return p, fmt.Errorf("仅表和数据对象可以接续数据")
	}
	if p.SourceTable == "" {
		p.SourceTable = obj.SourceName
	}
	if p.SourceTable == "" {
		return p, fmt.Errorf("缺少原源表名称，请补齐历史任务配置")
	}
	if p.TargetTable == "" {
		p.TargetTable = obj.TargetName
	}
	schema, err := ops.schema(ctx, w)
	if err != nil {
		return p, err
	}
	if cfg.TargetSchema != "" && schema != cfg.TargetSchema {
		return p, fmt.Errorf("目标有效 schema 已改变")
	}
	p.QualifiedTarget = w.Dialect().QualifyTable(schema, p.TargetTable)
	// The generic SQL Server reader exposes names without schemas. Reject a
	// duplicate name rather than selecting a different source relation by accident.
	if r.DBType() == "sqlserver" {
		tables, e := r.ListTables(ctx)
		if e != nil {
			return p, fmt.Errorf("核验源表身份: %w", e)
		}
		matches := 0
		for _, name := range tables {
			if name == p.SourceTable {
				matches++
			}
		}
		if matches != 1 {
			return p, fmt.Errorf("源表 %s 的身份不唯一或已不存在，无法安全补迁", p.SourceTable)
		}
	}
	info, err := r.GetTableDDLInfo(ctx, p.SourceTable)
	if err != nil {
		return p, fmt.Errorf("读取源表结构: %w", err)
	}
	if info == nil || len(info.Columns) == 0 {
		return p, fmt.Errorf("源表无可用列元数据")
	}
	p.SourceColumns = slices.Clone(info.Columns)
	if len(obj.SourceColumns) > 0 && repair.Hash(obj.SourceColumns) != repair.Hash(info.Columns) {
		return p, fmt.Errorf("源表结构已偏离迁移快照，请重新核对并创建迁移任务")
	}
	p.TargetColumns, err = ops.inspect(ctx, w, p.TargetTable)
	if err != nil {
		return p, fmt.Errorf("读取目标表结构: %w", err)
	}
	p.ColumnMapping = map[string]string{}
	for k, v := range obj.ColumnMapping {
		p.ColumnMapping[k] = v
	}
	for k, v := range mapping {
		p.ColumnMapping[k] = v
	}
	for _, c := range info.Columns {
		if p.ColumnMapping[c.Name] == "" {
			p.ColumnMapping[c.Name] = recoveryName(cfg, w.DBType(), r.DBType(), c.Name)
		}
	}
	if err = validateRecoveryColumns(r.DBType(), w, cfg, p.SourceColumns, p.TargetColumns, p.ColumnMapping); err != nil {
		return p, err
	}
	p.SourceRows, err = r.CountRows(ctx, p.SourceTable)
	if err != nil {
		return p, fmt.Errorf("读取源表行数: %w", err)
	}
	p.TargetRows, err = w.CountRows(ctx, p.TargetTable)
	if err != nil {
		return p, fmt.Errorf("读取目标表行数: %w", err)
	}
	p.DataRequired = cfg.Content != "schema_only" && !cfg.objectMode() && len(cfg.ViewNames) == 0 && obj.DataStatus != "succeeded"
	if !p.DataRequired && cfg.Content != "schema_only" && !cfg.objectMode() && len(cfg.ViewNames) == 0 && obj.DataStatus == "succeeded" && p.SourceRows != p.TargetRows {
		return p, fmt.Errorf("已完成的数据行数发生变化，请核对后另行恢复数据")
	}
	for _, o := range related {
		if o.Status == "succeeded" {
			continue
		}
		candidate := regenerateRepairCandidate(w, cfg, obj, o, p.ColumnMapping)
		p.Related = append(p.Related, candidate)
	}
	slices.SortStableFunc(p.Related, func(a, b repair.Object) int { return repairKindOrder(a.Kind) - repairKindOrder(b.Kind) })
	p.Fingerprint = recoveryFingerprint(cfg, p)
	return p, nil
}
func recoveryName(cfg Config, dst, src, s string) string {
	if dst == "oracle" || dst == "oscar" || dst == "dameng" && src == "sqlserver" {
		return strings.ToUpper(s)
	}
	if cfg.LowerCaseNames {
		return strings.ToLower(s)
	}
	return s
}
func recoveryFingerprint(cfg Config, p repair.RecoveryPreview) string {
	p.Fingerprint = ""
	cfg.RepairSink = nil
	return repair.Hash(struct {
		Config  Config
		Preview repair.RecoveryPreview
	}{cfg, p})
}
func repairKindOrder(k string) int {
	switch k {
	case "table":
		return 0
	case "primary_key":
		return 1
	case "index", "check", "constraint":
		return 2
	case "sequence":
		return 3
	case "foreign_key":
		return 4
	case "view":
		return 5
	case "comment":
		return 6
	}
	return 7
}

func validateRecoveryColumns(src string, w target.Writer, cfg Config, sourceCols, targetCols []source.ColumnInfo, mapping map[string]string) error {
	actual := map[string]source.ColumnInfo{}
	for _, c := range targetCols {
		actual[c.Name] = c
	}
	used := map[string]bool{}
	sourceNames := map[string]bool{}
	for _, c := range sourceCols {
		sourceNames[c.Name] = true
		n := mapping[c.Name]
		if n == "" {
			return fmt.Errorf("源列 %s 缺少目标映射", c.Name)
		}
		if used[n] {
			return fmt.Errorf("多个源列映射到目标列 %s", n)
		}
		used[n] = true
		dst, ok := actual[n]
		if !ok {
			return fmt.Errorf("目标列不存在: %s → %s", c.Name, n)
		}
		if dst.Generated != "" && !strings.EqualFold(dst.Generated, "NEVER") {
			return fmt.Errorf("目标列 %s 为生成列，不能写入", n)
		}
		expected := w.Dialect().MapType(c, src, dialect.TypeOpt{CharInLength: cfg.CharInLength, UseNvarchar2: cfg.UseNvarchar2})
		if !compatibleRecoveryType(expected, dst) {
			return fmt.Errorf("列 %s → %s 的目标类型 %s 不兼容原映射 %s；不支持缩窄或跨类型转换", c.Name, n, dst.DataType, expected)
		}
		if c.IsNullable && !dst.IsNullable {
			return fmt.Errorf("列 %s → %s 收紧了可空性，请先核对数据及列定义", c.Name, n)
		}
	}
	for k := range mapping {
		if !sourceNames[k] {
			return fmt.Errorf("列映射包含不存在的源列 %s", k)
		}
	}
	for _, c := range targetCols {
		if used[c.Name] || c.IsNullable || c.Default != nil || c.Identity != "" || c.Extra == "auto_increment" || c.Generated != "" {
			continue
		}
		return fmt.Errorf("未映射的目标必填列 %s 没有默认值", c.Name)
	}
	return nil
}

var recoveryTypeParams = regexp.MustCompile(`\(\s*(-?\d+)(?:\s*,\s*(\d+))?\s*(?:CHAR|BYTE)?\s*\)`)
var recoveryTemporalType = regexp.MustCompile(`^(TIMESTAMP|TIME)(\([^)]*\))? (WITH|WITHOUT) TIME ZONE$`)

func normalizedRecoveryType(t string) string {
	t = strings.ToUpper(strings.TrimSpace(t))
	t = strings.Join(strings.Fields(t), " ")
	if parts := recoveryTemporalType.FindStringSubmatch(t); len(parts) > 0 {
		base := parts[1]
		if parts[3] == "WITH" {
			base += "TZ"
		}
		return base + parts[2]
	}
	aliases := map[string]string{"CHARACTER VARYING": "VARCHAR", "CHARACTER": "CHAR", "INT4": "INTEGER", "INT": "INTEGER", "INT8": "BIGINT", "INT2": "SMALLINT", "DEC": "DECIMAL", "NUMERIC": "DECIMAL", "DOUBLE PRECISION": "DOUBLE", "BOOL": "BOOLEAN", "TIMESTAMP WITHOUT TIME ZONE": "TIMESTAMP", "TIMESTAMP WITH TIME ZONE": "TIMESTAMPTZ", "TIME WITHOUT TIME ZONE": "TIME", "TIME WITH TIME ZONE": "TIMETZ"}
	for from, to := range aliases {
		if t == from {
			return to
		}
		if strings.HasPrefix(t, from+"(") {
			return to + t[len(from):]
		}
	}
	return t
}
func compatibleRecoveryType(expected string, actual source.ColumnInfo) bool {
	e := normalizedRecoveryType(expected)
	a := normalizedRecoveryType(actual.DataType)
	eb := strings.TrimSpace(recoveryTypeParams.ReplaceAllString(e, ""))
	ab := strings.TrimSpace(recoveryTypeParams.ReplaceAllString(a, ""))
	if eb == "" || ab == "" {
		return false
	}
	if eb != ab {
		// Integer widening preserves the same writer conversion and signedness.
		width := map[string]int{"TINYINT": 8, "SMALLINT": 16, "MEDIUMINT": 24, "INTEGER": 32, "BIGINT": 64}
		es, as := strings.TrimSuffix(eb, " UNSIGNED"), strings.TrimSuffix(ab, " UNSIGNED")
		if width[es] > 0 && width[as] >= width[es] && strings.HasSuffix(eb, " UNSIGNED") == strings.HasSuffix(ab, " UNSIGNED") {
			return true
		}
		if eb == ab+"(MAX)" {
			return actual.Length < 0
		}
		return false
	}
	match := recoveryTypeParams.FindStringSubmatch(e)
	if len(match) == 0 {
		return true
	}
	size, _ := strconv.ParseInt(match[1], 10, 64)
	scale := int64(0)
	if len(match) > 2 {
		scale, _ = strconv.ParseInt(match[2], 10, 64)
	}
	am := recoveryTypeParams.FindStringSubmatch(a)
	actualSize := actual.Length
	actualScale := actual.Scale
	if eb == "DECIMAL" || eb == "NUMBER" {
		actualSize = actual.Precision
	}
	if len(am) > 0 {
		actualSize, _ = strconv.ParseInt(am[1], 10, 64)
		if len(am) > 2 && am[2] != "" {
			actualScale, _ = strconv.ParseInt(am[2], 10, 64)
		}
	}
	if size < 0 {
		return actualSize < 0
	}
	if actualSize < 0 {
		return true
	}
	if eb == "DECIMAL" || eb == "NUMBER" {
		return actualSize > 0 && actualScale >= scale && actualSize-actualScale >= size-scale
	}
	if eb == "TIMESTAMP" || eb == "TIME" || eb == "TIMESTAMPTZ" || eb == "TIMETZ" || eb == "DATETIME" || eb == "DATETIME2" {
		return actual.DateTimePrecision >= size || actualSize >= size
	}
	return actualSize >= size
}

// RunRecovery never appends after a failed run. Recheck the preview before TRUNCATE;
// retain committed pages and stop immediately on the first uncertain/failed page.
func RunRecovery(ctx context.Context, r source.Reader, w target.Writer, cfg Config, p repair.RecoveryPreview, truncate bool, progress func(repair.RecoveryResult) error) (res repair.RecoveryResult, err error) {
	return runRecovery(ctx, r, w, cfg, p, truncate, progress, defaultRecoveryOps())
}
func runRecovery(ctx context.Context, r source.Reader, w target.Writer, cfg Config, p repair.RecoveryPreview, truncate bool, progress func(repair.RecoveryResult) error, ops recoveryOps) (res repair.RecoveryResult, err error) {
	res = repair.RecoveryResult{Status: "running", DataStatus: "running", SourceRows: p.SourceRows, TargetRows: p.TargetRows}
	finish := func(e error) (repair.RecoveryResult, error) {
		if e != nil {
			if res.Status != "unknown" {
				res.Status = "failed"
			}
			res.Error = e.Error()
			if ctx.Err() != nil {
				res.Status = "unknown"
			}
		} else {
			res.Status = "succeeded"
		}
		if progress != nil {
			if pe := progress(res); pe != nil {
				res.Status = "unknown"
				res.Error = pe.Error()
				return res, pe
			}
		}
		return res, e
	}
	if p.Fingerprint != recoveryFingerprint(cfg, p) {
		return finish(fmt.Errorf("接续预览签名不匹配"))
	}
	obj := repair.Object{ID: p.ObjectID, Kind: "table", SourceName: p.SourceTable, SourceTable: p.SourceTable, Table: p.TargetTable, TargetName: p.TargetTable, SourceColumns: p.SourceColumns, ColumnMapping: p.ColumnMapping}
	if !p.DataRequired {
		obj.DataStatus = "succeeded"
		if cfg.Content == "schema_only" || cfg.objectMode() || len(cfg.ViewNames) > 0 {
			obj.DataStatus = "not_required"
		}
	}
	fresh, e := previewRecovery(ctx, r, w, cfg, obj, p.Related, p.ColumnMapping, ops)
	if e != nil {
		return finish(e)
	}
	if fresh.SourceRows != p.SourceRows || fresh.TargetRows != p.TargetRows || repair.Hash(fresh.SourceColumns) != repair.Hash(p.SourceColumns) || repair.Hash(fresh.TargetColumns) != repair.Hash(p.TargetColumns) {
		return finish(fmt.Errorf("源／目标结构或行数已变化，请重新预览并确认"))
	}
	if !p.DataRequired {
		res.DataStatus = obj.DataStatus
		return finish(nil)
	}
	if p.TargetRows > 0 && !truncate {
		return finish(fmt.Errorf("目标表非空，必须单独确认清空本表后完整重导"))
	}
	if setter, ok := w.(sourceTypeSetter); ok {
		setter.SetSourceType(r.DBType())
	}
	mapped := slices.Clone(p.SourceColumns)
	for i := range mapped {
		mapped[i].Name = p.ColumnMapping[mapped[i].Name]
	}
	if e = ops.prepare(ctx, w, p.TargetTable, mapped); e != nil {
		return finish(e)
	}
	// Read a page and the paging key before clearing anything. Conversion errors on
	// later pages still stop; a preview is not represented as a full-data guarantee.
	pks, e := r.GetPrimaryKey(ctx, p.SourceTable)
	if e != nil {
		return finish(e)
	}
	pageSize := int64(cfg.PageSize)
	if pageSize <= 0 || pageSize > 20000 {
		pageSize = 2000
	}
	cols, types, first, e := r.ReadPage(ctx, p.SourceTable, pks, 0, pageSize)
	if e != nil {
		return finish(e)
	}
	if e = checkRecoveryPageColumns(p, cols); e != nil {
		return finish(e)
	}
	if p.SourceRows > 0 && len(first) == 0 {
		return finish(fmt.Errorf("源行数与首批读取不一致，请重新预检"))
	}
	if source.IsPGFamily(r.DBType()) && w.DBType() == "mysql" {
		first, e = convertPGPage(source.PGTable{Info: source.TableDDLInfo{TableName: p.SourceTable, Columns: p.SourceColumns}}, cols, first)
		if e != nil {
			return finish(e)
		}
	}
	if progress != nil {
		if e = progress(res); e != nil {
			return finish(e)
		}
	}
	if truncate {
		if e = ops.truncate(ctx, w, p.TargetTable); e != nil {
			return finish(e)
		}
		res.TargetRows = 0
	}
	write := func(cols, types []string, rows [][]any) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		if len(rows) == 0 {
			return nil
		}
		dst := make([]string, len(cols))
		for i, c := range cols {
			dst[i] = p.ColumnMapping[c]
		}
		if e := w.CopyData(ctx, p.TargetTable, dst, types, rows); e != nil {
			res.Status = "unknown"
			return fmt.Errorf("写入失败，已确认提交 %d 行；禁止追加重试: %w", res.CommittedRows, e)
		}
		res.CommittedRows += int64(len(rows))
		if progress != nil {
			return progress(res)
		}
		return nil
	}
	if e = write(cols, types, first); e != nil {
		return finish(e)
	}
	if int64(len(first)) == pageSize {
		for offset := pageSize; ; offset += pageSize {
			if e = ctx.Err(); e != nil {
				return finish(e)
			}
			cols, types, rows, e := r.ReadPage(ctx, p.SourceTable, pks, offset, pageSize)
			if e != nil {
				return finish(e)
			}
			if e = checkRecoveryPageColumns(p, cols); e != nil {
				return finish(e)
			}
			if source.IsPGFamily(r.DBType()) && w.DBType() == "mysql" {
				rows, e = convertPGPage(source.PGTable{Info: source.TableDDLInfo{TableName: p.SourceTable, Columns: p.SourceColumns}}, cols, rows)
				if e != nil {
					return finish(e)
				}
			}
			if e = write(cols, types, rows); e != nil {
				return finish(e)
			}
			if int64(len(rows)) < pageSize {
				break
			}
		}
	}
	res.TargetRows, e = w.CountRows(ctx, p.TargetTable)
	if e != nil {
		return finish(e)
	}
	after, e := r.CountRows(ctx, p.SourceTable)
	if e != nil {
		return finish(e)
	}
	if after != p.SourceRows || res.CommittedRows != p.SourceRows || res.TargetRows != res.CommittedRows {
		return finish(fmt.Errorf("行数核验失败：预览源=%d 当前源=%d 已提交=%d 目标=%d", p.SourceRows, after, res.CommittedRows, res.TargetRows))
	}
	res.DataStatus = "succeeded"
	sequences, e := RecoverySequences(ctx, r, w, cfg, p)
	if e != nil {
		return finish(e)
	}
	// Selected sequence objects are created later in the confirmed dependency
	// list. Calibrating their missing binding here would prevent that continuation.
	ready := sequences[:0]
	for _, seq := range sequences {
		deferred := false
		for _, related := range p.Related {
			if related.Kind == "sequence" && related.Status != "succeeded" && related.Sequence != nil && related.Sequence.TableName == p.TargetTable && related.Sequence.ColumnName == seq.ColumnName {
				deferred = true
			}
		}
		if !deferred {
			ready = append(ready, seq)
		}
	}
	if e = ops.calibrate(ctx, w, p.TargetTable, ready); e != nil {
		return finish(fmt.Errorf("数据已导入并核验，自增校准失败: %w", e))
	}
	return finish(nil)
}
func RecoverySequences(ctx context.Context, r source.Reader, w target.Writer, cfg Config, p repair.RecoveryPreview) ([]source.SequenceInfo, error) {
	values, err := r.GetSequences(ctx)
	if err != nil {
		return nil, err
	}
	out := []source.SequenceInfo{}
	for _, v := range values {
		if v.TableName != p.SourceTable {
			continue
		}
		v.TableName = p.TargetTable
		name := p.ColumnMapping[v.ColumnName]
		if name == "" {
			return nil, fmt.Errorf("自增列 %s 无目标映射", v.ColumnName)
		}
		v.ColumnName = name
		out = append(out, v)
	}
	return out, nil
}
func checkRecoveryPageColumns(p repair.RecoveryPreview, cols []string) error {
	if len(cols) != len(p.SourceColumns) {
		return fmt.Errorf("源分页列数与预览不同")
	}
	for i, c := range p.SourceColumns {
		if c.Name != cols[i] {
			return fmt.Errorf("源分页列顺序与预览不同")
		}
	}
	return nil
}

func regenerateRepairCandidate(w target.Writer, cfg Config, table, o repair.Object, mapping map[string]string) repair.Object {
	old := o.ColumnMapping
	referenceOnly := o.ForeignKey != nil && o.Table != table.Table && o.ForeignKey.RefTable == table.Table
	if referenceOnly {
		old = o.RefColumnMapping
	}
	if len(old) == 0 {
		old = table.ColumnMapping
	}
	if repair.Hash(old) == repair.Hash(mapping) {
		return o
	}
	renames := map[string]string{}
	for sourceName, newName := range mapping {
		oldName := old[sourceName]
		if oldName == "" {
			oldName = sourceName
		}
		if oldName != newName {
			renames[oldName] = newName
		}
	}
	if len(renames) == 0 {
		return o
	}
	affected := o.Table == table.Table || o.ForeignKey != nil && o.ForeignKey.RefTable == table.Table || o.Kind == "view"
	if !affected {
		return o
	}
	o.RequiresReview = true
	rename := func(s string) string {
		if n, ok := renames[s]; ok {
			return n
		}
		return s
	}
	names := func(ss []string) []string {
		ss = slices.Clone(ss)
		for i, s := range ss {
			ss[i] = rename(s)
		}
		return ss
	}
	var ss []dialect.Statement
	switch {
	case o.Index != nil:
		v := *o.Index
		if v.Definition != "" {
			return o
		}
		v.Columns = names(v.Columns)
		o.Index = &v
		ss = w.Dialect().IndexStatements(o.Schema, v)
	case o.ForeignKey != nil:
		v := *o.ForeignKey
		if v.TableName == table.Table {
			v.Columns = names(v.Columns)
		}
		if v.RefTable == table.Table {
			v.RefColumns = names(v.RefColumns)
		}
		o.ForeignKey = &v
		ss = w.Dialect().ForeignKeyStatements(o.Schema, v)
	case o.Sequence != nil:
		v := *o.Sequence
		v.ColumnName = rename(v.ColumnName)
		o.Sequence = &v
		owner := ""
		if cfg.ChangeOwner {
			owner = cfg.TargetSchema
		}
		ss = w.Dialect().SequenceStatements(o.Schema, v, owner)
	case o.Comment != nil:
		v := *o.Comment
		v.ColumnName = rename(v.ColumnName)
		o.Comment = &v
		ss = w.Dialect().CommentStatements(o.Schema, v)
	default:
		return o
	}
	o.SuggestedDDL = dialect.JoinSQL(ss)
	custom := false
	for _, u := range o.Units {
		if strings.TrimSpace(u.SQL) != strings.TrimSpace(u.OriginalSQL) {
			custom = true
		}
	}
	if !custom {
		o.Units = unitsFor(ss)
	}
	updated := map[string]string{}
	for k, v := range mapping {
		updated[k] = v
	}
	if referenceOnly {
		o.RefColumnMapping = updated
	} else {
		o.ColumnMapping = updated
	}
	if o.ForeignKey != nil && o.Table == table.Table && o.ForeignKey.RefTable == table.Table {
		o.RefColumnMapping = updated
	}
	return o
}
