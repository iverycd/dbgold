package cdc

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func newTargetMetadataMock(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db, mock
}

func TestLoadTargetSchemaMetadataBuildsCaseSensitiveCompositeIndex(t *testing.T) {
	db, mock := newTargetMetadataMock(t)
	mock.ExpectQuery(loadTargetTablesSQL).WithArgs("TargetSchema").WillReturnRows(
		sqlmock.NewRows([]string{"table_name"}).AddRow("MixedCase"),
	)
	mock.ExpectQuery(loadTargetColumnsSQL).WithArgs("TargetSchema").WillReturnRows(
		sqlmock.NewRows([]string{"table_name", "column_name", "is_nullable", "column_default", "is_identity"}).
			AddRow("MixedCase", "part_a", "NO", nil, "NO").
			AddRow("MixedCase", "part_b", "NO", nil, "NO"),
	)
	// Catalog rows are deliberately returned in attnum order. indkey carries
	// the actual index-key order, which must remain part_b, part_a.
	mock.ExpectQuery(loadTargetUniqueIndexesSQL).WithArgs("TargetSchema").WillReturnRows(
		sqlmock.NewRows([]string{"relname", "indexrelid", "indkey", "attnum", "attname"}).
			AddRow("MixedCase", int64(42), "2 1", 1, "part_a").
			AddRow("MixedCase", int64(42), "2 1", 2, "part_b"),
	)

	metadata, err := loadTargetSchemaMetadata(context.Background(), db, "TargetSchema")
	require.NoError(t, err)
	require.Contains(t, metadata.Tables, "MixedCase")
	require.NotContains(t, metadata.Tables, "mixedcase")
	require.Equal(t, [][]string{{"part_b", "part_a"}}, metadata.Tables["MixedCase"].UniqueSets)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestLoadTargetSchemaMetadataQueryCountIsConstantFor3000Tables(t *testing.T) {
	for _, tableCount := range []int{1, 3000} {
		t.Run(fmt.Sprintf("tables_%d", tableCount), func(t *testing.T) {
			db, mock := newTargetMetadataMock(t)
			tableRows := sqlmock.NewRows([]string{"table_name"})
			columnRows := sqlmock.NewRows([]string{"table_name", "column_name", "is_nullable", "column_default", "is_identity"})
			for i := 0; i < tableCount; i++ {
				name := fmt.Sprintf("table_%04d", i)
				tableRows.AddRow(name)
				columnRows.AddRow(name, "id", "NO", nil, "NO")
			}
			mock.ExpectQuery(loadTargetTablesSQL).WithArgs("public").WillReturnRows(tableRows)
			mock.ExpectQuery(loadTargetColumnsSQL).WithArgs("public").WillReturnRows(columnRows)
			mock.ExpectQuery(loadTargetUniqueIndexesSQL).WithArgs("public").WillReturnRows(
				sqlmock.NewRows([]string{"relname", "indexrelid", "indkey", "attnum", "attname"}),
			)

			metadata, err := loadTargetSchemaMetadata(context.Background(), db, "public")
			require.NoError(t, err)
			require.Len(t, metadata.Tables, tableCount)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestResolveLocatorStrategiesFromMetadata(t *testing.T) {
	cfg := Config{LowerCaseNames: true}
	metadata := targetSchemaMetadata{Tables: map[string]*targetTableMetadata{
		"mixedcase": {UniqueSets: [][]string{{"email"}}},
		"fallback":  {},
	}}
	tables := []TableInfo{
		{Name: "MixedCase", Columns: []string{"ID", "Email"}, UniqueIndexes: []UniqueIndexInfo{{Name: "uq_email", Columns: []string{"Email"}}}},
		{Name: "Fallback", Columns: []string{"payload"}},
		{Name: "CompositePK", Columns: []string{"tenant", "id"}, PrimaryKey: []int{0, 1}},
	}

	resolved := resolveLocatorStrategiesFromMetadata(cfg, tables, metadata)
	require.Equal(t, LocatorUniqueKey, resolved[0].LocatorStrategy)
	require.Equal(t, []string{"Email"}, resolved[0].LocatorColumns)
	require.Equal(t, LocatorFullRow, resolved[1].LocatorStrategy)
	require.Equal(t, []string{"payload"}, resolved[1].LocatorColumns)
	require.Equal(t, LocatorPrimaryKey, resolved[2].LocatorStrategy)
	require.Equal(t, []string{"tenant", "id"}, resolved[2].LocatorColumns)
}

func TestValidateTargetTableCompatibilityFromMetadata(t *testing.T) {
	metadata := targetSchemaMetadata{Tables: map[string]*targetTableMetadata{
		"composite": {
			Columns: []targetColumnMetadata{
				{Name: "tenant", Nullable: "NO"},
				{Name: "id", Nullable: "NO"},
			},
			ColumnNames: map[string]bool{"tenant": true, "id": true},
			UniqueSets:  [][]string{{"tenant", "id"}},
		},
		"extra": {
			Columns: []targetColumnMetadata{
				{Name: "id", Nullable: "NO"},
				{Name: "required", Nullable: "NO"},
			},
			ColumnNames: map[string]bool{"id": true, "required": true},
		},
		"missing_column": {
			Columns:     []targetColumnMetadata{{Name: "id", Nullable: "NO"}},
			ColumnNames: map[string]bool{"id": true},
		},
	}}
	tables := []TableInfo{
		{Name: "Composite", Columns: []string{"Tenant", "ID"}, PrimaryKey: []int{0, 1}},
		{Name: "Extra", Columns: []string{"ID"}},
		{Name: "Missing_Column", Columns: []string{"ID", "Payload"}},
		{Name: "Missing_Table", Columns: []string{"ID"}},
	}

	failures := validateTargetTableCompatibilityFromMetadata(Config{LowerCaseNames: true}, tables, metadata)
	require.NotContains(t, failures, "Composite")
	require.Equal(t, "目标表存在无默认值的额外必填列: required", failures["Extra"])
	require.Equal(t, "目标表缺少列: payload", failures["Missing_Column"])
	require.Equal(t, "目标表不存在", failures["Missing_Table"])
}

func TestParseIndexKeyOrderRejectsInvalidCatalogValue(t *testing.T) {
	_, err := parseIndexKeyOrder("1 invalid")
	require.ErrorContains(t, err, "无效的 pg_index.indkey")
	_, err = parseIndexKeyOrder("0")
	require.ErrorContains(t, err, "为空")
}
