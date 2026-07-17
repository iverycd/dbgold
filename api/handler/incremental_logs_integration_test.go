//go:build integration

package handler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"dbgold/config"
	"dbgold/datamigrate/cdc"
	"dbgold/store"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

const (
	journalIntegrationMySQLDSN = "root:rootpass@tcp(127.0.0.1:13306)/cdc_source?parseTime=true&charset=utf8mb4"
	journalIntegrationPGDSN    = "host=127.0.0.1 port=15432 user=postgres password=postgrespass dbname=cdc_target sslmode=disable"
	journalIntegrationPrefix   = "log_journal_it_"
	journalIntegrationSchema   = "log_journal_it"
)

// TestIncrementalBootstrapJournalIntegration exercises the real full snapshot
// path with 100+ tables, a table spanning multiple COPY pages, and one DDL
// failure that must remain visible when the task enters bootstrap review.
func TestIncrementalBootstrapJournalIntegration(t *testing.T) {
	if os.Getenv("CDC_INTEGRATION") != "1" {
		t.Skip("set CDC_INTEGRATION=1 and start datamigrate/cdc/testdata/docker-compose.yml")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sourceDB := openJournalIntegrationDB(t, "mysql", journalIntegrationMySQLDSN)
	defer sourceDB.Close()
	targetDB := openJournalIntegrationDB(t, "postgres", journalIntegrationPGDSN)
	defer targetDB.Close()

	tableNames := make([]string, 0, 103)
	for index := 0; index < 100; index++ {
		tableNames = append(tableNames, fmt.Sprintf("%ssmall_%03d", journalIntegrationPrefix, index))
	}
	bulkTable := journalIntegrationPrefix + "bulk"
	badTable := journalIntegrationPrefix + "bad_geometry"
	digitsTable := "log_journal_seed_digits"
	tableNames = append(tableNames, bulkTable, badTable, digitsTable)
	cleanupJournalIntegrationFixtures(t, sourceDB, targetDB, tableNames)
	defer cleanupJournalIntegrationFixtures(t, sourceDB, targetDB, tableNames)

	require.NoError(t, execJournalIntegration(targetDB, `CREATE SCHEMA "`+journalIntegrationSchema+`"`))
	require.NoError(t, execJournalIntegration(sourceDB, "CREATE TABLE `"+digitsTable+"` (`n` INT NOT NULL PRIMARY KEY)"))
	require.NoError(t, execJournalIntegration(sourceDB, "INSERT INTO `"+digitsTable+"` (`n`) VALUES (0),(1),(2),(3),(4),(5),(6),(7),(8),(9)"))
	for index := 0; index < 100; index++ {
		name := fmt.Sprintf("%ssmall_%03d", journalIntegrationPrefix, index)
		require.NoError(t, execJournalIntegration(sourceDB, "CREATE TABLE `"+name+"` (`id` INT NOT NULL PRIMARY KEY, `payload` VARCHAR(32))"))
		require.NoError(t, execJournalIntegration(sourceDB, fmt.Sprintf("INSERT INTO `%s` (`id`,`payload`) VALUES (1,'table-%03d')", name, index)))
	}
	require.NoError(t, execJournalIntegration(sourceDB, "CREATE TABLE `"+bulkTable+"` (`id` INT NOT NULL PRIMARY KEY, `payload` VARCHAR(64))"))
	expression := "a.n*10000+b.n*1000+c.n*100+d.n*10+e.n"
	require.NoError(t, execJournalIntegration(sourceDB, fmt.Sprintf(
		"INSERT INTO `%s` (`id`,`payload`) SELECT %s, CONCAT('payload-', %s) FROM `%s` a CROSS JOIN `%s` b CROSS JOIN `%s` c CROSS JOIN `%s` d CROSS JOIN `%s` e WHERE %s <= 20000",
		bulkTable, expression, expression, digitsTable, digitsTable, digitsTable, digitsTable, digitsTable, expression,
	)))
	require.NoError(t, execJournalIntegration(sourceDB, "CREATE TABLE `"+badTable+"` (`id` INT NOT NULL PRIMARY KEY, `shape` GEOMETRY)"))

	store.Init(&config.Config{SQLitePath: ":memory:"})
	job := &store.IncrementalMigrationJob{
		OwnerID: 1, JobID: "journal-integration", StartMode: "full_then_cdc", PositionMode: "gtid",
		SrcDatabase: "cdc_source", TargetSchema: journalIntegrationSchema,
		MigrateMode: "include", TableFilter: journalIntegrationPrefix + "*", LowerCaseNames: true,
		BootstrapPolicy: "review_and_exclude", BootstrapState: "pending", Status: "initializing", Phase: "initializing",
	}
	require.NoError(t, store.CreateIncrementalJob(job))
	sourceConnection := &store.Connection{DBType: "mysql", Host: "127.0.0.1", Port: 13306, Database: "cdc_source", Username: "root", Password: "rootpass"}
	targetConnection := &store.Connection{DBType: "postgres", Host: "127.0.0.1", Port: 15432, Database: "cdc_target", Username: "postgres", Password: "postgrespass"}

	err := runIncrementalBootstrap(ctx, job, sourceConnection, targetConnection)
	require.ErrorIs(t, err, cdc.ErrBootstrapReview)
	storedJob, err := store.GetIncrementalJob(job.JobID)
	require.NoError(t, err)
	require.Equal(t, "paused_bootstrap_review", storedJob.Status)
	require.Equal(t, 101, storedJob.EffectiveCount)
	require.Equal(t, 1, storedJob.ExcludedCount)

	var logs []store.IncrementalMigrationLog
	require.NoError(t, store.DB.Where("job_id = ?", job.JobID).Order("id ASC").Find(&logs).Error)
	require.Greater(t, len(logs), 500)
	lines := make([]string, len(logs))
	for index := range logs {
		lines[index] = logs[index].Line
	}
	journal := strings.Join(lines, "\n")
	require.Contains(t, journal, "创建表 "+journalIntegrationPrefix+"small_000 ... OK")
	require.Contains(t, journal, "迁移 "+bulkTable+": 第 2 页 (1 行) ... OK")
	require.Contains(t, journal, "创建主键 "+bulkTable+" ... OK")
	require.Contains(t, journal, "行数校验 ["+bulkTable+"]: 源=20001 目标=20001 ... OK")
	require.Contains(t, journal, "排除候选表 "+badTable)
	require.Contains(t, journal, `type "geometry" does not exist`)

	tail, err := store.ListIncrementalMigrationLogs(job.JobID, 0, 0, 500)
	require.NoError(t, err)
	require.Len(t, tail.Items, 500)
	require.True(t, tail.HasOlder)
}

func openJournalIntegrationDB(t *testing.T, driverName, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open(driverName, dsn)
	require.NoError(t, err)
	require.NoError(t, db.Ping())
	return db
}

func execJournalIntegration(db *sql.DB, statement string) error {
	_, err := db.Exec(statement)
	return err
}

func cleanupJournalIntegrationFixtures(t *testing.T, sourceDB, targetDB *sql.DB, tableNames []string) {
	t.Helper()
	_, targetErr := targetDB.Exec(`DROP SCHEMA IF EXISTS "` + journalIntegrationSchema + `" CASCADE`)
	for index := len(tableNames) - 1; index >= 0; index-- {
		_, sourceErr := sourceDB.Exec("DROP TABLE IF EXISTS `" + tableNames[index] + "`")
		if sourceErr != nil && !errors.Is(sourceErr, context.Canceled) {
			t.Logf("cleanup source table %s: %v", tableNames[index], sourceErr)
		}
	}
	if targetErr != nil && !errors.Is(targetErr, context.Canceled) {
		t.Logf("cleanup target schema: %v", targetErr)
	}
}
