package source

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"dbgold/datamigrate/sqlsyntax"
)

func (r *OracleReader) PartitionMetadata(ctx context.Context, table string) (*PartitionSpec, error) {
	var method, submethod string
	var interval sql.NullString
	err := r.db.QueryRowContext(ctx, `SELECT PARTITIONING_TYPE,SUBPARTITIONING_TYPE,INTERVAL FROM ALL_PART_TABLES WHERE OWNER=:1 AND TABLE_NAME=:2`, r.owner, table).Scan(&method, &submethod, &interval)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取 Oracle 分区目录: %w", err)
	}
	rowMovement, err := catalogOracleStyleRowMovement(ctx, r.db, r.owner, table, false)
	if err != nil {
		return nil, err
	}
	spec, err := catalogOracleStylePartitions(ctx, r.db, r.owner, table, method, submethod, false)
	if spec != nil {
		spec.Automatic = strings.TrimSpace(interval.String)
		catalogSetPartitionRowMovement(spec, rowMovement)
		if err == nil && spec.Automatic != "" {
			err = catalogOracleInterval(ctx, r.db, r.owner, table, spec)
		}
	}
	return spec, err
}

func catalogOracleStyleRowMovement(ctx context.Context, db *sql.DB, owner, table string, dm bool) (string, error) {
	first, second, product := ":1", ":2", "Oracle"
	if dm {
		first, second, product = "?", "?", "达梦"
	}
	var value sql.NullString
	query := `SELECT ROW_MOVEMENT FROM ALL_TABLES WHERE OWNER=` + first + ` AND TABLE_NAME=` + second
	if err := db.QueryRowContext(ctx, query, owner, table).Scan(&value); err != nil {
		return "", fmt.Errorf("读取%s原生分区行移动属性失败，尚不能证明未来跨分区 UPDATE 等价: %w", product, err)
	}
	if value.Valid {
		switch strings.ToUpper(strings.TrimSpace(value.String)) {
		case "ENABLED":
			return "enabled", nil
		case "DISABLED":
			return "disabled", nil
		}
	}
	return "", fmt.Errorf("%s原生分区 ROW_MOVEMENT 属性缺失或不是 ENABLED/DISABLED，尚不能证明未来跨分区 UPDATE 等价", product)
}

func catalogSetPartitionRowMovement(spec *PartitionSpec, movement string) {
	spec.RowMovement = movement
	for i := range spec.Partitions {
		if sub := spec.Partitions[i].Subpartition; sub != nil {
			catalogSetPartitionRowMovement(sub, movement)
		}
	}
}

func (r *OracleReader) PartitionRoots(context.Context) (map[string]string, error) {
	// Oracle partitions and subpartitions have labels, not separate ALL_TABLES
	// entries. They must never hide an unrelated logical table of the same name.
	return map[string]string{}, nil
}

func catalogOracleStylePartitions(ctx context.Context, db *sql.DB, owner, table, method, submethod string, dm bool) (*PartitionSpec, error) {
	spec := &PartitionSpec{Method: strings.ToUpper(strings.TrimSpace(method))}
	switch spec.Method {
	case "RANGE", "LIST":
	case "HASH":
		spec.Algorithm = "oracle-native-hash"
		if dm {
			spec.Algorithm = "dameng-native-hash"
		}
	default:
		catalogPartitionUnsupported(spec, "尚不能等价表达的原生分区方法 "+spec.Method)
	}
	keys, err := catalogOracleStyleKeys(ctx, db, owner, table, false, dm)
	if err != nil {
		return nil, err
	}
	spec.Keys = keys
	if len(keys) == 0 {
		catalogPartitionUnsupported(spec, "原生分区键目录为空，可能为引用分区或表达式分区")
	}
	bind1, bind2, bind3 := ":1", ":2", ":3"
	if dm {
		bind1, bind2, bind3 = "?", "?", "?"
	}
	q := `SELECT PARTITION_NAME,PARTITION_POSITION,HIGH_VALUE,SUBPARTITION_COUNT FROM ALL_TAB_PARTITIONS WHERE TABLE_OWNER=` + bind1 + ` AND TABLE_NAME=` + bind2 + ` ORDER BY PARTITION_POSITION`
	rows, err := db.QueryContext(ctx, q, owner, table)
	if err != nil {
		return nil, fmt.Errorf("读取分区边界: %w", err)
	}
	var subcounts []int
	for rows.Next() {
		var part Partition
		var high sql.NullString
		var subcount int
		if err = rows.Scan(&part.Name, &part.Ordinal, &high, &subcount); err != nil {
			rows.Close()
			return nil, err
		}
		part.Schema, part.LowerInclusive = owner, true
		if part.Name == "" || part.Ordinal < 1 || subcount < 0 {
			rows.Close()
			return nil, fmt.Errorf("原生分区目录的名称、位置或子分区数量不完整")
		}
		if e := catalogPartitionBoundary(&part, spec.Method, high.String, len(keys)); e != nil {
			catalogPartitionUnsupported(spec, e.Error())
		}
		spec.Partitions = append(spec.Partitions, part)
		subcounts = append(subcounts, subcount)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(spec.Partitions) == 0 {
		return nil, fmt.Errorf("已声明分区表但没有可读取的分区清单")
	}
	submethod = strings.ToUpper(strings.TrimSpace(submethod))
	var subkeys []PartitionKey
	if submethod != "" && submethod != "NONE" {
		subkeys, err = catalogOracleStyleKeys(ctx, db, owner, table, true, dm)
		if err != nil {
			return nil, err
		}
		if len(subkeys) == 0 {
			catalogPartitionUnsupported(spec, "子分区键目录为空")
		}
	}
	for i := range spec.Partitions {
		if subcounts[i] == 0 {
			if len(subkeys) > 0 {
				catalogPartitionUnsupported(spec, "复合分区存在缺少子分区的父分区")
			}
			continue
		}
		if len(subkeys) == 0 {
			catalogPartitionUnsupported(spec, "已有子分区但缺少可识别的原生子分区策略")
			continue
		}
		sub := &PartitionSpec{Method: submethod, Keys: append([]PartitionKey(nil), subkeys...)}
		if submethod == "HASH" {
			sub.Algorithm = "oracle-native-hash"
			if dm {
				sub.Algorithm = "dameng-native-hash"
			}
		} else if submethod != "RANGE" && submethod != "LIST" {
			catalogPartitionUnsupported(sub, "尚不能等价表达的原生子分区方法 "+submethod)
		}
		q = `SELECT SUBPARTITION_NAME,SUBPARTITION_POSITION,HIGH_VALUE FROM ALL_TAB_SUBPARTITIONS WHERE TABLE_OWNER=` + bind1 + ` AND TABLE_NAME=` + bind2 + ` AND PARTITION_NAME=` + bind3 + ` ORDER BY SUBPARTITION_POSITION`
		rows, err = db.QueryContext(ctx, q, owner, table, spec.Partitions[i].Name)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var part Partition
			var high sql.NullString
			if err = rows.Scan(&part.Name, &part.Ordinal, &high); err != nil {
				rows.Close()
				return nil, err
			}
			part.Schema, part.LowerInclusive = owner, true
			if e := catalogPartitionBoundary(&part, submethod, high.String, len(subkeys)); e != nil {
				catalogPartitionUnsupported(sub, e.Error())
			}
			sub.Partitions = append(sub.Partitions, part)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if len(sub.Partitions) != subcounts[i] {
			return nil, fmt.Errorf("分区 %s 的子分区目录数量发生变化或不可完整读取", spec.Partitions[i].Name)
		}
		spec.Partitions[i].Subpartition = sub
	}
	catalogFinalizePartitionBounds(spec)
	return spec, nil
}

func catalogOracleStyleKeys(ctx context.Context, db *sql.DB, owner, table string, sub, dm bool) ([]PartitionKey, error) {
	view, first, second := "ALL_PART_KEY_COLUMNS", ":1", ":2"
	if sub {
		view = "ALL_SUBPART_KEY_COLUMNS"
	}
	if dm {
		first, second = "?", "?"
	}
	q := `SELECT k.COLUMN_NAME,c.DATA_TYPE FROM ` + view + ` k JOIN ALL_TAB_COLUMNS c ON c.OWNER=k.OWNER AND c.TABLE_NAME=k.NAME AND c.COLUMN_NAME=k.COLUMN_NAME WHERE k.OWNER=` + first + ` AND k.NAME=` + second + ` AND k.OBJECT_TYPE='TABLE' ORDER BY k.COLUMN_POSITION`
	rows, err := db.QueryContext(ctx, q, owner, table)
	if err != nil {
		return nil, fmt.Errorf("读取原生分区键: %w", err)
	}
	defer rows.Close()
	var keys []PartitionKey
	for rows.Next() {
		var key PartitionKey
		if err = rows.Scan(&key.Column, &key.DataType); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

// Oracle metadata deparses DATE bounds as TO_DATE calls. Accept only the exact
// literal constructors below; arbitrary source expressions remain unsupported.
func catalogOracleTemporalConstant(raw string) (PartitionValue, bool, error) {
	raw = strings.TrimSpace(raw)
	upper := strings.ToUpper(raw)
	name := ""
	for _, candidate := range []string{"TO_DATE", "TO_TIMESTAMP"} {
		if strings.HasPrefix(upper, candidate+"(") || strings.HasPrefix(upper, candidate+" (") {
			name = candidate
			break
		}
	}
	if name == "" {
		return PartitionValue{}, false, nil
	}
	open := strings.IndexByte(raw, '(')
	if open < 0 || !strings.HasSuffix(strings.TrimSpace(raw), ")") {
		return PartitionValue{}, true, fmt.Errorf("原生日期边界构造函数不完整")
	}
	args, err := catalogSplit(strings.TrimSpace(raw)[open+1 : len(strings.TrimSpace(raw))-1])
	if err != nil || len(args) < 2 || len(args) > 3 {
		return PartitionValue{}, true, fmt.Errorf("原生日期边界不是固定常量构造")
	}
	var values []string
	for _, arg := range args {
		tokens, e := sqlsyntax.Lex(arg)
		if e != nil || len(tokens) != 1 || tokens[0].Kind != 's' {
			return PartitionValue{}, true, fmt.Errorf("原生日期边界包含非字符串参数")
		}
		values = append(values, tokens[0].Value)
	}
	if len(values) == 3 && strings.ToUpper(strings.TrimSpace(values[2])) != "NLS_CALENDAR=GREGORIAN" {
		return PartitionValue{}, true, fmt.Errorf("原生日期边界依赖未支持的日历参数")
	}
	format := strings.ToUpper(strings.TrimSpace(values[1]))
	text := strings.TrimSpace(values[0])
	if format == "YYYY-MM-DD" || format == "SYYYY-MM-DD" {
		if _, err = time.Parse("2006-01-02", text); err != nil {
			return PartitionValue{}, true, fmt.Errorf("日期分区边界不在可证明的公历范围内")
		}
		return PartitionValue{Kind: "date", Text: text}, true, nil
	}
	valid := format == "YYYY-MM-DD HH24:MI:SS" || format == "SYYYY-MM-DD HH24:MI:SS"
	if name == "TO_TIMESTAMP" {
		for _, prefix := range []string{"YYYY-MM-DD HH24:MI:SS.FF", "SYYYY-MM-DD HH24:MI:SS.FF"} {
			valid = valid || format == prefix || len(format) == len(prefix)+1 && strings.HasPrefix(format, prefix) && format[len(format)-1] >= '1' && format[len(format)-1] <= '9'
		}
	}
	if !valid {
		return PartitionValue{}, true, fmt.Errorf("未支持的原生日期分区边界格式")
	}
	if _, err = time.Parse("2006-01-02 15:04:05.999999999", text); err != nil {
		return PartitionValue{}, true, fmt.Errorf("时间分区边界不在可证明的公历范围内")
	}
	return PartitionValue{Kind: "timestamp", Text: text}, true, nil
}

func (r *OracleReader) PartitionIndexes(ctx context.Context, table string) ([]IndexInfo, error) {
	return catalogOracleStyleIndexes(ctx, r.db, r.owner, table, false)
}

func catalogOracleStyleIndexes(ctx context.Context, db *sql.DB, owner, table string, dm bool) ([]IndexInfo, error) {
	b1, b2 := ":1", ":2"
	if dm {
		b1, b2 = "?", "?"
	}
	q := `SELECT i.INDEX_NAME,i.UNIQUENESS,i.INDEX_TYPE,p.LOCALITY,c.COLUMN_NAME,c.COLUMN_POSITION,c.DESCEND,CASE WHEN EXISTS(SELECT 1 FROM ALL_CONSTRAINTS k WHERE k.OWNER=i.OWNER AND k.INDEX_NAME=i.INDEX_NAME AND k.CONSTRAINT_TYPE='P') THEN 1 ELSE 0 END FROM ALL_INDEXES i LEFT JOIN ALL_PART_INDEXES p ON p.OWNER=i.OWNER AND p.INDEX_NAME=i.INDEX_NAME JOIN ALL_IND_COLUMNS c ON c.INDEX_OWNER=i.OWNER AND c.INDEX_NAME=i.INDEX_NAME WHERE i.TABLE_OWNER=` + b1 + ` AND i.TABLE_NAME=` + b2 + ` ORDER BY i.INDEX_NAME,c.COLUMN_POSITION`
	rows, err := db.QueryContext(ctx, q, owner, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []IndexInfo
	byName := map[string]int{}
	for rows.Next() {
		var name, uniqueness, method, column, direction string
		var locality sql.NullString
		var ordinal, primary int
		if err = rows.Scan(&name, &uniqueness, &method, &locality, &column, &ordinal, &direction, &primary); err != nil {
			return nil, err
		}
		i, ok := byName[name]
		if !ok {
			i = len(result)
			byName[name] = i
			loc := strings.ToLower(locality.String)
			if !locality.Valid {
				loc = "global"
			}
			result = append(result, IndexInfo{TableName: table, IndexName: name, IsUnique: uniqueness == "UNIQUE", IsPrimary: primary == 1, Method: method, Locality: loc})
		}
		if ordinal != len(result[i].Columns)+1 || strings.HasPrefix(column, "SYS_NC") || strings.Contains(method, "FUNCTION-BASED") {
			result[i].Unsupported = "分区索引包含表达式或不完整键列"
		}
		result[i].Columns = append(result[i].Columns, column)
		result[i].Descending = append(result[i].Descending, direction == "DESC")
	}
	return result, rows.Err()
}
