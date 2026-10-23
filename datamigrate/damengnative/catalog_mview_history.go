package damengnative

import (
	"fmt"
	"sort"
	"strings"
)

// MaterializedHistorySQL is a frozen group descriptor, not an independently
// executable refresh. Only the native writer's history operation may execute it.
func MaterializedHistorySQL(schema, leader string, plan *MaterializedHistoryPlan) string {
	return MaterializedBaselineSQL(schema, leader) + " /* DBGOLD_MV_HISTORY " + Fingerprint(plan) + " */"
}

// prepareMaterializedHistory runs only after all native definitions have been
// read. A stale view pulls every FAST/FORCE view into one ordered group: shared
// logs must retain different per-view cutoffs, including a current sibling.
func (r *catalogReader) prepareMaterializedHistory() error {
	stale := false
	type refreshInfo struct{ last, state string }
	info := map[string]refreshInfo{}
	for i, o := range r.snapshot.Objects {
		if o.Materialized == nil || !o.Materialized.Baseline {
			continue
		}
		var native refreshInfo
		if err := r.db.QueryRowContext(r.ctx, `SELECT TO_CHAR(LAST_REFRESH_DATE,'YYYY-MM-DD HH24:MI:SS.FF6'),STALENESS FROM SYS.USER_MVIEWS WHERE SCHID=(SELECT ID FROM SYS.SYSOBJECTS WHERE TYPE$='SCH' AND NAME=?) AND MVIEW_NAME=?`, r.schema, o.Name).Scan(&native.last, &native.state); err != nil {
			return fmt.Errorf("物化视图 %s 原增量刷新水位不可读取: %w", o.Name, err)
		}
		if native.last == "" {
			return fmt.Errorf("物化视图 %s 原增量刷新水位为空", o.Name)
		}
		info[o.ID] = native
		r.snapshot.Objects[i].Materialized.LastRefresh = native.last
		if native.last <= "1900-01-01 00:00:00.000000" {
			r.snapshot.Objects[i].Materialized.Baseline = false
			r.snapshot.Objects[i].Materialized.Staleness = "UNUSEABLE"
			units := r.snapshot.Objects[i].Units[:0]
			for _, u := range r.snapshot.Objects[i].Units {
				if u.Phase != "mv_prepare" && u.Phase != "mv_rebase" {
					units = append(units, u)
				}
			}
			r.snapshot.Objects[i].Units = units
			continue
		}
		// Native NEEDS_COMPILE can follow an UPDATE that left every value
		// unchanged. Equal output rows do not prove the original DML/log epoch
		// is FRESH; preserve that state through the same history recipe.
		stale = stale || o.Materialized.Staleness == "STALE" || native.state == "NEEDS_COMPILE" || native.state == "NEEDS_FULL_REFRESH"
	}
	if !stale {
		issues := r.snapshot.Issues[:0]
		for _, issue := range r.snapshot.Issues {
			skip := false
			for _, o := range r.snapshot.Objects {
				if o.ID == issue.ObjectID && o.Materialized != nil && o.Materialized.Staleness == "UNUSEABLE" && issue.Severity == "warning" && strings.HasPrefix(issue.Reason, "将逐行复制当前物化快照") {
					skip = true
				}
			}
			if !skip {
				issues = append(issues, issue)
			}
		}
		r.snapshot.Issues = issues
		return nil
	}
	plan := &MaterializedHistoryPlan{Version: 1, Schema: r.schema}
	var positions []int
	for i, o := range r.snapshot.Objects {
		if o.Materialized == nil || !o.Materialized.Baseline {
			continue
		}
		positions = append(positions, i)
		last, nativeStaleness := info[o.ID].last, info[o.ID].state
		store, sourceName, ddl, err := r.materializedStorageDefinition(o.Table)
		if err != nil {
			return fmt.Errorf("物化视图 %s: %w", o.Name, err)
		}
		columns, err := r.historyColumns(sourceName)
		if err != nil {
			return err
		}
		ddl, err = RestoreNativeXMLTableDDL(ddl, columns)
		if err != nil {
			return fmt.Errorf("物化快照 %s 的XMLTYPE存储属性不可完整恢复: %w", o.Name, err)
		}
		staging := "DBGOLD_MV_HIST_" + Fingerprint([]string{r.schema, o.Name})[:16]
		stagingDDL, err := renameDDLHead(ddl, "TABLE", r.schema, staging)
		if err != nil {
			return err
		}
		stageID, err := r.addHistoryStaging(staging, sourceName, stagingDDL)
		if err != nil {
			return err
		}
		var buffers []MaterializedExchangeBuffer
		var redefinition *MaterializedRedefinitionPlan
		if o.Materialized.Staleness != "PROVEN_CURRENT" {
			if materializedHashPartitioned(ddl) {
				work := "DBGOLD_MV_REDEF_" + Fingerprint([]string{r.schema, o.Name})[:16]
				workDDL, e := renameDDLHead(ddl, "TABLE", r.schema, work)
				if e != nil {
					return e
				}
				redefinition = &MaterializedRedefinitionPlan{WorkingTable: work, SwapName: "DBGOLD_MV_SWAP_" + Fingerprint([]string{r.schema, o.Name})[:16], CreateSQL: workDDL}
				if err = r.addHistoryBuffer(MaterializedExchangeBuffer{Name: work, CreateSQL: workDDL}); err != nil {
					return err
				}
			} else {
				buffers, err = r.historyViewBuffers(store, sourceName, ddl, columns, "DBGOLD_MV_BUF_"+Fingerprint([]string{r.schema, o.Name})[:16])
			}
			if err != nil {
				return fmt.Errorf("物化视图 %s 交换缓冲定义: %w", o.Name, err)
			}
		}
		for _, buffer := range buffers {
			if err = r.addHistoryBuffer(buffer); err != nil {
				return err
			}
		}
		view := MaterializedHistoryView{Current: o.Materialized.Staleness == "PROVEN_CURRENT", ObjectID: o.ID, Name: o.Name, Table: o.Table, Staging: staging, SourceRefresh: last, NativeStaleness: nativeStaleness, Query: o.Materialized.Query, Columns: columns, RowIDColumns: append([]int(nil), o.Materialized.RowIDColumns...)}
		if redefinition != nil {
			view.Strategy = "redefinition"
			view.Redefinition = redefinition
		}
		if len(buffers) == 1 && buffers[0].TablePartition == "" {
			view.Buffer = buffers[0]
		} else {
			view.Buffers = buffers
		}
		if len(view.RowIDColumns) > 0 {
			rows, e := r.db.QueryContext(r.ctx, "SP_GET_MV_DEPEND_COLS_INFO(?,?)", r.schema, o.Name)
			if e != nil {
				return fmt.Errorf("物化视图 %s ROWID归属不可读取: %w", o.Name, e)
			}
			owned := map[int]string{}
			for rows.Next() {
				var owner, mv, container, detailOwner, detailName, alias, kind, column string
				var position int
				if e = rows.Scan(&owner, &mv, &position, &container, &detailOwner, &detailName, &alias, &kind, &column); e != nil {
					break
				}
				if strings.EqualFold(strings.TrimSpace(column), "ROWID") && strings.TrimSpace(detailOwner) == r.schema {
					owned[position-1] = strings.TrimSpace(detailName)
				}
			}
			iteration := rows.Err()
			_ = rows.Close()
			if e != nil {
				return e
			}
			if iteration != nil {
				return iteration
			}
			for _, ordinal := range view.RowIDColumns {
				table := owned[ordinal]
				if table == "" {
					return fmt.Errorf("物化视图 %s 输出列 %d 未确认原生ROWID基表", o.Name, ordinal+1)
				}
				view.RowIDTables = append(view.RowIDTables, table)
			}
		}
		plan.Views = append(plan.Views, view)
		r.snapshot.Objects[i].Dependencies = appendUnique(r.snapshot.Objects[i].Dependencies, stageID)
	}
	for _, o := range append([]Object(nil), r.snapshot.Objects...) {
		if o.Kind != "materialized_view_log" || o.Managed {
			continue
		}
		physical := r.materializedLogDDLName(o.Table)
		ddl, err := r.getDDL("TABLE", physical)
		if err != nil {
			return fmt.Errorf("物化视图日志 %s 原始存储定义不可读取: %w", o.Name, err)
		}
		columns, err := r.historyColumns(physical)
		if err != nil {
			return err
		}
		ddl, err = RestoreNativeXMLTableDDL(ddl, columns)
		if err != nil {
			return fmt.Errorf("物化日志 %s 的XMLTYPE存储属性不可完整恢复: %w", o.Name, err)
		}
		staging := "DBGOLD_MV_LOG_" + Fingerprint([]string{r.schema, o.Table})[:16]
		stagingDDL, err := renameDDLHead(ddl, "TABLE", r.schema, staging)
		if err != nil {
			return err
		}
		if _, err = r.addHistoryStaging(staging, physical, stagingDDL); err != nil {
			return err
		}
		buffer, err := r.historyBuffer(o, physical, ddl, columns, "DBGOLD_MV_LBUF_"+Fingerprint([]string{r.schema, o.Table})[:16])
		if err != nil {
			return fmt.Errorf("物化视图日志 %s 交换缓冲定义: %w", o.Name, err)
		}
		if err = r.addHistoryBuffer(buffer); err != nil {
			return err
		}
		plan.Logs = append(plan.Logs, MaterializedHistoryLog{ObjectID: o.ID, BaseTable: o.Table, Table: physical, Staging: staging, Columns: columns, Buffer: buffer})
		for _, c := range columns {
			if !strings.EqualFold(c.Name, "M_ROW$$") {
				continue
			}
			mapping := "DBGOLD_MV_RID_" + Fingerprint([]string{r.schema, o.Table})[:16]
			if _, exists := r.index["table\x00"+mapping]; exists {
				return fmt.Errorf("物化日志 ROWID 映射名称 %s 已被源对象占用", mapping)
			}
			r.add(0, Object{Kind: "table", Name: mapping, Schema: r.schema, Generated: true, Dependencies: []string{}, Units: []Unit{{SQL: "CREATE TABLE " + Qualified(r.schema, mapping) + ` ("SourceRID" VARCHAR(32) NOT NULL,"TargetRID" VARCHAR(32) NOT NULL,NOT CLUSTER PRIMARY KEY("SourceRID")) STORAGE(ON "MAIN",CLUSTERBTR)`, Phase: "before_data"}}, DropSQL: "DROP TABLE " + Qualified(r.schema, mapping) + " CASCADE CONSTRAINTS"})
			plan.RowIDMaps = append(plan.RowIDMaps, MaterializedRowIDMap{BaseTable: o.Table, Table: mapping})
		}
	}
	if len(plan.Logs) == 0 {
		for _, i := range positions {
			if r.snapshot.Objects[i].Materialized.RefreshMethod != "FORCE" {
				return fmt.Errorf("陈旧 FAST 物化视图缺少原生增量日志清单")
			}
		}
	}
	// FORCE can legitimately have no logs and use a complex query that always
	// falls back to COMPLETE. Its old stored rows still need restoration, and
	// a direct ROWID output still needs physical identity mapping.
	for _, view := range plan.Views {
		for _, base := range view.RowIDTables {
			found := false
			for _, mapping := range plan.RowIDMaps {
				found = found || mapping.BaseTable == base
			}
			if found {
				continue
			}
			mapping := "DBGOLD_MV_RID_" + Fingerprint([]string{r.schema, base})[:16]
			if _, exists := r.index["table\x00"+mapping]; exists {
				return fmt.Errorf("物化快照 ROWID 映射名称 %s 已被源对象占用", mapping)
			}
			r.add(0, Object{Kind: "table", Name: mapping, Schema: r.schema, Generated: true, Dependencies: []string{}, Units: []Unit{{SQL: "CREATE TABLE " + Qualified(r.schema, mapping) + ` ("SourceRID" VARCHAR(32) NOT NULL,"TargetRID" VARCHAR(32) NOT NULL,NOT CLUSTER PRIMARY KEY("SourceRID")) STORAGE(ON "MAIN",CLUSTERBTR)`, Phase: "before_data"}}, DropSQL: "DROP TABLE " + Qualified(r.schema, mapping) + " CASCADE CONSTRAINTS"})
			plan.RowIDMaps = append(plan.RowIDMaps, MaterializedRowIDMap{BaseTable: base, Table: mapping})
		}
	}
	sort.SliceStable(plan.Views, func(i, j int) bool {
		if plan.Views[i].SourceRefresh != plan.Views[j].SourceRefresh {
			return plan.Views[i].SourceRefresh < plan.Views[j].SourceRefresh
		}
		return plan.Views[i].Name < plan.Views[j].Name
	})
	sort.Slice(plan.Logs, func(i, j int) bool { return plan.Logs[i].Table < plan.Logs[j].Table })
	// Immutable staging survives every attempt. The group leader depends on all
	// installed siblings without making the siblings depend on the leader.
	leader := positions[0]
	for _, i := range positions {
		o := &r.snapshot.Objects[i]
		units := o.Units[:0]
		for _, u := range o.Units {
			if u.Phase != "mv_prepare" && u.Phase != "mv_rebase" {
				units = append(units, u)
			}
		}
		o.Units = units
		if o.ID != r.snapshot.Objects[leader].ID {
			r.snapshot.Objects[leader].Dependencies = appendUnique(r.snapshot.Objects[leader].Dependencies, o.ID)
		}
	}
	r.snapshot.Objects[leader].Materialized.History = plan
	r.snapshot.Objects[leader].Units = append(r.snapshot.Objects[leader].Units, Unit{SQL: MaterializedHistorySQL(r.schema, r.snapshot.Objects[leader].Name, plan), Phase: "mv_history"})
	for _, o := range r.snapshot.Objects {
		if o.Kind == "materialized_view_log" && !o.Managed || strings.HasPrefix(o.Name, "DBGOLD_MV_HIST_") || strings.HasPrefix(o.Name, "DBGOLD_MV_LOG_") {
			r.snapshot.Objects[leader].Dependencies = appendUnique(r.snapshot.Objects[leader].Dependencies, o.ID)
		}
	}
	issues := r.snapshot.Issues[:0]
	for _, issue := range r.snapshot.Issues {
		replaced := false
		for _, v := range plan.Views {
			if issue.ObjectID == v.ObjectID && issue.Severity == "warning" && strings.HasPrefix(issue.Reason, "将逐行复制当前物化快照") {
				replaced = true
			}
		}
		if !replaced {
			issues = append(issues, issue)
		}
	}
	r.snapshot.Issues = issues
	r.snapshot.Issues = append(r.snapshot.Issues, Issue{ObjectID: r.snapshot.Objects[leader].ID, Kind: "materialized_view", Name: r.snapshot.Objects[leader].Name, Severity: "warning", Reason: "按源端各物化视图水位冻结原始快照及完整变化日志；目标以原生 COMPLETE 建立本地水位后，通过原生存储交换或在线重定义恢复陈旧快照及尚未消费日志，不刷新源端"})
	return nil
}

func (r *catalogReader) addHistoryBuffer(buffer MaterializedExchangeBuffer) error {
	for _, o := range r.snapshot.Objects {
		if o.Name == buffer.Name || (r.snapshot.Profile.Settings["CASE_SENSITIVE"] == "0" && strings.EqualFold(o.Name, buffer.Name)) {
			return fmt.Errorf("迁移缓冲名称 %s 已被源对象占用，不能合并或覆盖原对象", buffer.Name)
		}
	}
	o := Object{Kind: "table", Name: buffer.Name, Schema: r.schema, Generated: true, Dependencies: []string{}, Units: []Unit{{SQL: buffer.CreateSQL, Phase: "before_data"}}, DropSQL: "DROP TABLE " + Qualified(r.schema, buffer.Name) + " CASCADE CONSTRAINTS"}
	for _, ddl := range buffer.Indexes {
		o.Units = append(o.Units, Unit{SQL: ddl, Phase: "after_data"})
	}
	r.add(0, o)
	return nil
}

func (r *catalogReader) materializedHistoryTables() {
	for i := range r.snapshot.Tables {
		t := &r.snapshot.Tables[i]
		for _, o := range r.snapshot.Objects {
			if o.Materialized == nil || o.Materialized.History == nil {
				continue
			}
			for _, v := range o.Materialized.History.Views {
				if t.Name == v.Staging {
					t.Shadow = true
				}
			}
			for _, m := range o.Materialized.History.RowIDMaps {
				if t.Name == m.BaseTable {
					t.CaptureRowID = true
					t.RowIDMapping = m.Table
				}
			}
		}
	}
	byID := map[string]Object{}
	tables := map[string]Table{}
	protectedStorage := map[string]bool{}
	for _, o := range r.snapshot.Objects {
		byID[o.ID] = o
		if o.Materialized != nil && o.Table != "" {
			protectedStorage[o.Table] = true
		}
	}
	for _, t := range r.snapshot.Tables {
		tables[t.Name] = t
	}
	for i := range r.snapshot.Objects {
		leader := &r.snapshot.Objects[i]
		if leader.Materialized == nil || leader.Materialized.History == nil {
			continue
		}
		plan := leader.Materialized.History
		for j := range plan.Views {
			v := &plan.Views[j]
			if v.NativeStaleness != "NEEDS_COMPILE" {
				continue
			}
			seen := map[string]bool{}
			var candidates, empty []Table
			var walk func(string)
			walk = func(id string) {
				if seen[id] {
					return
				}
				seen[id] = true
				o, ok := byID[id]
				if !ok {
					return
				}
				if t, exists := tables[o.Name]; exists && o.Kind == "table" && !o.Generated && !protectedStorage[o.Name] && t.SourceName == "" {
					if t.Rows > 0 {
						candidates = append(candidates, t)
					} else {
						empty = append(empty, t)
					}
				}
				for _, d := range o.Dependencies {
					walk(d)
				}
			}
			walk(v.ObjectID)
			sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Name < candidates[j].Name })
			for _, t := range candidates {
				var writable, identity string
				for _, c := range t.Columns {
					if !c.Writable {
						continue
					}
					if c.Identity {
						identity = c.Name
					} else if writable == "" {
						writable = c.Name
					}
				}
				if writable != "" {
					q := Qualified(r.schema, t.Name)
					c := Quote(writable)
					v.TouchSQL = []string{"UPDATE " + q + " SET " + c + "=" + c + " WHERE ROWID=(SELECT ROWID FROM " + q + " WHERE ROWNUM=1)"}
					break
				}
				if identity != "" && len(t.Restore) == 2 {
					q := Qualified(r.schema, t.Name)
					c := Quote(identity)
					v.TouchSQL = []string{t.Restore[0].SQL, "UPDATE " + q + " SET " + c + "=" + c + " WHERE ROWID=(SELECT ROWID FROM " + q + " WHERE ROWNUM=1)", t.Restore[1].SQL}
					break
				}
			}
			if len(v.TouchSQL) == 0 && len(empty) > 0 {
				sort.SliceStable(empty, func(i, j int) bool { return empty[i].Name < empty[j].Name })
				var err error
				v.EmptyTouch, err = r.freezeMaterializedEmptyTouch(empty[0])
				if err != nil {
					r.issue(leader, fmt.Sprintf("物化视图 %s 的空基表状态恢复元数据不完整: %v", v.Name, err))
				}
			}
		}
		for j := range leader.Units {
			if leader.Units[j].Phase == "mv_history" {
				leader.Units[j].SQL = MaterializedHistorySQL(r.schema, leader.Name, plan)
			}
		}
	}
}

func (r *catalogReader) immutableMaterializedStaging(name string) bool {
	for _, o := range r.snapshot.Objects {
		if o.Materialized == nil || o.Materialized.History == nil {
			continue
		}
		for _, v := range o.Materialized.History.Views {
			if v.Staging == name {
				return true
			}
		}
		for _, l := range o.Materialized.History.Logs {
			if l.Staging == name {
				return true
			}
		}
	}
	return false
}

func (r *catalogReader) addHistoryStaging(name, source, ddl string) (string, error) {
	if _, exists := r.index["table\x00"+name]; exists {
		return "", fmt.Errorf("迁移暂存名称 %s 已被源对象占用", name)
	}
	i := r.add(0, Object{Kind: "table", Name: name, Schema: r.schema, Table: source, Generated: true, Dependencies: []string{}, Units: []Unit{{SQL: ddl, Phase: "before_data"}}, DropSQL: "DROP TABLE " + Qualified(r.schema, name) + " CASCADE CONSTRAINTS"})
	return r.snapshot.Objects[i].ID, nil
}

func (r *catalogReader) materializedStorageDefinition(name string) (Object, string, string, error) {
	for _, o := range r.snapshot.Objects {
		if o.Kind != "table" || o.Name != name {
			continue
		}
		source := o.Name
		if o.Table != "" && strings.HasPrefix(o.Name, "DBGOLD_MV_") {
			source = o.Table
		}
		for _, u := range o.Units {
			if u.Phase == "before_data" {
				return o, source, u.SQL, nil
			}
		}
		ddl, err := r.getDDL("TABLE", source)
		return o, source, ddl, err
	}
	return Object{}, "", "", fmt.Errorf("快照承载对象 %s 不在冻结清单中", name)
}

func (r *catalogReader) historyColumns(table string) ([]Column, error) {
	rows, err := r.db.QueryContext(r.ctx, `SELECT c.COLUMN_NAME,c.DATA_TYPE,NVL(c.DATA_PRECISION,0),NVL(c.DATA_SCALE,0),sc.INFO2,NVL(ci.INFO1,0) FROM ALL_TAB_COLUMNS c JOIN SYS.SYSOBJECTS o ON o.NAME=c.TABLE_NAME AND o.SCHID=(SELECT ID FROM SYS.SYSOBJECTS WHERE TYPE$='SCH' AND NAME=c.OWNER) JOIN SYS.SYSCOLUMNS sc ON sc.ID=o.ID AND sc.NAME=c.COLUMN_NAME LEFT JOIN SYS.SYSCOLINFOS ci ON ci.ID=o.ID AND ci.COLID=sc.COLID WHERE c.OWNER=? AND c.TABLE_NAME=? ORDER BY c.COLUMN_ID`, r.schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []Column
	for rows.Next() {
		c := Column{Writable: true}
		var flags, extra int64
		if err = rows.Scan(&c.Name, &c.DataType, &c.Precision, &c.Scale, &flags, &extra); err != nil {
			return nil, err
		}
		c.Identity = flags&1 != 0
		c.Writable = extra&1 == 0
		c.SQLType = columnSQLType(c)
		columns = append(columns, c)
	}
	return columns, rows.Err()
}

func (r *catalogReader) historyBuffer(object Object, source, ddl string, columns []Column, name string) (MaterializedExchangeBuffer, error) {
	buffer := MaterializedExchangeBuffer{Name: name, Partition: "DBGOLD_P"}
	var err error
	buffer.CreateSQL, err = MaterializedPartitionBufferDDL(ddl, r.schema, name, buffer.Partition, columns)
	if err != nil {
		return buffer, err
	}
	rows, err := r.db.QueryContext(r.ctx, `SELECT INDEX_NAME FROM ALL_INDEXES WHERE OWNER=? AND TABLE_NAME=? AND INDEX_TYPE<>'CLUSTER' AND INDEX_NAME NOT IN (SELECT INDEX_NAME FROM ALL_CONSTRAINTS WHERE OWNER=? AND TABLE_NAME=? AND CONSTRAINT_TYPE IN ('P','U') AND INDEX_NAME IS NOT NULL) ORDER BY INDEX_NAME`, r.schema, source, r.schema, source)
	if err != nil {
		return buffer, err
	}
	var names []string
	for rows.Next() {
		var index string
		if err = rows.Scan(&index); err != nil {
			break
		}
		names = append(names, index)
	}
	iteration := rows.Err()
	_ = rows.Close()
	if err != nil {
		return buffer, err
	}
	if iteration != nil {
		return buffer, iteration
	}
	for i, index := range names {
		indexDDL, e := r.getDDL("INDEX", index)
		if e != nil {
			return buffer, e
		}
		tokens, e := nativeTokens(indexDDL)
		if e != nil {
			return buffer, e
		}
		global := false
		for _, t := range tokens {
			global = global || t.kind == 'i' && strings.EqualFold(t.value, "GLOBAL")
		}
		if global {
			for j := range r.snapshot.Objects {
				if r.snapshot.Objects[j].Kind == "index" && r.snapshot.Objects[j].Name == index && !r.snapshot.Objects[j].Managed {
					for k := range r.snapshot.Objects[j].Units {
						r.snapshot.Objects[j].Units[k].Phase = "after_history"
					}
				}
			}
			continue
		}
		indexDDL, e = rebindMaterializedIndex(indexDDL, r.schema, source, name)
		if e != nil {
			return buffer, e
		}
		indexDDL, e = renameDDLHead(indexDDL, "INDEX", r.schema, fmt.Sprintf("%s_I%d", name, i))
		if e != nil {
			return buffer, e
		}
		buffer.Indexes = append(buffer.Indexes, indexDDL)
	}
	_ = object
	return buffer, nil
}

func (r *catalogReader) historyViewBuffers(object Object, source, ddl string, columns []Column, name string) ([]MaterializedExchangeBuffer, error) {
	plain, partitioned, err := MaterializedUnpartitionedBufferDDL(ddl, r.schema, name)
	if err != nil {
		return nil, err
	}
	if !partitioned {
		b, e := r.historyBuffer(object, source, ddl, columns, name)
		return []MaterializedExchangeBuffer{b}, e
	}
	var partitioning, subpartitioning string
	if err = r.db.QueryRowContext(r.ctx, `SELECT PARTITIONING_TYPE,SUBPARTITIONING_TYPE FROM ALL_PART_TABLES WHERE OWNER=? AND TABLE_NAME=?`, r.schema, source).Scan(&partitioning, &subpartitioning); err != nil {
		return nil, fmt.Errorf("原物化快照分区类型不可读取: %w", err)
	}
	if strings.EqualFold(partitioning, "HASH") {
		return nil, fmt.Errorf("HASH 预建快照必须使用已冻结的原生在线重定义通路，不能生成已知必败的分区交换步骤")
	}
	_ = subpartitioning
	rows, err := r.db.QueryContext(r.ctx, `SELECT PARTITION_NAME FROM ALL_TAB_PARTITIONS WHERE TABLE_OWNER=? AND TABLE_NAME=? AND SUBPARTITION_COUNT=0 ORDER BY PARTITION_POSITION`, r.schema, source)
	if err != nil {
		return nil, err
	}
	var leaves []MaterializedExchangeBuffer
	for rows.Next() {
		var leaf string
		if err = rows.Scan(&leaf); err != nil {
			break
		}
		leaves = append(leaves, MaterializedExchangeBuffer{TablePartition: leaf})
	}
	iteration := rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	if iteration != nil {
		return nil, iteration
	}
	rows, err = r.db.QueryContext(r.ctx, `SELECT p.SUBPARTITION_NAME FROM ALL_TAB_SUBPARTITIONS p WHERE p.TABLE_OWNER=? AND p.TABLE_NAME=? AND NOT EXISTS (SELECT 1 FROM ALL_TAB_SUBPARTITIONS c WHERE c.TABLE_OWNER=p.TABLE_OWNER AND c.TABLE_NAME=p.TABLE_NAME AND c.PARTITION_NAME=p.SUBPARTITION_NAME) ORDER BY p.LEVEL,p.SUBPARTITION_POSITION`, r.schema, source)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var leaf string
		if err = rows.Scan(&leaf); err != nil {
			break
		}
		leaves = append(leaves, MaterializedExchangeBuffer{TablePartition: leaf, Subpartition: true})
	}
	iteration = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	if iteration != nil {
		return nil, iteration
	}
	if len(leaves) == 0 {
		return nil, fmt.Errorf("原分区存储无可确认的叶分区")
	}
	// Obtain the same native local index definitions, then strip the artificial
	// partition clause from the buffer. The source storage is never flattened.
	for i := range leaves {
		b, e := r.historyBuffer(object, source, plain, columns, fmt.Sprintf("%s_P%d", name, i))
		if e != nil {
			return nil, e
		}
		b.CreateSQL, _, e = MaterializedUnpartitionedBufferDDL(b.CreateSQL, r.schema, b.Name)
		if e != nil {
			return nil, e
		}
		b.TablePartition = leaves[i].TablePartition
		b.Subpartition = leaves[i].Subpartition
		b.Partition = ""
		leaves[i] = b
	}
	return leaves, nil
}

func materializedHashPartitioned(ddl string) bool {
	ts, err := nativeTokens(ddl)
	if err != nil {
		return false
	}
	depth := 0
	for i, t := range ts {
		if t.kind == 'p' && t.value == "(" {
			depth++
		} else if t.kind == 'p' && t.value == ")" {
			depth--
		} else if depth == 0 && t.kind == 'i' && strings.EqualFold(t.value, "PARTITION") && i+2 < len(ts) && strings.EqualFold(ts[i+1].value, "BY") && strings.EqualFold(ts[i+2].value, "HASH") {
			return true
		}
	}
	return false
}

func MaterializedUnpartitionedBufferDDL(ddl, schema, name string) (string, bool, error) {
	ddl, err := renameDDLHead(ddl, "TABLE", schema, name)
	if err != nil {
		return "", false, err
	}
	ts, err := nativeTokens(ddl)
	if err != nil {
		return "", false, err
	}
	depth, start, end := 0, -1, len(ddl)
	for _, t := range ts {
		if t.kind == 'p' && t.value == "(" {
			depth++
		} else if t.kind == 'p' && t.value == ")" {
			depth--
		} else if depth == 0 && t.kind == 'i' {
			if start < 0 && strings.EqualFold(t.value, "PARTITION") {
				start = t.start
			}
			if start >= 0 && strings.EqualFold(t.value, "STORAGE") {
				end = t.start
				break
			}
		}
	}
	if start < 0 {
		return ddl, false, nil
	}
	if end == len(ddl) {
		return strings.TrimSpace(ddl[:start]) + ";", true, nil
	}
	return ddl[:start] + ddl[end:], true, nil
}

// MaterializedPartitionBufferDDL adds one catch-all partition to a native table
// declaration. The column and storage clauses remain byte-for-byte native.
func MaterializedPartitionBufferDDL(ddl, schema, name, partition string, columns []Column) (string, error) {
	ddl, err := RestoreNativeXMLTableDDL(ddl, columns)
	if err != nil {
		return "", err
	}
	ddl, err = renameDDLHead(ddl, "TABLE", schema, name)
	if err != nil {
		return "", err
	}
	ts, err := nativeTokens(ddl)
	if err != nil {
		return "", err
	}
	depth, end := 0, -1
	for i, t := range ts {
		if t.kind == 'p' && t.value == "(" {
			depth++
		} else if t.kind == 'p' && t.value == ")" {
			depth--
			if depth == 0 && end < 0 {
				end = i
			}
		} else if depth == 0 && t.kind == 'i' && strings.EqualFold(t.value, "PARTITION") {
			return "", fmt.Errorf("已分区的物化存储需要按原叶分区交换，不能合并原分区定义")
		}
	}
	if end < 0 {
		return "", fmt.Errorf("原生存储定义缺少完整列声明")
	}
	key := ""
	for _, c := range columns {
		switch strings.ToUpper(c.DataType) {
		case "TINYINT", "SMALLINT", "INT", "INTEGER", "BIGINT", "DECIMAL", "NUMERIC", "NUMBER", "VARCHAR", "VARCHAR2", "CHAR", "DATE", "DATETIME", "TIMESTAMP":
			key = c.Name
		}
		if key != "" {
			break
		}
	}
	if key == "" {
		return "", fmt.Errorf("原生存储缺少可用的分区交换键")
	}
	pos := ts[end].end
	return ddl[:pos] + " PARTITION BY RANGE(" + Quote(key) + ") (PARTITION " + Quote(partition) + " VALUES LESS THAN(MAXVALUE)) " + ddl[pos:], nil
}
