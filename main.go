package main

import (
	"context"
	"dbgold/api"
	"dbgold/api/handler"
	"dbgold/config"
	"dbgold/logger"
	"dbgold/middleware"
	"dbgold/store"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const defaultHealthcheckTimeout = 5 * time.Second

func main() {
	if err := runCLI(os.Args[1:]); err != nil {
		log.Printf("dbgold: %v", err)
		os.Exit(1)
	}
}

func runCLI(args []string) error {
	command := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command = args[0]
		args = args[1:]
	}
	switch command {
	case "serve":
		return runServeCommand(args)
	case "healthcheck":
		return runHealthcheckCommand(args)
	case "help":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown command %q (expected serve or healthcheck)", command)
	}
}

func runServeCommand(args []string) error {
	cfg, err := parseServeConfig(args)
	if err != nil {
		return err
	}
	return runManaged(func(ctx context.Context) error {
		return runServer(ctx, cfg)
	})
}

func parseServeConfig(args []string) (*config.Config, error) {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	configFile := flags.String("config", "", "path to an env configuration file")
	listenHost := flags.String("listen-host", "", "IPv4 address to listen on (overrides LISTEN_HOST)")
	port := flags.String("port", "", "TCP port to listen on (overrides PORT)")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if flags.NArg() != 0 {
		return nil, fmt.Errorf("unexpected serve arguments: %s", strings.Join(flags.Args(), " "))
	}

	cfg, err := config.LoadFromFile(*configFile)
	if err != nil {
		return nil, err
	}
	if *listenHost != "" {
		cfg.ListenHost = *listenHost
	}
	if *port != "" {
		cfg.Port = *port
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func runHealthcheckCommand(args []string) error {
	flags := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	configFile := flags.String("config", "", "path to an env configuration file")
	endpoint := flags.String("url", "", "readiness URL")
	timeout := flags.Duration("timeout", defaultHealthcheckTimeout, "request timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected healthcheck arguments: %s", strings.Join(flags.Args(), " "))
	}
	cfg, err := config.LoadFromFile(*configFile)
	if err != nil {
		return err
	}
	url := *endpoint
	if url == "" {
		url = "http://127.0.0.1:" + cfg.Port + "/api/health/ready"
	}
	client := &http.Client{Timeout: *timeout}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("healthcheck request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck returned %s", resp.Status)
	}
	fmt.Println("dbgold is ready")
	return nil
}

func printUsage() {
	fmt.Println(`Usage:
  dbgold serve [--config FILE] [--listen-host IPv4] [--port PORT]
  dbgold healthcheck [--config FILE] [--url URL] [--timeout 5s]`)
}

func runServer(ctx context.Context, cfg *config.Config) error {
	address := net.JoinHostPort(cfg.ListenHost, cfg.Port)
	listener, err := net.Listen("tcp4", address)
	if err != nil {
		return fmt.Errorf("listen on %s (the port may already be in use): %w", address, err)
	}
	defer listener.Close()

	cleanupLogger, err := logger.Init(&logger.Config{
		Dir:           cfg.LogDir,
		Level:         cfg.LogLevel,
		MaxFiles:      cfg.LogMaxFiles,
		MaxTotalBytes: cfg.LogMaxTotalMB * 1024 * 1024,
	})
	if err != nil {
		return fmt.Errorf("initialize logger: %w", err)
	}
	defer cleanupLogger()

	if err := store.InitWithError(cfg); err != nil {
		return err
	}
	defer func() {
		if err := store.Close(); err != nil {
			slog.Error("failed to close database", "err", err)
		}
	}()

	handler.SetQueryConfig(cfg.QueryTimeoutSeconds, cfg.QueryMaxRows)
	cleanupExpiredQueryAudits(cfg.QueryAuditRetentionDays)
	go runQueryAuditCleanup(cfg.QueryAuditRetentionDays)
	cleanupExpiredIncrementalLogs()
	go runIncrementalLogCleanup()
	if err := store.DiscardLegacyIncrementalJobs(); err != nil {
		slog.Error("failed to discard legacy incremental jobs", "err", err)
	}
	if err := store.PauseInterruptedIncrementalJobs(); err != nil {
		slog.Error("failed to pause interrupted incremental jobs", "err", err)
	}
	if err := store.EnsureAdminExists(cfg.AdminUser, cfg.AdminPass); err != nil {
		return fmt.Errorf("ensure admin: %w", err)
	}
	if err := store.BackfillOwner(cfg.AdminUser); err != nil {
		return fmt.Errorf("backfill owner: %w", err)
	}

	middleware.SetJWTSecret(cfg.JWTSecret)
	handler.SetJWTSecret(cfg.JWTSecret)
	handler.SetJWTExpireHours(cfg.JWTExpireHours)
	handler.SetUploadConfig(cfg.UploadDir, cfg.MaxUploadBytes)

	staticDir := cfg.StaticDir
	if _, err := os.Stat(filepath.Join(staticDir, "index.html")); err != nil && !strings.EqualFold(cfg.AppEnv, "production") {
		slog.Warn("frontend build not found; starting API-only development server", "static_dir", staticDir)
		staticDir = ""
	}
	r, err := api.NewRouterWithOptions(api.RouterOptions{
		StaticDir:      staticDir,
		TrustedProxies: cfg.TrustedProxies,
	})
	if err != nil {
		return err
	}

	server := &http.Server{
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	serveErr := make(chan error, 1)
	go func() {
		slog.Info("starting server", "address", address)
		serveErr <- server.Serve(listener)
	}()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve HTTP: %w", err)
	case <-ctx.Done():
		slog.Info("shutting down server")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return fmt.Errorf("shutdown HTTP server: %w", err)
		}
		err := <-serveErr
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve HTTP: %w", err)
		}
		return nil
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

func cleanupExpiredQueryAudits(retentionDays int) {
	if _, err := store.CleanupExpiredQueryAudits(time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)); err != nil {
		slog.Error("failed to clean expired query audits", "err", err)
	}
}

func runQueryAuditCleanup(retentionDays int) {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		cleanupExpiredQueryAudits(retentionDays)
	}
}
