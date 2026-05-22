// datamigrate/migrator_test.go
package datamigrate

import (
	"context"
	"strings"
	"testing"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/assert"
)

// mockReader 实现 source.Reader 接口，用于测试
type mockReader struct {
	tables []string
	ddl    map[string]*source.TableDDLInfo
	pk     map[string]string
	rows   map[string][][]interface{}
}

func (m *mockReader) DBType() string { return "mysql" }
func (m *mockReader) Close() error   { return nil }
func (m *mockReader) ListTables(_ context.Context) ([]string, error) { return m.tables, nil }
func (m *mockReader) GetTableDDLInfo(_ context.Context, t string) (*source.TableDDLInfo, error) {
	return m.ddl[t], nil
}
func (m *mockReader) GetPrimaryKey(_ context.Context, t string) (string, error) {
	return m.pk[t], nil
}
func (m *mockReader) ReadPage(_ context.Context, t, _ string, _, _ int64) ([]string, [][]interface{}, error) {
	rows := m.rows[t]
	if len(rows) == 0 {
		return []string{"id"}, nil, nil
	}
	return []string{"id"}, rows, nil
}
func (m *mockReader) GetSequences(_ context.Context) ([]source.SequenceInfo, error) { return nil, nil }
func (m *mockReader) GetIndexes(_ context.Context) ([]source.IndexInfo, error)      { return nil, nil }
func (m *mockReader) GetForeignKeys(_ context.Context) ([]source.FKInfo, error)     { return nil, nil }
func (m *mockReader) GetViews(_ context.Context) ([]source.ViewInfo, error)          { return nil, nil }
func (m *mockReader) GetTriggerCount(_ context.Context) (int, error)                { return 2, nil }

// mockWriter 实现 target.Writer 接口，用于测试
type mockWriter struct {
	created []string
	copied  []string
}

func (m *mockWriter) DBType() string { return "postgres" }
func (m *mockWriter) Close() error   { return nil }
func (m *mockWriter) CreateTable(_ context.Context, ddl string) error {
	m.created = append(m.created, ddl)
	return nil
}
func (m *mockWriter) CopyData(_ context.Context, table string, _ []string, _ [][]interface{}) error {
	m.copied = append(m.copied, table)
	return nil
}
func (m *mockWriter) CreateSequence(_ context.Context, _ source.SequenceInfo) error { return nil }
func (m *mockWriter) CreateIndex(_ context.Context, _ source.IndexInfo) error       { return nil }
func (m *mockWriter) CreateForeignKey(_ context.Context, _ source.FKInfo) error     { return nil }
func (m *mockWriter) CreateView(_ context.Context, _ source.ViewInfo) error         { return nil }

func newTestMigrator(reader source.Reader, writer target.Writer) (*Migrator, *Job) {
	ctx, cancel := context.WithCancel(context.Background())
	job := &Job{
		LogCh:  make(chan string, 512),
		Cancel: cancel,
	}
	_ = ctx
	cfg := Config{
		PageSize:    10,
		MaxParallel: 2,
		Mode:        "all",
		Filter:      "",
	}
	return NewMigrator(reader, writer, job, cfg), job
}

func TestMigratorRun_AllTables(t *testing.T) {
	reader := &mockReader{
		tables: []string{"users"},
		ddl: map[string]*source.TableDDLInfo{
			"users": {TableName: "users", Columns: []source.ColumnInfo{
				{Name: "id", DataType: "int", IsNullable: false},
				{Name: "name", DataType: "varchar", Length: 100, IsNullable: true},
			}},
		},
		pk:   map[string]string{"users": "id"},
		rows: map[string][][]interface{}{"users": {{1, "Alice"}, {2, "Bob"}}},
	}
	writer := &mockWriter{}
	m, job := newTestMigrator(reader, writer)

	ctx := context.Background()
	m.Run(ctx)
	close(job.LogCh)

	var logs []string
	for l := range job.LogCh {
		logs = append(logs, l)
	}

	assert.Contains(t, writer.copied, "users")
	hasDone := false
	for _, l := range logs {
		if strings.Contains(l, "[DONE]") {
			hasDone = true
		}
	}
	assert.True(t, hasDone, "should emit [DONE] log")
}

func TestMigratorRun_ContextCancelled(t *testing.T) {
	reader := &mockReader{tables: []string{"users"}, ddl: map[string]*source.TableDDLInfo{
		"users": {TableName: "users"},
	}, pk: map[string]string{}}
	writer := &mockWriter{}
	m, job := newTestMigrator(reader, writer)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消

	m.Run(ctx)
	close(job.LogCh)

	var logs []string
	for l := range job.LogCh {
		logs = append(logs, l)
	}
	hasCancelled := false
	for _, l := range logs {
		if strings.Contains(l, "取消") || strings.Contains(l, "cancelled") {
			hasCancelled = true
		}
	}
	assert.True(t, hasCancelled)
}
