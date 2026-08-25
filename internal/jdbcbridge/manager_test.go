package jdbcbridge

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHelperHandshake(t *testing.T) {
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
