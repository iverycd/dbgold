package source

import (
	"context"
	"dbgold/datamigrate/validation"
)

func (r *MySQLReader) OpenPartitionValidationScan(ctx context.Context, table string, p validation.PartitionLocation, c []validation.Column) (validation.RowStream, error) {
	return validation.NewSQLScanner(r.db, "mysql", r.dbName).WithPartition(p).OpenValidationScan(ctx, table, c)
}
func (r *PostgresReader) OpenPartitionValidationScan(ctx context.Context, table string, p validation.PartitionLocation, c []validation.Column) (validation.RowStream, error) {
	return validation.NewSQLScanner(r.db, "postgres", r.schema).WithCatalog(r.profile.Catalog).WithPartition(p).OpenValidationScan(ctx, table, c)
}
func (r *OracleReader) OpenPartitionValidationScan(ctx context.Context, table string, p validation.PartitionLocation, c []validation.Column) (validation.RowStream, error) {
	return validation.NewSQLScanner(r.db, "oracle", r.owner).WithPartition(p).OpenValidationScan(ctx, table, c)
}
func (r *SQLServerReader) OpenPartitionValidationScan(ctx context.Context, table string, p validation.PartitionLocation, c []validation.Column) (validation.RowStream, error) {
	return validation.NewSQLScanner(r.db, "sqlserver", "").WithPartition(p).OpenValidationScan(ctx, table, c)
}
func (r *DaMengReader) OpenPartitionValidationScan(ctx context.Context, table string, p validation.PartitionLocation, c []validation.Column) (validation.RowStream, error) {
	return validation.NewSQLScanner(r.db, "dameng", r.schema).WithPartition(p).OpenValidationScan(ctx, table, c)
}
