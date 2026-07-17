package main

import (
	"dbgold/api"
	"dbgold/api/handler"
	"dbgold/config"
	"dbgold/logger"
	"dbgold/middleware"
	"dbgold/store"
	"log"
	"log/slog"
	"os"
	"time"
)

func main() {
	cfg := config.Load()

	cleanup, err := logger.Init(&logger.Config{
		Dir:           cfg.LogDir,
		Level:         cfg.LogLevel,
		MaxFiles:      cfg.LogMaxFiles,
		MaxTotalBytes: cfg.LogMaxTotalMB * 1024 * 1024,
	})
	if err != nil {
		log.Fatalf("failed to init logger: %v", err)
	}
	defer cleanup()

	store.Init(cfg)
	cleanupExpiredIncrementalLogs()
	go runIncrementalLogCleanup()
	if err := store.DiscardLegacyIncrementalJobs(); err != nil {
		slog.Error("failed to discard legacy incremental jobs", "err", err)
	}
	if err := store.PauseInterruptedIncrementalJobs(); err != nil {
		slog.Error("failed to pause interrupted incremental jobs", "err", err)
	}
	if err := store.EnsureAdminExists(cfg.AdminUser, cfg.AdminPass); err != nil {
		slog.Error("failed to ensure admin", "err", err)
		os.Exit(1)
	}
	if err := store.BackfillOwner(cfg.AdminUser); err != nil {
		slog.Error("failed to backfill owner", "err", err)
		os.Exit(1)
	}

	middleware.SetJWTSecret(cfg.JWTSecret)
	handler.SetJWTSecret(cfg.JWTSecret)
	handler.SetJWTExpireHours(cfg.JWTExpireHours)
	handler.SetUploadConfig(cfg.UploadDir, cfg.MaxUploadBytes)

	r := api.NewRouter()
	slog.Info("starting server", "port", cfg.Port)
	if err := r.Run(":" + cfg.Port); err != nil {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}

func cleanupExpiredIncrementalLogs() {
	const retention = 30 * 24 * time.Hour
	if _, err := store.CleanupExpiredIncrementalMigrationLogs(time.Now().Add(-retention)); err != nil {
		slog.Error("failed to clean expired incremental migration logs", "err", err)
	}
}

func runIncrementalLogCleanup() {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		cleanupExpiredIncrementalLogs()
	}
}
