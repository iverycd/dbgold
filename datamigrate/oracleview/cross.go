package oracleview

import (
	"dbgold/datamigrate/sqlsyntax"
	"fmt"
	"github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"sort"
	"strings"
)

// ConvertCross parses a deliberately small shared SELECT grammar. No source
// expression is sent through verbatim as an Oracle expression.
func ConvertCross(raw, product, sourceSchema, targetSchema string) (string, []string, error) {
	ts, err := sqlsyntax.LexTSQL(raw)
	if err != nil {
		return "", nil, err
	}
	if len(ts) > 0 && (ts[0].Is("CREATE") || ts[0].Is("ALTER")) {
		i := 1
		if i+1 < len(ts) && ts[i].Is("OR") && (ts[i+1].Is("REPLACE") || ts[i+1].Is("ALTER")) {
			i += 2
		}
		if i >= len(ts) || !ts[i].Is("VIEW") {
			return "", nil, fmt.Errorf("视图头无效")
		}
		i++
		if i >= len(ts) {
			return "", nil, fmt.Errorf("缺少视图名")
		}
		i++
		if i < len(ts) && ts[i].Is(".") {
			i += 2
		}
		if i >= len(ts) || !ts[i].Is("AS") {
			return "", nil, fmt.Errorf("视图列清单或 WITH 选项需人工适配")
		}
		ts = ts[i+1:]
	}
	if len(ts) == 0 || !ts[0].Is("SELECT") {
		return "", nil, fmt.Errorf("只支持 SELECT 视图")
	}
	parts := []string{}
	for i, t := range ts {
		if t.Is(";") {
			if i == len(ts)-1 {
				continue
			}
			return "", nil, fmt.Errorf("视图包含多条语句")
		}
		if t.Kind == 'q' {
			parts = append(parts, "`"+strings.ReplaceAll(t.Value, "`", "``")+"`")
			continue
		}
		if t.Kind == 's' {
			parts = append(parts, literal(t.Value))
			continue
		}
		if t.Is("N") && i+1 < len(ts) && ts[i+1].Kind == 's' {
			continue
		}
		for _, bad := range []string{"WITH", "OVER", "TOP", "ORDER", "LIMIT", "OFFSET", "APPLY", "PIVOT", "CONNECT", "START", "COLLATE", "CAST", "CONVERT", "IF", "IFNULL", "ISNULL", "NVL", "NOW", "CURRENT_TIMESTAMP", "EXCEPT", "INTERSECT", "+", "-", "/", "%", "||"} {
			if t.Is(bad) {
				return "", nil, fmt.Errorf("%s 视图语法 %s 需人工适配", product, t.Text)
			}
		}
		parts = append(parts, t.Text)
	}
	p := parser.New()
	p.SetSQLMode(mysql.ModeNoBackslashEscapes | mysql.ModeIgnoreSpace)
	n, err := p.ParseOneStmt(strings.Join(parts, " "), "utf8mb4", "utf8mb4_bin")
	if err != nil {
		return "", nil, err
	}
	r := &renderer{source: sourceSchema, target: targetSchema, strip: map[string]bool{strings.ToUpper(sourceSchema): true}, deps: map[string]bool{}, cross: true}
	out := r.selectNode(n)
	deps := []string{}
	for d := range r.deps {
		deps = append(deps, d)
	}
	sort.Strings(deps)
	return out, deps, r.err
}
