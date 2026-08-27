package handler

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestResolveChangeOwner(t *testing.T) {
	for _, target := range []string{"oscar", "postgres", "dameng", "gaussdb"} {
		require.Equal(t, target != "oscar", resolveChangeOwner(target, nil))
		for _, value := range []bool{false, true} {
			require.Equal(t, value, resolveChangeOwner(target, &value))
		}
	}
}

func TestBatchOwnerDefaultsAndOverrides(t *testing.T) {
	for _, tt := range []struct {
		form            string
		oscar, postgres bool
	}{
		{"", false, true},
		{"change_owner=true", true, true},
		{"change_owner=false", false, false},
		{"change_owner=true&oscar_change_owner=false", false, true},
		{"change_owner=false&oscar_change_owner=true", true, false},
		{"oscar_change_owner=1", true, true},
		{"oscar_change_owner=0", false, true},
	} {
		t.Run(tt.form, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			form, err := url.ParseQuery(tt.form)
			require.NoError(t, err)
			c.Request = httptest.NewRequest("POST", "/migration/batch/start", strings.NewReader(form.Encode()))
			c.Request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			opts := parseBatchOptions(c)
			require.Equal(t, tt.oscar, opts.changeOwnerFor("oscar"))
			require.Equal(t, tt.postgres, opts.changeOwnerFor("postgres"))
		})
	}
}
