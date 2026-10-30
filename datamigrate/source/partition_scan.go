package source

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

type partitionQueryer interface {
	QueryContext(context.Context, string, ...interface{}) (*sql.Rows, error)
}

// streamPartitionQuery keeps one server result set alive across batches. It
// neither depends on row identifiers being globally unique across leaves nor
// re-issues OFFSET queries with an unspecified ordering.
func streamPartitionQuery(ctx context.Context, db partitionQueryer, query string, batchSize int, typesOverride []string, normalize func([]interface{}, []string), consume func([]string, []string, [][]interface{}) error) error {
	if batchSize <= 0 || consume == nil {
		return fmt.Errorf("分区流式读取需要正数批次大小和回调")
	}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return err
	}
	descriptors, err := rows.ColumnTypes()
	if err != nil {
		return err
	}
	types := make([]string, len(descriptors))
	for i, descriptor := range descriptors {
		types[i] = strings.ToUpper(descriptor.DatabaseTypeName())
	}
	if typesOverride != nil {
		if len(typesOverride) != len(types) {
			return fmt.Errorf("分区读取投影列数与冻结列类型不一致")
		}
		copy(types, typesOverride)
	}
	batch := make([][]interface{}, 0, batchSize)
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		values, pointers := make([]interface{}, len(columns)), make([]interface{}, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return err
		}
		// Retained batches must never alias a driver's reusable row buffer.
		for i, value := range values {
			if bytes, ok := value.([]byte); ok {
				values[i] = append([]byte{}, bytes...)
			}
		}
		if normalize != nil {
			normalize(values, types)
		}
		batch = append(batch, values)
		if len(batch) == batchSize {
			if err := consume(columns, types, batch); err != nil {
				return err
			}
			batch = make([][]interface{}, 0, batchSize)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(batch) > 0 {
		return consume(columns, types, batch)
	}
	return nil
}

func (r *MySQLReader) StreamPartitionRows(ctx context.Context, table string, batchSize int, consume func([]string, []string, [][]interface{}) error) error {
	normalize := func(values []interface{}, types []string) {
		for i, value := range values {
			if bytes, ok := value.([]byte); ok && !strings.Contains(types[i], "BLOB") && !strings.Contains(types[i], "BINARY") && types[i] != "BIT" && types[i] != "GEOMETRY" {
				// Preserve NUL as well: a destination that cannot store it must
				// report a write error rather than silently change a routing key.
				values[i] = string(bytes)
			}
		}
	}
	if r.oracleTarget {
		normalize = nil // Oracle/SQL Server preserve raw decimals, dates and NUL.
	}
	return streamPartitionQuery(ctx, r, "SELECT * FROM "+mysqlOracleIdent(r.dbName)+"."+mysqlOracleIdent(table), batchSize, nil, normalize, consume)
}

// QueryContext deliberately uses the captured MySQL transaction when present.
func (r *MySQLReader) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	return r.queryContext(ctx, query, args...)
}

func (r *PostgresReader) StreamPartitionRows(ctx context.Context, table string, batchSize int, consume func([]string, []string, [][]interface{}) error) error {
	normalize := func(values []interface{}, types []string) {
		for i, value := range values {
			if bytes, ok := value.([]byte); ok && types[i] != "BYTEA" {
				values[i] = string(bytes)
			}
		}
	}
	return streamPartitionQuery(ctx, r.db, "SELECT * FROM "+QuotePGIdent(r.schema)+"."+QuotePGIdent(table), batchSize, nil, normalize, consume)
}

func (r *OracleReader) StreamPartitionRows(ctx context.Context, table string, batchSize int, consume func([]string, []string, [][]interface{}) error) error {
	return streamPartitionQuery(ctx, r.db, "SELECT * FROM "+quoteOracleDameng(r.owner)+"."+quoteOracleDameng(table), batchSize, nil, partitionTextValues, consume)
}

func (r *OracleDamengReader) StreamPartitionRows(ctx context.Context, table string, batchSize int, consume func([]string, []string, [][]interface{}) error) error {
	info, err := r.GetTableDDLInfo(ctx, table)
	if err != nil {
		return err
	}
	var projections, types []string
	for _, column := range info.Columns {
		if column.Generated != "" {
			return fmt.Errorf("表 %s 列 %s 为虚拟列，暂不支持迁移", table, column.Name)
		}
		projections = append(projections, oracleDamengProjection(column))
		types = append(types, strings.ToUpper(OracleDamengSourceType(column)))
	}
	return streamPartitionQuery(ctx, r.db, "SELECT "+strings.Join(projections, ", ")+" FROM "+r.tableName(table)+" DBG_SOURCE", batchSize, types, nil, consume)
}

func (r *SQLServerReader) StreamPartitionRows(ctx context.Context, table string, batchSize int, consume func([]string, []string, [][]interface{}) error) error {
	// Resolve the exact source schema; dbName is a database, never a schema.
	spec, err := r.PartitionMetadata(ctx, table)
	if err != nil {
		return err
	}
	if spec == nil || len(spec.Partitions) == 0 || spec.Partitions[0].Schema == "" {
		return fmt.Errorf("分区流式读取前源表身份或分区结构已变化")
	}
	normalize := func(values []interface{}, types []string) {
		partitionTextValues(values, types)
		for i, value := range values {
			if flag, ok := value.(bool); ok && types[i] == "BIT" {
				if flag {
					values[i] = int64(1)
				} else {
					values[i] = int64(0)
				}
			}
		}
	}
	return streamPartitionQuery(ctx, r.db, "SELECT * FROM "+quoteMSSQL(spec.Partitions[0].Schema)+"."+quoteMSSQL(table), batchSize, nil, normalize, consume)
}

func (r *SQLServerDamengReader) StreamPartitionRows(ctx context.Context, table string, batchSize int, consume func([]string, []string, [][]interface{}) error) error {
	return streamPartitionQuery(ctx, r.db, "SELECT * FROM "+dboTable(table), batchSize, nil, nil, consume)
}

func (r *DaMengReader) StreamPartitionRows(ctx context.Context, table string, batchSize int, consume func([]string, []string, [][]interface{}) error) error {
	return streamPartitionQuery(ctx, r.db, "SELECT * FROM "+QuotePGIdent(r.schema)+"."+QuotePGIdent(table), batchSize, nil, partitionTextValues, consume)
}

func partitionTextValues(values []interface{}, types []string) {
	for i, value := range values {
		switch types[i] {
		case "CHAR", "VARCHAR", "VARCHAR2", "NCHAR", "NVARCHAR", "NVARCHAR2", "TEXT", "NTEXT", "CLOB", "NCLOB", "LONG", "LONGVARCHAR":
			if bytes, ok := value.([]byte); ok {
				values[i] = string(bytes)
			}
		}
	}
}
