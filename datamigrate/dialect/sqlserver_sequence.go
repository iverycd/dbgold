package dialect

import (
	"crypto/sha256"
	"dbgold/datamigrate/source"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

const SQLServerSequenceMarker = "DBGoldSequenceDefinition"

// SQLServerNativeSequence describes the checked target definition. A live
// start is not part of its marker because the counter advances independently.
type SQLServerNativeSequence struct {
	Start, Increment, Min, Max *big.Int
	Cache                      int64
	Marker                     string
}

func ValidateSQLServerNativeSequence(s source.SequenceInfo) (SQLServerNativeSequence, error) {
	n := s.Native
	if n == nil {
		return SQLServerNativeSequence{}, fmt.Errorf("缺少 SQL Server 命名序列元数据")
	}
	bad := func(reason string) (SQLServerNativeSequence, error) {
		return SQLServerNativeSequence{}, fmt.Errorf("序列 %s: %s", n.Name, reason)
	}
	if err := ValidateSQLServerName(n.Name); err != nil {
		return SQLServerNativeSequence{}, err
	}
	if n.Cycle || n.Ordered {
		return bad("有序或循环语义需要人工适配")
	}
	if s.Unsupported != "" {
		return bad(s.Unsupported)
	}
	start := s.StartValueText
	if start == "" {
		start = strconv.FormatInt(s.StartValue, 10)
	}
	values := []*big.Int{}
	for _, raw := range []string{start, n.IncrementBy, n.MinValue, n.MaxValue} {
		v, ok := new(big.Int).SetString(raw, 10)
		if !ok || len(strings.TrimPrefix(v.String(), "-")) > 38 {
			return bad("起点、步长或边界不能用 DECIMAL(38,0) 表示")
		}
		values = append(values, v)
	}
	spec := SQLServerNativeSequence{Start: values[0], Increment: values[1], Min: values[2], Max: values[3]}
	span := new(big.Int).Sub(spec.Max, spec.Min)
	width := new(big.Int).Abs(spec.Increment)
	if width.Sign() == 0 || width.Cmp(span) > 0 || spec.Start.Cmp(spec.Min) < 0 || spec.Start.Cmp(spec.Max) > 0 {
		return bad("起点、步长和边界不兼容")
	}
	if n.CacheSize != "" {
		cache, err := strconv.ParseInt(n.CacheSize, 10, 32)
		if err != nil || cache < 0 {
			return bad("缓存大小超出 SQL Server 范围")
		}
		// A source CACHE 1 often denotes an effectively uncached sequence.
		if cache > 1 {
			spec.Cache = cache
		}
	}
	seen := map[[2]string]bool{}
	for _, b := range n.Bindings {
		if b.Kind == "ownership" {
			continue
		}
		if b.Kind != "serial" && b.Kind != "default" || b.OnlyWhenNull {
			return bad("绑定只能是无条件直接列默认值")
		}
		if err := ValidateSQLServerName(b.TableName); err != nil {
			return SQLServerNativeSequence{}, err
		}
		if err := ValidateSQLServerName(b.ColumnName); err != nil {
			return SQLServerNativeSequence{}, err
		}
		key := [2]string{b.TableName, b.ColumnName}
		if seen[key] {
			return bad(fmt.Sprintf("重复的默认值绑定 %s.%s", b.TableName, b.ColumnName))
		}
		seen[key] = true
	}
	sourceName := n.SourceName
	if sourceName == "" {
		sourceName = n.Name
	}
	identity := strings.Join([]string{n.Schema, sourceName, spec.Increment.String(), spec.Min.String(), spec.Max.String(), strconv.FormatInt(spec.Cache, 10)}, "\x00")
	spec.Marker = fmt.Sprintf("%x", sha256.Sum256([]byte(identity)))
	return spec, nil
}

func (d *SQLServerDialect) SQLServerSequenceBindingStatement(schema, seq string, b source.SequenceBinding) Statement {
	owner := SQLServerObjectName(b.TableName, b.ColumnName, "dbgold_seq_default")
	return Statement{SQL: "ALTER TABLE " + d.QualifyTable(schema, b.TableName) + " ADD CONSTRAINT " + d.QuoteIdent(owner) +
		" DEFAULT (NEXT VALUE FOR " + d.QualifyTable(schema, seq) + ") FOR " + d.QuoteIdent(b.ColumnName)}
}

func (d *SQLServerDialect) sqlServerNativeSequenceStatements(schema string, s source.SequenceInfo) ([]Statement, error) {
	spec, err := ValidateSQLServerNativeSequence(s)
	if err != nil {
		return nil, err
	}
	if schema == "" {
		schema = "dbo"
	}
	if err := ValidateSQLServerName(schema); err != nil {
		return nil, err
	}
	n := s.Native
	name := d.QualifyTable(schema, n.Name)
	create := "CREATE SEQUENCE " + name + " AS DECIMAL(38,0) START WITH "
	cache := " NO CACHE"
	if spec.Cache > 0 {
		cache = " CACHE " + strconv.FormatInt(spec.Cache, 10)
	}
	options := " INCREMENT BY " + spec.Increment.String() + " MINVALUE " + spec.Min.String() + " MAXVALUE " + spec.Max.String() + " NO CYCLE" + cache
	var sql strings.Builder
	sql.WriteString("DECLARE @next decimal(38,0) = " + spec.Start.String() + ", @extreme decimal(38,0), @step decimal(38,0) = " + spec.Increment.String() + ", @ddl nvarchar(max); ")
	aggregate := "MAX"
	if spec.Increment.Sign() < 0 {
		aggregate = "MIN"
	}
	for _, b := range n.Bindings {
		if b.Kind == "ownership" {
			continue
		}
		sql.WriteString("SELECT @extreme=" + aggregate + "(CAST(" + d.QuoteIdent(b.ColumnName) + " AS decimal(38,0))) FROM " + d.QualifyTable(schema, b.TableName) + " WITH (TABLOCKX,HOLDLOCK); ")
		sql.WriteString(sqlServerAdvanceBeyond("@next", "@extreme", "@step"))
	}
	sql.WriteString("IF @next < " + spec.Min.String() + " OR @next > " + spec.Max.String() + " BEGIN RAISERROR (N'SEQUENCE cannot advance beyond imported rows',16,1); RETURN; END; ")
	sql.WriteString("SET @ddl=" + SQLServerLiteral(create) + "+CONVERT(nvarchar(40),@next)+" + SQLServerLiteral(options) + "; EXEC sys.sp_executesql @ddl")
	marker := "EXEC sys.sp_addextendedproperty @name=" + SQLServerLiteral(SQLServerSequenceMarker) + ", @value=" + SQLServerLiteral(spec.Marker) + ", @level0type=N'SCHEMA', @level0name=" + SQLServerLiteral(schema) + ", @level1type=N'SEQUENCE', @level1name=" + SQLServerLiteral(n.Name)
	statements := []Statement{{SQL: sql.String()}, {SQL: marker}}
	for _, b := range n.Bindings {
		if b.Kind != "ownership" {
			statements = append(statements, d.SQLServerSequenceBindingStatement(schema, n.Name, b))
		}
	}
	return statements, nil
}

func sqlServerAdvanceBeyond(next, extreme, step string) string {
	// The double modulo reduces both operands first so subtraction does not
	// have to represent the entire width of DECIMAL(38,0).
	return "IF " + extreme + " IS NOT NULL BEGIN " +
		"IF " + step + ">0 AND " + extreme + ">=" + next + " SET " + next + "=" + extreme + "+(" + step + "-((" + extreme + "%" + step + "-" + next + "%" + step + "+" + step + ")%" + step + ")); " +
		"IF " + step + "<0 AND " + extreme + "<=" + next + " SET " + next + "=" + extreme + "+(" + step + "+((" + next + "%ABS(" + step + ")-" + extreme + "%ABS(" + step + ")+ABS(" + step + "))%ABS(" + step + "))); END; "
}

func (d *SQLServerDialect) sqlServerIdentityStatements(schema string, s source.SequenceInfo) []Statement {
	c, ok := d.Column(s.TableName, s.ColumnName)
	if !ok || !SQLServerHasIdentity(c, d.source) {
		return nil
	}
	_, step, err := sqlServerIdentityParameters(c, d.source)
	if err != nil {
		return nil
	}
	mapped, err := d.ColumnType(c)
	if err != nil {
		return nil
	}
	return d.SQLServerIdentityCalibrationStatements(schema, s, mapped, step)
}

// SQLServerIdentityCalibrationStatements can also act on a catalog-verified
// identity when a dedicated object repair has no in-memory source table cache.
func (d *SQLServerDialect) SQLServerIdentityCalibrationStatements(schema string, s source.SequenceInfo, mapped, step string) []Statement {
	start := s.StartValueText
	if start == "" {
		start = strconv.FormatInt(s.StartValue, 10)
	}
	n, ok := new(big.Int).SetString(start, 10)
	if !ok || len(strings.TrimPrefix(n.String(), "-")) > 38 {
		return nil
	}
	increment, ok := new(big.Int).SetString(step, 10)
	if !ok || increment.Sign() == 0 || len(strings.TrimPrefix(increment.String(), "-")) > 38 {
		return nil
	}
	step = increment.String()
	minValue, maxValue := SQLServerIdentityBounds(mapped)
	if minValue == "" {
		return nil
	}
	q := d.QualifyTable(schema, s.TableName)
	qualified := SQLServerLiteral(q)
	aggregate := "MAX"
	if strings.HasPrefix(step, "-") {
		aggregate = "MIN"
	}
	sql := "DECLARE @source decimal(38,0)=" + n.String() + ", @step decimal(38,0)=" + step + ", @next decimal(38,0), @original decimal(38,0), @seed decimal(38,0), @last decimal(38,0), @extreme decimal(38,0), @increment decimal(38,0); " +
		"SELECT @extreme=" + aggregate + "(CAST(" + d.QuoteIdent(s.ColumnName) + " AS decimal(38,0))) FROM " + q + " WITH (TABLOCKX,HOLDLOCK); " +
		"SELECT @seed=CAST(seed_value AS decimal(38,0)),@last=CAST(last_value AS decimal(38,0)),@increment=CAST(increment_value AS decimal(38,0)) FROM sys.identity_columns WHERE object_id=OBJECT_ID(" + qualified + ",N'U') AND name=" + SQLServerLiteral(s.ColumnName) + "; " +
		"IF @seed IS NULL OR @increment<>@step BEGIN RAISERROR (N'IDENTITY column or increment does not match',16,1); RETURN; END; " +
		"SET @original=CASE WHEN @last IS NULL THEN @seed ELSE @last+@step END; SET @next=@original; " +
		"IF (@step>0 AND @source>@next) OR (@step<0 AND @source<@next) SET @next=@source; " +
		sqlServerAdvanceBeyond("@next", "@extreme", "@step") +
		"IF @next<" + minValue + " OR @next>" + maxValue + " BEGIN RAISERROR (N'IDENTITY next value exceeds target type range',16,1); RETURN; END; " +
		"IF @next<>@original BEGIN SET @seed=CASE WHEN @extreme IS NULL AND @last IS NULL THEN @next ELSE @next-@step END; DBCC CHECKIDENT (" + qualified + ", RESEED, @seed) WITH NO_INFOMSGS; END"
	return []Statement{{SQL: sql}}
}

func SQLServerIdentityBounds(mapped string) (string, string) {
	switch mapped {
	case "TINYINT":
		return "0", "255"
	case "SMALLINT":
		return "-32768", "32767"
	case "INT":
		return "-2147483648", "2147483647"
	case "BIGINT":
		return "-9223372036854775808", "9223372036854775807"
	default:
		p, ok := strings.CutPrefix(mapped, "DECIMAL(")
		if !ok || !strings.HasSuffix(p, ",0)") {
			return "", ""
		}
		precision, err := strconv.Atoi(strings.TrimSuffix(p, ",0)"))
		if err != nil || precision < 1 || precision > 38 {
			return "", ""
		}
		maxValue := strings.Repeat("9", precision)
		return "-" + maxValue, maxValue
	}
}
