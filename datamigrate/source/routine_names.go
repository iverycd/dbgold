package source

import (
	"context"
	"database/sql"
)

// ListProcedureNames reads names from the migration scope without requiring
// access to procedure bodies. The catalog spelling is preserved for callers.
func (r *MySQLReader) ListProcedureNames(ctx context.Context) ([]string, error) {
	return scanRoutineNames(r.queryContext(ctx, `SELECT ROUTINE_NAME FROM INFORMATION_SCHEMA.ROUTINES
		WHERE ROUTINE_SCHEMA = ? AND ROUTINE_TYPE = 'PROCEDURE' ORDER BY ROUTINE_NAME`, r.dbName))
}

func (r *SQLServerReader) ListProcedureNames(ctx context.Context) ([]string, error) {
	return scanRoutineNames(r.db.QueryContext(ctx, `SELECT name FROM sys.procedures
		WHERE is_ms_shipped = 0 ORDER BY name`))
}

func (r *OracleReader) ListProcedureNames(ctx context.Context) ([]string, error) {
	return scanRoutineNames(r.db.QueryContext(ctx, `SELECT OBJECT_NAME FROM ALL_OBJECTS
		WHERE OWNER = :1 AND OBJECT_TYPE = 'PROCEDURE' ORDER BY OBJECT_NAME`, r.owner))
}

func (r *DaMengReader) ListProcedureNames(ctx context.Context) ([]string, error) {
	return scanRoutineNames(r.db.QueryContext(ctx, `SELECT OBJECT_NAME FROM ALL_OBJECTS
		WHERE OWNER = ? AND OBJECT_TYPE = 'PROCEDURE' ORDER BY OBJECT_NAME`, r.schema))
}

// ListFunctionNames reads user-defined function names from readers that support
// routine inventory. Unsupported readers return an empty inventory.
func ListFunctionNames(ctx context.Context, r Reader) ([]string, error) {
	reader, ok := r.(interface {
		ListFunctionNames(context.Context) ([]string, error)
	})
	if !ok {
		return nil, nil
	}
	names, err := reader.ListFunctionNames(ctx)
	if err != nil {
		return nil, err
	}
	return names, nil
}

func (r *MySQLReader) ListFunctionNames(ctx context.Context) ([]string, error) {
	return scanRoutineNames(r.queryContext(ctx, `SELECT ROUTINE_NAME FROM INFORMATION_SCHEMA.ROUTINES
		WHERE ROUTINE_SCHEMA = ? AND ROUTINE_TYPE = 'FUNCTION' ORDER BY ROUTINE_NAME`, r.dbName))
}

func (r *SQLServerReader) ListFunctionNames(ctx context.Context) ([]string, error) {
	return scanRoutineNames(r.db.QueryContext(ctx, `SELECT name FROM sys.objects
		WHERE type IN ('FN','IF','TF','FS','FT') AND is_ms_shipped = 0 ORDER BY name`))
}

func (r *OracleReader) ListFunctionNames(ctx context.Context) ([]string, error) {
	return scanRoutineNames(r.db.QueryContext(ctx, `SELECT OBJECT_NAME FROM ALL_OBJECTS
		WHERE OWNER = :1 AND OBJECT_TYPE = 'FUNCTION' ORDER BY OBJECT_NAME`, r.owner))
}

func (r *DaMengReader) ListFunctionNames(ctx context.Context) ([]string, error) {
	return scanRoutineNames(r.db.QueryContext(ctx, `SELECT OBJECT_NAME FROM ALL_OBJECTS
		WHERE OWNER = ? AND OBJECT_TYPE = 'FUNCTION' ORDER BY OBJECT_NAME`, r.schema))
}

func scanRoutineNames(rows *sql.Rows, err error) ([]string, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return names, nil
}
