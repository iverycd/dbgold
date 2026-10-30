package source

import (
	"fmt"
	"strings"

	"dbgold/datamigrate/sqlsyntax"
)

// OscarPartitionCatalog is a read-only snapshot of the native V_SYS_PART_TABLES,
// V_SYS_TAB_PARTITIONS and V_SYS_TAB_SUBPARTITIONS views. JDBC owns transport;
// this decoder never queries a database or executes the catalog's SQL text.
type OscarPartitionCatalog struct {
	Schema, DDL, Interval                 string
	Method, Submethod                     int
	KeyCount, SubkeyCount, PartitionCount int
	Partitions                            []OscarPartitionCatalogRow
}

type OscarPartitionCatalogRow struct {
	Name, Boundary    string
	Position          int
	SubpartitionCount int
	Subpartitions     []OscarPartitionCatalogRow
}

// DecodeOscarPartitionCatalog follows the vendor's 2025-01-17 database
// development manual (V_SYS_PART_TABLES, V_SYS_TABLE and partition views).
// The Oracle-compatible ALL_TABLES.ROW_MOVEMENT is documented as a constant;
// it is deliberately not used as evidence of a per-table movement switch.
func DecodeOscarPartitionCatalog(c OscarPartitionCatalog) (*PartitionSpec, error) {
	if c.Schema == "" || c.DDL == "" || c.PartitionCount < 1 || c.PartitionCount != len(c.Partitions) {
		return nil, fmt.Errorf("神通原生分区目录的 Schema、DDL 或分区数量不完整")
	}
	tokens, err := sqlsyntax.Lex(c.DDL)
	if err != nil {
		return nil, fmt.Errorf("神通原生分区 DDL 不能安全解析: %w", err)
	}
	root, child, err := oscarPartitionKeys(tokens)
	if err != nil {
		return nil, err
	}
	if root.Method != oscarPartitionMethod(c.Method) || len(root.Keys) != c.KeyCount {
		return nil, fmt.Errorf("神通分区方法/键与原生 DDL 不一致")
	}
	if (child == nil) != (c.Submethod == 0) || child == nil && c.SubkeyCount != 0 || child != nil && (child.Method != oscarPartitionMethod(c.Submethod) || len(child.Keys) != c.SubkeyCount) {
		return nil, fmt.Errorf("神通子分区方法/键与原生 DDL 不一致")
	}
	if c.Interval != "" || root.Automatic != "" {
		return nil, fmt.Errorf("神通 INTERVAL 未来自动扩展规则尚不能原生等价核验")
	}
	root.SourceDDL = c.DDL
	names := map[string]bool{}
	var fill func(*PartitionSpec, []OscarPartitionCatalogRow, bool) error
	fill = func(spec *PartitionSpec, rows []OscarPartitionCatalogRow, nested bool) error {
		if len(rows) == 0 {
			return fmt.Errorf("神通原生分区清单为空")
		}
		// Native PART is an ordering key. Do not assume a zero/one-based vendor
		// ordinal; persistent logical ordinals are normalized to one-based order.
		for i, row := range rows {
			if row.SubpartitionCount < 0 || row.SubpartitionCount != len(row.Subpartitions) {
				return fmt.Errorf("神通分区 %s 的子分区数量与原生 PARTCNT 不一致", row.Name)
			}
			if row.Name == "" || names[row.Name] || row.Position < 0 || i > 0 && rows[i-1].Position >= row.Position {
				return fmt.Errorf("神通原生分区名称重复或顺序不完整")
			}
			names[row.Name] = true
			part := Partition{Schema: c.Schema, Name: row.Name, Ordinal: i + 1}
			boundary := strings.TrimSpace(row.Boundary)
			if boundary == "" {
				return fmt.Errorf("神通分区 %s 的原生边界为空", row.Name)
			}
			if strings.HasPrefix(boundary, "(") {
				bt, e := sqlsyntax.Lex(boundary)
				if e != nil {
					return e
				}
				if catalogMatchingParen(bt, 0) == len(bt)-1 {
					boundary = oscarPartitionTokens(bt[1 : len(bt)-1])
				}
			}
			if err := catalogPartitionBoundary(&part, spec.Method, boundary, len(spec.Keys)); err != nil {
				return fmt.Errorf("神通分区 %s 的原生边界不能完整解析: %w", row.Name, err)
			}
			if part.Default && i != len(rows)-1 {
				return fmt.Errorf("神通原生 LIST DEFAULT 分区必须位于最后")
			}
			if !nested && child != nil {
				part.Subpartition = ClonePartitionSpec(child)
				if err := fill(part.Subpartition, row.Subpartitions, true); err != nil {
					return err
				}
			} else if len(row.Subpartitions) > 0 {
				return fmt.Errorf("神通目录包含未声明或超过两级的子分区")
			}
			spec.Partitions = append(spec.Partitions, part)
		}
		return nil
	}
	if err := fill(root, c.Partitions, false); err != nil {
		return nil, err
	}
	catalogFinalizePartitionBounds(root)
	catalogSetPartitionRowMovement(root, "enabled")
	return root, nil
}

func oscarPartitionMethod(code int) string {
	switch code {
	case 1:
		return "RANGE"
	case 2:
		return "LIST"
	default:
		return ""
	}
}

func oscarPartitionKeys(tokens []sqlsyntax.Token) (*PartitionSpec, *PartitionSpec, error) {
	if len(tokens) < 3 || !tokens[0].Is("CREATE") || !tokens[1].Is("TABLE") {
		return nil, nil, fmt.Errorf("神通目录未返回完整 CREATE TABLE 原生定义")
	}
	var root, child *PartitionSpec
	depth := 0
	for i := 0; i < len(tokens); i++ {
		t := tokens[i]
		if t.Is(";") && i != len(tokens)-1 {
			return nil, nil, fmt.Errorf("神通原生表定义包含额外语句，不能作为单表分区证据")
		}
		if t.Is("(") {
			depth++
		} else if t.Is(")") {
			depth--
			if depth < 0 {
				return nil, nil, fmt.Errorf("神通原生 DDL 括号不完整")
			}
		}
		if depth != 0 || i+3 >= len(tokens) || (!t.Is("PARTITION") && !t.Is("SUBPARTITION")) || !tokens[i+1].Is("BY") {
			continue
		}
		method := strings.ToUpper(tokens[i+2].Text)
		if (method != "RANGE" && method != "LIST") || !tokens[i+3].Is("(") {
			return nil, nil, fmt.Errorf("神通 %s 原生分区方法尚未实现等价适配", method)
		}
		end := catalogMatchingParen(tokens, i+3)
		if end < 0 {
			return nil, nil, fmt.Errorf("神通分区键括号不完整")
		}
		spec := &PartitionSpec{Method: method}
		for j := i + 4; j < end; j += 2 {
			key := tokens[j]
			if (key.Kind != 'i' && key.Kind != 'q') || j+1 < end && !tokens[j+1].Is(",") || j+1 == end-1 {
				return nil, nil, fmt.Errorf("神通原生分区键不是完整的直接列名列表")
			}
			name := key.Value
			if key.Kind == 'i' {
				name = strings.ToUpper(name)
			}
			spec.Keys = append(spec.Keys, PartitionKey{Column: name})
		}
		if len(spec.Keys) == 0 || len(spec.Keys) > 16 || method == "LIST" && len(spec.Keys) != 1 {
			return nil, nil, fmt.Errorf("神通原生分区键数量不符合 RANGE/LIST 定义")
		}
		if end+1 < len(tokens) && tokens[end+1].Is("INTERVAL") {
			return nil, nil, fmt.Errorf("神通 INTERVAL 未来自动扩展规则尚不能原生等价核验")
		}
		if t.Is("PARTITION") && root == nil {
			root = spec
		} else if t.Is("SUBPARTITION") && root != nil && child == nil {
			child = spec
		} else {
			return nil, nil, fmt.Errorf("神通原生分区方法声明重复或层次不完整")
		}
		i = end
	}
	if depth != 0 || root == nil {
		return nil, nil, fmt.Errorf("神通原生 DDL 缺少完整分区定义")
	}
	return root, child, nil
}

func oscarPartitionTokens(tokens []sqlsyntax.Token) string {
	var out strings.Builder
	for i, token := range tokens {
		if i > 0 && token.Kind != 'p' && tokens[i-1].Kind != 'p' {
			out.WriteByte(' ')
		}
		out.WriteString(token.Text)
	}
	return out.String()
}
