package datamigrate

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"dbgold/datamigrate/validation"
)

// This adapter is limited to the approved PG -> Oracle mapping. It does not
// reuse migration cleaners: complete native metadata is checked before every
// read, bits retain their complete source projection, and only bounded Oracle
// character columns apply the explicitly accepted empty-string -> NULL rule.
type pgOracleValidationScanner struct {
	validation.Scanner
	expected  validation.Table
	columns   []validation.Column
	emptyNull map[string]bool
}

func pgOracleValidationMetadata(actual, expected validation.Table) error {
	if len(actual.Columns) != len(expected.Columns) || !slices.Equal(actual.PrimaryKey, expected.PrimaryKey) {
		return fmt.Errorf("PG→Oracle 内容校验完整列数或主键偏离冻结定义")
	}
	byName := map[string]validation.Column{}
	for _, c := range actual.Columns {
		if _, exists := byName[c.Name]; exists {
			return fmt.Errorf("PG→Oracle 内容校验列名重复: %s", c.Name)
		}
		byName[c.Name] = c
	}
	for _, c := range expected.Columns {
		if got, ok := byName[c.Name]; !ok || got != c {
			return fmt.Errorf("PG→Oracle 内容校验列 %s 偏离完整冻结定义", c.Name)
		}
	}
	return nil
}

func (s *pgOracleValidationScanner) DescribeValidationTable(ctx context.Context, name string) (validation.Table, error) {
	actual, err := s.Scanner.DescribeValidationTable(ctx, name)
	if err != nil {
		return validation.Table{}, err
	}
	if err := pgOracleValidationMetadata(actual, s.expected); err != nil {
		return validation.Table{}, err
	}
	actual.Columns = slices.Clone(s.columns)
	return actual, nil
}

func (s *pgOracleValidationScanner) OpenValidationScan(ctx context.Context, name string, columns []validation.Column) (validation.RowStream, error) {
	if _, err := s.DescribeValidationTable(ctx, name); err != nil {
		return nil, err
	}
	if !slices.Equal(columns, s.columns) {
		return nil, fmt.Errorf("PG→Oracle 内容校验投影偏离冻结映射")
	}
	byName := map[string]validation.Column{}
	for _, c := range s.expected.Columns {
		byName[c.Name] = c
	}
	raw := make([]validation.Column, len(columns))
	for i, c := range columns {
		var exists bool
		raw[i], exists = byName[c.Name]
		if !exists {
			return nil, fmt.Errorf("PG→Oracle 内容校验列 %s 不在完整冻结定义", c.Name)
		}
	}
	stream, err := s.Scanner.OpenValidationScan(ctx, name, raw)
	if err != nil {
		return nil, err
	}
	return &pgOracleValidationStream{RowStream: stream, columns: columns, emptyNull: s.emptyNull}, nil
}

type pgOracleValidationStream struct {
	validation.RowStream
	columns   []validation.Column
	emptyNull map[string]bool
}

func (s *pgOracleValidationStream) Next(ctx context.Context) ([]any, error) {
	row, err := s.RowStream.Next(ctx)
	if err != nil {
		return nil, err
	}
	if len(row) != len(s.columns) {
		return nil, fmt.Errorf("PG→Oracle 内容扫描列数偏离冻结映射")
	}
	row = slices.Clone(row)
	for i, c := range s.columns {
		if !s.emptyNull[c.Name] {
			continue
		}
		switch value := row[i].(type) {
		case string:
			if value == "" {
				row[i] = nil
			}
		case []byte:
			if len(value) == 0 {
				row[i] = nil
			}
		}
	}
	return row, nil
}

func (m *Migrator) pgOracleValidationScanners(ctx context.Context, src, dst validation.Scanner, frozen frozenValidationTable, plan validation.Plan) (validation.Scanner, validation.Scanner, error) {
	if !m.isPGOracle() {
		return src, dst, nil
	}
	meta, err := dst.DescribeValidationTable(ctx, plan.TargetTable)
	if err != nil {
		return nil, nil, err
	}
	if frozen.pgOracleTarget != nil {
		if err = pgOracleValidationMetadata(meta, *frozen.pgOracleTarget); err != nil {
			return nil, nil, err
		}
	}
	target := &pgOracleValidationScanner{Scanner: dst, expected: meta, columns: plan.TargetColumns}
	if src == nil {
		return nil, target, nil
	}
	emptyNull := map[string]bool{}
	for i, c := range plan.SourceColumns {
		if c.Kind != validation.Text {
			continue
		}
		switch strings.ToUpper(plan.TargetColumns[i].DatabaseType) {
		case "VARCHAR2", "NVARCHAR2", "CHAR", "NCHAR":
			emptyNull[c.Name] = true
		}
	}
	return &pgOracleValidationScanner{Scanner: src, expected: frozen.metadata, columns: plan.SourceColumns, emptyNull: emptyNull}, target, nil
}

func (m *Migrator) freezePGOracleAppendContent(ctx context.Context, frozen *frozenValidationTable) {
	if !m.isPGOracle() {
		return
	}
	frozen.note = "按已确认 PG→Oracle 映射校验：普通字符空串按 NULL 比较；LOB 保留空串，位串保留全部位及前导零"
	if m.cfg.Content != "data_only" {
		return
	}
	dst, ok := m.writer.(validation.Scanner)
	if !ok {
		frozen.err = fmt.Errorf("目标不支持追加前原值基线读取")
		return
	}
	plan, err := m.dataValidationPlan(ctx, dst, *frozen)
	if err != nil {
		frozen.err = err
		return
	}
	meta, err := dst.DescribeValidationTable(ctx, plan.TargetTable)
	if err != nil {
		frozen.err = err
		return
	}
	meta.Columns, meta.PrimaryKey = slices.Clone(meta.Columns), slices.Clone(meta.PrimaryKey)
	frozen.pgOracleTarget = &meta
	if len(meta.Columns) > len(plan.SourceColumns) {
		frozen.note += "；内容比较范围为迁移映射列，目标额外列核验完整结构"
	}
	// Partition tables also require every leaf baseline; the existing partition
	// snapshot handles both root and leaves together when there are old rows.
	if m.partitionState != nil {
		if t := m.partitionState.tables[frozen.name]; t != nil && t.Plan != nil && !allPartitionCountsZero(t.Baseline) {
			return
		}
	}
	_, target, err := m.pgOracleValidationScanners(ctx, nil, dst, *frozen, plan)
	if err == nil {
		frozen.appendBaseline, err = validation.CaptureAppendBaseline(ctx, target, plan)
	}
	if err != nil {
		frozen.err = fmt.Errorf("冻结 PG→Oracle 追加前内容基线: %w", err)
	}
}

func (m *Migrator) pgOracleValidationCopyReady(table string) error {
	if !m.isPGOracle() || !m.dataValidationEnabled() || m.cfg.Content != "data_only" {
		return nil
	}
	for _, frozen := range m.validationTables {
		if frozen.name != table {
			continue
		}
		if frozen.err != nil {
			return fmt.Errorf("追加前原值基线冻结失败，未写入该表: %w", frozen.err)
		}
		if frozen.appendBaseline != nil {
			return nil
		}
		break
	}
	return fmt.Errorf("追加前原值基线缺失，未写入该表")
}
