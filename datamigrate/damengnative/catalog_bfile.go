package damengnative

import (
	"fmt"
	"sort"
	"strings"
)

// Complex values carry the server's descriptor in the existing DM driver. The
// visitor identifies typed BFILE leaves, rather than mistaking ordinary UDT
// VARCHAR members containing ':' for external files.
type nativeBFileVisitor interface {
	VisitBFileLocators(func(string) error) error
}

func collectNativeBFileLocators(value any, scalar bool, add func(string) error) error {
	if value == nil {
		return nil
	}
	if !scalar {
		if visitor, ok := value.(nativeBFileVisitor); ok {
			return visitor.VisitBFileLocators(add)
		}
		return fmt.Errorf("原生复杂列返回了缺少可核验类型描述符的值 %T", value)
	}
	switch locator := value.(type) {
	case string:
		return add(locator)
	case []byte:
		return add(string(locator))
	default:
		return fmt.Errorf("原生 BFILE 定位器返回了不支持的值 %T", value)
	}
}

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
	add := func(locator string) error {
		if _, _, err := ParseBFileLocator(locator); err != nil {
			return err
		}
		if !seen[locator] {
			seen[locator] = true
			locators = append(locators, locator)
		}
		return nil
	}
	for _, column := range table.Columns {
		scalar := column.TypeSchema == "" && strings.EqualFold(strings.TrimSpace(column.DataType), "BFILE")
		if !scalar && column.TypeSchema == "" {
			continue
		}
		quoted := Quote(column.Name)
		// SQL DISTINCT may merge case/space variants under the source's
		// collation, while operating-system filenames still distinguish them.
		// Deduplicate the original bytes in Go instead.
		query := "SELECT " + quoted + " FROM " + Qualified(r.schema, sourceName)
		if scalar {
			query += " WHERE " + quoted + " IS NOT NULL"
		}
		rows, err := r.db.QueryContext(r.ctx, query)
		if err != nil {
			return fmt.Errorf("列 %s 的 BFILE 定位器不可读取: %w", column.Name, err)
		}
		for rows.Next() {
			var value any
			if err = rows.Scan(&value); err != nil {
				break
			}
			if err = collectNativeBFileLocators(value, scalar, add); err != nil {
				break
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
