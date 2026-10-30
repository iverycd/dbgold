package target

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"dbgold/datamigrate/partition"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

// This fixture owns a unique schema and is never enabled by a connection alone.
func TestPartitionKeyColumnDomainIntegration(t *testing.T) {
	dsn := os.Getenv("DBGOLD_PARTITION_PG_DSN")
	if os.Getenv("DBGOLD_PARTITION_ALLOW_DDL") != "1" || dsn == "" {
		t.Skip("requires explicit partition DDL opt-in and a disposable PostgreSQL DSN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	schema := fmt.Sprintf("dbgold_key_domain_%x", time.Now().UnixNano())
	w, err := NewPostgres(dsn, schema, ConnPoolConfig{MaxOpenConns: 1, MaxIdleConns: 1})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, w.Close()) })
	q := w.Dialect().QuoteIdent
	_, err = w.db.ExecContext(ctx, "CREATE SCHEMA "+q(schema))
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_, err := w.db.ExecContext(cleanup, "DROP SCHEMA "+q(schema)+" CASCADE")
		require.NoError(t, err)
	})
	qualified := func(table string) string { return q(schema) + "." + q(table) }
	statements := func(dataType string) []string {
		return []string{
			"CREATE TABLE " + qualified("Root") + ` ("MiXeD" ` + dataType + `) PARTITION BY LIST ((CAST("MiXeD" AS bigint)))`,
			"CREATE TABLE " + qualified("Mid") + " PARTITION OF " + qualified("Root") + ` DEFAULT PARTITION BY LIST ((CAST("MiXeD" AS bigint)))`,
			"CREATE TABLE " + qualified("Leaf") + " PARTITION OF " + qualified("Mid") + " DEFAULT",
		}
	}
	exec := func(query string) {
		t.Helper()
		_, err := w.db.ExecContext(ctx, query)
		require.NoError(t, err)
	}
	create := func(dataType string) {
		for _, statement := range statements(dataType) {
			exec(statement)
		}
	}
	create("integer")
	actual, err := w.InspectPartitionTable(ctx, "Root")
	require.NoError(t, err)
	plan := &partition.Plan{Source: &source.PartitionSpec{Method: "KEY"}, Target: actual,
		KeyColumns: []partition.KeyColumnDomain{{Column: "MiXeD", DataType: "integer", Nullable: true}}}
	obj := repair.Object{Kind: "table", Schema: schema, TargetName: "Root", PartitionPlan: plan,
		SourceColumns: []source.ColumnInfo{{Name: "MiXeD", DataType: "integer", IsNullable: true}}}
	verify := func() error { return VerifyPartitionKeyColumns(ctx, w, "Root", plan) }
	require.NoError(t, verify())
	for _, table := range []string{"Root", "Mid", "Leaf"} {
		t.Run(table+"_null_domain", func(t *testing.T) {
			exec("ALTER TABLE " + qualified(table) + ` ALTER COLUMN "MiXeD" SET NOT NULL`)
			require.ErrorContains(t, verify(), "可空性")
			v, err := InspectRepair(ctx, w, obj)
			require.NoError(t, err)
			require.Equal(t, "failed", v.Status)
			exec("ALTER TABLE " + qualified(table) + ` ALTER COLUMN "MiXeD" DROP NOT NULL`)
			require.NoError(t, verify())
		})
		t.Run(table+"_check_domain", func(t *testing.T) {
			exec("ALTER TABLE " + qualified(table) + ` ADD CONSTRAINT extra_check CHECK ("MiXeD">=0)`)
			// The native routing tree is unchanged, yet future negative inputs
			// are rejected. An empty-table content comparison cannot detect this.
			current, err := w.InspectPartitionTable(ctx, "Root")
			require.NoError(t, err)
			require.NoError(t, partition.Equivalent(plan.Target, current))
			require.ErrorContains(t, verify(), "CHECK")
			_, err = w.db.ExecContext(ctx, "INSERT INTO "+qualified("Root")+` ("MiXeD") VALUES (-1)`)
			require.Error(t, err)
			v, err := InspectRepair(ctx, w, obj)
			require.NoError(t, err)
			require.Equal(t, "failed", v.Status)
			exec("ALTER TABLE " + qualified(table) + " DROP CONSTRAINT extra_check")
			require.NoError(t, verify())
		})
	}
	exec("CREATE DOMAIN " + qualified("IntegerDomain") + " AS integer")
	for _, dataType := range []string{"numeric", qualified("IntegerDomain")} {
		t.Run("type_"+dataType, func(t *testing.T) {
			exec("DROP TABLE " + qualified("Root") + " CASCADE")
			create(dataType)
			require.ErrorContains(t, verify(), "原生 integer")
		})
	}
	for _, dataType := range []string{"numeric", "integer"} {
		t.Run("repair_single_connection_"+dataType, func(t *testing.T) {
			exec("DROP TABLE " + qualified("Root") + " CASCADE")
			var units []repair.Unit
			for i, statement := range statements(dataType) {
				units = append(units, repair.Unit{ID: fmt.Sprint(i), SQL: statement, OriginalSQL: statement, Selected: true, Status: "pending"})
			}
			out, err := ExecuteRepair(ctx, w, obj, units, nil)
			if dataType != "integer" {
				require.ErrorContains(t, err, "原生 integer")
				return
			}
			require.NoError(t, err)
			for _, unit := range out {
				require.Equal(t, "succeeded", unit.Status)
			}
			require.NoError(t, verify())
			v, err := InspectRepair(ctx, w, obj)
			require.NoError(t, err)
			require.Equal(t, "succeeded", v.Status)
		})
	}
}
