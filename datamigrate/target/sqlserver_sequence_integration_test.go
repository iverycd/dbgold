package target

import (
	"context"
	"database/sql"
	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSQLServerNativeSequenceLiveRepair(t *testing.T) {
	dsn := os.Getenv("DBGOLD_SQLSERVER_TEST_DSN")
	if dsn == "" || os.Getenv("DBGOLD_SQLSERVER_TEST_ALLOW_DDL") != "1" {
		t.Skip("requires a disposable SQL Server DSN and DBGOLD_SQLSERVER_TEST_ALLOW_DDL=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	db, err := sql.Open("sqlserver", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	schema := fmt.Sprintf("DBGOLD_SEQ_%x", time.Now().UnixNano())
	d := dialect.NewSQLServer()
	qualify := func(name string) string { return d.QualifyTable(schema, name) }
	apply := func(stmt string) {
		t.Helper()
		_, err := db.ExecContext(ctx, stmt)
		require.NoError(t, err)
	}
	apply("CREATE SCHEMA " + d.QuoteIdent(schema))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, stmt := range []string{
			"IF OBJECT_ID(N'" + qualify("A") + "',N'U') IS NOT NULL DROP TABLE " + qualify("A"),
			"IF OBJECT_ID(N'" + qualify("B") + "',N'U') IS NOT NULL DROP TABLE " + qualify("B"),
			"IF OBJECT_ID(N'" + qualify("IdentityRows") + "',N'U') IS NOT NULL DROP TABLE " + qualify("IdentityRows"),
			"IF OBJECT_ID(N'" + qualify("Shared]") + "',N'SO') IS NOT NULL EXEC (" + dialect.SQLServerLiteral("DROP SEQUENCE "+qualify("Shared]")) + ")",
			"IF OBJECT_ID(N'" + qualify("Independent") + "',N'SO') IS NOT NULL EXEC (" + dialect.SQLServerLiteral("DROP SEQUENCE "+qualify("Independent")) + ")",
			"DROP SCHEMA " + d.QuoteIdent(schema),
		} {
			_, err := db.ExecContext(ctx, stmt)
			if err != nil {
				t.Errorf("clean SQL Server sequence fixture: %v", err)
			}
		}
	})
	w, err := NewSQLServer(dsn, schema, ConnPoolConfig{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	major, _, err := w.SQLServerVersion(ctx)
	require.NoError(t, err)
	if major < 11 {
		t.Skip("native sequences require SQL Server 2012+")
	}
	w.SetSourceType("postgres")
	for _, table := range []string{"A", "B"} {
		apply("CREATE TABLE " + qualify(table) + " ([Id] BIGINT NOT NULL)")
	}
	apply("INSERT INTO " + qualify("A") + " VALUES (13)")
	s := testSQLServerNativeSequence()
	s.Native.Bindings = []source.SequenceBinding{{TableName: "A", ColumnName: "Id", Kind: "default"}, {TableName: "B", ColumnName: "Id", Kind: "default"}}
	require.NoError(t, w.CreateSequence(ctx, s, ""))
	insertWithDefault := func(table string) int64 {
		t.Helper()
		var id int64
		require.NoError(t, db.QueryRowContext(ctx, "INSERT INTO "+qualify(table)+" OUTPUT inserted.[Id] DEFAULT VALUES").Scan(&id))
		return id
	}
	require.EqualValues(t, 14, insertWithDefault("A"))
	apply("INSERT INTO " + qualify("B") + " VALUES (40)")
	require.NoError(t, CalibrateRecovery(ctx, w, "", []source.SequenceInfo{s}))
	require.Greater(t, insertWithDefault("B"), int64(40))
	apply("INSERT INTO " + qualify("B") + " VALUES (80)")
	s.SQLServerCalibrationOnly = true
	repairSequence := func(s source.SequenceInfo) repair.Object {
		t.Helper()
		obj := repair.Object{Kind: "sequence", Schema: schema, TargetName: s.Native.Name, Sequence: &s}
		for i, stmt := range w.dia.SequenceStatements(schema, s, "") {
			obj.Units = append(obj.Units, repair.Unit{ID: strconv.Itoa(i), SQL: stmt.SQL, OriginalSQL: stmt.SQL, Status: "failed", Selected: true})
		}
		units, err := ExecuteRepair(ctx, w, obj, obj.Units, nil)
		require.NoError(t, err)
		for _, u := range units {
			require.Equal(t, "succeeded", u.Status)
		}
		return obj
	}
	obj := repairSequence(s)
	generated := insertWithDefault("A")
	require.Greater(t, generated, int64(80))
	require.Zero(t, generated%2)
	verified, err := InspectRepair(ctx, w, obj)
	require.NoError(t, err)
	require.Equal(t, "succeeded", verified.Status)

	independent := source.SequenceInfo{StartValueText: "31", Native: &source.NativeSequenceInfo{
		Name: "Independent", Schema: "Source", IncrementBy: "3", MinValue: "1", MaxValue: "1000", CacheSize: "5",
	}}
	_ = repairSequence(independent)
	var next int64
	require.NoError(t, db.QueryRowContext(ctx, "SELECT CONVERT(bigint,NEXT VALUE FOR "+qualify("Independent")+")").Scan(&next))
	require.EqualValues(t, 31, next)

	identityTable := &source.TableDDLInfo{TableName: "IdentityRows", Columns: []source.ColumnInfo{{Name: "Id", DataType: "int4", SQLServerIdentitySeed: "10", SQLServerIdentityIncrement: "1"}}}
	stmts, err := w.dia.CreateTableStatements(schema, identityTable, "postgres", dialect.TypeOpt{}, func(name string) string { return name })
	require.NoError(t, err)
	require.NoError(t, w.CreateTable(ctx, stmts))
	w.dia.Tables[identityTable.TableName] = identityTable
	require.NoError(t, w.CopyData(ctx, identityTable.TableName, []string{"Id"}, nil, [][]any{{33}}))
	identity := source.SequenceInfo{TableName: identityTable.TableName, ColumnName: "Id", StartValueText: "50"}
	require.NoError(t, w.CreateSequence(ctx, identity, ""))
	require.EqualValues(t, 50, insertWithDefault(identityTable.TableName))
	identity.StartValueText = "25"
	require.NoError(t, CalibrateRecovery(ctx, w, identityTable.TableName, []source.SequenceInfo{identity}))
	require.EqualValues(t, 51, insertWithDefault(identityTable.TableName))
}
