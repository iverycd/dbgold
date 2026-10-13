package handler

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dbgold/config"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/store"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func repairRequest(t *testing.T, method, path, body string, owner uint, fn gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Handle(method, "/jobs/:jobID/repairs/:objectID", func(c *gin.Context) { c.Set("userID", owner); c.Set("role", "user"); fn(c) })
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}
func seedHandlerRepair(t *testing.T) {
	t.Helper()
	require.NoError(t, store.InitWithError(&config.Config{SQLitePath: ":memory:"}))
	require.NoError(t, store.CreateDataMigrationJob(&store.DataMigrationJob{JobID: "job", OwnerID: 1, Status: "failed", DstSchema: "app"}))
	obj := repair.Object{ID: "table", Kind: "table", TargetName: "Orders", Table: "Orders", Schema: "app", Status: "failed", RequiresReview: true, Units: []repair.Unit{{ID: "one", SQL: `CREATE TABLE "app"."Orders" (id integer)`, OriginalSQL: `CREATE TABLE "app"."Orders" (id integer)`, Status: "failed", Selected: true}}}
	require.NoError(t, store.SeedMigrationRepairs(&store.MigrationRepairBinding{JobID: "job", Version: 1}, []store.MigrationRepairObject{{JobID: "job", ObjectID: "table", Version: 1, ObjectJSON: repairJSON(obj)}}))
}
func TestRepairDraftOwnershipVersionAndStatusCannotBeForged(t *testing.T) {
	seedHandlerRepair(t)
	body := `{"version":1,"reviewed":true,"units":[{"id":"one","sql":"CREATE TABLE \"app\".\"Orders\" (id bigint)","status":"succeeded","selected":true}]}`
	require.Equal(t, 404, repairRequest(t, http.MethodPut, "/jobs/job/repairs/table", body, 2, SaveMigrationRepairDraft).Code)
	require.Equal(t, 200, repairRequest(t, http.MethodPut, "/jobs/job/repairs/table", body, 1, SaveMigrationRepairDraft).Code)
	row, e := store.GetMigrationRepairObject("job", "table")
	require.NoError(t, e)
	require.Equal(t, 2, row.Version)
	var obj repair.Object
	require.NoError(t, json.Unmarshal([]byte(row.ObjectJSON), &obj))
	require.Equal(t, "failed", obj.Units[0].Status)
	require.False(t, obj.RequiresReview)
	require.Contains(t, obj.Units[0].OriginalSQL, "integer")
	require.Equal(t, 409, repairRequest(t, http.MethodPut, "/jobs/job/repairs/table", body, 1, SaveMigrationRepairDraft).Code)
}
func TestRepairExecutionReplayDoesNotReconnect(t *testing.T) {
	seedHandlerRepair(t)
	hash := repair.Hash([]any{"ddl", "table", 1, true})
	require.NoError(t, store.DB.Create(&store.MigrationRepairAttempt{ID: "existing", JobID: "job", ObjectID: "table", RequestID: "same", RequestHash: hash, Status: "succeeded"}).Error)
	w := repairRequest(t, http.MethodPost, "/jobs/job/repairs/table", `{"version":1,"request_id":"same","confirmed":true}`, 1, ExecuteMigrationRepair)
	require.Equal(t, 202, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "existing")
}
func TestRepairConnectionIdentityIsFixedButPasswordCanRotate(t *testing.T) {
	j := &store.DataMigrationJob{DstDBType: "postgres", DstConnHost: "db", DstConnPort: 5432, DstConnDatabase: "prod", DstConnUsername: "migrator"}
	c := &store.Connection{DBType: "postgres", Host: "DB", Port: 5432, Database: "prod", Username: "migrator", Password: "new-password"}
	require.NoError(t, checkRepairConnection(j, c, false))
	before := connectionFingerprint(c)
	c.Password = "rotated"
	require.Equal(t, before, connectionFingerprint(c))
	c.Database = "other"
	require.Error(t, checkRepairConnection(j, c, false))
	c.Database = "prod"
	c.Username = "other"
	require.Error(t, checkRepairConnection(j, c, false))
}
func TestRepairSQLPreservesUnitsAndRejectsUnrelatedTargets(t *testing.T) {
	obj := repair.Object{Kind: "table", TargetName: "Orders", Table: "Orders", Schema: "app"}
	for _, sql := range []string{`CREATE TABLE "other"."Orders" (id int)`, `CREATE TABLE "app"."Wrong" (id int)`, `CREATE TABLE "app"."Orders"(id int); DELETE FROM secrets;`, `CREATE USER someone`} {
		u := []repair.Unit{{SQL: sql, Selected: true}}
		require.Error(t, validateRepairObjectScope(obj, u, "postgres"), sql)
	}
	require.NoError(t, validateRepairObjectScope(obj, []repair.Unit{{SQL: `CREATE TABLE "app"."Orders"(note text DEFAULT ';')`, Selected: true}}, "postgres"))
	native := []repair.Unit{{SQL: "DECLARE n NUMBER; BEGIN SELECT 1 INTO n FROM DUAL; END;", OriginalSQL: "DECLARE n NUMBER; BEGIN SELECT 1 INTO n FROM DUAL; END;", Selected: true}}
	require.NoError(t, validateRepairUnits(native))
	require.NoError(t, validateRepairObjectScope(obj, native, "oracle"))
	require.Error(t, validateRepairUnits([]repair.Unit{{SQL: "DROP /*guard*/ TABLE x", Selected: true}}))
}

func TestRepairBlocksCannotHideUnrelatedDynamicSQL(t *testing.T) {
	obj := repair.Object{Kind: "sequence", TargetName: "seq", Table: "Orders", Schema: "app"}
	for _, sql := range []string{
		`BEGIN EXECUTE IMMEDIATE 'DROP TABLE other_schema.victim'; END;`,
		`BEGIN EXECUTE IMMEDIATE 'CREATE SEQUENCE other_schema.seq'; END;`,
		`BEGIN EXECUTE IMMEDIATE 'CREATE SEQUENCE app.seq' || '; DROP TABLE victim'; END;`,
		`BEGIN EXECUTE IMMEDIATE dangerous_variable; END;`,
		`EXEC('TRUNCATE TABLE dbo.other')`,
		`CREATE SEQUENCE app.seq /*!50000 ; DROP TABLE victim */`,
	} {
		require.Error(t, validateRepairObjectScope(obj, []repair.Unit{{SQL: sql, Selected: true}}, "oracle"), sql)
	}
	require.Error(t, validateRepairObjectScope(obj, []repair.Unit{{SQL: `EXEC('TRUNCATE TABLE dbo.other')`, Selected: true}}, "sqlserver"))
	require.NoError(t, validateRepairObjectScope(obj, []repair.Unit{{SQL: `BEGIN EXECUTE IMMEDIATE 'CREATE SEQUENCE app.seq START WITH 20'; END;`, Selected: true}}, "oracle"))
	original := `DECLARE n NUMBER := 1; BEGIN EXECUTE IMMEDIATE 'CREATE SEQUENCE app.seq START WITH ' || TO_CHAR(n); END;`
	unit := repair.Unit{SQL: original, OriginalSQL: original, Selected: true}
	require.NoError(t, validateRepairObjectScope(obj, []repair.Unit{unit}, "oracle"))
	unit.SQL = strings.Replace(original, ":= 1", ":= 42", 1)
	require.NoError(t, validateRepairObjectScope(obj, []repair.Unit{unit}, "oracle"))
	unit.SQL = strings.Replace(original, "CREATE SEQUENCE app.seq START WITH ", "DROP TABLE other_schema.victim", 1)
	require.Error(t, validateRepairObjectScope(obj, []repair.Unit{unit}, "oracle"))
}

func TestRepairCannotRenameOrMoveTable(t *testing.T) {
	obj := repair.Object{Kind: "table", TargetName: "Orders", Table: "Orders", Schema: "app"}
	for _, sql := range []string{`ALTER TABLE app.Orders RENAME TO stolen`, `ALTER TABLE app.Orders RENAME TO other_schema.stolen`, `ALTER TABLE app.Orders SET SCHEMA archive`} {
		require.Error(t, validateRepairObjectScope(obj, []repair.Unit{{SQL: sql, Selected: true}}, "postgres"))
	}
	require.NoError(t, validateRepairObjectScope(obj, []repair.Unit{{SQL: `ALTER TABLE app.Orders RENAME COLUMN old_name TO new_name`, Selected: true}}, "postgres"))
}

func TestRepairEffectiveSourceNamespacesAreFixed(t *testing.T) {
	j := &store.DataMigrationJob{}
	migrationSourceLocation(j, &store.Connection{DBType: "postgres", Database: "prod"}, "")
	require.Equal(t, "public", j.SourceSchema)
	require.Equal(t, "prod", j.SourceDatabase)
	migrationSourceLocation(j, &store.Connection{DBType: "postgres", Database: "prod"}, "Sales")
	require.Equal(t, "Sales", j.SourceSchema)
	migrationSourceLocation(j, &store.Connection{DBType: "oracle", Database: "service"}, "app")
	require.Equal(t, "APP", j.SourceSchema)
	j.DstDBType = "dameng"
	migrationSourceLocation(j, &store.Connection{DBType: "sqlserver", Database: "prod"}, "other")
	require.Equal(t, "dbo", j.SourceSchema)
	require.Equal(t, "other", j.SourceDatabase)
}

func TestRepairBindingCannotFillNewTaskWithDifferentSourceSchema(t *testing.T) {
	seedHandlerRepair(t)
	src := &store.Connection{OwnerID: 1, DBType: "postgres", Database: "prod"}
	dst := &store.Connection{OwnerID: 1, DBType: "mysql", Database: "target"}
	require.NoError(t, store.CreateConnection(src))
	require.NoError(t, store.CreateConnection(dst))
	require.NoError(t, store.DB.Model(&store.DataMigrationJob{}).Where("job_id = ?", "job").Updates(map[string]any{"src_db_type": "postgres", "dst_db_type": "mysql", "source_connection_database": "prod"}).Error)
	body := repairJSON(map[string]any{"src_conn_id": src.ID, "dst_conn_id": dst.ID, "source_schema": "archive"})
	w := repairRequest(t, http.MethodPut, "/jobs/job/repairs/binding", body, 1, UpdateMigrationRepairBinding)
	require.Equal(t, 400, w.Code)
	require.Contains(t, w.Body.String(), "不能变更原任务源")
	b, err := store.GetMigrationRepairBinding("job")
	require.NoError(t, err)
	require.Empty(t, b.SourceSchema)
}

func TestRepairScopeCanonicalizesAliasesAndLoopback(t *testing.T) {
	ip := net.IPAddr{IP: net.ParseIP("192.0.2.8")}
	require.Equal(t, canonicalMigrationHost("primary", []net.IPAddr{ip}), canonicalMigrationHost("alias", []net.IPAddr{ip}))
	for _, host := range []string{"localhost", "127.0.0.1", "::1", "[::1]"} {
		require.Equal(t, "loopback", canonicalMigrationHost(host, nil))
	}
	a := &store.Connection{DBType: "postgres", Host: "127.0.0.1", Port: 5432, Database: "prod", Username: "first"}
	b := &store.Connection{DBType: "highgo", Host: "::1", Port: 5432, Database: "prod", Username: "second"}
	require.Equal(t, migrationScope(a, "public"), migrationScope(b, "public"))
}

func TestRecoveryUnknownRelatedIsNeverReplayedAndCalibrationIsScoped(t *testing.T) {
	for _, status := range []string{"unknown", "running", "unverified"} {
		require.Error(t, recoveryRelatedReady(repair.Object{Status: status}))
	}
	require.Error(t, recoveryRelatedReady(repair.Object{Status: "failed", Units: []repair.Unit{{Status: "unknown"}}}))
	require.NoError(t, recoveryRelatedReady(repair.Object{Status: "draft", Units: []repair.Unit{{Status: "succeeded"}, {Status: "pending", Selected: true}}}))
	seqs := []source.SequenceInfo{{ColumnName: "a"}, {ColumnName: "b"}}
	require.Empty(t, recoveryCalibrationScope(seqs, nil))
	require.Equal(t, seqs[1:], recoveryCalibrationScope(seqs, map[string]bool{"b": true}))
}

func TestRecoveryObserverPersistsExecutionBoundary(t *testing.T) {
	seedHandlerRepair(t)
	a := &store.MigrationRepairAttempt{ID: "recover", JobID: "job", Status: "running", UnitsJSON: "[]"}
	require.NoError(t, store.DB.Create(a).Error)
	units := []repair.Unit{}
	observe := recoveryAttemptObserver(a, &units)
	require.NoError(t, observe(repair.Event{SQL: "TRUNCATE TABLE app.Orders", Status: "running"}))
	var row store.MigrationRepairAttempt
	require.NoError(t, store.DB.First(&row, "id = ?", a.ID).Error)
	require.Contains(t, row.UnitsJSON, `"status":"running"`)
	require.NoError(t, observe(repair.Event{SQL: "TRUNCATE TABLE app.Orders", Status: "succeeded"}))
	require.Len(t, units, 1)
	require.Equal(t, "succeeded", units[0].Status)
	require.Equal(t, units[0].SQL, units[0].OriginalSQL)
}

func TestRepairSQLServerIndexUsesQualifiedTableNotQualifiedIndex(t *testing.T) {
	obj := repair.Object{Kind: "index", TargetName: "idx", Table: "Orders", Schema: "dbo"}
	u := []repair.Unit{{SQL: "CREATE INDEX [idx] ON [dbo].[Orders] ([id])", Selected: true}}
	require.NoError(t, validateRepairObjectScope(obj, u, "sqlserver"))
	u[0].SQL = "CREATE INDEX [idx] ON [other].[Orders] ([id])"
	require.Error(t, validateRepairObjectScope(obj, u, "sqlserver"))
}
func TestRepairConditionalDDLDoesNotImplyEffect(t *testing.T) {
	require.True(t, conditionalRepairEffect([]repair.Unit{{Selected: true, SQL: `CREATE TABLE IF NOT EXISTS "app"."Orders"(id int)`}}))
	require.False(t, conditionalRepairEffect([]repair.Unit{{Selected: true, SQL: `CREATE TABLE "app"."Orders"(id int)`}}))
	require.False(t, conditionalRepairEffect([]repair.Unit{{Selected: false, Status: "succeeded", SQL: `CREATE TABLE IF NOT EXISTS "app"."Orders"(id int)`}}))
}
func TestRecoveryDataSuccessSurvivesPostDDLFailure(t *testing.T) {
	seedHandlerRepair(t)
	row, e := store.GetMigrationRepairObject("job", "table")
	require.NoError(t, e)
	var obj repair.Object
	require.NoError(t, json.Unmarshal([]byte(row.ObjectJSON), &obj))
	obj.Status = "succeeded"
	obj.Table = "Orders"
	obj.SourceTable = "Orders"
	obj.DataStatus = "succeeded"
	obj.ColumnMapping = map[string]string{"Id": "ID"}
	other := repair.Object{ID: "data", Kind: "data", Table: "Orders", SourceTable: "Orders", Status: "failed", DataStatus: "failed"}
	require.NoError(t, store.DB.Create(&store.MigrationRepairObject{JobID: "job", ObjectID: "data", ObjectJSON: repairJSON(other), Version: 1}).Error)
	require.NoError(t, persistRecoveryData("job", obj))
	row, e = store.GetMigrationRepairObject("job", "data")
	require.NoError(t, e)
	require.NoError(t, json.Unmarshal([]byte(row.ObjectJSON), &other))
	require.Equal(t, "succeeded", other.Status)
	require.Equal(t, "succeeded", other.DataStatus)
	require.Equal(t, obj.ColumnMapping, other.ColumnMapping)
}
func TestRecoveryOrdersDependentViews(t *testing.T) {
	out := orderedRecoveryObjects([]repair.Object{{ID: "dependent", Dependencies: []string{"base"}}, {ID: "base"}})
	require.Equal(t, "base", out[0].ID)
	require.Equal(t, "dependent", out[1].ID)
}
