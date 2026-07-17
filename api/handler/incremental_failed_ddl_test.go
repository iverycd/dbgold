package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dbgold/config"
	"dbgold/datamigrate/cdc"
	"dbgold/store"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRenderIncrementalFailedDDLIsSafeAndOrdered(t *testing.T) {
	job := &store.IncrementalMigrationJob{JobID: "1234567890", SrcDatabase: "source_db\nDROP DATABASE unsafe;", TargetSchema: "target_schema"}
	review := cdc.BootstrapReview{BootstrapRecord: cdc.BootstrapRecord{
		Position: cdc.Position{File: "bin.000001", Pos: 88}, FailureReportVersion: 1,
	}}
	items := []cdc.BootstrapFailedObject{
		{Category: "index", Name: "idx_t", Error: "index\nfailed", DDL: "CREATE INDEX idx_t ON t(id)", Stage: "objects"},
		{Category: "table", Name: "t", Error: "bad type", DDL: "DROP TABLE IF EXISTS t CASCADE;\nCREATE TABLE t(id badtype);", Stage: "schema"},
		{Category: "data", Name: "data_t", Error: `invalid input syntax for type integer: "customer-secret"`, Stage: "data"},
	}
	content := renderIncrementalFailedDDL(job, review, items, time.Date(2026, 7, 17, 10, 0, 0, 0, time.FixedZone("CST", 8*3600)))
	require.Less(t, strings.Index(content, "表（1）"), strings.Index(content, "索引（1）"))
	require.Less(t, strings.Index(content, "索引（1）"), strings.Index(content, "数据写入（1）"))
	require.Contains(t, content, "-- DROP TABLE IF EXISTS t CASCADE;")
	require.Contains(t, content, "\nCREATE TABLE t(id badtype);\n")
	require.Contains(t, content, "\nCREATE INDEX idx_t ON t(id);\n")
	require.Contains(t, content, "-- DDL: （无）")
	require.Contains(t, content, "-- DROP DATABASE unsafe;")
	require.NotContains(t, content, "customer-secret")
	require.Contains(t, content, `"<redacted>"`)
	assertNoActiveDestructiveSQL(t, content)
}

func TestWriteRepairDDLCommentsEveryDestructiveStatement(t *testing.T) {
	var b strings.Builder
	writeRepairDDL(&b, `  dRoP TABLE "odd;name"
CASCADE
;
TrUnCaTe TABLE audit;
delete FROM audit;
ALTER TABLE audit ADD COLUMN note text`)
	content := b.String()
	require.Contains(t, content, `-- dRoP TABLE "odd;name"`)
	require.Contains(t, content, "-- CASCADE")
	require.Contains(t, content, "-- TrUnCaTe TABLE audit;")
	require.Contains(t, content, "-- delete FROM audit;")
	require.Contains(t, content, "\nALTER TABLE audit ADD COLUMN note text;\n")
	assertNoActiveDestructiveSQL(t, content)
}

func TestWriteRepairDDLActivatesSupportedObjectDDLAndAddsSemicolon(t *testing.T) {
	cases := []string{
		`ALTER TABLE "s"."t" ADD PRIMARY KEY ("id")`,
		`CREATE UNIQUE INDEX "idx_t" ON "s"."t" ("id")`,
		`ALTER TABLE "s"."child" ADD CONSTRAINT "fk" FOREIGN KEY ("pid") REFERENCES "s"."parent" ("id")`,
		`CREATE SEQUENCE "s"."seq_t_id"`,
		`CREATE OR REPLACE VIEW "s"."v" AS SELECT 1`,
		`COMMENT ON TABLE "s"."t" IS '说明'`,
	}
	for _, ddl := range cases {
		var b strings.Builder
		writeRepairDDL(&b, ddl)
		require.Equal(t, ddl+";\n", b.String())
	}
}

func assertNoActiveDestructiveSQL(t *testing.T, content string) {
	t.Helper()
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		upper := strings.ToUpper(trimmed)
		require.Falsef(t, strings.HasPrefix(upper, "DROP ") || strings.HasPrefix(upper, "TRUNCATE ") || strings.HasPrefix(upper, "DELETE "), "active destructive SQL: %q", line)
	}
}

func TestExportIncrementalFailedDDLFromPersistedReport(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store.Init(&config.Config{SQLitePath: ":memory:"})
	review := cdc.BootstrapReview{BootstrapRecord: cdc.BootstrapRecord{
		Position: cdc.Position{GTID: "uuid:1-10"}, FailureReportVersion: 1,
		FailedObjects: []cdc.BootstrapFailedObject{{Category: "view", Name: "bad_view", Error: "missing table", DDL: "CREATE VIEW bad_view AS SELECT * FROM missing", Stage: "objects"}},
	}}
	payload, err := json.Marshal(review)
	require.NoError(t, err)
	require.NoError(t, store.CreateIncrementalJob(&store.IncrementalMigrationJob{
		OwnerID: 7, JobID: "export-job-1234", StartMode: "full_then_cdc", SrcDatabase: "source_db", TargetSchema: "target_schema",
		BootstrapReport: string(payload), FailedObjectCount: 1, FailedDDLCount: 1, Status: "failed",
	}))

	router := gin.New()
	router.GET("/jobs/:jobID/export", func(c *gin.Context) {
		c.Set("userID", uint(7))
		c.Set("role", "user")
		ExportIncrementalFailedDDL(c)
	})
	router.GET("/other/:jobID/export", func(c *gin.Context) {
		c.Set("userID", uint(8))
		c.Set("role", "user")
		ExportIncrementalFailedDDL(c)
	})
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/jobs/export-job-1234/export", nil)
	router.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
	require.Contains(t, recorder.Header().Get("Content-Disposition"), "incremental-export-j-failed-ddl.sql")
	require.Contains(t, recorder.Body.String(), "\nCREATE VIEW bad_view AS SELECT * FROM missing;\n")

	otherRecorder := httptest.NewRecorder()
	router.ServeHTTP(otherRecorder, httptest.NewRequest(http.MethodGet, "/other/export-job-1234/export", nil))
	require.Equal(t, http.StatusNotFound, otherRecorder.Code)
}

func TestExportableBootstrapFailuresSupportsLegacyTableDDL(t *testing.T) {
	review := cdc.BootstrapReview{BootstrapRecord: cdc.BootstrapRecord{ExcludedTables: []cdc.BootstrapIssue{{
		Table: "legacy_table", Stage: "schema", Error: "legacy error", DDL: "CREATE TABLE legacy_table(id int)",
	}}}}
	items := exportableBootstrapFailures(review)
	require.Len(t, items, 1)
	require.Equal(t, "table", items[0].Category)
	require.Equal(t, "CREATE TABLE legacy_table(id int)", items[0].DDL)
}
