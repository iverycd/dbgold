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

func expectProcedureHeaderForInitialization(m sqlmock.Sqlmock, schema string) {
	expectUUIDHeader(m, schema)
	m.ExpectQuery(`SELECT EXISTS .*pg_attribute.*attname='prokind'`).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
}

func procedureMetadataRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"oid", "defaults", "variadic", "modes"})
}

func expectProcedureMissingForInitialization(m sqlmock.Sqlmock, schema string, specs ...compatibilityProcedureSpec) {
	spec := compatibilityProcedureSpecs[0]
	if len(specs) > 0 {
		spec = specs[0]
	}
	expectProcedureHeaderForInitialization(m, schema)
	m.ExpectQuery(regexp.QuoteMeta(`SELECT p.oid,p.pronargdefaults,p.provariadic,COALESCE("pg_catalog".array_to_string`)).
		WithArgs(schema, spec.name).WillReturnRows(procedureMetadataRows())
	expectUUIDSavepoint(m)
	m.ExpectQuery(regexp.QuoteMeta(`SELECT $1::regprocedure::oid`)).
		WithArgs(qualifiedFunction(schema, spec.name) + "(character varying)").
		WillReturnError(&pq.Error{Code: "42883", Message: "procedure does not exist"})
	expectUUIDProbe(m, true)
	expectBuiltinNoConflictsForInitialization(m, schema, spec.name, 0)
}

func expectProcedureCreatedForInitialization(m sqlmock.Sqlmock, schema, product string, createErr error, specs ...compatibilityProcedureSpec) {
	spec := compatibilityProcedureSpecs[0]
	if len(specs) > 0 {
		spec = specs[0]
	}
	expectProcedureMissingForInitialization(m, schema, spec)
	expectProcedureWritableCTEForInitialization(m, nil)
	expectUUIDSavepoint(m)
	native := product == "gaussdb" || product == "vastbase" || product == "gbase" || product == "seabox"
	create := m.ExpectExec(regexp.QuoteMeta(spec.ddl(schema, "pg_catalog", native)))
	if createErr != nil {
		create.WillReturnError(createErr)
		expectUUIDProbe(m, true)
		m.ExpectRollback()
	} else {
		create.WillReturnResult(sqlmock.NewResult(0, 0))
		expectUUIDProbe(m, false)
		m.ExpectCommit()
	}
}

func expectProcedureWritableCTEForInitialization(m sqlmock.Sqlmock, planErr error) {
	m.ExpectExec("SAVEPOINT procedure_capability_probe").WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectExec(`CREATE TEMP TABLE "dbgold_procedure_probe_[0-9]+_[0-9]+"`).WillReturnResult(sqlmock.NewResult(0, 0))
	plan := m.ExpectExec(`EXPLAIN WITH moved AS \(DELETE FROM pg_temp\..*RETURNING probe_id\), archived AS \(INSERT INTO pg_temp\..*RETURNING 1\) SELECT \(SELECT "pg_catalog"\.count\(\*\) FROM moved\),\(SELECT "pg_catalog"\.count\(\*\) FROM archived\)`)
	if planErr == nil {
		plan.WillReturnResult(sqlmock.NewResult(0, 0))
	} else {
		plan.WillReturnError(planErr)
	}
	m.ExpectExec("ROLLBACK TO SAVEPOINT procedure_capability_probe").WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectExec("RELEASE SAVEPOINT procedure_capability_probe").WillReturnResult(sqlmock.NewResult(0, 0))
}

func expectProcedureReusedForInitialization(m sqlmock.Sqlmock, schema string, specs ...compatibilityProcedureSpec) {
	spec := compatibilityProcedureSpecs[0]
	if len(specs) > 0 {
		spec = specs[0]
	}
	expectProcedureHeaderForInitialization(m, schema)
	m.ExpectQuery(regexp.QuoteMeta(`SELECT p.oid,p.pronargdefaults,p.provariadic,COALESCE("pg_catalog".array_to_string`)).
		WithArgs(schema, spec.name).WillReturnRows(procedureMetadataRows().AddRow(88, 0, 0, ""))
	expectBuiltinNoConflictsForInitialization(m, schema, spec.name, 88)
	expectUUIDSavepoint(m)
	m.ExpectQuery(regexp.QuoteMeta(`SELECT * FROM "pg_catalog".pg_get_functiondef($1::oid)`)).
		WithArgs(int64(88)).WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow("EXISTING PROCEDURE"))
	expectUUIDProbe(m, false)
	m.ExpectRollback()
}

func TestProcedureInitializationCreatesWithoutCallingBusinessCode(t *testing.T) {
	for _, spec := range compatibilityProcedureSpecs {
		t.Run(spec.name, func(t *testing.T) {
			for _, product := range []string{"postgres", "kingbase", "highgo", "gaussdb", "vastbase", "gbase", "seabox"} {
				t.Run(product, func(t *testing.T) {
					w, mock := repairPGMock(t)
					expectProcedureCreatedForInitialization(mock, "app", product, nil, spec)
					report, err := ensurePGProcedureSpecCompatibility(context.Background(), w.db, "app", product, spec)
					require.NoError(t, err)
					require.Equal(t, 1, report.Created)
					require.Equal(t, "procedure", report.Items[0].Kind)
					require.Equal(t, spec.signature, report.Items[0].Signature)
					require.NotEmpty(t, report.Items[0].DDL)
					require.NoError(t, mock.ExpectationsWereMet())
				})
			}
		})
	}
}

func TestProcedureHeaderFallbackOnlyOnSyntaxError(t *testing.T) {
	for _, spec := range compatibilityProcedureSpecs {
		t.Run(spec.name, func(t *testing.T) {
			for _, sample := range []struct {
				product string
				native  bool
			}{{"postgres", false}, {"gaussdb", true}} {
				t.Run(sample.product, func(t *testing.T) {
					w, mock := repairPGMock(t)
					expectProcedureMissingForInitialization(mock, "app", spec)
					expectProcedureWritableCTEForInitialization(mock, nil)
					expectUUIDSavepoint(mock)
					mock.ExpectExec(regexp.QuoteMeta(spec.ddl("app", "pg_catalog", sample.native))).
						WillReturnError(&pq.Error{Code: "42601", Message: "unsupported procedure header"})
					expectUUIDProbe(mock, true)
					expectUUIDSavepoint(mock)
					fallback := spec.ddl("app", "pg_catalog", !sample.native)
					mock.ExpectExec(regexp.QuoteMeta(fallback)).WillReturnResult(sqlmock.NewResult(0, 0))
					expectUUIDProbe(mock, false)
					mock.ExpectCommit()
					report, err := ensurePGProcedureSpecCompatibility(context.Background(), w.db, "app", sample.product, spec)
					require.NoError(t, err)
					require.Equal(t, 1, report.Created)
					require.Equal(t, fallback, report.Items[0].DDL)
					require.NoError(t, mock.ExpectationsWereMet())
				})
			}
			for _, code := range []string{"42501", "42P01", "42703", "42883"} {
				t.Run(code, func(t *testing.T) {
					w, mock := repairPGMock(t)
					expectProcedureCreatedForInitialization(mock, "app", "postgres", &pq.Error{Code: pq.ErrorCode(code), Message: "creation rejected"}, spec)
					report, err := ensurePGProcedureSpecCompatibility(context.Background(), w.db, "app", "postgres", spec)
					require.Error(t, err)
					require.Equal(t, code, compatibilitySQLState(err))
					require.Equal(t, 1, report.Failed)
					require.NoError(t, mock.ExpectationsWereMet())
				})
			}
		})
	}
}

func TestProcedureWritableCTEUnsupportedDoesNotCreateOrCallProcedure(t *testing.T) {
	for _, spec := range compatibilityProcedureSpecs {
		t.Run(spec.name, func(t *testing.T) {
			w, mock := repairPGMock(t)
			expectProcedureMissingForInitialization(mock, "app", spec)
			expectProcedureWritableCTEForInitialization(mock, &pq.Error{Code: "42601", Message: "data-modifying CTE unsupported"})
			mock.ExpectRollback()
			report, err := ensurePGProcedureSpecCompatibility(context.Background(), w.db, "app", "postgres", spec)
			require.ErrorContains(t, err, "DELETE RETURNING")
			require.ErrorContains(t, err, "data-modifying CTE unsupported")
			require.Equal(t, 1, report.Failed)
			require.Empty(t, report.Items[0].DDL)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestProcedureMissingProkindFailsOnlyItsOwnItem(t *testing.T) {
	for _, spec := range compatibilityProcedureSpecs {
		t.Run(spec.name, func(t *testing.T) {
			w, mock := repairPGMock(t)
			expectUUIDHeader(mock, "app")
			mock.ExpectQuery(`SELECT EXISTS .*pg_attribute.*attname='prokind'`).
				WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
			mock.ExpectRollback()
			report, err := ensurePGProcedureSpecCompatibility(context.Background(), w.db, "app", "gaussdb", spec)
			require.ErrorContains(t, err, "prokind")
			require.Equal(t, 1, report.Total)
			require.Equal(t, 1, report.Failed)
			require.Empty(t, report.Items[0].DDL)
			require.Equal(t, "procedure", report.Items[0].Kind)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestProcedureFunctionCollisionIsPreserved(t *testing.T) {
	for _, spec := range compatibilityProcedureSpecs {
		t.Run(spec.name, func(t *testing.T) {
			w, mock := repairPGMock(t)
			expectProcedureHeaderForInitialization(mock, "app")
			mock.ExpectQuery(`SELECT p.oid,p.pronargdefaults,p.provariadic,COALESCE`).
				WithArgs("app", spec.name).WillReturnRows(procedureMetadataRows())
			expectUUIDSavepoint(mock)
			mock.ExpectQuery(regexp.QuoteMeta(`SELECT $1::regprocedure::oid`)).
				WithArgs(qualifiedFunction("app", spec.name) + "(character varying)").
				WillReturnRows(sqlmock.NewRows([]string{"oid"}).AddRow(123))
			expectUUIDProbe(mock, false)
			mock.ExpectQuery(`SELECT p.oid,p.pronargdefaults,p.provariadic,COALESCE.*p.prokind='p'`).
				WithArgs(int64(123), "app").WillReturnRows(procedureMetadataRows())
			mock.ExpectRollback()
			report, err := ensurePGProcedureSpecCompatibility(context.Background(), w.db, "app", "postgres", spec)
			require.ErrorContains(t, err, "已保留原定义")
			require.Equal(t, 1, report.Failed)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestProcedureRejectsOutInoutVariadicAndAmbiguousOverloads(t *testing.T) {
	for _, spec := range compatibilityProcedureSpecs {
		t.Run(spec.name, func(t *testing.T) {
			for _, sample := range []struct {
				name, modes string
				variadic    int
				conflict    bool
			}{
				{name: "out", modes: "i,o"},
				{name: "inout", modes: "b"},
				{name: "variadic", modes: "v", variadic: 1043},
				{name: "default_overload", conflict: true},
				{name: "string_literal_overload", conflict: true},
			} {
				t.Run(sample.name, func(t *testing.T) {
					w, mock := repairPGMock(t)
					expectProcedureHeaderForInitialization(mock, "app")
					mock.ExpectQuery(`SELECT p.oid,p.pronargdefaults,p.provariadic,COALESCE`).
						WithArgs("app", spec.name).WillReturnRows(procedureMetadataRows().AddRow(88, 0, sample.variadic, sample.modes))
					mock.ExpectQuery(`SELECT EXISTS .*p.proname=COALESCE.*pg_type.*typcategory='S'`).WithArgs("app", spec.name, int64(88)).
						WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(sample.conflict))
					mock.ExpectRollback()
					report, err := ensurePGProcedureSpecCompatibility(context.Background(), w.db, "app", "postgres", spec)
					require.ErrorContains(t, err, "已保留原定义")
					require.Equal(t, 1, report.Failed)
					require.NoError(t, mock.ExpectationsWereMet())
				})
			}
		})
	}
}

func TestProcedureReadsActualDefinitionShapesAndWarnsWithoutFabrication(t *testing.T) {
	for _, spec := range compatibilityProcedureSpecs {
		t.Run(spec.name, func(t *testing.T) {
			for _, sample := range []struct {
				name string
				rows *sqlmock.Rows
				err  error
				want string
			}{
				{"scalar", sqlmock.NewRows([]string{"pg_get_functiondef"}).AddRow("ORIGINAL SCALAR"), nil, "ORIGINAL SCALAR"},
				{"vendor_record", sqlmock.NewRows([]string{"name", "headerlines", "definition"}).AddRow("proc", 2, []byte("ORIGINAL VENDOR")), nil, "ORIGINAL VENDOR"},
				{"vendor_uppercase_field", sqlmock.NewRows([]string{"name", "DEFINITION"}).AddRow("proc", "ORIGINAL UPPER"), nil, "ORIGINAL UPPER"},
				{"unreadable", nil, errors.New("definition permission denied"), ""},
				{"no_definition_field", sqlmock.NewRows([]string{"name", "header"}).AddRow("proc", "unknown"), nil, ""},
				{"null_definition", sqlmock.NewRows([]string{"definition"}).AddRow(nil), nil, ""},
				{"empty_definition", sqlmock.NewRows([]string{"definition"}).AddRow(" \n"), nil, ""},
				{"multiple_rows", sqlmock.NewRows([]string{"definition"}).AddRow("first").AddRow("second"), nil, ""},
			} {
				t.Run(sample.name, func(t *testing.T) {
					w, mock := repairPGMock(t)
					expectProcedureHeaderForInitialization(mock, "app")
					mock.ExpectQuery(`SELECT p.oid,p.pronargdefaults,p.provariadic,COALESCE`).
						WithArgs("app", spec.name).WillReturnRows(procedureMetadataRows().AddRow(88, 1, 0, "i"))
					expectBuiltinNoConflictsForInitialization(mock, "app", spec.name, 88)
					expectUUIDSavepoint(mock)
					query := mock.ExpectQuery(regexp.QuoteMeta(`SELECT * FROM "pg_catalog".pg_get_functiondef($1::oid)`)).WithArgs(int64(88))
					if sample.err != nil {
						query.WillReturnError(sample.err)
					} else {
						query.WillReturnRows(sample.rows)
					}
					expectUUIDProbe(mock, sample.want == "")
					mock.ExpectRollback()
					report, err := ensurePGProcedureSpecCompatibility(context.Background(), w.db, "app", "postgres", spec)
					require.NoError(t, err)
					require.Equal(t, 1, report.Reused)
					require.Equal(t, sample.want, report.Items[0].DDL)
					if sample.want == "" {
						require.NotEmpty(t, report.Items[0].DefinitionWarning)
					} else {
						require.Empty(t, report.Items[0].DefinitionWarning)
					}
					require.NoError(t, mock.ExpectationsWereMet())
				})
			}
		})
	}
}

func TestProcedureConcurrentCreationRetriesItsOwnTransaction(t *testing.T) {
	for _, spec := range compatibilityProcedureSpecs {
		t.Run(spec.name, func(t *testing.T) {
			w, mock := repairPGMock(t)
			expectProcedureCreatedForInitialization(mock, "app", "postgres", &pq.Error{Code: "42723", Message: "procedure already exists"}, spec)
			expectProcedureReusedForInitialization(mock, "app", spec)
			report, err := ensurePGProcedureSpecCompatibility(context.Background(), w.db, "app", "postgres", spec)
			require.NoError(t, err)
			require.Equal(t, 1, report.Reused)
			require.Zero(t, report.Created)
			require.Equal(t, "EXISTING PROCEDURE", report.Items[0].DDL)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func expectProcedureBothHeadersRejected(m sqlmock.Sqlmock, terminalCode string, specs ...compatibilityProcedureSpec) {
	spec := compatibilityProcedureSpecs[0]
	if len(specs) > 0 {
		spec = specs[0]
	}
	expectProcedureMissingForInitialization(m, "app", spec)
	expectProcedureWritableCTEForInitialization(m, nil)
	expectUUIDSavepoint(m)
	m.ExpectExec(regexp.QuoteMeta(spec.ddl("app", "pg_catalog", false))).
		WillReturnError(&pq.Error{Code: "42601", Message: "preferred header rejected"})
	expectUUIDProbe(m, true)
	expectUUIDSavepoint(m)
	m.ExpectExec(regexp.QuoteMeta(spec.ddl("app", "pg_catalog", true))).
		WillReturnError(&pq.Error{Code: pq.ErrorCode(terminalCode), Message: "alternate header rejected"})
	expectUUIDProbe(m, true)
	m.ExpectRollback()
}

func TestProcedureHeaderFailuresRetainBothDiagnosticsAndTerminalClassification(t *testing.T) {
	for _, spec := range compatibilityProcedureSpecs {
		t.Run(spec.name, func(t *testing.T) {
			for _, code := range []string{"42601", "42501"} {
				t.Run(code, func(t *testing.T) {
					w, mock := repairPGMock(t)
					expectProcedureBothHeadersRejected(mock, code, spec)
					report, err := ensurePGProcedureSpecCompatibility(context.Background(), w.db, "app", "postgres", spec)
					require.ErrorContains(t, err, "preferred header rejected")
					require.ErrorContains(t, err, "alternate header rejected")
					require.Equal(t, code, compatibilitySQLState(err))
					require.Equal(t, 1, report.Failed)
					require.Contains(t, report.Items[0].Error, "preferred header rejected")
					require.NoError(t, mock.ExpectationsWereMet())
				})
			}
			t.Run("terminal_creation_race_retries", func(t *testing.T) {
				w, mock := repairPGMock(t)
				expectProcedureBothHeadersRejected(mock, "42723", spec)
				expectProcedureReusedForInitialization(mock, "app", spec)
				report, err := ensurePGProcedureSpecCompatibility(context.Background(), w.db, "app", "postgres", spec)
				require.NoError(t, err)
				require.Equal(t, 1, report.Reused)
				require.NoError(t, mock.ExpectationsWereMet())
			})
		})
	}
}

func TestProcedureCatalogHelpersUseResolvedCatalog(t *testing.T) {
	for _, spec := range compatibilityProcedureSpecs {
		t.Run(spec.name, func(t *testing.T) {
			w, mock := repairPGMock(t)
			const schema = `target "helpers"`
			mock.ExpectBegin()
			expectUUIDSavepoint(mock)
			mock.ExpectQuery(`SELECT EXISTS .*pg_catalog.pg_proc`).
				WillReturnError(&pq.Error{Code: "42P01", Message: "use vendor catalog"})
			expectUUIDProbe(mock, true)
			expectUUIDSavepoint(mock)
			mock.ExpectQuery(`SELECT EXISTS .*sys_catalog.pg_proc`).
				WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
			expectUUIDProbe(mock, false)
			mock.ExpectExec(regexp.QuoteMeta("SET LOCAL search_path TO " + source.QuotePGIdent(schema) + ", sys_catalog, public")).
				WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectQuery(`SELECT EXISTS .*sys_catalog.pg_attribute.*attname='prokind'`).
				WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
			// Both helper calls must bind to the discovered system catalog even with
			// the target schema first in search_path. No user-defined helper is called.
			mock.ExpectQuery(regexp.QuoteMeta(`SELECT p.oid,p.pronargdefaults,p.provariadic,COALESCE("sys_catalog".array_to_string`)).
				WithArgs(schema, spec.name).WillReturnRows(procedureMetadataRows().AddRow(88, 0, 0, "i"))
			expectBuiltinNoConflictsForInitialization(mock, schema, spec.name, 88)
			expectUUIDSavepoint(mock)
			mock.ExpectQuery(regexp.QuoteMeta(`SELECT * FROM "sys_catalog".pg_get_functiondef($1::oid)`)).
				WithArgs(int64(88)).WillReturnRows(sqlmock.NewRows([]string{"name", "definition"}).AddRow("proc", "ORIGINAL VENDOR PROCEDURE"))
			expectUUIDProbe(mock, false)
			mock.ExpectRollback()
			report, err := ensurePGProcedureSpecCompatibility(context.Background(), w.db, schema, "kingbase", spec)
			require.NoError(t, err)
			require.Equal(t, 1, report.Reused)
			require.Equal(t, "ORIGINAL VENDOR PROCEDURE", report.Items[0].DDL)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
