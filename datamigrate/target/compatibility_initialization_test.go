package target

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"dbgold/datamigrate/source"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func expectBuiltinMissingForInitialization(m sqlmock.Sqlmock, schema string, spec compatibilityFunctionSpec) {
	expectUUIDHeader(m, schema)
	m.ExpectQuery(`SELECT p.oid,p.pronargdefaults,p.proretset,p.provariadic,p.prorettype FROM`).
		WithArgs(schema, spec.name).WillReturnRows(sqlmock.NewRows([]string{"oid", "defaults", "set", "variadic", "return"}))
	expectUUIDSavepoint(m)
	args := ""
	for i := range spec.inputs {
		if i > 0 {
			args += ","
		}
		args += "character varying"
	}
	m.ExpectQuery(regexp.QuoteMeta(`SELECT $1::regprocedure::oid`)).WithArgs(qualifiedFunction(schema, spec.name) + "(" + args + ")").
		WillReturnError(&pq.Error{Code: "42883", Message: "function does not exist"})
	expectUUIDProbe(m, true)
	expectBuiltinNoConflictsForInitialization(m, schema, spec.name, 0)
}

func expectBuiltinNoConflictsForInitialization(m sqlmock.Sqlmock, schema, name string, oid int64) {
	m.ExpectQuery(`SELECT EXISTS .*p.proname=COALESCE`).WithArgs(schema, name, oid).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
}

func expectBuiltinCreatedForInitialization(m sqlmock.Sqlmock, schema string, spec compatibilityFunctionSpec, createErr error) {
	expectBuiltinMissingForInitialization(m, schema, spec)
	create := m.ExpectExec(regexp.QuoteMeta("CREATE FUNCTION " + qualifiedFunction(schema, spec.name)))
	if createErr != nil {
		create.WillReturnError(createErr)
		m.ExpectRollback()
	} else {
		create.WillReturnResult(sqlmock.NewResult(0, 0))
		m.ExpectCommit()
	}
}

func TestFourCompatibilityFunctionsAcrossPGProducts(t *testing.T) {
	for _, product := range []string{"postgres", "gaussdb", "highgo", "vastbase", "gbase", "seabox", "kingbase"} {
		t.Run(product, func(t *testing.T) {
			w, mock := repairPGMock(t)
			w.dbType = product
			var actual Writer = w
			switch product {
			case "gaussdb":
				actual = &GaussDBWriter{db: w.db, schema: "app"}
			case "highgo":
				actual = &HighGoWriter{db: w.db, schema: "app"}
			case "kingbase":
				actual = &KingbaseWriter{db: w.db, schema: "app"}
			case "vastbase":
				actual = &VastbaseWriter{PostgresWriter: w}
			case "gbase":
				actual = &GBaseWriter{PostgresWriter: w}
			}
			expectUUIDReused(mock)
			for _, spec := range builtinCompatibilitySpecs {
				expectBuiltinCreatedForInitialization(mock, "app", spec, nil)
			}
			report, err := EnsureCompatibilityFunctions(context.Background(), actual)
			require.NoError(t, err)
			require.Equal(t, 4, report.Total)
			require.Equal(t, 4, report.Success)
			require.Equal(t, 1, report.Reused)
			require.Equal(t, 3, report.Created)
			require.Zero(t, report.Failed)
			for i, spec := range builtinCompatibilitySpecs {
				require.Equal(t, spec.signature, report.Items[i+1].Signature)
				require.Contains(t, report.Items[i+1].DDL, qualifiedFunction("app", spec.name))
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestCompatibilityFunctionFailuresDoNotRollbackOtherFunctions(t *testing.T) {
	for failed := -1; failed < len(builtinCompatibilitySpecs); failed++ {
		name := "get_uuid"
		if failed >= 0 {
			name = builtinCompatibilitySpecs[failed].name
		}
		t.Run(name, func(t *testing.T) {
			w, mock := repairPGMock(t)
			if failed < 0 {
				mock.ExpectBegin().WillReturnError(errors.New("UUID transaction unavailable"))
			} else {
				expectUUIDReused(mock)
			}
			for i, spec := range builtinCompatibilitySpecs {
				var createErr error
				if i == failed {
					createErr = &pq.Error{Code: "42501", Message: "permission denied"}
				}
				expectBuiltinCreatedForInitialization(mock, "app", spec, createErr)
			}
			report, err := EnsureCompatibilityFunctions(context.Background(), w)
			require.Error(t, err)
			require.Equal(t, 4, report.Total)
			require.Equal(t, 3, report.Success)
			require.Equal(t, 1, report.Failed)
			require.Equal(t, "failed", report.Items[failed+1].Status)
			for i, item := range report.Items {
				if i != failed+1 {
					require.Contains(t, []string{"created", "reused"}, item.Status)
					require.Empty(t, item.Error)
				}
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestBuiltinConcurrentCreationRetriesOnlyItsOwnTransaction(t *testing.T) {
	w, mock := repairPGMock(t)
	spec := builtinCompatibilitySpecs[1]
	expectBuiltinCreatedForInitialization(mock, "app", spec, &pq.Error{Code: "42723", Message: "already exists"})
	expectUUIDHeader(mock, "app")
	mock.ExpectQuery(`SELECT p.oid,p.pronargdefaults,p.proretset,p.provariadic,p.prorettype FROM`).
		WithArgs("app", "count_rows").WillReturnRows(sqlmock.NewRows([]string{"oid", "defaults", "set", "variadic", "return"}).AddRow(77, 1, false, 0, 1700))
	expectBuiltinNoConflictsForInitialization(mock, "app", "count_rows", 77)
	expectUUIDSavepoint(mock)
	mock.ExpectQuery(`SELECT pg_get_functiondef`).WithArgs(int64(77)).WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow("EXISTING ROW COUNTER"))
	expectUUIDProbe(mock, false)
	mock.ExpectRollback()
	report, err := ensurePGBuiltinCompatibility(context.Background(), w.db, "app", spec)
	require.NoError(t, err)
	require.Equal(t, 1, report.Reused)
	require.Zero(t, report.Created)
	require.Equal(t, "EXISTING ROW COUNTER", report.Items[0].DDL)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBuiltinRejectsIncompatibleExistingStructureWithoutReplacing(t *testing.T) {
	for _, sample := range []struct {
		name     string
		defaults int
		set      bool
		variadic int
		result   int
	}{
		{"missing_owner_default", 0, false, 0, 1700},
		{"set_returning", 1, true, 0, 1700},
		{"variadic", 1, false, 1043, 1700},
		{"procedure_or_void", 1, false, 0, 2278},
	} {
		t.Run(sample.name, func(t *testing.T) {
			w, mock := repairPGMock(t)
			expectUUIDHeader(mock, "app")
			mock.ExpectQuery(`SELECT p.oid,p.pronargdefaults,p.proretset,p.provariadic,p.prorettype FROM`).
				WithArgs("app", "count_rows").WillReturnRows(sqlmock.NewRows([]string{"oid", "defaults", "set", "variadic", "return"}).
				AddRow(77, sample.defaults, sample.set, sample.variadic, sample.result))
			expectBuiltinNoConflictsForInitialization(mock, "app", "count_rows", 77)
			mock.ExpectRollback()
			report, err := ensurePGBuiltinCompatibility(context.Background(), w.db, "app", builtinCompatibilitySpecs[1])
			require.ErrorContains(t, err, "已保留原定义")
			require.Equal(t, 1, report.Failed)
			require.Empty(t, report.Items[0].DDL)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestCompatibilityPinsDefaultSchemaForIndependentTransactions(t *testing.T) {
	w, mock := repairPGMock(t)
	w.schema = ""
	expectUUIDHeader(mock, "")
	expectUUIDExisting(mock, "app", "get_uuid", true)
	mock.ExpectRollback()
	for _, spec := range builtinCompatibilitySpecs {
		expectBuiltinCreatedForInitialization(mock, "app", spec, nil)
	}
	report, err := EnsureCompatibilityFunctions(context.Background(), w)
	require.NoError(t, err)
	for _, item := range report.Items {
		require.Equal(t, "app", item.Schema)
	}
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBuiltinQuotesSchemaAndDoesNotInventUnreadableDefinition(t *testing.T) {
	w, mock := repairPGMock(t)
	schema := `a"b'c\d`
	spec := builtinCompatibilitySpecs[0]
	expectUUIDHeader(mock, schema)
	mock.ExpectQuery(`SELECT p.oid,p.pronargdefaults,p.proretset,p.provariadic,p.prorettype FROM`).
		WithArgs(schema, spec.name).WillReturnRows(sqlmock.NewRows([]string{"oid", "defaults", "set", "variadic", "return"}).AddRow(88, 0, false, 0, 1043))
	expectBuiltinNoConflictsForInitialization(mock, schema, spec.name, 88)
	expectUUIDSavepoint(mock)
	mock.ExpectQuery(`SELECT pg_get_functiondef`).WithArgs(int64(88)).WillReturnError(errors.New("definition denied"))
	expectUUIDProbe(mock, true)
	mock.ExpectRollback()
	report, err := ensurePGBuiltinCompatibility(context.Background(), w.db, schema, spec)
	require.NoError(t, err)
	require.Equal(t, 1, report.Reused)
	require.Empty(t, report.Items[0].DDL)
	require.Contains(t, report.Items[0].DefinitionWarning, "definition denied")
	require.Contains(t, source.QuotePGIdent(schema), `""`)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBuiltinTypedAliasResolutionWithLegacyCatalog(t *testing.T) {
	w, mock := repairPGMock(t)
	spec := builtinCompatibilitySpecs[2]
	mock.ExpectBegin()
	expectUUIDSavepoint(mock)
	mock.ExpectQuery(`SELECT EXISTS .*pg_catalog.pg_proc`).WillReturnError(&pq.Error{Code: "42P01", Message: "catalog unavailable"})
	expectUUIDProbe(mock, true)
	expectUUIDSavepoint(mock)
	mock.ExpectQuery(`SELECT EXISTS .*sys_catalog.pg_proc`).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	expectUUIDProbe(mock, false)
	mock.ExpectExec(regexp.QuoteMeta(`SET LOCAL search_path TO "app", sys_catalog, public`)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT p.oid,p.pronargdefaults,p.proretset,p.provariadic,p.prorettype FROM sys_catalog.pg_proc`).
		WithArgs("app", "fun_getpy").WillReturnRows(sqlmock.NewRows([]string{"oid", "defaults", "set", "variadic", "return"}))
	expectUUIDSavepoint(mock)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT $1::regprocedure::oid`)).WithArgs(`"app"."fun_getpy"(character varying)`).
		WillReturnRows(sqlmock.NewRows([]string{"oid"}).AddRow(99))
	expectUUIDProbe(mock, false)
	mock.ExpectQuery(`SELECT p.oid,p.pronargdefaults,p.proretset,p.provariadic,p.prorettype FROM sys_catalog.pg_proc`).
		WithArgs(int64(99), "app").WillReturnRows(sqlmock.NewRows([]string{"oid", "defaults", "set", "variadic", "return"}).AddRow(99, 0, false, 0, 1043))
	expectBuiltinNoConflictsForInitialization(mock, "app", spec.name, 99)
	expectUUIDSavepoint(mock)
	mock.ExpectQuery(`SELECT pg_get_functiondef`).WithArgs(int64(99)).WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow("EXISTING FUN_GETPY"))
	expectUUIDProbe(mock, false)
	mock.ExpectRollback()
	report, err := ensurePGBuiltinCompatibility(context.Background(), w.db, "app", spec)
	require.NoError(t, err)
	require.Equal(t, 1, report.Reused)
	require.Equal(t, "EXISTING FUN_GETPY", report.Items[0].DDL)
	require.NoError(t, mock.ExpectationsWereMet())
}
