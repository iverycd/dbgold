package source

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"dbgold/datamigrate/failure"
)

type PostgresSQLServerReader struct {
	*PostgresReader
	readProjections sync.Map // table name -> postgresSQLServerProjection
}

type postgresSQLServerProjection struct {
	sql   string
	types []string
}

func NewPostgresProductForSQLServer(product, driverName, dsn, schema string, pool ConnPoolConfig) (*PostgresSQLServerReader, error) {
	// Missing privileges on secondary object catalogs must not block connecting
	// to tables. Each object reader reports its own metadata errors.
	r, err := newPostgresProduct(product, driverName, dsn, schema, pool, true)
	if err != nil {
		return nil, err
	}
	return WrapPostgresForSQLServer(r), nil
}

func WrapPostgresForSQLServer(r *PostgresReader) *PostgresSQLServerReader {
	return &PostgresSQLServerReader{PostgresReader: r}
}

func (r *PostgresSQLServerReader) GetTableDDLInfo(ctx context.Context, table string) (*TableDDLInfo, error) {
	if r.profile.Fields["pg_class.relrowsecurity"] {
		var restricted bool
		query := `SELECT c.relrowsecurity FROM ` + r.qcat("pg_class") + ` c JOIN ` + r.qcat("pg_namespace") + ` n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname=$2`
		if err := r.db.QueryRowContext(ctx, query, r.schema, table).Scan(&restricted); err != nil {
			return nil, err
		}
		if restricted {
			return nil, fmt.Errorf("源表 %s 启用了行级安全策略，不能证明 SELECT 能读取完整数据", table)
		}
	}
	info, err := r.pgColumnsForTarget(ctx, table, true)
	if err != nil {
		return nil, err
	}
	parts, types := make([]string, len(info.Columns)), make([]string, len(info.Columns))
	for i := range info.Columns {
		column := &info.Columns[i]
		column.Extra = "" // Native bindings determine the SQL Server generator.
		quoted := QuotePGIdent(column.Name)
		parts[i], types[i] = quoted, strings.ToUpper(column.DataType)
		switch column.DataType {
		case "time", "timetz":
			// Preserve 24:00:00 and original precision. Some Go drivers read
			// 24:00:00 as next-day midnight, losing evidence of an invalid SQL
			// Server TIME before the converter can check it.
			parts[i] = quoted + "::text AS " + quoted
		}
	}
	r.readProjections.Store(table, postgresSQLServerProjection{sql: strings.Join(parts, ", "), types: types})
	return info, nil
}

func (r *PostgresSQLServerReader) projection(ctx context.Context, table string) (postgresSQLServerProjection, error) {
	if cached, ok := r.readProjections.Load(table); ok {
		return cached.(postgresSQLServerProjection), nil
	}
	if _, err := r.GetTableDDLInfo(ctx, table); err != nil {
		return postgresSQLServerProjection{}, err
	}
	cached, _ := r.readProjections.Load(table)
	return cached.(postgresSQLServerProjection), nil
}

func (r *PostgresSQLServerReader) ReadPage(ctx context.Context, table string, pkCols []string, offset, limit int64) ([]string, []string, [][]interface{}, error) {
	projection, err := r.projection(ctx, table)
	if err != nil {
		return nil, nil, nil, err
	}
	columns, _, rows, err := r.readPageWithProjection(ctx, table, pkCols, offset, limit, projection.sql)
	return columns, projection.types, rows, err
}

func (r *PostgresSQLServerReader) StreamPartitionRows(ctx context.Context, table string, batchSize int, consume func([]string, []string, [][]interface{}) error) error {
	projection, err := r.projection(ctx, table)
	if err != nil {
		return err
	}
	query := "SELECT " + projection.sql + " FROM " + QuotePGIdent(r.schema) + "." + QuotePGIdent(table)
	return streamPartitionQuery(ctx, r.db, query, batchSize, projection.types, nil, consume)
}

func (r *PostgresSQLServerReader) GetPrimaryKey(ctx context.Context, table string) ([]string, error) {
	// Pagination requires only this table's key. In particular it does not
	// depend on access to pg_get_indexdef or on parsing another table's index.
	query := `SELECT a.attname FROM ` + r.qcat("pg_constraint") + ` c JOIN ` + r.qcat("pg_class") + ` t ON t.oid=c.conrelid JOIN ` + r.qcat("pg_namespace") + ` n ON n.oid=t.relnamespace
 JOIN generate_subscripts(c.conkey,1) k(position) ON true JOIN ` + r.qcat("pg_attribute") + ` a ON a.attrelid=t.oid AND a.attnum=c.conkey[k.position]
 WHERE n.nspname=$1 AND t.relname=$2 AND c.contype='p' ORDER BY k.position`
	rows, err := r.db.QueryContext(ctx, query, r.schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			return nil, err
		}
		columns = append(columns, column)
	}
	return columns, rows.Err()
}

func (r *PostgresSQLServerReader) GetPrimaryKeys(ctx context.Context) ([]IndexInfo, error) {
	return r.sqlServerIndexes(ctx, true)
}

func (r *PostgresSQLServerReader) GetIndexes(ctx context.Context) ([]IndexInfo, error) {
	return r.sqlServerIndexes(ctx, false)
}

func (r *PostgresSQLServerReader) sqlServerIndexes(ctx context.Context, primary bool) ([]IndexInfo, error) {
	indexes, err := r.pgIndexesForTarget(ctx, true)
	if err != nil {
		return nil, err
	}
	indexes = slices.DeleteFunc(indexes, func(index IndexInfo) bool { return index.IsPrimary != primary })
	if len(indexes) == 0 {
		return nil, nil
	}
	tables := make(map[string]*PGTable)
	for _, index := range indexes {
		if tables[index.TableName] == nil {
			tables[index.TableName] = &PGTable{}
		}
		tables[index.TableName].Indexes = append(tables[index.TableName].Indexes, index)
	}
	if err := r.markPGOperatorClassesForTarget(ctx, tables, true); err != nil {
		return nil, fmt.Errorf("无法核验源索引操作符类: %w", err)
	}
	for i := range indexes {
		index := &indexes[i]
		index.Unsupported = pgSQLServerIndexIssue(*index)
		if primary && index.PG.ConstraintName != "" {
			index.IndexName = index.PG.ConstraintName
		}
	}
	return indexes, nil
}

func pgSQLServerIndexIssue(index IndexInfo) string {
	pg := index.PG
	switch {
	case index.Method != "btree":
		return "仅支持普通 B-tree 索引"
	case pg.Expressions != "" || pg.Predicate != "" || pg.KeyCount != len(index.Attnums) || pg.KeyCount != len(index.Columns):
		return "表达式、部分或 INCLUDE 索引需要手工适配"
	case !pg.Valid || !pg.Immediate || !pg.Validated || pg.Deferrable:
		return "无效索引、未验证或可延迟约束需要手工适配"
	case pg.NullsNotDistinct:
		return "NULLS NOT DISTINCT 唯一规则需要手工适配"
	case pg.NonDefaultOpClass || pg.NonDefaultCollation:
		return "非默认排序规则或操作符类需要手工适配"
	}
	for _, option := range pg.Options {
		if option & ^3 != 0 {
			return fmt.Sprintf("未知 B-tree 索引选项 %d", option)
		}
	}
	return ""
}

func (r *PostgresSQLServerReader) GetForeignKeys(ctx context.Context) ([]FKInfo, error) {
	keys, err := r.pgForeignKeysForTarget(ctx, true)
	if err != nil {
		return nil, err
	}
	for i := range keys {
		key := &keys[i]
		switch {
		case key.RefSchema != r.schema:
			key.Unsupported = "跨 schema 外键引用需要手工适配"
		case key.PG.Deferrable || !key.PG.Validated || key.PG.Match != "s":
			key.Unsupported = "未验证、可延迟或非 MATCH SIMPLE 外键需要手工适配"
		case key.OnDelete == "" || key.OnUpdate == "":
			key.Unsupported = "未知的外键删除或更新动作"
		}
	}
	return keys, nil
}

func (r *PostgresSQLServerReader) GetViews(ctx context.Context) ([]ViewInfo, error) {
	if !r.pgMetadataFunction("pg_get_viewdef") {
		return nil, fmt.Errorf("缺少元数据函数 pg_get_viewdef 或 EXECUTE 权限")
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// The deparser must qualify references outside the system catalog. A user
	// routine named like a built-in must never be converted as that built-in.
	if _, err := tx.ExecContext(ctx, "SET LOCAL search_path TO "+QuotePGIdent(r.profile.Catalog)+", pg_temp"); err != nil {
		return nil, err
	}
	query := `SELECT c.relname,pg_get_viewdef(c.oid,true),c.relkind FROM ` + r.qcat("pg_class") + ` c JOIN ` + r.qcat("pg_namespace") + ` n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relkind IN ('v','m') ORDER BY c.relname`
	rows, err := tx.QueryContext(ctx, query, r.schema)
	if err != nil {
		return nil, err
	}
	var views []ViewInfo
	for rows.Next() {
		var view ViewInfo
		var definition sql.NullString
		var kind string
		if err := rows.Scan(&view.ViewName, &definition, &kind); err != nil {
			rows.Close()
			return nil, err
		}
		view.Definition = strings.TrimRight(strings.TrimSpace(definition.String), "; \t\r\n")
		switch {
		case view.Definition == "":
			view.Unsupported = "视图定义不可读取"
		case kind == "m":
			view.Unsupported = "物化视图需要手工适配"
		}
		views = append(views, view)
	}
	if err := pgFinishOptionalRows(rows); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	for i := range views {
		if views[i].Unsupported != "" {
			continue
		}
		info, err := r.pgColumnsForTarget(ctx, views[i].ViewName, true)
		if err != nil {
			if failure.IsTaskError(err) {
				return nil, err
			}
			views[i].Unsupported = fmt.Sprintf("读取视图输出列: %v", err)
			continue
		}
		views[i].Columns = info.Columns
	}
	return views, nil
}

func (r *PostgresSQLServerReader) GetTriggers(ctx context.Context) ([]TriggerInfo, error) {
	definition := "NULL::text"
	if r.pgMetadataFunction("pg_get_triggerdef") {
		definition = "pg_get_triggerdef(g.oid,true)"
	}
	query := `SELECT g.tgname,t.relname,n.nspname,` + definition + ` FROM ` + r.qcat("pg_trigger") + ` g JOIN ` + r.qcat("pg_class") + ` t ON t.oid=g.tgrelid JOIN ` + r.qcat("pg_namespace") + ` n ON n.oid=t.relnamespace WHERE n.nspname=$1 AND NOT g.tgisinternal`
	if r.profile.Fields["pg_trigger.tgparentid"] {
		query += " AND g.tgparentid=0"
	}
	rows, err := r.db.QueryContext(ctx, query+" ORDER BY t.relname,g.tgname", r.schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var triggers []TriggerInfo
	for rows.Next() {
		var trigger TriggerInfo
		var definition sql.NullString
		if err := rows.Scan(&trigger.Name, &trigger.TableName, &trigger.Schema, &definition); err != nil {
			return nil, err
		}
		trigger.SourceDDL = definition.String
		if strings.TrimSpace(trigger.SourceDDL) == "" {
			trigger.MetadataError = "无法读取触发器原始 DDL，请检查 pg_get_triggerdef 权限"
		}
		triggers = append(triggers, trigger)
	}
	return triggers, rows.Err()
}

func (r *PostgresSQLServerReader) GetSequences(ctx context.Context) ([]SequenceInfo, error) {
	return r.SQLServerSequences(ctx, nil)
}

func (r *PostgresSQLServerReader) SQLServerSequences(ctx context.Context, tables []string) ([]SequenceInfo, error) {
	selected := sqlServerTableSet(tables)
	var snapshot PGSnapshot
	if err := r.loadPGOracleSequences(ctx, &snapshot, selected); err != nil {
		return nil, err
	}
	sequences := make([]SequenceInfo, 0, len(snapshot.Sequences))
	for _, seq := range snapshot.Sequences {
		sequences = append(sequences, pgSQLServerSequence(seq, r.schema, selected))
	}
	return sequences, nil
}

func pgSQLServerSequence(seq PGSequence, schema string, selected map[string]bool) SequenceInfo {
	result := seq.SequenceInfo
	result.StartValueText = strconv.FormatInt(seq.StartValue, 10)
	result.Native = &NativeSequenceInfo{
		Name: seq.Name, Schema: seq.Schema, Cycle: seq.Cycle,
		IncrementBy: strconv.FormatInt(seq.Increment, 10), MinValue: strconv.FormatInt(seq.Min, 10),
		MaxValue: strconv.FormatInt(seq.Max, 10), CacheSize: strconv.FormatInt(seq.Cache, 10),
	}
	if seq.Exhausted {
		result.Unsupported = "源序列已耗尽，无法迁移后续值"
	}
	if result.Unsupported != "" && !seq.Exhausted {
		result.FailureStage = "metadata"
	}
	for _, row := range seq.Bindings {
		binding := SequenceBinding{Schema: row.Schema, TableName: row.TableName, ColumnName: row.ColumnName, Identity: row.Identity, Default: row.Default}
		switch {
		case row.Identity != "" && row.Identity != "0":
			binding.Kind = "identity"
		case row.Owned && pgSimpleNextval(row.Default):
			binding.Kind = "serial"
		case pgSimpleNextval(row.Default):
			binding.Kind = "default"
		case row.Owned && row.Default == "":
			binding.Kind = "ownership"
		default:
			binding.Kind = "expression"
		}
		result.Native.Bindings = append(result.Native.Bindings, binding)
		if result.TableName == "" && row.Schema == schema && (selected == nil || selected[row.TableName]) {
			result.TableName, result.ColumnName = row.TableName, row.ColumnName
		}
	}
	return result
}

func (r *PostgresSQLServerReader) SQLServerDecimalProfile(ctx context.Context, table string, columns []string, consume func(string, interface{}) error) error {
	return sqlServerDecimalProfile(ctx, r.db, QuotePGIdent(r.schema)+"."+QuotePGIdent(table), columns, func(name string) string {
		return QuotePGIdent(name) + "::text"
	}, consume)
}

var _ Reader = (*PostgresSQLServerReader)(nil)
