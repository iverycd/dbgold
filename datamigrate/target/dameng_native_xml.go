package target

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"dbgold/datamigrate/damengnative"
)

// GET_DDL may flatten XMLTYPE to TEXT. A nested native recipe must prove its
// actual target column flags before comparing that declaration, just as the
// full catalog reader does. Ordinary TEXT/CLOB are never promoted by spelling.
func nativeRestoreXMLDefinition(ctx context.Context, conn *sql.Conn, schema, table, ddl string) (string, error) {
	tokens, err := nativeSQLTokens(ddl)
	if err != nil {
		return "", err
	}
	possible := false
	for _, token := range tokens {
		possible = possible || strings.EqualFold(token, "TEXT") || strings.EqualFold(token, "CLOB")
	}
	if !possible {
		return ddl, nil
	}
	seen := map[string]bool{}
	for attempt := 0; attempt < 64; attempt++ {
		if seen[table] {
			return "", fmt.Errorf("XMLTYPE物理分区的目录父链存在循环")
		}
		seen[table] = true
		rows, err := conn.QueryContext(ctx, `SELECT COLUMN_NAME,DATA_TYPE,NVL(DATA_SCALE,0) FROM ALL_TAB_COLUMNS WHERE OWNER=? AND TABLE_NAME=? AND DATA_TYPE IN ('TEXT','CLOB') ORDER BY COLUMN_ID`, schema, table)
		if err != nil {
			return "", err
		}
		var columns []damengnative.Column
		for rows.Next() {
			var column damengnative.Column
			if err = rows.Scan(&column.Name, &column.DataType, &column.Scale); err != nil {
				break
			}
			columns = append(columns, column)
		}
		iteration := rows.Err()
		_ = rows.Close()
		if err != nil {
			return "", err
		}
		if iteration != nil {
			return "", iteration
		}
		if len(columns) > 0 {
			return damengnative.RestoreNativeXMLTableDDL(ddl, columns)
		}
		// DM omits all physical-partition columns from ALL_TAB_COLUMNS and
		// SYSCOLUMNS. Their attributes are inherited from the catalog-proved
		// logical parent, never inferred from an engine-assigned name prefix.
		var parent string
		err = conn.QueryRowContext(ctx, `SELECT b.NAME FROM SYS.SYSOBJECTS p JOIN SYS.SYSHPARTTABLEINFO h ON h.PART_TABLE_ID=p.ID JOIN SYS.SYSOBJECTS b ON b.ID=h.BASE_TABLE_ID AND b.SCHID=p.SCHID JOIN SYS.SYSOBJECTS s ON s.ID=p.SCHID WHERE s.TYPE$='SCH' AND s.NAME=? AND p.NAME=? AND p.SUBTYPE$='UTAB' AND b.SUBTYPE$='UTAB' AND b.ID<>p.ID`, schema, table).Scan(&parent)
		if errors.Is(err, sql.ErrNoRows) {
			return ddl, nil
		}
		if err != nil {
			return "", err
		}
		table = parent
	}
	return "", fmt.Errorf("XMLTYPE物理分区的目录父链超出原生层级范围")
}
