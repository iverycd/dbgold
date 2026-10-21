// api/handler/datamigration.go
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
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
	{Source: "oracle", Target: "dameng"},
	{Source: "sqlserver", Target: "dameng"},
	{Source: "mysql", Target: "sqlserver"},
	{Source: "mysql", Target: "oracle"},
	{Source: "postgres", Target: "mysql"},
	{Source: "gaussdb", Target: "mysql"},
	{Source: "highgo", Target: "mysql"},
	{Source: "vastbase", Target: "mysql"},
	{Source: "gbase", Target: "mysql"},
	{Source: "seabox", Target: "mysql"},
	{Source: "kingbase", Target: "mysql"},

	{Source: "mysql", Target: "postgres"},
	{Source: "mysql", Target: "gaussdb"},
	{Source: "mysql", Target: "seabox"},
	{Source: "mysql", Target: "dameng"},
	{Source: "sqlserver", Target: "postgres"},
	{Source: "sqlserver", Target: "gaussdb"},
	{Source: "sqlserver", Target: "seabox"},
	{Source: "dameng", Target: "postgres"},
	{Source: "dameng", Target: "gaussdb"},
	{Source: "dameng", Target: "seabox"},
	{Source: "oracle", Target: "postgres"},
	{Source: "oracle", Target: "gaussdb"},
	{Source: "oracle", Target: "seabox"},
	{Source: "mysql", Target: "highgo"},
	{Source: "sqlserver", Target: "highgo"},
	{Source: "dameng", Target: "highgo"},
	{Source: "oracle", Target: "highgo"},
	{Source: "mysql", Target: "vastbase"},
	{Source: "sqlserver", Target: "vastbase"},
	{Source: "dameng", Target: "vastbase"},
	{Source: "oracle", Target: "vastbase"},
	{Source: "mysql", Target: "gbase"},
	{Source: "sqlserver", Target: "gbase"},
	{Source: "dameng", Target: "gbase"},
	{Source: "oracle", Target: "gbase"},
	{Source: "mysql", Target: "kingbase"},
	{Source: "sqlserver", Target: "kingbase"},
	{Source: "dameng", Target: "kingbase"},
	{Source: "oracle", Target: "kingbase"},
	{Source: "oracle", Target: "mysql"},
	{Source: "mysql", Target: "mysql"},
	{Source: "sqlserver", Target: "mysql"},
	{Source: "dameng", Target: "mysql"},
	{Source: "postgres", Target: "dameng"},
	{Source: "gaussdb", Target: "dameng"},
	{Source: "highgo", Target: "dameng"},
	{Source: "vastbase", Target: "dameng"},
	{Source: "gbase", Target: "dameng"},
	{Source: "seabox", Target: "dameng"},
	{Source: "kingbase", Target: "dameng"},
	{Source: "mysql", Target: "oscar"},
	{Source: "sqlserver", Target: "oscar"},
	{Source: "oracle", Target: "oscar"},
	{Source: "dameng", Target: "oscar"},
}

// GetSupportedPairs 返回支持的迁移组合列表
func GetSupportedPairs(c *gin.Context) {
	c.JSON(http.StatusOK, supportedPairs)
}

type startDataMigrationRequest struct {
	ExcludedObjects    []datamigrate.ObjectRef `json:"excluded_objects"`
	SrcConnID          uint                    `json:"src_conn_id" binding:"required"`
	DstConnID          uint                    `json:"dst_conn_id" binding:"required"`
	MigrateMode        string                  `json:"migrate_mode" binding:"required,oneof=all include exclude"`
	TableFilter        string                  `json:"table_filter"`
	MigrateContent     string                  `json:"migrate_content"` // "both" | "schema_only" | "data_only"，空值默认 "both"
	PageSize           int                     `json:"page_size"`
	MaxParallel        int                     `json:"max_parallel"`
	IntraTableParallel int                     `json:"intra_table_parallel"`
	LowerCaseNames     bool                    `json:"lower_case_names"`
	CharInLength       bool                    `json:"char_in_length"`
	UseNvarchar2       bool                    `json:"use_nvarchar2"`
	Distributed        bool                    `json:"distributed"`
	ChangeOwner        *bool                   `json:"change_owner"`       // nil: PG 兼容目标 true，其他目标 false
	SrcDatabase        string                  `json:"src_database"`       // 可选，覆盖连接中的默认数据库
	TargetSchema       string                  `json:"target_schema"`      // 可选，目标库 schema，为空时使用连接默认 search_path
	StripViewSchemas   string                  `json:"strip_view_schemas"` // 逗号分隔的模式名，迁移视图时从定义中剥离前缀(忽略大小写)
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
	srcConn, err := store.GetConnectionOwned(req.SrcConnID, middleware.GetCurrentUserID(c), middleware.IsAdmin(c))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "源库连接不存在"})
		return
	}
	dstConn, err := store.GetConnectionOwned(req.DstConnID, middleware.GetCurrentUserID(c), middleware.IsAdmin(c))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "目标库连接不存在"})
		return
	}

	if dstConn.DBType == "oracle" {
		req.LowerCaseNames = false
		req.CharInLength = true
		req.Distributed = false
	}
	changeOwner := resolveChangeOwner(dstConn.DBType, req.ChangeOwner)

	// 校验迁移组合是否支持
	if !isSupportedPair(srcConn.DBType, dstConn.DBType) {
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
		ExcludedObjects:    encodeExcludedObjects(req.ExcludedObjects),
		OwnerID:            middleware.GetCurrentUserID(c),
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
		ChangeOwner:        changeOwner,
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

	p := migrationParams{
		ExcludedObjects:    req.ExcludedObjects,
		MigrateMode:        req.MigrateMode,
		TableFilter:        req.TableFilter,
		MigrateContent:     req.MigrateContent,
		TargetSchema:       req.TargetSchema,
		StripViewSchemas:   req.StripViewSchemas,
		SrcDatabase:        req.SrcDatabase,
		PageSize:           req.PageSize,
		MaxParallel:        req.MaxParallel,
		IntraTableParallel: req.IntraTableParallel,
		LowerCaseNames:     req.LowerCaseNames,
		CharInLength:       req.CharInLength,
		UseNvarchar2:       req.UseNvarchar2,
		Distributed:        req.Distributed,
		ChangeOwner:        changeOwner,
		SrcMaxOpenConns:    req.SrcMaxOpenConns,
		SrcMaxIdleConns:    req.SrcMaxIdleConns,
		SrcConnMaxLifetime: req.SrcConnMaxLifetime,
		DstMaxOpenConns:    req.DstMaxOpenConns,
		DstMaxIdleConns:    req.DstMaxIdleConns,
		DstConnMaxLifetime: req.DstConnMaxLifetime,
	}

	go func() {
		defer func() {
			close(job.LogCh)
			datamigrate.Registry.Remove(jobID)
		}()
		runDataMigration(ctx, job, dbJob, srcConn, dstConn, p)
	}()

	c.JSON(http.StatusOK, gin.H{"job_id": jobID})
}

// migrationParams 承载一次数据迁移所需的全部参数，由单任务请求或批量行构造。
type migrationParams struct {
	ExcludedObjects  []datamigrate.ObjectRef
	MigrateMode      string
	TableFilter      string
	MigrateContent   string
	TargetSchema     string
	StripViewSchemas string
	SrcDatabase      string // 可选，覆盖连接默认库

	PageSize           int
	MaxParallel        int
	IntraTableParallel int

	LowerCaseNames bool
	CharInLength   bool
	UseNvarchar2   bool
	Distributed    bool
	ChangeOwner    bool

	SrcMaxOpenConns    int
	SrcMaxIdleConns    int
	SrcConnMaxLifetime int // 秒
	DstMaxOpenConns    int
	DstMaxIdleConns    int
	DstConnMaxLifetime int // 秒
}

// runDataMigration 同步执行一次数据迁移：构造 reader/writer → 校验目标 Schema →
// 运行 Migrator → 存储报告 → 更新任务状态。供单任务（goroutine 内调用）和批量串行复用。
// 不负责 close(job.LogCh) 与 Registry.Remove，由调用方控制（批量串行需复用同一 worker）。
func runDataMigration(ctx context.Context, job *datamigrate.Job, dbJob *store.DataMigrationJob,
	srcConn, dstConn *store.Connection, p migrationParams) {

	startup := newMigrationStartup(ctx, job, dbJob)
	started, proceed := startup.begin("连接源数据库")
	if !proceed {
		return
	}
	srcPool := source.ConnPoolConfig{
		MaxOpenConns:    p.SrcMaxOpenConns,
		MaxIdleConns:    p.SrcMaxIdleConns,
		ConnMaxLifetime: time.Duration(p.SrcConnMaxLifetime) * time.Second,
	}
	reader, readerErr := buildSrcReader(srcConn, p.SrcDatabase, srcPool, dstConn.DBType)
	if readerErr == nil {
		defer reader.Close()
	}
	if readerErr != nil && ctx.Err() == nil {
		slog.Warn("连接源库失败", "job_id", dbJob.JobID, "db_type", srcConn.DBType, "err", readerErr)
	}
	if !startup.finish("连接源数据库", started, readerErr) {
		return
	}

	started, proceed = startup.begin("连接目标数据库")
	if !proceed {
		return
	}
	dstPool := target.ConnPoolConfig{
		MaxOpenConns:    p.DstMaxOpenConns,
		MaxIdleConns:    p.DstMaxIdleConns,
		ConnMaxLifetime: time.Duration(p.DstConnMaxLifetime) * time.Second,
	}
	writer, writerErr := buildDstWriter(dstConn, p.TargetSchema, dstPool)
	if writerErr == nil {
		defer writer.Close()
	}
	if writerErr != nil && ctx.Err() == nil {
		slog.Warn("连接目标库失败", "job_id", dbJob.JobID, "db_type", dstConn.DBType, "err", writerErr)
	}
	if !startup.finish("连接目标数据库", started, writerErr) {
		return
	}

	if p.TargetSchema != "" {
		started, proceed = startup.begin("检查目标 Schema")
		if !proceed {
			return
		}
		exists, err := writer.SchemaExists(ctx, p.TargetSchema)
		if err == nil && !exists {
			err = fmt.Errorf("目标 Schema '%s' 不存在，请先在目标数据库中创建该 Schema", p.TargetSchema)
		}
		if !startup.finish("检查目标 Schema", started, err) {
			return
		}
	}

	var stripSchemas []string
	for _, s := range strings.Split(p.StripViewSchemas, ",") {
		if t := strings.TrimSpace(s); t != "" {
			stripSchemas = append(stripSchemas, t)
		}
	}
	cfg := datamigrate.Config{
		PreparationStartedAt: startup.startedAt,
		ExcludedObjects:      p.ExcludedObjects,
		PageSize:             p.PageSize,
		MaxParallel:          p.MaxParallel,
		Mode:                 p.MigrateMode,
		Filter:               p.TableFilter,
		Content:              p.MigrateContent,
		LowerCaseNames:       p.LowerCaseNames,
		CharInLength:         p.CharInLength,
		UseNvarchar2:         p.UseNvarchar2,
		Distributed:          p.Distributed,
		TargetSchema:         p.TargetSchema,
		ChangeOwner:          p.ChangeOwner,
		IntraTableParallel:   p.IntraTableParallel,
		TargetDBType:         dstConn.DBType,
		StripViewSchemas:     stripSchemas,
	}
	started, proceed = startup.begin("初始化迁移准备信息")
	if !proceed {
		return
	}
	releaseRepair, repairErr := prepareMigrationRepair(ctx, dbJob, srcConn, dstConn, p.SrcDatabase, &cfg, writer)
	if releaseRepair != nil {
		defer releaseRepair()
	}
	if !startup.finish("初始化迁移准备信息", started, repairErr) {
		return
	}
	m := datamigrate.NewMigrator(reader, writer, job, cfg)
	report := m.Run(ctx)

	if reportJSON, err := json.Marshal(report); err == nil {
		if saveErr := store.CreateDataMigrationReport(&store.DataMigrationReport{JobID: dbJob.JobID, ReportJSON: string(reportJSON)}); saveErr != nil {
			updateJobStatus(dbJob, "failed", "保存报告失败: "+saveErr.Error())
			return
		}
	} else {
		updateJobStatus(dbJob, "failed", "序列化报告失败: "+err.Error())
		return
	}

	status := "done"
	if ctx.Err() != nil {
		status = "cancelled"
	} else if report.HasFailures() {
		status = "failed"
	}
	updateJobStatus(dbJob, status, "")
}

func updateJobStatus(job *store.DataMigrationJob, status, summary string) {
	now := time.Now()
	job.Status = status
	job.Summary = summary
	job.FinishedAt = &now
	_ = store.UpdateDataMigrationJob(job)
}

type startObjectMigrationRequest struct {
	ExcludedObjects    []datamigrate.ObjectRef `json:"excluded_objects"`
	SrcConnID          uint                    `json:"src_conn_id" binding:"required"`
	DstConnID          uint                    `json:"dst_conn_id" binding:"required"`
	MigrateObjects     []string                `json:"migrate_objects" binding:"required,min=1,dive,oneof=primary_keys indexes sequences foreign_keys comments"`
	TableNames         []string                `json:"table_names" binding:"required,min=1"`
	SrcDatabase        string                  `json:"src_database"`
	TargetSchema       string                  `json:"target_schema"`
	LowerCaseNames     bool                    `json:"lower_case_names"`
	ChangeOwner        *bool                   `json:"change_owner"` // nil: PG 兼容目标 true，其他目标 false
	Distributed        bool                    `json:"distributed"`
	SrcMaxOpenConns    int                     `json:"src_max_open_conns"`
	SrcMaxIdleConns    int                     `json:"src_max_idle_conns"`
	SrcConnMaxLifetime int                     `json:"src_conn_max_lifetime"`
	DstMaxOpenConns    int                     `json:"dst_max_open_conns"`
	DstMaxIdleConns    int                     `json:"dst_max_idle_conns"`
	DstConnMaxLifetime int                     `json:"dst_conn_max_lifetime"`
}

// StartObjectMigration 创建并启动"仅对象迁移"后台任务:只为指定表补建所选对象类型
// (主键/索引/序列/外键),不建表、不迁数据。复用 SSE 日志流与迁移报告基础设施。
func StartObjectMigration(c *gin.Context) {
	var req startObjectMigrationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	srcConn, err := store.GetConnectionOwned(req.SrcConnID, middleware.GetCurrentUserID(c), middleware.IsAdmin(c))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "源库连接不存在"})
		return
	}
	dstConn, err := store.GetConnectionOwned(req.DstConnID, middleware.GetCurrentUserID(c), middleware.IsAdmin(c))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "目标库连接不存在"})
		return
	}
	if dstConn.DBType == "oracle" {
		req.LowerCaseNames = false
		req.Distributed = false
	}
	changeOwner := resolveChangeOwner(dstConn.DBType, req.ChangeOwner)
	if !isSupportedPair(srcConn.DBType, dstConn.DBType) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("不支持 %s → %s 的数据迁移", srcConn.DBType, dstConn.DBType),
		})
		return
	}

	jobID := uuid.New().String()
	ctx, cancel := context.WithCancel(context.Background())
	job := datamigrate.Registry.Register(jobID, cancel)

	srcConnDatabase := srcConn.Database
	if req.SrcDatabase != "" {
		srcConnDatabase = req.SrcDatabase
	}
	dbJob := &store.DataMigrationJob{
		ExcludedObjects: encodeExcludedObjects(req.ExcludedObjects),
		OwnerID:         middleware.GetCurrentUserID(c),
		JobID:           jobID,
		SrcConnID:       req.SrcConnID,
		DstConnID:       req.DstConnID,
		SrcDBType:       srcConn.DBType,
		DstDBType:       dstConn.DBType,
		MigrateMode:     "all",
		MigrateObjects:  strings.Join(req.MigrateObjects, ","),
		LowerCaseNames:  req.LowerCaseNames,
		DstSchema:       req.TargetSchema,
		ChangeOwner:     changeOwner,
		Status:          "running",
		SrcConnName:     srcConn.Name,
		SrcConnHost:     srcConn.Host,
		SrcConnPort:     srcConn.Port,
		SrcConnDatabase: srcConnDatabase,
		SrcConnUsername: srcConn.Username,
		DstConnName:     dstConn.Name,
		DstConnHost:     dstConn.Host,
		DstConnPort:     dstConn.Port,
		DstConnDatabase: dstConn.Database,
		DstConnUsername: dstConn.Username,
	}
	if err := store.CreateDataMigrationJob(dbJob); err != nil {
		cancel()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建任务记录失败"})
		return
	}

	go func() {
		defer func() {
			close(job.LogCh)
			datamigrate.Registry.Remove(jobID)
		}()

		startup := newMigrationStartup(ctx, job, dbJob)
		started, proceed := startup.begin("连接源数据库")
		if !proceed {
			return
		}
		srcPool := source.ConnPoolConfig{
			MaxOpenConns:    req.SrcMaxOpenConns,
			MaxIdleConns:    req.SrcMaxIdleConns,
			ConnMaxLifetime: time.Duration(req.SrcConnMaxLifetime) * time.Second,
		}
		reader, readerErr := buildSrcReader(srcConn, req.SrcDatabase, srcPool, dstConn.DBType)
		if readerErr == nil {
			defer reader.Close()
		}
		if readerErr != nil && ctx.Err() == nil {
			slog.Warn("连接源库失败", "job_id", jobID, "db_type", srcConn.DBType, "err", readerErr)
		}
		if !startup.finish("连接源数据库", started, readerErr) {
			return
		}

		started, proceed = startup.begin("连接目标数据库")
		if !proceed {
			return
		}
		dstPool := target.ConnPoolConfig{
			MaxOpenConns:    req.DstMaxOpenConns,
			MaxIdleConns:    req.DstMaxIdleConns,
			ConnMaxLifetime: time.Duration(req.DstConnMaxLifetime) * time.Second,
		}
		writer, writerErr := buildDstWriter(dstConn, req.TargetSchema, dstPool)
		if writerErr == nil {
			defer writer.Close()
		}
		if writerErr != nil && ctx.Err() == nil {
			slog.Warn("连接目标库失败", "job_id", jobID, "db_type", dstConn.DBType, "err", writerErr)
		}
		if !startup.finish("连接目标数据库", started, writerErr) {
			return
		}

		if req.TargetSchema != "" {
			started, proceed = startup.begin("检查目标 Schema")
			if !proceed {
				return
			}
			exists, err := writer.SchemaExists(ctx, req.TargetSchema)
			if err == nil && !exists {
				err = fmt.Errorf("目标 Schema '%s' 不存在，请先在目标数据库中创建该 Schema", req.TargetSchema)
			}
			if !startup.finish("检查目标 Schema", started, err) {
				return
			}
		}

		cfg := datamigrate.Config{
			PreparationStartedAt: startup.startedAt,
			ExcludedObjects:      req.ExcludedObjects,
			Mode:                 "all",
			Objects:              req.MigrateObjects,
			TableNames:           req.TableNames,
			LowerCaseNames:       req.LowerCaseNames,
			Distributed:          req.Distributed,
			TargetSchema:         req.TargetSchema,
			ChangeOwner:          changeOwner,
			TargetDBType:         dstConn.DBType,
		}
		started, proceed = startup.begin("初始化迁移准备信息")
		if !proceed {
			return
		}
		releaseRepair, repairErr := prepareMigrationRepair(ctx, dbJob, srcConn, dstConn, req.SrcDatabase, &cfg, writer)
		if releaseRepair != nil {
			defer releaseRepair()
		}
		if !startup.finish("初始化迁移准备信息", started, repairErr) {
			return
		}
		m := datamigrate.NewMigrator(reader, writer, job, cfg)
		report := m.Run(ctx)

		if reportJSON, err := json.Marshal(report); err == nil {
			if saveErr := store.CreateDataMigrationReport(&store.DataMigrationReport{JobID: jobID, ReportJSON: string(reportJSON)}); saveErr != nil {
				updateJobStatus(dbJob, "failed", "保存报告失败: "+saveErr.Error())
				return
			}
		} else {
			updateJobStatus(dbJob, "failed", "序列化报告失败: "+err.Error())
			return
		}

		status := "done"
		if ctx.Err() != nil {
			status = "cancelled"
		} else if report.HasFailures() {
			status = "failed"
		}
		updateJobStatus(dbJob, status, "")
	}()

	c.JSON(http.StatusOK, gin.H{"job_id": jobID})
}

// StreamDataMigration 通过 SSE 推送迁移日志（token 从 query string 读取，因为 EventSource 不支持 Authorization header）
func StreamDataMigration(c *gin.Context) {
	// 手动验证 token（EventSource 不支持自定义 header）
	tokenStr := c.Query("token")
	if tokenStr == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "缺少 token"})
		return
	}
	claims, err := middleware.ValidateTokenString(tokenStr)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "token 无效"})
		return
	}
	isAdmin := claims.Role == "admin"

	jobID := c.Query("jobID")
	if jobID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "jobID 必填"})
		return
	}
	// 归属校验：普通用户只能订阅自己的任务，admin 不限
	ownerJob, ownerErr := store.GetDataMigrationJob(jobID)
	if ownerErr != nil || ownerJob == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "任务不存在或已完成"})
		return
	}
	if !isAdmin && ownerJob.OwnerID != claims.UserID {
		c.JSON(http.StatusNotFound, gin.H{"error": "任务不存在或已完成"})
		return
	}
	streamDataMigrationLogs(c, ownerJob, datamigrate.Registry.Get(jobID), dataMigrationHeartbeatInterval)
}

// buildSrcReader 根据连接与可选的 srcDatabase 构造源库 Reader。
// srcDatabase 非空时覆盖连接默认库(mysql/sqlserver 需重建 DSN,oracle/dameng 的库即 schema 不重建)。
func buildSrcReader(conn *store.Connection, srcDatabase string, pool source.ConnPoolConfig, targetTypes ...string) (source.Reader, error) {
	db := conn.Database
	if srcDatabase != "" {
		db = srcDatabase
	}
	dsn := buildDSN(conn)
	if srcDatabase != "" {
		switch conn.DBType {
		case "mysql", "sqlserver":
			selected := *conn
			selected.Database = srcDatabase
			dsn = buildDSN(&selected)
		}
	}
	if conn.DBType == "mysql" && len(targetTypes) > 0 && targetTypes[0] == "sqlserver" {
		return source.NewMySQLForSQLServer(dsn, db, pool)
	}
	if conn.DBType == "mysql" && len(targetTypes) > 0 && targetTypes[0] == "oracle" {
		return source.NewMySQLForOracle(dsn, db, pool)
	}
	if conn.DBType == "mysql" && len(targetTypes) > 0 && targetTypes[0] == "dameng" {
		return source.NewMySQLForDameng(dsn, db, pool)
	}
	switch conn.DBType {
	case "sqlserver":
		if len(targetTypes) > 0 && targetTypes[0] == "dameng" {
			return source.NewSQLServerForDameng(dsn, db, pool)
		}
		return source.NewSQLServer(dsn, db, pool)
	case "dameng":
		return source.NewDaMeng(dsn, db, pool)
	case "oracle":
		if len(targetTypes) > 0 && targetTypes[0] == "dameng" {
			return source.NewOracleForDameng(dsn, db, pool)
		}
		return source.NewOracle(dsn, db, pool)
	case "postgres", "highgo", "vastbase", "gbase", "seabox", "kingbase":
		// PG 兼容库共用元数据读取逻辑；HighGo 使用专用驱动。迁移单元是库内 schema，
		// srcDatabase 即目标 schema，DSN 的 dbname 不变。
		return source.NewPostgresProduct(conn.DBType, pgCompatibleDriverName(conn.DBType), dsn, srcDatabase, pool)
	case "gaussdb":
		// GaussDB 需 opengauss 驱动，其余逻辑与 PG 一致
		return source.NewPostgresProduct(conn.DBType, "opengauss", dsn, srcDatabase, pool)
	default: // mysql
		return source.NewMySQL(dsn, db, pool)
	}
}

// buildDstWriter 根据目标连接类型构造对应的 Writer(消除多处重复的初始化 switch)
func buildDstWriter(dstConn *store.Connection, targetSchema string, pool target.ConnPoolConfig) (target.Writer, error) {
	dstDSN := buildDSN(dstConn)
	switch dstConn.DBType {
	case "sqlserver":
		return target.NewSQLServer(dstDSN, targetSchema, pool)
	case "oracle":
		return target.NewOracle(dstDSN, targetSchema, pool)
	case "gaussdb":
		return target.NewGaussDB(dstDSN, targetSchema, pool)
	case "dameng":
		return target.NewDaMeng(dstDSN, targetSchema, pool)
	case "highgo":
		return target.NewHighGo(dstDSN, targetSchema, pool)
	case "vastbase":
		return target.NewVastbase(dstDSN, targetSchema, pool)
	case "gbase":
		return target.NewGBase(dstDSN, targetSchema, pool)
	case "kingbase":
		return target.NewKingbase(dstDSN, targetSchema, pool)
	case "seabox":
		return target.NewPostgresCompatible(dstDSN, targetSchema, "seabox", pool)
	case "mysql":
		return target.NewMySQL(dstDSN, targetSchema, pool)
	case "oscar":
		return target.NewOscar(dstDSN, dstConn.Username, dstConn.Password, targetSchema, pool)
	default: // postgres
		return target.NewPostgres(dstDSN, targetSchema, pool)
	}
}

// isSupportedPair 校验源→目标迁移组合是否受支持
func isSupportedPair(srcType, dstType string) bool {
	for _, p := range supportedPairs {
		if p.Source == srcType && p.Target == dstType {
			return true
		}
	}
	return false
}

// ListConnectionViews 列出源连接指定库下的全部视图名(query 参数 database 可选覆盖默认库)
func ListConnectionViews(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	conn, err := store.GetConnectionOwned(uint(id), middleware.GetCurrentUserID(c), middleware.IsAdmin(c))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "connection not found"})
		return
	}
	reader, err := buildSrcReader(conn, c.Query("database"), source.ConnPoolConfig{}, c.Query("target_type"))
	if err != nil {
		c.Error(err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	defer reader.Close()
	views, err := reader.GetViews(c.Request.Context())
	if err != nil {
		c.Error(err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	names := make([]string, 0, len(views))
	for _, v := range views {
		names = append(names, v.ViewName)
	}
	c.JSON(http.StatusOK, names)
}

// ListConnectionTables 列出源连接指定库下的全部表名(query 参数 database 可选覆盖默认库)
func ListConnectionTables(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	conn, err := store.GetConnectionOwned(uint(id), middleware.GetCurrentUserID(c), middleware.IsAdmin(c))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "connection not found"})
		return
	}
	reader, err := buildSrcReader(conn, c.Query("database"), source.ConnPoolConfig{}, c.Query("target_type"))
	if err != nil {
		c.Error(err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	defer reader.Close()
	tables, err := reader.ListTables(c.Request.Context())
	if err != nil {
		c.Error(err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, tables)
}

type migrateViewsRequest struct {
	ExcludedObjects  []datamigrate.ObjectRef `json:"excluded_objects"`
	SrcConnID        uint                    `json:"src_conn_id" binding:"required"`
	DstConnID        uint                    `json:"dst_conn_id" binding:"required"`
	ViewNames        []string                `json:"view_names" binding:"required,min=1"`
	SrcDatabase      string                  `json:"src_database"`
	TargetSchema     string                  `json:"target_schema"`
	LowerCaseNames   bool                    `json:"lower_case_names"`
	ChangeOwner      *bool                   `json:"change_owner"` // nil: PG 兼容目标 true，其他目标 false
	StripViewSchemas string                  `json:"strip_view_schemas"`
}

// MigrateViews 按所选视图名同步批量在目标库创建视图,返回每个视图的结果
func MigrateViews(c *gin.Context) {
	var req migrateViewsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	srcConn, err := store.GetConnectionOwned(req.SrcConnID, middleware.GetCurrentUserID(c), middleware.IsAdmin(c))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "源库连接不存在"})
		return
	}
	dstConn, err := store.GetConnectionOwned(req.DstConnID, middleware.GetCurrentUserID(c), middleware.IsAdmin(c))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "目标库连接不存在"})
		return
	}

	if !isSupportedPair(srcConn.DBType, dstConn.DBType) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("不支持 %s → %s 的数据迁移", srcConn.DBType, dstConn.DBType),
		})
		return
	}

	if dstConn.DBType == "oracle" {
		req.LowerCaseNames = false
	}
	changeOwner := resolveChangeOwner(dstConn.DBType, req.ChangeOwner)
	reader, err := buildSrcReader(srcConn, req.SrcDatabase, source.ConnPoolConfig{}, dstConn.DBType)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("连接源库失败: %v", err)})
		return
	}
	defer reader.Close()

	writer, writerErr := buildDstWriter(dstConn, req.TargetSchema, target.ConnPoolConfig{})
	if writerErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("连接目标库失败: %v", writerErr)})
		return
	}
	defer writer.Close()

	ctx := c.Request.Context()
	if req.TargetSchema != "" {
		exists, err := writer.SchemaExists(ctx, req.TargetSchema)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("检查目标 Schema 失败: %v", err)})
			return
		}
		if !exists {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": fmt.Sprintf("目标 Schema '%s' 不存在，请先在目标数据库中创建该 Schema", req.TargetSchema),
			})
			return
		}
	}

	var stripSchemas []string
	for _, s := range strings.Split(req.StripViewSchemas, ",") {
		if t := strings.TrimSpace(s); t != "" {
			stripSchemas = append(stripSchemas, t)
		}
	}

	// 同步执行:用本地 Job(不注册 Registry),后台 drain 日志避免写阻塞
	job := &datamigrate.Job{LogCh: make(chan string, 512)}
	done := make(chan struct{})
	go func() {
		for range job.LogCh {
		}
		close(done)
	}()

	cfg := datamigrate.Config{
		ExcludedObjects:  req.ExcludedObjects,
		Mode:             "all",
		LowerCaseNames:   req.LowerCaseNames,
		TargetSchema:     req.TargetSchema,
		ChangeOwner:      changeOwner,
		TargetDBType:     dstConn.DBType,
		StripViewSchemas: stripSchemas,
	}
	schema, scopeErr := target.ResolveRepairSchema(ctx, writer)
	if scopeErr != nil {
		close(job.LogCh)
		<-done
		repairError(c, scopeErr)
		return
	}
	cfg.TargetSchema = schema
	scope := migrationScope(dstConn, schema)
	scopeOwner := uuid.NewString()
	if scopeErr = store.AcquireMigrationScope(scope, scopeOwner); scopeErr != nil {
		close(job.LogCh)
		<-done
		repairError(c, scopeErr)
		return
	}
	defer store.ReleaseMigrationScope(scope, scopeOwner)
	m := datamigrate.NewMigrator(reader, writer, job, cfg)
	results := m.MigrateViews(ctx, req.ViewNames)
	close(job.LogCh)
	<-done

	response := gin.H{"results": results}
	if compatibility := m.CompatibilityFunctions(); compatibility != nil {
		response["compatibility_functions"] = compatibility
	}
	c.JSON(http.StatusOK, response)
}

// CancelDataMigration 取消运行中的迁移任务
func CancelDataMigration(c *gin.Context) {
	jobID := c.Param("jobID")
	// 归属校验：普通用户只能取消自己的任务
	dbJob, err := store.GetDataMigrationJob(jobID)
	if err != nil || dbJob == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "任务不存在或已完成"})
		return
	}
	if !middleware.IsAdmin(c) && dbJob.OwnerID != middleware.GetCurrentUserID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "任务不存在或已完成"})
		return
	}
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
	filter, err := parseJobListFilter(c, dataMigrationListStatuses, true)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	jobs, err := store.QueryDataMigrationJobsWithConn(middleware.GetCurrentUserID(c), middleware.IsAdmin(c), filter)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, jobs)
}

// GetDataMigrationJobDetail returns one owned task with its frozen connection snapshots.
func GetDataMigrationJobDetail(c *gin.Context) {
	jobID := c.Param("jobID")
	job, err := store.GetDataMigrationJob(jobID)
	if err != nil || job == nil || (!middleware.IsAdmin(c) && job.OwnerID != middleware.GetCurrentUserID(c)) {
		c.JSON(http.StatusNotFound, gin.H{"error": "任务不存在"})
		return
	}
	detail, err := store.GetDataMigrationJobWithConn(jobID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, detail)
}

// GetDataMigrationReport 返回指定任务的迁移报告 JSON
func GetDataMigrationReport(c *gin.Context) {
	jobID := c.Param("jobID")
	// 归属校验：普通用户只能查看自己任务的报告
	dbJob, err := store.GetDataMigrationJob(jobID)
	if err != nil || dbJob == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "报告不存在"})
		return
	}
	if !middleware.IsAdmin(c) && dbJob.OwnerID != middleware.GetCurrentUserID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "报告不存在"})
		return
	}
	r, err := store.GetDataMigrationReport(jobID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "报告不存在"})
		return
	}
	if dbJob.SrcDBType == "oracle" && dbJob.DstDBType == "dameng" {
		body, err := enrichOracleDamengTriggerReport([]byte(r.ReportJSON))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "报告格式错误"})
			return
		}
		c.Data(http.StatusOK, "application/json", body)
		return
	}
	c.Data(http.StatusOK, "application/json", []byte(r.ReportJSON))
}
