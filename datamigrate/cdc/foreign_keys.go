package cdc

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/lib/pq"
)

type foreignKey struct {
	Schema     string
	Table      string
	Name       string
	Columns    []string
	RefColumns []string
	OnUpdate   string
	OnDelete   string
}

// The referenced schema is the parent schema, so children from other schemas
// are visible to the extent allowed by the MySQL account. Such children aren't
// migrated in a single-schema job.
const sourceInboundForeignKeysSQL = `SELECT k.REFERENCED_TABLE_NAME,k.TABLE_SCHEMA,k.TABLE_NAME,k.CONSTRAINT_NAME,
	k.COLUMN_NAME,k.REFERENCED_COLUMN_NAME,r.UPDATE_RULE,r.DELETE_RULE
	FROM information_schema.KEY_COLUMN_USAGE k
	JOIN information_schema.REFERENTIAL_CONSTRAINTS r
	  ON BINARY r.CONSTRAINT_SCHEMA=BINARY k.CONSTRAINT_SCHEMA AND BINARY r.TABLE_NAME=BINARY k.TABLE_NAME
	  AND BINARY r.CONSTRAINT_NAME=BINARY k.CONSTRAINT_NAME
	WHERE BINARY k.REFERENCED_TABLE_SCHEMA=BINARY ?
	ORDER BY BINARY k.REFERENCED_TABLE_NAME,BINARY k.TABLE_SCHEMA,BINARY k.TABLE_NAME,
	  BINARY k.CONSTRAINT_NAME,k.ORDINAL_POSITION`

// LoadSourceInboundForeignKeys must run in the same metadata capture as the
// source columns. Empty and unknown inventories have different meanings.
func LoadSourceInboundForeignKeys(ctx context.Context, db SourceMetadataReader, database string, tables []TableInfo) error {
	if len(tables) == 0 {
		return nil
	}
	byName := make(map[string]*TableInfo, len(tables))
	for i := range tables {
		byName[tables[i].Name] = &tables[i]
		tables[i].InboundForeignKeys = nil
		tables[i].ForeignKeysKnown = false
	}
	rows, err := db.QueryContext(ctx, sourceInboundForeignKeysSQL, database)
	if err != nil {
		return fmt.Errorf("读取 MySQL 入向外键失败: %w", err)
	}
	defer rows.Close()
	var previous *foreignKey
	var previousParent string
	for rows.Next() {
		var parent, column, refColumn string
		var key foreignKey
		if err = rows.Scan(&parent, &key.Schema, &key.Table, &key.Name, &column, &refColumn, &key.OnUpdate, &key.OnDelete); err != nil {
			return fmt.Errorf("读取 MySQL 入向外键列失败: %w", err)
		}
		table := byName[parent]
		if table == nil {
			continue
		}
		if previous == nil || previousParent != parent || previous.Schema != key.Schema || previous.Table != key.Table || previous.Name != key.Name {
			table.InboundForeignKeys = append(table.InboundForeignKeys, key)
			previous = &table.InboundForeignKeys[len(table.InboundForeignKeys)-1]
			previousParent = parent
		}
		previous.Columns = append(previous.Columns, column)
		previous.RefColumns = append(previous.RefColumns, refColumn)
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("读取 MySQL 入向外键失败: %w", err)
	}
	for i := range tables {
		tables[i].ForeignKeysKnown = true
	}
	return nil
}

func canReferenceChangedKey(t *TableInfo, before, after []any) bool {
	for _, columns := range [][]int{t.PrimaryKey, t.AutoIncrement} {
		for _, index := range columns {
			if index >= 0 && index < len(before) && index < len(after) && !reflect.DeepEqual(before[index], after[index]) {
				return true
			}
		}
	}
	for _, index := range t.UniqueIndexes {
		if columnsChanged(t, index.Columns, before, after) {
			return true
		}
	}
	return t.LocatorStrategy != LocatorFullRow && locatorChanged(t, before, after)
}

func columnsChanged(t *TableInfo, columns []string, before, after []any) bool {
	for _, column := range columns {
		i := columnIndex(t, column)
		if i >= 0 && i < len(before) && i < len(after) && !reflect.DeepEqual(before[i], after[i]) {
			return true
		}
	}
	return false
}

func changesReferencedColumns(t *TableInfo, rule foreignKey, ch Change) bool {
	return ch.Action == "delete" || (ch.Action == "update" && columnsChanged(t, rule.RefColumns, ch.Before, ch.After))
}

func fkAction(rule foreignKey, operation string) string {
	if operation == "delete" {
		return rule.OnDelete
	}
	return rule.OnUpdate
}

func fkMutatesChild(action string) bool {
	return action != "NO ACTION" && action != "RESTRICT"
}

func sourceChildInScope(cfg Config, rule foreignKey) bool {
	if rule.Schema != cfg.SourceDatabase {
		return false
	}
	if cfg.TableNames == nil {
		return true
	}
	for _, table := range cfg.TableNames {
		if rule.Table == table {
			return true
		}
	}
	return false
}

// Read FK actions and pair child/parent attributes by their catalog ordinals.
// Parsing the ordinals avoids dependencies on server-version-specific array
// helper expressions in the catalog dialect of supported PG derivatives.
const postgresInboundForeignKeysSQL = `SELECT f.oid,nc.nspname,c.relname,f.conname,f.confupdtype,f.confdeltype,
	f.conkey::text,f.confkey::text,ca.attnum,ca.attname,pa.attnum,pa.attname,
	(current_setting('session_replication_role') <> 'replica' AND NOT EXISTS (
		SELECT 1 FROM pg_catalog.pg_trigger trg WHERE trg.tgconstraint=f.oid AND trg.tgenabled NOT IN ('O','A')
	))
	FROM pg_catalog.pg_constraint f
	JOIN pg_catalog.pg_class p ON p.oid=f.confrelid
	JOIN pg_catalog.pg_namespace np ON np.oid=p.relnamespace
	JOIN pg_catalog.pg_class c ON c.oid=f.conrelid
	JOIN pg_catalog.pg_namespace nc ON nc.oid=c.relnamespace
	JOIN pg_catalog.pg_attribute ca ON ca.attrelid=c.oid AND ca.attnum=ANY(f.conkey)
	JOIN pg_catalog.pg_attribute pa ON pa.attrelid=p.oid AND pa.attnum=ANY(f.confkey)
	WHERE f.contype='f' AND np.nspname=$1 AND p.relname=$2
	ORDER BY f.oid`

type postgresForeignKey struct {
	rule    foreignKey
	enabled bool
}

func postgresFKAction(action string) string {
	switch action {
	case "a":
		return "NO ACTION"
	case "r":
		return "RESTRICT"
	case "c":
		return "CASCADE"
	case "n":
		return "SET NULL"
	case "d":
		return "SET DEFAULT"
	default:
		return action
	}
}

func readPostgresInboundForeignKeys(ctx context.Context, tx *sql.Tx, schema, table string) ([]postgresForeignKey, error) {
	rows, err := tx.QueryContext(ctx, postgresInboundForeignKeysSQL, schema, table)
	if err != nil {
		return nil, fmt.Errorf("核对目标表 %s.%s 的入向外键失败: %w", schema, table, err)
	}
	defer rows.Close()
	type constraint struct {
		rule                          postgresForeignKey
		childOrdinals, parentOrdinals []int64
		childNames, parentNames       map[int64]string
	}
	var constraints []*constraint
	var previousID int64 = -1
	var current *constraint
	for rows.Next() {
		var id, childOrdinal, parentOrdinal int64
		var fk postgresForeignKey
		var childName, parentName string
		var childKey, parentKey []int64
		err = rows.Scan(&id, &fk.rule.Schema, &fk.rule.Table, &fk.rule.Name, &fk.rule.OnUpdate, &fk.rule.OnDelete,
			pq.Array(&childKey), pq.Array(&parentKey), &childOrdinal, &childName, &parentOrdinal, &parentName, &fk.enabled)
		if err != nil {
			return nil, err
		}
		if id != previousID {
			fk.rule.OnUpdate = postgresFKAction(fk.rule.OnUpdate)
			fk.rule.OnDelete = postgresFKAction(fk.rule.OnDelete)
			current = &constraint{rule: fk, childOrdinals: childKey, parentOrdinals: parentKey, childNames: map[int64]string{}, parentNames: map[int64]string{}}
			constraints = append(constraints, current)
			previousID = id
		}
		current.childNames[childOrdinal] = childName
		current.parentNames[parentOrdinal] = parentName
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	result := make([]postgresForeignKey, 0, len(constraints))
	for _, constraint := range constraints {
		if len(constraint.childOrdinals) == 0 || len(constraint.parentOrdinals) != len(constraint.childOrdinals) {
			return nil, fmt.Errorf("目标外键 %s 的列元数据不完整", constraint.rule.rule.Name)
		}
		for i, child := range constraint.childOrdinals {
			childName := constraint.childNames[child]
			parentName := constraint.parentNames[constraint.parentOrdinals[i]]
			if childName == "" || parentName == "" {
				return nil, fmt.Errorf("目标外键 %s 的列元数据不完整", constraint.rule.rule.Name)
			}
			constraint.rule.rule.Columns = append(constraint.rule.rule.Columns, childName)
			constraint.rule.rule.RefColumns = append(constraint.rule.rule.RefColumns, parentName)
		}
		result = append(result, constraint.rule)
	}
	return result, nil
}

func mappedForeignKey(cfg Config, rule foreignKey) foreignKey {
	result := rule
	result.Schema, result.Table = targetSchemaName(cfg), targetObjectName(cfg, rule.Table)
	result.Columns, result.RefColumns = nil, nil
	for i, column := range rule.Columns {
		result.Columns = append(result.Columns, targetObjectName(cfg, column))
		result.RefColumns = append(result.RefColumns, targetObjectName(cfg, rule.RefColumns[i]))
	}
	return result
}

func fkSignature(rule foreignKey, action string) string {
	pairs := make([]string, len(rule.Columns))
	for i, column := range rule.Columns {
		pairs[i] = column + "\x00" + rule.RefColumns[i]
	}
	sort.Strings(pairs)
	return strings.Join(append([]string{rule.Schema, rule.Table, action}, pairs...), "\x00")
}

func setApplierForeignKeyScope(applier targetApplier, tables []TableInfo) {
	switch a := applier.(type) {
	case *PostgresApplier:
		a.cfg.TableNames = tableNames(tables)
		a.foreignKeyTables = tableMap(tables)
	case *DamengApplier:
		a.cfg.TableNames = tableNames(tables)
	}
}

type pgForeignKeyVisit struct {
	table, action, columns string
}
type pgForeignKeyChecks struct {
	rules          map[string][]postgresForeignKey
	lockedChildren map[string]bool
	visited        map[pgForeignKeyVisit]bool
}

func (a *PostgresApplier) checkForeignKeyActions(ctx context.Context, tx *sql.Tx, ch Change, checks *pgForeignKeyChecks) error {
	if !ch.Table.ForeignKeysKnown || (ch.Action != "update" && ch.Action != "delete") {
		return nil
	}
	changed := make(map[string]bool)
	if ch.Action == "update" {
		for i, name := range ch.Table.Columns {
			if i < len(ch.Before) && i < len(ch.After) && !reflect.DeepEqual(ch.Before[i], ch.After[i]) {
				changed[name] = true
			}
		}
		if len(changed) == 0 {
			return nil
		}
	}
	return a.checkCascade(ctx, tx, ch.Table, ch.Action, changed, checks)
}

func (a *PostgresApplier) checkCascade(ctx context.Context, tx *sql.Tx, table *TableInfo, operation string, changed map[string]bool, checks *pgForeignKeyChecks) error {
	var columns []string
	for _, column := range table.Columns {
		if changed[column] {
			columns = append(columns, column)
		}
	}
	visit := pgForeignKeyVisit{table.Name, operation, strings.Join(columns, "\x00")}
	if checks.visited[visit] {
		return nil
	}
	checks.visited[visit] = true

	rules, loaded := checks.rules[table.Name]
	if !loaded {
		var err error
		rules, err = readPostgresInboundForeignKeys(ctx, tx, a.schema, a.name(table.Name))
		if err != nil {
			return err
		}
		checks.rules[table.Name] = rules
	}
	touches := func(refColumns []string, changed map[string]bool) bool {
		if operation == "delete" {
			return true
		}
		for _, col := range refColumns {
			if changed[col] {
				return true
			}
		}
		return false
	}
	mappedChanged := make(map[string]bool, len(changed))
	for name := range changed {
		mappedChanged[a.name(name)] = true
	}
	expected := make(map[string]foreignKey)
	for _, rule := range table.InboundForeignKeys {
		action := fkAction(rule, operation)
		if sourceChildInScope(a.cfg, rule) && fkMutatesChild(action) && touches(rule.RefColumns, changed) {
			expected[fkSignature(mappedForeignKey(a.cfg, rule), action)] = rule
		}
	}
	for _, rule := range rules {
		action := fkAction(rule.rule, operation)
		if !fkMutatesChild(action) || !touches(rule.rule.RefColumns, mappedChanged) {
			continue
		}
		key := fkSignature(rule.rule, action)
		sourceRule, exists := expected[key]
		if !exists || !rule.enabled {
			return fmt.Errorf("目标表 %s 的入向外键 %s.%s (%s) 级联动作 %s 与源端不一致或未启用", table.Name, rule.rule.Schema, rule.rule.Table, rule.rule.Name, action)
		}
		if err := a.lockCascadeChild(ctx, tx, sourceRule.Table, checks); err != nil {
			return err
		}
		if child := a.foreignKeyTables[sourceRule.Table]; child != nil {
			childOperation := "update"
			if operation == "delete" && action == "CASCADE" {
				childOperation = "delete"
			}
			childChanged := make(map[string]bool, len(sourceRule.Columns))
			for i, col := range sourceRule.Columns {
				if action != "CASCADE" || changed[sourceRule.RefColumns[i]] {
					childChanged[col] = true
				}
			}
			if err := a.checkCascade(ctx, tx, child, childOperation, childChanged, checks); err != nil {
				return err
			}
		}
		delete(expected, key)
	}
	for _, rule := range expected {
		return fmt.Errorf("目标表 %s 缺少与源入向外键 %s.%s (%s) 匹配的 %s 级联规则", table.Name, rule.Schema, rule.Table, rule.Name, fkAction(rule, operation))
	}
	return nil
}

func (a *PostgresApplier) lockCascadeChild(ctx context.Context, tx *sql.Tx, child string, checks *pgForeignKeyChecks) error {
	if checks.lockedChildren[child] {
		return nil
	}
	if _, err := tx.ExecContext(ctx, "LOCK TABLE "+a.qualified(child)+" IN ROW EXCLUSIVE MODE"); err != nil {
		return fmt.Errorf("锁定目标级联子表 %s: %w", child, err)
	}
	if err := checkTargetBusinessTriggers(ctx, tx, a.cfg, []TableInfo{{Name: child}}); err != nil {
		return err
	}
	checks.lockedChildren[child] = true
	return nil
}

const damengInboundConstraintSQL = `SELECT c.OWNER,c.TABLE_NAME,c.CONSTRAINT_NAME
	FROM DBA_CONSTRAINTS c JOIN DBA_CONSTRAINTS p ON p.OWNER=c.R_OWNER AND p.CONSTRAINT_NAME=c.R_CONSTRAINT_NAME
	WHERE p.OWNER=? AND p.TABLE_NAME=? AND c.CONSTRAINT_TYPE='R' AND ROWNUM=1`

func (r *damengRow) guardIdentityReplacement(ctx context.Context, tx damengDMLExecutor) error {
	if r.identityReplacementChecked {
		return nil
	}
	// ALL_CONSTRAINTS can hide child tables owned by other schemas. A row
	// replacement needs authoritative evidence, including such references.
	if _, err := tx.ExecContext(ctx, "LOCK TABLE "+r.qualified()+" IN EXCLUSIVE MODE"); err != nil {
		return fmt.Errorf("锁定达梦 IDENTITY 替换目标表 %s: %w", r.qualified(), err)
	}
	var owner, table, name string
	err := tx.QueryRowContext(ctx, damengInboundConstraintSQL, r.applier.schema, r.applier.name(r.table.Name)).Scan(&owner, &table, &name)
	if err == sql.ErrNoRows {
		r.identityReplacementChecked = true
		return nil
	}
	if err != nil {
		return fmt.Errorf("无法核验达梦 IDENTITY 替换的入向外键，已保留检查点: %w", err)
	}
	return fmt.Errorf("达梦表 %s 存在入向外键 %s.%s (%s)，禁止通过删除重插替换 IDENTITY", r.qualified(), owner, table, name)
}

func (r *damengRow) checkForeignKeyUpdate(ch Change) error {
	if !r.table.ForeignKeysKnown {
		if r.applier.checkpointGuard != nil && (canReferenceChangedKey(r.table, ch.Before, ch.After) || r.identityChanged(ch.Before, ch.After)) {
			return fmt.Errorf("达梦表 %s 的旧检查点缺少可信的入向外键信息，禁止更新可能被引用的键，请重新建立增量起点", r.table.Name)
		}
		return nil
	}
	for _, rule := range r.table.InboundForeignKeys {
		if sourceChildInScope(r.applier.cfg, rule) && fkMutatesChild(rule.OnUpdate) && changesReferencedColumns(r.table, rule, ch) {
			return fmt.Errorf("达梦不支持表 %s 的源入向外键 %s.%s (%s) 所需的 ON UPDATE %s，已保留检查点", r.table.Name, rule.Schema, rule.Table, rule.Name, rule.OnUpdate)
		}
	}
	return nil
}
