package handler

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAnalyzeQuerySQLClassification(t *testing.T) {
	tests := []struct {
		name        string
		sql         string
		statement   string
		risk        string
		returnsRows bool
	}{
		{name: "select with comments", sql: "-- inspect\nSELECT * FROM Users;", statement: "select", risk: "readonly", returnsRows: true},
		{name: "read cte", sql: "WITH recent AS (SELECT id FROM orders) SELECT * FROM recent", statement: "select", risk: "readonly", returnsRows: true},
		{name: "write cte", sql: "WITH changed AS (UPDATE orders SET state='done' RETURNING id) SELECT * FROM changed", statement: "update", risk: "write", returnsRows: true},
		{name: "insert returning", sql: "INSERT INTO t(v) VALUES (1) RETURNING id", statement: "insert", risk: "write", returnsRows: true},
		{name: "ddl", sql: "CREATE TABLE audit(id bigint)", statement: "create", risk: "dangerous"},
		{name: "unknown is dangerous", sql: "VACUUM ANALYZE orders", statement: "vacuum", risk: "dangerous"},
		{name: "outfile is dangerous", sql: "SELECT * FROM users INTO OUTFILE '/tmp/u'", statement: "select", risk: "dangerous", returnsRows: true},
		{name: "quoted semicolon", sql: "SELECT 'a;b' AS value;", statement: "select", risk: "readonly", returnsRows: true},
		{name: "dollar quote", sql: "DO $$ BEGIN RAISE NOTICE 'a;b'; END $$;", statement: "do", risk: "dangerous"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			analysis, err := analyzeQuerySQL(test.sql)
			require.NoError(t, err)
			require.Equal(t, test.statement, analysis.StatementType)
			require.Equal(t, test.risk, analysis.RiskLevel)
			require.Equal(t, test.returnsRows, analysis.ReturnsRows)
		})
	}
}

func TestAnalyzeQuerySQLRejectsUnsupportedInput(t *testing.T) {
	_, err := analyzeQuerySQL("SELECT 1; DELETE FROM users")
	require.ErrorContains(t, err, "一个 SQL")

	_, err = analyzeQuerySQL("DELIMITER $$ CREATE PROCEDURE p() BEGIN SELECT 1; END $$")
	require.ErrorContains(t, err, "DELIMITER")

	_, err = analyzeQuerySQL("BEGIN")
	require.ErrorContains(t, err, "事务")

	_, err = analyzeQuerySQL("SELECT 'unterminated")
	require.ErrorContains(t, err, "引号未闭合")
}

func TestIsProductionEnvironment(t *testing.T) {
	require.True(t, isProductionEnvironment("生产"))
	require.True(t, isProductionEnvironment("PROD-CN"))
	require.True(t, isProductionEnvironment("production"))
	require.False(t, isProductionEnvironment("测试"))
}

func TestNormalizeQueryValue(t *testing.T) {
	require.Equal(t, "9007199254740992", normalizeQueryValue(int64(9007199254740992), "BIGINT"))
	require.Equal(t, "hello", normalizeQueryValue([]byte("hello"), "VARCHAR"))
	require.Equal(t, "base64:AP8=", normalizeQueryValue([]byte{0, 255}, "BYTEA"))
	require.Nil(t, normalizeQueryValue(nil, "VARCHAR"))
}
