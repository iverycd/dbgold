package migrate

import (
	"dbgold/diff"
	"dbgold/schema"
	"fmt"
	"strings"
)

func PostgresGenerateDiffSQL(r *diff.Result) ([]string, error) {
	var sqls []string
	for _, t := range r.AddedTables {
		sqls = append(sqls, postgresCreateTable(t))
	}
	for _, t := range r.DroppedTables {
		sqls = append(sqls, fmt.Sprintf(`DROP TABLE IF EXISTS "%s"`, t.Name))
	}
	for _, td := range r.ModifiedTables {
		for _, col := range td.AddedColumns {
			sqls = append(sqls, postgresAddColumn(td.TableName, col))
		}
		for _, col := range td.DroppedColumns {
			sqls = append(sqls, fmt.Sprintf(`ALTER TABLE "%s" DROP COLUMN "%s"`, td.TableName, col.Name))
		}
		for _, cd := range td.ModifiedColumns {
			sqls = append(sqls, postgresModifyColumn(td.TableName, cd)...)
		}
		for _, idx := range td.AddedIndexes {
			sqls = append(sqls, postgresCreateIndex(td.TableName, idx))
		}
		for _, idx := range td.DroppedIndexes {
			sqls = append(sqls, fmt.Sprintf(`DROP INDEX "%s"`, idx.Name))
		}
		for _, fk := range td.AddedForeignKeys {
			sqls = append(sqls, postgresAddForeignKey(td.TableName, fk))
		}
		for _, fk := range td.DroppedForeignKeys {
			sqls = append(sqls, fmt.Sprintf(`ALTER TABLE "%s" DROP CONSTRAINT "%s"`, td.TableName, fk.Name))
		}
	}
	return sqls, nil
}

func PostgresGenerateFullMigrationSQL(src, dst *schema.FullSchema) ([]string, error) {
	var sqls []string
	for _, seq := range dst.Sequences {
		sqls = append(sqls, fmt.Sprintf(`CREATE SEQUENCE "%s" START %d INCREMENT BY %d`, seq.Name, seq.Start, seq.Increment))
	}
	for _, t := range dst.Tables {
		sqls = append(sqls, postgresCreateTable(t))
	}
	for _, v := range dst.Views {
		sqls = append(sqls, fmt.Sprintf(`CREATE OR REPLACE VIEW "%s" AS %s`, v.Name, v.Def))
	}
	for _, tr := range dst.Triggers {
		sqls = append(sqls, tr.Body)
	}
	return sqls, nil
}

func PostgresGenerateSelectiveSQL(objects *schema.SelectedObjects) ([]string, error) {
	var sqls []string
	for _, seq := range objects.Sequences {
		sqls = append(sqls, fmt.Sprintf(`CREATE SEQUENCE "%s" START %d INCREMENT BY %d`, seq.Name, seq.Start, seq.Increment))
	}
	for _, t := range objects.Tables {
		sqls = append(sqls, postgresCreateTable(t))
	}
	for _, v := range objects.Views {
		sqls = append(sqls, fmt.Sprintf(`CREATE OR REPLACE VIEW "%s" AS %s`, v.Name, v.Def))
	}
	for _, tr := range objects.Triggers {
		sqls = append(sqls, tr.Body)
	}
	return sqls, nil
}

func postgresCreateTable(t schema.Table) string {
	var lines []string
	var pkCols []string
	for _, col := range t.Columns {
		lines = append(lines, "  "+postgresColumnDef(col))
		if col.PrimaryKey {
			pkCols = append(pkCols, fmt.Sprintf(`"%s"`, col.Name))
		}
	}
	if len(pkCols) > 0 {
		lines = append(lines, fmt.Sprintf("  PRIMARY KEY (%s)", strings.Join(pkCols, ", ")))
	}
	return fmt.Sprintf("CREATE TABLE \"%s\" (\n%s\n)", t.Name, strings.Join(lines, ",\n"))
}

func postgresColumnDef(col schema.Column) string {
	def := fmt.Sprintf(`"%s" %s`, col.Name, col.Type)
	if !col.Nullable {
		def += " NOT NULL"
	}
	if col.Default != nil {
		def += fmt.Sprintf(" DEFAULT %s", *col.Default)
	}
	return def
}

func postgresAddColumn(table string, col schema.Column) string {
	def := fmt.Sprintf(`ALTER TABLE "%s" ADD COLUMN "%s" %s`, table, col.Name, col.Type)
	if !col.Nullable {
		def += " NOT NULL"
	}
	if col.Default != nil {
		def += fmt.Sprintf(" DEFAULT %s", *col.Default)
	}
	return def
}

func postgresModifyColumn(table string, cd diff.ColumnDiff) []string {
	var sqls []string
	if cd.TypeChanged {
		sqls = append(sqls, fmt.Sprintf(`ALTER TABLE "%s" ALTER COLUMN "%s" TYPE %s`, table, cd.Column.Name, cd.Column.Type))
	}
	if cd.NullableChanged {
		if cd.Column.Nullable {
			sqls = append(sqls, fmt.Sprintf(`ALTER TABLE "%s" ALTER COLUMN "%s" DROP NOT NULL`, table, cd.Column.Name))
		} else {
			sqls = append(sqls, fmt.Sprintf(`ALTER TABLE "%s" ALTER COLUMN "%s" SET NOT NULL`, table, cd.Column.Name))
		}
	}
	if cd.DefaultChanged {
		if cd.Column.Default != nil {
			sqls = append(sqls, fmt.Sprintf(`ALTER TABLE "%s" ALTER COLUMN "%s" SET DEFAULT %s`, table, cd.Column.Name, *cd.Column.Default))
		} else {
			sqls = append(sqls, fmt.Sprintf(`ALTER TABLE "%s" ALTER COLUMN "%s" DROP DEFAULT`, table, cd.Column.Name))
		}
	}
	return sqls
}

func postgresCreateIndex(table string, idx schema.Index) string {
	cols := make([]string, len(idx.Columns))
	for i, c := range idx.Columns {
		cols[i] = fmt.Sprintf(`"%s"`, c)
	}
	unique := ""
	if idx.Unique {
		unique = "UNIQUE "
	}
	if table == "" {
		return fmt.Sprintf(`CREATE %sINDEX "%s" (%s)`, unique, idx.Name, strings.Join(cols, ", "))
	}
	return fmt.Sprintf(`CREATE %sINDEX "%s" ON "%s" (%s)`, unique, idx.Name, table, strings.Join(cols, ", "))
}

func postgresAddForeignKey(table string, fk schema.ForeignKey) string {
	cols := make([]string, len(fk.Columns))
	for i, c := range fk.Columns {
		cols[i] = fmt.Sprintf(`"%s"`, c)
	}
	refCols := make([]string, len(fk.RefColumns))
	for i, c := range fk.RefColumns {
		refCols[i] = fmt.Sprintf(`"%s"`, c)
	}
	s := fmt.Sprintf(`ALTER TABLE "%s" ADD CONSTRAINT "%s" FOREIGN KEY (%s) REFERENCES "%s"(%s)`,
		table, fk.Name, strings.Join(cols, ", "), fk.RefTable, strings.Join(refCols, ", "))
	if fk.OnDelete != "" {
		s += " ON DELETE " + fk.OnDelete
	}
	if fk.OnUpdate != "" {
		s += " ON UPDATE " + fk.OnUpdate
	}
	return s
}
