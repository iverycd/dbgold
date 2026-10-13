package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"dbgold/datamigrate"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/store"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Keep persistence tests and benchmarks isolated from the application's DB.
func repairPersistenceDB(t testing.TB) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "repair-persistence.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	previous := store.DB
	store.DB = db
	t.Cleanup(func() {
		store.DB = previous
		require.NoError(t, sqlDB.Close())
	})
	require.NoError(t, db.AutoMigrate(&store.MigrationRepairBinding{}, &store.MigrationRepairObject{}))
	return db
}

func repairPersistenceFixture(count int) (*store.DataMigrationJob, *store.Connection, *store.Connection, datamigrate.RepairManifest) {
	j := &store.DataMigrationJob{JobID: "persistence", SrcDBType: "mysql", DstDBType: "gaussdb", SourceDatabase: "src", DstSchema: "dst"}
	src := &store.Connection{DBType: "mysql", Host: "source.invalid", Database: "src", Username: "reader"}
	dst := &store.Connection{DBType: "gaussdb", Host: "target.invalid", Database: "db", Username: "writer"}
	m := datamigrate.RepairManifest{Version: 1, Config: datamigrate.Config{CaptureRepair: true, TargetSchema: "dst", Content: "schema_only"}}
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("table_%04d", i)
		ddl := fmt.Sprintf(`CREATE TABLE "dst"."%s" ("ID" INTEGER, "Name" VARCHAR(64))`, name)
		m.Objects = append(m.Objects, repair.Object{
			ID: name, Kind: "table", Name: name, Table: name, SourceName: name, SourceTable: name, TargetName: name, Schema: "dst",
			DDL: ddl, Status: "pending", DataStatus: "not_required", RequiresReview: true, Dependencies: []string{},
			ColumnMapping: map[string]string{"ID": "ID", "Name": "Name"},
			SourceColumns: []source.ColumnInfo{{Name: "ID", DataType: "int"}, {Name: "Name", DataType: "varchar", Length: 64}},
			Columns:       []source.ColumnInfo{{Name: "ID", DataType: "INTEGER"}, {Name: "Name", DataType: "VARCHAR(64)"}},
			Units:         []repair.Unit{{ID: "step-1", SQL: ddl, OriginalSQL: ddl, Status: "pending", Selected: true}},
		})
	}
	return j, src, dst, m
}

type repairPersistenceWrites struct{ objects, bindings int }

func countRepairPersistenceWrites(t testing.TB, db *gorm.DB) *repairPersistenceWrites {
	t.Helper()
	writes := &repairPersistenceWrites{}
	require.NoError(t, db.Callback().Create().After("gorm:create").Register("test:count-repair-persistence", func(tx *gorm.DB) {
		switch tx.Statement.Table {
		case "migration_repair_objects":
			writes.objects++
		case "migration_repair_bindings":
			writes.bindings++
		}
	}))
	return writes
}

func TestPersistMigrationRepairObjectUpdatesOnlyCurrentRecord(t *testing.T) {
	db := repairPersistenceDB(t)
	j, src, dst, m := repairPersistenceFixture(3)
	m.Config.RepairSink = func(datamigrate.RepairManifest) error { return nil }
	m.Config.RepairObjectSink = func(repair.Object) error { return nil }
	require.NoError(t, persistMigrationManifest(j, src, dst, m))
	require.NoError(t, db.Model(&store.MigrationRepairObject{}).Where("job_id = ? AND object_id = ?", j.JobID, m.Objects[0].ID).Updates(map[string]any{"version": 7, "updated_at": time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)}).Error)
	before, err := store.GetMigrationRepairObject(j.JobID, m.Objects[0].ID)
	require.NoError(t, err)
	other, err := store.GetMigrationRepairObject(j.JobID, m.Objects[1].ID)
	require.NoError(t, err)
	binding, err := store.GetMigrationRepairBinding(j.JobID)
	require.NoError(t, err)
	require.NotContains(t, binding.ConfigJSON, "RepairSink")
	require.NotContains(t, binding.ConfigJSON, "RepairObjectSink")
	cfg, err := recoveryConfig(binding)
	require.NoError(t, err)
	require.False(t, cfg.CaptureRepair)
	require.Nil(t, cfg.RepairSink)
	require.Nil(t, cfg.RepairObjectSink)

	writes := countRepairPersistenceWrites(t, db)
	m.Objects[0].Status, m.Objects[0].Units[0].Status = "running", "running"
	require.NoError(t, persistMigrationRepairObject(j.JobID, m.Objects[0]))
	require.Equal(t, repairPersistenceWrites{objects: 1}, *writes)
	got, err := store.GetMigrationRepairObject(j.JobID, m.Objects[0].ID)
	require.NoError(t, err)
	require.Equal(t, before.ID, got.ID)
	require.Equal(t, 7, got.Version)
	require.True(t, got.UpdatedAt.After(before.UpdatedAt))
	require.JSONEq(t, repairJSON(m.Objects[0]), got.ObjectJSON)
	unchanged, err := store.GetMigrationRepairObject(j.JobID, m.Objects[1].ID)
	require.NoError(t, err)
	require.Equal(t, other, unchanged)
	unchangedBinding, err := store.GetMigrationRepairBinding(j.JobID)
	require.NoError(t, err)
	require.Equal(t, binding, unchangedBinding)

	// A planned object discovered after initialization must be inserted.
	dynamic := repair.Object{ID: "late-index", Kind: "index", Name: "late_index", Table: m.Objects[0].Table, TargetName: "late_index", Status: "running", Dependencies: []string{m.Objects[0].ID}}
	require.NoError(t, persistMigrationRepairObject(j.JobID, dynamic))
	require.Equal(t, repairPersistenceWrites{objects: 2}, *writes)
	inserted, err := store.GetMigrationRepairObject(j.JobID, dynamic.ID)
	require.NoError(t, err)
	require.Equal(t, 1, inserted.Version)
	require.JSONEq(t, repairJSON(dynamic), inserted.ObjectJSON)
}

func TestPersistMigrationRepairObjectMatchesFullCheckpoints(t *testing.T) {
	repairPersistenceDB(t)
	j, src, dst, m := repairPersistenceFixture(4)
	full := *j
	full.JobID = "full-checkpoints"
	require.NoError(t, persistMigrationManifest(j, src, dst, m))
	require.NoError(t, persistMigrationManifest(&full, src, dst, m))
	for i := range m.Objects {
		for _, state := range []string{"running", "succeeded"} {
			m.Objects[i].Status, m.Objects[i].Units[0].Status = state, state
			m.Objects[i].Units[0].Selected = state != "succeeded"
			require.NoError(t, persistMigrationRepairObject(j.JobID, m.Objects[i]))
			require.NoError(t, persistMigrationManifest(&full, src, dst, m))
		}
	}
	assertSameObjects := func() {
		incremental, err := store.ListMigrationRepairObjects(j.JobID)
		require.NoError(t, err)
		checkpoints, err := store.ListMigrationRepairObjects(full.JobID)
		require.NoError(t, err)
		require.Len(t, incremental, len(checkpoints))
		for i := range incremental {
			require.Equal(t, checkpoints[i].ObjectID, incremental[i].ObjectID)
			require.Equal(t, checkpoints[i].Version, incremental[i].Version)
			require.JSONEq(t, checkpoints[i].ObjectJSON, incremental[i].ObjectJSON)
		}
	}
	assertSameObjects()
	for i := range m.Objects {
		m.Objects[i].OriginalStatus = m.Objects[i].Status
	}
	m.Objects = append(m.Objects, repair.Object{ID: "metadata-failure", Kind: "index", Name: "metadata", Error: "unavailable", Status: "failed", OriginalStatus: "failed"})
	require.NoError(t, persistMigrationManifest(j, src, dst, m))
	require.NoError(t, persistMigrationManifest(&full, src, dst, m))
	assertSameObjects()
}

func TestPersistMigrationRepairObjectFailureKeepsDurableState(t *testing.T) {
	db := repairPersistenceDB(t)
	j, src, dst, m := repairPersistenceFixture(2)
	m.Objects[0].Status, m.Objects[0].Units[0].Status = "running", "running"
	require.NoError(t, persistMigrationManifest(j, src, dst, m))
	before, err := store.GetMigrationRepairObject(j.JobID, m.Objects[0].ID)
	require.NoError(t, err)
	binding, err := store.GetMigrationRepairBinding(j.JobID)
	require.NoError(t, err)
	diskErr := errors.New("simulated persistence failure")
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register("test:fail-repair-persistence", func(tx *gorm.DB) {
		if tx.Statement.Table == "migration_repair_objects" {
			tx.AddError(diskErr)
		}
	}))
	m.Objects[0].Status, m.Objects[0].Units[0].Status = "succeeded", "succeeded"
	require.ErrorIs(t, persistMigrationRepairObject(j.JobID, m.Objects[0]), diskErr)
	got, err := store.GetMigrationRepairObject(j.JobID, m.Objects[0].ID)
	require.NoError(t, err)
	require.Equal(t, before, got)
	unchangedBinding, err := store.GetMigrationRepairBinding(j.JobID)
	require.NoError(t, err)
	require.Equal(t, binding, unchangedBinding)
	var durable repair.Object
	require.NoError(t, json.Unmarshal([]byte(got.ObjectJSON), &durable))
	require.Equal(t, "running", durable.Units[0].Status)
}

func TestPersistMigrationRepairObjectWriteCountIndependentOfManifestSize(t *testing.T) {
	for _, count := range []int{100, 1000, 2704} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			db := repairPersistenceDB(t)
			j, src, dst, m := repairPersistenceFixture(count)
			require.NoError(t, persistMigrationManifest(j, src, dst, m))
			writes := countRepairPersistenceWrites(t, db)
			for _, state := range []string{"running", "succeeded"} {
				m.Objects[0].Status, m.Objects[0].Units[0].Status = state, state
				require.NoError(t, persistMigrationRepairObject(j.JobID, m.Objects[0]))
			}
			require.Equal(t, repairPersistenceWrites{objects: 2}, *writes)
		})
	}
}

func BenchmarkMigrationRepairProgressPersistence(b *testing.B) {
	for _, count := range []int{100, 1000, 2704} {
		for _, incremental := range []bool{false, true} {
			name := "full"
			if incremental {
				name = "single"
			}
			b.Run(fmt.Sprintf("objects=%d/%s", count, name), func(b *testing.B) {
				db := repairPersistenceDB(b)
				j, src, dst, m := repairPersistenceFixture(count)
				require.NoError(b, persistMigrationManifest(j, src, dst, m))
				writes := countRepairPersistenceWrites(b, db)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					state := "running"
					if i%2 == 1 {
						state = "succeeded"
					}
					m.Objects[0].Status, m.Objects[0].Units[0].Status = state, state
					var err error
					if incremental {
						err = persistMigrationRepairObject(j.JobID, m.Objects[0])
					} else {
						err = persistMigrationManifest(j, src, dst, m)
					}
					if err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(writes.objects)/float64(b.N), "object-writes/op")
				b.ReportMetric(float64(writes.bindings)/float64(b.N), "binding-writes/op")
			})
		}
	}
}
