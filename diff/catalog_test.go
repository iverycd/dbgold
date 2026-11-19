package diff

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReadPGStructureCatalog(t *testing.T) {
	dsn := os.Getenv("DBGOLD_STRUCTURE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("需要可创建隔离 schema 的 DBGOLD_STRUCTURE_TEST_PG_DSN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer db.Close()
	schema := fmt.Sprintf("DbGoldCompare%d", time.Now().UnixNano())
	scope := `"` + schema + `"`
	_, err = db.ExecContext(ctx, "CREATE SCHEMA "+scope)
	require.NoError(t, err)
	defer func() { _, _ = db.Exec("DROP SCHEMA " + scope + " CASCADE") }()
	for _, ddl := range []string{
		`CREATE TABLE ` + scope + `."Parent" ("Key" bigint PRIMARY KEY)`,
		`CREATE TABLE ` + scope + `."Child" ("Id" bigserial PRIMARY KEY, "ParentKey" bigint REFERENCES ` + scope + `."Parent"("Key") ON DELETE CASCADE,
"Text" varchar(40) COLLATE "C", "Score" numeric(8,2) DEFAULT 1.25 CHECK ("Score" >= 0), "Computed" numeric GENERATED ALWAYS AS ("Score" * 2) STORED)`,
		`CREATE INDEX "ByText" ON ` + scope + `."Child" ("Text" DESC)`,
		`CREATE INDEX "Partial" ON ` + scope + `."Child" ("Score") WHERE "Score" > 0`,
		`COMMENT ON COLUMN ` + scope + `."Child"."Text" IS 'Mixed Case'`,
		`CREATE VIEW ` + scope + `."SameNameView" AS SELECT 1`,
	} {
		_, err = db.ExecContext(ctx, ddl)
		require.NoError(t, err)
	}
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	c, err := ReadCatalog(ctx, tx, "postgres", schema, []string{"Child"})
	require.NoError(t, err)
	require.True(t, c.TransactionalDDL)
	require.Equal(t, "v", c.Occupied["SameNameView"])
	require.Equal(t, "r", c.Occupied["Parent"])
	require.NotContains(t, c.Tables, "Parent")
	table := c.Tables["Child"]
	require.NotNil(t, table)
	require.Empty(t, table.Unsupported)
	require.Len(t, table.Columns, 5)
	require.Equal(t, "auto_increment", table.Columns[0].Extra)
	require.Equal(t, "pg_catalog.C", table.Columns[2].Collation)
	require.Equal(t, "Mixed Case", table.Columns[2].Comment)
	require.Equal(t, int64(8), table.Columns[3].Precision)
	require.Equal(t, int64(2), table.Columns[3].Scale)
	require.NotEmpty(t, table.Columns[4].Generated)
	require.Len(t, table.Keys, 3)
	for _, key := range table.Keys {
		if key.Kind == "foreign" {
			require.Equal(t, []string{"Key"}, key.RefColumns)
			require.Equal(t, "CASCADE", key.OnDelete)
		}
	}
	require.Len(t, table.Indexes, 2)
	require.Equal(t, []bool{true}, table.Indexes[0].Descending)
	require.NotEmpty(t, table.Indexes[0].Unsupported) // Column has a non-default collation.
	require.NotEmpty(t, table.Indexes[1].Unsupported) // Partial index.
}

func TestReadMySQLStructureCatalog(t *testing.T) {
	dsn := os.Getenv("DBGOLD_STRUCTURE_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("需要可创建隔离数据库的 DBGOLD_STRUCTURE_TEST_MYSQL_DSN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := sql.Open("mysql", dsn)
	require.NoError(t, err)
	defer db.Close()
	schema := fmt.Sprintf("dbgold_compare%d", time.Now().UnixNano())
	_, err = db.ExecContext(ctx, "CREATE DATABASE "+schema+" CHARACTER SET utf8mb4 COLLATE utf8mb4_bin")
	require.NoError(t, err)
	defer func() { _, _ = db.Exec("DROP DATABASE " + schema) }()
	for _, ddl := range []string{
		`CREATE TABLE ` + schema + `.parent (Id BIGINT UNSIGNED PRIMARY KEY)`,
		`CREATE TABLE ` + schema + `.child (Id BIGINT UNSIGNED PRIMARY KEY AUTO_INCREMENT, ParentId BIGINT UNSIGNED,
Name VARCHAR(40) DEFAULT 'Mixed Case' COMMENT 'Preserve text', Computed BIGINT GENERATED ALWAYS AS (ParentId * 2),
INDEX NamePrefix (Name(8)), CONSTRAINT ParentFK FOREIGN KEY (ParentId) REFERENCES ` + schema + `.parent(Id) ON DELETE CASCADE)`,
		`CREATE VIEW ` + schema + `.namespace_view AS SELECT 1`,
	} {
		_, err = db.ExecContext(ctx, ddl)
		require.NoError(t, err)
	}
	c, err := ReadCatalog(ctx, db, "mysql", schema, []string{"child"})
	require.NoError(t, err)
	require.False(t, c.TransactionalDDL)
	require.Equal(t, "VIEW", c.Occupied["namespace_view"])
	require.Equal(t, "BASE TABLE", c.Occupied["parent"])
	require.NotContains(t, c.Tables, "parent")
	table := c.Tables["child"]
	require.NotNil(t, table)
	require.Empty(t, table.Unsupported)
	require.Equal(t, "InnoDB", table.Engine)
	require.Equal(t, "utf8mb4", table.Charset)
	require.Equal(t, "utf8mb4_bin", table.Collation)
	require.Len(t, table.Columns, 4)
	require.Contains(t, table.Columns[0].FormattedType, "unsigned")
	require.Equal(t, "auto_increment", table.Columns[0].Extra)
	require.Equal(t, "Name", table.Columns[2].Name)
	require.NotNil(t, table.Columns[2].Default)
	require.Equal(t, "Mixed Case", *table.Columns[2].Default)
	require.Equal(t, "Preserve text", table.Columns[2].Comment)
	require.NotEmpty(t, table.Columns[3].Generated)
	require.Len(t, table.Keys, 2)
	for _, key := range table.Keys {
		if key.Kind == "foreign" {
			require.Equal(t, []string{"Id"}, key.RefColumns)
			require.Equal(t, "CASCADE", key.OnDelete)
		}
	}
	var foundPrefix bool
	for _, index := range table.Indexes {
		if index.Name == "NamePrefix" {
			foundPrefix = true
			require.Equal(t, []int{8}, index.PrefixLengths)
		}
	}
	require.True(t, foundPrefix)
}

func TestReadDamengStructureCatalog(t *testing.T) {
	dsn := os.Getenv("DBGOLD_STRUCTURE_TEST_DM_DSN")
	if dsn == "" {
		t.Skip("需要可创建隔离 schema 的 DBGOLD_STRUCTURE_TEST_DM_DSN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := sql.Open("dm", dsn)
	require.NoError(t, err)
	defer db.Close()
	schema := fmt.Sprintf("DBGOLD_COMPARE%d", time.Now().UnixNano())
	_, err = db.ExecContext(ctx, "CREATE SCHEMA "+schema+" AUTHORIZATION SYSDBA")
	require.NoError(t, err)
	defer func() { _, _ = db.Exec("DROP SCHEMA " + schema + " CASCADE") }()
	for _, ddl := range []string{
		`CREATE TABLE ` + schema + `.PARENT (ID BIGINT PRIMARY KEY)`,
		`CREATE TABLE ` + schema + `.CHILD (ID BIGINT IDENTITY(1,1) PRIMARY KEY, PARENT_ID BIGINT,
"Name" VARCHAR2(40 CHAR) DEFAULT 'Mixed Case', AMOUNT NUMBER(10,2), STAMP TIMESTAMP(4),
CONSTRAINT CHILD_FK FOREIGN KEY (PARENT_ID) REFERENCES ` + schema + `.PARENT(ID) ON DELETE CASCADE)`,
		`CREATE INDEX CHILD_NAME ON ` + schema + `.CHILD ("Name" DESC)`,
		`COMMENT ON COLUMN ` + schema + `.CHILD."Name" IS 'Preserve text'`,
		`CREATE VIEW ` + schema + `.NAMESPACE_VIEW AS SELECT 1 AS DUMMY`,
	} {
		_, err = db.ExecContext(ctx, ddl)
		require.NoError(t, err)
	}
	c, err := ReadCatalog(ctx, db, "dameng", schema, []string{"CHILD"})
	require.NoError(t, err)
	require.False(t, c.TransactionalDDL)
	require.Equal(t, "VIEW", c.Occupied["NAMESPACE_VIEW"])
	require.Equal(t, "TABLE", c.Occupied["PARENT"])
	require.NotContains(t, c.Tables, "PARENT")
	table := c.Tables["CHILD"]
	require.NotNil(t, table)
	require.Empty(t, table.Unsupported)
	require.Len(t, table.Columns, 5)
	require.Equal(t, "auto_increment", table.Columns[0].Extra)
	require.Equal(t, "Name", table.Columns[2].Name)
	require.Equal(t, "Preserve text", table.Columns[2].Comment)
	require.Equal(t, "C", table.Columns[2].CharUsed)
	require.Contains(t, table.Columns[2].FormattedType, "(40 CHAR)")
	require.Equal(t, int64(10), table.Columns[3].Precision)
	require.Equal(t, int64(2), table.Columns[3].Scale)
	require.Equal(t, int64(4), table.Columns[4].DateTimePrecision)
	require.Len(t, table.Keys, 2)
	for _, key := range table.Keys {
		if key.Kind == "foreign" {
			require.Empty(t, key.Unsupported)
			require.Equal(t, []string{"ID"}, key.RefColumns)
			require.Equal(t, "CASCADE", key.OnDelete)
		}
	}
	require.Len(t, table.Indexes, 1)
	require.Equal(t, []bool{true}, table.Indexes[0].Descending)
}
