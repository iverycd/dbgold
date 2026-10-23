package validation

import (
	"context"
	"fmt"
	"strings"

	"dbgold/internal/dmmeta"
)

// Describe actual columns, rather than CAST expressions or NUMBER_MODE: the
// current mode does not prove how an already existing table stores its values.
func (s *SQLScanner) describeDamengFloatStorage(ctx context.Context, schema, table string, columns []Column) error {
	var indexes []int
	var projection []string
	for i, c := range columns {
		if dmmeta.IsFloatType(c.DatabaseType) {
			indexes = append(indexes, i)
			projection = append(projection, QuoteValidationIdent("dameng", c.Name))
		}
	}
	if len(indexes) == 0 {
		return nil
	}
	query := "SELECT " + strings.Join(projection, ",") + " FROM " + QuoteValidationIdent("dameng", schema) + "." + QuoteValidationIdent("dameng", table) + " WHERE 1=0"
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("读取达梦表 %s.%s 浮点校验元数据: %w", schema, table, err)
	}
	actual, err := rows.ColumnTypes()
	closeErr := rows.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if len(actual) != len(indexes) {
		return fmt.Errorf("达梦表 %s.%s 浮点校验元数据列数不一致", schema, table)
	}
	for i, index := range indexes {
		c := &columns[index]
		if actual[i].Name() != c.Name {
			return fmt.Errorf("达梦浮点校验列名称 %q 与目录名称 %q 不一致", actual[i].Name(), c.Name)
		}
		kind, err := dmmeta.FloatStorage(actual[i], c.Precision)
		if err != nil {
			return fmt.Errorf("达梦列 %s.%s.%s: %w", schema, table, c.Name, err)
		}
		c.Kind, c.FloatBits = Float, 64
		switch kind {
		case "decimal":
			c.Kind, c.FloatBits = Number, 0
		case "real":
			c.FloatBits = 32
		}
	}
	return nil
}
