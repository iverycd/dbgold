package validation

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"time"
)

const maxSamples = 20

type options struct {
	records, fanIn int
	tempParent     string
}

// Validate performs a complete, order-independent comparison. Temporary files
// contain fixed-size hashes only and are removed on every return path.
func Validate(ctx context.Context, src, dst Scanner, plan Plan) TableResult {
	return validateWithOptions(ctx, src, dst, plan, options{records: sortMemoryBytes / recordBytes, fanIn: mergeFanIn})
}
func validateWithOptions(ctx context.Context, src, dst Scanner, p Plan, opt options) (result TableResult) {
	result = TableResult{SourceTable: p.SourceTable, TargetTable: p.TargetTable, Status: StatusIncomplete}
	fail := func(e error) TableResult {
		result.Error = e.Error()
		result.Status = StatusError
		var u *UnsupportedError
		if errors.As(e, &u) {
			result.Status = StatusUnsupported
		}
		if errors.Is(e, context.Canceled) || errors.Is(e, context.DeadlineExceeded) || ctx.Err() != nil {
			result.Status = StatusCancelled
		}
		return result
	}
	if e := ctx.Err(); e != nil {
		return fail(e)
	}
	if src == nil || dst == nil {
		return fail(unsupported("源或目标未提供原始数据扫描能力"))
	}
	sourceMeta, e := src.DescribeValidationTable(ctx, p.SourceTable)
	if e != nil {
		return fail(fmt.Errorf("读取源表校验元数据: %w", e))
	}
	targetMeta, e := dst.DescribeValidationTable(ctx, p.TargetTable)
	if e != nil {
		return fail(fmt.Errorf("读取目标表校验元数据: %w", e))
	}
	if len(p.SourceColumns) == 0 {
		p.SourceColumns = sourceMeta.Columns
		if p.PrimaryKey == nil {
			p.PrimaryKey = sourceMeta.PrimaryKey
		}
	}
	if len(p.TargetColumns) == 0 {
		byName := columnMap(targetMeta.Columns)
		for _, c := range p.SourceColumns {
			t, ok := byName[c.Name]
			if !ok {
				return fail(unsupported("目标缺少映射列 %s", c.Name))
			}
			p.TargetColumns = append(p.TargetColumns, t)
		}
	}
	if len(p.SourceColumns) == 0 || len(p.SourceColumns) != len(p.TargetColumns) {
		return fail(unsupported("源目标校验列数量不一致或没有可校验列"))
	}
	if e = checkMetadata(sourceMeta, p.SourceColumns, p.PrimaryKey, true); e != nil {
		return fail(fmt.Errorf("源表结构在迁移后发生变化: %w", e))
	}
	if e = checkMetadata(targetMeta, p.TargetColumns, nil, false); e != nil {
		return fail(fmt.Errorf("目标表结构不符合校验范围: %w", e))
	}
	keyIndices, e := primaryIndices(p.SourceColumns, p.PrimaryKey)
	if e != nil {
		return fail(e)
	}
	for i, c := range p.SourceColumns {
		if c.Kind == Unsupported || c.Kind == "" || p.TargetColumns[i].Kind == Unsupported || p.TargetColumns[i].Kind == "" {
			return fail(unsupported("列 %s (%s → %s) 缺少无损内容校验器", c.Name, c.DatabaseType, p.TargetColumns[i].DatabaseType))
		}
	}
	dir, e := os.MkdirTemp(opt.tempParent, "dbgold-validation-")
	if e != nil {
		return fail(fmt.Errorf("创建校验临时目录: %w", e))
	}
	defer os.RemoveAll(dir)
	if e = os.Chmod(dir, 0700); e != nil {
		return fail(e)
	}
	sorter := newSorter(dir, opt.records, opt.fanIn)
	var sourceFile, targetFile string
	result.SourceRows, sourceFile, e = scanSorted(ctx, src, p.SourceTable, p.SourceColumns, p.SourceColumns, keyIndices, sorter)
	if e != nil {
		return fail(fmt.Errorf("源表内容扫描未完成: %w", e))
	}
	result.TargetRows, targetFile, e = scanSorted(ctx, dst, p.TargetTable, p.SourceColumns, p.TargetColumns, keyIndices, sorter)
	if e != nil {
		return fail(fmt.Errorf("目标表内容扫描未完成: %w", e))
	}
	refs, e := compareFiles(ctx, sourceFile, targetFile, len(keyIndices) > 0, &result)
	if e != nil {
		return fail(fmt.Errorf("内容摘要比较未完成: %w", e))
	}
	if len(refs) > 0 {
		if e = loadSamples(ctx, src, dst, p, keyIndices, refs, &result); e != nil {
			return fail(fmt.Errorf("差异样例读取未完成: %w", e))
		}
	}
	// A metadata change must never turn an incomplete scan into a pass. Target
	// catalog order can differ; binding is by exact name and frozen position.
	afterSource, e := src.DescribeValidationTable(ctx, p.SourceTable)
	if e != nil {
		return fail(e)
	}
	if e = checkMetadata(afterSource, p.SourceColumns, p.PrimaryKey, true); e != nil {
		return fail(fmt.Errorf("校验期间源表结构发生变化: %w", e))
	}
	afterTarget, e := dst.DescribeValidationTable(ctx, p.TargetTable)
	if e != nil {
		return fail(e)
	}
	if e = checkMetadata(afterTarget, p.TargetColumns, targetMeta.PrimaryKey, true); e != nil {
		return fail(fmt.Errorf("校验期间目标表结构发生变化: %w", e))
	}
	if e = ctx.Err(); e != nil {
		return fail(e)
	}
	result.Status = StatusPassed
	if result.MissingRows+result.ExtraRows+result.ChangedRows+result.DuplicateKeys > 0 {
		result.Status = StatusMismatch
	}
	return result
}
func columnMap(columns []Column) map[string]Column {
	out := make(map[string]Column, len(columns))
	for _, c := range columns {
		out[c.Name] = c
	}
	return out
}
func checkMetadata(actual Table, expected []Column, pk []string, checkPK bool) error {
	if len(actual.Columns) != len(expected) {
		return unsupported("实际列数 %d，冻结列数 %d；不能跳过额外列或缺失列", len(actual.Columns), len(expected))
	}
	byName := columnMap(actual.Columns)
	if len(byName) != len(actual.Columns) {
		return unsupported("实际元数据包含重复列名")
	}
	seen := map[string]bool{}
	for _, c := range expected {
		if seen[c.Name] {
			return unsupported("映射列 %s 重复", c.Name)
		}
		seen[c.Name] = true
		a, ok := byName[c.Name]
		if !ok || a != c {
			return unsupported("列 %s 的名称、类型或精度与冻结定义不同", c.Name)
		}
	}
	if checkPK && !slices.Equal(actual.PrimaryKey, pk) {
		return unsupported("主键定义与冻结定义不同")
	}
	return nil
}
func primaryIndices(columns []Column, names []string) ([]int, error) {
	var out []int
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			return nil, unsupported("主键列 %s 重复", name)
		}
		seen[name] = true
		found := -1
		for i, c := range columns {
			if c.Name == name {
				found = i
				break
			}
		}
		if found < 0 {
			return nil, unsupported("主键列 %s 不在校验列中", name)
		}
		out = append(out, found)
	}
	return out, nil
}
func rowRecord(row []any, logical, native []Column, keys []int) (record, [][32]byte, error) {
	var out record
	if len(row) != len(logical) {
		return out, nil, unsupported("扫描返回列数与冻结定义不一致")
	}
	cells := make([][32]byte, len(row))
	h := sha256.New()
	_, _ = h.Write([]byte("dbgold-content-v1\x00"))
	for i, v := range row {
		b, e := canonical(v, logical[i], native[i])
		if e != nil {
			return out, nil, e
		}
		cells[i] = sha256.Sum256(b)
		_, _ = h.Write(cells[i][:])
	}
	copy(out.row[:], h.Sum(nil))
	if len(keys) == 0 {
		out.key = out.row
	} else {
		h.Reset()
		_, _ = h.Write([]byte("dbgold-key-v1\x00"))
		for _, i := range keys {
			if row[i] == nil {
				return out, nil, unsupported("主键列 %s 返回 NULL", logical[i].Name)
			}
			_, _ = h.Write(cells[i][:])
		}
		copy(out.key[:], h.Sum(nil))
	}
	return out, cells, nil
}
func scanSorted(ctx context.Context, scanner Scanner, table string, logical, native []Column, keys []int, sorter *externalSorter) (count int64, path string, err error) {
	stream, e := scanner.OpenValidationScan(ctx, table, native)
	if e != nil {
		return 0, "", e
	}
	defer func() {
		if e := stream.Close(); err == nil {
			err = e
		}
	}()
	for {
		if e = ctx.Err(); e != nil {
			return count, "", e
		}
		row, e := stream.Next(ctx)
		if e == io.EOF {
			break
		}
		if e != nil {
			return count, "", e
		}
		r, _, e := rowRecord(row, logical, native, keys)
		if e != nil {
			return count, "", e
		}
		if e = sorter.add(ctx, r); e != nil {
			return count, "", e
		}
		count++
	}
	path, err = sorter.finish(ctx)
	return
}

type recordGroup struct {
	first  record
	count  int64
	digest [32]byte
}
type groupReader struct {
	file *os.File
	r    *bufio.Reader
	peek *record
}

func openGroups(path string) (*groupReader, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	return &groupReader{file: f, r: bufio.NewReaderSize(f, 64<<10)}, nil
}
func (g *groupReader) next(ctx context.Context) (recordGroup, error) {
	var out recordGroup
	var r record
	var e error
	if g.peek != nil {
		r = *g.peek
		g.peek = nil
	} else {
		r, e = readRecord(g.r)
		if e != nil {
			return out, e
		}
	}
	out.first = r
	h := sha256.New()
	for {
		if e = ctx.Err(); e != nil {
			return out, e
		}
		out.count++
		_, _ = h.Write(r.row[:])
		r, e = readRecord(g.r)
		if e == io.EOF {
			break
		}
		if e != nil {
			return out, e
		}
		if r.key != out.first.key {
			g.peek = &r
			break
		}
	}
	copy(out.digest[:], h.Sum(nil))
	return out, nil
}

type sampleRef struct {
	diff           Difference
	source, target *record
}

func recordPointer(r record) *record { return &r }
func compareFiles(ctx context.Context, source, target string, resultHasKey bool, result *TableResult) ([]sampleRef, error) {
	s, e := openGroups(source)
	if e != nil {
		return nil, e
	}
	defer s.file.Close()
	d, e := openGroups(target)
	if e != nil {
		return nil, e
	}
	defer d.file.Close()
	a, ea := s.next(ctx)
	b, eb := d.next(ctx)
	refs := []sampleRef{}
	add := func(kind string, src, dst *record) {
		if len(refs) >= maxSamples {
			return
		}
		r := src
		if r == nil {
			r = dst
		}
		diff := Difference{Type: kind, RowHash: hex.EncodeToString(r.row[:])}
		if resultHasKey {
			diff.KeyHash = hex.EncodeToString(r.key[:])
		}
		refs = append(refs, sampleRef{diff, src, dst})
	}
	for ea != io.EOF || eb != io.EOF {
		if e = ctx.Err(); e != nil {
			return nil, e
		}
		if ea != nil && ea != io.EOF {
			return nil, ea
		}
		if eb != nil && eb != io.EOF {
			return nil, eb
		}
		cmp := 0
		if ea == io.EOF {
			cmp = 1
		} else if eb == io.EOF {
			cmp = -1
		} else {
			cmp = bytes.Compare(a.first.key[:], b.first.key[:])
		}
		if cmp < 0 {
			result.MissingRows += a.count
			add("missing", recordPointer(a.first), nil)
			if resultHasKey && a.count > 1 {
				result.DuplicateKeys++
				add("duplicate_key", recordPointer(a.first), nil)
			}
			a, ea = s.next(ctx)
			continue
		}
		if cmp > 0 {
			result.ExtraRows += b.count
			add("extra", nil, recordPointer(b.first))
			if resultHasKey && b.count > 1 {
				result.DuplicateKeys++
				add("duplicate_key", nil, recordPointer(b.first))
			}
			b, eb = d.next(ctx)
			continue
		}
		if resultHasKey {
			if a.count > 1 || b.count > 1 {
				result.DuplicateKeys++
				add("duplicate_key", recordPointer(a.first), recordPointer(b.first))
			} else if a.digest != b.digest {
				result.ChangedRows++
				add("changed", recordPointer(a.first), recordPointer(b.first))
			}
			if a.count > b.count {
				result.MissingRows += a.count - b.count
			} else if b.count > a.count {
				result.ExtraRows += b.count - a.count
			}
		} else if a.count > b.count {
			result.MissingRows += a.count - b.count
			add("missing", recordPointer(a.first), nil)
		} else if b.count > a.count {
			result.ExtraRows += b.count - a.count
			add("extra", nil, recordPointer(b.first))
		}
		a, ea = s.next(ctx)
		b, eb = d.next(ctx)
	}
	return refs, nil
}

type sampleRow struct {
	cells  [][32]byte
	values []ValueSummary
}

func loadSamples(ctx context.Context, src, dst Scanner, p Plan, keys []int, refs []sampleRef, result *TableResult) error {
	sourceRows, e := sampleScan(ctx, src, p.SourceTable, p.SourceColumns, p.SourceColumns, keys, refs, true)
	if e != nil {
		return e
	}
	targetRows, e := sampleScan(ctx, dst, p.TargetTable, p.SourceColumns, p.TargetColumns, keys, refs, false)
	if e != nil {
		return e
	}
	for i, ref := range refs {
		diff := ref.diff
		a, b := sourceRows[i], targetRows[i]
		keyRow := a
		if keyRow == nil {
			keyRow = b
		}
		if len(keys) > 0 && keyRow != nil {
			diff.Key = make(map[string]ValueSummary, len(keys))
			for _, k := range keys {
				diff.Key[p.SourceColumns[k].Name] = keyRow.values[k]
			}
		}
		for c, column := range p.SourceColumns {
			if a != nil && b != nil && a.cells[c] == b.cells[c] {
				continue
			}
			field := FieldDifference{Column: column.Name}
			if a != nil {
				v := a.values[c]
				field.Source = &v
			}
			if b != nil {
				v := b.values[c]
				field.Target = &v
			}
			diff.Columns = append(diff.Columns, column.Name)
			diff.Fields = append(diff.Fields, field)
		}
		result.Samples = append(result.Samples, diff)
	}
	return nil
}
func recordID(r record) string { return string(r.key[:]) + string(r.row[:]) }
func sampleScan(ctx context.Context, scanner Scanner, table string, logical, native []Column, keys []int, refs []sampleRef, source bool) (out []*sampleRow, err error) {
	out = make([]*sampleRow, len(refs))
	wanted := map[string][]int{}
	for i, ref := range refs {
		r := ref.target
		if source {
			r = ref.source
		}
		if r != nil {
			wanted[recordID(*r)] = append(wanted[recordID(*r)], i)
		}
	}
	if len(wanted) == 0 {
		return out, nil
	}
	stream, e := scanner.OpenValidationScan(ctx, table, native)
	if e != nil {
		return out, e
	}
	defer func() {
		if e := stream.Close(); err == nil {
			err = e
		}
	}()
	for len(wanted) > 0 {
		if e = ctx.Err(); e != nil {
			return out, e
		}
		row, e := stream.Next(ctx)
		if e == io.EOF {
			break
		}
		if e != nil {
			return out, e
		}
		r, cells, e := rowRecord(row, logical, native, keys)
		if e != nil {
			return out, e
		}
		id := recordID(r)
		indices, ok := wanted[id]
		if !ok {
			continue
		}
		sample := &sampleRow{cells: cells, values: make([]ValueSummary, len(row))}
		for c, v := range row {
			sample.values[c] = summarize(v, logical[c])
		}
		for _, i := range indices {
			out[i] = sample
		}
		delete(wanted, id)
	}
	if len(wanted) > 0 {
		return out, unsupported("两次读取之间数据发生变化，差异样例无法稳定回读；请停止两端写入后重试")
	}
	return out, nil
}
func summarize(v any, column Column) ValueSummary {
	if v == nil {
		return ValueSummary{Null: true}
	}
	if column.Kind == Binary {
		if b, ok := v.([]byte); ok {
			h := sha256.Sum256(b)
			return ValueSummary{Text: "[binary]", Length: int64(len(b)), SHA256: hex.EncodeToString(h[:])}
		}
	}
	var text string
	switch x := v.(type) {
	case time.Time:
		text = x.Format(time.RFC3339Nano)
	case float64:
		text = strconv.FormatFloat(x, 'g', -1, 64)
	case float32:
		text = strconv.FormatFloat(float64(x), 'g', -1, 32)
	default:
		text, _ = scalarText(v)
	}
	length := len(text)
	count := 0
	end := len(text)
	for i := range text {
		if count == 256 {
			end = i
			break
		}
		count++
	}
	truncated := end < len(text)
	return ValueSummary{Text: text[:end], Truncated: truncated, Length: int64(length)}
}
