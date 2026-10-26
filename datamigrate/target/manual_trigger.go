package target

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/repair"
)

type manualTriggerUnknownError struct{ error }

func (e manualTriggerUnknownError) Unwrap() error { return e.error }

// An INVALID catalog status is definite even when its diagnostic lookup fails.
// A diagnostic timeout does not make the already observed CREATE outcome
// uncertain; do not expose it as a cancellation of the DDL itself.
type manualTriggerInvalidError struct{ error }

type manualTriggerCatalog struct {
	exists                                        bool
	id                                            int64
	kind, compile, tableSchema, table, state, ddl string
}
type manualTriggerQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}
type manualTriggerBaselineKey struct{}
type manualTriggerSessionKey struct{}
type manualTriggerSession struct {
	w    *DaMengWriter
	conn *sql.Conn
}

func (s *manualTriggerSession) open(ctx context.Context) (*sql.Conn, error) {
	if s.conn != nil {
		return s.conn, nil
	}
	conn, err := s.w.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err = conn.ExecContext(ctx, "SET SCHEMA "+damengnative.Quote(s.w.schema)); err != nil {
		discardNativeEventConnection(conn)
		return nil, err
	}
	s.conn = conn
	return conn, nil
}

func readManualTriggerCatalog(ctx context.Context, q manualTriggerQueryer, schema, name string) (c manualTriggerCatalog, err error) {
	rows, err := q.QueryContext(ctx, `SELECT OBJECT_ID,OBJECT_TYPE,STATUS FROM ALL_OBJECTS WHERE OWNER=? AND OBJECT_NAME=?`, schema, name)
	if err != nil {
		return c, err
	}
	count := 0
	for rows.Next() {
		count++
		if count > 1 {
			rows.Close()
			return c, fmt.Errorf("目标同名对象不唯一，禁止替换触发器")
		}
		if err = rows.Scan(&c.id, &c.kind, &c.compile); err != nil {
			rows.Close()
			return c, err
		}
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return c, err
	}
	if count == 0 {
		return c, nil
	}
	c.exists = true
	c.kind = strings.ToUpper(strings.TrimSpace(c.kind))
	c.compile = strings.ToUpper(strings.TrimSpace(c.compile))
	if c.kind != "TRIGGER" {
		return c, fmt.Errorf("目标同名对象是 %s，禁止覆盖", c.kind)
	}
	if err = q.QueryRowContext(ctx, `SELECT TABLE_OWNER,TABLE_NAME,STATUS FROM ALL_TRIGGERS WHERE OWNER=? AND TRIGGER_NAME=?`, schema, name).Scan(&c.tableSchema, &c.table, &c.state); err != nil {
		return c, err
	}
	var ddl sql.NullString
	if err = q.QueryRowContext(ctx, `SELECT DBMS_METADATA.GET_DDL(?,?,?)`, "TRIGGER", name, schema).Scan(&ddl); err != nil {
		return c, err
	}
	if !ddl.Valid || strings.TrimSpace(ddl.String) == "" {
		return c, fmt.Errorf("无法读取完整目标触发器定义，禁止覆盖")
	}
	c.ddl = damengnative.StripClientDelimiter(ddl.String)
	return c, nil
}

func manualTriggerOwned(c manualTriggerCatalog, o repair.Object) bool {
	if !c.exists {
		return true
	}
	if c.kind != "TRIGGER" || c.tableSchema != o.Schema || c.table != o.Table {
		return false
	}
	p := o.ManualTrigger
	candidates := append([]string{p.SQL}, p.PreviousSQL...)
	if original := p.Original; original != nil {
		candidates = append(candidates, original.DDL)
		for _, u := range original.Units {
			candidates = append(candidates, u.SQL, u.OriginalSQL)
		}
		if original.MySQLDamengTrigger != nil {
			candidates = append(candidates, original.MySQLDamengTrigger.TriggerSQL)
		}
		if original.Native != nil {
			for _, u := range original.Native.Units {
				candidates = append(candidates, u.SQL)
			}
		}
	}
	for _, candidate := range candidates {
		if damengnative.TableTriggerDefinitionsEqual(c.ddl, candidate, o.Schema) {
			return true
		}
	}
	return false
}

func manualTriggerCatalogEqual(a, b manualTriggerCatalog, schema string) bool {
	return a.exists == b.exists && (!a.exists || a.id == b.id && a.kind == b.kind && a.compile == b.compile && a.tableSchema == b.tableSchema && a.table == b.table && a.state == b.state && damengnative.TableTriggerDefinitionsEqual(a.ddl, b.ddl, schema))
}

func manualTriggerEnabled(state string) (bool, bool) {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "ENABLED", "ENABLE", "Y", "1":
		return true, true
	case "DISABLED", "DISABLE", "N", "0":
		return false, true
	default:
		return false, false
	}
}

func manualTriggerDesiredState(o repair.Object) bool {
	if o.Native != nil {
		enabled, known := manualTriggerEnabled(o.Native.Status)
		if known {
			return enabled
		}
	}
	// A manual ordinary CREATE is enabled by default. An explicit header state
	// is retained; words inside the program or literals cannot change it.
	ts, _ := nativeSQLTokens(o.ManualTrigger.SQL)
	for _, t := range ts {
		if strings.EqualFold(t, "BEGIN") || strings.EqualFold(t, "DECLARE") {
			break
		}
		if strings.EqualFold(t, "DISABLE") {
			return false
		}
	}
	return true
}

func manualTriggerCompilation(ctx context.Context, q manualTriggerQueryer, o repair.Object, c manualTriggerCatalog) error {
	if c.compile == "VALID" {
		return nil
	}
	if c.compile != "INVALID" {
		return manualTriggerUnknownError{fmt.Errorf("无法确认目标触发器编译状态: %s", c.compile)}
	}
	rows, err := q.QueryContext(ctx, `SELECT LINE,POSITION,TEXT FROM ALL_ERRORS WHERE OWNER=? AND NAME=? AND TYPE=? ORDER BY SEQUENCE`, o.Schema, o.TargetName, "TRIGGER")
	detailFailure := func(cause error) error {
		return manualTriggerInvalidError{fmt.Errorf("触发器 %s.%s 编译无效，读取详细错误失败: %w", o.Schema, o.TargetName, cause)}
	}
	if err != nil {
		return detailFailure(err)
	}
	var details []string
	for rows.Next() {
		var line, pos int
		var detail string
		if err = rows.Scan(&line, &pos, &detail); err != nil {
			rows.Close()
			return detailFailure(err)
		}
		details = append(details, fmt.Sprintf("%d:%d %s", line, pos, detail))
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return detailFailure(err)
	}
	return manualTriggerInvalidError{fmt.Errorf("触发器 %s.%s 编译无效: %s", o.Schema, o.TargetName, strings.Join(details, "; "))}
}

// executeManualTriggerCreate runs only the saved CREATE revision. Its caller
// has already durably recorded running; compilation is proved before success.
func executeManualTriggerCreate(ctx context.Context, w *DaMengWriter, o repair.Object) error {
	d, err := damengnative.ParseTableTriggerSQL(o.ManualTrigger.SQL, o.Schema)
	if err != nil {
		return err
	}
	baseline, ok := ctx.Value(manualTriggerBaselineKey{}).(manualTriggerCatalog)
	if !ok {
		return fmt.Errorf("手工触发器缺少替换前目录核对")
	}
	session, ok := ctx.Value(manualTriggerSessionKey{}).(*manualTriggerSession)
	if !ok {
		return fmt.Errorf("手工触发器缺少固定执行会话")
	}
	conn, err := session.open(ctx)
	if err != nil {
		return err
	}
	current, err := readManualTriggerCatalog(ctx, conn, o.Schema, o.TargetName)
	if err != nil {
		return err
	}
	if !manualTriggerCatalogEqual(baseline, current, o.Schema) {
		return fmt.Errorf("目标触发器在执行前已变化，请刷新后重试")
	}
	if current.exists && !d.Replace {
		return fmt.Errorf("目标触发器已存在，请使用 CREATE OR REPLACE TRIGGER")
	}
	if _, err = conn.ExecContext(ctx, d.CreateSQL(current.exists && d.Replace)); err != nil {
		return err
	}
	actual, err := readManualTriggerCatalog(ctx, conn, o.Schema, o.TargetName)
	if err != nil {
		return manualTriggerUnknownError{fmt.Errorf("CREATE 已提交，读取结果失败: %w", err)}
	}
	if !actual.exists || actual.tableSchema != o.Schema || actual.table != o.Table || !damengnative.TableTriggerDefinitionsEqual(actual.ddl, d.SQL, o.Schema) {
		return manualTriggerUnknownError{fmt.Errorf("CREATE 已提交，但目标完整定义不能确认与保存的修订一致")}
	}
	return manualTriggerCompilation(ctx, conn, o, actual)
}

func executeManualNativeTriggerUnit(ctx context.Context, w *DaMengNativeWriter, o repair.Object, index int) error {
	unit := o.Native.Units[index]
	if unit.SQL == o.ManualTrigger.SQL {
		return executeManualTriggerCreate(ctx, w.DaMengWriter, o)
	}
	if err := validateNativeObjectUnit(o.Schema, *o.Native, unit); err != nil {
		return err
	}
	session, ok := ctx.Value(manualTriggerSessionKey{}).(*manualTriggerSession)
	if !ok {
		return fmt.Errorf("手工触发器缺少固定执行会话")
	}
	conn, err := session.open(ctx)
	if err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, unit.SQL); err != nil {
		return err
	}
	if unit.Phase == "compile" {
		actual, err := readManualTriggerCatalog(ctx, conn, o.Schema, o.TargetName)
		if err != nil {
			return manualTriggerUnknownError{err}
		}
		return manualTriggerCompilation(ctx, conn, o, actual)
	}
	return nil
}

func executeManualTriggerRepair(ctx context.Context, w Writer, o repair.Object, units []repair.Unit, onStep func([]repair.Unit) error) (out []repair.Unit, err error) {
	out = slices.Clone(units)
	check := o
	check.Units = out
	if !check.TrustedManualTrigger() {
		return out, fmt.Errorf("手工触发器修订或对象身份不可信")
	}
	var dm *DaMengWriter
	switch writer := w.(type) {
	case *DaMengWriter:
		dm = writer
	case *DaMengNativeWriter:
		dm = writer.DaMengWriter
	default:
		return out, fmt.Errorf("手工触发器仅支持达梦目标")
	}
	if dm.schema != o.Schema {
		return out, fmt.Errorf("目标 Schema 与手工触发器不一致")
	}
	if o.Native != nil && (o.Native.EventTrigger || o.Native.Managed) {
		return out, fmt.Errorf("仅支持普通表的独立触发器")
	}
	var ordinary int
	if err = dm.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_TABLES WHERE OWNER=? AND TABLE_NAME=?`, o.Schema, o.Table).Scan(&ordinary); err != nil {
		return out, err
	}
	if ordinary != 1 {
		return out, fmt.Errorf("触发器所属对象不是可见的普通表")
	}
	create := -1
	uncertain := o.Status == "unknown"
	for i, u := range out {
		if u.SQL == o.ManualTrigger.SQL {
			create = i
		}
		uncertain = uncertain || u.Status == "unknown" || u.Status == "running"
	}
	persist := func() error {
		if onStep == nil {
			return nil
		}
		return onStep(slices.Clone(out))
	}
	unknown := func(cause error) ([]repair.Unit, error) {
		if create >= 0 {
			out[create].Status, out[create].Error = "unknown", cause.Error()
		}
		return out, manualTriggerUnknownError{errors.Join(cause, persist())}
	}
	baseline, err := readManualTriggerCatalog(ctx, dm.db, o.Schema, o.TargetName)
	if err != nil {
		if uncertain {
			return unknown(err)
		}
		return out, err
	}
	if !manualTriggerOwned(baseline, o) {
		err = fmt.Errorf("目标同名触发器不属于原失败定义或已保存的手工修订，禁止覆盖")
		if uncertain {
			return unknown(err)
		}
		return out, err
	}
	if !baseline.exists && out[create].Status == "succeeded" {
		return unknown(fmt.Errorf("已成功的触发器从目标目录消失，禁止重放成功步骤"))
	}
	currentDefinition := baseline.exists && damengnative.TableTriggerDefinitionsEqual(baseline.ddl, o.ManualTrigger.SQL, o.Schema)
	definition, _ := damengnative.ParseTableTriggerSQL(o.ManualTrigger.SQL, o.Schema)
	compilePending := false
	if currentDefinition && o.Native != nil && out[create].Status == "succeeded" {
		for i, unit := range o.Native.Units {
			compilePending = compilePending || unit.Phase == "compile" && out[i].Status != "succeeded" && out[i].Selected
		}
	}
	if baseline.exists && !definition.Replace && !compilePending && !(currentDefinition && baseline.compile == "VALID") && !(uncertain && currentDefinition && baseline.compile == "INVALID") {
		err = fmt.Errorf("目标触发器已存在，请使用 CREATE OR REPLACE TRIGGER")
		out[create].Status, out[create].Error = "failed", err.Error()
		return out, errors.Join(err, persist())
	}
	if currentDefinition {
		if err = manualTriggerCompilation(ctx, dm.db, o, baseline); err != nil {
			var unknownError manualTriggerUnknownError
			if errors.As(err, &unknownError) {
				return unknown(err)
			}
			if uncertain {
				out[create].Status, out[create].Error = "failed", err.Error()
				return out, errors.Join(err, persist())
			}
			// A known INVALID definition may compile successfully after its
			// dependencies are repaired. An explicit replacement is allowed to
			// run again; a previously successful native CREATE can instead use
			// its still-pending frozen COMPILE step.
			if !compilePending && (!definition.Replace || out[create].Status == "succeeded") {
				out[create].Status, out[create].Error = "failed", err.Error()
				return out, errors.Join(err, persist())
			}
		} else {
			out[create].Status, out[create].Error, out[create].Selected = "succeeded", "", false
			enabled, known := manualTriggerEnabled(baseline.state)
			for i := range out {
				if i == create {
					continue
				}
				if o.Native != nil && (o.Native.Units[i].Phase == "compile" || o.Native.Units[i].Phase == "restore" && known && enabled == manualTriggerDesiredState(o)) {
					out[i].Status, out[i].Error, out[i].Selected = "succeeded", "", false
				} else if out[i].Status == "unknown" || out[i].Status == "running" {
					out[i].Status, out[i].Error, out[i].Selected = "pending", "", true
				}
			}
			if err = persist(); err != nil {
				return unknown(fmt.Errorf("目录已核对但保存结果失败: %w", err))
			}
		}
	} else {
		for i := range out {
			if out[i].Status == "unknown" || out[i].Status == "running" {
				out[i].Status, out[i].Error, out[i].Selected = "pending", "", true
			}
		}
	}
	ctx = context.WithValue(ctx, manualTriggerBaselineKey{}, baseline)
	session := &manualTriggerSession{w: dm}
	defer func() {
		if session.conn != nil {
			discardNativeEventConnection(session.conn)
		}
	}()
	ctx = context.WithValue(ctx, manualTriggerSessionKey{}, session)
	if native, ok := w.(*DaMengNativeWriter); ok && o.Native != nil {
		selected := false
		for _, unit := range out {
			selected = selected || unit.Selected && unit.Status != "succeeded"
		}
		if selected {
			out, err = native.executeNativeRepair(ctx, o, out, onStep)
		}
	} else {
		if o.Native != nil {
			return out, fmt.Errorf("原生手工触发器必须使用原生目标写入器")
		}
		if out[create].Status != "succeeded" {
			if !out[create].Selected {
				return out, fmt.Errorf("未选择手工触发器执行单元")
			}
			out[create].Status, out[create].Error = "running", ""
			if err = persist(); err != nil {
				out[create].Status = "pending"
				return out, fmt.Errorf("保存执行前状态失败，未提交 SQL: %w", err)
			}
			err = executeManualTriggerCreate(ctx, dm, o)
			if err != nil {
				out[create].Status, out[create].Error = repairErrorStatus(err), err.Error()
				return out, errors.Join(err, persist())
			}
			out[create].Status, out[create].Error, out[create].Selected = "succeeded", "", false
			if err = persist(); err != nil {
				out[create].Status, out[create].Error = "unknown", err.Error()
				return out, manualTriggerUnknownError{fmt.Errorf("SQL 已执行但保存结果失败: %w", err)}
			}
		}
	}
	if err != nil {
		return out, err
	}
	var finalQueryer manualTriggerQueryer = dm.db
	if session.conn != nil {
		finalQueryer = session.conn
	}
	actual, err := readManualTriggerCatalog(ctx, finalQueryer, o.Schema, o.TargetName)
	if err != nil {
		out[create].Status = "unknown"
		return unknown(fmt.Errorf("读取触发器最终状态失败: %w", err))
	}
	enabled, known := manualTriggerEnabled(actual.state)
	if !actual.exists || !known || enabled != manualTriggerDesiredState(o) || !damengnative.TableTriggerDefinitionsEqual(actual.ddl, o.ManualTrigger.SQL, o.Schema) {
		out[create].Status = "unknown"
		return unknown(fmt.Errorf("触发器最终定义或启停状态与保存的修订不一致"))
	}
	if err = manualTriggerCompilation(ctx, finalQueryer, o, actual); err != nil {
		out[create].Status = repairErrorStatus(err)
		out[create].Error = err.Error()
		return out, errors.Join(err, persist())
	}
	invalidateRecoveryCache(w)
	return out, nil
}
