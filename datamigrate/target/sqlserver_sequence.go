package target

import (
	"context"
	"database/sql"
	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

type sqlServerNativeState struct {
	id                               int64
	step, min, max, current, initial string
	cycle, exhausted, cached         bool
	cache                            sql.NullInt64
	marker                           sql.NullString
}

func (w *SQLServerWriter) createSQLServerNativeSequence(ctx context.Context, s source.SequenceInfo) error {
	return w.applySQLServerNativeSequence(ctx, s, !s.SQLServerCalibrationOnly)
}

func (w *SQLServerWriter) applySQLServerNativeSequence(ctx context.Context, s source.SequenceInfo, create bool) error {
	spec, err := dialect.ValidateSQLServerNativeSequence(s)
	if err != nil {
		return err
	}
	conn, release, err := w.session(ctx)
	if err != nil {
		return err
	}
	defer release()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	name := s.Native.Name
	qualified := w.dia.QualifyTable(w.schema, name)
	state, err := sqlServerSequenceState(ctx, tx, qualified)
	if err != nil {
		return err
	}
	if state.id == 0 && !create {
		return fmt.Errorf("目标序列 %s 不存在", qualified)
	}
	if state.id != 0 {
		if err := state.validate(spec, qualified); err != nil {
			return err
		}
		if create && !state.marker.Valid {
			return fmt.Errorf("目标序列 %s 已存在且无法确认迁移所有权", qualified)
		}
	}
	next := new(big.Int).Set(spec.Start)
	var attach []dialect.Statement
	bindings := map[[2]string]bool{}
	for _, b := range s.Native.Bindings {
		if b.Kind == "ownership" {
			continue
		}
		bound, err := w.sqlServerSequenceBinding(ctx, tx, b, state.id, name)
		if err != nil {
			return err
		}
		if !bound {
			if !create {
				return fmt.Errorf("目标列 %s.%s 缺少指向 %s 的直接默认值", b.TableName, b.ColumnName, qualified)
			}
			attach = append(attach, w.dia.SQLServerSequenceBindingStatement(w.schema, name, b))
		}
		bindings[[2]string{b.TableName, b.ColumnName}] = true
		if state.id != 0 {
			extreme, err := w.sqlServerSequenceExtreme(ctx, tx, b, spec.Increment)
			if err != nil {
				return err
			}
			if extreme != nil {
				next = farther(next, afterExtreme(extreme, spec.Increment), spec.Increment)
			}
		}
	}
	if state.id == 0 {
		statements := w.dia.SequenceStatements(w.schema, s, "")
		if len(statements) == 0 {
			return fmt.Errorf("序列 %s 的 SQL Server DDL 为空", qualified)
		}
		return execObservedTransaction(ctx, tx, statements)
	}
	if !state.marker.Valid && len(bindings) == 0 {
		return fmt.Errorf("目标独立序列 %s 缺少迁移所有权标记", qualified)
	}
	if err := w.checkSQLServerSequenceScope(ctx, tx, state.id, bindings); err != nil {
		return err
	}
	if err := reserveSQLServerSequence(ctx, tx, qualified, state, spec, next); err != nil {
		return err
	}
	return execObservedTransaction(ctx, tx, attach)
}

type sqlServerSequenceQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func sqlServerSequenceState(ctx context.Context, db sqlServerSequenceQuery, qualified string) (sqlServerNativeState, error) {
	var s sqlServerNativeState
	err := db.QueryRowContext(ctx, `SELECT q.object_id,CONVERT(varchar(50),q.increment),CONVERT(varchar(50),q.minimum_value),CONVERT(varchar(50),q.maximum_value),CONVERT(varchar(50),q.current_value),CONVERT(varchar(50),q.start_value),q.is_cycling,q.is_exhausted,q.is_cached,q.cache_size,CONVERT(varchar(128),ep.value) FROM sys.sequences q LEFT JOIN sys.extended_properties ep ON ep.class=1 AND ep.major_id=q.object_id AND ep.minor_id=0 AND ep.name=@p2 WHERE q.object_id=OBJECT_ID(@p1,N'SO')`, qualified, dialect.SQLServerSequenceMarker).Scan(&s.id, &s.step, &s.min, &s.max, &s.current, &s.initial, &s.cycle, &s.exhausted, &s.cached, &s.cache, &s.marker)
	if err == sql.ErrNoRows {
		return sqlServerNativeState{}, nil
	}
	return s, err
}

func (s sqlServerNativeState) validate(spec dialect.SQLServerNativeSequence, name string) error {
	if s.step != spec.Increment.String() || s.min != spec.Min.String() || s.max != spec.Max.String() || s.cycle || s.cached != (spec.Cache > 0) || s.cached && s.cache.Int64 != spec.Cache {
		return fmt.Errorf("目标序列 %s 的步长、边界、循环或缓存定义与源序列不一致", name)
	}
	if s.marker.Valid && s.marker.String != spec.Marker {
		return fmt.Errorf("目标序列 %s 的迁移标记与源序列定义不一致", name)
	}
	if s.exhausted {
		return fmt.Errorf("目标序列 %s 已耗尽", name)
	}
	return nil
}

func (s sqlServerNativeState) next(ctx context.Context, db sqlServerSequenceQuery, step *big.Int) (*big.Int, bool, error) {
	current, err := repairInteger(s.current)
	if err != nil {
		return nil, false, err
	}
	if s.current != s.initial {
		return current.Add(current, step), true, nil
	}
	// SQL Server 2012–2016 expose the same current_value before and after
	// the first issuance. Keep a conservative lower progress bound there.
	var available bool
	var last sql.NullString
	q := `IF COL_LENGTH(N'sys.sequences',N'last_used_value') IS NULL SELECT CAST(0 AS bit),CAST(NULL AS varchar(50)); ELSE EXEC sys.sp_executesql N'SELECT CAST(1 AS bit),CONVERT(varchar(50),last_used_value) FROM sys.sequences WHERE object_id=@id',N'@id int',@p1`
	if err := db.QueryRowContext(ctx, q, s.id).Scan(&available, &last); err != nil {
		return nil, false, err
	}
	if last.Valid {
		current.Add(current, step)
	}
	return current, available, nil
}

var sqlServerNextValuePrefix = regexp.MustCompile(`(?i)^NEXT\s+VALUE\s+FOR\s+`)

func (w *SQLServerWriter) sqlServerSequenceBinding(ctx context.Context, db sqlServerSequenceQuery, b source.SequenceBinding, seqID int64, name string) (bool, error) {
	var typ string
	var scale int64
	var identity, computed, proven bool
	var current sql.NullString
	q := `SELECT t.name,c.scale,c.is_identity,c.is_computed,dc.definition,CAST(CASE WHEN EXISTS (SELECT 1 FROM sys.sql_expression_dependencies d WHERE d.referencing_id=dc.object_id AND d.referenced_id=@p3) THEN 1 ELSE 0 END AS bit) FROM sys.columns c JOIN sys.types t ON c.user_type_id=t.user_type_id LEFT JOIN sys.default_constraints dc ON dc.parent_object_id=c.object_id AND dc.parent_column_id=c.column_id WHERE c.object_id=OBJECT_ID(@p1,N'U') AND c.name=@p2`
	err := db.QueryRowContext(ctx, q, w.dia.QualifyTable(w.schema, b.TableName), b.ColumnName, seqID).Scan(&typ, &scale, &identity, &computed, &current, &proven)
	if err == sql.ErrNoRows {
		return false, fmt.Errorf("目标序列绑定列 %s.%s 不存在", b.TableName, b.ColumnName)
	}
	if err != nil {
		return false, err
	}
	switch typ {
	case "tinyint", "smallint", "int", "bigint", "decimal", "numeric":
	default:
		return false, fmt.Errorf("目标序列绑定列 %s.%s 的 %s 类型不能可靠保留序列值", b.TableName, b.ColumnName, typ)
	}
	if identity || computed || scale != 0 {
		return false, fmt.Errorf("目标序列绑定列 %s.%s 不是普通整数列", b.TableName, b.ColumnName)
	}
	if !current.Valid {
		return false, nil
	}
	def := strings.TrimSpace(current.String)
	for strings.HasPrefix(def, "(") && strings.HasSuffix(def, ")") {
		def = strings.TrimSpace(def[1 : len(def)-1])
	}
	if prefix := sqlServerNextValuePrefix.FindStringIndex(def); prefix != nil && proven && seqID != 0 {
		ref := def[prefix[1]:]
		if ref == w.dia.QualifyTable(w.schema, name) || ref == w.dia.QuoteIdent(name) {
			return true, nil
		}
	}
	return false, fmt.Errorf("目标列 %s.%s 已存在其他默认值", b.TableName, b.ColumnName)
}

func (w *SQLServerWriter) sqlServerSequenceExtreme(ctx context.Context, db sqlServerSequenceQuery, b source.SequenceBinding, step *big.Int) (*big.Int, error) {
	agg := "MAX"
	if step.Sign() < 0 {
		agg = "MIN"
	}
	var n sql.NullString
	q := "SELECT CONVERT(varchar(50)," + agg + "(" + w.dia.QuoteIdent(b.ColumnName) + ")) FROM " + w.dia.QualifyTable(w.schema, b.TableName) + " WITH (TABLOCKX,HOLDLOCK)"
	if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return nil, err
	}
	if !n.Valid {
		return nil, nil
	}
	return repairInteger(n.String)
}

func (w *SQLServerWriter) checkSQLServerSequenceScope(ctx context.Context, tx *sql.Tx, id int64, bindings map[[2]string]bool) error {
	rows, err := tx.QueryContext(ctx, `SELECT s.name,t.name,c.name FROM sys.sql_expression_dependencies dep JOIN sys.default_constraints dc ON dc.object_id=dep.referencing_id JOIN sys.tables t ON t.object_id=dc.parent_object_id JOIN sys.schemas s ON s.schema_id=t.schema_id JOIN sys.columns c ON c.object_id=t.object_id AND c.column_id=dc.parent_column_id WHERE dep.referenced_id=@p1`, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var schema, table, col string
		if err := rows.Scan(&schema, &table, &col); err != nil {
			return err
		}
		if schema != w.schema || !bindings[[2]string{table, col}] {
			return fmt.Errorf("目标序列还有迁移范围外的默认值绑定 %s.%s.%s", schema, table, col)
		}
	}
	return rows.Err()
}

func reserveSQLServerSequence(ctx context.Context, tx *sql.Tx, name string, state sqlServerNativeState, spec dialect.SQLServerNativeSequence, bound *big.Int) error {
	current, known, err := state.next(ctx, tx, spec.Increment)
	if err != nil {
		return err
	}
	next := advanceProgression(current, bound, spec.Increment)
	if next.Cmp(spec.Min) < 0 || next.Cmp(spec.Max) > 0 {
		return fmt.Errorf("目标序列 %s 无法在边界内安全续号", name)
	}
	if next.Cmp(current) == 0 {
		return nil
	}
	worstNext := new(big.Int).Add(next, spec.Increment)
	if !known && (worstNext.Cmp(spec.Min) < 0 || worstNext.Cmp(spec.Max) > 0) {
		return fmt.Errorf("目标序列 %s 接近边界，无法保证校准后仍可发号", name)
	}
	count := new(big.Int).Quo(new(big.Int).Sub(next, current), spec.Increment)
	if !count.IsInt64() {
		return fmt.Errorf("目标序列 %s 要跨越的序列值超过 SQL Server 支持的范围", name)
	}
	// Reservation is atomic and can only advance the counter, including
	// when unrelated clients concurrently request NEXT VALUE FOR.
	_, err = execObserved(ctx, tx, `DECLARE @first sql_variant, @status int; EXEC @status=sys.sp_sequence_get_range @sequence_name=@p1,@range_size=@p2,@range_first_value=@first OUTPUT; IF @status<>0 RAISERROR (N'SEQUENCE range reservation failed',16,1)`, name, count.Int64())
	return err
}
