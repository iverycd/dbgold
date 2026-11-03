package datamigrate

import (
	"context"
	"fmt"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
)

type oracleMySQLAutoReader interface {
	GetOracleMySQLAutoIncrements(context.Context) ([]source.OracleMySQLAutoIncrement, error)
}
type oracleMySQLAutoWriter interface {
	PrepareOracleMySQLAutoIncrement(context.Context, source.SequenceInfo) ([]dialect.Statement, string, error)
	ExecuteOracleMySQLAutoIncrement(context.Context, source.SequenceInfo, []dialect.Statement, string) error
	ValidateOracleMySQLAutoIncrement(context.Context, source.SequenceInfo, string, bool) error
}

func (m *Migrator) initializeOracleMySQLAuto(ctx context.Context, tables []string) {
	if m.reader.DBType() != "oracle" || m.writer.DBType() != "mysql" {
		return
	}
	r, ok := m.reader.(oracleMySQLAutoReader)
	if !ok {
		return
	}
	m.oracleMySQLAuto = &TriggerAdaptationSummary{Mode: "mysql_auto_increment", Recorded: true}
	bindings, err := r.GetOracleMySQLAutoIncrements(ctx)
	if err != nil {
		m.oracleMySQLAuto.Recorded = false
		return
	}
	selected := map[string]bool{}
	for _, table := range tables {
		selected[table] = true
	}
	for _, a := range bindings {
		if !selected[a.TableName] {
			continue
		}
		v := OracleMySQLAutoResult{Table: a.TableName, Column: a.ColumnName, Trigger: a.TriggerName, SequenceOwner: a.SequenceOwner, Sequence: a.SequenceName, TargetTable: m.objName(a.TableName), TargetColumn: m.targetColumn(a.TableName, a.ColumnName), SharedSequence: a.SharedSequence, ConversionStatus: "pending", CalibrationStatus: "pending", Error: a.Error}
		if a.Disabled {
			v.ConversionStatus, v.CalibrationStatus = "disabled", "not_applicable"
		} else if a.Error != "" {
			v.ConversionStatus, v.CalibrationStatus = "blocked", "blocked"
		}
		m.oracleMySQLAuto.Items = append(m.oracleMySQLAuto.Items, v)
	}
}

func (m *Migrator) finishOracleMySQLAuto(report *MigrationReport) {
	s := m.oracleMySQLAuto
	if s == nil {
		return
	}
	for i := range s.Items {
		v := &s.Items[i]
		if v.ConversionStatus == "disabled" {
			continue
		}
		s.Total++
		for _, item := range report.Tables.Items {
			if item.Name == v.Table {
				v.ConversionStatus, v.CalibrationStatus, v.Error = "blocked", "blocked", item.Error
			}
		}
		if s := m.oracleMySQL; s != nil && s.postEligible[v.Table] && v.ConversionStatus == "pending" {
			v.ConversionStatus = "converted"
		}
		// A successful CREATE can be followed by a failed copy. Keep both facts.
		if s := m.oracleMySQL; s != nil && s.touched[m.oracleMySQLTargetKey(v.TargetTable)] && s.blocked[v.Table] == nil && v.ConversionStatus == "pending" {
			v.ConversionStatus = "converted"
		}
		for _, item := range report.Data.Items {
			if item.Name == v.Table {
				v.CalibrationStatus, v.Error = "blocked", item.Error
			}
		}
		for _, item := range report.Sequences.Items {
			if item.Table == v.Table || item.Table == v.TargetTable || item.Name == v.TargetTable+"_"+v.TargetColumn+"_seq" {
				if v.CalibrationStatus == "pending" {
					v.CalibrationStatus, v.Error = "blocked", item.Error
				}
			}
		}
		switch {
		case v.CalibrationStatus == "succeeded":
			s.Success++
		case v.CalibrationStatus == "failed":
			s.Failed++
		case v.CalibrationStatus == "blocked" || v.ConversionStatus == "blocked":
			s.Blocked++
		default:
			s.Pending++
		}
	}
	report.TriggerAdaptation = s
}

func (m *Migrator) recordOracleMySQLCalibration(v source.SequenceInfo, start string, err error) {
	if m.oracleMySQLAuto == nil || v.OracleMySQL == nil {
		return
	}
	for i := range m.oracleMySQLAuto.Items {
		item := &m.oracleMySQLAuto.Items[i]
		if item.Trigger != v.OracleMySQL.TriggerName || item.Table != v.OracleMySQL.TableName {
			continue
		}
		item.Start = start
		if err != nil {
			item.CalibrationStatus, item.Error = "failed", err.Error()
		} else {
			item.CalibrationStatus = "succeeded"
			if m.oracleMySQLSchemaMode() {
				item.ConversionStatus = "converted"
			} else {
				item.ConversionStatus = "reused"
			}
		}
	}
}

// Existing-table modes must validate bindings before copying or altering anything.
func (m *Migrator) preflightOracleMySQLAutoExisting(ctx context.Context, tables []string, report *MigrationReport, failed map[string]bool) {
	if m.oracleMySQLAuto == nil || m.cfg.Content != "data_only" && !m.cfg.objectMode() {
		return
	}
	if m.cfg.objectMode() {
		selected := false
		for _, kind := range m.cfg.Objects {
			selected = selected || kind == "sequences"
		}
		if !selected {
			return
		}
	}
	for _, table := range tables {
		if failed[table] {
			continue
		}
		info, err := m.sourceTableDDLInfo(ctx, table)
		if err == nil {
			for _, c := range info.Columns {
				if c.OracleMySQLAutoType == "" {
					continue
				}
				w, ok := m.writer.(oracleMySQLAutoWriter)
				if !ok {
					err = fmt.Errorf("目标 Writer 缺少自增校验能力")
					break
				}
				v := source.SequenceInfo{TableName: m.objName(table), ColumnName: m.targetColumn(table, c.Name)}
				err = w.ValidateOracleMySQLAutoIncrement(ctx, v, c.OracleMySQLAutoType, m.cfg.Content == "data_only")
				if err != nil {
					break
				}
				for i := range m.oracleMySQLAuto.Items {
					item := &m.oracleMySQLAuto.Items[i]
					if item.Table == table && item.Column == c.Name && item.ConversionStatus != "disabled" {
						item.ConversionStatus = "reused"
					}
				}
			}
		}
		if err != nil {
			failed[table] = true
			m.blockMigrationTable(table, "preflight", err)
			report.Tables.Failed++
			report.Tables.Items = append(report.Tables.Items, ObjectResult{Name: table, Stage: "preflight", Error: err.Error()})
		}
	}
}

// Type changes on automatic IDs must not leave incompatible source FK plans.
// Validate both sides before the first target DROP; no implicit FK type rewrite.
func (m *Migrator) validateOracleMySQLAutoForeignKeys(ctx context.Context) {
	s := m.oracleMySQL
	for _, fk := range s.foreignKeys {
		if !s.selected[fk.TableName] || !s.selected[fk.RefTable] || fk.RefSchema != "" && fk.RefSchema != m.oracleMySQLSourceSchema() {
			continue
		}
		left, right := s.plans[fk.TableName], s.plans[fk.RefTable]
		if left == nil || right == nil {
			continue
		}
		for i, name := range fk.Columns {
			if i >= len(fk.RefColumns) {
				break
			}
			var a, b source.ColumnInfo
			for _, c := range left.Columns {
				if c.Name == name {
					a = c
				}
			}
			for _, c := range right.Columns {
				if c.Name == fk.RefColumns[i] {
					b = c
				}
			}
			if a.OracleMySQLAutoType == "" && b.OracleMySQLAutoType == "" {
				continue
			}
			if !strings.EqualFold(a.DataType, b.DataType) {
				err := fmt.Errorf("自增转换后外键 %s 两侧类型不兼容: %s.%s %s / %s.%s %s", fk.ConstraintName, fk.TableName, a.Name, a.DataType, fk.RefTable, b.Name, b.DataType)
				s.blocked[fk.TableName], s.blocked[fk.RefTable] = err, err
			}
		}
	}
}
