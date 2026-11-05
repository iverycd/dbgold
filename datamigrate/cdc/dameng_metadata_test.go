package cdc

import (
	"context"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestDamengTargetMetadataKeepsCatalogNamesAndValidOrderedUniqueKeys(t *testing.T) {
	db, mock := newTargetMetadataMock(t)
	mock.ExpectQuery(loadDamengTargetTablesSQL).WithArgs("APP").WillReturnRows(sqlmock.NewRows([]string{"TABLE_NAME"}).AddRow("T").AddRow("Mixed"))
	mock.ExpectQuery(loadDamengTargetColumnsSQL).WithArgs("APP").WillReturnRows(sqlmock.NewRows([]string{"T", "C", "N", "D", "TYPE", "I"}).
		AddRow("T", "ID", "N", nil, "BIGINT", "YES").AddRow("T", "EMAIL", "N", nil, "VARCHAR", "NO").AddRow("T", "BODY", "Y", nil, "CLOB", "NO").AddRow("Mixed", "Case", "Y", nil, "INT", "NO"))
	mock.ExpectQuery(loadDamengTargetIndexesSQL).WithArgs("APP").WillReturnRows(sqlmock.NewRows([]string{"O", "I", "TO", "T", "U", "S", "K"}).
		AddRow("APP", "PK", "APP", "T", "UNIQUE", "VALID", "CLUSTER").
		AddRow("APP", "UQ", "APP", "T", "UNIQUE", "VALID", "NORMAL").
		AddRow("APP", "FUNC", "APP", "T", "UNIQUE", "VALID", "FUNCTION-BASED NORMAL").
		AddRow("APP", "BAD", "APP", "T", "UNIQUE", "INVALID", "NORMAL").
		AddRow("APP", "GAP", "APP", "T", "UNIQUE", "VALID", "NORMAL"))
	mock.ExpectQuery(loadDamengTargetIndexColumnsSQL).WithArgs("APP").WillReturnRows(sqlmock.NewRows([]string{"O", "I", "TO", "T", "C", "P"}).
		AddRow("APP", "PK", "APP", "T", "ID", 1).
		AddRow("APP", "UQ", "APP", "T", "ID", 2).AddRow("APP", "UQ", "APP", "T", "EMAIL", 1).
		AddRow("APP", "FUNC", "APP", "T", "EMAIL", 1).AddRow("APP", "BAD", "APP", "T", "EMAIL", 1).AddRow("APP", "GAP", "APP", "T", "EMAIL", 2))
	metadata, err := loadConfiguredTargetSchemaMetadata(context.Background(), db, Config{TargetDBType: "dameng", TargetSchema: "app"})
	require.NoError(t, err)
	require.Equal(t, [][]string{{"ID"}, {"EMAIL", "ID"}}, metadata.Tables["T"].UniqueSets)
	require.Equal(t, "YES", metadata.Tables["T"].Columns[0].Identity)
	require.Equal(t, "NO", metadata.Tables["T"].Columns[0].Nullable)
	require.Equal(t, "CLOB", metadata.Tables["T"].Columns[2].DataType)
	require.Contains(t, metadata.Tables["Mixed"].ColumnNames, "Case")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDamengNamesAndUniqueLocatorUseFullMigrationUppercase(t *testing.T) {
	cfg := Config{TargetDBType: "dameng", TargetSchema: "App", LowerCaseNames: true}
	require.Equal(t, "APP", targetSchemaName(cfg))
	require.Equal(t, "MIXED", targetObjectName(cfg, "Mixed"))
	require.Equal(t, "App", targetSchemaName(Config{TargetSchema: "App", LowerCaseNames: true}))
	tables := []TableInfo{{Name: "Orders", Columns: []string{"Id", "Email"}, UniqueIndexes: []UniqueIndexInfo{{Name: "uq", Columns: []string{"Email"}}}}}
	resolved := resolveLocatorStrategiesFromMetadata(cfg, tables, targetSchemaMetadata{Tables: map[string]*targetTableMetadata{"ORDERS": {UniqueSets: [][]string{{"EMAIL"}}}}})
	require.Equal(t, LocatorUniqueKey, resolved[0].LocatorStrategy)
	require.Equal(t, []string{"Email"}, resolved[0].LocatorColumns)
	require.Empty(t, validateDamengNames(cfg, tables))
	issues := validateDamengNames(cfg, []TableInfo{{Name: "Same", Columns: []string{"x", "X", "rowid"}}, {Name: "same"}, {Name: CheckpointTableName}, {Name: strings.Repeat("a", 129)}})
	require.Len(t, issues, 5)
	require.Len(t, validateDamengNameCollisions(cfg, []TableInfo{{Name: "Same", Columns: []string{"x", "X", "rowid"}}, {Name: "same"}, {Name: CheckpointTableName}, {Name: strings.Repeat("a", 129)}}), 3)
}

func TestDamengMissingTargetExclusionsLeaveOnlyEffectiveNameChecks(t *testing.T) {
	cfg := Config{TargetDBType: "dameng", TargetSchema: "APP"}
	tables := []TableInfo{{Name: "Gone"}, {Name: "gone"}, {Name: "Orders", Columns: []string{"Id"}}}
	missing := map[string]TargetTableIssue{
		"Gone": {SourceTable: "Gone", TargetSchema: "APP", TargetTable: "GONE"},
		"gone": {SourceTable: "gone", TargetSchema: "APP", TargetTable: "GONE"},
	}
	effective, unresolved, accepted, issues := applyMissingTargetExclusions(tables, missing, []string{"Gone", "gone"})
	require.Empty(t, unresolved)
	require.Empty(t, issues)
	require.Len(t, accepted, 2)
	require.Empty(t, validateDamengNames(cfg, effective))
	require.Len(t, validateDamengNameCollisions(cfg, tables), 1)
}

func TestDamengLegacyLowerCaseMatchesFullMigrationUnicode(t *testing.T) {
	cfg := Config{TargetDBType: "dameng", TargetSchema: "APP", LowerCaseNames: true}
	require.Equal(t, "K", targetObjectName(cfg, "K"))
	require.NotEmpty(t, validateDamengNameCollisions(cfg, []TableInfo{{Name: "K"}, {Name: "K"}}))
	cfg.LowerCaseNames = false
	require.Equal(t, "K", targetObjectName(cfg, "K"))
	require.Empty(t, validateDamengNameCollisions(cfg, []TableInfo{{Name: "K"}, {Name: "K"}}))
}
