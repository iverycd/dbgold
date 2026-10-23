package typemap

import (
	"fmt"
	"strings"
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

var floatingPGTargets = []string{"postgres", "gaussdb", "seabox", "highgo", "vastbase", "gbase", "kingbase"}

type floatingTarget struct{ name, single, double string }

func floatingTargets(extra ...floatingTarget) []floatingTarget {
	result := make([]floatingTarget, 0, len(floatingPGTargets)+len(extra))
	for _, name := range floatingPGTargets {
		result = append(result, floatingTarget{name, "real", "double precision"})
	}
	return append(result, extra...)
}

func requireFloatingMapping(t *testing.T, src string, dst floatingTarget, col source.ColumnInfo, single bool) {
	t.Helper()
	mapper, ok := Get(src, dst.name)
	require.True(t, ok, "missing %s -> %s registration", src, dst.name)
	want := dst.double
	if single {
		want = dst.single
	}
	for _, options := range [][2]bool{{false, false}, {true, false}, {false, true}} {
		require.Equal(t, want, mapper(col, options[0], options[1]), "%s -> %s %+v", src, dst.name, col)
	}
}

func TestMySQLBinaryFloatsAcrossRegisteredTargets(t *testing.T) {
	for _, dst := range floatingTargets(
		floatingTarget{"mysql", "float", "double"},
		floatingTarget{"dameng", "REAL", "DOUBLE"},
		floatingTarget{"oscar", "REAL", "DOUBLE PRECISION"},
		floatingTarget{"oracle", "BINARY_FLOAT", "BINARY_DOUBLE"},
		floatingTarget{"sqlserver", "REAL", "FLOAT(53)"},
	) {
		for _, name := range []string{"float", "double", "double precision", "real"} {
			for _, size := range [][2]int64{{0, 0}, {8, 2}, {53, 0}, {255, 30}} {
				t.Run(fmt.Sprintf("%s/%s/%d,%d", dst.name, name, size[0], size[1]), func(t *testing.T) {
					col := source.ColumnInfo{DataType: " " + strings.ToUpper(name) + " ", Precision: size[0], Scale: size[1]}
					requireFloatingMapping(t, "mysql", dst, col, name == "float")
				})
			}
		}
	}
}

func TestSQLServerBinaryFloatsAcrossRegisteredTargets(t *testing.T) {
	for _, dst := range floatingTargets(
		floatingTarget{"mysql", "float", "double"},
		floatingTarget{"dameng", "REAL", "DOUBLE"},
		floatingTarget{"oscar", "REAL", "DOUBLE PRECISION"},
	) {
		for _, tc := range []struct {
			typeName  string
			precision int64
			single    bool
		}{
			{"REAL", 0, true}, {"FLOAT", -1, false}, {"FLOAT", 0, false},
			{"FLOAT", 1, true}, {"FLOAT", 24, true}, {"FLOAT", 25, false}, {"FLOAT", 53, false},
		} {
			t.Run(fmt.Sprintf("%s/%s/%d", dst.name, tc.typeName, tc.precision), func(t *testing.T) {
				requireFloatingMapping(t, "sqlserver", dst, source.ColumnInfo{DataType: tc.typeName, Precision: tc.precision}, tc.single)
			})
		}
	}
}

func TestOracleBinaryFloatsAcrossRegisteredTargets(t *testing.T) {
	for _, dst := range floatingTargets(
		floatingTarget{"mysql", "float", "double"},
		floatingTarget{"dameng", "REAL", "DOUBLE"},
		floatingTarget{"oscar", "REAL", "DOUBLE PRECISION"},
	) {
		for _, tc := range []struct {
			name   string
			single bool
		}{{"BINARY_FLOAT", true}, {"BINARY_DOUBLE", false}} {
			t.Run(dst.name+"/"+tc.name, func(t *testing.T) {
				requireFloatingMapping(t, "oracle", dst, source.ColumnInfo{DataType: tc.name}, tc.single)
			})
		}
	}
}

func TestDamengBinaryFloatsUseObservedStorage(t *testing.T) {
	for _, dst := range floatingTargets(
		floatingTarget{"mysql", "float", "double"},
		floatingTarget{"oscar", "REAL", "DOUBLE PRECISION"},
	) {
		for _, name := range []string{"REAL", "FLOAT", "DOUBLE", "DOUBLE PRECISION"} {
			for _, storage := range []string{"real", "double"} {
				t.Run(dst.name+"/"+name+"/"+storage, func(t *testing.T) {
					col := source.ColumnInfo{DataType: name, Precision: 53, Scale: -1, DamengFloatStorage: storage}
					requireFloatingMapping(t, "dameng", dst, col, storage == "real")
				})
			}
		}
	}
}

func TestDamengDecimalAndUnknownFloatStorageKeepExistingMappings(t *testing.T) {
	for _, dst := range floatingTargets(
		floatingTarget{"mysql", "float", "double"},
		floatingTarget{"oscar", "REAL", "DOUBLE PRECISION"},
	) {
		mapper, ok := Get("dameng", dst.name)
		require.True(t, ok)
		for _, storage := range []string{"", "decimal"} {
			for _, name := range []string{"float", "double", "double precision"} {
				for _, precision := range []int64{0, 53} {
					col := source.ColumnInfo{DataType: name, Precision: precision, Scale: 2, DamengFloatStorage: storage}
					want := "decimal"
					if precision > 0 {
						want = "decimal(53,2)"
					} else if dst.name == "mysql" {
						want = "double"
					}
					if dst.name == "oscar" {
						want = strings.ToUpper(want)
					}
					require.Equal(t, want, mapper(col, false, false), "%s %+v", dst.name, col)
				}
			}
			require.Equal(t, dst.single, mapper(source.ColumnInfo{DataType: "real", DamengFloatStorage: storage}, false, false))
		}
	}
}

func TestPGBinaryFloatMappings(t *testing.T) {
	for _, src := range floatingPGTargets {
		for _, tc := range []struct {
			name   string
			single bool
		}{{"float4", true}, {"real", true}, {"float8", false}, {"double precision", false}} {
			requireFloatingMapping(t, src, floatingTarget{"mysql", "FLOAT", "DOUBLE"}, source.ColumnInfo{DataType: tc.name}, tc.single)
			// The DM dialect normalizes every PG-family source to postgres.
			requireFloatingMapping(t, "postgres", floatingTarget{"dameng", "REAL", "DOUBLE"}, source.ColumnInfo{DataType: tc.name}, tc.single)
		}
	}
}

func TestOracleDecimalFloatAliasesKeepExistingMappings(t *testing.T) {
	for _, dst := range floatingTargets(
		floatingTarget{"mysql", "float", "double"},
		floatingTarget{"dameng", "REAL", "DOUBLE"},
		floatingTarget{"oscar", "REAL", "DOUBLE PRECISION"},
	) {
		mapper, ok := Get("oracle", dst.name)
		require.True(t, ok)
		for _, name := range []string{"FLOAT", "REAL", "DOUBLE PRECISION", "FLOAT(126)"} {
			want := "decimal"
			if name == "REAL" {
				want = "real"
			}
			switch dst.name {
			case "mysql":
				want = "double"
				if name == "REAL" {
					want = "float"
				}
			case "dameng":
				want = "FLOAT(126)"
			case "oscar":
				want = strings.ToUpper(want)
			}
			require.Equal(t, want, mapper(source.ColumnInfo{DataType: name, Precision: 126}, false, false), "%s/%s", dst.name, name)
		}
	}
}

func TestExactDecimalMappingsUnaffectedByFloatMetadata(t *testing.T) {
	for _, src := range []string{"mysql", "sqlserver", "oracle", "dameng"} {
		for _, dst := range floatingTargets(floatingTarget{"mysql", "", ""}, floatingTarget{"oscar", "", ""}) {
			mapper, ok := Get(src, dst.name)
			require.True(t, ok)
			for _, name := range []string{"decimal", "numeric"} {
				want := "decimal(18,4)"
				if dst.name == "oscar" {
					want = strings.ToUpper(want)
				}
				require.Equal(t, want, mapper(source.ColumnInfo{DataType: name, Precision: 18, Scale: 4, DamengFloatStorage: "double"}, false, false), "%s/%s/%s", src, dst.name, name)
			}
		}
	}
}

func TestMySQLSameDatabaseFloatingDeclarations(t *testing.T) {
	for _, tc := range []struct{ name, dataType, formatted, want string }{
		{"implicit float", "float", "float", "float"},
		{"implicit double", "double", "double", "double"},
		{"missing declaration", "double", "", "double"},
		{"explicit float size", "float", "FLOAT(8,2)", "float(8,2)"},
		{"explicit double zero scale", "double", "double(12,0)", "double(12,0)"},
		{"explicit modifiers", "double", " DOUBLE PRECISION ( 20 , 5 ) UNSIGNED ZEROFILL ", "double(20,5) unsigned zerofill"},
		{"float unsigned", "float", "float unsigned", "float unsigned"},
		{"float zerofill", "float", "float zerofill", "float zerofill"},
		{"binary precision float", "float", "float(24) unsigned", "float unsigned"},
		{"binary precision double", "double", "float(53) unsigned", "double unsigned"},
		{"real alias", "real", "real(12,2)", "double(12,2)"},
		{"real normalized by server", "float", "real(8,2)", "float(8,2)"},
		{"double precision alias", "double precision", "double precision", "double"},
		{"invalid scale", "float", "float(8,9)", "float"},
		{"invalid precision", "double", "double(256,2)", "double"},
		{"mismatched type", "double", "float(8,2) unsigned", "double"},
		{"malformed declaration", "double", "double; DROP TABLE t", "double"},
		{"duplicate modifier", "double", "double unsigned unsigned", "double"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			col := source.ColumnInfo{DataType: tc.dataType, FormattedType: tc.formatted, Precision: 53, Scale: 0}
			require.Equal(t, tc.want, MySQLToMySQL(col, false, false))
		})
	}
}
