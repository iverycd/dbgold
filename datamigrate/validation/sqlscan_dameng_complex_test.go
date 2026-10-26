package validation

import (
	"context"
	"math"
	"math/big"
	"strings"
	"testing"
	"time"

	"gitee.com/chunanyong/dm"
	"github.com/stretchr/testify/require"
)

func complexFixtureCell(t *testing.T, values ...any) damengComplexCell {
	t.Helper()
	e := damengComplexEncoder{ctx: context.Background(), limit: MaxValueBytes, active: map[any]bool{}}
	require.NoError(t, e.container(1, values, 0))
	return damengComplexCell(e.data)
}

func TestDamengComplexNativeFramingPreservesValues(t *testing.T) {
	cells := []damengComplexCell{
		complexFixtureCell(t, nil),
		complexFixtureCell(t, ""),
		complexFixtureCell(t, []byte{}),
		complexFixtureCell(t, "a\x00b"),
		complexFixtureCell(t, "ab", "c"),
		complexFixtureCell(t, "a", "bc"),
		complexFixtureCell(t, "Dir:file"),
		complexFixtureCell(t, "dir:file"),
		complexFixtureCell(t, "Dir:file "),
		complexFixtureCell(t, float64(0)),
		complexFixtureCell(t, math.Copysign(0, -1)),
		complexFixtureCell(t, float32(1)),
		complexFixtureCell(t, float64(1)),
		complexFixtureCell(t, int64(1)),
	}
	for i := range cells {
		for j := 0; j < i; j++ {
			require.NotEqual(t, cells[j], cells[i], "distinct cells %d/%d cannot share encoding", i, j)
		}
	}
	var emptyCollection damengComplexEncoder
	emptyCollection.ctx, emptyCollection.limit = context.Background(), MaxValueBytes
	require.NoError(t, emptyCollection.container(2, nil, 0))
	require.NotEqual(t, complexFixtureCell(t), damengComplexCell(emptyCollection.data), "object and collection are distinct")
	require.Equal(t, complexFixtureCell(t, dm.NewClob("中文\x00😀")), complexFixtureCell(t, "中文\x00😀"))
	require.Equal(t, complexFixtureCell(t, dm.NewBlob([]byte{0, 255})), complexFixtureCell(t, []byte{0, 255}))
}

func TestDamengComplexLOBBytesBudgetAndCancellation(t *testing.T) {
	long := strings.Repeat("中文😀\x00", 5000)
	require.Equal(t, complexFixtureCell(t, long), complexFixtureCell(t, dm.NewClob(long)), "chunk boundaries preserve all code points and NULs")
	for _, v := range []any{dm.NewClob("中文😀"), dm.NewBlob(make([]byte, 100))} {
		e := damengComplexEncoder{ctx: context.Background(), limit: 8, active: map[any]bool{}}
		require.ErrorContains(t, e.node(v, 0), "预算")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e := damengComplexEncoder{ctx: ctx, limit: MaxValueBytes}
	require.ErrorIs(t, e.node(dm.NewClob(long), 0), context.Canceled)
	e.ctx = context.Background()
	require.ErrorContains(t, e.node("deep", 129), "嵌套")
	require.ErrorContains(t, e.node(struct{ Text string }{"unknown"}, 0), "没有无损")
	_, err := encodeDamengComplex(context.Background(), "forged JSON", 100)
	require.ErrorContains(t, err, "原生复杂列")
	_, err = encodeDamengComplex(context.Background(), dm.NewDmStruct("Unbound", nil), 100)
	require.Error(t, err, "missing native descriptor must return an error, never panic or fake an empty object")
}

func TestDamengComplexDoesNotEraseTimezoneOrDecimalPrecision(t *testing.T) {
	a := time.Date(2026, 10, 26, 12, 34, 56, 123456789, time.FixedZone("A", 8*3600))
	b := a.In(time.FixedZone("B", 7*3600))
	require.NotEqual(t, complexFixtureCell(t, a), complexFixtureCell(t, b))
	value, ok := new(big.Float).SetPrec(1024).SetString("1234567890123456789.1234567890123456789")
	require.True(t, ok)
	decimal, err := dm.NewDecimalFromBigFloat(value)
	require.NoError(t, err)
	value, ok = new(big.Float).SetPrec(1024).SetString("1234567890123456789.1234567890123456788")
	require.True(t, ok)
	changed, err := dm.NewDecimalFromBigFloat(value)
	require.NoError(t, err)
	require.NotEqual(t, complexFixtureCell(t, decimal), complexFixtureCell(t, changed), "last precision-38 digit must participate")
}

func TestDamengComplexValidationFindsMemberChanges(t *testing.T) {
	meta := Table{Columns: []Column{{Name: "Id", DatabaseType: "INT", Kind: Number}, {Name: "Object", DatabaseType: "CLASS123", Kind: DamengComplex}}, PrimaryKey: []string{"Id"}}
	src := &memoryScanner{meta: meta, rows: [][]any{{1, complexFixtureCell(t, "Dir:file", "中文\x00", nil)}}}
	dst := &memoryScanner{meta: meta, rows: [][]any{{1, complexFixtureCell(t, "Dir:FILE", "中文\x00", nil)}}}
	result := Validate(context.Background(), src, dst, Plan{SourceTable: "S", TargetTable: "T"})
	require.Equal(t, StatusMismatch, result.Status)
	require.EqualValues(t, 1, result.ChangedRows)
	require.Equal(t, []string{"Object"}, result.Samples[0].Columns)
	require.Equal(t, "[dameng object]", result.Samples[0].Fields[0].Source.Text)
	require.NotEmpty(t, result.Samples[0].Fields[0].Source.SHA256)
	dst.rows = src.rows
	require.Equal(t, StatusPassed, Validate(context.Background(), src, dst, Plan{SourceTable: "S", TargetTable: "T"}).Status)
	_, err := canonical([]byte("forged"), meta.Columns[1], meta.Columns[1])
	require.ErrorContains(t, err, "未由原生")
	_, err = canonical(nil, meta.Columns[1], Column{Kind: Text})
	require.Error(t, err, "NULL does not justify comparing incompatible codecs")
}

func TestDamengComplexClassifiesOnlyNativeClassIDs(t *testing.T) {
	for _, family := range []string{"dameng", "oracle", "postgres", "mysql"} {
		for _, name := range []string{"CLASS123", "Class456", "CLASS", "CLASS1;DROP", "OTHER123"} {
			c := Column{Name: "Object", DatabaseType: name}
			ClassifyColumn(family, &c)
			if family == "dameng" && (name == "CLASS123" || name == "Class456") {
				require.Equal(t, DamengComplex, c.Kind)
				q, err := ValidationProjection(family, c)
				require.NoError(t, err)
				require.Equal(t, `"Object"`, q)
			} else {
				require.Equal(t, Unsupported, c.Kind)
			}
		}
	}
}
