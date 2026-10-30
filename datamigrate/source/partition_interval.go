package source

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"dbgold/datamigrate/sqlsyntax"
	"github.com/shopspring/decimal"
)

// ParsePartitionInterval accepts only positive numeric constants and built-in
// interval constructors. It never executes an expression on either database.
func ParsePartitionInterval(raw string) (*PartitionInterval, error) {
	raw = strings.TrimSpace(raw)
	ts, err := sqlsyntax.Lex(strings.TrimSpace(raw))
	if err != nil || len(ts) == 0 {
		return nil, fmt.Errorf("INTERVAL 不是可证明的原生常量")
	}
	unit, number := "number", strings.TrimSpace(raw)
	if len(ts) >= 6 && (ts[0].Is("NUMTOYMINTERVAL") || ts[0].Is("NUMTODSINTERVAL")) && ts[1].Is("(") && ts[len(ts)-1].Is(")") {
		args, e := catalogSplit(strings.TrimSpace(raw)[strings.Index(raw, "(")+1 : strings.LastIndex(raw, ")")])
		if e != nil || len(args) != 2 {
			return nil, fmt.Errorf("INTERVAL 构造函数必须含两个固定常量")
		}
		u, e := sqlsyntax.Lex(args[1])
		if e != nil || len(u) != 1 || u[0].Kind != 's' {
			return nil, fmt.Errorf("INTERVAL 单位不是固定字符串")
		}
		number = args[0]
		unit = strings.ToLower(u[0].Value)
		if ts[0].Is("NUMTOYMINTERVAL") && unit != "year" && unit != "month" || ts[0].Is("NUMTODSINTERVAL") && unit != "day" && unit != "hour" && unit != "minute" && unit != "second" {
			return nil, fmt.Errorf("INTERVAL 构造函数单位不匹配")
		}
	}
	if !catalogPartitionNumber.MatchString(number) {
		return nil, fmt.Errorf("INTERVAL 含非原生常量表达式，不能执行源函数")
	}
	n, err := decimal.NewFromString(number)
	if err != nil || !n.IsPositive() {
		return nil, fmt.Errorf("INTERVAL 必须为正精确常量")
	}
	switch unit {
	case "year":
		n = n.Mul(decimal.NewFromInt(12))
		unit = "month"
	case "day":
		n = n.Mul(decimal.NewFromInt(86400))
		unit = "second"
	case "hour":
		n = n.Mul(decimal.NewFromInt(3600))
		unit = "second"
	case "minute":
		n = n.Mul(decimal.NewFromInt(60))
		unit = "second"
	}
	if unit == "month" && (!n.Equal(n.Truncate(0)) || n.GreaterThan(decimal.NewFromInt(120000))) {
		return nil, fmt.Errorf("INTERVAL 月份不是可精确表示的整数")
	}
	if unit == "second" && (!n.Equal(n.Truncate(6)) || n.GreaterThan(decimal.NewFromInt(315537897599))) {
		return nil, fmt.Errorf("INTERVAL 秒数超过已验证微秒精度或日期范围")
	}
	return &PartitionInterval{Unit: unit, Amount: n.String()}, nil
}

func intervalTime(v PartitionValue) (time.Time, error) {
	if v.Kind == "date" {
		return time.Parse("2006-01-02", v.Text)
	}
	if v.Kind == "timestamp" {
		return time.Parse("2006-01-02 15:04:05.999999999", v.Text)
	}
	return time.Time{}, fmt.Errorf("INTERVAL 日期边界类型不可证明: %s", v.Kind)
}

// IntervalPreviousBound calculates the actual lower bound of one automatic
// partition. The preceding *existing* partition is deliberately irrelevant.
func IntervalPreviousBound(rule *PartitionInterval, upper PartitionValue) (PartitionValue, error) {
	if rule == nil {
		return PartitionValue{}, fmt.Errorf("缺少 INTERVAL 原生规则")
	}
	n, e := decimal.NewFromString(rule.Amount)
	if e != nil || !n.IsPositive() {
		return PartitionValue{}, fmt.Errorf("INTERVAL 步长无效")
	}
	if rule.Unit == "number" {
		if upper.Kind != "number" {
			return PartitionValue{}, fmt.Errorf("数值 INTERVAL 的边界不是数值")
		}
		u, e := decimal.NewFromString(upper.Text)
		if e != nil {
			return PartitionValue{}, e
		}
		return PartitionValue{Kind: "number", Text: u.Sub(n).String()}, nil
	}
	u, e := intervalTime(upper)
	if e != nil {
		return PartitionValue{}, e
	}
	var lower time.Time
	switch rule.Unit {
	case "month":
		if !n.Equal(n.Truncate(0)) || n.GreaterThan(decimal.NewFromInt(120000)) {
			return PartitionValue{}, fmt.Errorf("INTERVAL 月数无效")
		}
		lower = u.AddDate(0, -int(n.IntPart()), 0)
		if lower.Day() != u.Day() {
			return PartitionValue{}, fmt.Errorf("INTERVAL 月末日历语义无法精确还原")
		}
	case "second":
		seconds := n.IntPart()
		nanos := n.Sub(decimal.NewFromInt(seconds)).Mul(decimal.NewFromInt(1e9))
		if !nanos.Equal(nanos.Truncate(0)) {
			return PartitionValue{}, fmt.Errorf("INTERVAL 秒精度不可表示")
		}
		lower = time.Unix(u.Unix()-seconds, int64(u.Nanosecond())-nanos.IntPart()).UTC()
	default:
		return PartitionValue{}, fmt.Errorf("未知 INTERVAL 单位 %s", rule.Unit)
	}
	if lower.Year() < 1 || lower.Year() > 9999 {
		return PartitionValue{}, fmt.Errorf("INTERVAL 下界超出日期范围")
	}
	if upper.Kind == "date" {
		if lower.Hour() != 0 || lower.Minute() != 0 || lower.Second() != 0 || lower.Nanosecond() != 0 {
			return PartitionValue{}, fmt.Errorf("DATE INTERVAL 含非整日边界")
		}
		return PartitionValue{Kind: "date", Text: lower.Format("2006-01-02")}, nil
	}
	return PartitionValue{Kind: "timestamp", Text: lower.Format("2006-01-02 15:04:05.999999999")}, nil
}

// IntervalBoundaryIndex proves a generated upper bound belongs to the frozen
// transition grid. A positive index denotes an automatic interval slot.
func IntervalBoundaryIndex(rule *PartitionInterval, upper PartitionValue) (string, error) {
	if rule == nil {
		return "", fmt.Errorf("缺少 INTERVAL 规则")
	}
	n, e := decimal.NewFromString(rule.Amount)
	if e != nil || !n.IsPositive() {
		return "", fmt.Errorf("INTERVAL 步长无效")
	}
	var delta decimal.Decimal
	if rule.Unit == "number" {
		if rule.Transition.Kind != "number" || upper.Kind != "number" {
			return "", fmt.Errorf("INTERVAL 数值边界类型不同")
		}
		a, e := decimal.NewFromString(rule.Transition.Text)
		if e != nil {
			return "", e
		}
		b, e := decimal.NewFromString(upper.Text)
		if e != nil {
			return "", e
		}
		delta = b.Sub(a)
	} else {
		a, e := intervalTime(rule.Transition)
		if e != nil {
			return "", e
		}
		b, e := intervalTime(upper)
		if e != nil {
			return "", e
		}
		if rule.Unit == "month" {
			months := (b.Year()-a.Year())*12 + int(b.Month()-a.Month())
			if !a.AddDate(0, months, 0).Equal(b) || a.Day() != b.Day() {
				return "", fmt.Errorf("INTERVAL 月份边界偏离原生基点")
			}
			delta = decimal.NewFromInt(int64(months))
		} else if rule.Unit == "second" {
			delta = decimal.NewFromInt(b.Unix() - a.Unix()).Add(decimal.New(int64(b.Nanosecond()-a.Nanosecond()), -9))
		} else {
			return "", fmt.Errorf("INTERVAL 单位无效")
		}
	}
	if !delta.IsPositive() || !delta.Mod(n).IsZero() {
		return "", fmt.Errorf("自动分区上界偏离 INTERVAL 原生网格")
	}
	return delta.Div(n).String(), nil
}

func catalogOracleInterval(ctx context.Context, db *sql.DB, owner, table string, spec *PartitionSpec) error {
	rows, err := db.QueryContext(ctx, `SELECT PARTITION_NAME,INTERVAL FROM ALL_TAB_PARTITIONS WHERE TABLE_OWNER=:1 AND TABLE_NAME=:2 ORDER BY PARTITION_POSITION`, owner, table)
	if err != nil {
		return fmt.Errorf("读取 Oracle 初始/自动分区标志: %w", err)
	}
	defer rows.Close()
	initial := map[string]bool{}
	seen := map[string]bool{}
	for rows.Next() {
		var name, flag string
		if err = rows.Scan(&name, &flag); err != nil {
			return err
		}
		if seen[name] || name == "" {
			return fmt.Errorf("Oracle INTERVAL 目录名称重复或缺失")
		}
		seen[name] = true
		switch strings.TrimSpace(flag) {
		case "NO":
			initial[name] = true
		case "YES":
		default:
			return fmt.Errorf("Oracle INTERVAL 标志不可识别: %q", flag)
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if len(seen) != len(spec.Partitions) {
		return fmt.Errorf("Oracle INTERVAL 目录读取期间发生变化")
	}
	for _, p := range spec.Partitions {
		if !seen[p.Name] {
			return fmt.Errorf("Oracle INTERVAL 缺少冻结分区标志")
		}
	}
	if err = catalogFreezeInterval(spec, initial); err != nil {
		catalogPartitionUnsupported(spec, err.Error())
	}
	return nil
}

func catalogDamengInterval(spec *PartitionSpec, ddl string) error {
	ts, err := sqlsyntax.Lex(ddl)
	if err != nil {
		return err
	}
	initial := map[string]bool{}
	for i := 0; i+1 < len(ts); i++ {
		if !ts[i].Is("INTERVAL") || !ts[i+1].Is("(") {
			continue
		}
		end := catalogMatchingParen(ts, i+1)
		if end < 0 || end+1 >= len(ts) || !ts[end+1].Is("(") {
			continue
		}
		last := catalogMatchingParen(ts, end+1)
		if last < 0 {
			return fmt.Errorf("达梦 INTERVAL 初始分区定义不完整")
		}
		for j := end + 2; j+1 < last; j++ {
			if ts[j].Is("SUBPARTITION") {
				return fmt.Errorf("INTERVAL 子分区未来模板未完整读取")
			}
			if ts[j].Is("PARTITION") {
				name := ts[j+1].Value
				if ts[j+1].Kind == 'i' {
					name = strings.ToUpper(name)
				}
				if initial[name] {
					return fmt.Errorf("INTERVAL 初始分区名称重复")
				}
				initial[name] = true
			}
		}
		break
	}
	return catalogFreezeInterval(spec, initial)
}

func catalogFreezeInterval(spec *PartitionSpec, initial map[string]bool) error {
	if spec.Method != "RANGE" || len(spec.Keys) != 1 || len(initial) == 0 {
		return fmt.Errorf("INTERVAL 缺少单列 RANGE 或初始范围基点")
	}
	rule, err := ParsePartitionInterval(spec.Automatic)
	if err != nil {
		return err
	}
	automatic := false
	found := 0
	for i := range spec.Partitions {
		p := &spec.Partitions[i]
		if p.Subpartition != nil {
			return fmt.Errorf("INTERVAL 子分区未来模板尚未完整读取")
		}
		if len(p.Upper) != 1 {
			return fmt.Errorf("INTERVAL 上界不完整")
		}
		if initial[p.Name] {
			if automatic {
				return fmt.Errorf("INTERVAL 初始范围与自动叶顺序冲突")
			}
			rule.Transition = p.Upper[0]
			found++
		} else {
			automatic = true
			p.IsInterval = true
		}
	}
	if found != len(initial) {
		return fmt.Errorf("INTERVAL 原生定义与目录初始分区不一致")
	}
	spec.Interval = rule
	seen := map[string]bool{}
	for i := range spec.Partitions {
		p := &spec.Partitions[i]
		if !p.IsInterval {
			continue
		}
		index, e := IntervalBoundaryIndex(rule, p.Upper[0])
		if e != nil {
			return e
		}
		if seen[index] {
			return fmt.Errorf("INTERVAL 自动分区范围重复")
		}
		seen[index] = true
		lower, e := IntervalPreviousBound(rule, p.Upper[0])
		if e != nil {
			return e
		}
		p.Lower = []PartitionValue{lower}
	}
	return nil
}
