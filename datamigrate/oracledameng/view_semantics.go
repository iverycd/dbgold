package oracledameng

import "sort"

// ViewSemanticsFeatures identifies the finite set of expression behaviors whose
// Oracle meaning must be observed on the target. Inspect the original SELECT,
// before CAST AS DATE and TO_DATE are translated.
func ViewSemanticsFeatures(raw string) ([]string, error) {
	ts, err := tokenizeView(raw)
	if err != nil {
		return nil, err
	}
	features := map[string]bool{}
	if viewUsesRownum(ts) {
		features["ROWNUM"] = true
	}
	for i, token := range ts {
		if token.text == "/" {
			features["integer_division"] = true
		}
		if token.text == "|" && i+1 < len(ts) && ts[i+1].text == "|" {
			features["operator_concat"] = true
		}
		if i+1 >= len(ts) || ts[i+1].text != "(" {
			continue
		}
		for _, fn := range []string{"CONCAT", "LPAD", "RPAD", "SOUNDEX", "LAST_DAY", "TRIM", "LTRIM", "RTRIM", "REPLACE", "TRANSLATE", "SUBSTR", "SUBSTRB"} {
			if token.is(fn) {
				features[fn] = true
			}
		}
		if !token.is("CAST") {
			continue
		}
		close, depth := viewClosingParen(ts, i+1, len(ts)), 0
		for j := i + 2; j < close; j++ {
			if ts[j].text == "(" {
				depth++
			} else if ts[j].text == ")" {
				depth--
			} else if depth == 0 && ts[j].is("AS") && j+2 == close && ts[j+1].is("DATE") {
				features["cast_date"] = true
			}
		}
	}
	result := make([]string, 0, len(features))
	for key := range features {
		result = append(result, key)
	}
	sort.Strings(result)
	return result, nil
}

// Only projection expressions can contain the supported ROWNUM pseudo-column.
// Inspect each query block separately so an identically named output alias,
// quoted identifier or qualified physical column cannot request this probe.
func viewUsesRownum(ts []viewToken) bool {
	for start, token := range ts {
		if !token.is("SELECT") {
			continue
		}
		from, depth := -1, 0
		for i := start + 1; i < len(ts); i++ {
			switch ts[i].text {
			case "(":
				depth++
			case ")":
				depth--
			}
			if depth < 0 {
				break
			}
			if depth == 0 && ts[i].is("FROM") {
				from = i
				break
			}
		}
		if from < 0 {
			continue
		}
		projectionStart := start + 1
		if projectionStart < from && (ts[projectionStart].is("DISTINCT") || ts[projectionStart].is("ALL")) {
			projectionStart++
		}
		for _, span := range splitViewTokens(ts, projectionStart, from, ",") {
			_, exprEnd := projectionAlias(ts, span[0], span[1])
			for i := span[0]; i < exprEnd; i++ {
				if !ts[i].is("ROWNUM") || i > span[0] && ts[i-1].text == "." || i+1 < exprEnd && (ts[i+1].text == "." || ts[i+1].text == "(") {
					continue
				}
				return true
			}
		}
	}
	return false
}
