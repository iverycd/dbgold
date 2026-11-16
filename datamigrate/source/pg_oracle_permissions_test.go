package source

import (
	"context"
	"fmt"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestPGOracleProfileOptionalPermissionsDoNotBlockTables(t *testing.T) {
	for _, oracle := range []bool{false, true} {
		t.Run(fmt.Sprintf("oracle_%t", oracle), func(t *testing.T) {
			db, mock := partitionCatalogMock(t)
			r := &PostgresReader{db: db, schema: "MiXeD", oracleMetadata: oracle}
			mock.ExpectQuery(`SELECT version\(\)`).WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow("PostgreSQL 15"))
			fields := sqlmock.NewRows([]string{"table", "column"})
			for _, field := range [][2]string{{"pg_class", "relname"}, {"pg_attribute", "attname"}, {"pg_attribute", "atttypid"}, {"pg_type", "typtype"}, {"pg_index", "indkey"}, {"pg_constraint", "conkey"}, {"pg_constraint", "confkey"}, {"pg_depend", "refobjid"}} {
				fields.AddRow(field[0], field[1])
			}
			mock.ExpectQuery(`SELECT c.relname, a.attname FROM pg_catalog.pg_attribute`).WithArgs("pg_catalog").WillReturnRows(fields)
			functions := sqlmock.NewRows([]string{"function", "allowed"})
			for _, name := range []string{"pg_get_expr", "format_type", "has_table_privilege"} {
				functions.AddRow(name, true)
			}
			for _, name := range []string{"pg_get_indexdef", "pg_get_constraintdef", "pg_get_triggerdef", "pg_get_viewdef", "obj_description", "col_description", "oidvectortypes"} {
				functions.AddRow(name, false)
			}
			mock.ExpectQuery(`SELECT p.proname,has_function_privilege`).WithArgs("pg_catalog").WillReturnRows(functions)
			if oracle {
				mock.ExpectQuery(`SELECT EXISTS`).WithArgs("MiXeD").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
				mock.ExpectQuery(`SELECT name,setting FROM`).WillReturnRows(sqlmock.NewRows([]string{"name", "setting"}))
			}
			err := r.detectProfile(context.Background(), "postgres")
			if oracle {
				require.NoError(t, err)
				require.False(t, r.profile.Functions["pg_get_indexdef"])
			} else {
				require.ErrorContains(t, err, "pg_get_indexdef")
			}
		})
	}
}

func TestPGOracleSnapshotMissingOptionalMetadataStillReadsColumns(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	r := &PostgresReader{db: db, schema: "MiXeD", oracleMetadata: true, profile: PGProfile{Catalog: "pg_catalog", Functions: map[string]bool{"pg_get_expr": true, "format_type": true, "has_table_privilege": true}}}
	mock.ExpectQuery(`SELECT c.oid,c.relname,c.relkind,.*NULL::text,has_table_privilege`).WithArgs("MiXeD").WillReturnRows(pgSnapshotIdentityRows().AddRow(1, "Orders", "r", false, nil, true, false))
	mock.ExpectQuery(`SELECT a.attnum,a.attname,t.typname,.*NULL::text,format_type`).WithArgs("MiXeD", "Orders").WillReturnRows(sqlmock.NewRows([]string{"ordinal", "name", "type", "kind", "type_schema", "mod", "nullable", "default", "identity", "generated", "comment", "formatted"}).AddRow(1, "MiXeDColumn", "int8", "b", "pg_catalog", -1, false, nil, "", "", nil, "bigint"))
	mock.ExpectQuery(`SELECT t.relname,i.relname FROM`).WithArgs("MiXeD").WillReturnRows(sqlmock.NewRows([]string{"table", "index"}))
	mock.ExpectQuery(`SELECT c.oid,n.nspname,c.relname`).WithArgs("MiXeD").WillReturnError(fmt.Errorf("permission denied for pg_class sequence inventory"))
	snap, err := r.ReadPGOracleSnapshot(context.Background(), []string{"Orders"})
	require.NoError(t, err)
	require.Len(t, snap.Tables, 1)
	require.Equal(t, "MiXeDColumn", snap.Tables[0].Info.Columns[0].Name)
	require.Empty(t, snap.Tables[0].Unsupported)
	kinds := map[string]bool{}
	for _, object := range snap.Objects {
		require.NotEmpty(t, object.Reason)
		kinds[object.Kind] = true
	}
	for _, kind := range []string{"index", "primary_key", "foreign_key", "check", "comment", "trigger", "routine", "view", "sequence"} {
		require.True(t, kinds[kind], "missing diagnostic for %s", kind)
	}
}

func TestPGOracleOptionalQueryPreservesFatalAndLegacyFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		neutral bool
		err     error
		fatal   bool
	}{
		{"permission", true, fmt.Errorf("permission denied"), false},
		{"legacy_permission", false, fmt.Errorf("permission denied"), true},
		{"cancelled", true, context.Canceled, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock := partitionCatalogMock(t)
			r := &PostgresReader{db: db}
			mock.ExpectQuery(`SELECT optional_metadata`).WillReturnError(tc.err)
			snap := &PGSnapshot{}
			rows, err := r.pgOptionalObjectQuery(context.Background(), snap, tc.neutral, "view", "Views", "", "SELECT optional_metadata")
			require.Nil(t, rows)
			if tc.fatal {
				require.ErrorIs(t, err, tc.err)
				require.Empty(t, snap.Objects)
			} else {
				require.NoError(t, err)
				require.Equal(t, "view", snap.Objects[0].Kind)
			}
		})
	}
}
