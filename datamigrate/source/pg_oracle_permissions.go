package source

import (
	"context"
	"database/sql"
	"fmt"

	"dbgold/datamigrate/failure"
)

func (r *PostgresReader) pgMetadataFunction(name string) bool {
	return r.profile.Functions == nil || r.profile.Functions[name]
}

func pgObjectMetadataFailure(snap *PGSnapshot, kind, name string, err error) {
	snap.Objects = append(snap.Objects, PGObject{Kind: kind, Name: name, Reason: err.Error()})
}

func (r *PostgresReader) pgOptionalObjectQuery(ctx context.Context, snap *PGSnapshot, neutral bool, kind, name, function, query string, args ...any) (*sql.Rows, error) {
	var err error
	if neutral && function != "" && !r.pgMetadataFunction(function) {
		err = fmt.Errorf("缺少元数据函数 %s 或 EXECUTE 权限", function)
	}
	var rows *sql.Rows
	if err == nil {
		rows, err = r.db.QueryContext(ctx, query, args...)
	}
	if err != nil && neutral && !failure.IsTaskError(err) {
		pgObjectMetadataFailure(snap, kind, name, err)
		return nil, nil
	}
	return rows, err
}

func pgFinishOptionalRows(rows *sql.Rows) error {
	if rows == nil {
		return nil
	}
	err := rows.Err()
	closed := rows.Close()
	if err == nil {
		err = closed
	}
	return err
}

func pgTableMetadataBlocked(objects []PGObject) bool {
	for _, o := range objects {
		if o.Kind == "table" || o.Kind == "metadata" {
			return true
		}
	}
	return false
}
