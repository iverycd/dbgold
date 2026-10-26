package damengnative

import (
	"fmt"
	"strings"
)

// NativeGeneratedColumnDeclaration returns the original complete declaration,
// never an expression synthesized from driver type names. It is used only by
// the authenticated empty-target interval executor to restore an attribute it
// temporarily changed. Quoted names, comments and strings remain untouched.
func NativeGeneratedColumnDeclaration(ddl, column string) (string, error) {
	start, end, _, _, err := nativeGeneratedColumnSpan(ddl, column)
	if err != nil {
		return "", err
	}
	return ddl[start:end], nil
}

// RewriteNativeGeneratedColumnConstant changes only AS's expression. The new
// expression must consist entirely of native constants/casts; this API cannot
// introduce a call to an application routine, a query or another column.
func RewriteNativeGeneratedColumnConstant(ddl, column, expression string) (string, error) {
	if !materializedEmptyPartitionConstant(expression) {
		return "", fmt.Errorf("间隔生成键的临时表达式不是原生常量")
	}
	_, _, start, end, err := nativeGeneratedColumnSpan(ddl, column)
	if err != nil {
		return "", err
	}
	return ddl[:start] + expression + ddl[end:], nil
}

// RestoreNativeGeneratedColumnDeclaration replaces one complete declaration
// with the same named declaration from the frozen original table. Callers must
// first prove the current declaration is original or a trusted temporary one.
func RestoreNativeGeneratedColumnDeclaration(ddl, column, originalDDL string) (string, error) {
	start, end, _, _, err := nativeGeneratedColumnSpan(ddl, column)
	if err != nil {
		return "", err
	}
	declaration, err := NativeGeneratedColumnDeclaration(originalDDL, column)
	if err != nil {
		return "", err
	}
	return ddl[:start] + declaration + ddl[end:], nil
}

func nativeGeneratedColumnSpan(ddl, column string) (start, end, expressionStart, expressionEnd int, err error) {
	ts, err := nativeTokens(ddl)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	depth, first := 0, -1
	for i, token := range ts {
		if token.kind == 'p' && token.value == "(" {
			depth++
			if depth == 1 && first < 0 {
				first = i + 1
			}
			continue
		}
		if token.kind == 'p' && depth == 1 && (token.value == "," || token.value == ")") && first >= 0 {
			if first < i {
				name := ts[first].value
				if ts[first].kind == 'i' {
					name = strings.ToUpper(name)
				}
				if (ts[first].kind == 'q' || ts[first].kind == 'i') && name == column {
					local := 0
					for j := first + 1; j+1 < i; j++ {
						if local == 0 && ts[j].kind == 'i' && strings.EqualFold(ts[j].value, "AS") && ts[j+1].kind == 'p' && ts[j+1].value == "(" {
							level := 1
							for k := j + 2; k < i; k++ {
								if ts[k].kind == 'p' && ts[k].value == "(" {
									level++
								}
								if ts[k].kind == 'p' && ts[k].value == ")" {
									level--
									if level == 0 {
										return ts[first].start, ts[i-1].end, ts[j+1].end, ts[k].start, nil
									}
								}
							}
						}
						if ts[j].kind == 'p' && ts[j].value == "(" {
							local++
						}
						if ts[j].kind == 'p' && ts[j].value == ")" {
							local--
						}
					}
					return 0, 0, 0, 0, fmt.Errorf("生成分区列 %s 缺少完整原生表达式", column)
				}
			}
			first = i + 1
		}
		if token.kind == 'p' && token.value == ")" {
			depth--
			if depth == 0 && first >= 0 {
				break
			}
		}
	}
	return 0, 0, 0, 0, fmt.Errorf("生成分区列 %s 不在原生表定义中", column)
}
