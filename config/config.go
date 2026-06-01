package config

import (
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Port        string
	SQLitePath  string
	JWTSecret   string
	AdminUser   string
	AdminPass   string
	LogDir      string
	LogLevel    string
	LogMaxFiles int
}

func Load() *Config {
	return &Config{
		Port:        getEnv("PORT", "8080"),
		SQLitePath:  getEnv("SQLITE_PATH", "dbgold.db"),
		JWTSecret:   getEnv("JWT_SECRET", "change-me-in-production"),
		AdminUser:   getEnv("ADMIN_USER", "admin"),
		AdminPass:   getEnv("ADMIN_PASS", "Admin@123"),
		LogDir:      getEnv("LOG_DIR", "log"),
		LogLevel:    getEnv("LOG_LEVEL", "info"),
		LogMaxFiles: getEnvInt("LOG_MAX_FILES", 7),
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
