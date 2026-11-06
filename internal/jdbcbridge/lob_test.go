package jdbcbridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
	"io"
	"math"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type recordedBatchRequest struct {
	op   byte
	size int
	rows uint32
}

type recordingBatchPipe struct {
	t        *testing.T
	manager  *Manager
	buffer   []byte
	requests []recordedBatchRequest
	handle   func(byte, *decoder) error
}

func newRecordingBatchManager(t *testing.T) (*Manager, *recordingBatchPipe) {
	m := NewManager(Config{})
	pipe := &recordingBatchPipe{t: t, manager: m}
	m.stdin = pipe
	// Existing-session requests must not attempt to launch a process.
	m.cmd = &exec.Cmd{Process: &os.Process{Pid: 1}}
	return m, pipe
}

func (p *recordingBatchPipe) Close() error { return nil }

func (p *recordingBatchPipe) Write(data []byte) (int, error) {
	p.buffer = append(p.buffer, data...)
	for len(p.buffer) >= 4 {
		n := int(binary.BigEndian.Uint32(p.buffer))
		if len(p.buffer) < 4+n {
			break
		}
		frame := p.buffer[4 : 4+n]
		d := newDecoder(frame)
		version, _ := d.byte()
		require.Equal(p.t, protocolVersion, version)
		op, _ := d.byte()
		id, _ := d.uint64()
		record := recordedBatchRequest{op: op, size: n}
		if op == opBatchAdd {
			peek := newDecoder(frame[10:])
			_, _ = peek.uint64()
			record.rows, _ = peek.uint32()
		}
		p.requests = append(p.requests, record)
		var err error
		if p.handle != nil {
			err = p.handle(op, d)
		}
		p.manager.pendingMu.Lock()
		ch := p.manager.pending[id]
		delete(p.manager.pending, id)
		p.manager.pendingMu.Unlock()
		if ch != nil {
			ch <- response{err: err}
		}
		p.buffer = p.buffer[4+n:]
	}
	return len(data), nil
}

func TestEncodedValueSizeMatchesWire(t *testing.T) {
	values := []any{nil, true, false, int(-1), int8(-2), int16(-3), int32(-4), int64(-5),
		uint(6), uint8(7), uint16(8), uint32(9), uint64(math.MaxUint64), float32(1.25), 1.5,
		"中文\x00", []byte{0, 1}, Decimal("123.450"), Date("2026-11-06"), Time("01:02:03.4"),
		Timestamp("2026-11-06 01:02:03.4"), time.Date(2026, 11, 6, 1, 2, 3, 4, time.UTC), lobRef(99)}
	for _, value := range values {
		e := &encoder{}
		require.NoError(t, encodeValue(e, value))
		n, err := encodedValueSize(value)
		require.NoError(t, err)
		require.Equal(t, int64(e.Len()), n, "%T", value)
	}
}

func TestAddBatchBoundsFullEncodedFramesAndRowCounts(t *testing.T) {
	m, pipe := newRecordingBatchManager(t)
	// This row fills the eight-MiB frame exactly, including every header.
	exact := strings.Repeat("x", batchMaxFrameBytes-batchFrameOverhead-4-5)
	rows := [][]any{{exact}, {"next"}}
	for i := 0; i < 2001; i++ {
		rows = append(rows, []any{int64(i), nil, uint64(math.MaxUint64)})
	}
	require.NoError(t, m.AddBatch(context.Background(), 7, rows))
	total := 0
	for _, request := range pipe.requests {
		require.Equal(t, opBatchAdd, request.op)
		require.LessOrEqual(t, request.size, batchMaxFrameBytes)
		require.LessOrEqual(t, request.rows, uint32(batchMaxRows))
		total += int(request.rows)
	}
	require.Equal(t, len(rows), total)
	require.Equal(t, batchMaxFrameBytes, pipe.requests[0].size)
}

func TestAddBatchPageBeyondFrameLimitUsesOrderedNormalFrames(t *testing.T) {
	m, pipe := newRecordingBatchManager(t)
	value := strings.Repeat("x", 7<<20)
	rows := make([][]any, 10) // Seventy MiB in total, with each row below eight MiB.
	for i := range rows {
		rows[i] = []any{int64(i), value}
	}
	var order []int64
	pipe.handle = func(op byte, d *decoder) error {
		require.Equal(t, opBatchAdd, op, "ordinary rows must not need LOB uploads")
		_, _ = d.uint64()
		count, _ := d.uint32()
		for i := uint32(0); i < count; i++ {
			columns, _ := d.uint32()
			require.Equal(t, uint32(2), columns)
			tag, _ := d.byte()
			require.Equal(t, valueInt64, tag)
			id, _ := d.int64()
			order = append(order, id)
			tag, _ = d.byte()
			require.Equal(t, valueString, tag)
			n, _ := d.uint32()
			require.Equal(t, uint32(len(value)), n)
			_, err := d.Seek(int64(n), io.SeekCurrent)
			require.NoError(t, err)
		}
		require.Zero(t, d.Len())
		return nil
	}
	require.NoError(t, m.AddBatch(context.Background(), 7, rows))
	require.Equal(t, []int64{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}, order)
	require.Len(t, pipe.requests, 10)
	for _, request := range pipe.requests {
		require.LessOrEqual(t, request.size, batchMaxFrameBytes)
	}
}

func TestAddBatchStreamsRowBeyondFrameLimit(t *testing.T) {
	m, pipe := newRecordingBatchManager(t)
	// The UTF-8 representation crosses a one-MiB boundary inside a character.
	text := strings.Repeat("中文", 15_213_482) // 91,280,892 bytes.
	wantHash := sha256.Sum256([]byte(text))
	var digest hash.Hash
	var lobID uint64
	var received, total int64
	refs := 0
	pipe.handle = func(op byte, d *decoder) error {
		batch, err := d.uint64()
		require.NoError(t, err)
		require.Equal(t, uint64(7), batch)
		switch op {
		case opLOBBegin:
			lobID, _ = d.uint64()
			kind, _ := d.byte()
			require.Equal(t, lobText, kind)
			total, _ = d.int64()
			digest = sha256.New()
		case opLOBChunk:
			id, _ := d.uint64()
			require.Equal(t, lobID, id)
			offset, _ := d.int64()
			require.Equal(t, received, offset)
			n, _ := d.uint32()
			require.LessOrEqual(t, n, uint32(lobChunkBytes))
			copied, err := io.CopyN(digest, d.Reader, int64(n))
			require.NoError(t, err)
			received += copied
		case opLOBEnd:
			id, _ := d.uint64()
			require.Equal(t, lobID, id)
			require.Equal(t, total, received)
			require.Equal(t, wantHash[:], digest.Sum(nil))
		case opBatchAdd:
			n, _ := d.uint32()
			require.Equal(t, uint32(1), n)
			columns, _ := d.uint32()
			require.Equal(t, uint32(2), columns)
			tag, _ := d.byte()
			require.Equal(t, valueInt64, tag)
			_, _ = d.int64()
			tag, _ = d.byte()
			require.Equal(t, valueLOBRef, tag)
			id, _ := d.uint64()
			require.Equal(t, lobID, id)
			refs++
		default:
			t.Fatalf("unexpected operation %d", op)
		}
		return nil
	}
	row := []any{int64(1), text}
	require.NoError(t, m.AddBatch(context.Background(), 7, [][]any{row}))
	require.Equal(t, 1, refs)
	require.Equal(t, text, row[1], "caller-owned rows must not be changed")
	for _, request := range pipe.requests {
		require.LessOrEqual(t, request.size, batchMaxFrameBytes)
	}
}

func TestAddBatchStreamsMultipleLargeFieldsAndBinaryExactly(t *testing.T) {
	m, pipe := newRecordingBatchManager(t)
	binaryValue := bytes.Repeat([]byte{0, 255, 1}, 2<<20)
	text := strings.Repeat("x", 6<<20)
	var kinds []byte
	var ids []uint64
	var binaryDigest = sha256.New()
	pipe.handle = func(op byte, d *decoder) error {
		_, _ = d.uint64()
		switch op {
		case opLOBBegin:
			id, _ := d.uint64()
			ids = append(ids, id)
			kind, _ := d.byte()
			kinds = append(kinds, kind)
		case opLOBChunk:
			_, _ = d.uint64()
			_, _ = d.int64()
			n, _ := d.uint32()
			if kinds[len(kinds)-1] == lobBinary {
				_, err := io.CopyN(binaryDigest, d.Reader, int64(n))
				require.NoError(t, err)
			}
		}
		return nil
	}
	// All fields together exceed the normal budget; the two largest must
	// become references before the row can be transmitted.
	rows := [][]any{{binaryValue, text, strings.Repeat("y", 3<<20), nil, "", []byte{}}}
	require.NoError(t, m.AddBatch(context.Background(), 9, rows))
	require.Equal(t, []byte{lobBinary, lobText}, kinds)
	require.Len(t, ids, 2)
	require.NotEqual(t, ids[0], ids[1])
	want := sha256.Sum256(binaryValue)
	require.Equal(t, want[:], binaryDigest.Sum(nil))
	for _, request := range pipe.requests {
		require.LessOrEqual(t, request.size, batchMaxFrameBytes)
	}
}

func TestAddBatchLOBFailureStopsBeforeInsertOrCommit(t *testing.T) {
	m, pipe := newRecordingBatchManager(t)
	failure := errors.New("LOB disk write failed")
	pipe.handle = func(op byte, _ *decoder) error {
		if op == opLOBChunk {
			return failure
		}
		return nil
	}
	err := m.AddBatch(context.Background(), 7, [][]any{{strings.Repeat("x", batchMaxFrameBytes)}})
	require.ErrorIs(t, err, failure)
	require.ErrorContains(t, err, "row 1: column 1 LOB upload")
	require.Equal(t, []recordedBatchRequest{{opLOBBegin, 35, 0}, {opLOBChunk, 10 + 8 + 8 + 8 + 4 + lobChunkBytes, 0}}, pipe.requests)
}

func TestAddBatchCanceledAndUnsupportedValuesDoNotSend(t *testing.T) {
	m, pipe := newRecordingBatchManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, m.AddBatch(ctx, 7, [][]any{{"value"}}), context.Canceled)
	secret := "secret-do-not-include"
	err := m.AddBatch(context.Background(), 7, [][]any{{struct{ Secret string }{secret}}})
	require.ErrorContains(t, err, "unsupported JDBC bridge value type")
	require.NotContains(t, err.Error(), secret)
	require.Empty(t, pipe.requests)
	require.NoError(t, m.AddBatch(context.Background(), 7, nil))
}

func TestAddBatchCancellationDuringLOBStopsUpload(t *testing.T) {
	m, pipe := newRecordingBatchManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pipe.handle = func(op byte, _ *decoder) error {
		if op == opLOBChunk {
			cancel()
			return context.Canceled
		}
		return nil
	}
	err := m.AddBatch(ctx, 7, [][]any{{strings.Repeat("x", batchMaxFrameBytes)}})
	require.ErrorIs(t, err, context.Canceled)
	for _, request := range pipe.requests {
		require.Contains(t, []byte{opLOBBegin, opLOBChunk, opCancel}, request.op)
	}
	require.LessOrEqual(t, len(pipe.requests), 3)
}

func TestConcurrentBatchLOBIDsAreUnique(t *testing.T) {
	m, pipe := newRecordingBatchManager(t)
	ids := make(map[uint64]uint64)
	pipe.handle = func(op byte, d *decoder) error {
		if op == opLOBBegin {
			batch, _ := d.uint64()
			id, _ := d.uint64()
			// All pipe writes are serialized by Manager.writeMu.
			require.NotContains(t, ids, id)
			ids[id] = batch
		}
		return nil
	}
	value := strings.Repeat("x", batchMaxFrameBytes)
	var wg sync.WaitGroup
	errors := make(chan error, 4)
	for batch := uint64(1); batch <= 4; batch++ {
		wg.Add(1)
		go func(batch uint64) {
			defer wg.Done()
			errors <- m.AddBatch(context.Background(), batch, [][]any{{value}})
		}(batch)
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	require.Len(t, ids, 4)
}
