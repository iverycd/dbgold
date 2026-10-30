package datamigrate

import (
	"context"
	"fmt"
	"strings"

	"dbgold/datamigrate/partition"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/validation"
)

type partitionValueScanner struct {
	validation.Scanner
	scanner  validation.PartitionScanner
	root     string
	location validation.PartitionLocation
}

func (s partitionValueScanner) OpenValidationScan(ctx context.Context, _ string, cols []validation.Column) (validation.RowStream, error) {
	return s.scanner.OpenPartitionValidationScan(ctx, s.root, s.location, cols)
}

func partitionLocation(spec *source.PartitionSpec, schema, name string, ordinal int) (validation.PartitionLocation, bool) {
	var walk func(*source.PartitionSpec, bool) (validation.PartitionLocation, bool)
	walk = func(s *source.PartitionSpec, nested bool) (validation.PartitionLocation, bool) {
		if s == nil {
			return validation.PartitionLocation{}, false
		}
		for _, p := range s.Partitions {
			if p.Subpartition != nil {
				if loc, ok := walk(p.Subpartition, true); ok {
					return loc, true
				}
				continue
			}
			if p.Name == name && (p.Schema == schema || schema == "") {
				return validation.PartitionLocation{Schema: p.Schema, Name: p.Name, Ordinal: p.Ordinal, Subpartition: nested}, true
			}
			if ordinal > 0 && p.Ordinal == ordinal {
				return validation.PartitionLocation{Schema: p.Schema, Name: p.Name, Ordinal: p.Ordinal, Subpartition: nested}, true
			}
		}
		return validation.PartitionLocation{}, false
	}
	location, ok := walk(spec, false)
	location.Native = spec != nil && strings.HasPrefix(spec.Algorithm, "pg_partition:")
	return location, ok
}

func (m *Migrator) validatePartitionContent(ctx context.Context, report *MigrationReport) {
	s := m.partitionState
	if s == nil || !s.hasPartitions || report.DataValidation == nil {
		return
	}
	src, srcOK := validationSource(m.reader)
	dst, dstOK := m.writer.(validation.Scanner)
	sp, spOK := partitionSourceReader(m.reader).(validation.PartitionScanner)
	dp, dpOK := m.writer.(validation.PartitionScanner)
	for _, frozen := range m.validationTables {
		t := s.tables[frozen.name]
		if t == nil || t.Plan == nil {
			continue
		}
		if _, copied := m.validationCopied.Load(frozen.name); !copied {
			continue
		}
		for _, mapping := range t.Plan.Mapping {
			result := validation.TableResult{SourceTable: t.SourceTable + " PARTITION " + mapping.SourceName, TargetTable: t.TargetTable + " PARTITION " + mapping.TargetName}
			sl, sok := partitionLocation(t.Source, mapping.SourceSchema, mapping.SourceName, 0)
			dl, dok := partitionLocation(t.Plan.Target, mapping.TargetSchema, mapping.TargetName, mapping.TargetOrdinal)
			switch {
			case frozen.err != nil:
				result.Status, result.Error = validation.StatusError, frozen.err.Error()
			case !srcOK || !dstOK || !spOK || !dpOK:
				result.Status, result.Error = validation.StatusUnsupported, "源库或目标库尚未实现分区原值内容读取，不能把整表校验当作逐分区校验"
			case !sok || !dok:
				result.Status, result.Error = validation.StatusError, "冻结计划缺少分区物理身份"
			default:
				plan, e := m.dataValidationPlan(ctx, dst, frozen)
				if e != nil {
					result.Status, result.Error = validation.StatusError, e.Error()
				} else {
					sourceScan := partitionValueScanner{src, sp, t.SourceTable, sl}
					targetScan := partitionValueScanner{dst, dp, t.TargetTable, dl}
					if len(t.Baseline) > 0 && !allPartitionCountsZero(t.Baseline) {
						baseline := frozen.partitionBaselines[partition.LeafKey(mapping.TargetSchema, mapping.TargetName)]
						result = validation.ValidateAppend(ctx, sourceScan, targetScan, plan, baseline)
					} else {
						result = validation.Validate(ctx, sourceScan, targetScan, plan)
					}
					result.SourceTable = t.SourceTable + " PARTITION " + partition.LeafKey(mapping.SourceSchema, mapping.SourceName)
					result.TargetTable = t.TargetTable + " PARTITION " + partition.LeafKey(mapping.TargetSchema, mapping.TargetName)
				}
			}
			report.DataValidation.Tables = append(report.DataValidation.Tables, result)
		}
	}
	report.DataValidation.Finalize()
}

func (m *Migrator) closeValidationBaselines() {
	for _, frozen := range m.validationTables {
		_ = frozen.appendBaseline.Close()
		for _, baseline := range frozen.partitionBaselines {
			_ = baseline.Close()
		}
	}
}

// A failed pre-copy snapshot cannot be recovered after appending business rows.
// Keep this gate at both generic and PG-specific partition streaming entrances.
func (m *Migrator) partitionCopyReady(table string) error {
	if !m.dataValidationEnabled() || m.cfg.Content != "data_only" || m.partitionState == nil {
		return nil
	}
	t := m.partitionState.tables[table]
	if t == nil || t.Plan == nil || allPartitionCountsZero(t.Baseline) {
		return nil
	}
	for _, frozen := range m.validationTables {
		if frozen.name != table {
			continue
		}
		if frozen.err != nil {
			return fmt.Errorf("追加前原值基线冻结失败，未写入该表: %w", frozen.err)
		}
		if frozen.appendBaseline == nil || len(frozen.partitionBaselines) != len(t.Plan.Mapping) {
			break
		}
		return nil
	}
	return fmt.Errorf("追加前根表或叶分区原值基线不完整，未写入该表")
}

// Freeze only during execution preparation, never the metadata-only preflight.
// Full-content validation is opt-in; row-count baselines remain independent.
func (m *Migrator) freezePartitionAppendContent(ctx context.Context, frozen *frozenValidationTable) {
	if m.cfg.Content != "data_only" || m.partitionState == nil {
		return
	}
	t := m.partitionState.tables[frozen.name]
	if t == nil || t.Plan == nil || allPartitionCountsZero(t.Baseline) {
		return
	}
	dst, ok := m.writer.(validation.Scanner)
	dp, partitionOK := m.writer.(validation.PartitionScanner)
	if !ok || !partitionOK {
		frozen.err = fmt.Errorf("目标不支持追加前根表与叶分区原值基线读取")
		return
	}
	plan, err := m.dataValidationPlan(ctx, dst, *frozen)
	if err != nil {
		frozen.err = err
		return
	}
	frozen.appendBaseline, err = validation.CaptureAppendBaseline(ctx, dst, plan)
	if err != nil {
		frozen.err = fmt.Errorf("冻结追加前根表内容: %w", err)
		return
	}
	frozen.partitionBaselines = make(map[string]*validation.AppendBaseline)
	var total int64
	for _, mapping := range t.Plan.Mapping {
		location, ok := partitionLocation(t.Plan.Target, mapping.TargetSchema, mapping.TargetName, mapping.TargetOrdinal)
		if !ok {
			frozen.err = fmt.Errorf("冻结计划缺少目标叶分区身份")
			return
		}
		baseline, err := validation.CaptureAppendBaseline(ctx, partitionValueScanner{dst, dp, t.TargetTable, location}, plan)
		if err != nil {
			frozen.err = fmt.Errorf("冻结追加前分区 %s 内容: %w", mapping.TargetName, err)
			return
		}
		frozen.partitionBaselines[partition.LeafKey(mapping.TargetSchema, mapping.TargetName)] = baseline
		count, found := partitionRowCount(t.Baseline, mapping.TargetSchema, mapping.TargetName, mapping.TargetOrdinal)
		if !found || count < 0 || count != baseline.Rows() {
			frozen.err = fmt.Errorf("目标分区 %s 在冻结行数与内容基线之间已变化", mapping.TargetName)
			return
		}
		total += count
	}
	if frozen.appendBaseline.Rows() != total {
		frozen.err = fmt.Errorf("目标根表内容基线与全部叶分区基线行数不一致")
	}
}

func allPartitionCountsZero(counts map[string]int64) bool {
	for _, n := range counts {
		if n != 0 {
			return false
		}
	}
	return true
}

func checkRecoveryPartitions(ctx context.Context, r source.Reader, w interface {
	InspectPartitionTable(context.Context, string) (*source.PartitionSpec, error)
}, table string, p *partition.Plan) error {
	reader, ok := partitionSourceReader(r).(source.PartitionReader)
	if !ok {
		return fmt.Errorf("源库缺少分区核验能力")
	}
	fresh, e := reader.PartitionMetadata(ctx, table)
	if e != nil {
		return e
	}
	if e = partition.Equivalent(p.Source, fresh); e != nil {
		return fmt.Errorf("源分区定义漂移: %w", e)
	}
	return nil
}
