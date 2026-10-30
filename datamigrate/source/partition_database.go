package source

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// ReadPartitionMetadata reuses source catalog readers for target verification.
// It neither opens/closes db nor executes DDL or evaluates source expressions.
func ReadPartitionMetadata(ctx context.Context, db *sql.DB, product, schema, table string, versions ...string) (*PartitionSpec, error) {
	switch product {
	case "mysql":
		return (&MySQLReader{db: db, dbName: schema}).PartitionMetadata(ctx, table)
	case "oracle":
		return (&OracleReader{db: db, owner: schema}).PartitionMetadata(ctx, table)
	case "sqlserver":
		return sqlServerPartitionMetadata(ctx, db, schema, table)
	case "dameng":
		return (&DaMengReader{db: db, schema: schema}).PartitionMetadata(ctx, table)
	default:
		if IsPGFamily(product) {
			version := ""
			if len(versions) > 0 {
				version = versions[0]
			}
			return (&PostgresReader{db: db, schema: schema, profile: PGProfile{Product: product, Version: version}}).PartitionMetadata(ctx, table)
		}
		return nil, fmt.Errorf("数据库 %s 缺少分区目录适配器", product)
	}
}

func ReadPartitionRoots(ctx context.Context, db *sql.DB, product, schema string) (map[string]string, error) {
	switch product {
	case "mysql":
		return (&MySQLReader{db: db, dbName: schema}).PartitionRoots(ctx)
	case "oracle":
		return (&OracleReader{db: db, owner: schema}).PartitionRoots(ctx)
	case "sqlserver":
		return (&SQLServerReader{db: db, dbName: schema}).PartitionRoots(ctx)
	case "dameng":
		return (&DaMengReader{db: db, schema: schema}).PartitionRoots(ctx)
	default:
		if IsPGFamily(product) {
			return (&PostgresReader{db: db, schema: schema, profile: PGProfile{Product: product}}).PartitionRoots(ctx)
		}
		return nil, fmt.Errorf("数据库 %s 缺少分区根表目录适配器", product)
	}
}

// CountPartitionRows returns native physical-leaf counts, never predicates
// reconstructed from untrusted source expressions. Keys are schema.name, or the
// native ordinal for SQL Server, matching the persistent partition mapping.
func CountPartitionRows(ctx context.Context, db *sql.DB, product, schema, table string, spec *PartitionSpec) (map[string]int64, error) {
	return countPartitionRows(ctx, db, product, schema, table, spec)
}

func countPartitionRows(ctx context.Context, db partitionQueryer, product, schema, table string, spec *PartitionSpec) (map[string]int64, error) {
	if spec == nil || spec.Unsupported != "" {
		return nil, fmt.Errorf("缺少可核验的完整分区结构")
	}
	result := map[string]int64{}
	qualified := func(owner, name string) string { return QuotePGIdent(owner) + "." + QuotePGIdent(name) }
	if product == "sqlserver" {
		function := strings.TrimPrefix(spec.Algorithm, "sqlserver-range:")
		if function == spec.Algorithm || function == "" || len(spec.Keys) != 1 || spec.Keys[0].Column == "" {
			return nil, fmt.Errorf("缺少 SQL Server 原生分区函数或分区键")
		}
		owner := schema
		if owner == "" {
			owner = "dbo"
		}
		query := "SELECT $PARTITION." + quoteMSSQL(function) + "(" + quoteMSSQL(spec.Keys[0].Column) + "),COUNT_BIG(*) FROM " + quoteMSSQL(owner) + "." + quoteMSSQL(table) + " GROUP BY $PARTITION." + quoteMSSQL(function) + "(" + quoteMSSQL(spec.Keys[0].Column) + ")"
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for _, part := range spec.Partitions {
			result[part.Name] = 0
		}
		for rows.Next() {
			var ordinal string
			var count int64
			if err := rows.Scan(&ordinal, &count); err != nil {
				return nil, err
			}
			if _, known := result[ordinal]; !known {
				return nil, fmt.Errorf("发现冻结目录外的 SQL Server 分区 %s", ordinal)
			}
			result[ordinal] = count
		}
		return result, rows.Err()
	}
	var visit func(*PartitionSpec, bool) error
	visit = func(current *PartitionSpec, nested bool) error {
		if current.Unsupported != "" {
			return fmt.Errorf("子分区结构不可核验: %s", current.Unsupported)
		}
		for _, part := range current.Partitions {
			if part.Subpartition != nil {
				if err := visit(part.Subpartition, true); err != nil {
					return err
				}
				continue
			}
			owner := part.Schema
			if owner == "" {
				owner = schema
			}
			key := part.Name
			if owner != "" {
				key = owner + "." + part.Name
			}
			if _, duplicate := result[key]; duplicate {
				return fmt.Errorf("分区叶身份重复: %s", key)
			}
			var query string
			switch product {
			case "mysql":
				query = "SELECT COUNT(*) FROM " + mysqlOracleIdent(schema) + "." + mysqlOracleIdent(table) + " PARTITION (" + mysqlOracleIdent(part.Name) + ")"
			case "oracle", "dameng":
				kind := "PARTITION"
				if nested {
					kind = "SUBPARTITION"
				}
				query = "SELECT COUNT(*) FROM " + qualified(schema, table) + " " + kind + " (" + QuotePGIdent(part.Name) + ")"
			default:
				if !IsPGFamily(product) {
					return fmt.Errorf("数据库 %s 缺少原生叶分区计数", product)
				}
				if strings.HasPrefix(spec.Algorithm, "pg_partition:") {
					kind := "PARTITION"
					if nested {
						kind = "SUBPARTITION"
					}
					query = "SELECT COUNT(*) FROM " + qualified(schema, table) + " " + kind + " (" + QuotePGIdent(part.Name) + ")"
				} else {
					query = "SELECT COUNT(*) FROM ONLY " + qualified(owner, part.Name)
				}
			}
			rows, err := db.QueryContext(ctx, query)
			if err != nil {
				return err
			}
			var count int64
			if !rows.Next() {
				err = rows.Err()
				if err == nil {
					err = fmt.Errorf("叶分区计数缺少结果")
				}
			} else if err = rows.Scan(&count); err == nil {
				if rows.Next() {
					err = fmt.Errorf("叶分区计数返回多个结果")
				} else {
					err = rows.Err()
				}
			}
			closeErr := rows.Close()
			if err == nil {
				err = closeErr
			}
			if err != nil {
				return err
			}
			result[key] = count
		}
		return nil
	}
	if err := visit(spec, false); err != nil {
		return nil, err
	}
	return result, nil
}

func (r *MySQLReader) CountPartitionRows(ctx context.Context, table string, spec *PartitionSpec) (map[string]int64, error) {
	return countPartitionRows(ctx, r, "mysql", r.dbName, table, spec)
}
func (r *PostgresReader) CountPartitionRows(ctx context.Context, table string, spec *PartitionSpec) (map[string]int64, error) {
	return countPartitionRows(ctx, r.db, "postgres", r.schema, table, spec)
}
func (r *OracleReader) CountPartitionRows(ctx context.Context, table string, spec *PartitionSpec) (map[string]int64, error) {
	return countPartitionRows(ctx, r.db, "oracle", r.owner, table, spec)
}
func (r *DaMengReader) CountPartitionRows(ctx context.Context, table string, spec *PartitionSpec) (map[string]int64, error) {
	return countPartitionRows(ctx, r.db, "dameng", r.schema, table, spec)
}
func (r *SQLServerReader) CountPartitionRows(ctx context.Context, table string, spec *PartitionSpec) (map[string]int64, error) {
	schema := "dbo"
	if spec != nil && len(spec.Partitions) > 0 && spec.Partitions[0].Schema != "" {
		schema = spec.Partitions[0].Schema
	}
	return countPartitionRows(ctx, r.db, "sqlserver", schema, table, spec)
}
