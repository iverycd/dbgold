package handler

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestResolveChangeOwner(t *testing.T) {
	for _, tt := range []struct {
		target string
		want   bool
	}{
		{"postgres", true}, {"gaussdb", true}, {"seabox", true},
		{"highgo", true}, {"vastbase", true}, {"gbase", true}, {"kingbase", true},
		{"dameng", false}, {"mysql", false}, {"oscar", false},
		{"sqlserver", false}, {"oracle", false}, {"", false}, {"unknown", false},
	} {
		t.Run(tt.target, func(t *testing.T) {
			require.Equal(t, tt.want, resolveChangeOwner(tt.target, nil))
			for _, value := range []bool{false, true} {
				require.Equal(t, value && tt.target != "oracle", resolveChangeOwner(tt.target, &value))
			}
		})
	}
}

func TestBatchOwnerDefaultsAndOverrides(t *testing.T) {
	for _, tt := range []struct {
		form                   string
		oscar, postgres, nonPG bool
	}{
		{"", false, true, false},
		{"change_owner=true", true, true, true},
		{"change_owner=false", false, false, false},
		{"change_owner=true&oscar_change_owner=false", false, true, true},
		{"change_owner=false&oscar_change_owner=true", true, false, false},
		{"oscar_change_owner=1", true, true, false},
		{"oscar_change_owner=0", false, true, false},
	} {
		t.Run(tt.form, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			form, err := url.ParseQuery(tt.form)
			require.NoError(t, err)
			c.Request = httptest.NewRequest("POST", "/migration/batch/start", strings.NewReader(form.Encode()))
			c.Request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			opts := parseBatchOptions(c)
			require.Equal(t, tt.oscar, opts.changeOwnerFor("oscar"))
			for _, target := range []string{"postgres", "gaussdb", "seabox", "highgo", "vastbase", "gbase", "kingbase"} {
				require.Equal(t, tt.postgres, opts.changeOwnerFor(target), target)
			}
			for _, target := range []string{"dameng", "mysql", "sqlserver", "oracle", "", "unknown"} {
				require.Equal(t, tt.nonPG && target != "oracle" && target != "sqlserver", opts.changeOwnerFor(target), target)
			}
		})
	}
}

func TestMySQLDamengNewRequestTriggerDefaults(t *testing.T) {
	for _, content := range []string{"", "both", "schema_only", "data_only"} {
		for _, option := range []string{"", `,"migrate_triggers":false`, `,"migrate_triggers":true`} {
			var req startDataMigrationRequest
			require.NoError(t, json.Unmarshal([]byte(`{"migrate_content":"`+content+`"`+option+`}`), &req))
			resolved := resolveTriggerMigration("mysql", "dameng", req.MigrateContent, req.MigrateTriggers)
			want := option == `,"migrate_triggers":true` || option == "" && content != "data_only"
			require.Equal(t, want, resolved, content+option)
			if content == "data_only" && option == `,"migrate_triggers":true` {
				require.ErrorContains(t, validateTriggerMigration("mysql", "dameng", content, resolved), "仅迁移数据")
			} else {
				require.NoError(t, validateTriggerMigration("mysql", "dameng", content, resolved))
			}
		}
	}
	for _, pair := range [][2]string{{"mysql", "postgres"}, {"mysql", "oracle"}, {"oracle", "dameng"}} {
		require.False(t, resolveTriggerMigration(pair[0], pair[1], "both", nil))
	}
}

func TestBatchTriggerDefaultsResolvePerPair(t *testing.T) {
	for _, form := range []string{"", "migrate_triggers=false", "migrate_triggers=true"} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/", strings.NewReader(form))
		c.Request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		opts := parseBatchOptions(c)
		require.Equal(t, form == "", opts.MigrateTriggers == nil)
		for _, content := range []string{"both", "schema_only", "data_only"} {
			wantDM := form == "migrate_triggers=true" || form == "" && content != "data_only"
			require.Equal(t, wantDM, resolveTriggerMigration("mysql", "dameng", content, opts.MigrateTriggers))
			require.Equal(t, form == "migrate_triggers=true", resolveTriggerMigration("mysql", "postgres", content, opts.MigrateTriggers))
		}
	}
}
