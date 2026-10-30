package target

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/partition"
	"dbgold/datamigrate/source"
	"github.com/lib/pq"
)

// PartitionInspector is optional: ordinary tables keep the established Writer
// contract. Every implementation is read-only and must return catalog errors.
type PartitionInspector interface {
	InspectPartitionProfile(context.Context) (partition.Profile, error)
	InspectPartitionTable(context.Context, string) (*source.PartitionSpec, error)
	CountPartitionRows(context.Context, string, *source.PartitionSpec) (map[string]int64, error)
}

type PartitionNameValidator interface {
	ValidatePartitionNames(context.Context, string, *source.PartitionSpec) error
}

type dbPartitionInspector struct {
	db              *sql.DB
	product, schema string
}

func (i dbPartitionInspector) resolved(ctx context.Context) (dbPartitionInspector, error) {
	if i.schema != "" {
		return i, nil
	}
	query := ""
	switch {
	case source.IsPGFamily(i.product):
		query = `SELECT current_schema()`
	case i.product == "mysql":
		query = `SELECT DATABASE()`
	case i.product == "oracle":
		query = `SELECT SYS_CONTEXT('USERENV','CURRENT_SCHEMA') FROM DUAL`
	case i.product == "dameng":
		query = `SELECT CURRENT_SCHEMA`
	case i.product == "sqlserver":
		query = `SELECT SCHEMA_NAME()`
	default:
		return i, fmt.Errorf("%s 的默认分区 Schema 适配尚未实现", i.product)
	}
	var schema sql.NullString
	if err := i.db.QueryRowContext(ctx, query).Scan(&schema); err != nil {
		return i, err
	}
	if !schema.Valid || schema.String == "" {
		return i, fmt.Errorf("无法确认目标有效 Schema")
	}
	i.schema = schema.String
	return i, nil
}

func (i dbPartitionInspector) profile(ctx context.Context) (partition.Profile, error) {
	p := partition.Profile{Product: i.product, Features: map[string]bool{}}
	if i.db == nil {
		return p, fmt.Errorf("目标分区检查缺少数据库连接")
	}
	resolved, err := i.resolved(ctx)
	if err != nil {
		return p, err
	}
	i = resolved
	p.Schema = i.schema
	if source.IsPGFamily(i.product) {
		return i.pgProfile(ctx, p)
	}
	switch i.product {
	case "mysql":
		var versionComment string
		if err := i.db.QueryRowContext(ctx, `SELECT VERSION(),@@SESSION.sql_mode,@@version_comment`).Scan(&p.Version, &p.Mode, &versionComment); err != nil {
			return p, err
		}
		if _, err := dialect.ParseMySQLVersion(p.Version); err != nil {
			return p, fmt.Errorf("目标 MySQL 分区版本尚未验证: %w", err)
		}
		_, p.Features["mysql_key_innodb"] = source.MySQLPartitionKeyVersion(p.Version, versionComment)
		p.Features["inline_mysql"] = true
		p.Features[partition.FeatureSubpartition] = true
		p.Features["row_movement"] = true
		p.Features[partition.FeatureRange] = true
		p.Features[partition.FeatureList] = true
		p.Features[partition.FeatureHash] = true
		p.Features[partition.FeatureMultiRange] = true
		p.Features[partition.FeatureMultiList] = true
		p.Features["multi_column_hash"] = false
		p.Features[partition.FeatureExpressions] = true
	case "oracle":
		if err := i.db.QueryRowContext(ctx, `SELECT BANNER FROM V$VERSION WHERE BANNER LIKE 'Oracle%' AND ROWNUM=1`).Scan(&p.Version); err != nil {
			return p, err
		}
		if err := i.db.QueryRowContext(ctx, `SELECT VALUE FROM V$PARAMETER WHERE NAME='compatible'`).Scan(&p.Mode); err != nil {
			return p, err
		}
		major := partitionVersionMajor(p.Mode)
		if major < 11 {
			return p, fmt.Errorf("Oracle 分区版本 %s 尚未实现/验证，基线为 11g 以上", p.Version)
		}
		var available string
		if err := i.db.QueryRowContext(ctx, `SELECT VALUE FROM V$OPTION WHERE PARAMETER='Partitioning'`).Scan(&available); err != nil {
			return p, err
		}
		if !strings.EqualFold(available, "TRUE") {
			return p, fmt.Errorf("目标实例未启用 Oracle 原生 Partitioning 能力")
		}
		p.Features["inline_oracle"] = true
		p.Features[partition.FeatureSubpartition] = true
		p.Features["row_movement_clause"] = true
		p.Features[partition.FeatureRange] = true
		p.Features[partition.FeatureList] = true
		p.Features[partition.FeatureHash] = true
		p.Features[partition.FeatureDefaultList] = true
		p.Features[partition.FeatureMultiRange] = true
		p.Features["multi_column_hash"] = true
	case "dameng":
		if err := i.db.QueryRowContext(ctx, `SELECT SVR_VERSION FROM V$INSTANCE`).Scan(&p.Version); err != nil {
			return p, err
		}
		if err := i.db.QueryRowContext(ctx, `SELECT PARA_VALUE FROM V$DM_INI WHERE PARA_NAME='COMPATIBLE_MODE'`).Scan(&p.Mode); err != nil {
			return p, err
		}
		if !strings.Contains(strings.ToUpper(p.Version), "V8") && partitionVersionMajor(p.Version) != 8 {
			return p, fmt.Errorf("达梦分区构建 %s 尚未实现/验证，基线为 DM8", p.Version)
		}
		rows, err := i.db.QueryContext(ctx, `SELECT PARTITION_NAME,HIGH_VALUE,PARTITION_POSITION FROM ALL_TAB_PARTITIONS WHERE 1=0`)
		if err != nil {
			return p, fmt.Errorf("达梦原生分区目录不可读: %w", err)
		}
		if err = rows.Close(); err != nil {
			return p, err
		}
		p.Features["inline_oracle"] = true
		// MPP does not support INTERVAL. A missing/unreadable runtime flag is
		// not evidence of standalone automatic-partition semantics.
		var mpp sql.NullString
		if err := i.db.QueryRowContext(ctx, `SELECT PARA_VALUE FROM V$DM_INI WHERE PARA_NAME='MPP_INI'`).Scan(&mpp); err == nil && mpp.Valid && strings.TrimSpace(mpp.String) == "0" {
			p.Features[partition.FeatureInterval] = true
		}
		p.Features[partition.FeatureSubpartition] = true
		p.Features["row_movement_clause"] = true
		p.Features[partition.FeatureRange] = true
		p.Features[partition.FeatureList] = true
		p.Features[partition.FeatureHash] = true
		p.Features[partition.FeatureDefaultList] = true
		p.Features[partition.FeatureMultiRange] = true
		p.Features["multi_column_hash"] = true
	case "sqlserver":
		var group string
		if err := i.db.QueryRowContext(ctx, `SELECT CAST(SERVERPROPERTY('ProductVersion') AS NVARCHAR(128))`).Scan(&p.Version); err != nil {
			return p, err
		}
		parts := strings.Split(p.Version, ".")
		major := partitionVersionMajor(p.Version)
		minor := 0
		if len(parts) > 1 {
			minor, _ = strconv.Atoi(parts[1])
		}
		if major < 10 || major == 10 && minor < 50 {
			return p, fmt.Errorf("SQL Server 分区构建 %s 尚未实现/验证，基线为 2008 R2 以上", p.Version)
		}
		if err := i.db.QueryRowContext(ctx, `SELECT name FROM sys.filegroups WHERE is_default=1`).Scan(&group); err != nil {
			return p, err
		}
		p.Mode = "filegroup:" + group
		p.Features["native_sqlserver"] = true
		p.Features["row_movement"] = true
		p.Features[partition.FeatureRange] = true
	default:
		return p, fmt.Errorf("目标 %s 的分区能力适配尚未实现/验证", i.product)
	}
	return p, nil
}

var partitionVersionRE = regexp.MustCompile(`(?:^|[^0-9])([0-9]+)(?:\.[0-9]+)`)

func partitionVersionMajor(version string) int {
	m := partitionVersionRE.FindStringSubmatch(version)
	if len(m) != 2 {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func (i dbPartitionInspector) pgProfile(ctx context.Context, p partition.Profile) (partition.Profile, error) {
	var versionNumber string
	if err := i.db.QueryRowContext(ctx, `SELECT version(),current_setting('server_version_num')`).Scan(&p.Version, &versionNumber); err != nil {
		return p, err
	}
	v, _ := strconv.Atoi(versionNumber)
	if i.product == "postgres" && (v < 100000 || !strings.Contains(strings.ToLower(p.Version), "postgresql")) {
		return p, fmt.Errorf("目标 PostgreSQL 分区版本/产品身份尚未验证: %s", p.Version)
	}
	for _, catalog := range []string{"pg_catalog", "sys_catalog"} {
		rows, err := i.db.QueryContext(ctx, `SELECT c.relname,a.attname FROM `+partitionQuotePG(catalog)+`.pg_attribute a JOIN `+partitionQuotePG(catalog)+`.pg_class c ON c.oid=a.attrelid JOIN `+partitionQuotePG(catalog)+`.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname IN ('pg_class','pg_partitioned_table','pg_partition') AND a.attnum>0 AND NOT a.attisdropped`, catalog)
		if err != nil {
			if catalog == "sys_catalog" {
				return p, fmt.Errorf("目标产品 %s 的分区目录无法读取: %w", i.product, err)
			}
			continue
		}
		fields := map[string]bool{}
		for rows.Next() {
			var table, column string
			if err = rows.Scan(&table, &column); err != nil {
				break
			}
			fields[table+"."+column] = true
		}
		if err == nil {
			err = rows.Err()
		}
		closeErr := rows.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return p, err
		}
		if fields["pg_partitioned_table.partstrat"] && fields["pg_partitioned_table.partattrs"] && fields["pg_class.relpartbound"] {
			p.Mode = "declarative:" + catalog
			p.Features["postgres_declarative"] = true
			p.Features["row_movement"] = v >= 110000
			p.Features[partition.FeatureRange] = true
			p.Features[partition.FeatureList] = true
			p.Features[partition.FeatureHash] = v >= 110000
			p.Features[partition.FeatureDefaultRange] = v >= 110000
			p.Features[partition.FeatureDefaultList] = v >= 110000
			p.Features[partition.FeatureMultiRange] = true
			p.Features[partition.FeatureExpressions] = true
			var searchPath pq.StringArray
			p.Features[partition.FeatureExpressions] = false
			if err := i.db.QueryRowContext(ctx, `SELECT current_schemas(true)::text`).Scan(&searchPath); err != nil {
				p.Features[partition.FeatureExpressions] = false
			} else {
				for _, name := range searchPath {
					if strings.HasPrefix(name, "pg_temp_") {
						continue
					}
					p.Features[partition.FeatureExpressions] = name == "pg_catalog"
					break
				}
			}
			p.Features[partition.FeatureSubpartition] = true
			p.Features["multi_column_hash"] = v >= 110000
			return p, nil
		}
		if fields["pg_partition.parentid"] {
			p.Mode = "native:" + catalog
			for _, field := range []string{"parttype", "parentid", "partstrategy", "partkey", "boundaries", "interval", "transit"} {
				if !fields["pg_partition."+field] {
					return p, fmt.Errorf("原生 pg_partition 缺少完整字段 %s，不能启用分区迁移", field)
				}
			}
			if !fields["pg_class.relrowmovement"] {
				return p, fmt.Errorf("原生 pg_partition 缺少可核对的行移动规则")
			}
			p.Features["inline_gauss"] = true
			p.Features["row_movement_clause"] = true
			p.Features[partition.FeatureRange] = true
			p.Features[partition.FeatureList] = true
			p.Features[partition.FeatureMultiRange] = true
			p.Features[partition.FeatureDefaultList] = true
			p.Features[partition.FeatureSubpartition] = true
			return p, nil
		}
	}
	return p, fmt.Errorf("%s %s 没有可验证的声明式分区目录，不能因 PG 协议兼容启用分区 SQL", i.product, p.Version)
}

func partitionQuotePG(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func (i dbPartitionInspector) table(ctx context.Context, table string) (*source.PartitionSpec, error) {
	resolved, err := i.resolved(ctx)
	if err != nil {
		return nil, err
	}
	if source.IsPGFamily(i.product) {
		var version string
		if err := i.db.QueryRowContext(ctx, `SELECT version()`).Scan(&version); err != nil {
			return nil, err
		}
		return source.ReadPartitionMetadata(ctx, resolved.db, resolved.product, resolved.schema, table, version)
	}
	return source.ReadPartitionMetadata(ctx, resolved.db, resolved.product, resolved.schema, table)
}
func (i dbPartitionInspector) counts(ctx context.Context, table string, spec *source.PartitionSpec) (map[string]int64, error) {
	resolved, err := i.resolved(ctx)
	if err != nil {
		return nil, err
	}
	return source.CountPartitionRows(ctx, resolved.db, resolved.product, resolved.schema, table, spec)
}

func (i dbPartitionInspector) names(ctx context.Context, root string, spec *source.PartitionSpec) error {
	if spec == nil {
		return nil
	}
	resolved, e := i.resolved(ctx)
	if e != nil {
		return e
	}
	i = resolved
	profile, err := i.profile(ctx)
	if err != nil {
		return err
	}
	if profile.Features["postgres_declarative"] {
		catalog := strings.TrimPrefix(profile.Mode, "declarative:")
		qcat := partitionQuotePG(catalog)
		var visit func(*source.PartitionSpec) error
		visit = func(current *source.PartitionSpec) error {
			for _, p := range current.Partitions {
				owner := p.Schema
				if owner == "" {
					owner = i.schema
				}
				q := `WITH RECURSIVE owned AS (SELECT c.oid FROM ` + qcat + `.pg_class c JOIN ` + qcat + `.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname=$2 UNION ALL SELECT h.inhrelid FROM ` + qcat + `.pg_inherits h JOIN owned o ON h.inhparent=o.oid) SELECT EXISTS(SELECT 1 FROM ` + qcat + `.pg_class c JOIN ` + qcat + `.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$3 AND c.relname=$4 AND NOT EXISTS(SELECT 1 FROM owned o WHERE o.oid=c.oid))`
				var conflict bool
				if err := i.db.QueryRowContext(ctx, q, i.schema, root, owner, p.Name).Scan(&conflict); err != nil {
					return err
				}
				if conflict {
					return fmt.Errorf("目标分区关系名 %s 已被迁移父表之外的对象占用", partition.LeafKey(owner, p.Name))
				}
				if p.Subpartition != nil {
					if err := visit(p.Subpartition); err != nil {
						return err
					}
				}
			}
			return nil
		}
		return visit(spec)
	}
	if i.product == "sqlserver" {
		fn, scheme := partition.SQLServerHelperNames(i.schema, root)
		var existing int
		q := `SELECT (SELECT COUNT(*) FROM sys.partition_functions WHERE name=@p1)+(SELECT COUNT(*) FROM sys.partition_schemes WHERE name=@p2)`
		if err := i.db.QueryRowContext(ctx, q, fn, scheme).Scan(&existing); err != nil {
			return err
		}
		if existing > 0 {
			return fmt.Errorf("目标数据库已存在分区函数/方案 %s / %s，不能覆盖或占用共享分区对象", fn, scheme)
		}
	}
	// Oracle/DM/MySQL partition labels have table scope. The compiler checks
	// duplicates after case conversion; existing root conflicts use normal DDL.
	return nil
}

func (w *PostgresWriter) partitionInspector() dbPartitionInspector {
	return dbPartitionInspector{w.db, w.DBType(), w.schema}
}
func (w *PostgresWriter) InspectPartitionProfile(ctx context.Context) (partition.Profile, error) {
	return w.partitionInspector().profile(ctx)
}
func (w *PostgresWriter) InspectPartitionTable(ctx context.Context, t string) (*source.PartitionSpec, error) {
	return w.partitionInspector().table(ctx, t)
}
func (w *PostgresWriter) CountPartitionRows(ctx context.Context, t string, s *source.PartitionSpec) (map[string]int64, error) {
	return w.partitionInspector().counts(ctx, t, s)
}
func (w *PostgresWriter) ValidatePartitionNames(ctx context.Context, t string, s *source.PartitionSpec) error {
	return w.partitionInspector().names(ctx, t, s)
}

func (w *GaussDBWriter) partitionInspector() dbPartitionInspector {
	return dbPartitionInspector{w.db, w.DBType(), w.schema}
}
func (w *GaussDBWriter) InspectPartitionProfile(ctx context.Context) (partition.Profile, error) {
	return w.partitionInspector().profile(ctx)
}
func (w *GaussDBWriter) InspectPartitionTable(ctx context.Context, t string) (*source.PartitionSpec, error) {
	return w.partitionInspector().table(ctx, t)
}
func (w *GaussDBWriter) CountPartitionRows(ctx context.Context, t string, s *source.PartitionSpec) (map[string]int64, error) {
	return w.partitionInspector().counts(ctx, t, s)
}
func (w *GaussDBWriter) ValidatePartitionNames(ctx context.Context, t string, s *source.PartitionSpec) error {
	return w.partitionInspector().names(ctx, t, s)
}

func (w *HighGoWriter) partitionInspector() dbPartitionInspector {
	return dbPartitionInspector{w.db, w.DBType(), w.schema}
}
func (w *HighGoWriter) InspectPartitionProfile(ctx context.Context) (partition.Profile, error) {
	return w.partitionInspector().profile(ctx)
}
func (w *HighGoWriter) InspectPartitionTable(ctx context.Context, t string) (*source.PartitionSpec, error) {
	return w.partitionInspector().table(ctx, t)
}
func (w *HighGoWriter) CountPartitionRows(ctx context.Context, t string, s *source.PartitionSpec) (map[string]int64, error) {
	return w.partitionInspector().counts(ctx, t, s)
}
func (w *HighGoWriter) ValidatePartitionNames(ctx context.Context, t string, s *source.PartitionSpec) error {
	return w.partitionInspector().names(ctx, t, s)
}

func (w *KingbaseWriter) partitionInspector() dbPartitionInspector {
	return dbPartitionInspector{w.db, w.DBType(), w.schema}
}
func (w *KingbaseWriter) InspectPartitionProfile(ctx context.Context) (partition.Profile, error) {
	return w.partitionInspector().profile(ctx)
}
func (w *KingbaseWriter) InspectPartitionTable(ctx context.Context, t string) (*source.PartitionSpec, error) {
	return w.partitionInspector().table(ctx, t)
}
func (w *KingbaseWriter) CountPartitionRows(ctx context.Context, t string, s *source.PartitionSpec) (map[string]int64, error) {
	return w.partitionInspector().counts(ctx, t, s)
}
func (w *KingbaseWriter) ValidatePartitionNames(ctx context.Context, t string, s *source.PartitionSpec) error {
	return w.partitionInspector().names(ctx, t, s)
}

func (w *MySQLWriter) partitionInspector() dbPartitionInspector {
	return dbPartitionInspector{w.db, w.DBType(), w.schema}
}
func (w *MySQLWriter) InspectPartitionProfile(ctx context.Context) (partition.Profile, error) {
	return w.partitionInspector().profile(ctx)
}
func (w *MySQLWriter) InspectPartitionTable(ctx context.Context, t string) (*source.PartitionSpec, error) {
	return w.partitionInspector().table(ctx, t)
}
func (w *MySQLWriter) CountPartitionRows(ctx context.Context, t string, s *source.PartitionSpec) (map[string]int64, error) {
	return w.partitionInspector().counts(ctx, t, s)
}
func (w *MySQLWriter) ValidatePartitionNames(ctx context.Context, t string, s *source.PartitionSpec) error {
	return w.partitionInspector().names(ctx, t, s)
}

func (w *OracleWriter) partitionInspector() dbPartitionInspector {
	return dbPartitionInspector{w.db, w.DBType(), w.schema}
}
func (w *OracleWriter) InspectPartitionProfile(ctx context.Context) (partition.Profile, error) {
	return w.partitionInspector().profile(ctx)
}
func (w *OracleWriter) InspectPartitionTable(ctx context.Context, t string) (*source.PartitionSpec, error) {
	return w.partitionInspector().table(ctx, t)
}
func (w *OracleWriter) CountPartitionRows(ctx context.Context, t string, s *source.PartitionSpec) (map[string]int64, error) {
	return w.partitionInspector().counts(ctx, t, s)
}
func (w *OracleWriter) ValidatePartitionNames(ctx context.Context, t string, s *source.PartitionSpec) error {
	return w.partitionInspector().names(ctx, t, s)
}

func (w *DaMengWriter) partitionInspector() dbPartitionInspector {
	return dbPartitionInspector{w.db, w.DBType(), w.schema}
}
func (w *DaMengWriter) InspectPartitionProfile(ctx context.Context) (partition.Profile, error) {
	return w.partitionInspector().profile(ctx)
}
func (w *DaMengWriter) InspectPartitionTable(ctx context.Context, t string) (*source.PartitionSpec, error) {
	return w.partitionInspector().table(ctx, t)
}
func (w *DaMengWriter) CountPartitionRows(ctx context.Context, t string, s *source.PartitionSpec) (map[string]int64, error) {
	return w.partitionInspector().counts(ctx, t, s)
}
func (w *DaMengWriter) ValidatePartitionNames(ctx context.Context, t string, s *source.PartitionSpec) error {
	return w.partitionInspector().names(ctx, t, s)
}

func (w *SQLServerWriter) partitionInspector() dbPartitionInspector {
	return dbPartitionInspector{w.db, w.DBType(), w.schema}
}
func (w *SQLServerWriter) InspectPartitionProfile(ctx context.Context) (partition.Profile, error) {
	return w.partitionInspector().profile(ctx)
}
func (w *SQLServerWriter) InspectPartitionTable(ctx context.Context, t string) (*source.PartitionSpec, error) {
	return w.partitionInspector().table(ctx, t)
}
func (w *SQLServerWriter) CountPartitionRows(ctx context.Context, t string, s *source.PartitionSpec) (map[string]int64, error) {
	return w.partitionInspector().counts(ctx, t, s)
}
func (w *SQLServerWriter) ValidatePartitionNames(ctx context.Context, t string, s *source.PartitionSpec) error {
	return w.partitionInspector().names(ctx, t, s)
}
