package dialect

import (
	"crypto/sha256"
	"dbgold/datamigrate/source"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

func ValidateCrossOracleSequence(s source.SequenceInfo) error {
	if s.Unsupported != "" {
		return fmt.Errorf("%s", s.Unsupported)
	}
	if s.Native == nil {
		return fmt.Errorf("缺少冻结序列元数据")
	}
	n := s.Native
	if err := ValidateOracleName(n.Name); err != nil {
		return err
	}
	if n.Cycle || n.Ordered {
		return fmt.Errorf("循环/有序序列需要人工适配")
	}
	cache, err := strconv.ParseInt(n.CacheSize, 10, 64)
	if err != nil || cache < 0 || cache == 1 {
		return fmt.Errorf("序列缓存 %q 无法在 Oracle 保留", n.CacheSize)
	}
	values := []*big.Int{}
	for _, v := range []string{s.StartValueText, n.IncrementBy, n.MinValue, n.MaxValue} {
		x, ok := new(big.Int).SetString(v, 10)
		if !ok || len(strings.TrimPrefix(x.String(), "-")) > 28 {
			return fmt.Errorf("序列 %s 起点、步长或边界超出 Oracle 范围", n.Name)
		}
		values = append(values, x)
	}
	if values[1].Sign() == 0 || values[2].Cmp(values[3]) >= 0 || values[0].Cmp(values[2]) < 0 || values[0].Cmp(values[3]) > 0 {
		return fmt.Errorf("序列 %s 状态无效或已耗尽", n.Name)
	}
	if new(big.Int).Abs(new(big.Int).Set(values[1])).Cmp(new(big.Int).Sub(values[3], values[2])) >= 0 {
		return fmt.Errorf("序列步长超过边界跨度")
	}
	for _, b := range n.Bindings {
		if b.Schema != "" && b.Schema != n.Schema {
			return fmt.Errorf("序列绑定跨源 Schema")
		}
		if ValidateOracleName(b.TableName) != nil || ValidateOracleName(b.ColumnName) != nil {
			return fmt.Errorf("序列绑定对象名不合法")
		}
		if b.Kind != "identity" && b.Kind != "default" {
			return fmt.Errorf("不支持的序列绑定 %s", b.Kind)
		}
	}
	return nil
}
func CrossOracleSequenceMarker(s source.SequenceInfo) string {
	n := s.Native
	return fmt.Sprintf("DBGOLD_CROSS_ORACLE:%x", sha256.Sum256([]byte(strings.Join([]string{n.Schema, n.SourceName, n.Name, n.IncrementBy, n.MinValue, n.MaxValue, n.CacheSize}, "\x00"))))
}
func CrossOracleTriggerName(s source.SequenceInfo, b source.SequenceBinding) string {
	return OracleObjectName("TRG", s.Native.Name, b.TableName, b.ColumnName)
}

func (d *OracleDialect) crossSequenceStatements(schema string, s source.SequenceInfo) []Statement {
	if ValidateCrossOracleSequence(s) != nil {
		return nil
	}
	n := s.Native
	qualified := d.QualifyTable(schema, n.Name)
	step, _ := new(big.Int).SetString(n.IncrementBy, 10)
	agg, compare := "MAX", ">="
	if step.Sign() < 0 {
		agg, compare = "MIN", "<="
	}
	cacheSQL := "NOCACHE"
	if n.CacheSize != "0" {
		cacheSQL = "CACHE " + n.CacheSize
	}
	body := "DECLARE n NUMBER := " + s.StartValueText + "; x NUMBER; cur NUMBER; BEGIN\n"
	for _, b := range n.Bindings {
		body += "SELECT " + agg + "(" + d.QuoteIdent(b.ColumnName) + ") INTO x FROM " + d.QualifyTable(schema, b.TableName) + ";\nIF x " + compare + " n THEN n := n + (FLOOR(ABS(x-n)/ABS(" + n.IncrementBy + "))+1)*(" + n.IncrementBy + "); END IF;\n"
	}
	body += "IF n < " + n.MinValue + " OR n > " + n.MaxValue + " THEN RAISE_APPLICATION_ERROR(-20001,'Sequence range exhausted'); END IF;\n"
	if d.OwnedSequences[n.Name] {
		body += "SELECT " + qualified + ".NEXTVAL INTO cur FROM DUAL;\nIF n " + compare + " cur AND n <> cur THEN\nEXECUTE IMMEDIATE " + oracleLiteral("ALTER SEQUENCE "+qualified+" INCREMENT BY ") + " || TO_CHAR(n-cur,'TM9','NLS_NUMERIC_CHARACTERS=''.,''');\nBEGIN SELECT " + qualified + ".NEXTVAL INTO cur FROM DUAL; EXCEPTION WHEN OTHERS THEN EXECUTE IMMEDIATE " + oracleLiteral("ALTER SEQUENCE "+qualified+" INCREMENT BY "+n.IncrementBy) + "; RAISE; END;\nEXECUTE IMMEDIATE " + oracleLiteral("ALTER SEQUENCE "+qualified+" INCREMENT BY "+n.IncrementBy) + "; END IF;\n"
	} else {
		body += "EXECUTE IMMEDIATE " + oracleLiteral("CREATE SEQUENCE "+qualified+" START WITH ") + " || TO_CHAR(n,'TM9','NLS_NUMERIC_CHARACTERS=''.,''') || " + oracleLiteral(" INCREMENT BY "+n.IncrementBy+" MINVALUE "+n.MinValue+" MAXVALUE "+n.MaxValue+" NOCYCLE "+cacheSQL) + ";\n"
	}
	out := []Statement{{SQL: body + "END;"}}
	for _, b := range n.Bindings {
		trig := d.QualifyTable(schema, CrossOracleTriggerName(s, b))
		col := d.QuoteIdent(b.ColumnName)
		out = append(out, Statement{SQL: "CREATE OR REPLACE TRIGGER " + trig + " BEFORE INSERT ON " + d.QualifyTable(schema, b.TableName) + " FOR EACH ROW\nBEGIN\n/* " + CrossOracleSequenceMarker(s) + " */\nIF :NEW." + col + " IS NULL THEN SELECT " + qualified + ".NEXTVAL INTO :NEW." + col + " FROM DUAL; END IF;\nEND;"})
	}
	return out
}
