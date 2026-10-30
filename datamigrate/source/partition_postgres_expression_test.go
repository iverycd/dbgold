package source

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestPGPartitionExpressionDependenciesBindCatalogIdentities(t *testing.T) {
	for _, tc := range []struct{ kind, owner, name string }{
		{"function", "App", "abs"},
		{"function", "App", "mod"},
		{"function", "App", "div"},
		{"function", "App", "coalesce"},
		{"operator", "App", "+"},
		{"operator", "App", "*"},
		{"function", "pg_catalog", "ABS"},
		{"type", "App", "int8"},
		{"type", "pg_catalog", "int8"},
	} {
		t.Run(tc.kind+"/"+tc.owner+"/"+tc.name, func(t *testing.T) {
			db, mock := partitionCatalogMock(t)
			r := &PostgresReader{db: db, profile: PGProfile{Catalog: "pg_catalog"}}
			rows := sqlmock.NewRows([]string{"kind", "oid", "owner", "name"}).AddRow(tc.kind, 12345, tc.owner, tc.name)
			mock.ExpectQuery(`SELECT CASE WHEN d.refclassid=.*FROM "pg_catalog"."pg_depend".*d.objid=\$1 AND \(d.objsubid=0 OR d.refclassid=`).WithArgs(int64(42)).WillReturnRows(rows).RowsWillBeClosed()
			spec := &PartitionSpec{Keys: []PartitionKey{{Expression: `abs("Key"::numeric)`}}}
			require.NoError(t, r.pgPartitionExpressionDependencies(context.Background(), 42, "0", spec))
			require.Contains(t, spec.Unsupported, tc.owner+"."+tc.name)
			require.Contains(t, spec.Unsupported, "12345")
		})
	}
}

func TestPGPartitionExpressionDependenciesKeepPinnedBuiltinsAndDirectColumns(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	r := &PostgresReader{db: db, profile: PGProfile{Catalog: "pg_catalog"}}
	direct := &PartitionSpec{Keys: []PartitionKey{{Column: "Key"}}}
	require.NoError(t, r.pgPartitionExpressionDependencies(context.Background(), 42, "1", direct))
	mock.ExpectQuery(`SELECT CASE WHEN d.refclassid=`).WithArgs(int64(42)).WillReturnRows(sqlmock.NewRows([]string{"kind", "oid", "owner", "name"})).RowsWillBeClosed()
	// Even a deparser which reduces an expression to a bare column must not
	// bypass inspection when native partattrs marks the key as an expression.
	require.NoError(t, r.pgPartitionExpressionDependencies(context.Background(), 42, "0", direct))
	require.Empty(t, direct.Unsupported)
}

func TestPGPartitionExpressionDependencyErrorsNeverCertifyBuiltins(t *testing.T) {
	for _, failure := range []string{"permission", "row", "identity", "close"} {
		t.Run(failure, func(t *testing.T) {
			db, mock := partitionCatalogMock(t)
			r := &PostgresReader{db: db, profile: PGProfile{Catalog: "pg_catalog"}}
			expect := mock.ExpectQuery(`SELECT CASE WHEN d.refclassid=`).WithArgs(int64(42))
			rows := sqlmock.NewRows([]string{"kind", "oid", "owner", "name"})
			switch failure {
			case "permission":
				expect.WillReturnError(errors.New("catalog denied"))
			case "row":
				rows.AddRow("function", 12, "App", "abs").RowError(0, errors.New("truncated dependency cursor"))
			case "identity":
				rows.AddRow("function", 12, "", "")
			case "close":
				rows.CloseError(errors.New("catalog close failed"))
			}
			if failure != "permission" {
				expect.WillReturnRows(rows).RowsWillBeClosed()
			}
			spec := &PartitionSpec{Keys: []PartitionKey{{Expression: `abs("Key"::numeric)`}}}
			require.Error(t, r.pgPartitionExpressionDependencies(context.Background(), 42, "0", spec))
		})
	}
}
