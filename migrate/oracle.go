package migrate

import (
	"dbgold/diff"
	"dbgold/schema"
	"fmt"
	"strings"
)

func OracleGenerateDiffSQL(r *diff.Result, lowerCase bool) ([]string, error) {
	var sqls []string
	for _, t := range r.AddedTables {
		sqls = append(sqls, oracleCreateTable(t, lowerCase))
	}
	for _, t := range r.DroppedTables {
		sqls = append(sqls, fmt.Sprintf(`DROP TABLE "%s"`, normName(t.Name, lowerCase)))
	}
	for _, td := range r.ModifiedTables {
		tbl := normName(td.TableName, lowerCase)
		for _, col := range td.AddedColumns {
			sqls = append(sqls, fmt.Sprintf(`ALTER TABLE "%s" ADD ("%s" %s)`, tbl, normName(col.Name, lowerCase), oracleColType(col)))
		}
		for _, col := range td.DroppedColumns {
			sqls = append(sqls, fmt.Sprintf(`ALTER TABLE "%s" DROP COLUMN "%s"`, tbl, normName(col.Name, lowerCase)))
		}
		for _, cd := range td.ModifiedColumns {
			sqls = append(sqls, fmt.Sprintf(`ALTER TABLE "%s" MODIFY ("%s" %s)`, tbl, normName(cd.Column.Name, lowerCase), oracleColType(cd.Column)))
		}
		for _, idx := range td.AddedIndexes {
			sqls = append(sqls, oracleCreateIndex(tbl, idx, lowerCase))
		}
		for _, idx := range td.DroppedIndexes {
			sqls = append(sqls, fmt.Sprintf(`DROP INDEX "%s"`, normName(idx.Name, lowerCase)))
		}
		for _, fk := range td.AddedForeignKeys {
			sqls = append(sqls, oracleAddForeignKey(tbl, fk, lowerCase))
		}
		for _, fk := range td.DroppedForeignKeys {
			sqls = append(sqls, fmt.Sprintf(`ALTER TABLE "%s" DROP CONSTRAINT "%s"`, tbl, normName(fk.Name, lowerCase)))
		}
	}
	return sqls, nil
}

func OracleGenerateFullMigrationSQL(src, dst *schema.FullSchema, lowerCase bool) ([]string, error) {
	var sqls []string
	for _, seq := range dst.Sequences {
		sqls = append(sqls, fmt.Sprintf(`CREATE SEQUENCE "%s" START WITH %d INCREMENT BY %d`, normName(seq.Name, lowerCase), seq.Start, seq.Increment))
	}
	for _, t := range dst.Tables {
		sqls = append(sqls, oracleCreateTable(t, lowerCase))
	}
	for _, v := range dst.Views {
		sqls = append(sqls, fmt.Sprintf(`CREATE OR REPLACE VIEW "%s" AS %s`, normName(v.Name, lowerCase), v.Def))
	}
	for _, tr := range dst.Triggers {
		sqls = append(sqls, tr.Body)
	}
	return sqls, nil
}

func OracleGenerateSelectiveSQL(objects *schema.SelectedObjects, lowerCase bool) ([]string, error) {
	var sqls []string
	for _, seq := range objects.Sequences {
		sqls = append(sqls, fmt.Sprintf(`CREATE SEQUENCE "%s" START WITH %d INCREMENT BY %d`, normName(seq.Name, lowerCase), seq.Start, seq.Increment))
	}
	for _, t := range objects.Tables {
		sqls = append(sqls, oracleCreateTable(t, lowerCase))
	}
	for _, v := range objects.Views {
		sqls = append(sqls, fmt.Sprintf(`CREATE OR REPLACE VIEW "%s" AS %s`, normName(v.Name, lowerCase), v.Def))
	}
	for _, tr := range objects.Triggers {
		sqls = append(sqls, tr.Body)
	}
	return sqls, nil
}

func oracleCreateTable(t schema.Table, lowerCase bool) string {
	var lines []string
	var pkCols []string
	for _, col := range t.Columns {
		lines = append(lines, "  "+oracleColDef(col, lowerCase))
		if col.PrimaryKey {
			pkCols = append(pkCols, fmt.Sprintf(`"%s"`, normName(col.Name, lowerCase)))
		}
	}
	if len(pkCols) > 0 {
		lines = append(lines, fmt.Sprintf("  PRIMARY KEY (%s)", strings.Join(pkCols, ", ")))
	}
	return fmt.Sprintf("CREATE TABLE \"%s\" (\n%s\n)", normName(t.Name, lowerCase), strings.Join(lines, ",\n"))
}

func oracleColDef(col schema.Column, lowerCase bool) string {
	def := fmt.Sprintf(`"%s" %s`, normName(col.Name, lowerCase), col.Type)
	if !col.Nullable {
		def += " NOT NULL"
	}
	if col.Default != nil {
		def += " DEFAULT " + *col.Default
	}
	return def
}

func oracleColType(col schema.Column) string {
	t := col.Type
	if !col.Nullable {
		t += " NOT NULL"
	}
	return t
}

func oracleCreateIndex(table string, idx schema.Index, lowerCase bool) string {
	cols := make([]string, len(idx.Columns))
	for i, c := range idx.Columns {
		cols[i] = fmt.Sprintf(`"%s"`, normName(c, lowerCase))
	}
	unique := ""
	if idx.Unique {
		unique = "UNIQUE "
	}
	return fmt.Sprintf(`CREATE %sINDEX "%s" ON "%s" (%s)`, unique, normName(idx.Name, lowerCase), table, strings.Join(cols, ", "))
}

func oracleAddForeignKey(table string, fk schema.ForeignKey, lowerCase bool) string {
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
	return s
}
