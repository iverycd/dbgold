package partition

import (
	"encoding/hex"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func mysqlKeyPGFixture(types ...string) (*source.TableDDLInfo, Profile, Options) {
	info := compilerLinearHashFixture(3)
	info.Partition.Method, info.Partition.Algorithm = "KEY", "mysql-key-innodb-2:8.0.36"
	info.Columns = nil
	info.Partition.Keys = nil
	for i, typ := range types {
		name := string(rune('A' + i))
		base := strings.Fields(typ)[0]
		info.Columns = append(info.Columns, source.ColumnInfo{Name: name, DataType: base, FormattedType: typ, IsNullable: true})
		info.Partition.Keys = append(info.Partition.Keys, source.PartitionKey{Column: name, DataType: typ})
	}
	return info, compilerProfile("postgres"), Options{SourceType: "mysql", TargetType: "postgres", TargetSchema: "Dst"}
}

func TestMySQLKeyPGCompilesProvenBytesAndNativeDomainGuard(t *testing.T) {
	for _, types := range [][]string{{"tinyint"}, {"smallint unsigned"}, {"mediumint"}, {"int"}, {"smallint", "smallint"}, {"tinyint unsigned", "mediumint unsigned"}} {
		info, p, opt := mysqlKeyPGFixture(types...)
		plan, err := Compile(info, compilerBase(t, info, "postgres"), p, opt)
		require.NoError(t, err, types)
		require.Equal(t, "LIST", plan.Target.Method)
		require.Len(t, plan.Target.Keys, 1)
		require.Len(t, plan.KeyColumns, len(types))
		for _, domain := range plan.KeyColumns {
			require.Equal(t, "integer", domain.DataType)
			require.True(t, domain.Nullable)
		}
		expr := plan.Target.Keys[0].Expression
		require.Contains(t, expr, "pg_catalog.int8xor(")
		require.Contains(t, expr, "pg_catalog.int8lt(")
		require.Contains(t, expr, "pg_catalog.get_byte(")
		require.Contains(t, expr, "CAST('-1' AS bigint)")
		require.Less(t, len(expr), 512*1024)
		ddl := dialect.JoinSQL(plan.Statements)
		require.NotContains(t, ddl, "CREATE FUNCTION")
		require.NotContains(t, ddl, "GENERATED")
		for _, part := range plan.Target.Partitions {
			for _, tuple := range part.Values {
				require.NotEqual(t, "-1", tuple[0].Text)
			}
		}
		require.Contains(t, plan.Rule, "mysql-innodb-key-bytes-to-native-list")
	}
}

func TestMySQLKeyPGRejectsBaseDDLTypeAndNullableDrift(t *testing.T) {
	info, p, opt := mysqlKeyPGFixture("smallint")
	for _, ddl := range []string{`CREATE TABLE "Dst"."Orders" ("A" numeric)`, `CREATE TABLE "Dst"."Orders" ("A" smallint)`, `CREATE TABLE "Dst"."Orders" ("A" bigint)`, `CREATE TABLE "Dst"."Orders" ("A" integer NOT NULL)`, `CREATE TABLE "Dst"."Orders" ("B" integer)`, `CREATE TABLE "Dst"."Orders" ("A" integer,"A" integer)`} {
		_, err := Compile(info, []dialect.Statement{{SQL: ddl}}, p, opt)
		require.Error(t, err, ddl)
	}
	for _, typ := range []string{"int", "integer", "int4"} {
		_, err := Compile(info, []dialect.Statement{{SQL: `CREATE TABLE "Dst"."Orders" ("A" ` + typ + ` DEFAULT (CASE WHEN 1 IS NOT NULL THEN 1 ELSE NULL END))`}}, p, opt)
		require.NoError(t, err, typ)
	}
	info.Columns[0].IsNullable = false
	_, err := Compile(info, []dialect.Statement{{SQL: `CREATE TABLE "Dst"."Orders" ("A" integer)`}}, p, opt)
	require.Error(t, err)
	_, err = Compile(info, []dialect.Statement{{SQL: `CREATE TABLE "Dst"."Orders" ("A" integer NOT NULL)`}}, p, opt)
	require.NoError(t, err)
}

func TestMySQLKeyPGLinearBucketsKeepMaskFoldIncludingNonPowerOfTwo(t *testing.T) {
	for _, n := range []int{1, 2, 3, 5, 7, 8, 15} {
		info, p, opt := mysqlKeyPGFixture("int")
		info.Partition.Method, info.Partition.Algorithm = "LINEAR KEY", "mysql-key-innodb-1:8.0.36"
		info.Partition.Partitions = compilerLinearHashFixture(n).Partition.Partitions
		plan, err := Compile(info, compilerBase(t, info, "postgres"), p, opt)
		require.NoError(t, err)
		modulus := 1
		for modulus < n {
			modulus <<= 1
		}
		seen := map[int]int{}
		for bucket, part := range plan.Target.Partitions {
			for _, tuple := range part.Values {
				var value int
				_, err := fmt.Sscan(tuple[0].Text, &value)
				require.NoError(t, err)
				seen[value] = bucket
			}
		}
		require.Len(t, seen, modulus)
		for residue := 0; residue < modulus; residue++ {
			want := residue
			if want >= n {
				want &= modulus/2 - 1
			}
			require.Equal(t, want, seen[residue])
		}
	}
}

func TestMySQLKeyPGLow32InvariantAndFixedLatin1Weights(t *testing.T) {
	weights, err := hex.DecodeString(mysqlKeyLatin1WeightsHex)
	require.NoError(t, err)
	require.Len(t, weights, 256)
	// Values that distinguish ALGORITHM 2 from binary: ASCII case folding,
	// accented Latin-1 letters, and Swedish ordering rather than Unicode folding.
	for _, v := range [][2]byte{{'a', 'A'}, {0xc0, 'A'}, {0xe4, 92}, {0xc5, 91}, {0xf6, 93}, {0xff, 255}} {
		require.Equal(t, v[1], weights[v[0]])
	}
	rng := rand.New(rand.NewSource(81236))
	for i := 0; i < 10000; i++ {
		h := rng.Uint64()
		sequence := uint64(rng.Intn(17))
		weight := uint64(rng.Intn(256))
		full := h ^ (((h&63)+sequence)*weight + (h << 8))
		low := uint64(uint32(h))
		low ^= ((low&63)+sequence)*weight + (low << 8)
		require.Equal(t, uint32(full), uint32(low))
		require.Equal(t, uint32(h^((h<<1)|1)), uint32(uint64(uint32(h))^((uint64(uint32(h))<<1)|1)))
	}
}

func TestMySQLKeyPGBlocksUnprovedEvidenceAndExpansion(t *testing.T) {
	for _, what := range []string{"version", "vendor", "type", "unsigned int", "width", "physical order", "expression", "generated"} {
		t.Run(what, func(t *testing.T) {
			info, p, opt := mysqlKeyPGFixture("smallint", "smallint")
			switch what {
			case "version":
				info.Partition.Algorithm = "mysql-key-innodb-2:8.0.41"
			case "vendor":
				info.Partition.Algorithm = "mysql-key-ndb-2:8.0.36"
			case "type":
				info.Columns[0].FormattedType = "decimal(4,0)"
				info.Partition.Keys[0].DataType = "decimal(4,0)"
			case "unsigned int":
				info, p, opt = mysqlKeyPGFixture("int unsigned")
			case "width":
				info, p, opt = mysqlKeyPGFixture("smallint", "mediumint")
			case "physical order":
				info.Partition.Keys[0], info.Partition.Keys[1] = info.Partition.Keys[1], info.Partition.Keys[0]
			case "expression":
				info.Partition.Keys[0].Expression = "A+1"
			case "generated":
				info.Columns[0].Generated = "A+1"
			}
			_, err := Compile(info, compilerBase(t, info, "postgres"), p, opt)
			require.Error(t, err)
		})
	}
}
