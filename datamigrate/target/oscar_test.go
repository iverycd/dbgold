package target

import (
	"context"
	"errors"
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/valueconv"
	"dbgold/internal/jdbcbridge"
)

type fakeOscarBridge struct {
	calls      []string
	rows       int
	failAdd    bool
	lastSQL    string
	lastSchema string
}

func (f *fakeOscarBridge) OpenSession(context.Context, jdbcbridge.SessionOptions) (uint64, error) {
	return 1, nil
}
func (f *fakeOscarBridge) Exec(_ context.Context, _ uint64, sql string) error {
	f.calls = append(f.calls, "exec")
	f.lastSQL = sql
	return nil
}
func (f *fakeOscarBridge) QueryInt64(context.Context, uint64, string) (int64, error) {
	return 42, nil
}
func (f *fakeOscarBridge) SchemaExists(_ context.Context, _ uint64, schema string) (bool, error) {
	f.lastSchema = schema
	return true, nil
}
func (f *fakeOscarBridge) BeginBatch(_ context.Context, _ uint64, sql string) (uint64, error) {
	f.calls = append(f.calls, "begin")
	f.lastSQL = sql
	return 9, nil
}
func (f *fakeOscarBridge) AddBatch(_ context.Context, _ uint64, rows [][]any) error {
	f.calls = append(f.calls, "add")
	f.rows += len(rows)
	if f.failAdd {
		return errors.New("batch failed")
	}
	return nil
}
func (f *fakeOscarBridge) CommitBatch(context.Context, uint64) error {
	f.calls = append(f.calls, "commit")
	return nil
}
func (f *fakeOscarBridge) AbortBatch(context.Context, uint64) error {
	f.calls = append(f.calls, "abort")
	return nil
}
func (f *fakeOscarBridge) CloseSession(context.Context, uint64) error { return nil }

func newFakeOscarWriter(bridge oscarBridge) *OscarWriter {
	return &OscarWriter{bridge: bridge, session: 1, schema: "业务", srcType: "mysql", valueConv: valueconv.NewOscar(), dia: dialect.NewOscar()}
}

func TestOscarWriterBatchCommitOrderAndChunking(t *testing.T) {
	fake := &fakeOscarBridge{}
	w := newFakeOscarWriter(fake)
	rows := make([][]interface{}, 1001)
	for i := range rows {
		rows[i] = []interface{}{int64(i), "name"}
	}
	if err := w.CopyData(context.Background(), "用户", []string{"id", "名称"}, []string{"BIGINT", "VARCHAR"}, rows); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fake.calls, ","); got != "begin,add,add,commit" {
		t.Fatalf("unexpected call order: %s", got)
	}
	if fake.rows != 1001 || !strings.Contains(fake.lastSQL, `INSERT INTO "业务"."用户"`) {
		t.Fatalf("unexpected batch: rows=%d sql=%s", fake.rows, fake.lastSQL)
	}
}

func TestOscarWriterBatchFailureRollsBack(t *testing.T) {
	fake := &fakeOscarBridge{failAdd: true}
	w := newFakeOscarWriter(fake)
	err := w.CopyData(context.Background(), "t", []string{"id"}, []string{"BIGINT"}, [][]interface{}{{1}})
	if err == nil {
		t.Fatal("expected batch error")
	}
	if got := strings.Join(fake.calls, ","); got != "begin,add,abort" {
		t.Fatalf("unexpected call order: %s", got)
	}
}

func TestOscarWriterMethodsUseBridge(t *testing.T) {
	fake := &fakeOscarBridge{}
	w := newFakeOscarWriter(fake)
	count, err := w.CountRows(context.Background(), "T")
	if err != nil || count != 42 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	exists, err := w.SchemaExists(context.Background(), "业务")
	if err != nil || !exists || fake.lastSchema != "业务" {
		t.Fatalf("exists=%v schema=%s err=%v", exists, fake.lastSchema, err)
	}
}
