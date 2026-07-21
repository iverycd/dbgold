package store

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQueryDataMigrationJobsWithConnFiltersAndPaginates(t *testing.T) {
	setupTestDB(t)
	src := &Connection{OwnerID: 1, Name: "legacy-source", DBType: "mysql", Host: "10.0.0.10", Port: 3306, Database: "orders", Username: "reader", Password: "secret"}
	dst := &Connection{OwnerID: 1, Name: "legacy-target", DBType: "postgres", Host: "10.0.0.20", Port: 5432, Database: "warehouse", Username: "writer", Password: "secret"}
	require.NoError(t, CreateConnection(src))
	require.NoError(t, CreateConnection(dst))

	for i := 0; i < 22; i++ {
		job := &DataMigrationJob{
			OwnerID: 1, JobID: fmt.Sprintf("owned-%02d", i), SrcConnID: src.ID, DstConnID: dst.ID,
			SrcDBType: "mysql", DstDBType: "postgres", Status: "done", DstSchema: "analytics",
		}
		if i == 0 {
			job.BatchID = "batch-special"
			job.Status = "failed"
			job.SrcConnName = "frozen-source"
			job.SrcConnHost = "192.0.2.10"
			job.SrcConnDatabase = "frozen_orders"
			job.DstConnName = "frozen-target"
			job.DstConnHost = "192.0.2.20"
			job.DstConnDatabase = "frozen_warehouse"
		}
		require.NoError(t, CreateDataMigrationJob(job))
	}
	require.NoError(t, CreateDataMigrationJob(&DataMigrationJob{OwnerID: 2, JobID: "other-owner", Status: "done"}))

	page, err := QueryDataMigrationJobsWithConn(1, false, JobListFilter{Page: 1, PageSize: 20, Origin: "all"})
	require.NoError(t, err)
	assert.EqualValues(t, 22, page.Total)
	assert.Len(t, page.Items, 20)
	assert.Equal(t, "owned-21", page.Items[0].JobID)

	batch, err := QueryDataMigrationJobsWithConn(1, false, JobListFilter{Page: 1, PageSize: 20, Origin: "batch", Status: "failed", Keyword: "192.0.2.10"})
	require.NoError(t, err)
	require.Len(t, batch.Items, 1)
	assert.Equal(t, "batch-special", batch.Items[0].BatchID)
	assert.Equal(t, "frozen-source", batch.Items[0].SrcConn.Name)

	legacy, err := QueryDataMigrationJobsWithConn(1, false, JobListFilter{Page: 1, PageSize: 20, Origin: "single", Keyword: "legacy-source"})
	require.NoError(t, err)
	assert.EqualValues(t, 21, legacy.Total)
	assert.NotEmpty(t, legacy.Items)
	assert.Equal(t, "legacy-source", legacy.Items[0].SrcConn.Name)
}

func TestQueryIncrementalJobsWithConnStatusGroupsAndSearch(t *testing.T) {
	setupTestDB(t)
	src := &Connection{OwnerID: 1, Name: "cdc-source", DBType: "mysql", Host: "10.1.0.10", Port: 3306, Database: "source_db", Username: "reader", Password: "secret"}
	dst := &Connection{OwnerID: 1, Name: "cdc-target", DBType: "postgres", Host: "10.1.0.20", Port: 5432, Database: "target_db", Username: "writer", Password: "secret"}
	require.NoError(t, CreateConnection(src))
	require.NoError(t, CreateConnection(dst))

	statuses := []string{"running", "snapshot", "paused_ddl", "ready_with_warnings", "failed", "stopped", "aborted"}
	for i, status := range statuses {
		require.NoError(t, CreateIncrementalJob(&IncrementalMigrationJob{
			OwnerID: 1, JobID: fmt.Sprintf("cdc-%d", i), SrcConnID: src.ID, DstConnID: dst.ID,
			SrcDatabase: "selected_source", TargetSchema: "app_schema", Status: status, LocatorStrategyVersion: 1,
		}))
	}
	require.NoError(t, CreateIncrementalJob(&IncrementalMigrationJob{OwnerID: 2, JobID: "cdc-other", Status: "running"}))

	active, err := QueryIncrementalJobsWithConn(1, false, JobListFilter{Page: 1, PageSize: 20, Status: "active", Keyword: "cdc-source"})
	require.NoError(t, err)
	assert.EqualValues(t, 2, active.Total)

	attention, err := QueryIncrementalJobsWithConn(1, false, JobListFilter{Page: 1, PageSize: 20, Status: "attention", Keyword: "app_schema"})
	require.NoError(t, err)
	assert.EqualValues(t, 3, attention.Total)

	completed, err := QueryIncrementalJobsWithConn(1, false, JobListFilter{Page: 1, PageSize: 20, Status: "completed"})
	require.NoError(t, err)
	require.Len(t, completed.Items, 1)
	assert.Equal(t, "stopped", completed.Items[0].Status)
}
