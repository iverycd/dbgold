package target

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/partition"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
)

// repairTarget keeps access to credentials and driver sessions inside target.
type repairTarget struct {
	db             *sql.DB
	schema, family string
}

func repairAccess(w Writer) (repairTarget, error) {
	switch v := w.(type) {
	case *PostgresWriter:
		return repairTarget{v.db, v.schema, "postgres"}, nil
	case *GBaseWriter:
		return repairTarget{v.db, v.schema, "postgres"}, nil
	case *VastbaseWriter:
		return repairTarget{v.db, v.schema, "postgres"}, nil
	case *HighGoWriter:
		return repairTarget{v.db, v.schema, "postgres"}, nil
	case *KingbaseWriter:
		return repairTarget{v.db, v.schema, "postgres"}, nil
	case *GaussDBWriter:
		return repairTarget{v.db, v.schema, "postgres"}, nil
	case *MySQLWriter:
		return repairTarget{v.db, v.schema, "mysql"}, nil
	case *DaMengWriter:
		return repairTarget{v.db, v.schema, "dameng"}, nil
	case *DaMengNativeWriter:
		return repairTarget{v.db, v.schema, "dameng"}, nil
	case *OracleWriter:
		return repairTarget{v.db, v.schema, "oracle"}, nil
	case *SQLServerWriter:
		return repairTarget{v.db, v.schema, "sqlserver"}, nil
	case *OscarWriter:
		return repairTarget{nil, v.schema, "oscar"}, nil
	default:
		return repairTarget{}, fmt.Errorf("目标 %s 不支持对象修复", w.DBType())
	}
}

func ResolveRepairSchema(ctx context.Context, w Writer) (string, error) {
	a, err := repairAccess(w)
	if err != nil {
		return "", err
	}
	if a.schema != "" {
		return a.schema, nil
	}
	q := "SELECT current_schema()"
	switch a.family {
	case "mysql":
		q = "SELECT DATABASE()"
	case "sqlserver":
		q = "SELECT SCHEMA_NAME()"
	case "oracle":
		q = "SELECT SYS_CONTEXT('USERENV','CURRENT_SCHEMA') FROM DUAL"
	case "dameng":
		q = "SELECT CURRENT_SCHEMA"
	case "oscar":
		return "", fmt.Errorf("神通修复需要明确的目标 schema")
	}
	var schema sql.NullString
	if err = a.db.QueryRowContext(ctx, q).Scan(&schema); err != nil {
		return "", err
	}
	if !schema.Valid || schema.String == "" {
		return "", fmt.Errorf("无法确定目标 schema")
	}
	setRepairSchema(w, schema.String)
	return schema.String, nil
}

func setRepairSchema(w Writer, schema string) {
	switch v := w.(type) {
	case *PostgresWriter:
		v.schema = schema
	case *GBaseWriter:
		v.schema = schema
	case *VastbaseWriter:
		v.schema = schema
	case *HighGoWriter:
		v.schema = schema
	case *KingbaseWriter:
		v.schema = schema
	case *GaussDBWriter:
		v.schema = schema
	case *MySQLWriter:
		v.schema = schema
	case *OracleWriter:
		v.schema = schema
	case *DaMengWriter:
		v.schema = schema
	case *DaMengNativeWriter:
		v.schema = schema
	case *SQLServerWriter:
		v.schema = schema
	case *OscarWriter:
		v.schema = schema
	}
}

func uncertainRepairError(err error) bool {
	var network net.Error
	var state interface{ SQLState() string }
	if errors.As(err, &state) && (strings.HasPrefix(state.SQLState(), "08") || state.SQLState() == "40003") {
		return true
	}
	if err != nil {
		message := strings.ToLower(err.Error())
		for _, marker := range []string{"connection reset", "connection lost", "broken pipe", "bridge unavailable", "bridge exited", "bridge protocol"} {
			if strings.Contains(message, marker) {
				return true
			}
		}
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, driver.ErrBadConn) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &network)
}

func repairErrorStatus(err error) string {
	var manualInvalid manualTriggerInvalidError
	if errors.As(err, &manualInvalid) {
		return "failed"
	}
	var manualUnknown manualTriggerUnknownError
	if errors.As(err, &manualUnknown) {
		return "unknown"
	}
	if uncertainRepairError(err) {
		return "unknown"
	}
	return "failed"
}

// ValidatePGUniqueConstraintRepair binds a generated ALTER TABLE to the saved
// index object and its column plan. The generic table DDL guard does not check
// the name or keys of constraints inside an ALTER TABLE statement.
func ValidatePGUniqueConstraintRepair(obj repair.Object, units []repair.Unit, dbType string) error {
	idx := obj.Index
	if !source.IsPGFamily(dbType) || obj.Kind != "index" || obj.Schema == "" || obj.Table == "" ||
		idx == nil || !idx.PGUniqueConstraint || !idx.IsUnique || idx.IsPrimary || idx.Unsupported != "" ||
		idx.TableName != obj.Table || len(idx.Columns) != 2 || idx.Columns[0] == "" || idx.Columns[1] == "" ||
		idx.Columns[0] == idx.Columns[1] {
		return fmt.Errorf("联合唯一约束与原修复对象不一致")
	}
	d := dialect.NewPostgres(dbType)
	if obj.TargetName == "" || obj.TargetName != d.IndexName(*idx) {
		return fmt.Errorf("联合唯一约束名称与原修复对象不一致")
	}
	sql := dialect.JoinSQL(d.IndexStatements(obj.Schema, *idx))
	if sql == "" || len(units) != 1 || len(obj.Units) != 1 || units[0].ID != obj.Units[0].ID ||
		units[0].SQL != sql || units[0].OriginalSQL != sql || obj.Units[0].OriginalSQL != sql {
		return fmt.Errorf("联合唯一约束修复须保留原表、约束名称及两列定义")
	}
	return nil
}

// ExecuteRepair executes the stored units verbatim. A PL/SQL body is one unit;
// neither semicolons nor client-side delimiters are used to split SQL here.
// The caller validates statement scope and supplies an immutable object identity.
func ExecuteRepair(ctx context.Context, w Writer, obj repair.Object, units []repair.Unit, onStep func([]repair.Unit) error) ([]repair.Unit, error) {
	if obj.ManualTrigger != nil {
		return executeManualTriggerRepair(ctx, w, obj, units, onStep)
	}
	if native, ok := w.(*DaMengNativeWriter); ok {
		return native.executeNativeRepair(ctx, obj, units, onStep)
	}
	if obj.Kind == "trigger" && obj.MySQLPGTrigger != nil {
		return executeMySQLPGTriggerRepair(ctx, w, obj, units, onStep)
	}
	if obj.Kind == "trigger" && obj.MySQLDamengTrigger != nil {
		return executeMySQLDamengTriggerRepair(ctx, w, obj, units, onStep)
	}
	out := slices.Clone(units)
	persist := func() error {
		if onStep == nil {
			return nil
		}
		return onStep(slices.Clone(out))
	}
	a, err := repairAccess(w)
	if err != nil {
		return out, err
	}
	schema, err := ResolveRepairSchema(ctx, w)
	if err != nil {
		return out, err
	}
	if obj.Schema == "" || obj.Schema != schema {
		return out, fmt.Errorf("目标 schema 与原修复对象不一致")
	}
	generatedUnique := obj.Index != nil && obj.Index.PGUniqueConstraint
	if generatedUnique {
		if err := ValidatePGUniqueConstraintRepair(obj, units, w.DBType()); err != nil {
			return out, err
		}
	}
	if mysql, ok := w.(*MySQLWriter); ok && repairKind(obj.Kind) == "sequence" && obj.Sequence != nil && obj.Sequence.OracleMySQL != nil {
		return mysql.executeOracleMySQLAutoRepair(ctx, obj, out, onStep)
	}
	if obj.PartitionPlan != nil && len(obj.PartitionPlan.KeyColumns) == 0 {
		// Legacy cross-product KEY plans lack the domain proof needed to make
		// any repair executable. Reject before opening a DDL session.
		if err = VerifyPartitionKeyColumns(ctx, w, obj.TargetName, obj.PartitionPlan); err != nil {
			return out, err
		}
	}
	if dm, ok := w.(*DaMengWriter); ok && repairKind(obj.Kind) == "sequence" && obj.Sequence != nil && obj.Sequence.Native != nil && !repairDefinitionEdited(obj) {
		return dm.executeOracleDamengSequenceRepair(ctx, obj, out, onStep)
	}
	selected := []int{}
	for i, u := range out {
		if u.Status == "succeeded" {
			out[i].Selected = false
			continue
		}
		if u.Selected {
			if strings.TrimSpace(u.SQL) == "" {
				return out, fmt.Errorf("执行步骤 %d SQL 为空", i+1)
			}
			selected = append(selected, i)
		}
	}
	if len(selected) == 0 {
		return out, fmt.Errorf("未选择执行步骤")
	}
	if a.family == "postgres" {
		if err := preparePGSpatialRepair(ctx, a.db, obj); err != nil {
			return out, err
		}
	}
	if a.family == "postgres" && repairKind(obj.Kind) == "view" {
		// Preparation is best effort. Let the selected view SQL determine whether
		// it actually depends on get_uuid; unrelated views must remain repairable.
		if _, err := EnsureCompatibilityFunctions(ctx, w); err != nil && ctx.Err() != nil {
			return out, ctx.Err()
		}
	}
	var tx *sql.Tx
	var exec func(string) error
	var releaseIntervalSession func()
	onlyIntervalMarker := len(selected) == 1 && obj.PartitionPlan != nil && obj.PartitionPlan.Interval != nil && out[selected[0]].SQL == partition.IntervalMaterializationSQL(obj.PartitionPlan)
	if generatedUnique {
		// Use the same strict creation and duplicate verification as migration.
		// The validation above proved that this executes the saved SQL.
		exec = func(string) error { return w.CreateIndex(ctx, *obj.Index) }
	} else if onlyIntervalMarker {
		// The typed executor qualifies every object itself. A marker-only retry
		// needs no SQL session and must work with a single-connection pool.
		exec = func(string) error { return fmt.Errorf("INTERVAL 物化标记不能作为原始 SQL 执行") }
	} else if oscar, ok := w.(*OscarWriter); ok {
		exec = func(q string) error { return oscar.bridge.Exec(ctx, oscar.session, q) }
	} else {
		var conn *sql.Conn
		var close func()
		if sw, ok := w.(*SQLServerWriter); ok {
			conn, close, err = sw.session(ctx)
		} else if mw, ok := w.(*MySQLWriter); ok && source.IsPGFamily(mw.srcType) {
			conn, close, err = mw.pgSession(ctx)
		} else {
			conn, err = a.db.Conn(ctx)
			if err == nil {
				close = func() { _ = conn.Raw(func(any) error { return driver.ErrBadConn }); _ = conn.Close() }
			}
		}
		if err != nil {
			return out, err
		}
		closeSession := close
		closed := false
		close = func() {
			if !closed {
				closed = true
				closeSession()
			}
		}
		defer close()
		if a.family == "dameng" || obj.PartitionPlan != nil && len(obj.PartitionPlan.KeyColumns) > 0 {
			releaseIntervalSession = close
		}
		// Constrain unqualified identifiers on the same physical connection as
		// every repair unit. Dispose changed sessions rather than leaking state.
		sessionSQL := ""
		switch a.family {
		case "postgres":
			if repairKind(obj.Kind) != "view" {
				sessionSQL = "SET search_path TO " + w.Dialect().QuoteIdent(schema)
			}
		case "oracle":
			sessionSQL = "ALTER SESSION SET CURRENT_SCHEMA = " + w.Dialect().QuoteIdent(schema)
		case "dameng":
			sessionSQL = "SET SCHEMA " + w.Dialect().QuoteIdent(schema)
		case "mysql":
			// pgSession restores sql_mode/time_zone and closes its connection. Mark
			// it non-reusable first so USE cannot leak its database to another job.
			defer func() { _ = conn.Raw(func(any) error { return driver.ErrBadConn }) }()
			sessionSQL = "USE " + w.Dialect().QuoteIdent(schema)
		}
		if sessionSQL != "" {
			if _, err = conn.ExecContext(ctx, sessionSQL); err != nil {
				return out, err
			}
		}
		// Generated MySQL comment candidates use standard doubled quotes while
		// preserving literal backslashes from the complete SHOW CREATE definition.
		if a.family == "mysql" && repairKind(obj.Kind) == "comment" {
			if _, err = conn.ExecContext(ctx, "SET SESSION sql_mode=CONCAT(@@SESSION.sql_mode, ',NO_BACKSLASH_ESCAPES')"); err != nil {
				return out, err
			}
		}
		// Views need their source identifiers resolved under the original schema.
		if a.family == "sqlserver" || a.family == "postgres" && repairKind(obj.Kind) == "view" {
			tx, err = conn.BeginTx(ctx, nil)
			if err != nil {
				return out, err
			}
			defer tx.Rollback()
			if a.family == "postgres" {
				if _, err = tx.ExecContext(ctx, "SET LOCAL search_path TO "+w.Dialect().QuoteIdent(schema)); err != nil {
					return out, err
				}
			}
			exec = func(q string) error { _, e := tx.ExecContext(ctx, q); return e }
		} else {
			exec = func(q string) error { _, e := conn.ExecContext(ctx, q); return e }
		}
	}
	executed := []int{}
	rollback := func() {
		if tx == nil {
			return
		}
		e := tx.Rollback()
		state := "rolled_back"
		if e != nil {
			state = "unknown"
		}
		for _, n := range executed {
			out[n].Status = state
			if e != nil {
				out[n].Error = e.Error()
			}
		}
	}
	for _, i := range selected {
		if err = ctx.Err(); err != nil {
			rollback()
			_ = persist()
			return out, err
		}
		out[i].Status, out[i].Error = "running", ""
		if err = persist(); err != nil {
			out[i].Status = "pending"
			rollback()
			return out, fmt.Errorf("保存执行前状态失败，未执行该步骤: %w", err)
		}
		if ow, ok := w.(*OracleWriter); ok {
			err = ow.validateSQLText(out[i].SQL)
		}
		if err == nil {
			if obj.PartitionPlan != nil && obj.PartitionPlan.Interval != nil && out[i].SQL == partition.IntervalMaterializationSQL(obj.PartitionPlan) {
				materializer, ok := w.(PartitionIntervalMaterializer)
				if !ok || i != len(out)-1 || obj.PartitionPlan.Interval.Table != obj.TargetName || obj.PartitionPlan.Interval.Schema != obj.Schema {
					err = fmt.Errorf("INTERVAL 物化单元不属于目标冻结表或不是最终步骤")
				} else {
					if releaseIntervalSession != nil {
						releaseIntervalSession()
						releaseIntervalSession = nil
					}
					err = materializer.MaterializePartitionIntervals(ctx, obj.PartitionPlan)
				}
			} else {
				err = exec(out[i].SQL)
			}
		}
		if err != nil {
			out[i].Status, out[i].Error = repairErrorStatus(err), err.Error()
			rollback()
			pe := persist()
			return out, errors.Join(err, pe)
		}
		executed = append(executed, i)
		if tx == nil {
			out[i].Status = "succeeded"
		}
		if err = persist(); err != nil {
			rollback()
			if tx == nil {
				out[i].Status, out[i].Error = "unknown", err.Error()
			}
			return out, fmt.Errorf("SQL 已执行，但保存结果失败: %w", err)
		}
	}
	if tx != nil {
		err = tx.Commit()
		for _, i := range executed {
			out[i].Status = "succeeded"
			if err != nil {
				out[i].Status, out[i].Error = "unknown", err.Error()
			}
		}
		if pe := persist(); err != nil || pe != nil {
			for _, i := range executed {
				out[i].Status = "unknown"
				if pe != nil {
					out[i].Error = pe.Error()
				}
			}
			return out, errors.Join(err, pe)
		}
	}
	if obj.PartitionPlan != nil && (obj.PartitionPlan.Interval != nil || len(obj.PartitionPlan.KeyColumns) > 0 || partitionConvertedKeyNeedsColumns(obj.PartitionPlan.Source, obj.PartitionPlan.Target)) {
		if releaseIntervalSession != nil {
			releaseIntervalSession()
			releaseIntervalSession = nil
		}
		inspector, ok := w.(PartitionInspector)
		if !ok {
			return out, fmt.Errorf("分区修复缺少最终目录核验")
		}
		actual, e := inspector.InspectPartitionTable(ctx, obj.TargetName)
		if e != nil {
			return out, e
		}
		if e = partition.Equivalent(obj.PartitionPlan.Target, actual); e != nil {
			return out, e
		}
		if e = VerifyPartitionKeyColumns(ctx, w, obj.TargetName, obj.PartitionPlan); e != nil {
			return out, e
		}
		if e = ctx.Err(); e != nil {
			return out, e
		}
	}
	invalidateRecoveryCache(w)
	return out, nil
}

func repairKind(kind string) string {
	switch kind {
	case "tables":
		return "table"
	case "primary_keys", "primaryKeys":
		return "primary_key"
	case "indexes":
		return "index"
	case "foreign_keys", "constraints":
		return "foreign_key"
	case "views":
		return "view"
	case "sequences":
		return "sequence"
	case "comments":
		return "comment"
	}
	return kind
}

func invalidateRecoveryCache(w Writer) {
	switch v := w.(type) {
	case *OracleWriter:
		v.mu.Lock()
		clear(v.columns)
		v.mu.Unlock()
	case *DaMengWriter:
		v.identMu.Lock()
		clear(v.identCache)
		v.identMu.Unlock()
	}
}
