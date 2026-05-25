package migrate

import (
	"dbgold/diff"
	"dbgold/schema"
	"fmt"
	"strings"
)

func MySQLGenerateDiffSQL(r *diff.Result, lowerCase bool) ([]string, error) {
	var sqls []string
	for _, t := range r.AddedTables {
		sqls = append(sqls, mysqlCreateTable(t, lowerCase))
	}
	for _, t := range r.DroppedTables {
		sqls = append(sqls, fmt.Sprintf("DROP TABLE IF EXISTS `%s`", normName(t.Name, lowerCase)))
	}
	for _, td := range r.ModifiedTables {
		for _, col := range td.AddedColumns {
			sqls = append(sqls, mysqlAddColumn(normName(td.TableName, lowerCase), col, lowerCase))
		}
		for _, col := range td.DroppedColumns {
			sqls = append(sqls, fmt.Sprintf("ALTER TABLE `%s` DROP COLUMN `%s`", normName(td.TableName, lowerCase), normName(col.Name, lowerCase)))
		}
		for _, cd := range td.ModifiedColumns {
			sqls = append(sqls, mysqlModifyColumn(normName(td.TableName, lowerCase), cd, lowerCase))
		}
		for _, idx := range td.AddedIndexes {
			sqls = append(sqls, mysqlCreateIndex(normName(td.TableName, lowerCase), idx, lowerCase))
		}
		for _, idx := range td.DroppedIndexes {
			sqls = append(sqls, fmt.Sprintf("DROP INDEX `%s` ON `%s`", normName(idx.Name, lowerCase), normName(td.TableName, lowerCase)))
		}
		for _, fk := range td.AddedForeignKeys {
			sqls = append(sqls, mysqlAddForeignKey(normName(td.TableName, lowerCase), fk, lowerCase))
		}
		for _, fk := range td.DroppedForeignKeys {
			sqls = append(sqls, fmt.Sprintf("ALTER TABLE `%s` DROP FOREIGN KEY `%s`", normName(td.TableName, lowerCase), normName(fk.Name, lowerCase)))
		}
	}
	return sqls, nil
}

func MySQLGenerateFullMigrationSQL(src, dst *schema.FullSchema, lowerCase bool) ([]string, error) {
	var sqls []string
	for _, t := range dst.Tables {
		sqls = append(sqls, mysqlCreateTable(t, lowerCase))
	}
	for _, v := range dst.Views {
		sqls = append(sqls, fmt.Sprintf("CREATE OR REPLACE VIEW `%s` AS %s", normName(v.Name, lowerCase), v.Def))
	}
	for _, tr := range dst.Triggers {
		sqls = append(sqls, tr.Body)
	}
	return sqls, nil
}

func MySQLGenerateSelectiveSQL(objects *schema.SelectedObjects, lowerCase bool) ([]string, error) {
	var sqls []string
	for _, t := range objects.Tables {
		sqls = append(sqls, mysqlCreateTable(t, lowerCase))
	}
	for _, v := range objects.Views {
		sqls = append(sqls, fmt.Sprintf("CREATE OR REPLACE VIEW `%s` AS %s", normName(v.Name, lowerCase), v.Def))
	}
	for _, tr := range objects.Triggers {
		sqls = append(sqls, tr.Body)
	}
	return sqls, nil
}

func mysqlCreateTable(t schema.Table, lowerCase bool) string {
	var lines []string
	var pkCols []string
	for _, col := range t.Columns {
		lines = append(lines, "  "+mysqlColumnDef(col, lowerCase))
		if col.PrimaryKey {
			pkCols = append(pkCols, fmt.Sprintf("`%s`", normName(col.Name, lowerCase)))
		}
	}
	if len(pkCols) > 0 {
		lines = append(lines, fmt.Sprintf("  PRIMARY KEY (%s)", strings.Join(pkCols, ", ")))
	}
	return fmt.Sprintf("CREATE TABLE `%s` (\n%s\n)", normName(t.Name, lowerCase), strings.Join(lines, ",\n"))
}

func mysqlColumnDef(col schema.Column, lowerCase bool) string {
	def := fmt.Sprintf("`%s` %s", normName(col.Name, lowerCase), col.Type)
	if !col.Nullable {
		def += " NOT NULL"
	}
	if col.AutoIncrement {
		def += " AUTO_INCREMENT"
	}
	if col.Default != nil {
		def += fmt.Sprintf(" DEFAULT %s", *col.Default)
	}
	return def
}

func mysqlAddColumn(table string, col schema.Column, lowerCase bool) string {
	def := fmt.Sprintf("ALTER TABLE `%s` ADD COLUMN `%s` %s", table, normName(col.Name, lowerCase), col.Type)
	if !col.Nullable {
		def += " NOT NULL"
	}
	if col.AutoIncrement {
		def += " AUTO_INCREMENT"
	}
	if col.Default != nil {
		def += fmt.Sprintf(" DEFAULT %s", *col.Default)
	}
	return def
}

func mysqlModifyColumn(table string, cd diff.ColumnDiff, lowerCase bool) string {
	def := fmt.Sprintf("ALTER TABLE `%s` MODIFY COLUMN `%s` %s", table, normName(cd.Column.Name, lowerCase), cd.Column.Type)
	if !cd.Column.Nullable {
		def += " NOT NULL"
	}
	if cd.DefaultChanged && cd.Column.Default != nil {
		def += fmt.Sprintf(" DEFAULT %s", *cd.Column.Default)
	}
	return def
}

func mysqlCreateIndex(table string, idx schema.Index, lowerCase bool) string {
	cols := make([]string, len(idx.Columns))
	for i, c := range idx.Columns {
		cols[i] = fmt.Sprintf("`%s`", normName(c, lowerCase))
	}
	unique := ""
	if idx.Unique {
		unique = "UNIQUE "
	}
	if table == "" {
		return fmt.Sprintf("CREATE %sINDEX `%s` (%s)", unique, normName(idx.Name, lowerCase), strings.Join(cols, ", "))
	}
	return fmt.Sprintf("CREATE %sINDEX `%s` ON `%s` (%s)", unique, normName(idx.Name, lowerCase), table, strings.Join(cols, ", "))
}

func mysqlAddForeignKey(table string, fk schema.ForeignKey, lowerCase bool) string {
	cols := make([]string, len(fk.Columns))
	for i, c := range fk.Columns {
		cols[i] = fmt.Sprintf("`%s`", normName(c, lowerCase))
	}
	refCols := make([]string, len(fk.RefColumns))
	for i, c := range fk.RefColumns {
		refCols[i] = fmt.Sprintf("`%s`", normName(c, lowerCase))
	}
	s := fmt.Sprintf("ALTER TABLE `%s` ADD CONSTRAINT `%s` FOREIGN KEY (%s) REFERENCES `%s`(%s)",
		table, normName(fk.Name, lowerCase), strings.Join(cols, ", "), normName(fk.RefTable, lowerCase), strings.Join(refCols, ", "))
	if fk.OnDelete != "" {
		s += " ON DELETE " + fk.OnDelete
	}
	if fk.OnUpdate != "" {
		s += " ON UPDATE " + fk.OnUpdate
	}
	return s
}
