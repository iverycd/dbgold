package damengnative

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// ResolveSchema uses catalog spelling. Only an exact match or one unambiguous
// unquoted-name match is accepted; quoted, mixed-case owners are never folded.
func ResolveSchema(ctx context.Context, db *sql.DB, schema string) (string, error) {
	if schema == "" {
		if err := db.QueryRowContext(ctx, `SELECT CURRENT_SCHEMA`).Scan(&schema); err != nil {
			return "", err
		}
	}
	rows, err := db.QueryContext(ctx, `SELECT NAME FROM SYS.SYSOBJECTS WHERE TYPE$='SCH' ORDER BY NAME`)
	if err != nil {
		return "", fmt.Errorf("读取达梦 schema: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err = rows.Scan(&n); err != nil {
			return "", err
		}
		names = append(names, n)
	}
	if err = rows.Err(); err != nil {
		return "", err
	}
	for _, n := range names {
		if n == schema {
			return n, nil
		}
	}
	var folded []string
	for _, n := range names {
		if strings.EqualFold(n, schema) {
			folded = append(folded, n)
		}
	}
	if len(folded) == 1 {
		return folded[0], nil
	}
	if len(folded) == 0 {
		return "", fmt.Errorf("达梦 schema %q 不存在或不可见", schema)
	}
	return "", fmt.Errorf("达梦 schema %q 的大小写匹配不唯一，请填写目录中的准确名称", schema)
}

// ReadProfile returns only database identity and semantic settings, never a DSN.
func ReadProfile(ctx context.Context, db *sql.DB, schema string) (Profile, error) {
	p := Profile{Schema: schema, Settings: map[string]string{}}
	var server, dictionary, build, instance, host string
	if err := db.QueryRowContext(ctx, `SELECT SVR_VERSION,DB_VERSION,BUILD_VERSION,INSTANCE_NAME,HOST_NAME FROM V$INSTANCE`).Scan(&server, &dictionary, &build, &instance, &host); err != nil {
		// Older DM8 builds expose fewer version columns. Probe the actual row
		// shape instead of interpreting an optional field as a broken database.
		values, probeErr := profileRow(ctx, db, `SELECT * FROM V$INSTANCE`)
		if probeErr != nil {
			return p, fmt.Errorf("读取达梦版本/实例: %w", probeErr)
		}
		server, dictionary, build, instance, host = values["SVR_VERSION"], values["DB_VERSION"], values["BUILD_VERSION"], values["INSTANCE_NAME"], values["HOST_NAME"]
		if instance == "" {
			instance = values["NAME"]
		}
		if server == "" || instance == "" || host == "" {
			return p, fmt.Errorf("达梦实例目录缺少版本、实例名或主机身份")
		}
	}
	p.Version = strings.Join([]string{server, dictionary, build}, " | ")
	var magic string
	if err := db.QueryRowContext(ctx, `SELECT CAST(DB_MAGIC AS VARCHAR(80)) FROM V$RLOG`).Scan(&magic); err != nil {
		return p, fmt.Errorf("读取达梦数据库身份: %w", err)
	}
	p.Database = magic
	var port string
	rows, err := db.QueryContext(ctx, `SELECT PARA_NAME,PARA_VALUE FROM V$DM_INI WHERE PARA_NAME IN ('COMPATIBLE_MODE','IFUN_DATETIME_MODE','PK_WITH_CLUSTER','NUMBER_MODE','LENGTH_IN_CHAR','CHARACTER_LENGTH_IN_CHAR','BLANK_PAD_MODE','AUTO_INCREMENT_INCREMENT','AUTO_INCREMENT_OFFSET','NO_AUTO_VALUE_ON_ZERO','PORT_NUM') ORDER BY PARA_NAME`)
	if err != nil {
		return p, fmt.Errorf("读取达梦兼容参数: %w", err)
	}
	for rows.Next() {
		var n, v string
		if err = rows.Scan(&n, &v); err != nil {
			rows.Close()
			return p, err
		}
		if n == "PORT_NUM" {
			port = v
		} else {
			p.Settings[n] = v
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return p, err
	}
	if _, ok := p.Settings["COMPATIBLE_MODE"]; !ok {
		return p, fmt.Errorf("达梦 COMPATIBLE_MODE 不可见")
	}
	var charset, caseSensitive, page string
	if err = db.QueryRowContext(ctx, `SELECT CAST(SF_GET_UNICODE_FLAG() AS VARCHAR(20)),CAST(SF_GET_CASE_SENSITIVE_FLAG() AS VARCHAR(20)),CAST(PAGE() AS VARCHAR(20))`).Scan(&charset, &caseSensitive, &page); err != nil {
		return p, fmt.Errorf("读取达梦字符集/大小写/页大小: %w", err)
	}
	p.Settings["CHARSET"], p.Settings["CASE_SENSITIVE"], p.Settings["PAGE_SIZE"] = charset, caseSensitive, page
	p.Instance = Fingerprint([]string{host, instance, magic, port})
	return p, nil
}

func profileRow(ctx context.Context, db *sql.DB, query string) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	if !rows.Next() {
		if err = rows.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("实例目录为空")
	}
	values := make([]any, len(columns))
	pointers := make([]any, len(columns))
	for i := range pointers {
		pointers[i] = &values[i]
	}
	if err = rows.Scan(pointers...); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for i, c := range columns {
		if values[i] == nil {
			continue
		}
		if b, ok := values[i].([]byte); ok {
			out[strings.ToUpper(c)] = string(b)
		} else {
			out[strings.ToUpper(c)] = fmt.Sprint(values[i])
		}
	}
	return out, rows.Err()
}

type catalogObject struct {
	id, pid                 int64
	name, main, sub, status string
	info1, info2, info3     int64
}
type catalogReader struct {
	ctx          context.Context
	db           *sql.DB
	schema       string
	snapshot     *Snapshot
	raw          map[int64]catalogObject
	byRaw        map[int64][]int
	index        map[string]int
	constraints  map[string]string
	definitions  bool
	verification bool
}

// ReadCatalog inventories every schema-owned dictionary object before extracting
// definitions. A failed extraction remains an explicit issue, never an omission.
// definitions=false is the target inventory path and performs no source scans.
func ReadCatalog(ctx context.Context, db *sql.DB, schema string, definitions bool) (*Snapshot, error) {
	return readCatalog(ctx, db, schema, definitions, false)
}

// ReadCatalogForVerification reads native definitions without synthesizing a
// second source migration plan from already imported MV staging objects.
func ReadCatalogForVerification(ctx context.Context, db *sql.DB, schema string) (*Snapshot, error) {
	return readCatalog(ctx, db, schema, true, true)
}

func readCatalog(ctx context.Context, db *sql.DB, schema string, definitions, verification bool) (*Snapshot, error) {
	profile, err := ReadProfile(ctx, db, schema)
	if err != nil {
		return nil, err
	}
	r := &catalogReader{ctx: ctx, db: db, schema: schema, snapshot: &Snapshot{Profile: profile}, raw: map[int64]catalogObject{}, byRaw: map[int64][]int{}, index: map[string]int{}, constraints: map[string]string{}, definitions: definitions}
	r.verification = verification
	if err = r.inventory(); err != nil {
		return nil, err
	}
	if err = r.classifyMaterializedViews(); err != nil {
		return nil, err
	}
	if err = r.readConstraints(); err != nil {
		return nil, err
	}
	if err = r.readMaterializedLogs(); err != nil {
		return nil, err
	}
	if !definitions {
		if err = r.checkActiveEventTriggers(); err != nil {
			return nil, err
		}
	}
	if err = r.readDependencies(); err != nil {
		return nil, err
	}
	for i := range r.snapshot.Objects {
		_, _, r.snapshot.Objects[i].DropSQL, r.snapshot.Objects[i].CompileSQL = ddlOptions(r.snapshot.Objects[i])
	}
	if definitions {
		r.extractDefinitions()
		if !verification {
			if err = r.prepareMaterializedHistory(); err != nil {
				r.issue(nil, err.Error())
			}
		}
	}
	if err = r.readTables(); err != nil {
		return nil, err
	}
	r.materializedHistoryTables()
	r.deferMaterializedEmptyIndexes()
	if err = r.readCommentsAndGrants(); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return r.snapshot, nil
}

func (r *catalogReader) issue(o *Object, reason string) {
	i := Issue{Severity: "error", Reason: reason}
	if o != nil {
		i.ObjectID, i.Kind, i.Name = o.ID, o.Kind, o.Name
	}
	r.snapshot.Issues = append(r.snapshot.Issues, i)
}
func (r *catalogReader) add(rawID int64, o Object) int {
	key := o.Kind + "\x00" + o.Name
	if i, ok := r.index[key]; ok {
		if rawID != 0 {
			found := false
			for _, known := range r.byRaw[rawID] {
				if known == i {
					found = true
				}
			}
			if !found {
				r.byRaw[rawID] = append(r.byRaw[rawID], i)
			}
		}
		return i
	}
	o.ID = ObjectID(o.Schema, o.Kind, o.Name)
	if o.Dependencies == nil {
		o.Dependencies = []string{}
	}
	i := len(r.snapshot.Objects)
	r.snapshot.Objects = append(r.snapshot.Objects, o)
	r.index[key] = i
	if rawID != 0 {
		r.byRaw[rawID] = append(r.byRaw[rawID], i)
	}
	return i
}
func nativeKind(sub string, info int64) string {
	switch sub {
	case "UTAB":
		return "table"
	case "STAB":
		return "system_table"
	case "VIEW":
		return "view"
	case "PROC":
		if info&1 != 0 {
			return "procedure"
		}
		return "function"
	case "SEQ":
		return "sequence"
	case "PKG":
		return "package"
	case "TRIG":
		return "trigger"
	case "DBLINK":
		return "db_link"
	case "SYNOM":
		return "synonym"
	case "CLASS", "TYPE", "JCLASS", "DOMAIN", "CONTEXT":
		return strings.ToLower(sub)
	case "CLLT":
		return "type"
	case "INDEX":
		return "index"
	case "CNTIND":
		return "context_index"
	case "CONS":
		return "constraint"
	default:
		return strings.ToLower(sub)
	}
}
func normalizedKind(kind string) string {
	switch strings.ToUpper(kind) {
	case "DATABASE LINK", "DB LINK", "DBLINK":
		return "db_link"
	case "MATERIALIZED VIEW":
		return "materialized_view"
	case "MATERIALIZED VIEW LOG":
		return "materialized_view_log"
	case "PACKAGE BODY", "PACKAGE_BODY":
		return "package_body"
	case "TYPE BODY", "CLASS BODY", "CLASS_BODY":
		return "class_body"
	case "TABLE PARTITION", "TABLE SUBPARTITION":
		return "table_partition"
	case "INDEX PARTITION", "INDEX SUBPARTITION":
		return "index_partition"
	case "CONTEXT INDEX":
		return "context_index"
	default:
		return strings.ReplaceAll(strings.ToLower(kind), " ", "_")
	}
}

func (r *catalogReader) inventory() error {
	rows, err := r.db.QueryContext(r.ctx, `SELECT o.ID,o.NAME,o.TYPE$,o.SUBTYPE$,o.PID,o.VALID,o.INFO1,o.INFO2,o.INFO3 FROM SYS.SYSOBJECTS o JOIN SYS.SYSOBJECTS s ON o.SCHID=s.ID WHERE s.TYPE$='SCH' AND s.NAME=? ORDER BY o.ID`, r.schema)
	if err != nil {
		return fmt.Errorf("无法完整盘点达梦 schema 对象: %w", err)
	}
	for rows.Next() {
		var o catalogObject
		var pid, info1, info2, info3 sql.NullInt64
		var status sql.NullString
		if err = rows.Scan(&o.id, &o.name, &o.main, &o.sub, &pid, &status, &info1, &info2, &info3); err != nil {
			rows.Close()
			return err
		}
		o.pid, o.status, o.info1, o.info2, o.info3 = pid.Int64, status.String, info1.Int64, info2.Int64, info3.Int64
		r.raw[o.id] = o
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	type compatible struct {
		id                 int64
		name, kind, status string
	}
	var compat []compatible
	rows, err = r.db.QueryContext(r.ctx, `SELECT OBJECT_ID,OBJECT_NAME,OBJECT_TYPE,STATUS FROM ALL_OBJECTS WHERE OWNER=? ORDER BY OBJECT_ID,OBJECT_TYPE`, r.schema)
	if err != nil {
		return fmt.Errorf("读取达梦兼容对象清单: %w", err)
	}
	for rows.Next() {
		var c compatible
		var status sql.NullString
		if err = rows.Scan(&c.id, &c.name, &c.kind, &status); err != nil {
			rows.Close()
			return err
		}
		c.kind, c.status = normalizedKind(c.kind), status.String
		compat = append(compat, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	ids := make([]int64, 0, len(r.raw))
	for id := range r.raw {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		raw := r.raw[id]
		kind := nativeKind(raw.sub, raw.info1)
		status := raw.status
		for _, c := range compat {
			if c.id == id && c.kind != "package_body" && c.kind != "class_body" && c.kind != "sch" {
				kind, status = c.kind, c.status
				break
			}
		}
		event := raw.sub == "TRIG" && raw.info1&2 == 0
		r.add(id, Object{Kind: kind, Name: raw.name, Schema: r.schema, Status: status, EventTrigger: event})
	}
	for _, c := range compat {
		if c.kind == "sch" || c.kind == "schema" {
			continue
		}
		if c.kind == "package_body" || c.kind == "class_body" {
			i := r.add(c.id, Object{Kind: c.kind, Name: c.name, Schema: r.schema, Status: c.status})
			if parent := r.base(c.id); parent != nil {
				r.snapshot.Objects[i].ParentID = parent.ID
				r.snapshot.Objects[i].Dependencies = append(r.snapshot.Objects[i].Dependencies, parent.ID)
			}
		} else if _, ok := r.raw[c.id]; !ok {
			i := r.add(c.id, Object{Kind: c.kind, Name: c.name, Schema: r.schema, Status: c.status})
			r.issue(&r.snapshot.Objects[i], "对象在 ALL_OBJECTS 可见但原生字典缺失，无法证明盘点完整")
		}
	}
	for id, positions := range r.byRaw {
		raw := r.raw[id]
		for _, i := range positions {
			o := &r.snapshot.Objects[i]
			// DM's identity DROP/ADD creates a native sequence owned by the
			// table in INFO2; it is deliberately absent from ALL_SEQUENCES.
			// The dictionary relation, never an SEQ_ name prefix, proves that
			// replaying the table's identity restores this derived object.
			if raw.sub == "SEQ" && raw.info2 > 0 {
				if owner, ok := r.raw[raw.info2]; ok && owner.sub == "UTAB" {
					if parent := r.base(owner.id); parent != nil {
						o.Managed = true
						o.Generated = true
						o.ParentID = parent.ID
						o.Table = parent.Name
						o.Dependencies = appendUnique(o.Dependencies, parent.ID)
					}
				}
			}
			if raw.pid > 0 {
				if parent := r.base(raw.pid); parent != nil && parent.ID != o.ID {
					o.ParentID = parent.ID
					o.Table = parent.Name
					o.Dependencies = appendUnique(o.Dependencies, parent.ID)
					if o.Kind == "table" && (parent.Kind == "table" || parent.Kind == "index" || parent.Kind == "context_index" || parent.Kind == "materialized_view") {
						o.Managed = true
						o.Generated = true
					}
				}
			}
			if o.Kind == "table_partition" || o.Kind == "index_partition" {
				if o.ParentID == "" {
					r.issue(o, "分区对象缺少可确认的父对象，不能独立重建")
				}
				o.Managed = true
			}
			if o.Kind == "system_table" {
				r.issue(o, "所选 schema 包含系统表，不能当普通用户表迁移")
			}
		}
	}
	return nil
}
func (r *catalogReader) base(rawID int64) *Object {
	for _, i := range r.byRaw[rawID] {
		o := &r.snapshot.Objects[i]
		if o.Kind != "package_body" && o.Kind != "class_body" {
			return o
		}
	}
	return nil
}
func appendUnique(ss []string, s string) []string {
	for _, v := range ss {
		if s == v {
			return ss
		}
	}
	return append(ss, s)
}

func (r *catalogReader) readConstraints() error {
	rows, err := r.db.QueryContext(r.ctx, `SELECT CONSTRAINT_NAME,TABLE_NAME,CONSTRAINT_TYPE,INDEX_NAME,STATUS,VALIDATED FROM ALL_CONSTRAINTS WHERE OWNER=? ORDER BY TABLE_NAME,CONSTRAINT_NAME`, r.schema)
	if err != nil {
		return fmt.Errorf("读取达梦约束/内部索引归属: %w", err)
	}
	for rows.Next() {
		var name, table, kind string
		var idx, status, validated sql.NullString
		if err = rows.Scan(&name, &table, &kind, &idx, &status, &validated); err != nil {
			rows.Close()
			return err
		}
		r.constraints[name] = kind
		parent := ObjectID(r.schema, "table", table)
		if owner := r.namedObject(table); owner != nil {
			parent = owner.ID
		}
		i, ok := r.index["constraint\x00"+name]
		if !ok {
			i = r.add(0, Object{Kind: "constraint", Name: name, Schema: r.schema, Table: table, ParentID: parent, Status: status.String})
		}
		o := &r.snapshot.Objects[i]
		o.Table, o.ParentID = table, parent
		o.Dependencies = appendUnique(o.Dependencies, parent)
		o.Managed = kind != "R"
		o.ConstraintType = kind
		o.ConstraintIndex = idx.String
		switch strings.ToUpper(status.String) {
		case "N", "0", "DISABLE", "DISABLED":
			o.Status = "DISABLED"
		case "Y", "1", "ENABLE", "ENABLED":
			o.Status = "ENABLED"
		}
		o.ConstraintNoValidate = o.Status == "ENABLED" && !strings.EqualFold(validated.String, "VALIDATED")
		for id, raw := range r.raw {
			if raw.sub == "CONS" && raw.name == name && id >= 134217728 && id <= 150994943 && name == "CONS"+strconv.FormatInt(id, 10) {
				o.Generated = true
			}
		}
		if idx.Valid && (kind == "P" || kind == "U") {
			if j, ok := r.index["index\x00"+idx.String]; ok {
				r.snapshot.Objects[j].Managed = true
				r.snapshot.Objects[j].ParentID = parent
				r.snapshot.Objects[j].Table = table
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	// Clustered physical indexes belong to the table recipe. Their names are
	// disposable only when the native catalog proves a generated identity; a
	// user-created clustered index still needs its original definition.
	rows, err = r.db.QueryContext(r.ctx, `SELECT i.NAME,t.NAME,x.FLAG,x.XTYPE FROM SYS.SYSINDEXES x JOIN SYS.SYSOBJECTS i ON i.ID=x.ID JOIN SYS.SYSOBJECTS t ON t.ID=i.PID JOIN SYS.SYSOBJECTS s ON s.ID=t.SCHID WHERE s.NAME=? AND ((x.FLAG & 1)=1 OR x.XTYPE=0)`, r.schema)
	if err != nil {
		return fmt.Errorf("无法确认系统/聚集索引的父对象: %w", err)
	}
	for rows.Next() {
		var name, table string
		var flags, xtype int64
		if err = rows.Scan(&name, &table, &flags, &xtype); err != nil {
			rows.Close()
			return err
		}
		if j, ok := r.index["index\x00"+name]; ok {
			r.snapshot.Objects[j].Managed = true
			// FLAG's low bit is also set when a user-created index is
			// attached to a primary key. Only its exact dictionary-assigned
			// name and native object-ID range prove a disposable name.
			generated := false
			for id, raw := range r.raw {
				if raw.sub == "INDEX" && raw.name == name && id >= 33554432 && id <= 50331647 && name == "INDEX"+strconv.FormatInt(id, 10) {
					generated = true
					break
				}
			}
			r.snapshot.Objects[j].Generated = generated
			if parent := r.namedObject(table); parent != nil {
				r.snapshot.Objects[j].ParentID = parent.ID
			} else {
				for _, parent := range r.snapshot.Objects {
					if parent.Name == table && (parent.Kind == "table_partition" || parent.Kind == "index_partition") {
						r.snapshot.Objects[j].ParentID = parent.ID
						break
					}
				}
				if r.snapshot.Objects[j].ParentID == "" {
					r.snapshot.Objects[j].ParentID = ObjectID(r.schema, "table", table)
				}
			}
			r.snapshot.Objects[j].Table = table
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if err := r.markPartitionDerivedIndexes(); err != nil {
		return err
	}
	return r.markPartitionDerivedConstraints()
}

func (r *catalogReader) readDependencies() error {
	rows, err := r.db.QueryContext(r.ctx, `SELECT d.ID,d.TYPE$,d.REFED_ID,d.REFED_TYPE$,s.NAME,o.NAME FROM SYS.SYSDEPENDENCIES d JOIN SYS.SYSOBJECTS o ON o.ID=d.REFED_ID LEFT JOIN SYS.SYSOBJECTS s ON s.ID=o.SCHID WHERE d.ID IN (SELECT o.ID FROM SYS.SYSOBJECTS o JOIN SYS.SYSOBJECTS s ON s.ID=o.SCHID WHERE s.NAME=?) ORDER BY d.ID,d.REFED_ID`, r.schema)
	if err != nil {
		return fmt.Errorf("无法完整读取达梦对象依赖: %w", err)
	}
	for rows.Next() {
		var id, ref int64
		var kind, refKind, name string
		var owner sql.NullString
		if err = rows.Scan(&id, &kind, &ref, &refKind, &owner, &name); err != nil {
			rows.Close()
			return err
		}
		for _, i := range r.byRaw[id] {
			o := &r.snapshot.Objects[i]
			if parent := r.base(ref); parent != nil {
				if parent.ID != o.ID {
					o.Dependencies = appendUnique(o.Dependencies, parent.ID)
				}
			} else if owner.Valid && owner.String != r.schema && owner.String != "SYS" {
				o.External = append(o.External, Reference{Schema: owner.String, Name: name, Kind: normalizedKind(refKind)})
			}
		}
	}
	err = rows.Err()
	rows.Close()
	return err
}

func (r *catalogReader) readTables() error {
	for _, o := range r.snapshot.Objects {
		if o.Kind != "table" || o.Managed {
			continue
		}
		if o.Generated && o.Table == "" && (strings.HasPrefix(o.Name, "DBGOLD_MV_RID_") || strings.HasPrefix(o.Name, "DBGOLD_MV_BUF_") || strings.HasPrefix(o.Name, "DBGOLD_MV_LBUF_") || strings.HasPrefix(o.Name, "DBGOLD_MV_REDEF_")) {
			continue
		}
		t := Table{ObjectID: o.ID, Name: o.Name, Schema: r.schema}
		sourceName := o.Name
		if o.Table != "" && strings.HasPrefix(o.Name, "DBGOLD_MV_") {
			sourceName = o.Table
			t.SourceName = sourceName
		}
		query := `SELECT c.COLUMN_NAME,c.DATA_TYPE,NVL(c.DATA_PRECISION,0),NVL(c.DATA_SCALE,0),sc.INFO2,NVL(ci.INFO1,0),c.DATA_TYPE_OWNER FROM ALL_TAB_COLUMNS c JOIN SYS.SYSOBJECTS o ON o.NAME=c.TABLE_NAME AND o.SCHID=(SELECT ID FROM SYS.SYSOBJECTS WHERE TYPE$='SCH' AND NAME=c.OWNER) JOIN SYS.SYSCOLUMNS sc ON sc.ID=o.ID AND sc.NAME=c.COLUMN_NAME LEFT JOIN SYS.SYSCOLINFOS ci ON ci.ID=o.ID AND ci.COLID=sc.COLID WHERE c.OWNER=? AND c.TABLE_NAME=? ORDER BY c.COLUMN_ID`
		rows, err := r.db.QueryContext(r.ctx, query, r.schema, sourceName)
		if err != nil { // DATA_TYPE_OWNER is absent in some DM8 dictionary builds;
			// ordinary scalar tables remain usable, and unknown CLASSn types will
			// fail the target's explicit type capability check.
			rows, err = r.db.QueryContext(r.ctx, strings.Replace(query, "c.DATA_TYPE_OWNER", "CAST(NULL AS VARCHAR(128))", 1), r.schema, sourceName)
		}
		if err != nil {
			r.issue(&o, "列元数据读取失败: "+err.Error())
			continue
		}
		for rows.Next() {
			var c Column
			var flags, extra sql.NullInt64
			var owner sql.NullString
			if err = rows.Scan(&c.Name, &c.DataType, &c.Precision, &c.Scale, &flags, &extra, &owner); err != nil {
				break
			}
			c.Writable = extra.Int64&1 == 0
			c.Identity = flags.Int64&1 != 0 || declaredNativeIdentity(o, c.Name)
			c.SQLType = columnSQLType(c)
			if owner.Valid && owner.String != "" && owner.String != "SYS" {
				c.TypeSchema = owner.String
				c.TypeName = c.DataType
				c.SQLType = Qualified(c.TypeSchema, c.TypeName)
			}
			// DM dictionaries can expose CLASS<n> rather than the declared UDT
			// name. The untouched native column declaration is authoritative.
			if schema, name, ok := declaredNativeType(o, c.Name, r.schema, r.snapshot.Objects); ok {
				c.TypeSchema, c.TypeName = schema, name
				c.SQLType = Qualified(schema, name)
			}
			t.Columns = append(t.Columns, c)
		}
		scanErr := rows.Err()
		rows.Close()
		if err != nil {
			r.issue(&o, "列元数据读取失败: "+err.Error())
			continue
		}
		if scanErr != nil {
			r.issue(&o, "列元数据读取失败: "+scanErr.Error())
			continue
		}
		if len(t.Columns) == 0 {
			r.issue(&o, "没有可见列元数据")
			continue
		}
		if i, ok := r.index[o.Kind+"\x00"+o.Name]; ok {
			if err = restoreNativeXMLAttributes(&r.snapshot.Objects[i], &t); err != nil {
				r.issue(&r.snapshot.Objects[i], "XMLTYPE 原生列属性恢复失败: "+err.Error())
			}
		}
		if err = r.freezeNativeCopyDefaults(o, &t, sourceName); err != nil {
			r.issue(&o, "原生复制默认值读取失败: "+err.Error())
		}
		if r.definitions {
			if err = r.db.QueryRowContext(r.ctx, `SELECT COUNT(*) FROM `+Qualified(r.schema, sourceName)).Scan(&t.Rows); err != nil {
				r.issue(&o, "源数据行数不可读: "+err.Error())
			}
			if !r.immutableMaterializedStaging(t.Name) {
				r.identityRestore(&o, &t)
			}
			r.validateLOBLengths(&o, &t, sourceName)
			if i, ok := r.index[o.Kind+"\x00"+o.Name]; ok && !r.verification {
				if err = r.freezeNativeBFiles(&r.snapshot.Objects[i], t, sourceName); err != nil {
					r.issue(&r.snapshot.Objects[i], "BFILE 外部资源读取失败: "+err.Error())
				}
			}
		} else {
			t.Rows = -1
		}
		if r.definitions && !r.verification && t.SourceName == "" && !o.Generated {
			if i, ok := r.index["table\x00"+o.Name]; ok {
				if err = r.freezeIntervalPartitions(&r.snapshot.Objects[i], t); err != nil {
					r.issue(&r.snapshot.Objects[i], "间隔分区恢复计划提取失败: "+err.Error())
				}
			}
		}
		r.snapshot.Tables = append(r.snapshot.Tables, t)
	}
	return nil
}

func (r *catalogReader) validateLOBLengths(o *Object, t *Table, sourceName string) {
	for _, c := range t.Columns {
		if !c.Writable || c.TypeSchema != "" {
			continue
		}
		switch strings.ToUpper(c.DataType) {
		case "BLOB", "IMAGE", "LONGVARBINARY", "CLOB", "NCLOB", "TEXT", "LONG", "LONGVARCHAR":
		default:
			continue
		}
		var length sql.NullInt64
		query := "SELECT MAX(DBMS_LOB.GETLENGTH(" + Quote(c.Name) + ")) FROM " + Qualified(r.schema, sourceName)
		if err := r.db.QueryRowContext(r.ctx, query).Scan(&length); err != nil {
			r.issue(o, "列 "+c.Name+" 的 LOB 长度无法只读核验: "+err.Error())
			continue
		}
		if length.Valid && (length.Int64 < 0 || length.Int64 > math.MaxInt32) {
			r.issue(o, fmt.Sprintf("列 %s 的 LOB 长度 %d 超过当前 DM Go 驱动无损读取范围 %d", c.Name, length.Int64, math.MaxInt32))
		}
	}
}

func columnSQLType(c Column) string {
	if nativeXMLColumn(c) {
		return "XMLTYPE"
	}
	base := strings.ToUpper(strings.TrimSpace(c.DataType))
	if strings.Contains(base, "(") {
		return c.DataType
	}
	switch base {
	case "NUMBER", "DEC", "DECIMAL", "NUMERIC":
		if c.Precision > 0 {
			return fmt.Sprintf("%s(%d,%d)", base, c.Precision, c.Scale)
		}
	case "TIMESTAMP", "TIME", "DATETIME":
		return fmt.Sprintf("%s(%d)", base, c.Scale)
	case "TIMESTAMP WITH TIME ZONE":
		return fmt.Sprintf("TIMESTAMP(%d) WITH TIME ZONE", c.Scale)
	case "TIME WITH TIME ZONE":
		return fmt.Sprintf("TIME(%d) WITH TIME ZONE", c.Scale)
	case "TIMESTAMP WITH LOCAL TIME ZONE":
		return fmt.Sprintf("TIMESTAMP(%d) WITH LOCAL TIME ZONE", c.Scale)
	}
	return c.DataType
}

func (r *catalogReader) identityRestore(o *Object, t *Table) {
	for _, c := range t.Columns {
		if !c.Identity {
			continue
		}
		var seed, increment, current sql.NullString
		q := `SELECT CAST(IDENT_SEED(?) AS VARCHAR(80)),CAST(IDENT_INCR(?) AS VARCHAR(80)),CAST(IDENT_CURRENT(?) AS VARCHAR(80))`
		name := Qualified(r.schema, t.Name)
		sourceName := name
		if t.SourceName != "" {
			sourceName = Qualified(r.schema, t.SourceName)
		}
		if err := r.db.QueryRowContext(r.ctx, q, sourceName, sourceName, sourceName).Scan(&seed, &increment, &current); err != nil {
			r.issue(o, "自增列状态读取失败: "+err.Error())
			return
		}
		s, e1 := strconv.ParseInt(seed.String, 10, 64)
		step, e2 := strconv.ParseInt(increment.String, 10, 64)
		cur, e3 := strconv.ParseInt(current.String, 10, 64)
		if e1 != nil || e2 != nil || e3 != nil || step == 0 {
			r.issue(o, "自增列种子/步长/当前值不可精确读取")
			return
		}
		reportedCurrent := cur
		allocatorTable := t.Name
		if t.SourceName != "" {
			allocatorTable = t.SourceName
		}
		cur, err := r.partitionIdentityCurrent(allocatorTable, c.Name, cur, step)
		if err != nil {
			r.issue(o, "分区自增列活动序列读取失败: "+err.Error())
			return
		}
		next := cur + step
		if (step > 0 && next < cur) || (step < 0 && next > cur) {
			r.issue(o, "自增列下一值溢出 int64")
			return
		}
		t.Restore = append(t.Restore, Unit{SQL: "ALTER TABLE " + name + " DROP IDENTITY", Phase: "restore"}, Unit{SQL: fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s IDENTITY(%d, %d)", name, Quote(c.Name), next, step), Phase: "restore"})
		if next != s {
			reason := fmt.Sprintf("自增列 %s 保留原种子 %d、步长 %d、当前值 %d 的审计信息；目标用下一值 %d 作为续号种子", c.Name, s, step, reportedCurrent, next)
			if reportedCurrent != cur {
				reason += "；已按唯一原生活动序列及已有值对齐原步长级数，可能保守跳号"
			}
			if t.Rows == 0 && cur == s {
				// IDENT_CURRENT returns the seed both before any allocation
				// and after that first value has been used and deleted. Public
				// read-only metadata cannot distinguish these states. Never
				// guess that a consumed value is safe to allocate again.
				reason += "；源空表的当前值等于种子，无法只读区分首值是否已消费；采用安全续号边界，可能跳过尚未使用的首值"
			}
			r.snapshot.Issues = append(r.snapshot.Issues, Issue{ObjectID: o.ID, Kind: o.Kind, Name: o.Name, Severity: "warning", Reason: reason})
		}
	}
}
