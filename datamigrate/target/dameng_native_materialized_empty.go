package target

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"dbgold/datamigrate/damengnative"
)

type historyEmptyConstraint struct {
	name, table, kind string
	columns           []string
	enabled           bool
	definition        string
	noValidate        bool
	clustered         bool
}

func historyEmptyForeignDefinition(ddl string, generated bool) (string, error) {
	tokens, err := nativeSQLTokens(ddl)
	if err != nil {
		return "", err
	}
	var out []string
	for i := 0; i < len(tokens); i++ {
		if strings.EqualFold(tokens[i], "ENABLE") || strings.EqualFold(tokens[i], "DISABLE") || strings.EqualFold(tokens[i], "NOVALIDATE") {
			continue
		}
		out = append(out, tokens[i])
		if generated && strings.EqualFold(tokens[i], "CONSTRAINT") {
			i++
			if i >= len(tokens) || !nativeIdentifierToken(tokens[i]) {
				return "", fmt.Errorf("外键原生约束名称无效")
			}
			out = append(out, `"<GENERATED_CONSTRAINT>"`)
		}
	}
	return strings.Join(out, " "), nil
}

func historyEmptyDefinition(sqlText string, p *damengnative.MaterializedEmptyTouch) (string, error) {
	tokens, err := nativeSQLTokens(sqlText)
	if err != nil {
		return "", err
	}
	columns := map[string]damengnative.MaterializedEmptyColumn{}
	for _, c := range p.Columns {
		columns[c.Name] = c
	}
	var out []string
	depth, start := 0, -1
	for i, token := range tokens {
		if token == "(" {
			depth++
			if depth == 1 && start < 0 {
				start = i + 1
				out = append(out, tokens[:start]...)
			}
			continue
		}
		end := token == "," && depth == 1 || token == ")" && depth == 1
		if end && start >= 0 {
			part := tokens[start:i]
			if len(part) > 0 {
				c, isColumn := columns[nativeTokenName(part[0])]
				var normalized []string
				inner := 0
				for j := 0; j < len(part); j++ {
					t := strings.ToUpper(part[j])
					if inner == 0 && isColumn {
						if c.NotNull && c.ValueSQL == "" && t == "NOT" && j+1 < len(part) && strings.EqualFold(part[j+1], "NULL") {
							j++
							continue
						}
						if len(p.IdentityRestore) > 0 && t == "IDENTITY" && j+1 < len(part) && part[j+1] == "(" {
							j += 2
							for j < len(part) && part[j] != ")" {
								j++
							}
							if j == len(part) {
								return "", fmt.Errorf("自增定义未闭合")
							}
							continue
						}
						if c.DefaultOnNull && c.ValueSQL == "" && t == "DEFAULT" {
							j++
							if j+1 >= len(part) || !strings.EqualFold(part[j], "ON") || !strings.EqualFold(part[j+1], "NULL") {
								return "", fmt.Errorf("默认值语义已偏离冻结计划")
							}
							j += 2
							want, e := nativeSQLTokens(c.DefaultSQL)
							if e != nil || len(want) == 0 || j+len(want) > len(part) || !nativeDefinitionsEqual(strings.Join(part[j:j+len(want)], " "), c.DefaultSQL) {
								return "", fmt.Errorf("默认值表达式已偏离冻结计划")
							}
							j += len(want) - 1
							continue
						}
					}
					if inner == 0 && !isColumn && (t == "DISABLE" || t == "ENABLE" || t == "NOVALIDATE") {
						continue
					}
					normalized = append(normalized, part[j])
					if part[j] == "(" {
						inner++
					}
					if part[j] == ")" {
						inner--
					}
				}
				out = append(out, normalized...)
			}
			out = append(out, token)
			start = i + 1
		}
		if token == ")" {
			depth--
			if depth == 0 && start >= 0 {
				out = append(out, tokens[i+1:]...)
				break
			}
		}
	}
	if start < 0 {
		return "", fmt.Errorf("缺少原生表定义")
	}
	return strings.Join(out, " "), nil
}

func historyEmptyConstraints(ctx context.Context, conn *sql.Conn, schema, table string) ([]historyEmptyConstraint, error) {
	rows, err := conn.QueryContext(ctx, `SELECT c.OWNER,c.CONSTRAINT_NAME,c.TABLE_NAME,c.CONSTRAINT_TYPE,c.STATUS,c.VALIDATED,cc.COLUMN_NAME FROM ALL_CONSTRAINTS c LEFT JOIN ALL_CONS_COLUMNS cc ON cc.OWNER=c.OWNER AND cc.CONSTRAINT_NAME=c.CONSTRAINT_NAME WHERE (c.OWNER=? AND c.TABLE_NAME=?) OR (c.CONSTRAINT_TYPE='R' AND c.R_OWNER=? AND c.R_CONSTRAINT_NAME IN (SELECT CONSTRAINT_NAME FROM ALL_CONSTRAINTS WHERE OWNER=? AND TABLE_NAME=? AND CONSTRAINT_TYPE IN ('P','U'))) ORDER BY c.OWNER,c.TABLE_NAME,c.CONSTRAINT_NAME,cc.POSITION`, schema, table, schema, schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []historyEmptyConstraint
	for rows.Next() {
		var owner, name, table, kind, status, validated string
		var col sql.NullString
		if err = rows.Scan(&owner, &name, &table, &kind, &status, &validated, &col); err != nil {
			return nil, err
		}
		if owner != schema {
			return nil, fmt.Errorf("空基表状态恢复存在选定模式外的引用约束，不能改变其状态")
		}
		if len(result) == 0 || result[len(result)-1].name != name || result[len(result)-1].table != table {
			enabled := strings.EqualFold(status, "ENABLED")
			result = append(result, historyEmptyConstraint{name: name, table: table, kind: kind, enabled: enabled, noValidate: enabled && !strings.EqualFold(validated, "VALIDATED")})
		}
		if col.Valid {
			result[len(result)-1].columns = append(result[len(result)-1].columns, col.String)
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	for i := range result {
		if result[i].kind == "R" {
			if err = conn.QueryRowContext(ctx, `SELECT DBMS_METADATA.GET_DDL('CONSTRAINT',?,?) FROM DUAL`, result[i].name, schema).Scan(&result[i].definition); err != nil {
				return nil, fmt.Errorf("无法冻结核对外键完整定义: %w", err)
			}
		}
	}
	return result, nil
}

func historyResolveEmptyConstraints(actual []historyEmptyConstraint, p *damengnative.MaterializedEmptyTouch) ([]historyEmptyConstraint, error) {
	if len(actual) != len(p.Constraints) {
		return nil, fmt.Errorf("空基表约束清单与冻结计划不同")
	}
	used := make([]bool, len(actual))
	resolved := make([]historyEmptyConstraint, 0, len(actual))
	for _, want := range p.Constraints {
		match := -1
		for i, got := range actual {
			if used[i] || got.table != want.Table || got.kind != want.Kind || strings.Join(got.columns, "\x00") != strings.Join(want.Columns, "\x00") || !want.Generated && got.name != want.Name {
				continue
			}
			if got.kind == "R" {
				if want.DefinitionSQL == "" {
					return nil, fmt.Errorf("空基表外键缺少完整原生定义")
				}
				a, e := historyEmptyForeignDefinition(got.definition, want.Generated)
				if e != nil {
					return nil, e
				}
				b, e := historyEmptyForeignDefinition(want.DefinitionSQL, want.Generated)
				if e != nil {
					return nil, e
				}
				if !nativeDefinitionsEqual(a, b) {
					continue
				}
			}
			if got.kind == "C" || got.kind == "U" {
				// These constraints are never modified. Their expressions and
				// references already passed full table-definition proof. Repeated
				// generated constraints on the same columns form a multiset;
				// preserve the exact enabled-state multiplicity when pairing them.
				if got.enabled != want.Enabled || got.noValidate != want.NoValidate {
					continue
				}
				match = i
				break
			}
			if match >= 0 {
				return nil, fmt.Errorf("空基表系统约束不能唯一匹配")
			}
			match = i
		}
		if match < 0 {
			return nil, fmt.Errorf("空基表约束定义与冻结计划不同")
		}
		got := actual[match]
		used[match] = true
		// Only a frozen enabled PK or its referencing FK may be left disabled
		// by an interrupted attempt. Every other state must already be exact.
		mutable := got.kind == "P" && !want.Clustered || got.kind == "R"
		if got.enabled != want.Enabled && (!mutable || !want.Enabled || got.enabled) {
			return nil, fmt.Errorf("空基表约束状态已偏离冻结计划")
		}
		if got.enabled && got.noValidate != want.NoValidate {
			return nil, fmt.Errorf("空基表约束验证状态已偏离冻结计划")
		}
		got.enabled = want.Enabled
		got.noValidate = want.NoValidate
		got.clustered = want.Clustered
		resolved = append(resolved, got)
	}
	return resolved, nil
}

func historyEmptyReadDDL(ctx context.Context, conn *sql.Conn, schema, table string) (string, error) {
	var ddl string
	err := conn.QueryRowContext(ctx, `SELECT DBMS_METADATA.GET_DDL('TABLE',?,?) FROM DUAL`, table, schema).Scan(&ddl)
	return ddl, err
}

// nativeHistoryEmptyTouch uses a transactional NULL-only temporary row. It
// never evaluates an ON NULL default, consumes an identity value, or exposes
// an intermediate committed row. Independent cleanup restores every frozen
// attribute even after cancellation; the next attempt reconciles catalog state
// before executing another DML pair.
func nativeHistoryEmptyTouch(ctx context.Context, conn *sql.Conn, schema string, p *damengnative.MaterializedEmptyTouch) (retErr error) {
	if p == nil || p.Table == "" || len(p.Columns) == 0 || p.DefinitionSQL == "" {
		return fmt.Errorf("缺少冻结的空基表状态恢复计划")
	}
	q := damengnative.Qualified(schema, p.Table)
	for _, constraint := range p.Constraints {
		if constraint.Clustered && constraint.Kind == "P" {
			for _, name := range constraint.Columns {
				found := false
				for _, c := range p.Columns {
					if c.Name == name && c.ValueSQL != "" {
						found = true
					}
				}
				if !found {
					return fmt.Errorf("聚集主键缺少冻结的非空原生占位值")
				}
			}
		}
	}
	var count int64
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+q).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return fmt.Errorf("空基表已存在数据，拒绝临时行状态恢复")
	}
	actualDDL, err := historyEmptyReadDDL(ctx, conn, schema, p.Table)
	if err != nil {
		return err
	}
	wantDDL, err := historyEmptyDefinition(p.DefinitionSQL, p)
	if err != nil {
		return err
	}
	gotDDL, err := historyEmptyDefinition(actualDDL, p)
	if err != nil {
		return err
	}
	if !nativeDefinitionsEqual(wantDDL, gotDDL) {
		return fmt.Errorf("空基表原生定义与冻结计划不同")
	}
	actual, err := historyEmptyConstraints(ctx, conn, schema, p.Table)
	if err != nil {
		return err
	}
	constraints, err := historyResolveEmptyConstraints(actual, p)
	if err != nil {
		return err
	}
	var identity *nativeHistoryTouchRecipe
	if len(p.IdentityRestore) > 0 {
		if len(p.IdentityRestore) != 2 {
			return fmt.Errorf("空基表自增恢复定义不完整")
		}
		for _, c := range p.Columns {
			u := "UPDATE " + q + " SET " + damengnative.Quote(c.Name) + "=" + damengnative.Quote(c.Name) + " WHERE ROWID=(SELECT ROWID FROM " + q + " WHERE ROWNUM=1)"
			v, e := parseNativeHistoryTouch(schema, []string{p.IdentityRestore[0], u, p.IdentityRestore[1]})
			if e == nil {
				identity = &v
				break
			}
		}
		if identity == nil {
			return fmt.Errorf("空基表自增恢复不属于冻结表列")
		}
		present, e := nativeHistoryIdentityPresent(ctx, conn, schema, *identity)
		if e != nil {
			return e
		}
		if present {
			if e = nativeHistoryIdentityMatches(ctx, conn, schema, *identity); e != nil {
				return e
			}
		}
	}
	// Restore FKs last, after PKs and NOT NULL. Resolve generated names once
	// by their frozen kind/column identity; no user-supplied identifier runs.
	sort.SliceStable(constraints, func(i, j int) bool { return constraints[i].kind != "R" && constraints[j].kind == "R" })
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		var cleanupErr error
		for _, c := range p.Columns {
			if c.DefaultOnNull && c.ValueSQL == "" {
				_, e := conn.ExecContext(cleanup, "ALTER TABLE "+q+" ALTER COLUMN "+damengnative.Quote(c.Name)+" SET DEFAULT ON NULL "+c.DefaultSQL)
				cleanupErr = errors.Join(cleanupErr, e)
			}
			if c.NotNull && c.ValueSQL == "" {
				_, e := conn.ExecContext(cleanup, "ALTER TABLE "+q+" ALTER COLUMN "+damengnative.Quote(c.Name)+" SET NOT NULL")
				cleanupErr = errors.Join(cleanupErr, e)
			}
		}
		if identity != nil {
			present, e := nativeHistoryIdentityPresent(cleanup, conn, schema, *identity)
			if e == nil && !present {
				_, e = conn.ExecContext(cleanup, identity.restore)
			}
			if e == nil {
				e = nativeHistoryIdentityMatches(cleanup, conn, schema, *identity)
			}
			cleanupErr = errors.Join(cleanupErr, e)
		}
		for _, c := range constraints {
			if c.enabled && (c.kind == "P" && !c.clustered || c.kind == "R") {
				statement := "ALTER TABLE " + damengnative.Qualified(schema, c.table) + " ENABLE CONSTRAINT " + damengnative.Quote(c.name)
				if c.noValidate {
					statement += " NOVALIDATE"
				}
				_, e := conn.ExecContext(cleanup, statement)
				cleanupErr = errors.Join(cleanupErr, e)
			}
		}
		if _, e := conn.ExecContext(cleanup, "CALL SET_SESSION_CONS_CHK(1)"); e != nil {
			cleanupErr = errors.Join(cleanupErr, e)
		}
		if cleanupErr == nil {
			ddl, e := historyEmptyReadDDL(cleanup, conn, schema, p.Table)
			if e == nil && !nativeDefinitionsEqual(ddl, p.DefinitionSQL) {
				e = fmt.Errorf("空基表清理后的原生定义不同")
			}
			cleanupErr = errors.Join(cleanupErr, e)
		}
		if cleanupErr == nil {
			got, e := historyEmptyConstraints(cleanup, conn, schema, p.Table)
			if e == nil {
				if len(got) != len(constraints) {
					e = fmt.Errorf("空基表清理后的约束清单不同")
				}
				for _, want := range constraints {
					found := false
					for _, actual := range got {
						definitionMatches := true
						if actual.kind == "R" {
							a, e1 := historyEmptyForeignDefinition(actual.definition, false)
							b, e2 := historyEmptyForeignDefinition(want.definition, false)
							definitionMatches = e1 == nil && e2 == nil && nativeDefinitionsEqual(a, b)
						}
						if actual.name == want.name && actual.table == want.table && actual.kind == want.kind && actual.enabled == want.enabled && actual.noValidate == want.noValidate && strings.Join(actual.columns, "\x00") == strings.Join(want.columns, "\x00") && definitionMatches {
							found = true
							break
						}
					}
					if !found {
						e = fmt.Errorf("空基表清理后的约束状态不同")
						break
					}
				}
			}
			cleanupErr = errors.Join(cleanupErr, e)
		}
		if cleanupErr == nil {
			var n int64
			e := conn.QueryRowContext(cleanup, "SELECT COUNT(*) FROM "+q).Scan(&n)
			if e == nil && n != 0 {
				e = fmt.Errorf("空基表清理后仍有临时数据")
			}
			cleanupErr = errors.Join(cleanupErr, e)
		}
		if cleanupErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("恢复空基表冻结定义失败，需目录核对后补迁: %w", cleanupErr))
		}
	}()
	// Inbound FKs are disabled before the referenced PK. Constraints on other
	// schemas were rejected by the read-only inventory above.
	for i := len(constraints) - 1; i >= 0; i-- {
		c := constraints[i]
		if c.enabled && (c.kind == "P" && !c.clustered || c.kind == "R") {
			if _, err = conn.ExecContext(ctx, "ALTER TABLE "+damengnative.Qualified(schema, c.table)+" DISABLE CONSTRAINT "+damengnative.Quote(c.name)); err != nil {
				return err
			}
		}
	}
	if identity != nil {
		present, e := nativeHistoryIdentityPresent(ctx, conn, schema, *identity)
		if e != nil {
			return e
		}
		if present {
			if _, err = conn.ExecContext(ctx, identity.drop); err != nil {
				return err
			}
		}
	}
	var cols, values []string
	seen := map[string]bool{}
	for _, c := range p.Columns {
		if c.Name == "" || seen[c.Name] || c.DefaultOnNull && c.DefaultSQL == "" {
			return fmt.Errorf("空基表可写列定义不完整")
		}
		seen[c.Name] = true
		if c.DefaultOnNull && c.ValueSQL == "" {
			if _, err = conn.ExecContext(ctx, "ALTER TABLE "+q+" ALTER COLUMN "+damengnative.Quote(c.Name)+" DROP DEFAULT"); err != nil {
				return err
			}
		}
		if c.NotNull && c.ValueSQL == "" {
			if _, err = conn.ExecContext(ctx, "ALTER TABLE "+q+" ALTER COLUMN "+damengnative.Quote(c.Name)+" SET NULL"); err != nil {
				return err
			}
		}
		cols = append(cols, damengnative.Quote(c.Name))
		value := "NULL"
		if c.ValueSQL != "" {
			tokens, e := nativeSQLTokens(c.ValueSQL)
			if e != nil || len(tokens) == 0 {
				return fmt.Errorf("聚集主键占位值不是冻结的原生表达式")
			}
			for _, token := range tokens {
				if token == ";" || strings.EqualFold(token, "SELECT") || strings.EqualFold(token, "NEXTVAL") {
					return fmt.Errorf("聚集主键占位值不能查询或消耗序列")
				}
			}
			value = c.ValueSQL
		}
		values = append(values, value)
	}
	if _, err = conn.ExecContext(ctx, "CALL SET_SESSION_CONS_CHK(0)"); err != nil {
		return err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := tx.ExecContext(ctx, "INSERT INTO "+q+"("+strings.Join(cols, ",")+") VALUES("+strings.Join(values, ",")+")")
	if err != nil {
		return err
	}
	if n, e := r.RowsAffected(); e != nil || n != 1 {
		return errors.Join(fmt.Errorf("空基表临时行插入数不同: %d", n), e)
	}
	r, err = tx.ExecContext(ctx, "DELETE FROM "+q)
	if err != nil {
		return err
	}
	if n, e := r.RowsAffected(); e != nil || n != 1 {
		return errors.Join(fmt.Errorf("空基表临时行删除数不同: %d", n), e)
	}
	return tx.Commit()
}
