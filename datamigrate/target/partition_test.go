package target

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"

	"dbgold/datamigrate/partition"
	"dbgold/datamigrate/source"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func partitionInspectorDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, mock.ExpectationsWereMet())
		db.Close()
	})
	return db, mock
}

func partitionDeclarativeFields() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"relation", "attribute"}).AddRow("pg_partitioned_table", "partstrat").AddRow("pg_partitioned_table", "partattrs").AddRow("pg_class", "relpartbound")
}

func partitionNativePGFields(omit string) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{"relation", "attribute"})
	for _, column := range []string{"parttype", "parentid", "partstrategy", "partkey", "boundaries", "interval", "transit"} {
		if omit != column {
			rows.AddRow("pg_partition", column)
		}
	}
	if omit != "relrowmovement" {
		rows.AddRow("pg_class", "relrowmovement")
	}
	return rows
}

func expectPartitionPGProfile(mock sqlmock.Sqlmock, version, number, catalog, searchPath string) {
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT version(),current_setting('server_version_num')`)).WillReturnRows(sqlmock.NewRows([]string{"version", "number"}).AddRow(version, number))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT c.relname,a.attname FROM "` + catalog + `".pg_attribute`)).WithArgs(catalog).WillReturnRows(partitionDeclarativeFields()).RowsWillBeClosed()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT current_schemas(true)::text`)).WillReturnRows(sqlmock.NewRows([]string{"schemas"}).AddRow(searchPath))
}

func TestPartitionInspectorResolvesActualDefaultSchemaBeforeVersionProbes(t *testing.T) {
	db, mock := partitionInspectorDB(t)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT DATABASE()`)).WillReturnRows(sqlmock.NewRows([]string{"database"}).AddRow("MiXeD_Target"))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT VERSION(),@@SESSION.sql_mode,@@version_comment`)).WillReturnRows(sqlmock.NewRows([]string{"version", "mode", "comment"}).AddRow("8.0.41", "STRICT_ALL_TABLES", "MySQL Community Server - GPL"))
	p, err := (dbPartitionInspector{db: db, product: "mysql"}).profile(context.Background())
	require.NoError(t, err)
	require.Equal(t, "MiXeD_Target", p.Schema)
	require.Equal(t, "mysql", p.Product)
	require.Equal(t, "8.0.41", p.Version)
	require.True(t, p.Features["inline_mysql"])
	require.True(t, p.Features["row_movement"])
}

func TestPartitionInspectorUnknownSchemaOrProductNeverGuessesDefaults(t *testing.T) {
	t.Run("null default", func(t *testing.T) {
		db, mock := partitionInspectorDB(t)
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT current_schema()`)).WillReturnRows(sqlmock.NewRows([]string{"schema"}).AddRow(nil))
		_, err := (dbPartitionInspector{db: db, product: "postgres"}).profile(context.Background())
		require.ErrorContains(t, err, "Schema")
	})
	t.Run("unknown product", func(t *testing.T) {
		db, _ := partitionInspectorDB(t)
		_, err := (dbPartitionInspector{db: db, product: "future_vendor", schema: "App"}).profile(context.Background())
		require.Error(t, err)
	})
	t.Run("nil database", func(t *testing.T) {
		_, err := (dbPartitionInspector{product: "postgres", schema: "App"}).profile(context.Background())
		require.Error(t, err)
	})
}

func TestPartitionInspectorMySQLAcceptsOnlyExistingVersionBaselines(t *testing.T) {
	for _, version := range []string{"5.7.44", "8.0.36", "8.4.1", "10.6.1-MariaDB"} {
		t.Run(version, func(t *testing.T) {
			db, mock := partitionInspectorDB(t)
			mock.ExpectQuery(regexp.QuoteMeta(`SELECT VERSION(),@@SESSION.sql_mode,@@version_comment`)).WillReturnRows(sqlmock.NewRows([]string{"version", "mode", "comment"}).AddRow(version, "STRICT_ALL_TABLES", "MySQL Community Server - GPL"))
			p, err := (dbPartitionInspector{db: db, product: "mysql", schema: "App"}).profile(context.Background())
			if version == "5.7.44" || version == "8.0.36" {
				require.NoError(t, err)
				require.True(t, p.Features[partition.FeatureRange])
			} else {
				require.Error(t, err)
				require.False(t, p.Features["inline_mysql"])
			}
		})
	}
}

func TestPartitionInspectorPostgresVersionControlsFutureRowMovementAndHash(t *testing.T) {
	for _, version := range []struct {
		banner, number string
		modern         bool
	}{{"PostgreSQL 10.23", "100023", false}, {"PostgreSQL 16.6", "160006", true}} {
		t.Run(version.number, func(t *testing.T) {
			db, mock := partitionInspectorDB(t)
			expectPartitionPGProfile(mock, version.banner, version.number, "pg_catalog", "{pg_catalog,public}")
			p, err := (dbPartitionInspector{db: db, product: "postgres", schema: "App"}).profile(context.Background())
			require.NoError(t, err)
			require.Equal(t, "declarative:pg_catalog", p.Mode)
			require.Equal(t, version.modern, p.Features[partition.FeatureHash])
			require.Equal(t, version.modern, p.Features[partition.FeatureDefaultRange])
			require.Equal(t, version.modern, p.Features["row_movement"])
		})
	}
}

func TestPartitionInspectorExpressionSupportRequiresSafeBuiltinResolution(t *testing.T) {
	for _, tc := range []struct {
		path    string
		allowed bool
	}{{"{pg_catalog,public}", true}, {"{pg_temp_5,pg_catalog,public}", true}, {"{custom,pg_catalog,public}", false}, {"{}", false}} {
		t.Run(tc.path, func(t *testing.T) {
			db, mock := partitionInspectorDB(t)
			expectPartitionPGProfile(mock, "PostgreSQL 16.6", "160006", "pg_catalog", tc.path)
			p, err := (dbPartitionInspector{db: db, product: "postgres", schema: "App"}).profile(context.Background())
			require.NoError(t, err)
			require.Equal(t, tc.allowed, p.Features[partition.FeatureExpressions])
		})
	}
}

func TestPartitionInspectorNativePGCatalogIsNotAssumedDeclarative(t *testing.T) {
	for _, missing := range []string{"", "transit", "relrowmovement"} {
		t.Run("missing="+missing, func(t *testing.T) {
			db, mock := partitionInspectorDB(t)
			mock.ExpectQuery(regexp.QuoteMeta(`SELECT version(),current_setting('server_version_num')`)).WillReturnRows(sqlmock.NewRows([]string{"version", "number"}).AddRow("openGauss 5.0.3", "90204"))
			mock.ExpectQuery(regexp.QuoteMeta(`SELECT c.relname,a.attname FROM "pg_catalog".pg_attribute`)).WithArgs("pg_catalog").WillReturnRows(partitionNativePGFields(missing)).RowsWillBeClosed()
			p, err := (dbPartitionInspector{db: db, product: "gaussdb", schema: "MiXeD"}).profile(context.Background())
			require.False(t, p.Features["postgres_declarative"])
			if missing == "" {
				require.NoError(t, err)
				require.Equal(t, "native:pg_catalog", p.Mode)
				require.True(t, p.Features["inline_gauss"])
				require.True(t, p.Features["row_movement_clause"])
				require.False(t, p.Features[partition.FeatureExpressions])
			} else {
				require.Error(t, err)
				require.False(t, p.Features["inline_gauss"])
			}
		})
	}
}

func TestPartitionInspectorCannotEnableCapabilitiesFromAliasOrUnreadableCatalog(t *testing.T) {
	db, mock := partitionInspectorDB(t)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT version(),current_setting('server_version_num')`)).WillReturnRows(sqlmock.NewRows([]string{"version", "number"}).AddRow("HighGo unknown build", "90600"))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT c.relname,a.attname FROM "pg_catalog".pg_attribute`)).WithArgs("pg_catalog").WillReturnError(errors.New("catalog denied"))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT c.relname,a.attname FROM "sys_catalog".pg_attribute`)).WithArgs("sys_catalog").WillReturnError(errors.New("catalog denied"))
	p, err := (dbPartitionInspector{db: db, product: "highgo", schema: "App"}).profile(context.Background())
	require.ErrorContains(t, err, "catalog denied")
	require.False(t, p.Features["postgres_declarative"])
	require.False(t, p.Features["inline_gauss"])
}

func TestPartitionInspectorOracleOptionAndDMNativeCatalogAreReadOnlyRequirements(t *testing.T) {
	t.Run("Oracle option absent", func(t *testing.T) {
		db, mock := partitionInspectorDB(t)
		mock.ExpectQuery(`SELECT BANNER FROM V\$VERSION`).WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow("Oracle Database 19c"))
		mock.ExpectQuery(`SELECT VALUE FROM V\$PARAMETER`).WillReturnRows(sqlmock.NewRows([]string{"compatible"}).AddRow("19.0.0"))
		mock.ExpectQuery(`SELECT VALUE FROM V\$OPTION`).WillReturnRows(sqlmock.NewRows([]string{"partitioning"}).AddRow("FALSE"))
		p, err := (dbPartitionInspector{db: db, product: "oracle", schema: "APP"}).profile(context.Background())
		require.ErrorContains(t, err, "Partitioning")
		require.False(t, p.Features["inline_oracle"])
	})
	t.Run("DM8 observed build", func(t *testing.T) {
		db, mock := partitionInspectorDB(t)
		mock.ExpectQuery(`SELECT SVR_VERSION FROM V\$INSTANCE`).WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow("DM Database Server 64 V8.1.2.192"))
		mock.ExpectQuery(`SELECT PARA_VALUE FROM V\$DM_INI`).WillReturnRows(sqlmock.NewRows([]string{"mode"}).AddRow("2"))
		mock.ExpectQuery(`SELECT PARTITION_NAME,HIGH_VALUE,PARTITION_POSITION FROM ALL_TAB_PARTITIONS WHERE 1=0`).WillReturnRows(sqlmock.NewRows([]string{"name", "bound", "position"})).RowsWillBeClosed()
		p, err := (dbPartitionInspector{db: db, product: "dameng", schema: "MiXeD"}).profile(context.Background())
		require.NoError(t, err)
		require.Equal(t, "MiXeD", p.Schema)
		require.Equal(t, "2", p.Mode)
		require.True(t, p.Features["row_movement_clause"])
	})
}

func TestPartitionInspectorPostgresNamesUseExactBoundIdentitiesAndRespectRootOwnership(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "owned or free", true: "unrelated object"}[conflict], func(t *testing.T) {
			db, mock := partitionInspectorDB(t)
			expectPartitionPGProfile(mock, "PostgreSQL 16.6", "160006", "pg_catalog", "{pg_catalog,public}")
			mock.ExpectQuery(`WITH RECURSIVE owned AS .*NOT EXISTS`).WithArgs(`Root"Owner`, `Root"Table`, `Leaf"Owner`, `P'; DROP TABLE victim; --`).WillReturnRows(sqlmock.NewRows([]string{"conflict"}).AddRow(conflict))
			spec := &source.PartitionSpec{Partitions: []source.Partition{{Schema: `Leaf"Owner`, Name: `P'; DROP TABLE victim; --`}}}
			err := (dbPartitionInspector{db: db, product: "postgres", schema: `Root"Owner`}).names(context.Background(), `Root"Table`, spec)
			if conflict {
				require.ErrorContains(t, err, "占用")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestPartitionInspectorSQLServerChecksDatabaseScopedHelpersBeforeDDL(t *testing.T) {
	for _, existing := range []int{0, 1} {
		t.Run(map[int]string{0: "free", 1: "existing shared helper"}[existing], func(t *testing.T) {
			db, mock := partitionInspectorDB(t)
			mock.ExpectQuery(regexp.QuoteMeta(`SELECT CAST(SERVERPROPERTY('ProductVersion') AS NVARCHAR(128))`)).WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow("10.50.6000.34"))
			mock.ExpectQuery(`SELECT name FROM sys.filegroups WHERE is_default=1`).WillReturnRows(sqlmock.NewRows([]string{"filegroup"}).AddRow("ActualDefault"))
			fn, scheme := partition.SQLServerHelperNames("MiXeD", "Orders")
			mock.ExpectQuery(`SELECT \(SELECT COUNT\(\*\) FROM sys.partition_functions`).WithArgs(fn, scheme).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(existing))
			err := (dbPartitionInspector{db: db, product: "sqlserver", schema: "MiXeD"}).names(context.Background(), "Orders", &source.PartitionSpec{})
			if existing > 0 {
				require.ErrorContains(t, err, "不能覆盖")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestPartitionInspectorTableUsesResolvedSchemaRatherThanConfiguredAlias(t *testing.T) {
	db, mock := partitionInspectorDB(t)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT SCHEMA_NAME()`)).WillReturnRows(sqlmock.NewRows([]string{"schema"}).AddRow("ActualOwner"))
	mock.ExpectQuery(`SELECT s.name,t.object_id.*s.name=@p2`).WithArgs("SameTable", "ActualOwner").WillReturnRows(sqlmock.NewRows([]string{"owner", "id"}).AddRow("ActualOwner", 42))
	mock.ExpectQuery(`SELECT pf.name,pf.boundary_value_on_right`).WithArgs(int64(42)).WillReturnRows(sqlmock.NewRows([]string{"function", "right", "fanout", "key", "type", "collation"}))
	spec, err := (dbPartitionInspector{db: db, product: "sqlserver"}).table(context.Background(), "SameTable")
	require.NoError(t, err)
	require.Nil(t, spec)
}
