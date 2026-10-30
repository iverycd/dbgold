package validation

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func partitionValidationDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		require.NoError(t, mock.ExpectationsWereMet())
		db.Close()
	})
	return db, mock
}

func partitionValidationColumns() []Column {
	return []Column{{Name: "Id", DatabaseType: "bigint", Kind: Number, Precision: 64}}
}

func expectPartitionValidationDescribe(mock sqlmock.Sqlmock, family, schema, root string) {
	columns := sqlmock.NewRows([]string{"name", "type", "precision", "scale", "fsp", "length"}).AddRow("Id", "bigint", 64, 0, 0, 0)
	primary := sqlmock.NewRows([]string{"column"}).AddRow("Id")
	switch family {
	case "mysql":
		mock.ExpectQuery(`SELECT COLUMN_NAME, DATA_TYPE`).WithArgs(schema, root).WillReturnRows(columns).RowsWillBeClosed()
		mock.ExpectQuery(`SELECT COLUMN_NAME FROM information_schema.KEY_COLUMN_USAGE`).WithArgs(schema, root).WillReturnRows(primary).RowsWillBeClosed()
	case "oracle", "dameng":
		mock.ExpectQuery(`SELECT COLUMN_NAME, DATA_TYPE`).WithArgs(schema, root).WillReturnRows(columns).RowsWillBeClosed()
		mock.ExpectQuery(`SELECT c.COLUMN_NAME FROM ALL_CONS_COLUMNS`).WithArgs(schema, root).WillReturnRows(primary).RowsWillBeClosed()
	case "postgres":
		mock.ExpectQuery(`SELECT c.column_name, CASE`).WithArgs(schema, root).WillReturnRows(sqlmock.NewRows([]string{"name", "type", "precision", "scale", "fsp", "length", "ordinal"}).AddRow("Id", "bigint", 64, 0, 0, 0, 1)).RowsWillBeClosed()
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT c.conkey::text FROM "pg_catalog".pg_constraint`)).WithArgs(schema, root).WillReturnRows(sqlmock.NewRows([]string{"key"}).AddRow("{1}")).RowsWillBeClosed()
	case "sqlserver":
		mock.ExpectQuery(`SELECT c.name, t.name`).WithArgs(schema, root).WillReturnRows(columns).RowsWillBeClosed()
		mock.ExpectQuery(`SELECT c.name FROM sys.indexes`).WithArgs(schema, root).WillReturnRows(primary).RowsWillBeClosed()
	}
}

func expectPartitionValidationPGSettings(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT current_setting('TimeZone'), current_setting('DateStyle')`)).WillReturnRows(sqlmock.NewRows([]string{"timezone", "datestyle"}).AddRow("Asia/Shanghai", "ISO, DMY"))
	mock.ExpectExec(regexp.QuoteMeta(`SELECT set_config('TimeZone','UTC',false),set_config('DateStyle','ISO, YMD',false)`)).WillReturnResult(sqlmock.NewResult(0, 1))
}

func expectPartitionValidationPGRestore(mock sqlmock.Sqlmock) {
	mock.ExpectExec(regexp.QuoteMeta(`SELECT set_config('TimeZone',$1,false),set_config('DateStyle',$2,false)`)).WithArgs("Asia/Shanghai", "ISO, DMY").WillReturnResult(sqlmock.NewResult(0, 1))
}

func TestPartitionValidationUsesQuotedNativeSelectors(t *testing.T) {
	for _, tc := range []struct {
		family, schema, root, name, from string
		sub                              bool
	}{
		{"mysql", "Ow`ner", "Ro`ot", "P`One", "`Ow``ner`.`Ro``ot` PARTITION (`P``One`)", false},
		{"mysql", "Ow`ner", "Ro`ot", "Sub`One", "`Ow``ner`.`Ro``ot` PARTITION (`Sub``One`)", true},
		{"oracle", `Ow"ner`, `Ro"ot`, `P"One`, `"Ow""ner"."Ro""ot" PARTITION ("P""One")`, false},
		{"oracle", `Ow"ner`, `Ro"ot`, `Sub"One`, `"Ow""ner"."Ro""ot" SUBPARTITION ("Sub""One")`, true},
		{"dameng", `Ow"ner`, `Ro"ot`, `Sub"One`, `"Ow""ner"."Ro""ot" SUBPARTITION ("Sub""One")`, true},
	} {
		t.Run(tc.family+tc.name, func(t *testing.T) {
			db, mock := partitionValidationDB(t)
			expectPartitionValidationDescribe(mock, tc.family, tc.schema, tc.root)
			projection, err := ValidationProjection(tc.family, partitionValidationColumns()[0])
			require.NoError(t, err)
			mock.ExpectQuery(regexp.QuoteMeta("SELECT " + projection + " FROM " + tc.from)).WillReturnRows(sqlmock.NewRows([]string{"Id"}).AddRow("9223372036854775807")).RowsWillBeClosed()
			base := NewSQLScanner(db, tc.family, tc.schema)
			scanner := base.WithPartition(PartitionLocation{Name: tc.name, Subpartition: tc.sub})
			require.Nil(t, base.partition, "leaf scans must not mutate the shared root scanner")
			stream, err := scanner.OpenValidationScan(context.Background(), tc.root, partitionValidationColumns())
			require.NoError(t, err)
			row, err := stream.Next(context.Background())
			require.NoError(t, err)
			require.Equal(t, []any{"9223372036854775807"}, row)
			_, err = stream.Next(context.Background())
			require.ErrorIs(t, err, io.EOF)
			require.NoError(t, stream.Close())
			require.Zero(t, db.Stats().InUse)
		})
	}
}

func TestPartitionValidationPostgresDeclarativeLeafUsesOnlyAndExactSchema(t *testing.T) {
	db, mock := partitionValidationDB(t)
	expectPartitionValidationDescribe(mock, "postgres", "RootOwner", "Root")
	expectPartitionValidationPGSettings(mock)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT "Id"::text FROM ONLY "Leaf""Owner"."Leaf""One"`)).WillReturnRows(sqlmock.NewRows([]string{"Id"}).AddRow("1")).RowsWillBeClosed()
	expectPartitionValidationPGRestore(mock)
	scanner := NewSQLScanner(db, "postgres", "RootOwner").WithCatalog("pg_catalog").WithPartition(PartitionLocation{Schema: `Leaf"Owner`, Name: `Leaf"One`, Subpartition: true})
	stream, err := scanner.OpenValidationScan(context.Background(), "Root", partitionValidationColumns())
	require.NoError(t, err)
	row, err := stream.Next(context.Background())
	require.NoError(t, err)
	require.Equal(t, []any{"1"}, row)
	require.NoError(t, stream.Close())
	require.Zero(t, db.Stats().InUse)
}

func TestPartitionValidationNativePGSelectorDoesNotTreatLabelAsRelation(t *testing.T) {
	db, mock := partitionValidationDB(t)
	expectPartitionValidationDescribe(mock, "postgres", "RootOwner", "Root")
	expectPartitionValidationPGSettings(mock)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT "Id"::text FROM "RootOwner"."Root" SUBPARTITION ("Leaf""Label")`)).WillReturnRows(sqlmock.NewRows([]string{"Id"}).AddRow("1")).RowsWillBeClosed()
	expectPartitionValidationPGRestore(mock)
	scanner := NewSQLScanner(db, "postgres", "RootOwner").WithCatalog("pg_catalog").WithPartition(PartitionLocation{Name: `Leaf"Label`, Subpartition: true, Native: true})
	stream, err := scanner.OpenValidationScan(context.Background(), "Root", partitionValidationColumns())
	require.NoError(t, err)
	require.NoError(t, stream.Close())
	require.Zero(t, db.Stats().InUse)
}

func TestPartitionValidationSQLServerUsesFrozenOrdinalAndQuotedNativeFunction(t *testing.T) {
	db, mock := partitionValidationDB(t)
	expectPartitionValidationDescribe(mock, "sqlserver", "Ow]ner", "Ro]ot")
	mock.ExpectQuery(`SELECT pf.name,c.name FROM sys.tables`).WithArgs("Ow]ner", "Ro]ot").WillReturnRows(sqlmock.NewRows([]string{"function", "column"}).AddRow("Func]Name", "Id"))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT CONVERT(varchar(100),[Id],2) FROM [Ow]]ner].[Ro]]ot] WHERE $PARTITION.[Func]]Name]([Id])=3`)).WillReturnRows(sqlmock.NewRows([]string{"Id"}).AddRow("7")).RowsWillBeClosed()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	scanner := NewSQLScanner(db, "sqlserver", "Ow]ner").WithPartition(PartitionLocation{Name: "3); DROP TABLE anything --", Ordinal: 3})
	stream, err := scanner.OpenValidationScan(ctx, "Ro]ot", partitionValidationColumns())
	require.NoError(t, err, "native metadata lookup must share the acquired connection, including a one-connection pool")
	row, err := stream.Next(ctx)
	require.NoError(t, err)
	require.Equal(t, []any{"7"}, row)
	require.NoError(t, stream.Close())
	require.Zero(t, db.Stats().InUse)
}

func TestPartitionValidationSQLServerErrorsReleaseDedicatedConnection(t *testing.T) {
	for _, failure := range []string{"metadata", "ordinal", "scan"} {
		t.Run(failure, func(t *testing.T) {
			db, mock := partitionValidationDB(t)
			expectPartitionValidationDescribe(mock, "sqlserver", "Owner", "Root")
			expect := mock.ExpectQuery(`SELECT pf.name,c.name FROM sys.tables`).WithArgs("Owner", "Root")
			ordinal := 1
			if failure == "metadata" {
				expect.WillReturnError(errors.New("partition catalog denied"))
			} else {
				expect.WillReturnRows(sqlmock.NewRows([]string{"function", "column"}).AddRow("F", "Id"))
				if failure == "ordinal" {
					ordinal = 0
				} else {
					mock.ExpectQuery(regexp.QuoteMeta(`SELECT CONVERT(varchar(100),[Id],2) FROM [Owner].[Root] WHERE $PARTITION.[F]([Id])=1`)).WillReturnError(errors.New("scan denied"))
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			scanner := NewSQLScanner(db, "sqlserver", "Owner").WithPartition(PartitionLocation{Ordinal: ordinal})
			_, err := scanner.OpenValidationScan(ctx, "Root", partitionValidationColumns())
			require.Error(t, err)
			require.Zero(t, db.Stats().InUse, "early failures must release the dedicated connection")
		})
	}
}

func TestPartitionValidationLocationsDoNotAliasRootOrSiblingScanner(t *testing.T) {
	base := NewSQLScanner(nil, "mysql", "App")
	location := PartitionLocation{Name: "First"}
	first := base.WithPartition(location)
	location.Name = "ChangedByCaller"
	second := base.WithPartition(PartitionLocation{Name: "Second"})
	require.Nil(t, base.partition)
	require.Equal(t, "First", first.partition.Name)
	require.Equal(t, "Second", second.partition.Name)
}
