package datamigrate

import (
	"encoding/json"
	"strings"
	"testing"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

func TestOracleMySQLIndexPlanPrefixes(t *testing.T) {
	str := func(name, typ string, maxBytes int) target.MySQLIndexColumn {
		return target.MySQLIndexColumn{Name: name, DataType: typ, MaxBytes: maxBytes}
	}
	for _, tc := range []struct {
		name     string
		columns  []target.MySQLIndexColumn
		limit    int
		prefixes []int
	}{
		{"768 boundary", []target.MySQLIndexColumn{str("URL", "varchar(768)", 4)}, 3072, nil},
		{"769 boundary", []target.MySQLIndexColumn{str("URL", "varchar(769)", 4)}, 3072, []int{768}},
		{"long Oracle string", []target.MySQLIndexColumn{str("URL", "varchar(2000)", 4)}, 3072, []int{768}},
		{"utf8mb3", []target.MySQLIndexColumn{str("URL", "varchar(2000)", 3)}, 3072, []int{1024}},
		{"eight kilobyte page", []target.MySQLIndexColumn{str("URL", "varchar(2000)", 4)}, 1536, []int{384}},
		{"four kilobyte page", []target.MySQLIndexColumn{str("URL", "varchar(2000)", 4)}, 768, []int{192}},
		{"legacy limit", []target.MySQLIndexColumn{str("URL", "varchar(2000)", 4)}, 767, []int{191}},
		{"two long strings", []target.MySQLIndexColumn{str("A", "varchar(2000)", 4), str("B", "varchar(2000)", 4)}, 3072, []int{384, 384}},
		{"short string redistribution", []target.MySQLIndexColumn{str("A", "varchar(20)", 4), str("B", "varchar(2000)", 4)}, 3072, []int{0, 748}},
		{"rounding in source order", []target.MySQLIndexColumn{str("A", "varchar(2000)", 4), str("B", "varchar(2000)", 4)}, 3076, []int{385, 384}},
		{"integer exact boundary", []target.MySQLIndexColumn{str("A", "varchar(767)", 4), str("B", "int", 0)}, 3072, nil},
		{"integer and long string", []target.MySQLIndexColumn{str("A", "varchar(2000)", 4), str("B", "int", 0)}, 3072, []int{767, 0}},
		{"decimal exact boundary", []target.MySQLIndexColumn{str("A", "varchar(766)", 4), str("B", "decimal(18,0)", 0)}, 3072, nil},
		{"decimal overflow", []target.MySQLIndexColumn{str("A", "varchar(767)", 4), str("B", "decimal(18,0)", 0)}, 3072, []int{766, 0}},
		{"datetime fraction", []target.MySQLIndexColumn{str("A", "varchar(2000)", 4), str("B", "datetime(6)", 0)}, 3072, []int{766, 0}},
		{"binary uses bytes", []target.MySQLIndexColumn{str("A", "varbinary(4000)", 0)}, 3072, []int{3072}},
		{"longtext mandatory prefix", []target.MySQLIndexColumn{str("A", "longtext", 4)}, 3072, []int{768}},
		{"longblob mandatory prefix", []target.MySQLIndexColumn{str("A", "longblob", 0)}, 3072, []int{3072}},
		{"tinytext bound", []target.MySQLIndexColumn{str("A", "tinytext", 4)}, 3072, []int{63}},
		{"bare target type uses catalog length", []target.MySQLIndexColumn{{Name: "A", DataType: "varchar", Length: 2000, MaxBytes: 4}}, 3072, []int{768}},
		{"different character widths preserve whole units", []target.MySQLIndexColumn{str("A", "varchar(2000)", 4), str("B", "varchar(2000)", 1)}, 9, []int{1, 5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idx := source.IndexInfo{TableName: "FRAME_MODULE", IndexName: "IX_FRAME_MODULE_URL"}
			for _, c := range tc.columns {
				idx.Columns = append(idx.Columns, c.Name)
			}
			got, warning, err := planOracleMySQLIndex(idx, tc.columns, tc.limit)
			require.NoError(t, err)
			require.Equal(t, tc.prefixes, got.PrefixLengths)
			if tc.prefixes == nil {
				require.Empty(t, warning)
			} else {
				require.Contains(t, warning, idx.IndexName)
				require.Contains(t, warning, "排序和覆盖查询")
				var bytes int64
				for i, c := range tc.columns {
					part, known, err := oracleMySQLIndexColumnPart(c)
					require.NoError(t, err)
					require.True(t, known)
					if part.units == 0 {
						bytes += part.fixedBytes
					} else if got.PrefixLengths[i] > 0 {
						bytes += int64(got.PrefixLengths[i]) * part.unitBytes
					} else {
						bytes += part.units * part.unitBytes
					}
				}
				require.LessOrEqual(t, bytes, int64(tc.limit))
			}
		})
	}
}

func TestOracleMySQLIndexPlanPreservesUniqueUnknownAndMetadata(t *testing.T) {
	column := target.MySQLIndexColumn{Name: "MixedCase", DataType: "varchar(2000)", MaxBytes: 4}
	for _, tc := range []struct {
		name string
		idx  source.IndexInfo
		col  target.MySQLIndexColumn
	}{
		{"unique", source.IndexInfo{IsUnique: true}, column},
		{"primary", source.IndexInfo{IsPrimary: true}, column},
		{"unsupported", source.IndexInfo{Unsupported: "expression index"}, column},
		{"unknown type", source.IndexInfo{}, target.MySQLIndexColumn{Name: "MixedCase", DataType: "geometry"}},
		{"unknown type syntax", source.IndexInfo{}, target.MySQLIndexColumn{Name: "MixedCase", DataType: "enum('large','small')"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.idx.IndexName, tc.idx.TableName = "MixedIndex", "MixedTable"
			tc.idx.Columns = []string{"MixedCase"}
			got, warning, err := planOracleMySQLIndex(tc.idx, []target.MySQLIndexColumn{tc.col}, 3072)
			require.NoError(t, err)
			require.Empty(t, warning)
			require.Equal(t, tc.idx, got)
		})
	}
	idx := source.IndexInfo{TableName: "MixedTable", IndexName: "MixedIndex", Columns: []string{"MixedCase"}, Descending: []bool{true}, Attnums: []int{3}}
	original, err := json.Marshal(idx)
	require.NoError(t, err)
	columns := []target.MySQLIndexColumn{column}
	got, warning, err := planOracleMySQLIndex(idx, columns, 3072)
	require.NoError(t, err)
	require.NotEmpty(t, warning)
	require.Equal(t, []int{768}, got.PrefixLengths)
	again, againWarning, err := planOracleMySQLIndex(got, columns, 3072)
	require.NoError(t, err)
	require.Empty(t, againWarning)
	require.Equal(t, got, again)
	got.Columns[0], got.Descending[0], got.Attnums[0] = "renamed", false, 5
	current, err := json.Marshal(idx)
	require.NoError(t, err)
	require.JSONEq(t, string(original), string(current))
	require.Equal(t, column, columns[0])
}

func TestOracleMySQLIndexPlanRejectsIncompleteMetadata(t *testing.T) {
	idx := source.IndexInfo{IndexName: "IDX", Columns: []string{"A"}}
	for _, tc := range []struct {
		name    string
		columns []target.MySQLIndexColumn
		limit   int
		message string
	}{
		{"missing column", nil, 3072, "缺少列 A"},
		{"missing charset width", []target.MySQLIndexColumn{{Name: "A", DataType: "varchar(2000)"}}, 3072, "目标字符集最大字节数缺失"},
		{"missing length", []target.MySQLIndexColumn{{Name: "A", DataType: "varchar", MaxBytes: 4}}, 3072, "长度无效"},
		{"invalid limit", []target.MySQLIndexColumn{{Name: "A", DataType: "int"}}, 0, "上限无效"},
		{"minimum character does not fit", []target.MySQLIndexColumn{{Name: "A", DataType: "varchar(2000)", MaxBytes: 4}}, 3, "最短前缀需要"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, warning, err := planOracleMySQLIndex(idx, tc.columns, tc.limit)
			require.ErrorContains(t, err, tc.message)
			require.Empty(t, warning)
			require.Equal(t, idx, got)
		})
	}
}

func TestOracleMySQLIndexFixedWidths(t *testing.T) {
	for typ, want := range map[string]int64{
		"tinyint(1)": 1, "smallint unsigned": 2, "mediumint": 3, "int(11) unsigned zerofill": 4, "bigint": 8,
		"float": 4, "float(24)": 4, "float(25)": 8, "float(10,2)": 4, "double": 8,
		"decimal(9,0)": 4, "decimal(10,0)": 5, "decimal(18,0)": 8, "decimal(18,9)": 8, "decimal(10,2)": 5, "decimal(65,30)": 30,
		"bit(17)": 3, "bit": 1, "year(4)": 1, "date": 3, "time": 3, "time(1)": 4, "datetime": 5, "datetime(6)": 8, "timestamp(3)": 6,
	} {
		t.Run(typ, func(t *testing.T) {
			part, known, err := oracleMySQLIndexColumnPart(target.MySQLIndexColumn{DataType: typ})
			require.NoError(t, err)
			require.True(t, known)
			require.Equal(t, want, part.fixedBytes)
		})
	}
	for _, typ := range []string{"decimal(66,0)", "decimal(5,6)", "bit(65)", "datetime(7)", "varchar(" + strings.Repeat("9", 30) + ")"} {
		t.Run(typ, func(t *testing.T) {
			_, known, err := oracleMySQLIndexColumnPart(target.MySQLIndexColumn{DataType: typ, MaxBytes: 4})
			require.True(t, known)
			require.Error(t, err)
		})
	}
}
