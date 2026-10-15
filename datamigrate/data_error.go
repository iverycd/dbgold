package datamigrate

import (
	"errors"
	"strings"
)

// formatDataWriteError preserves the server diagnostics that pq's Error()
// omits. lib/pq, openGauss and highgopq expose the same protocol-field accessor.
func formatDataWriteError(err error) string {
	var diagnostic interface{ Get(byte) string }
	if !errors.As(err, &diagnostic) {
		return err.Error()
	}
	lines := []string{err.Error()}
	for _, field := range []struct {
		key   byte
		label string
	}{
		{'C', "SQLSTATE"},
		{'c', "目标列"},
		{'D', "详细原因"},
		{'W', "数据库上下文"},
		{'H', "提示"},
	} {
		value := diagnostic.Get(field.key)
		if value == "" {
			continue
		}
		label := field.label
		if field.key == 'W' && strings.Contains(value, "COPY ") {
			// COPY is buffered: the failing Exec call need not be the failing
			// row. Only the server's COPY context identifies the row/column.
			label = "COPY 上下文（line 为本批次内行号，column 为目标列）"
		}
		lines = append(lines, label+"："+value)
	}
	return strings.Join(lines, "\n")
}
