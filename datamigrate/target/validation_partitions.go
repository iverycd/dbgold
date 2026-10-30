package target

import (
	"context"
	"fmt"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/validation"
)

func (w *PostgresWriter) OpenPartitionValidationScan(ctx context.Context, table string, p validation.PartitionLocation, cols []validation.Column) (validation.RowStream, error) {
	return validation.NewSQLScanner(w.db, "postgres", w.schema).WithPartition(p).OpenValidationScan(ctx, table, cols)
}
func (w *GaussDBWriter) OpenPartitionValidationScan(ctx context.Context, table string, p validation.PartitionLocation, cols []validation.Column) (validation.RowStream, error) {
	return validation.NewSQLScanner(w.db, "postgres", w.schema).WithPartition(p).OpenValidationScan(ctx, table, cols)
}
func (w *HighGoWriter) OpenPartitionValidationScan(ctx context.Context, table string, p validation.PartitionLocation, cols []validation.Column) (validation.RowStream, error) {
	return validation.NewSQLScanner(w.db, "postgres", w.schema).WithPartition(p).OpenValidationScan(ctx, table, cols)
}
func (w *KingbaseWriter) OpenPartitionValidationScan(ctx context.Context, table string, p validation.PartitionLocation, cols []validation.Column) (validation.RowStream, error) {
	return validation.NewSQLScanner(w.db, "postgres", w.schema).WithPartition(p).OpenValidationScan(ctx, table, cols)
}
func (w *MySQLWriter) OpenPartitionValidationScan(ctx context.Context, table string, p validation.PartitionLocation, cols []validation.Column) (validation.RowStream, error) {
	return validation.NewSQLScanner(w.db, "mysql", w.schema).WithPartition(p).OpenValidationScan(ctx, table, cols)
}
func (w *DaMengWriter) OpenPartitionValidationScan(ctx context.Context, table string, p validation.PartitionLocation, cols []validation.Column) (validation.RowStream, error) {
	return validation.NewSQLScanner(w.db, "dameng", w.schema).WithPartition(p).OpenValidationScan(ctx, table, cols)
}
func (w *OracleWriter) OpenPartitionValidationScan(ctx context.Context, table string, p validation.PartitionLocation, cols []validation.Column) (validation.RowStream, error) {
	return validation.NewSQLScanner(w.db, "oracle", w.schema).WithPartition(p).OpenValidationScan(ctx, table, cols)
}
func (w *SQLServerWriter) OpenPartitionValidationScan(ctx context.Context, table string, p validation.PartitionLocation, cols []validation.Column) (validation.RowStream, error) {
	return validation.NewSQLScanner(w.db, "sqlserver", w.schema).WithPartition(p).OpenValidationScan(ctx, table, cols)
}

func (w *OscarWriter) OpenPartitionValidationScan(ctx context.Context, table string, p validation.PartitionLocation, cols []validation.Column) (validation.RowStream, error) {
	actual, err := w.InspectPartitionTable(ctx, table)
	if err != nil {
		return nil, err
	}
	if actual == nil || p.Name == "" || p.Native {
		return nil, fmt.Errorf("神通内容校验缺少原生叶分区身份")
	}
	schema, err := w.partitionSchema(ctx)
	if err != nil {
		return nil, err
	}
	found := false
	var visit func(*source.PartitionSpec, bool)
	visit = func(s *source.PartitionSpec, nested bool) {
		for _, part := range s.Partitions {
			if part.Subpartition != nil {
				visit(part.Subpartition, true)
			} else if part.Name == p.Name && nested == p.Subpartition && (p.Schema == "" || p.Schema == part.Schema) && (p.Ordinal == 0 || p.Ordinal == part.Ordinal) {
				found = true
			}
		}
	}
	visit(actual, false)
	if !found {
		return nil, fmt.Errorf("神通内容校验分区不属于当前目标根表或层级/序号发生变化")
	}
	kind := "PARTITION"
	if p.Subpartition {
		kind = "SUBPARTITION"
	}
	from := w.dia.QualifyTable(schema, table) + " " + kind + " (" + w.dia.QuoteIdent(p.Name) + ")"
	return w.openValidationScan(ctx, table, cols, from)
}
