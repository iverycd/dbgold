package jdbcbridge

import (
	"context"
	"fmt"
	"sort"
	"time"
)

const (
	batchMaxRows       = 1000
	batchMaxFrameBytes = 8 << 20
	lobChunkBytes      = 1 << 20
	// version, opcode, request ID, batch ID and row count.
	batchFrameOverhead      = 1 + 1 + 8 + 8 + 4
	lobText            byte = 1
	lobBinary          byte = 2
)

// encodedValueSize mirrors encodeValue without materializing large payloads.
func encodedValueSize(value any) (int64, error) {
	switch v := value.(type) {
	case nil:
		return 1, nil
	case bool:
		return 2, nil
	case int, int8, int16, int32, int64, float32, float64, lobRef:
		return 9, nil
	case uint, uint8, uint16, uint32, uint64:
		return 5 + int64(len(fmt.Sprint(v))), nil
	case string:
		return 5 + int64(len(v)), nil
	case []byte:
		return 5 + int64(len(v)), nil
	case Decimal:
		return 5 + int64(len(v)), nil
	case Date:
		return 5 + int64(len(v)), nil
	case Time:
		return 5 + int64(len(v)), nil
	case Timestamp:
		return 5 + int64(len(v)), nil
	case time.Time:
		return 5 + int64(len(v.Format("2006-01-02 15:04:05.999999999"))), nil
	default:
		return 0, fmt.Errorf("unsupported JDBC bridge value type %T", value)
	}
}

func encodedRowSize(row []any) (int64, error) {
	size := int64(4) // column count
	for i, value := range row {
		n, err := encodedValueSize(value)
		if err != nil {
			return 0, fmt.Errorf("column %d: %w", i+1, err)
		}
		size += n
	}
	return size, nil
}

// AddBatch sends bounded requests within the caller's existing transaction.
// A large row is sent on its own and its largest string/byte values are spooled
// by the JVM, so neither a full LOB nor a source page becomes a bridge frame.
// Errors leave the transaction uncommitted; the caller must AbortBatch.
func (m *Manager) AddBatch(ctx context.Context, batch uint64, rows [][]any) error {
	chunk := make([][]any, 0, min(len(rows), batchMaxRows))
	size := int64(batchFrameOverhead)
	flush := func() error {
		if len(chunk) == 0 {
			return nil
		}
		if err := m.addBatchRows(ctx, batch, chunk); err != nil {
			return err
		}
		chunk = chunk[:0]
		size = batchFrameOverhead
		return nil
	}
	for i, row := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		rowSize, err := encodedRowSize(row)
		if err != nil {
			return fmt.Errorf("Oscar batch row %d: %w", i+1, err)
		}
		if rowSize+batchFrameOverhead > batchMaxFrameBytes {
			if err := flush(); err != nil {
				return err
			}
			prepared, err := m.uploadLargeRow(ctx, batch, row, rowSize)
			if err != nil {
				return fmt.Errorf("Oscar batch row %d: %w", i+1, err)
			}
			// Execute immediately so outstanding LOB files are bounded by one
			// large row, even when a page contains many large rows.
			if err := m.addBatchRows(ctx, batch, [][]any{prepared}); err != nil {
				return err
			}
			continue
		}
		if len(chunk) >= batchMaxRows || size+rowSize > batchMaxFrameBytes {
			if err := flush(); err != nil {
				return err
			}
		}
		chunk = append(chunk, row)
		size += rowSize
	}
	return flush()
}

func (m *Manager) uploadLargeRow(ctx context.Context, batch uint64, row []any, size int64) ([]any, error) {
	type candidate struct {
		column int
		size   int64
	}
	candidates := make([]candidate, 0)
	for column, value := range row {
		switch value.(type) {
		case string, []byte:
			n, _ := encodedValueSize(value)
			if n > 9 { // A LOB reference uses one tag byte and an eight-byte ID.
				candidates = append(candidates, candidate{column, n})
			}
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].size > candidates[j].size })
	// Check feasibility before creating any uploads.
	minimum := size
	for _, candidate := range candidates {
		minimum -= candidate.size - 9
	}
	if minimum+batchFrameOverhead > batchMaxFrameBytes {
		return nil, fmt.Errorf("encoded row cannot fit within %d bytes after LOB streaming", batchMaxFrameBytes)
	}
	prepared := append([]any(nil), row...)
	for _, candidate := range candidates {
		if size+batchFrameOverhead <= batchMaxFrameBytes {
			break
		}
		ref, err := m.uploadLOB(ctx, batch, row[candidate.column])
		if err != nil {
			return nil, fmt.Errorf("column %d LOB upload: %w", candidate.column+1, err)
		}
		prepared[candidate.column] = ref
		size -= candidate.size - 9
	}
	return prepared, nil
}

func (m *Manager) uploadLOB(ctx context.Context, batch uint64, value any) (lobRef, error) {
	id := m.nextLOBID.Add(1)
	kind := lobText
	var total int
	switch v := value.(type) {
	case string:
		total = len(v)
	case []byte:
		kind, total = lobBinary, len(v)
	default:
		return 0, fmt.Errorf("unsupported LOB value type %T", value)
	}
	begin := &encoder{}
	begin.uint64(batch)
	begin.uint64(id)
	begin.byte(kind)
	begin.int64(int64(total))
	if _, err := m.requestExisting(ctx, opLOBBegin, begin.Bytes()); err != nil {
		return 0, err
	}
	for offset := 0; offset < total; {
		end := offset + min(lobChunkBytes, total-offset)
		chunk := &encoder{}
		chunk.uint64(batch)
		chunk.uint64(id)
		chunk.int64(int64(offset))
		switch v := value.(type) {
		case string:
			chunk.string(v[offset:end]) // Raw UTF-8 may cross chunk boundaries.
		case []byte:
			chunk.bytes(v[offset:end])
		}
		if _, err := m.requestExisting(ctx, opLOBChunk, chunk.Bytes()); err != nil {
			return 0, err
		}
		offset = end
	}
	end := &encoder{}
	end.uint64(batch)
	end.uint64(id)
	if _, err := m.requestExisting(ctx, opLOBEnd, end.Bytes()); err != nil {
		return 0, err
	}
	return lobRef(id), nil
}
