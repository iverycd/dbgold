package handler

import (
	"dbgold/datamigrate/typemap"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"net/http/httptest"
	"testing"
)

func TestSQLServerDamengSupportedPair(t *testing.T) {
	require.True(t, isSupportedPair("sqlserver", "dameng"))
	_, ok := typemap.Get("sqlserver", "dameng")
	require.True(t, ok)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	GetSupportedPairs(ctx)
	var pairs []SupportedPair
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pairs))
	count := 0
	for _, p := range pairs {
		if p.Source == "sqlserver" && p.Target == "dameng" {
			count++
		}
	}
	require.Equal(t, 1, count)
}
