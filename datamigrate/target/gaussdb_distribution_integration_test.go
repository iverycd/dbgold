package target

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

// Opt-in only: use a disposable GaussDB 507 distributed test database. The test
// creates a unique schema and cleans up only that schema. A centralized instance
// is not evidence that uniqueness is enforced across remote nodes.
func TestGaussDB507DistributedUniqueIntegration(t *testing.T) {
	dsn := os.Getenv("DBGOLD_TEST_GAUSSDB507_DISTRIBUTED_DSN")
	if dsn == "" {
		t.Skip("DBGOLD_TEST_GAUSSDB507_DISTRIBUTED_DSN is not set")
	}
	db, err := sql.Open("opengauss", dsn)
	require.NoError(t, err)
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var version string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT version()").Scan(&version))
	require.Regexp(t, gauss507, version)
	var nodes int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM pg_catalog.pgxc_node WHERE node_type='D'`).Scan(&nodes))
	require.GreaterOrEqual(t, nodes, 2, "require at least two data nodes")
	d := dialect.NewPostgres("gaussdb")
	schema := fmt.Sprintf("dbgold_gsi_%d", time.Now().UnixNano())
	_, err = db.ExecContext(ctx, "CREATE SCHEMA "+d.QuoteIdent(schema))
	require.NoError(t, err)
	defer func() {
		cleanup, c := context.WithTimeout(context.Background(), 30*time.Second)
		defer c()
		_, e := db.ExecContext(cleanup, "DROP SCHEMA "+d.QuoteIdent(schema)+" CASCADE")
		require.NoError(t, e)
	}()
	w := &GaussDBWriter{db: db, schema: schema, dia: d, srcType: "mysql"}
	for _, baseKey := range []string{"patient_id", "OtherKey"} {
		t.Run(baseKey, func(t *testing.T) {
			info := &source.TableDDLInfo{TableName: "Patients_" + baseKey, Columns: []source.ColumnInfo{{Name: "patient_id", DataType: "int"}, {Name: "auto_id", DataType: "bigint"}, {Name: "OtherKey", DataType: "int"}}}
			ss, e := d.CreateTableStatements(schema, info, "mysql", dialect.TypeOpt{GaussDBDistribution: []string{baseKey}}, func(s string) string { return s })
			require.NoError(t, e)
			require.NoError(t, w.CreateTable(ctx, ss))
			require.NoError(t, w.CopyData(ctx, info.TableName, []string{"patient_id", "auto_id", "OtherKey"}, []string{"int", "bigint", "int"}, [][]interface{}{{1, int64(100), 10}, {2, int64(200), 20}}))
			for _, idx := range []source.IndexInfo{{TableName: info.TableName, IndexName: "PRIMARY", IsPrimary: true, IsUnique: true, Columns: []string{"patient_id"}}, {TableName: info.TableName, IndexName: "auto_" + baseKey, IsUnique: true, Columns: []string{"auto_id"}, Method: "BTREE"}} {
				plan, e := w.PlanGaussDBIndex(ctx, idx)
				require.NoError(t, e)
				require.NoError(t, w.CreateIndex(ctx, plan))
				require.NoError(t, w.CreateIndex(ctx, plan), "matching duplicate is verified and reused")
			}
			q := d.QualifyTable(schema, info.TableName)
			_, e = db.ExecContext(ctx, "INSERT INTO "+q+" VALUES (1,300,30)")
			require.Error(t, e)
			require.Equal(t, "23505", compatibilitySQLState(e), "patient_id must remain unique")
			_, e = db.ExecContext(ctx, "INSERT INTO "+q+" VALUES (3,100,30)")
			require.Error(t, e)
			require.Equal(t, "23505", compatibilitySQLState(e), "auto_id must remain globally unique")
			count, e := w.CountRows(ctx, info.TableName)
			require.NoError(t, e)
			require.EqualValues(t, 2, count)
		})
	}
}
