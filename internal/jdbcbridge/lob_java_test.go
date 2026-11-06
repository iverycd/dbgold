package jdbcbridge

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestJavaLOBProtocol(t *testing.T) {
	javac, err := exec.LookPath("javac")
	if err != nil {
		t.Skip("javac is unavailable")
	}
	java, err := exec.LookPath("java")
	if err != nil {
		t.Skip("java is unavailable")
	}
	dir := t.TempDir()
	main := filepath.Join("..", "..", "jdbcbridge", "java", "src", "com", "dbgold", "oscar", "BridgeMain.java")
	if out, err := exec.Command(javac, "--release", "8", "-encoding", "UTF-8", "-d", dir, main, filepath.Join("testdata", "BridgeMainLOBTest.java")).CombinedOutput(); err != nil {
		t.Fatalf("compile Java LOB test: %v\n%s", err, out)
	}
	if out, err := exec.Command(java, "-Xmx256m", "-cp", dir, "com.dbgold.oscar.BridgeMainLOBTest").CombinedOutput(); err != nil {
		t.Fatalf("Java LOB protocol: %v\n%s", err, out)
	}
}
