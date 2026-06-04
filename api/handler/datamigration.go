// api/handler/datamigration.go
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"dbgold/datamigrate"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/middleware"
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
	{Source: "mysql", Target: "gaussdb"},
	{Source: "mysql", Target: "seabox"},
	{Source: "sqlserver", Target: "postgres"},
	{Source: "sqlserver", Target: "gaussdb"},
	{Source: "sqlserver", Target: "seabox"},
	{Source: "dameng", Target: "postgres"},
	{Source: "dameng", Target: "gaussdb"},
	{Source: "dameng", Target: "seabox"},
	{Source: "oracle", Target: "postgres"},
	{Source: "oracle", Target: "gaussdb"},
	{Source: "oracle", Target: "seabox"},
}

// GetSupportedPairs 返回支持的迁移组合列表
func GetSupportedPairs(c *gin.Context) {
	c.JSON(http.StatusOK, supportedPairs)
}

type startDataMigrationRequest struct {
	SrcConnID          uint   `json:"src_conn_id" binding:"required"`
	DstConnID          uint   `json:"dst_conn_id" binding:"required"`
	MigrateMode        string `json:"migrate_mode" binding:"required,oneof=all include exclude"`
	TableFilter        string `json:"table_filter"`
	MigrateContent     string `json:"migrate_content"` // "both" | "schema_only" | "data_only"，空值默认 "both"
	PageSize           int    `json:"page_size"`
	MaxParallel        int    `json:"max_parallel"`
	IntraTableParallel int    `json:"intra_table_parallel"`
	LowerCaseNames     bool   `json:"lower_case_names"`
	CharInLength       bool   `json:"char_in_length"`
	UseNvarchar2       bool   `json:"use_nvarchar2"`
	Distributed        bool   `json:"distributed"`
	SrcDatabase        string `json:"src_database"`  // 可选，覆盖连接中的默认数据库
	TargetSchema       string `json:"target_schema"` // 可选，目标库 schema，为空时使用连接默认 search_path
	// 连接池配置，0 表示使用默认值（MaxOpenConns=50, MaxIdleConns=25, ConnMaxLifetime=3600s）
	SrcMaxOpenConns    int `json:"src_max_open_conns"`
	SrcMaxIdleConns    int `json:"src_max_idle_conns"`
	SrcConnMaxLifetime int `json:"src_conn_max_lifetime"` // 秒
	DstMaxOpenConns    int `json:"dst_max_open_conns"`
	DstMaxIdleConns    int `json:"dst_max_idle_conns"`
	DstConnMaxLifetime int `json:"dst_conn_max_lifetime"` // 秒
}

// StartDataMigration 创建并启动迁移任务，立即返回 jobID
func StartDataMigration(c *gin.Context) {
	var req startDataMigrationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.PageSize <= 0 {
		req.PageSize = 20000
	}
	if req.MaxParallel <= 0 {
		req.MaxParallel = 10
	}
	if req.IntraTableParallel <= 0 {
		req.IntraTableParallel = 1
	}
	if req.MigrateContent == "" {
		req.MigrateContent = "both"
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
	srcConnDatabase := srcConn.Database
	if req.SrcDatabase != "" {
		srcConnDatabase = req.SrcDatabase
	}
	dbJob := &store.DataMigrationJob{
		JobID:              jobID,
		SrcConnID:          req.SrcConnID,
		DstConnID:          req.DstConnID,
		SrcDBType:          srcConn.DBType,
		DstDBType:          dstConn.DBType,
		MigrateMode:        req.MigrateMode,
		TableFilter:        req.TableFilter,
		PageSize:           req.PageSize,
		MaxParallel:        req.MaxParallel,
		IntraTableParallel: req.IntraTableParallel,
		LowerCaseNames:     req.LowerCaseNames,
		CharInLength:       req.CharInLength,
		UseNvarchar2:       req.UseNvarchar2,
		DstSchema:          req.TargetSchema,
		Status:             "running",
		SrcConnName:        srcConn.Name,
		SrcConnHost:        srcConn.Host,
		SrcConnPort:        srcConn.Port,
		SrcConnDatabase:    srcConnDatabase,
		SrcConnUsername:    srcConn.Username,
		DstConnName:        dstConn.Name,
		DstConnHost:        dstConn.Host,
		DstConnPort:        dstConn.Port,
		DstConnDatabase:    dstConn.Database,
		DstConnUsername:    dstConn.Username,
	}
	if err := store.CreateDataMigrationJob(dbJob); err != nil {
		cancel()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建任务记录失败"})
		return
	}

	srcDSN := buildDSN(srcConn)
	dstDSN := buildDSN(dstConn)

	// 若请求中指定了源库数据库，覆盖连接默认值
	// 注意：Oracle 的 SrcDatabase 是 schema/owner 名，不是 service name，不能用来重建 DSN
	if req.SrcDatabase != "" {
		switch srcConn.DBType {
		case "mysql":
			srcDSN = fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?parseTime=true&charset=utf8mb4",
				srcConn.Username, srcConn.Password, srcConn.Host, srcConn.Port, srcConnDatabase)
		case "sqlserver":
			srcDSN = fmt.Sprintf("server=%s;port=%d;database=%s;user id=%s;password=%s;trustservercertificate=true;encrypt=DISABLE",
				srcConn.Host, srcConn.Port, srcConnDatabase, srcConn.Username, srcConn.Password)
		}
	}

	go func() {
		defer func() {
			close(job.LogCh)
			datamigrate.Registry.Remove(jobID)
		}()

		srcPool := source.ConnPoolConfig{
			MaxOpenConns:    req.SrcMaxOpenConns,
			MaxIdleConns:    req.SrcMaxIdleConns,
			ConnMaxLifetime: time.Duration(req.SrcConnMaxLifetime) * time.Second,
		}
		var reader source.Reader
		var readerErr error
		switch srcConn.DBType {
		case "sqlserver":
			reader, readerErr = source.NewSQLServer(srcDSN, srcConnDatabase, srcPool)
		case "dameng":
			reader, readerErr = source.NewDaMeng(srcDSN, srcConnDatabase, srcPool)
		case "oracle":
			reader, readerErr = source.NewOracle(srcDSN, srcConnDatabase, srcPool)
		default: // mysql
			reader, readerErr = source.NewMySQL(srcDSN, srcConnDatabase, srcPool)
		}
		if readerErr != nil {
			slog.Warn("连接源库失败", "job_id", jobID, "db_type", srcConn.DBType, "err", readerErr)
			job.LogCh <- fmt.Sprintf("[ERROR] 连接源库失败: %v", readerErr)
			updateJobStatus(dbJob, "failed", fmt.Sprintf("连接源库失败: %v", readerErr))
			return
		}
		defer reader.Close()

		dstPool := target.ConnPoolConfig{
			MaxOpenConns:    req.DstMaxOpenConns,
			MaxIdleConns:    req.DstMaxIdleConns,
			ConnMaxLifetime: time.Duration(req.DstConnMaxLifetime) * time.Second,
		}
		var writer target.Writer
		var writerErr error
		if dstConn.DBType == "gaussdb" {
			writer, writerErr = target.NewGaussDB(dstDSN, req.TargetSchema, dstPool)
		} else {
			writer, writerErr = target.NewPostgres(dstDSN, req.TargetSchema, dstPool)
		}
		if writerErr != nil {
			slog.Warn("连接目标库失败", "job_id", jobID, "db_type", dstConn.DBType, "err", writerErr)
			job.LogCh <- fmt.Sprintf("[ERROR] 连接目标库失败: %v", writerErr)
			updateJobStatus(dbJob, "failed", fmt.Sprintf("连接目标库失败: %v", writerErr))
			return
		}
		defer writer.Close()

		if req.TargetSchema != "" {
			exists, err := writer.SchemaExists(ctx, req.TargetSchema)
			if err != nil {
				job.LogCh <- fmt.Sprintf("[ERROR] 检查目标 Schema 失败: %v", err)
				updateJobStatus(dbJob, "failed", fmt.Sprintf("检查目标 Schema 失败: %v", err))
				return
			}
			if !exists {
				msg := fmt.Sprintf("目标 Schema '%s' 不存在，请先在目标数据库中创建该 Schema", req.TargetSchema)
				job.LogCh <- "[ERROR] " + msg
				updateJobStatus(dbJob, "failed", msg)
				return
			}
		}

		cfg := datamigrate.Config{
			PageSize:           req.PageSize,
			MaxParallel:        req.MaxParallel,
			Mode:               req.MigrateMode,
			Filter:             req.TableFilter,
			Content:            req.MigrateContent,
			LowerCaseNames:     req.LowerCaseNames,
			CharInLength:       req.CharInLength,
			UseNvarchar2:       req.UseNvarchar2,
			Distributed:        req.Distributed,
			TargetSchema:       req.TargetSchema,
			IntraTableParallel: req.IntraTableParallel,
		}
		m := datamigrate.NewMigrator(reader, writer, job, cfg)
		report := m.Run(ctx)

		if reportJSON, err := json.Marshal(report); err == nil {
			_ = store.CreateDataMigrationReport(&store.DataMigrationReport{
				JobID:      jobID,
				ReportJSON: string(reportJSON),
			})
		}

		status := "done"
		if ctx.Err() != nil {
			status = "cancelled"
		} else if report.Tables.Failed+report.Data.Failed+report.PrimaryKeys.Failed+
			report.Views.Failed+report.Indexes.Failed+report.Constraints.Failed+
			report.Sequences.Failed > 0 {
			status = "failed"
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

// StreamDataMigration 通过 SSE 推送迁移日志（token 从 query string 读取，因为 EventSource 不支持 Authorization header）
func StreamDataMigration(c *gin.Context) {
	// 手动验证 token（EventSource 不支持自定义 header）
	tokenStr := c.Query("token")
	if tokenStr == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "缺少 token"})
		return
	}
	if err := middleware.ValidateTokenString(tokenStr); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "token 无效"})
		return
	}

	jobID := c.Query("jobID")
	if jobID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "jobID 必填"})
		return
	}
	job := datamigrate.Registry.Get(jobID)
	if job == nil {
		// goroutine 可能已退出（如连接失败），从数据库读取 summary 通过 SSE 推给前端
		dbJob, dbErr := store.GetDataMigrationJob(jobID)
		if dbErr != nil || dbJob == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "任务不存在或已完成"})
			return
		}
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("X-Accel-Buffering", "no")
		if dbJob.Summary != "" {
			c.SSEvent("message", "[ERROR] "+dbJob.Summary)
		}
		c.SSEvent("message", "[STREAM_END]")
		c.Writer.Flush()
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

// ListDataMigrationJobs 返回历史任务列表（含连接快照信息）
func ListDataMigrationJobs(c *gin.Context) {
	jobs, err := store.ListDataMigrationJobsWithConn()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, jobs)
}

// GetDataMigrationReport 返回指定任务的迁移报告 JSON
func GetDataMigrationReport(c *gin.Context) {
	jobID := c.Param("jobID")
	r, err := store.GetDataMigrationReport(jobID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "报告不存在"})
		return
	}
	c.Data(http.StatusOK, "application/json", []byte(r.ReportJSON))
}
