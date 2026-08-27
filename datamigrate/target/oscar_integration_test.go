package target

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/internal/jdbcbridge"
)

// This is the release-gating Oscar capability probe. It is intentionally all
// or nothing: an unsupported required capability fails the test instead of
// silently reducing migration coverage.
func TestOscarIntegrationCapabilityProbe(t *testing.T) {
	url := os.Getenv("OSCAR_TEST_URL")
	username := os.Getenv("OSCAR_TEST_USER")
	password := os.Getenv("OSCAR_TEST_PASSWORD")
	schema := os.Getenv("OSCAR_TEST_SCHEMA")
	if url == "" || username == "" || password == "" || schema == "" {
		t.Skip("OSCAR_TEST_URL/USER/PASSWORD/SCHEMA are not all set")
	}
	root := filepath.Clean(filepath.Join("..", ".."))
	bridgeJar := os.Getenv("OSCAR_BRIDGE_JAR")
	if bridgeJar == "" {
		bridgeJar = filepath.Join(root, "jdbcbridge", "java", "build", "dbgold-oscar-bridge.jar")
	}
	jdbcJar := os.Getenv("OSCAR_JDBC_JAR")
	if jdbcJar == "" {
		jdbcJar = filepath.Join(root, "third_party", "oscar", "oscarJDBC8.jar")
	}
	manager := jdbcbridge.NewManager(jdbcbridge.Config{BridgeJar: bridgeJar, JDBCJar: jdbcJar})
	w, err := newOscarWithBridge(manager, url, username, password, schema, ConnPoolConfig{MaxOpenConns: 4, MaxIdleConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	defer w.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	parent := "DBGOLD_PROBE_PARENT_" + suffix
	child := "DBGOLD_PROBE_CHILD_" + suffix
	view := "DBGOLD_PROBE_VIEW_" + suffix
	sequence := dialect.SequenceName(dialect.NewOscar(), source.SequenceInfo{TableName: parent, ColumnName: "ID"})
	cleanup := func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		for _, sql := range []string{
			"DROP VIEW IF EXISTS " + w.dia.QualifyTable(schema, view),
			"DROP TABLE IF EXISTS " + w.dia.QualifyTable(schema, child) + " CASCADE",
			"DROP TABLE IF EXISTS " + w.dia.QualifyTable(schema, parent) + " CASCADE",
			"DROP SEQUENCE IF EXISTS " + w.dia.QualifyTable(schema, sequence),
		} {
			_ = manager.Exec(cleanupCtx, w.session, sql)
		}
	}
	cleanup()
	defer cleanup()

	info := &source.TableDDLInfo{TableName: parent, Columns: []source.ColumnInfo{
		{Name: "ID", DataType: "bigint", IsNullable: false, Extra: "auto_increment"},
		{Name: "NAME", DataType: "varchar", Length: 128, IsNullable: true},
		{Name: "BODY", DataType: "longtext", IsNullable: true},
		{Name: "PAYLOAD", DataType: "blob", IsNullable: true},
		{Name: "RAW_VALUE", DataType: "varbinary", Length: 32, IsNullable: true},
		{Name: "FLAG", DataType: "boolean", IsNullable: true},
		{Name: "AMOUNT", DataType: "decimal", Precision: 30, Scale: 8, IsNullable: true},
		{Name: "CREATED_AT", DataType: "datetime", IsNullable: true},
	}}
	statements, err := w.dia.CreateTableStatements(schema, info, "mysql", dialect.TypeOpt{}, func(v string) string { return v })
	if err != nil {
		t.Fatal(err)
	}
	if err := w.CreateTable(ctx, statements); err != nil {
		t.Fatalf("Oscar CREATE TABLE/type probe: %v", err)
	}
	childDDL := []dialect.Statement{{SQL: fmt.Sprintf("CREATE TABLE %s (%s BIGINT NOT NULL, %s BIGINT)",
		w.dia.QualifyTable(schema, child), w.dia.QuoteIdent("ID"), w.dia.QuoteIdent("PARENT_ID"))}}
	if err := w.CreateTable(ctx, childDDL); err != nil {
		t.Fatalf("Oscar child table probe: %v", err)
	}
	if err := w.CreateSequence(ctx, source.SequenceInfo{TableName: parent, ColumnName: "ID", StartValue: 1}); err != nil {
		t.Fatalf("Oscar sequence/nextval/default probe: %v", err)
	}
	if err := w.CreateIndex(ctx, source.IndexInfo{TableName: parent, IsPrimary: true, Columns: []string{"ID"}}); err != nil {
		t.Fatalf("Oscar primary key probe: %v", err)
	}
	if err := w.CreateIndex(ctx, source.IndexInfo{TableName: parent, IndexName: "DBGOLD_PROBE_UQ_" + suffix, IsUnique: true, Columns: []string{"NAME"}}); err != nil {
		t.Fatalf("Oscar unique index probe: %v", err)
	}
	if err := w.CreateIndex(ctx, source.IndexInfo{TableName: child, IsPrimary: true, Columns: []string{"ID"}}); err != nil {
		t.Fatalf("Oscar child primary key probe: %v", err)
	}
	// Both source tables may legitimately use the same local index name.
	// The target relation names must differ by their table prefix.
	for _, table := range []string{parent, child} {
		if err := w.CreateIndex(ctx, source.IndexInfo{TableName: table, IndexName: "IDX_SHARED", Columns: []string{"ID"}}); err != nil {
			t.Fatalf("Oscar same-name indexes on different tables probe: %v", err)
		}
	}
	if err := w.CreateForeignKey(ctx, source.FKInfo{TableName: child, ConstraintName: "DBGOLD_PROBE_FK_" + suffix, Columns: []string{"PARENT_ID"}, RefTable: parent, RefColumns: []string{"ID"}, OnDelete: "CASCADE", OnUpdate: "NO ACTION"}); err != nil {
		t.Fatalf("Oscar foreign key action probe: %v", err)
	}
	if err := w.CreateComment(ctx, source.CommentInfo{TableName: parent, Comment: "dbgold 表注释"}); err != nil {
		t.Fatalf("Oscar table comment probe: %v", err)
	}
	if err := w.CreateComment(ctx, source.CommentInfo{TableName: parent, ColumnName: "NAME", Comment: "dbgold 列注释"}); err != nil {
		t.Fatalf("Oscar column comment probe: %v", err)
	}
	viewDefinition := fmt.Sprintf("SELECT %s, %s FROM %s", w.dia.QuoteIdent("ID"), w.dia.QuoteIdent("NAME"), w.dia.QualifyTable(schema, parent))
	if err := w.CreateView(ctx, source.ViewInfo{ViewName: view, Definition: viewDefinition}); err != nil {
		t.Fatalf("Oscar CREATE OR REPLACE VIEW probe: %v", err)
	}
	w.SetSourceType("mysql")
	longText := strings.Repeat("神通数据库", 20000)
	if err := w.CopyData(ctx, parent,
		[]string{"ID", "NAME", "BODY", "PAYLOAD", "RAW_VALUE", "FLAG", "AMOUNT", "CREATED_AT"},
		[]string{"BIGINT", "VARCHAR", "LONGTEXT", "BLOB", "VARBINARY", "BOOLEAN", "DECIMAL", "DATETIME"},
		[][]interface{}{{int64(1), "中文Name", longText, []byte{0, 1, 2}, []byte{3, 4}, true, []byte("1234567890.12345678"), time.Now()}, {int64(2), nil, nil, nil, nil, false, nil, nil}},
	); err != nil {
		t.Fatalf("Oscar JDBC batch/NULL/LOB/Decimal/time probe: %v", err)
	}
	if count, err := w.CountRows(ctx, parent); err != nil || count != 2 {
		t.Fatalf("Oscar count probe: count=%d err=%v", count, err)
	}
	if err := w.ChangeOwner(ctx, "TABLE", parent, schema); err != nil {
		t.Fatalf("Oscar ALTER OWNER probe: %v", err)
	}
}
