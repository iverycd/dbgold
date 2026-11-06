package target

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/internal/jdbcbridge"
	"github.com/stretchr/testify/require"
)

// Exercise the actual writer and vendor JDBC streams. Readback uses JDBC
// directly because the validation cursor deliberately limits each result row.
func TestOscarIntegrationLargeLOBAndDefaults(t *testing.T) {
	url, user, password, schema := os.Getenv("OSCAR_TEST_URL"), os.Getenv("OSCAR_TEST_USER"), os.Getenv("OSCAR_TEST_PASSWORD"), os.Getenv("OSCAR_TEST_SCHEMA")
	if url == "" || user == "" || password == "" || schema == "" {
		t.Skip("Oscar LOB/default roundtrip NOT verified: OSCAR_TEST_URL/USER/PASSWORD/SCHEMA are not all set")
	}
	java, err := exec.LookPath("java")
	require.NoError(t, err)
	javac, err := exec.LookPath("javac")
	require.NoError(t, err)
	root := filepath.Join("..", "..")
	bridgeJar, jdbcJar := os.Getenv("OSCAR_BRIDGE_JAR"), os.Getenv("OSCAR_JDBC_JAR")
	if bridgeJar == "" {
		bridgeJar = filepath.Join(root, "jdbcbridge", "java", "build", "dbgold-oscar-bridge.jar")
	}
	if jdbcJar == "" {
		jdbcJar = filepath.Join(root, "third_party", "oscar", "oscarJDBC8.jar")
	}
	manager := jdbcbridge.NewManager(jdbcbridge.Config{BridgeJar: bridgeJar, JDBCJar: jdbcJar, TempDir: t.TempDir()})
	defer manager.Close()
	w, err := newOscarWithBridge(manager, url, user, password, schema, ConnPoolConfig{MaxOpenConns: 2, MaxIdleConns: 1})
	require.NoError(t, err)
	defer w.Close()
	w.SetSourceType("mysql")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	table := fmt.Sprintf("DBGOLD_LOB_%d", time.Now().UnixNano())
	qualified := w.dia.QualifyTable(schema, table)
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		require.NoError(t, manager.Exec(cleanupCtx, w.session, "DROP TABLE IF EXISTS "+qualified+" CASCADE"))
	}()
	smallDefault, wideDefault := "b'1010'", "b'"+strings.Repeat("1", 64)+"'"
	info := &source.TableDDLInfo{TableName: table, Columns: []source.ColumnInfo{
		{Name: "ID", DataType: "int"},
		{Name: "FLAG", DataType: "bit", FormattedType: "bit(4)", Precision: 4, Default: &smallDefault, IsNullable: true},
		{Name: "BITS64", DataType: "bit", FormattedType: "bit(64)", Precision: 64, Default: &wideDefault, IsNullable: true},
		{Name: "WARNINGMSG", DataType: "varchar", Length: 10000, IsNullable: true},
		{Name: "BODY", DataType: "longtext", IsNullable: true},
		{Name: "PAYLOAD", DataType: "longblob", IsNullable: true},
	}}
	stmts, err := w.dia.CreateTableStatements(schema, info, "mysql", dialect.TypeOpt{UseNvarchar2: true}, func(s string) string { return s })
	require.NoError(t, err)
	require.NoError(t, w.CreateTable(ctx, stmts))
	text := strings.Repeat("中文🙂a", 91280898/len("中文🙂a"))
	binary := make([]byte, 91280898)
	for i := range binary {
		binary[i] = byte(i % 251)
	}
	warning := strings.Repeat("迁移", 5000)
	require.NoError(t, w.CopyData(ctx, table,
		[]string{"ID", "FLAG", "BITS64", "WARNINGMSG", "BODY", "PAYLOAD"},
		[]string{"INT", "BIT", "BIT", "VARCHAR", "LONGTEXT", "LONGBLOB"},
		[][]any{{int64(1), []byte{1}, []byte{255, 255, 255, 255, 255, 255, 255, 255}, warning, text, binary}},
	))
	require.NoError(t, manager.Exec(ctx, w.session, "INSERT INTO "+qualified+` ("ID") VALUES (2)`))
	classes := t.TempDir()
	out, err := exec.CommandContext(ctx, javac, "--release", "8", "-encoding", "UTF-8", "-d", classes, filepath.Join("testdata", "OscarLOBRoundTripProbe.java")).CombinedOutput()
	require.NoError(t, err, "%s", out)
	command := exec.CommandContext(ctx, java, "-Xmx512m", "-cp", classes+string(os.PathListSeparator)+jdbcJar, "OscarLOBRoundTripProbe", qualified,
		fmt.Sprintf("%x", sha256.Sum256([]byte(warning))), fmt.Sprintf("%x", sha256.Sum256([]byte(text))), fmt.Sprintf("%x", sha256.Sum256(binary)))
	out, err = command.CombinedOutput()
	require.NoError(t, err, "%s", out)
}
