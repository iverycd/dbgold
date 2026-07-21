package api

import (
	"dbgold/api/handler"
	"dbgold/middleware"
	"dbgold/store"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
)

type RouterOptions struct {
	StaticDir      string
	TrustedProxies []string
}

func NewRouter() *gin.Engine {
	r, err := NewRouterWithOptions(RouterOptions{})
	if err != nil {
		panic(err)
	}
	return r
}

func NewRouterWithOptions(options RouterOptions) (*gin.Engine, error) {
	r := gin.New()
	if err := r.SetTrustedProxies(options.TrustedProxies); err != nil {
		return nil, fmt.Errorf("configure trusted proxies: %w", err)
	}
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
	r.GET("/api/health/live", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	r.GET("/api/health/ready", readinessHandler(options.StaticDir))

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

		authed.GET("/query/connections/:id/namespaces", handler.ListQueryNamespaces)
		authed.GET("/query/connections/:id/objects", handler.ListQueryObjects)
		authed.GET("/query/connections/:id/columns", handler.ListQueryColumns)
		authed.POST("/query/execute", handler.ExecuteQuery)
		authed.GET("/query/history", handler.ListQueryHistory)

		authed.GET("/schema/export-routines", handler.ExportRoutines)

		authed.GET("/migration/data-migrate/supported-pairs", handler.GetSupportedPairs)
		authed.POST("/migration/data-migrate", handler.StartDataMigration)
		authed.POST("/migration/data-migrate/:jobID/cancel", handler.CancelDataMigration)
		authed.GET("/migration/data-migrate/jobs", handler.ListDataMigrationJobs)
		authed.GET("/migration/data-migrate/jobs/:jobID", handler.GetDataMigrationJobDetail)
		authed.GET("/migration/data-migrate/:jobID/report", handler.GetDataMigrationReport)
		authed.POST("/migration/view-migrate", handler.MigrateViews)
		authed.POST("/migration/object-migrate", handler.StartObjectMigration)
		authed.POST("/migration/incremental/preflight", handler.PreflightIncremental)
		authed.POST("/migration/incremental/jobs", handler.StartIncremental)
		authed.GET("/migration/incremental/jobs", handler.ListIncremental)
		authed.GET("/migration/incremental/jobs/:jobID", handler.GetIncremental)
		authed.GET("/migration/incremental/jobs/:jobID/logs", handler.GetIncrementalLogs)
		authed.GET("/migration/incremental/jobs/:jobID/export-failed-ddl", handler.ExportIncrementalFailedDDL)
		authed.GET("/migration/incremental/jobs/:jobID/bootstrap-review", handler.GetIncrementalBootstrapReview)
		authed.POST("/migration/incremental/jobs/:jobID/accept-bootstrap-exclusions", handler.AcceptIncrementalBootstrapExclusions)
		authed.POST("/migration/incremental/jobs/:jobID/pause", handler.PauseIncremental)
		authed.POST("/migration/incremental/jobs/:jobID/resume", handler.ResumeIncremental)
		authed.POST("/migration/incremental/jobs/:jobID/prepare-cutover", handler.PrepareIncrementalCutover)
		authed.POST("/migration/incremental/jobs/:jobID/cancel-cutover", handler.CancelIncrementalCutover)
		authed.POST("/migration/incremental/jobs/:jobID/stop", handler.StopIncremental)
		authed.POST("/migration/incremental/jobs/:jobID/abort", handler.AbortIncremental)
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

	if options.StaticDir != "" {
		indexPath := filepath.Join(options.StaticDir, "index.html")
		if info, err := os.Stat(indexPath); err != nil || info.IsDir() {
			return nil, fmt.Errorf("static site index not found: %s", indexPath)
		}
		configureStaticSite(r, options.StaticDir)
	}

	return r, nil
}

func readinessHandler(staticDir string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if store.DB == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready", "error": "database is not initialized"})
			return
		}
		sqlDB, err := store.DB.DB()
		if err != nil || sqlDB.PingContext(c.Request.Context()) != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready", "error": "database is unavailable"})
			return
		}
		if staticDir != "" {
			if info, statErr := os.Stat(filepath.Join(staticDir, "index.html")); statErr != nil || info.IsDir() {
				c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready", "error": "static site is unavailable"})
				return
			}
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}

func configureStaticSite(r *gin.Engine, staticDir string) {
	staticRoot, _ := filepath.Abs(staticDir)
	r.NoRoute(func(c *gin.Context) {
		requestPath := c.Request.URL.Path
		if requestPath == "/api" || strings.HasPrefix(requestPath, "/api/") {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}

		relPath := strings.TrimPrefix(filepath.Clean("/"+requestPath), string(filepath.Separator))
		candidate := filepath.Join(staticRoot, relPath)
		if rel, err := filepath.Rel(staticRoot, candidate); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				if filepath.Base(candidate) == "index.html" {
					c.Header("Cache-Control", "no-cache")
				} else if strings.HasPrefix(requestPath, "/assets/") {
					c.Header("Cache-Control", "public, max-age=31536000, immutable")
				}
				c.File(candidate)
				return
			}
		}

		c.Header("Cache-Control", "no-cache")
		c.File(filepath.Join(staticRoot, "index.html"))
	})
}
