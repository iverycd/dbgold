package damengnative

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// Freeze every locator as an explicit external dependency. The directories and
// files are instance resources prepared by the operator, so their identifiers
// never participate in the schema mapping or migration DDL.
func (r *catalogReader) freezeNativeBFiles(object *Object, table Table, sourceName string) error {
	seen := map[string]bool{}
	for _, ref := range object.External {
		if ref.Kind == "bfile" && ref.Schema == "" {
			seen[ref.Name] = true
		}
	}
	var locators []string
	for _, column := range table.Columns {
		if !column.Writable || column.TypeSchema != "" || !strings.EqualFold(strings.TrimSpace(column.DataType), "BFILE") {
			continue
		}
		quoted := Quote(column.Name)
		// SQL DISTINCT may merge case/space variants under the source's
		// collation, while operating-system filenames still distinguish them.
		// Deduplicate the original bytes in Go instead.
		rows, err := r.db.QueryContext(r.ctx, "SELECT "+quoted+" FROM "+Qualified(r.schema, sourceName)+" WHERE "+quoted+" IS NOT NULL")
		if err != nil {
			return fmt.Errorf("列 %s 的 BFILE 定位器不可读取: %w", column.Name, err)
		}
		for rows.Next() {
			var locator sql.NullString
			if err = rows.Scan(&locator); err != nil {
				break
			}
			if !locator.Valid {
				continue
			}
			if _, _, err = ParseBFileLocator(locator.String); err != nil {
				break
			}
			if !seen[locator.String] {
				seen[locator.String] = true
				locators = append(locators, locator.String)
			}
		}
		iteration := rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
		if iteration != nil {
			return iteration
		}
	}
	// Close every table cursor before checking resources so a one-connection
	// source pool remains usable. No file/package calls execute for NULL rows.
	sort.Strings(locators)
	for _, locator := range locators {
		if err := CheckBFileResource(r.ctx, r.db, locator); err != nil {
			return err
		}
		object.External = append(object.External, Reference{Kind: "bfile", Name: locator})
	}
	return nil
}
