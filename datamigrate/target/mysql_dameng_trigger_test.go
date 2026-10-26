package target

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/mysqldmtrigger"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func dmTriggerPlanFixture(t *testing.T) mysqldmtrigger.Plan {
	t.Helper()
	info := source.TriggerInfo{Schema: "Source", Name: "BeforeInsert", TableName: "Orders", Timing: "BEFORE", Event: "INSERT", Body: "SET NEW.Value = NEW.Value + 1", SourceDDL: "CREATE TRIGGER BeforeInsert BEFORE INSERT ON Orders FOR EACH ROW SET NEW.Value = NEW.Value + 1", SQLMode: "STRICT_TRANS_TABLES"}
	table := &source.TableDDLInfo{TableName: "Orders", Columns: []source.ColumnInfo{{Name: "Value", DataType: "int"}}}
	p, err := mysqldmtrigger.Convert(info, map[string]*source.TableDDLInfo{"Orders": table}, mysqldmtrigger.Options{Schema: "APP", TableNames: map[string]string{"Orders": "ORDERS"}, ColumnNames: map[string]map[string]string{"Orders": {"Value": "VALUE"}}, Capabilities: mysqldmtrigger.Capabilities{CompatibleMode: "0"}})
	require.NoError(t, err)
	return p
}

func dmTriggerWriterFixture(t *testing.T) (*DaMengWriter, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return &DaMengWriter{db: db, schema: "APP", srcType: "mysql", dia: dialect.NewDaMeng()}, mock
}

func expectDMTriggerTarget(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`SELECT SF_GET_PARA_VALUE`).WillReturnRows(sqlmock.NewRows([]string{"mode"}).AddRow("0"))
	mock.ExpectQuery(`SELECT CASE WHEN '' IS NULL`).WillReturnRows(sqlmock.NewRows([]string{"empty", "concat"}).AddRow(0, 0))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM ALL_TABLES`).WithArgs("APP", "ORDERS").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`SELECT c.COLUMN_NAME,c.DATA_TYPE`).WithArgs("APP", "ORDERS").WillReturnRows(sqlmock.NewRows([]string{"name", "type", "length", "precision", "scale", "flags"}).AddRow("VALUE", "INTEGER", 4, 10, 0, 0))
}

func dmTriggerRows(p mysqldmtrigger.Plan, body string) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{"owner", "name", "table_owner", "table", "timing", "event", "state", "description", "body"})
	if body != "" {
		rows.AddRow(p.Schema, p.Name, p.Schema, p.Table, "BEFORE EACH ROW", "INSERT", "Y", nil, body)
	}
	return rows
}

func expectDMTriggerInspection(mock sqlmock.Sqlmock, p mysqldmtrigger.Plan, exists bool, body string) {
	objects := sqlmock.NewRows([]string{"type", "status"})
	if exists {
		objects.AddRow("TRIGGER", "VALID")
	}
	mock.ExpectQuery(`SELECT OBJECT_TYPE,STATUS FROM ALL_OBJECTS`).WithArgs(p.Schema, p.Name).WillReturnRows(objects)
	mock.ExpectQuery(`SELECT OWNER,TRIGGER_NAME,TABLE_OWNER`).WithArgs(p.Schema, p.Table).WillReturnRows(dmTriggerRows(p, body))
}

func dmTriggerRepairFixture(p mysqldmtrigger.Plan, status string) repair.Object {
	return repair.Object{Kind: "trigger", Schema: p.Schema, Table: p.Table, TargetName: p.Name, DDL: p.TriggerSQL, MySQLDamengTrigger: &p, Status: status, Units: []repair.Unit{{ID: "create", SQL: p.TriggerSQL, OriginalSQL: p.TriggerSQL, Status: status, Selected: true}}}
}

func TestMySQLDamengTriggerWholeUnitAndCatalogValidation(t *testing.T) {
	p := dmTriggerPlanFixture(t)
	w, mock := dmTriggerWriterFixture(t)
	expectDMTriggerTarget(mock)
	expectDMTriggerInspection(mock, p, false, "")
	mock.ExpectExec(regexp.QuoteMeta(p.TriggerSQL)).WillReturnResult(sqlmock.NewResult(0, 0))
	expectDMTriggerInspection(mock, p, true, p.TriggerSQL)
	var states []string
	ctx := repair.WithObserver(context.Background(), func(e repair.Event) error {
		states = append(states, e.Status)
		require.Equal(t, p.TriggerSQL, e.SQL)
		return nil
	})
	require.NoError(t, CreateMySQLDamengTrigger(ctx, w, p))
	require.Equal(t, []string{"running", "succeeded"}, states)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestMySQLDamengTriggerCapabilitiesDoNotRequireINIPrivilege(t *testing.T) {
	w, mock := dmTriggerWriterFixture(t)
	mock.ExpectQuery(`SELECT SF_GET_PARA_VALUE`).WillReturnError(errors.New("[-5730] No select INI parameter privilege"))
	mock.ExpectQuery(`SELECT CASE WHEN '' IS NULL`).WillReturnRows(sqlmock.NewRows([]string{"empty", "concat"}).AddRow(1, 0))
	c, err := DetectMySQLDamengTriggerCapabilities(context.Background(), w)
	require.NoError(t, err)
	require.Equal(t, mysqldmtrigger.Capabilities{CompatibleMode: "unknown", EmptyStringIsNull: true}, c)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestMySQLDamengTriggerCapabilitiesRequireBehavioralEvidence(t *testing.T) {
	w, mock := dmTriggerWriterFixture(t)
	mock.ExpectQuery(`SELECT SF_GET_PARA_VALUE`).WillReturnError(errors.New("not permitted"))
	mock.ExpectQuery(`SELECT CASE WHEN '' IS NULL`).WillReturnError(errors.New("behavior query failed"))
	_, err := DetectMySQLDamengTriggerCapabilities(context.Background(), w)
	require.ErrorContains(t, err, "behavior query failed")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestMySQLDamengTriggerOrdinaryMigrationReusesExactExistingDefinition(t *testing.T) {
	p := dmTriggerPlanFixture(t)
	w, mock := dmTriggerWriterFixture(t)
	expectDMTriggerTarget(mock)
	expectDMTriggerInspection(mock, p, true, p.TriggerSQL)
	var state string
	ctx := repair.WithObserver(context.Background(), func(e repair.Event) error { state = e.Status; return nil })
	require.NoError(t, CreateMySQLDamengTrigger(ctx, w, p))
	require.Equal(t, "reused", state)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestMySQLDamengTriggerKnownInvalidIsFailed(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(map[bool]string{false: "created", true: "retry"}[retry], func(t *testing.T) {
			p := dmTriggerPlanFixture(t)
			w, mock := dmTriggerWriterFixture(t)
			expectDMTriggerTarget(mock)
			if !retry {
				expectDMTriggerInspection(mock, p, false, "")
				mock.ExpectExec(regexp.QuoteMeta(p.TriggerSQL)).WillReturnResult(sqlmock.NewResult(0, 0))
			}
			mock.ExpectQuery(`SELECT OBJECT_TYPE,STATUS FROM ALL_OBJECTS`).WithArgs(p.Schema, p.Name).WillReturnRows(sqlmock.NewRows([]string{"type", "status"}).AddRow("TRIGGER", "INVALID"))
			var state string
			ctx := repair.WithObserver(context.Background(), func(e repair.Event) error { state = e.Status; return nil })
			err := w.createMySQLDamengTrigger(ctx, p, retry)
			require.ErrorContains(t, err, "编译无效")
			require.Equal(t, "failed", state)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestMySQLDamengTriggerConflictsAndUnknownOutcomes(t *testing.T) {
	for _, scenario := range []string{"collision", "execution_unknown", "catalog_unknown", "observer_running"} {
		t.Run(scenario, func(t *testing.T) {
			p := dmTriggerPlanFixture(t)
			w, mock := dmTriggerWriterFixture(t)
			expectDMTriggerTarget(mock)
			if scenario == "collision" {
				expectDMTriggerInspection(mock, p, true, strings.Replace(p.TriggerSQL, " + 1", " + 99", 1))
			} else {
				expectDMTriggerInspection(mock, p, false, "")
				if scenario == "execution_unknown" {
					mock.ExpectExec(regexp.QuoteMeta(p.TriggerSQL)).WillReturnError(context.DeadlineExceeded)
				}
				if scenario == "catalog_unknown" {
					mock.ExpectExec(regexp.QuoteMeta(p.TriggerSQL)).WillReturnResult(sqlmock.NewResult(0, 0))
					mock.ExpectQuery(`SELECT OBJECT_TYPE,STATUS FROM ALL_OBJECTS`).WillReturnError(errors.New("catalog unavailable"))
				}
			}
			var state string
			ctx := repair.WithObserver(context.Background(), func(e repair.Event) error {
				state = e.Status
				if scenario == "observer_running" {
					return errors.New("ledger unavailable")
				}
				return nil
			})
			err := CreateMySQLDamengTrigger(ctx, w, p)
			require.Error(t, err)
			if scenario == "collision" || scenario == "observer_running" {
				require.True(t, IsMySQLDamengTriggerPreflight(err))
			} else {
				require.Equal(t, "unknown", state)
				require.False(t, IsMySQLDamengTriggerPreflight(err))
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestMySQLDamengTriggerRepairReconcilesWithoutReplay(t *testing.T) {
	for _, status := range []string{"failed", "unknown"} {
		for _, targetState := range []string{"identical", "absent", "different", "unreadable"} {
			t.Run(status+"/"+targetState, func(t *testing.T) {
				p := dmTriggerPlanFixture(t)
				w, mock := dmTriggerWriterFixture(t)
				expectDMTriggerTarget(mock)
				switch targetState {
				case "identical":
					expectDMTriggerInspection(mock, p, true, p.TriggerSQL)
				case "absent":
					expectDMTriggerInspection(mock, p, false, "")
					mock.ExpectExec(regexp.QuoteMeta(p.TriggerSQL)).WillReturnResult(sqlmock.NewResult(0, 0))
					expectDMTriggerInspection(mock, p, true, p.TriggerSQL)
				case "different":
					expectDMTriggerInspection(mock, p, true, p.TriggerSQL+"\nNULL;")
				case "unreadable":
					mock.ExpectQuery(`SELECT OBJECT_TYPE,STATUS FROM ALL_OBJECTS`).WillReturnError(errors.New("permission denied"))
				}
				o := dmTriggerRepairFixture(p, status)
				var saved []repair.Unit
				units, err := ExecuteRepair(context.Background(), w, o, o.Units, func(units []repair.Unit) error { saved = units; return nil })
				if targetState == "identical" || targetState == "absent" {
					require.NoError(t, err)
					require.Equal(t, "succeeded", units[0].Status)
					require.False(t, units[0].Selected)
				} else {
					require.Error(t, err)
					require.Equal(t, "unknown", units[0].Status)
				}
				require.Equal(t, units, saved)
				require.NoError(t, mock.ExpectationsWereMet())
			})
		}
	}
}

func TestMySQLDamengTriggerRepairPersistenceFailureRemainsUnknown(t *testing.T) {
	p := dmTriggerPlanFixture(t)
	w, mock := dmTriggerWriterFixture(t)
	expectDMTriggerTarget(mock)
	expectDMTriggerInspection(mock, p, true, p.TriggerSQL)
	o := dmTriggerRepairFixture(p, "unknown")
	units, err := ExecuteRepair(context.Background(), w, o, o.Units, func([]repair.Unit) error { return errors.New("ledger failed") })
	require.ErrorContains(t, err, "ledger failed")
	require.Equal(t, "unknown", units[0].Status)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestMySQLDamengTriggerImmutableRepairAndLexicalEvidence(t *testing.T) {
	p := dmTriggerPlanFixture(t)
	o := dmTriggerRepairFixture(p, "failed")
	require.NoError(t, ValidateMySQLDamengTriggerRepair(o, o.Units))
	edited := append([]repair.Unit(nil), o.Units...)
	edited[0].SQL += "\nNULL;"
	require.Error(t, ValidateMySQLDamengTriggerRepair(o, edited))
	require.Error(t, ValidateMySQLDamengTriggerRepair(o, append(edited, edited[0])))
	actual := oracleDamengSequenceTrigger{owner: p.Schema, name: p.Name, tableOwner: p.Schema, table: p.Table, timing: "BEFORE ROW", event: "INSERT", status: "ENABLED", body: p.TriggerSQL}
	require.True(t, mysqlDMTriggerMatches(actual, p.TriggerSQL, p.Schema))
	actual.body = strings.Replace(p.TriggerSQL, `"VALUE"`, `"Value"`, 1)
	require.False(t, mysqlDMTriggerMatches(actual, p.TriggerSQL, p.Schema))
	actual.body = p.TriggerSQL + "\nNULL;"
	require.False(t, mysqlDMTriggerMatches(actual, p.TriggerSQL, p.Schema))
	pos := strings.Index(p.TriggerSQL, "\nBEGIN")
	actual.body = p.TriggerSQL[pos:]
	actual.description = sql.NullString{String: p.TriggerSQL[:pos], Valid: true}
	require.True(t, mysqlDMTriggerMatches(actual, p.TriggerSQL, p.Schema), "catalog may separate declaration and body")
	actual.description.String += " WHEN (1=1)"
	require.False(t, mysqlDMTriggerMatches(actual, p.TriggerSQL, p.Schema), "a body alone never proves trigger scope")
}

func TestMySQLDamengTriggerColumnCapacity(t *testing.T) {
	require.True(t, mysqlDMCompatibleColumn("VARCHAR2(30)", source.ColumnInfo{DataType: "NVARCHAR2", Length: 40}))
	require.False(t, mysqlDMCompatibleColumn("VARCHAR2(30)", source.ColumnInfo{DataType: "VARCHAR", Length: 20}))
	require.True(t, mysqlDMCompatibleColumn("NUMBER(12,2)", source.ColumnInfo{DataType: "DECIMAL", Precision: 14, Scale: 3}))
	require.False(t, mysqlDMCompatibleColumn("NUMBER(12,2)", source.ColumnInfo{DataType: "NUMBER", Precision: 12, Scale: 3}))
	require.False(t, mysqlDMCompatibleColumn("INT", source.ColumnInfo{DataType: "VARCHAR", Length: 30}))
}

func TestMySQLDamengTriggerRejectsGeneratedTargetColumns(t *testing.T) {
	p := dmTriggerPlanFixture(t)
	w, mock := dmTriggerWriterFixture(t)
	mock.ExpectQuery(`SELECT SF_GET_PARA_VALUE`).WillReturnRows(sqlmock.NewRows([]string{"mode"}).AddRow("0"))
	mock.ExpectQuery(`SELECT CASE WHEN '' IS NULL`).WillReturnRows(sqlmock.NewRows([]string{"empty", "concat"}).AddRow(0, 0))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM ALL_TABLES`).WithArgs("APP", "ORDERS").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`SELECT c.COLUMN_NAME,c.DATA_TYPE.*SYSCOLINFOS`).WithArgs("APP", "ORDERS").WillReturnRows(sqlmock.NewRows([]string{"name", "type", "length", "precision", "scale", "flags"}).AddRow("VALUE", "INT", 0, 0, 0, 1))
	err := CreateMySQLDamengTrigger(context.Background(), w, p)
	require.ErrorContains(t, err, "名称、类型或容量不兼容")
	require.True(t, IsMySQLDamengTriggerPreflight(err))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestMySQLDamengTriggerFrozenOrderingRejectsChangedPredecessor(t *testing.T) {
	first := dmTriggerPlanFixture(t)
	sourceTables := map[string]*source.TableDDLInfo{first.SourceTables[0].TableName: &first.SourceTables[0]}
	src := first.Source
	src.Name = "Second"
	options := first.Options
	options.Predecessor = first.Name
	options.PredecessorSQLs = map[string]string{first.Name: first.TriggerSQL}
	second, err := mysqldmtrigger.Convert(src, sourceTables, options)
	require.NoError(t, err)
	w, mock := dmTriggerWriterFixture(t)
	expectDMTriggerTarget(mock)
	mock.ExpectQuery(`SELECT OBJECT_TYPE,STATUS FROM ALL_OBJECTS`).WithArgs(second.Schema, second.Name).WillReturnRows(sqlmock.NewRows([]string{"type", "status"}))
	rows := dmTriggerRows(first, first.TriggerSQL+"\nNULL;")
	mock.ExpectQuery(`SELECT OWNER,TRIGGER_NAME,TABLE_OWNER`).WithArgs(second.Schema, second.Table).WillReturnRows(rows)
	err = CreateMySQLDamengTrigger(context.Background(), w, second)
	require.ErrorContains(t, err, "冻结顺序链")
	require.True(t, IsMySQLDamengTriggerPreflight(err))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestMySQLDamengTriggerRerunAcceptsOnlyFrozenExistingPeers(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "identical_peer", true: "changed_peer"}[changed], func(t *testing.T) {
			first := dmTriggerPlanFixture(t)
			sourceTables := map[string]*source.TableDDLInfo{first.SourceTables[0].TableName: &first.SourceTables[0]}
			src := first.Source
			src.Name = "Second"
			options := first.Options
			options.Predecessor = first.Name
			options.PredecessorSQLs = map[string]string{first.Name: first.TriggerSQL}
			second, err := mysqldmtrigger.Convert(src, sourceTables, options)
			require.NoError(t, err)
			first.Options.PeerSQLs = map[string]string{second.Name: second.TriggerSQL}
			require.NoError(t, mysqldmtrigger.ValidatePlan(first))
			w, mock := dmTriggerWriterFixture(t)
			expectDMTriggerTarget(mock)
			mock.ExpectQuery(`SELECT OBJECT_TYPE,STATUS FROM ALL_OBJECTS`).WithArgs(first.Schema, first.Name).WillReturnRows(sqlmock.NewRows([]string{"type", "status"}).AddRow("TRIGGER", "VALID"))
			peerBody := second.TriggerSQL
			if changed {
				peerBody += "\nNULL;"
			}
			rows := dmTriggerRows(first, first.TriggerSQL)
			rows.AddRow(second.Schema, second.Name, second.Schema, second.Table, "BEFORE ROW", "INSERT", "Y", nil, peerBody)
			mock.ExpectQuery(`SELECT OWNER,TRIGGER_NAME,TABLE_OWNER`).WithArgs(first.Schema, first.Table).WillReturnRows(rows)
			if !changed {
				mock.ExpectQuery(`SELECT OBJECT_TYPE,STATUS FROM ALL_OBJECTS`).WithArgs(second.Schema, second.Name).WillReturnRows(sqlmock.NewRows([]string{"type", "status"}).AddRow("TRIGGER", "VALID"))
			}
			err = CreateMySQLDamengTrigger(context.Background(), w, first)
			if changed {
				require.ErrorContains(t, err, "同组定义")
			} else {
				require.NoError(t, err)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestMySQLDamengLoadTriggerGuard(t *testing.T) {
	for _, status := range []string{"Y", "ENABLED", "N", "DISABLED", "unknown"} {
		t.Run(status, func(t *testing.T) {
			w, mock := dmTriggerWriterFixture(t)
			mock.ExpectQuery(`SELECT TRIGGER_NAME,STATUS FROM ALL_TRIGGERS`).WithArgs("APP", "ORDERS").WillReturnRows(sqlmock.NewRows([]string{"name", "status"}).AddRow("BUSINESS", status))
			err := CheckMySQLDamengLoadTriggers(context.Background(), w, "ORDERS")
			if status == "N" || status == "DISABLED" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestMySQLDamengRecoveryDoesNotExemptGeneratedBusinessTriggers(t *testing.T) {
	w, mock := dmTriggerWriterFixture(t)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM ALL_TABLES`).WithArgs("APP", "ORDERS").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM ALL_TRIGGERS.*STATUS IN`).WithArgs("APP", "ORDERS").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	_, err := InspectRecovery(context.Background(), w, "ORDERS")
	require.ErrorContains(t, err, "启用的自定义触发器")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestMySQLDamengRecoveryChecksTriggersImmediatelyBeforeMutation(t *testing.T) {
	for _, action := range []string{"truncate", "load"} {
		t.Run(action, func(t *testing.T) {
			w, mock := dmTriggerWriterFixture(t)
			mock.ExpectQuery(`SELECT TRIGGER_NAME,STATUS FROM ALL_TRIGGERS`).WithArgs("APP", "ORDERS").WillReturnRows(sqlmock.NewRows([]string{"name", "status"}).AddRow("BUSINESS", "Y"))
			var err error
			if action == "truncate" {
				err = TruncateRecovery(context.Background(), w, "ORDERS")
			} else {
				err = PrepareRecovery(context.Background(), w, "ORDERS", []source.ColumnInfo{{Name: "VALUE", DataType: "int"}})
			}
			require.ErrorContains(t, err, "禁止复制数据")
			require.NoError(t, mock.ExpectationsWereMet(), "neither truncation nor data preparation can proceed after the guard fails")
		})
	}
}
