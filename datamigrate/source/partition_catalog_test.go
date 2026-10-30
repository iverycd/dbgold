package source

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func partitionCatalogMock(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, mock.ExpectationsWereMet())
		db.Close()
	})
	return db, mock
}

func mysqlPartitionCatalogRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"PARTITION_NAME", "PARTITION_METHOD", "PARTITION_EXPRESSION", "PARTITION_DESCRIPTION", "PARTITION_ORDINAL_POSITION", "SUBPARTITION_NAME", "SUBPARTITION_METHOD", "SUBPARTITION_EXPRESSION", "SUBPARTITION_ORDINAL_POSITION"})
}

func TestMySQLPartitionCatalogPreservesNamesAndBounds(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	mock.ExpectQuery(`SELECT PARTITION_NAME,PARTITION_METHOD`).WithArgs("MiXeD", "Events").WillReturnRows(mysqlPartitionCatalogRows().
		AddRow("Before", "RANGE COLUMNS", "`Key`", "0", 1, nil, nil, nil, nil).
		AddRow("After", "RANGE COLUMNS", "`Key`", "MAXVALUE", 2, nil, nil, nil, nil)).RowsWillBeClosed()
	mock.ExpectQuery(`SELECT COLUMN_NAME,COLUMN_TYPE,COLLATION_NAME`).WithArgs("MiXeD", "Events").WillReturnRows(sqlmock.NewRows([]string{"name", "type", "collation"}).AddRow("Key", "bigint", nil)).RowsWillBeClosed()
	mock.ExpectQuery("SHOW CREATE TABLE `MiXeD`.`Events`").WillReturnRows(sqlmock.NewRows([]string{"Table", "Create Table"}).AddRow("Events", "CREATE TABLE `Events` (`Key` BIGINT) PARTITION BY RANGE COLUMNS (`Key`) (...)"))
	spec, err := (&MySQLReader{db: db, dbName: "MiXeD"}).PartitionMetadata(context.Background(), "Events")
	require.NoError(t, err)
	require.Empty(t, spec.Unsupported)
	require.Equal(t, "RANGE COLUMNS", spec.Method)
	require.Equal(t, PartitionKey{Column: "Key", DataType: "bigint"}, spec.Keys[0])
	require.Equal(t, "MiXeD", spec.Partitions[0].Schema)
	require.Equal(t, "Before", spec.Partitions[0].Name)
	require.Equal(t, []PartitionValue{{Kind: "minvalue"}}, spec.Partitions[0].Lower)
	require.Equal(t, []PartitionValue{{Kind: "number", Text: "0"}}, spec.Partitions[1].Lower)
	require.Equal(t, []PartitionValue{{Kind: "maxvalue"}}, spec.Partitions[1].Upper)
}

func TestMySQLPartitionCatalogHashSubpartitionsAndExpressions(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	mock.ExpectQuery(`SELECT PARTITION_NAME,PARTITION_METHOD`).WithArgs("App", "T").WillReturnRows(mysqlPartitionCatalogRows().
		AddRow("P1", "RANGE", "YEAR(`OccurredAt`)", "2020", 1, "H1", "HASH", "`Id`", 1).
		AddRow("P1", "RANGE", "YEAR(`OccurredAt`)", "2020", 1, "H2", "HASH", "`Id`", 2))
	mock.ExpectQuery(`SELECT COLUMN_NAME,COLUMN_TYPE,COLLATION_NAME`).WithArgs("App", "T").WillReturnRows(sqlmock.NewRows([]string{"name", "type", "collation"}).AddRow("OccurredAt", "date", nil).AddRow("Id", "int", nil))
	mock.ExpectQuery("SHOW CREATE TABLE `App`.`T`").WillReturnRows(sqlmock.NewRows([]string{"Table", "Create Table"}).AddRow("T", "CREATE TABLE `T` (...)"))
	spec, err := (&MySQLReader{db: db, dbName: "App"}).PartitionMetadata(context.Background(), "T")
	require.NoError(t, err)
	require.Equal(t, "YEAR(`OccurredAt`)", spec.Keys[0].Expression)
	require.Empty(t, spec.Keys[0].Column)
	require.Len(t, spec.Partitions, 1)
	sub := spec.Partitions[0].Subpartition
	require.Equal(t, "mysql-mod", sub.Algorithm)
	require.Equal(t, 2, sub.Partitions[0].HashModulus)
	require.Equal(t, 1, sub.Partitions[1].HashRemainder)
	require.Equal(t, "H2", sub.Partitions[1].Name)
}

func TestMySQLPartitionCatalogNonpartitionedAndErrors(t *testing.T) {
	t.Run("ordinary", func(t *testing.T) {
		db, mock := partitionCatalogMock(t)
		mock.ExpectQuery(`SELECT PARTITION_NAME,PARTITION_METHOD`).WithArgs("App", "T").WillReturnRows(mysqlPartitionCatalogRows().AddRow(nil, nil, nil, nil, nil, nil, nil, nil, nil))
		spec, err := (&MySQLReader{db: db, dbName: "App"}).PartitionMetadata(context.Background(), "T")
		require.NoError(t, err)
		require.Nil(t, spec)
	})
	t.Run("missing catalog", func(t *testing.T) {
		db, mock := partitionCatalogMock(t)
		mock.ExpectQuery(`SELECT PARTITION_NAME,PARTITION_METHOD`).WithArgs("App", "T").WillReturnError(errors.New("catalog denied"))
		spec, err := (&MySQLReader{db: db, dbName: "App"}).PartitionMetadata(context.Background(), "T")
		require.ErrorContains(t, err, "catalog denied")
		require.Nil(t, spec)
	})
	t.Run("key requires native algorithm evidence", func(t *testing.T) {
		spec := mysqlCatalogSpec("LINEAR KEY", "`Id`", map[string]PartitionKey{"Id": {Column: "Id", DataType: "int"}})
		require.Empty(t, spec.Unsupported)
		require.Equal(t, "mysql-key-unresolved", spec.Algorithm)
		require.Equal(t, "LINEAR KEY", spec.Method)
	})
}

func TestPartitionCatalogConstantListsAreExactAndNeverExecuteExpressions(t *testing.T) {
	part := Partition{}
	require.NoError(t, catalogPartitionBoundary(&part, "LIST COLUMNS", "(1,'a,b'),(2,'O''Brien')", 2))
	require.Equal(t, [][]PartitionValue{{{Kind: "number", Text: "1"}, {Kind: "string", Text: "a,b"}}, {{Kind: "number", Text: "2"}, {Kind: "string", Text: "O'Brien"}}}, part.Values)
	for _, raw := range []string{"danger(1)", "'x' -- ignored", "'a\\b'", "1; DROP TABLE t", "(1"} {
		p := Partition{}
		require.Error(t, catalogPartitionBoundary(&p, "RANGE", raw, 1), raw)
	}
}

func TestOraclePartitionCatalogDateIntervalAndRawOwner(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	mock.ExpectQuery(`SELECT PARTITIONING_TYPE,SUBPARTITIONING_TYPE,INTERVAL`).WithArgs("MiXeD", "Events").WillReturnRows(sqlmock.NewRows([]string{"method", "submethod", "interval"}).AddRow("RANGE", "NONE", "NUMTOYMINTERVAL(1,'MONTH')"))
	mock.ExpectQuery(`SELECT ROW_MOVEMENT FROM ALL_TABLES WHERE OWNER=:1 AND TABLE_NAME=:2`).WithArgs("MiXeD", "Events").WillReturnRows(sqlmock.NewRows([]string{"row_movement"}).AddRow("DISABLED"))
	mock.ExpectQuery(`SELECT k.COLUMN_NAME,c.DATA_TYPE FROM ALL_PART_KEY_COLUMNS`).WithArgs("MiXeD", "Events").WillReturnRows(sqlmock.NewRows([]string{"name", "type"}).AddRow("At", "DATE"))
	mock.ExpectQuery(`SELECT PARTITION_NAME,PARTITION_POSITION,HIGH_VALUE,SUBPARTITION_COUNT`).WithArgs("MiXeD", "Events").WillReturnRows(sqlmock.NewRows([]string{"name", "position", "bound", "subs"}).
		AddRow("Before", 1, "TO_DATE(' 2024-01-01 00:00:00', 'SYYYY-MM-DD HH24:MI:SS', 'NLS_CALENDAR=GREGORIAN')", 0).
		AddRow("Future", 2, "TO_DATE(' 2024-02-01 00:00:00', 'SYYYY-MM-DD HH24:MI:SS', 'NLS_CALENDAR=GREGORIAN')", 0))
	mock.ExpectQuery(`SELECT PARTITION_NAME,INTERVAL FROM ALL_TAB_PARTITIONS`).WithArgs("MiXeD", "Events").WillReturnRows(sqlmock.NewRows([]string{"name", "interval"}).AddRow("Before", "NO").AddRow("Future", "YES"))
	spec, err := (&OracleReader{db: db, owner: "MiXeD"}).PartitionMetadata(context.Background(), "Events")
	require.NoError(t, err)
	require.Empty(t, spec.Unsupported)
	require.Equal(t, "NUMTOYMINTERVAL(1,'MONTH')", spec.Automatic)
	require.Equal(t, "disabled", spec.RowMovement)
	require.Equal(t, []PartitionValue{{Kind: "timestamp", Text: "2024-01-01 00:00:00"}}, spec.Partitions[0].Upper)
	require.Equal(t, "MiXeD", spec.Partitions[1].Schema)
}

func TestOraclePartitionCatalogUnsupportedBoundaryIsVisible(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	mock.ExpectQuery(`SELECT PARTITIONING_TYPE,SUBPARTITIONING_TYPE,INTERVAL`).WithArgs("APP", "T").WillReturnRows(sqlmock.NewRows([]string{"method", "submethod", "interval"}).AddRow("RANGE", "NONE", nil))
	mock.ExpectQuery(`SELECT ROW_MOVEMENT FROM ALL_TABLES WHERE OWNER=:1 AND TABLE_NAME=:2`).WithArgs("APP", "T").WillReturnRows(sqlmock.NewRows([]string{"row_movement"}).AddRow("ENABLED"))
	mock.ExpectQuery(`SELECT k.COLUMN_NAME,c.DATA_TYPE FROM ALL_PART_KEY_COLUMNS`).WithArgs("APP", "T").WillReturnRows(sqlmock.NewRows([]string{"name", "type"}).AddRow("Id", "NUMBER"))
	mock.ExpectQuery(`SELECT PARTITION_NAME,PARTITION_POSITION,HIGH_VALUE,SUBPARTITION_COUNT`).WithArgs("APP", "T").WillReturnRows(sqlmock.NewRows([]string{"name", "position", "bound", "subs"}).AddRow("P", 1, "user_function(1)", 0))
	spec, err := (&OracleReader{db: db, owner: "APP"}).PartitionMetadata(context.Background(), "T")
	require.NoError(t, err)
	require.NotEmpty(t, spec.Unsupported)
}

func TestOracleTemporalPartitionConstantsRejectCalendarAndFunctions(t *testing.T) {
	for _, raw := range []string{
		"TO_DATE('2024-01-01','YYYY-MM-DD','NLS_CALENDAR=THAI BUDDHA')",
		"TO_DATE(user_value(),'YYYY-MM-DD')",
		"TO_DATE('2024-99-01','YYYY-MM-DD')",
		"TO_DATE('2024','YYYY')",
	} {
		_, handled, err := catalogOracleTemporalConstant(raw)
		require.True(t, handled)
		require.Error(t, err)
	}
}

func TestPartitionCatalogRowMovementReadsExactOwnerAndKnownNativeState(t *testing.T) {
	for _, dm := range []bool{false, true} {
		for _, state := range []string{"ENABLED", "DISABLED"} {
			t.Run(fmt.Sprintf("dm=%t/%s", dm, state), func(t *testing.T) {
				db, mock := partitionCatalogMock(t)
				query := `SELECT ROW_MOVEMENT FROM ALL_TABLES WHERE OWNER=:1 AND TABLE_NAME=:2`
				if dm {
					query = `SELECT ROW_MOVEMENT FROM ALL_TABLES WHERE OWNER=? AND TABLE_NAME=?`
				}
				mock.ExpectQuery(regexp.QuoteMeta(query)).WithArgs("MiXeD", "TaBle").WillReturnRows(sqlmock.NewRows([]string{"row_movement"}).AddRow(state))
				movement, err := catalogOracleStyleRowMovement(context.Background(), db, "MiXeD", "TaBle", dm)
				require.NoError(t, err)
				require.Equal(t, strings.ToLower(state), movement)
			})
		}
	}
}

func TestPartitionCatalogUnknownRowMovementNeverAdoptsNativeDefault(t *testing.T) {
	for _, state := range []any{nil, "", "UNKNOWN", "NO", "DISABLE"} {
		t.Run(fmt.Sprint(state), func(t *testing.T) {
			db, mock := partitionCatalogMock(t)
			mock.ExpectQuery(`SELECT ROW_MOVEMENT FROM ALL_TABLES`).WithArgs("App", "T").WillReturnRows(sqlmock.NewRows([]string{"row_movement"}).AddRow(state))
			movement, err := catalogOracleStyleRowMovement(context.Background(), db, "App", "T", true)
			require.ErrorContains(t, err, "不能证明未来跨分区 UPDATE 等价")
			require.Empty(t, movement)
		})
	}
	t.Run("catalog unavailable", func(t *testing.T) {
		db, mock := partitionCatalogMock(t)
		mock.ExpectQuery(`SELECT PARTITIONING_TYPE,SUBPARTITIONING_TYPE`).WithArgs("MiXeD", "T").WillReturnRows(sqlmock.NewRows([]string{"method", "submethod"}).AddRow("RANGE", "NONE"))
		mock.ExpectQuery(`SELECT ROW_MOVEMENT FROM ALL_TABLES`).WithArgs("MiXeD", "T").WillReturnError(errors.New("invalid column ROW_MOVEMENT"))
		spec, err := (&DaMengReader{db: db, schema: "MiXeD"}).PartitionMetadata(context.Background(), "T")
		require.ErrorContains(t, err, "invalid column ROW_MOVEMENT")
		require.Nil(t, spec)
	})
}

func TestDamengPartitionCatalogPreservesEnabledRowMovement(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	ddl := `CREATE TABLE "MiXeD"."Events"("Id" INT) PARTITION BY RANGE("Id")(PARTITION "P" VALUES LESS THAN(MAXVALUE)) ENABLE ROW MOVEMENT`
	mock.ExpectQuery(`SELECT PARTITIONING_TYPE,SUBPARTITIONING_TYPE`).WithArgs("MiXeD", "Events").WillReturnRows(sqlmock.NewRows([]string{"method", "submethod"}).AddRow("RANGE", "NONE"))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ROW_MOVEMENT FROM ALL_TABLES WHERE OWNER=? AND TABLE_NAME=?`)).WithArgs("MiXeD", "Events").WillReturnRows(sqlmock.NewRows([]string{"movement"}).AddRow("ENABLED"))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT DBMS_METADATA.GET_DDL('TABLE',?,?)`)).WithArgs("Events", "MiXeD").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(ddl))
	mock.ExpectQuery(`SELECT k.COLUMN_NAME,c.DATA_TYPE FROM ALL_PART_KEY_COLUMNS`).WithArgs("MiXeD", "Events").WillReturnRows(sqlmock.NewRows([]string{"name", "type"}).AddRow("Id", "INT"))
	mock.ExpectQuery(`SELECT PARTITION_NAME,PARTITION_POSITION,HIGH_VALUE,SUBPARTITION_COUNT`).WithArgs("MiXeD", "Events").WillReturnRows(sqlmock.NewRows([]string{"name", "position", "bound", "subs"}).AddRow("P", 1, "MAXVALUE", 0))
	spec, err := (&DaMengReader{db: db, schema: "MiXeD"}).PartitionMetadata(context.Background(), "Events")
	require.NoError(t, err)
	require.Equal(t, "enabled", spec.RowMovement)
	require.Equal(t, ddl, spec.SourceDDL)
	require.Empty(t, spec.Unsupported)
}

func TestPartitionCatalogRowMovementIsInheritedByCompleteSubpartitionTree(t *testing.T) {
	spec := &PartitionSpec{Partitions: []Partition{{Name: "P", Subpartition: &PartitionSpec{Partitions: []Partition{{Name: "S", Subpartition: &PartitionSpec{}}}}}}}
	catalogSetPartitionRowMovement(spec, "disabled")
	require.Equal(t, "disabled", spec.RowMovement)
	require.Equal(t, "disabled", spec.Partitions[0].Subpartition.RowMovement)
	require.Equal(t, "disabled", spec.Partitions[0].Subpartition.Partitions[0].Subpartition.RowMovement)
}

func TestSQLServerPartitionCatalogRangeRightAndNumberedIdentity(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	mock.ExpectQuery(`SELECT s.name,t.object_id.*s.name=@p2`).WithArgs("T", "MiXeD").WillReturnRows(sqlmock.NewRows([]string{"schema", "id"}).AddRow("MiXeD", 42))
	mock.ExpectQuery(`SELECT pf.name,pf.boundary_value_on_right`).WithArgs(int64(42)).WillReturnRows(sqlmock.NewRows([]string{"function", "right", "fanout", "column", "type", "collation"}).AddRow("NativeRange", true, 3, "Key", "bigint", nil))
	mock.ExpectQuery(`SELECT rv.boundary_id,CONVERT`).WithArgs("NativeRange").WillReturnRows(sqlmock.NewRows([]string{"ordinal", "kind", "bound"}).AddRow(1, "bigint", "-10").AddRow(2, "bigint", "9223372036854775807"))
	spec, err := sqlServerPartitionMetadata(context.Background(), db, "MiXeD", "T")
	require.NoError(t, err)
	require.Empty(t, spec.Unsupported)
	require.Equal(t, "sqlserver-range:NativeRange", spec.Algorithm)
	require.Equal(t, "1", spec.Partitions[0].Name)
	require.Equal(t, "MiXeD", spec.Partitions[0].Schema)
	require.True(t, spec.Partitions[1].LowerInclusive)
	require.False(t, spec.Partitions[1].UpperInclusive)
	require.Equal(t, "9223372036854775807", spec.Partitions[1].Upper[0].Text)
	require.Equal(t, "maxvalue", spec.Partitions[2].Upper[0].Kind)
}

func TestSQLServerPartitionCatalogRejectsAmbiguousSourceTable(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	mock.ExpectQuery(`SELECT s.name,t.object_id`).WithArgs("T").WillReturnRows(sqlmock.NewRows([]string{"schema", "id"}).AddRow("dbo", 1).AddRow("Other", 2))
	spec, err := (&SQLServerReader{db: db, dbName: "Database"}).PartitionMetadata(context.Background(), "T")
	require.ErrorContains(t, err, "不唯一")
	require.Nil(t, spec)
}

func TestSQLServerPartitionBoundaryReadsTypedNativeValues(t *testing.T) {
	value, err := catalogSQLServerBoundary("date", sql.NullString{Valid: true, String: "2024-02-29"})
	require.NoError(t, err)
	require.Equal(t, PartitionValue{Kind: "date", Text: "2024-02-29"}, value)
	_, err = catalogSQLServerBoundary("uniqueidentifier", sql.NullString{Valid: true, String: "11111111-1111-1111-1111-111111111111"})
	require.Error(t, err)
	value, err = catalogSQLServerBoundary("", sql.NullString{})
	require.NoError(t, err)
	require.Equal(t, "null", value.Kind)
}

func TestDamengPartitionRootsFollowNativeParentsWithOriginalCase(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	mock.ExpectQuery(`SELECT physical.NAME,root.NAME FROM SYS.SYSHPARTTABLEINFO`).WithArgs("MiXeD").WillReturnRows(sqlmock.NewRows([]string{"child", "parent"}).AddRow("PhysicalLeaf", "PhysicalBranch").AddRow("PhysicalBranch", "Root"))
	roots, err := (&DaMengReader{db: db, schema: "MiXeD"}).PartitionRoots(context.Background())
	require.NoError(t, err)
	require.Equal(t, "Root", roots["PhysicalLeaf"])
	require.Equal(t, "Root", roots["MiXeD.PhysicalLeaf"])
}

func TestDamengPartitionRootsRejectCycles(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	mock.ExpectQuery(`SELECT physical.NAME,root.NAME FROM SYS.SYSHPARTTABLEINFO`).WithArgs("App").WillReturnRows(sqlmock.NewRows([]string{"child", "parent"}).AddRow("A", "B").AddRow("B", "A"))
	_, err := (&DaMengReader{db: db, schema: "App"}).PartitionRoots(context.Background())
	require.ErrorContains(t, err, "循环")
}

func TestDamengPartitionNativeOptionsKeepAutomaticAndRejectLossyShape(t *testing.T) {
	spec := &PartitionSpec{}
	require.NoError(t, catalogDamengPartitionOptions(spec, `CREATE TABLE "App"."T" ("Id" INT, "Text" VARCHAR(40) DEFAULT 'INTERVAL(danger())') PARTITION BY RANGE("Id") INTERVAL(10)(PARTITION "P" VALUES LESS THAN(0))`))
	require.Equal(t, "10", spec.Automatic)
	require.Error(t, catalogDamengPartitionOptions(&PartitionSpec{}, `CREATE TABLE "App"."T"("Id" INT) PARTITION BY RANGE("Id")(PARTITION "P" VALUES EQU OR LESS THAN(0))`))
	require.Error(t, catalogDamengPartitionOptions(&PartitionSpec{}, `CREATE TABLE "App"."T"("Id" INT) PARTITION BY RANGE("Id") SUBPARTITION BY LIST("Id") SUBPARTITION BY LIST("Id") (...)`))
}

func TestPartitionIndexesPreserveNativeUniquenessLocality(t *testing.T) {
	t.Run("mysql", func(t *testing.T) {
		db, mock := partitionCatalogMock(t)
		mock.ExpectQuery(`SELECT INDEX_NAME,NON_UNIQUE,SEQ_IN_INDEX`).WithArgs("App", "T").WillReturnRows(sqlmock.NewRows([]string{"name", "nonunique", "position", "column", "prefix", "method", "direction"}).AddRow("PRIMARY", 0, 1, "PartitionKey", nil, "BTREE", "A").AddRow("PRIMARY", 0, 2, "Id", nil, "BTREE", "A"))
		indexes, err := (&MySQLReader{db: db, dbName: "App"}).PartitionIndexes(context.Background(), "T")
		require.NoError(t, err)
		require.Equal(t, "local", indexes[0].Locality)
		require.True(t, indexes[0].IsUnique)
		require.True(t, indexes[0].IsPrimary)
		require.Equal(t, []string{"PartitionKey", "Id"}, indexes[0].Columns)
	})
	t.Run("oracle global", func(t *testing.T) {
		db, mock := partitionCatalogMock(t)
		mock.ExpectQuery(`SELECT i.INDEX_NAME,i.UNIQUENESS`).WithArgs("APP", "T").WillReturnRows(sqlmock.NewRows([]string{"name", "unique", "method", "locality", "column", "position", "direction", "primary"}).AddRow("GlobalUnique", "UNIQUE", "NORMAL", nil, "Id", 1, "ASC", 0))
		indexes, err := (&OracleReader{db: db, owner: "APP"}).PartitionIndexes(context.Background(), "T")
		require.NoError(t, err)
		require.Equal(t, "global", indexes[0].Locality)
		require.True(t, indexes[0].IsUnique)
	})
}
