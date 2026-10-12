package datamigrate

import (
	"context"
	"strings"
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

type mysqlDMDateReader struct{ *mockReader }

func (r *mysqlDMDateReader) ReadPage(_ context.Context, table string, _ []string, offset, _ int64) ([]string, []string, [][]interface{}, error) {
	if offset > 0 {
		return []string{"equipment_validity"}, []string{"TIMESTAMP"}, nil, nil
	}
	return []string{"equipment_validity"}, []string{"TIMESTAMP"}, r.rows[table], nil
}

func TestMySQLDamengMigrationModes(t *testing.T) {
	for _, mode := range []string{"schema_only", "data_only", "both"} {
		t.Run(mode, func(t *testing.T) {
			zero := "0000-00-00 00:00:00"
			r := &mysqlDMDateReader{mockReader: &mockReader{
				tables: []string{"device_baseinfo"},
				ddl: map[string]*source.TableDDLInfo{"device_baseinfo": {
					TableName: "device_baseinfo", Columns: []source.ColumnInfo{
						{Name: "equipment_validity", DataType: "timestamp", Default: &zero},
					},
				}},
				rows: map[string][][]interface{}{"device_baseinfo": {{zero}}},
			}}
			w := &dmFixtureWriter{mockWriter: &mockWriter{}}
			m, job := newTestMigrator(r, w)
			m.cfg.Content = mode
			m.cfg.TargetSchema = "ZNFXPT"
			report := m.Run(context.Background())
			require.False(t, report.HasFailures())
			if mode == "data_only" {
				require.Empty(t, w.created)
			} else {
				require.Len(t, w.created, 1)
				require.Contains(t, w.created[0], `"EQUIPMENT_VALIDITY" TIMESTAMP NOT NULL DEFAULT '1900-01-01 00:00:00'`)
			}
			if mode == "schema_only" {
				require.Empty(t, w.copied)
			} else {
				require.Equal(t, []string{"device_baseinfo"}, w.copied)
			}
			require.Equal(t, "0000-00-00 00:00:00", zero, "source metadata must not be mutated")
			require.Contains(t, strings.Join(drainLogs(job), "\n"), "零日期数据统一替换为 1900-01-01 00:00:00")
		})
	}
}
