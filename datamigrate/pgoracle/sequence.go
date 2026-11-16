// Package pgoracle contains durable PG-to-Oracle plans without connection state.
package pgoracle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
)

type Binding struct {
	SourceTable  string `json:"source_table"`
	SourceColumn string `json:"source_column"`
	Table        string `json:"table"`
	Column       string `json:"column"`
	Trigger      string `json:"trigger"`
}

type SequenceIdentity struct {
	ObjectID    int64  `json:"object_id"`
	Fingerprint string `json:"fingerprint"`
}

type SequencePlan struct {
	Source   source.PGSequence `json:"source"`
	Schema   string            `json:"schema"`
	Name     string            `json:"name"`
	Bindings []Binding         `json:"bindings"`
	Identity *SequenceIdentity `json:"identity,omitempty"`
}

func SequenceFingerprint(increment, minimum, maximum, cache string, cycle bool, ordered ...bool) string {
	order := len(ordered) > 0 && ordered[0]
	b, _ := json.Marshal([]any{increment, minimum, maximum, cache, cycle, order})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (p SequencePlan) Fingerprint() string {
	cache := p.Source.Cache
	if cache < 2 {
		cache = 0
	}
	return SequenceFingerprint(fmt.Sprint(p.Source.Increment), fmt.Sprint(p.Source.Min), fmt.Sprint(p.Source.Max), fmt.Sprint(cache), p.Source.Cycle)
}

func (p SequencePlan) Next() (*big.Int, error) {
	s := p.Source
	if s.Unsupported != "" {
		return nil, fmt.Errorf("序列 %s: %s", s.Name, s.Unsupported)
	}
	if s.Exhausted {
		return nil, fmt.Errorf("序列 %s 已耗尽", s.Name)
	}
	if s.Increment == 0 || s.Min >= s.Max {
		return nil, fmt.Errorf("序列 %s 步长或边界无效", s.Name)
	}
	if s.LastValue < s.Min || s.LastValue > s.Max || s.Cache < 1 {
		return nil, fmt.Errorf("序列 %s 状态或缓存值无效", s.Name)
	}
	step := big.NewInt(s.Increment)
	lo, hi := big.NewInt(s.Min), big.NewInt(s.Max)
	if new(big.Int).Abs(new(big.Int).Set(step)).Cmp(new(big.Int).Sub(hi, lo)) >= 0 {
		return nil, fmt.Errorf("序列 %s 步长超出边界跨度", s.Name)
	}
	n := big.NewInt(s.LastValue)
	if s.IsCalled {
		n.Add(n, step)
	}
	if n.Cmp(lo) < 0 || n.Cmp(hi) > 0 {
		if !s.Cycle {
			return nil, fmt.Errorf("序列 %s 已耗尽", s.Name)
		}
		if step.Sign() > 0 {
			n.Set(lo)
		} else {
			n.Set(hi)
		}
	}
	return n, nil
}

func (p SequencePlan) Validate() error {
	if err := dialect.ValidatePGOracleName(p.Schema); err != nil {
		return err
	}
	if err := dialect.ValidatePGOracleName(p.Name); err != nil {
		return err
	}
	if p.Source.Name == "" || p.Source.Schema == "" {
		return fmt.Errorf("序列缺少源对象身份")
	}
	if _, err := p.Next(); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, b := range p.Bindings {
		for _, n := range []string{b.Table, b.Column, b.Trigger} {
			if err := dialect.ValidatePGOracleName(n); err != nil {
				return err
			}
		}
		if b.SourceTable == "" || b.SourceColumn == "" {
			return fmt.Errorf("序列绑定缺少源列身份")
		}
		if seen[b.Table+"\x00"+b.Column] || seen["trigger\x00"+b.Trigger] {
			return fmt.Errorf("序列绑定或触发器名称重复")
		}
		seen[b.Table+"\x00"+b.Column], seen["trigger\x00"+b.Trigger] = true, true
		found := false
		for _, raw := range p.Source.Bindings {
			if raw.TableName == b.SourceTable && raw.ColumnName == b.SourceColumn {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("序列绑定 %s.%s 已偏离源依赖", b.SourceTable, b.SourceColumn)
		}
	}
	return nil
}

func (p SequencePlan) SequenceStatements() ([]dialect.Statement, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if p.Identity != nil {
		return p.CalibrationStatements()
	}
	n, err := p.Next()
	if err != nil {
		return nil, err
	}
	s := p.Source
	d := dialect.NewOracle()
	q := d.QualifyTable(p.Schema, p.Name)
	cache := " NOCACHE"
	if s.Cache >= 2 {
		cache = fmt.Sprintf(" CACHE %d", s.Cache)
	}
	cycle := " NOCYCLE"
	if s.Cycle {
		cycle = " CYCLE"
	}
	if s.Cycle && s.Cache >= 2 {
		// Oracle's cyclic cache limit is strictly below CEIL(span/abs(step)).
		// https://docs.oracle.com/en/database/oracle/oracle-database/19/sqlrf/CREATE-SEQUENCE.html
		span := new(big.Int).Sub(big.NewInt(s.Max), big.NewInt(s.Min))
		step := new(big.Int).Abs(big.NewInt(s.Increment))
		limit := new(big.Int).Div(new(big.Int).Add(span, new(big.Int).Sub(step, big.NewInt(1))), step)
		if big.NewInt(s.Cache).Cmp(limit) >= 0 {
			return nil, fmt.Errorf("循环序列 %s 的缓存大小超出 Oracle 循环容量", s.Name)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "DECLARE n NUMBER := %s; e NUMBER; BEGIN\n", n.String())
	if !s.Cycle {
		for _, binding := range p.Bindings {
			agg := "MAX"
			op := ">="
			if s.Increment < 0 {
				agg = "MIN"
				op = "<="
			}
			fmt.Fprintf(&b, "SELECT %s(%s) INTO e FROM %s;\n", agg, d.QuoteIdent(binding.Column), d.QualifyTable(p.Schema, binding.Table))
			fmt.Fprintf(&b, "IF e IS NOT NULL AND e %s n THEN n := n + (FLOOR((e-n)/(%d))+1)*(%d); END IF;\n", op, s.Increment, s.Increment)
		}
	}
	fmt.Fprintf(&b, "IF n < %d OR n > %d THEN RAISE_APPLICATION_ERROR(-20001, 'PG Oracle sequence exhausted'); END IF;\n", s.Min, s.Max)
	create := "CREATE SEQUENCE " + q + " START WITH "
	fmt.Fprintf(&b, "EXECUTE IMMEDIATE %s || TO_CHAR(n, 'TM9', 'NLS_NUMERIC_CHARACTERS=''.,''') || %s;\nEND;", literal(create), literal(fmt.Sprintf(" INCREMENT BY %d MINVALUE %d MAXVALUE %d%s%s NOORDER", s.Increment, s.Min, s.Max, cycle, cache)))
	return []dialect.Statement{{SQL: b.String()}}, nil
}

// CalibrationStatements is a frozen forward-only action for an already-owned
// generator. Target code must recheck ObjectID, definition and every binding
// immediately before executing these statements.
func (p SequencePlan) CalibrationStatements() ([]dialect.Statement, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if p.Identity == nil || p.Identity.ObjectID <= 0 || p.Identity.Fingerprint != p.Fingerprint() {
		return nil, fmt.Errorf("序列 %s 缺少有效的持久化身份", p.Name)
	}
	if p.Source.Cycle {
		return []dialect.Statement{{SQL: "BEGIN /* DBGOLD_PG_ORACLE_CYCLE:" + p.Fingerprint() + " */ NULL; END;"}}, nil
	}
	start, err := p.Next()
	if err != nil {
		return nil, err
	}
	d := dialect.NewOracle()
	q := d.QualifyTable(p.Schema, p.Name)
	s := p.Source
	agg, compare, beyond := "MAX", ">=", ">"
	if s.Increment < 0 {
		agg, compare, beyond = "MIN", "<=", "<"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "DECLARE desired NUMBER := %s; n NUMBER; e NUMBER; desired_last NUMBER; jump NUMBER; changed BOOLEAN := FALSE; BEGIN\n", start.String())
	for _, binding := range p.Bindings {
		fmt.Fprintf(&b, "SELECT %s(%s) INTO e FROM %s;\n", agg, d.QuoteIdent(binding.Column), d.QualifyTable(p.Schema, binding.Table))
		fmt.Fprintf(&b, "IF e IS NOT NULL AND e %s desired THEN desired := desired + (FLOOR((e-desired)/(%d))+1)*(%d); END IF;\n", compare, s.Increment, s.Increment)
	}
	fmt.Fprintf(&b, "IF desired < %d OR desired > %d THEN RAISE_APPLICATION_ERROR(-20001, 'PG Oracle sequence exhausted'); END IF;\n", s.Min, s.Max)
	// LAST_NUMBER is a cache boundary, not the next value to be allocated.
	// https://docs.oracle.com/cd/E18283_01/server.112/e17110/statviews_2062.htm
	fmt.Fprintf(&b, "SELECT %s.NEXTVAL INTO n FROM DUAL;\n", q)
	fmt.Fprintf(&b, "desired_last := n + (CEIL((desired-n)/(%d))-1)*(%d);\n", s.Increment, s.Increment)
	alter := literal("ALTER SEQUENCE " + q + " INCREMENT BY ")
	restore := literal(fmt.Sprintf("ALTER SEQUENCE %s INCREMENT BY %d", q, s.Increment))
	fmt.Fprintf(&b, "IF desired_last %s n THEN\n  jump := desired_last-n;\n  EXECUTE IMMEDIATE %s || TO_CHAR(jump,'TM9','NLS_NUMERIC_CHARACTERS=''.,''');\n  changed := TRUE;\n  SELECT %s.NEXTVAL INTO n FROM DUAL;\n  EXECUTE IMMEDIATE %s;\n  changed := FALSE;\nEND IF;\n", beyond, alter, q, restore)
	fmt.Fprintf(&b, "IF n+(%d) < %d OR n+(%d) > %d THEN RAISE_APPLICATION_ERROR(-20001, 'PG Oracle sequence exhausted'); END IF;\n", s.Increment, s.Min, s.Increment, s.Max)
	fmt.Fprintf(&b, "EXCEPTION WHEN OTHERS THEN IF changed THEN EXECUTE IMMEDIATE %s; END IF; RAISE; END;", restore)
	return []dialect.Statement{{SQL: b.String()}}, nil
}

func (p SequencePlan) TriggerSQL(b Binding) string {
	d := dialect.NewOracle()
	return "CREATE OR REPLACE TRIGGER " + d.QualifyTable(p.Schema, b.Trigger) + "\nBEFORE INSERT ON " + d.QualifyTable(p.Schema, b.Table) + " FOR EACH ROW\nBEGIN\n  /* DBGOLD_PG_ORACLE:" + p.Fingerprint() + " */\n  IF :NEW." + d.QuoteIdent(b.Column) + " IS NULL THEN\n    SELECT " + d.QualifyTable(p.Schema, p.Name) + ".NEXTVAL INTO :NEW." + d.QuoteIdent(b.Column) + " FROM DUAL;\n  END IF;\nEND;"
}

func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
