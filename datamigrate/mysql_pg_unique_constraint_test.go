package datamigrate

import (
	"context"
	"testing"

	"dbgold/datamigrate/source"

	"github.com/stretchr/testify/require"
)

func TestMySQLConditionalConcatColumns(t *testing.T) {
	for _, tc := range []struct {
		name, expression, first, second string
	}{
		{"screenshot", "(case when (`OutCaseCode` <> _utf8mb4'') then concat(`Channel`,_utf8mb4':',`OutCaseCode`) end)", "Channel", "OutCaseCode"},
		{"catalog escape", "(case when (`OutCaseCode` <> _utf8mb4\\'\\') then concat(`Chan\\\\\\'nel`,_utf8mb4\\':\\',`OutCaseCode`) end)", "Chan\\'nel", "OutCaseCode"},
		{"catalog escaped newline", "CASE WHEN Code <> _utf8mb4\\'\\' THEN CONCAT(`Chan\\nnel`, _utf8mb4\\':\\', Code) END", "Chan\nnel", "Code"},
		{"explicit null and parentheses", "CASE WHEN ((Code)) != '' THEN (CONCAT((Channel), (':'), code)) ELSE (NULL) END", "Channel", "code"},
		{name: "missing condition", expression: "CONCAT(Channel, ':', Code)"},
		{name: "different condition", expression: "CASE WHEN Other <> '' THEN CONCAT(Channel, ':', Code) END"},
		{name: "different delimiter", expression: "CASE WHEN Code <> '' THEN CONCAT(Channel, '-', Code) END"},
		{name: "other expression", expression: "CASE WHEN Code <> '' THEN CONCAT(LOWER(Channel), ':', Code) END"},
		{name: "other branch", expression: "CASE WHEN Code <> '' THEN CONCAT(Channel, ':', Code) ELSE 'x' END"},
		{name: "qualified column", expression: "CASE WHEN Code <> '' THEN CONCAT(other.Channel, ':', Code) END"},
		{name: "same column", expression: "CASE WHEN Code <> '' THEN CONCAT(Code, ':', Code) END"},
		{name: "trailing SQL", expression: "CASE WHEN Code <> '' THEN CONCAT(Channel, ':', Code) END) FROM other -- "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first, second, ok := mysqlConditionalConcatColumns(tc.expression)
			if tc.first == "" {
				require.False(t, ok)
				return
			}
			require.True(t, ok)
			require.Equal(t, tc.first, first)
			require.Equal(t, tc.second, second)
		})
	}
}

func TestMySQLPGUniqueConstraintUsesSourceColumns(t *testing.T) {
	r := newMySQLDMIndexReader()
	w := &mysqlDMIndexWriter{targetType: "postgres"}
	m, _ := newTestMigrator(r, w)
	idx := source.IndexInfo{TableName: "Orders", IndexName: "ConditionalKey", Method: "BTREE", IsUnique: true,
		MySQLExpression: "CASE WHEN code <> '' THEN CONCAT(row_id, ':', code) END", Unsupported: "manual"}
	ctx := context.Background()
	require.False(t, m.targetPostIndex(ctx, idx).PGUniqueConstraint, "numeric condition")
	r.ddl["Orders"].Columns[1].DataType = "varchar"

	for _, tc := range []struct {
		lowerCase bool
		table     string
		name      string
		columns   []string
	}{
		{false, "Orders", "ConditionalKey", []string{"ROW_ID", "Code"}},
		{true, "orders", "conditionalkey", []string{"row_id", "code"}},
	} {
		m.cfg.LowerCaseNames = tc.lowerCase
		got := m.targetPostIndex(ctx, idx)
		require.True(t, got.PGUniqueConstraint)
		require.Equal(t, tc.table, got.TableName)
		require.Equal(t, tc.name, got.IndexName)
		require.Equal(t, tc.columns, got.Columns)
		require.Empty(t, got.Unsupported)
	}
	idx.MySQLExpression = "CASE WHEN Missing <> '' THEN CONCAT(row_id, ':', Missing) END"
	require.Equal(t, "manual", m.targetPostIndex(ctx, idx).Unsupported)
	m.cfg.MigrationPurpose = "cdc"
	idx.MySQLExpression = "CASE WHEN code <> '' THEN CONCAT(row_id, ':', code) END"
	require.Equal(t, "manual", m.targetPostIndex(ctx, idx).Unsupported)
}
