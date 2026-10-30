package partition

import (
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func integerExpressionCompiler() compiler {
	return compiler{info: &source.TableDDLInfo{Columns: []source.ColumnInfo{
		{Name: "MiXeD", DataType: "int"}, {Name: "b", DataType: "smallint"},
		{Name: "n", DataType: "int", IsNullable: true}, {Name: "wide", DataType: "bigint"},
		{Name: "u", DataType: "bigint", FormattedType: "bigint unsigned"},
	}}, profile: compilerProfile("postgres"), opt: Options{SourceType: "mysql", TargetType: "postgres", ColumnName: strings.ToLower}}
}

func TestIntegerPartitionExpressionFullDomainProof(t *testing.T) {
	c := integerExpressionCompiler()
	for _, tc := range []struct {
		raw, lo, hi string
		nullable    bool
	}{
		{"ABS(`MiXeD` * 3 + b) DIV 5", "0", "1288496742", false},
		{"MOD(`wide`,7)", "-6", "6", false},
		{"`wide` % -7", "-6", "6", false},
		{"MOD(10,3)", "0", "2", false},
		{"MOD(-10,3)", "-2", "0", false},
		{"FLOOR(ABS(n)) + CEILING(b)", "-32768", "2147516415", true},
		{"(`MiXeD` + b) * 2", "-4295032832", "4295032828", false},
		{"`MiXeD` DIV -5", "-429496729", "429496729", false},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			n, err := c.integerExpression(tc.raw)
			require.NoError(t, err)
			require.Equal(t, tc.lo, n.lo.String())
			require.Equal(t, tc.hi, n.hi.String())
			require.Equal(t, tc.nullable, n.nullable)
			require.NotContains(t, n.sql, "`MiXeD`")
			require.NotContains(t, n.sql, "/")
		})
	}
}

func TestIntegerPartitionExpressionRejectsUnprovenSemantics(t *testing.T) {
	c := integerExpressionCompiler()
	for _, raw := range []string{"ABS(wide)", "wide + 1", "-wide", "MiXeD * MiXeD * MiXeD", "MiXeD DIV b", "MiXeD MOD 0", "u + 1", "MiXeD / 2", "MiXeD + 1.5", "rand()", "schema.abs(MiXeD)", "ABS(MiXeD); DROP TABLE x", "missing + 1", "MiXeD COLLATE utf8_bin", "COALESCE(n,0)", "MiXeD + 9223372036854775808"} {
		t.Run(raw, func(t *testing.T) { _, err := c.integerExpression(raw); require.Error(t, err) })
	}
}

func TestComplexIntegerRangeAndHashCompileNative(t *testing.T) {
	for _, method := range []string{"RANGE", "HASH", "LINEAR HASH"} {
		t.Run(method, func(t *testing.T) {
			info := compilerRangeFixture()
			info.Partition.Method = method
			info.Partition.Keys = []source.PartitionKey{{Expression: "ABS(`Id` * 3 + 1) DIV 5"}}
			if method != "RANGE" {
				info.Partition.Algorithm = "mysql-mod"
				if method == "LINEAR HASH" {
					info.Partition.Algorithm = "mysql-linear-hash"
				}
				for i := range info.Partition.Partitions {
					p := &info.Partition.Partitions[i]
					p.Lower = nil
					p.Upper = nil
					p.HashModulus = 2
					p.HashRemainder = i
				}
			}
			plan, err := Compile(info, compilerBase(t, info, "postgres"), compilerProfile("postgres"), Options{SourceType: "mysql", TargetType: "postgres", TargetSchema: "Dst"})
			require.NoError(t, err)
			require.Contains(t, dialect.JoinSQL(plan.Statements), "pg_catalog.div(")
			require.Contains(t, dialect.JoinSQL(plan.Statements), "PARTITION OF")
			require.Len(t, plan.Mapping, 2)
			info.Columns[0].IsNullable = true
			_, err = Compile(info, compilerBase(t, info, "postgres"), compilerProfile("postgres"), Options{SourceType: "mysql", TargetType: "postgres", TargetSchema: "Dst"})
			if method == "RANGE" {
				require.ErrorContains(t, err, "NULL")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestIntegerExpressionCatalogCanonicalizationPreservesOperations(t *testing.T) {
	a := source.PartitionKey{Expression: `pg_catalog.div(pg_catalog.abs((CAST("MiXeD" AS numeric) * CAST('3' AS numeric)) + CAST('1' AS numeric)),CAST('5' AS numeric))`}
	b := source.PartitionKey{Expression: `div(abs((("MiXeD")::numeric * ('3'::numeric)) + '1'::numeric),'5'::numeric)`}
	require.Equal(t, CanonicalKey(a), CanonicalKey(b))
	b.Expression = strings.Replace(b.Expression, "*", "-", 1)
	require.NotEqual(t, CanonicalKey(a), CanonicalKey(b))
	b.Expression = strings.Replace(a.Expression, "numeric", "bigint", 1)
	require.NotEqual(t, CanonicalKey(a), CanonicalKey(b))
}
