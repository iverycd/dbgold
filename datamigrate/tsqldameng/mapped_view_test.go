package tsqldameng

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMappedViews(t *testing.T) {
	catalog := map[string][]MappedColumn{
		"T": {{"RowId", "ROW_ID_2"}, {"row_id", "ROW_ID"}, {"Name", "NAME"}},
		"U": {{"RowId", "ROW_ID"}},
	}
	for _, server := range []bool{true, false} {
		raw := `SELECT t.RowId, t.row_id, N'ROWID ''test''' AS txt FROM dbo.T t WHERE t.RowId = 1`
		if server {
			raw = "CREATE VIEW dbo.V AS " + raw
		}
		got, err := MappedView(raw, "APP", "dbo", server, catalog)
		require.NoError(t, err)
		require.Contains(t, got.SQL, `"T"."ROW_ID_2" AS "ROW_ID_2"`)
		require.Contains(t, got.SQL, `"T"."ROW_ID" AS "ROW_ID"`)
		require.Contains(t, got.SQL, `'ROWID ''test'''`)
		require.Equal(t, []MappedColumn{{"RowId", "ROW_ID_2"}, {"row_id", "ROW_ID"}, {"txt", "TXT"}}, got.Columns)
		catalog["V"] = got.Columns
		child, err := MappedView(`SELECT v.RowId AS KeepName FROM dbo.V v`, "APP", "dbo", false, catalog)
		require.NoError(t, err)
		require.Contains(t, child.SQL, `"V"."ROW_ID_2" AS "KEEPNAME"`)
	}
	got, err := MappedView(`SELECT t.*, u.RowId AS OtherId FROM dbo.T t JOIN dbo.U u ON t.RowId=u.RowId`, "APP", "dbo", false, catalog)
	require.NoError(t, err)
	require.Len(t, got.Columns, 4)
	require.Contains(t, got.SQL, `"T"."ROW_ID_2" = "U"."ROW_ID"`)
	require.NotContains(t, got.SQL, "*")
	got, err = MappedView(`SELECT RowId AS ID, COUNT(*) AS n FROM dbo.T GROUP BY RowId HAVING COUNT(*) > 0`, "APP", "dbo", false, catalog)
	require.NoError(t, err)
	require.Contains(t, got.SQL, `GROUP BY "T"."ROW_ID_2"`)
	_, err = MappedView(`SELECT RowId FROM dbo.T t JOIN dbo.U u ON t.RowId=u.RowId`, "APP", "dbo", false, catalog)
	require.ErrorContains(t, err, "歧义")
	for _, raw := range []string{`SELECT TOP 1 RowId FROM dbo.T`, `SELECT t.RowId FROM other.T t`, `SELECT x.RowId FROM dbo.T t`, `SELECT * FROM dbo.T t, dbo.U u`, `SELECT RowId FROM dbo.Missing`, `SELECT RowId FROM dbo.T UNION SELECT RowId FROM dbo.U`} {
		_, err := MappedView(raw, "APP", "dbo", false, catalog)
		require.Error(t, err, raw)
	}
}
