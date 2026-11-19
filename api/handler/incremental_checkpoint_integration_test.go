//go:build integration

package handler

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dbgold/config"
	"dbgold/datamigrate/cdc"
	"dbgold/store"
	"github.com/stretchr/testify/require"
)

func TestManagedCDCFullCopyAndSameSecondRecoveryIntegration(t *testing.T) {
	for _, target := range []string{"postgres", "dameng"} {
		t.Run(target, func(t *testing.T) {
			if (target == "postgres" && os.Getenv("CDC_INTEGRATION") != "1") ||
				(target == "dameng" && os.Getenv("CDC_DAMENG_INTEGRATION") != "1") {
				t.Skip("requires isolated CDC test databases")
			}
			for _, mode := range []string{"gtid", "file"} {
				t.Run(mode, func(t *testing.T) {
					exerciseManagedCDCBoundaries(t, target, mode)
				})
			}
		})
	}
}

func exerciseManagedCDCBoundaries(t *testing.T, targetType, mode string) {
	ctx, cancel := context.WithTimeout(t.Context(), 75*time.Second)
	defer cancel()
	sourceDB, targetDB, src, dst, schema := managedCDCFixture(t, ctx, targetType)
	exec := func(db *sql.DB, statement string, args ...any) {
		t.Helper()
		_, err := db.ExecContext(ctx, statement, args...)
		require.NoError(t, err, statement)
	}

	// A keyless table makes duplicate replay observable by row count as well.
	table := fmt.Sprintf("boundary_%x", time.Now().UnixNano())
	exec(sourceDB, "CREATE TABLE "+table+" (id INT, payload VARCHAR(40)) ENGINE=InnoDB")
	t.Cleanup(func() { _, _ = sourceDB.Exec("DROP TABLE IF EXISTS " + table) })
	exec(sourceDB, "INSERT INTO "+table+" VALUES (1,'snapshot')")
	checkpointTable := cdc.CheckpointTableName
	targetTable := table
	bind := "$1"
	if targetType == "dameng" {
		checkpointTable = strings.ToUpper(checkpointTable)
		targetTable = strings.ToUpper(targetTable)
		bind = "?"
	}
	onTarget := func(table string) string { return `"` + schema + `"."` + table + `"` }

	job := &store.IncrementalMigrationJob{
		OwnerID: 1, JobID: "boundary-" + table, SrcConnID: src.ID, DstConnID: dst.ID,
		SrcDBType: src.DBType, DstDBType: dst.DBType, SrcDatabase: src.Database, TargetSchema: schema,
		CheckpointManaged: true, SourceSchemaVersion: cdc.SourceSchemaVersion,
		StartMode: "full_then_cdc", PositionMode: mode, MigrateMode: "include", TableFilter: table,
		BootstrapPolicy: "fail_all", BootstrapState: "pending", Status: "initializing", ServerID: 30309322,
		LowerCaseNames: targetType == "postgres",
	}
	require.NoError(t, store.CreateIncrementalJob(job))
	cfg := configFromIncremental(job, src, dst)
	require.NoError(t, claimIncrementalTarget(ctx, cfg))
	t.Cleanup(func() { _ = cdc.ReleaseTargetSchema(context.Background(), cfg) })

	alias := *dst
	alias.ID = 0
	alias.Host = "localhost"
	alias.Name = "other connection to this schema"
	require.NoError(t, store.CreateConnection(&alias))
	alternate := *job
	alternate.JobID, alternate.DstConnID = "other-"+table, alias.ID
	require.ErrorIs(t, claimIncrementalTarget(ctx, configFromIncremental(&alternate, src, &alias)), errIncrementalTargetInUse)

	// Hold the physical owner row so bootstrap establishes the source snapshot
	// but cannot yet publish its first checkpoint or start copying tables.
	lock, err := targetDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer lock.Rollback()
	var owner string
	err = lock.QueryRowContext(ctx, "SELECT binlog_file FROM "+onTarget(checkpointTable)+" WHERE bootstrap_state='schema_owner' FOR UPDATE").Scan(&owner)
	require.NoError(t, err)
	require.Equal(t, job.JobID, owner)
	boot := make(chan error, 1)
	go func() { boot <- runIncrementalBootstrap(ctx, job, src, dst) }()
	require.Eventually(t, func() bool {
		logs, err := store.ListIncrementalMigrationLogs(job.JobID, 0, 0, 10)
		if err != nil {
			return false
		}
		for _, log := range logs.Items {
			if strings.Contains(log.Line, "已获取一致快照起始位点") {
				return true
			}
		}
		return false
	}, 10*time.Second, 25*time.Millisecond)

	session, err := sourceDB.Conn(ctx)
	require.NoError(t, err)
	defer session.Close()
	second := time.Now().Unix()
	_, err = session.ExecContext(ctx, fmt.Sprintf("SET timestamp = %d", second))
	require.NoError(t, err)
	defer session.ExecContext(ctx, "SET timestamp = DEFAULT")
	write, err := session.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer write.Rollback()
	_, err = write.ExecContext(ctx, "UPDATE "+table+" SET payload='during-copy' WHERE id=1")
	require.NoError(t, err)
	_, err = write.ExecContext(ctx, "INSERT INTO "+table+" VALUES (2,'first'),(3,'second')")
	require.NoError(t, err)
	require.NoError(t, write.Commit(), "source DML should commit while the full snapshot is open")
	require.NoError(t, lock.Rollback())
	require.NoError(t, <-boot)

	check := func(expected int, payload string) {
		t.Helper()
		var count int
		var value string
		require.NoError(t, targetDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+onTarget(targetTable)).Scan(&count))
		require.Equal(t, expected, count)
		require.NoError(t, targetDB.QueryRowContext(ctx, "SELECT payload FROM "+onTarget(targetTable)+" WHERE id=1").Scan(&value))
		require.Equal(t, payload, value)
	}
	check(1, "snapshot")
	cfg = configFromIncremental(job, src, dst)
	cfg.RequireExistingCheckpoint = true
	positionDB, err := cdc.OpenSource(cfg.SourceDSN)
	require.NoError(t, err)
	defer positionDB.Close()
	runToHead := func() cdc.Stats {
		t.Helper()
		head, err := cdc.CurrentPosition(ctx, positionDB)
		require.NoError(t, err)
		runCtx, err := cdc.Registry.Register(job.JobID)
		require.NoError(t, err)
		defer cdc.Registry.Remove(job.JobID)
		runCtx, stop := context.WithTimeout(runCtx, 12*time.Second)
		defer stop()
		require.True(t, cdc.Registry.RequestCutover(job.JobID, head))
		var last cdc.Stats
		err = cdc.NewRunner(cfg, cdc.Hooks{Stats: func(stats cdc.Stats) { last = stats }}).Run(runCtx)
		require.ErrorIs(t, err, cdc.ErrCutoverReady)
		return last
	}
	first := runToHead()
	check(3, "during-copy")
	_, err = session.ExecContext(ctx, "INSERT INTO "+table+" VALUES (4,'after-checkpoint'),(5,'also-after-checkpoint')")
	require.NoError(t, err)
	resumed := runToHead()
	check(5, "during-copy")
	require.Equal(t, second, first.LastEventAt.Unix())
	require.Equal(t, second, resumed.LastEventAt.Unix())
	require.True(t, cdc.PositionReached(resumed.Position, first.Position))
	require.False(t, cdc.PositionReached(first.Position, resumed.Position))
	require.EqualValues(t, 2, resumed.Inserts)

	expectedRows := 5
	if mode == "file" {
		// A receiver starting at the previous file's end must process the
		// actual rotation and header of a file which has no business writes.
		exec(positionDB, "FLUSH BINARY LOGS")
		rotated := runToHead()
		require.NotEqual(t, resumed.Position.File, rotated.Position.File)
		require.EqualValues(t, 0, rotated.Inserts)
		check(expectedRows, "during-copy")
		_, err = session.ExecContext(ctx, "INSERT INTO "+table+" VALUES (6,'after-rotation')")
		require.NoError(t, err)
		require.EqualValues(t, 1, runToHead().Inserts)
		expectedRows++
	}

	// Deleting the authoritative checkpoint makes recovery fail without writing
	// any rows or creating a replacement checkpoint at the original start.
	ready, runErr := make(chan struct{}, 1), make(chan error, 1)
	go func() {
		runErr <- cdc.NewRunner(cfg, cdc.Hooks{Stats: func(stats cdc.Stats) {
			if stats.CaughtUp {
				select {
				case ready <- struct{}{}:
				default:
				}
			}
		}}).Run(ctx)
	}()
	select {
	case <-ready:
	case err := <-runErr:
		t.Fatalf("runner never became ready: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	exec(targetDB, "DELETE FROM "+onTarget(checkpointTable)+" WHERE job_id="+bind, job.JobID)
	_, err = session.ExecContext(ctx, "INSERT INTO "+table+" VALUES (7,'must-not-replay')")
	require.NoError(t, err)
	require.ErrorContains(t, <-runErr, "目标检查点")
	require.ErrorContains(t, cdc.NewRunner(cfg, cdc.Hooks{}).Run(ctx), "目标检查点")
	require.ErrorIs(t, checkIncrementalRecovery(ctx, job, src, dst), cdc.ErrCheckpointMissing)
	_, exists, err := cdc.LoadTargetCheckpoint(ctx, cfg)
	require.NoError(t, err)
	require.False(t, exists)
	check(expectedRows, "during-copy")
	require.NoError(t, cdc.ReleaseTargetSchema(ctx, cfg))
	require.NoError(t, claimIncrementalTarget(ctx, configFromIncremental(&alternate, src, &alias)))
	require.NoError(t, cdc.ReleaseTargetSchema(ctx, configFromIncremental(&alternate, src, &alias)))
	exec(targetDB, "DROP TABLE "+onTarget(checkpointTable))
	require.Error(t, cdc.NewRunner(cfg, cdc.Hooks{}).Run(ctx))
	var rows int
	require.Error(t, targetDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+onTarget(checkpointTable)).Scan(&rows))
}

func managedCDCFixture(t *testing.T, ctx context.Context, target string) (*sql.DB, *sql.DB, *store.Connection, *store.Connection, string) {
	t.Helper()
	if target == "dameng" {
		return incrementalDamengFixture(t, ctx)
	}
	sourceDB := openJournalIntegrationDB(t, "mysql", journalIntegrationMySQLDSN)
	targetDB := openJournalIntegrationDB(t, "postgres", journalIntegrationPGDSN)
	schema := fmt.Sprintf("boundary_%x", time.Now().UnixNano())
	_, err := targetDB.ExecContext(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = targetDB.Exec("DROP SCHEMA " + schema + " CASCADE")
		sourceDB.Close()
		targetDB.Close()
	})
	store.Init(&config.Config{SQLitePath: filepath.Join(t.TempDir(), "boundary.sqlite")})
	stateDB, err := store.DB.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = stateDB.Close() })
	src := &store.Connection{DBType: "mysql", Name: "boundary source", Host: "127.0.0.1", Port: 13306, Database: "cdc_source", Username: "root", Password: "rootpass"}
	dst := &store.Connection{DBType: "postgres", Name: "boundary target", Host: "127.0.0.1", Port: 15432, Database: "cdc_target", Username: "postgres", Password: "postgrespass"}
	require.NoError(t, store.CreateConnection(src))
	require.NoError(t, store.CreateConnection(dst))
	return sourceDB, targetDB, src, dst, schema
}
