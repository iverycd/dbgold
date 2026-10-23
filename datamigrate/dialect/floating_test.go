package dialect

import (
	"strings"
	"testing"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/typemap"
	"github.com/stretchr/testify/require"
)

// Exercise the DDL entry points, including the separately planned PG → MySQL
// path, so precision metadata cannot silently turn binary floats into decimals.
func TestFloatingCreateTablePreservesBinaryWidth(t *testing.T) {
	sources := []struct {
		name, family string
		single, dual source.ColumnInfo
	}{
		{"mysql", "mysql", source.ColumnInfo{DataType: "FlOaT", Precision: 12, Scale: 0}, source.ColumnInfo{DataType: "DoUbLe", Precision: 22, Scale: 0}},
		{"mysql_real_alias", "mysql", source.ColumnInfo{DataType: "float"}, source.ColumnInfo{DataType: "real"}},
		{"mysql_double_precision_alias", "mysql", source.ColumnInfo{DataType: "float"}, source.ColumnInfo{DataType: "double precision"}},
		{"sqlserver_real_float", "sqlserver", source.ColumnInfo{DataType: "REAL", Precision: 24}, source.ColumnInfo{DataType: "FLOAT", Precision: 53}},
		{"sqlserver_boundary", "sqlserver", source.ColumnInfo{DataType: "float", Precision: 24}, source.ColumnInfo{DataType: "float", Precision: 25}},
		{"sqlserver_default", "sqlserver", source.ColumnInfo{DataType: "real"}, source.ColumnInfo{DataType: "float"}},
		{"oracle_binary", "oracle", source.ColumnInfo{DataType: "BINARY_FLOAT"}, source.ColumnInfo{DataType: "BINARY_DOUBLE"}},
		{"dameng_binary", "dameng", source.ColumnInfo{DataType: "FLOAT", Precision: 24, DamengFloatStorage: "real"}, source.ColumnInfo{DataType: "DOUBLE", Precision: 53, DamengFloatStorage: "double"}},
		{"postgres", "postgres", source.ColumnInfo{DataType: "float4"}, source.ColumnInfo{DataType: "float8"}},
		{"postgres_aliases", "postgres", source.ColumnInfo{DataType: "real"}, source.ColumnInfo{DataType: "double precision"}},
	}
	targets := []struct {
		d            Dialect
		single, dual string
	}{
		{NewMySQL(), "FLOAT", "DOUBLE"},
		{NewSQLServer(), "REAL", "FLOAT(53)"},
		{NewOracle(), "BINARY_FLOAT", "BINARY_DOUBLE"},
		{NewDaMeng(), "REAL", "DOUBLE"},
		{NewOscar(), "REAL", "DOUBLE PRECISION"},
	}
	for _, family := range []string{"postgres", "gaussdb", "seabox", "highgo", "vastbase", "gbase", "kingbase"} {
		targets = append(targets, struct {
			d            Dialect
			single, dual string
		}{NewPostgres(family), "REAL", "DOUBLE PRECISION"})
	}
	for _, src := range sources {
		for _, dst := range targets {
			// Enumerate supported paths independently of the registry; a missing
			// registration must fail rather than silently omit its DDL checks.
			if src.family == "postgres" && dst.d.Name() != "mysql" && dst.d.Name() != "dameng" {
				continue
			}
			if src.family != "mysql" && (dst.d.Name() == "oracle" || dst.d.Name() == "sqlserver") {
				continue
			}
			if src.family == "dameng" && dst.d.Name() == "dameng" { // Native schema replay does not use mapped DDL.
				continue
			}
			t.Run(src.name+"_to_"+dst.d.Name(), func(t *testing.T) {
				_, registered := typemap.Get(src.family, dst.d.Name())
				require.True(t, registered, "supported migration pair must have a mapper")
				single, dual := src.single, src.dual
				single.Name, dual.Name = "SINGLE_VALUE", "DOUBLE_VALUE"
				info := &source.TableDDLInfo{TableName: "FLOAT_PROBE", Columns: []source.ColumnInfo{single, dual}}
				var statements []Statement
				var err error
				if src.family == "postgres" && dst.d.Name() == "mysql" {
					version, e := ParseMySQLVersion("8.0.36")
					require.NoError(t, e)
					statements, _, err = NewMySQL().PGTableStatements("DST", source.PGTable{Info: *info}, version, func(v string) string { return v })
				} else {
					statements, err = dst.d.CreateTableStatements("DST", info, src.family, TypeOpt{}, func(v string) string { return v })
				}
				require.NoError(t, err)
				ddl := strings.ToUpper(JoinSQL(statements))
				require.Contains(t, ddl, dst.d.QuoteIdent(single.Name)+" "+dst.single+" NOT NULL")
				require.Contains(t, ddl, dst.d.QuoteIdent(dual.Name)+" "+dst.dual+" NOT NULL")
				require.NotContains(t, ddl, "DECIMAL")
				require.NotContains(t, ddl, "NUMERIC")
			})
		}
	}
}

func TestMySQLFloatingDDLOnlyRetainsDeclaredDisplayPrecision(t *testing.T) {
	info := &source.TableDDLInfo{TableName: "FLOAT_PROBE", Columns: []source.ColumnInfo{
		{Name: "PLAIN_DOUBLE", DataType: "double", FormattedType: "double", Precision: 22, Scale: 0},
		{Name: "PLAIN_FLOAT", DataType: "float", FormattedType: "float", Precision: 12, Scale: 0},
		{Name: "SCALED_DOUBLE", DataType: "double", FormattedType: "double(12,5)", Precision: 12, Scale: 5},
		{Name: "SCALED_FLOAT", DataType: "float", FormattedType: "float(7,3)", Precision: 7, Scale: 3},
	}}
	statements, err := NewMySQL().CreateTableStatements("DST", info, "mysql", TypeOpt{}, func(v string) string { return v })
	require.NoError(t, err)
	ddl := strings.ToUpper(JoinSQL(statements))
	for _, column := range []string{"`PLAIN_DOUBLE` DOUBLE NOT NULL", "`PLAIN_FLOAT` FLOAT NOT NULL", "`SCALED_DOUBLE` DOUBLE(12,5) NOT NULL", "`SCALED_FLOAT` FLOAT(7,3) NOT NULL"} {
		require.Contains(t, ddl, column)
	}
	statements, err = NewOscar().CreateTableStatements("DST", info, "mysql", TypeOpt{}, func(v string) string { return v })
	require.NoError(t, err)
	ddl = JoinSQL(statements)
	require.Contains(t, ddl, `"SCALED_DOUBLE" DOUBLE PRECISION NOT NULL`)
	require.Contains(t, ddl, `"SCALED_FLOAT" REAL NOT NULL`)
}
