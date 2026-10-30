package source

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"dbgold/datamigrate/sqlsyntax"
)

// PartitionMetadata deliberately reads native catalog values without changing
// identifier case. Neither expressions nor SourceDDL are executed as SQL.
func (r *MySQLReader) PartitionMetadata(ctx context.Context, table string) (*PartitionSpec, error) {
	rows, err := r.queryContext(ctx, `SELECT PARTITION_NAME,PARTITION_METHOD,PARTITION_EXPRESSION,PARTITION_DESCRIPTION,PARTITION_ORDINAL_POSITION,SUBPARTITION_NAME,SUBPARTITION_METHOD,SUBPARTITION_EXPRESSION,SUBPARTITION_ORDINAL_POSITION FROM information_schema.PARTITIONS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? ORDER BY PARTITION_ORDINAL_POSITION,SUBPARTITION_ORDINAL_POSITION`, r.dbName, table)
	if err != nil {
		return nil, fmt.Errorf("读取 MySQL 分区目录: %w", err)
	}
	type catalogPart struct {
		name, method, expression, description, subname, submethod, subexpression sql.NullString
		ordinal, subordinal                                                      sql.NullInt64
	}
	var native []catalogPart
	for rows.Next() {
		var p catalogPart
		if err = rows.Scan(&p.name, &p.method, &p.expression, &p.description, &p.ordinal, &p.subname, &p.submethod, &p.subexpression, &p.subordinal); err != nil {
			rows.Close()
			return nil, err
		}
		native = append(native, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(native) == 0 {
		return nil, fmt.Errorf("MySQL 表 %s 的分区目录为空，无法确认对象仍存在", table)
	}
	if !native[0].name.Valid {
		if len(native) != 1 || native[0].method.Valid {
			return nil, fmt.Errorf("MySQL 表 %s 的非分区目录不一致", table)
		}
		return nil, nil
	}
	columns, columnOrder, err := r.mysqlPartitionColumns(ctx, table)
	if err != nil {
		return nil, err
	}
	ddl, err := r.GetTableSourceDDL(ctx, table)
	if err != nil {
		return nil, fmt.Errorf("读取 MySQL 分区原始定义: %w", err)
	}
	spec := mysqlCatalogSpec(native[0].method.String, native[0].expression.String, columns)
	spec.SourceDDL = ddl
	positions := map[string]int{}
	for _, p := range native {
		if !p.name.Valid || !p.ordinal.Valid || p.ordinal.Int64 < 1 || p.method.String != native[0].method.String || p.expression.String != native[0].expression.String {
			return nil, fmt.Errorf("MySQL 分区目录的方法、键或顺序不一致")
		}
		index, exists := positions[p.name.String]
		if !exists {
			part := Partition{Schema: r.dbName, Name: p.name.String, Ordinal: int(p.ordinal.Int64), LowerInclusive: true}
			if e := catalogPartitionBoundary(&part, spec.Method, p.description.String, len(spec.Keys)); e != nil {
				catalogPartitionUnsupported(spec, e.Error())
			}
			positions[p.name.String] = len(spec.Partitions)
			spec.Partitions = append(spec.Partitions, part)
			index = len(spec.Partitions) - 1
		} else if spec.Partitions[index].Ordinal != int(p.ordinal.Int64) {
			return nil, fmt.Errorf("MySQL 同名分区的顺序不一致")
		}
		if p.subname.Valid {
			part := &spec.Partitions[index]
			if !p.subordinal.Valid || p.subordinal.Int64 < 1 || !p.submethod.Valid {
				return nil, fmt.Errorf("MySQL 子分区目录不完整")
			}
			if part.Subpartition == nil {
				part.Subpartition = mysqlCatalogSpec(p.submethod.String, p.subexpression.String, columns)
			} else if part.Subpartition.Method != strings.ToUpper(strings.TrimSpace(p.submethod.String)) {
				return nil, fmt.Errorf("MySQL 子分区方法不一致")
			}
			part.Subpartition.Partitions = append(part.Subpartition.Partitions, Partition{Schema: r.dbName, Name: p.subname.String, Ordinal: int(p.subordinal.Int64)})
		}
	}
	if err := r.freezeMySQLPartitionKeys(ctx, table, ddl, columns, columnOrder, spec); err != nil {
		catalogPartitionUnsupported(spec, err.Error())
	}
	catalogFinalizePartitionBounds(spec)
	return spec, nil
}

func (r *MySQLReader) mysqlPartitionColumns(ctx context.Context, table string) (map[string]PartitionKey, []string, error) {
	rows, err := r.queryContext(ctx, `SELECT COLUMN_NAME,COLUMN_TYPE,COLLATION_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? ORDER BY ORDINAL_POSITION`, r.dbName, table)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := map[string]PartitionKey{}
	var order []string
	for rows.Next() {
		var name, typ string
		var collation sql.NullString
		if err = rows.Scan(&name, &typ, &collation); err != nil {
			return nil, nil, err
		}
		order = append(order, name)
		out[name] = PartitionKey{Column: name, DataType: typ, Collation: collation.String}
	}
	return out, order, rows.Err()
}

func mysqlCatalogSpec(method, expression string, columns map[string]PartitionKey) *PartitionSpec {
	spec := &PartitionSpec{Method: strings.ToUpper(strings.TrimSpace(method)), RowMovement: "enabled"}
	parts, err := catalogSplit(expression)
	isKey := spec.Method == "KEY" || spec.Method == "LINEAR KEY"
	if isKey && strings.TrimSpace(expression) == "" {
		parts, err = nil, nil // KEY() is resolved against its native primary/unique index below.
	}
	if err != nil {
		catalogPartitionUnsupported(spec, err.Error())
	} else {
		for _, part := range parts {
			tokens, e := sqlsyntax.Lex(part)
			if e == nil && len(tokens) == 1 && (tokens[0].Kind == 'i' || tokens[0].Kind == 'q') {
				name := tokens[0].Text
				if tokens[0].Kind == 'q' {
					name = tokens[0].Value
				}
				if key, ok := columns[name]; ok {
					spec.Keys = append(spec.Keys, key)
					continue
				}
			}
			spec.Keys = append(spec.Keys, PartitionKey{Expression: part})
		}
	}
	switch spec.Method {
	case "RANGE", "RANGE COLUMNS", "LIST", "LIST COLUMNS":
	case "HASH":
		spec.Algorithm = "mysql-mod"
	case "LINEAR HASH":
		spec.Algorithm = "mysql-linear-hash"
	case "KEY", "LINEAR KEY":
		spec.Algorithm = "mysql-key-unresolved"
	default:
		catalogPartitionUnsupported(spec, "尚不能识别的 MySQL 分区方法 "+spec.Method)
	}
	if len(spec.Keys) == 0 && !isKey {
		catalogPartitionUnsupported(spec, "分区键为空或隐式键尚不能冻结")
	}
	return spec
}

// MySQL partitions are labels below one logical table, not independently
// selectable tables in information_schema.TABLES.
func (r *MySQLReader) PartitionRoots(context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}

func (r *MySQLReader) PartitionIndexes(ctx context.Context, table string) ([]IndexInfo, error) {
	rows, err := r.queryContext(ctx, `SELECT INDEX_NAME,NON_UNIQUE,SEQ_IN_INDEX,COLUMN_NAME,SUB_PART,INDEX_TYPE,COLLATION FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? ORDER BY INDEX_NAME,SEQ_IN_INDEX`, r.dbName, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []IndexInfo
	byName := map[string]int{}
	for rows.Next() {
		var name, method string
		var nonunique, ordinal int
		var column, direction sql.NullString
		var prefix sql.NullInt64
		if err = rows.Scan(&name, &nonunique, &ordinal, &column, &prefix, &method, &direction); err != nil {
			return nil, err
		}
		i, ok := byName[name]
		if !ok {
			i = len(result)
			byName[name] = i
			result = append(result, IndexInfo{TableName: table, IndexName: name, IsUnique: nonunique == 0, IsPrimary: name == "PRIMARY", Method: method, Locality: "local"})
		}
		if ordinal != len(result[i].Columns)+1 || !column.Valid {
			result[i].Unsupported = "分区索引包含表达式或不完整列顺序"
		}
		if prefix.Valid {
			result[i].Unsupported = "分区索引包含前缀键，不能忽略其唯一性语义"
		}
		result[i].Columns = append(result[i].Columns, column.String)
		result[i].Descending = append(result[i].Descending, direction.String == "D")
	}
	return result, rows.Err()
}

func catalogPartitionUnsupported(spec *PartitionSpec, reason string) {
	if spec.Unsupported == "" {
		spec.Unsupported = reason
	} else if !strings.Contains(spec.Unsupported, reason) {
		spec.Unsupported += "; " + reason
	}
}

// catalogSplit splits native constant lists while respecting nested tuples and
// quoted commas. It deliberately rejects comments and backslash escapes whose
// meaning depends on a database session mode.
func catalogSplit(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("分区表达式为空")
	}
	var result []string
	depth, start := 0, 0
	var quote byte
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if quote != 0 {
			if c == '\\' {
				return nil, fmt.Errorf("分区常量包含需按会话模式解释的反斜杠")
			}
			if c == quote {
				if i+1 < len(raw) && raw[i+1] == quote {
					i++
				} else {
					quote = 0
				}
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			quote = c
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return nil, fmt.Errorf("分区常量括号不平衡")
			}
		case ',':
			if depth == 0 {
				result = append(result, strings.TrimSpace(raw[start:i]))
				start = i + 1
			}
		case ';':
			return nil, fmt.Errorf("分区常量包含语句分隔符")
		case '-':
			if i+1 < len(raw) && raw[i+1] == '-' {
				return nil, fmt.Errorf("分区常量包含注释")
			}
		case '/':
			if i+1 < len(raw) && raw[i+1] == '*' {
				return nil, fmt.Errorf("分区常量包含注释")
			}
		}
	}
	if depth != 0 || quote != 0 {
		return nil, fmt.Errorf("分区常量括号或引号不完整")
	}
	result = append(result, strings.TrimSpace(raw[start:]))
	for _, part := range result {
		if part == "" {
			return nil, fmt.Errorf("分区常量列表包含空值")
		}
	}
	return result, nil
}

var catalogPartitionNumber = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)

func catalogPartitionValue(raw string) (PartitionValue, error) {
	raw = strings.TrimSpace(raw)
	upper := strings.ToUpper(raw)
	for _, keyword := range []string{"NULL", "MINVALUE", "MAXVALUE"} {
		if upper == keyword {
			return PartitionValue{Kind: strings.ToLower(keyword)}, nil
		}
	}
	if catalogPartitionNumber.MatchString(raw) {
		return PartitionValue{Kind: "number", Text: raw}, nil
	}
	tokens, err := sqlsyntax.Lex(raw)
	if err == nil && len(tokens) == 1 && tokens[0].Kind == 's' && !strings.Contains(raw, "\\") {
		return PartitionValue{Kind: "string", Text: tokens[0].Value}, nil
	}
	if err == nil && len(tokens) == 2 && tokens[1].Kind == 's' && tokens[0].Is("DATETIME") {
		return PartitionValue{Kind: "timestamp", Text: strings.TrimSpace(tokens[1].Value)}, nil
	}
	if err == nil && len(tokens) == 2 && tokens[1].Kind == 's' && (tokens[0].Is("DATE") || tokens[0].Is("TIMESTAMP")) {
		return PartitionValue{Kind: strings.ToLower(tokens[0].Text), Text: strings.TrimSpace(tokens[1].Value)}, nil
	}
	if value, handled, temporalErr := catalogOracleTemporalConstant(raw); handled {
		return value, temporalErr
	}
	return PartitionValue{}, fmt.Errorf("分区边界不是受支持的原生常量: %s", raw)
}

func catalogPartitionTuple(raw string) ([]PartitionValue, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "(") && strings.HasSuffix(raw, ")") {
		raw = strings.TrimSpace(raw[1 : len(raw)-1])
	}
	parts, err := catalogSplit(raw)
	if err != nil {
		return nil, err
	}
	var values []PartitionValue
	for _, part := range parts {
		value, e := catalogPartitionValue(part)
		if e != nil {
			return nil, e
		}
		values = append(values, value)
	}
	return values, nil
}

func catalogPartitionBoundary(part *Partition, method, description string, keyCount int) error {
	if strings.HasPrefix(method, "RANGE") {
		values, err := catalogPartitionTuple(description)
		if err != nil {
			return err
		}
		if keyCount > 0 && len(values) != keyCount {
			return fmt.Errorf("RANGE 分区边界与键列数量不同")
		}
		part.Upper = values
		return nil
	}
	if strings.HasPrefix(method, "LIST") {
		if strings.EqualFold(strings.TrimSpace(description), "DEFAULT") {
			part.Default = true
			return nil
		}
		entries, err := catalogSplit(description)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			values, e := catalogPartitionTuple(entry)
			if e != nil {
				return e
			}
			if keyCount > 0 && len(values) != keyCount {
				return fmt.Errorf("LIST 分区值与键列数量不同")
			}
			part.Values = append(part.Values, values)
		}
	}
	return nil
}

func catalogFinalizePartitionBounds(spec *PartitionSpec) {
	sort.SliceStable(spec.Partitions, func(i, j int) bool { return spec.Partitions[i].Ordinal < spec.Partitions[j].Ordinal })
	for i := range spec.Partitions {
		part := &spec.Partitions[i]
		if i > 0 && spec.Partitions[i-1].Ordinal >= part.Ordinal {
			catalogPartitionUnsupported(spec, "分区顺序存在重复")
		}
		if strings.HasPrefix(spec.Method, "RANGE") {
			if i == 0 {
				for range spec.Keys {
					part.Lower = append(part.Lower, PartitionValue{Kind: "minvalue"})
				}
			} else {
				part.Lower = append([]PartitionValue(nil), spec.Partitions[i-1].Upper...)
			}
			part.LowerInclusive = true
		}
		if strings.Contains(spec.Method, "HASH") || strings.Contains(spec.Method, "KEY") {
			part.HashModulus, part.HashRemainder = len(spec.Partitions), i
		}
		if part.Subpartition != nil {
			catalogFinalizePartitionBounds(part.Subpartition)
			if part.Subpartition.Unsupported != "" {
				catalogPartitionUnsupported(spec, "子分区: "+part.Subpartition.Unsupported)
			}
		}
	}
}

// SQL Server exposes numbered partitions rather than native names.
func catalogPartitionNumberName(number int) string { return strconv.Itoa(number) }
