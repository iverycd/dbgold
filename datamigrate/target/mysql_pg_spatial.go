package target

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
)

// PGSpatialCapabilities describes the actual target installation. A missing
// GiST capability does not prevent loading geometry columns.
type PGSpatialCapabilities struct {
	Schema           string `json:"schema"`
	GISTSchema       string `json:"gist_schema,omitempty"`
	GISTOpClass      string `json:"gist_op_class,omitempty"`
	GISTError        string `json:"gist_error,omitempty"`
	ExtensionName    string `json:"extension_name,omitempty"`
	ExtensionVersion string `json:"extension_version,omitempty"`
	ExtensionCreated bool   `json:"extension_created,omitempty"`
}

const pgSpatialTypesSQL = `SELECT n.nspname,EXISTS (SELECT 1 FROM pg_depend d JOIN pg_extension e ON e.oid=d.refobjid WHERE d.classid='pg_type'::regclass AND d.objid=t.oid AND d.refclassid='pg_extension'::regclass AND d.deptype='e' AND e.extname='postgis'),pg_type_is_visible(t.oid) FROM pg_type t JOIN pg_namespace n ON n.oid=t.typnamespace WHERE t.typname='geometry' AND t.typtype='b' ORDER BY n.nspname`
const pgSpatialAvailableSQL = `SELECT default_version,installed_version FROM pg_available_extensions WHERE name=$1`
const pgSpatialGISTSQL = `SELECT n.nspname,o.opcname FROM pg_opclass o JOIN pg_namespace n ON n.oid=o.opcnamespace JOIN pg_am am ON am.oid=o.opcmethod JOIN pg_type t ON t.oid=o.opcintype JOIN pg_namespace tn ON tn.oid=t.typnamespace WHERE am.amname='gist' AND tn.nspname=$1 AND t.typname='geometry' AND o.opcname='gist_geometry_ops_2d' ORDER BY n.nspname`
const pgSpatialColumnSQL = `SELECT tn.nspname,t.typname,a.atttypmod FROM pg_attribute a JOIN pg_class c ON c.oid=a.attrelid JOIN pg_namespace n ON n.oid=c.relnamespace JOIN pg_type t ON t.oid=a.atttypid JOIN pg_namespace tn ON tn.oid=t.typnamespace WHERE n.nspname=$1 AND c.relname=$2 AND a.attname=$3 AND a.attnum>0 AND NOT a.attisdropped`
const pgSpatialChecksSQL = `SELECT pg_get_constraintdef(co.oid) FROM pg_constraint co JOIN pg_class c ON c.oid=co.conrelid JOIN pg_namespace n ON n.oid=c.relnamespace JOIN pg_attribute a ON a.attrelid=c.oid AND a.attname=$3 WHERE n.nspname=$1 AND c.relname=$2 AND co.contype='c' AND a.attnum=ANY(co.conkey)`

func pgSpatialQuote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// InspectPGSpatial is read-only. In particular, recovery previews must not
// install extensions as a side effect of inspecting a saved migration.
func InspectPGSpatial(ctx context.Context, db *sql.DB) (*PGSpatialCapabilities, error) {
	return preparePGSpatial(ctx, db, false)
}

// PreparePGSpatial only enables a server-installed, explicitly supported
// extension. It never downloads packages, changes server settings, or upgrades
// an existing installation.
func PreparePGSpatial(ctx context.Context, db *sql.DB) (*PGSpatialCapabilities, error) {
	return preparePGSpatial(ctx, db, true)
}

func findPGSpatialSchema(ctx context.Context, db *sql.DB) (string, bool, error) {
	rows, err := db.QueryContext(ctx, pgSpatialTypesSQL)
	if err != nil {
		return "", false, err
	}
	defer rows.Close()
	type candidate struct {
		schema         string
		postgis, shown bool
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.schema, &c.postgis, &c.shown); err != nil {
			return "", false, err
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	for _, c := range candidates {
		if c.postgis {
			return c.schema, true, nil
		}
	}
	for _, c := range candidates {
		if c.shown {
			return c.schema, false, nil
		}
	}
	if len(candidates) == 1 {
		return candidates[0].schema, false, nil
	}
	if len(candidates) > 1 {
		return "", false, fmt.Errorf("存在多个不可见的 geometry 类型，无法确定目标空间类型")
	}
	return "", false, nil
}

func preparePGSpatial(ctx context.Context, db *sql.DB, install bool) (*PGSpatialCapabilities, error) {
	caps := &PGSpatialCapabilities{}
	schema, postgis, err := findPGSpatialSchema(ctx, db)
	if err != nil {
		return caps, fmt.Errorf("检查目标 geometry 类型失败: %w", err)
	}
	if schema == "" {
		if !install {
			return caps, fmt.Errorf("目标 geometry 类型尚未启用；只读预览不会创建空间扩展")
		}
		var available string
		var installed sql.NullString
		if err := db.QueryRowContext(ctx, pgSpatialAvailableSQL, "postgis").Scan(&available, &installed); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return caps, fmt.Errorf("目标没有 geometry 类型，服务器也未安装可启用的 postgis 扩展包")
			}
			return caps, fmt.Errorf("检查可用 postgis 扩展失败: %w", err)
		}
		if installed.Valid {
			return caps, fmt.Errorf("postgis 扩展已注册但 geometry 类型不可用，请检查目标安装")
		}
		created := true
		var raceErr error
		_, createErr := execObservedAllow(ctx, db, `CREATE EXTENSION IF NOT EXISTS "postgis"`, func(err error) bool {
			if !compatibilityRace(err) {
				return false
			}
			var found string
			var extension bool
			found, extension, raceErr = findPGSpatialSchema(ctx, db)
			created = false
			return raceErr == nil && found != "" && extension
		})
		if createErr != nil {
			return caps, fmt.Errorf("启用已安装的 postgis 扩展失败（不会修改服务器参数或安装软件包）: %w", errors.Join(createErr, raceErr))
		}
		caps.ExtensionCreated = created
		schema, postgis, err = findPGSpatialSchema(ctx, db)
		if err != nil {
			return caps, fmt.Errorf("启用 postgis 后检查 geometry 类型失败: %w", err)
		}
		if schema == "" {
			return caps, fmt.Errorf("启用 postgis 后仍未发现 geometry 类型")
		}
	}
	caps.Schema = schema
	if postgis {
		caps.ExtensionName = "postgis"
		if err := db.QueryRowContext(ctx, `SELECT extversion FROM pg_extension WHERE extname=$1`, "postgis").Scan(&caps.ExtensionVersion); err != nil {
			return caps, fmt.Errorf("读取 postgis 版本失败: %w", err)
		}
	}
	// Test the same HEXEWKB text input used by COPY, including a nonzero SRID
	// and signed fractional coordinates. NDR is also used by CDC row matching.
	const point = "0101000020e6100000000000000000f43f00000000000004c0"
	q := pgSpatialQuote(schema)
	query := `SELECT ` + q + `.ST_SRID(g),` + q + `.ST_GeometryType(g),` + q + `.ST_AsEWKB(g,'NDR') FROM (SELECT $1::` + q + `.geometry AS g) s`
	var srid int64
	var kind string
	var binary []byte
	if err := db.QueryRowContext(ctx, query, point).Scan(&srid, &kind, &binary); err != nil {
		return caps, fmt.Errorf("目标空间类型不支持保留 SRID 的 HEXEWKB 输入/输出: %w", err)
	}
	want, _ := hex.DecodeString(point)
	if srid != 4326 || !strings.EqualFold(kind, "ST_Point") || !bytes.Equal(binary, want) {
		return caps, fmt.Errorf("目标空间类型 HEXEWKB 往返改变了坐标、类型或 SRID")
	}
	if err := inspectPGSpatialGIST(ctx, db, caps); err != nil {
		if ctx.Err() != nil {
			return caps, ctx.Err()
		}
		if uncertainRepairError(err) {
			return caps, err
		}
		caps.GISTError = err.Error()
	}
	return caps, nil
}

func inspectPGSpatialGIST(ctx context.Context, db *sql.DB, caps *PGSpatialCapabilities) error {
	rows, err := db.QueryContext(ctx, pgSpatialGISTSQL, caps.Schema)
	if err != nil {
		return fmt.Errorf("检查空间 GiST 操作符类失败: %w", err)
	}
	var schemas, classes []string
	for rows.Next() {
		var schema, class string
		if err := rows.Scan(&schema, &class); err != nil {
			rows.Close()
			return err
		}
		schemas, classes = append(schemas, schema), append(classes, class)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(classes) != 1 {
		return fmt.Errorf("目标 geometry 缺少唯一可用的 gist_geometry_ops_2d 操作符类")
	}
	var setting string
	err = db.QueryRowContext(ctx, `SELECT setting FROM pg_settings WHERE name='enable_astore_gist'`).Scan(&setting)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("检查目标 GiST 开关失败: %w", err)
	}
	if err == nil && (strings.EqualFold(setting, "off") || setting == "0" || strings.EqualFold(setting, "false")) {
		return fmt.Errorf("目标 enable_astore_gist 未开启，不能创建空间 GiST 索引")
	}
	caps.GISTSchema, caps.GISTOpClass = schemas[0], classes[0]
	return nil
}

func pgSpatialSubtype(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "geometry", "point", "linestring", "polygon", "multipoint", "multilinestring", "multipolygon", "geometrycollection":
		return strings.ToUpper(strings.TrimSpace(raw))
	case "geomcollection":
		return "GEOMETRYCOLLECTION"
	}
	return ""
}

// PGSpatialQueryer permits column checks inside the transaction holding a
// table lock, as well as startup checks through a connection pool.
type PGSpatialQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// CheckPGSpatialTable checks that existing columns will not narrow the source
// subtype/SRID domain or silently assign a target SRID to source SRID zero.
// info contains target column names and the original source types/constraints.
func CheckPGSpatialTable(ctx context.Context, db PGSpatialQueryer, schema, table string, info *source.TableDDLInfo, caps *PGSpatialCapabilities) error {
	if caps == nil || caps.Schema == "" {
		return fmt.Errorf("缺少目标空间能力检测结果")
	}
	if info == nil {
		return fmt.Errorf("缺少表 %s 的源列元数据", table)
	}
	if schema == "" {
		if err := db.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
			return err
		}
	}
	for _, col := range info.Columns {
		kind := pgSpatialSubtype(col.DataType)
		if kind == "" {
			continue
		}
		if err := checkPGSpatialColumn(ctx, db, schema, table, col, kind, caps); err != nil {
			return fmt.Errorf("目标空间列 %s.%s.%s 不兼容: %w", schema, table, col.Name, err)
		}
	}
	return nil
}

func checkPGSpatialColumn(ctx context.Context, db PGSpatialQueryer, schema, table string, col source.ColumnInfo, kind string, caps *PGSpatialCapabilities) error {
	var typeSchema, dataType string
	var typmod int64
	if err := db.QueryRowContext(ctx, pgSpatialColumnSQL, schema, table, col.Name).Scan(&typeSchema, &dataType, &typmod); err != nil {
		return err
	}
	if typeSchema != caps.Schema || dataType != "geometry" {
		return fmt.Errorf("目标类型 %s.%s 不是已验证的 %s.geometry", typeSchema, dataType, caps.Schema)
	}
	targetKind := "GEOMETRY"
	var targetSRID *uint32
	if typmod >= 0 {
		var srid, dimensions int64
		q := pgSpatialQuote(caps.Schema)
		query := `SELECT ` + q + `.postgis_typmod_type($1),` + q + `.postgis_typmod_srid($1),` + q + `.postgis_typmod_dims($1)`
		if err := db.QueryRowContext(ctx, query, typmod).Scan(&targetKind, &srid, &dimensions); err != nil {
			return fmt.Errorf("无法核验目标空间列类型约束: %w", err)
		}
		targetKind = strings.ToUpper(targetKind)
		if dimensions != 2 {
			return fmt.Errorf("目标空间列维度为 %d，MySQL 空间数据为二维", dimensions)
		}
		if srid > 0 {
			v := uint32(srid)
			targetSRID = &v
		}
	}
	rows, err := db.QueryContext(ctx, pgSpatialChecksSQL, schema, table, col.Name)
	if err != nil {
		return err
	}
	var checks []string
	for rows.Next() {
		var check string
		if err := rows.Scan(&check); err != nil {
			rows.Close()
			return err
		}
		checks = append(checks, check)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, check := range checks {
		lower := strings.ToLower(check)
		if strings.Contains(lower, "geometrytype") || strings.Contains(lower, "st_ndims") {
			return fmt.Errorf("目标空间列含额外子类型或维度 CHECK，需要核验后再补迁: %s", check)
		}
		if strings.Contains(lower, "st_srid") {
			v, err := pgSpatialCheckSRID(check, col.Name)
			if err != nil {
				return err
			}
			if targetSRID != nil && *targetSRID != v {
				return fmt.Errorf("目标空间列具有冲突的 SRID 约束")
			}
			targetSRID = &v
		}
	}
	if targetKind != "GEOMETRY" && targetKind != kind {
		return fmt.Errorf("源类型 %s 不保证满足目标子类型 %s", kind, targetKind)
	}
	if targetSRID != nil && (col.SpatialSRID == nil || *col.SpatialSRID != *targetSRID) {
		return fmt.Errorf("目标固定 SRID=%d，但源列 SRID 约束不同或未限制，拒绝隐式赋值/转换", *targetSRID)
	}
	return nil
}

func inspectPGSpatialRepairColumn(ctx context.Context, db *sql.DB, schema, table string, col *source.ColumnInfo) error {
	var typeSchema, name string
	var typmod int64
	if err := db.QueryRowContext(ctx, pgSpatialColumnSQL, schema, table, col.Name).Scan(&typeSchema, &name, &typmod); err != nil {
		return err
	}
	if name != "geometry" {
		return nil
	}
	col.DataType, col.TypeSchema, col.FormattedType = "geometry", typeSchema, "geometry"
	if typmod >= 0 {
		var kind string
		var srid, dims int64
		q := pgSpatialQuote(typeSchema)
		query := `SELECT ` + q + `.postgis_typmod_type($1),` + q + `.postgis_typmod_srid($1),` + q + `.postgis_typmod_dims($1)`
		if err := db.QueryRowContext(ctx, query, typmod).Scan(&kind, &srid, &dims); err != nil {
			return fmt.Errorf("读取目标空间列类型约束失败: %w", err)
		}
		col.FormattedType = fmt.Sprintf("geometry(%s,%d)", kind, srid)
		if srid > 0 {
			v := uint32(srid)
			col.SpatialSRID = &v
		}
	}
	rows, err := db.QueryContext(ctx, pgSpatialChecksSQL, schema, table, col.Name)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var check string
		if err := rows.Scan(&check); err != nil {
			return err
		}
		if strings.Contains(strings.ToLower(check), "st_srid") {
			v, err := pgSpatialCheckSRID(check, col.Name)
			if err != nil {
				return err
			}
			if col.SpatialSRID != nil && *col.SpatialSRID != v {
				return fmt.Errorf("目标空间列存在冲突的 SRID 约束")
			}
			col.SpatialSRID = &v
		}
	}
	return rows.Err()
}

// PostgreSQL deparses CHECK with extra parentheses. Accept only a simple SRID
// equality on this column; an arbitrary expression cannot establish safety.
func pgSpatialCheckSRID(check, column string) (uint32, error) {
	identifier := `(?:"(?:[^"]|"")+"|[a-zA-Z_][a-zA-Z_0-9$]*)`
	columnRE := regexp.QuoteMeta(pgSpatialQuote(column))
	if regexp.MustCompile(`^[a-z_][a-z_0-9$]*$`).MatchString(column) {
		columnRE = `(?:` + columnRE + `|` + regexp.QuoteMeta(column) + `)`
	}
	pattern := `(?i)^\s*CHECK\s*\(\s*\(*\s*(?:` + identifier + `\s*\.\s*)?"?st_srid"?\s*\(\s*` + columnRE + `\s*\)\s*=\s*([0-9]+)\s*\)*\s*\)\s*(?:NOT VALID)?\s*$`
	matches := regexp.MustCompile(pattern).FindStringSubmatch(check)
	if len(matches) != 2 {
		return 0, fmt.Errorf("无法可靠核验目标空间列 SRID CHECK: %s", check)
	}
	v, err := strconv.ParseUint(matches[1], 10, 32)
	if err != nil {
		return 0, fmt.Errorf("目标 SRID CHECK 超出范围: %w", err)
	}
	return uint32(v), nil
}

func preparePGSpatialRepair(ctx context.Context, db *sql.DB, obj repair.Object) error {
	kind := repairKind(obj.Kind)
	spatialIndex := kind == "index" && obj.Index != nil && strings.EqualFold(obj.Index.Method, "gist")
	var spatialColumns []source.ColumnInfo
	if kind == "table" {
		for _, c := range obj.Columns {
			if strings.EqualFold(c.DataType, "geometry") {
				spatialColumns = append(spatialColumns, c)
			}
		}
		if len(spatialColumns) == 0 {
			for _, c := range obj.SourceColumns {
				if strings.EqualFold(c.DataType, "geometry") || c.SpatialSRID != nil {
					return fmt.Errorf("空间表修复缺少已验证的目标类型快照，请重新生成修复计划")
				}
			}
		}
	}
	if !spatialIndex && len(spatialColumns) == 0 {
		return nil
	}
	caps, err := PreparePGSpatial(ctx, db)
	if err != nil {
		return err
	}
	for _, c := range spatialColumns {
		if c.TypeSchema == "" || c.TypeSchema != caps.Schema {
			return fmt.Errorf("空间列 %s 的类型 schema 与原修复计划不一致，请重新生成计划；未修改或执行原 SQL", c.Name)
		}
	}
	if spatialIndex {
		if caps.GISTError != "" {
			return fmt.Errorf("目标空间索引能力不足: %s", caps.GISTError)
		}
		if obj.Index.PGSpatialSchema != caps.GISTSchema || obj.Index.PGSpatialOpClass != caps.GISTOpClass {
			return fmt.Errorf("空间索引操作符类与原修复计划不一致，请重新生成计划；未修改或执行原 SQL")
		}
	}
	return nil
}

func (w *PostgresWriter) PrepareMySQLPGSpatial(ctx context.Context) (*PGSpatialCapabilities, error) {
	return PreparePGSpatial(ctx, w.db)
}
func (w *HighGoWriter) PrepareMySQLPGSpatial(ctx context.Context) (*PGSpatialCapabilities, error) {
	return PreparePGSpatial(ctx, w.db)
}
func (w *GaussDBWriter) PrepareMySQLPGSpatial(ctx context.Context) (*PGSpatialCapabilities, error) {
	return PreparePGSpatial(ctx, w.db)
}
func (w *KingbaseWriter) PrepareMySQLPGSpatial(ctx context.Context) (*PGSpatialCapabilities, error) {
	return PreparePGSpatial(ctx, w.db)
}
func (w *PostgresWriter) InspectMySQLPGSpatial(ctx context.Context) (*PGSpatialCapabilities, error) {
	return InspectPGSpatial(ctx, w.db)
}
func (w *HighGoWriter) InspectMySQLPGSpatial(ctx context.Context) (*PGSpatialCapabilities, error) {
	return InspectPGSpatial(ctx, w.db)
}
func (w *GaussDBWriter) InspectMySQLPGSpatial(ctx context.Context) (*PGSpatialCapabilities, error) {
	return InspectPGSpatial(ctx, w.db)
}
func (w *KingbaseWriter) InspectMySQLPGSpatial(ctx context.Context) (*PGSpatialCapabilities, error) {
	return InspectPGSpatial(ctx, w.db)
}
func (w *PostgresWriter) CheckMySQLPGSpatialTable(ctx context.Context, table string, info *source.TableDDLInfo, caps *PGSpatialCapabilities) error {
	return CheckPGSpatialTable(ctx, w.db, w.schema, table, info, caps)
}
func (w *HighGoWriter) CheckMySQLPGSpatialTable(ctx context.Context, table string, info *source.TableDDLInfo, caps *PGSpatialCapabilities) error {
	return CheckPGSpatialTable(ctx, w.db, w.schema, table, info, caps)
}
func (w *GaussDBWriter) CheckMySQLPGSpatialTable(ctx context.Context, table string, info *source.TableDDLInfo, caps *PGSpatialCapabilities) error {
	return CheckPGSpatialTable(ctx, w.db, w.schema, table, info, caps)
}
func (w *KingbaseWriter) CheckMySQLPGSpatialTable(ctx context.Context, table string, info *source.TableDDLInfo, caps *PGSpatialCapabilities) error {
	return CheckPGSpatialTable(ctx, w.db, w.schema, table, info, caps)
}

const pgSpatialIndexSQL = `SELECT a.attname,ix.indisunique,ix.indisprimary,ix.indisvalid,ix.indpred IS NOT NULL OR ix.indexprs IS NOT NULL,am.amname,onsp.nspname,opc.opcname FROM pg_index ix JOIN pg_class t ON t.oid=ix.indrelid JOIN pg_namespace ns ON ns.oid=t.relnamespace JOIN pg_class i ON i.oid=ix.indexrelid JOIN pg_am am ON am.oid=i.relam CROSS JOIN LATERAL unnest(ix.indkey) WITH ORDINALITY k(attnum,n) LEFT JOIN pg_attribute a ON a.attrelid=t.oid AND a.attnum=k.attnum LEFT JOIN pg_opclass opc ON opc.oid=ix.indclass[k.n-1] LEFT JOIN pg_namespace onsp ON onsp.oid=opc.opcnamespace WHERE ns.nspname=$1 AND t.relname=$2 AND i.relname=$3 ORDER BY k.n`

func inspectPGSpatialIndex(ctx context.Context, db *sql.DB, schema, table, name string, idx source.IndexInfo) (repair.Verification, error) {
	if len(idx.Columns) != 1 || idx.IsPrimary || idx.IsUnique || idx.PGSpatialSchema == "" || idx.PGSpatialOpClass == "" {
		return verification("unknown", "缺少可核验的单列空间 GiST 索引计划")
	}
	rows, err := db.QueryContext(ctx, pgSpatialIndexSQL, schema, table, name)
	if err != nil {
		return repair.Verification{Status: "unknown", Message: err.Error()}, err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var column, opSchema, opClass sql.NullString
		var method string
		var unique, primary, valid, special bool
		if err := rows.Scan(&column, &unique, &primary, &valid, &special, &method, &opSchema, &opClass); err != nil {
			return repair.Verification{}, err
		}
		count++
		if count != 1 || !column.Valid || column.String != idx.Columns[0] || unique || primary || !valid || special || method != "gist" || opSchema.String != idx.PGSpatialSchema || opClass.String != idx.PGSpatialOpClass {
			return verification("failed", "目标同名索引的表、列、GiST 操作符类或有效状态与空间索引计划不符")
		}
	}
	if err := rows.Err(); err != nil {
		return repair.Verification{}, err
	}
	if count == 0 {
		return verification("failed", "目标空间索引不存在")
	}
	return verification("succeeded", "空间 GiST 索引的表、列、操作符类与有效状态核验通过")
}
