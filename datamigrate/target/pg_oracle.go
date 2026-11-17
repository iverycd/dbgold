package target

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/typemap"
	"dbgold/datamigrate/valueconv"
)

// PGOracleProfile describes the observed destination, not vendor certification.
type PGOracleProfile struct {
	Product      string `json:"product"`
	Version      string `json:"version"`
	Schema       string `json:"schema"`
	Charset      string `json:"charset"`
	NCharset     string `json:"ncharset"`
	Verification string `json:"verification"`
}

func (w *OracleWriter) InspectPGOracle(ctx context.Context) (PGOracleProfile, error) {
	p := PGOracleProfile{Product: "oracle", Schema: w.schema, Verification: "未完成实库验证（能力探测不等于实库认证）"}
	if err := w.db.QueryRowContext(ctx, `SELECT VERSION FROM PRODUCT_COMPONENT_VERSION WHERE PRODUCT LIKE 'Oracle%' AND ROWNUM=1`).Scan(&p.Version); err != nil {
		return p, err
	}
	major, _ := strconv.Atoi(strings.Split(p.Version, ".")[0])
	if major < 11 {
		return p, fmt.Errorf("Oracle %s 不满足 11g 兼容基线", p.Version)
	}
	rows, err := w.db.QueryContext(ctx, `SELECT PARAMETER,VALUE FROM NLS_DATABASE_PARAMETERS WHERE PARAMETER IN ('NLS_CHARACTERSET','NLS_NCHAR_CHARACTERSET')`)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err = rows.Scan(&k, &v); err != nil {
			return p, err
		}
		if k == "NLS_CHARACTERSET" {
			p.Charset = v
		} else {
			p.NCharset = v
		}
	}
	if err := rows.Err(); err != nil {
		return p, err
	}
	if p.Charset == "" || p.NCharset == "" {
		return p, fmt.Errorf("Oracle 字符集配置不完整，不能验证目标容量")
	}
	return p, nil
}

func (w *OracleWriter) PGOracleRelationExists(ctx context.Context, name string) (bool, error) {
	if err := w.ValidatePGOracleName(name); err != nil {
		return false, err
	}
	var count int
	err := w.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_OBJECTS WHERE OWNER=:1 AND OBJECT_NAME=:2 AND OBJECT_TYPE IN ('TABLE','VIEW') AND STATUS='VALID'`, w.schema, name).Scan(&count)
	return count > 0, err
}

// ValidatePGOracleDataTable is observational and also used before recovery.
// Only exact, frozen tool trigger programs may run during a data-only load.
func (w *OracleWriter) ValidatePGOracleDataTable(ctx context.Context, table string, columns []source.ColumnInfo, allowedTriggers map[string]string) error {
	if err := w.ValidatePGOracleName(table); err != nil {
		return err
	}
	var tableCount int
	if err := w.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_TABLES WHERE OWNER=:1 AND TABLE_NAME=:2 AND TEMPORARY='N' AND SECONDARY='N' AND NESTED='NO'`, w.schema, table).Scan(&tableCount); err != nil {
		return err
	}
	if tableCount != 1 {
		return fmt.Errorf("目标 %s.%s 不是可迁入的普通表", w.schema, table)
	}
	// Recovery and data-only preflights must observe repaired definitions afresh.
	w.mu.Lock()
	delete(w.columns, table)
	w.mu.Unlock()
	actual, err := w.loadColumns(ctx, table)
	if err != nil {
		return err
	}
	if len(actual) == 0 {
		return fmt.Errorf("目标表 %s 不存在或没有可写列", table)
	}
	wanted := map[string]bool{}
	for _, c := range columns {
		name := strings.ToUpper(c.Name)
		if wanted[name] {
			return fmt.Errorf("源列大写映射冲突: %s", name)
		}
		wanted[name] = true
		if err := w.ValidatePGOracleName(name); err != nil {
			return err
		}
		dst, ok := actual[name]
		if !ok {
			return fmt.Errorf("目标表 %s 缺少列 %s", table, name)
		}
		if err := w.pgOracleCompatibleColumn(c, dst); err != nil {
			return fmt.Errorf("目标表 %s.%s: %w", table, name, err)
		}
	}
	// Extra required columns without defaults cannot be supplied by this load;
	// virtual columns must never be targets of an explicit INSERT.
	rows, err := w.db.QueryContext(ctx, `SELECT COLUMN_NAME,NULLABLE,DEFAULT_LENGTH,VIRTUAL_COLUMN FROM ALL_TAB_COLS WHERE OWNER=:1 AND TABLE_NAME=:2 AND HIDDEN_COLUMN='NO'`, w.schema, table)
	if err != nil {
		return err
	}
	for rows.Next() {
		var name, nullable, virtual string
		var length sql.NullInt64
		if err = rows.Scan(&name, &nullable, &length, &virtual); err != nil {
			rows.Close()
			return err
		}
		if wanted[name] && virtual == "YES" {
			rows.Close()
			return fmt.Errorf("目标列 %s.%s 是虚拟列", table, name)
		}
		if !wanted[name] && nullable == "N" && (!length.Valid || length.Int64 == 0) && virtual != "YES" {
			rows.Close()
			return fmt.Errorf("目标必填列 %s.%s 未映射且无默认值", table, name)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	return w.ValidatePGOracleTriggers(ctx, table, allowedTriggers)
}

func (w *OracleWriter) ValidatePGOracleTriggers(ctx context.Context, table string, allowed map[string]string) error {
	rows, err := w.db.QueryContext(ctx, `SELECT OWNER,TRIGGER_NAME FROM ALL_TRIGGERS WHERE TABLE_OWNER=:1 AND TABLE_NAME=:2 AND STATUS='ENABLED' ORDER BY OWNER,TRIGGER_NAME`, w.schema, table)
	if err != nil {
		return err
	}
	type trigger struct{ owner, name string }
	var found []trigger
	for rows.Next() {
		var t trigger
		if err = rows.Scan(&t.owner, &t.name); err != nil {
			rows.Close()
			return err
		}
		found = append(found, t)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, t := range found {
		expected := allowed[t.name]
		if t.owner != w.schema || expected == "" {
			return fmt.Errorf("目标表 %s 存在启用的业务触发器 %s.%s，未自动禁用", table, t.owner, t.name)
		}
		sourceRows, e := w.db.QueryContext(ctx, `SELECT TEXT FROM ALL_SOURCE WHERE OWNER=:1 AND NAME=:2 AND TYPE='TRIGGER' ORDER BY LINE`, t.owner, t.name)
		if e != nil {
			return e
		}
		var body strings.Builder
		for sourceRows.Next() {
			var line string
			if e = sourceRows.Scan(&line); e != nil {
				sourceRows.Close()
				return e
			}
			body.WriteString(line)
		}
		e = sourceRows.Err()
		sourceRows.Close()
		if e != nil {
			return e
		}
		// ALL_SOURCE omits CREATE OR REPLACE and may omit the owner qualifier.
		normalize := func(s string) string {
			s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), ";"))
			s = strings.TrimPrefix(s, "CREATE OR REPLACE ")
			s = strings.Replace(s, "TRIGGER "+w.dia.QualifyTable(w.schema, t.name), "TRIGGER "+w.dia.QuoteIdent(t.name), 1)
			return s
		}
		if normalize(strings.ReplaceAll(body.String(), "\r\n", "\n")) != normalize(strings.ReplaceAll(expected, "\r\n", "\n")) {
			return fmt.Errorf("工具触发器 %s 的定义已漂移", t.name)
		}
		if err := w.validateObject(ctx, t.name, "TRIGGER"); err != nil {
			return err
		}
	}
	return nil
}

// ValidatePGOracleName applies the Oracle 11g byte limit in the destination
// character set rather than assuming UTF-8 for every Oracle installation.
func (w *OracleWriter) ValidatePGOracleName(name string) error {
	if err := dialect.ValidatePGOracleName(name); err != nil {
		return err
	}
	encoded := []byte(name)
	if w.codec != nil {
		codec := w.codec.Clone()
		encoded = codec.Encode(name)
		if codec.Decode(encoded) != name {
			return fmt.Errorf("对象名 %s 含 Oracle 目标字符集无法表示的字符", name)
		}
	}
	if len(encoded) > 30 {
		return fmt.Errorf("Oracle 对象名 %s 在目标字符集中超过 30 字节", name)
	}
	return nil
}

// ValidatePGOracleStatements checks final generated SQL without executing DDL.
// Oracle syntax/privileges are still verified when the statement executes.
func (w *OracleWriter) ValidatePGOracleStatements(ctx context.Context, statements []dialect.Statement) error {
	for _, statement := range statements {
		if err := ctx.Err(); err != nil {
			return err
		}
		if strings.TrimSpace(statement.SQL) == "" {
			return fmt.Errorf("Oracle DDL 为空")
		}
		if err := w.validateSQLText(statement.SQL); err != nil {
			return err
		}
		if err := w.validatePGOracleQuotedNames(statement.SQL); err != nil {
			return err
		}
	}
	return nil
}

// Scan only quoted identifiers. The expression parser intentionally rejects
// some generated PL/SQL operators, and names inside string literals/comments
// must not be mistaken for identifiers (including UNISTR and dynamic SQL).
func (w *OracleWriter) validatePGOracleQuotedNames(sql string) error {
	for i := 0; i < len(sql); {
		if strings.HasPrefix(sql[i:], "--") {
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
			continue
		}
		if strings.HasPrefix(sql[i:], "/*") {
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				return fmt.Errorf("Oracle SQL 注释未闭合")
			}
			i += end + 4
			continue
		}
		quote := sql[i]
		if quote != '\'' && quote != '"' {
			i++
			continue
		}
		i++
		var value strings.Builder
		closed := false
		for i < len(sql) {
			if sql[i] == quote {
				i++
				if i < len(sql) && sql[i] == quote {
					value.WriteByte(quote)
					i++
					continue
				}
				closed = true
				break
			}
			value.WriteByte(sql[i])
			i++
		}
		if !closed {
			return fmt.Errorf("Oracle SQL 引号未闭合")
		}
		if quote == '"' {
			if err := w.ValidatePGOracleName(value.String()); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *OracleWriter) ExecutePGOracleStatements(ctx context.Context, kind, name string, statements []dialect.Statement) error {
	if len(statements) == 0 {
		return fmt.Errorf("对象 %s 缺少可执行 Oracle DDL", name)
	}
	if kind == "table" {
		return w.CreateTable(ctx, statements)
	}
	if err := w.execStatements(ctx, statements); err != nil {
		return err
	}
	if kind == "view" || kind == "trigger" {
		return w.observeValidity(ctx, statements, name, strings.ToUpper(kind))
	}
	return nil
}

var pgOracleTypeParameters = regexp.MustCompile(`\([^)]*\)`)

func (w *OracleWriter) pgOracleCompatibleColumn(c source.ColumnInfo, dst valueconv.OracleColumn) error {
	if w.crossSource != "" {
		return w.crossOracleCompatibleColumn(c, dst)
	}
	dt := strings.Join(strings.Fields(pgOracleTypeParameters.ReplaceAllString(strings.ToUpper(dst.DataType), "")), " ")
	national := dt == "NCHAR" || dt == "NVARCHAR2" || dt == "NCLOB"
	mapped, err := typemap.PGToOracleChecked(c, national, w.dia.CharBytes, w.dia.NCharBytes)
	if err != nil {
		return err
	}
	base := strings.Join(strings.Fields(pgOracleTypeParameters.ReplaceAllString(mapped, "")), " ")
	if base != dt {
		return fmt.Errorf("类型 %s 不匹配 PG 映射 %s", dst.DataType, mapped)
	}
	if c.IsNullable && !dst.Nullable {
		return fmt.Errorf("可空源列不能写入非空目标列")
	}
	src := typemap.PGOracleBaseType(c.DataType)
	switch dt {
	case "NUMBER":
		dp := dst.Precision
		if dp == 0 && (dst.ScaleKnown || dst.Scale != 0) {
			dp = 38
		}
		if dp == 0 {
			return nil
		}
		sp, ss := c.Precision, c.Scale
		switch src {
		case "int2":
			sp, ss = 5, 0
		case "int4":
			sp, ss = 10, 0
		case "int8":
			sp, ss = 19, 0
		case "bool":
			sp, ss = 1, 0
		}
		if sp == 0 || dst.Scale < ss || dp-dst.Scale < sp-ss {
			return fmt.Errorf("目标 NUMBER(%d,%d) 缩小源数值范围/精度", dp, dst.Scale)
		}
	case "CHAR", "NCHAR", "VARCHAR2", "NVARCHAR2":
		n := c.Length
		switch src {
		case "uuid":
			n = 36
		case "inet", "cidr":
			n = 49
		case "macaddr":
			n = 17
		case "macaddr8":
			n = 23
		case "bpchar":
			n = max(n, 1)
			if dst.CharLength != n {
				return fmt.Errorf("目标 CHAR 长度必须等于源 bpchar 长度 %d，避免改变尾空格填充", n)
			}
		}
		width := max(w.dia.CharBytes, 1)
		if national {
			width = max(w.dia.NCharBytes, 1)
		}
		units := n
		if national {
			units = 2 * n
		}
		if dst.CharLength < units || dst.DataLength < n*int64(width) {
			return fmt.Errorf("目标字符容量无法保留源声明长度 %d", n)
		}
	case "TIMESTAMP", "TIMESTAMP WITH TIME ZONE":
		if dst.Scale < c.DateTimePrecision {
			return fmt.Errorf("目标时间精度 %d 低于源精度 %d", dst.Scale, c.DateTimePrecision)
		}
	}
	return nil
}
