package target

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
)

// inspectColumns reads actual target metadata, including writable/identity
// properties. It deliberately does not reverse-engineer a user-edited SQL text.
func inspectColumns(ctx context.Context, w Writer, table string) ([]source.ColumnInfo, error) {
	a, err := repairAccess(w)
	if err != nil {
		return nil, err
	}
	schema, err := ResolveRepairSchema(ctx, w)
	if err != nil {
		return nil, err
	}
	var query string
	args := []any{schema, table}
	switch a.family {
	case "postgres":
		query = `SELECT column_name,data_type,COALESCE(character_maximum_length,0),COALESCE(numeric_precision,0),COALESCE(numeric_scale,0),is_nullable,column_default,CASE WHEN column_default LIKE 'nextval(%' OR COALESCE(row_to_json(c)->>'is_identity','NO')='YES' THEN 'auto_increment' ELSE '' END,ordinal_position,COALESCE(datetime_precision,0),CASE WHEN COALESCE(row_to_json(c)->>'is_generated','NEVER')='ALWAYS' THEN COALESCE(row_to_json(c)->>'generation_expression','generated') ELSE '' END FROM information_schema.columns c WHERE table_schema=$1 AND table_name=$2 ORDER BY ordinal_position`
	case "mysql":
		query = `SELECT COLUMN_NAME,COLUMN_TYPE,COALESCE(CHARACTER_MAXIMUM_LENGTH,0),COALESCE(NUMERIC_PRECISION,0),COALESCE(NUMERIC_SCALE,0),IS_NULLABLE,COLUMN_DEFAULT,EXTRA,ORDINAL_POSITION,COALESCE(DATETIME_PRECISION,0),GENERATION_EXPRESSION FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? ORDER BY ORDINAL_POSITION`
	case "oracle":
		// ALL_TAB_COLUMNS preserves the visible column set. Oracle exposes
		// VIRTUAL_COLUMN only through ALL_TAB_COLS (including on 11g).
		query = `SELECT c.COLUMN_NAME,c.DATA_TYPE,COALESCE(c.CHAR_LENGTH,c.DATA_LENGTH),COALESCE(c.DATA_PRECISION,0),COALESCE(c.DATA_SCALE,0),CASE WHEN c.NULLABLE='Y' THEN 'YES' ELSE 'NO' END,c.DATA_DEFAULT,'',c.COLUMN_ID,COALESCE(c.DATA_SCALE,0),CASE WHEN vc.VIRTUAL_COLUMN='YES' THEN 'generated' ELSE '' END
 FROM ALL_TAB_COLUMNS c
 JOIN ALL_TAB_COLS vc ON vc.OWNER=c.OWNER AND vc.TABLE_NAME=c.TABLE_NAME AND vc.COLUMN_NAME=c.COLUMN_NAME
 WHERE c.OWNER=:1 AND c.TABLE_NAME=:2 ORDER BY c.COLUMN_ID`
	case "dameng":
		query = `SELECT COLUMN_NAME,DATA_TYPE,COALESCE(CHAR_LENGTH,DATA_LENGTH),COALESCE(DATA_PRECISION,0),COALESCE(DATA_SCALE,0),CASE WHEN NULLABLE='Y' THEN 'YES' ELSE 'NO' END,DATA_DEFAULT,'',COLUMN_ID,COALESCE(DATA_SCALE,0),CASE WHEN VIRTUAL_COLUMN='YES' THEN 'generated' ELSE '' END FROM ALL_TAB_COLUMNS WHERE OWNER=? AND TABLE_NAME=? ORDER BY COLUMN_ID`
	case "sqlserver":
		query = `SELECT c.name,t.name,CASE WHEN t.name IN ('nvarchar','nchar') AND c.max_length>0 THEN c.max_length/2 ELSE c.max_length END,c.precision,c.scale,CASE WHEN c.is_nullable=1 THEN 'YES' ELSE 'NO' END,OBJECT_DEFINITION(c.default_object_id),CASE WHEN c.is_identity=1 THEN 'auto_increment' ELSE '' END,c.column_id,c.scale,CASE WHEN c.is_computed=1 OR t.name IN ('timestamp','rowversion') THEN 'generated' ELSE '' END FROM sys.columns c JOIN sys.types t ON c.user_type_id=t.user_type_id JOIN sys.tables tb ON tb.object_id=c.object_id JOIN sys.schemas s ON s.schema_id=tb.schema_id WHERE s.name=@p1 AND tb.name=@p2 ORDER BY c.column_id`
	default:
		return nil, fmt.Errorf("目标 %s 尚无可验证的列元数据接口，不能自动补迁", w.DBType())
	}
	rows, err := a.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []source.ColumnInfo
	for rows.Next() {
		var c source.ColumnInfo
		var nullable string
		var def, extra, generated sql.NullString
		if err = rows.Scan(&c.Name, &c.DataType, &c.Length, &c.Precision, &c.Scale, &nullable, &def, &extra, &c.Ordinal, &c.DateTimePrecision, &generated); err != nil {
			return nil, err
		}
		c.IsNullable = nullable == "YES"
		c.Extra = extra.String
		c.Generated = generated.String
		if def.Valid {
			v := def.String
			c.Default = &v
		}
		c.FormattedType = actualColumnType(c, a.family)
		columns = append(columns, c)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("目标表 %s.%s 不存在或列元数据不可见", schema, table)
	}
	return columns, nil
}

func actualColumnType(c source.ColumnInfo, family string) string {
	t := strings.ToUpper(c.DataType)
	switch t {
	case "VARCHAR", "NVARCHAR", "CHAR", "NCHAR", "VARBINARY", "BINARY", "VARCHAR2", "NVARCHAR2", "CHARACTER VARYING", "CHARACTER":
		if c.Length < 0 {
			return t + "(MAX)"
		}
		if c.Length > 0 {
			return fmt.Sprintf("%s(%d)", t, c.Length)
		}
	case "DECIMAL", "NUMERIC", "NUMBER":
		if c.Precision > 0 {
			return fmt.Sprintf("%s(%d,%d)", t, c.Precision, c.Scale)
		}
	case "DATETIME2", "TIME":
		if family == "sqlserver" {
			return fmt.Sprintf("%s(%d)", t, c.DateTimePrecision)
		}
	case "FLOAT":
		if family == "sqlserver" {
			return fmt.Sprintf("FLOAT(%d)", c.Precision)
		}
	}
	return t
}

func verification(status, message string) (repair.Verification, error) {
	return repair.Verification{Status: status, Message: message}, nil
}

// InspectRepair never equates a same-name object with a verified repair. Where
// the dialect cannot prove the saved definition, an explicit unknown is returned.
func InspectRepair(ctx context.Context, w Writer, obj repair.Object) (repair.Verification, error) {
	a, err := repairAccess(w)
	if err != nil {
		return repair.Verification{}, err
	}
	schema, err := ResolveRepairSchema(ctx, w)
	if err != nil {
		return repair.Verification{}, err
	}
	if schema != obj.Schema {
		return verification("failed", "目标 schema 与原对象不一致")
	}
	kind := repairKind(obj.Kind)
	if dm, ok := w.(*DaMengWriter); ok && kind == "sequence" && obj.Sequence != nil && obj.Sequence.Native != nil && !repairDefinitionEdited(obj) {
		plan, e := dm.planOracleDamengSequence(ctx, *obj.Sequence)
		if e != nil {
			return repair.Verification{Status: "failed", Message: e.Error()}, e
		}
		if !plan.sequenceExists || len(plan.missingTriggers) > 0 {
			return verification("failed", "序列或关联触发器尚未完整创建，请刷新序列修复方案")
		}
		if len(plan.statements) != 0 {
			return verification("failed", "序列定义兼容但仍需向前校准，请刷新序列修复方案")
		}
		return verification("succeeded", "序列及关联触发器完整兼容，当前续号已越过源水位和关联列极值")
	}
	if kind == "table" {
		exists, e := inspectOrdinaryTable(ctx, a, obj.Schema, obj.TargetName)
		if e != nil {
			return repair.Verification{Status: "unknown", Message: e.Error()}, e
		}
		if !exists {
			return verification("failed", "目标同名对象不是普通表或不存在")
		}
		cols, e := inspectColumns(ctx, w, obj.TargetName)
		if e != nil {
			return repair.Verification{Status: "unknown", Message: e.Error()}, e
		}
		expectedColumns := obj.SourceColumns
		if len(expectedColumns) == 0 {
			expectedColumns = obj.Columns
		}
		if len(expectedColumns) == 0 {
			return verification("unknown", "表存在，但原任务缺少列定义，无法确认结构")
		}
		found := map[string]source.ColumnInfo{}
		for _, c := range cols {
			found[c.Name] = c
		}
		for _, c := range expectedColumns {
			n := c.Name
			if mapped, ok := obj.ColumnMapping[n]; ok {
				n = mapped
			}
			if _, ok := found[n]; !ok {
				return verification("failed", "目标表缺少预期列 "+n)
			}
		}
		// Source types are not target types. Existence + all expected columns is
		// the structural repair criterion; recovery has a separate compatibility check.
		return verification("succeeded", "目标表存在且全部预期列可见；数据兼容性在补迁前单独检查")
	}
	if kind == "primary_key" || kind == "index" {
		return inspectRepairIndex(ctx, w, a, obj)
	}
	if kind == "foreign_key" {
		return inspectRepairFK(ctx, w, a, obj)
	}
	if kind == "comment" {
		return inspectRepairComment(ctx, a, obj)
	}
	if kind == "view" {
		if ow, ok := w.(*OracleWriter); ok {
			if e := ow.validateObject(ctx, obj.TargetName, "VIEW"); e != nil {
				return verification("failed", e.Error())
			}
		}
		return verification("unknown", "需核对实际视图定义及依赖；仅存在或编译有效不能证明修订 SQL 已生效")
	}
	return verification("unknown", "此类对象需复核完整定义与关联状态，不能仅凭对象同名确认成功")
}

func inspectOrdinaryTable(ctx context.Context, a repairTarget, schema, table string) (bool, error) {
	q := ""
	switch a.family {
	case "postgres":
		q = `SELECT COUNT(*) FROM pg_class t JOIN pg_namespace n ON n.oid=t.relnamespace WHERE n.nspname=$1 AND t.relname=$2 AND t.relkind IN ('r','p')`
	case "mysql":
		q = `SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND TABLE_TYPE='BASE TABLE'`
	case "sqlserver":
		q = `SELECT COUNT(*) FROM sys.tables t JOIN sys.schemas s ON s.schema_id=t.schema_id WHERE s.name=@p1 AND t.name=@p2`
	case "oracle":
		q = `SELECT COUNT(*) FROM ALL_TABLES WHERE OWNER=:1 AND TABLE_NAME=:2`
	case "dameng":
		q = `SELECT COUNT(*) FROM ALL_TABLES WHERE OWNER=? AND TABLE_NAME=?`
	default:
		return false, fmt.Errorf("当前目标缺少普通表目录核验能力")
	}
	var count int
	err := a.db.QueryRowContext(ctx, q, schema, table).Scan(&count)
	return count == 1, err
}

func repairDefinitionEdited(obj repair.Object) bool {
	for _, u := range obj.Units {
		if strings.TrimSpace(u.SQL) != "" && (u.OriginalSQL == "" || u.SQL != u.OriginalSQL) {
			return true
		}
	}
	return false
}

func inspectRepairIndex(ctx context.Context, w Writer, a repairTarget, obj repair.Object) (repair.Verification, error) {
	if obj.Index == nil {
		return verification("unknown", "缺少原索引列定义，不能核验")
	}
	if repairDefinitionEdited(obj) {
		return verification("unknown", "索引 DDL 已编辑，需要核对修订后的定义")
	}
	idx := obj.Index
	name := obj.TargetName
	table := idx.TableName
	if obj.Table != "" {
		table = obj.Table
	}
	var q string
	var args []any
	switch a.family {
	case "postgres":
		q = `SELECT a.attname,ix.indisunique,ix.indisprimary,ix.indisvalid,ix.indpred IS NOT NULL OR ix.indexprs IS NOT NULL,(ix.indoption[k.n-1] & 1)<>0 FROM pg_index ix JOIN pg_class t ON t.oid=ix.indrelid JOIN pg_namespace ns ON ns.oid=t.relnamespace JOIN pg_class i ON i.oid=ix.indexrelid CROSS JOIN LATERAL unnest(ix.indkey) WITH ORDINALITY k(attnum,n) LEFT JOIN pg_attribute a ON a.attrelid=t.oid AND a.attnum=k.attnum WHERE ns.nspname=$1 AND t.relname=$2 AND (($3 AND ix.indisprimary) OR (NOT $3 AND i.relname=$4)) ORDER BY k.n`
		args = []any{obj.Schema, table, idx.IsPrimary, name}
	case "mysql":
		q = `SELECT COLUMN_NAME,NON_UNIQUE=0,INDEX_NAME='PRIMARY',TRUE,SUB_PART IS NOT NULL,COLLATION='D' FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND INDEX_NAME=? ORDER BY SEQ_IN_INDEX`
		if idx.IsPrimary {
			name = "PRIMARY"
		}
		args = []any{obj.Schema, table, name}
	case "sqlserver":
		q = `SELECT c.name,i.is_unique,i.is_primary_key,CASE WHEN i.is_disabled=0 THEN 1 ELSE 0 END,i.has_filter,ic.is_descending_key FROM sys.indexes i JOIN sys.tables t ON t.object_id=i.object_id JOIN sys.schemas s ON s.schema_id=t.schema_id JOIN sys.index_columns ic ON ic.object_id=i.object_id AND ic.index_id=i.index_id JOIN sys.columns c ON c.object_id=ic.object_id AND c.column_id=ic.column_id WHERE s.name=@p1 AND t.name=@p2 AND ((@p3=1 AND i.is_primary_key=1) OR (@p3=0 AND i.name=@p4)) AND ic.key_ordinal>0 ORDER BY ic.key_ordinal`
		args = []any{obj.Schema, table, idx.IsPrimary, name}
	case "oracle", "dameng":
		p1, p2, p3 := ":1", ":2", ":3"
		if a.family == "dameng" {
			p1, p2, p3 = "?", "?", "?"
		}
		primary := "0"
		where := `i.INDEX_NAME=` + p3
		args = []any{obj.Schema, table, name}
		if idx.IsPrimary {
			primary = "1"
			where = `EXISTS (SELECT 1 FROM ALL_CONSTRAINTS pc WHERE pc.OWNER=i.OWNER AND pc.TABLE_NAME=i.TABLE_NAME AND pc.INDEX_NAME=i.INDEX_NAME AND pc.CONSTRAINT_TYPE='P')`
			args = []any{obj.Schema, table}
		}
		q = `SELECT ic.COLUMN_NAME,CASE WHEN i.UNIQUENESS='UNIQUE' THEN 1 ELSE 0 END,` + primary + `,CASE WHEN i.STATUS='VALID' THEN 1 ELSE 0 END,CASE WHEN i.INDEX_TYPE='NORMAL' THEN 0 ELSE 1 END,CASE WHEN ic.DESCEND='DESC' THEN 1 ELSE 0 END FROM ALL_INDEXES i JOIN ALL_IND_COLUMNS ic ON ic.INDEX_OWNER=i.OWNER AND ic.INDEX_NAME=i.INDEX_NAME WHERE i.TABLE_OWNER=` + p1 + ` AND i.TABLE_NAME=` + p2 + ` AND ` + where + ` ORDER BY ic.COLUMN_POSITION`
	default:
		return verification("unknown", "当前目标方言尚不能完整核验索引定义")
	}
	rows, e := a.db.QueryContext(ctx, q, args...)
	if e != nil {
		return repair.Verification{Status: "unknown", Message: e.Error()}, e
	}
	defer rows.Close()
	var cols []string
	var desc []bool
	for rows.Next() {
		var c sql.NullString
		var unique, primary, valid, special, descending bool
		if e = rows.Scan(&c, &unique, &primary, &valid, &special, &descending); e != nil {
			return repair.Verification{}, e
		}
		if !valid || special || !c.Valid {
			return verification("unknown", "索引含表达式、过滤条件、前缀列或无效状态，需要人工核对")
		}
		if unique != (idx.IsUnique || idx.IsPrimary) || primary != idx.IsPrimary {
			return verification("failed", "索引唯一性或主键属性与原计划不符")
		}
		cols = append(cols, c.String)
		desc = append(desc, descending)
	}
	if e = rows.Err(); e != nil {
		return repair.Verification{}, e
	}
	if len(cols) == 0 {
		return verification("failed", "目标索引不存在")
	}
	if !slices.Equal(cols, idx.Columns) {
		return verification("failed", "索引键列或顺序与原计划不符")
	}
	for i, d := range desc {
		want := i < len(idx.Descending) && idx.Descending[i]
		if d != want {
			return verification("failed", "索引排序方向与原计划不符")
		}
	}
	return verification("succeeded", "索引表、键列、顺序及唯一性核验通过")
}

func inspectRepairFK(ctx context.Context, w Writer, a repairTarget, obj repair.Object) (repair.Verification, error) {
	if obj.ForeignKey == nil || repairDefinitionEdited(obj) {
		return verification("unknown", "缺少可核对的完整外键定义或 DDL 已编辑")
	}
	f := obj.ForeignKey
	var q string
	args := []any{obj.Schema, f.TableName, obj.TargetName}
	switch a.family {
	case "postgres":
		q = `SELECT ca.attname,rn.nspname,rt.relname,ra.attname,c.confdeltype::text,c.confupdtype::text,c.convalidated FROM pg_constraint c JOIN pg_class t ON t.oid=c.conrelid JOIN pg_namespace n ON n.oid=t.relnamespace JOIN pg_class rt ON rt.oid=c.confrelid JOIN pg_namespace rn ON rn.oid=rt.relnamespace CROSS JOIN LATERAL unnest(c.conkey,c.confkey) WITH ORDINALITY k(a,b,pos) JOIN pg_attribute ca ON ca.attrelid=t.oid AND ca.attnum=k.a JOIN pg_attribute ra ON ra.attrelid=rt.oid AND ra.attnum=k.b WHERE c.contype='f' AND n.nspname=$1 AND t.relname=$2 AND c.conname=$3 ORDER BY k.pos`
	case "mysql":
		q = `SELECT k.COLUMN_NAME,k.REFERENCED_TABLE_SCHEMA,k.REFERENCED_TABLE_NAME,k.REFERENCED_COLUMN_NAME,r.DELETE_RULE,r.UPDATE_RULE,TRUE FROM information_schema.KEY_COLUMN_USAGE k JOIN information_schema.REFERENTIAL_CONSTRAINTS r ON r.CONSTRAINT_SCHEMA=k.CONSTRAINT_SCHEMA AND r.TABLE_NAME=k.TABLE_NAME AND r.CONSTRAINT_NAME=k.CONSTRAINT_NAME WHERE k.TABLE_SCHEMA=? AND k.TABLE_NAME=? AND k.CONSTRAINT_NAME=? ORDER BY k.ORDINAL_POSITION`
	case "sqlserver":
		q = `SELECT c.name,rs.name,rt.name,rc.name,f.delete_referential_action_desc,f.update_referential_action_desc,CASE WHEN f.is_disabled=0 AND f.is_not_trusted=0 THEN 1 ELSE 0 END FROM sys.foreign_keys f JOIN sys.tables t ON t.object_id=f.parent_object_id JOIN sys.schemas s ON s.schema_id=t.schema_id JOIN sys.tables rt ON rt.object_id=f.referenced_object_id JOIN sys.schemas rs ON rs.schema_id=rt.schema_id JOIN sys.foreign_key_columns k ON k.constraint_object_id=f.object_id JOIN sys.columns c ON c.object_id=t.object_id AND c.column_id=k.parent_column_id JOIN sys.columns rc ON rc.object_id=rt.object_id AND rc.column_id=k.referenced_column_id WHERE s.name=@p1 AND t.name=@p2 AND f.name=@p3 ORDER BY k.constraint_column_id`
	case "oracle", "dameng":
		p1, p2, p3 := ":1", ":2", ":3"
		if a.family == "dameng" {
			p1, p2, p3 = "?", "?", "?"
		}
		q = `SELECT cc.COLUMN_NAME,p.OWNER,p.TABLE_NAME,pc.COLUMN_NAME,c.DELETE_RULE,'NO ACTION',CASE WHEN c.STATUS='ENABLED' AND c.VALIDATED='VALIDATED' THEN 1 ELSE 0 END FROM ALL_CONSTRAINTS c JOIN ALL_CONS_COLUMNS cc ON cc.OWNER=c.OWNER AND cc.CONSTRAINT_NAME=c.CONSTRAINT_NAME JOIN ALL_CONSTRAINTS p ON p.OWNER=c.R_OWNER AND p.CONSTRAINT_NAME=c.R_CONSTRAINT_NAME JOIN ALL_CONS_COLUMNS pc ON pc.OWNER=p.OWNER AND pc.CONSTRAINT_NAME=p.CONSTRAINT_NAME AND pc.POSITION=cc.POSITION WHERE c.CONSTRAINT_TYPE='R' AND c.OWNER=` + p1 + ` AND c.TABLE_NAME=` + p2 + ` AND c.CONSTRAINT_NAME=` + p3 + ` ORDER BY cc.POSITION`
	default:
		return verification("unknown", "当前目标方言尚不能完整核验外键定义")
	}
	rows, e := a.db.QueryContext(ctx, q, args...)
	if e != nil {
		return repair.Verification{Status: "unknown", Message: e.Error()}, e
	}
	defer rows.Close()
	var cols, refs []string
	for rows.Next() {
		var c, rs, rt, rc, del, upd string
		var valid bool
		if e = rows.Scan(&c, &rs, &rt, &rc, &del, &upd, &valid); e != nil {
			return repair.Verification{}, e
		}
		if !valid {
			return verification("failed", "外键未启用或尚未验证已有数据")
		}
		if rs != obj.Schema || rt != f.RefTable || normalFKAction(del) != normalFKAction(f.OnDelete) || normalFKAction(upd) != normalFKAction(f.OnUpdate) {
			return verification("failed", "外键引用目标或更新删除规则不符")
		}
		cols = append(cols, c)
		refs = append(refs, rc)
	}
	if e = rows.Err(); e != nil {
		return repair.Verification{}, e
	}
	if len(cols) == 0 {
		return verification("failed", "目标外键不存在")
	}
	if !slices.Equal(cols, f.Columns) || !slices.Equal(refs, f.RefColumns) {
		return verification("failed", "外键列对应关系不符")
	}
	return verification("succeeded", "外键引用、列顺序及动作核验通过")
}

func normalFKAction(v string) string {
	switch strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(v), "_", " ")) {
	case "", "A", "R", "RESTRICT", "NO ACTION":
		return "NO ACTION"
	case "C":
		return "CASCADE"
	case "N":
		return "SET NULL"
	case "D":
		return "SET DEFAULT"
	default:
		return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(v), "_", " "))
	}
}
