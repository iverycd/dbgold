package source

import (
	"context"
	"database/sql"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
)

// Native openGauss-family partition catalogs use physical labels, not child
// pg_class relations. Catalog fields are probed before this path is enabled.
// Boundary serialization follows the engine's untransformPartitionBoundary and
// MakeListPartitionBoundaryConst, not SQL string splitting.
// https://docs.opengauss.org/en/docs/latest/database_reference/pg_partition.html
// https://github.com/opengauss-mirror/openGauss-server/blob/master/src/gausskernel/cbb/utils/partition/partitionmap.cpp
type pgNativePartition struct {
	oid, parent                   int64
	name, kind, strategy          string
	keys                          sql.NullString
	boundaries, interval, transit sql.NullString
	expression                    sql.NullString
}

func (r *PostgresReader) pgNativePartitionMetadata(ctx context.Context, oid int64, schema string) (*PartitionSpec, error) {
	for _, field := range []string{"parttype", "parentid", "partstrategy", "partkey", "boundaries", "interval", "transit"} {
		if !r.profile.Fields["pg_partition."+field] {
			return &PartitionSpec{Unsupported: "原生 pg_partition 缺少完整分区字段 " + field}, nil
		}
	}
	expression := "NULL::text"
	if r.profile.Fields["pg_partition.partkeyexpr"] {
		expression = "p.partkeyexpr::text"
	}
	fields := `p.oid,p.parentid,p.relname,p.parttype,p.partstrategy,p.partkey::text,p.boundaries::text,p.interval::text,p.transit::text,` + expression
	query := `SELECT ` + fields + ` FROM ` + r.qcat("pg_partition") + ` p WHERE p.parentid=$1 AND p.parttype='r'`
	rows, err := r.db.QueryContext(ctx, query, oid)
	if err != nil {
		return nil, err
	}
	var root pgNativePartition
	rootCount := 0
	for rows.Next() {
		if err = scanPGNativePartition(rows, &root); err != nil {
			break
		}
		rootCount++
	}
	if err == nil {
		err = rows.Err()
	}
	closeErr := rows.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	if rootCount != 1 {
		return nil, fmt.Errorf("原生分区根目录不唯一或不可见")
	}
	query = `WITH RECURSIVE family AS (SELECT p.oid FROM ` + r.qcat("pg_partition") + ` p WHERE p.parentid=$1 AND p.parttype IN ('p','s') UNION ALL SELECT p.oid FROM ` + r.qcat("pg_partition") + ` p JOIN family f ON p.parentid=f.oid WHERE p.parttype IN ('p','s')) SELECT ` + fields + ` FROM ` + r.qcat("pg_partition") + ` p JOIN family f ON f.oid=p.oid ORDER BY p.parentid,p.oid`
	rows, err = r.db.QueryContext(ctx, query, oid)
	if err != nil {
		return nil, err
	}
	children := map[int64][]pgNativePartition{}
	known := map[int64]bool{}
	for rows.Next() {
		var node pgNativePartition
		if err = scanPGNativePartition(rows, &node); err != nil {
			break
		}
		if known[node.oid] {
			err = fmt.Errorf("原生分区父链存在重复身份或循环")
			break
		}
		known[node.oid] = true
		children[node.parent] = append(children[node.parent], node)
	}
	if err == nil {
		err = rows.Err()
	}
	closeErr = rows.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	query = `SELECT a.attnum,a.attname,format_type(a.atttypid,a.atttypmod),COALESCE(coll.collname,'') FROM ` + r.qcat("pg_attribute") + ` a LEFT JOIN ` + r.qcat("pg_collation") + ` coll ON coll.oid=a.attcollation WHERE a.attrelid=$1 AND a.attnum>0 AND NOT a.attisdropped ORDER BY a.attnum`
	rows, err = r.db.QueryContext(ctx, query, oid)
	if err != nil {
		return nil, err
	}
	columns := map[int]PartitionKey{}
	for rows.Next() {
		var number int
		var key PartitionKey
		if err = rows.Scan(&number, &key.Column, &key.DataType, &key.Collation); err != nil {
			break
		}
		columns[number] = key
	}
	if err == nil {
		err = rows.Err()
	}
	closeErr = rows.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	return r.buildPGNativePartitionSpec(root, oid, schema, children, columns, map[int64]bool{})
}

func scanPGNativePartition(rows *sql.Rows, node *pgNativePartition) error {
	return rows.Scan(&node.oid, &node.parent, &node.name, &node.kind, &node.strategy, &node.keys, &node.boundaries, &node.interval, &node.transit, &node.expression)
}

func (r *PostgresReader) buildPGNativePartitionSpec(root pgNativePartition, parent int64, schema string, children map[int64][]pgNativePartition, columns map[int]PartitionKey, visiting map[int64]bool) (*PartitionSpec, error) {
	if visiting[parent] {
		return nil, fmt.Errorf("原生分区父链存在循环")
	}
	visiting[parent] = true
	defer delete(visiting, parent)
	method := map[string]string{"r": "RANGE", "i": "RANGE", "l": "LIST", "h": "HASH"}[root.strategy]
	product := r.profile.Product
	if product == "" {
		product = "unknown"
	}
	spec := &PartitionSpec{Method: method, Algorithm: "pg_partition:" + product + ":" + root.strategy}
	if method == "" {
		spec.Unsupported = "原生分区策略 " + root.strategy + " 尚不能精确转换"
		return spec, nil
	}
	if root.expression.Valid && strings.TrimSpace(root.expression.String) != "" {
		spec.Unsupported = "原生表达式分区键尚不能完整冻结"
		return spec, nil
	}
	keyNumbers, err := ParsePGNumbers(root.keys.String)
	if err != nil {
		return nil, err
	}
	for _, number := range keyNumbers {
		key, known := columns[number]
		if !known || number <= 0 {
			spec.Unsupported = "原生分区键不是可完整读取的直接列"
			return spec, nil
		}
		spec.Keys = append(spec.Keys, key)
	}
	if len(spec.Keys) == 0 {
		spec.Unsupported = "原生分区键目录为空"
		return spec, nil
	}
	interval, err := pgNativeArray(root.interval)
	if err != nil {
		return nil, err
	}
	transit, err := pgNativeArray(root.transit)
	if err != nil {
		return nil, err
	}
	if root.strategy == "i" || len(interval) > 0 || len(transit) > 0 {
		spec.Automatic = "interval=" + root.interval.String + ";transit=" + root.transit.String
		spec.Unsupported = "原生 INTERVAL 的过渡边界及既有空自动分区恢复尚未实现，不能只复制当前分区快照"
	}
	nodes := children[parent]
	if len(nodes) == 0 {
		return nil, fmt.Errorf("原生分区表没有完整可读取的叶清单")
	}
	for i, node := range nodes {
		part := Partition{Schema: schema, Name: node.name, Ordinal: i + 1, LowerInclusive: true}
		if part.Name == "" {
			return nil, fmt.Errorf("原生分区缺少名称")
		}
		values, e := pgNativeArray(node.boundaries)
		if e != nil {
			return nil, e
		}
		switch method {
		case "RANGE":
			if len(values) != len(spec.Keys) {
				return nil, fmt.Errorf("原生 RANGE 边界数量与分区键不同")
			}
			for j, raw := range values {
				value, e := pgNativeValue(raw, spec.Keys[j], true)
				if e != nil {
					catalogPartitionUnsupported(spec, e.Error())
				}
				part.Upper = append(part.Upper, value)
			}
		case "LIST":
			if len(values) == 0 {
				return nil, fmt.Errorf("原生 LIST 分区缺少值清单")
			}
			for _, raw := range values {
				if !raw.Valid {
					if len(values) != 1 {
						return nil, fmt.Errorf("原生 LIST 默认分区与普通值混合")
					}
					part.Default = true
					continue
				}
				tuple := []sql.NullString{raw}
				if len(spec.Keys) > 1 {
					tuple, e = pgNativeArray(raw)
					if e != nil {
						return nil, e
					}
				}
				if len(tuple) != len(spec.Keys) {
					return nil, fmt.Errorf("原生 LIST 元组数量与分区键不同")
				}
				var converted []PartitionValue
				for j, value := range tuple {
					v, e := pgNativeValue(value, spec.Keys[j], false)
					if e != nil {
						catalogPartitionUnsupported(spec, e.Error())
					}
					converted = append(converted, v)
				}
				part.Values = append(part.Values, converted)
			}
		case "HASH":
			if len(values) != 1 || !values[0].Valid {
				return nil, fmt.Errorf("原生 HASH 缺少完整桶身份")
			}
			remainder, e := strconv.Atoi(values[0].String)
			if e != nil || remainder < 0 || remainder >= len(nodes) {
				return nil, fmt.Errorf("原生 HASH 桶编号不能精确表达")
			}
			part.HashModulus, part.HashRemainder = len(nodes), remainder
		}
		if nested := children[node.oid]; len(nested) > 0 {
			subroot := node
			subroot.strategy = nested[0].strategy
			for _, child := range nested {
				if child.strategy != subroot.strategy {
					return nil, fmt.Errorf("原生子分区方法不一致")
				}
			}
			part.Subpartition, e = r.buildPGNativePartitionSpec(subroot, node.oid, schema, children, columns, visiting)
			if e != nil {
				return nil, e
			}
		}
		spec.Partitions = append(spec.Partitions, part)
	}
	if method == "RANGE" && spec.Unsupported == "" {
		var compareErr error
		sort.SliceStable(spec.Partitions, func(i, j int) bool {
			comparison, e := pgNativeCompareTuple(spec.Partitions[i].Upper, spec.Partitions[j].Upper, spec.Keys)
			if e != nil {
				compareErr = e
			}
			return comparison < 0
		})
		if compareErr != nil {
			catalogPartitionUnsupported(spec, compareErr.Error())
		} else {
			lower := make([]PartitionValue, len(spec.Keys))
			for i := range lower {
				lower[i] = PartitionValue{Kind: "minvalue"}
			}
			for i := range spec.Partitions {
				spec.Partitions[i].Ordinal = i + 1
				spec.Partitions[i].Lower = append([]PartitionValue(nil), lower...)
				lower = spec.Partitions[i].Upper
			}
		}
	}
	return spec, nil
}

func pgNativeArray(raw sql.NullString) ([]sql.NullString, error) {
	if !raw.Valid {
		return nil, nil
	}
	var values []sql.NullString
	if err := pq.Array(&values).Scan(raw.String); err != nil {
		return nil, fmt.Errorf("原生分区 text[] 边界不可完整解析: %w", err)
	}
	return values, nil
}

func pgNativeValue(raw sql.NullString, key PartitionKey, rangeBound bool) (PartitionValue, error) {
	if !raw.Valid {
		kind := "null"
		if rangeBound {
			kind = "maxvalue"
		}
		return PartitionValue{Kind: kind}, nil
	}
	typ := strings.ToLower(key.DataType)
	switch {
	case typ == "int2" || typ == "smallint" || typ == "int4" || typ == "integer" || typ == "int8" || typ == "bigint" || strings.HasPrefix(typ, "numeric") || strings.HasPrefix(typ, "decimal"):
		if !catalogPartitionNumber.MatchString(raw.String) {
			return PartitionValue{}, fmt.Errorf("原生数值分区边界不是精确常量")
		}
		return PartitionValue{Kind: "number", Text: raw.String}, nil
	case typ == "date":
		if _, err := time.Parse("2006-01-02", raw.String); err != nil {
			return PartitionValue{}, fmt.Errorf("原生日期边界不是可证明的 ISO 日期")
		}
		return PartitionValue{Kind: "date", Text: raw.String}, nil
	case strings.HasPrefix(typ, "timestamp"):
		if _, err := pgNativeTime(raw.String, strings.Contains(typ, "with time zone")); err != nil {
			return PartitionValue{}, err
		}
		return PartitionValue{Kind: "timestamp", Text: raw.String}, nil
	case strings.Contains(typ, "char") || typ == "text":
		return PartitionValue{Kind: "string", Text: raw.String}, nil
	default:
		return PartitionValue{}, fmt.Errorf("原生分区边界类型 %s 尚无精确常量适配", key.DataType)
	}
}

func pgNativeTime(raw string, zoned bool) (time.Time, error) {
	layouts := []string{"2006-01-02 15:04:05.999999999"}
	if zoned {
		layouts = []string{"2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999Z07"}
	}
	for _, layout := range layouts {
		if value, err := time.Parse(layout, raw); err == nil {
			return value, nil
		}
	}
	return time.Time{}, fmt.Errorf("原生时间分区边界不是可证明的 ISO 时间")
}

func pgNativeCompareTuple(left, right []PartitionValue, keys []PartitionKey) (int, error) {
	for i, a := range left {
		b := right[i]
		if a.Kind == "maxvalue" || b.Kind == "maxvalue" {
			if a.Kind == b.Kind {
				continue
			}
			if a.Kind == "maxvalue" {
				return 1, nil
			}
			return -1, nil
		}
		if a.Kind != b.Kind {
			return 0, fmt.Errorf("原生 RANGE 边界类型不一致")
		}
		comparison := 0
		switch a.Kind {
		case "number":
			x, ok := new(big.Rat).SetString(a.Text)
			if !ok {
				return 0, fmt.Errorf("原生数值边界不能精确排序")
			}
			y, ok := new(big.Rat).SetString(b.Text)
			if !ok {
				return 0, fmt.Errorf("原生数值边界不能精确排序")
			}
			comparison = x.Cmp(y)
		case "date":
			comparison = strings.Compare(a.Text, b.Text)
		case "timestamp":
			zoned := strings.Contains(strings.ToLower(keys[i].DataType), "with time zone")
			x, e := pgNativeTime(a.Text, zoned)
			if e != nil {
				return 0, e
			}
			y, e := pgNativeTime(b.Text, zoned)
			if e != nil {
				return 0, e
			}
			if x.Before(y) {
				comparison = -1
			} else if x.After(y) {
				comparison = 1
			}
		case "string":
			if keys[i].Collation != "C" && keys[i].Collation != "POSIX" {
				return 0, fmt.Errorf("原生文本 RANGE 的排序规则不能按字节顺序推断")
			}
			comparison = strings.Compare(a.Text, b.Text)
		default:
			return 0, fmt.Errorf("原生 RANGE 边界不能完整排序")
		}
		if comparison != 0 {
			return comparison, nil
		}
	}
	return 0, nil
}

func (r *PostgresReader) pgNativePartitionIndexes(ctx context.Context, table string) ([]IndexInfo, error) {
	locality := "'unknown'"
	if r.profile.Fields["pg_index.indisglobal"] {
		locality = "CASE WHEN x.indisglobal THEN 'global' ELSE 'local' END"
	}
	query := `SELECT i.relname,x.indisunique,x.indisprimary,x.indkey::text,` + locality + ` FROM ` + r.qcat("pg_class") + ` t JOIN ` + r.qcat("pg_namespace") + ` n ON n.oid=t.relnamespace JOIN ` + r.qcat("pg_index") + ` x ON x.indrelid=t.oid JOIN ` + r.qcat("pg_class") + ` i ON i.oid=x.indexrelid WHERE n.nspname=$1 AND t.relname=$2 ORDER BY i.relname`
	rows, err := r.db.QueryContext(ctx, query, r.schema, table)
	if err != nil {
		return nil, err
	}
	var indexes []IndexInfo
	var numbers [][]int
	for rows.Next() {
		var index IndexInfo
		var keys string
		if err = rows.Scan(&index.IndexName, &index.IsUnique, &index.IsPrimary, &keys, &index.Locality); err != nil {
			break
		}
		index.TableName = table
		keyNumbers, e := ParsePGNumbers(keys)
		if e != nil {
			err = e
			break
		}
		indexes = append(indexes, index)
		numbers = append(numbers, keyNumbers)
	}
	if err == nil {
		err = rows.Err()
	}
	closeErr := rows.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	query = `SELECT a.attnum,a.attname FROM ` + r.qcat("pg_attribute") + ` a JOIN ` + r.qcat("pg_class") + ` t ON t.oid=a.attrelid JOIN ` + r.qcat("pg_namespace") + ` n ON n.oid=t.relnamespace WHERE n.nspname=$1 AND t.relname=$2 AND a.attnum>0 AND NOT a.attisdropped`
	rows, err = r.db.QueryContext(ctx, query, r.schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := map[int]string{}
	for rows.Next() {
		var number int
		var name string
		if err := rows.Scan(&number, &name); err != nil {
			return nil, err
		}
		columns[number] = name
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range indexes {
		for _, number := range numbers[i] {
			column, known := columns[number]
			if !known {
				indexes[i].Unsupported = "原生分区索引含表达式或缺失键列"
				continue
			}
			indexes[i].Columns = append(indexes[i].Columns, column)
		}
	}
	return indexes, nil
}
