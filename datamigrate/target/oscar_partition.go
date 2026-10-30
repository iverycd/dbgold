package target

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"dbgold/datamigrate/partition"
	"dbgold/datamigrate/source"
)

// These native views and VERSION/CURRENT_SCHEMA are documented in the vendor's
// 2025-01-17 database development manual. Oscar is not a PostgreSQL/Oracle
// catalog alias. In particular ALL_TABLES.ROW_MOVEMENT is only a compatibility
// constant and must not be mistaken for a configurable native table property.
type oscarPartitionBridge interface {
	OpenCursor(context.Context, uint64, string) (uint64, error)
	FetchCursor(context.Context, uint64, int, int) ([][]any, bool, error)
	CloseCursor(context.Context, uint64) error
}

func (w *OscarWriter) partitionQuery(ctx context.Context, query string) (result [][]any, retErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b, ok := w.bridge.(oscarPartitionBridge)
	if !ok || w.session == 0 {
		return nil, fmt.Errorf("神通 JDBC 连接不支持原生分区目录游标")
	}
	id, err := b.OpenCursor(ctx, w.session, query)
	if err != nil {
		return nil, err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		retErr = errors.Join(retErr, b.CloseCursor(closeCtx, id))
	}()
	bytes := 0
	for {
		rows, done, err := b.FetchCursor(ctx, id, 128, 8<<20)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 && !done {
			return nil, fmt.Errorf("神通分区目录游标没有返回数据或完成状态")
		}
		for _, row := range rows {
			for _, cell := range row {
				if s, ok := cell.(string); ok {
					bytes += len(s)
				}
			}
		}
		if len(result)+len(rows) > 65536 || bytes > 16<<20 {
			return nil, fmt.Errorf("神通分区目录超过可完整核验的读取上限")
		}
		result = append(result, rows...)
		if done {
			return result, nil
		}
	}
}

func oscarPartitionString(value any) (string, error) {
	s, ok := value.(string)
	if !ok || strings.ContainsRune(s, '\x00') {
		return "", fmt.Errorf("神通分区目录返回非文本或含 NUL 的标识/定义")
	}
	return s, nil
}

func oscarPartitionInteger(value any) (int, error) {
	var s string
	switch v := value.(type) {
	case int64:
		s = strconv.FormatInt(v, 10)
	case string:
		s = v
	default:
		return 0, fmt.Errorf("神通分区目录返回非精确整数")
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("神通分区目录返回无效计数或对象编号")
	}
	return n, nil
}

func oscarPartitionLiteral(s string) (string, error) {
	if s == "" || strings.ContainsAny(s, "\x00\r\n\\") {
		return "", fmt.Errorf("神通分区目录标识为空或含未验证的字符")
	}
	return "'" + strings.ReplaceAll(s, "'", "''") + "'", nil
}

func (w *OscarWriter) partitionSchema(ctx context.Context) (string, error) {
	if w.schema != "" {
		return w.schema, nil
	}
	rows, err := w.partitionQuery(ctx, `SELECT CURRENT_SCHEMA()`)
	if err != nil {
		return "", err
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		return "", fmt.Errorf("神通默认 Schema 不能唯一确认")
	}
	return oscarPartitionString(rows[0][0])
}

func (w *OscarWriter) InspectPartitionProfile(ctx context.Context) (partition.Profile, error) {
	p := partition.Profile{Product: "oscar", Features: map[string]bool{}}
	rows, err := w.partitionQuery(ctx, `SELECT VERSION(),CURRENT_SCHEMA()`)
	if err != nil {
		return p, fmt.Errorf("读取神通原生版本/Schema 失败: %w", err)
	}
	if len(rows) != 1 || len(rows[0]) != 2 {
		return p, fmt.Errorf("神通原生版本/Schema 结果不完整")
	}
	p.Version, err = oscarPartitionString(rows[0][0])
	if err != nil {
		return p, err
	}
	p.Schema, err = oscarPartitionString(rows[0][1])
	if err != nil {
		return p, err
	}
	if w.schema != "" {
		p.Schema = w.schema
	}
	lower := strings.ToLower(p.Version)
	if p.Schema == "" || (!strings.Contains(lower, "shentong") && !strings.Contains(lower, "oscar") && !strings.Contains(p.Version, "神通")) || strings.Contains(lower, "kstore") || partitionVersionMajor(p.Version) != 7 {
		return p, fmt.Errorf("神通产品/版本 %q 尚未适配原生分区目录；当前基线为神通数据库 7.x（非 KSTORE）", p.Version)
	}
	for _, query := range []string{
		`SELECT p.OID,p.PARTTYPE1,p.PARTTYPE2,p.PARTKEYCOLS1,p.PARTKEYCOLS2,p.PARTCNT,p.INTERVAL_STR FROM V_SYS_PART_TABLES p WHERE 1=0`,
		`SELECT p.OID,p.BO,p.PARTNAME,p.PART,p.PART_KEY_VALUES,p.PARTCNT FROM V_SYS_TAB_PARTITIONS p WHERE 1=0`,
		`SELECT p.BO,p.SUBPARTNAME,p.PART,p.PART_KEY_VALUES FROM V_SYS_TAB_SUBPARTITIONS p WHERE 1=0`,
		`SELECT SCHEMANAME,TABLENAME,SYS_GET_TABLEDEF FROM V_SYS_TABLE WHERE 1=0`,
		`SELECT c.OID,c.RELKIND,c.RELNAME,n.NSPNAME FROM SYS_CLASS c JOIN SYS_NAMESPACE n ON n.OID=c.RELNAMESPACE WHERE 1=0`,
	} {
		if _, err := w.partitionQuery(ctx, query); err != nil {
			return p, fmt.Errorf("神通原生分区目录缺少必要字段或权限，不能核对键、边界和子分区: %w", err)
		}
	}
	p.Mode = "native-oscar"
	p.Features["inline_oracle"] = true // native Oscar uses VALUES/VALUES LESS THAN
	p.Features["row_movement"] = true  // native UPDATE routes by the partition key
	p.Features[partition.FeatureRange] = true
	p.Features[partition.FeatureList] = true
	p.Features[partition.FeatureMultiRange] = true
	p.Features[partition.FeatureDefaultList] = true
	p.Features[partition.FeatureSubpartition] = true
	return p, nil
}

func (w *OscarWriter) InspectPartitionTable(ctx context.Context, table string) (*source.PartitionSpec, error) {
	schema, err := w.partitionSchema(ctx)
	if err != nil {
		return nil, err
	}
	owner, err := oscarPartitionLiteral(schema)
	if err != nil {
		return nil, err
	}
	name, err := oscarPartitionLiteral(table)
	if err != nil {
		return nil, err
	}
	rows, err := w.partitionQuery(ctx, `SELECT c.OID,c.RELKIND FROM SYS_CLASS c JOIN SYS_NAMESPACE n ON n.OID=c.RELNAMESPACE WHERE n.NSPNAME=`+owner+` AND c.RELNAME=`+name)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 || len(rows[0]) != 2 {
		return nil, fmt.Errorf("神通目标表不能在指定 Schema 中唯一确认")
	}
	oid, err := oscarPartitionInteger(rows[0][0])
	if err != nil || oid == 0 {
		return nil, fmt.Errorf("神通目标表缺少有效 OID")
	}
	kind, err := oscarPartitionString(rows[0][1])
	if err != nil {
		return nil, err
	}
	if kind == "r" {
		return nil, nil
	}
	if kind != "f" {
		return nil, fmt.Errorf("神通目标对象不是原生普通表或水平分区表")
	}
	rows, err = w.partitionQuery(ctx, `SELECT PARTTYPE1,PARTTYPE2,PARTKEYCOLS1,PARTKEYCOLS2,PARTCNT,INTERVAL_STR FROM V_SYS_PART_TABLES WHERE OID=`+strconv.Itoa(oid))
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 || len(rows[0]) != 6 {
		return nil, fmt.Errorf("神通目标分区主表目录缺失或不唯一")
	}
	c := source.OscarPartitionCatalog{Schema: schema}
	for i, dst := range []*int{&c.Method, &c.Submethod, &c.KeyCount, &c.SubkeyCount, &c.PartitionCount} {
		*dst, err = oscarPartitionInteger(rows[0][i])
		if err != nil {
			return nil, err
		}
	}
	if rows[0][5] != nil {
		c.Interval, err = oscarPartitionString(rows[0][5])
		if err != nil {
			return nil, err
		}
	}
	rows, err = w.partitionQuery(ctx, `SELECT SYS_GET_TABLEDEF FROM V_SYS_TABLE WHERE SCHEMANAME=`+owner+` AND TABLENAME=`+name)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		return nil, fmt.Errorf("神通目标原生建表 DDL 缺失或不唯一")
	}
	c.DDL, err = oscarPartitionString(rows[0][0])
	if err != nil {
		return nil, err
	}
	rows, err = w.partitionQuery(ctx, `SELECT OID,PARTNAME,PART,PART_KEY_VALUES,PARTCNT FROM V_SYS_TAB_PARTITIONS WHERE BO=`+strconv.Itoa(oid)+` ORDER BY PART`)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if len(row) != 5 {
			return nil, fmt.Errorf("神通一级分区目录字段不完整")
		}
		parent, err := oscarPartitionInteger(row[0])
		if err != nil || parent == 0 {
			return nil, fmt.Errorf("神通一级分区缺少有效 OID")
		}
		part, err := decodeOscarPartitionRow(row[1:4])
		if err != nil {
			return nil, err
		}
		part.SubpartitionCount, err = oscarPartitionInteger(row[4])
		if err != nil {
			return nil, fmt.Errorf("神通一级分区缺少有效子分区数量: %w", err)
		}
		if c.Submethod != 0 {
			subs, err := w.partitionQuery(ctx, `SELECT SUBPARTNAME,PART,PART_KEY_VALUES FROM V_SYS_TAB_SUBPARTITIONS WHERE BO=`+strconv.Itoa(parent)+` ORDER BY PART`)
			if err != nil {
				return nil, err
			}
			for _, sub := range subs {
				leaf, err := decodeOscarPartitionRow(sub)
				if err != nil {
					return nil, err
				}
				part.Subpartitions = append(part.Subpartitions, leaf)
			}
		}
		c.Partitions = append(c.Partitions, part)
	}
	return source.DecodeOscarPartitionCatalog(c)
}

func decodeOscarPartitionRow(row []any) (source.OscarPartitionCatalogRow, error) {
	var p source.OscarPartitionCatalogRow
	if len(row) != 3 {
		return p, fmt.Errorf("神通分区目录字段不完整")
	}
	var err error
	if p.Name, err = oscarPartitionString(row[0]); err != nil {
		return p, err
	}
	if p.Position, err = oscarPartitionInteger(row[1]); err != nil {
		return p, err
	}
	p.Boundary, err = oscarPartitionString(row[2])
	return p, err
}

func (w *OscarWriter) CountPartitionRows(ctx context.Context, table string, spec *source.PartitionSpec) (map[string]int64, error) {
	actual, err := w.InspectPartitionTable(ctx, table)
	if err != nil {
		return nil, err
	}
	if spec == nil || actual == nil {
		return nil, fmt.Errorf("神通叶分区计数缺少完整冻结分区结构")
	}
	if err := partition.Equivalent(spec, actual); err != nil {
		return nil, fmt.Errorf("神通目标分区定义漂移: %w", err)
	}
	schema, err := w.partitionSchema(ctx)
	if err != nil {
		return nil, err
	}
	counts := map[string]int64{}
	var visit func(*source.PartitionSpec, bool) error
	visit = func(s *source.PartitionSpec, nested bool) error {
		for _, p := range s.Partitions {
			if p.Subpartition != nil {
				if err := visit(p.Subpartition, true); err != nil {
					return err
				}
				continue
			}
			kind := "PARTITION"
			if nested {
				kind = "SUBPARTITION"
			}
			query := "SELECT COUNT(*) FROM " + w.dia.QualifyTable(schema, table) + " " + kind + " (" + w.dia.QuoteIdent(p.Name) + ")"
			n, err := w.bridge.QueryInt64(ctx, w.session, query)
			if err != nil {
				return err
			}
			if n < 0 {
				return fmt.Errorf("神通叶分区计数不能为负数")
			}
			counts[partition.LeafKey(p.Schema, p.Name)] = n
		}
		return nil
	}
	if err := visit(actual, false); err != nil {
		return nil, err
	}
	return counts, nil
}

var _ PartitionInspector = (*OscarWriter)(nil)
