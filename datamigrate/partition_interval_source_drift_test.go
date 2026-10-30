package datamigrate

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func TestIntervalSourceProofRechecksColumnsOutsidePartitionCatalog(t *testing.T) {
	for _, change := range []string{"nullable", "precision", "type", "missing snapshot"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			spec := &source.PartitionSpec{Method: "RANGE", Interval: &source.PartitionInterval{Unit: "month", Amount: "1"}}
			info := &source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{{Name: "K", DataType: "TIMESTAMP", DateTimePrecision: 6}}}
			r := &partitionFlowReader{mockReader: &mockReader{ddl: map[string]*source.TableDDLInfo{"T": info}}, specs: map[string]*source.PartitionSpec{"T": spec}, metadataCalls: map[string]int{}}
			m := NewMigrator(r, &mockWriter{}, &Job{LogCh: make(chan string, 10)}, Config{})
			m.resetSourceTableDDLCache()
			defer m.clearSourceTableDDLCache()
			_, err := m.sourceTableDDLInfo(ctx, "T")
			require.NoError(t, err)
			m.partitionState = &partitionRunState{tables: map[string]*PartitionTableResult{"T": {Source: source.ClonePartitionSpec(spec)}}}
			require.NoError(t, m.recheckPartitionSource(ctx, "T"))
			switch change {
			case "nullable":
				info.Columns[0].IsNullable = true
			case "precision":
				info.Columns[0].DateTimePrecision = 9
			case "type":
				info.Columns[0].DataType = "TIMESTAMP WITH TIME ZONE"
			case "missing snapshot":
				m.clearSourceTableDDLCache()
			}
			err = m.recheckPartitionSource(ctx, "T")
			if change == "missing snapshot" {
				require.ErrorContains(t, err, "冻结快照")
			} else {
				require.ErrorContains(t, err, "列类型、精度或可空性", "unchanged partition metadata cannot hide a changed proof domain")
			}
		})
	}
}

func TestKeySourceProofRechecksNullableAndNativeStorage(t *testing.T) {
	for _, nested := range []bool{false, true} {
		for _, change := range []string{"nullable", "unsigned", "missing snapshot"} {
			t.Run(fmt.Sprintf("nested=%t/%s", nested, change), func(t *testing.T) {
				spec := &source.PartitionSpec{Method: "KEY", Keys: []source.PartitionKey{{Column: "K", DataType: "smallint"}}}
				if nested {
					spec = &source.PartitionSpec{Method: "RANGE", Partitions: []source.Partition{{Name: "P0", Subpartition: spec}}}
				}
				info := &source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{{Name: "K", DataType: "smallint", FormattedType: "smallint"}}}
				r := &partitionFlowReader{mockReader: &mockReader{ddl: map[string]*source.TableDDLInfo{"T": info}}, specs: map[string]*source.PartitionSpec{"T": spec}, metadataCalls: map[string]int{}}
				m := NewMigrator(r, &mockWriter{}, &Job{LogCh: make(chan string, 10)}, Config{})
				m.resetSourceTableDDLCache()
				defer m.clearSourceTableDDLCache()
				_, err := m.sourceTableDDLInfo(context.Background(), "T")
				require.NoError(t, err)
				m.partitionState = &partitionRunState{tables: map[string]*PartitionTableResult{"T": {Source: source.ClonePartitionSpec(spec)}}}
				require.NoError(t, m.recheckPartitionSource(context.Background(), "T"))
				switch change {
				case "nullable":
					info.Columns[0].IsNullable = true
				case "unsigned":
					info.Columns[0].FormattedType = "smallint unsigned"
				case "missing snapshot":
					m.clearSourceTableDDLCache()
				}
				require.Error(t, m.recheckPartitionSource(context.Background(), "T"))
			})
		}
	}
}

func TestKeySourceSnapshotLivesThroughFinalRunVerification(t *testing.T) {
	for _, drift := range []bool{false, true} {
		t.Run(fmt.Sprint(drift), func(t *testing.T) {
			m, r, _ := newPartitionFlow(t, "Root")
			m.cfg.Content = "schema_only"
			r.ddl["Root"].Columns[0].DataType = "int"
			r.ddl["Root"].Columns[0].FormattedType = "int"
			spec := partitionFlowSpec()
			spec.Method, spec.Algorithm = "KEY", "mysql-key-innodb-2:8.0.36"
			spec.Keys[0].DataType = "int"
			for i := range spec.Partitions {
				p := &spec.Partitions[i]
				p.Lower, p.Upper = nil, nil
				p.LowerInclusive = false
				p.HashModulus, p.HashRemainder = len(spec.Partitions), i
			}
			r.specs["Root"] = spec
			r.metadataAt = func(table string, call int) *source.PartitionSpec {
				if drift && call == 3 {
					r.ddl[table].Columns[0].IsNullable = true
				}
				return r.specs[table]
			}
			report := m.Run(context.Background())
			require.Nil(t, m.sourceDDLCache.tables, "snapshot is released only after final verification")
			require.NotNil(t, report.PartitionMigration)
			result := report.PartitionMigration.Tables[0]
			if drift {
				require.Equal(t, "failed", result.Status)
				require.Contains(t, result.Error, "可空性")
			} else {
				require.False(t, report.HasFailures(), "%+v", result)
				require.Equal(t, "succeeded", result.Status)
			}
		})
	}
}

func TestKeyTargetColumnDriftBlocksAppendBeforeAnyWrite(t *testing.T) {
	m, r, w := newPartitionFlow(t, "Root")
	m.cfg.Content = "data_only"
	r.ddl["Root"].Columns[0].DataType = "int"
	r.ddl["Root"].Columns[0].FormattedType = "int"
	spec := partitionFlowSpec()
	spec.Method, spec.Algorithm = "KEY", "mysql-key-innodb-2:8.0.36"
	spec.Keys[0].DataType = "int"
	for i := range spec.Partitions {
		p := &spec.Partitions[i]
		p.Lower, p.Upper = nil, nil
		p.LowerInclusive = false
		p.HashModulus, p.HashRemainder = len(spec.Partitions), i
	}
	r.specs["Root"] = spec
	w.keyColumnErr = errors.New("目标 KEY 列可空性与冻结计划不同")
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.Equal(t, "blocked", report.PartitionMigration.Tables[0].Status)
	require.Contains(t, report.PartitionMigration.Tables[0].Error, "可空性")
	require.Empty(t, w.created)
	require.Empty(t, w.copied)
	require.Empty(t, r.streamed)
}
