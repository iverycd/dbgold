package source

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

func sqlServerTableSet(tables []string) map[string]bool {
	if tables == nil {
		return nil
	}
	selected := make(map[string]bool, len(tables))
	for _, table := range tables {
		selected[table] = true
	}
	return selected
}

func sqlServerDecimalProfile(ctx context.Context, db *sql.DB, table string, columns []string, projection func(string) string, consume func(string, interface{}) error) error {
	if len(columns) == 0 {
		return nil
	}
	if consume == nil {
		return fmt.Errorf("缺少 SQL Server 数值画像的数据接收回调")
	}
	projections := make([]string, len(columns))
	values := make([]sql.NullString, len(columns))
	pointers := make([]any, len(columns))
	for i, column := range columns {
		projections[i], pointers[i] = projection(column), &values[i]
	}
	rows, err := db.QueryContext(ctx, "SELECT "+strings.Join(projections, ", ")+" FROM "+table)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := rows.Scan(pointers...); err != nil {
			return err
		}
		for i, value := range values {
			if value.Valid {
				if err := consume(columns[i], value.String); err != nil {
					return err
				}
			}
		}
	}
	return rows.Err()
}
