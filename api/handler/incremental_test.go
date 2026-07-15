package handler

import (
	"testing"

	"dbgold/datamigrate"
	"dbgold/store"
	"github.com/stretchr/testify/require"
)

func TestConfigFromIncrementalUsesFrozenEffectiveTables(t *testing.T) {
	job := &store.IncrementalMigrationJob{
		JobID:          "job-1",
		SrcDatabase:    "source_db",
		TargetSchema:   "target_schema",
		MigrateMode:    "all",
		BootstrapDone:  true,
		EffectiveJSON:  `["Orders","customers"]`,
		LowerCaseNames: true,
	}
	src := &store.Connection{DBType: "mysql", Host: "127.0.0.1", Port: 3306, Username: "root"}
	dst := &store.Connection{DBType: "postgres", Host: "127.0.0.1", Port: 5432, Username: "postgres", Database: "target"}

	cfg := configFromIncremental(job, src, dst)
	require.Equal(t, []string{"Orders", "customers"}, cfg.TableNames)

	job.EffectiveJSON = ""
	cfg = configFromIncremental(job, src, dst)
	require.Nil(t, cfg.TableNames, "legacy completed jobs must retain their original table filter")

	job.EffectiveJSON = "not-json"
	cfg = configFromIncremental(job, src, dst)
	require.NotNil(t, cfg.TableNames)
	require.Empty(t, cfg.TableNames, "a corrupt frozen manifest must fail closed")
}

func TestStrictBootstrapFailure(t *testing.T) {
	success := datamigrate.MigrationReport{
		Tables:    datamigrate.CategoryReport{Total: 1, Success: 1},
		Data:      datamigrate.CategoryReport{Total: 1, Success: 1},
		RowCounts: []datamigrate.TableRowCount{{Table: "users", Src: 2, Dst: 2, Match: true}},
	}
	require.NoError(t, strictBootstrapFailure(success, []string{"users"}))

	withIndexFailure := success
	withIndexFailure.Indexes = datamigrate.CategoryReport{Total: 1, Failed: 1}
	require.ErrorContains(t, strictBootstrapFailure(withIndexFailure, []string{"users"}), "对象创建失败")

	withMismatch := success
	withMismatch.RowCounts = []datamigrate.TableRowCount{{Table: "users", Src: 2, Dst: 1, Match: false}}
	require.ErrorContains(t, strictBootstrapFailure(withMismatch, []string{"users"}), "行数校验失败")
}
