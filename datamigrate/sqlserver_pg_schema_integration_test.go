package datamigrate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

// This opt-in check reads only SQL Server catalogs. All target DDL is confined
// to a new schema in a dedicated dbgold_s2kb_it_ database; no data is copied.
func TestSQLServerPGSchemaIntegration(t *testing.T) {
	if os.Getenv("DBGOLD_SQLSERVER_PG_SCHEMA_INTEGRATION") != "1" {
		t.Skip("requires DBGOLD_SQLSERVER_PG_SCHEMA_INTEGRATION=1")
	}
	srcDSN, dstDSN := os.Getenv("DBGOLD_SQLSERVER_PG_SOURCE_DSN"), os.Getenv("DBGOLD_TEST_KINGBASE_DSN")
	if srcDSN == "" || dstDSN == "" {
		t.Fatal("requires SQL Server source and dedicated Kingbase test DSNs")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	srcDB, err := sql.Open("sqlserver", srcDSN)
	if err != nil {
		t.Fatal("cannot initialize SQL Server metadata connection")
	}
	t.Cleanup(func() { require.NoError(t, srcDB.Close()) })
	var version string
	if err := srcDB.QueryRowContext(ctx, `SELECT CAST(SERVERPROPERTY('ProductVersion') AS nvarchar(128))`).Scan(&version); err != nil {
		t.Fatal("cannot read SQL Server product version")
	}
	t.Logf("SQL Server product version: %s", version)
	require.True(t, strings.HasPrefix(version, "11."), "requires actual SQL Server 2012")
	r, err := source.NewSQLServer(srcDSN, "", source.ConnPoolConfig{MaxOpenConns: 1, MaxIdleConns: 1})
	if err != nil {
		t.Fatal("cannot open SQL Server catalog reader")
	}
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	views, err := r.GetViewsForTarget(ctx, "kingbase")
	require.NoError(t, err)
	require.Len(t, views, 29, "the reported source inventory must remain explicit")
	viewNames := map[string]bool{}
	selected := make([]source.ViewInfo, 0, 28)
	manualCount := 0
	for _, view := range views {
		viewNames[strings.ToLower(view.ViewName)] = true
		if strings.EqualFold(view.ViewName, "view_tjfx_hzzdnameandtable") {
			manualCount++
			continue
		}
		selected = append(selected, view)
	}
	require.Equal(t, 1, manualCount, "only the approved system-catalog view is excluded")
	require.Len(t, selected, 28)

	db, err := sql.Open("postgres", dstDSN)
	if err != nil {
		t.Fatal("cannot initialize Kingbase test connection")
	}
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	var database string
	if err := db.QueryRowContext(ctx, "SELECT current_database()").Scan(&database); err != nil {
		t.Fatal("cannot inspect dedicated Kingbase test database")
	}
	require.True(t, strings.HasPrefix(database, "dbgold_s2kb_it_"), "target must be a dedicated temporary test database")
	schema := fmt.Sprintf("dbgold_s2pg_schema_%d", time.Now().UnixNano())
	_, err = db.ExecContext(ctx, "CREATE SCHEMA "+source.QuotePGIdent(schema))
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := db.ExecContext(cleanup, "DROP SCHEMA "+source.QuotePGIdent(schema)+" CASCADE")
		require.NoError(t, err)
	})
	w, err := target.NewKingbase(dstDSN, schema, target.ConnPoolConfig{})
	if err != nil {
		t.Fatal("cannot open isolated Kingbase schema writer")
	}
	t.Cleanup(func() { require.NoError(t, w.Close()) })
	m := NewMigrator(r, w, &Job{LogCh: make(chan string, 8192)}, Config{TargetDBType: "kingbase", TargetSchema: schema, LowerCaseNames: true, Content: "schema_only"})
	dependencies := map[string]string{}
	for _, view := range selected {
		refs, err := migrationViewDependencies(view.Definition, "sqlserver")
		require.NoError(t, err, "dependency scan: %s", view.ViewName)
		for _, ref := range refs {
			if !viewNames[strings.ToLower(ref)] {
				dependencies[strings.ToLower(ref)] = ref
			}
		}
	}
	tableNames := make([]string, 0, len(dependencies))
	for _, name := range dependencies {
		tableNames = append(tableNames, name)
	}
	sort.Strings(tableNames)
	for _, name := range tableNames {
		// Resolve source spelling and ambiguity using the same catalog interface
		// as view binding, then use production table DDL without type overrides.
		identity, err := r.GetViewRelationColumns(ctx, "", name)
		require.NoError(t, err, "source relation: %s", name)
		info, err := r.GetTableDDLInfo(ctx, identity.TableName)
		require.NoError(t, err, "source table structure: %s", identity.TableName)
		for _, col := range identity.Columns {
			require.NotEqual(t, "user_defined", col.TypeKind, "unmapped user-defined type: %s.%s", identity.TableName, col.Name)
		}
		statements, err := w.Dialect().CreateTableStatements(schema, info, "sqlserver", dialect.TypeOpt{}, strings.ToLower)
		require.NoError(t, err, "generate table structure: %s", identity.TableName)
		var create []dialect.Statement
		for _, statement := range statements {
			if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(statement.SQL)), "CREATE TABLE ") {
				create = append(create, statement)
			}
		}
		require.Len(t, create, 1, "must execute exactly one CREATE, never DROP: %s", identity.TableName)
		require.NoError(t, w.CreateTable(ctx, create), "create target table structure: %s", identity.TableName)
	}
	category := newCategoryReport()
	category.Total = len(selected)
	m.migrateIndependentViews(ctx, selected, &category)
	for _, item := range category.Items {
		t.Errorf("view %s stage=%s: %s", item.Name, item.Stage, item.Error)
	}
	t.Logf("source_views=%d manual_views=%d target_tables=%d created_views=%d failed_views=%d", len(views), manualCount, len(tableNames), category.Success, category.Failed)
	require.Zero(t, category.Failed)
	require.Equal(t, 28, category.Success)
	var targetViews int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM information_schema.views WHERE table_schema=$1", schema).Scan(&targetViews))
	require.Equal(t, 28, targetViews)
}
