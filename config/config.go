package config

import (
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Port           string
	SQLitePath     string
	JWTSecret      string
	AdminUser      string
	AdminPass      string
	LogDir         string
	LogLevel       string
	LogMaxFiles    int
	UploadDir      string // 工单离线文件落盘目录
	MaxUploadBytes int64  // 单个上传文件大小上限（字节）
}

func Load() *Config {
	return &Config{
		Port:           getEnv("PORT", "8080"),
		SQLitePath:     getEnv("SQLITE_PATH", "dbgold.db"),
		JWTSecret:      getEnv("JWT_SECRET", "change-me-in-production"),
		AdminUser:      getEnv("ADMIN_USER", "admin"),
		AdminPass:      getEnv("ADMIN_PASS", "Admin@123"),
		LogDir:         getEnv("LOG_DIR", "log"),
		LogLevel:       getEnv("LOG_LEVEL", "info"),
		LogMaxFiles:    getEnvInt("LOG_MAX_FILES", 7),
		UploadDir:      getEnv("UPLOAD_DIR", "uploads"),
		MaxUploadBytes: getEnvInt64("MAX_UPLOAD_BYTES", 50<<30), // 默认 50GB
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
