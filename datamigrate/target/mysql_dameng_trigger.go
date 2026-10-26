package target

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/mysqldmtrigger"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
)

type mysqlDamengTriggerPreflightError struct{ error }

func (e mysqlDamengTriggerPreflightError) Unwrap() error { return e.error }

func IsMySQLDamengTriggerPreflight(err error) bool {
	var e mysqlDamengTriggerPreflightError
	return errors.As(err, &e)
}

// All probes are read-only. The migration must never change instance-wide
// compatibility settings merely to install a generated trigger.
func DetectMySQLDamengTriggerCapabilities(ctx context.Context, w Writer) (mysqldmtrigger.Capabilities, error) {
	if w.DBType() != "dameng" {
		return mysqldmtrigger.Capabilities{}, fmt.Errorf("目标 %s 不支持 MySQL→达梦触发器", w.DBType())
	}
	if p, ok := w.(interface {
		DetectMySQLDamengTriggerCapabilities(context.Context) (mysqldmtrigger.Capabilities, error)
	}); ok {
		return p.DetectMySQLDamengTriggerCapabilities(ctx)
	}
	return mysqldmtrigger.Capabilities{}, fmt.Errorf("目标不支持达梦触发器能力检测")
}

func (w *DaMengWriter) DetectMySQLDamengTriggerCapabilities(ctx context.Context) (c mysqldmtrigger.Capabilities, err error) {
	var empty, concat int
	err = w.db.QueryRowContext(ctx, `SELECT PARA_VALUE FROM V$DM_INI WHERE PARA_NAME='COMPATIBLE_MODE'`).Scan(&c.CompatibleMode)
	if err == nil {
		err = w.db.QueryRowContext(ctx, `SELECT CASE WHEN '' IS NULL THEN 1 ELSE 0 END, CASE WHEN CONCAT('x',CAST(NULL AS VARCHAR(1))) IS NULL THEN 1 ELSE 0 END FROM DUAL`).Scan(&empty, &concat)
	}
	if err != nil {
		return c, fmt.Errorf("读取达梦触发器兼容能力: %w", err)
	}
	c.CompatibleMode = strings.TrimSpace(c.CompatibleMode)
	c.EmptyStringIsNull, c.ConcatNullYieldsNull = empty == 1, concat == 1
	return c, nil
}

func CheckMySQLDamengLoadTriggers(ctx context.Context, w Writer, table string) error {
	if w.DBType() != "dameng" {
		return fmt.Errorf("目标 %s 不是达梦", w.DBType())
	}
	if p, ok := w.(interface {
		CheckMySQLDamengLoadTriggers(context.Context, string) error
	}); ok {
		return p.CheckMySQLDamengLoadTriggers(ctx, table)
	}
	return fmt.Errorf("目标不能检查达梦业务触发器")
}

func (w *DaMengWriter) CheckMySQLDamengLoadTriggers(ctx context.Context, table string) error {
	rows, err := w.db.QueryContext(ctx, `SELECT TRIGGER_NAME,STATUS FROM ALL_TRIGGERS WHERE TABLE_OWNER=? AND TABLE_NAME=? ORDER BY TRIGGER_NAME`, w.schema, table)
	if err != nil {
		return fmt.Errorf("读取目标业务触发器: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, state string
		if err := rows.Scan(&name, &state); err != nil {
			return err
		}
		if state = strings.TrimSpace(state); state != "N" && state != "DISABLED" {
			return fmt.Errorf("目标表 %s.%s 存在启用或状态不明的业务触发器 %s，禁止复制数据以免重复执行副作用；请先停用触发器", w.schema, table, name)
		}
	}
	return rows.Err()
}

func CreateMySQLDamengTrigger(ctx context.Context, w Writer, p mysqldmtrigger.Plan) error {
	if w.DBType() != "dameng" {
		return mysqlDamengTriggerPreflightError{fmt.Errorf("目标 %s 不支持 MySQL→达梦触发器", w.DBType())}
	}
	if err := mysqldmtrigger.ValidatePlan(p); err != nil {
		return mysqlDamengTriggerPreflightError{err}
	}
	if creator, ok := w.(interface {
		CreateMySQLDamengTrigger(context.Context, mysqldmtrigger.Plan) error
	}); ok {
		return creator.CreateMySQLDamengTrigger(ctx, p)
	}
	return mysqlDamengTriggerPreflightError{fmt.Errorf("目标不能执行达梦触发器计划")}
}

func (w *DaMengWriter) CreateMySQLDamengTrigger(ctx context.Context, p mysqldmtrigger.Plan) error {
	return w.createMySQLDamengTrigger(ctx, p, false, false)
}

// Dameng DDL commits independently of a Go transaction. Keep the entire CREATE
// as one durable unit, and only record success after inspecting the catalog.
func (w *DaMengWriter) createMySQLDamengTrigger(ctx context.Context, p mysqldmtrigger.Plan, allowReuse, uncertain bool) (err error) {
	started := false
	defer func() {
		if err != nil && !started {
			err = mysqlDamengTriggerPreflightError{err}
		}
	}()
	if err = mysqldmtrigger.ValidatePlan(p); err != nil {
		return err
	}
	if p.Schema != w.schema {
		return fmt.Errorf("目标 schema 与冻结触发器计划不一致")
	}
	if err = w.validateMySQLDamengTriggerTarget(ctx, p); err != nil {
		return err
	}
	exists, err := w.inspectMySQLDamengTrigger(ctx, p)
	if err != nil {
		if allowReuse {
			started = true
			return errors.Join(err, repair.Observe(ctx, p.TriggerSQL, "unknown", err))
		}
		return err
	}
	if exists {
		if !allowReuse {
			return fmt.Errorf("目标触发器 %s.%s 已存在，普通迁移禁止覆盖或接管", p.Schema, p.Name)
		}
		started = true
		return repair.Observe(ctx, p.TriggerSQL, "reused", nil)
	}
	if uncertain {
		started = true
		err = fmt.Errorf("原执行结果未知且目标触发器不存在，不能证明先前执行结果，禁止自动重放")
		return errors.Join(err, repair.Observe(ctx, p.TriggerSQL, "unknown", err))
	}
	if err = repair.Observe(ctx, p.TriggerSQL, "running", nil); err != nil {
		return fmt.Errorf("保存触发器执行前状态: %w", err)
	}
	started = true
	if _, err = w.db.ExecContext(ctx, p.TriggerSQL); err != nil {
		return errors.Join(err, repair.Observe(ctx, p.TriggerSQL, repairErrorStatus(err), err))
	}
	exists, err = w.inspectMySQLDamengTrigger(ctx, p)
	if err != nil || !exists {
		if err == nil {
			err = fmt.Errorf("CREATE 返回成功但未找到目标触发器")
		}
		return errors.Join(err, repair.Observe(ctx, p.TriggerSQL, "unknown", err))
	}
	return repair.Observe(ctx, p.TriggerSQL, "succeeded", nil)
}

func (w *DaMengWriter) validateMySQLDamengTriggerTarget(ctx context.Context, p mysqldmtrigger.Plan) error {
	c, err := w.DetectMySQLDamengTriggerCapabilities(ctx)
	if err != nil {
		return err
	}
	if c != p.Options.Capabilities {
		return fmt.Errorf("达梦兼容模式或空串/NULL 拼接能力已偏离冻结触发器计划")
	}
	seen := map[string]bool{}
	for _, table := range p.SourceTables {
		name := p.Options.TableNames[table.TableName]
		if name == "" {
			name = strings.ToUpper(table.TableName)
		}
		if seen[name] {
			return fmt.Errorf("触发器依赖表映射重复: %s", name)
		}
		seen[name] = true
		var ordinary int
		if err = w.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_TABLES WHERE OWNER=? AND TABLE_NAME=?`, w.schema, name).Scan(&ordinary); err != nil {
			return err
		}
		if ordinary != 1 {
			return fmt.Errorf("触发器依赖表 %s.%s 不存在或不可见", w.schema, name)
		}
		columns, err := inspectColumns(ctx, w, name)
		if err != nil {
			return err
		}
		actual := map[string]source.ColumnInfo{}
		for _, column := range columns {
			actual[column.Name] = column
		}
		mapped := map[string]bool{}
		for _, col := range table.Columns {
			column := p.Options.ColumnNames[table.TableName][col.Name]
			if column == "" {
				column = strings.ToUpper(col.Name)
			}
			a, ok := actual[column]
			if !ok || mapped[column] || a.Generated != "" || !mysqlDMCompatibleColumn(w.dia.MapType(col, "mysql", dialect.TypeOpt{}), a) {
				return fmt.Errorf("触发器依赖列 %s.%s 的名称、类型或容量不兼容", name, column)
			}
			mapped[column] = true
		}
		if len(actual) != len(mapped) {
			return fmt.Errorf("触发器依赖表 %s 列集合已偏离冻结结构", name)
		}
	}
	if !seen[p.Table] {
		return fmt.Errorf("冻结触发器计划缺少所属表结构")
	}
	for _, name := range p.Dependencies {
		if !seen[name] {
			return fmt.Errorf("触发器依赖表 %s 缺少冻结结构", name)
		}
	}
	return nil
}

func mysqlDMCompatibleColumn(want string, actual source.ColumnInfo) bool {
	base := func(s string) string {
		s = strings.ToUpper(strings.TrimSpace(strings.SplitN(s, "(", 2)[0]))
		switch s {
		case "INTEGER":
			return "INT"
		case "VARCHAR2", "NVARCHAR2", "NVARCHAR", "CHARACTER VARYING":
			return "VARCHAR"
		case "NCHAR", "CHARACTER":
			return "CHAR"
		case "NUMERIC", "DECIMAL":
			return "NUMBER"
		case "DOUBLE PRECISION":
			return "DOUBLE"
		case "CLOB", "LONGVARCHAR":
			return "TEXT"
		}
		return s
	}
	if base(want) != base(actual.DataType) {
		return false
	}
	var precision, scale int64
	if _, err := fmt.Sscanf(strings.ReplaceAll(want, " CHAR", ""), strings.SplitN(want, "(", 2)[0]+"(%d,%d)", &precision, &scale); err == nil {
		return actual.Precision-actual.Scale >= precision-scale && actual.Scale >= scale
	}
	if _, err := fmt.Sscanf(strings.ReplaceAll(want, " CHAR", ""), strings.SplitN(want, "(", 2)[0]+"(%d)", &precision); err == nil {
		switch base(want) {
		case "VARCHAR", "CHAR":
			return actual.Length >= precision
		case "NUMBER":
			return actual.Precision-actual.Scale >= precision
		}
	}
	return true
}

func ValidateMySQLDamengTriggerRepair(o repair.Object, units []repair.Unit) error {
	if o.Kind != "trigger" || o.MySQLDamengTrigger == nil || o.MySQLPGTrigger != nil || o.Native != nil || o.DiagnosticOnly {
		return fmt.Errorf("缺少可信 MySQL→达梦触发器计划")
	}
	p := *o.MySQLDamengTrigger
	if err := mysqldmtrigger.ValidatePlan(p); err != nil {
		return err
	}
	if p.Schema != o.Schema || p.Table != o.Table || p.Name != o.TargetName || len(units) != 1 || len(o.Units) != 1 {
		return fmt.Errorf("触发器对象身份或完整执行单元与冻结计划不一致")
	}
	u := units[0]
	if u.ID != o.Units[0].ID || u.SQL != p.TriggerSQL || u.OriginalSQL != p.TriggerSQL || o.Units[0].SQL != p.TriggerSQL || o.Units[0].OriginalSQL != p.TriggerSQL {
		return fmt.Errorf("达梦触发器只允许重试完整原生成 SQL，不可编辑或拆分")
	}
	if u.Status == "succeeded" || o.Status == "succeeded" {
		return fmt.Errorf("触发器已成功，无需重试")
	}
	if !u.Selected {
		return fmt.Errorf("必须选择完整触发器执行单元")
	}
	return nil
}

func executeMySQLDamengTriggerRepair(ctx context.Context, w Writer, o repair.Object, units []repair.Unit, onStep func([]repair.Unit) error) ([]repair.Unit, error) {
	out := slices.Clone(units)
	if err := ValidateMySQLDamengTriggerRepair(o, out); err != nil {
		return out, err
	}
	dw, ok := w.(*DaMengWriter)
	if !ok {
		return out, fmt.Errorf("MySQL→达梦触发器修复需要达梦 Writer")
	}
	observed := repair.WithObserver(ctx, func(e repair.Event) error {
		if e.SQL != o.MySQLDamengTrigger.TriggerSQL {
			return fmt.Errorf("触发器执行出现未记录 SQL")
		}
		out[0].Status, out[0].Error = e.Status, e.Error
		if e.Status == "reused" {
			out[0].Status = "succeeded"
		}
		if out[0].Status == "succeeded" {
			out[0].Selected = false
		}
		if onStep != nil {
			if err := onStep(slices.Clone(out)); err != nil {
				// DDL can already be committed when saving its final event fails.
				// Preserve the need to reconcile rather than returning success.
				out[0].Status, out[0].Selected, out[0].Error = "unknown", true, err.Error()
				return err
			}
		}
		return nil
	})
	uncertain := o.Status == "unknown" || o.Status == "running" || out[0].Status == "unknown" || out[0].Status == "running"
	err := dw.createMySQLDamengTrigger(observed, *o.MySQLDamengTrigger, true, uncertain)
	return out, err
}
