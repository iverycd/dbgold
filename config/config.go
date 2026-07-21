package config

import (
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

type Config struct {
	Port                    string
	SQLitePath              string
	JWTSecret               string
	AdminUser               string
	AdminPass               string
	LogDir                  string
	LogLevel                string
	LogMaxFiles             int
	LogMaxTotalMB           int64  // 日志总量上限（MB），默认 2048（=2GB）
	UploadDir               string // 工单离线文件落盘目录
	MaxUploadBytes          int64  // 单个上传文件大小上限（字节）
	JWTExpireHours          int    // JWT 过期时间（小时），默认 240
	QueryTimeoutSeconds     int    // 查询中心单语句超时，默认 30 秒
	QueryMaxRows            int    // 查询中心最大返回行数，默认 1000
	QueryAuditRetentionDays int    // 查询中心审计保留天数，默认 90 天
}

func Load() *Config {
	// 自动加载 .env 文件（若存在）。godotenv.Load 不会覆盖已存在的真实环境变量，
	// 因此 systemd/容器注入的变量优先级高于 .env 文件。文件不存在时静默跳过。
	_ = godotenv.Load()

	return &Config{
		Port:                    getEnv("PORT", "8080"),
		SQLitePath:              getEnv("SQLITE_PATH", "dbgold.db"),
		JWTSecret:               getEnv("JWT_SECRET", "change-me-in-production"),
		AdminUser:               getEnv("ADMIN_USER", "admin"),
		AdminPass:               getEnv("ADMIN_PASS", "Admin@123"),
		LogDir:                  getEnv("LOG_DIR", "log"),
		LogLevel:                getEnv("LOG_LEVEL", "info"),
		LogMaxFiles:             getEnvInt("LOG_MAX_FILES", 7),
		LogMaxTotalMB:           getEnvInt64("LOG_MAX_TOTAL_MB", 2048),
		UploadDir:               getEnv("UPLOAD_DIR", "uploads"),
		MaxUploadBytes:          getEnvInt64("MAX_UPLOAD_BYTES", 50<<30), // 默认 50GB
		JWTExpireHours:          getEnvInt("JWT_EXPIRE_HOURS", 240),
		QueryTimeoutSeconds:     getEnvInt("QUERY_TIMEOUT_SECONDS", 30),
		QueryMaxRows:            getEnvInt("QUERY_MAX_ROWS", 1000),
		QueryAuditRetentionDays: getEnvInt("QUERY_AUDIT_RETENTION_DAYS", 90),
	}
}

func getEnv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

func getEnvInt64(key string, fallback int64) int64 {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}
