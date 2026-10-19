package target

import (
	"context"
	"database/sql"
	"fmt"
	"math/big"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
)

// CalibrateRecovery advances existing identity/sequence state after data copy.
// It never creates/drops an object, removes a binding, or moves a generator back.
// Missing bindings are left to the separately recorded sequence repair steps.
func CalibrateRecovery(ctx context.Context, w Writer, table string, sequences []source.SequenceInfo) error {
	a, e := repairAccess(w)
	if e != nil {
		return e
	}
	schema, e := ResolveRepairSchema(ctx, w)
	if e != nil {
		return e
	}
	seen := map[string]bool{}
	for _, s := range sequences {
		if s.Native != nil && a.family == "dameng" {
			n := *s.Native
			n.Bindings = nil
			for _, b := range s.Native.Bindings {
				if b.TableName == table {
					n.Bindings = append(n.Bindings, b)
				}
			}
			if len(n.Bindings) == 0 {
				continue
			}
			s.Native = &n
			if e = w.(*DaMengWriter).CalibrateOracleDameng(ctx, s); e != nil {
				return e
			}
			continue
		}
		if s.TableName != table || s.ColumnName == "" || seen[s.ColumnName] {
			continue
		}
		seen[s.ColumnName] = true
		switch a.family {
		case "postgres":
			e = calibratePG(ctx, w, a, schema, s)
		case "mysql":
			e = calibrateMySQL(ctx, w, a, schema, s)
		case "sqlserver":
			e = calibrateSQLServer(ctx, w.(*SQLServerWriter), s)
		case "oracle":
			e = calibrateOracle(ctx, w.(*OracleWriter), s)
		case "dameng":
			e = calibrateDameng(ctx, w.(*DaMengWriter), s)
		default:
			e = fmt.Errorf("目标 %s 尚不能可靠检查自增绑定，请单独核验", w.DBType())
		}
		if e != nil {
			return fmt.Errorf("自增校准 %s.%s: %w", table, s.ColumnName, e)
		}
	}
	return nil
}

func repairInteger(v string) (*big.Int, error) {
	n, ok := new(big.Int).SetString(strings.TrimSpace(v), 10)
	if !ok {
		return nil, fmt.Errorf("无效整数元数据 %q", v)
	}
	return n, nil
}
func recoveryStart(s source.SequenceInfo) (*big.Int, error) {
	v := s.StartValueText
	if v == "" {
		v = fmt.Sprint(s.StartValue)
	}
	return repairInteger(v)
}
func farther(a, b *big.Int, step *big.Int) *big.Int {
	if b == nil {
		return new(big.Int).Set(a)
	}
	if step.Sign() > 0 && b.Cmp(a) > 0 || step.Sign() < 0 && b.Cmp(a) < 0 {
		return new(big.Int).Set(b)
	}
	return new(big.Int).Set(a)
}

// Advance on the existing arithmetic progression, preserving its offset as well
// as its increment. An explicit off-progression row must not change future IDs.
func advanceProgression(next, bound, step *big.Int) *big.Int {
	bound = farther(next, bound, step)
	distance := new(big.Int).Abs(new(big.Int).Sub(bound, next))
	stride := new(big.Int).Abs(new(big.Int).Set(step))
	q, r := new(big.Int), new(big.Int)
	q.QuoRem(distance, stride, r)
	if r.Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}
	return new(big.Int).Add(next, new(big.Int).Mul(q, step))
}

func afterExtreme(extreme, step *big.Int) *big.Int {
	return new(big.Int).Add(extreme, big.NewInt(int64(step.Sign())))
}
func repairLiteral(v string) string { return "'" + strings.ReplaceAll(v, "'", "''") + "'" }
func recoveryExtreme(ctx context.Context, w Writer, db *sql.DB, schema string, s source.SequenceInfo, step *big.Int) (*big.Int, error) {
	agg := "MAX"
	if step.Sign() < 0 {
		agg = "MIN"
	}
	var value sql.NullString
	e := db.QueryRowContext(ctx, "SELECT "+agg+"("+w.Dialect().QuoteIdent(s.ColumnName)+") FROM "+w.Dialect().QualifyTable(schema, s.TableName)).Scan(&value)
	if e != nil {
		return nil, e
	}
	if !value.Valid {
		return nil, nil
	}
	return repairInteger(value.String)
}

func calibratePG(ctx context.Context, w Writer, a repairTarget, schema string, s source.SequenceInfo) error {
	var seq sql.NullString
	e := a.db.QueryRowContext(ctx, `SELECT pg_get_serial_sequence($1,$2)`, w.Dialect().QualifyTable(schema, s.TableName), s.ColumnName).Scan(&seq)
	if e != nil {
		return e
	}
	if !seq.Valid || seq.String == "" {
		return nil
	}
	// Ownership alone does not prove the current default uses this generator:
	// a repaired column may have been changed to nextval(other_sequence).
	var bound bool
	e = a.db.QueryRowContext(ctx, `SELECT COALESCE(row_to_json(a)->>'attidentity','') IN ('a','d') OR EXISTS (SELECT 1 FROM pg_attrdef d JOIN pg_depend dep ON dep.classid='pg_attrdef'::regclass AND dep.objid=d.oid AND dep.refclassid='pg_class'::regclass WHERE d.adrelid=a.attrelid AND d.adnum=a.attnum AND dep.refobjid=$3::regclass AND pg_get_expr(d.adbin,d.adrelid)='nextval(' || quote_literal($3::regclass::text) || '::regclass)') FROM pg_attribute a WHERE a.attrelid=$1::regclass AND a.attname=$2 AND NOT a.attisdropped`, w.Dialect().QualifyTable(schema, s.TableName), s.ColumnName, seq.String).Scan(&bound)
	if e != nil {
		return e
	}
	if !bound {
		return fmt.Errorf("列 DEFAULT 不是直接引用其所属序列的 nextval，请先修复并核对自增绑定")
	}
	var owner, name, increment string
	e = a.db.QueryRowContext(ctx, `SELECT n.nspname,c.relname,s.increment FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace JOIN information_schema.sequences s ON s.sequence_schema=n.nspname AND s.sequence_name=c.relname WHERE c.oid=$1::regclass AND c.relkind='S'`, seq.String).Scan(&owner, &name, &increment)
	if e != nil {
		return e
	}
	if owner != schema {
		return fmt.Errorf("序列属于其它 schema，拒绝修改")
	}
	step, e := repairInteger(increment)
	if e != nil {
		return e
	}
	if step.Sign() == 0 {
		return fmt.Errorf("序列步长为零")
	}
	qualified := w.Dialect().QualifyTable(owner, name)
	var last string
	var called bool
	if e = a.db.QueryRowContext(ctx, "SELECT last_value,is_called FROM "+qualified).Scan(&last, &called); e != nil {
		return e
	}
	current, e := repairInteger(last)
	if e != nil {
		return e
	}
	next := new(big.Int).Set(current)
	if called {
		next.Add(next, step)
	}
	desired := new(big.Int).Set(next)
	extreme, e := recoveryExtreme(ctx, w, a.db, schema, s, step)
	if e != nil {
		return e
	}
	if extreme != nil {
		desired = farther(desired, afterExtreme(extreme, step), step)
	}
	start, e := recoveryStart(s)
	if e != nil {
		return e
	}
	if start.Sign() != 0 {
		desired = farther(desired, start, step)
	}
	desired = advanceProgression(next, desired, step)
	if desired.Cmp(next) == 0 {
		return nil
	}
	_, e = execObserved(ctx, a.db, "SELECT setval("+repairLiteral(qualified)+"::regclass, "+desired.String()+", false)")
	return e
}

func calibrateMySQL(ctx context.Context, w Writer, a repairTarget, schema string, s source.SequenceInfo) error {
	var extra string
	e := a.db.QueryRowContext(ctx, `SELECT EXTRA FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND COLUMN_NAME=?`, schema, s.TableName, s.ColumnName).Scan(&extra)
	if e == sql.ErrNoRows {
		return nil
	}
	if e != nil {
		return e
	}
	if !strings.Contains(extra, "auto_increment") {
		return nil
	}
	var value sql.NullString
	if e = a.db.QueryRowContext(ctx, `SELECT AUTO_INCREMENT FROM information_schema.TABLES WHERE TABLE_SCHEMA=? AND TABLE_NAME=?`, schema, s.TableName).Scan(&value); e != nil {
		return e
	}
	if !value.Valid {
		return nil
	}
	current, e := repairInteger(value.String)
	if e != nil {
		return e
	}
	step := big.NewInt(1)
	extreme, e := recoveryExtreme(ctx, w, a.db, schema, s, step)
	if e != nil {
		return e
	}
	desired := new(big.Int).Set(current)
	if extreme != nil {
		desired = farther(desired, new(big.Int).Add(extreme, step), step)
	}
	start, e := recoveryStart(s)
	if e != nil {
		return e
	}
	if start.Sign() > 0 {
		desired = farther(desired, start, step)
	}
	if desired.Cmp(current) == 0 {
		return nil
	}
	_, e = execObserved(ctx, a.db, "ALTER TABLE "+w.Dialect().QualifyTable(schema, s.TableName)+" AUTO_INCREMENT = "+desired.String())
	return e
}

func calibrateSQLServer(ctx context.Context, w *SQLServerWriter, s source.SequenceInfo) error {
	qualified := w.dia.QualifyTable(w.schema, s.TableName)
	var increment, seed string
	var last sql.NullString
	e := w.db.QueryRowContext(ctx, `SELECT CONVERT(varchar(50),increment_value),CONVERT(varchar(50),seed_value),CONVERT(varchar(50),last_value) FROM sys.identity_columns WHERE object_id=OBJECT_ID(@p1) AND name=@p2`, qualified, s.ColumnName).Scan(&increment, &seed, &last)
	if e == sql.ErrNoRows {
		return nil
	}
	if e != nil {
		return e
	}
	step, e := repairInteger(increment)
	if e != nil {
		return e
	}
	if step.Sign() == 0 {
		return fmt.Errorf("IDENTITY 步长为零")
	}
	extreme, e := recoveryExtreme(ctx, w, w.db, w.schema, s, step)
	if e != nil {
		return e
	}
	if extreme == nil {
		return nil
	}
	current, e := repairInteger(seed)
	if e != nil {
		return e
	}
	if last.Valid {
		current, e = repairInteger(last.String)
		if e != nil {
			return e
		}
	}
	next := new(big.Int).Set(current)
	if last.Valid {
		next.Add(next, step)
	}
	desiredNext := farther(next, afterExtreme(extreme, step), step)
	start, e := recoveryStart(s)
	if e != nil {
		return e
	}
	if start.Sign() != 0 {
		desiredNext = farther(desiredNext, start, step)
	}
	desiredNext = advanceProgression(next, desiredNext, step)
	desired := new(big.Int).Sub(desiredNext, step)
	if desired.Cmp(current) == 0 && last.Valid {
		return nil
	}
	q := "DBCC CHECKIDENT (" + dialect.SQLServerLiteral(qualified) + ", RESEED, " + desired.String() + ") WITH NO_INFOMSGS"
	return w.execStatements(ctx, []dialect.Statement{{SQL: q}})
}

func calibrateOracle(ctx context.Context, w *OracleWriter, s source.SequenceInfo) error {
	// inspectSequence proves both the trigger's table/column marker and sequence
	// reference. A same-name unowned sequence is never adjusted.
	if e := w.inspectSequence(ctx, s); e != nil {
		return e
	}
	name := w.dia.SequenceName(s)
	if !w.dia.OwnedSequences[name] {
		return nil
	}
	var increment, last string
	e := w.db.QueryRowContext(ctx, `SELECT TO_CHAR(INCREMENT_BY,'TM9'),TO_CHAR(LAST_NUMBER,'TM9') FROM ALL_SEQUENCES WHERE SEQUENCE_OWNER=:1 AND SEQUENCE_NAME=:2`, w.schema, name).Scan(&increment, &last)
	if e == sql.ErrNoRows {
		return nil
	}
	if e != nil {
		return e
	}
	step, e := repairInteger(increment)
	if e != nil {
		return e
	}
	if step.Sign() == 0 {
		return fmt.Errorf("序列步长为零")
	}
	current, e := repairInteger(last)
	if e != nil {
		return e
	}
	extreme, e := recoveryExtreme(ctx, w, w.db, w.schema, s, step)
	if e != nil {
		return e
	}
	desired := new(big.Int).Set(current)
	if extreme != nil {
		desired = farther(desired, afterExtreme(extreme, step), step)
	}
	start, e := recoveryStart(s)
	if e != nil {
		return e
	}
	if start.Sign() != 0 {
		desired = farther(desired, start, step)
	}
	desired = advanceProgression(current, desired, step)
	seq := w.dia.QualifyTable(w.schema, name)
	compare := "<"
	if step.Sign() < 0 {
		compare = ">"
	}
	// LAST_NUMBER is a cache boundary, not the actual next value. Always read
	// NEXTVAL inside the block, even if the catalog boundary already exceeds
	// imported rows. Oracle 11g has no RESTART WITH: temporarily advance the
	// increment and restore it even on an exception, without DROP/CREATE.
	restore := repairLiteral("ALTER SEQUENCE " + seq + " INCREMENT BY " + step.String())
	q := "DECLARE n NUMBER; jump NUMBER; changed BOOLEAN := FALSE; BEGIN SELECT " + seq + ".NEXTVAL INTO n FROM DUAL; IF n " + compare + " " + desired.String() + " THEN jump := " + desired.String() + " - n; EXECUTE IMMEDIATE " + repairLiteral("ALTER SEQUENCE "+seq+" INCREMENT BY ") + " || TO_CHAR(jump,'TM9','NLS_NUMERIC_CHARACTERS=''.,'''); changed := TRUE; SELECT " + seq + ".NEXTVAL INTO n FROM DUAL; EXECUTE IMMEDIATE " + restore + "; changed := FALSE; END IF; EXCEPTION WHEN OTHERS THEN IF changed THEN EXECUTE IMMEDIATE " + restore + "; END IF; RAISE; END;"
	return w.execStatements(ctx, []dialect.Statement{{SQL: q}})
}

func calibrateDameng(ctx context.Context, w *DaMengWriter, s source.SequenceInfo) error {
	var count int
	e := w.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM SYS.SYSCOLUMNS c JOIN ALL_OBJECTS o ON o.object_id=c.id WHERE o.owner=? AND o.object_name=? AND c.name=? AND c.INFO2 & 0x01=0x01`, w.schema, s.TableName, s.ColumnName).Scan(&count)
	if e != nil {
		return e
	}
	if count == 0 {
		return nil
	}
	qualified := w.dia.QualifyTable(w.schema, s.TableName)
	var increment, last string
	if e = w.db.QueryRowContext(ctx, `SELECT IDENT_INCR(?),IDENT_CURRENT(?)`, qualified, qualified).Scan(&increment, &last); e != nil {
		return e
	}
	step, e := repairInteger(increment)
	if e != nil {
		return e
	}
	if step.Sign() == 0 {
		return fmt.Errorf("IDENTITY 步长为零")
	}
	current, e := repairInteger(last)
	if e != nil {
		return e
	}
	extreme, e := recoveryExtreme(ctx, w, w.db, w.schema, s, step)
	if e != nil {
		return e
	}
	next := new(big.Int).Add(current, step)
	bound := new(big.Int).Set(next)
	if extreme != nil {
		bound = farther(bound, afterExtreme(extreme, step), step)
	}
	start, e := recoveryStart(s)
	if e != nil {
		return e
	}
	if start.Sign() != 0 {
		bound = farther(bound, start, step)
	}
	if bound.Cmp(next) != 0 {
		return fmt.Errorf("达梦 IDENTITY 当前值 %s 尚未覆盖数据边界 %s；没有可验证的无重建校准操作，请单独修复", current, extreme)
	}
	return nil
}
