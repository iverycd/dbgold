package target

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"github.com/lib/pq"
)

// GaussDBIndexPlanner is optional; ordinary Writers retain their contract.
type GaussDBIndexPlanner interface {
	PlanGaussDBIndex(context.Context, source.IndexInfo) (source.IndexInfo, error)
}

type gaussDBIndexPreflightError struct{ error }

func (e gaussDBIndexPreflightError) Unwrap() error { return e.error }
func IsGaussDBIndexPreflight(err error) bool {
	var e gaussDBIndexPreflightError
	return errors.As(err, &e)
}

// GaussDBHashType deliberately excludes extension, binary, float and expression
// domains. These known built-in domains cover ordinary MySQL keys without probes
// that create temporary target objects.
func GaussDBHashType(t string) bool {
	t = strings.ToLower(strings.TrimSpace(t))
	if i := strings.IndexByte(t, '('); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	switch t {
	case "tinyint", "smallint", "integer", "int", "bigint", "int2", "int4", "int8", "numeric", "decimal", "char", "character", "varchar", "character varying", "nvarchar2", "text", "date", "timestamp", "timestamp without time zone", "timestamp with time zone", "timestamptz", "time", "time without time zone", "interval", "boolean", "bool", "uuid":
		return true
	}
	return false
}

const gaussDistributionSQL = `SELECT c.oid,x.pclocatortype,COALESCE(x.pcattnum::text,''),c.relkind,c.relpersistence,c.parttype,COALESCE(c.reloptions,ARRAY[]::text[])
FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
LEFT JOIN pg_catalog.pgxc_class x ON x.pcrelid=c.oid WHERE n.nspname=$1 AND c.relname=$2`
const gaussColumnsSQL = `SELECT a.attnum,a.attname,pg_catalog.format_type(a.atttypid,a.atttypmod) FROM pg_catalog.pg_attribute a WHERE a.attrelid=$1 AND a.attnum>0 AND NOT a.attisdropped ORDER BY a.attnum`

func (w *GaussDBWriter) InspectGaussDBDistribution(ctx context.Context, table string) (source.GaussDBDistribution, error) {
	schema := w.schema
	if schema == "" {
		if err := w.db.QueryRowContext(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
			return source.GaussDBDistribution{}, err
		}
	}
	b := source.GaussDBDistribution{Schema: schema, Table: table, Types: map[string]string{}}
	var mode sql.NullString
	var numbers string
	if err := w.db.QueryRowContext(ctx, gaussDistributionSQL, schema, table).Scan(&b.OID, &mode, &numbers, &b.Kind, &b.Persistence, &b.Partition, pq.Array(&b.Options)); err != nil {
		return b, fmt.Errorf("读取 GaussDB 表分布 %s.%s: %w", schema, table, err)
	}
	b.Mode = mode.String
	rows, err := w.db.QueryContext(ctx, gaussColumnsSQL, b.OID)
	if err != nil {
		return b, err
	}
	defer rows.Close()
	names := map[int]string{}
	for rows.Next() {
		var n int
		var name, typ string
		if err = rows.Scan(&n, &name, &typ); err != nil {
			return b, err
		}
		names[n], b.Types[name] = name, typ
	}
	if err = rows.Err(); err != nil {
		return b, err
	}
	for _, value := range strings.Fields(numbers) {
		n, e := strconv.Atoi(value)
		if e != nil || names[n] == "" {
			return b, fmt.Errorf("分布列元数据无效: %q", numbers)
		}
		b.Columns = append(b.Columns, names[n])
	}
	slices.Sort(b.Options)
	return b, nil
}

var gauss507 = regexp.MustCompile(`(?i)GaussDB Kernel 507\.[0-9]+\.[0-9]+\b`)

func (w *GaussDBWriter) PlanGaussDBIndex(ctx context.Context, idx source.IndexInfo) (source.IndexInfo, error) {
	b, err := w.InspectGaussDBDistribution(ctx, idx.TableName)
	if err != nil {
		return idx, err
	}
	p := &source.GaussDBIndexPlan{Base: b}
	idx.GaussDB = p
	if !idx.IsPrimary && !idx.IsUnique {
		return idx, fmt.Errorf("仅主键和唯一索引需要 GaussDB 分布计划")
	}
	if b.Kind != "r" || b.Partition != "n" || b.Persistence != "p" {
		return idx, fmt.Errorf("GaussDB 分布索引目前仅适配普通永久非分区表")
	}
	if len(idx.Columns) == 0 {
		return idx, fmt.Errorf("索引缺少完整键列")
	}
	for _, c := range idx.Columns {
		if b.Types[c] == "" {
			return idx, fmt.Errorf("目标表缺少索引列 %s", c)
		}
	}
	contains := len(b.Columns) > 0
	for _, c := range b.Columns {
		contains = contains && slices.Contains(idx.Columns, c)
	}
	if b.Mode == "R" || b.Mode == "" || contains {
		return idx, nil
	}
	if b.Mode != "H" {
		return idx, fmt.Errorf("不含分布列的唯一键仅适配 HASH 表 GSI，当前分布方式 %s", b.Mode)
	}
	if len(b.Columns) == 0 {
		return idx, fmt.Errorf("HASH 表缺少分布列元数据")
	}
	p.Global = true
	if idx.Unsupported != "" {
		return idx, fmt.Errorf("%s", idx.Unsupported)
	}
	if idx.MySQLExpression != "" || len(idx.MySQLPrefixLengths) > 0 || idx.Definition != "" || idx.Method != "" && !strings.EqualFold(idx.Method, "btree") && !strings.EqualFold(idx.Method, "ubtree") {
		return idx, fmt.Errorf("GSI 自动适配仅支持完整列 B-tree 唯一键，不改写前缀或表达式索引")
	}
	for _, option := range b.Options {
		if strings.HasPrefix(option, "orientation=") && option != "orientation=row" {
			return idx, fmt.Errorf("GSI 仅适配行存表")
		}
	}
	// CREATE GLOBAL INDEX fixes a distribution key. BY GLOBAL INDEX has no
	// distribution clause: validate that a key is possible, then let the kernel
	// choose and verify its actual distribution against the constraint key set.
	for _, c := range idx.Columns {
		if GaussDBHashType(b.Types[c]) {
			p.Columns = []string{c}
			break
		}
	}
	if len(p.Columns) == 0 {
		return idx, fmt.Errorf("GSI 键中没有可确认的 HASH 分布列")
	}
	if idx.IsPrimary || idx.PGUniqueConstraint {
		p.Columns = nil
	}
	if err = w.db.QueryRowContext(ctx, "SELECT version()").Scan(&p.Version); err != nil {
		return idx, err
	}
	if !gauss507.MatchString(p.Version) {
		return idx, fmt.Errorf("GSI 自动适配尚未验证此版本: %s；目前支持 GaussDB Kernel 507.x", p.Version)
	}
	if err = w.db.QueryRowContext(ctx, "SHOW enable_gtm_free").Scan(&p.GTMFree); err != nil {
		return idx, fmt.Errorf("读取 GTM 模式: %w", err)
	}
	if err = w.db.QueryRowContext(ctx, "SHOW gtm_option").Scan(&p.GTMOption); err != nil {
		return idx, fmt.Errorf("读取 GTM 模式: %w", err)
	}
	// 507 treats the historical option 0 as GTM-LITE as well.
	if p.GTMFree != "off" || p.GTMOption != "1" && p.GTMOption != "0" {
		return idx, fmt.Errorf("GSI 要求 GTM-LITE，当前 enable_gtm_free=%s, gtm_option=%s", p.GTMFree, p.GTMOption)
	}
	return idx, nil
}

func (w *GaussDBWriter) validateGaussDBPlan(ctx context.Context, idx source.IndexInfo) error {
	if idx.GaussDB == nil {
		return fmt.Errorf("缺少分布索引计划，请重新生成索引计划")
	}
	current, err := w.PlanGaussDBIndex(ctx, idx)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current.GaussDB, idx.GaussDB) {
		return fmt.Errorf("目标分布或 GSI 能力已变化，请重新生成索引计划")
	}
	return nil
}

func ValidateGaussDBIndexRepair(obj repair.Object, units []repair.Unit, dbType string) error {
	if dbType != "gaussdb" || obj.Index == nil || obj.Index.GaussDB == nil {
		return fmt.Errorf("缺少 GaussDB 索引计划")
	}
	idx := obj.Index
	if idx.TableName != obj.Table || idx.GaussDB.Base.Schema != obj.Schema || idx.GaussDB.Base.Table != obj.Table || dialect.IndexName(dialect.NewPostgres("gaussdb"), *idx) != obj.TargetName {
		return fmt.Errorf("GaussDB 索引计划作用域不匹配")
	}
	expected := dialect.NewPostgres("gaussdb").IndexStatements(obj.Schema, *idx)
	if len(units) != len(expected) {
		return fmt.Errorf("请保留完整 GaussDB 索引计划")
	}
	for i, u := range units {
		if u.SQL != expected[i].SQL || u.OriginalSQL != expected[i].SQL {
			return fmt.Errorf("GaussDB 索引计划已编辑，请重新生成并保存计划")
		}
	}
	return nil
}

// No IF NOT EXISTS: even a duplicate must be proved on the intended base table.
func (w *GaussDBWriter) createGaussDBIndex(ctx context.Context, idx source.IndexInfo) error {
	if err := w.validateGaussDBPlan(ctx, idx); err != nil {
		return gaussDBIndexPreflightError{err}
	}
	query := dialect.JoinSQL(w.dia.IndexStatements(idx.GaussDB.Base.Schema, idx))
	var verifyErr error
	_, err := execObservedAllow(ctx, w.db, query, func(e error) bool {
		code := compatibilitySQLState(e)
		if code != "42P07" && code != "42710" && code != "42P16" {
			return false
		}
		verifyErr = w.verifyGaussDBIndex(ctx, idx)
		return verifyErr == nil
	})
	if err != nil {
		return errors.Join(err, verifyErr)
	}
	return w.verifyGaussDBIndex(ctx, idx)
}

const gaussIndexSQL = `SELECT i.oid,i.relkind,am.amname,ix.indisunique,ix.indisprimary,ix.indisvalid,ix.indisready,ix.indpred IS NOT NULL OR ix.indexprs IS NOT NULL,ix.indkey::text,ix.indoption::text,ix.indnatts,ix.indnkeyatts
FROM pg_catalog.pg_index ix JOIN pg_catalog.pg_class t ON t.oid=ix.indrelid
JOIN pg_catalog.pg_namespace ns ON ns.oid=t.relnamespace JOIN pg_catalog.pg_class i ON i.oid=ix.indexrelid
JOIN pg_catalog.pg_am am ON am.oid=i.relam
WHERE ns.nspname=$1 AND t.relname=$2 AND (($3 AND ix.indisprimary) OR (NOT $3 AND i.relname=$4))`

func (w *GaussDBWriter) verifyGaussDBIndex(ctx context.Context, idx source.IndexInfo) error {
	var oid uint32
	var kind, method, keys, options string
	var totalColumns, keyColumns int
	var unique, primary, valid, ready, special bool
	p := idx.GaussDB
	err := w.db.QueryRowContext(ctx, gaussIndexSQL, p.Base.Schema, idx.TableName, idx.IsPrimary, dialect.IndexName(w.dia, idx)).Scan(&oid, &kind, &method, &unique, &primary, &valid, &ready, &special, &keys, &options, &totalColumns, &keyColumns)
	if err != nil {
		return fmt.Errorf("核验目标索引: %w", err)
	}
	if !valid || !ready || special || unique != (idx.IsPrimary || idx.IsUnique) || primary != idx.IsPrimary || (kind == "G") != p.Global || method != "btree" && method != "ubtree" {
		return fmt.Errorf("目标索引有效性、唯一性、主键属性、方法或 GSI 类型不匹配")
	}
	if idx.PGUniqueConstraint {
		var constraint bool
		err = w.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_constraint WHERE conrelid=$1 AND conindid=$2 AND conname=$3 AND contype='u' AND convalidated AND NOT condeferrable AND NOT condeferred)`, p.Base.OID, oid, dialect.IndexName(w.dia, idx)).Scan(&constraint)
		if err != nil {
			return err
		}
		if !constraint {
			return fmt.Errorf("同名索引不是计划中的唯一约束")
		}
	}
	rows, err := w.db.QueryContext(ctx, gaussColumnsSQL, p.Base.OID)
	if err != nil {
		return err
	}
	names := map[string]string{}
	for rows.Next() {
		var num, name, typ string
		if err = rows.Scan(&num, &name, &typ); err != nil {
			rows.Close()
			return err
		}
		names[num] = name
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	actual := strings.Fields(keys)
	dirs := strings.Fields(options)
	if keyColumns <= 0 || keyColumns > totalColumns || len(actual) != totalColumns || len(dirs) != keyColumns {
		return fmt.Errorf("目标索引目录元数据异常: 预期键列=%d, indnatts=%d, indnkeyatts=%d, indkey列数=%d, indoption列数=%d", len(idx.Columns), totalColumns, keyColumns, len(actual), len(dirs))
	}
	if keyColumns != len(idx.Columns) {
		return fmt.Errorf("目标索引键数量不匹配: 预期=%d, indnkeyatts=%d, indnatts=%d", len(idx.Columns), keyColumns, totalColumns)
	}
	// indkey includes GSI system attributes and other non-key columns. Only
	// the first indnkeyatts entries define the keys; indoption covers those keys.
	for i, key := range actual[:keyColumns] {
		n, e := strconv.Atoi(dirs[i])
		if e != nil || names[key] != idx.Columns[i] || (n&1 != 0) != (i < len(idx.Descending) && idx.Descending[i]) {
			return fmt.Errorf("目标索引键列或顺序不匹配")
		}
	}
	if p.Global {
		// GSI has its own attributes and PGXC_CLASS distribution record.
		var indexName string
		if err = w.db.QueryRowContext(ctx, `SELECT relname FROM pg_catalog.pg_class WHERE oid=$1`, oid).Scan(&indexName); err != nil {
			return err
		}
		copy := *w
		copy.schema = p.Base.Schema
		actual, e := copy.InspectGaussDBDistribution(ctx, indexName)
		if e != nil {
			return e
		}
		matches := slices.Equal(actual.Columns, p.Columns)
		if idx.IsPrimary || idx.PGUniqueConstraint {
			matches = len(actual.Columns) > 0
			for _, column := range actual.Columns {
				matches = matches && slices.Contains(idx.Columns, column)
			}
		}
		if actual.Mode != "H" || !matches || slices.Equal(actual.Columns, p.Base.Columns) {
			return fmt.Errorf("目标 GSI 分布列不匹配")
		}
	}
	return nil
}
