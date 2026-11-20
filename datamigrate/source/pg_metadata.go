package source

import (
	"context"
	"database/sql"
	"dbgold/datamigrate/failure"
	"dbgold/datamigrate/sqlsyntax"
	"fmt"
	"strconv"
	"strings"
)

// PGProfile records observed capabilities, not a claim of vendor certification.
type PGProfile struct {
	Product      string          `json:"product"`
	Version      string          `json:"version"`
	Mode         string          `json:"mode"`
	Schema       string          `json:"schema"`
	Catalog      string          `json:"catalog"`
	Fields       map[string]bool `json:"fields"`
	Functions    map[string]bool `json:"functions"`
	Verification string          `json:"verification"`
}
type PGObject struct {
	PG         *PGConstraintMetadata `json:"pg,omitempty"`
	Kind       string                `json:"kind"`
	Table      string                `json:"table,omitempty"`
	Name       string                `json:"name"`
	Definition string                `json:"definition,omitempty"`
	Reason     string                `json:"reason,omitempty"`
}
type PGTable struct {
	Info     TableDDLInfo
	OID      int64
	Kind     string
	Inherits bool
	// PartitionChild is a physical descendant, never a separate copy unit.
	PartitionChild bool
	Indexes        []IndexInfo
	ForeignKeys    []FKInfo
	Checks         []PGObject
	Unsupported    []PGObject
	Sequences      []PGSequence
}
type PGSequence struct {
	SequenceInfo
	OID       int64
	Name      string
	Schema    string
	Increment int64
	Min       int64
	Max       int64
	Cycle     bool
	Shared    bool
	Identity  string
	LastValue int64
	IsCalled  bool
	Cache     int64
	Exhausted bool
	Bindings  []PGSequenceBinding
}

// PGSequenceBinding retains source identities, including consumers outside the
// selected migration scope. Default is the untouched pg_get_expr result.
type PGSequenceBinding struct {
	Schema, TableName, ColumnName, Default, Identity string
	Owned                                            bool
}

// PGConstraintMetadata describes source semantics without deciding whether a
// particular target can reproduce them.
type PGConstraintMetadata struct {
	Type, Match, Definition                  string
	Deferrable, InitiallyDeferred, Validated bool
	NoInherit                                bool
}

type PGIndexMetadata struct {
	KeyCount                                 int
	KeyDefinitions                           []string
	Expressions, Predicate                   string
	Options                                  []int
	NullsNotDistinct, Valid, Immediate       bool
	ConstraintName, ConstraintType           string
	Deferrable, InitiallyDeferred, Validated bool
	NonDefaultOpClass, NonDefaultCollation   bool
}
type PGSnapshot struct {
	Profile   PGProfile
	Tables    []PGTable
	Views     []ViewInfo
	Objects   []PGObject
	Sequences []PGSequence
}

func IsPGFamily(s string) bool {
	switch s {
	case "postgres", "gaussdb", "highgo", "vastbase", "gbase", "seabox", "kingbase":
		return true
	}
	return false
}
func QuotePGIdent(s string) string             { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func (r *PostgresReader) PGProfile() PGProfile { return r.profile }
func (r *PostgresReader) qcat(name string) string {
	return QuotePGIdent(r.profile.Catalog) + "." + QuotePGIdent(name)
}

func (r *PostgresReader) detectProfile(ctx context.Context, product string) error {
	r.profile = PGProfile{Product: product, Schema: r.schema, Fields: map[string]bool{}, Verification: "待验证（能力探测不等于实库认证）"}
	if err := r.db.QueryRowContext(ctx, `SELECT version()`).Scan(&r.profile.Version); err != nil {
		return err
	}
	// Only fixed, trusted catalog identifiers are interpolated. Probe aliases too:
	// some Kingbase versions expose their native catalog as sys_catalog.
	var last error
	for _, catalog := range []string{"pg_catalog", "sys_catalog"} {
		q := `SELECT c.relname, a.attname FROM ` + catalog + `.pg_attribute a JOIN ` + catalog + `.pg_class c ON c.oid=a.attrelid JOIN ` + catalog + `.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND a.attnum>0 AND NOT a.attisdropped`
		rows, err := r.db.QueryContext(ctx, q, catalog)
		if err != nil {
			last = err
			continue
		}
		fields := map[string]bool{}
		for rows.Next() {
			var table, col string
			if err = rows.Scan(&table, &col); err != nil {
				rows.Close()
				return err
			}
			fields[table+"."+col] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if fields["pg_class.relname"] && fields["pg_attribute.attname"] {
			r.profile.Catalog = catalog
			r.profile.Fields = fields
			break
		}
	}
	if r.profile.Catalog == "" {
		return fmt.Errorf("无法识别 PG 系统目录: %v", last)
	}
	fieldsRequired := []string{"pg_attribute.atttypid", "pg_index.indkey", "pg_constraint.conkey", "pg_constraint.confkey", "pg_depend.refobjid", "pg_type.typtype"}
	if r.oracleMetadata {
		fieldsRequired = []string{"pg_attribute.atttypid", "pg_type.typtype"}
	}
	for _, f := range fieldsRequired {
		if !r.profile.Fields[f] {
			return fmt.Errorf("源库缺少必要元数据 %s", f)
		}
	}
	funcRows, err := r.db.QueryContext(ctx, `SELECT p.proname,has_function_privilege(p.oid,'EXECUTE') FROM `+r.qcat("pg_proc")+` p JOIN `+r.qcat("pg_namespace")+` n ON n.oid=p.pronamespace WHERE n.nspname=$1 AND p.proname IN ('pg_get_expr','pg_get_indexdef','pg_get_constraintdef','pg_get_triggerdef','pg_get_viewdef','col_description','obj_description','format_type','oidvectortypes','has_table_privilege')`, r.profile.Catalog)
	if err != nil {
		return fmt.Errorf("探测元数据函数权限: %w", err)
	}
	r.profile.Functions = map[string]bool{}
	for funcRows.Next() {
		var name string
		var allowed bool
		if err = funcRows.Scan(&name, &allowed); err != nil {
			funcRows.Close()
			return err
		}
		r.profile.Functions[name] = r.profile.Functions[name] || allowed
	}
	err = funcRows.Err()
	funcRows.Close()
	if err != nil {
		return err
	}
	functionsRequired := []string{"pg_get_expr", "pg_get_indexdef", "pg_get_constraintdef", "pg_get_triggerdef", "pg_get_viewdef", "col_description", "obj_description", "format_type", "oidvectortypes", "has_table_privilege"}
	if r.oracleMetadata {
		functionsRequired = []string{"pg_get_expr", "format_type", "has_table_privilege"}
	}
	for _, name := range functionsRequired {
		if !r.profile.Functions[name] {
			return fmt.Errorf("缺少元数据函数 %s 或 EXECUTE 权限", name)
		}
	}
	var exists bool
	if err := r.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM `+r.qcat("pg_namespace")+` WHERE nspname=$1)`, r.schema).Scan(&exists); err != nil {
		return err
	}
	if !exists && r.schema != "*" {
		return fmt.Errorf("源 schema %q 不存在", r.schema)
	}
	rows, err := r.db.QueryContext(ctx, `SELECT name,setting FROM `+r.qcat("pg_settings")+` WHERE name IN ('sql_compatibility','behavior_compat_options') ORDER BY name`)
	if err != nil {
		return fmt.Errorf("读取兼容模式: %w", err)
	}
	defer rows.Close()
	var modes []string
	for rows.Next() {
		var k, v string
		if err = rows.Scan(&k, &v); err != nil {
			return err
		}
		modes = append(modes, k+"="+v)
	}
	r.profile.Mode = strings.Join(modes, "; ")
	return rows.Err()
}

func (r *PostgresReader) field(table, col, alias, fallback string) string {
	if r.profile.Fields[table+"."+col] {
		return alias + "." + col
	}
	return fallback
}

// ParsePGNumbers handles both int2vector output and SQL array text. Ordering is
// retained in Go; no LATERAL / generate_subscripts / WITH ORDINALITY is needed.
func ParsePGNumbers(raw string) ([]int, error) {
	raw = strings.TrimSpace(raw)
	if i := strings.Index(raw, "="); i >= 0 {
		raw = raw[i+1:]
	}
	raw = strings.Trim(raw, "{}")
	var out []int
	for _, p := range strings.FieldsFunc(raw, func(c rune) bool { return c == ',' || c == ' ' }) {
		n, e := strconv.Atoi(p)
		if e != nil {
			return nil, fmt.Errorf("无效目录数组 %q", raw)
		}
		out = append(out, n)
	}
	return out, nil
}

func (r *PostgresReader) pgColumns(ctx context.Context, table string) (*TableDDLInfo, error) {
	return r.pgColumnsForTarget(ctx, table, r.oracleMetadata)
}

func (r *PostgresReader) pgColumnsForTarget(ctx context.Context, table string, neutral bool) (*TableDDLInfo, error) {
	identity := r.field("pg_attribute", "attidentity", "a", "''")
	generated := r.field("pg_attribute", "attgenerated", "a", "''")
	q := `SELECT a.attnum,a.attname,t.typname,t.typtype,tn.nspname,a.atttypmod,NOT a.attnotnull,pg_get_expr(d.adbin,d.adrelid),` + identity + `,` + generated + `,col_description(c.oid,a.attnum),format_type(a.atttypid,a.atttypmod)
 FROM ` + r.qcat("pg_class") + ` c JOIN ` + r.qcat("pg_namespace") + ` n ON n.oid=c.relnamespace JOIN ` + r.qcat("pg_attribute") + ` a ON a.attrelid=c.oid JOIN ` + r.qcat("pg_type") + ` t ON t.oid=a.atttypid JOIN ` + r.qcat("pg_namespace") + ` tn ON tn.oid=t.typnamespace LEFT JOIN ` + r.qcat("pg_attrdef") + ` d ON d.adrelid=c.oid AND d.adnum=a.attnum WHERE n.nspname=$1 AND c.relname=$2 AND a.attnum>0 AND NOT a.attisdropped ORDER BY a.attnum`
	if neutral && !r.pgMetadataFunction("col_description") {
		q = strings.Replace(q, "col_description(c.oid,a.attnum)", "NULL::text", 1)
	}
	rows, err := r.db.QueryContext(ctx, q, r.schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	info := &TableDDLInfo{TableName: table}
	for rows.Next() {
		var col ColumnInfo
		var mod int64
		var def, comment sql.NullString
		var identity, generated string
		if err = rows.Scan(&col.Ordinal, &col.Name, &col.DataType, &col.TypeKind, &col.TypeSchema, &mod, &col.IsNullable, &def, &identity, &generated, &comment, &col.FormattedType); err != nil {
			return nil, err
		}
		col.Comment = comment.String
		col.Identity = strings.Trim(identity, "\x00")
		col.Generated = strings.Trim(generated, "\x00")
		if def.Valid {
			v := def.String
			col.Default = &v
		}
		switch col.DataType {
		case "varchar", "bpchar":
			if mod >= 4 {
				col.Length = mod - 4
			}
		case "numeric":
			if mod >= 4 {
				v := mod - 4
				col.Precision = (v >> 16) & 65535
				col.Scale = v & 2047
				if col.Scale >= 1024 {
					col.Scale -= 2048
				}
			}
		case "timestamp", "timestamptz", "time", "timetz":
			if mod < 0 {
				col.DateTimePrecision = 6
			} else {
				col.DateTimePrecision = mod
			}
		case "bit", "varbit":
			if mod >= 0 {
				col.Length = mod
			}
		}
		if col.Identity != "" && col.Identity != "0" || def.Valid && pgSimpleNextval(def.String) {
			col.Extra = "auto_increment"
		}
		info.Columns = append(info.Columns, col)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(info.Columns) == 0 {
		return nil, fmt.Errorf("表 %q 无可读取列", table)
	}
	return info, nil
}

func (r *PostgresReader) pgIndexes(ctx context.Context) ([]IndexInfo, error) {
	return r.pgIndexesForTarget(ctx, false)
}

func (r *PostgresReader) pgIndexesForTarget(ctx context.Context, neutral bool) ([]IndexInfo, error) {
	if neutral && !r.pgMetadataFunction("pg_get_indexdef") {
		return nil, fmt.Errorf("缺少元数据函数 pg_get_indexdef 或 EXECUTE 权限")
	}
	keyCount := r.field("pg_index", "indnkeyatts", "ix", "ix.indnatts")
	nulls := r.field("pg_index", "indnullsnotdistinct", "ix", "false")
	q := `SELECT t.relname,i.relname,ix.indkey::text,ix.indisunique,ix.indisprimary,am.amname,ix.indexprs IS NOT NULL,ix.indpred IS NOT NULL,` + keyCount + `,ix.indnatts,` + nulls + `,ix.indisvalid,ix.indimmediate,ix.indoption::text,pg_get_indexdef(ix.indexrelid)
 FROM ` + r.qcat("pg_index") + ` ix JOIN ` + r.qcat("pg_class") + ` i ON i.oid=ix.indexrelid JOIN ` + r.qcat("pg_class") + ` t ON t.oid=ix.indrelid JOIN ` + r.qcat("pg_namespace") + ` n ON n.oid=t.relnamespace JOIN ` + r.qcat("pg_am") + ` am ON am.oid=i.relam WHERE n.nspname=$1 ORDER BY t.relname,i.relname`
	if neutral {
		extra := `,ix.indexrelid,COALESCE(pg_get_expr(ix.indexprs,ix.indrelid),''),COALESCE(pg_get_expr(ix.indpred,ix.indrelid),''),COALESCE(con.conname,''),COALESCE(con.contype::text,''),COALESCE(con.condeferrable,false),COALESCE(con.condeferred,false),COALESCE(con.convalidated,true),EXISTS(SELECT 1 FROM ` + r.qcat("pg_collation") + ` coll JOIN ` + r.qcat("pg_namespace") + ` cn ON cn.oid=coll.collnamespace WHERE coll.oid=ANY(ix.indcollation) AND NOT (cn.nspname IN ('pg_catalog','sys_catalog') AND coll.collname='default'))`
		q = strings.Replace(q, "\n FROM ", extra+"\n FROM ", 1)
		q = strings.Replace(q, " WHERE n.nspname=$1 ORDER BY", ` LEFT JOIN `+r.qcat("pg_constraint")+` con ON con.conindid=ix.indexrelid AND con.contype IN ('p','u') WHERE n.nspname=$1 ORDER BY`, 1)
	}
	rows, err := r.db.QueryContext(ctx, q, r.schema)
	if err != nil {
		return nil, err
	}
	var indexes []IndexInfo
	var indexOIDs []int64
	for rows.Next() {
		var idx IndexInfo
		var keys, options string
		var nkeys, total int
		var expr, partial, nd, valid, immediate bool
		values := []any{&idx.TableName, &idx.IndexName, &keys, &idx.IsUnique, &idx.IsPrimary, &idx.Method, &expr, &partial, &nkeys, &total, &nd, &valid, &immediate, &options, &idx.Definition}
		var oid int64
		if neutral {
			idx.PG = &PGIndexMetadata{}
			values = append(values, &oid, &idx.PG.Expressions, &idx.PG.Predicate, &idx.PG.ConstraintName, &idx.PG.ConstraintType, &idx.PG.Deferrable, &idx.PG.InitiallyDeferred, &idx.PG.Validated, &idx.PG.NonDefaultCollation)
		}
		if err = rows.Scan(values...); err != nil {
			rows.Close()
			return nil, err
		}
		idx.Attnums, err = ParsePGNumbers(keys)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if len(idx.Attnums) != total || nkeys < 1 || nkeys > total {
			rows.Close()
			return nil, fmt.Errorf("索引 %s 目录数组长度与属性数不一致", idx.IndexName)
		}
		for _, n := range idx.Attnums {
			if n == 0 {
				expr = true
			}
		}
		var why []string
		if expr {
			why = append(why, "表达式索引")
		}
		if partial {
			why = append(why, "部分索引")
		}
		if nkeys != total {
			why = append(why, "INCLUDE 索引")
		}
		if nd {
			why = append(why, "NULLS NOT DISTINCT")
		}
		if !valid {
			why = append(why, "无效索引")
		}
		if !immediate {
			why = append(why, "延迟唯一约束")
		}
		if idx.Method != "btree" {
			why = append(why, "非 BTREE 索引")
		}
		opts, e := ParsePGNumbers(options)
		if e != nil {
			rows.Close()
			return nil, e
		}
		if len(opts) != nkeys {
			rows.Close()
			return nil, fmt.Errorf("索引 %s 的 indoption 数量 %d 与键列数量 %d 不一致", idx.IndexName, len(opts), nkeys)
		}
		if idx.Method == "btree" {
			// BTREE indoption is a bitmask, not an enum: DESC=1, NULLS_FIRST=2.
			// An ascending NULLS FIRST index (2) matches MySQL's native ordering;
			// it must not be rejected just because PostgreSQL's default is 0.
			// Existing ASC NULLS LAST (0) mappings continue to use target defaults.
			// Keep DESC and unknown vendor bits blocked instead of dropping them.
			for i, v := range opts {
				if v < 0 || v & ^3 != 0 {
					why = append(why, fmt.Sprintf("索引第 %d 个键列含未知选项（indoption=%d）", i+1, v))
				} else if v&1 != 0 {
					why = append(why, fmt.Sprintf("索引第 %d 个键列为降序 DESC，暂不自动转换（indoption=%d）", i+1, v))
				}
			}
		}
		if neutral {
			idx.PG.KeyCount, idx.PG.Options = nkeys, opts
			idx.PG.NullsNotDistinct, idx.PG.Valid, idx.PG.Immediate = nd, valid, immediate
			idx.Descending = make([]bool, nkeys)
			for i, option := range opts {
				idx.Descending[i] = option&1 != 0
			}
		} else {
			idx.Unsupported = strings.Join(why, "、")
		}
		indexes = append(indexes, idx)
		indexOIDs = append(indexOIDs, oid)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	cols, err := r.pgAttributeNames(ctx)
	if err != nil {
		return nil, err
	}
	for i := range indexes {
		for _, n := range indexes[i].Attnums {
			if n == 0 {
				continue
			}
			name, ok := cols[indexes[i].TableName][n]
			if !ok {
				return nil, fmt.Errorf("索引 %s 引用未知列 %d", indexes[i].IndexName, n)
			}
			indexes[i].Columns = append(indexes[i].Columns, name)
		}
		if neutral {
			// The server renders each key independently: no string splitting of
			// expressions containing commas, casts, quoted names or literals.
			for key := 1; key <= indexes[i].PG.KeyCount; key++ {
				var definition string
				if err := r.db.QueryRowContext(ctx, `SELECT pg_get_indexdef($1::oid,$2,true)`, indexOIDs[i], key).Scan(&definition); err != nil {
					return nil, fmt.Errorf("读取索引 %s 第 %d 个键: %w", indexes[i].IndexName, key, err)
				}
				indexes[i].PG.KeyDefinitions = append(indexes[i].PG.KeyDefinitions, definition)
			}
		}
	}
	return indexes, nil
}
func (r *PostgresReader) pgAttributeNames(ctx context.Context) (map[string]map[int]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT c.relname,a.attnum,a.attname FROM `+r.qcat("pg_attribute")+` a JOIN `+r.qcat("pg_class")+` c ON c.oid=a.attrelid JOIN `+r.qcat("pg_namespace")+` n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND a.attnum>0 AND NOT a.attisdropped`, r.schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[int]string{}
	for rows.Next() {
		var t, c string
		var n int
		if err = rows.Scan(&t, &n, &c); err != nil {
			return nil, err
		}
		if out[t] == nil {
			out[t] = map[int]string{}
		}
		out[t][n] = c
	}
	return out, rows.Err()
}

func (r *PostgresReader) pgForeignKeys(ctx context.Context) ([]FKInfo, error) {
	return r.pgForeignKeysForTarget(ctx, false)
}

func (r *PostgresReader) pgForeignKeysForTarget(ctx context.Context, neutral bool) ([]FKInfo, error) {
	if neutral && !r.pgMetadataFunction("pg_get_constraintdef") {
		return nil, fmt.Errorf("缺少元数据函数 pg_get_constraintdef 或 EXECUTE 权限")
	}
	q := `SELECT c.conname,t.relname,rn.nspname,rt.relname,c.conkey::text,c.confkey::text,c.confdeltype,c.confupdtype,c.condeferrable,c.confmatchtype,c.confrelid FROM ` + r.qcat("pg_constraint") + ` c JOIN ` + r.qcat("pg_class") + ` t ON t.oid=c.conrelid JOIN ` + r.qcat("pg_namespace") + ` n ON n.oid=t.relnamespace JOIN ` + r.qcat("pg_class") + ` rt ON rt.oid=c.confrelid JOIN ` + r.qcat("pg_namespace") + ` rn ON rn.oid=rt.relnamespace WHERE n.nspname=$1 AND c.contype='f' ORDER BY t.relname,c.conname`
	if neutral {
		q = strings.Replace(q, " FROM ", ",c.condeferred,c.convalidated,pg_get_constraintdef(c.oid) FROM ", 1)
	}
	rows, err := r.db.QueryContext(ctx, q, r.schema)
	if err != nil {
		return nil, err
	}
	type fkRaw struct {
		fk         FKInfo
		keys, refs []int
		oid        int64
	}
	var raw []fkRaw
	for rows.Next() {
		var f fkRaw
		var keys, refs, del, upd, match string
		var deferred bool
		values := []any{&f.fk.ConstraintName, &f.fk.TableName, &f.fk.RefSchema, &f.fk.RefTable, &keys, &refs, &del, &upd, &deferred, &match, &f.oid}
		if neutral {
			f.fk.PG = &PGConstraintMetadata{Type: "f"}
			values = append(values, &f.fk.PG.InitiallyDeferred, &f.fk.PG.Validated, &f.fk.PG.Definition)
		}
		if err = rows.Scan(values...); err != nil {
			rows.Close()
			return nil, err
		}
		f.keys, err = ParsePGNumbers(keys)
		if err != nil {
			rows.Close()
			return nil, err
		}
		f.refs, err = ParsePGNumbers(refs)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if len(f.keys) == 0 || len(f.keys) != len(f.refs) {
			rows.Close()
			return nil, fmt.Errorf("外键列数量不匹配")
		}
		action := func(s string) string {
			return map[string]string{"a": "NO ACTION", "r": "RESTRICT", "c": "CASCADE", "n": "SET NULL", "d": "SET DEFAULT"}[s]
		}
		f.fk.OnDelete = action(del)
		f.fk.OnUpdate = action(upd)
		if neutral {
			f.fk.PG.Deferrable, f.fk.PG.Match = deferred, match
		} else if deferred || match != "s" || f.fk.OnDelete == "" || f.fk.OnUpdate == "" {
			f.fk.Unsupported = "延迟外键或非 MATCH SIMPLE"
		}
		raw = append(raw, f)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	cols, err := r.pgAttributeNames(ctx)
	if err != nil {
		return nil, err
	}
	var out []FKInfo
	for _, f := range raw {
		refcols := map[int]string{}
		rs, e := r.db.QueryContext(ctx, `SELECT attnum,attname FROM `+r.qcat("pg_attribute")+` WHERE attrelid=$1 AND attnum>0 AND NOT attisdropped`, f.oid)
		if e != nil {
			return nil, e
		}
		for rs.Next() {
			var n int
			var name string
			if e = rs.Scan(&n, &name); e != nil {
				rs.Close()
				return nil, e
			}
			refcols[n] = name
		}
		e = rs.Err()
		rs.Close()
		if e != nil {
			return nil, e
		}
		for i, n := range f.keys {
			a, b := cols[f.fk.TableName][n], refcols[f.refs[i]]
			if a == "" || b == "" {
				return nil, fmt.Errorf("外键 %s 引用未知列", f.fk.ConstraintName)
			}
			f.fk.Columns = append(f.fk.Columns, a)
			f.fk.RefColumns = append(f.fk.RefColumns, b)
		}
		out = append(out, f.fk)
	}
	return out, nil
}

// ReadPGSnapshot reads all object identities before filtering. This makes missing
// privileges and unsupported dependencies visible rather than an empty success.
func (r *PostgresReader) ReadPGSnapshot(ctx context.Context) (*PGSnapshot, error) {
	return r.readPGSnapshot(ctx, nil, false)
}

// ReadPGSnapshotForTables keeps the caller's frozen logical-root scope. Catalog
// inventory is still read in full, but excluded roots and physical descendants
// do not acquire SELECT/column-read requirements during a strict target plan.
// The original ReadPGSnapshot keeps its schema-wide compatibility behavior.
func (r *PostgresReader) ReadPGSnapshotForTables(ctx context.Context, tables []string) (*PGSnapshot, error) {
	selected := make(map[string]bool, len(tables))
	for _, table := range tables {
		selected[table] = true
	}
	return r.readPGSnapshot(ctx, selected, false)
}

// ReadPGMigrationSnapshot preserves usable tables when a selected table has a
// local metadata/privilege failure. The strict public snapshot stays unchanged.
func (r *PostgresReader) ReadPGMigrationSnapshot(ctx context.Context, tables []string) (*PGSnapshot, error) {
	var selected map[string]bool
	if tables != nil {
		selected = map[string]bool{}
		for _, table := range tables {
			selected[table] = true
		}
	}
	return r.readPGSnapshot(ctx, selected, true)
}

// ReadPGOracleSnapshot retains raw PG object semantics and complete named
// sequence state. A nil selection means all tables; an empty selection means
// no tables. Independent sequences in this schema remain visible in either
// case, and shared generators retain consumers outside the selected tables.
func (r *PostgresReader) ReadPGOracleSnapshot(ctx context.Context, tables []string) (*PGSnapshot, error) {
	var selected map[string]bool
	if tables != nil {
		selected = make(map[string]bool, len(tables))
		for _, table := range tables {
			selected[table] = true
		}
	}
	return r.readPGSnapshotForTarget(ctx, selected, true, true)
}

func (r *PostgresReader) readPGSnapshot(ctx context.Context, selected map[string]bool, partial bool) (*PGSnapshot, error) {
	return r.readPGSnapshotForTarget(ctx, selected, partial, false)
}

func (r *PostgresReader) readPGSnapshotForTarget(ctx context.Context, selected map[string]bool, partial, neutral bool) (*PGSnapshot, error) {
	snap := &PGSnapshot{Profile: r.profile}
	found := make(map[string]bool, len(selected))
	partition := "false"
	if r.profile.Fields["pg_class.parttype"] {
		partition = "c.parttype IN ('p','s')"
	}
	if r.profile.Fields["pg_partition.parentid"] {
		partition += " OR EXISTS(SELECT 1 FROM " + r.qcat("pg_partition") + " part WHERE part.parentid=c.oid)"
	}
	security := "false"
	if r.profile.Fields["pg_class.relrowsecurity"] {
		security = "c.relrowsecurity"
	}
	q := `SELECT c.oid,c.relname,c.relkind,(` + partition + ` OR EXISTS(SELECT 1 FROM ` + r.qcat("pg_inherits") + ` h WHERE h.inhrelid=c.oid OR h.inhparent=c.oid)),obj_description(c.oid,'pg_class'),has_table_privilege(c.oid,'SELECT'),` + security + ` FROM ` + r.qcat("pg_class") + ` c JOIN ` + r.qcat("pg_namespace") + ` n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relkind IN ('r','p','f','m') ORDER BY c.relname`
	if neutral && !r.pgMetadataFunction("obj_description") {
		q = strings.Replace(q, "obj_description(c.oid,'pg_class')", "NULL::text", 1)
	}
	declarative := r.profile.Fields["pg_class.relispartition"] && r.profile.Fields["pg_class.relpartbound"] && r.profile.Fields["pg_partitioned_table.partstrat"]
	nativePartitionCatalog := r.profile.Fields["pg_partition.partstrategy"] && r.profile.Fields["pg_partition.parentid"]
	if declarative {
		q = strings.Replace(q, " FROM "+r.qcat("pg_class")+" c JOIN ", ",c.relispartition FROM "+r.qcat("pg_class")+" c JOIN ", 1)
	}
	rows, err := r.db.QueryContext(ctx, q, r.schema)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t PGTable
		var comment sql.NullString
		var readable, rowSecurity bool
		values := []interface{}{&t.OID, &t.Info.TableName, &t.Kind, &t.Inherits, &comment, &readable, &rowSecurity}
		if declarative {
			values = append(values, &t.PartitionChild)
		}
		if err = rows.Scan(values...); err != nil {
			rows.Close()
			return nil, err
		}
		if selected != nil && !selected[t.Info.TableName] {
			continue
		}
		found[t.Info.TableName] = true
		t.Info.Comment = comment.String
		if rowSecurity {
			t.Unsupported = append(t.Unsupported, PGObject{Kind: "table", Name: t.Info.TableName, Reason: "源表启用了行级安全策略，不能证明 SELECT 能读取完整数据"})
		}
		if !readable {
			err := fmt.Errorf("缺少表 %s 的 SELECT 权限", t.Info.TableName)
			if !partial {
				rows.Close()
				return nil, err
			}
			t.Unsupported = append(t.Unsupported, PGObject{Kind: "table", Table: t.Info.TableName, Name: t.Info.TableName, Reason: err.Error()})
		}
		snap.Tables = append(snap.Tables, t)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for name := range selected {
		if !found[name] {
			if !partial {
				return nil, fmt.Errorf("选择的源表 %s 不存在或身份已变化", name)
			}
			snap.Tables = append(snap.Tables, PGTable{Info: TableDDLInfo{TableName: name}, Unsupported: []PGObject{{Kind: "table", Name: name, Table: name, Reason: "选择的源表不存在或身份已变化"}}})
		}
	}
	indexes, err := r.pgIndexesForTarget(ctx, neutral)
	if err != nil {
		if !neutral || failure.IsTaskError(err) {
			return nil, err
		}
		pgObjectMetadataFailure(snap, "index", "索引元数据", err)
		pgObjectMetadataFailure(snap, "primary_key", "主键元数据", err)
	}
	fks, err := r.pgForeignKeysForTarget(ctx, neutral)
	if err != nil {
		if !neutral || failure.IsTaskError(err) {
			return nil, err
		}
		pgObjectMetadataFailure(snap, "foreign_key", "外键元数据", err)
	}
	byName := map[string]*PGTable{}
	for i := range snap.Tables {
		t := &snap.Tables[i]
		byName[t.Info.TableName] = t
		if partial && (neutral && pgTableMetadataBlocked(t.Unsupported) || !neutral && len(t.Unsupported) > 0) {
			continue
		}
		info, e := r.pgColumnsForTarget(ctx, t.Info.TableName, neutral)
		if e != nil {
			if !partial || failure.IsTaskError(e) {
				return nil, e
			}
			t.Unsupported = append(t.Unsupported, PGObject{Kind: "table", Name: t.Info.TableName, Table: t.Info.TableName, Reason: e.Error()})
			continue
		}
		info.Comment = t.Info.Comment
		t.Info = *info
		if neutral && (!r.pgMetadataFunction("obj_description") || !r.pgMetadataFunction("col_description")) {
			snap.Objects = append(snap.Objects, PGObject{Kind: "comment", Table: t.Info.TableName, Name: t.Info.TableName + " 注释", Reason: "缺少 obj_description/col_description 元数据函数或 EXECUTE 权限"})
		}
		byName[t.Info.TableName] = t
		if declarative && t.Kind == "p" && !t.PartitionChild || nativePartitionCatalog && t.Kind == "r" && t.Inherits {
			part, partitionErr := r.PartitionMetadata(ctx, t.Info.TableName)
			if partitionErr != nil {
				t.Unsupported = append(t.Unsupported, PGObject{Kind: "table", Table: t.Info.TableName, Name: t.Info.TableName, Reason: "分区元数据读取失败: " + partitionErr.Error()})
			} else {
				t.Info.Partition = part
			}
		}
		if (!declarative && (t.Kind != "r" || t.Inherits && t.Info.Partition == nil)) || (declarative && (t.Kind != "r" && t.Kind != "p" || t.Inherits && t.Kind != "p" && !t.PartitionChild)) {
			t.Unsupported = append(t.Unsupported, PGObject{Kind: "table", Table: t.Info.TableName, Name: t.Info.TableName, Reason: "分区、继承、外部表或物化视图首版不自动转换"})
		}
	}
	for _, idx := range indexes {
		if t := byName[idx.TableName]; t != nil {
			t.Indexes = append(t.Indexes, idx)
		}
	}
	for _, fk := range fks {
		if t := byName[fk.TableName]; t != nil {
			t.ForeignKeys = append(t.ForeignKeys, fk)
		}
	}
	// CHECK and exclusion constraints are not represented by ordinary indexes.
	constraintQuery := `SELECT t.relname,c.conname,c.contype,pg_get_constraintdef(c.oid),c.convalidated FROM ` + r.qcat("pg_constraint") + ` c JOIN ` + r.qcat("pg_class") + ` t ON t.oid=c.conrelid JOIN ` + r.qcat("pg_namespace") + ` n ON n.oid=t.relnamespace WHERE n.nspname=$1 AND c.contype IN ('c','x') ORDER BY t.relname,c.conname`
	if neutral {
		constraintQuery = strings.Replace(constraintQuery, " FROM ", ",c.condeferrable,c.condeferred,"+r.field("pg_constraint", "connoinherit", "c", "false")+" FROM ", 1)
	}
	rows, err = r.pgOptionalObjectQuery(ctx, snap, neutral, "check", "检查/排除约束元数据", "pg_get_constraintdef", constraintQuery, r.schema)
	if err != nil {
		return nil, err
	}
	for rows != nil && rows.Next() {
		var o PGObject
		var kind string
		var valid bool
		values := []any{&o.Table, &o.Name, &kind, &o.Definition, &valid}
		if neutral {
			o.PG = &PGConstraintMetadata{}
			values = append(values, &o.PG.Deferrable, &o.PG.InitiallyDeferred, &o.PG.NoInherit)
		}
		if err = rows.Scan(values...); err != nil {
			rows.Close()
			return nil, err
		}
		o.Kind = "check"
		if kind == "x" {
			o.Kind = "constraint"
			if !neutral {
				o.Reason = "排除约束不支持"
			}
		}
		if neutral {
			o.PG.Type, o.PG.Validated, o.PG.Definition = kind, valid, o.Definition
		} else if !valid {
			o.Reason = "未验证约束不支持"
		}
		if t := byName[o.Table]; t != nil {
			if o.Reason != "" {
				t.Unsupported = append(t.Unsupported, o)
			} else {
				t.Checks = append(t.Checks, o)
			}
		}
	}
	err = pgFinishOptionalRows(rows)
	if err != nil {
		if !neutral || failure.IsTaskError(err) {
			return nil, err
		}
		pgObjectMetadataFailure(snap, "check", "检查/排除约束元数据", err)
	}
	if err := r.markPGOperatorClassesForTarget(ctx, byName, neutral); err != nil {
		if !neutral || failure.IsTaskError(err) {
			return nil, err
		}
		for _, table := range byName {
			for i := range table.Indexes {
				table.Indexes[i].Unsupported = "索引操作符类元数据无法核验: " + err.Error()
			}
		}
	}
	rows, err = r.pgOptionalObjectQuery(ctx, snap, neutral, "trigger", "触发器元数据", "pg_get_triggerdef", `SELECT t.relname,g.tgname,pg_get_triggerdef(g.oid) FROM `+r.qcat("pg_trigger")+` g JOIN `+r.qcat("pg_class")+` t ON t.oid=g.tgrelid JOIN `+r.qcat("pg_namespace")+` n ON n.oid=t.relnamespace WHERE n.nspname=$1 AND NOT g.tgisinternal ORDER BY t.relname,g.tgname`, r.schema)
	if err != nil {
		return nil, err
	}
	for rows != nil && rows.Next() {
		o := PGObject{Kind: "trigger", Reason: "触发器需要人工适配"}
		if err = rows.Scan(&o.Table, &o.Name, &o.Definition); err != nil {
			rows.Close()
			return nil, err
		}
		snap.Objects = append(snap.Objects, o)
	}
	err = pgFinishOptionalRows(rows)
	if err != nil {
		if !neutral || failure.IsTaskError(err) {
			return nil, err
		}
		pgObjectMetadataFailure(snap, "trigger", "触发器元数据", err)
	}
	// Do not claim a routine is absent because information_schema hides it.
	prokind := r.field("pg_proc", "prokind", "p", "'f'")
	filter := `p.proisagg=false`
	if r.profile.Fields["pg_proc.prokind"] {
		filter = `p.prokind IN ('f','p')`
	}
	rows, err = r.pgOptionalObjectQuery(ctx, snap, neutral, "routine", "函数/过程元数据", "oidvectortypes", `SELECT p.proname,oidvectortypes(p.proargtypes),`+prokind+` FROM `+r.qcat("pg_proc")+` p JOIN `+r.qcat("pg_namespace")+` n ON n.oid=p.pronamespace WHERE n.nspname=$1 AND `+filter+` ORDER BY p.proname,p.oid`, r.schema)
	if err != nil {
		return nil, err
	}
	for rows != nil && rows.Next() {
		var n, args, k string
		if err = rows.Scan(&n, &args, &k); err != nil {
			rows.Close()
			return nil, err
		}
		snap.Objects = append(snap.Objects, PGObject{Kind: "routine", Name: n + "(" + args + ")", Reason: "函数/过程仅支持原始定义导出，不进行跨库转换"})
	}
	err = pgFinishOptionalRows(rows)
	if err != nil {
		if !neutral || failure.IsTaskError(err) {
			return nil, err
		}
		pgObjectMetadataFailure(snap, "routine", "函数/过程元数据", err)
	}
	rows, err = r.pgOptionalObjectQuery(ctx, snap, neutral, "view", "视图元数据", "pg_get_viewdef", `SELECT c.relname,pg_get_viewdef(c.oid,true) FROM `+r.qcat("pg_class")+` c JOIN `+r.qcat("pg_namespace")+` n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relkind='v' ORDER BY c.relname`, r.schema)
	if err != nil {
		return nil, err
	}
	for rows != nil && rows.Next() {
		var v ViewInfo
		var def sql.NullString
		if err = rows.Scan(&v.ViewName, &def); err != nil {
			rows.Close()
			return nil, err
		}
		if !def.Valid || strings.TrimSpace(def.String) == "" {
			if !partial {
				rows.Close()
				return nil, fmt.Errorf("视图 %s 定义不可读取", v.ViewName)
			}
			v.Unsupported = "视图定义不可读取"
		} else {
			v.Definition = strings.TrimRight(strings.TrimSpace(def.String), ";")
		}
		snap.Views = append(snap.Views, v)
	}
	err = pgFinishOptionalRows(rows)
	if err != nil {
		if !neutral || failure.IsTaskError(err) {
			return nil, err
		}
		pgObjectMetadataFailure(snap, "view", "视图元数据", err)
	}
	for i := range snap.Views {
		if snap.Views[i].Unsupported != "" {
			continue
		}
		info, e := r.pgColumnsForTarget(ctx, snap.Views[i].ViewName, neutral)
		if e != nil {
			if !partial || failure.IsTaskError(e) {
				return nil, e
			}
			snap.Views[i].Unsupported = e.Error()
			continue
		}
		snap.Views[i].Columns = info.Columns
	}
	if neutral {
		err = r.loadPGOracleSequences(ctx, snap, selected)
	} else {
		err = r.loadPGSequencesForTablesPartial(ctx, snap, selected, partial)
	}
	if err != nil {
		if !neutral || failure.IsTaskError(err) {
			return nil, err
		}
		pgObjectMetadataFailure(snap, "sequence", "序列元数据", err)
	}
	return snap, nil
}

func (r *PostgresReader) loadPGSequences(ctx context.Context, snap *PGSnapshot) error {
	return r.loadPGSequencesForTables(ctx, snap, nil)
}

func (r *PostgresReader) loadPGSequencesForTables(ctx context.Context, snap *PGSnapshot, selected map[string]bool) error {
	return r.loadPGSequencesForTablesPartial(ctx, snap, selected, false)
}
func (r *PostgresReader) loadPGSequencesForTablesPartial(ctx context.Context, snap *PGSnapshot, selected map[string]bool, partial bool) error {
	rows, err := r.db.QueryContext(ctx, `SELECT c.oid,n.nspname,c.relname FROM `+r.qcat("pg_class")+` c JOIN `+r.qcat("pg_namespace")+` n ON n.oid=c.relnamespace WHERE c.relkind='S' AND (n.nspname=$1 OR EXISTS (SELECT 1 FROM `+r.qcat("pg_depend")+` d JOIN `+r.qcat("pg_attrdef")+` a ON a.oid=d.objid JOIN `+r.qcat("pg_class")+` t ON t.oid=a.adrelid JOIN `+r.qcat("pg_namespace")+` tn ON tn.oid=t.relnamespace WHERE d.refobjid=c.oid AND tn.nspname=$1)) ORDER BY n.nspname,c.relname`, r.schema)
	if err != nil {
		return err
	}
	type seqID struct {
		oid          int64
		schema, name string
	}
	var ids []seqID
	for rows.Next() {
		var s seqID
		if err = rows.Scan(&s.oid, &s.schema, &s.name); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, s)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		var refs []pgSequenceReference
		if selected != nil || partial {
			var e error
			refs, e = r.pgSequenceReferences(ctx, id.oid)
			if e != nil {
				return e
			}
			relevant := false
			for _, ref := range refs {
				if ref.s == r.schema && (selected == nil || selected[ref.t]) {
					relevant = true
					break
				}
			}
			if !relevant {
				// Keep independent object diagnostics, but never acquire state-read
				// privileges on a sequence owned solely by an excluded root.
				if len(refs) == 0 && id.schema == r.schema {
					snap.Objects = append(snap.Objects, PGObject{Kind: "sequence", Name: id.name, Reason: "MySQL 不支持独立序列"})
				}
				continue
			}
		}
		seq := PGSequence{Name: id.name, Schema: id.schema}
		qn := QuotePGIdent(id.schema) + "." + QuotePGIdent(id.name)
		var last int64
		var called bool
		if err = r.db.QueryRowContext(ctx, `SELECT last_value,is_called FROM `+qn).Scan(&last, &called); err != nil {
			if !partial || failure.IsTaskError(err) || len(refs) == 0 {
				return fmt.Errorf("读取序列 %s: %w", qn, err)
			}
			markPGSequenceFailure(snap, refs, err)
			continue
		}
		if r.profile.Fields["pg_sequence.seqincrement"] {
			err = r.db.QueryRowContext(ctx, `SELECT seqincrement,seqmin,seqmax,seqcycle FROM `+r.qcat("pg_sequence")+` WHERE seqrelid=$1`, id.oid).Scan(&seq.Increment, &seq.Min, &seq.Max, &seq.Cycle)
		} else {
			err = r.db.QueryRowContext(ctx, `SELECT increment_by,min_value,max_value,is_cycled FROM `+qn).Scan(&seq.Increment, &seq.Min, &seq.Max, &seq.Cycle)
		}
		if err != nil {
			if !partial || failure.IsTaskError(err) || len(refs) == 0 {
				return fmt.Errorf("读取序列属性 %s: %w", qn, err)
			}
			markPGSequenceFailure(snap, refs, err)
			continue
		}
		seq.StartValue = last
		if called {
			if seq.Increment > 0 && last > seq.Max-seq.Increment {
				seq.Cycle = true
			} else {
				seq.StartValue += seq.Increment
			}
		}
		if selected == nil && !partial {
			refs, err = r.pgSequenceReferences(ctx, id.oid)
			if err != nil {
				return err
			}
		}
		seq.Shared = len(refs) != 1
		if len(refs) == 0 && id.schema == r.schema {
			snap.Objects = append(snap.Objects, PGObject{Kind: "sequence", Name: id.name, Reason: "MySQL 不支持独立序列"})
		}
		for _, ref := range refs {
			if ref.s != r.schema {
				continue
			}
			for i := range snap.Tables {
				t := &snap.Tables[i]
				if t.Info.TableName != ref.t {
					continue
				}
				copy := seq
				copy.TableName = ref.t
				copy.ColumnName = ref.c
				// Read only sequence catalog state here. Data maxima are collected
				// during copying, not by scanning business tables in preflight.
				t.Sequences = append(t.Sequences, copy)
			}
		}
	}
	return nil
}

type pgSequenceReference struct{ s, t, c string }

func (r *PostgresReader) pgSequenceReferences(ctx context.Context, oid int64) ([]pgSequenceReference, error) {
	// Preserve all references, including excluded roots and other schemas, so
	// selecting one consumer never turns a shared generator into an owned one.
	query := `SELECT n.nspname,t.relname,a.attname FROM ` + r.qcat("pg_depend") + ` d JOIN ` + r.qcat("pg_class") + ` t ON t.oid=d.refobjid JOIN ` + r.qcat("pg_namespace") + ` n ON n.oid=t.relnamespace JOIN ` + r.qcat("pg_attribute") + ` a ON a.attrelid=t.oid AND a.attnum=d.refobjsubid WHERE d.objid=$1 AND d.deptype IN ('a','i')
 UNION SELECT n.nspname,t.relname,a.attname FROM ` + r.qcat("pg_depend") + ` d JOIN ` + r.qcat("pg_attrdef") + ` ad ON ad.oid=d.objid JOIN ` + r.qcat("pg_class") + ` t ON t.oid=ad.adrelid JOIN ` + r.qcat("pg_namespace") + ` n ON n.oid=t.relnamespace JOIN ` + r.qcat("pg_attribute") + ` a ON a.attrelid=t.oid AND a.attnum=ad.adnum WHERE d.refobjid=$1 AND d.classid='pg_attrdef'::regclass`
	rows, err := r.db.QueryContext(ctx, query, oid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []pgSequenceReference
	for rows.Next() {
		var ref pgSequenceReference
		if err := rows.Scan(&ref.s, &ref.t, &ref.c); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

// A dependency on a sequence is not sufficient: nextval(seq)+1 is not an
// AUTO_INCREMENT default. Accept only the complete built-in call.
func pgSimpleNextval(raw string) bool {
	ts, err := sqlsyntax.Lex(raw)
	if err != nil {
		return false
	}
	if len(ts) >= 2 && ts[0].Value == "pg_catalog" && ts[1].Is(".") {
		ts = ts[2:]
	}
	return len(ts) == 6 && ts[0].Is("nextval") && ts[1].Is("(") && ts[2].Kind == 's' && ts[3].Is("::") && ts[4].Is("regclass") && ts[5].Is(")")
}

func (r *PostgresReader) markPGOperatorClasses(ctx context.Context, byName map[string]*PGTable) error {
	return r.markPGOperatorClassesForTarget(ctx, byName, false)
}

func (r *PostgresReader) markPGOperatorClassesForTarget(ctx context.Context, byName map[string]*PGTable, neutral bool) error {
	// Source collations are intentionally ignored; MySQL inherits its own defaults.
	// Non-default operator classes still require semantic adaptation.
	rows, err := r.db.QueryContext(ctx, `SELECT t.relname,i.relname FROM `+r.qcat("pg_index")+` ix JOIN `+r.qcat("pg_class")+` t ON t.oid=ix.indrelid JOIN `+r.qcat("pg_class")+` i ON i.oid=ix.indexrelid JOIN `+r.qcat("pg_namespace")+` n ON n.oid=t.relnamespace WHERE n.nspname=$1 AND EXISTS (SELECT 1 FROM `+r.qcat("pg_opclass")+` op WHERE op.oid=ANY(ix.indclass) AND NOT op.opcdefault)`, r.schema)
	if err != nil {
		return err
	}
	for rows.Next() {
		var t, n string
		if err = rows.Scan(&t, &n); err != nil {
			rows.Close()
			return err
		}
		if table := byName[t]; table != nil {
			for j := range table.Indexes {
				if table.Indexes[j].IndexName == n {
					if neutral {
						if table.Indexes[j].PG == nil {
							table.Indexes[j].PG = &PGIndexMetadata{}
						}
						table.Indexes[j].PG.NonDefaultOpClass = true
					} else {
						table.Indexes[j].Unsupported = strings.TrimPrefix(table.Indexes[j].Unsupported+"、非默认操作符类", "、")
					}
				}
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	return nil
}

func markPGSequenceFailure(snap *PGSnapshot, refs []pgSequenceReference, err error) {
	for _, ref := range refs {
		for i := range snap.Tables {
			t := &snap.Tables[i]
			if t.Info.TableName == ref.t {
				t.Unsupported = append(t.Unsupported, PGObject{Kind: "sequence", Table: ref.t, Name: ref.c, Reason: err.Error()})
			}
		}
	}
}
