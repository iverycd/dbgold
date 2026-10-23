package damengnative

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

type intervalDefinition struct {
	column, interval, last string
	inclusive              bool
	initial                map[string]bool
}

func nativeIntervalDefinition(ddl string) (*intervalDefinition, error) {
	ts, err := nativeTokens(ddl)
	if err != nil {
		return nil, err
	}
	word := func(i int, s string) bool {
		return i < len(ts) && ts[i].kind == 'i' && strings.EqualFold(ts[i].value, s)
	}
	close := func(open int) (int, error) {
		depth := 0
		for i := open; i < len(ts); i++ {
			if ts[i].kind == 'p' {
				if ts[i].value == "(" {
					depth++
				}
				if ts[i].value == ")" {
					depth--
					if depth == 0 {
						return i, nil
					}
				}
			}
		}
		return 0, fmt.Errorf("间隔分区括号不完整")
	}
	for i := 0; i+4 < len(ts); i++ {
		if !word(i, "PARTITION") || !word(i+1, "BY") || !word(i+2, "RANGE") || ts[i+3].value != "(" {
			continue
		}
		end, e := close(i + 3)
		if e != nil {
			return nil, e
		}
		if end+2 >= len(ts) || !word(end+1, "INTERVAL") {
			return nil, nil
		}
		if end != i+5 || ts[i+4].kind != 'q' && ts[i+4].kind != 'i' {
			return nil, fmt.Errorf("间隔分区键不是原生单列")
		}
		p := &intervalDefinition{column: ts[i+4].value, initial: map[string]bool{}}
		if ts[i+4].kind == 'i' {
			p.column = strings.ToUpper(p.column)
		}
		open := end + 2
		if ts[open].value != "(" {
			return nil, fmt.Errorf("间隔表达式不完整")
		}
		end, e = close(open)
		if e != nil {
			return nil, e
		}
		p.interval = ddl[ts[open].end:ts[end].start]
		// Native date interval constructors are pure; user routines are never
		// evaluated while probing a source boundary.
		if !materializedEmptyPartitionConstant(p.interval) {
			return nil, fmt.Errorf("间隔表达式不是原生常量或日期间隔构造函数")
		}
		for j := end + 1; j+2 < len(ts); j++ {
			if !word(j, "PARTITION") || word(j+1, "BY") {
				continue
			}
			name := ts[j+1].value
			if ts[j+1].kind == 'i' {
				name = strings.ToUpper(name)
			}
			p.initial[name] = true
			k := j + 2
			p.inclusive = false
			for ; k < len(ts) && ts[k].value != "("; k++ {
				p.inclusive = p.inclusive || word(k, "EQU")
			}
			if k >= len(ts) {
				return nil, fmt.Errorf("初始间隔分区边界不完整")
			}
			last, e := close(k)
			if e != nil {
				return nil, e
			}
			p.last = ddl[ts[k].end:ts[last].start]
			j = last
		}
		if len(p.initial) == 0 || p.last == "" {
			return nil, fmt.Errorf("间隔分区没有初始边界")
		}
		return p, nil
	}
	return nil, nil
}

func (r *catalogReader) freezeIntervalPartitions(o *Object, t Table) error {
	var ddl string
	for _, u := range o.Units {
		if u.Phase == "before_data" {
			ddl = u.SQL
			break
		}
	}
	d, err := nativeIntervalDefinition(ddl)
	if err != nil || d == nil {
		return err
	}
	var column Column
	found := false
	for _, c := range t.Columns {
		if c.Name == d.column {
			column = c
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("间隔分区列不在完整列清单中")
	}
	rows, err := r.db.QueryContext(r.ctx, `SELECT PARTITION_NAME,HIGH_VALUE,TABLESPACE_NAME FROM ALL_TAB_PARTITIONS WHERE TABLE_OWNER=? AND TABLE_NAME=? ORDER BY PARTITION_POSITION`, r.schema, t.Name)
	if err != nil {
		return err
	}
	defer rows.Close()
	plan := &IntervalPartitionPlan{Column: d.column}
	extra := false
	for rows.Next() {
		var p IntervalPartition
		var high, space sql.NullString
		if err = rows.Scan(&p.Name, &high, &space); err != nil {
			break
		}
		if !high.Valid {
			return fmt.Errorf("间隔分区边界为空")
		}
		p.HighSQL = high.String
		p.Tablespace = space.String
		extra = extra || !d.initial[p.Name]
		plan.Partitions = append(plan.Partitions, p)
	}
	iteration := rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	if iteration != nil {
		return iteration
	}
	if !extra {
		return nil
	}
	touch, err := r.freezeTargetEmptyTouch(t, false)
	if err != nil {
		return err
	}
	touch.DefinitionSQL, _, err = deferForeignKeys(touch.DefinitionSQL, r.schema, t.Name)
	if err != nil {
		return err
	}
	removed := map[string]bool{}
	for _, c := range r.snapshot.Objects {
		if c.Kind == "constraint" && c.Table == t.Name && c.ConstraintType != "R" && c.ConstraintNoValidate {
			definition, e := r.getDDL("CONSTRAINT", c.Name)
			if e != nil {
				return e
			}
			definition = repairGeneratedConstraintIdentity(definition, c)
			touch.DefinitionSQL, e = removeNativeTableConstraint(touch.DefinitionSQL, c, definition)
			if e != nil {
				return e
			}
			removed[c.Name] = true
		}
	}
	keep := touch.Constraints[:0]
	for _, c := range touch.Constraints {
		if c.Table == t.Name && c.Kind != "R" && !removed[c.Name] {
			keep = append(keep, c)
		}
	}
	touch.Constraints = keep
	for _, c := range t.Columns {
		if !c.Identity {
			continue
		}
		var seed, step string
		q := Qualified(r.schema, t.Name)
		if err = r.db.QueryRowContext(r.ctx, `SELECT CAST(IDENT_SEED(?) AS VARCHAR(80)),CAST(IDENT_INCR(?) AS VARCHAR(80))`, q, q).Scan(&seed, &step); err != nil {
			return err
		}
		if _, e := strconv.ParseInt(seed, 10, 64); e != nil {
			return e
		}
		if n, e := strconv.ParseInt(step, 10, 64); e != nil || n == 0 {
			return fmt.Errorf("初始自增步长不可精确读取")
		}
		touch.IdentityRestore = []string{"ALTER TABLE " + q + " DROP IDENTITY", "ALTER TABLE " + q + " ADD COLUMN " + Quote(c.Name) + " IDENTITY(" + seed + ", " + step + ")"}
	}
	plan.EmptyTouch = touch
	plan.DefinitionSQL = touch.DefinitionSQL
	for i := range plan.Partitions {
		p := &plan.Partitions[i]
		if !materializedEmptyPartitionConstant(p.HighSQL) {
			return fmt.Errorf("分区 %s 边界不是原生常量", p.Name)
		}
		// Read the physical definition by catalog-owned name rather than guessing
		// an automatic partition's ID/name prefix.
		var physical string
		if err = r.db.QueryRowContext(r.ctx, `SELECT o.NAME FROM SYS.SYSHPARTTABLEINFO p JOIN SYS.SYSOBJECTS o ON o.ID=p.PART_TABLE_ID WHERE p.BASE_TABLE_ID=(SELECT b.ID FROM SYS.SYSOBJECTS b JOIN SYS.SYSOBJECTS s ON s.ID=b.SCHID WHERE s.TYPE$='SCH' AND s.NAME=? AND b.NAME=? AND b.SUBTYPE$='UTAB') AND p.PARTITION_NAME=?`, r.schema, t.Name, p.Name).Scan(&physical); err != nil {
			return err
		}
		p.DefinitionSQL, err = r.getDDL("TABLE", physical)
		if err != nil {
			return err
		}
		p.DefinitionSQL, err = RestoreNativeXMLTableDDL(p.DefinitionSQL, t.Columns)
		if err != nil {
			return fmt.Errorf("间隔物理分区 %s 的XMLTYPE属性不可完整恢复: %w", p.Name, err)
		}
		for _, object := range r.snapshot.Objects {
			if object.Kind == "constraint" && object.Table == physical && object.Managed && object.Generated {
				p.DerivedConstraintNames = append(p.DerivedConstraintNames, object.Name)
			}
		}
	}
	o.Interval = plan
	for i := range plan.Partitions {
		p := &plan.Partitions[i]
		if d.initial[p.Name] {
			continue
		}
		if column.Writable {
			p.SeedSQL, err = r.freezeIntervalSeed(t.Name, column, *d, p.HighSQL)
		} else {
			p.SeedSQL, p.Values, err = r.freezeGeneratedIntervalSeed(t, column, *d, plan.DefinitionSQL, p.HighSQL, plan.EmptyTouch)
		}
		if err != nil {
			return fmt.Errorf("间隔分区 %s: %w", p.Name, err)
		}
	}
	o.Units = append(o.Units, Unit{SQL: IntervalPartitionSQL(r.schema, t.Name, plan), Phase: "interval_prepare"})
	return nil
}

func (r *catalogReader) freezeIntervalSeed(table string, c Column, d intervalDefinition, high string) (string, error) {
	cast := func(s string) string { return "CAST(" + s + " AS " + c.SQLType + ")" }
	upper := cast(high)
	lower := cast(upper + "-(" + d.interval + ")")
	candidates := materializedEmptyPartitionValues(c, high, true)
	candidates = append(candidates, lower, cast(upper+"-("+d.interval+")/2"))
	var last error
	for _, seed := range candidates {
		predicate := seed + ">=" + lower + " AND " + seed + "<" + upper
		if d.inclusive {
			predicate += " AND " + seed + ">" + cast(d.last)
		}
		var valid int
		probe := "SELECT CASE WHEN " + predicate + " THEN (SELECT COUNT(*) FROM " + Qualified(r.schema, table) + " PARTITION FOR(" + seed + ")) ELSE -1 END FROM DUAL"
		if e := r.db.QueryRowContext(r.ctx, probe).Scan(&valid); e != nil {
			last = e
			if r.ctx.Err() != nil {
				return "", r.ctx.Err()
			}
			continue
		}
		if valid >= 0 {
			return seed, nil
		}
	}
	return "", fmt.Errorf("无法冻结原生间隔内的精确临时键值: %v", last)
}
