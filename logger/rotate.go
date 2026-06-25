package logger

import (
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type rotator struct {
	cfg     *Config
	mu      sync.Mutex
	file    *os.File
	curDate string
}

func newRotator(cfg *Config) *rotator {
	return &rotator{cfg: cfg}
}

func (r *rotator) open() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	today := time.Now().Format("2006-01-02")
	f, err := os.OpenFile(
		filepath.Join(r.cfg.Dir, today+".log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644,
	)
	if err != nil {
		return err
	}
	r.file = f
	r.curDate = today
	r.cleanup()
	return nil
}

func (r *rotator) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file != nil {
		r.file.Close()
		r.file = nil
	}
}

// Write 实现 io.Writer，跨天时自动切换文件。
func (r *rotator) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	today := time.Now().Format("2006-01-02")
	if today != r.curDate {
		r.rotate(today)
	}

	n, err := r.file.Write(p)

	if r.totalSizeOf(r.logFiles()) > r.cfg.MaxTotalBytes {
		r.cleanup()
	}
	return n, err
}

// rotate 关闭旧文件，打开新文件。调用方持有锁。
func (r *rotator) rotate(newDate string) {
	if r.file != nil {
		r.file.Close()
	}
	f, err := os.OpenFile(
		filepath.Join(r.cfg.Dir, newDate+".log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644,
	)
	if err != nil {
		// 无法打开新文件时降级：继续写旧文件
		slog.Error("log rotate failed", "err", err)
		return
	}
	r.file = f
	r.curDate = newDate
	r.cleanup()
}

// cleanup 按 ModTime 升序排序，先按数量裁剪，再按总大小裁剪。调用方持有锁。
func (r *rotator) cleanup() {
	files := r.logFiles()
	for len(files) > r.cfg.MaxFiles {
		os.Remove(filepath.Join(r.cfg.Dir, files[0].Name()))
		files = files[1:]
	}
	for r.totalSizeOf(files) > r.cfg.MaxTotalBytes && len(files) > 1 {
		os.Remove(filepath.Join(r.cfg.Dir, files[0].Name()))
		files = files[1:]
	}
}

// logFiles 返回 log 目录下所有 *.log 文件，按 ModTime 升序（最旧在前）。
func (r *rotator) logFiles() []fs.FileInfo {
	entries, _ := os.ReadDir(r.cfg.Dir)
	var infos []fs.FileInfo
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".log" {
			if info, err := e.Info(); err == nil {
				infos = append(infos, info)
			}
		}
	}
	sort.Slice(infos, func(i, j int) bool {
		return infos[i].ModTime().Before(infos[j].ModTime())
	})
	return infos
}

func (r *rotator) totalSizeOf(files []fs.FileInfo) int64 {
	var total int64
	for _, f := range files {
		total += f.Size()
	}
	return total
}

// startDailyRotation 在后台等到次日 00:00:05 触发轮转。
func (r *rotator) startDailyRotation() {
	go func() {
		for {
			now := time.Now()
			next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 5, 0, now.Location())
			time.Sleep(time.Until(next))

			r.mu.Lock()
			today := time.Now().Format("2006-01-02")
			if today != r.curDate {
				r.rotate(today)
			}
			r.mu.Unlock()
		}
	}()
}
