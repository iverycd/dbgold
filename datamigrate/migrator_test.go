// datamigrate/migrator_test.go
package datamigrate

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/assert"
)

// mockReader 实现 source.Reader 接口，用于测试
type mockReader struct {
	tables       []string
	ddl          map[string]*source.TableDDLInfo
	pk           map[string][]string
	rows         map[string][][]interface{}
	triggerCount int
}

func (m *mockReader) DBType() string                                 { return "mysql" }
func (m *mockReader) Close() error                                   { return nil }
func (m *mockReader) ListTables(_ context.Context) ([]string, error) { return m.tables, nil }
func (m *mockReader) GetTableDDLInfo(_ context.Context, t string) (*source.TableDDLInfo, error) {
	return m.ddl[t], nil
}
func (m *mockReader) GetPrimaryKey(_ context.Context, t string) ([]string, error) {
	return m.pk[t], nil
}
func (m *mockReader) ReadPage(_ context.Context, t string, _ []string, offset, _ int64) ([]string, [][]interface{}, error) {
	if offset > 0 {
		return []string{"id"}, nil, nil
	}
	rows := m.rows[t]
	if len(rows) == 0 {
		return []string{"id"}, nil, nil
	}
	return []string{"id"}, rows, nil
}
func (m *mockReader) GetSequences(_ context.Context) ([]source.SequenceInfo, error) { return nil, nil }
func (m *mockReader) GetPrimaryKeys(_ context.Context) ([]source.IndexInfo, error)  { return nil, nil }
func (m *mockReader) GetIndexes(_ context.Context) ([]source.IndexInfo, error)      { return nil, nil }
func (m *mockReader) GetForeignKeys(_ context.Context) ([]source.FKInfo, error)     { return nil, nil }
func (m *mockReader) GetViews(_ context.Context) ([]source.ViewInfo, error)         { return nil, nil }
func (m *mockReader) GetTriggerCount(_ context.Context) (int, error)                { return m.triggerCount, nil }
func (m *mockReader) CountRows(_ context.Context, _ string) (int64, error)          { return 0, nil }
func (m *mockReader) ListDatabases(_ context.Context) ([]string, error)             { return nil, nil }

// mockWriter 实现 target.Writer 接口，用于测试
type mockWriter struct {
	created         []string
	copied          []string
	createTableFail bool
	copyDataFail    bool
}

func (m *mockWriter) DBType() string { return "postgres" }
func (m *mockWriter) Close() error   { return nil }
func (m *mockWriter) CreateTable(_ context.Context, ddl string) error {
	if m.createTableFail {
		return fmt.Errorf("create table failed")
	}
	m.created = append(m.created, ddl)
	return nil
}
func (m *mockWriter) CopyData(_ context.Context, table string, _ []string, _ [][]interface{}) error {
	if m.copyDataFail {
		return fmt.Errorf("copy data failed")
	}
	m.copied = append(m.copied, table)
	return nil
}
func (m *mockWriter) CreateSequence(_ context.Context, _ source.SequenceInfo) error { return nil }
func (m *mockWriter) CreateIndex(_ context.Context, _ source.IndexInfo) error       { return nil }
func (m *mockWriter) CreateForeignKey(_ context.Context, _ source.FKInfo) error     { return nil }
func (m *mockWriter) CreateView(_ context.Context, _ source.ViewInfo) error         { return nil }
func (m *mockWriter) AlterDistribute(_ context.Context, _ string, _ []string) error { return nil }
func (m *mockWriter) CountRows(_ context.Context, _ string) (int64, error)          { return 0, nil }

func newTestMigrator(reader source.Reader, writer target.Writer) (*Migrator, *Job) {
	_, cancel := context.WithCancel(context.Background())
	job := &Job{
		LogCh:  make(chan string, 512),
		Cancel: cancel,
	}
	cfg := Config{PageSize: 10, MaxParallel: 2, Mode: "all", Filter: ""}
	return NewMigrator(reader, writer, job, cfg), job
}

func drainLogs(job *Job) []string {
	close(job.LogCh)
	var logs []string
	for l := range job.LogCh {
		logs = append(logs, l)
	}
	return logs
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
		pk:           map[string][]string{"users": {"id"}},
		rows:         map[string][][]interface{}{"users": {{1, "Alice"}, {2, "Bob"}}},
		triggerCount: 3,
	}
	writer := &mockWriter{}
	m, job := newTestMigrator(reader, writer)

	report := m.Run(context.Background())
	logs := drainLogs(job)

	assert.Contains(t, writer.copied, "users")
	assert.Equal(t, 1, report.Tables.Total)
	assert.Equal(t, 1, report.Tables.Success)
	assert.Equal(t, 0, report.Tables.Failed)
	assert.Equal(t, 1, report.Data.Success)
	assert.Equal(t, 0, report.Data.Failed)
	assert.Equal(t, 3, report.Triggers.Total)

	hasDone := false
	for _, l := range logs {
		if strings.Contains(l, "[DONE]") {
			hasDone = true
		}
	}
	assert.True(t, hasDone, "should emit [DONE] log")
}

func TestMigratorRun_TableCreationFailed(t *testing.T) {
	reader := &mockReader{
		tables: []string{"users"},
		ddl: map[string]*source.TableDDLInfo{
			"users": {TableName: "users", Columns: []source.ColumnInfo{
				{Name: "id", DataType: "int", IsNullable: false},
			}},
		},
		pk: map[string][]string{},
	}
	writer := &mockWriter{createTableFail: true}
	m, job := newTestMigrator(reader, writer)

	report := m.Run(context.Background())
	drainLogs(job)

	assert.Equal(t, 1, report.Tables.Failed)
	assert.Equal(t, 0, report.Tables.Success)
	assert.Equal(t, 1, len(report.Tables.Items))
	assert.Equal(t, "users", report.Tables.Items[0].Name)
	assert.NotEmpty(t, report.Tables.Items[0].Error)
	assert.Equal(t, 0, report.Data.Total)
}

func TestMigratorRun_DataWriteFailed(t *testing.T) {
	reader := &mockReader{
		tables: []string{"users"},
		ddl: map[string]*source.TableDDLInfo{
			"users": {TableName: "users", Columns: []source.ColumnInfo{
				{Name: "id", DataType: "int", IsNullable: false},
			}},
		},
		pk:   map[string][]string{"users": {"id"}},
		rows: map[string][][]interface{}{"users": {{1}, {2}}},
	}
	writer := &mockWriter{copyDataFail: true}
	m, job := newTestMigrator(reader, writer)

	report := m.Run(context.Background())
	drainLogs(job)

	assert.Equal(t, 0, report.Tables.Failed)
	assert.Equal(t, 1, report.Data.Failed)
	assert.Equal(t, 1, len(report.Data.Items))
	assert.Equal(t, "users", report.Data.Items[0].Name)
	assert.NotEmpty(t, report.Data.Items[0].Error)
	assert.Empty(t, report.Data.Items[0].DDL)
}

func TestMigratorRun_ContextCancelled(t *testing.T) {
	reader := &mockReader{tables: []string{"users"}, ddl: map[string]*source.TableDDLInfo{
		"users": {TableName: "users"},
	}, pk: map[string][]string{}}
	writer := &mockWriter{}
	m, job := newTestMigrator(reader, writer)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	m.Run(ctx)
	logs := drainLogs(job)

	hasCancelled := false
	for _, l := range logs {
		if strings.Contains(l, "取消") || strings.Contains(l, "cancelled") {
			hasCancelled = true
		}
	}
	assert.True(t, hasCancelled)
}
