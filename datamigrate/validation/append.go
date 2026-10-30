package validation

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
)

// AppendBaseline freezes a multiset of target row fingerprints before copying.
// No business values are persisted. The caller must Close it on every exit.
type AppendBaseline struct {
	dir, path string
	rows      int64
	plan      Plan
	metadata  Table
}

func (b *AppendBaseline) Rows() int64 { return b.rows }

func (b *AppendBaseline) Close() error {
	if b == nil || b.dir == "" {
		return nil
	}
	err := os.RemoveAll(b.dir)
	if err == nil {
		b.dir, b.path = "", ""
	}
	return err
}

// CaptureAppendBaseline binds native target values to the source's logical
// types, just as Validate does. Sorting preserves duplicate row multiplicity.
func CaptureAppendBaseline(ctx context.Context, dst Scanner, plan Plan) (*AppendBaseline, error) {
	return captureAppendBaseline(ctx, dst, plan, options{records: sortMemoryBytes / recordBytes, fanIn: mergeFanIn})
}

func captureAppendBaseline(ctx context.Context, dst Scanner, p Plan, opt options) (_ *AppendBaseline, err error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if dst == nil || len(p.SourceColumns) == 0 || len(p.SourceColumns) != len(p.TargetColumns) {
		return nil, unsupported("追加校验缺少目标扫描器或完整列映射")
	}
	for i, column := range p.SourceColumns {
		if column.Kind == "" || column.Kind == Unsupported || p.TargetColumns[i].Kind == "" || p.TargetColumns[i].Kind == Unsupported {
			return nil, unsupported("列 %s 缺少无损追加内容校验器", column.Name)
		}
	}
	meta, err := dst.DescribeValidationTable(ctx, p.TargetTable)
	if err != nil {
		return nil, err
	}
	if err = checkMetadata(meta, p.TargetColumns, nil, false); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(opt.tempParent, "dbgold-append-baseline-")
	if err != nil {
		return nil, err
	}
	b := &AppendBaseline{dir: dir, plan: p, metadata: meta}
	b.plan.SourceColumns = slices.Clone(p.SourceColumns)
	b.plan.TargetColumns = slices.Clone(p.TargetColumns)
	b.plan.PrimaryKey = slices.Clone(p.PrimaryKey)
	b.metadata.Columns = slices.Clone(meta.Columns)
	b.metadata.PrimaryKey = slices.Clone(meta.PrimaryKey)
	defer func() {
		if err != nil {
			_ = b.Close()
		}
	}()
	b.rows, b.path, err = scanSorted(ctx, dst, p.TargetTable, p.SourceColumns, p.TargetColumns, nil, newSorter(dir, opt.records, opt.fanIn))
	if err != nil {
		return nil, err
	}
	after, err := dst.DescribeValidationTable(ctx, p.TargetTable)
	if err != nil {
		return nil, err
	}
	if err = checkMetadata(after, b.metadata.Columns, b.metadata.PrimaryKey, true); err != nil {
		return nil, fmt.Errorf("冻结追加基线期间目标结构发生变化: %w", err)
	}
	return b, nil
}

// ValidateAppend proves final target = frozen target + source as multisets.
// Comparing all fingerprints also detects changes to pre-existing target rows.
// TargetRows is the target increment, matching partition row-count reporting.
func ValidateAppend(ctx context.Context, src, dst Scanner, p Plan, baseline *AppendBaseline) TableResult {
	return validateAppend(ctx, src, dst, p, baseline, options{records: sortMemoryBytes / recordBytes, fanIn: mergeFanIn})
}

func validateAppend(ctx context.Context, src, dst Scanner, p Plan, b *AppendBaseline, opt options) (result TableResult) {
	result = TableResult{SourceTable: p.SourceTable, TargetTable: p.TargetTable, Status: StatusIncomplete,
		Note: "按追加前目标原值基线与源数据的完整多重集合核验；目标行数显示新增量，重复行保留次数"}
	fail := func(err error) TableResult {
		result.Status, result.Error = StatusError, err.Error()
		var unsupportedErr *UnsupportedError
		if errors.As(err, &unsupportedErr) {
			result.Status = StatusUnsupported
		}
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			result.Status = StatusCancelled
		}
		return result
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if src == nil || dst == nil || b == nil || b.path == "" {
		return fail(fmt.Errorf("追加前原值基线不存在或已释放，不能用迁移后的数据重建"))
	}
	if p.SourceTable != b.plan.SourceTable || p.TargetTable != b.plan.TargetTable ||
		!slices.Equal(p.SourceColumns, b.plan.SourceColumns) || !slices.Equal(p.TargetColumns, b.plan.TargetColumns) || !slices.Equal(p.PrimaryKey, b.plan.PrimaryKey) {
		return fail(fmt.Errorf("追加校验范围与冻结基线不同"))
	}
	check := func() error {
		source, err := src.DescribeValidationTable(ctx, p.SourceTable)
		if err != nil {
			return err
		}
		if err = checkMetadata(source, p.SourceColumns, p.PrimaryKey, true); err != nil {
			return fmt.Errorf("追加校验源结构发生变化: %w", err)
		}
		target, err := dst.DescribeValidationTable(ctx, p.TargetTable)
		if err != nil {
			return err
		}
		if err = checkMetadata(target, b.metadata.Columns, b.metadata.PrimaryKey, true); err != nil {
			return fmt.Errorf("追加校验目标结构发生变化: %w", err)
		}
		return nil
	}
	if err := check(); err != nil {
		return fail(err)
	}
	dir, err := os.MkdirTemp(opt.tempParent, "dbgold-append-validation-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(dir)
	sorter := newSorter(dir, opt.records, opt.fanIn)
	f, err := os.Open(b.path)
	if err != nil {
		return fail(err)
	}
	r := bufio.NewReader(f)
	var baselineRows int64
	for {
		rec, e := readRecord(r)
		if e == io.EOF {
			break
		}
		if e != nil {
			err = e
			break
		}
		if err = sorter.add(ctx, rec); err != nil {
			break
		}
		baselineRows++
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return fail(err)
	}
	if baselineRows != b.rows {
		return fail(fmt.Errorf("追加基线摘要不完整"))
	}
	var expected, actual string
	result.SourceRows, expected, err = scanSorted(ctx, src, p.SourceTable, p.SourceColumns, p.SourceColumns, nil, sorter)
	if err != nil {
		return fail(fmt.Errorf("追加源内容扫描未完成: %w", err))
	}
	result.TargetRows, actual, err = scanSorted(ctx, dst, p.TargetTable, p.SourceColumns, p.TargetColumns, nil, sorter)
	if err != nil {
		return fail(fmt.Errorf("追加目标内容扫描未完成: %w", err))
	}
	result.TargetRows -= b.rows
	refs, err := compareFiles(ctx, expected, actual, false, &result)
	if err != nil {
		return fail(err)
	}
	// Old baseline values are deliberately unavailable for a second sample scan.
	// Retain bounded fingerprints instead of attributing them to current source.
	for _, ref := range refs {
		result.Samples = append(result.Samples, ref.diff)
	}
	if err = check(); err != nil {
		return fail(err)
	}
	if err = ctx.Err(); err != nil {
		return fail(err)
	}
	result.Status = StatusPassed
	if result.MissingRows != 0 || result.ExtraRows != 0 {
		result.Status = StatusMismatch
	}
	return result
}
