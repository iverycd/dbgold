//go:build integration

package cdc

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPostgresCDCFKAndSequenceIntegration(t *testing.T) {
	if os.Getenv("CDC_INTEGRATION") != "1" {
		t.Skip("requires disposable MySQL and PostgreSQL instances")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	source := mustOpenIntegrationDB(t, "mysql", integrationSourceAdminDSN)
	defer source.Close()
	target := mustOpenIntegrationDB(t, "postgres", integrationTargetDSN)
	defer target.Close()
	namespace := fmt.Sprintf("cdc_k_%d", time.Now().UnixNano())
	exec := func(db *sql.DB, query string) {
		t.Helper()
		_, err := db.ExecContext(ctx, query)
		require.NoError(t, err, "%s", query)
	}
	exec(source, "CREATE DATABASE "+namespace)
	defer source.Exec("DROP DATABASE " + namespace)
	exec(target, "CREATE SCHEMA "+quoteIdent(namespace))
	defer target.Exec("DROP SCHEMA " + quoteIdent(namespace) + " CASCADE")
	sourceTable := func(name string) string { return namespace + "." + name }
	targetTable := func(name string) string { return qualifiedIdent(namespace, name) }
	exec(source, "CREATE TABLE "+sourceTable("Parent")+" (Id BIGINT AUTO_INCREMENT PRIMARY KEY, Note VARCHAR(20))")
	exec(source, "CREATE TABLE "+sourceTable("Child")+" (Id BIGINT PRIMARY KEY, ParentId BIGINT UNIQUE, CONSTRAINT MixedFK FOREIGN KEY (ParentId) REFERENCES "+sourceTable("Parent")+"(Id) ON UPDATE CASCADE ON DELETE CASCADE)")
	exec(source, "CREATE TABLE "+sourceTable("Grandchild")+" (Id BIGINT PRIMARY KEY, ParentId BIGINT, CONSTRAINT GrandFK FOREIGN KEY (ParentId) REFERENCES "+sourceTable("Child")+"(ParentId) ON UPDATE CASCADE ON DELETE CASCADE)")
	exec(source, "INSERT INTO "+sourceTable("Parent")+" VALUES (1,'initial'),(1000,'deleted')")
	exec(source, "DELETE FROM "+sourceTable("Parent")+" WHERE Id=1000")
	exec(target, "CREATE TABLE "+targetTable("parent")+" (id BIGSERIAL PRIMARY KEY, note VARCHAR(20))")
	exec(target, "CREATE TABLE "+targetTable("child")+" (id BIGINT PRIMARY KEY, parentid BIGINT UNIQUE, CONSTRAINT target_fk FOREIGN KEY (parentid) REFERENCES "+targetTable("parent")+"(id) ON UPDATE CASCADE ON DELETE CASCADE)")
	exec(target, "CREATE TABLE "+targetTable("grandchild")+" (id BIGINT PRIMARY KEY, parentid BIGINT, CONSTRAINT target_grand_fk FOREIGN KEY (parentid) REFERENCES "+targetTable("child")+"(parentid) ON UPDATE CASCADE ON DELETE CASCADE)")
	exec(target, "INSERT INTO "+targetTable("parent")+" VALUES(1,'initial')")
	exec(target, "INSERT INTO "+targetTable("child")+" VALUES(10,1)")
	exec(target, "INSERT INTO "+targetTable("grandchild")+" VALUES(100,1)")

	table := TableInfo{Name: "Parent", Columns: []string{"Id", "Note"}, ColumnTypes: []string{"BIGINT", "VARCHAR"}, PrimaryKey: []int{0}, AutoIncrement: []int{0}, LocatorStrategy: LocatorPrimaryKey, LocatorColumns: []string{"Id"}}
	scope := []TableInfo{table,
		{Name: "Child", Columns: []string{"Id", "ParentId"}},
		{Name: "Grandchild", Columns: []string{"Id", "ParentId"}},
	}
	require.NoError(t, LoadSourceInboundForeignKeys(ctx, source, namespace, scope))
	require.Len(t, scope[0].InboundForeignKeys, 1)
	require.Len(t, scope[1].InboundForeignKeys, 1)
	cfg := Config{JobID: "cascade", SourceDSN: integrationSourceAdminDSN, SourceDatabase: namespace, TargetDSN: integrationTargetDSN, TargetSchema: namespace, LowerCaseNames: true}
	a, err := newPostgresApplier(ctx, cfg)
	require.NoError(t, err)
	defer a.Close()
	setApplierForeignKeyScope(a, scope)
	tables := scope[:1]
	p1, p2 := Position{File: "bin.000001", Pos: 100}, Position{File: "bin.000001", Pos: 200}
	_, err = a.Apply(ctx, []Change{{Action: "update", Table: &tables[0], Before: []any{int64(1), "initial"}, After: []any{int64(2), "updated"}}}, p1)
	require.NoError(t, err)
	var parentID int64
	require.NoError(t, target.QueryRowContext(ctx, "SELECT parentid FROM "+targetTable("child")+" WHERE id=10").Scan(&parentID))
	require.EqualValues(t, 2, parentID)
	require.NoError(t, target.QueryRowContext(ctx, "SELECT parentid FROM "+targetTable("grandchild")+" WHERE id=100").Scan(&parentID))
	require.EqualValues(t, 2, parentID)
	exec(target, "ALTER TABLE "+targetTable("grandchild")+" DROP CONSTRAINT target_grand_fk")
	_, err = a.Apply(ctx, []Change{{Action: "update", Table: &tables[0], Before: []any{int64(2), "updated"}, After: []any{int64(3), "wrong"}}}, p2)
	require.ErrorContains(t, err, "缺少与源入向外键")
	checkpoint, ok, err := a.LoadCheckpoint(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, p1, checkpoint)

	require.NoError(t, a.SyncSequences(ctx, tables))
	qseq := targetTable("parent_id_seq")
	var generated int64
	require.NoError(t, target.QueryRowContext(ctx, "SELECT nextval($1::regclass)", qseq).Scan(&generated))
	require.EqualValues(t, 1001, generated)
	_, err = target.ExecContext(ctx, "SELECT setval($1::regclass,5000,false)", qseq)
	require.NoError(t, err)
	require.NoError(t, a.SyncSequences(ctx, tables))
	require.NoError(t, target.QueryRowContext(ctx, "SELECT nextval($1::regclass)", qseq).Scan(&generated))
	require.EqualValues(t, 5000, generated)

	exec(source, "CREATE TABLE "+sourceTable("Overflow")+" (Id BIGINT AUTO_INCREMENT PRIMARY KEY) AUTO_INCREMENT=200")
	exec(target, "CREATE TABLE "+targetTable("overflow")+" (id BIGSERIAL PRIMARY KEY)")
	exec(target, "ALTER SEQUENCE "+targetTable("overflow_id_seq")+" MAXVALUE 100")
	_, err = target.ExecContext(ctx, "SELECT setval($1::regclass,1,false)", qseq)
	require.NoError(t, err)
	tables = append(tables, TableInfo{Name: "Overflow", Columns: []string{"Id"}, AutoIncrement: []int{0}})
	require.ErrorContains(t, a.SyncSequences(ctx, tables), "超出目标序列最大值")
	require.NoError(t, target.QueryRowContext(ctx, "SELECT nextval($1::regclass)", qseq).Scan(&generated))
	require.EqualValues(t, 1, generated, "all sequences must be validated before changing any")

	exec(source, "CREATE TABLE "+sourceTable("Narrow")+" (Id BIGINT AUTO_INCREMENT PRIMARY KEY) AUTO_INCREMENT=40000")
	exec(target, "CREATE TABLE "+targetTable("narrow")+" (id SMALLINT PRIMARY KEY)")
	exec(target, "CREATE SEQUENCE "+targetTable("seq_narrow_id")+" OWNED BY "+targetTable("narrow")+".id")
	require.ErrorContains(t, a.SyncSequences(ctx, []TableInfo{{Name: "Narrow", Columns: []string{"Id"}, AutoIncrement: []int{0}}}), "超出目标列最大值 32767")
}

func TestDamengCDCKeyChangesAndInboundForeignKeysIntegration(t *testing.T) {
	dsn := os.Getenv("CDC_DAMENG_DSN")
	if os.Getenv("CDC_DAMENG_INTEGRATION") != "1" || dsn == "" {
		t.Skip("requires a disposable Dameng instance")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	db, err := sql.Open("dm", dsn)
	require.NoError(t, err)
	defer db.Close()
	schema := fmt.Sprintf("CDC_FK_%d", time.Now().UnixNano())
	childSchema := schema + "_C"
	exec := func(query string) {
		t.Helper()
		_, e := db.ExecContext(ctx, query)
		require.NoError(t, e, "%s", query)
	}
	for _, name := range []string{schema, childSchema} {
		exec("CREATE SCHEMA " + quoteIdent(name) + " AUTHORIZATION SYSDBA")
		defer db.Exec("DROP SCHEMA " + quoteIdent(name) + " CASCADE")
	}
	parent := qualifiedIdent(schema, "PARENT")
	child := qualifiedIdent(childSchema, "CHILD")
	plain := qualifiedIdent(schema, "PLAIN")
	exec("CREATE TABLE " + parent + " (ID BIGINT IDENTITY(1,1) PRIMARY KEY, NOTE VARCHAR(20))")
	exec("CREATE TABLE " + child + " (ID BIGINT PRIMARY KEY, PID BIGINT, CONSTRAINT CFK FOREIGN KEY (PID) REFERENCES " + parent + "(ID) ON DELETE CASCADE)")
	exec("CREATE TABLE " + plain + " (ID BIGINT PRIMARY KEY, NOTE VARCHAR(20))")
	exec("INSERT INTO " + parent + " (NOTE) VALUES ('old')")
	exec("INSERT INTO " + child + " VALUES (100,1)")
	exec("INSERT INTO " + plain + " VALUES (1,'old')")
	a, err := newDamengApplier(ctx, Config{TargetDBType: "dameng", TargetDSN: dsn, TargetSchema: schema, JobID: "fk-safety"})
	require.NoError(t, err)
	defer a.Close()
	p1, p2 := Position{File: "bin.000001", Pos: 100}, Position{File: "bin.000001", Pos: 200}
	table := TableInfo{Name: "Plain", Columns: []string{"Id", "Note"}, ColumnTypes: []string{"BIGINT", "VARCHAR"}, LocatorStrategy: LocatorPrimaryKey, LocatorColumns: []string{"Id"}, ForeignKeysKnown: true}
	_, err = a.Apply(ctx, []Change{{Action: "update", Table: &table, Before: []any{int64(1), "old"}, After: []any{int64(9), "new"}}}, p1)
	require.NoError(t, err)
	table.Name = "Parent"
	_, err = a.Apply(ctx, []Change{{Action: "update", Table: &table, Before: []any{int64(1), "old"}, After: []any{int64(9), "new"}}}, p2)
	require.ErrorContains(t, err, "入向外键")
	require.ErrorContains(t, err, childSchema+".CHILD")
	var parentID int64
	require.NoError(t, db.QueryRowContext(ctx, "SELECT ID FROM "+plain).Scan(&parentID))
	require.EqualValues(t, 9, parentID)
	require.NoError(t, db.QueryRowContext(ctx, "SELECT ID FROM "+parent).Scan(&parentID))
	require.EqualValues(t, 1, parentID)
	require.NoError(t, db.QueryRowContext(ctx, "SELECT PID FROM "+child).Scan(&parentID))
	require.EqualValues(t, 1, parentID)
	cp, ok, err := a.LoadCheckpoint(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, p1, cp)
}
