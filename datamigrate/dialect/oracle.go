package dialect

import (
	"crypto/sha256"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/typemap"
)

type OracleDialect struct {
	CharBytes, NCharBytes int
	// Filled by observational target inspection before any DROP. Never infer
	// ownership from a generated name alone.
	OwnedSequences map[string]bool
}

func NewOracle() *OracleDialect {
	return &OracleDialect{CharBytes: 4, NCharBytes: 4, OwnedSequences: map[string]bool{}}
}
func (d *OracleDialect) Name() string       { return "oracle" }
func (d *OracleDialect) Caps() Capabilities { return Capabilities{UsesSequences: true} }
func (d *OracleDialect) QuoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
func (d *OracleDialect) QualifyTable(schema, table string) string {
	if schema == "" {
		return d.QuoteIdent(table)
	}
	return d.QuoteIdent(schema) + "." + d.QuoteIdent(table)
}
func (d *OracleDialect) MapType(c source.ColumnInfo, src string, opt TypeOpt) string {
	t, _ := typemap.OracleType(c, opt.UseNvarchar2, d.CharBytes, d.NCharBytes)
	return t
}

// OracleObjectName has a stable ASCII digest and never splits a UTF-8 rune.
func OracleObjectName(parts ...string) string {
	n := strings.ToUpper(strings.Join(parts, "_"))
	if len(n) <= 30 {
		return n
	}
	sum := sha256.Sum256([]byte(n))
	prefix := n[:19]
	for !utf8.ValidString(prefix) {
		prefix = prefix[:len(prefix)-1]
	}
	return fmt.Sprintf("%s_%x", prefix, sum[:5])
}
func ValidateOracleName(s string) error {
	if s == "" || len(s) > 30 || strings.ContainsAny(s, "\x00\r\n\"") || !utf8.ValidString(s) {
		return fmt.Errorf("Oracle 对象名 %q 必须为 1–30 字节有效标识符", s)
	}
	if s == "ROWID" {
		return fmt.Errorf("Oracle 不允许使用大写 ROWID 作为列名")
	}
	return nil
}

func (d *OracleDialect) CreateTableStatements(schema string, info *source.TableDDLInfo, src string, opt TypeOpt, name NameFunc) ([]Statement, error) {
	if src != "mysql" {
		return nil, fmt.Errorf("Oracle 目标仅支持 MySQL 源库")
	}
	if err := ValidateOracleName(name(info.TableName)); err != nil {
		return nil, err
	}
	cols := []string{}
	seen := map[string]bool{}
	for _, c := range info.Columns {
		n := name(c.Name)
		if err := ValidateOracleName(n); err != nil {
			return nil, err
		}
		if seen[n] {
			return nil, fmt.Errorf("表 %s 的列名转大写后冲突: %s", info.TableName, n)
		}
		seen[n] = true
		t, err := typemap.OracleType(c, opt.UseNvarchar2, d.CharBytes, d.NCharBytes)
		if err != nil {
			return nil, fmt.Errorf("表 %s: %w", info.TableName, err)
		}
		extra := strings.ToLower(c.Extra)
		if c.Generated != "" || strings.Contains(extra, "virtual generated") || strings.Contains(extra, "stored generated") || strings.Contains(extra, "on update") {
			return nil, fmt.Errorf("表 %s 列 %s 的生成列/自动更新时间规则无法等价迁移", info.TableName, c.Name)
		}
		def, err := oracleDefault(c, t)
		if err != nil {
			return nil, fmt.Errorf("表 %s 列 %s: %w", info.TableName, c.Name, err)
		}
		col := d.QuoteIdent(n) + " " + t + def
		if !c.IsNullable {
			col += " NOT NULL"
		}
		cols = append(cols, col)
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("表 %s 无列", info.TableName)
	}
	qn := d.QualifyTable(schema, name(info.TableName))
	return []Statement{{SQL: "DROP TABLE " + qn + " CASCADE CONSTRAINTS"}, {SQL: "CREATE TABLE " + qn + " (\n  " + strings.Join(cols, ",\n  ") + "\n)"}}, nil
}

var oracleNumericLiteral = regexp.MustCompile(`^[+-]?[0-9]+(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)
var oracleNowDefault = regexp.MustCompile(`(?i)^(?:CURRENT_TIMESTAMP|NOW|LOCALTIMESTAMP)(?:\([0-6]?\))?$`)
var oracleDateLiteral = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}(?: \d{2}:\d{2}:\d{2}(?:\.\d{1,6})?)?$`)

func oracleLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
func oracleDefault(c source.ColumnInfo, target string) (string, error) {
	if c.Default == nil || strings.Contains(strings.ToLower(c.Extra), "auto_increment") {
		return "", nil
	}
	v := *c.Default
	if strings.ContainsRune(v, '\x00') {
		return "", fmt.Errorf("暂不支持含 NUL 的默认值")
	}
	dt := strings.ToLower(c.DataType)
	date := dt == "date" || dt == "datetime" || dt == "timestamp"
	zeroDate := date && (strings.HasPrefix(v, "0000-") || len(v) >= 10 && (v[5:7] == "00" || v[8:10] == "00"))
	if v == "" || zeroDate {
		if !c.IsNullable {
			return "", fmt.Errorf("非空列的空字符串/零日期默认值不能转换为 NULL")
		}
		return " DEFAULT NULL", nil
	}
	if date && oracleNowDefault.MatchString(v) {
		return " DEFAULT CURRENT_TIMESTAMP", nil
	}
	if strings.Contains(strings.ToLower(c.Extra), "default_generated") {
		return "", fmt.Errorf("暂不支持默认值表达式 %q", v)
	}
	if strings.HasPrefix(target, "NUMBER") || strings.HasPrefix(target, "BINARY_") {
		if dt == "bit" && len(v) > 3 && (v[0] == 'b' || v[0] == 'B') && v[1] == '\'' && v[len(v)-1] == '\'' {
			n, ok := new(big.Int).SetString(v[2:len(v)-1], 2)
			if !ok {
				return "", fmt.Errorf("无效 BIT 默认值")
			}
			v = n.String()
		}
		if !oracleNumericLiteral.MatchString(v) {
			return "", fmt.Errorf("不支持的数值默认值 %q", v)
		}
		if target == "BINARY_FLOAT" {
			return " DEFAULT " + v + "F", nil
		}
		if target == "BINARY_DOUBLE" {
			return " DEFAULT " + v + "D", nil
		}
		return " DEFAULT " + v, nil
	}
	if date {
		if !oracleDateLiteral.MatchString(v) {
			return "", fmt.Errorf("无效日期默认值 %q", v)
		}
		layout := "2006-01-02 15:04:05.999999"
		if len(v) == 10 {
			layout = "2006-01-02"
		}
		if _, err := time.Parse(layout, v); err != nil {
			return "", fmt.Errorf("无效日期默认值 %q", v)
		}
		if len(v) == 10 {
			return " DEFAULT DATE " + oracleLiteral(v), nil
		}
		return " DEFAULT TIMESTAMP " + oracleLiteral(v), nil
	}
	if target == "BLOB" {
		return "", fmt.Errorf("暂不支持 BLOB 默认值")
	}
	if len(v) > 1000 {
		return "", fmt.Errorf("默认值超过安全 SQL 字面量长度")
	}
	return " DEFAULT " + oracleLiteral(v), nil
}
