package damengnative

import (
	"database/sql"
	"fmt"
	"strings"
)

// freezeMaterializedEmptyTouch is read-only. In particular it does not test the
// INSERT on the source: the source remains stopped and every original attribute
// needed by the target's reversible empty-table epoch operation is persisted.
func (r *catalogReader) freezeMaterializedEmptyTouch(t Table) (*MaterializedEmptyTouch, error) {
	if t.Rows != 0 || t.SourceName != "" {
		return nil, fmt.Errorf("空基表状态恢复只能使用冻结的真实空基表")
	}
	return r.freezeTargetEmptyTouch(t, true)
}

// Interval preparation runs before data import against a newly created empty
// target. Its source may contain rows; only native definitions are read here.
func (r *catalogReader) freezeTargetEmptyTouch(t Table, sourceEmpty bool) (*MaterializedEmptyTouch, error) {
	if t.SourceName != "" {
		return nil, fmt.Errorf("临时行计划必须属于真实基表")
	}
	ddl, err := r.getDDL("TABLE", t.Name)
	if err != nil {
		return nil, err
	}
	ddl, err = RestoreNativeXMLTableDDL(ddl, t.Columns)
	if err != nil {
		return nil, err
	}
	onNull, err := materializedDefaultOnNullColumns(ddl)
	if err != nil {
		return nil, err
	}
	p := &MaterializedEmptyTouch{Table: t.Name, DefinitionSQL: ddl}
	writable := map[string]bool{}
	identity := false
	for _, c := range t.Columns {
		if c.Writable {
			writable[c.Name] = true
			identity = identity || c.Identity
		}
	}
	rows, err := r.db.QueryContext(r.ctx, `SELECT COLUMN_NAME,NULLABLE,DATA_DEFAULT FROM ALL_TAB_COLUMNS WHERE OWNER=? AND TABLE_NAME=? ORDER BY COLUMN_ID`, r.schema, t.Name)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var name, nullable string
		var d sql.NullString
		if err = rows.Scan(&name, &nullable, &d); err != nil {
			break
		}
		if writable[name] {
			p.Columns = append(p.Columns, MaterializedEmptyColumn{Name: name, NotNull: strings.EqualFold(nullable, "N"), DefaultSQL: d.String, DefaultOnNull: onNull[name]})
			delete(writable, name)
		}
	}
	iteration := rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	if iteration != nil {
		return nil, iteration
	}
	if len(writable) != 0 || len(p.Columns) == 0 {
		return nil, fmt.Errorf("空基表 %s 的显式可写列元数据不完整", t.Name)
	}
	if identity {
		if len(t.Restore) != 2 {
			return nil, fmt.Errorf("空基表 %s 缺少冻结的自增续号恢复单元", t.Name)
		}
		p.IdentityRestore = []string{t.Restore[0].SQL, t.Restore[1].SQL}
	}
	// Inbound foreign keys must be disabled before their referenced primary key.
	// Their original names, column order and enabled states are frozen as well as
	// the base's own constraints. No catalogue name is case-folded.
	rows, err = r.db.QueryContext(r.ctx, `SELECT CONSTRAINT_NAME,TABLE_NAME,CONSTRAINT_TYPE,GENERATED,STATUS,VALIDATED FROM ALL_CONSTRAINTS WHERE OWNER=? AND (TABLE_NAME=? OR (CONSTRAINT_TYPE='R' AND R_OWNER=? AND R_CONSTRAINT_NAME IN (SELECT CONSTRAINT_NAME FROM ALL_CONSTRAINTS WHERE OWNER=? AND TABLE_NAME=? AND CONSTRAINT_TYPE IN ('P','U')))) ORDER BY TABLE_NAME,CONSTRAINT_NAME`, r.schema, t.Name, r.schema, r.schema, t.Name)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c MaterializedEmptyConstraint
		var generated, status, validated sql.NullString
		if err = rows.Scan(&c.Name, &c.Table, &c.Kind, &generated, &status, &validated); err != nil {
			break
		}
		c.Generated = strings.EqualFold(generated.String, "GENERATED NAME")
		c.Enabled = strings.EqualFold(status.String, "ENABLED")
		c.NoValidate = c.Enabled && !strings.EqualFold(validated.String, "VALIDATED")
		if r.snapshot != nil {
			for _, object := range r.snapshot.Objects {
				if object.Kind == "constraint" && object.Name == c.Name && object.Table == c.Table {
					// Older DM8 exposes GENERATED as NULL even for an engine
					// allocated name. The inventory already proves that name
					// from the native CONS object's ID, without prefix guesses.
					c.Generated = c.Generated || object.Generated
					break
				}
			}
		}
		p.Constraints = append(p.Constraints, c)
	}
	iteration = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	if iteration != nil {
		return nil, iteration
	}
	for i := range p.Constraints {
		c := &p.Constraints[i]
		if c.Kind == "R" {
			c.DefinitionSQL, err = r.getDDL("CONSTRAINT", c.Name)
			if err != nil {
				return nil, fmt.Errorf("引用约束 %s 的完整原生定义不可读取: %w", c.Name, err)
			}
		}
		rows, err = r.db.QueryContext(r.ctx, `SELECT COLUMN_NAME FROM ALL_CONS_COLUMNS WHERE OWNER=? AND TABLE_NAME=? AND CONSTRAINT_NAME=? ORDER BY POSITION`, r.schema, c.Table, c.Name)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var name string
			if err = rows.Scan(&name); err != nil {
				break
			}
			c.Columns = append(c.Columns, name)
		}
		iteration = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
		if iteration != nil {
			return nil, iteration
		}
	}
	definition := p.DefinitionSQL
	if !sourceEmpty {
		// Cluster-key constants still come from native key types/history, but
		// the interval planner supplies its own proved route for each leaf.
		p.DefinitionSQL, _, err = MaterializedUnpartitionedBufferDDL(definition, r.schema, t.Name)
		if err != nil {
			return nil, err
		}
	}
	if err = r.populateMaterializedEmptyCluster(t, p); err != nil {
		return nil, err
	}
	p.DefinitionSQL = definition
	return p, nil
}

// Inspect column clauses rather than searching the DDL text: quoted column
// names, string defaults and CHECK expressions may all contain these keywords.
func materializedDefaultOnNullColumns(ddl string) (map[string]bool, error) {
	ts, err := nativeTokens(ddl)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	depth, start := 0, -1
	inspect := func(end int) {
		if start < 0 || start >= end {
			return
		}
		first := ts[start]
		if first.kind != 'q' && first.kind != 'i' {
			return
		}
		name := first.value
		if first.kind == 'i' {
			name = strings.ToUpper(name)
		}
		local := 0
		for i := start + 1; i+2 < end; i++ {
			if ts[i].kind == 'p' && ts[i].value == "(" {
				local++
			} else if ts[i].kind == 'p' && ts[i].value == ")" {
				local--
			}
			if local == 0 && ts[i].kind == 'i' && strings.EqualFold(ts[i].value, "DEFAULT") && ts[i+1].kind == 'i' && strings.EqualFold(ts[i+1].value, "ON") && ts[i+2].kind == 'i' && strings.EqualFold(ts[i+2].value, "NULL") {
				out[name] = true
			}
		}
	}
	for i, t := range ts {
		if t.kind != 'p' {
			continue
		}
		switch t.value {
		case "(":
			depth++
			if depth == 1 && start < 0 {
				start = i + 1
			}
		case ",":
			if depth == 1 {
				inspect(i)
				start = i + 1
			}
		case ")":
			depth--
			if depth == 0 && start >= 0 {
				inspect(i)
				return out, nil
			}
		}
	}
	return nil, fmt.Errorf("空基表原生定义缺少完整列声明")
}
