package handler

import (
	"net/http"
	"slices"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/partition"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/store"
	"github.com/stretchr/testify/require"
)

func partitionRepairScopeFixture() repair.Object {
	p := &partition.Plan{Source: &source.PartitionSpec{Method: "RANGE"}, Target: &source.PartitionSpec{Method: "RANGE"}, Interval: &partition.IntervalMaterialization{Schema: "app", Table: "Orders"}}
	p.Statements = []dialect.Statement{{SQL: `CREATE TABLE "app"."Orders" (id integer)`}, {SQL: partition.IntervalMaterializationSQL(p)}}
	o := repair.Object{ID: "table", Kind: "table", Table: "Orders", TargetName: "Orders", Schema: "app", Status: "failed", PartitionPlan: p, ColumnMapping: map[string]string{"ID": "id"}}
	for i, s := range p.Statements {
		o.Units = append(o.Units, repair.Unit{ID: string(rune('a' + i)), SQL: s.SQL, OriginalSQL: s.SQL, Status: "failed", Selected: true})
	}
	return o
}

func TestPartitionRepairScopeRetainsEveryFrozenStepInOrder(t *testing.T) {
	o := partitionRepairScopeFixture()
	require.NoError(t, validateRepairObjectScope(o, o.Units, "dameng"))
	for _, tc := range []string{"remove", "duplicate", "reorder", "edit", "original", "id", "scope", "product"} {
		t.Run(tc, func(t *testing.T) {
			o := partitionRepairScopeFixture()
			units := slices.Clone(o.Units)
			product := "dameng"
			switch tc {
			case "remove":
				units = units[:1]
			case "duplicate":
				units[1] = units[0]
			case "reorder":
				units[0], units[1] = units[1], units[0]
			case "edit":
				units[1].SQL += " altered"
			case "original":
				units[1].OriginalSQL = ""
			case "id":
				units[1].ID = "other"
			case "scope":
				o.Schema = "other"
			case "product":
				product = "oracle"
			}
			require.Error(t, validateRepairObjectScope(o, units, product))
		})
	}
	u := slices.Clone(o.Units)
	u[0].Status, u[0].Selected = "succeeded", false
	require.NoError(t, validateRepairObjectScope(o, u, "dameng"), "successful steps stay saved but are not replayed")
}

func TestPartitionRepairDraftCannotRemoveMaterializationOrRemapKeys(t *testing.T) {
	for _, scenario := range []string{"remove", "mapping", "selection"} {
		t.Run(scenario, func(t *testing.T) {
			seedHandlerRepair(t)
			o := partitionRepairScopeFixture()
			require.NoError(t, store.DB.Model(&store.DataMigrationJob{}).Where("job_id = ?", "job").Update("dst_db_type", "dameng").Error)
			require.NoError(t, store.DB.Model(&store.MigrationRepairObject{}).Where("job_id = ? AND object_id = ?", "job", "table").Update("object_json", repairJSON(o)).Error)
			body := map[string]any{"version": 1, "units": o.Units}
			if scenario == "remove" {
				body["units"] = o.Units[:1]
			} else if scenario == "mapping" {
				body["column_mapping"] = map[string]string{"ID": "different"}
			} else {
				o.Units[0].Selected = false
				body["units"] = o.Units
			}
			w := repairRequest(t, http.MethodPut, "/jobs/job/repairs/table", repairJSON(body), 1, SaveMigrationRepairDraft)
			row, err := store.GetMigrationRepairObject("job", "table")
			require.NoError(t, err)
			if scenario == "selection" {
				require.Equal(t, 200, w.Code, w.Body.String())
				require.Equal(t, 2, row.Version)
			} else {
				require.NotEqual(t, 200, w.Code, w.Body.String())
				require.Equal(t, 1, row.Version)
				require.JSONEq(t, repairJSON(o), row.ObjectJSON)
			}
		})
	}
}
