package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"dbgold/config"
	"github.com/gin-gonic/gin"
)

func runPortableCommand(args []string) error {
	flags := flag.NewFlagSet("portable", flag.ContinueOnError)
	noBrowser := flags.Bool("no-browser", false, "do not open the Windows browser")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected portable arguments: %v", flags.Args())
	}
	root, err := portableExecutableDir()
	if err != nil {
		return err
	}
	unlock, err := lockPortableRoot(root)
	if err != nil {
		return err
	}
	defer unlock()
	if err := checkPortableUpgradeState(root); err != nil {
		return err
	}
	if err := preparePortableRuntime(root); err != nil {
		return err
	}
	if err := initPortableConfig(root); err != nil {
		return err
	}
	cfg, err := loadPortableConfig(root)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	gin.SetMode(gin.ReleaseMode)
	return runServerWithReady(ctx, cfg, func() {
		fmt.Println("dbgold 已就绪。迁移期间请保持启动窗口或 SSH 会话，按 Ctrl+C 停止服务。")
		for _, url := range portableAccessURLs(cfg) {
			fmt.Println(url)
		}
		if runtime.GOOS == "windows" && !*noBrowser {
			if err := openPortableBrowser(localServerURL(cfg)); err != nil {
				fmt.Fprintln(os.Stderr, "未能自动打开浏览器，请手动访问上面的地址。", err)
			}
		}
	})
}

func portableExecutableDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return "", err
	}
	return filepath.Dir(exe), nil
}

func lockPortableRoot(root string) (func(), error) {
	// Leave this file in place. Removing it permits another process to lock a
	// different inode while the original owner is still running.
	dir, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	file, err := dir.OpenFile(".dbgold.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("便携目录必须对当前账号可写: %w", err)
	}
	if err := tryPortableLock(file); err != nil {
		file.Close()
		return nil, fmt.Errorf("无法独占此便携目录，可能已有 dbgold 实例或升级操作正在运行: %w", err)
	}
	return func() { closePortableLock(file) }, nil
}

func preparePortableRuntime(root string) error {
	bundle, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer bundle.Close()
	if err := os.Chdir(root); err != nil {
		return fmt.Errorf("进入便携目录: %w", err)
	}
	for _, name := range []string{"config", "data", "uploads", "logs", "tmp", "backups"} {
		if err := bundle.MkdirAll(name, 0o700); err != nil {
			return fmt.Errorf("便携目录 %s 必须位于程序目录内且可写: %w", name, err)
		}
		if runtime.GOOS != "windows" {
			if err := bundle.Chmod(name, 0o700); err != nil {
				return err
			}
		}
		probe := filepath.Join(name, ".write-check-"+rand.Text())
		file, err := bundle.OpenFile(probe, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("便携目录 %s 不可写: %w", name, err)
		}
		closeErr := file.Close()
		if err := bundle.Remove(probe); err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	for _, path := range []string{"config/dbgold.env", "data/dbgold.db", "data/dbgold.db-wal", "data/dbgold.db-shm", "data/dbgold.db-journal"} {
		if _, err := bundle.Stat(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	tmp := filepath.Join(root, "tmp")
	// os.TempDir, SQLite and the bundled JVM use different environment keys.
	for _, key := range []string{"TMPDIR", "TEMP", "TMP", "SQLITE_TMPDIR"} {
		if err := os.Setenv(key, tmp); err != nil {
			return err
		}
	}
	return nil
}

func initPortableConfig(root string) error {
	path := filepath.Join(root, "config", "dbgold.env")
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := os.Stat(filepath.Join(root, "data", "dbgold.db")); err == nil {
		return fmt.Errorf("已有数据库但缺少 %s，请从备份恢复配置", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	var secret [32]byte
	var password [16]byte
	rand.Read(secret[:])
	rand.Read(password[:])
	file, err := os.CreateTemp(filepath.Dir(path), ".dbgold.env-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	pass := hex.EncodeToString(password[:])
	_, writeErr := fmt.Fprintf(file, "# dbgold portable\nLISTEN_HOST=%s\nPORT=18089\nJWT_SECRET=%x\nADMIN_USER=admin\nADMIN_PASS=%s\n", portableDefaultHost(), secret, pass)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	fmt.Printf("首次启动，管理员 admin 的初始密码为 %s。配置已保存到 %s。\n", pass, path)
	return nil
}

// preparePortableRuntime sets cwd to root. Keeping UploadDir relative makes
// new ticket file references still valid after moving the release directory.
func loadPortableConfig(root string) (*config.Config, error) {
	cfg, err := config.LoadStandaloneFile(filepath.Join(root, "config", "dbgold.env"), portableDefaultHost())
	if err != nil {
		return nil, err
	}
	cfg.AppEnv = "production"
	cfg.StaticDir = filepath.Join(root, "web")
	cfg.SQLitePath = filepath.Join(root, "data", "dbgold.db")
	cfg.UploadDir = "uploads"
	cfg.LogDir = filepath.Join(root, "logs")
	cfg.OscarTempDir = filepath.Join(root, "tmp")
	java := "java"
	if runtime.GOOS == "windows" {
		java = "java.exe"
	}
	cfg.OscarJavaBin = filepath.Join(root, "runtime", "bin", java)
	cfg.OscarBridgeJar = filepath.Join(root, "lib", "dbgold-oscar-bridge.jar")
	cfg.OscarJDBCJar = filepath.Join(root, "lib", "oscarJDBC8.jar")
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func portableDefaultHost() string {
	if runtime.GOOS == "windows" {
		return "127.0.0.1"
	}
	return "0.0.0.0"
}

func localServerURL(cfg *config.Config) string {
	host := cfg.ListenHost
	if net.ParseIP(host).IsUnspecified() {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, cfg.Port) + "/"
}

func portableAccessURLs(cfg *config.Config) []string {
	if !net.ParseIP(cfg.ListenHost).IsUnspecified() {
		return []string{localServerURL(cfg)}
	}
	var urls []string
	addrs, _ := net.InterfaceAddrs()
	for _, addr := range addrs {
		ip, _, err := net.ParseCIDR(addr.String())
		if err != nil || ip.To4() == nil || ip.IsLoopback() || !ip.IsGlobalUnicast() {
			continue
		}
		urls = append(urls, "http://"+net.JoinHostPort(ip.String(), cfg.Port)+"/")
	}
	return append(urls, localServerURL(cfg))
}
