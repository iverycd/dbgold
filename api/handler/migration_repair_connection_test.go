package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"dbgold/datamigrate"
	"dbgold/store"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRepairCapturedDamengConnectionPreservesEmptyDatabase(t *testing.T) {
	j := &store.DataMigrationJob{
		SrcDBType: "dameng", SrcConnHost: "source", SrcConnPort: 5236, SrcConnUsername: "sysdba",
		SrcConnDatabase: "EPOINTWEBBUILDER5_3_942", SourceSchema: "EPOINTWEBBUILDER5_3_942",
		RepairConfigJSON: repairJSON(datamigrate.Config{Content: "both"}),
	}
	c := &store.Connection{DBType: "dameng", Host: "SOURCE", Port: 5236, Username: "sysdba"}
	require.NoError(t, checkRepairConnection(j, c, true))
	c.Database = j.SrcConnDatabase
	err := checkRepairConnection(j, c, true)
	require.Error(t, err)
	require.Contains(t, err.Error(), "源连接")
	require.Contains(t, err.Error(), "数据库")

	j.SourceConnectionDatabase = "default_owner"
	c.Database = "default_owner"
	require.NoError(t, checkRepairConnection(j, c, true))
	c.Database = ""
	require.Error(t, checkRepairConnection(j, c, true))
}

func TestRepairConnectionMismatchNamesRoleAndChangedFieldsInOrder(t *testing.T) {
	j := &store.DataMigrationJob{
		SrcDBType: "dameng", SrcConnHost: "source", SrcConnPort: 5236, SrcConnUsername: "source_user",
		SourceConnectionDatabase: "source_db", SrcConnDatabase: "source_owner", RepairConfigJSON: "{}",
		DstDBType: "vastbase", DstConnHost: "target", DstConnPort: 5432, DstConnDatabase: "target_db", DstConnUsername: "target_user",
	}
	fields := []struct {
		name   string
		change func(*store.Connection)
		value  func(*store.Connection) any
	}{
		{"数据库类型", func(c *store.Connection) { c.DBType = "mysql" }, func(c *store.Connection) any { return c.DBType }},
		{"主机", func(c *store.Connection) { c.Host = "other-host" }, func(c *store.Connection) any { return c.Host }},
		{"端口", func(c *store.Connection) { c.Port++ }, func(c *store.Connection) any { return c.Port }},
		{"数据库", func(c *store.Connection) { c.Database = "other-db" }, func(c *store.Connection) any { return c.Database }},
		{"账号", func(c *store.Connection) { c.Username = "other-user" }, func(c *store.Connection) any { return c.Username }},
	}
	for _, src := range []bool{true, false} {
		role := "目标连接"
		original := store.Connection{DBType: j.DstDBType, Host: j.DstConnHost, Port: j.DstConnPort, Database: j.DstConnDatabase, Username: j.DstConnUsername}
		if src {
			role = "源连接"
			original = store.Connection{DBType: j.SrcDBType, Host: j.SrcConnHost, Port: j.SrcConnPort, Database: j.SourceConnectionDatabase, Username: j.SrcConnUsername}
		}
		original.Password = "original-secret-password"
		t.Run(role, func(t *testing.T) {
			for _, field := range fields {
				t.Run(field.name, func(t *testing.T) {
					changed := original
					changed.Password = "current-secret-password"
					field.change(&changed)
					err := checkRepairConnection(j, &changed, src)
					require.Error(t, err)
					require.Contains(t, err.Error(), role)
					require.Contains(t, err.Error(), field.name)
					require.Contains(t, err.Error(), fmt.Sprint(field.value(&original)))
					require.Contains(t, err.Error(), fmt.Sprint(field.value(&changed)))
					require.NotContains(t, err.Error(), original.Password)
					require.NotContains(t, err.Error(), changed.Password)
				})
			}
			changed := original
			for _, field := range fields {
				field.change(&changed)
			}
			err := checkRepairConnection(j, &changed, src)
			require.Error(t, err)
			require.Contains(t, err.Error(), role)
			require.Regexp(t, "数据库类型.*主机.*端口.*数据库.*账号", err.Error())
			original.Name, original.Password = "renamed", "rotated-password"
			original.Host = strings.ToUpper(original.Host)
			require.NoError(t, checkRepairConnection(j, &original, src))
		})
	}
}

func TestRepairSourceSchemaFillPreservesFrozenScope(t *testing.T) {
	for _, tc := range []struct {
		name string
		job  store.DataMigrationJob
		b    store.MigrationRepairBinding
		want string
	}{
		{"captured schema", store.DataMigrationJob{SrcDBType: "dameng", SourceSchema: "APP", SrcConnDatabase: "wrong", RepairConfigJSON: "{}"}, store.MigrationRepairBinding{}, "APP"},
		{"legacy dameng owner", store.DataMigrationJob{SrcDBType: "dameng", SrcConnDatabase: "app"}, store.MigrationRepairBinding{Legacy: true}, "APP"},
		{"frozen schema precedes legacy owner", store.DataMigrationJob{SrcDBType: "dameng", SourceSchema: "FROZEN", SrcConnDatabase: "wrong"}, store.MigrationRepairBinding{Legacy: true}, "FROZEN"},
		{"existing schema remains fixed", store.DataMigrationJob{SrcDBType: "dameng", SourceSchema: "APP", SrcConnDatabase: "wrong"}, store.MigrationRepairBinding{Legacy: true, SourceSchema: "FIXED"}, "FIXED"},
		{"captured empty schema cannot use old field", store.DataMigrationJob{SrcDBType: "dameng", SrcConnDatabase: "app", RepairConfigJSON: "{}"}, store.MigrationRepairBinding{}, ""},
		{"oracle legacy remains explicit", store.DataMigrationJob{SrcDBType: "oracle", SrcConnDatabase: "app"}, store.MigrationRepairBinding{Legacy: true}, ""},
		{"mysql legacy has no owner", store.DataMigrationJob{SrcDBType: "mysql", SrcConnDatabase: "app"}, store.MigrationRepairBinding{Legacy: true}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fillRepairSourceSchema(&tc.job, &tc.b)
			require.Equal(t, tc.want, tc.b.SourceSchema)
		})
	}
}

func TestRepairInitialBindingRestoresDamengOwner(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		j := &store.DataMigrationJob{SrcDBType: "dameng", SrcConnDatabase: "app"}
		if !legacy {
			j.SourceSchema, j.RepairConfigJSON = "APP", "{}"
		}
		b := initialRepairBinding(j, "{}", legacy)
		require.Equal(t, "APP", b.SourceSchema)
	}
}

func TestRepairDamengExistingBindingRestoresSchemaAndResolvesOriginalConnections(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "captured"
		if legacy {
			name = "legacy"
		}
		t.Run(name, func(t *testing.T) {
			seedHandlerRepair(t)
			src := &store.Connection{OwnerID: 1, DBType: "dameng", Host: "source", Port: 5236, Username: "sysdba"}
			dst := &store.Connection{OwnerID: 1, DBType: "vastbase", Host: "target", Port: 5432, Database: "prod", Username: "admin"}
			require.NoError(t, store.CreateConnection(src))
			require.NoError(t, store.CreateConnection(dst))
			j := &store.DataMigrationJob{
				JobID: "job", OwnerID: 1, Status: "failed", SrcConnID: src.ID, DstConnID: dst.ID,
				SrcDBType: src.DBType, SrcConnHost: src.Host, SrcConnPort: src.Port, SrcConnDatabase: "app", SrcConnUsername: src.Username,
				DstDBType: dst.DBType, DstConnHost: dst.Host, DstConnPort: dst.Port, DstConnDatabase: dst.Database, DstConnUsername: dst.Username, DstSchema: "app",
			}
			if !legacy {
				j.SourceSchema, j.RepairConfigJSON = "APP", "{}"
			}
			existingJob, err := store.GetDataMigrationJob(j.JobID)
			require.NoError(t, err)
			j.ID, j.CreatedAt = existingJob.ID, existingJob.CreatedAt
			require.NoError(t, store.DB.Model(&store.DataMigrationJob{}).Where("job_id = ?", j.JobID).Select("*").Updates(j).Error)
			binding := &store.MigrationRepairBinding{JobID: "job", SrcConnID: src.ID, DstConnID: dst.ID, TargetSchema: "app", Legacy: legacy, Configured: true, ConfigJSON: repairJSON(datamigrate.Config{Content: "both"}), Version: 1}
			require.NoError(t, store.DB.Model(&store.MigrationRepairBinding{}).Where("job_id = ?", j.JobID).Select("*").Updates(binding).Error)

			w := repairRequest(t, http.MethodGet, "/jobs/job/repairs/table", "", 1, ListMigrationRepairs)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var listed struct {
				Binding struct {
					SourceSchema string `json:"source_schema"`
				} `json:"binding"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &listed))
			require.Equal(t, "APP", listed.Binding.SourceSchema)
			w = repairRequest(t, http.MethodGet, "/jobs/job/repairs/table", "", 1, func(c *gin.Context) {
				_, b, _, _, ok := loadRepairObject(c, false)
				if ok {
					c.JSON(http.StatusOK, gin.H{"source_schema": b.SourceSchema})
				}
			})
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.JSONEq(t, `{"source_schema":"APP"}`, w.Body.String())
			unchanged, err := store.GetMigrationRepairBinding(j.JobID)
			require.NoError(t, err)
			require.Empty(t, unchanged.SourceSchema)
			require.Equal(t, 1, unchanged.Version)

			body := repairJSON(map[string]any{"src_conn_id": src.ID, "dst_conn_id": dst.ID, "source_schema": "OTHER"})
			w = repairRequest(t, http.MethodPut, "/jobs/job/repairs/binding", body, 1, UpdateMigrationRepairBinding)
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			require.Contains(t, w.Body.String(), "不能变更原任务源")
			unchanged, err = store.GetMigrationRepairBinding(j.JobID)
			require.NoError(t, err)
			require.Equal(t, 1, unchanged.Version)
			require.Empty(t, unchanged.SourceSchema)

			body = repairJSON(map[string]any{"src_conn_id": src.ID, "dst_conn_id": dst.ID})
			w = repairRequest(t, http.MethodPut, "/jobs/job/repairs/binding", body, 1, UpdateMigrationRepairBinding)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			saved, err := store.GetMigrationRepairBinding(j.JobID)
			require.NoError(t, err)
			require.Equal(t, "APP", saved.SourceSchema)
			require.Equal(t, "APP", sourceOverride(saved, "dameng"))
			require.Equal(t, 2, saved.Version)
			require.Equal(t, binding.ConfigJSON, saved.ConfigJSON)
			require.Equal(t, connectionFingerprint(src), saved.SourceFingerprint)
			require.Equal(t, connectionFingerprint(dst), saved.TargetFingerprint)
			c, _ := gin.CreateTestContext(nil)
			c.Set("userID", uint(1))
			c.Set("role", "user")
			resolvedSrc, resolvedDst, err := resolveRepairConnections(c, j, saved, true)
			require.NoError(t, err)
			require.Equal(t, src.ID, resolvedSrc.ID)
			require.Equal(t, dst.ID, resolvedDst.ID)
		})
	}
}

func TestRepairLegacyDamengReportSeedsFrozenOwnerAndBindsWithEmptyDatabase(t *testing.T) {
	seedHandlerRepair(t)
	src := &store.Connection{OwnerID: 1, DBType: "dameng", Host: "source", Port: 5236, Username: "sysdba"}
	dst := &store.Connection{OwnerID: 1, DBType: "vastbase", Host: "target", Port: 5432, Database: "prod", Username: "admin"}
	require.NoError(t, store.CreateConnection(src))
	require.NoError(t, store.CreateConnection(dst))
	j, err := store.GetDataMigrationJob("job")
	require.NoError(t, err)
	j.SrcConnID, j.DstConnID = src.ID, dst.ID
	j.SrcDBType, j.SrcConnHost, j.SrcConnPort, j.SrcConnDatabase, j.SrcConnUsername = src.DBType, src.Host, src.Port, "app", src.Username
	j.DstDBType, j.DstConnHost, j.DstConnPort, j.DstConnDatabase, j.DstConnUsername = dst.DBType, dst.Host, dst.Port, dst.Database, dst.Username
	require.NoError(t, store.UpdateDataMigrationJob(j))
	require.NoError(t, store.DB.Delete(&store.MigrationRepairBinding{}, "job_id = ?", j.JobID).Error)
	report := datamigrate.MigrationReport{Tables: datamigrate.CategoryReport{Items: []datamigrate.ObjectResult{{Name: "Orders", Error: "legacy DDL failure"}}}}
	require.NoError(t, store.CreateDataMigrationReport(&store.DataMigrationReport{JobID: j.JobID, ReportJSON: repairJSON(report)}))
	require.NoError(t, ensureRepairLedger(j))
	b, err := store.GetMigrationRepairBinding(j.JobID)
	require.NoError(t, err)
	require.True(t, b.Legacy)
	require.False(t, b.Configured)
	require.Empty(t, b.SourceDatabase)
	require.Equal(t, "APP", b.SourceSchema)

	body := repairJSON(map[string]any{
		"src_conn_id": src.ID, "dst_conn_id": dst.ID, "source_database": "", "source_schema": "OTHER",
		"config": datamigrate.Config{Content: "both", TargetSchema: "app"},
	})
	w := repairRequest(t, http.MethodPut, "/jobs/job/repairs/binding", body, 1, UpdateMigrationRepairBinding)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "不能变更原任务源")
	body = repairJSON(map[string]any{
		"src_conn_id": src.ID, "dst_conn_id": dst.ID, "source_database": "", "source_schema": "APP",
		"config": datamigrate.Config{Content: "both", TargetSchema: "app"},
	})
	w = repairRequest(t, http.MethodPut, "/jobs/job/repairs/binding", body, 1, UpdateMigrationRepairBinding)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	saved, err := store.GetMigrationRepairBinding(j.JobID)
	require.NoError(t, err)
	require.True(t, saved.Configured)
	require.Empty(t, saved.SourceDatabase)
	require.Equal(t, "APP", saved.SourceSchema)
	require.Equal(t, "APP", sourceOverride(saved, "dameng"))
	require.Equal(t, 2, saved.Version)
}

func TestRepairLegacySourceDatabaseFallbackRemainsScopedByDatabaseType(t *testing.T) {
	for _, typ := range []string{"mysql", "sqlserver", "oracle", "dameng", "postgres", "gaussdb", "highgo", "vastbase", "gbase", "seabox", "kingbase"} {
		t.Run(typ, func(t *testing.T) {
			j := &store.DataMigrationJob{SrcDBType: typ, SrcConnDatabase: "original"}
			c := &store.Connection{DBType: typ, Database: "original"}
			require.NoError(t, checkRepairConnection(j, c, true))
			c.Database = "other"
			if typ == "mysql" || typ == "sqlserver" || typ == "oracle" {
				require.Error(t, checkRepairConnection(j, c, true))
			} else {
				require.NoError(t, checkRepairConnection(j, c, true))
			}
			j.SourceConnectionDatabase = "captured"
			c.Database = "captured"
			require.NoError(t, checkRepairConnection(j, c, true))
			c.Database = "other"
			require.Error(t, checkRepairConnection(j, c, true))
		})
	}
}
