//go:build integration

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dbgold/config"
	"dbgold/datamigrate/cdc"
	"dbgold/store"
	"github.com/stretchr/testify/require"
)

func TestIncrementalCutoverContentWaiverIntegration(t *testing.T) {
	if os.Getenv("CDC_INTEGRATION") != "1" {
		t.Skip("set CDC_INTEGRATION=1 and start datamigrate/cdc/testdata/docker-compose.yml")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	my := openJournalIntegrationDB(t, "mysql", journalIntegrationMySQLDSN)
	defer my.Close()
	pg := openJournalIntegrationDB(t, "postgres", journalIntegrationPGDSN)
	defer pg.Close()
	const schema = "cdc_cutover_validation_it"
	const keyed = "cutover_validation_items_it"
	const keyless = "cutover_validation_bag_it"
	for _, table := range []string{keyed, keyless} {
		require.NoError(t, execJournalIntegration(my, "DROP TABLE IF EXISTS "+table))
		defer execJournalIntegration(my, "DROP TABLE IF EXISTS "+table)
	}
	require.NoError(t, execJournalIntegration(pg, "DROP SCHEMA IF EXISTS "+schema+" CASCADE"))
	defer execJournalIntegration(pg, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	require.NoError(t, execJournalIntegration(pg, "CREATE SCHEMA "+schema))
	require.NoError(t, execJournalIntegration(my, "CREATE TABLE "+keyed+" (id INT PRIMARY KEY, payload VARCHAR(40)) ENGINE=InnoDB"))
	require.NoError(t, execJournalIntegration(my, "CREATE TABLE "+keyless+" (payload VARCHAR(40)) ENGINE=InnoDB"))
	require.NoError(t, execJournalIntegration(my, "INSERT INTO "+keyed+" VALUES (1,'source')"))
	require.NoError(t, execJournalIntegration(my, "INSERT INTO "+keyless+" VALUES ('a'),('a')"))
	require.NoError(t, execJournalIntegration(pg, "CREATE TABLE "+schema+"."+keyed+" (id integer PRIMARY KEY, payload varchar(40))"))
	require.NoError(t, execJournalIntegration(pg, "CREATE TABLE "+schema+"."+keyless+" (payload varchar(40))"))
	require.NoError(t, execJournalIntegration(pg, "INSERT INTO "+schema+"."+keyed+" VALUES (1,'target')"))
	require.NoError(t, execJournalIntegration(pg, "INSERT INTO "+schema+"."+keyless+" VALUES ('a'),('b')"))
	store.Init(&config.Config{SQLitePath: filepath.Join(t.TempDir(), "dbgold.sqlite")})
	src := &store.Connection{OwnerID: 1, Name: "cdc-cutover-source", DBType: "mysql", Host: "127.0.0.1", Port: 13306, Database: "cdc_source", Username: "root", Password: "rootpass"}
	dst := &store.Connection{OwnerID: 1, Name: "cdc-cutover-target", DBType: "postgres", Host: "127.0.0.1", Port: 15432, Database: "cdc_target", Username: "postgres", Password: "postgrespass"}
	require.NoError(t, store.CreateConnection(src))
	require.NoError(t, store.CreateConnection(dst))
	start, err := cdc.CurrentPosition(ctx, my)
	require.NoError(t, err)
	resp := incrementalDamengHandler(t, ctx, StartIncremental, "", incrementalRequest{
		SrcConnID: src.ID, DstConnID: dst.ID, SrcDatabase: src.Database, TargetSchema: schema,
		StartMode: "incremental_only", PositionMode: "file", StartFile: start.File, StartPosition: start.Pos,
		MigrateMode: "include", TableFilter: "cutover_validation_*_it",
	})
	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
	var created struct {
		JobID string `json:"job_id"`
	}
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &created))
	require.NotEmpty(t, created.JobID)
	defer func() {
		if cdc.Registry.Cancel(created.JobID, "abort") {
			require.Eventually(t, func() bool { return !cdc.Registry.Running(created.JobID) }, 5*time.Second, 20*time.Millisecond)
		}
	}()
	job := waitIncrementalDamengStatus(t, created.JobID, "running")
	resp = incrementalDamengHandler(t, ctx, PrepareIncrementalCutover, job.JobID, nil)
	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
	job = waitIncrementalDamengStatus(t, job.JobID, "ready_with_warnings")
	require.Equal(t, "passed", job.ValidationState)
	require.Equal(t, contentValidationNeedsAcknowledgment, job.ContentValidationState)
	var results []cdc.CutoverTableValidation
	require.NoError(t, json.Unmarshal([]byte(job.ValidationJSON), &results))
	require.Len(t, results, 2)
	byTable := map[string]cdc.CutoverTableValidation{}
	for _, result := range results {
		require.True(t, result.Match)
		require.NotNil(t, result.Content)
		require.Equal(t, "mismatch", result.Content.Status)
		byTable[result.Table] = result
	}
	require.EqualValues(t, 1, byTable[keyed].Content.ChangedRows)
	require.EqualValues(t, 1, byTable[keyless].Content.MissingRows)
	require.EqualValues(t, 1, byTable[keyless].Content.ExtraRows)
	resp = incrementalDamengHandler(t, ctx, StopIncremental, job.JobID, completeCutoverRequest{AcknowledgeWarnings: true})
	require.Equal(t, http.StatusConflict, resp.Code, resp.Body.String())
	require.Contains(t, resp.Body.String(), "必须单独确认豁免")
	// Source writes after the captured boundary are a hard failure even when
	// the caller approves the content waiver.
	require.NoError(t, execJournalIntegration(my, "INSERT INTO "+keyed+" VALUES (2,'later')"))
	resp = incrementalDamengHandler(t, ctx, StopIncremental, job.JobID, completeCutoverRequest{AcknowledgeContentValidation: true})
	require.Equal(t, http.StatusConflict, resp.Code, resp.Body.String())
	require.Contains(t, resp.Body.String(), "又产生了新事务")
	resp = incrementalDamengHandler(t, ctx, CancelIncrementalCutover, job.JobID, nil)
	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
	waitIncrementalDamengCheckpoint(t, ctx, my, configFromIncremental(job, src, dst))
	waitIncrementalDamengStatus(t, job.JobID, "running")
	resp = incrementalDamengHandler(t, ctx, PrepareIncrementalCutover, job.JobID, nil)
	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
	job = waitIncrementalDamengStatus(t, job.JobID, "ready_with_warnings")
	require.Equal(t, contentValidationNeedsAcknowledgment, job.ContentValidationState)
	resp = incrementalDamengHandler(t, ctx, StopIncremental, job.JobID, completeCutoverRequest{AcknowledgeContentValidation: true})
	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
	finished := waitIncrementalDamengStatus(t, job.JobID, "stopped")
	require.Equal(t, contentValidationWaived, finished.ContentValidationState)
	require.EqualValues(t, 1, finished.ContentValidationWaivedBy)
	require.NotNil(t, finished.ContentValidationWaivedAt)
	require.Equal(t, job.ValidationJSON, finished.ValidationJSON)
}
