package cdc

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func schemaTestTables() []TableInfo {
	return []TableInfo{{Name: "Mixed", Columns: []string{"Id", "A", "B"}, ColumnTypes: []string{"INT", "INT", "INT"}, ColumnDefinitions: []string{"int", "int", "int"}, ColumnCharsets: []string{"", "", ""}, PrimaryKey: []int{0}, AutoIncrement: []int{0}}}
}
func TestSourceSchemaRoundTripFreezesColumnOrder(t *testing.T) {
	tables := schemaTestTables()
	snapshot, err := NewSourceSchema("SourceCase", tables)
	require.NoError(t, err)
	tables[0].Columns[1], tables[0].Columns[2] = "B", "A"
	raw, err := json.Marshal(snapshot)
	require.NoError(t, err)
	var saved SourceSchemaSnapshot
	require.NoError(t, json.Unmarshal(raw, &saved))
	restored, err := saved.TableInfos("SourceCase", []string{"Mixed"})
	require.NoError(t, err)
	require.Equal(t, []string{"Id", "A", "B"}, restored[0].Columns)
	require.Equal(t, []string{"int", "int", "int"}, restored[0].ColumnDefinitions)
	values, err := convertDamengBinlogRow(&restored[0], []any{int32(1), int32(11), int32(22)})
	require.NoError(t, err)
	require.EqualValues(t, 11, values[1])
	require.EqualValues(t, 22, values[2])
	restored[0].Columns[1] = "mutated"
	require.NoError(t, saved.Validate("SourceCase"))
	require.Error(t, saved.Validate("sourcecase"))
	require.Equal(t, snapshot.Hash, saved.Hash)
}
func TestSourceSchemaRejectsMissingCorruptOrUnsupportedSnapshots(t *testing.T) {
	snapshot, err := NewSourceSchema("source", schemaTestTables())
	require.NoError(t, err)
	_, err = snapshot.TableInfos("source", []string{"missing"})
	require.Error(t, err)
	_, err = snapshot.TableInfos("source", []string{})
	require.Error(t, err)
	snapshot.Tables[0].Columns[1], snapshot.Tables[0].Columns[2] = "B", "A"
	require.ErrorContains(t, snapshot.Validate("source"), "摘要")
	require.ErrorContains(t, (SourceSchemaSnapshot{}).Validate("source"), "旧任务不能恢复")
}
func TestSourceSchemaHashChangesWithColumnDefinition(t *testing.T) {
	tables := schemaTestTables()
	a, err := NewSourceSchema("source", tables)
	require.NoError(t, err)
	tables[0].ColumnDefinitions[1] = "int unsigned"
	b, err := NewSourceSchema("source", tables)
	require.NoError(t, err)
	require.NotEqual(t, a.Hash, b.Hash)
}
