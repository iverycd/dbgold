package target

import (
	"context"
	"database/sql"
	"fmt"
)

// MigrationDependency describes a physical target reference. Identifiers retain
// the catalog's exact spelling; this capability performs no DDL.
type MigrationDependency struct {
	Schema    string
	Table     string
	Name      string
	RefSchema string
	RefTable  string
}
type MigrationDependencies struct {
	FoldNames bool
	Schema    string
	Relations []MigrationDependency
}

const pgMigrationDependenciesSQL = `SELECT cn.nspname,c.relname,f.conname,pn.nspname,p.relname
 FROM pg_constraint f
 JOIN pg_class c ON c.oid=f.conrelid JOIN pg_namespace cn ON cn.oid=c.relnamespace
 JOIN pg_class p ON p.oid=f.confrelid JOIN pg_namespace pn ON pn.oid=p.relnamespace
 WHERE f.contype='f' AND pn.nspname=$1
 ORDER BY cn.nspname,c.relname,f.conname`
const dmMigrationDependenciesSQL = `SELECT c.OWNER,c.TABLE_NAME,c.CONSTRAINT_NAME,p.OWNER,p.TABLE_NAME
 FROM ALL_CONSTRAINTS c JOIN ALL_CONSTRAINTS p ON p.OWNER=c.R_OWNER AND p.CONSTRAINT_NAME=c.R_CONSTRAINT_NAME
 WHERE c.CONSTRAINT_TYPE='R' AND p.OWNER=? ORDER BY c.OWNER,c.TABLE_NAME,c.CONSTRAINT_NAME`

func readMigrationDependencies(ctx context.Context, db *sql.DB, schema, query, currentSchema string) (MigrationDependencies, error) {
	if schema == "" {
		if err := db.QueryRowContext(ctx, currentSchema).Scan(&schema); err != nil {
			return MigrationDependencies{}, err
		}
	}
	out := MigrationDependencies{Schema: schema, Relations: []MigrationDependency{}}
	if schema == "" {
		return out, fmt.Errorf("无法确定目标依赖检查的实际 schema")
	}
	rows, err := db.QueryContext(ctx, query, schema)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var d MigrationDependency
		if err := rows.Scan(&d.Schema, &d.Table, &d.Name, &d.RefSchema, &d.RefTable); err != nil {
			return out, err
		}
		if d.Schema == "" || d.Table == "" || d.Name == "" || d.RefSchema == "" || d.RefTable == "" {
			return out, fmt.Errorf("目标依赖元数据不完整，无法安全跳过失败表")
		}
		out.Relations = append(out.Relations, d)
	}
	return out, rows.Err()
}
func (w *PostgresWriter) InspectMigrationDependencies(ctx context.Context) (MigrationDependencies, error) {
	return readMigrationDependencies(ctx, w.db, w.schema, pgMigrationDependenciesSQL, "SELECT current_schema()")
}
func (w *GaussDBWriter) InspectMigrationDependencies(ctx context.Context) (MigrationDependencies, error) {
	return readMigrationDependencies(ctx, w.db, w.schema, pgMigrationDependenciesSQL, "SELECT current_schema()")
}
func (w *HighGoWriter) InspectMigrationDependencies(ctx context.Context) (MigrationDependencies, error) {
	return readMigrationDependencies(ctx, w.db, w.schema, pgMigrationDependenciesSQL, "SELECT current_schema()")
}
func (w *KingbaseWriter) InspectMigrationDependencies(ctx context.Context) (MigrationDependencies, error) {
	return readMigrationDependencies(ctx, w.db, w.schema, pgMigrationDependenciesSQL, "SELECT current_schema()")
}
func (w *DaMengWriter) InspectMigrationDependencies(ctx context.Context) (MigrationDependencies, error) {
	return readMigrationDependencies(ctx, w.db, w.schema, dmMigrationDependenciesSQL, "SELECT CURRENT_SCHEMA")
}
func (w *OracleWriter) InspectMigrationDependencies(ctx context.Context) (MigrationDependencies, error) {
	const query = `SELECT c.OWNER,c.TABLE_NAME,c.CONSTRAINT_NAME,p.OWNER,p.TABLE_NAME FROM ALL_CONSTRAINTS c JOIN ALL_CONSTRAINTS p ON p.OWNER=c.R_OWNER AND p.CONSTRAINT_NAME=c.R_CONSTRAINT_NAME WHERE c.CONSTRAINT_TYPE='R' AND p.OWNER=:1 ORDER BY c.OWNER,c.TABLE_NAME,c.CONSTRAINT_NAME`
	return readMigrationDependencies(ctx, w.db, w.schema, query, "SELECT SYS_CONTEXT('USERENV','CURRENT_SCHEMA') FROM DUAL")
}
func (w *SQLServerWriter) InspectMigrationDependencies(ctx context.Context) (MigrationDependencies, error) {
	const query = `SELECT cs.name,c.name,f.name,ps.name,p.name FROM sys.foreign_keys f JOIN sys.tables c ON c.object_id=f.parent_object_id JOIN sys.schemas cs ON cs.schema_id=c.schema_id JOIN sys.tables p ON p.object_id=f.referenced_object_id JOIN sys.schemas ps ON ps.schema_id=p.schema_id WHERE ps.name=@p1 ORDER BY cs.name,c.name,f.name`
	return readMigrationDependencies(ctx, w.db, w.schema, query, "SELECT SCHEMA_NAME()")
}
func (w *MySQLWriter) InspectMigrationDependencies(ctx context.Context) (MigrationDependencies, error) {
	schema, err := w.OracleMySQLSchema(ctx)
	if err != nil {
		return MigrationDependencies{}, err
	}
	keys, err := w.OracleMySQLForeignKeys(ctx)
	if err != nil {
		return MigrationDependencies{}, err
	}
	mode, err := w.OracleMySQLLowerCaseTableNames(ctx)
	if err != nil {
		return MigrationDependencies{}, err
	}
	out := MigrationDependencies{Schema: schema, FoldNames: mode != 0, Relations: []MigrationDependency{}}
	for _, fk := range keys {
		out.Relations = append(out.Relations, MigrationDependency{Schema: fk.Schema, Table: fk.Table, Name: fk.ConstraintName, RefSchema: fk.RefSchema, RefTable: fk.RefTable})
	}
	return out, nil
}

// The bundled Oscar JDBC driver's DatabaseMetaData.getExportedKeys uses
// V_SYS_FOREIGN_KEYS with these native column names (oscarJDBC8, 2025-01-17).
// Use the existing bounded catalog cursor, rather than assuming Oracle views.
func (w *OscarWriter) InspectMigrationDependencies(ctx context.Context) (MigrationDependencies, error) {
	schema, err := w.partitionSchema(ctx)
	if err != nil {
		return MigrationDependencies{}, err
	}
	literal, err := oscarPartitionLiteral(schema)
	if err != nil {
		return MigrationDependencies{}, err
	}
	query := `SELECT DISTINCT FKTABLE_SCHEM,FKTABLE_NAME,FK_NAME,PKTABLE_SCHEM,PKTABLE_NAME FROM V_SYS_FOREIGN_KEYS WHERE PKTABLE_SCHEM=` + literal + ` ORDER BY FKTABLE_SCHEM,FKTABLE_NAME,FK_NAME`
	rows, err := w.partitionQuery(ctx, query)
	if err != nil {
		return MigrationDependencies{}, err
	}
	out := MigrationDependencies{Schema: schema, Relations: []MigrationDependency{}}
	for _, row := range rows {
		if len(row) != 5 {
			return out, fmt.Errorf("神通目标外键目录返回列数不正确")
		}
		var d MigrationDependency
		for i, dst := range []*string{&d.Schema, &d.Table, &d.Name, &d.RefSchema, &d.RefTable} {
			value, err := oscarPartitionString(row[i])
			if err != nil || value == "" {
				return out, fmt.Errorf("神通目标外键目录返回不完整的标识")
			}
			*dst = value
		}
		out.Relations = append(out.Relations, d)
	}
	return out, nil
}
