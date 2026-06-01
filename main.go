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
)

func main() {
	cfg := config.Load()

	cleanup, err := logger.Init(&logger.Config{
		Dir:      cfg.LogDir,
		Level:    cfg.LogLevel,
		MaxFiles: cfg.LogMaxFiles,
	})
	if err != nil {
		log.Fatalf("failed to init logger: %v", err)
	}
	defer cleanup()

	store.Init(cfg)
	if err := store.EnsureAdminExists(cfg.AdminUser, cfg.AdminPass); err != nil {
		slog.Error("failed to ensure admin", "err", err)
		os.Exit(1)
	}

	middleware.SetJWTSecret(cfg.JWTSecret)
	handler.SetJWTSecret(cfg.JWTSecret)

	r := api.NewRouter()
	slog.Info("starting server", "port", cfg.Port)
	if err := r.Run(":" + cfg.Port); err != nil {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}
