package target

import (
	"context"
	"fmt"

	"dbgold/datamigrate/oracledameng"
)

// ValidateOracleDamengView probes only mode-sensitive constructs actually used
// by the original Oracle SELECT. It never changes a session or writes an object.
func (w *DaMengWriter) ValidateOracleDamengView(ctx context.Context, definition string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	features, err := oracledameng.ViewSemanticsFeatures(definition)
	if err != nil {
		return err
	}
	useDate := false
	if d, ok := w.dia.(interface{ OracleDamengUsesDate() bool }); ok {
		useDate = d.OracleDamengUsesDate()
	}
	w.oracleDMMu.Lock()
	defer w.oracleDMMu.Unlock()
	if w.oracleDM.features == nil {
		w.oracleDM.features = map[string]error{}
	}
	for _, feature := range features {
		key := "view:" + feature
		if feature == "cast_date" {
			key += fmt.Sprintf(":%t", useDate)
		}
		if cached, ok := w.oracleDM.features[key]; ok {
			if cached != nil {
				return cached
			}
			continue
		}
		probe, args, err := oracleDMViewSentinel(feature, useDate)
		if err == nil {
			var same int
			err = w.db.QueryRowContext(ctx, probe, args...).Scan(&same)
			if err == nil && same != 1 {
				err = fmt.Errorf("目标返回的表达式行为与 Oracle 不一致")
			}
		}
		if err != nil {
			err = fmt.Errorf("视图表达式 %s 的 Oracle 语义未通过只读验证，未执行该视图 DDL: %w", feature, err)
		}
		w.oracleDM.features[key] = err
		if err != nil {
			return err
		}
	}
	return nil
}

func oracleDMViewSentinel(feature string, useDate bool) (string, []any, error) {
	var predicate string
	var args []any
	switch feature {
	case "operator_concat":
		predicate = `(CAST(NULL AS VARCHAR(10)) || 'x')='x' AND ('x' || CAST(NULL AS VARCHAR(10)))='x' AND (CAST(NULL AS VARCHAR(10)) || CAST(NULL AS VARCHAR(10))) IS NULL`
	case "CONCAT":
		predicate = `CONCAT(CAST(NULL AS VARCHAR(10)),'x')='x' AND CONCAT('x',CAST(NULL AS VARCHAR(10)))='x' AND CONCAT(CAST(NULL AS VARCHAR(10)),CAST(NULL AS VARCHAR(10))) IS NULL`
	case "TRIM":
		predicate = `TRIM(' ') IS NULL AND TRIM(' x ')='x'`
	case "LTRIM":
		predicate = `LTRIM('xxx','x') IS NULL AND LTRIM('xxAb','x')='Ab'`
	case "RTRIM":
		predicate = `RTRIM('xxx','x') IS NULL AND RTRIM('Abxx','x')='Ab'`
	case "REPLACE":
		predicate = `REPLACE('a','a',NULL) IS NULL AND REPLACE('aba','a',NULL)='b'`
	case "TRANSLATE":
		predicate = `TRANSLATE('b','ab','x') IS NULL AND TRANSLATE('aba','ab','x')='xx' AND TRANSLATE('a','a',NULL) IS NULL`
	case "SUBSTR":
		predicate = `SUBSTR('abc',1,0) IS NULL AND SUBSTR('abc',0,1)='a' AND SUBSTR('abc',-1,1)='c' AND SUBSTR('汉字',2,1)='字'`
	case "SUBSTRB":
		return "", nil, fmt.Errorf("SUBSTRB 的字节位置依赖源/目标字符集，尚无已验证的字符集转换路径")
	case "LPAD":
		predicate = `LPAD('汉',3,'x')='xx汉' AND LPAD(NULL,3,'x') IS NULL AND LPAD('x',0,'a') IS NULL AND LPAD('x',3,NULL) IS NULL`
	case "RPAD":
		predicate = `RPAD('汉',3,'x')='汉xx' AND RPAD(NULL,3,'x') IS NULL AND RPAD('x',0,'a') IS NULL AND RPAD('x',3,NULL) IS NULL`
	case "SOUNDEX":
		predicate = `SOUNDEX('Robert')='R163' AND SOUNDEX('汉字') IS NULL`
	case "LAST_DAY":
		// Oracle returns DATE even for TIMESTAMP input: retain whole seconds,
		// discard the fraction, and never round into the following second.
		predicate = `TO_CHAR(LAST_DAY(TO_TIMESTAMP(?, 'YYYY-MM-DD HH24:MI:SS')), 'YYYY-MM-DD HH24:MI:SS')=? AND EXTRACT(SECOND FROM CAST(LAST_DAY(TO_TIMESTAMP(?, 'YYYY-MM-DD HH24:MI:SS.FF6')) AS TIMESTAMP(6)))=5`
		args = []any{"2024-02-10 03:04:05", "2024-02-29 03:04:05", "2024-02-10 03:04:05.900000"}
	case "integer_division":
		predicate = `1/2=CAST(0.5 AS DECIMAL(2,1))`
	case "cast_date":
		targetType := "TIMESTAMP(0)"
		if useDate {
			targetType = "DATE"
		}
		predicate = fmt.Sprintf(`TO_CHAR(CAST(TO_TIMESTAMP(?, 'YYYY-MM-DD HH24:MI:SS.FF9') AS %s), 'YYYY-MM-DD HH24:MI:SS')=?`, targetType)
		args = []any{"2024-01-02 03:04:05.900000000", "2024-01-02 03:04:05"}
	default:
		return "", nil, fmt.Errorf("未知视图语义能力 %s", feature)
	}
	return "SELECT CASE WHEN " + predicate + " THEN 1 ELSE 0 END FROM DUAL", args, nil
}
