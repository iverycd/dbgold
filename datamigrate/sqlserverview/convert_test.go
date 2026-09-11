package sqlserverview

import (
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func viewInfo() map[string]*source.TableDDLInfo {
	return map[string]*source.TableDDLInfo{"T": {TableName: "T", Columns: []source.ColumnInfo{{Name: "ID", DataType: "int"}, {Name: "Name", DataType: "varchar", Length: 30}}}}
}
func TestSQLServerViewSubset(t *testing.T) {
	for _, q := range []string{"SELECT ID, Name FROM Src.T", "SELECT t.ID, IFNULL(t.Name, 'x') AS N FROM Src.T t WHERE t.ID > 0", "SELECT CONCAT(Name, '中文', NULL) AS N FROM T", "SELECT ID + 2 AS N, ID / 2 AS Half FROM T", "SELECT Name, COUNT(*) AS C FROM T GROUP BY Name HAVING COUNT(*) > 1", "SELECT ID FROM T ORDER BY ID LIMIT 0,10", "SELECT ID FROM T UNION ALL SELECT ID FROM T", "SELECT CASE WHEN ID > 0 THEN 'yes' ELSE 'no' END AS V FROM T", "SELECT ID FROM T WHERE ID IN (SELECT ID FROM T)", "SELECT a.ID FROM T a LEFT JOIN T b ON a.ID=b.ID"} {
		t.Run(q, func(t *testing.T) {
			got, deps, e := Convert(q, "Src", "dbo", nil, strings.ToLower, viewInfo())
			require.NoError(t, e)
			require.NotEmpty(t, got)
			require.Equal(t, []string{"T"}, deps)
			require.NotContains(t, got, "FROM DUAL")
			require.NotContains(t, got, "NULLS LAST")
		})
	}
	q, eDeps, e := Convert("SELECT 'IFNULL(`Name`, 0)' AS X FROM Src.T ORDER BY ID LIMIT 2", "Src", "dbo", nil, strings.ToLower, viewInfo())
	require.NoError(t, e)
	require.Contains(t, q, "N'IFNULL(`Name`, 0)'")
	require.Contains(t, q, "TOP (2)")
	require.Equal(t, []string{"T"}, eDeps)
}
func TestSQLServerRejectsUnknownSemantics(t *testing.T) {
	for _, q := range []string{"SELECT ID FROM T ORDER BY ID", "SELECT ID FROM T LIMIT 1,10", "SELECT GROUP_CONCAT(Name) FROM T", "SELECT JSON_EXTRACT(Name, '$.a') FROM T", "SELECT ROW_NUMBER() OVER(ORDER BY ID) FROM T", "SELECT IFNULL(Name, 1) FROM T", "SELECT Name+1 FROM T", "SELECT ID FROM Other.T", "SELECT ID FROM T WHERE Name", "SELECT * FROM T NATURAL JOIN T b"} {
		t.Run(q, func(t *testing.T) {
			_, _, e := Convert(q, "Src", "dbo", nil, strings.ToLower, viewInfo())
			require.Error(t, e)
		})
	}
}

func TestViewResolvesOnlyReferencedTable(t *testing.T) {
	infos := viewInfo()
	infos["Unrelated"] = &source.TableDDLInfo{TableName: "Unrelated", Columns: []source.ColumnInfo{{Name: "ID", DataType: "int"}}}
	got, _, err := Convert("SELECT ID / 2 AS Half FROM T WHERE ID > 0", "Src", "dbo", nil, strings.ToLower, infos)
	require.NoError(t, err)
	require.Contains(t, got, "decimal")
}

func TestSQLServerDivisionPrecision(t *testing.T) {
	got, _, err := Convert("SELECT ID/2 AS Result FROM T", "Src", "dbo", nil, strings.ToLower, viewInfo(), 2)
	require.NoError(t, err)
	require.Contains(t, got, "decimal(12,2)")
}

func TestSQLServerHighDivisionPrecisionPreserved(t *testing.T) {
	got, _, err := Convert("SELECT 1/3 AS Result", "Src", "dbo", nil, strings.ToLower, nil, 10)
	require.NoError(t, err)
	require.Contains(t, got, "CAST(1 AS decimal(9,8))")
	require.Contains(t, got, "decimal(11,10)")
}

func TestSQLServerRejectsLossyUnion(t *testing.T) {
	_, _, err := Convert("SELECT 'x' AS V UNION SELECT 1", "Src", "dbo", nil, strings.ToLower, nil)
	require.Error(t, err)
	infos := map[string]*source.TableDDLInfo{"T": {TableName: "T", Columns: []source.ColumnInfo{{Name: "A", DataType: "decimal", Precision: 38, Scale: 30}, {Name: "B", DataType: "bigint"}}}}
	_, _, err = Convert("SELECT COALESCE(A,B) AS V FROM T", "Src", "dbo", nil, strings.ToLower, infos)
	require.ErrorContains(t, err, "38 位")
}
