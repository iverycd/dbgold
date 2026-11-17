package source

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"dbgold/datamigrate/sqlsyntax"
)

var oscarVersionMajor = regexp.MustCompile(`\b([0-9]+)\.[0-9]+`)

func (r *OscarReader) checkPartitionVersion(ctx context.Context) error {
	rows, err := r.query(ctx, `SELECT VERSION()`)
	if err != nil {
		return err
	}
	version, err := oscarSingleText(rows)
	if err != nil {
		return err
	}
	lower := strings.ToLower(version)
	match := oscarVersionMajor.FindStringSubmatch(version)
	if len(match) != 2 || match[1] != "7" ||
		(!strings.Contains(lower, "shentong") && !strings.Contains(lower, "oscar") && !strings.Contains(version, "神通")) ||
		strings.Contains(lower, "kstore") {
		return fmt.Errorf("源产品或版本 %q 尚未适配神通 7.x 原生分区目录", version)
	}
	return nil
}

func (r *OscarReader) PartitionRoots(ctx context.Context) (map[string]string, error) {
	if err := r.checkPartitionVersion(ctx); err != nil {
		return nil, err
	}
	// A partition label alone cannot be used to suppress a separately named
	// ordinary object. Join both root and physical storage identities by OID.
	rows, err := r.query(ctx, `SELECT physical.RELNAME,root.RELNAME
FROM V_SYS_TAB_PARTITIONS p JOIN SYS_CLASS physical ON physical.OID=p.OID
JOIN SYS_CLASS root ON root.OID=p.BO JOIN SYS_NAMESPACE n ON n.OID=root.RELNAMESPACE
WHERE n.NSPNAME=? AND root.RELKIND='f'
UNION ALL
SELECT physical.RELNAME,root.RELNAME
FROM V_SYS_TAB_SUBPARTITIONS s JOIN V_SYS_TAB_PARTITIONS p ON p.OID=s.BO
JOIN SYS_CLASS physical ON physical.OID=s.OID
JOIN SYS_CLASS root ON root.OID=p.BO JOIN SYS_NAMESPACE n ON n.OID=root.RELNAMESPACE
WHERE n.NSPNAME=? AND root.RELKIND='f'`, r.schema, r.schema)
	if err != nil {
		return nil, fmt.Errorf("读取神通原生物理分区父链: %w", err)
	}
	out := make(map[string]string, 2*len(rows))
	for _, row := range rows {
		if len(row) != 2 {
			return nil, fmt.Errorf("神通物理分区父链字段不完整")
		}
		child, _ := row[0].(string)
		root, _ := row[1].(string)
		if child == "" || root == "" || child == root || out[child] != "" && out[child] != root {
			return nil, fmt.Errorf("神通物理分区父链缺失或不唯一")
		}
		out[child], out[r.schema+"."+child] = root, root
	}
	return out, nil
}

func (r *OscarReader) PartitionMetadata(ctx context.Context, table string) (*PartitionSpec, error) {
	rows, err := r.query(ctx, `SELECT c.OID,c.RELKIND FROM SYS_CLASS c JOIN SYS_NAMESPACE n ON n.OID=c.RELNAMESPACE WHERE n.NSPNAME=? AND c.RELNAME=?`, r.schema, table)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 || len(rows[0]) != 2 {
		return nil, fmt.Errorf("神通表 %s.%s 未在原生目录唯一确认", r.schema, table)
	}
	kind, _ := rows[0][1].(string)
	if kind == "r" {
		return nil, nil
	}
	oid, err := oscarInteger(rows[0][0])
	if err != nil || oid <= 0 || kind != "f" {
		return nil, fmt.Errorf("神通对象 %s.%s 不是原生普通表或水平分区根表", r.schema, table)
	}
	rows, err = r.query(ctx, `SELECT PARTTYPE1,PARTTYPE2,PARTKEYCOLS1,PARTKEYCOLS2,PARTCNT,INTERVAL_STR
FROM V_SYS_PART_TABLES WHERE OID=`+strconv.FormatInt(oid, 10))
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 || len(rows[0]) != 6 {
		return nil, fmt.Errorf("神通分区根表 %s 原生目录缺失或不唯一", table)
	}
	c := OscarPartitionCatalog{Schema: r.schema}
	for i, dest := range []*int{&c.Method, &c.Submethod, &c.KeyCount, &c.SubkeyCount, &c.PartitionCount} {
		n, err := oscarInteger(rows[0][i])
		if err != nil || n < 0 || int64(int(n)) != n {
			return nil, fmt.Errorf("神通分区根表 %s 原生目录计数或方法编号无效", table)
		}
		*dest = int(n)
	}
	if rows[0][5] != nil {
		c.Interval, err = oscarText(rows[0][5])
		if err != nil {
			return nil, err
		}
	}
	c.DDL, err = r.GetTableSourceDDL(ctx, table)
	if err != nil {
		return nil, err
	}
	rows, err = r.query(ctx, `SELECT OID,PARTNAME,PART,PART_KEY_VALUES,PARTCNT FROM V_SYS_TAB_PARTITIONS WHERE BO=`+
		strconv.FormatInt(oid, 10)+` ORDER BY PART`)
	if err != nil {
		return nil, err
	}
	parents := make(map[int64]bool, len(rows))
	for _, row := range rows {
		if len(row) != 5 {
			return nil, fmt.Errorf("神通一级分区目录字段不完整")
		}
		parent, err := oscarInteger(row[0])
		if err != nil || parent <= 0 || parents[parent] {
			return nil, fmt.Errorf("神通一级分区缺少唯一原生 OID")
		}
		parents[parent] = true
		part, err := oscarPartitionRow(row[1:4])
		if err != nil {
			return nil, err
		}
		count, err := oscarInteger(row[4])
		if err != nil || count < 0 || int64(int(count)) != count {
			return nil, fmt.Errorf("神通一级分区缺少有效子分区数量")
		}
		part.SubpartitionCount = int(count)
		if c.Submethod != 0 {
			subs, err := r.query(ctx, `SELECT SUBPARTNAME,PART,PART_KEY_VALUES FROM V_SYS_TAB_SUBPARTITIONS WHERE BO=`+
				strconv.FormatInt(parent, 10)+` ORDER BY PART`)
			if err != nil {
				return nil, err
			}
			for _, sub := range subs {
				leaf, err := oscarPartitionRow(sub)
				if err != nil {
					return nil, err
				}
				part.Subpartitions = append(part.Subpartitions, leaf)
			}
		}
		c.Partitions = append(c.Partitions, part)
	}
	spec, err := DecodeOscarPartitionCatalog(c)
	if err != nil {
		// Keep original CREATE text as report evidence for unrepresentable
		// source shapes. Callers block the table before any target DDL.
		return &PartitionSpec{SourceDDL: c.DDL, Unsupported: err.Error()}, nil
	}
	return spec, nil
}

func oscarPartitionRow(row []any) (OscarPartitionCatalogRow, error) {
	var part OscarPartitionCatalogRow
	if len(row) != 3 {
		return part, fmt.Errorf("神通分区目录字段不完整")
	}
	name, nameErr := oscarText(row[0])
	ordinal, ordinalErr := oscarInteger(row[1])
	bound, boundErr := oscarText(row[2])
	if nameErr != nil || ordinalErr != nil || boundErr != nil || name == "" || ordinal < 0 || int64(int(ordinal)) != ordinal {
		return part, fmt.Errorf("神通分区目录缺少有效名称、序号或边界")
	}
	return OscarPartitionCatalogRow{Name: name, Position: int(ordinal), Boundary: bound}, nil
}

func (r *OscarReader) PartitionHasChecks(ctx context.Context, table string) (bool, error) {
	ddl, err := r.GetTableSourceDDL(ctx, table)
	if err != nil {
		return false, err
	}
	tokens, err := sqlsyntax.Lex(ddl)
	if err != nil {
		return false, err
	}
	for _, token := range tokens {
		if token.Is("CHECK") {
			return true, nil
		}
	}
	return false, nil
}

func (r *OscarReader) StreamPartitionRows(ctx context.Context, table string, batchSize int, consume func([]string, []string, [][]any) error) error {
	return r.StreamOscarTable(ctx, table, batchSize, consume)
}

func (r *OscarReader) partitionFrom(table string, leaf Partition, nested bool) string {
	kind := " PARTITION "
	if nested {
		kind = " SUBPARTITION "
	}
	return r.table(table) + kind + "(" + oscarQuoteIdent(leaf.Name) + ")"
}

func oscarVisitLeaves(spec *PartitionSpec, nested bool, visit func(Partition, bool) error) error {
	for _, p := range spec.Partitions {
		if p.Subpartition != nil {
			if err := oscarVisitLeaves(p.Subpartition, true, visit); err != nil {
				return err
			}
		} else if err := visit(p, nested); err != nil {
			return err
		}
	}
	return nil
}

func (r *OscarReader) CountPartitionRows(ctx context.Context, table string, frozen *PartitionSpec) (map[string]int64, error) {
	actual, err := r.PartitionMetadata(ctx, table)
	if err != nil {
		return nil, err
	}
	if frozen == nil || actual == nil || actual.Unsupported != "" || !reflect.DeepEqual(actual, frozen) {
		return nil, fmt.Errorf("神通源表 %s 的原生分区定义发生变化", table)
	}
	out := make(map[string]int64)
	err = oscarVisitLeaves(actual, false, func(leaf Partition, nested bool) error {
		count, err := r.bridge.QueryInt64(ctx, r.session, "SELECT COUNT(*) FROM "+r.partitionFrom(table, leaf, nested))
		if err != nil {
			return err
		}
		if count < 0 {
			return fmt.Errorf("神通原生分区 %s 返回无效计数", leaf.Name)
		}
		out[leaf.Schema+"."+leaf.Name] = count
		return nil
	})
	return out, err
}

var _ PartitionReader = (*OscarReader)(nil)
var _ PartitionCounter = (*OscarReader)(nil)
