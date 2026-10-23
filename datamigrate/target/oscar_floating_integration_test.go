package target

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/validation"
	"dbgold/internal/jdbcbridge"
	"github.com/stretchr/testify/require"
)

// Release gate for REAL/DOUBLE PRECISION: only a configured Oscar instance can
// establish its actual storage, JDBC metadata and value roundtrip semantics.
// The only writes target a uniquely named probe table, removed on every exit.
func TestOscarIntegrationFloatingPrecision(t *testing.T) {
	url, username, password, schema := os.Getenv("OSCAR_TEST_URL"), os.Getenv("OSCAR_TEST_USER"), os.Getenv("OSCAR_TEST_PASSWORD"), os.Getenv("OSCAR_TEST_SCHEMA")
	if url == "" || username == "" || password == "" || schema == "" {
		t.Skip("Oscar floating capability NOT verified: OSCAR_TEST_URL/USER/PASSWORD/SCHEMA are not all set")
	}
	root := filepath.Clean(filepath.Join("..", ".."))
	bridgeJar, jdbcJar := os.Getenv("OSCAR_BRIDGE_JAR"), os.Getenv("OSCAR_JDBC_JAR")
	if bridgeJar == "" {
		bridgeJar = filepath.Join(root, "jdbcbridge", "java", "build", "dbgold-oscar-bridge.jar")
	}
	if jdbcJar == "" {
		jdbcJar = filepath.Join(root, "third_party", "oscar", "oscarJDBC8.jar")
	}
	manager := jdbcbridge.NewManager(jdbcbridge.Config{BridgeJar: bridgeJar, JDBCJar: jdbcJar})
	defer manager.Close()
	w, err := newOscarWithBridge(manager, url, username, password, schema, ConnPoolConfig{MaxOpenConns: 2, MaxIdleConns: 1})
	require.NoError(t, err)
	defer w.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// No schema objects are needed for the CAST capability check. 16777217
	// rounds in binary32; the second expression must retain its binary64 bit.
	castRows := readOscarFloatingRows(t, ctx, manager, w.session,
		"SELECT CAST(16777217 AS REAL), CAST(1.0000000000000002 AS DOUBLE PRECISION), CAST(NULL AS REAL), CAST(NULL AS DOUBLE PRECISION) FROM V_SYS_TYPE_INFO WHERE SQL_CONCISE_TYPE=7")
	require.NotEmpty(t, castRows, "Oscar must advertise JDBC REAL in V_SYS_TYPE_INFO")
	for _, row := range castRows {
		require.Equal(t, []any{float64(16777216), math.Nextafter(1, 2), nil, nil}, row)
	}

	table := fmt.Sprintf("DBGOLD_FLOAT_%d", time.Now().UnixNano())
	qualified := w.dia.QualifyTable(schema, table)
	info := &source.TableDDLInfo{TableName: table, Columns: []source.ColumnInfo{
		{Name: "ID", DataType: "int"},
		{Name: "SINGLE_VALUE", DataType: "float", FormattedType: "float", Precision: 12, IsNullable: true},
		{Name: "DOUBLE_VALUE", DataType: "double", FormattedType: "double", Precision: 22, IsNullable: true},
	}}
	statements, err := w.dia.CreateTableStatements(schema, info, "mysql", dialect.TypeOpt{}, func(v string) string { return v })
	require.NoError(t, err)
	require.Contains(t, dialect.JoinSQL(statements), `"SINGLE_VALUE" REAL`)
	require.Contains(t, dialect.JoinSQL(statements), `"DOUBLE_VALUE" DOUBLE PRECISION`)
	require.NoError(t, w.CreateTable(ctx, statements), "Oscar must accept the actual generated MySQL FLOAT/DOUBLE DDL")
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if err := manager.Exec(cleanupCtx, w.session, "DROP TABLE "+qualified); err != nil {
			t.Errorf("remove Oscar floating probe table: %v", err)
		}
	}()

	jdbcMeta, err := manager.DescribeTable(ctx, w.session, schema, table)
	require.NoError(t, err)
	require.Len(t, jdbcMeta.Columns, 3)
	for i, bits := range []int{32, 64} {
		column := jdbcMeta.Columns[i+1]
		require.Equal(t, info.Columns[i+1].Name, column.Name)
		// JDBC Types.FLOAT=6, REAL=7, DOUBLE=8. Decimal metadata is a
		// capability failure even if the same small test values survive.
		if bits == 32 {
			require.Equal(t, int32(7), column.SQLType, "actual JDBC REAL column: %+v", column)
		} else {
			require.Contains(t, []int32{6, 8}, column.SQLType, "actual JDBC DOUBLE column: %+v", column)
		}
		native := validation.Column{DatabaseType: column.TypeName, Precision: column.Precision, Scale: column.Scale}
		validation.ClassifyColumn("oscar", &native)
		require.Equal(t, validation.Float, native.Kind, "actual JDBC column: %+v", column)
		require.Equal(t, bits, native.FloatBits, "actual JDBC column: %+v", column)
		t.Logf("Oscar %s: JDBC type=%d name=%s precision=%d scale=%d binary bits=%d", column.Name, column.SQLType, column.TypeName, column.Precision, column.Scale, bits)
	}

	rows := [][]any{
		{int64(1), nil, nil},
		{int64(2), float64(0), float64(0)},
		{int64(3), float64(float32(-1.25)), -1.25},
		{int64(4), float64(float32(.1)), .1},
		{int64(5), float64(float32(1e30)), 1e300},
		{int64(6), float64(float32(1e-30)), 1e-300},
		{int64(7), float64(16777216), float64(16777217)},
		{int64(8), float64(math.Nextafter32(1, 2)), math.Nextafter(1, 2)},
		{int64(9), float64(math.SmallestNonzeroFloat32), float64(math.SmallestNonzeroFloat64)},
	}
	w.SetSourceType("mysql")
	require.NoError(t, w.CopyData(ctx, table, []string{"ID", "SINGLE_VALUE", "DOUBLE_VALUE"}, []string{"INT", "FLOAT", "DOUBLE"}, rows))
	actual := readOscarFloatingRows(t, ctx, manager, w.session, `SELECT "ID","SINGLE_VALUE","DOUBLE_VALUE" FROM `+qualified+` ORDER BY "ID"`)
	require.Equal(t, rows, actual, "binary precision, exponents, subnormals and NULLs must survive the JDBC batch and cursor")

	// Use the actual content validator as well as direct value comparison;
	// the reference represents already-decoded MySQL native float values.
	reference := &oscarFloatingReference{rows: rows, meta: validation.Table{
		Columns: []validation.Column{
			{Name: "ID", DatabaseType: "int", Kind: validation.Number},
			{Name: "SINGLE_VALUE", DatabaseType: "float", Kind: validation.Float, FloatBits: 32},
			{Name: "DOUBLE_VALUE", DatabaseType: "double", Kind: validation.Float, FloatBits: 64},
		},
		PrimaryKey: []string{"ID"},
	}}
	plan := validation.Plan{SourceTable: table, TargetTable: table}
	result := validation.Validate(ctx, reference, w, plan)
	require.Equal(t, validation.StatusPassed, result.Status, "%+v", result)
	// Introduce precisely the corruption a double → real mapping would cause.
	require.NoError(t, manager.Exec(ctx, w.session, `UPDATE `+qualified+` SET "DOUBLE_VALUE"=16777216 WHERE "ID"=7`))
	result = validation.Validate(ctx, reference, w, plan)
	require.Equal(t, validation.StatusMismatch, result.Status, "%+v", result)
	require.Equal(t, int64(1), result.ChangedRows, "%+v", result)
}

func readOscarFloatingRows(t *testing.T, ctx context.Context, manager *jdbcbridge.Manager, session uint64, query string) [][]any {
	t.Helper()
	cursor, err := manager.OpenCursor(ctx, session, query)
	require.NoError(t, err)
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, manager.CloseCursor(closeCtx, cursor))
	}()
	var rows [][]any
	for {
		page, done, err := manager.FetchCursor(ctx, cursor, 10, 1<<20)
		require.NoError(t, err)
		rows = append(rows, page...)
		if done {
			return rows
		}
		require.NotEmpty(t, page, "cursor must progress")
	}
}

type oscarFloatingReference struct {
	meta validation.Table
	rows [][]any
}

func (s *oscarFloatingReference) DescribeValidationTable(context.Context, string) (validation.Table, error) {
	return s.meta, nil
}

func (s *oscarFloatingReference) OpenValidationScan(_ context.Context, _ string, columns []validation.Column) (validation.RowStream, error) {
	positions := make([]int, len(columns))
	for i, column := range columns {
		found := false
		for j, actual := range s.meta.Columns {
			if column.Name == actual.Name {
				positions[i], found = j, true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("unknown reference column %s", column.Name)
		}
	}
	return &oscarFloatingStream{rows: s.rows, positions: positions}, nil
}

type oscarFloatingStream struct {
	rows      [][]any
	positions []int
	next      int
}

func (s *oscarFloatingStream) Next(ctx context.Context) ([]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.next == len(s.rows) {
		return nil, io.EOF
	}
	row := make([]any, len(s.positions))
	for i, position := range s.positions {
		row[i] = s.rows[s.next][position]
	}
	s.next++
	return row, nil
}

func (*oscarFloatingStream) Close() error { return nil }
