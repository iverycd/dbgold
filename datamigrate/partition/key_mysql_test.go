package partition

import (
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func mysqlKeyFixture() (*source.TableDDLInfo, Profile, Options) {
	info := compilerLinearHashFixture(3)
	info.Partition.Method, info.Partition.Algorithm = "LINEAR KEY", "mysql-key-innodb-1:8.0.41"
	info.Columns = []source.ColumnInfo{{Name: "Id", DataType: "bigint", FormattedType: "bigint unsigned", IsNullable: true}, {Name: "MiXeD", DataType: "varchar", FormattedType: "varchar(40)", Length: 40, IsNullable: true}}
	info.Partition.Keys = []source.PartitionKey{{Column: "Id", DataType: "bigint unsigned"}, {Column: "MiXeD", DataType: "varchar(40)", Collation: "utf8mb4_bin"}}
	profile := compilerProfile("mysql")
	profile.Features["mysql_key_innodb"] = true
	return info, profile, Options{SourceType: "mysql", TargetType: "mysql", TargetSchema: "Dst"}
}

func TestMySQLNativeKeyRetainsStorageAndAlgorithm(t *testing.T) {
	info, profile, opt := mysqlKeyFixture()
	plan, err := Compile(info, compilerBase(t, info, "mysql"), profile, opt)
	require.NoError(t, err)
	ddl := dialect.JoinSQL(plan.Statements)
	require.Contains(t, ddl, "PARTITION BY LINEAR KEY ALGORITHM = 1 (`Id`,`MiXeD`)")
	require.Contains(t, ddl, "`Id` bigint unsigned")
	require.Contains(t, ddl, "`MiXeD` varchar(40) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin")
	require.Equal(t, info.Partition.Keys, plan.Target.Keys)
	require.Len(t, plan.Mapping, 3)
	actual := source.ClonePartitionSpec(plan.Target)
	actual.Keys[0].DataType = "bigint(20) unsigned"
	require.NoError(t, Equivalent(plan.Target, actual))
	actual.Keys[0].DataType = "bigint"
	require.Error(t, Equivalent(plan.Target, actual))
	actual = source.ClonePartitionSpec(plan.Target)
	actual.Keys[0], actual.Keys[1] = actual.Keys[1], actual.Keys[0]
	require.Error(t, Equivalent(plan.Target, actual), "physical field order is part of native hash identity")
	actual = source.ClonePartitionSpec(plan.Target)
	actual.Algorithm = "mysql-key-innodb-2:8.0.41"
	require.Error(t, Equivalent(plan.Target, actual))
}

func TestMySQLNativeKeyRejectsUnprovenTypesAlgorithmsAndVersions(t *testing.T) {
	for _, what := range []string{"unresolved", "version", "target product", "target vendor", "missing collation", "injection", "temporal", "generated", "mismatched storage"} {
		t.Run(what, func(t *testing.T) {
			info, p, opt := mysqlKeyFixture()
			switch what {
			case "unresolved":
				info.Partition.Algorithm = "mysql-key-unresolved"
			case "version":
				p.Version = "8.0.36"
			case "target product":
				p = compilerProfile("postgres")
				opt.TargetType = "postgres"
			case "target vendor":
				p.Features["mysql_key_innodb"] = false
			case "missing collation":
				info.Partition.Keys[1].Collation = ""
			case "injection":
				info.Partition.Keys[0].DataType = "bigint); DROP TABLE t; --"
			case "temporal":
				info.Partition.Keys[0].DataType = "datetime(6)"
				info.Columns[0].FormattedType = "datetime(6)"
			case "generated":
				info.Columns[0].Extra = "STORED GENERATED"
			case "mismatched storage":
				info.Columns[0].FormattedType = "bigint"
			}
			_, err := Compile(info, compilerBase(t, info, opt.TargetType), p, opt)
			require.Error(t, err)
		})
	}
}

func TestMySQLNativeKeyTypeGrammarAndDDLGuards(t *testing.T) {
	for _, raw := range []string{"tinyint(3) unsigned", "mediumint(9)", "decimal(65,30)", "bit(64)", "binary(16)", "varbinary(32)", "char(0)"} {
		_, err := canonicalMySQLKeyType(raw)
		require.NoError(t, err, raw)
	}
	for _, raw := range []string{"int ; SELECT 1", "decimal(66,2)", "decimal(10,31)", "bit(65)", "varchar(10) COLLATE utf8_bin", "varchar(-1)", "enum('a','b')", "float", "timestamp(6)"} {
		_, err := canonicalMySQLKeyType(raw)
		require.Error(t, err, raw)
	}
	info, p, opt := mysqlKeyFixture()
	opt.Name = func(s string) string { return s }
	opt.ColumnName = opt.Name
	c := compiler{info: info, profile: p, opt: opt}
	_, err := c.nativeMySQLKeyDDL("CREATE TABLE T (`Id` bigint, `MiXeD` varchar(40) CHARACTER SET ascii)", info.Partition)
	require.Error(t, err, "do not silently retain a conflicting target collation")
	_, err = c.nativeMySQLKeyDDL("CREATE TABLE T (`MiXeD` varchar(40), `Id` bigint)", info.Partition)
	require.ErrorContains(t, err, "物理列顺序", "KEY(B,A) does not override actual table field order")
	ddl, err := c.nativeMySQLKeyDDL("CREATE TABLE T (`Id` bigint DEFAULT 0, `MiXeD` varchar(40) NOT NULL DEFAULT 'a,b') ENGINE=InnoDB", info.Partition)
	require.NoError(t, err)
	require.Contains(t, ddl, "NOT NULL DEFAULT 'a,b'")
	require.False(t, strings.Contains(ddl, "mysql-key"))
}
