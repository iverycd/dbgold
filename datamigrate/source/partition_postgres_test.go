package source

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestPGPartitionBoundsPreserveTypedNativeValues(t *testing.T) {
	t.Run("quoted negative integer", func(t *testing.T) {
		var part Partition
		require.NoError(t, pgPartitionBound(`FOR VALUES IN ('-1'::integer, 0)`, "LIST", []PartitionKey{{Column: "Id", DataType: "integer"}}, &part))
		require.Equal(t, "number", part.Values[0][0].Kind)
		require.Equal(t, "-1", part.Values[0][0].Text)
	})
	keys := []PartitionKey{{Column: "At", DataType: "date"}}
	var part Partition
	require.NoError(t, pgPartitionBound(`FOR VALUES FROM (MINVALUE) TO ('2024-01-01'::date)`, "RANGE", keys, &part))
	require.Equal(t, []PartitionValue{{Kind: "minvalue"}}, part.Lower)
	require.Equal(t, []PartitionValue{{Kind: "date", Text: "2024-01-01"}}, part.Upper)
	require.True(t, part.LowerInclusive)
	require.False(t, part.UpperInclusive)
	part = Partition{}
	require.NoError(t, pgPartitionBound(`FOR VALUES IN ('2024-01-01'::date, '2024-02-01'::date, NULL)`, "LIST", keys, &part))
	require.Equal(t, [][]PartitionValue{{{Kind: "date", Text: "2024-01-01"}}, {{Kind: "date", Text: "2024-02-01"}}, {{Kind: "null"}}}, part.Values)
	part = Partition{}
	require.NoError(t, pgPartitionBound(`FOR VALUES WITH (modulus 8, remainder 3)`, "HASH", keys, &part))
	require.Equal(t, 8, part.HashModulus)
	require.Equal(t, 3, part.HashRemainder)
	for _, raw := range []string{`FOR VALUES FROM (danger()) TO (10)`, `FOR VALUES FROM (1) TO (10) trailing`, `FOR VALUES IN (1; DELETE FROM t)`} {
		require.Error(t, pgPartitionBound(raw, "RANGE", keys, &Partition{}), raw)
	}
}

func TestPGPartitionKeysPreserveQuotedNamesAndExpressions(t *testing.T) {
	keys, err := pgPartitionKeys(`RANGE ("MiXeD", plain, (EXTRACT(year FROM "At")))`)
	require.NoError(t, err)
	require.Equal(t, "MiXeD", keys[0].Column)
	require.Equal(t, "plain", keys[1].Column)
	require.Empty(t, keys[2].Column)
	require.Contains(t, keys[2].Expression, `"At"`)
}

func TestPGPartitionMetadataReadsPhysicalDescendantsWithOriginalCase(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	r := &PostgresReader{db: db, schema: "MiXeD", profile: PGProfile{Catalog: "pg_catalog", Fields: map[string]bool{"pg_class.relispartition": true, "pg_class.relpartbound": true, "pg_partitioned_table.partstrat": true}}}
	mock.ExpectQuery(`SELECT c.oid,c.relkind`).WithArgs("MiXeD", "Root").WillReturnRows(sqlmock.NewRows([]string{"oid", "kind", "native", "inherits"}).AddRow(42, "p", false, true))
	mock.ExpectQuery(`SELECT p.partstrat,p.partattrs`).WithArgs(int64(42)).WillReturnRows(sqlmock.NewRows([]string{"strategy", "attrs", "key"}).AddRow("r", "1", `RANGE ("Key")`))
	mock.ExpectQuery(`SELECT a.attname,format_type`).WithArgs(int64(42)).WillReturnRows(sqlmock.NewRows([]string{"name", "type", "collation"}).AddRow("Key", "bigint", ""))
	mock.ExpectQuery(`SELECT child.oid,ns.nspname`).WithArgs(int64(42)).WillReturnRows(sqlmock.NewRows([]string{"oid", "schema", "name", "kind", "bound"}).AddRow(43, "Other", "LowerCase", "r", `FOR VALUES FROM (MINVALUE) TO (10)`).AddRow(44, "MiXeD", "UpperCase", "r", `FOR VALUES FROM (10) TO (MAXVALUE)`))
	spec, err := r.PartitionMetadata(context.Background(), "Root")
	require.NoError(t, err)
	require.Empty(t, spec.Unsupported)
	require.Equal(t, "Key", spec.Keys[0].Column)
	require.Equal(t, "Other", spec.Partitions[0].Schema)
	require.Equal(t, "LowerCase", spec.Partitions[0].Name)
	require.Equal(t, []PartitionValue{{Kind: "number", Text: "10"}}, spec.Partitions[1].Lower)
}

func TestPGPartitionRootsHideOnlyActualNativeDescendants(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	r := &PostgresReader{db: db, schema: "App", profile: PGProfile{Catalog: "pg_catalog", Fields: map[string]bool{"pg_class.relispartition": true}}}
	mock.ExpectQuery(`WITH RECURSIVE roots`).WithArgs("App").WillReturnRows(sqlmock.NewRows([]string{"schema", "leaf", "root"}).AddRow("App", "Leaf", "Root").AddRow("Other", "SameName", "Root"))
	roots, err := r.PartitionRoots(context.Background())
	require.NoError(t, err)
	require.Equal(t, map[string]string{"Leaf": "Root", "App.Leaf": "Root", "Other.SameName": "Root"}, roots)
}

func TestPGPartitionMetadataCatalogErrorsNeverFlatten(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	r := &PostgresReader{db: db, schema: "App", profile: PGProfile{Catalog: "pg_catalog", Fields: map[string]bool{}}}
	mock.ExpectQuery(`SELECT c.oid,c.relkind`).WithArgs("App", "T").WillReturnError(errors.New("metadata denied"))
	spec, err := r.PartitionMetadata(context.Background(), "T")
	require.ErrorContains(t, err, "metadata denied")
	require.Nil(t, spec)
}

func TestClonePartitionSpecKeepsNestedPlansIndependent(t *testing.T) {
	spec := &PartitionSpec{Method: "RANGE", Keys: []PartitionKey{{Column: "Key"}}, Partitions: []Partition{{Name: "P", Upper: []PartitionValue{{Kind: "number", Text: "10"}}, Subpartition: &PartitionSpec{Method: "LIST", Partitions: []Partition{{Name: "S", Values: [][]PartitionValue{{{Kind: "string", Text: "original"}}}}}}}}}
	clone := ClonePartitionSpec(spec)
	clone.Keys[0].Column = "changed"
	clone.Partitions[0].Upper[0].Text = "20"
	clone.Partitions[0].Subpartition.Partitions[0].Values[0][0].Text = "changed"
	require.Equal(t, "Key", spec.Keys[0].Column)
	require.Equal(t, "10", spec.Partitions[0].Upper[0].Text)
	require.Equal(t, "original", spec.Partitions[0].Subpartition.Partitions[0].Values[0][0].Text)
}
