package jdbcbridge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestHelperHandshake(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	bridgeJar := filepath.Join(root, "jdbcbridge", "java", "build", "dbgold-oscar-bridge.jar")
	jdbcJar := filepath.Join(root, "third_party", "oscar", "oscarJDBC8.jar")
	if _, err := os.Stat(bridgeJar); err != nil {
		t.Skip("Java helper has not been built")
	}
	manager := NewManager(Config{BridgeJar: bridgeJar, JDBCJar: jdbcJar, MaxHeapMB: 64, TempDir: t.TempDir()})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := manager.ensureStarted(ctx); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBridgeCrashFailsOldSessionWithoutRestart(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	bridgeJar := filepath.Join(root, "jdbcbridge", "java", "build", "dbgold-oscar-bridge.jar")
	jdbcJar := filepath.Join(root, "third_party", "oscar", "oscarJDBC8.jar")
	if _, err := os.Stat(bridgeJar); err != nil {
		t.Skip("Java helper has not been built")
	}
	manager := NewManager(Config{BridgeJar: bridgeJar, JDBCJar: jdbcJar, MaxHeapMB: 64})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := manager.ensureStarted(ctx); err != nil {
		t.Fatal(err)
	}
	manager.processMu.Lock()
	generation := manager.generation
	manager.processMu.Unlock()
	if err := manager.stopProcess(generation); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		manager.processMu.Lock()
		stopped := manager.cmd == nil
		manager.processMu.Unlock()
		if stopped || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := manager.Ping(context.Background(), 1); err == nil {
		t.Fatal("old session operation unexpectedly succeeded")
	}
	manager.processMu.Lock()
	defer manager.processMu.Unlock()
	if manager.generation != generation || manager.cmd != nil {
		t.Fatal("old session operation restarted the bridge")
	}
}

func TestExistingSessionOperationDoesNotStartJVM(t *testing.T) {
	manager := NewManager(Config{})
	err := manager.Ping(context.Background(), 123)
	if err == nil {
		t.Fatal("expected closed bridge error")
	}
	manager.processMu.Lock()
	started := manager.cmd != nil
	manager.processMu.Unlock()
	if started {
		t.Fatal("an existing-session operation must not restart the JVM")
	}
}

func TestOscarJavaDiscovery(t *testing.T) {
	root := t.TempDir()
	name := "java"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	hostJava := func(parts ...string) string {
		t.Helper()
		path := filepath.Join(append([]string{root}, parts...)...)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte("fixture"), 0o700))
		return path
	}
	inPath := hostJava("path", name)
	inHome := hostJava("java home", "bin", name)
	inEnv := hostJava("explicit env", name)
	inFile := hostJava("显式路径", name)
	t.Setenv("OSCAR_JAVA_BIN", inEnv)
	t.Setenv("JAVA_HOME", filepath.Join(root, "java home"))
	t.Setenv("PATH", filepath.Dir(inPath))
	check := func(setting, expected string) {
		t.Helper()
		path, err := resolveJava(setting)
		require.NoError(t, err)
		require.Equal(t, expected, path)
	}
	check(inFile, inFile)
	check("", inEnv)
	bad := filepath.Join(root, "invalid")
	_, err := resolveJava(bad)
	require.ErrorContains(t, err, "OSCAR_JAVA_BIN")
	require.ErrorContains(t, err, "invalid")
	t.Setenv("OSCAR_JAVA_BIN", "")
	check("", inHome)
	t.Setenv("JAVA_HOME", bad)
	check("", inPath)
	t.Setenv("PATH", t.TempDir())
	t.Chdir(root)
	hostJava("runtime", "bin", name)
	_, err = resolveJava("")
	require.ErrorContains(t, err, "Java 8")
}

func TestOscarStartupIncludesJavaError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell fixture for the failing JVM")
	}
	root := t.TempDir()
	java := filepath.Join(root, "java")
	script := "#!/bin/sh\nprintf '%s\\n' 'Picked up JAVA_TOOL_OPTIONS: secret-value' 'java.lang.UnsupportedClassVersionError: unsupported class file version' >&2\nexit 1\n"
	require.NoError(t, os.WriteFile(java, []byte(script), 0o700))
	jar := filepath.Join(root, "fixture.jar")
	require.NoError(t, os.WriteFile(jar, nil, 0o600))
	manager := NewManager(Config{JavaBin: java, BridgeJar: jar, JDBCJar: jar, TempDir: root})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := manager.OpenSession(ctx, SessionOptions{})
	require.ErrorContains(t, err, "Java 8")
	require.ErrorContains(t, err, "UnsupportedClassVersionError")
	require.NotContains(t, err.Error(), "secret-value")
}

func TestConcurrentOscarStartupWait(t *testing.T) {
	manager := NewManager(Config{})
	attempt := &bridgeStartup{done: make(chan struct{})}
	manager.starting = attempt
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, manager.ensureStarted(ctx), context.DeadlineExceeded)
	attempt.err = errors.New("incompatible Java")
	close(attempt.done)
	require.ErrorIs(t, manager.ensureStarted(context.Background()), attempt.err)
}

func TestFailedOscarHandshakeReapsLauncherWithInheritedPipes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a Unix shell launcher and signal 0")
	}
	root := t.TempDir()
	launcher := filepath.Join(root, "java")
	jar := filepath.Join(root, "fixture.jar")
	const script = `#!/bin/sh
echo $$ > "$PID_DIR/launcher"
sleep 30 &
echo $! > "$PID_DIR/child"
echo "fixture JVM startup error" >&2
printf '\000\000\000\000'
exit 1
`
	require.NoError(t, os.WriteFile(launcher, []byte(script), 0o700))
	require.NoError(t, os.WriteFile(jar, nil, 0o600))
	t.Setenv("PID_DIR", root)
	process := func(name string) *os.Process {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, name))
		require.NoError(t, err)
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		require.NoError(t, err)
		p, err := os.FindProcess(pid)
		require.NoError(t, err)
		return p
	}
	t.Cleanup(func() {
		if _, err := os.Stat(filepath.Join(root, "child")); err == nil {
			_ = process("child").Kill()
		}
	})
	manager := NewManager(Config{JavaBin: launcher, BridgeJar: jar, JDBCJar: jar})
	failed := make(chan error, 1)
	go func() { failed <- manager.ensureStarted(context.Background()) }()
	select {
	case err := <-failed:
		require.ErrorContains(t, err, "fixture JVM startup error")
	case <-time.After(5 * time.Second):
		t.Fatal("failed handshake waited for the launcher's child")
	}
	require.NoError(t, process("child").Signal(syscall.Signal(0)))
	require.Error(t, process("launcher").Signal(syscall.Signal(0)), "launcher is still alive or unreaped")
}
