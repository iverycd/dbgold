package datamigrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/store"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func openCommentRepairSQLite(tb testing.TB, path string, models ...any) *gorm.DB {
	tb.Helper()
	// Match store.InitWithError: a file-backed database, the default SQLite
	// durability settings, and GORM's default transaction behavior.
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	require.NoError(tb, err)
	sqlDB, err := db.DB()
	require.NoError(tb, err)
	tb.Cleanup(func() { _ = sqlDB.Close() })
	if len(models) == 0 {
		models = []any{&store.MigrationRepairObject{}}
	}
	require.NoError(tb, db.AutoMigrate(models...))
	return db
}

func saveCommentRepairSQLite(db *gorm.DB, jobID string, object repair.Object) error {
	payload, err := json.Marshal(object)
	if err != nil {
		return err
	}
	// Keep the statement identical to the API's upsertMigrationRepairObject.
	row := store.MigrationRepairObject{JobID: jobID, ObjectID: object.ID, ObjectJSON: string(payload), Version: 1}
	return db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "job_id"}, {Name: "object_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"object_json", "updated_at"}),
	}).Create(&row).Error
}

func plannedCommentRepairObjects(count int) ([]repair.Object, [][]dialect.Statement) {
	objects := make([]repair.Object, count)
	statements := make([][]dialect.Statement, count)
	for i := range objects {
		object, sql := commentOptimizationObject(fmt.Sprintf("Column_%05d", i))
		object.ID = repairID(object.Kind, object.Table, object.TargetName)
		object.Schema = "APP"
		object.Status = "pending"
		object.DDL = dialect.JoinSQL(sql)
		object.Units = unitsFor(sql)
		object.Dependencies = []string{}
		objects[i], statements[i] = object, sql
	}
	return objects, statements
}

func seedCommentRepairSQLite(tb testing.TB, db *gorm.DB, jobID string, objects []repair.Object) {
	tb.Helper()
	rows := make([]store.MigrationRepairObject, len(objects))
	for i, object := range objects {
		payload, err := json.Marshal(object)
		require.NoError(tb, err)
		rows[i] = store.MigrationRepairObject{JobID: jobID, ObjectID: object.ID, ObjectJSON: string(payload), Version: 1}
	}
	// Preparation is outside the benchmark timer. Each measured progress save
	// below still uses the production single-object synchronous upsert.
	require.NoError(tb, db.CreateInBatches(rows, 100).Error)
}

func commentRepairSQLiteMigrator(objects []repair.Object, optimized bool) *Migrator {
	m, _ := newTestMigrator(
		&commentOptimizationReader{sourceType: "mysql"},
		&commentOptimizationWriter{targetType: "dameng"},
	)
	m.cfg.CaptureRepair = optimized
	m.cfg.TargetSchema = "APP"
	m.repairs = &repairRecorder{objects: append([]repair.Object(nil), objects...)}
	m.repairs.rebuildObjectIndex()
	return m
}

// Run explicitly with -run '^$' -bench '^BenchmarkCommentRepairSQLite27463$'
// -benchtime=1x. The observer-only target removes network/database variability;
// the benchmark measures local bookkeeping and real durable SQLite saves.
func BenchmarkCommentRepairSQLite27463(b *testing.B) {
	const commentCount = 27463
	for _, test := range []struct {
		name      string
		optimized bool
		savesEach int
	}{
		{name: "legacy_four_saves", savesEach: 4},
		{name: "optimized_two_saves", optimized: true, savesEach: 2},
	} {
		b.Run(test.name, func(b *testing.B) {
			b.StopTimer()
			objects, statements := plannedCommentRepairObjects(commentCount)
			db := openCommentRepairSQLite(b, filepath.Join(b.TempDir(), "repairs.db"))
			const jobID = "comment-benchmark"
			seedCommentRepairSQLite(b, db, jobID, objects)
			saves := 0
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				m := commentRepairSQLiteMigrator(objects, test.optimized)
				m.cfg.RepairObjectSink = func(object repair.Object) error {
					saves++
					return saveCommentRepairSQLite(db, jobID, object)
				}
				b.StartTimer()
				for j, object := range objects {
					if err := m.captureRepairCall(context.Background(), object, statements[j], func(ctx context.Context) error {
						return observeIncrementalRepairSQL(ctx, statements[j], nil)
					}); err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
			}
			require.Equal(b, b.N*commentCount*test.savesEach, saves)
			b.ReportMetric(commentCount, "comments/op")
			b.ReportMetric(float64(saves)/float64(b.N*commentCount), "saves/comment")
		})
	}
}

func TestCommentRepairSQLiteInterruptedFinalSaveRecoversUnknown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repairs.db")
	db := openCommentRepairSQLite(t, path,
		&store.MigrationRepairObject{}, &store.MigrationRepairAttempt{},
		&store.DataMigrationJob{}, &store.MigrationRepairLock{},
	)
	const jobID = "interrupted-comment"
	objects, statements := plannedCommentRepairObjects(1)
	seedCommentRepairSQLite(t, db, jobID, objects)
	require.NoError(t, db.Create(&store.DataMigrationJob{JobID: jobID, Status: "running"}).Error)
	m := commentRepairSQLiteMigrator(objects, true)
	interrupted := errors.New("simulated interruption before final state save")
	saves, executions := 0, 0
	m.cfg.RepairObjectSink = func(object repair.Object) error {
		saves++
		if saves == 2 {
			return interrupted
		}
		return saveCommentRepairSQLite(db, jobID, object)
	}
	err := m.captureRepairCall(context.Background(), objects[0], statements[0], func(ctx context.Context) error {
		return observeIncrementalRepairSQL(ctx, statements[0], &executions)
	})
	require.ErrorIs(t, err, interrupted)
	require.Equal(t, 2, saves)
	require.Equal(t, 1, executions)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())

	// Read only the newly opened file: in-memory execution success cannot be
	// mistaken for evidence that its final durable save succeeded.
	reopened := openCommentRepairSQLite(t, path)
	readObject := func() repair.Object {
		var row store.MigrationRepairObject
		require.NoError(t, reopened.Where("job_id = ? AND object_id = ?", jobID, objects[0].ID).First(&row).Error)
		var object repair.Object
		require.NoError(t, json.Unmarshal([]byte(row.ObjectJSON), &object))
		return object
	}
	durable := readObject()
	require.Equal(t, "running", durable.Status)
	require.Equal(t, "running", durable.Units[0].Status)

	// This package's tests do not run store operations in parallel. Restore the
	// global handle immediately after exercising the production startup routine.
	previousDB := store.DB
	store.DB = reopened
	defer func() { store.DB = previousDB }()
	require.NoError(t, store.MarkInterruptedRepairsUnknown())
	durable = readObject()
	require.Equal(t, "unknown", durable.Status)
	require.Equal(t, "unknown", durable.Units[0].Status)
	require.Equal(t, 1, executions, "startup recovery must not replay an uncertain target statement")
	var job store.DataMigrationJob
	require.NoError(t, reopened.First(&job, "job_id = ?", jobID).Error)
	require.Equal(t, "failed", job.Status)
	require.Contains(t, job.Summary, "禁止自动重放")
}
