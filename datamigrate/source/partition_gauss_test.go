package source

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func pgNativeMockRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"oid", "parent", "name", "kind", "strategy", "keys", "bounds", "interval", "transit", "expression"})
}

func TestPGNativeRangeCatalogUsesExactBoundsAndOriginalNames(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	fields := map[string]bool{"pg_class.parttype": true, "pg_class.relrowmovement": true, "pg_partition.parentid": true, "pg_partition.parttype": true, "pg_partition.partstrategy": true, "pg_partition.partkey": true, "pg_partition.boundaries": true, "pg_partition.interval": true, "pg_partition.transit": true}
	r := &PostgresReader{db: db, schema: "MiXeD", profile: PGProfile{Product: "gaussdb", Catalog: "pg_catalog", Fields: fields}}
	mock.ExpectQuery(`SELECT c.oid,c.relkind`).WithArgs("MiXeD", "Root").WillReturnRows(sqlmock.NewRows([]string{"oid", "kind", "native", "inherits", "rowmovement"}).AddRow(42, "r", true, false, true))
	mock.ExpectQuery(`SELECT p.oid,p.parentid.*p.parttype='r'`).WithArgs(int64(42)).WillReturnRows(pgNativeMockRows().AddRow(999, 42, "Root", "r", "r", "1", nil, nil, nil, nil))
	// OID order is not range order; 10 must follow 2, not precede it.
	mock.ExpectQuery(`WITH RECURSIVE family`).WithArgs(int64(42)).WillReturnRows(pgNativeMockRows().AddRow(43, 42, "UpperTen", "p", "r", nil, "{10}", nil, nil, nil).AddRow(44, 42, "LowerTwo", "p", "r", nil, "{2}", nil, nil, nil).AddRow(45, 42, "Rest", "p", "r", nil, "{NULL}", nil, nil, nil))
	mock.ExpectQuery(`SELECT a.attnum,a.attname,format_type`).WithArgs(int64(42)).WillReturnRows(sqlmock.NewRows([]string{"number", "name", "type", "collation"}).AddRow(1, "MiXeDKey", "bigint", ""))
	spec, err := r.PartitionMetadata(context.Background(), "Root")
	require.NoError(t, err)
	require.Empty(t, spec.Unsupported)
	require.Equal(t, "pg_partition:gaussdb:r", spec.Algorithm)
	require.Equal(t, "enabled", spec.RowMovement)
	require.Equal(t, "MiXeDKey", spec.Keys[0].Column)
	require.Equal(t, "LowerTwo", spec.Partitions[0].Name)
	require.Equal(t, "MiXeD", spec.Partitions[0].Schema)
	require.Equal(t, []PartitionValue{{Kind: "minvalue"}}, spec.Partitions[0].Lower)
	require.Equal(t, []PartitionValue{{Kind: "number", Text: "2"}}, spec.Partitions[1].Lower)
	require.Equal(t, []PartitionValue{{Kind: "maxvalue"}}, spec.Partitions[2].Upper)
}

func TestPGNativeListArraysDistinguishLiteralNULLAndDefault(t *testing.T) {
	r := &PostgresReader{profile: PGProfile{Product: "gaussdb"}}
	root := pgNativePartition{strategy: "l", keys: sql.NullString{Valid: true, String: "1"}}
	children := map[int64][]pgNativePartition{42: {{oid: 43, name: "Literal", boundaries: sql.NullString{Valid: true, String: `{"NULL","a,b"}`}}, {oid: 44, name: "Default", boundaries: sql.NullString{Valid: true, String: `{NULL}`}}}}
	spec, err := r.buildPGNativePartitionSpec(root, 42, "App", children, map[int]PartitionKey{1: {Column: "Text", DataType: "text", Collation: "C"}}, map[int64]bool{})
	require.NoError(t, err)
	require.Empty(t, spec.Unsupported)
	require.Equal(t, [][]PartitionValue{{{Kind: "string", Text: "NULL"}}, {{Kind: "string", Text: "a,b"}}}, spec.Partitions[0].Values)
	require.True(t, spec.Partitions[1].Default)
	require.Empty(t, spec.Partitions[1].Values)
}

func TestPGNativeMultiListDecodesNestedArrayAndActualNull(t *testing.T) {
	r := &PostgresReader{profile: PGProfile{Product: "gaussdb"}}
	root := pgNativePartition{strategy: "l", keys: sql.NullString{Valid: true, String: "1 2"}}
	children := map[int64][]pgNativePartition{42: {{oid: 43, name: "Tuple", boundaries: sql.NullString{Valid: true, String: `{"{1,\"A,B\"}","{2,NULL}"}`}}}}
	spec, err := r.buildPGNativePartitionSpec(root, 42, "App", children, map[int]PartitionKey{1: {Column: "Id", DataType: "integer"}, 2: {Column: "Text", DataType: "text", Collation: "C"}}, map[int64]bool{})
	require.NoError(t, err)
	require.Empty(t, spec.Unsupported)
	require.Equal(t, [][]PartitionValue{{{Kind: "number", Text: "1"}, {Kind: "string", Text: "A,B"}}, {{Kind: "number", Text: "2"}, {Kind: "null"}}}, spec.Partitions[0].Values)
}

func TestPGNativeIntervalKeepsFutureRuleBlocked(t *testing.T) {
	r := &PostgresReader{profile: PGProfile{Product: "gaussdb"}}
	root := pgNativePartition{strategy: "i", keys: sql.NullString{Valid: true, String: "1"}, interval: sql.NullString{Valid: true, String: `{"1 day"}`}, transit: sql.NullString{Valid: true, String: `{2024-01-01}`}}
	children := map[int64][]pgNativePartition{42: {{oid: 43, name: "Existing", boundaries: sql.NullString{Valid: true, String: `{2024-02-01}`}}}}
	spec, err := r.buildPGNativePartitionSpec(root, 42, "App", children, map[int]PartitionKey{1: {Column: "At", DataType: "date"}}, map[int64]bool{})
	require.NoError(t, err)
	require.Contains(t, spec.Automatic, "1 day")
	require.Contains(t, spec.Automatic, "2024-01-01")
	require.Contains(t, spec.Unsupported, "不能只复制当前分区快照")
}

func TestPGNativeMetadataErrorsDoNotFlattenSource(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	fields := map[string]bool{}
	for _, field := range []string{"parttype", "parentid", "partstrategy", "partkey", "boundaries", "interval", "transit"} {
		fields["pg_partition."+field] = true
	}
	r := &PostgresReader{db: db, profile: PGProfile{Catalog: "pg_catalog", Fields: fields}}
	mock.ExpectQuery(`SELECT p.oid,p.parentid`).WithArgs(int64(42)).WillReturnError(errors.New("native catalog denied"))
	spec, err := r.pgNativePartitionMetadata(context.Background(), 42, "App")
	require.Nil(t, spec)
	require.ErrorContains(t, err, "native catalog denied")
	spec, err = (&PostgresReader{profile: PGProfile{Fields: map[string]bool{}}}).pgNativePartitionMetadata(context.Background(), 42, "App")
	require.NoError(t, err)
	require.Contains(t, spec.Unsupported, "缺少完整分区字段")
}

func TestPGNativeLeafCountsUsePartitionSelectors(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	spec := &PartitionSpec{Method: "RANGE", Algorithm: "pg_partition:gaussdb:r", Partitions: []Partition{{Schema: "App", Name: "Leaf"}, {Schema: "App", Name: "Parent", Subpartition: &PartitionSpec{Method: "LIST", Partitions: []Partition{{Schema: "App", Name: "Sub"}}}}}}
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "App"\."Root" PARTITION \("Leaf"\)`).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "App"\."Root" SUBPARTITION \("Sub"\)`).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	counts, err := CountPartitionRows(context.Background(), db, "postgres", "App", "Root", spec)
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"App.Leaf": 2, "App.Sub": 0}, counts)
}

func TestPGNativeRangeCannotInferTextCollationOrAmbiguousDates(t *testing.T) {
	_, err := pgNativeCompareTuple([]PartitionValue{{Kind: "string", Text: "Z"}}, []PartitionValue{{Kind: "string", Text: "a"}}, []PartitionKey{{Collation: "default"}})
	require.ErrorContains(t, err, "排序规则")
	_, err = pgNativeValue(sql.NullString{Valid: true, String: "01-02-2024"}, PartitionKey{DataType: "date"}, true)
	require.ErrorContains(t, err, "ISO 日期")
}
