package validation

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func pgCatalogFields(omitted ...string) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{"relation", "attribute"})
	for _, pair := range [][2]string{{"pg_class", "oid"}, {"pg_class", "relname"}, {"pg_class", "relnamespace"}, {"pg_namespace", "oid"}, {"pg_namespace", "nspname"}, {"pg_constraint", "conrelid"}, {"pg_constraint", "contype"}, {"pg_constraint", "conkey"}, {"pg_attribute", "attrelid"}, {"pg_attribute", "attname"}, {"pg_attribute", "attnum"}, {"pg_attribute", "attisdropped"}, {"pg_attribute", "atttypid"}, {"pg_type", "oid"}, {"pg_type", "typname"}, {"pg_type", "typbasetype"}} {
		if slices.Contains(omitted, pair[0]+"."+pair[1]) {
			continue
		}
		rows.AddRow(pair[0], pair[1])
	}
	return rows
}
func pgCatalogProbe(catalog string) string {
	return regexp.QuoteMeta(`SELECT c.relname,a.attname FROM "` + catalog + `".pg_attribute`)
}
func expectPGValidationDescribe(mock sqlmock.Sqlmock, catalog, vector string) {
	mock.ExpectQuery("SELECT c.column_name, CASE").WithArgs("CaseSchema", "Rows").WillReturnRows(sqlmock.NewRows([]string{"name", "type", "precision", "scale", "fsp", "length", "ordinal"}).
		AddRow("First", "bigint", 64, 0, 0, 0, 1).
		AddRow("Included", "text", 0, 0, 0, 0, 3).
		AddRow("Last", "bigint", 64, 0, 0, 0, 7))
	pk := regexp.QuoteMeta(`SELECT c.conkey::text FROM "` + catalog + `".pg_constraint`)
	mock.ExpectQuery(pk).WithArgs("CaseSchema", "Rows").WillReturnRows(sqlmock.NewRows([]string{"conkey"}).AddRow(vector))
}

func TestPGValidationReusesDetectedCatalogAndPreservesKeyOrder(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	expectPGValidationDescribe(mock, "sys_catalog", "[0:1]={7,1}")
	meta, err := NewSQLScanner(db, "postgres", "CaseSchema").WithCatalog("sys_catalog").DescribeValidationTable(context.Background(), "Rows")
	require.NoError(t, err)
	require.Equal(t, []string{"Last", "First"}, meta.PrimaryKey)
	require.Equal(t, "Included", meta.Columns[1].Name)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGValidationTargetCatalogProbeFallsBack(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery(pgCatalogProbe("pg_catalog")).WithArgs("pg_catalog").WillReturnError(fmt.Errorf("catalog unavailable"))
	mock.ExpectQuery(pgCatalogProbe("sys_catalog")).WithArgs("sys_catalog").WillReturnRows(pgCatalogFields())
	expectPGValidationDescribe(mock, "sys_catalog", "7 1")
	meta, err := NewSQLScanner(db, "postgres", "CaseSchema").DescribeValidationTable(context.Background(), "Rows")
	require.NoError(t, err)
	require.Equal(t, []string{"Last", "First"}, meta.PrimaryKey)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGValidationWithoutUDTNameCanScan(t *testing.T) {
	for _, catalog := range []string{"pg_catalog", "sys_catalog"} {
		t.Run(catalog, func(t *testing.T) {
			matcher := sqlmock.QueryMatcherFunc(func(expected, actual string) error {
				if strings.Contains(actual, "information_schema.columns") {
					// Model Kingbase's MySQL-compatible view: even an unused CASE
					// branch referencing this absent column must fail to prepare.
					if strings.Contains(actual, "udt_name") {
						return fmt.Errorf(`pq: column "udt_name" does not exist`)
					}
					for _, part := range []string{
						`JOIN "` + catalog + `".pg_type t ON t.oid=a.atttypid`,
						`LEFT JOIN "` + catalog + `".pg_type bt ON bt.oid=t.typbasetype`,
						`COALESCE(bt.typname,t.typname) ELSE c.data_type END`,
					} {
						if !strings.Contains(actual, part) {
							return fmt.Errorf("missing native type resolution: %s", part)
						}
					}
				}
				return sqlmock.QueryMatcherRegexp.Match(expected, actual)
			})
			db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(matcher))
			require.NoError(t, err)
			defer db.Close()
			scanner := NewSQLScanner(db, "postgres", "CaseSchema").WithCatalog(catalog)
			expectPGValidationDescribe(mock, catalog, "{7,1}")
			meta, err := scanner.DescribeValidationTable(context.Background(), "Rows")
			require.NoError(t, err)
			require.Equal(t, []string{"Last", "First"}, meta.PrimaryKey)
			expectPGValidationDescribe(mock, catalog, "{7,1}")
			mock.ExpectQuery(regexp.QuoteMeta(`SELECT current_setting('TimeZone'), current_setting('DateStyle')`)).
				WillReturnRows(sqlmock.NewRows([]string{"zone", "date"}).AddRow("Asia/Shanghai", "ISO, DMY"))
			mock.ExpectExec(regexp.QuoteMeta(`SELECT set_config('TimeZone','UTC',false),set_config('DateStyle','ISO, YMD',false)`)).
				WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectQuery(regexp.QuoteMeta(`SELECT "First"::text,"Included","Last"::text FROM "CaseSchema"."Rows"`)).
				WillReturnRows(sqlmock.NewRows([]string{"First", "Included", "Last"}).AddRow("42", "original\x00尾空格  ", "84")).RowsWillBeClosed()
			mock.ExpectExec(regexp.QuoteMeta(`SELECT set_config('TimeZone',$1,false),set_config('DateStyle',$2,false)`)).
				WithArgs("Asia/Shanghai", "ISO, DMY").WillReturnResult(sqlmock.NewResult(0, 1))
			stream, err := scanner.OpenValidationScan(context.Background(), "Rows", meta.Columns)
			require.NoError(t, err)
			defer stream.Close()
			row, err := stream.Next(context.Background())
			require.NoError(t, err)
			require.Equal(t, []any{"42", "original\x00尾空格  ", "84"}, row)
			_, err = stream.Next(context.Background())
			require.ErrorIs(t, err, io.EOF)
			require.NoError(t, stream.Close())
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestPGValidationColumnMetadataPreservesTypesAndPrecision(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery("SELECT c.column_name, CASE").WithArgs("CaseSchema", "Rows").
		WillReturnRows(sqlmock.NewRows([]string{"name", "type", "precision", "scale", "fsp", "length", "ordinal"}).
			AddRow("Amount", "numeric", 38, 10, 0, 0, 1).
			AddRow("Stamp", "timestamp without time zone", 0, 0, 3, 0, 3).
			AddRow("Label", "character varying", 0, 0, 0, 27, 4).
			AddRow("Array", "_int4", 0, 0, 0, 0, 5).
			AddRow("Custom", "CustomEnum", 0, 0, 0, 0, 6))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT c.conkey::text FROM "pg_catalog".pg_constraint`)).
		WithArgs("CaseSchema", "Rows").WillReturnRows(sqlmock.NewRows([]string{"conkey"}))
	meta, err := NewSQLScanner(db, "postgres", "CaseSchema").WithCatalog("pg_catalog").DescribeValidationTable(context.Background(), "Rows")
	require.NoError(t, err)
	require.Equal(t, []Column{
		{Name: "Amount", DatabaseType: "numeric", Kind: Number, Precision: 38, Scale: 10},
		{Name: "Stamp", DatabaseType: "timestamp without time zone", Kind: Timestamp, TimePrecision: 3},
		{Name: "Label", DatabaseType: "character varying", Kind: Text, Length: 27},
		{Name: "Array", DatabaseType: "_int4", Kind: Unsupported},
		{Name: "Custom", DatabaseType: "CustomEnum", Kind: Unsupported},
	}, meta.Columns)
	require.Empty(t, meta.PrimaryKey)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGValidationCatalogProbeChecksTypeCapabilities(t *testing.T) {
	for _, field := range []string{"pg_attribute.atttypid", "pg_type.oid", "pg_type.typname", "pg_type.typbasetype"} {
		t.Run(field, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			for _, catalog := range []string{"pg_catalog", "sys_catalog"} {
				mock.ExpectQuery(pgCatalogProbe(catalog)).WithArgs(catalog).WillReturnRows(pgCatalogFields(field))
			}
			_, err = NewSQLScanner(db, "postgres", "CaseSchema").DescribeValidationTable(context.Background(), "Rows")
			require.ErrorContains(t, err, field)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestPGValidationCatalogProbeFallsBackWithoutTypeCapabilities(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery(pgCatalogProbe("pg_catalog")).WithArgs("pg_catalog").WillReturnRows(pgCatalogFields("pg_type.typbasetype"))
	mock.ExpectQuery(pgCatalogProbe("sys_catalog")).WithArgs("sys_catalog").WillReturnRows(pgCatalogFields())
	expectPGValidationDescribe(mock, "sys_catalog", "{7,1}")
	meta, err := NewSQLScanner(db, "postgres", "CaseSchema").DescribeValidationTable(context.Background(), "Rows")
	require.NoError(t, err)
	require.Equal(t, []string{"Last", "First"}, meta.PrimaryKey)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGValidationCatalogProbeRejectsMissingCapabilities(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	for _, catalog := range []string{"pg_catalog", "sys_catalog"} {
		mock.ExpectQuery(pgCatalogProbe(catalog)).WithArgs(catalog).WillReturnRows(sqlmock.NewRows([]string{"relation", "attribute"}).AddRow("pg_class", "relname"))
	}
	_, err = NewSQLScanner(db, "postgres", "CaseSchema").DescribeValidationTable(context.Background(), "Rows")
	require.ErrorContains(t, err, "无法识别")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGValidationRejectsUntrustedCatalogIdentifier(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	_, err = NewSQLScanner(db, "postgres", "CaseSchema").WithCatalog(`pg_catalog"; SELECT 1`).DescribeValidationTable(context.Background(), "Rows")
	require.ErrorContains(t, err, "不支持")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGValidationPrimaryVectorDecoding(t *testing.T) {
	names := map[int]string{1: "MiXeD", 3: "Other", 7: "Last"}
	for _, vector := range []string{"7 1", "{7,1}", "[0:1]={7,1}"} {
		key, err := decodePGPrimaryKey(vector, names)
		require.NoError(t, err)
		require.Equal(t, []string{"Last", "MiXeD"}, key)
	}
	for _, vector := range []string{"{}", "{0}", "{-1}", "{7,7}", "{2}", "{x}"} {
		_, err := decodePGPrimaryKey(vector, names)
		require.Error(t, err, vector)
	}
}

func TestPGValidationCompositePrimaryIncludeIntegration(t *testing.T) {
	dsn := os.Getenv("VALIDATION_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("VALIDATION_POSTGRES_DSN is not set")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	name := fmt.Sprintf("validation_pk_%d", time.Now().UnixNano())
	quoted := QuoteValidationIdent("postgres", "public") + "." + QuoteValidationIdent("postgres", name)
	_, err = db.ExecContext(ctx, `CREATE TABLE `+quoted+` ("First" bigint, "Dropped" text, "Last" bigint, "Included" text, PRIMARY KEY ("Last","First") INCLUDE ("Included"))`)
	require.NoError(t, err)
	defer db.ExecContext(context.Background(), "DROP TABLE "+quoted)
	_, err = db.ExecContext(ctx, `ALTER TABLE `+quoted+` DROP COLUMN "Dropped"`)
	require.NoError(t, err)
	scanner := NewSQLScanner(db, "postgres", "public")
	meta, err := scanner.DescribeValidationTable(ctx, name)
	require.NoError(t, err)
	require.Equal(t, []string{"Last", "First"}, meta.PrimaryKey)
	require.Len(t, meta.Columns, 3)
	require.Equal(t, "Included", meta.Columns[2].Name)
}
