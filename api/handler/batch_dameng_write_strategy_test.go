package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"dbgold/store"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/xuri/excelize/v2"
)

func TestBatchDamengWriteStrategy(t *testing.T) {
	migrationHandlerTestDB(t)
	require.NoError(t, store.DB.AutoMigrate(&store.BatchMigration{}))
	f := excelize.NewFile()
	defer f.Close()
	require.NoError(t, f.SetSheetRow("Sheet1", "A1", &batchHeaders))
	for i, dst := range []string{"dameng", "gaussdb"} {
		row := []string{"mysql", "127.0.0.1", "1", "source", "reader", "", dst, "127.0.0.1", "1", "target", "writer", "", "APP"}
		require.NoError(t, f.SetSheetRow("Sheet1", fmt.Sprintf("A%d", i+2), &row))
	}
	excel, err := f.WriteToBuffer()
	require.NoError(t, err)
	for _, tc := range []struct {
		name, content, option, want string
		invalid                     bool
	}{
		{"default", "", "", "batch", false},
		{"explicit row", "both", "row", "row", false},
		{"explicit batch", "data_only", "batch", "batch", false},
		{"schema only default", "schema_only", "", "", false},
		{"schema only override ignored", "schema_only", "batch", "", false},
		{"invalid value", "both", "fast", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var request bytes.Buffer
			form := multipart.NewWriter(&request)
			part, err := form.CreateFormFile("file", "batch.xlsx")
			require.NoError(t, err)
			_, err = part.Write(excel.Bytes())
			require.NoError(t, err)
			if tc.content != "" {
				require.NoError(t, form.WriteField("migrate_content", tc.content))
			}
			if tc.option != "" {
				require.NoError(t, form.WriteField("dameng_write_strategy", tc.option))
			}
			require.NoError(t, form.Close())
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest("POST", "/migration/batch/start", &request)
			c.Request.Header.Set("Content-Type", form.FormDataContentType())
			c.Set("userID", uint(7))
			if tc.invalid {
				var batchCount, jobCount int64
				require.NoError(t, store.DB.Model(&store.BatchMigration{}).Count(&batchCount).Error)
				require.NoError(t, store.DB.Model(&store.DataMigrationJob{}).Count(&jobCount).Error)
				StartBatch(c)
				require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
				require.Contains(t, rec.Body.String(), "dameng_write_strategy")
				var now int64
				require.NoError(t, store.DB.Model(&store.BatchMigration{}).Count(&now).Error)
				require.Equal(t, batchCount, now)
				require.NoError(t, store.DB.Model(&store.DataMigrationJob{}).Count(&now).Error)
				require.Equal(t, jobCount, now)
				return
			}
			StartBatch(c)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var response struct {
				BatchID string `json:"batch_id"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
			require.NotEmpty(t, response.BatchID)
			require.Eventually(t, func() bool {
				b, err := store.GetBatchMigration(response.BatchID)
				return err == nil && b.Status == "done"
			}, 3*time.Second, 5*time.Millisecond)
			jobs, err := store.ListJobsByBatch(response.BatchID)
			require.NoError(t, err)
			require.Len(t, jobs, 2)
			// The closed source port proves both choices were persisted before any connection succeeded.
			require.Equal(t, "failed", jobs[0].Status)
			require.Equal(t, "dameng", jobs[0].DstDBType)
			require.Equal(t, tc.want, jobs[0].DamengWriteStrategy)
			require.Equal(t, "gaussdb", jobs[1].DstDBType)
			require.Empty(t, jobs[1].DamengWriteStrategy)
		})
	}
}
