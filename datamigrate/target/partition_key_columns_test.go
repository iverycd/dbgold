package target

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"regexp"
	"testing"

	"dbgold/datamigrate/partition"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func keyColumnPlan() *partition.Plan {
	return &partition.Plan{
		Source: &source.PartitionSpec{Method: "KEY"},
		Target: &source.PartitionSpec{Method: "LIST", Keys: []source.PartitionKey{{Expression: `CAST("MiXeD" AS bigint)`}}, Partitions: []source.Partition{
			{Schema: "App", Name: "Mid", Subpartition: &source.PartitionSpec{Method: "LIST", Partitions: []source.Partition{{Schema: "App", Name: "Leaf"}}}},
		}},
		KeyColumns: []partition.KeyColumnDomain{{Column: "MiXeD", DataType: "integer", Nullable: true}},
	}
}

var keyColumnCatalogNames = []string{"oid", "parent", "schema", "table", "kind", "ispartition", "column", "typeoid", "notnull", "identity", "generated", "native_int", "has_checks"}

func keyColumnCatalogValues() [][]driver.Value {
	return [][]driver.Value{
		{int64(10), int64(0), "App", "Root", "p", false, "MiXeD", int64(23), false, "", "", true, false},
		{int64(11), int64(10), "App", "Mid", "p", true, "MiXeD", int64(23), false, "", "", true, false},
		{int64(12), int64(11), "App", "Leaf", "r", true, "MiXeD", int64(23), false, "", "", true, false},
	}
}

func keyColumnCatalogRows(values [][]driver.Value) *sqlmock.Rows {
	rows := sqlmock.NewRows(keyColumnCatalogNames)
	for _, row := range values {
		rows.AddRow(row...)
	}
	return rows
}

func TestPartitionKeyColumnVerificationChecksEveryPhysicalLevel(t *testing.T) {
	for _, mutation := range []string{"none", "root_not_null", "branch_not_null", "leaf_not_null", "root_check", "branch_check", "leaf_check", "domain", "numeric", "identity", "generated", "missing_column", "missing_leaf", "wrong_parent", "wrong_schema", "ordinary_root", "foreign_leaf", "duplicate"} {
		t.Run(mutation, func(t *testing.T) {
			db, mock := partitionInspectorDB(t)
			values := keyColumnCatalogValues()
			switch mutation {
			case "root_not_null":
				values[0][8] = true
			case "branch_not_null":
				values[1][8] = true
			case "leaf_not_null":
				values[2][8] = true
			case "root_check":
				values[0][12] = true
			case "branch_check":
				values[1][12] = true
			case "leaf_check":
				values[2][12] = true
			case "domain":
				values[0][7], values[0][11] = int64(16000), false
			case "numeric":
				values[0][7], values[0][11] = int64(1700), false
			case "identity":
				values[0][9] = "a"
			case "generated":
				values[2][10] = "s"
			case "missing_column":
				values[2][6], values[2][7], values[2][8], values[2][11] = nil, nil, nil, nil
			case "missing_leaf":
				values = values[:2]
			case "wrong_parent":
				values[2][1] = int64(10)
			case "wrong_schema":
				values[2][2] = "Other"
			case "ordinary_root":
				values[0][4] = "r"
			case "foreign_leaf":
				values[2][4] = "f"
			case "duplicate":
				values = append(values, values[2])
			}
			mock.ExpectQuery(regexp.QuoteMeta(pgPartitionKeyColumnsSQL)).WithArgs("App", "Root", "MiXeD").WillReturnRows(keyColumnCatalogRows(values)).RowsWillBeClosed()
			w := &PostgresWriter{db: db, schema: "App", dbType: "postgres"}
			err := VerifyPartitionKeyColumns(context.Background(), w, "Root", keyColumnPlan())
			if mutation == "none" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestPartitionKeyColumnVerificationFailsClosedOnCatalogErrors(t *testing.T) {
	for _, failure := range []string{"query", "rows", "close", "empty"} {
		t.Run(failure, func(t *testing.T) {
			db, mock := partitionInspectorDB(t)
			query := mock.ExpectQuery(regexp.QuoteMeta(pgPartitionKeyColumnsSQL)).WithArgs("App", "Root", "MiXeD")
			sentinel := errors.New("catalog unavailable")
			switch failure {
			case "query":
				query.WillReturnError(sentinel)
			case "rows":
				query.WillReturnRows(keyColumnCatalogRows(keyColumnCatalogValues()).RowError(1, sentinel)).RowsWillBeClosed()
			case "close":
				query.WillReturnRows(keyColumnCatalogRows(keyColumnCatalogValues()).CloseError(sentinel)).RowsWillBeClosed()
			case "empty":
				query.WillReturnRows(keyColumnCatalogRows(nil)).RowsWillBeClosed()
			}
			err := VerifyPartitionKeyColumns(context.Background(), &PostgresWriter{db: db, schema: "App", dbType: "postgres"}, "Root", keyColumnPlan())
			require.Error(t, err)
			if failure != "empty" {
				require.ErrorIs(t, err, sentinel)
			}
		})
	}
}

func TestPartitionKeyColumnVerificationChecksAllReferencedColumns(t *testing.T) {
	db, mock := partitionInspectorDB(t)
	plan := keyColumnPlan()
	plan.KeyColumns = append(plan.KeyColumns, partition.KeyColumnDomain{Column: "Second", DataType: "integer", Nullable: false})
	mock.ExpectQuery(regexp.QuoteMeta(pgPartitionKeyColumnsSQL)).WithArgs("App", "Root", "MiXeD").WillReturnRows(keyColumnCatalogRows(keyColumnCatalogValues())).RowsWillBeClosed()
	values := keyColumnCatalogValues()
	for _, value := range values {
		value[6], value[8] = "Second", true
	}
	values[2][8] = false
	mock.ExpectQuery(regexp.QuoteMeta(pgPartitionKeyColumnsSQL)).WithArgs("App", "Root", "Second").WillReturnRows(keyColumnCatalogRows(values)).RowsWillBeClosed()
	require.Error(t, VerifyPartitionKeyColumns(context.Background(), &PostgresWriter{db: db, schema: "App", dbType: "postgres"}, "Root", plan))
}

type keyColumnVerifierStub struct {
	Writer
	calls int
	err   error
}

func (w *keyColumnVerifierStub) VerifyPartitionKeyColumns(context.Context, string, *partition.Plan) error {
	w.calls++
	return w.err
}

func TestPartitionKeyColumnVerificationCapabilityAndLegacyPlans(t *testing.T) {
	plan := keyColumnPlan()
	stub := &keyColumnVerifierStub{err: errors.New("guard rejected")}
	require.ErrorIs(t, VerifyPartitionKeyColumns(context.Background(), stub, "Root", plan), stub.err)
	require.Equal(t, 1, stub.calls)
	require.ErrorContains(t, VerifyPartitionKeyColumns(context.Background(), &DaMengWriter{}, "Root", plan), "核验能力")
	plan.KeyColumns = nil
	require.ErrorContains(t, VerifyPartitionKeyColumns(context.Background(), nil, "Root", plan), "列域快照")
	plan.Source = &source.PartitionSpec{Method: "RANGE", Partitions: []source.Partition{{Subpartition: &source.PartitionSpec{Method: "LINEAR KEY"}}}}
	plan.Target = &source.PartitionSpec{Method: "RANGE", Partitions: []source.Partition{{Subpartition: &source.PartitionSpec{Method: "LIST", Keys: []source.PartitionKey{{Expression: "native_hash"}}}}}}
	require.ErrorContains(t, VerifyPartitionKeyColumns(context.Background(), nil, "Root", plan), "列域快照")
	plan.Target.Partitions[0].Subpartition.Method = "LINEAR KEY"
	require.NoError(t, VerifyPartitionKeyColumns(context.Background(), nil, "Root", plan), "same-product native KEY keeps the legacy plan format")
	require.NoError(t, VerifyPartitionKeyColumns(context.Background(), nil, "Root", nil))
}

func TestPartitionKeyColumnRepairRejectsLegacyPlanBeforeExecution(t *testing.T) {
	for _, ruleOnly := range []bool{false, true} {
		t.Run(fmt.Sprint(ruleOnly), func(t *testing.T) {
			db, _ := partitionInspectorDB(t) // No catalog read, connection session or DDL is permitted.
			plan := keyColumnPlan()
			plan.KeyColumns = nil
			if ruleOnly {
				plan.Source, plan.Target = nil, nil
				plan.Rule = "mysql-innodb-key-bytes-to-native-list"
			}
			obj := repair.Object{Kind: "table", Schema: "App", TargetName: "Root", PartitionPlan: plan}
			units := []repair.Unit{{ID: "root", SQL: "CREATE TABLE should_not_execute (id integer)", Selected: true, Status: "pending"}}
			out, err := ExecuteRepair(context.Background(), &PostgresWriter{db: db, schema: "App", dbType: "postgres"}, obj, units, nil)
			require.ErrorContains(t, err, "列域快照")
			require.Equal(t, "pending", out[0].Status)
		})
	}
}
