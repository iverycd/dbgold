package target

import (
	"context"
	"database/sql"
	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/typemap"
	"dbgold/datamigrate/valueconv"
	"fmt"
	"strconv"
	"strings"
)

func (w *OracleWriter) inspectCrossOracleSequence(ctx context.Context, s source.SequenceInfo) error {
	if err := dialect.ValidateCrossOracleSequence(s); err != nil {
		return err
	}
	var id int64
	var step, min, max, cache string
	var cycle, order string
	err := w.db.QueryRowContext(ctx, `SELECT o.OBJECT_ID,TO_CHAR(s.INCREMENT_BY),TO_CHAR(s.MIN_VALUE),TO_CHAR(s.MAX_VALUE),s.CYCLE_FLAG,s.ORDER_FLAG,TO_CHAR(s.CACHE_SIZE) FROM ALL_SEQUENCES s JOIN ALL_OBJECTS o ON o.OWNER=s.SEQUENCE_OWNER AND o.OBJECT_NAME=s.SEQUENCE_NAME AND o.OBJECT_TYPE='SEQUENCE' WHERE s.SEQUENCE_OWNER=:1 AND s.SEQUENCE_NAME=:2`, w.schema, s.Native.Name).Scan(&id, &step, &min, &max, &cycle, &order, &cache)
	if err == sql.ErrNoRows {
		if s.Native.OracleObjectID > 0 {
			return fmt.Errorf("持久化工具序列 %s 已不存在", s.Native.Name)
		}
		delete(w.dia.OwnedSequences, s.Native.Name)
		var count int
		if e := w.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_OBJECTS WHERE OWNER=:1 AND OBJECT_NAME=:2 AND OBJECT_TYPE IN ('TABLE','VIEW','SEQUENCE','SYNONYM','PROCEDURE','FUNCTION','PACKAGE','TYPE')`, w.schema, s.Native.Name).Scan(&count); e != nil {
			return e
		}
		if count != 0 {
			return fmt.Errorf("序列名称 %s 被已有目标对象占用", s.Native.Name)
		}
		return w.inspectCrossOracleBindings(ctx, s, false)
	}
	if err != nil {
		return err
	}
	if s.Native.OracleObjectID <= 0 || s.Native.OracleObjectID != id {
		return fmt.Errorf("序列 %s 已存在但缺少匹配的持久化工具身份", s.Native.Name)
	}
	if step != s.Native.IncrementBy || min != s.Native.MinValue || max != s.Native.MaxValue || cycle != "N" || order != "N" || cache != s.Native.CacheSize {
		return fmt.Errorf("序列 %s 定义偏离冻结计划", s.Native.Name)
	}
	w.dia.OwnedSequences[s.Native.Name] = true
	return w.inspectCrossOracleBindings(ctx, s, false)
}
func (w *OracleWriter) createCrossOracleSequence(ctx context.Context, s source.SequenceInfo) error {
	if err := w.inspectCrossOracleSequence(ctx, s); err != nil {
		return err
	}
	stmts := w.dia.SequenceStatements(w.schema, s, "")
	if len(stmts) == 0 {
		return fmt.Errorf("序列计划为空")
	}
	parent := ctx
	ctx = repair.WithObserver(ctx, func(e repair.Event) error {
		if e.SQL == stmts[0].SQL && e.Status == "succeeded" {
			if err := w.db.QueryRowContext(ctx, `SELECT OBJECT_ID FROM ALL_OBJECTS WHERE OWNER=:1 AND OBJECT_NAME=:2 AND OBJECT_TYPE='SEQUENCE'`, w.schema, s.Native.Name).Scan(&s.Native.OracleObjectID); err != nil {
				if saveErr := repair.Observe(parent, e.SQL, "unknown", err); saveErr != nil {
					return saveErr
				}
				return err
			}
		}
		var execErr error
		if e.Error != "" {
			execErr = fmt.Errorf("%s", e.Error)
		}
		return repair.Observe(parent, e.SQL, e.Status, execErr)
	})
	if err := w.execStatements(ctx, stmts); err != nil {
		return err
	}
	for _, b := range s.Native.Bindings {
		if err := w.observeValidity(ctx, stmts, dialect.CrossOracleTriggerName(s, b), "TRIGGER"); err != nil {
			return err
		}
	}
	return nil
}

func (w *OracleWriter) crossOracleCompatibleColumn(c source.ColumnInfo, dst valueconv.OracleColumn) error {
	mapped, err := typemap.OracleTypeForSource(c, w.crossSource, strings.HasPrefix(dst.DataType, "N"), w.dia.CharBytes, w.dia.NCharBytes)
	if err != nil {
		return err
	}
	base := func(s string) string {
		return strings.Join(strings.Fields(pgOracleTypeParameters.ReplaceAllString(strings.ToUpper(s), "")), " ")
	}
	if base(mapped) != base(dst.DataType) {
		return fmt.Errorf("目标 %s 不匹配源类型映射 %s", dst.DataType, mapped)
	}
	if c.IsNullable && !dst.Nullable {
		return fmt.Errorf("目标列不允许源 NULL")
	}
	args := []int64{}
	if start := strings.IndexByte(mapped, '('); start >= 0 {
		end := strings.IndexByte(mapped, ')')
		for _, raw := range strings.Split(mapped[start+1:end], ",") {
			n, e := strconv.ParseInt(strings.Fields(strings.TrimSpace(raw))[0], 10, 64)
			if e == nil {
				args = append(args, n)
			}
		}
	}
	switch base(mapped) {
	case "NUMBER":
		if dst.Precision > 0 {
			if len(args) != 2 || dst.Scale < args[1] || dst.Precision-dst.Scale < args[0]-args[1] {
				return fmt.Errorf("目标 NUMBER 范围或精度不足")
			}
		}
	case "CHAR", "VARCHAR2", "NCHAR", "NVARCHAR2":
		if len(args) > 0 && (dst.CharLength < args[0] || dst.CharUsed != "C" && dst.DataLength < args[0]*int64(w.dia.CharBytes)) {
			return fmt.Errorf("目标字符容量不足")
		}
	case "RAW":
		if len(args) > 0 && dst.DataLength < args[0] {
			return fmt.Errorf("目标 RAW 容量不足")
		}
	case "TIMESTAMP", "TIMESTAMP WITH TIME ZONE":
		if len(args) > 0 && dst.Scale < args[0] {
			return fmt.Errorf("目标时间精度不足")
		}
	}
	return nil
}

func (w *OracleWriter) preflightCrossOracleData(ctx context.Context, tables map[string]*source.TableDDLInfo, seqs []source.SequenceInfo) error {
	for _, s := range seqs {
		if s.Unsupported != "" {
			continue
		}
		if err := w.inspectCrossOracleSequence(ctx, s); err != nil {
			return &OracleObjectPreflightError{Kind: "SEQUENCE", Name: s.Native.Name, Err: err}
		}
		if s.Native.OracleObjectID <= 0 {
			return &OracleObjectPreflightError{Kind: "SEQUENCE", Name: s.Native.Name, Err: fmt.Errorf("仅数据模式缺少已创建工具序列身份")}
		}
		if err := w.inspectCrossOracleBindings(ctx, s, true); err != nil {
			return &OracleObjectPreflightError{Kind: "SEQUENCE", Name: s.Native.Name, Err: err}
		}
	}
	for table, info := range tables {
		allowed := map[string]string{}
		for _, s := range seqs {
			if s.Unsupported != "" || s.Native == nil || s.Native.OracleObjectID <= 0 {
				continue
			}
			stmts := w.dia.SequenceStatements(w.schema, s, "")
			for i, b := range s.Native.Bindings {
				if b.TableName == table {
					allowed[dialect.CrossOracleTriggerName(s, b)] = stmts[i+1].SQL
				}
			}
		}
		if err := w.ValidatePGOracleDataTable(ctx, table, info.Columns, allowed); err != nil {
			return oracleTablePreflightError(table, err)
		}
	}
	return nil
}

// InspectCrossOracleSequence verifies identity, definition and every binding.
func (w *OracleWriter) InspectCrossOracleSequence(ctx context.Context, s source.SequenceInfo, requireBindings bool) error {
	if s.Native == nil || s.Native.OracleObjectID <= 0 {
		return fmt.Errorf("缺少持久化工具序列身份")
	}
	if err := w.inspectCrossOracleSequence(ctx, s); err != nil {
		return err
	}
	return w.inspectCrossOracleBindings(ctx, s, requireBindings)
}
func (w *OracleWriter) inspectCrossOracleBindings(ctx context.Context, s source.SequenceInfo, required bool) error {
	stmts := w.dia.SequenceStatements(w.schema, s, "")
	if len(stmts) != len(s.Native.Bindings)+1 {
		return fmt.Errorf("序列绑定计划无效")
	}
	for i, b := range s.Native.Bindings {
		name := dialect.CrossOracleTriggerName(s, b)
		var owner, table, status string
		err := w.db.QueryRowContext(ctx, `SELECT TABLE_OWNER,TABLE_NAME,STATUS FROM ALL_TRIGGERS WHERE OWNER=:1 AND TRIGGER_NAME=:2`, w.schema, name).Scan(&owner, &table, &status)
		if err == sql.ErrNoRows && !required {
			continue
		}
		if err != nil {
			return fmt.Errorf("核验序列绑定 %s: %w", name, err)
		}
		if s.Native.OracleObjectID <= 0 || owner != w.schema || table != b.TableName || status != "ENABLED" {
			return fmt.Errorf("触发器 %s 的身份、状态或绑定不匹配", name)
		}
		rows, err := w.db.QueryContext(ctx, `SELECT TEXT FROM ALL_SOURCE WHERE OWNER=:1 AND NAME=:2 AND TYPE='TRIGGER' ORDER BY LINE`, w.schema, name)
		if err != nil {
			return err
		}
		var body strings.Builder
		for rows.Next() {
			var line string
			if err = rows.Scan(&line); err != nil {
				rows.Close()
				return err
			}
			body.WriteString(line)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		normalize := func(v string) string {
			v = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(strings.ReplaceAll(v, "\r\n", "\n")), ";"))
			v = strings.TrimPrefix(v, "CREATE OR REPLACE ")
			return strings.Replace(v, "TRIGGER "+w.dia.QualifyTable(w.schema, name), "TRIGGER "+w.dia.QuoteIdent(name), 1)
		}
		if normalize(body.String()) != normalize(stmts[i+1].SQL) {
			return fmt.Errorf("工具触发器 %s 定义已漂移", name)
		}
		if required {
			if err := w.validateObject(ctx, name, "TRIGGER"); err != nil {
				return err
			}
		}
	}
	return nil
}
func (w *OracleWriter) CalibrateCrossOracle(ctx context.Context, s source.SequenceInfo) error {
	if err := w.InspectCrossOracleSequence(ctx, s, true); err != nil {
		return err
	}
	ss := w.dia.SequenceStatements(w.schema, s, "")
	return w.execStatements(ctx, ss[:1])
}

func (w *OracleWriter) CrossOracleRelationExists(ctx context.Context, name string) (bool, error) {
	var n int
	err := w.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_OBJECTS WHERE OWNER=:1 AND OBJECT_NAME=:2 AND OBJECT_TYPE IN ('TABLE','VIEW') AND STATUS='VALID'`, w.schema, name).Scan(&n)
	return n > 0, err
}
