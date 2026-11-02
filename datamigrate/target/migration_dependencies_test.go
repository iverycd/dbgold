package target

import (
	"context"
	"errors"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestMigrationDependencyCatalogPreservesCaseAndExternalChildren(t *testing.T) {
	for _, kind := range []string{"postgres", "gaussdb", "highgo", "kingbase", "dameng"} {
		t.Run(kind, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			pattern := `SELECT cn.nspname,c.relname,f.conname`
			if kind == "dameng" {
				pattern = `SELECT c.OWNER,c.TABLE_NAME,c.CONSTRAINT_NAME`
			}
			mock.ExpectQuery(pattern).WithArgs("MiXeD").WillReturnRows(sqlmock.NewRows([]string{"schema", "table", "name", "refschema", "reftable"}).AddRow("Other", "RetainedChild", "FkMixed", "MiXeD", "Parent"))
			var reader interface {
				InspectMigrationDependencies(context.Context) (MigrationDependencies, error)
			}
			switch kind {
			case "postgres":
				reader = &PostgresWriter{db: db, schema: "MiXeD"}
			case "gaussdb":
				reader = &GaussDBWriter{db: db, schema: "MiXeD"}
			case "highgo":
				reader = &HighGoWriter{db: db, schema: "MiXeD"}
			case "kingbase":
				reader = &KingbaseWriter{db: db, schema: "MiXeD"}
			case "dameng":
				reader = &DaMengWriter{db: db, schema: "MiXeD"}
			}
			snapshot, err := reader.InspectMigrationDependencies(context.Background())
			require.NoError(t, err)
			require.Equal(t, "MiXeD", snapshot.Schema)
			require.Equal(t, []MigrationDependency{{Schema: "Other", Table: "RetainedChild", Name: "FkMixed", RefSchema: "MiXeD", RefTable: "Parent"}}, snapshot.Relations)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
func TestMigrationDependencyCatalogFailsOnIncompleteRead(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery(`SELECT cn.nspname,c.relname,f.conname`).WithArgs("App").WillReturnRows(sqlmock.NewRows([]string{"schema", "table", "name", "refschema", "reftable"}).AddRow("App", "C", "FK", "App", "P").RowError(0, errors.New("catalog stream interrupted")))
	_, err = (&PostgresWriter{db: db, schema: "App"}).InspectMigrationDependencies(context.Background())
	require.ErrorContains(t, err, "catalog stream interrupted")
	require.NoError(t, mock.ExpectationsWereMet())
}
func TestOscarMigrationDependenciesUseNativeJDBCCatalog(t *testing.T) {
	w, f := newOscarPartitionFixture()
	query := `SELECT DISTINCT FKTABLE_SCHEM,FKTABLE_NAME,FK_NAME,PKTABLE_SCHEM,PKTABLE_NAME FROM V_SYS_FOREIGN_KEYS WHERE PKTABLE_SCHEM='业务' ORDER BY FKTABLE_SCHEM,FKTABLE_NAME,FK_NAME`
	f.results[query] = [][]any{{"Other", "RetainedChild", "FkMixed", "业务", "Parent"}}
	snapshot, err := w.InspectMigrationDependencies(context.Background())
	require.NoError(t, err)
	require.Equal(t, "业务", snapshot.Schema)
	require.Len(t, snapshot.Relations, 1)
	require.Equal(t, "RetainedChild", snapshot.Relations[0].Table)
	require.Equal(t, 1, f.closes)
	f.results[query] = [][]any{{"Other", "Child", nil, "业务", "Parent"}}
	_, err = w.InspectMigrationDependencies(context.Background())
	require.Error(t, err)
}
