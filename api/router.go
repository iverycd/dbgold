package api

import (
	"dbgold/api/handler"
	"dbgold/middleware"

	"github.com/gin-gonic/gin"
)

func NewRouter() *gin.Engine {
	r := gin.Default()

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

		authed.POST("/schema/extract", handler.ExtractSchema)
		authed.POST("/schema/extract-full", handler.ExtractFullSchema)
		authed.POST("/schema/parse", handler.ParseDDLFile)
		authed.GET("/schema/export", handler.ExportDDL)
		authed.POST("/diff", handler.DiffSchemas)

		authed.POST("/migration/diff", handler.RunDiffMigration)
		authed.POST("/migration/full", handler.RunFullMigration)
		authed.POST("/migration/selective", handler.RunSelectiveMigration)
		authed.GET("/migration", handler.ListMigrations)
		authed.GET("/migration/:id", handler.GetMigration)

		authed.GET("/migration/data-migrate/supported-pairs", handler.GetSupportedPairs)
		authed.POST("/migration/data-migrate", handler.StartDataMigration)
		authed.GET("/migration/data-migrate/stream", handler.StreamDataMigration)
		authed.POST("/migration/data-migrate/:jobID/cancel", handler.CancelDataMigration)
		authed.GET("/migration/data-migrate/jobs", handler.ListDataMigrationJobs)
	}

	admin := r.Group("/api/admin")
	admin.Use(middleware.Auth(), middleware.AdminOnly())
	{
		admin.GET("/users", handler.ListUsers)
		admin.POST("/users", handler.CreateUser)
		admin.PUT("/users/:id", handler.UpdateUser)
	}

	return r
}
