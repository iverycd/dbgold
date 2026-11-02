package datamigrate

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func TestMySQLDamengFulltextIndexOnlyReportAndManifest(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "execution failure"
		}
		t.Run(name, func(t *testing.T) {
			r := newMySQLDMIndexReader()
			r.ddl["Orders"].Columns = []source.ColumnInfo{{Name: "Body", DataType: "text"}, {Name: "Title", DataType: "varchar", Length: 100}}
			r.sourceDDL = "CREATE TABLE `Orders` (`Body` text, `Title` varchar(100), FULLTEXT KEY `ByBody` (`Body`), FULLTEXT KEY `Combined` (`Body`, `Title`))"
			r.indexes = []source.IndexInfo{
				{TableName: "Orders", IndexName: "ByBody", Columns: []string{"Body"}, Method: "FULLTEXT"},
				{TableName: "Orders", IndexName: "Combined", Method: "FULLTEXT", Unsupported: "达梦全文索引仅支持单列，多列全文索引请手工适配"},
				{TableName: "Excluded", IndexName: "ByBody", Columns: []string{"Body"}, Method: "FULLTEXT"},
			}
			w := &mysqlDMIndexWriter{targetType: "dameng", failures: map[string]error{}}
			if fail {
				w.failures["bybody"] = errors.New("CREATE CONTEXT INDEX permission denied")
			}
			m, _ := newTestMigrator(r, w)
			m.cfg.Mode, m.cfg.Filter = "include", "Orders"
			m.cfg.Content, m.cfg.Objects = "schema_only", []string{"indexes"}
			m.cfg.TargetDBType, m.cfg.TargetSchema = "dameng", "APP"
			m.cfg.CaptureRepair, m.cfg.LowerCaseNames = true, true
			report := m.Run(context.Background())

			const ddl = `CREATE CONTEXT INDEX "ORDERS_BYBODY" ON "APP"."ORDERS" ("BODY") SYNC TRANSACTION`
			require.Equal(t, 2, report.Indexes.Total)
			wantFailed := 1
			if fail {
				wantFailed++
			}
			require.Equal(t, wantFailed, report.Indexes.Failed)
			require.Equal(t, 2-wantFailed, report.Indexes.Success)
			require.Empty(t, w.created, "index-only migration must preserve the existing table")
			require.Len(t, w.indexes, 1)
			require.Equal(t, source.IndexInfo{TableName: "orders", IndexName: "bybody", Columns: []string{"BODY"}, Method: "FULLTEXT"}, w.indexes[0])
			require.Equal(t, []string{"Body"}, r.indexes[0].Columns, "target column planning must not mutate source keys")
			require.Equal(t, "Orders", r.indexes[0].TableName)
			require.Equal(t, []string{"Orders"}, r.definitionReads)
			for _, item := range report.Indexes.Items {
				if item.SourceName == "Combined" {
					require.Equal(t, "unsupported", item.Stage)
					require.Empty(t, item.DDL)
					require.Equal(t, r.sourceDDL, item.SourceDDL)
					continue
				}
				require.Equal(t, "ByBody", item.SourceName)
				require.Equal(t, "ORDERS_BYBODY", item.TargetName)
				require.Equal(t, "ORDERS", item.TargetTable)
				require.Equal(t, "APP", item.TargetSchema)
				require.Equal(t, "execute", item.Stage)
				require.Equal(t, ddl, item.DDL)
				require.Empty(t, item.SourceDDL)
			}

			data, err := json.Marshal(report.RepairManifest)
			require.NoError(t, err)
			var manifest RepairManifest
			require.NoError(t, json.Unmarshal(data, &manifest))
			indexCount := 0
			for _, obj := range manifest.Objects {
				if obj.Kind != "index" {
					continue
				}
				indexCount++
				require.Equal(t, "Orders", obj.SourceTable)
				require.Equal(t, "FULLTEXT", obj.Index.Method)
				if obj.SourceName == "Combined" {
					require.Equal(t, "failed", obj.Status)
					require.Empty(t, obj.DDL)
					require.Empty(t, obj.Units)
					continue
				}
				require.Equal(t, "ByBody", obj.SourceName)
				require.Equal(t, "ORDERS_BYBODY", obj.TargetName)
				require.Equal(t, ddl, obj.DDL)
				require.Len(t, obj.Units, 1)
				require.Equal(t, ddl, obj.Units[0].SQL)
				require.Equal(t, ddl, obj.Units[0].OriginalSQL)
				status := "succeeded"
				if fail {
					status = "failed"
				}
				require.Equal(t, status, obj.Status)
				require.Equal(t, status, obj.Units[0].Status)
			}
			require.Equal(t, 2, indexCount)
		})
	}
}

func TestMySQLDamengFulltextRecoveryColumnMapping(t *testing.T) {
	w := &mysqlDMIndexWriter{targetType: "dameng"}
	table := repair.Object{Kind: "table", Table: "ORDERS", ColumnMapping: map[string]string{"Body": "BODY"}}
	const originalDDL = `CREATE CONTEXT INDEX "ORDERS_BYBODY" ON "APP"."ORDERS" ("BODY") SYNC TRANSACTION`
	obj := repair.Object{
		Kind: "index", Schema: "APP", Table: "ORDERS", TargetName: "ORDERS_BYBODY", DDL: originalDDL,
		Index: &source.IndexInfo{TableName: "ORDERS", IndexName: "ByBody", Columns: []string{"BODY"}, Method: "FULLTEXT"},
		Units: []repair.Unit{{SQL: originalDDL, OriginalSQL: originalDDL}},
	}
	got := regenerateRepairCandidate(w, Config{}, table, obj, map[string]string{"Body": "RENAMED_BODY"})
	require.True(t, got.RequiresReview)
	require.Equal(t, "FULLTEXT", got.Index.Method)
	require.Equal(t, []string{"RENAMED_BODY"}, got.Index.Columns)
	require.Equal(t, `CREATE CONTEXT INDEX "ORDERS_BYBODY" ON "APP"."ORDERS" ("RENAMED_BODY") SYNC TRANSACTION`, got.SuggestedDDL)
	require.Len(t, got.Units, 1)
	require.Equal(t, got.SuggestedDDL, got.Units[0].SQL)
	require.Equal(t, []string{"BODY"}, obj.Index.Columns)
}
