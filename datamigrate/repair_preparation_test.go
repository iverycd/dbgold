package datamigrate

import (
	"context"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func TestRepairReferencePreservesCompositeColumnIdentity(t *testing.T) {
	require.Equal(t, repairReference("T", nil), repairReference("T", []string{}))
	for _, columns := range [][]string{{"A,B"}, {"B", "A"}, {"a", "B"}, {"A", "B", ""}} {
		require.NotEqual(t, repairReference("T", []string{"A", "B"}), repairReference("T", columns))
	}
	require.NotEqual(t, repairReference("T", []string{"A"}), repairReference("t", []string{"A"}))
	require.NotEqual(t, repairReference("T", []string{"1:", "A"}), repairReference("T", []string{"1", ":A"}))
}

func TestRepairPreparationAssociationsKeepOrderedMatchesAndIndependentMaps(t *testing.T) {
	objects := []repair.Object{
		{ID: "parent-old", Kind: "table", Table: "Parent", ColumnMapping: map[string]string{"ID": "old"}},
		{ID: "child", Kind: "table", Table: "Child", ColumnMapping: map[string]string{"Key": "child_key"}},
		{ID: "parent-last", Kind: "table", Table: "Parent", ColumnMapping: map[string]string{"ID": "new"}},
		{ID: "empty-table", Kind: "table", Table: "Empty"},
		{ID: "first-key", Kind: "index", Table: "Parent", Index: &source.IndexInfo{IsUnique: true, Columns: []string{"A", "B"}}},
		{ID: "later-key", Kind: "primary_key", Table: "Parent", Index: &source.IndexInfo{IsPrimary: true, Columns: []string{"A", "B"}}},
		{ID: "reverse-key", Kind: "index", Table: "Parent", Index: &source.IndexInfo{IsUnique: true, Columns: []string{"B", "A"}}},
		{ID: "fk", Kind: "foreign_key", Table: "Child", Dependencies: []string{"parent-last", "child"}, ForeignKey: &source.FKInfo{RefTable: "Parent", RefColumns: []string{"A", "B"}}},
		{ID: "reverse-fk", Kind: "foreign_key", Table: "Child", ForeignKey: &source.FKInfo{RefTable: "Parent", RefColumns: []string{"B", "A"}}},
		{ID: "other-fk", Kind: "foreign_key", Table: "Child", ForeignKey: &source.FKInfo{RefTable: "parent", RefColumns: []string{"A", "B"}}},
		{ID: "empty-owner", Kind: "owner", Table: "Empty"},
	}
	linkRepairForeignKeys(objects)
	require.Equal(t, []string{"parent-last", "child", "first-key"}, objects[7].Dependencies)
	require.Equal(t, []string{"reverse-key"}, objects[8].Dependencies)
	require.Empty(t, objects[9].Dependencies)
	// Exclusion happens after FK linking. A dependency on the original first
	// matching key remains identifiable even when that key is excluded.
	objects = append(objects[:4], objects[5:]...)
	attachRepairColumnMappings(objects)
	fk := &objects[6]
	require.Equal(t, "fk", fk.ID)
	require.Equal(t, "first-key", fk.Dependencies[2])
	require.Equal(t, map[string]string{"ID": "new"}, fk.RefColumnMapping)
	require.Equal(t, map[string]string{"Key": "child_key"}, fk.ColumnMapping)
	require.NotNil(t, objects[len(objects)-1].ColumnMapping)
	fk.RefColumnMapping["ID"] = "edited"
	fk.ColumnMapping["Key"] = "edited"
	require.Equal(t, "new", objects[2].ColumnMapping["ID"])
	require.Equal(t, "new", objects[7].RefColumnMapping["ID"])
	require.Equal(t, "child_key", objects[1].ColumnMapping["Key"])
	require.Equal(t, "child_key", objects[7].ColumnMapping["Key"])
}

func TestRepairMetadataProgressThrottlesButAlwaysReportsCompletion(t *testing.T) {
	ch := make(chan string, 10)
	now := time.Unix(0, 0)
	progress := newRepairMetadataProgress(NewLogger(ch), func() time.Time { return now })
	progress(0, 118)
	progress(1, 118)
	require.Empty(t, ch)
	now = now.Add(time.Second)
	progress(50, 118)
	require.Contains(t, <-ch, "读取表结构：50/118")
	now = now.Add(100 * time.Millisecond)
	progress(100, 118)
	require.Empty(t, ch)
	progress(118, 118)
	require.Contains(t, <-ch, "读取表结构：118/118")
	progress(0, 0)
	require.Contains(t, <-ch, "读取表结构：0/0")
}

type preparationStageReader struct {
	*mockReader
	stage string
	block func()
}

func (r *preparationStageReader) ListTables(ctx context.Context) ([]string, error) {
	if r.stage == "读取迁移表列表" {
		r.block()
	}
	return r.mockReader.ListTables(ctx)
}
func (r *preparationStageReader) GetTableDDLInfo(ctx context.Context, table string) (*source.TableDDLInfo, error) {
	if r.stage == "读取表结构" {
		r.block()
	}
	return r.mockReader.GetTableDDLInfo(ctx, table)
}
func (r *preparationStageReader) GetIndexes(ctx context.Context) ([]source.IndexInfo, error) {
	if r.stage == "收集索引" {
		r.block()
	}
	return r.mockReader.GetIndexes(ctx)
}

func TestRepairPreparationLogsBeforeSlowOperationsComplete(t *testing.T) {
	for _, stage := range []string{"读取迁移表列表", "读取表结构", "收集索引", "保存迁移对象清单"} {
		t.Run(stage, func(t *testing.T) {
			entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
			defer close(release)
			r := &preparationStageReader{mockReader: incrementalRepairReader("MixedTable"), stage: stage}
			r.block = func() { close(entered); <-release }
			m, job := newTestMigrator(r, &mockWriter{})
			m.resetSourceTableDDLCache()
			if stage == "保存迁移对象清单" {
				m.cfg.RepairSink = func(RepairManifest) error { r.block(); return nil }
			}
			go func() { defer close(finished); m.beginRepair(context.Background()) }()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("preparation did not reach the controlled operation")
			}
			lines := []string{}
			for len(job.LogCh) > 0 {
				lines = append(lines, <-job.LogCh)
			}
			joined := strings.Join(lines, "\n")
			require.Contains(t, joined, "正在"+stage)
			require.NotContains(t, joined, "修复清单准备：")
			// Release and join before inspecting mutable preparation state.
			release <- struct{}{}
			select {
			case <-finished:
			case <-time.After(3 * time.Second):
				t.Fatal("preparation did not finish")
			}
			require.Nil(t, m.repairs.sinkErr)
		})
	}
}
