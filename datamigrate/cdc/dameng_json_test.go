package cdc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"
	"github.com/stretchr/testify/require"
)

func TestDamengJSONCanonicalizesBothImagesWithoutChangingInput(t *testing.T) {
	db, mock := newSequenceMock(t)
	raw := `{"long_key":[1.0,{"z":"中文"}],"a":true}`
	canonical := `{"a": true, "long_key": [1.0, {"z": "中文"}]}`
	mock.ExpectQuery("SELECT " + damengSourceJSONExpression).WithArgs(raw).WillReturnRows(sqlmock.NewRows([]string{"json"}).AddRow(canonical))
	before := []any{raw}
	after := []any{raw}
	got, err := normalizeDamengJSON(context.Background(), db, []Change{{Action: "update", Table: &TableInfo{Name: "T", Columns: []string{"Doc"}, ColumnTypes: []string{"JSON"}}, Before: before, After: after}})
	require.NoError(t, err)
	require.Equal(t, canonical, got[0].Before[0])
	require.Equal(t, canonical, got[0].After[0])
	require.Equal(t, raw, before[0])
	require.Equal(t, raw, after[0])
}

func TestDamengJSONReadsOpaqueDecimalPrecisionAtAnyDepth(t *testing.T) {
	decimal := []byte{gomysql.MYSQL_TYPE_NEWDECIMAL, 5, 5, 2, 0x80, 1, 20}
	require.NoError(t, checkDamengBinaryJSON(replication.JSONB_OPAQUE, decimal, 0))
	array := append([]byte{1, 0, 14, 0, replication.JSONB_OPAQUE, 7, 0}, decimal...)
	items, err := collectDamengJSONOpaque(replication.JSONB_SMALL_ARRAY, array, 0, nil)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, "DECIMAL(5,2)", items[0].sqlType)
	require.Equal(t, 0, items[0].parts[0].index)
	require.True(t, items[0].parts[0].array)
	require.ErrorContains(t, checkDamengBinaryJSON(replication.JSONB_OPAQUE, []byte{gomysql.MYSQL_TYPE_BLOB, 1, 0}, 0), "暂不支持")
	require.NoError(t, checkDamengBinaryJSON(replication.JSONB_STRING, []byte{2, replication.JSONB_OPAQUE, gomysql.MYSQL_TYPE_NEWDECIMAL}, 0), "opaque-looking bytes inside JSON text are ordinary text")
	require.ErrorContains(t, checkDamengBinaryJSON(replication.JSONB_SMALL_ARRAY, []byte{255, 255, 1, 0}, 0), "长度损坏")
}

func TestDamengOpaqueJSONRestoresDecimalWithoutJSONNumberRoundtrip(t *testing.T) {
	doc, err := buildDamengJSONDocument(`{"a\"b\\c":[{"01":"-123.4500"}]}`, []damengJSONOpaque{{typ: gomysql.MYSQL_TYPE_NEWDECIMAL, precision: 12, scale: 4, sqlType: "DECIMAL(12,4)", parts: []damengJSONPathPart{{key: "a\"b\\c"}, {array: true, index: 0}, {key: "01"}}}})
	require.NoError(t, err)
	query, args := doc.query()
	require.Equal(t, "JSON_SET(CAST(? AS JSON),?,CAST(? AS DECIMAL(12,4)))", query)
	require.Equal(t, []any{doc.Text, `$."a\"b\\c"[0]."01"`, "-123.4500"}, args)
	root, err := buildDamengJSONDocument(`"123.4500"`, []damengJSONOpaque{{typ: gomysql.MYSQL_TYPE_NEWDECIMAL, precision: 12, scale: 4, sqlType: "DECIMAL(12,4)"}})
	require.NoError(t, err)
	require.Equal(t, "$", root.Patches[0].Path)
}

func TestDamengJSONRejectsPartialDiff(t *testing.T) {
	db, _ := newSequenceMock(t)
	_, err := normalizeDamengJSON(context.Background(), db, []Change{{Action: "update", Table: &TableInfo{Name: "T", Columns: []string{"Doc"}, ColumnTypes: []string{"JSON"}}, After: []any{&replication.JsonDiff{}}}})
	require.ErrorContains(t, err, "PARTIAL_JSON")
}

func TestDamengJSONOpaqueDecimalInternalPrecisionAndBounds(t *testing.T) {
	// MySQL stores a rounded internal precision of 66 for DECIMAL(65,30).
	payload := append([]byte{gomysql.MYSQL_TYPE_NEWDECIMAL, 32, 66, 30}, make([]byte, 30)...)
	items, err := collectDamengJSONOpaque(replication.JSONB_OPAQUE, payload, 0, nil)
	require.NoError(t, err)
	require.Equal(t, "DECIMAL(65,30)", items[0].sqlType)
	value := "-" + strings.Repeat("9", 35) + "." + strings.Repeat("1", 30)
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	doc, err := buildDamengJSONDocument(string(encoded), items)
	require.NoError(t, err)
	require.Equal(t, value, doc.Patches[0].Value)
	require.NotEqual(t, (damengJSONDocument{Text: string(encoded)}).key(), doc.key())
	encoded, err = json.Marshal(strings.Repeat("9", 36) + "." + strings.Repeat("1", 30))
	require.NoError(t, err)
	_, err = buildDamengJSONDocument(string(encoded), items)
	require.ErrorContains(t, err, "原始精度")
	require.ErrorContains(t, checkDamengBinaryJSON(replication.JSONB_OPAQUE, []byte{gomysql.MYSQL_TYPE_NEWDECIMAL, 3, 65, 30, 0}, 0), "二进制长度")
}

func TestDamengJSONOpaqueTemporalTextDoesNotDependOnSQLMode(t *testing.T) {
	for _, tc := range []struct {
		typ           byte
		decoded, want string
	}{
		{gomysql.MYSQL_TYPE_DATE, "2024-02-31 00:00:00.000000", "2024-02-31"},
		{gomysql.MYSQL_TYPE_DATE, "2024-00-02 00:00:00.000000", "2024-00-02"},
		{gomysql.MYSQL_TYPE_DATETIME, "2024-02-31 03:04:05.123456", "2024-02-31 03:04:05.123456"},
		{gomysql.MYSQL_TYPE_DATETIME, "0000-00-00 00:00:00", "0000-00-00 00:00:00.000000"},
		{gomysql.MYSQL_TYPE_TIME, "-111:22:33.123456", "-111:22:33.123456"},
		{gomysql.MYSQL_TYPE_TIME, "00:00:00", "00:00:00.000000"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			encoded, err := json.Marshal(tc.decoded)
			require.NoError(t, err)
			doc, err := buildDamengJSONDocument(string(encoded), []damengJSONOpaque{{typ: tc.typ}})
			require.NoError(t, err)
			query, args := doc.query()
			require.Equal(t, "JSON_SET(CAST(? AS JSON),?,?)", query)
			require.Equal(t, []any{string(encoded), "$", tc.want}, args)
		})
	}
}
