package target

import (
	"context"
	"dbgold/datamigrate/validation"
)

func (w *PostgresWriter) DescribeValidationTable(ctx context.Context, table string) (validation.Table, error) {
	return validation.NewSQLScanner(w.db, "postgres", w.schema).DescribeValidationTable(ctx, table)
}
func (w *PostgresWriter) OpenValidationScan(ctx context.Context, table string, cols []validation.Column) (validation.RowStream, error) {
	return validation.NewSQLScanner(w.db, "postgres", w.schema).OpenValidationScan(ctx, table, cols)
}
func (w *GaussDBWriter) DescribeValidationTable(ctx context.Context, table string) (validation.Table, error) {
	return validation.NewSQLScanner(w.db, "postgres", w.schema).DescribeValidationTable(ctx, table)
}
func (w *GaussDBWriter) OpenValidationScan(ctx context.Context, table string, cols []validation.Column) (validation.RowStream, error) {
	return validation.NewSQLScanner(w.db, "postgres", w.schema).OpenValidationScan(ctx, table, cols)
}
func (w *HighGoWriter) DescribeValidationTable(ctx context.Context, table string) (validation.Table, error) {
	return validation.NewSQLScanner(w.db, "postgres", w.schema).DescribeValidationTable(ctx, table)
}
func (w *HighGoWriter) OpenValidationScan(ctx context.Context, table string, cols []validation.Column) (validation.RowStream, error) {
	return validation.NewSQLScanner(w.db, "postgres", w.schema).OpenValidationScan(ctx, table, cols)
}
func (w *KingbaseWriter) DescribeValidationTable(ctx context.Context, table string) (validation.Table, error) {
	return validation.NewSQLScanner(w.db, "postgres", w.schema).DescribeValidationTable(ctx, table)
}
func (w *KingbaseWriter) OpenValidationScan(ctx context.Context, table string, cols []validation.Column) (validation.RowStream, error) {
	return validation.NewSQLScanner(w.db, "postgres", w.schema).OpenValidationScan(ctx, table, cols)
}
func (w *MySQLWriter) DescribeValidationTable(ctx context.Context, table string) (validation.Table, error) {
	return validation.NewSQLScanner(w.db, "mysql", w.schema).DescribeValidationTable(ctx, table)
}
func (w *MySQLWriter) OpenValidationScan(ctx context.Context, table string, cols []validation.Column) (validation.RowStream, error) {
	return validation.NewSQLScanner(w.db, "mysql", w.schema).OpenValidationScan(ctx, table, cols)
}
func (w *DaMengWriter) DescribeValidationTable(ctx context.Context, table string) (validation.Table, error) {
	return validation.NewSQLScanner(w.db, "dameng", w.schema).DescribeValidationTable(ctx, table)
}
func (w *DaMengWriter) OpenValidationScan(ctx context.Context, table string, cols []validation.Column) (validation.RowStream, error) {
	return validation.NewSQLScanner(w.db, "dameng", w.schema).OpenValidationScan(ctx, table, cols)
}
func (w *OracleWriter) DescribeValidationTable(ctx context.Context, table string) (validation.Table, error) {
	return validation.NewSQLScanner(w.db, "oracle", w.schema).DescribeValidationTable(ctx, table)
}
func (w *OracleWriter) OpenValidationScan(ctx context.Context, table string, cols []validation.Column) (validation.RowStream, error) {
	return validation.NewSQLScanner(w.db, "oracle", w.schema).OpenValidationScan(ctx, table, cols)
}
func (w *SQLServerWriter) DescribeValidationTable(ctx context.Context, table string) (validation.Table, error) {
	return validation.NewSQLScanner(w.db, "sqlserver", w.schema).DescribeValidationTable(ctx, table)
}
func (w *SQLServerWriter) OpenValidationScan(ctx context.Context, table string, cols []validation.Column) (validation.RowStream, error) {
	return validation.NewSQLScanner(w.db, "sqlserver", w.schema).OpenValidationScan(ctx, table, cols)
}
