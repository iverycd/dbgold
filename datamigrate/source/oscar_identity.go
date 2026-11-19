package source

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"dbgold/datamigrate/failure"
	"dbgold/datamigrate/sqlsyntax"
	"dbgold/internal/jdbcbridge"
)

func (r *OscarReader) GetSequences(ctx context.Context) ([]SequenceInfo, error) {
	tables, err := r.ListTables(ctx)
	if err != nil {
		return nil, err
	}
	var out []SequenceInfo
	for _, table := range tables {
		cols, err := r.columns(ctx, table)
		if failure.IsTaskError(err) {
			return nil, err
		}
		if err != nil {
			// The table preparation path records its exact metadata failure.
			continue
		}
		for _, col := range cols {
			seq, related, err := r.oscarAutoIncrement(ctx, table, col)
			if err != nil {
				return nil, err
			}
			if related {
				out = append(out, seq)
			}
		}
	}
	return out, nil
}

func (r *OscarReader) oscarAutoIncrement(ctx context.Context, table string, col jdbcbridge.MigrationColumn) (SequenceInfo, bool, error) {
	seq := SequenceInfo{TableName: table, ColumnName: col.Name}
	identity := strings.EqualFold(col.AutoIncrement, "YES")
	var owner, name string
	if col.Default != nil {
		owner, name = oscarSequenceDefault(*col.Default)
	}
	if !identity && name == "" {
		return seq, false, nil
	}
	targetMaximum := int64(math.MaxInt64)
	switch strings.ToUpper(col.Type) {
	case "TINYINT":
		targetMaximum = math.MaxInt8
	case "SMALLINT", "INT2", "SMALLSERIAL":
		targetMaximum = math.MaxInt16
	case "INT", "INTEGER", "INT4", "SERIAL":
		targetMaximum = math.MaxInt32
	case "BIGINT", "INT8", "BIGSERIAL":
	default:
		seq.Unsupported = "源自增列没有等价的 MySQL 整数类型"
		return seq, true, nil
	}
	if col.Nullable {
		seq.Unsupported = "源自增列允许 NULL，无法等价应用 MySQL AUTO_INCREMENT"
		return seq, true, nil
	}
	if name == "" {
		seq.Unsupported = "源 JDBC 自增标记未提供可只读核验的关联序列与当前值"
		return seq, true, nil
	}
	if owner == "" {
		owner = r.schema
	}
	if owner != r.schema {
		seq.Unsupported = "自增序列位于迁移 Schema 之外"
		return seq, true, nil
	}
	if !identity {
		// A default alone is insufficient proof of exclusive sequence usage.
		// Require a catalog-owned binding between the sequence and this column.
		seq.Unsupported = "源默认表达式引用了独立序列，目录未证明它专属于该列"
		return seq, true, nil
	}
	next, err := r.readSequenceNext(ctx, owner, name, targetMaximum)
	if failure.IsTaskError(err) {
		return seq, true, err
	}
	if err != nil {
		seq.Unsupported = err.Error()
		return seq, true, nil
	}
	seq.StartValue = next
	seq.StartValueText = strconv.FormatInt(next, 10)
	return seq, true, nil
}

// oscarSequenceDefault recognizes a single qualified sequence reference. The
// result is used only as catalog identifiers, never as executable source SQL.
func oscarSequenceDefault(raw string) (string, string) {
	tokens, err := sqlsyntax.Lex(raw)
	if err != nil || len(tokens) < 4 || !tokens[0].Is("NEXTVAL") || !tokens[1].Is("(") ||
		tokens[2].Kind != 's' || !tokens[len(tokens)-1].Is(")") {
		return "", ""
	}
	if len(tokens) != 4 && (len(tokens) != 6 || !tokens[3].Is("::") || !tokens[4].Is("REGCLASS")) {
		return "", ""
	}
	names, err := sqlsyntax.Lex(tokens[2].Value)
	if err != nil || len(names) == 0 {
		return "", ""
	}
	nameOf := func(token sqlsyntax.Token) string {
		if token.Kind == 'i' {
			return strings.ToUpper(token.Value)
		}
		if token.Kind == 'q' && strings.HasPrefix(token.Text, `"`) {
			return token.Value
		}
		return ""
	}
	if len(names) == 1 {
		return "", nameOf(names[0])
	}
	if len(names) == 3 && names[1].Is(".") {
		owner, name := nameOf(names[0]), nameOf(names[2])
		if owner != "" {
			return owner, name
		}
	}
	return "", ""
}

type oscarSequenceState struct {
	start, step, maximum, last int64
	cycle, called              bool
}

func (r *OscarReader) readOscarSequenceState(ctx context.Context, owner, name string) (state oscarSequenceState, _ error) {
	// Inspect the native sequence relation directly. Unlike NEXTVAL(), this
	// read does not change the source high water mark or consume a value.
	rows, err := r.query(ctx, `SELECT c.RELKIND FROM SYS_CLASS c JOIN SYS_NAMESPACE n ON n.OID=c.RELNAMESPACE
WHERE n.NSPNAME=? AND c.RELNAME=?`, owner, name)
	if err != nil {
		return state, err
	}
	kind, err := oscarSingleText(rows)
	if err != nil || kind != "S" {
		return state, fmt.Errorf("神通序列 %s.%s 的原生对象身份不明确", owner, name)
	}
	rows, err = r.query(ctx, "SELECT START_VALUE,INCREMENT_BY,MAX_VALUE,IS_CYCLED,LAST_VALUE,IS_CALLED FROM "+
		oscarQuoteIdent(owner)+"."+oscarQuoteIdent(name))
	if err != nil {
		return state, fmt.Errorf("读取神通序列 %s.%s 的只读状态: %w", owner, name, err)
	}
	if len(rows) != 1 || len(rows[0]) != 6 {
		return state, fmt.Errorf("神通序列 %s.%s 的只读状态不完整", owner, name)
	}
	row := rows[0]
	start, startErr := oscarInteger(row[0])
	step, stepErr := oscarInteger(row[1])
	maximum, limitErr := oscarInteger(row[2])
	cycle, cycleErr := oscarBoolean(row[3])
	last, lastErr := oscarInteger(row[4])
	called, calledErr := oscarBoolean(row[5])
	if startErr != nil || stepErr != nil || limitErr != nil || cycleErr != nil || lastErr != nil || calledErr != nil {
		return state, fmt.Errorf("神通序列 %s.%s 的只读状态无效", owner, name)
	}
	return oscarSequenceState{start: start, step: step, maximum: maximum, last: last, cycle: cycle, called: called}, nil
}

func (r *OscarReader) readSequenceNext(ctx context.Context, owner, name string, targetMaximum int64) (int64, error) {
	state, err := r.readOscarSequenceState(ctx, owner, name)
	if err != nil {
		return 0, err
	}
	start, step, maximum, last, cycle, called := state.start, state.step, state.maximum, state.last, state.cycle, state.called
	if start <= 0 || step != 1 || cycle || maximum < targetMaximum || last < start || last > targetMaximum || called && last == targetMaximum {
		return 0, fmt.Errorf("神通序列 %s.%s 的起点、步长、上界、循环或当前值不能等价转换为 MySQL AUTO_INCREMENT", owner, name)
	}
	if called {
		last++
	}
	return last, nil
}
