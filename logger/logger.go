package logger

import (
	"io"
	"log"
	"log/slog"
	"os"
	"strings"
)

// Config 日志配置，独立于 config.Config 避免循环依赖。
type Config struct {
	Dir      string // 日志目录，默认 "log"
	Level    string // debug/info/warn/error，默认 "info"
	MaxFiles int    // 最多保留文件数，默认 7
}

// Init 初始化全局 slog logger，同时输出到 stdout 和日志文件。
// 必须在 main() 最开始调用，早于其他模块初始化。
// 返回的 cleanup 函数需在 main() 退出前调用以关闭文件句柄。
func Init(cfg *Config) (cleanup func(), err error) {
	if cfg.Dir == "" {
		cfg.Dir = "log"
	}
	if cfg.MaxFiles <= 0 {
		cfg.MaxFiles = 7
	}
	if cfg.Level == "" {
		cfg.Level = "info"
	}

	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, err
	}

	rot := newRotator(cfg)
	if err := rot.open(); err != nil {
		return nil, err
	}

	w := io.MultiWriter(os.Stdout, rot)
	level := parseLevel(cfg.Level)
	handler := slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(handler))

	// 同步标准库 log 包的输出到 slog，避免遗漏的 log.Printf 调用丢失
	log.SetOutput(io.Discard) // slog 接管，标准库 log 不再重复输出

	rot.startDailyRotation()

	return func() { rot.close() }, nil
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
