package datamigrate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"dbgold/datamigrate/mysqldmtrigger"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

// Existing fixtures have no user triggers. The dedicated writer below exercises
// both load protection and the complete immutable installation observer.
func (w *dmFixtureWriter) CheckMySQLDamengLoadTriggers(context.Context, string) error { return nil }

type mysqlDMTriggerWriter struct {
	*dmFixtureWriter
	plans         []mysqldmtrigger.Plan
	failName      string
	loadBlocked   string
	beforeInstall func()
}

func (w *mysqlDMTriggerWriter) DetectMySQLDamengTriggerCapabilities(context.Context) (mysqldmtrigger.Capabilities, error) {
	return mysqldmtrigger.Capabilities{CompatibleMode: "2", EmptyStringIsNull: true}, nil
}
func (w *mysqlDMTriggerWriter) CheckMySQLDamengLoadTriggers(_ context.Context, table string) error {
	if table == w.loadBlocked {
		return errors.New("目标表存在启用触发器")
	}
	return nil
}
func (w *mysqlDMTriggerWriter) CreateMySQLDamengTrigger(ctx context.Context, p mysqldmtrigger.Plan) error {
	if w.beforeInstall != nil {
		w.beforeInstall()
	}
	if err := mysqldmtrigger.ValidatePlan(p); err != nil {
		return err
	}
	w.plans = append(w.plans, p)
	if err := repair.Observe(ctx, p.TriggerSQL, "running", nil); err != nil {
		return err
	}
	if p.Name == w.failName {
		err := errors.New("create denied")
		_ = repair.Observe(ctx, p.TriggerSQL, "failed", err)
		return err
	}
	return repair.Observe(ctx, p.TriggerSQL, "succeeded", nil)
}

func mysqlDMTriggerFixture(t *testing.T) (*Migrator, *triggerTestReader, *mysqlDMTriggerWriter) {
	t.Helper()
	_, r, _ := triggerFixture(t)
	w := &mysqlDMTriggerWriter{dmFixtureWriter: &dmFixtureWriter{mockWriter: &mockWriter{}}}
	m, _ := newTestMigrator(r, w)
	m.cfg.TargetSchema, m.cfg.TargetDBType, m.cfg.MigrateTriggers = "dst", "dameng", true
	return m, r, w
}

func TestMySQLDamengTriggersCreatedAfterDataWithFrozenLedger(t *testing.T) {
	m, r, w := mysqlDMTriggerFixture(t)
	w.beforeInstall = func() { require.NotEmpty(t, w.copied) }
	m.cfg.CaptureRepair = true
	report := m.Run(context.Background())
	require.Zero(t, report.Triggers.Failed, "%+v", report.Triggers.Items)
	require.Equal(t, 1, report.Triggers.Success)
	require.True(t, report.TriggerMigrationEnabled)
	require.Len(t, w.plans, 1)
	require.Contains(t, w.plans[0].TriggerSQL, `:NEW."AMOUNT"`)
	require.Equal(t, "OrderData", r.triggers[0].TableName)
	for _, o := range report.RepairManifest.Objects {
		if o.Kind != "trigger" {
			continue
		}
		require.Equal(t, "succeeded", o.Status)
		require.NotNil(t, o.MySQLDamengTrigger)
		require.False(t, o.DiagnosticOnly)
		require.Len(t, o.Units, 1)
		require.Equal(t, "succeeded", o.Units[0].Status)
		require.Equal(t, []string{repairID("table", "OrderData", "OrderData")}, o.Dependencies)
	}
}

type mysqlDMRowGUIDReader struct{ *triggerCharacterTestReader }

func (r *mysqlDMRowGUIDReader) ReadPage(_ context.Context, table string, _ []string, offset, _ int64) ([]string, []string, [][]interface{}, error) {
	if offset > 0 {
		return nil, nil, nil, nil
	}
	return []string{"Id", "RowGuid"}, []string{"INT", "VARCHAR"}, r.rows[table], nil
}

type mysqlDMRowGUIDWriter struct {
	*mysqlDMTriggerWriter
	rows [][]interface{}
}

func (w *mysqlDMRowGUIDWriter) CopyData(ctx context.Context, table string, columns, types []string, rows [][]interface{}) error {
	for _, row := range rows {
		w.rows = append(w.rows, append([]interface{}(nil), row...))
	}
	return w.dmFixtureWriter.CopyData(ctx, table, columns, types, rows)
}

func TestMySQLDamengRowGUIDTriggerPreservesHistoricalRows(t *testing.T) {
	for _, tc := range []struct {
		name, rhs, historical, expectedSQL string
		length                             int64
	}{
		{"compact", "(\nSELECT REPLACE(UUID(), '-', '')\n)", "0123456789ABCDEF0123456789ABCDEF", `:NEW."ROWGUID" := LOWER(RAWTOHEX(SYS_GUID()));`, 32},
		{"hyphenated", "UUID()", "01234567-89AB-CDEF-0123-456789ABCDEF", `:NEW."ROWGUID" := SUBSTR(`, 36},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, objectsOnly := range []bool{false, true} {
				name := "after historical data"
				if objectsOnly {
					name = "triggers only"
				}
				t.Run(name, func(t *testing.T) {
					m, r, w := mysqlDMTriggerFixture(t)
					r.ddl["OrderData"].Columns = []source.ColumnInfo{{Name: "Id", DataType: "int"}, {Name: "RowGuid", DataType: "varchar", Length: tc.length}}
					historical := [][]interface{}{{1, tc.historical}, {2, strings.ToLower(tc.historical)}}
					r.rows["OrderData"] = historical
					r.triggers[0].Name = "FillRowGuid"
					r.triggers[0].Body = "BEGIN\nSET NEW.RowGuid = " + tc.rhs + ";\nEND"
					r.triggers[0].SourceDDL = "CREATE TRIGGER FillRowGuid BEFORE INSERT ON OrderData FOR EACH ROW " + r.triggers[0].Body
					reader := &mysqlDMRowGUIDReader{triggerCharacterTestReader: &triggerCharacterTestReader{triggerTestReader: r, characterErr: errors.New("source character metadata unavailable")}}
					writer := &mysqlDMRowGUIDWriter{mysqlDMTriggerWriter: w}
					m.reader, m.writer = reader, writer
					m.cfg.CaptureRepair = true
					if objectsOnly {
						m.cfg.Content, m.cfg.Objects, m.cfg.TableNames = "schema_only", []string{"triggers"}, []string{"OrderData"}
					}
					w.beforeInstall = func() {
						if objectsOnly {
							require.Empty(t, writer.rows)
							require.Empty(t, w.created)
						} else {
							require.Equal(t, historical, writer.rows, "historical RowGuid values must load unchanged before the trigger is installed")
						}
					}
					report := m.Run(context.Background())
					require.Zero(t, report.Triggers.Failed, "%+v", report.Triggers.Items)
					require.Equal(t, 1, report.Triggers.Success)
					require.Equal(t, 1, reader.characterReads, "unavailable character metadata must not block UUID generation")
					require.Len(t, w.plans, 1)
					require.Nil(t, w.plans[0].Options.CharacterMetadata)
					require.Contains(t, w.plans[0].TriggerSQL, tc.expectedSQL)
					require.Equal(t, 1, strings.Count(w.plans[0].TriggerSQL, "SYS_GUID()"))
					require.Equal(t, r.triggers[0].SourceDDL, w.plans[0].Source.SourceDDL)
					triggerObjects := 0
					for _, object := range report.RepairManifest.Objects {
						if object.Kind == "trigger" {
							triggerObjects++
							require.Equal(t, "succeeded", object.Status)
							require.False(t, object.DiagnosticOnly)
							require.NotNil(t, object.MySQLDamengTrigger)
							require.Len(t, object.Units, 1)
							require.Equal(t, "succeeded", object.Units[0].Status)
						}
					}
					require.Equal(t, 1, triggerObjects)
				})
			}
		})
	}
}

func TestMySQLDamengTriggerOrderAndIndependentFailure(t *testing.T) {
	for _, failure := range []string{"unsupported", "execute"} {
		t.Run(failure, func(t *testing.T) {
			m, r, w := mysqlDMTriggerFixture(t)
			first := r.triggers[0]
			second := first
			second.Name, second.ActionOrder = "Second", 2
			third := first
			third.Name, third.Event = "Independent", "UPDATE"
			r.triggers = []source.TriggerInfo{second, third, first}
			if failure == "unsupported" {
				r.triggers[2].Body = "CALL unsupported()"
			} else {
				w.failName = "FILLAMOUNT"
			}
			report := m.Run(context.Background())
			require.Equal(t, 3, report.Triggers.Total)
			require.Equal(t, 1, report.Triggers.Success)
			require.Equal(t, 2, report.Triggers.Failed)
			require.Contains(t, report.Triggers.Items[len(report.Triggers.Items)-1].Error, "前序触发器")
			for _, p := range w.plans {
				require.NotEqual(t, "SECOND", p.Name)
			}
		})
	}
	t.Run("ordered successes", func(t *testing.T) {
		m, r, w := mysqlDMTriggerFixture(t)
		second := r.triggers[0]
		second.Name, second.ActionOrder = "Second", 2
		r.triggers = []source.TriggerInfo{second, r.triggers[0]}
		report := m.Run(context.Background())
		require.Equal(t, 2, report.Triggers.Success, "%+v", report.Triggers.Items)
		require.Equal(t, "FILLAMOUNT", w.plans[1].Predecessor)
		require.Contains(t, w.plans[1].TriggerSQL, `FOLLOWS "DST"."FILLAMOUNT"`)
		require.Equal(t, w.plans[0].TriggerSQL, w.plans[1].Options.PredecessorSQLs["FILLAMOUNT"])
	})
}

func TestMySQLDamengTriggerModesSelectionAndLoadGuard(t *testing.T) {
	for _, mode := range []string{"schema_only", "objects", "disabled", "data_only"} {
		t.Run(mode, func(t *testing.T) {
			m, r, w := mysqlDMTriggerFixture(t)
			switch mode {
			case "objects":
				m.cfg.Content = "schema_only"
				m.cfg.Objects = []string{"triggers"}
				m.cfg.TableNames = []string{"OrderData"}
			case "disabled":
				m.cfg.MigrateTriggers = false
			default:
				m.cfg.Content = mode
			}
			report := m.Run(context.Background())
			if mode == "disabled" || mode == "data_only" {
				require.False(t, report.TriggerMigrationEnabled)
				require.Zero(t, r.reads)
				require.Empty(t, w.plans)
			} else {
				require.Equal(t, 1, report.Triggers.Success, "%+v", report.Triggers.Items)
				require.Empty(t, w.copied)
			}
			if mode == "objects" {
				require.Empty(t, w.created)
			}
		})
	}
	t.Run("only blocked table skipped", func(t *testing.T) {
		m, r, w := mysqlDMTriggerFixture(t)
		r.tables = append(r.tables, "Other")
		r.ddl["Other"] = &source.TableDDLInfo{TableName: "Other", Columns: []source.ColumnInfo{{Name: "Id", DataType: "int"}}}
		r.rows["Other"] = [][]interface{}{{2}}
		m.cfg.Content = "data_only"
		w.loadBlocked = "ORDERDATA"
		report := m.Run(context.Background())
		require.Equal(t, 2, report.Data.Total)
		require.Equal(t, 1, report.Data.Failed)
		require.Equal(t, []string{"Other"}, w.copied)
		require.Equal(t, "preflight", report.Data.Items[0].Stage)
	})
	t.Run("excluded trigger", func(t *testing.T) {
		m, _, w := mysqlDMTriggerFixture(t)
		m.cfg.ExcludedObjects = []ObjectRef{{Kind: "trigger", Name: "FillAmount"}}
		report := m.Run(context.Background())
		require.Zero(t, report.Triggers.Total)
		require.Empty(t, w.plans)
	})
}

func TestMySQLDamengTriggerFailuresAndMappings(t *testing.T) {
	for _, kind := range []string{"metadata", "schema", "data", "name_collision", "column_mapping", "validation"} {
		t.Run(kind, func(t *testing.T) {
			m, r, w := mysqlDMTriggerFixture(t)
			switch kind {
			case "metadata":
				r.triggerErr = errors.New("permission denied")
			case "schema":
				w.createTableFail = true
			case "data":
				w.copyDataFail = true
			case "name_collision":
				other := r.triggers[0]
				other.Name = strings.ToUpper(other.Name)
				other.Event = "UPDATE"
				r.triggers = append(r.triggers, other)
			case "column_mapping":
				r.ddl["OrderData"].Columns = append(r.ddl["OrderData"].Columns, source.ColumnInfo{Name: "ROWID", DataType: "int"})
				r.triggers[0].Body = "SET NEW.ROWID=1"
			case "validation":
				m.cfg.ValidateData = true
			}
			report := m.Run(context.Background())
			if kind == "column_mapping" {
				require.Equal(t, 1, report.Triggers.Success, "%+v", report.Triggers.Items)
				require.Contains(t, w.plans[0].TriggerSQL, `:NEW."ROW_ID"`)
			} else {
				require.NotZero(t, report.Triggers.Failed, "%+v", report)
				require.Empty(t, w.plans)
			}
		})
	}
}
