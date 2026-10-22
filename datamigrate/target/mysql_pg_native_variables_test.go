package target

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/mysqltrigger"
	"dbgold/datamigrate/source"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func nativeVariableWriterFixture(t *testing.T) (*KingbaseWriter, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return &KingbaseWriter{db: db, schema: "Target", dia: dialect.NewPostgres("kingbase")}, mock
}

func expectNativeVariableCapability(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(regexp.QuoteMeta(mysqlPGNativeVariableModeSQL)).WillReturnRows(sqlmock.NewRows([]string{"mode"}).AddRow("mysql"))
	for _, statement := range mysqlPGNativeVariableExplainSQL {
		mock.ExpectQuery(regexp.QuoteMeta(statement)).WillReturnRows(sqlmock.NewRows([]string{"QUERY PLAN"}).AddRow("Result"))
	}
}

func TestDetectMySQLPGNativeUserVariables(t *testing.T) {
	t.Run("other product does not query", func(t *testing.T) {
		w, mock := triggerWriterFixture(t)
		supported, err := DetectMySQLPGNativeUserVariables(context.Background(), w)
		require.NoError(t, err)
		require.False(t, supported)
		require.NoError(t, mock.ExpectationsWereMet())
	})
	for _, mode := range []any{nil, "oracle", "pg", "mysql", " MYSQL "} {
		name, _ := mode.(string)
		t.Run(name, func(t *testing.T) {
			w, mock := nativeVariableWriterFixture(t)
			mock.ExpectQuery(regexp.QuoteMeta(mysqlPGNativeVariableModeSQL)).WillReturnRows(sqlmock.NewRows([]string{"mode"}).AddRow(mode))
			want := mode == "mysql" || mode == " MYSQL "
			if want {
				for _, statement := range mysqlPGNativeVariableExplainSQL {
					mock.ExpectQuery(regexp.QuoteMeta(statement)).WillReturnRows(sqlmock.NewRows([]string{"QUERY PLAN"}).AddRow("Result"))
				}
			}
			supported, err := DetectMySQLPGNativeUserVariables(context.Background(), w)
			require.NoError(t, err)
			require.Equal(t, want, supported)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestDetectMySQLPGNativeUserVariablesDistinguishesFailure(t *testing.T) {
	for _, stage := range []string{"mode", "syntax", "permission", "rows"} {
		t.Run(stage, func(t *testing.T) {
			w, mock := nativeVariableWriterFixture(t)
			if stage == "mode" {
				mock.ExpectQuery(regexp.QuoteMeta(mysqlPGNativeVariableModeSQL)).WillReturnError(errors.New("connection failed"))
			} else {
				mock.ExpectQuery(regexp.QuoteMeta(mysqlPGNativeVariableModeSQL)).WillReturnRows(sqlmock.NewRows([]string{"mode"}).AddRow("mysql"))
				explain := mock.ExpectQuery(regexp.QuoteMeta(mysqlPGNativeVariableExplainSQL[0]))
				switch stage {
				case "syntax":
					explain.WillReturnError(&pq.Error{Code: "42601", Message: "syntax error"})
				case "permission":
					explain.WillReturnError(&pq.Error{Code: "42501", Message: "permission denied"})
				case "rows":
					explain.WillReturnRows(sqlmock.NewRows([]string{"QUERY PLAN"}).AddRow("Result").RowError(0, errors.New("read failed")))
				}
			}
			supported, err := DetectMySQLPGNativeUserVariables(context.Background(), w)
			require.False(t, supported)
			if stage == "syntax" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

type nativeVariableDetectorWriter struct {
	Writer
	product string
	called  bool
}

func (w *nativeVariableDetectorWriter) DBType() string { return w.product }
func (w *nativeVariableDetectorWriter) DetectMySQLPGNativeUserVariables(context.Context) (bool, error) {
	w.called = true
	return true, nil
}

func TestNativeVariableDetectorCannotEnableOtherProduct(t *testing.T) {
	for _, product := range []string{"postgres", "kingbase"} {
		w := &nativeVariableDetectorWriter{product: product}
		supported, err := DetectMySQLPGNativeUserVariables(context.Background(), w)
		require.NoError(t, err)
		require.Equal(t, product == "kingbase", supported)
		require.Equal(t, product == "kingbase", w.called)
	}
}

type nativeVariableCreatorWriter struct {
	Writer
	called bool
}

func (w *nativeVariableCreatorWriter) CreateMySQLPGTrigger(context.Context, mysqltrigger.Plan, string) error {
	w.called = true
	return nil
}

func TestNativeVariableCustomCreatorCannotSkipTransactionCheck(t *testing.T) {
	p := nativeVariablePlanFixture(t)
	builtin, mock := nativeVariableWriterFixture(t)
	w := &nativeVariableCreatorWriter{Writer: builtin}
	err := CreateMySQLPGTrigger(context.Background(), w, p, "")
	require.Error(t, err)
	require.False(t, w.called)
	require.NoError(t, mock.ExpectationsWereMet())
}

func nativeVariablePlanFixture(t *testing.T) mysqltrigger.Plan {
	t.Helper()
	info := source.TriggerInfo{Schema: "Source", TableName: "Orders", Name: "BeforeUpdate", Timing: "BEFORE", Event: "UPDATE", Body: "SET @CurrentValue = OLD.Value + 1", SQLMode: "STRICT_TRANS_TABLES"}
	table := source.TableDDLInfo{TableName: "Orders", Columns: []source.ColumnInfo{{Name: "Value", DataType: "int"}}}
	meta := &source.TriggerCharacterMetadata{ServerVersion: "8.0.42 Source distribution", NumericColumns: []source.TriggerNumericColumn{{Schema: "Source", Table: "Orders", Name: "Value", ColumnType: "int"}}}
	plan, err := mysqltrigger.Convert(info, map[string]*source.TableDDLInfo{"Orders": &table}, mysqltrigger.Options{Schema: "Target", CharacterMetadata: meta, NativeUserVariables: true})
	require.NoError(t, err)
	require.True(t, plan.NativeUserVariables)
	plan.Tables = []source.TableDDLInfo{{TableName: "Orders", Columns: []source.ColumnInfo{{Name: "Value", DataType: "INTEGER"}}}}
	return plan
}

func TestMySQLPGNativeUserVariablesRechecksInstallationAndRepair(t *testing.T) {
	for _, repair := range []bool{false, true} {
		t.Run(map[bool]string{false: "install", true: "repair"}[repair], func(t *testing.T) {
			p := nativeVariablePlanFixture(t)
			w, mock := nativeVariableWriterFixture(t)
			expectNativeVariableCapability(mock)
			supported, err := DetectMySQLPGNativeUserVariables(context.Background(), w)
			require.NoError(t, err)
			require.True(t, supported)
			mock.ExpectBegin()
			mock.ExpectQuery(regexp.QuoteMeta(mysqlPGNativeVariableModeSQL)).WillReturnRows(sqlmock.NewRows([]string{"mode"}).AddRow("oracle"))
			mock.ExpectRollback()
			if repair {
				o := recordedTriggerFixture(p)
				_, err = ExecuteRepair(context.Background(), w, o, o.Units, nil)
			} else {
				err = CreateMySQLPGTrigger(context.Background(), w, p, "")
			}
			require.ErrorContains(t, err, "不支持原生用户变量")
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestMySQLPGNativeUserVariablesRejectOtherProductBeforeDDL(t *testing.T) {
	p := nativeVariablePlanFixture(t)
	w, mock := triggerWriterFixture(t)
	mock.ExpectBegin()
	mock.ExpectRollback()
	err := CreateMySQLPGTrigger(context.Background(), w, p, "")
	require.ErrorContains(t, err, "仅支持 Kingbase")
	require.True(t, IsMySQLPGTriggerPreflight(err))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestMySQLPGNativeUserVariablesInstallChecksExactTransaction(t *testing.T) {
	p := nativeVariablePlanFixture(t)
	w, mock := nativeVariableWriterFixture(t)
	mock.ExpectBegin()
	expectNativeVariableCapability(mock)
	mock.ExpectQuery("SELECT EXISTS.*pg_language").WillReturnRows(sqlmock.NewRows([]string{"installed"}).AddRow(true))
	mock.ExpectExec(regexp.QuoteMeta(`LOCK TABLE "Target"."Orders" IN SHARE ROW EXCLUSIVE MODE`)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT a.attname,format_type").WithArgs("Target", "Orders").WillReturnRows(sqlmock.NewRows([]string{"column", "type", "generated"}).AddRow("Value", "integer", ""))
	mock.ExpectQuery("SELECT p.oid,p.prosrc").WithArgs("Target", p.FunctionName).WillReturnRows(sqlmock.NewRows([]string{"oid", "body", "security", "trigger", "args", "language", "config", "owner", "plain"}))
	mock.ExpectQuery("SELECT g.tgname,g.tgtype").WithArgs("Target", "Orders").WillReturnRows(sqlmock.NewRows([]string{"name", "type", "enabled", "fn", "args", "noWhen", "constraint"}))
	mock.ExpectExec(regexp.QuoteMeta(p.FunctionSQL)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(p.TriggerSQL)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()
	require.NoError(t, CreateMySQLPGTrigger(context.Background(), w, p, ""))
	require.NoError(t, mock.ExpectationsWereMet())
}
