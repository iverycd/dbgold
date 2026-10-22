package validation

import (
	"bufio"
	"bytes"
	"container/heap"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
)

const sortMemoryBytes = 64 << 20
const mergeFanIn = 32
const recordBytes = 64

type record struct{ key, row [32]byte }

func compareRecord(a, b record) int {
	if c := bytes.Compare(a.key[:], b.key[:]); c != 0 {
		return c
	}
	return bytes.Compare(a.row[:], b.row[:])
}

type externalSorter struct {
	dir             string
	buffer          []record
	paths           []string
	sequence        int
	capacity, fanIn int
}

func newSorter(dir string, capacity, fanIn int) *externalSorter {
	if capacity < 1 {
		capacity = 1
	}
	if fanIn < 2 {
		fanIn = 2
	}
	return &externalSorter{dir: dir, capacity: capacity, fanIn: fanIn, buffer: make([]record, 0, min(capacity, 4096))}
}
func (s *externalSorter) add(ctx context.Context, r record) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	if len(s.buffer) == cap(s.buffer) {
		nextCapacity := min(s.capacity, max(4096, cap(s.buffer)*2))
		grown := make([]record, len(s.buffer), nextCapacity)
		copy(grown, s.buffer)
		s.buffer = grown
	}
	s.buffer = append(s.buffer, r)
	if len(s.buffer) == s.capacity {
		return s.flush(ctx)
	}
	return nil
}
func (s *externalSorter) nextPath() string {
	s.sequence++
	return filepath.Join(s.dir, fmt.Sprintf("run-%08d", s.sequence))
}
func (s *externalSorter) flush(ctx context.Context) error {
	if len(s.buffer) == 0 {
		return nil
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	slices.SortFunc(s.buffer, compareRecord)
	path := s.nextPath()
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	w := bufio.NewWriterSize(f, 64<<10)
	for i, r := range s.buffer {
		if i%1024 == 0 {
			if e = ctx.Err(); e != nil {
				break
			}
		}
		if e = writeRecord(w, r); e != nil {
			break
		}
	}
	if e == nil {
		e = w.Flush()
	}
	if e2 := f.Close(); e == nil {
		e = e2
	}
	if e != nil {
		_ = os.Remove(path)
		return e
	}
	s.paths = append(s.paths, path)
	s.buffer = s.buffer[:0]
	return nil
}
func (s *externalSorter) finish(ctx context.Context) (string, error) {
	if e := s.flush(ctx); e != nil {
		return "", e
	}
	if len(s.paths) == 0 {
		path := s.nextPath()
		f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return "", e
		}
		if e = f.Close(); e != nil {
			return "", e
		}
		s.paths = append(s.paths, path)
	}
	for len(s.paths) > 1 {
		var next []string
		for i := 0; i < len(s.paths); i += s.fanIn {
			if e := ctx.Err(); e != nil {
				return "", e
			}
			end := min(i+s.fanIn, len(s.paths))
			group := s.paths[i:end]
			if len(group) == 1 {
				next = append(next, group[0])
				continue
			}
			out := s.nextPath()
			if e := mergeRuns(ctx, group, out); e != nil {
				return "", e
			}
			next = append(next, out)
			for _, p := range group {
				if e := os.Remove(p); e != nil {
					return "", e
				}
			}
		}
		s.paths = next
	}
	path := s.paths[0]
	s.paths = nil
	s.buffer = nil
	return path, nil
}
func writeRecord(w io.Writer, r record) error {
	var b [recordBytes]byte
	copy(b[:32], r.key[:])
	copy(b[32:], r.row[:])
	_, e := w.Write(b[:])
	return e
}
func readRecord(r io.Reader) (record, error) {
	var out record
	var b [recordBytes]byte
	_, e := io.ReadFull(r, b[:])
	if e != nil {
		return out, e
	}
	copy(out.key[:], b[:32])
	copy(out.row[:], b[32:])
	return out, nil
}

type mergeItem struct {
	value  record
	reader int
}
type mergeHeap []mergeItem

func (h mergeHeap) Len() int           { return len(h) }
func (h mergeHeap) Less(i, j int) bool { return compareRecord(h[i].value, h[j].value) < 0 }
func (h mergeHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *mergeHeap) Push(x any)        { *h = append(*h, x.(mergeItem)) }
func (h *mergeHeap) Pop() any          { n := len(*h); x := (*h)[n-1]; *h = (*h)[:n-1]; return x }
func mergeRuns(ctx context.Context, paths []string, out string) (err error) {
	files := make([]*os.File, 0, len(paths))
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()
	readers := make([]*bufio.Reader, 0, len(paths))
	var h mergeHeap
	for i, p := range paths {
		f, e := os.Open(p)
		if e != nil {
			return e
		}
		files = append(files, f)
		r := bufio.NewReaderSize(f, 64<<10)
		readers = append(readers, r)
		v, e := readRecord(r)
		if e == io.EOF {
			continue
		}
		if e != nil {
			return e
		}
		heap.Push(&h, mergeItem{v, i})
	}
	f, e := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer func() {
		if e := f.Close(); err == nil {
			err = e
		}
		if err != nil {
			_ = os.Remove(out)
		}
	}()
	w := bufio.NewWriterSize(f, 64<<10)
	for h.Len() > 0 {
		if e = ctx.Err(); e != nil {
			return e
		}
		item := heap.Pop(&h).(mergeItem)
		if e = writeRecord(w, item.value); e != nil {
			return e
		}
		next, e := readRecord(readers[item.reader])
		if e == io.EOF {
			continue
		}
		if e != nil {
			return e
		}
		heap.Push(&h, mergeItem{next, item.reader})
	}
	return w.Flush()
}
