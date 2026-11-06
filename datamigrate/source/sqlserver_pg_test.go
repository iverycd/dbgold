package source

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestSQLServerPGViewsPreserveOriginalDefinitionsAndUnreadableObjects(t *testing.T) {
	for _, target := range []string{"postgres", "gaussdb", "highgo", "vastbase", "gbase", "seabox", "kingbase"} {
		t.Run(target, func(t *testing.T) {
			db, mock := partitionCatalogMock(t)
			raw := "CREATE VIEW [dbo].[MixedView] AS SELECT N'KeepMiXeD [dbo]. + ' AS [Label], 1000000000 + a.[ROW_ID] FROM [dbo].[MixedTable] a"
			mock.ExpectQuery(regexp.QuoteMeta(sqlServerPGViewsQuery)).WillReturnRows(sqlmock.NewRows([]string{"name", "definition"}).
				AddRow("MixedView", raw).AddRow("Encrypted", nil)).RowsWillBeClosed()
			views, err := (&SQLServerReader{db: db}).GetViewsForTarget(context.Background(), target)
			require.NoError(t, err)
			require.Equal(t, []ViewInfo{{ViewName: "MixedView", Definition: raw}, {ViewName: "Encrypted"}}, views)
		})
	}
}

func TestSQLServerNonPGViewsRetainLegacyReader(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	mock.ExpectQuery(`SELECT v.name,.*LOWER\(m.definition\)`).WillReturnRows(sqlmock.NewRows([]string{"name", "definition"}).AddRow("MixedView", "create or replace view v as select 'abc' as label"))
	views, err := (&SQLServerReader{db: db}).GetViewsForTarget(context.Background(), "mysql")
	require.NoError(t, err)
	require.Equal(t, "select 'abc' as label", views[0].Definition)
}

func TestSQLServerDamengOptionalViewReaderRemainsRawAndScoped(t *testing.T) {
	r, mock := dmSQLReader(t)
	raw := "CREATE VIEW [dbo].[MixedView] AS SELECT N'KeepMiXeD' AS [Label]"
	mock.ExpectQuery(`SELECT v.name,m.definition.*v.schema_id=SCHEMA_ID\('dbo'\)`).
		WillReturnRows(sqlmock.NewRows([]string{"name", "definition"}).AddRow("MixedView", raw).AddRow("Encrypted", nil))
	views, err := r.GetViewsForTarget(context.Background(), "dameng")
	require.NoError(t, err)
	require.Equal(t, []ViewInfo{{ViewName: "MixedView", Definition: raw}, {ViewName: "Encrypted"}}, views)
}

func TestSQLServerPGViewRelationColumnsResolveCatalogIdentityBeforeColumns(t *testing.T) {
	for _, schema := range []string{"", "Own]er'"} {
		t.Run(schema, func(t *testing.T) {
			db, mock := partitionCatalogMock(t)
			db.SetMaxOpenConns(1)
			query := sqlServerViewRelationQuery
			if schema != "" {
				query += ` AND s.name=@p2`
			}
			expectation := mock.ExpectQuery(regexp.QuoteMeta(query + ` ORDER BY s.name,o.name`))
			if schema == "" {
				expectation.WithArgs("portal_item")
			} else {
				expectation.WithArgs("portal_item", schema)
			}
			expectation.WillReturnRows(sqlmock.NewRows([]string{"schema", "name", "object_id"}).AddRow("dbo", "Portal_Item", 42)).RowsWillBeClosed()
			mock.ExpectQuery(regexp.QuoteMeta(sqlServerViewRelationColumnsQuery)).WithArgs(int64(42)).
				WillReturnRows(sqlmock.NewRows([]string{"name", "type", "length", "data_length", "precision", "scale", "nullable", "ordinal", "user_defined"}).
					AddRow("Disabled", "nvarchar", 50, 100, 0, 0, true, 1, false).
					AddRow("ROW_ID", "int", 4, 4, 10, 0, false, 2, false).
					AddRow("Body", "nvarchar", -1, -1, 0, 0, true, 3, false).
					AddRow("Custom", "MyType", 8, 8, 18, 2, true, 4, true)).RowsWillBeClosed()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			info, err := (&SQLServerReader{db: db}).GetViewRelationColumns(ctx, schema, "portal_item")
			require.NoError(t, err)
			require.Equal(t, "Portal_Item", info.TableName)
			require.Equal(t, []ColumnInfo{
				{Name: "Disabled", DataType: "nvarchar", Length: 50, DataLength: 100, IsNullable: true, Ordinal: 1},
				{Name: "ROW_ID", DataType: "int", Length: 4, DataLength: 4, Precision: 10, Ordinal: 2},
				{Name: "Body", DataType: "nvarchar", Length: -1, DataLength: -1, IsNullable: true, Ordinal: 3},
				{Name: "Custom", DataType: "MyType", Length: 8, DataLength: 8, Precision: 18, Scale: 2, DateTimePrecision: 2, IsNullable: true, Ordinal: 4, TypeKind: "user_defined"},
			}, info.Columns)
		})
	}
}

func TestSQLServerPGViewRelationColumnsRejectMissingOrAmbiguousIdentity(t *testing.T) {
	for _, scenario := range []string{"missing", "ambiguous", "columns missing"} {
		t.Run(scenario, func(t *testing.T) {
			db, mock := partitionCatalogMock(t)
			rows := sqlmock.NewRows([]string{"schema", "name", "object_id"})
			if scenario != "missing" {
				rows.AddRow("dbo", "Shared", 42)
			}
			if scenario == "ambiguous" {
				rows.AddRow("Archive", "Shared", 84)
			}
			mock.ExpectQuery(regexp.QuoteMeta(sqlServerViewRelationQuery + ` ORDER BY s.name,o.name`)).WithArgs("Shared").WillReturnRows(rows).RowsWillBeClosed()
			if scenario == "columns missing" {
				mock.ExpectQuery(regexp.QuoteMeta(sqlServerViewRelationColumnsQuery)).WithArgs(int64(42)).
					WillReturnRows(sqlmock.NewRows([]string{"name", "type", "length", "data_length", "precision", "scale", "nullable", "ordinal", "user_defined"})).RowsWillBeClosed()
			}
			info, err := (&SQLServerReader{db: db}).GetViewRelationColumns(context.Background(), "", "Shared")
			require.Error(t, err)
			require.Nil(t, info)
			if scenario == "columns missing" {
				require.ErrorContains(t, err, "列元数据不可用")
			} else {
				require.ErrorContains(t, err, "schema 身份不唯一或不存在")
			}
		})
	}
}

func TestSQLServerPGViewCatalogErrorsAreNotHidden(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	failure := errors.New("catalog access denied")
	mock.ExpectQuery(regexp.QuoteMeta(sqlServerPGViewsQuery)).WillReturnError(failure)
	_, err := (&SQLServerReader{db: db}).GetViewsForTarget(context.Background(), "kingbase")
	require.ErrorIs(t, err, failure)
	mock.ExpectQuery(regexp.QuoteMeta(sqlServerViewRelationQuery + ` ORDER BY s.name,o.name`)).WithArgs("T").
		WillReturnRows(sqlmock.NewRows([]string{"schema", "name", "object_id"}).AddRow("dbo", "T", 42).RowError(0, failure)).RowsWillBeClosed()
	_, err = (&SQLServerReader{db: db}).GetViewRelationColumns(context.Background(), "", "T")
	require.ErrorIs(t, err, failure)
}
