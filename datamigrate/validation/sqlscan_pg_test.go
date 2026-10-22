package validation

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func pgCatalogFields() *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{"relation", "attribute"})
	for _, pair := range [][2]string{{"pg_class", "oid"}, {"pg_class", "relname"}, {"pg_class", "relnamespace"}, {"pg_namespace", "oid"}, {"pg_namespace", "nspname"}, {"pg_constraint", "conrelid"}, {"pg_constraint", "contype"}, {"pg_constraint", "conkey"}} {
		rows.AddRow(pair[0], pair[1])
	}
	return rows
}
func pgCatalogProbe(catalog string) string {
	return regexp.QuoteMeta(`SELECT c.relname,a.attname FROM "` + catalog + `".pg_attribute`)
}
func expectPGValidationDescribe(mock sqlmock.Sqlmock, catalog, vector string) {
	mock.ExpectQuery("SELECT column_name, CASE").WithArgs("CaseSchema", "Rows").WillReturnRows(sqlmock.NewRows([]string{"name", "type", "precision", "scale", "fsp", "length", "ordinal"}).
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
