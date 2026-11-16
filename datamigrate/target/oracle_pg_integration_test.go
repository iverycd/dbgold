package target

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

// Run with a dedicated Oracle 11g/19c test user, never an application schema:
// DBGOLD_ORACLE_TEST_DSN, DBGOLD_ORACLE_TEST_SCHEMA (prefix DBGOLD_), and
// DBGOLD_ORACLE_TEST_ALLOW_DDL=1. Only a unique fixture table is created/dropped.
// This validates actual go-ora bindings; it is not source-vendor certification.
func TestPGOracleWriterIntegration(t *testing.T) {
	dsn, schema := os.Getenv("DBGOLD_ORACLE_TEST_DSN"), os.Getenv("DBGOLD_ORACLE_TEST_SCHEMA")
	if dsn == "" || schema == "" || os.Getenv("DBGOLD_ORACLE_TEST_ALLOW_DDL") != "1" {
		t.Skip("requires dedicated DBGOLD_ORACLE_TEST_DSN/SCHEMA and DBGOLD_ORACLE_TEST_ALLOW_DDL=1")
	}
	require.True(t, strings.HasPrefix(schema, "DBGOLD_"), "integration schema must start with DBGOLD_")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	w, err := NewOracle(dsn, schema, ConnPoolConfig{})
	require.NoError(t, err)
	defer w.Close()
	table := fmt.Sprintf("PGORA_%X", time.Now().UnixNano())
	columns := []source.ColumnInfo{
		{Name: "ID", DataType: "int4"},
		{Name: "N", DataType: "numeric", IsNullable: true},
		{Name: "AT", DataType: "timestamptz", DateTimePrecision: 6, IsNullable: true},
		{Name: "BODY", DataType: "text", IsNullable: true},
		{Name: "PAYLOAD", DataType: "bytea", IsNullable: true},
		{Name: "LABEL", DataType: "varchar", Length: 50, IsNullable: true},
		{Name: "FIXED", DataType: "bpchar", Length: 3, IsNullable: true},
	}
	statements, err := w.dia.CreateTableStatements(schema, &source.TableDDLInfo{TableName: table, Columns: columns}, "postgres", dialect.TypeOpt{}, strings.ToUpper)
	require.NoError(t, err)
	qn := w.dia.QualifyTable(schema, table)
	defer func() {
		cleanup, c := context.WithTimeout(context.Background(), 15*time.Second)
		defer c()
		_, _ = w.db.ExecContext(cleanup, "DROP TABLE "+qn+" CASCADE CONSTRAINTS")
	}()
	require.NoError(t, w.CreateTable(ctx, statements))
	w.ConfigurePGOracle(map[string][]source.ColumnInfo{table: columns})
	names := []string{"ID", "N", "AT", "BODY", "PAYLOAD", "LABEL", "FIXED"}
	rows := [][]interface{}{
		{1, "1e-130", "2024-11-03 01:30:00.123456-04:00", "", []byte{}, "", "a  "},
		{2, "-1e-130", nil, "body", []byte{0, 255, 0}, "label", "b  "},
		{3, "12345678901234567890.123456789012345678", "2024-11-03 01:30:00.123456-05:00", nil, nil, nil, nil},
	}
	require.NoError(t, w.CopyData(ctx, table, names, nil, rows))
	var positive, negative, exact, empty, nul, tz, bytes, fixed int
	query := `SELECT
 (SELECT COUNT(*) FROM ` + qn + ` WHERE ID=1 AND N=1e-130),
 (SELECT COUNT(*) FROM ` + qn + ` WHERE ID=2 AND N=-1e-130),
 (SELECT COUNT(*) FROM ` + qn + ` WHERE ID=3 AND N=12345678901234567890.123456789012345678),
 (SELECT COUNT(*) FROM ` + qn + ` WHERE ID=1 AND BODY IS NOT NULL AND DBMS_LOB.GETLENGTH(BODY)=0 AND PAYLOAD IS NOT NULL AND DBMS_LOB.GETLENGTH(PAYLOAD)=0),
 (SELECT COUNT(*) FROM ` + qn + ` WHERE ID=3 AND BODY IS NULL AND PAYLOAD IS NULL),
 (SELECT COUNT(*) FROM ` + qn + ` WHERE ID=1 AND SYS_EXTRACT_UTC(AT)=TIMESTAMP '2024-11-03 05:30:00.123456'),
 (SELECT COUNT(*) FROM ` + qn + ` WHERE ID=2 AND RAWTOHEX(DBMS_LOB.SUBSTR(PAYLOAD,3,1))='00FF00'),
 (SELECT COUNT(*) FROM ` + qn + ` WHERE ID=1 AND LENGTH(FIXED)=3)
 FROM DUAL`
	require.NoError(t, w.db.QueryRowContext(ctx, query).Scan(&positive, &negative, &exact, &empty, &nul, &tz, &bytes, &fixed))
	require.Equal(t, []int{1, 1, 1, 1, 1, 1, 1, 1}, []int{positive, negative, exact, empty, nul, tz, bytes, fixed})
	require.Len(t, w.MigrationWarnings(), 1)
	require.Equal(t, int64(1), w.MigrationWarnings()[0].Count)
	// A later all-NULL TZ/LOB array must retain the same Oracle bind types.
	require.NoError(t, w.CopyData(ctx, table, names, nil, [][]interface{}{{4, nil, nil, nil, nil, nil, nil}, {5, nil, nil, nil, nil, nil, nil}}))
}
