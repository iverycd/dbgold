package target

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/repair"
)

func (w *DaMengNativeWriter) materializedHistoryPlan(object damengnative.Object, unit damengnative.Unit) (*damengnative.MaterializedHistoryPlan, error) {
	if object.Schema != w.schema || object.Kind != "materialized_view" || object.Materialized == nil || object.Materialized.History == nil {
		return nil, fmt.Errorf("缺少属于目标模式的冻结物化历史组")
	}
	p := object.Materialized.History
	if p.Version != 1 || p.Schema != w.schema || len(p.Views) == 0 || unit.Phase != "mv_history" || unit.SQL != damengnative.MaterializedHistorySQL(w.schema, object.Name, p) {
		return nil, fmt.Errorf("物化历史组执行单元与冻结内容不一致")
	}
	found := false
	for _, u := range object.Units {
		found = found || u.Phase == unit.Phase && u.SQL == unit.SQL
	}
	if !found {
		return nil, fmt.Errorf("物化历史组不在冻结执行清单中")
	}
	for _, v := range p.Views {
		if v.Name == "" || v.Table == "" || v.Staging == "" || len(v.Columns) == 0 || !v.Current && len(historyViewBuffers(v)) == 0 && v.Strategy != "redefinition" || len(v.RowIDColumns) != len(v.RowIDTables) {
			return nil, fmt.Errorf("物化历史组快照字段不完整")
		}
		if v.Strategy == "redefinition" {
			if err := historyRedefinitionFields(v); err != nil {
				return nil, err
			}
		}
		if _, e := time.Parse("2006-01-02 15:04:05.000000", v.SourceRefresh); e != nil {
			return nil, fmt.Errorf("原始物化刷新水位格式不合法")
		}
	}
	return p, nil
}

// RestoreDamengNativeMaterializedHistory imports a stale snapshot and its
// pending native logs. No refresh touches the source, and protected dictionary
// rows are never modified. Disposable partition buffers are refilled from
// immutable staging before every attempt, making a whole-group retry safe even
// after a lost EXCHANGE response.
func (w *DaMengNativeWriter) RestoreDamengNativeMaterializedHistory(ctx context.Context, object damengnative.Object, unit damengnative.Unit) (retErr error) {
	p, err := w.materializedHistoryPlan(object, unit)
	if err != nil {
		return err
	}
	conn, err := w.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Raw(func(any) error { return driver.ErrBadConn }); _ = conn.Close() }()
	if _, err = conn.ExecContext(ctx, "SET SCHEMA "+damengnative.Quote(w.schema)); err != nil {
		return err
	}
	if err = repair.Observe(ctx, unit.SQL, "running", nil); err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, repair.Observe(ctx, unit.SQL, repairErrorStatus(retErr), retErr))
		}
	}()
	for _, v := range p.Views {
		if v.Strategy == "redefinition" {
			if err = historyReconcileRedefinition(ctx, conn, w.schema, v); err != nil {
				return err
			}
		}
	}
	// Mapped identities for already deleted source rows are stable across retry.
	// A valid native ROWID token need not refer to a surviving row, but must not
	// collide with any copied current base row.
	if err = w.historyMissingRowIDs(ctx, conn, p); err != nil {
		return err
	}
	for _, v := range p.Views {
		if v.Strategy == "redefinition" {
			projection, e := historyProjection(p, v.Staging, v.Columns, v.RowIDColumns, v.RowIDTables)
			if e != nil {
				return e
			}
			if err = historyFillBuffer(ctx, conn, w.schema, v.Redefinition.WorkingTable, projection, v.Columns, false); err != nil {
				return err
			}
			if err = historyBag(ctx, conn, w.schema, v.Redefinition.WorkingTable, projection, v.Columns); err != nil {
				return err
			}
		}
		for _, b := range historyViewBuffers(v) {
			projection, err := historyProjection(p, v.Staging, v.Columns, v.RowIDColumns, v.RowIDTables)
			if err != nil {
				return err
			}
			projection = historyPartitionProjection(projection, b)
			if err = historyFillBuffer(ctx, conn, w.schema, b.Name, projection, v.Columns, false); err != nil {
				return fmt.Errorf("物化视图 %s 原快照缓冲失败: %w", v.Name, err)
			}
			if err = historyBag(ctx, conn, w.schema, b.Name, projection, v.Columns); err != nil {
				return fmt.Errorf("物化视图 %s 交换前完整快照核验失败: %w", v.Name, err)
			}
		}
	}
	var targetCutoffs []string
	for _, v := range p.Views {
		if _, err = conn.ExecContext(ctx, "ALTER MATERIALIZED VIEW "+damengnative.Qualified(w.schema, v.Name)+" REFRESH COMPLETE ON DEMAND DISABLE QUERY REWRITE"); err != nil {
			return err
		}
		if _, err = conn.ExecContext(ctx, damengnative.MaterializedBaselineSQL(w.schema, v.Name)); err != nil {
			return err
		}
		var last string
		if err = conn.QueryRowContext(ctx, `SELECT TO_CHAR(LAST_REFRESH_DATE,'YYYY-MM-DD HH24:MI:SS.FF6') FROM SYS.USER_MVIEWS WHERE SCHID=(SELECT ID FROM SYS.SYSOBJECTS WHERE TYPE$='SCH' AND NAME=?) AND MVIEW_NAME=?`, w.schema, v.Name).Scan(&last); err != nil {
			return err
		}
		if len(targetCutoffs) > 0 && last <= targetCutoffs[len(targetCutoffs)-1] {
			return fmt.Errorf("目标原生刷新水位没有严格递增，整个物化历史组需要重新执行")
		}
		targetCutoffs = append(targetCutoffs, last)
		if err = historyClockBarrier(ctx, conn, last); err != nil {
			return fmt.Errorf("物化视图 %s 原生日志时间边界未稳定: %w", v.Name, err)
		}
		if v.Strategy == "redefinition" {
			if err = historyRebaseStorage(ctx, conn, w.schema, v); err != nil {
				return err
			}
		}
		for _, b := range historyViewBuffers(v) {
			if _, err = conn.ExecContext(ctx, historyExchangeSQL(w.schema, b, v.Table)); err != nil {
				return fmt.Errorf("物化视图 %s 恢复原存储快照失败: %w", v.Name, err)
			}
		}
		projection, _ := historyProjection(p, v.Staging, v.Columns, v.RowIDColumns, v.RowIDTables)
		if err = historyBag(ctx, conn, w.schema, v.Name, projection, v.Columns); err != nil {
			return fmt.Errorf("物化视图 %s 交换后完整快照核验失败: %w", v.Name, err)
		}
		if v.EmptyTouch != nil {
			if err = nativeHistoryEmptyTouch(ctx, conn, w.schema, v.EmptyTouch); err != nil {
				return fmt.Errorf("物化视图 %s 空基表原生陈旧状态恢复失败: %w", v.Name, err)
			}
		} else if len(v.TouchSQL) > 0 {
			if err = nativeHistoryTouch(ctx, conn, w.schema, v.TouchSQL); err != nil {
				return fmt.Errorf("物化视图 %s 原生陈旧状态恢复失败: %w", v.Name, err)
			}
		}
		if err = historyRestoreUnusableState(ctx, conn, w.schema, v); err != nil {
			return fmt.Errorf("物化视图 %s 原生完全刷新要求恢复失败: %w", v.Name, err)
		}
	}
	for _, log := range p.Logs {
		projection, err := historyLogProjection(p, log, targetCutoffs)
		if err != nil {
			return err
		}
		if err = historyFillBuffer(ctx, conn, w.schema, log.Buffer.Name, projection, log.Columns, true); err != nil {
			return fmt.Errorf("物化日志 %s 原历史缓冲失败: %w", log.Table, err)
		}
		if err = historyBag(ctx, conn, w.schema, log.Buffer.Name, projection, log.Columns); err != nil {
			return err
		}
		if _, err = conn.ExecContext(ctx, historyExchangeSQL(w.schema, log.Buffer, log.Table)); err != nil {
			return fmt.Errorf("物化日志 %s 原历史交换失败: %w", log.Table, err)
		}
		if err = historyBag(ctx, conn, w.schema, log.Table, projection, log.Columns); err != nil {
			return fmt.Errorf("物化日志 %s 原历史完整核验失败: %w", log.Table, err)
		}
	}
	for _, v := range p.Views {
		if v.NativeStaleness == "" {
			continue
		}
		var state string
		for attempt := 0; attempt < 4; attempt++ {
			if err = conn.QueryRowContext(ctx, `SELECT STALENESS FROM SYS.USER_MVIEWS WHERE SCHID=(SELECT ID FROM SYS.SYSOBJECTS WHERE TYPE$='SCH' AND NAME=?) AND MVIEW_NAME=?`, w.schema, v.Name).Scan(&state); err != nil {
				return err
			}
			if state == v.NativeStaleness {
				break
			}
		}
		if state != v.NativeStaleness {
			return fmt.Errorf("物化视图 %s 原生状态不同：源=%s 目标=%s", v.Name, v.NativeStaleness, state)
		}
	}
	if len(targetCutoffs) > 0 {
		if err = historyClockBarrier(ctx, conn, targetCutoffs[len(targetCutoffs)-1]); err != nil {
			return err
		}
	}
	return repair.Observe(ctx, unit.SQL, "succeeded", nil)
}

// A source requiring COMPLETE must retain that requirement. NEVER invalidates
// only this view's newly initialized baseline without changing its stored data
// or its siblings' baselines. Restoring FAST/FORCE policy later keeps this state;
// a subsequent FORCE still chooses the engine's ordinary COMPLETE fallback.
func historyRestoreUnusableState(ctx context.Context, conn *sql.Conn, schema string, view damengnative.MaterializedHistoryView) error {
	if view.NativeStaleness != "NEEDS_FULL_REFRESH" {
		return nil
	}
	_, err := conn.ExecContext(ctx, "ALTER MATERIALIZED VIEW "+damengnative.Qualified(schema, view.Name)+" NEVER REFRESH")
	return err
}

// DM8's native MV log watermark clock is coarser than LAST_REFRESH_DATE. On
// affected builds an immediate first FAST refresh in a second's first 100ms
// can purge history needed by a sibling MV. Wait for a server-observed natural
// second plus 110ms; local wall-clock time is never used to establish readiness.
func historyClockBarrier(ctx context.Context, conn *sql.Conn, cutoff string) error {
	parsed, err := time.Parse("2006-01-02 15:04:05.000000", cutoff)
	if err != nil {
		return err
	}
	boundary := parsed.Truncate(time.Second).Add(time.Second + 110*time.Millisecond).Format("2006-01-02 15:04:05.000000")
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for attempt := 0; attempt < 250; attempt++ {
		if err = bounded.Err(); err != nil {
			return err
		}
		var now string
		if err = conn.QueryRowContext(bounded, `SELECT TO_CHAR(NOW(6),'YYYY-MM-DD HH24:MI:SS.FF6') FROM DUAL`).Scan(&now); err != nil {
			return err
		}
		if _, err = time.Parse("2006-01-02 15:04:05.000000", now); err != nil {
			return fmt.Errorf("服务端原生日志时钟不可解析: %w", err)
		}
		if now >= boundary {
			return nil
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-bounded.Done():
			timer.Stop()
			return bounded.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("服务端原生日志时间边界在有界等待内没有推进")
}

func historyExchangeSQL(schema string, b damengnative.MaterializedExchangeBuffer, table string) string {
	if b.TablePartition != "" {
		kind := "PARTITION"
		if b.Subpartition {
			kind = "SUBPARTITION"
		}
		return "ALTER TABLE " + damengnative.Qualified(schema, table) + " EXCHANGE " + kind + " " + damengnative.Quote(b.TablePartition) + " WITH TABLE " + damengnative.Qualified(schema, b.Name)
	}
	return "ALTER TABLE " + damengnative.Qualified(schema, b.Name) + " EXCHANGE PARTITION " + damengnative.Quote(b.Partition) + " WITH TABLE " + damengnative.Qualified(schema, table)
}

func historyViewBuffers(v damengnative.MaterializedHistoryView) []damengnative.MaterializedExchangeBuffer {
	if len(v.Buffers) > 0 {
		return v.Buffers
	}
	if v.Buffer.Name != "" {
		return []damengnative.MaterializedExchangeBuffer{v.Buffer}
	}
	return nil
}

func historyPartitionProjection(projection string, b damengnative.MaterializedExchangeBuffer) string {
	if b.TablePartition == "" {
		return projection
	}
	kind := "PARTITION"
	if b.Subpartition {
		kind = "SUBPARTITION"
	}
	return strings.TrimSuffix(projection, " S") + " " + kind + "(" + damengnative.Quote(b.TablePartition) + ") S"
}

func historyColumnNames(columns []damengnative.Column) []string {
	out := make([]string, len(columns))
	for i, c := range columns {
		out[i] = damengnative.Quote(c.Name)
	}
	return out
}

func historyFillBuffer(ctx context.Context, conn *sql.Conn, schema, name, projection string, columns []damengnative.Column, identity bool) (retErr error) {
	q := damengnative.Qualified(schema, name)
	var writable []string
	for _, c := range columns {
		if c.Writable {
			writable = append(writable, damengnative.Quote(c.Name))
			identity = identity || c.Identity
		}
	}
	if len(writable) == 0 {
		return fmt.Errorf("物化交换缓冲没有显式可写列")
	}
	if _, err := conn.ExecContext(ctx, "TRUNCATE TABLE "+q); err != nil {
		return err
	}
	if identity {
		if _, err := conn.ExecContext(ctx, "SET IDENTITY_INSERT "+q+" ON"); err != nil {
			return err
		}
		defer func() {
			clean, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, e := conn.ExecContext(clean, "SET IDENTITY_INSERT "+q+" OFF")
			retErr = errors.Join(retErr, e)
		}()
	}
	_, err := conn.ExecContext(ctx, "INSERT INTO "+q+" ("+strings.Join(writable, ",")+") SELECT "+strings.Join(writable, ",")+" FROM ("+projection+") AS \"DBGOLD_HISTORY_ROWS\" ("+strings.Join(historyColumnNames(columns), ",")+")")
	return err
}

func historyBag(ctx context.Context, conn *sql.Conn, schema, name, projection string, columns []damengnative.Column) error {
	return damengnative.CheckMaterializedBag(ctx, conn, schema, name, projection, func() []string {
		a := make([]string, len(columns))
		for i, c := range columns {
			a[i] = c.Name
		}
		return a
	}(), nil)
}

func historyProjection(p *damengnative.MaterializedHistoryPlan, staging string, columns []damengnative.Column, rowids []int, tables []string) (string, error) {
	expressions := make([]string, len(columns))
	for i, c := range columns {
		expressions[i] = "S." + damengnative.Quote(c.Name)
	}
	for j, i := range rowids {
		if i < 0 || i >= len(columns) || j >= len(tables) {
			return "", fmt.Errorf("ROWID输出映射列不完整")
		}
		mapping := ""
		for _, m := range p.RowIDMaps {
			if m.BaseTable == tables[j] {
				mapping = m.Table
			}
		}
		if mapping == "" {
			return "", fmt.Errorf("ROWID基表 %s 缺少冻结的物理身份映射", tables[j])
		}
		source := expressions[i]
		if strings.EqualFold(columns[i].DataType, "ROWID") {
			source = "ROWIDTOCHAR(" + source + ")"
		}
		expressions[i] = "(SELECT M.\"TargetRID\" FROM " + damengnative.Qualified(p.Schema, mapping) + " M WHERE M.\"SourceRID\"=" + source + ")"
		if strings.EqualFold(columns[i].DataType, "ROWID") {
			expressions[i] = "CHARTOROWID(" + expressions[i] + ")"
		}
	}
	return "SELECT " + strings.Join(expressions, ",") + " FROM " + damengnative.Qualified(p.Schema, staging) + " S", nil
}

func historyLogProjection(p *damengnative.MaterializedHistoryPlan, log damengnative.MaterializedHistoryLog, targets []string) (string, error) {
	var rowids []int
	var tables []string
	for i, c := range log.Columns {
		if strings.EqualFold(c.Name, "M_ROW$$") {
			rowids = append(rowids, i)
			tables = append(tables, log.BaseTable)
		}
	}
	// Rebuild expressions rather than editing SQL text: values and quoted names
	// are never scanned for apparent keywords.
	ts := make([]string, len(log.Columns))
	for i, c := range log.Columns {
		ts[i] = "S." + damengnative.Quote(c.Name)
	}
	if len(rowids) > 0 {
		for j, i := range rowids {
			m := ""
			for _, x := range p.RowIDMaps {
				if x.BaseTable == tables[j] {
					m = x.Table
				}
			}
			if m == "" {
				return "", fmt.Errorf("物化日志 ROWID 基表缺少冻结映射")
			}
			source := "S." + damengnative.Quote(log.Columns[i].Name)
			if strings.EqualFold(log.Columns[i].DataType, "ROWID") {
				source = "ROWIDTOCHAR(" + source + ")"
			}
			ts[i] = "(SELECT M.\"TargetRID\" FROM " + damengnative.Qualified(p.Schema, m) + " M WHERE M.\"SourceRID\"=" + source + ")"
			if strings.EqualFold(log.Columns[i].DataType, "ROWID") {
				ts[i] = "CHARTOROWID(" + ts[i] + ")"
			}
		}
	}
	var clauses []string
	for i, v := range p.Views {
		if i >= len(targets) {
			return "", fmt.Errorf("目标刷新水位不完整")
		}
		target, e := time.Parse("2006-01-02 15:04:05.000000", targets[i])
		if e != nil {
			return "", e
		}
		mapped := target.Add(-time.Microsecond)
		if i > 0 {
			prior, e := time.Parse("2006-01-02 15:04:05.000000", targets[i-1])
			if e != nil {
				return "", e
			}
			mapped = prior.Add(time.Microsecond)
			if !mapped.Before(target) {
				return "", fmt.Errorf("目标刷新水位间隔不足以保留逐视图日志边界")
			}
		}
		clauses = append(clauses, "WHEN S.\"SNAPTIME$$\"<=TIMESTAMP '"+v.SourceRefresh+"' THEN TIMESTAMP '"+mapped.Format("2006-01-02 15:04:05.000000")+"'")
	}
	for i, c := range log.Columns {
		if c.Name == "SNAPTIME$$" {
			ts[i] = "CASE " + strings.Join(clauses, " ") + " ELSE TIMESTAMP '4000-01-01 00:00:00.000000' END"
		}
		if c.Name == "SEQUENCE$$" {
			ts[i] = `ROW_NUMBER() OVER(ORDER BY S."SEQUENCE$$")-COUNT(*) OVER()-1`
		}
	}
	return "SELECT " + strings.Join(ts, ",") + " FROM " + damengnative.Qualified(p.Schema, log.Staging) + " S", nil
}

func (w *DaMengNativeWriter) historyMissingRowIDs(ctx context.Context, conn *sql.Conn, p *damengnative.MaterializedHistoryPlan) error {
	for _, m := range p.RowIDMaps {
		var sources []string
		for _, log := range p.Logs {
			if log.BaseTable == m.BaseTable {
				for _, c := range log.Columns {
					if strings.EqualFold(c.Name, "M_ROW$$") {
						value := damengnative.Quote(c.Name)
						if strings.EqualFold(c.DataType, "ROWID") {
							value = "ROWIDTOCHAR(" + value + ")"
						}
						sources = append(sources, "SELECT "+value+" AS R FROM "+damengnative.Qualified(p.Schema, log.Staging)+" WHERE "+damengnative.Quote(c.Name)+" IS NOT NULL")
					}
				}
			}
		}
		for _, v := range p.Views {
			for j, i := range v.RowIDColumns {
				if v.RowIDTables[j] == m.BaseTable {
					c := damengnative.Quote(v.Columns[i].Name)
					sources = append(sources, "SELECT ROWIDTOCHAR("+c+") AS R FROM "+damengnative.Qualified(p.Schema, v.Staging)+" WHERE "+c+" IS NOT NULL")
				}
			}
		}
		if len(sources) == 0 {
			continue
		}
		mapping := damengnative.Qualified(p.Schema, m.Table)
		candidate := int64(math.MaxInt64)
		for {
			rows, err := conn.QueryContext(ctx, "SELECT DISTINCT X.R FROM ("+strings.Join(sources, " UNION ALL ")+") X WHERE NOT EXISTS(SELECT 1 FROM "+mapping+" M WHERE M.\"SourceRID\"=X.R) AND ROWNUM<=256")
			if err != nil {
				return err
			}
			var missing []string
			for rows.Next() {
				var rid string
				if err = rows.Scan(&rid); err != nil {
					break
				}
				missing = append(missing, rid)
			}
			iteration := rows.Err()
			_ = rows.Close()
			if err != nil {
				return err
			}
			if iteration != nil {
				return iteration
			}
			if len(missing) == 0 {
				break
			}
			for _, source := range missing {
				for {
					var token string
					var collision int
					if err = conn.QueryRowContext(ctx, "SELECT ROWIDTOCHAR(CAST(? AS ROWID)) FROM DUAL", candidate).Scan(&token); err != nil {
						return err
					}
					candidate--
					if err = conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+mapping+" WHERE \"TargetRID\"=?", token).Scan(&collision); err != nil {
						return err
					}
					if collision != 0 {
						continue
					}
					var roundtrip string
					if err = conn.QueryRowContext(ctx, "SELECT ROWIDTOCHAR(CHARTOROWID(?)) FROM DUAL", token).Scan(&roundtrip); err != nil || roundtrip != token {
						return fmt.Errorf("已删除行的原生ROWID映射不可表示: %w", err)
					}
					if _, err = conn.ExecContext(ctx, "INSERT INTO "+mapping+" (\"SourceRID\",\"TargetRID\") VALUES(?,?)", source, token); err != nil {
						return err
					}
					break
				}
			}
		}
	}
	return nil
}

func (w *DaMengNativeWriter) ReconcileDamengNativeMaterializedHistory(ctx context.Context, object damengnative.Object, unit damengnative.Unit) (done, retry bool, err error) {
	p, err := w.materializedHistoryPlan(object, unit)
	if err != nil {
		return false, false, err
	}
	for _, v := range p.Views {
		var n int64
		if err = w.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+damengnative.Qualified(w.schema, v.Staging)).Scan(&n); err != nil {
			return false, false, err
		}
	}
	// The full group is rebuilt from immutable staging. A catalog object or an
	// exchanged buffer alone never proves that the pending history was imported.
	return false, true, nil
}
