package source

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestMySQLPartitionChecksPreserveIdentityAndBlockUnreadableCatalog(t *testing.T) {
	for _, tc := range []struct {
		name  string
		count int64
		err   error
	}{{"none", 0, nil}, {"enforced or unenforced", 2, nil}, {"denied", 0, errors.New("catalog denied")}, {"invalid count", -1, nil}} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock := partitionCatalogMock(t)
			query := mock.ExpectQuery(regexp.QuoteMeta(`SELECT COUNT(*) FROM information_schema.TABLE_CONSTRAINTS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND CONSTRAINT_TYPE='CHECK'`)).WithArgs("MiXeD", "Order's")
			if tc.err != nil {
				query.WillReturnError(tc.err)
			} else {
				query.WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(tc.count))
			}
			has, err := (&MySQLReader{db: db, dbName: "MiXeD"}).PartitionHasChecks(context.Background(), "Order's")
			if tc.err != nil || tc.count < 0 {
				require.Error(t, err)
				require.False(t, has)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.count > 0, has)
			}
		})
	}
}

func TestMySQLKeyDeclarationsParseNativeExecutableComments(t *testing.T) {
	ddl := "CREATE TABLE `T` (`MiXeD` int, `Other` int, UNIQUE KEY `Z` (`Other`,`MiXeD`), UNIQUE KEY `A` (`MiXeD`,`Other`)) ENGINE=InnoDB /*!50100 PARTITION BY LINEAR KEY */ /*!50611 ALGORITHM = 1 */ /*!50100 (`Other`,`MiXeD`) PARTITIONS 3 */"
	d, indexes, err := mysqlPartitionKeyDeclarations(ddl, false)
	require.NoError(t, err)
	require.Equal(t, []string{"Z", "A"}, indexes)
	require.Equal(t, mysqlKeyDeclaration{method: "LINEAR KEY", algorithm: 1, columns: []string{"Other", "MiXeD"}}, d["PARTITION"])
	d, _, err = mysqlPartitionKeyDeclarations("CREATE TABLE `T` (`Id` int, `Key` int) /*!50100 PARTITION BY RANGE (`Id`) SUBPARTITION BY KEY */ /*!50611 ALGORITHM = 1 */ /*!50100 (`Key`) (PARTITION `P` VALUES LESS THAN MAXVALUE (SUBPARTITION `S`)) */", false)
	require.NoError(t, err)
	require.Equal(t, 1, d["SUBPARTITION"].algorithm)
	d, _, err = mysqlPartitionKeyDeclarations("CREATE TABLE `T` (`Id` int COMMENT '/*! KEY ALGORITHM=1 */') /*!50100 PARTITION BY KEY () PARTITIONS 2 */", false)
	require.NoError(t, err)
	require.Equal(t, 2, d["PARTITION"].algorithm)
	require.Empty(t, d["PARTITION"].columns)
	for _, raw := range []string{"KEY ALGORITHM=3 (Id)", "KEY (Id,)", "KEY (lower(Id))", "KEY (Id) PARTITION BY KEY (Id)"} {
		_, _, err = mysqlPartitionKeyDeclarations("CREATE TABLE t (Id int) PARTITION BY "+raw, false)
		require.Error(t, err, raw)
	}
}

func TestMySQLKeyFreezeUsesPhysicalColumnOrderAndExplicitAlgorithm(t *testing.T) {
	for _, scenario := range []string{"explicit", "primary", "first unique", "subpartition"} {
		t.Run(scenario, func(t *testing.T) {
			db, mock := partitionCatalogMock(t)
			r := &MySQLReader{db: db, dbName: "App"}
			columns := map[string]PartitionKey{"MiXeD": {Column: "MiXeD", DataType: "bigint unsigned"}, "Other": {Column: "Other", DataType: "varchar(20)", Collation: "utf8mb4_bin"}}
			root := mysqlCatalogSpec("LINEAR KEY", "`Other`,`MiXeD`", columns)
			ddl := "CREATE TABLE `T` (`MiXeD` bigint unsigned, `Other` varchar(20)) PARTITION BY LINEAR KEY ALGORITHM=1 (`Other`,`MiXeD`) PARTITIONS 3"
			rows := sqlmock.NewRows([]string{"index", "position", "column", "prefix", "nullable"})
			spec := root
			switch scenario {
			case "primary":
				root = mysqlCatalogSpec("LINEAR KEY", "", columns)
				spec = root
				ddl = "CREATE TABLE `T` (`MiXeD` bigint unsigned, `Other` varchar(20), PRIMARY KEY (`Other`,`MiXeD`)) PARTITION BY LINEAR KEY ALGORITHM=1 () PARTITIONS 3"
				rows.AddRow("PRIMARY", 1, "Other", nil, "").AddRow("PRIMARY", 2, "MiXeD", nil, "")
			case "first unique":
				root = mysqlCatalogSpec("LINEAR KEY", "", columns)
				spec = root
				ddl = "CREATE TABLE `T` (`MiXeD` bigint unsigned, `Other` varchar(20), UNIQUE KEY `Z` (`Other`,`MiXeD`), UNIQUE KEY `A` (`MiXeD`,`Other`)) PARTITION BY LINEAR KEY ALGORITHM=1 () PARTITIONS 3"
				rows.AddRow("A", 1, "MiXeD", nil, "").AddRow("A", 2, "Other", nil, "").AddRow("Z", 1, "Other", nil, "").AddRow("Z", 2, "MiXeD", nil, "")
			case "subpartition":
				root = &PartitionSpec{Method: "RANGE", Partitions: []Partition{{Subpartition: spec}}}
				ddl = "CREATE TABLE `T` (`MiXeD` bigint unsigned, `Other` varchar(20)) PARTITION BY RANGE (`MiXeD`) SUBPARTITION BY LINEAR KEY ALGORITHM=1 (`Other`,`MiXeD`) SUBPARTITIONS 3 (PARTITION P VALUES LESS THAN MAXVALUE)"
			}
			mock.ExpectQuery(`SELECT VERSION\(\),@@version_comment,@@SESSION.sql_mode`).WillReturnRows(sqlmock.NewRows([]string{"version", "comment", "mode"}).AddRow("8.0.36", "MySQL Community Server - GPL", ""))
			mock.ExpectQuery(`SELECT ENGINE FROM information_schema.TABLES`).WithArgs("App", "T").WillReturnRows(sqlmock.NewRows([]string{"engine"}).AddRow("InnoDB"))
			mock.ExpectQuery(`SELECT INDEX_NAME,SEQ_IN_INDEX,COLUMN_NAME,SUB_PART,NULLABLE`).WithArgs("App", "T").WillReturnRows(rows)
			require.NoError(t, r.freezeMySQLPartitionKeys(context.Background(), "T", ddl, columns, []string{"MiXeD", "Other"}, root))
			require.Equal(t, "mysql-key-innodb-1:8.0.36", spec.Algorithm)
			require.Equal(t, []PartitionKey{columns["MiXeD"], columns["Other"]}, spec.Keys)
		})
	}
}

func TestMySQLKeyFreezeRejectsUnprovenNativeIdentityAndPrefix(t *testing.T) {
	for _, scenario := range []string{"vendor", "engine", "prefix", "missing implicit", "nullable implicit"} {
		t.Run(scenario, func(t *testing.T) {
			db, mock := partitionCatalogMock(t)
			comment, engine := "MySQL Community Server - GPL", "InnoDB"
			if scenario == "vendor" {
				comment = "Percona Server (GPL)"
			}
			if scenario == "engine" {
				engine = "NDBCLUSTER"
			}
			mock.ExpectQuery(`SELECT VERSION\(\),@@version_comment,@@SESSION.sql_mode`).WillReturnRows(sqlmock.NewRows([]string{"version", "comment", "mode"}).AddRow("8.0.36", comment, ""))
			if scenario != "vendor" {
				mock.ExpectQuery(`SELECT ENGINE FROM information_schema.TABLES`).WillReturnRows(sqlmock.NewRows([]string{"engine"}).AddRow(engine))
				if scenario != "engine" {
					rows := sqlmock.NewRows([]string{"index", "position", "column", "prefix", "nullable"})
					if scenario == "prefix" {
						rows.AddRow("PRIMARY", 1, "Id", 2, "")
					}
					if scenario == "nullable implicit" {
						rows.AddRow("U", 1, "Id", nil, "YES")
					}
					mock.ExpectQuery(`SELECT INDEX_NAME,SEQ_IN_INDEX,COLUMN_NAME,SUB_PART,NULLABLE`).WillReturnRows(rows)
				}
			}
			columns := map[string]PartitionKey{"Id": {Column: "Id", DataType: "int"}}
			spec := mysqlCatalogSpec("KEY", "", columns)
			err := (&MySQLReader{db: db, dbName: "App"}).freezeMySQLPartitionKeys(context.Background(), "T", "CREATE TABLE T (Id int, UNIQUE KEY U (Id)) PARTITION BY KEY () PARTITIONS 2", columns, []string{"Id"}, spec)
			require.Error(t, err)
			require.Equal(t, "mysql-key-unresolved", spec.Algorithm)
		})
	}
}
