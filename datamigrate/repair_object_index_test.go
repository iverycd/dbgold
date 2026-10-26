package datamigrate

import (
	"fmt"
	"testing"

	"dbgold/datamigrate/repair"
	"github.com/stretchr/testify/require"
)

func TestRepairObjectIndexPreservesFirstMatch(t *testing.T) {
	r := &repairRecorder{objects: []repair.Object{{ID: "a"}, {ID: "b"}, {ID: "a"}}}
	require.Equal(t, 0, r.objectPosition("a"))
	require.Equal(t, 1, r.objectPosition("b"))
	require.Equal(t, -1, r.objectPosition("missing"))
	require.Equal(t, 3, r.appendObject(repair.Object{ID: "c"}))
	require.Equal(t, 4, r.appendObject(repair.Object{ID: "a"}))
	require.Equal(t, 0, r.objectPosition("a"))
	require.Equal(t, 3, r.objectPosition("c"))
	require.Equal(t, len(r.objects), r.indexedObjectCount)
	require.Equal(t, []repair.Object{{ID: "a"}, {ID: "b"}, {ID: "a"}, {ID: "c"}, {ID: "a"}}, r.objects)
}

func TestRepairObjectIndexRebuildsAfterFilteringAndReplacement(t *testing.T) {
	r := &repairRecorder{objects: []repair.Object{{ID: "a"}, {ID: "b"}, {ID: "c"}}}
	r.rebuildObjectIndex()
	r.objects = r.objects[1:]
	r.rebuildObjectIndex()
	require.Equal(t, -1, r.objectPosition("a"))
	require.Equal(t, 0, r.objectPosition("b"))
	require.Equal(t, 1, r.objectPosition("c"))

	// A replacement can keep the same length, so its owner explicitly rebuilds.
	r.objects = []repair.Object{{ID: "new"}, {ID: "b"}}
	r.rebuildObjectIndex()
	require.Equal(t, 0, r.objectPosition("new"))
	require.Equal(t, 1, r.objectPosition("b"))
	require.Equal(t, -1, r.objectPosition("c"))
}

func TestRepairObjectIndexHandlesLegacySliceChanges(t *testing.T) {
	r := &repairRecorder{}
	require.Equal(t, -1, r.objectPosition("missing"))
	require.Equal(t, 0, r.appendObject(repair.Object{ID: "a"}))
	// Old constructors and tests can append directly without an existing index.
	r.objects = append(r.objects, repair.Object{ID: "b"})
	require.Equal(t, 1, r.objectPosition("b"))
	r.objects[0], r.objects[1] = r.objects[1], r.objects[0]
	require.Equal(t, 1, r.objectPosition("a"))
	require.Equal(t, 0, r.objectPosition("b"))
	r.objectIndex["a"] = len(r.objects) + 1
	require.Equal(t, 1, r.objectPosition("a"))

	r.objects = nil
	require.Equal(t, -1, r.objectPosition("a"))
	require.Equal(t, 0, r.appendObject(repair.Object{ID: "c"}))
	require.Equal(t, 0, r.objectPosition("c"))
}

var repairObjectIndexBenchmarkPosition int

func BenchmarkRepairObjectPosition(b *testing.B) {
	for _, size := range []int{32, 27463} {
		objects := make([]repair.Object, size)
		for i := range objects {
			objects[i].ID = fmt.Sprintf("comment-%05d", i)
		}
		id := objects[len(objects)-1].ID
		b.Run(fmt.Sprintf("indexed/%d", size), func(b *testing.B) {
			r := &repairRecorder{objects: objects}
			r.rebuildObjectIndex()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				repairObjectIndexBenchmarkPosition = r.objectPosition(id)
			}
		})
		b.Run(fmt.Sprintf("linear/%d", size), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for position := range objects {
					if objects[position].ID == id {
						repairObjectIndexBenchmarkPosition = position
						break
					}
				}
			}
		})
	}
}
