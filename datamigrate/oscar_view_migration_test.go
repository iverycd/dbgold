package datamigrate

import (
	"context"
	"fmt"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

type oscarViewSchemaReader struct {
	*oscarFixtureReader
	schema string
}

func (r *oscarViewSchemaReader) MigrationSchema() string { return r.schema }

type oscarViewResultWriter struct {
	recordingOscarWriter
	viewSQL string
	fail    bool
}

func (w *oscarViewResultWriter) CreateView(_ context.Context, v source.ViewInfo) error {
	w.viewSQL = dialect.JoinSQL(w.Dialect().ViewStatements("MixedSchema", v))
	if w.fail {
		return fmt.Errorf("view creation failed")
	}
	return nil
}

func TestOscarDottedSchemaViewMigrationAndDrafts(t *testing.T) {
	for _, tt := range []struct {
		name, secondSchema, strip, secondPrefix string
	}{
		{"current database", "FinancePlatform_3.0", "", ""},
		{"case distinct external database retained", "financeplatform_3.0", "", `"financeplatform_3.0".`},
		{"external database retained", "OtherDb.2", "", `"OtherDb.2".`},
		{"external database explicitly stripped", "OtherDb.2", " otherdb.2 ", ""},
	} {
		for _, mode := range []string{"full success", "full failure", "views success", "views failure", "initial draft"} {
			t.Run(tt.name+"/"+mode, func(t *testing.T) {
				ctx := context.Background()
				r := &oscarViewSchemaReader{oscarFixtureReader: newOscarFixture(), schema: "FinancePlatform_3.0"}
				raw := fmt.Sprintf("SELECT `FinancePlatform_3.0`.`Orders`.`id` AS `OrderId`, `%s`.`Items`.*, 'FinancePlatform_3.0.Orders' AS `Label`, 1.0 AS `Amount` FROM `FinancePlatform_3.0`.`Orders` JOIN `%s`.`Items` ON `FinancePlatform_3.0`.`Orders`.`id` = `%s`.`Items`.`id` /* FinancePlatform_3.0.Orders */", tt.secondSchema, tt.secondSchema, tt.secondSchema)
				r.views = []source.ViewInfo{{ViewName: "vOrders", Definition: raw}}
				w := &oscarViewResultWriter{fail: mode == "full failure" || mode == "views failure"}
				m, _ := newTestMigrator(r, w)
				m.cfg.TargetDBType, m.cfg.TargetSchema, m.cfg.CaptureRepair = "oscar", "MixedSchema", true
				// Spare capacity catches accidental append into the user's config slice.
				stripBacking := []string{tt.strip, "unchanged"}
				m.cfg.StripViewSchemas = stripBacking[:1]
				var initialView *repair.Object
				m.cfg.RepairSink = func(manifest RepairManifest) error {
					if initialView == nil {
						for _, o := range manifest.Objects {
							if o.Kind == "view" {
								copy := o
								copy.Units = append([]repair.Unit(nil), o.Units...)
								initialView = &copy
								break
							}
						}
					}
					return nil
				}
				want := fmt.Sprintf(`CREATE OR REPLACE VIEW "MixedSchema"."VORDERS" AS SELECT "ORDERS"."ID" AS "ORDERID", %s"ITEMS".*, 'FinancePlatform_3.0.Orders' AS "LABEL", 1.0 AS "AMOUNT" FROM "ORDERS" JOIN %s"ITEMS" ON "ORDERS"."ID" = %s"ITEMS"."ID" /* FinancePlatform_3.0.Orders */`, tt.secondPrefix, tt.secondPrefix, tt.secondPrefix)
				switch mode {
				case "full success", "full failure":
					report := m.Run(ctx)
					require.Zero(t, report.Tables.Failed)
					if w.fail {
						require.Equal(t, 1, report.Views.Failed)
						require.Len(t, report.Views.Items, 1)
						require.Equal(t, want, report.Views.Items[0].DDL)
						require.Equal(t, "view creation failed", report.Views.Items[0].Error)
					} else {
						require.Equal(t, 1, report.Views.Success)
						require.Zero(t, report.Views.Failed)
					}
				case "views success", "views failure":
					m.beginRepair(ctx)
					results := m.MigrateViews(ctx, []string{"vOrders"})
					require.Len(t, results, 1)
					require.Equal(t, want, results[0].DDL)
					if w.fail {
						require.Equal(t, "view creation failed", results[0].Error)
					} else {
						require.Empty(t, results[0].Error)
					}
				case "initial draft":
					m.beginRepair(ctx)
					require.Empty(t, w.sql)
					require.Empty(t, w.viewSQL)
				}
				require.NotNil(t, initialView)
				require.Equal(t, "vOrders", initialView.SourceName)
				require.Equal(t, want, initialView.DDL)
				require.Len(t, initialView.Units, 1)
				require.Equal(t, want, initialView.Units[0].SQL)
				if mode != "initial draft" {
					require.Equal(t, want, w.viewSQL)
				}
				require.Equal(t, raw, r.views[0].Definition)
				require.Equal(t, "vOrders", r.views[0].ViewName)
				require.Equal(t, []string{tt.strip}, m.cfg.StripViewSchemas)
				require.Equal(t, []string{tt.strip, "unchanged"}, stripBacking)
			})
		}
	}
}

func TestOscarViewDraftRejectsMalformedDefinition(t *testing.T) {
	r := &oscarViewSchemaReader{oscarFixtureReader: newOscarFixture(), schema: "FinancePlatform_3.0"}
	r.views = []source.ViewInfo{{ViewName: "vOrders", Definition: "SELECT `bad"}}
	w := &oscarViewResultWriter{}
	m, _ := newTestMigrator(r, w)
	m.cfg.TargetDBType, m.cfg.TargetSchema = "oscar", "MixedSchema"
	m.beginRepair(context.Background())
	var views []repair.Object
	for _, o := range m.repairs.objects {
		if o.Kind == "view" {
			views = append(views, o)
		}
	}
	require.Len(t, views, 1)
	require.Contains(t, views[0].Error, "未闭合的标识符")
	require.Empty(t, views[0].Units)
	require.Empty(t, w.sql)
	require.Empty(t, w.viewSQL)
}
