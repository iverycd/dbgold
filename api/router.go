package api

import (
	"dbgold/api/handler"
	"dbgold/middleware"

	"github.com/gin-gonic/gin"
)

func NewRouter() *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(middleware.RequestLogger())

	r.Use(func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type,Authorization")
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	})

	public := r.Group("/api/auth")
	{
		public.POST("/login", handler.Login)
	}

	// 公开端点：系统版本信息，登录页也能取到，本身无敏感性。
	r.GET("/api/version", handler.GetVersion)

	// 公开端点：前台用户无需登录即可提交迁移工单 / 上传源库离线文件。
	// 均按客户端 IP 限流，提交接口额外要求图形验证码，防止滥用。
	r.GET("/api/tickets/captcha", middleware.RateLimit(30, 10), handler.IssueCaptcha)
	r.POST("/api/tickets", middleware.RateLimit(5, 3), handler.SubmitTicket)
	r.POST("/api/tickets/upload", middleware.RateLimit(10, 5), handler.UploadTicketFile)

	authed := r.Group("/api")
	authed.Use(middleware.Auth())
	{
		authed.GET("/auth/me", handler.GetMe)
		authed.PUT("/auth/password", handler.ChangePassword)

		authed.GET("/connections", handler.GetConnections)
		authed.POST("/connections", handler.CreateConnection)
		authed.PUT("/connections/:id", handler.UpdateConnection)
		authed.DELETE("/connections/:id", handler.DeleteConnection)
		authed.POST("/connections/:id/test", handler.TestConnection)
		authed.GET("/connections/:id/databases", handler.ListConnectionDatabases)
		authed.GET("/connections/:id/schemas", handler.ListConnectionSchemas)
		authed.GET("/connections/:id/views", handler.ListConnectionViews)
		authed.GET("/connections/:id/tables", handler.ListConnectionTables)

		authed.POST("/schema/extract", handler.ExtractSchema)
		authed.POST("/schema/extract-full", handler.ExtractFullSchema)
		authed.POST("/schema/parse", handler.ParseDDLFile)
		authed.GET("/schema/export", handler.ExportDDL)
		authed.GET("/schema/export-routines", handler.ExportRoutines)
		authed.POST("/diff", handler.DiffSchemas)

		authed.POST("/migration/diff", handler.RunDiffMigration)
		authed.POST("/migration/full", handler.RunFullMigration)
		authed.POST("/migration/selective", handler.RunSelectiveMigration)
		authed.GET("/migration", handler.ListMigrations)
		authed.GET("/migration/:id", handler.GetMigration)

		authed.GET("/migration/data-migrate/supported-pairs", handler.GetSupportedPairs)
		authed.POST("/migration/data-migrate", handler.StartDataMigration)
		authed.POST("/migration/data-migrate/:jobID/cancel", handler.CancelDataMigration)
		authed.GET("/migration/data-migrate/jobs", handler.ListDataMigrationJobs)
		authed.GET("/migration/data-migrate/:jobID/report", handler.GetDataMigrationReport)
		authed.POST("/migration/view-migrate", handler.MigrateViews)
		authed.POST("/migration/object-migrate", handler.StartObjectMigration)
		authed.POST("/migration/incremental/preflight", handler.PreflightIncremental)
		authed.POST("/migration/incremental/jobs", handler.StartIncremental)
		authed.GET("/migration/incremental/jobs", handler.ListIncremental)
		authed.GET("/migration/incremental/jobs/:jobID", handler.GetIncremental)
		authed.POST("/migration/incremental/jobs/:jobID/pause", handler.PauseIncremental)
		authed.POST("/migration/incremental/jobs/:jobID/resume", handler.ResumeIncremental)
		authed.POST("/migration/incremental/jobs/:jobID/stop", handler.StopIncremental)
		authed.POST("/migration/incremental/jobs/:jobID/ack-ddl", handler.AckIncrementalDDL)

		authed.POST("/migration/batch/validate", handler.ValidateBatch)
		authed.POST("/migration/batch/start", handler.StartBatch)
		authed.GET("/migration/batch", handler.ListBatches)
		authed.GET("/migration/batch/template", handler.DownloadBatchTemplate)
		authed.GET("/migration/batch/:batchID/jobs", handler.ListBatchJobs)
		authed.POST("/migration/batch/:batchID/cancel", handler.CancelBatch)

		// 工单管理:只读 + 处理,普通用户也可用(仅需登录)。
		// 路径保留 /admin/tickets 字面命名以兼容前端,功能上不限角色。
		authed.GET("/admin/tickets", handler.ListTickets)
		authed.GET("/admin/tickets/:id", handler.GetTicket)
		authed.PUT("/admin/tickets/:id", handler.UpdateTicket)
		authed.PUT("/admin/tickets/:id/info", handler.UpdateTicketInfo)
		authed.POST("/admin/tickets/:id/connections", handler.CreateTicketConnections)
	}

	// SSE 端点：token 从 query string 读取，因为浏览器 EventSource 不支持自定义 header
	r.GET("/api/migration/data-migrate/stream", handler.StreamDataMigration)

	admin := r.Group("/api/admin")
	admin.Use(middleware.Auth(), middleware.AdminOnly())
	{
		admin.GET("/users", handler.ListUsers)
		admin.POST("/users", handler.CreateUser)
		admin.PUT("/users/:id", handler.UpdateUser)
		admin.GET("/login-history", handler.ListLoginHistory)

		// 工单删除仅限 admin;其余工单接口已下放到 authed 组。
		admin.DELETE("/tickets/:id", handler.DeleteTicket)
	}

	return r
}
