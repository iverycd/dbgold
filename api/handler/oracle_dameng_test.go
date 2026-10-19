package handler

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"dbgold/datamigrate/typemap"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOracleDamengSupportedPair(t *testing.T) {
	require.True(t, isSupportedPair("oracle", "dameng"))
	_, ok := typemap.Get("oracle", "dameng")
	require.True(t, ok)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	GetSupportedPairs(ctx)
	var pairs []SupportedPair
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pairs))
	count := 0
	for _, p := range pairs {
		if p.Source == "oracle" && p.Target == "dameng" {
			count++
		}
	}
	require.Equal(t, 1, count)
}
