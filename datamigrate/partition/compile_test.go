package partition

import (
	"encoding/json"
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func compilerProfile(product string) Profile {
	p := Profile{Product: product, Version: "16.6", Schema: "Dst", Features: map[string]bool{FeatureRange: true, FeatureList: true, FeatureHash: true, FeatureMultiRange: true, FeatureExpressions: true}}
	switch product {
	case "postgres":
		p.Features["postgres_declarative"], p.Features["row_movement"], p.Features[FeatureSubpartition] = true, true, true
		p.Features[FeatureDefaultRange], p.Features[FeatureDefaultList] = true, true
	case "mysql":
		p.Version = "8.0.41"
		p.Features["inline_mysql"], p.Features["row_movement"], p.Features[FeatureMultiList] = true, true, true
	case "oracle", "dameng":
		p.Version = "19.0"
		p.Features["inline_oracle"], p.Features["row_movement_clause"], p.Features[FeatureDefaultList] = true, true, true
	case "sqlserver":
		p.Version, p.Mode = "10.50.6000.34", "filegroup:DefaultGroup"
		p.Features["native_sqlserver"], p.Features["row_movement"] = true, true
	}
	return p
}

func compilerRangeFixture() *source.TableDDLInfo {
	return &source.TableDDLInfo{TableName: "Orders", Columns: []source.ColumnInfo{{Name: "Id", DataType: "int", IsNullable: false}}, Partition: &source.PartitionSpec{Method: "RANGE COLUMNS", RowMovement: "enabled", Keys: []source.PartitionKey{{Column: "Id", DataType: "int"}}, Partitions: []source.Partition{
		{Schema: "Src", Name: "P_Neg", Ordinal: 1, Lower: []source.PartitionValue{{Kind: "minvalue"}}, Upper: []source.PartitionValue{{Kind: "number", Text: "0"}}, LowerInclusive: true},
		{Schema: "Src", Name: "P_Rest", Ordinal: 2, Lower: []source.PartitionValue{{Kind: "number", Text: "0"}}, Upper: []source.PartitionValue{{Kind: "maxvalue"}}, LowerInclusive: true},
	}}}
}

func compilerListFixture() *source.TableDDLInfo {
	info := compilerRangeFixture()
	info.Partition.Method = "LIST"
	info.Partition.Partitions = []source.Partition{
		{Schema: "Src", Name: "P_Neg", Ordinal: 1, Values: [][]source.PartitionValue{{{Kind: "number", Text: "-1"}}, {{Kind: "number", Text: "0"}}, {{Kind: "null"}}}},
		{Schema: "Src", Name: "P_Rest", Ordinal: 2, Values: [][]source.PartitionValue{{{Kind: "number", Text: "10"}}}},
	}
	info.Columns[0].IsNullable = true
	return info
}

func compilerBase(t *testing.T, info *source.TableDDLInfo, product string) []dialect.Statement {
	t.Helper()
	var d dialect.Dialect
	switch product {
	case "postgres":
		d = dialect.NewPostgres("postgres")
	case "mysql":
		d = dialect.NewMySQL()
	case "oracle":
		d = dialect.NewOracle()
	case "dameng":
		d = dialect.NewDaMeng()
	case "sqlserver":
		d = dialect.NewSQLServer()
	default:
		t.Fatal(product)
	}
	stmts, err := d.CreateTableStatements("Dst", info, "mysql", dialect.TypeOpt{}, func(name string) string { return name })
	require.NoError(t, err)
	return stmts
}

func TestPartitionCompileRangeAndListProduceTargetNativeDDL(t *testing.T) {
	for _, product := range []string{"postgres", "dameng", "mysql", "oracle"} {
		for _, shape := range []string{"RANGE", "LIST"} {
			t.Run(product+"/"+shape, func(t *testing.T) {
				info := compilerRangeFixture()
				if shape == "LIST" {
					info = compilerListFixture()
				}
				base := compilerBase(t, info, product)
				beforeInfo, _ := json.Marshal(info)
				beforeBase, _ := json.Marshal(base)
				options := Options{SourceType: "mysql", TargetType: product, TargetSchema: "Dst"}
				plan, err := Compile(info, base, compilerProfile(product), options)
				require.NoError(t, err)
				require.NotEmpty(t, plan.Fingerprint)
				require.Len(t, plan.Mapping, 2)
				require.Equal(t, "P_Neg", plan.Mapping[0].SourceName)
				require.Equal(t, "P_Neg", plan.Mapping[0].TargetName)
				require.Zero(t, plan.Mapping[0].TargetOrdinal, "only targets without named partitions use ordinal mappings")
				ddl := dialect.JoinSQL(plan.Statements)
				if product == "postgres" {
					require.Contains(t, ddl, "PARTITION OF")
					if shape == "RANGE" {
						require.Contains(t, ddl, "FOR VALUES FROM (MINVALUE) TO (0)")
					} else {
						require.Contains(t, ddl, "FOR VALUES IN (-1,0,NULL)")
					}
				} else if product == "mysql" {
					require.Contains(t, ddl, "PARTITION BY "+shape+" COLUMNS")
					if shape == "LIST" {
						require.Contains(t, ddl, "VALUES IN (-1,0,NULL)")
					}
				} else {
					require.Contains(t, ddl, "PARTITION BY "+shape)
					require.Contains(t, ddl, "ENABLE ROW MOVEMENT")
					if shape == "LIST" {
						require.Contains(t, ddl, "VALUES (-1,0,NULL)")
					}
				}
				afterInfo, _ := json.Marshal(info)
				afterBase, _ := json.Marshal(base)
				require.JSONEq(t, string(beforeInfo), string(afterInfo), "target mapping must not mutate raw source topology")
				require.JSONEq(t, string(beforeBase), string(afterBase), "base DDL remains reusable")
				second, err := Compile(info, base, compilerProfile(product), options)
				require.NoError(t, err)
				require.Equal(t, plan.Fingerprint, second.Fingerprint)
			})
		}
	}
}

func TestPartitionCompileSQLServerPreservesNamesAsExplicitOrdinalMapping(t *testing.T) {
	info := compilerRangeFixture()
	plan, err := Compile(info, compilerBase(t, info, "sqlserver"), compilerProfile("sqlserver"), Options{SourceType: "mysql", TargetType: "sqlserver", TargetSchema: "Dst"})
	require.NoError(t, err)
	require.Equal(t, "native-range-ordinal-map", plan.Rule)
	require.Equal(t, "P_Neg", plan.Mapping[0].SourceName)
	require.Equal(t, "1", plan.Mapping[0].TargetName)
	require.Equal(t, 1, plan.Mapping[0].TargetOrdinal)
	require.Equal(t, 2, plan.Mapping[1].TargetOrdinal)
	require.Equal(t, "Dst", plan.Mapping[0].TargetSchema, "numbered partitions still belong to the exact target root schema")
	ddl := dialect.JoinSQL(plan.Statements)
	require.Contains(t, ddl, "CREATE PARTITION FUNCTION [Dst_Orders__partition_function]")
	require.Contains(t, ddl, "AS RANGE RIGHT FOR VALUES (0)")
	require.Contains(t, ddl, "CREATE PARTITION SCHEME [Dst_Orders__partition_scheme]")
	require.Contains(t, ddl, "ALL TO ([DefaultGroup])")
	require.Contains(t, ddl, "ON [Dst_Orders__partition_scheme] ([Id])")
	require.Equal(t, "sqlserver-range:Dst_Orders__partition_function", plan.Target.Algorithm)
}

func TestPartitionCompileSQLServerMatchesNativeNonDefaultSchemaCatalog(t *testing.T) {
	info := compilerRangeFixture()
	profile := compilerProfile("sqlserver")
	profile.Schema = "MiXeDPartitionOwner"
	plan, err := Compile(info, compilerBase(t, info, "sqlserver"), profile, Options{SourceType: "mysql", TargetType: "sqlserver", TargetSchema: profile.Schema})
	require.NoError(t, err)
	// This is the SQL Server native RANGE RIGHT catalog shape: ordinal names
	// do not remove schema identity. Column types are independently verified.
	actual := &source.PartitionSpec{Method: "RANGE", RowMovement: "enabled", Algorithm: "sqlserver-range:MiXeDPartitionOwner_Orders__partition_function", Keys: []source.PartitionKey{{Column: "Id", DataType: "int"}}, Partitions: []source.Partition{
		{Schema: profile.Schema, Name: "1", Ordinal: 1, Lower: []source.PartitionValue{{Kind: "minvalue"}}, Upper: []source.PartitionValue{{Kind: "number", Text: "0"}}, LowerInclusive: true},
		{Schema: profile.Schema, Name: "2", Ordinal: 2, Lower: []source.PartitionValue{{Kind: "number", Text: "0"}}, Upper: []source.PartitionValue{{Kind: "maxvalue"}}, LowerInclusive: true},
	}}
	require.NoError(t, Equivalent(plan.Target, actual))
	require.Equal(t, profile.Schema, plan.Mapping[0].TargetSchema)
	actual.Partitions[0].Schema = "dbo"
	require.Error(t, Equivalent(plan.Target, actual), "a different schema must never pass exact native verification")
}

func TestPartitionCompileQuotingAndColumnMappingAreSeparateFromSourceNames(t *testing.T) {
	info := compilerRangeFixture()
	info.Partition.Partitions[0].Name = `P"Neg`
	plan, err := Compile(info, []dialect.Statement{{SQL: `CREATE TABLE "Dst"."Orders" ("MappedKey" INTEGER NOT NULL)`}}, compilerProfile("postgres"), Options{SourceType: "mysql", TargetType: "postgres", TargetSchema: "Dst", ColumnName: func(string) string { return "MappedKey" }})
	require.NoError(t, err)
	require.Contains(t, dialect.JoinSQL(plan.Statements), `PARTITION BY RANGE ("MappedKey")`)
	require.Contains(t, dialect.JoinSQL(plan.Statements), `"Dst"."P""Neg"`)
	require.Equal(t, `P"Neg`, plan.Mapping[0].SourceName)
	require.Equal(t, "Id", info.Partition.Keys[0].Column)
	require.Equal(t, "MappedKey", plan.Target.Keys[0].Column)
}

func TestPartitionCompileRejectsUnsafeOrUnprovenRouting(t *testing.T) {
	for _, scenario := range []string{"collision", "root collision", "nullable range", "default mysql", "numeric injection", "expression injection", "unknown literal", "cross hash", "automatic", "open lower", "bad product", "missing version"} {
		t.Run(scenario, func(t *testing.T) {
			info := compilerRangeFixture()
			profile := compilerProfile("postgres")
			opt := Options{SourceType: "mysql", TargetType: "postgres", TargetSchema: "Dst"}
			switch scenario {
			case "collision":
				info.Partition.Partitions[0].Name, info.Partition.Partitions[1].Name = "Same", "same"
				opt.Name = strings.ToLower
			case "root collision":
				info.Partition.Partitions[0].Name = info.TableName
			case "nullable range":
				info.Columns[0].IsNullable = true
			case "default mysql":
				info = compilerListFixture()
				info.Partition.Partitions[1].Default, info.Partition.Partitions[1].Values = true, nil
				profile, opt.TargetType, opt.SourceType = compilerProfile("mysql"), "mysql", "oracle"
			case "numeric injection":
				info.Partition.Partitions[0].Upper[0].Text = "0); DROP TABLE victim; --"
			case "expression injection":
				info.Partition.Keys[0] = source.PartitionKey{Expression: `Id); DROP TABLE victim; --`}
			case "unknown literal":
				info.Partition.Partitions[0].Upper[0] = source.PartitionValue{Kind: "sql", Text: "danger()"}
			case "cross hash":
				info.Partition.Method, info.Partition.Algorithm = "HASH", "pg-native-hash"
				for i := range info.Partition.Partitions {
					info.Partition.Partitions[i].HashModulus, info.Partition.Partitions[i].HashRemainder = 2, i
				}
				profile, opt.TargetType, opt.SourceType = compilerProfile("mysql"), "mysql", "postgres"
			case "automatic":
				info.Partition.Automatic = "NUMTOYMINTERVAL(1,'MONTH')"
			case "open lower":
				info.Partition.Partitions[0].LowerInclusive = false
			case "bad product":
				profile.Product = "gaussdb"
			case "missing version":
				profile.Version = ""
			}
			plan, err := Compile(info, []dialect.Statement{{SQL: `DROP TABLE "Dst"."Orders"`}, {SQL: `CREATE TABLE "Dst"."Orders" ("Id" INT)`}}, profile, opt)
			require.Error(t, err)
			require.Nil(t, plan, "no executable fallback DDL is returned for an unsafe partition definition")
		})
	}
}

func TestPartitionCompileRowMovementMismatchAndNativeClauses(t *testing.T) {
	for _, movement := range []string{"enabled", "disabled"} {
		for _, target := range []string{"oracle", "dameng"} {
			t.Run(target+movement, func(t *testing.T) {
				info := compilerRangeFixture()
				info.Partition.RowMovement = movement
				plan, err := Compile(info, compilerBase(t, info, target), compilerProfile(target), Options{SourceType: "mysql", TargetType: target, TargetSchema: "Dst"})
				require.NoError(t, err)
				keyword := "ENABLE"
				if movement == "disabled" {
					keyword = "DISABLE"
				}
				require.Contains(t, dialect.JoinSQL(plan.Statements), keyword+" ROW MOVEMENT")
				require.Equal(t, movement, plan.Target.RowMovement)
			})
		}
	}
	info := compilerRangeFixture()
	info.Partition.RowMovement = "disabled"
	_, err := Compile(info, compilerBase(t, info, "postgres"), compilerProfile("postgres"), Options{SourceType: "oracle", TargetType: "postgres", TargetSchema: "Dst"})
	require.ErrorContains(t, err, "UPDATE")
	info.Partition.RowMovement = "unknown"
	_, err = Compile(info, compilerBase(t, info, "postgres"), compilerProfile("postgres"), Options{SourceType: "oracle", TargetType: "postgres", TargetSchema: "Dst"})
	require.Error(t, err)
}

func TestPartitionCompileQuotesStringLiteralAndNeverExecutesSourceDDL(t *testing.T) {
	info := compilerListFixture()
	info.Columns[0].DataType, info.Partition.Keys[0].DataType = "varchar", "varchar"
	info.Partition.SourceDDL = "DROP TABLE source_evidence_only"
	info.Partition.Partitions[0].Values = [][]source.PartitionValue{{{Kind: "string", Text: "x'); DROP TABLE victim; --"}}}
	info.Partition.Partitions[1].Values = [][]source.PartitionValue{{{Kind: "string", Text: "safe"}}}
	plan, err := Compile(info, []dialect.Statement{{SQL: `CREATE TABLE "Dst"."Orders" ("Id" VARCHAR(100))`}}, compilerProfile("postgres"), Options{SourceType: "postgres", TargetType: "postgres", TargetSchema: "Dst"})
	require.NoError(t, err)
	ddl := dialect.JoinSQL(plan.Statements)
	require.Contains(t, ddl, `FOR VALUES IN ('x''); DROP TABLE victim; --')`)
	require.NotContains(t, ddl, "source_evidence_only")
}
