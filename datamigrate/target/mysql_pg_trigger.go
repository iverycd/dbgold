package target

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/mysqltrigger"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
)

const mysqlPGLoadTriggerSQL = `SELECT t.relname,g.tgname FROM pg_trigger g JOIN pg_class t ON t.oid=g.tgrelid JOIN pg_namespace n ON n.oid=t.relnamespace WHERE n.nspname=$1 AND NOT g.tgisinternal AND g.tgenabled<>'D' ORDER BY t.relname,g.tgname`

func quoteTriggerIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func qualifiedTriggerIdent(schema, name string) string {
	return quoteTriggerIdent(schema) + "." + quoteTriggerIdent(name)
}

// CheckMySQLPGLoadTriggers prevents copying already-materialized source rows
// through business triggers a second time. Disabled and internal FK triggers
// are deliberately unaffected.
func CheckMySQLPGLoadTriggers(ctx context.Context, w Writer, tables []string) error {
	if !source.IsPGFamily(w.DBType()) {
		return fmt.Errorf("目标 %s 不支持 MySQL 触发器迁移", w.DBType())
	}
	if checker, ok := w.(interface {
		CheckMySQLPGLoadTriggers(context.Context, []string) error
	}); ok {
		return checker.CheckMySQLPGLoadTriggers(ctx, tables)
	}
	a, err := repairAccess(w)
	if err != nil {
		return err
	}
	schema, err := ResolveRepairSchema(ctx, w)
	if err != nil {
		return err
	}
	rows, err := a.db.QueryContext(ctx, mysqlPGLoadTriggerSQL, schema)
	if err != nil {
		return fmt.Errorf("检查目标业务触发器: %w", err)
	}
	defer rows.Close()
	selected := map[string]bool{}
	for _, t := range tables {
		selected[t] = true
	}
	for rows.Next() {
		var table, name string
		if err := rows.Scan(&table, &name); err != nil {
			return err
		}
		if selected[table] {
			return fmt.Errorf("目标表 %s.%s 存在启用的业务触发器 %s，禁止复制数据以免重复执行副作用；请先停用触发器", schema, table, name)
		}
	}
	return rows.Err()
}

// ValidateMySQLPGTriggerRepair accepts only the complete frozen conversion.
// These procedural units are never editable or independently selectable.
func ValidateMySQLPGTriggerRepair(o repair.Object, units []repair.Unit) error {
	if o.Kind != "trigger" || o.MySQLPGTrigger == nil {
		return fmt.Errorf("缺少可信 MySQL 触发器计划")
	}
	p := *o.MySQLPGTrigger
	if err := mysqltrigger.ValidatePlan(p); err != nil {
		return err
	}
	if p.Schema != o.Schema || p.Table != o.Table || p.Name != o.TargetName {
		return fmt.Errorf("触发器对象身份与冻结计划不一致")
	}
	if o.Status == "unknown" || o.Status == "running" {
		return fmt.Errorf("触发器执行结果未知，不允许重放")
	}
	if len(units) != 2 {
		return fmt.Errorf("触发器必须完整执行函数和触发器两个原始步骤")
	}
	for i, sqlText := range p.Statements() {
		u := units[i]
		if u.SQL != sqlText || u.OriginalSQL != sqlText {
			return fmt.Errorf("触发器只允许重试完整原生成 SQL")
		}
		if u.Status == "unknown" || u.Status == "running" {
			return fmt.Errorf("触发器存在未知执行结果，不允许重放")
		}
		if u.Status != "succeeded" && !u.Selected {
			return fmt.Errorf("触发器两个步骤必须一并选择")
		}
	}
	if units[0].Status == "succeeded" != (units[1].Status == "succeeded") {
		return fmt.Errorf("触发器存在部分提交记录，禁止自动重放")
	}
	if units[0].Status == "succeeded" {
		return fmt.Errorf("触发器已成功，无需重试")
	}
	return nil
}

type mysqlPGTriggerPreflightError struct{ error }

func (e mysqlPGTriggerPreflightError) Unwrap() error { return e.error }

func IsMySQLPGTriggerPreflight(err error) bool {
	var preflight mysqlPGTriggerPreflightError
	return errors.As(err, &preflight)
}

func CreateMySQLPGTrigger(ctx context.Context, w Writer, p mysqltrigger.Plan, owner string) error {
	return createMySQLPGTrigger(ctx, w, p, owner, false)
}
func createMySQLPGTrigger(ctx context.Context, w Writer, p mysqltrigger.Plan, owner string, allowReuse bool) (err error) {
	started := false
	defer func() {
		if err != nil && !started {
			err = mysqlPGTriggerPreflightError{err}
		}
	}()
	if !source.IsPGFamily(w.DBType()) {
		return fmt.Errorf("目标 %s 不支持 MySQL 触发器迁移", w.DBType())
	}
	if err := mysqltrigger.ValidatePlan(p); err != nil {
		return err
	}
	if p.Owner != owner {
		return fmt.Errorf("触发器 owner 与冻结计划不一致")
	}
	if creator, ok := w.(interface {
		CreateMySQLPGTrigger(context.Context, mysqltrigger.Plan, string) error
	}); ok && !p.NativeUserVariables {
		return creator.CreateMySQLPGTrigger(ctx, p, owner)
	}
	a, err := repairAccess(w)
	if err != nil {
		return err
	}
	schema, err := ResolveRepairSchema(ctx, w)
	if err != nil {
		return err
	}
	if schema != p.Schema {
		return fmt.Errorf("目标 schema 与触发器计划不一致")
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if p.NativeUserVariables {
		if err = validateMySQLPGNativeUserVariables(ctx, tx, w); err != nil {
			return err
		}
	}
	if err = validateMySQLPGTriggerTarget(ctx, tx, p); err != nil {
		return err
	}
	reused, err := inspectMySQLPGTrigger(ctx, tx, p)
	if err != nil {
		return err
	}
	if reused {
		if !allowReuse {
			return fmt.Errorf("目标触发器及辅助函数已存在，普通迁移禁止覆盖或接管")
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		started = true
		for _, q := range p.Statements() {
			if err = repair.Observe(ctx, q, "reused", nil); err != nil {
				return err
			}
		}
		return nil
	}
	stmts := []dialect.Statement{{SQL: p.FunctionSQL}, {SQL: p.TriggerSQL}}
	started = true
	// Keep two durable execution units even when ownership is requested. An
	// owner failure rolls both object definitions back with the same transaction.
	if owner == "" {
		return execObservedTransaction(ctx, tx, stmts)
	}
	return execMySQLPGTriggerOwner(ctx, tx, p, stmts)
}

func execMySQLPGTriggerOwner(ctx context.Context, tx *sql.Tx, p mysqltrigger.Plan, stmts []dialect.Statement) error {
	executed := []dialect.Statement{}
	rollback := func(cause error) error {
		err := tx.Rollback()
		status := "rolled_back"
		if err != nil {
			status = "unknown"
		}
		for _, s := range executed {
			cause = errors.Join(cause, repair.Observe(ctx, s.SQL, status, err))
		}
		return cause
	}
	for _, s := range stmts {
		if err := repair.Observe(ctx, s.SQL, "running", nil); err != nil {
			return rollback(err)
		}
		if _, err := tx.ExecContext(ctx, s.SQL); err != nil {
			return rollback(errors.Join(err, repair.Observe(ctx, s.SQL, repairErrorStatus(err), err)))
		}
		executed = append(executed, s)
	}
	if _, err := tx.ExecContext(ctx, "ALTER FUNCTION "+qualifiedTriggerIdent(p.Schema, p.FunctionName)+"() OWNER TO "+quoteTriggerIdent(p.Owner)); err != nil {
		return rollback(err)
	}
	commitErr := tx.Commit()
	status := "succeeded"
	if commitErr != nil {
		status = "unknown"
	}
	result := commitErr
	for _, s := range executed {
		result = errors.Join(result, repair.Observe(ctx, s.SQL, status, commitErr))
	}
	return result
}

func validateMySQLPGTriggerTarget(ctx context.Context, tx *sql.Tx, p mysqltrigger.Plan) error {
	var available bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_language WHERE lanname='plpgsql')`).Scan(&available); err != nil {
		return fmt.Errorf("检测 plpgsql: %w", err)
	}
	if !available {
		return fmt.Errorf("目标未安装 plpgsql，无法创建触发器函数")
	}
	if p.RequiresTextSemantics {
		if err := validateMySQLPGTextSemantics(ctx, tx); err != nil {
			return err
		}
	}
	if p.CharacterMetadata != nil && len(p.CharacterMetadata.Profiles) > 0 {
		if err := validateMySQLPGCharacterCapabilities(ctx, tx); err != nil {
			return err
		}
	}
	if len(p.Tables) == 0 {
		return fmt.Errorf("触发器计划缺少冻结目标表结构")
	}
	tables := slices.Clone(p.Tables)
	slices.SortFunc(tables, func(a, b source.TableDDLInfo) int { return strings.Compare(a.TableName, b.TableName) })
	seen := map[string]bool{}
	for _, table := range tables {
		if table.TableName == "" || seen[table.TableName] {
			return fmt.Errorf("触发器计划表结构不完整或重复")
		}
		seen[table.TableName] = true
		if _, err := tx.ExecContext(ctx, "LOCK TABLE "+qualifiedTriggerIdent(p.Schema, table.TableName)+" IN SHARE ROW EXCLUSIVE MODE"); err != nil {
			return fmt.Errorf("锁定触发器依赖表 %s: %w", table.TableName, err)
		}
		rows, err := tx.QueryContext(ctx, `SELECT a.attname,format_type(a.atttypid,a.atttypmod),COALESCE(row_to_json(a)->>'attgenerated','') FROM pg_attribute a JOIN pg_class t ON t.oid=a.attrelid JOIN pg_namespace n ON n.oid=t.relnamespace WHERE n.nspname=$1 AND t.relname=$2 AND t.relkind='r' AND a.attnum>0 AND NOT a.attisdropped ORDER BY a.attnum`, p.Schema, table.TableName)
		if err != nil {
			return err
		}
		actual := map[string]string{}
		for rows.Next() {
			var name, typ, generated string
			if err = rows.Scan(&name, &typ, &generated); err != nil {
				rows.Close()
				return err
			}
			if generated != "" {
				rows.Close()
				return fmt.Errorf("依赖表 %s 含生成列，无法确认触发器兼容性", table.TableName)
			}
			actual[name] = canonicalTriggerType(typ)
		}
		err = errors.Join(rows.Err(), rows.Close())
		if err != nil {
			return err
		}
		if len(actual) != len(table.Columns) {
			return fmt.Errorf("依赖表 %s 的列集合已偏离触发器计划", table.TableName)
		}
		for _, col := range table.Columns {
			if actual[col.Name] != canonicalTriggerType(col.DataType) {
				return fmt.Errorf("依赖列 %s.%s 类型或名称已偏离触发器计划", table.TableName, col.Name)
			}
		}
	}
	if !seen[p.Table] {
		return fmt.Errorf("触发器计划缺少所属表结构")
	}
	for _, name := range p.Dependencies {
		if !seen[name] {
			return fmt.Errorf("触发器依赖表 %s 缺少冻结结构", name)
		}
	}
	return nil
}

const mysqlPGCharacterCapabilitySQL = `SELECT current_setting('server_encoding'),
pg_catalog.convert_to(pg_catalog.translate(pg_catalog.regexp_replace(pg_catalog.rtrim($1::text,' ') COLLATE pg_catalog."C",$2::text,$3::text,'g'),$4::text,$5::text),'UTF8')=pg_catalog.convert_to($6::text,'UTF8')
AND pg_catalog.translate(NULL::text,'a','b') IS NULL
AND pg_catalog.convert_to(pg_catalog.rtrim(''::text,' '),'UTF8')=pg_catalog.decode('','hex')`

const mysqlPGTextSemanticsSQL = `SELECT pg_catalog.octet_length(''::text)=0
AND ('a'::text || ''::text)='a'::text
AND ('a'::text || NULL::text) IS NULL
AND pg_catalog.octet_length(COALESCE(NULL::text,'a '::text))=2`

func validateMySQLPGTextSemantics(ctx context.Context, tx *sql.Tx) error {
	var compatible sql.NullBool
	if err := tx.QueryRowContext(ctx, mysqlPGTextSemanticsSQL).Scan(&compatible); err != nil {
		return fmt.Errorf("目标空字符串及 NULL 拼接语义验证失败: %w", err)
	}
	if !compatible.Valid || !compatible.Bool {
		return fmt.Errorf("目标兼容模式的空字符串、NULL 拼接或文本转换语义与 MySQL 不一致")
	}
	return nil
}

// Probe the exact primitives used by v2 character keys before installing DDL.
// This intentionally does not depend on the target database's business collation.
func validateMySQLPGCharacterCapabilities(ctx context.Context, tx *sql.Tx) error {
	var encoding string
	var compatible bool
	err := tx.QueryRowContext(ctx, mysqlPGCharacterCapabilitySQL, "Aé😀 ", "[\U00010000-\U0010ffff]", "�", "Aé", "ae", "ae�").Scan(&encoding, &compatible)
	if err != nil {
		return fmt.Errorf("目标字符比较能力验证失败: %w", err)
	}
	if !strings.EqualFold(encoding, "UTF8") || !compatible {
		return fmt.Errorf("目标需要 UTF8 及兼容的字符映射、补充字符和 NULL 比较能力（当前编码 %s）", encoding)
	}
	return nil
}

var triggerTypeSpaces = regexp.MustCompile(`\s+`)

func canonicalTriggerType(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = triggerTypeSpaces.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "withouttimezone", "")
	s = regexp.MustCompile(`^timestamp(\([0-9]+\))?withtimezone$`).ReplaceAllString(s, `timestamptz$1`)
	s = regexp.MustCompile(`^time(\([0-9]+\))?withtimezone$`).ReplaceAllString(s, `timetz$1`)
	for _, pair := range [][2]string{{"charactervarying", "varchar"}, {"character", "char"}, {"timestampwithouttimezone", "timestamp"}, {"timestampwithtimezone", "timestamptz"}, {"timewithouttimezone", "time"}, {"timewithtimezone", "timetz"}, {"doubleprecision", "float8"}, {"integer", "int4"}, {"bigint", "int8"}, {"smallint", "int2"}, {"boolean", "bool"}, {"decimal", "numeric"}, {"real", "float4"}} {
		s = strings.ReplaceAll(s, pair[0], pair[1])
	}
	switch s {
	case "int":
		s = "int4"
	case "float", "double":
		s = "float8"
	case "char":
		s = "char(1)"
	case "bit":
		s = "bit(1)"
	}
	if s == "timestamp(6)" {
		s = "timestamp"
	}
	if s == "time(6)" {
		s = "time"
	}
	s = regexp.MustCompile(`^numeric\(([0-9]+)\)$`).ReplaceAllString(s, `numeric($1,0)`)
	return s
}

func inspectMySQLPGTrigger(ctx context.Context, tx *sql.Tx, p mysqltrigger.Plan) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT p.oid,p.prosrc,p.prosecdef,p.prorettype='trigger'::regtype,p.pronargs,l.lanname,p.proconfig::text,pg_get_userbyid(p.proowner),p.provolatile='v' AND NOT p.proisstrict AND NOT p.proretset AND COALESCE(row_to_json(p)->>'proleakproof','false')='false' AND COALESCE(row_to_json(p)->>'prokind','f')='f' FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace JOIN pg_language l ON l.oid=p.prolang WHERE n.nspname=$1 AND p.proname=$2`, p.Schema, p.FunctionName)
	if err != nil {
		return false, err
	}
	var oid int64
	var body, lang, owner string
	var security, trigger, ordinary bool
	var args, count int
	var config sql.NullString
	for rows.Next() {
		count++
		if err = rows.Scan(&oid, &body, &security, &trigger, &args, &lang, &config, &owner, &ordinary); err != nil {
			rows.Close()
			return false, err
		}
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return false, err
	}
	functionExists := count > 0
	if functionExists && (count != 1 || body != p.FunctionBody || security || !trigger || !ordinary || args != 0 || lang != "plpgsql" || config.Valid || (p.Owner != "" && owner != p.Owner)) {
		return false, fmt.Errorf("辅助函数 %s 已存在且无法证明与原生成定义一致，禁止覆盖", p.FunctionName)
	}
	rows, err = tx.QueryContext(ctx, `SELECT g.tgname,g.tgtype,g.tgenabled,g.tgfoid,g.tgnargs,g.tgqual IS NULL,g.tgconstraint FROM pg_trigger g JOIN pg_class t ON t.oid=g.tgrelid JOIN pg_namespace n ON n.oid=t.relnamespace WHERE n.nspname=$1 AND t.relname=$2 AND NOT g.tgisinternal`, p.Schema, p.Table)
	if err != nil {
		return false, err
	}
	expected := 1
	if p.Source.Timing == "BEFORE" {
		expected |= 2
	}
	switch p.Source.Event {
	case "INSERT":
		expected |= 4
	case "DELETE":
		expected |= 8
	case "UPDATE":
		expected |= 16
	}
	found := false
	for rows.Next() {
		var name, enabled string
		var typ, nargs int
		var fn, constraint int64
		var noWhen bool
		if err = rows.Scan(&name, &typ, &enabled, &fn, &nargs, &noWhen, &constraint); err != nil {
			rows.Close()
			return false, err
		}
		if name == p.Name {
			if !functionExists || typ != expected || enabled != "O" || fn != oid || nargs != 0 || !noWhen || constraint != 0 {
				rows.Close()
				return false, fmt.Errorf("触发器 %s 已存在且定义不一致，禁止覆盖", name)
			}
			found = true
		} else if enabled != "D" && (typ&2 == expected&2) && (typ&28&expected != 0) {
			rows.Close()
			return false, fmt.Errorf("目标表 %s 存在同类启用触发器 %s，无法保证执行顺序", p.Table, name)
		}
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return false, err
	}
	if functionExists != found {
		return false, fmt.Errorf("辅助函数和触发器存在不完整对象，禁止自动覆盖或重放")
	}
	return found, nil
}

func executeMySQLPGTriggerRepair(ctx context.Context, w Writer, o repair.Object, units []repair.Unit, onStep func([]repair.Unit) error) ([]repair.Unit, error) {
	out := slices.Clone(units)
	if err := ValidateMySQLPGTriggerRepair(o, out); err != nil {
		return out, err
	}
	persist := func() error {
		if onStep != nil {
			return onStep(slices.Clone(out))
		}
		return nil
	}
	observed := repair.WithObserver(ctx, func(e repair.Event) error {
		for i, q := range o.MySQLPGTrigger.Statements() {
			if q == e.SQL {
				out[i].Status = e.Status
				out[i].Error = e.Error
				if e.Status == "reused" {
					out[i].Status = "succeeded"
				}
				if out[i].Status == "succeeded" {
					out[i].Selected = false
				}
				return persist()
			}
		}
		return fmt.Errorf("触发器执行出现未记录 SQL")
	})
	err := createMySQLPGTrigger(observed, w, *o.MySQLPGTrigger, o.MySQLPGTrigger.Owner, true)
	return out, err
}
