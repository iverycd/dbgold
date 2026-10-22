package source

import (
	"context"
	"dbgold/datamigrate/validation"
)

func (r *MySQLReader) DescribeValidationTable(ctx context.Context, table string) (validation.Table, error) {
	return validation.NewSQLScanner(r.db, "mysql", r.dbName).DescribeValidationTable(ctx, table)
}
func (r *MySQLReader) OpenValidationScan(ctx context.Context, table string, cols []validation.Column) (validation.RowStream, error) {
	return validation.NewSQLScanner(r.db, "mysql", r.dbName).OpenValidationScan(ctx, table, cols)
}
func (r *PostgresReader) DescribeValidationTable(ctx context.Context, table string) (validation.Table, error) {
	return validation.NewSQLScanner(r.db, "postgres", r.schema).WithCatalog(r.profile.Catalog).DescribeValidationTable(ctx, table)
}
func (r *PostgresReader) OpenValidationScan(ctx context.Context, table string, cols []validation.Column) (validation.RowStream, error) {
	return validation.NewSQLScanner(r.db, "postgres", r.schema).WithCatalog(r.profile.Catalog).OpenValidationScan(ctx, table, cols)
}
func (r *OracleReader) DescribeValidationTable(ctx context.Context, table string) (validation.Table, error) {
	return validation.NewSQLScanner(r.db, "oracle", r.owner).DescribeValidationTable(ctx, table)
}
func (r *OracleReader) OpenValidationScan(ctx context.Context, table string, cols []validation.Column) (validation.RowStream, error) {
	return validation.NewSQLScanner(r.db, "oracle", r.owner).OpenValidationScan(ctx, table, cols)
}
func (r *SQLServerReader) DescribeValidationTable(ctx context.Context, table string) (validation.Table, error) {
	return validation.NewSQLScanner(r.db, "sqlserver", "").DescribeValidationTable(ctx, table)
}
func (r *SQLServerReader) OpenValidationScan(ctx context.Context, table string, cols []validation.Column) (validation.RowStream, error) {
	return validation.NewSQLScanner(r.db, "sqlserver", "").OpenValidationScan(ctx, table, cols)
}
func (r *DaMengReader) DescribeValidationTable(ctx context.Context, table string) (validation.Table, error) {
	return validation.NewSQLScanner(r.db, "dameng", r.schema).DescribeValidationTable(ctx, table)
}
func (r *DaMengReader) OpenValidationScan(ctx context.Context, table string, cols []validation.Column) (validation.RowStream, error) {
	return validation.NewSQLScanner(r.db, "dameng", r.schema).OpenValidationScan(ctx, table, cols)
}
