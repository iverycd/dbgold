package source

import (
	"context"
	"database/sql"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"sync"

	"dbgold/datamigrate/sqlsyntax"
)

// OracleMySQLAutoIncrement is an immutable source binding, not executable SQL.
// Keep names as returned by the dictionary; only unquoted SQL references fold.
type OracleMySQLAutoIncrement struct {
	TableName      string `json:"table"`
	ColumnName     string `json:"column"`
	TriggerName    string `json:"trigger"`
	TriggerOwner   string `json:"trigger_owner"`
	SequenceOwner  string `json:"sequence_owner"`
	SequenceName   string `json:"sequence"`
	StartValueText string `json:"source_start"`
	SharedSequence bool   `json:"shared_sequence"`
	Disabled       bool   `json:"disabled"`
	Error          string `json:"error,omitempty"`
}

type OracleMySQLReader struct {
	*OracleReader
	autoMu     sync.Mutex
	autoLoaded bool
	auto       []OracleMySQLAutoIncrement
	autoErr    error
}

func WrapOracleForMySQL(r *OracleReader) *OracleMySQLReader {
	return &OracleMySQLReader{OracleReader: r}
}
func NewOracleForMySQL(dsn, owner string, pool ConnPoolConfig) (*OracleMySQLReader, error) {
	r, err := NewOracle(dsn, owner, pool)
	if err != nil {
		return nil, err
	}
	return WrapOracleForMySQL(r), nil
}

func (r *OracleMySQLReader) ResetOracleMySQLAutoIncrements() {
	r.autoMu.Lock()
	defer r.autoMu.Unlock()
	r.autoLoaded, r.auto, r.autoErr = false, nil, nil
	r.pageColumns.Clear()
}

func (r *OracleMySQLReader) GetOracleMySQLAutoIncrements(ctx context.Context) ([]OracleMySQLAutoIncrement, error) {
	r.autoMu.Lock()
	defer r.autoMu.Unlock()
	if !r.autoLoaded {
		r.auto, r.autoErr = r.readOracleMySQLAutoIncrements(ctx)
		r.autoLoaded = true
	}
	return slices.Clone(r.auto), r.autoErr
}

const oracleMySQLTriggersSQL = `SELECT TABLE_NAME,TRIGGER_NAME,TRIGGER_TYPE,TRIGGERING_EVENT,TRIGGER_BODY,WHEN_CLAUSE,STATUS,OWNER
 FROM ALL_TRIGGERS WHERE TABLE_OWNER=:1 AND TABLE_NAME IS NOT NULL ORDER BY TABLE_NAME,TRIGGER_NAME`
const oracleMySQLSequenceSQL = `SELECT TO_CHAR(LAST_NUMBER,'TM9'),TO_CHAR(INCREMENT_BY,'TM9'),CYCLE_FLAG
 FROM ALL_SEQUENCES WHERE SEQUENCE_OWNER=:1 AND SEQUENCE_NAME=:2`

func (r *OracleMySQLReader) readOracleMySQLAutoIncrements(ctx context.Context) ([]OracleMySQLAutoIncrement, error) {
	rows, err := r.db.QueryContext(ctx, oracleMySQLTriggersSQL, r.owner)
	if err != nil {
		return nil, fmt.Errorf("读取自增触发器: %w", err)
	}
	var out []OracleMySQLAutoIncrement
	for rows.Next() {
		var a OracleMySQLAutoIncrement
		var kind, event, status string
		var body, when sql.NullString
		if err = rows.Scan(&a.TableName, &a.TriggerName, &kind, &event, &body, &when, &status, &a.TriggerOwner); err != nil {
			rows.Close()
			return nil, err
		}
		clean, e := oracleDMWithoutComments(body.String)
		if e == nil && !oracleDMNextval.MatchString(clean) {
			continue
		}
		// A literal/comment saying NEXTVAL is not a sequence dependency.
		if tokens, lexErr := sqlsyntax.Lex(clean); e == nil && lexErr == nil {
			found := false
			for _, token := range tokens {
				if token.Is("NEXTVAL") {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		a.Disabled = status != "ENABLED"
		binding, owner, seq, parseErr := parseOracleDMTrigger(body.String, when.String, a.TriggerOwner, a.TriggerName)
		a.ColumnName, a.SequenceOwner, a.SequenceName = binding.ColumnName, owner, seq
		if e != nil {
			parseErr = e
		}
		if kind != "BEFORE EACH ROW" || strings.TrimSpace(event) != "INSERT" {
			parseErr = fmt.Errorf("仅支持 INSERT-only BEFORE EACH ROW 触发器")
		}
		if parseErr != nil {
			a.Error = parseErr.Error()
		}
		out = append(out, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	// Close the trigger cursor before querying sequences, including pool size 1.
	type sequenceState struct {
		start string
		err   error
	}
	states := map[string]sequenceState{}
	uses := map[string]map[string]bool{}
	claims := map[string]int{}
	for i := range out {
		a := &out[i]
		if a.Disabled {
			continue
		}
		claims[a.TableName]++
		if a.Error != "" {
			continue
		}
		key := a.SequenceOwner + "\x00" + a.SequenceName
		if uses[key] == nil {
			uses[key] = map[string]bool{}
		}
		uses[key][a.TableName] = true
		state, ok := states[key]
		if !ok {
			var step, cycle string
			state.err = r.db.QueryRowContext(ctx, oracleMySQLSequenceSQL, a.SequenceOwner, a.SequenceName).Scan(&state.start, &step, &cycle)
			if state.err == nil && (step != "1" || cycle != "N") {
				state.err = fmt.Errorf("仅支持步长为 1 的非循环递增序列")
			}
			if state.err == nil {
				if _, ok := new(big.Int).SetString(state.start, 10); !ok {
					state.err = fmt.Errorf("序列起点不是精确整数: %s", state.start)
				}
			}
			states[key] = state
		}
		a.StartValueText = state.start
		if state.err != nil {
			a.Error = fmt.Sprintf("读取/验证序列 %s.%s: %v", a.SequenceOwner, a.SequenceName, state.err)
		}
	}
	for i := range out {
		a := &out[i]
		a.SharedSequence = len(uses[a.SequenceOwner+"\x00"+a.SequenceName]) > 1
		if !a.Disabled && claims[a.TableName] > 1 {
			a.Error = "同一表存在多个序列赋值触发器，无法确定唯一自增列"
		}
	}
	return out, nil
}

func (r *OracleMySQLReader) GetSequences(ctx context.Context) ([]SequenceInfo, error) {
	bindings, err := r.GetOracleMySQLAutoIncrements(ctx)
	if err != nil {
		return nil, err
	}
	var out []SequenceInfo
	for _, a := range bindings {
		if a.Disabled {
			continue
		}
		copy := a
		out = append(out, SequenceInfo{TableName: a.TableName, ColumnName: a.ColumnName, StartValueText: a.StartValueText, OracleMySQL: &copy, Unsupported: a.Error})
	}
	return out, nil
}

func (r *OracleMySQLReader) GetTableDDLInfo(ctx context.Context, table string) (*TableDDLInfo, error) {
	bindings, err := r.GetOracleMySQLAutoIncrements(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range bindings {
		if a.TableName == table && !a.Disabled && a.Error != "" {
			return nil, fmt.Errorf("自增触发器 %s: %s", a.TriggerName, a.Error)
		}
	}
	info, err := r.OracleReader.GetTableDDLInfo(ctx, table)
	if err != nil {
		return info, err
	}
	for _, a := range bindings {
		if a.TableName != table || a.Disabled {
			continue
		}
		found := false
		for i := range info.Columns {
			c := &info.Columns[i]
			if c.Name != a.ColumnName {
				continue
			}
			found = true
			switch strings.ToUpper(c.DataType) {
			case "NUMBER", "NUMERIC", "DECIMAL", "INTEGER", "INT", "SMALLINT":
			default:
				return nil, fmt.Errorf("自增列 %s.%s 不是整数 NUMBER", table, c.Name)
			}
			if c.Scale != 0 {
				return nil, fmt.Errorf("自增列 %s.%s 的标度不为 0", table, c.Name)
			}
			qcol := quoteOracleIdentifier(c.Name)
			var count, nonnull, fractional int64
			var lo, hi sql.NullString
			q := "SELECT COUNT(*),COUNT(" + qcol + "),NVL(SUM(CASE WHEN " + qcol + "<>TRUNC(" + qcol + ") THEN 1 ELSE 0 END),0),TO_CHAR(MIN(" + qcol + "),'TM9','NLS_NUMERIC_CHARACTERS=''.,'''),TO_CHAR(MAX(" + qcol + "),'TM9','NLS_NUMERIC_CHARACTERS=''.,''') FROM " + quoteOracleIdentifier(r.owner) + "." + quoteOracleIdentifier(table)
			if err := r.db.QueryRowContext(ctx, q).Scan(&count, &nonnull, &fractional, &lo, &hi); err != nil {
				return nil, fmt.Errorf("校验自增列 %s.%s: %w", table, c.Name, err)
			}
			if count != nonnull || fractional != 0 {
				return nil, fmt.Errorf("自增列 %s.%s 含 NULL 或非整数，禁止重新编号", table, c.Name)
			}
			typeName, err := oracleMySQLAutoColumnType(*c, lo.String, hi.String, a.StartValueText)
			if err != nil {
				return nil, fmt.Errorf("自增列 %s.%s: %w", table, c.Name, err)
			}
			c.Extra, c.OracleMySQLAutoType = "auto_increment", typeName
		}
		if !found {
			return nil, fmt.Errorf("自增触发器 %s 的列 %s 不存在", a.TriggerName, a.ColumnName)
		}
	}
	return info, nil
}

func oracleMySQLAutoColumnType(c ColumnInfo, lo, hi, start string) (string, error) {
	typeName, bits := "bigint", uint(64)
	if c.Precision > 0 && c.Precision <= 4 {
		typeName, bits = "smallint", 16
	} else if c.Precision > 0 && c.Precision <= 9 {
		typeName, bits = "int", 32
	}
	// Widen a mapped integer when the retained sequence progress requires it.
	for {
		upper := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), bits-1), big.NewInt(1))
		lower := new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), bits-1))
		fits := true
		for i, s := range []string{lo, hi, start} {
			if s == "" && i < 2 {
				continue
			}
			n, ok := new(big.Int).SetString(s, 10)
			if !ok {
				return "", fmt.Errorf("非精确整数: %q", s)
			}
			if i == 1 {
				n.Add(n, big.NewInt(1))
			}
			if n.Cmp(lower) < 0 || n.Cmp(upper) > 0 {
				fits = false
			}
		}
		if fits {
			return typeName, nil
		}
		if bits == 64 {
			return "", fmt.Errorf("原 ID 或下一自增值超出有符号 BIGINT 范围")
		}
		typeName, bits = "bigint", 64
	}
}

func (r *OracleMySQLReader) autoProjection(ctx context.Context, table string) (string, map[string]bool, error) {
	bindings, err := r.GetOracleMySQLAutoIncrements(ctx)
	if err != nil {
		return "", nil, err
	}
	auto := map[string]bool{}
	for _, a := range bindings {
		if a.TableName == table && !a.Disabled {
			if a.Error != "" {
				return "", nil, fmt.Errorf("%s", a.Error)
			}
			auto[a.ColumnName] = true
		}
	}
	if len(auto) == 0 {
		return "*", auto, nil
	}
	names, err := r.pageColumnNames(ctx, table)
	if err != nil {
		return "", nil, err
	}
	projection := make([]string, len(names))
	for i, name := range names {
		q := quoteOracleIdentifier(name)
		projection[i] = q
		if auto[name] {
			projection[i] = "TO_CHAR(" + q + ",'TM9','NLS_NUMERIC_CHARACTERS=''.,''') AS " + q
		}
	}
	return strings.Join(projection, ", "), auto, nil
}

func (r *OracleMySQLReader) ReadPage(ctx context.Context, table string, pks []string, offset, limit int64) ([]string, []string, [][]interface{}, error) {
	projection, auto, err := r.autoProjection(ctx, table)
	if err != nil {
		return nil, nil, nil, err
	}
	cols, types, rows, err := r.readPage(ctx, table, pks, offset, limit, projection)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := normalizeOracleMySQLAutoRows(table, auto, cols, types, rows); err != nil {
		return nil, nil, nil, err
	}
	return cols, types, rows, nil
}

func normalizeOracleMySQLAutoRows(table string, auto map[string]bool, cols, types []string, rows [][]interface{}) error {
	for i, col := range cols {
		if !auto[col] {
			continue
		}
		types[i] = "NUMBER"
		for _, row := range rows {
			var value string
			switch v := row[i].(type) {
			case string:
				value = v
			case []byte:
				value = string(v)
			default:
				return fmt.Errorf("自增列 %s.%s 未返回精确数值文本", table, col)
			}
			n, ok := new(big.Int).SetString(value, 10)
			if !ok || !n.IsInt64() {
				return fmt.Errorf("自增列 %s.%s 含非整数或超出 BIGINT", table, col)
			}
			row[i] = n.String()
		}
	}
	return nil
}

func (r *OracleMySQLReader) StreamPartitionRows(ctx context.Context, table string, batch int, consume func([]string, []string, [][]interface{}) error) error {
	projection, auto, err := r.autoProjection(ctx, table)
	if err != nil {
		return err
	}
	if consume == nil {
		return fmt.Errorf("缺少分区数据接收回调")
	}
	return streamPartitionQuery(ctx, r.db, "SELECT "+projection+" FROM "+quoteOracleIdentifier(r.owner)+"."+quoteOracleIdentifier(table), batch, nil, partitionTextValues, func(cols, types []string, rows [][]interface{}) error {
		if err := normalizeOracleMySQLAutoRows(table, auto, cols, types, rows); err != nil {
			return err
		}
		return consume(cols, types, rows)
	})
}
