package partition

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/typemap"
	"github.com/shopspring/decimal"
)

// IntervalMaterializationSQL is intentionally incomplete ALTER syntax. Only
// an executor holding the matching immutable plan may interpret this marker.
func IntervalMaterializationSQL(p *Plan) string {
	if p == nil || p.Interval == nil {
		return ""
	}
	b, _ := json.Marshal(struct {
		Target *source.PartitionSpec
		Recipe *IntervalMaterialization
	}{p.Target, p.Interval})
	h := sha256.Sum256(b)
	quote := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	return "ALTER TABLE " + quote(p.Interval.Schema) + "." + quote(p.Interval.Table) + " /* DBGOLD_INTERVAL_MATERIALIZE " + hex.EncodeToString(h[:]) + " */"
}

// IntervalMaterialization is an immutable recipe for empty, already-created
// tables. Values are typed literals; source defaults/functions are never run.
type IntervalMaterialization struct {
	Schema   string                  `json:"schema"`
	Table    string                  `json:"table"`
	Columns  []string                `json:"columns"`
	Values   []source.PartitionValue `json:"values"`
	KeyIndex int                     `json:"key_index"`
	Seeds    []IntervalSeed          `json:"seeds"`
}
type IntervalSeed struct {
	PartitionName string                `json:"partition_name"`
	Value         source.PartitionValue `json:"value"`
}

func (c *compiler) prepareInterval(dst, src *source.PartitionSpec, depth int) error {
	if src.Automatic == "" && src.Interval == nil {
		return nil
	}
	if !c.profile.Features[FeatureInterval] || c.opt.TargetType != "dameng" || (c.opt.SourceType != "oracle" && c.opt.SourceType != "dameng") {
		return fmt.Errorf("%s → %s 的 INTERVAL 原生自动扩展尚未实现/验证；不能降级为静态 RANGE", c.opt.SourceType, c.opt.TargetType)
	}
	if depth != 0 || baseMethod(src.Method) != "RANGE" || len(src.Keys) != 1 || src.Keys[0].Column == "" || src.Interval == nil {
		return fmt.Errorf("INTERVAL 缺少完整的单列原生规则/初始基点")
	}
	rule, err := source.ParsePartitionInterval(src.Automatic)
	if err != nil {
		return err
	}
	if rule.Unit != src.Interval.Unit || rule.Amount != src.Interval.Amount {
		return fmt.Errorf("INTERVAL 原始表达式与冻结常量不一致")
	}
	initial := 0
	seenAuto := false
	for _, p := range src.Partitions {
		if p.Subpartition != nil || p.Default || p.UpperInclusive || !p.LowerInclusive || len(p.Upper) != 1 {
			return fmt.Errorf("INTERVAL 复合模板、默认分区或非标准边界尚未原生适配")
		}
		if !p.IsInterval {
			if seenAuto {
				return fmt.Errorf("INTERVAL 初始分区排序不一致")
			}
			initial++
			continue
		}
		seenAuto = true
		if _, err = source.IntervalBoundaryIndex(src.Interval, p.Upper[0]); err != nil {
			return err
		}
		lower, e := source.IntervalPreviousBound(src.Interval, p.Upper[0])
		if e != nil {
			return e
		}
		if !sameValues(p.Lower, []source.PartitionValue{lower}) {
			return fmt.Errorf("INTERVAL 自动分区下界与原生规则不一致")
		}
	}
	if initial == 0 || !sameValues(src.Partitions[initial-1].Upper, []source.PartitionValue{src.Interval.Transition}) {
		return fmt.Errorf("INTERVAL 初始 RANGE 转换点缺失或不一致")
	}
	if src.Interval.Unit == "month" {
		v := src.Interval.Transition
		layout := "2006-01-02 15:04:05.999999999"
		if v.Kind == "date" {
			layout = "2006-01-02"
		}
		at, e := time.Parse(layout, v.Text)
		if e != nil || at.Day() > 28 {
			return fmt.Errorf("INTERVAL 月末 29/30/31 日的跨产品未来日历行为未证明等价")
		}
	}
	k, ok := c.column(src.Keys[0].Column)
	if !ok {
		return fmt.Errorf("INTERVAL 分区键列缺失")
	}
	if c.opt.SourceType == "oracle" && c.opt.TargetType == "dameng" && k.IsNullable {
		return fmt.Errorf("Oracle INTERVAL 拒绝 NULL 分区键，而达梦原生 INTERVAL 接受 NULL；可空键不能原生等价迁移")
	}
	t := strings.ToUpper(strings.TrimSpace(k.DataType))
	if at := strings.Index(t, "("); at >= 0 {
		if end := strings.Index(t[at:], ")"); end >= 0 {
			t = strings.TrimSpace(t[:at] + t[at+end+1:])
		} else {
			return fmt.Errorf("INTERVAL 分区键类型精度声明不完整")
		}
	}
	if src.Interval.Unit == "number" {
		if c.opt.SourceType == "oracle" && c.opt.TargetType == "dameng" {
			return fmt.Errorf("Oracle 数值 INTERVAL 可接受列域最大值并生成超出列精度的上界，而达梦同类型会溢出；扩大精度并加 CHECK/DOMAIN 后，拒绝越界值仍可能生成额外自动空叶，不能保留原生语义，尚无已验证的等价迁移方案")
		}
		switch t {
		case "NUMBER", "NUMERIC", "DECIMAL", "DEC", "INT", "INTEGER", "BIGINT", "SMALLINT", "TINYINT":
		default:
			return fmt.Errorf("INTERVAL 数值键 %s 不是已验证精确数值类型", t)
		}
	} else if t != "DATE" && t != "TIMESTAMP" && t != "DATETIME" {
		return fmt.Errorf("INTERVAL 时间键 %s 的精度/时区语义尚未证明", t)
	}
	if src.Interval.Unit == "second" && c.opt.SourceType == "oracle" {
		precision := int64(0)
		if t == "TIMESTAMP" {
			precision = typemap.OracleDamengTimestampPrecision(k)
		}
		step, e := decimal.NewFromString(src.Interval.Amount)
		if e != nil {
			return e
		}
		if precision < 0 || precision > 6 || !step.Equal(step.Truncate(0)) {
			return fmt.Errorf("Oracle INTERVAL 仅证明整秒步长与不超过6位的时间键精度；小数秒自动边界证据不足")
		}
	}
	if c.opt.SourceType == "oracle" && src.Interval.Unit != "number" {
		if t == "TIMESTAMP" {
			precision := typemap.OracleDamengTimestampPrecision(k)
			if precision < 0 || precision > 6 {
				return fmt.Errorf("Oracle INTERVAL 时间键精度不能超过目标已验证的6位")
			}
		}
		layout := "2006-01-02 15:04:05.999999999"
		if src.Interval.Transition.Kind == "date" {
			layout = "2006-01-02"
		}
		anchor, e := time.Parse(layout, src.Interval.Transition.Text)
		if e != nil {
			return e
		}
		if anchor.Nanosecond() != 0 {
			return fmt.Errorf("Oracle INTERVAL 自动叶不能完整保留带小数秒的原始转换点；仅放行整秒转换点")
		}
	}
	if c.opt.SourceType == "oracle" && c.opt.TargetType == "dameng" {
		if err := oracleDamengIntervalCapacity(src, k, initial); err != nil {
			return err
		}
	}
	dst.Automatic, err = IntervalSQL(src.Interval)
	if err == nil {
		// Oracle positions can denote interval slots, while DM positions denote
		// physical materialization order. Preserve identity/bounds, and freeze
		// the target's actual consecutive order without changing the source.
		for i := range dst.Partitions {
			dst.Partitions[i].Ordinal = i + 1
		}
		c.rules = append(c.rules, "native-interval-preserve-transition-and-sparse-leaves")
	}
	return err
}

// DM's standalone native partition limit is 65535; Oracle permits 1048575.
// Prove the whole declared key domain fits, not merely today's populated leaves.
// https://eco.dameng.com/document/dm/zh-cn/pm/manage-partition.html (15.6)
// https://docs.oracle.com/en/error-help/db/ora-14300/
func oracleDamengIntervalCapacity(spec *source.PartitionSpec, key source.ColumnInfo, initial int) error {
	rule := spec.Interval
	step, err := decimal.NewFromString(rule.Amount)
	if err != nil {
		return err
	}
	var delta decimal.Decimal
	if rule.Unit == "number" {
		maximum := decimal.New(1, int32(key.Precision-key.Scale)).Sub(decimal.New(1, -int32(key.Scale)))
		anchor, e := decimal.NewFromString(rule.Transition.Text)
		if e != nil {
			return e
		}
		delta = maximum.Sub(anchor)
		for _, p := range spec.Partitions {
			if p.IsInterval {
				seed, e := decimal.NewFromString(p.Lower[0].Text)
				if e != nil {
					return e
				}
				if seed.Abs().GreaterThan(maximum) || !seed.Equal(seed.Truncate(int32(key.Scale))) {
					return fmt.Errorf("现有 INTERVAL 空叶 %s 没有已证明可按目标列域物化的下界常量", p.Name)
				}
			}
		}
	} else {
		layout := "2006-01-02 15:04:05.999999999"
		if rule.Transition.Kind == "date" {
			layout = "2006-01-02"
		}
		anchor, e := time.Parse(layout, rule.Transition.Text)
		if e != nil {
			return e
		}
		if rule.Unit == "month" {
			delta = decimal.NewFromInt(int64((9999-anchor.Year())*12 + 12 - int(anchor.Month())))
		} else {
			precision := int64(0)
			if typemap.OracleDamengBaseType(key.DataType) == "TIMESTAMP" {
				precision = typemap.OracleDamengTimestampPrecision(key)
			}
			fraction := decimal.Zero
			if precision > 0 {
				fraction = decimal.NewFromInt(1).Sub(decimal.New(1, -int32(precision)))
			}
			maximum := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
			delta = decimal.NewFromInt(maximum.Unix() - anchor.Unix()).Add(fraction).Sub(decimal.New(int64(anchor.Nanosecond()), -9))
		}
	}
	slots := decimal.Zero
	if !delta.IsNegative() {
		whole, _ := delta.QuoRem(step, 0)
		slots = whole.Add(decimal.NewFromInt(1))
	}
	total := slots.Add(decimal.NewFromInt(int64(initial)))
	if total.GreaterThan(decimal.NewFromInt(65535)) {
		return fmt.Errorf("INTERVAL 完整键域最多需要 %s 个初始/自动分区，超过达梦普通实例65535上限；不能把仅当前叶或近未来通过当作全域等价", total.String())
	}
	return nil
}

// IntervalSQL renders only a canonical native numeric/constructor expression.
func IntervalSQL(rule *source.PartitionInterval) (string, error) {
	if rule == nil {
		return "", fmt.Errorf("缺少 INTERVAL 规则")
	}
	var raw string
	switch rule.Unit {
	case "number":
		raw = rule.Amount
	case "month":
		raw = "NUMTOYMINTERVAL(" + rule.Amount + ",'MONTH')"
	case "second":
		raw = "NUMTODSINTERVAL(" + rule.Amount + ",'SECOND')"
	default:
		return "", fmt.Errorf("未知 INTERVAL 单位")
	}
	parsed, e := source.ParsePartitionInterval(raw)
	if e != nil || parsed.Unit != rule.Unit || parsed.Amount != rule.Amount {
		return "", fmt.Errorf("INTERVAL 常量不是规范的正数")
	}
	return raw, nil
}

func (c *compiler) inlineInterval(s *source.PartitionSpec, depth int) (string, error) {
	if depth != 0 || s.Interval == nil {
		return "", fmt.Errorf("INTERVAL 只支持冻结的根规则")
	}
	expr, e := IntervalSQL(s.Interval)
	if e != nil {
		return "", e
	}
	var defs []string
	for _, p := range s.Partitions {
		if p.IsInterval {
			continue
		}
		values, e := c.tuple(p.Upper)
		if e != nil {
			return "", e
		}
		defs = append(defs, "PARTITION "+c.quote(p.Name)+" VALUES LESS THAN ("+values+")")
	}
	if len(defs) == 0 {
		return "", fmt.Errorf("INTERVAL 没有初始 RANGE")
	}
	clause := "PARTITION BY RANGE (" + c.keys(s) + ") INTERVAL (" + expr + ") (\n  " + strings.Join(defs, ",\n  ") + "\n)"
	if s.RowMovement != "" {
		if s.RowMovement == "enabled" {
			clause += " ENABLE ROW MOVEMENT"
		} else {
			clause += " DISABLE ROW MOVEMENT"
		}
	}
	return clause, nil
}

func (c *compiler) freezeIntervalMaterialization(s *source.PartitionSpec) (*IntervalMaterialization, error) {
	if s == nil || s.Interval == nil {
		return nil, nil
	}
	r := &IntervalMaterialization{Schema: c.opt.TargetSchema, Table: c.opt.Name(c.info.TableName), KeyIndex: -1}
	for _, p := range s.Partitions {
		if p.IsInterval {
			if len(p.Lower) != 1 {
				return nil, fmt.Errorf("INTERVAL 自动叶下界缺失")
			}
			r.Seeds = append(r.Seeds, IntervalSeed{PartitionName: p.Name, Value: p.Lower[0]})
		}
	}
	if len(r.Seeds) == 0 {
		return nil, nil
	}
	for _, col := range c.info.Columns {
		if col.Generated != "" || col.Identity != "" || strings.Contains(strings.ToLower(col.Extra), "generated") || strings.Contains(strings.ToLower(col.Extra), "auto_increment") {
			return nil, fmt.Errorf("INTERVAL 空叶物化尚不能安全处理生成/自增列 %s", col.Name)
		}
		r.Columns = append(r.Columns, c.opt.ColumnName(col.Name))
		if col.Name == c.info.Partition.Keys[0].Column {
			r.KeyIndex = len(r.Values)
			r.Values = append(r.Values, r.Seeds[0].Value)
			continue
		}
		v := source.PartitionValue{Kind: "null"}
		if !col.IsNullable {
			t := strings.ToUpper(strings.TrimSpace(col.DataType))
			if i := strings.Index(t, "("); i >= 0 {
				if end := strings.Index(t[i:], ")"); end >= 0 {
					t = strings.TrimSpace(t[:i] + t[i+end+1:])
				} else {
					return nil, fmt.Errorf("INTERVAL 列类型精度声明不完整")
				}
			}
			switch t {
			case "NUMBER", "NUMERIC", "DECIMAL", "DEC", "INT", "INTEGER", "BIGINT", "SMALLINT", "TINYINT", "FLOAT", "REAL", "DOUBLE", "DOUBLE PRECISION", "BINARY_FLOAT", "BINARY_DOUBLE":
				v = source.PartitionValue{Kind: "number", Text: "0"}
			case "CHAR", "NCHAR", "VARCHAR", "VARCHAR2", "NVARCHAR", "NVARCHAR2", "TEXT", "CLOB", "NCLOB":
				v = source.PartitionValue{Kind: "string", Text: "x"}
			case "DATE", "TIMESTAMP", "DATETIME":
				v = source.PartitionValue{Kind: "timestamp", Text: "2000-01-01 00:00:00"}
			default:
				return nil, fmt.Errorf("INTERVAL 空叶物化没有列 %s 类型 %s 的无副作用非空常量", col.Name, col.DataType)
			}
		}
		r.Values = append(r.Values, v)
	}
	if r.KeyIndex < 0 {
		return nil, fmt.Errorf("INTERVAL 物化键未映射到目标列")
	}
	return r, nil
}

// IntervalLiteral is shared by the typed executor. It deliberately excludes
// raw SQL, identifiers, defaults, routines, and arbitrary cast expressions.
func IntervalLiteral(v source.PartitionValue) (string, error) {
	v, e := canonicalValue(v)
	if e != nil {
		return "", e
	}
	switch v.Kind {
	case "number":
		return v.Text, nil
	case "null":
		return "NULL", nil
	case "string":
		return "'" + strings.ReplaceAll(v.Text, "'", "''") + "'", nil
	case "date":
		return "DATE '" + v.Text + "'", nil
	case "timestamp":
		return "TIMESTAMP '" + v.Text + "'", nil
	}
	return "", fmt.Errorf("INTERVAL 物化值不能含无穷边界")
}
