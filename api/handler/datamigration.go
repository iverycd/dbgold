// api/handler/datamigration.go
package handler

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"dbgold/datamigrate"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/store"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// SupportedPair 表示一个支持的迁移组合
type SupportedPair struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

// supportedPairs 列出后端已实现的迁移组合，新增实现时在此追加
var supportedPairs = []SupportedPair{
	{Source: "mysql", Target: "postgres"},
}

// GetSupportedPairs 返回支持的迁移组合列表
func GetSupportedPairs(c *gin.Context) {
	c.JSON(http.StatusOK, supportedPairs)
}

type startDataMigrationRequest struct {
	SrcConnID   uint   `json:"src_conn_id" binding:"required"`
	DstConnID   uint   `json:"dst_conn_id" binding:"required"`
	MigrateMode string `json:"migrate_mode" binding:"required,oneof=all include exclude"`
	TableFilter string `json:"table_filter"`
	PageSize    int    `json:"page_size"`
	MaxParallel int    `json:"max_parallel"`
}

// StartDataMigration 创建并启动迁移任务，立即返回 jobID
func StartDataMigration(c *gin.Context) {
	var req startDataMigrationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.PageSize <= 0 {
		req.PageSize = 10000
	}
	if req.MaxParallel <= 0 {
		req.MaxParallel = 5
	}

	srcConn, err := store.GetConnection(req.SrcConnID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "源库连接不存在"})
		return
	}
	dstConn, err := store.GetConnection(req.DstConnID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "目标库连接不存在"})
		return
	}

	// 校验迁移组合是否支持
	supported := false
	for _, p := range supportedPairs {
		if p.Source == srcConn.DBType && p.Target == dstConn.DBType {
			supported = true
			break
		}
	}
	if !supported {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("不支持 %s → %s 的数据迁移", srcConn.DBType, dstConn.DBType),
		})
		return
	}

	jobID := uuid.New().String()
	ctx, cancel := context.WithCancel(context.Background())
	job := datamigrate.Registry.Register(jobID, cancel)

	// 持久化任务记录
	dbJob := &store.DataMigrationJob{
		JobID:       jobID,
		SrcConnID:   req.SrcConnID,
		DstConnID:   req.DstConnID,
		SrcDBType:   srcConn.DBType,
		DstDBType:   dstConn.DBType,
		MigrateMode: req.MigrateMode,
		TableFilter: req.TableFilter,
		PageSize:    req.PageSize,
		MaxParallel: req.MaxParallel,
		Status:      "running",
	}
	if err := store.CreateDataMigrationJob(dbJob); err != nil {
		cancel()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建任务记录失败"})
		return
	}

	srcDSN := buildDSN(srcConn)
	dstDSN := buildDSN(dstConn)

	go func() {
		defer func() {
			close(job.LogCh)
			datamigrate.Registry.Remove(jobID)
		}()

		reader, err := source.NewMySQL(srcDSN, srcConn.Database)
		if err != nil {
			job.LogCh <- fmt.Sprintf("[ERROR] 连接源库失败: %v", err)
			updateJobStatus(dbJob, "failed", fmt.Sprintf("连接源库失败: %v", err))
			return
		}
		defer reader.Close()

		writer, err := target.NewPostgres(dstDSN)
		if err != nil {
			job.LogCh <- fmt.Sprintf("[ERROR] 连接目标库失败: %v", err)
			updateJobStatus(dbJob, "failed", fmt.Sprintf("连接目标库失败: %v", err))
			return
		}
		defer writer.Close()

		cfg := datamigrate.Config{
			PageSize:    req.PageSize,
			MaxParallel: req.MaxParallel,
			Mode:        req.MigrateMode,
			Filter:      req.TableFilter,
		}
		m := datamigrate.NewMigrator(reader, writer, job, cfg)
		m.Run(ctx)

		status := "done"
		if ctx.Err() != nil {
			status = "cancelled"
		}
		updateJobStatus(dbJob, status, "")
	}()

	c.JSON(http.StatusOK, gin.H{"job_id": jobID})
}

func updateJobStatus(job *store.DataMigrationJob, status, summary string) {
	now := time.Now()
	job.Status = status
	job.Summary = summary
	job.FinishedAt = &now
	_ = store.UpdateDataMigrationJob(job)
}

// StreamDataMigration 通过 SSE 推送迁移日志
func StreamDataMigration(c *gin.Context) {
	jobID := c.Query("jobID")
	if jobID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "jobID 必填"})
		return
	}
	job := datamigrate.Registry.Get(jobID)
	if job == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "任务不存在或已完成"})
		return
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	clientGone := c.Request.Context().Done()
	for {
		select {
		case <-clientGone:
			return
		case msg, ok := <-job.LogCh:
			if !ok {
				// channel 关闭，迁移结束
				c.SSEvent("message", "[STREAM_END]")
				c.Writer.Flush()
				return
			}
			c.SSEvent("message", msg)
			c.Writer.Flush()
		}
	}
}

// CancelDataMigration 取消运行中的迁移任务
func CancelDataMigration(c *gin.Context) {
	jobID := c.Param("jobID")
	job := datamigrate.Registry.Get(jobID)
	if job == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "任务不存在或已完成"})
		return
	}
	job.Cancel()
	c.JSON(http.StatusOK, gin.H{"message": "已发送取消信号"})
}

// ListDataMigrationJobs 返回历史任务列表
func ListDataMigrationJobs(c *gin.Context) {
	jobs, err := store.ListDataMigrationJobs()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, jobs)
}
