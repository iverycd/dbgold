package partition

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/sqlsyntax"
	"github.com/shopspring/decimal"
)

// CanonicalKey keeps quoted identifier case and literal bytes, while ignoring
// whitespace, comments, and the spelling case of SQL keywords/functions.
func CanonicalKey(key source.PartitionKey) string {
	if key.Column != "" {
		return "column:" + key.Column
	}
	if normalized, ok := canonicalRoutingExpression(key.Expression); ok {
		return normalized
	}
	ts, err := sqlsyntax.Lex(strings.TrimSpace(key.Expression))
	if err != nil {
		return "invalid:" + key.Expression
	}
	for len(ts) >= 2 && ts[0].Text == "(" && ts[len(ts)-1].Text == ")" {
		depth, wraps := 0, true
		for i, t := range ts {
			if t.Text == "(" {
				depth++
			}
			if t.Text == ")" {
				depth--
			}
			if depth == 0 && i != len(ts)-1 {
				wraps = false
				break
			}
		}
		if !wraps {
			break
		}
		ts = ts[1 : len(ts)-1]
	}
	var values []string
	for _, t := range ts {
		switch t.Kind {
		case 'i':
			values = append(values, "i:"+strings.ToLower(t.Value))
		case 'q':
			values = append(values, "q:"+t.Value)
		case 's':
			values = append(values, "s:"+t.Value)
		default:
			values = append(values, string(t.Kind)+":"+t.Text)
		}
	}
	return strings.Join(values, "\x00")
}

func canonicalValue(v source.PartitionValue) (source.PartitionValue, error) {
	v.Kind = strings.ToLower(strings.TrimSpace(v.Kind))
	switch v.Kind {
	case "number":
		n, err := decimal.NewFromString(v.Text)
		if err != nil {
			return v, fmt.Errorf("分区边界不是精确数字: %q", v.Text)
		}
		v.Text = n.String()
	case "date":
		t, err := time.Parse("2006-01-02", v.Text)
		if err != nil {
			return v, fmt.Errorf("分区日期边界无效: %q", v.Text)
		}
		v.Text = t.Format("2006-01-02")
	case "timestamp":
		var parsed time.Time
		var err error
		for _, layout := range []string{"2006-01-02 15:04:05.999999999", "2006-01-02T15:04:05.999999999"} {
			parsed, err = time.Parse(layout, v.Text)
			if err == nil {
				break
			}
		}
		if err != nil {
			return v, fmt.Errorf("分区时间边界无效或带未验证时区: %q", v.Text)
		}
		v.Text = parsed.Format("2006-01-02 15:04:05.999999999")
	case "string":
	case "null", "minvalue", "maxvalue":
		v.Text = ""
	default:
		return v, fmt.Errorf("分区边界类型 %q 尚未实现/验证", v.Kind)
	}
	return v, nil
}

// CanonicalSpec removes extraction-only evidence, not logical topology. Column
// types are validated by the migration's normal column compatibility checks;
// partition equivalence compares keys, collations, bounds and leaf identities.
func CanonicalSpec(spec *source.PartitionSpec) (*source.PartitionSpec, error) {
	if spec != nil && spec.Unsupported != "" {
		return nil, fmt.Errorf("分区目录包含未解析的语义，不能核验通过: %s", spec.Unsupported)
	}
	out := source.ClonePartitionSpec(spec)
	if out == nil {
		return nil, nil
	}
	out.Method = strings.ToUpper(strings.Join(strings.Fields(out.Method), " "))
	out.SourceDDL, out.Unsupported = "", ""
	if out.Interval != nil {
		var err error
		out.Interval.Transition, err = canonicalValue(out.Interval.Transition)
		if err != nil {
			return nil, err
		}
		out.Automatic, err = IntervalSQL(out.Interval)
		if err != nil {
			return nil, err
		}
	}
	if !strings.Contains(out.Method, "HASH") && !strings.Contains(out.Method, "KEY") {
		out.Algorithm = ""
	}
	for i := range out.Keys {
		if baseMethod(out.Method) == "KEY" {
			var err error
			out.Keys[i].DataType, err = canonicalMySQLKeyType(out.Keys[i].DataType)
			if err != nil {
				return nil, err
			}
		} else {
			out.Keys[i].DataType = ""
		}
		if out.Keys[i].Column != "" {
			out.Keys[i].Expression = ""
		} else {
			out.Keys[i].Expression = CanonicalKey(out.Keys[i])
		}
	}
	for i := range out.Partitions {
		p := &out.Partitions[i]
		if baseMethod(out.Method) != "RANGE" {
			p.LowerInclusive, p.UpperInclusive = false, false
		}
		for _, values := range [][]source.PartitionValue{p.Lower, p.Upper} {
			for j := range values {
				v, err := canonicalValue(values[j])
				if err != nil {
					return nil, err
				}
				values[j] = v
			}
		}
		for j := range p.Values {
			for k := range p.Values[j] {
				v, err := canonicalValue(p.Values[j][k])
				if err != nil {
					return nil, err
				}
				p.Values[j][k] = v
			}
		}
		sort.Slice(p.Values, func(a, b int) bool {
			x, _ := json.Marshal(p.Values[a])
			y, _ := json.Marshal(p.Values[b])
			return string(x) < string(y)
		})
		var err error
		p.Subpartition, err = CanonicalSpec(p.Subpartition)
		if err != nil {
			return nil, err
		}
		if p.Subpartition != nil {
			p.Subpartition.RowMovement = ""
		}
	}
	if out.Interval != nil {
		// Named automatic leaves route by the interval grid, not allocation
		// order. A resumed materialization can allocate missing near leaves after
		// existing far leaves without changing any name, boundary or membership.
		indices := map[string]decimal.Decimal{}
		for _, p := range out.Partitions {
			if !p.IsInterval {
				continue
			}
			if len(p.Upper) != 1 {
				return nil, fmt.Errorf("INTERVAL 自动叶缺少单列上界")
			}
			index, err := source.IntervalBoundaryIndex(out.Interval, p.Upper[0])
			if err != nil {
				return nil, err
			}
			indices[LeafKey(p.Schema, p.Name)], err = decimal.NewFromString(index)
			if err != nil {
				return nil, err
			}
		}
		sort.SliceStable(out.Partitions, func(i, j int) bool {
			a, b := out.Partitions[i], out.Partitions[j]
			if a.IsInterval != b.IsInterval {
				return !a.IsInterval
			}
			if a.IsInterval {
				return indices[LeafKey(a.Schema, a.Name)].LessThan(indices[LeafKey(b.Schema, b.Name)])
			}
			return a.Ordinal < b.Ordinal
		})
		for i := range out.Partitions {
			out.Partitions[i].Ordinal = i + 1
		}
	}
	sort.SliceStable(out.Partitions, func(i, j int) bool {
		if out.Partitions[i].Ordinal != out.Partitions[j].Ordinal {
			return out.Partitions[i].Ordinal < out.Partitions[j].Ordinal
		}
		return LeafKey(out.Partitions[i].Schema, out.Partitions[i].Name) < LeafKey(out.Partitions[j].Schema, out.Partitions[j].Name)
	})
	return out, nil
}

func SpecFingerprint(spec *source.PartitionSpec) (string, error) {
	c, err := CanonicalSpec(spec)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func Equivalent(expected, actual *source.PartitionSpec) error {
	e, err := SpecFingerprint(expected)
	if err != nil {
		return err
	}
	a, err := SpecFingerprint(actual)
	if err != nil {
		return err
	}
	if e != a {
		return fmt.Errorf("目标分区定义与冻结计划不一致（expected %s, actual %s）", e[:12], a[:12])
	}
	return nil
}

func planFingerprint(p *Plan) (string, error) {
	b, err := json.Marshal(struct {
		Source, Target *source.PartitionSpec
		Statements     any
		Mapping        []Mapping
		Rule           string
		Interval       *IntervalMaterialization
		KeyColumns     []KeyColumnDomain `json:",omitempty"`
	}{p.Source, p.Target, p.Statements, p.Mapping, p.Rule, p.Interval, p.KeyColumns})
	if err != nil {
		return "", err
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:]), nil
}
