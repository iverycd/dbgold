package migrate

import (
	"dbgold/diff"
	"dbgold/schema"
	"fmt"
	"strings"
)

func normName(s string, lower bool) string {
	if lower {
		return strings.ToLower(s)
	}
	return s
}

func PostgresGenerateDiffSQL(r *diff.Result, lowerCase bool) ([]string, error) {
	var sqls []string
	for _, t := range r.AddedTables {
		sqls = append(sqls, postgresCreateTable(t, lowerCase))
	}
	for _, t := range r.DroppedTables {
		sqls = append(sqls, fmt.Sprintf(`DROP TABLE IF EXISTS "%s"`, normName(t.Name, lowerCase)))
	}
	for _, td := range r.ModifiedTables {
		for _, col := range td.AddedColumns {
			sqls = append(sqls, postgresAddColumn(normName(td.TableName, lowerCase), col, lowerCase))
		}
		for _, col := range td.DroppedColumns {
			sqls = append(sqls, fmt.Sprintf(`ALTER TABLE "%s" DROP COLUMN "%s"`, normName(td.TableName, lowerCase), normName(col.Name, lowerCase)))
		}
		for _, cd := range td.ModifiedColumns {
			sqls = append(sqls, postgresModifyColumn(normName(td.TableName, lowerCase), cd, lowerCase)...)
		}
		for _, idx := range td.AddedIndexes {
			sqls = append(sqls, postgresCreateIndex(normName(td.TableName, lowerCase), idx, lowerCase))
		}
		for _, idx := range td.DroppedIndexes {
			sqls = append(sqls, fmt.Sprintf(`DROP INDEX "%s"`, normName(idx.Name, lowerCase)))
		}
		for _, fk := range td.AddedForeignKeys {
			sqls = append(sqls, postgresAddForeignKey(normName(td.TableName, lowerCase), fk, lowerCase))
		}
		for _, fk := range td.DroppedForeignKeys {
			sqls = append(sqls, fmt.Sprintf(`ALTER TABLE "%s" DROP CONSTRAINT "%s"`, normName(td.TableName, lowerCase), normName(fk.Name, lowerCase)))
		}
	}
	return sqls, nil
}

func PostgresGenerateFullMigrationSQL(src, dst *schema.FullSchema, lowerCase bool) ([]string, error) {
	var sqls []string
	for _, seq := range dst.Sequences {
		sqls = append(sqls, fmt.Sprintf(`CREATE SEQUENCE "%s" START %d INCREMENT BY %d`, normName(seq.Name, lowerCase), seq.Start, seq.Increment))
	}
	for _, t := range dst.Tables {
		sqls = append(sqls, postgresCreateTable(t, lowerCase))
	}
	for _, v := range dst.Views {
		sqls = append(sqls, fmt.Sprintf(`CREATE OR REPLACE VIEW "%s" AS %s`, normName(v.Name, lowerCase), v.Def))
	}
	for _, tr := range dst.Triggers {
		sqls = append(sqls, tr.Body)
	}
	return sqls, nil
}

func PostgresGenerateSelectiveSQL(objects *schema.SelectedObjects, lowerCase bool) ([]string, error) {
	var sqls []string
	for _, seq := range objects.Sequences {
		sqls = append(sqls, fmt.Sprintf(`CREATE SEQUENCE "%s" START %d INCREMENT BY %d`, normName(seq.Name, lowerCase), seq.Start, seq.Increment))
	}
	for _, t := range objects.Tables {
		sqls = append(sqls, postgresCreateTable(t, lowerCase))
	}
	for _, v := range objects.Views {
		sqls = append(sqls, fmt.Sprintf(`CREATE OR REPLACE VIEW "%s" AS %s`, normName(v.Name, lowerCase), v.Def))
	}
	for _, tr := range objects.Triggers {
		sqls = append(sqls, tr.Body)
	}
	return sqls, nil
}

func postgresCreateTable(t schema.Table, lowerCase bool) string {
	var lines []string
	var pkCols []string
	for _, col := range t.Columns {
		lines = append(lines, "  "+postgresColumnDef(col, lowerCase))
		if col.PrimaryKey {
			pkCols = append(pkCols, fmt.Sprintf(`"%s"`, normName(col.Name, lowerCase)))
		}
	}
	if len(pkCols) > 0 {
		lines = append(lines, fmt.Sprintf("  PRIMARY KEY (%s)", strings.Join(pkCols, ", ")))
	}
	return fmt.Sprintf("CREATE TABLE \"%s\" (\n%s\n)", normName(t.Name, lowerCase), strings.Join(lines, ",\n"))
}

func postgresColumnDef(col schema.Column, lowerCase bool) string {
	def := fmt.Sprintf(`"%s" %s`, normName(col.Name, lowerCase), col.Type)
	if !col.Nullable {
		def += " NOT NULL"
	}
	if col.Default != nil {
		def += fmt.Sprintf(" DEFAULT %s", *col.Default)
	}
	return def
}

func postgresAddColumn(table string, col schema.Column, lowerCase bool) string {
	def := fmt.Sprintf(`ALTER TABLE "%s" ADD COLUMN "%s" %s`, table, normName(col.Name, lowerCase), col.Type)
	if !col.Nullable {
		def += " NOT NULL"
	}
	if col.Default != nil {
		def += fmt.Sprintf(" DEFAULT %s", *col.Default)
	}
	return def
}

func postgresModifyColumn(table string, cd diff.ColumnDiff, lowerCase bool) []string {
	var sqls []string
	colName := normName(cd.Column.Name, lowerCase)
	if cd.TypeChanged {
		sqls = append(sqls, fmt.Sprintf(`ALTER TABLE "%s" ALTER COLUMN "%s" TYPE %s`, table, colName, cd.Column.Type))
	}
	if cd.NullableChanged {
		if cd.Column.Nullable {
			sqls = append(sqls, fmt.Sprintf(`ALTER TABLE "%s" ALTER COLUMN "%s" DROP NOT NULL`, table, colName))
		} else {
			sqls = append(sqls, fmt.Sprintf(`ALTER TABLE "%s" ALTER COLUMN "%s" SET NOT NULL`, table, colName))
		}
	}
	if cd.DefaultChanged {
		if cd.Column.Default != nil {
			sqls = append(sqls, fmt.Sprintf(`ALTER TABLE "%s" ALTER COLUMN "%s" SET DEFAULT %s`, table, colName, *cd.Column.Default))
		} else {
			sqls = append(sqls, fmt.Sprintf(`ALTER TABLE "%s" ALTER COLUMN "%s" DROP DEFAULT`, table, colName))
		}
	}
	return sqls
}

func postgresCreateIndex(table string, idx schema.Index, lowerCase bool) string {
	cols := make([]string, len(idx.Columns))
	for i, c := range idx.Columns {
		cols[i] = fmt.Sprintf(`"%s"`, normName(c, lowerCase))
	}
	unique := ""
	if idx.Unique {
		unique = "UNIQUE "
	}
	if table == "" {
		return fmt.Sprintf(`CREATE %sINDEX "%s" (%s)`, unique, normName(idx.Name, lowerCase), strings.Join(cols, ", "))
	}
	return fmt.Sprintf(`CREATE %sINDEX "%s" ON "%s" (%s)`, unique, normName(idx.Name, lowerCase), table, strings.Join(cols, ", "))
}

func postgresAddForeignKey(table string, fk schema.ForeignKey, lowerCase bool) string {
	cols := make([]string, len(fk.Columns))
	for i, c := range fk.Columns {
		cols[i] = fmt.Sprintf(`"%s"`, normName(c, lowerCase))
	}
	refCols := make([]string, len(fk.RefColumns))
	for i, c := range fk.RefColumns {
		refCols[i] = fmt.Sprintf(`"%s"`, normName(c, lowerCase))
	}
	s := fmt.Sprintf(`ALTER TABLE "%s" ADD CONSTRAINT "%s" FOREIGN KEY (%s) REFERENCES "%s"(%s)`,
		table, normName(fk.Name, lowerCase), strings.Join(cols, ", "), normName(fk.RefTable, lowerCase), strings.Join(refCols, ", "))
	if fk.OnDelete != "" {
		s += " ON DELETE " + fk.OnDelete
	}
	if fk.OnUpdate != "" {
		s += " ON UPDATE " + fk.OnUpdate
	}
	return s
}
